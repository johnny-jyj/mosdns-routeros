package routeros

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	ros "github.com/go-routeros/routeros/v3"
	"github.com/go-routeros/routeros/v3/proto"
	"go.uber.org/zap"
)

// errNotConnected is returned by every command while the API connection is down.
var errNotConnected = errors.New("routeros: not connected")

// remoteRoute is one /ip/route entry owned by this plugin (matched by comment).
type remoteRoute struct {
	id       uint32
	dst      netip.Prefix
	gw       netip.Addr
	table    string
	distance int
}

// backend is the RouterOS side as seen by the pool. It is an interface so the
// pool can be unit-tested with a fake.
type backend interface {
	Connected() bool
	AddRoute(dst netip.Prefix, gw netip.Addr) (uint32, error)
	SetRoute(id uint32, gw netip.Addr) error
	RemoveRoutes(ids []uint32) error
	ListRoutes() ([]remoteRoute, error)
}

type clientConfig struct {
	address            string
	username           string
	password           string
	useTLS             bool
	insecureSkipVerify bool
	timeout            time.Duration // dial + login
	comment            string
	routingTable       string
	distance           int
}

// client wraps one persistent go-routeros connection in Async mode and
// reconnects with exponential backoff when the read loop reports an error.
type client struct {
	cfg    clientConfig
	logger *zap.Logger

	mu        sync.Mutex // guards c, cancel and generation
	c         *ros.Client
	cancel    context.CancelFunc
	gen       uint64 // incremented on every (re)connect
	connected atomic.Bool
	closed    atomic.Bool
	closeCh   chan struct{}
	wg        sync.WaitGroup

	// onReconnect is invoked (in its own goroutine) after every successful
	// reconnect; the first connect does not trigger it.
	onReconnect func()
}

func newClient(cfg clientConfig, logger *zap.Logger) *client {
	return &client{cfg: cfg, logger: logger, closeCh: make(chan struct{})}
}

// Connect dials, logs in and switches the connection to async mode. A login
// rejection is returned as a *ros.DeviceError so the caller can fail fast; a
// network error is returned as-is so the caller may decide to retry later.
func (cl *client) Connect() error {
	d := net.Dialer{Timeout: cl.cfg.timeout, KeepAlive: 10 * time.Second}
	conn, err := d.Dial("tcp", cl.cfg.address)
	if err != nil {
		return err
	}
	if cl.cfg.useTLS {
		host, _, _ := net.SplitHostPort(cl.cfg.address)
		conn = tls.Client(conn, &tls.Config{
			ServerName:         host,
			InsecureSkipVerify: cl.cfg.insecureSkipVerify, //nolint:gosec
		})
	}
	// Login runs in sync mode; bound it with a deadline so a silent peer cannot
	// hang startup forever.
	_ = conn.SetDeadline(time.Now().Add(cl.cfg.timeout))
	c, err := ros.NewClient(conn)
	if err != nil {
		_ = conn.Close()
		return err
	}
	c.SetLogHandler(zapSlogHandler{l: cl.logger.Named("go-routeros")})
	if err := c.Login(cl.cfg.username, cl.cfg.password); err != nil {
		_ = c.Close()
		return err
	}
	_ = conn.SetDeadline(time.Time{})

	asyncCtx, asyncCancel := context.WithCancel(context.Background())
	errC := c.AsyncContext(asyncCtx)

	cl.mu.Lock()
	if cl.closed.Load() {
		// Close() raced with this connect: do not keep the new connection.
		cl.mu.Unlock()
		asyncCancel()
		_ = c.Close()
		return errNotConnected
	}
	cl.c = c
	cl.cancel = asyncCancel
	cl.gen++
	gen := cl.gen
	cl.connected.Store(true)
	cl.wg.Add(1)
	cl.mu.Unlock()
	go cl.watch(gen, c, asyncCancel, errC)
	return nil
}

// watch waits for the async read loop of connection gen to end and starts the
// reconnect loop unless the client is being closed.
func (cl *client) watch(gen uint64, c *ros.Client, cancel context.CancelFunc, errC <-chan error) {
	defer cl.wg.Done()
	defer cancel() // releases AsyncContext's cancellation waiter
	err, ok := <-errC
	if cl.closed.Load() {
		return
	}
	cl.mu.Lock()
	if cl.gen != gen { // a newer connection already replaced us
		cl.mu.Unlock()
		return
	}
	cl.c = nil
	cl.cancel = nil
	cl.connected.Store(false)
	cl.mu.Unlock()
	_ = c.Close()
	if !ok || err == nil {
		err = errors.New("read loop ended")
	}
	cl.logger.Warn("routeros api connection lost, reconnecting", zap.Error(err))
	cl.reconnectLoop()
}

func (cl *client) reconnectLoop() {
	backoff := time.Second
	for {
		select {
		case <-cl.closeCh:
			return
		case <-time.After(backoff):
		}
		if cl.closed.Load() {
			return
		}
		if err := cl.Connect(); err != nil {
			cl.logger.Warn("routeros reconnect failed", zap.Error(err), zap.Duration("retry_in", backoff*2))
			backoff *= 2
			if backoff > 30*time.Second {
				backoff = 30 * time.Second
			}
			continue
		}
		cl.logger.Info("routeros api reconnected")
		if cl.onReconnect != nil {
			go cl.onReconnect()
		}
		return
	}
}

// Close stops reconnecting and closes the connection.
func (cl *client) Close() error {
	if !cl.closed.CompareAndSwap(false, true) {
		return nil
	}
	close(cl.closeCh)
	cl.mu.Lock()
	c := cl.c
	cancel := cl.cancel
	cl.c = nil
	cl.cancel = nil
	cl.connected.Store(false)
	cl.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	var err error
	if c != nil {
		err = c.Close()
	}
	cl.wg.Wait()
	return err
}

func (cl *client) Connected() bool { return cl.connected.Load() }

// run executes one API sentence with the configured command deadline. The
// pinned go-routeros patch abandons only this command's tag on cancellation;
// we then invalidate this connection generation so sync can resolve whether a
// timed-out mutating command reached RouterOS.
func (cl *client) run(words ...string) (*ros.Reply, error) {
	return cl.runTimeout(cl.cfg.timeout, words...)
}

func (cl *client) runTimeout(timeout time.Duration, words ...string) (*ros.Reply, error) {
	cl.mu.Lock()
	c := cl.c
	gen := cl.gen
	cl.mu.Unlock()
	if c == nil {
		return nil, errNotConnected
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	// RunArgsContext only observes ctx after the sentence has been written.
	// If the peer stops reading, force-close this exact generation at the
	// deadline so a blocked Flush (and commands queued on the writer) unwind.
	stopWatchdog := context.AfterFunc(ctx, func() { cl.invalidate(gen, c) })
	defer stopWatchdog()
	r, err := c.RunArgsContext(ctx, words)
	if err != nil && !isTrap(err) {
		cl.invalidate(gen, c)
	}
	return r, err
}

// invalidate closes c only if it is still the active connection generation.
// The generation's watcher owns reconnecting, so concurrent command failures
// cannot start duplicate reconnect loops.
func (cl *client) invalidate(gen uint64, c *ros.Client) {
	cl.mu.Lock()
	if cl.gen != gen || cl.c != c {
		cl.mu.Unlock()
		return
	}
	cancel := cl.cancel
	cl.c = nil
	cl.cancel = nil
	cl.connected.Store(false)
	cl.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	_ = c.Close()
}

func (cl *client) bulkTimeout() time.Duration {
	const minBulkTimeout = 30 * time.Second
	return max(cl.cfg.timeout, minBulkTimeout)
}

func (cl *client) AddRoute(dst netip.Prefix, gw netip.Addr) (uint32, error) {
	words := []string{
		"/ip/route/add",
		"=dst-address=" + dst.String(),
		"=gateway=" + gw.String(),
		"=comment=" + cl.cfg.comment,
	}
	if cl.cfg.routingTable != "" {
		words = append(words, "=routing-table="+cl.cfg.routingTable)
	}
	if cl.cfg.distance > 0 {
		words = append(words, "=distance="+strconv.Itoa(cl.cfg.distance))
	}
	r, err := cl.run(words...)
	if err != nil {
		return 0, err
	}
	if r.Done == nil {
		return 0, errors.New("routeros: add returned no !done")
	}
	return parseID(r.Done.Map["ret"])
}

func (cl *client) SetRoute(id uint32, gw netip.Addr) error {
	words := []string{"/ip/route/set", "=.id=" + formatID(id), "=gateway=" + gw.String()}
	if cl.cfg.distance > 0 {
		words = append(words, "=distance="+strconv.Itoa(cl.cfg.distance))
	} else {
		words = append(words, "=distance=1")
	}
	_, err := cl.run(words...)
	return err
}

// RemoveRoutes removes ids in chunks of removeBatch per command.
func (cl *client) RemoveRoutes(ids []uint32) error {
	var firstErr error
	for len(ids) > 0 {
		n := min(len(ids), removeBatch)
		chunk := ids[:n]
		ids = ids[n:]
		parts := make([]string, n)
		for i, id := range chunk {
			parts[i] = formatID(id)
		}
		if _, err := cl.runTimeout(cl.bulkTimeout(), "/ip/route/remove", "=numbers="+strings.Join(parts, ",")); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// ListRoutes returns every /ip/route whose comment equals ours.
func (cl *client) ListRoutes() ([]remoteRoute, error) {
	r, err := cl.runTimeout(cl.bulkTimeout(), "/ip/route/print", "?comment="+cl.cfg.comment,
		"=.proplist=.id,dst-address,gateway,routing-table,distance")
	if err != nil {
		return nil, err
	}
	out := make([]remoteRoute, 0, len(r.Re))
	for _, s := range r.Re {
		id, err := parseID(s.Map[".id"])
		if err != nil {
			cl.logger.Warn("skip route with unparsable id", zap.String("id", s.Map[".id"]))
			continue
		}
		dst, err := parseDst(s.Map["dst-address"])
		if err != nil {
			cl.logger.Warn("skip route with unparsable dst-address", zap.String("dst", s.Map["dst-address"]))
			continue
		}
		gw, err := netip.ParseAddr(s.Map["gateway"])
		if err != nil {
			// gateway can be an interface name or "a.b.c.d%iface"; not ours.
			gw = netip.Addr{}
		}
		table := s.Map["routing-table"]
		if table == "" {
			table = "main"
		}
		distance, err := strconv.Atoi(s.Map["distance"])
		if err != nil || distance <= 0 {
			distance = 1
		}
		out = append(out, remoteRoute{id: id, dst: dst, gw: gw, table: table, distance: distance})
	}
	return out, nil
}

// print runs an arbitrary print-like command and returns the !re sentences.
func (cl *client) print(words ...string) ([]*proto.Sentence, error) {
	r, err := cl.run(words...)
	if err != nil {
		return nil, err
	}
	return r.Re, nil
}

func parseID(s string) (uint32, error) {
	s = strings.TrimPrefix(s, "*")
	if s == "" {
		return 0, errors.New("routeros: empty .id")
	}
	v, err := strconv.ParseUint(s, 16, 32)
	if err != nil {
		return 0, fmt.Errorf("routeros: bad .id %q: %w", s, err)
	}
	return uint32(v), nil
}

func formatID(id uint32) string { return "*" + strconv.FormatUint(uint64(id), 16) }

func parseDst(s string) (netip.Prefix, error) {
	if strings.Contains(s, "/") {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			return netip.Prefix{}, err
		}
		return p.Masked(), nil
	}
	a, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Prefix{}, err
	}
	return netip.PrefixFrom(a, a.BitLen()), nil
}

func isTrap(err error) bool {
	var de *ros.DeviceError
	return errors.As(err, &de) && de.Sentence != nil && de.Sentence.Word == "!trap"
}

func isNotFoundTrap(err error) bool {
	var de *ros.DeviceError
	if !errors.As(err, &de) || de.Sentence == nil || de.Sentence.Word != "!trap" {
		return false
	}
	return strings.Contains(strings.ToLower(de.Sentence.Map["message"]), "no such item")
}

func isFatal(err error) bool {
	var de *ros.DeviceError
	return errors.As(err, &de) && de.Sentence != nil && de.Sentence.Word == "!fatal"
}

// zapSlogHandler forwards go-routeros' slog output (warn and above only) to zap.
type zapSlogHandler struct{ l *zap.Logger }

func (h zapSlogHandler) Enabled(_ context.Context, lv slog.Level) bool { return lv >= slog.LevelWarn }
func (h zapSlogHandler) Handle(_ context.Context, r slog.Record) error {
	fields := make([]zap.Field, 0, r.NumAttrs())
	r.Attrs(func(a slog.Attr) bool {
		fields = append(fields, zap.Any(a.Key, a.Value.Any()))
		return true
	})
	if r.Level >= slog.LevelError {
		h.l.Error(r.Message, fields...)
	} else {
		h.l.Warn(r.Message, fields...)
	}
	return nil
}
func (h zapSlogHandler) WithAttrs(_ []slog.Attr) slog.Handler { return h }
func (h zapSlogHandler) WithGroup(_ string) slog.Handler      { return h }
