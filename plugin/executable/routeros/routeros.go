// Package routeros implements the `routeros` executable plugin: every IPv4
// address in the DNS answer is installed on a MikroTik RouterOS device as a
// /32 static route (via the binary API) so that a L3HW-capable switch forwards
// traffic to that destination through the configured gateway.
package routeros

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/IrineSistiana/mosdns/v5/coremain"
	"github.com/IrineSistiana/mosdns/v5/pkg/query_context"
	"github.com/IrineSistiana/mosdns/v5/plugin/executable/sequence"
	"github.com/go-chi/chi/v5"
	"github.com/miekg/dns"
	"go.uber.org/zap"
)

const PluginType = "routeros"

func init() {
	coremain.RegNewPluginFunc(PluginType, Init, func() any { return new(Args) })
	sequence.MustRegExecQuickSetup(PluginType, func(_ sequence.BQ, _ string) (any, error) {
		return nil, fmt.Errorf("%s must be referenced as a configured plugin via $tag", PluginType)
	})
}

type StaticGroup struct {
	Gateway string   `yaml:"gateway"`
	IPs     []string `yaml:"ips"`
	Files   []string `yaml:"files"`
}

type Args struct {
	// connection
	Address            string `yaml:"address"`
	Username           string `yaml:"username"`
	Password           string `yaml:"password"`
	TLS                bool   `yaml:"tls"`
	InsecureSkipVerify bool   `yaml:"insecure_skip_verify"`
	Timeout            string `yaml:"timeout"` // dial/login/add/set duration, default 2s

	// route attributes
	Gateway      string `yaml:"gateway"`
	Comment      string `yaml:"comment"` // default "mosdns"
	RoutingTable string `yaml:"routing_table"`
	Distance     int    `yaml:"distance"`

	// dynamic pool
	MaxRoutes int    `yaml:"max_routes"` // default 40000
	Conflict  string `yaml:"conflict"`   // keep (default) | replace
	Wait      *bool  `yaml:"wait"`       // default true
	OnExit    string `yaml:"on_exit"`    // clean (default) | keep
	OnStart   string `yaml:"on_start"`   // adopt (default) | clean

	Static []StaticGroup `yaml:"static"`
}

// RouterOS is the plugin instance.
type RouterOS struct {
	logger  *zap.Logger
	cl      *client
	pool    *pool
	defGw   uint8
	wait    bool
	timeout time.Duration
	onExit  string

	startMode    syncMode // on_start policy, used by the first successful sync
	firstSyncMu  sync.Mutex
	firstSynced  bool // set after the on_start list/reconciliation has run
	staticGroups []StaticGroup

	queue            chan func()
	queueSlots       chan struct{}
	workerWG         sync.WaitGroup
	opsWG            sync.WaitGroup
	lifeMu           sync.RWMutex // guards paused/closed and opsWG.Add versus Wait
	controlMu        sync.Mutex   // serialises Flush and Close
	paused           bool
	closed           bool
	reconnectPending bool

	warnAAAA rateLimiter
	warnWait rateLimiter
}

// onConnected runs after every successful API connection. The first time it
// applies the on_start policy and the one-off L3HW diagnostics; afterwards the
// pool is the source of truth and unknown routes are removed.
func (r *RouterOS) onConnected() {
	r.firstSyncMu.Lock()
	if !r.firstSynced {
		res := r.pool.Sync(r.startMode)
		if res.Listed {
			r.firstSynced = true
			r.schedule(func() { r.startupDiag(r.pool.gwAddr(r.defGw), r.staticGroups) })
		}
		r.firstSyncMu.Unlock()
		return
	}
	r.firstSyncMu.Unlock()
	r.pool.Sync(syncClean)
}

// schedule tracks background control-plane work so Close can establish a
// barrier before its final sweep. It is never used by the dynamic hit path.
func (r *RouterOS) schedule(f func()) bool {
	r.lifeMu.RLock()
	if r.paused || r.closed {
		r.lifeMu.RUnlock()
		return false
	}
	r.opsWG.Add(1)
	r.lifeMu.RUnlock()
	go func() {
		defer r.opsWG.Done()
		f()
	}()
	return true
}

func (r *RouterOS) scheduleConnected() {
	r.lifeMu.Lock()
	if r.closed {
		r.lifeMu.Unlock()
		return
	}
	if r.paused {
		// Flush owns the control plane. Collapse any number of reconnects into
		// one reconciliation that is launched when Flush releases its barrier.
		r.reconnectPending = true
		r.lifeMu.Unlock()
		return
	}
	r.opsWG.Add(1)
	r.lifeMu.Unlock()
	go func() {
		defer r.opsWG.Done()
		r.onConnected()
	}()
}

var (
	_ sequence.Executable            = (*RouterOS)(nil)
	_ sequence.QuickConfigurableExec = (*RouterOS)(nil)
	_ sequence.Executable            = (*gwExec)(nil)
)

func normalizeAddress(s string, useTLS bool) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" || strings.ContainsAny(s, " \t\r\n") {
		return "", fmt.Errorf("invalid address %q", s)
	}
	defaultPort := 8728
	if useTLS {
		defaultPort = 8729
	}
	if host, port, err := net.SplitHostPort(s); err == nil {
		if host == "" {
			return "", fmt.Errorf("invalid address %q: empty host", s)
		}
		if _, err := netip.ParseAddr(host); err != nil && !validHostname(host) {
			return "", fmt.Errorf("invalid address %q: bad host", s)
		}
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return "", fmt.Errorf("invalid address %q: bad port", s)
		}
		return net.JoinHostPort(host, port), nil
	}

	rawIP := strings.TrimSuffix(strings.TrimPrefix(s, "["), "]")
	if ip, err := netip.ParseAddr(rawIP); err == nil {
		return net.JoinHostPort(ip.String(), strconv.Itoa(defaultPort)), nil
	}
	if strings.Contains(s, ":") {
		return "", fmt.Errorf("invalid address %q", s)
	}
	if !validHostname(s) {
		return "", fmt.Errorf("invalid address %q", s)
	}
	return net.JoinHostPort(s, strconv.Itoa(defaultPort)), nil
}

func validHostname(s string) bool {
	if len(s) == 0 || len(s) > 253 {
		return false
	}
	s = strings.TrimSuffix(s, ".")
	for _, label := range strings.Split(s, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, c := range label {
			if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') &&
				(c < '0' || c > '9') && c != '-' {
				return false
			}
		}
	}
	return true
}

func Init(bp *coremain.BP, args any) (any, error) {
	a := args.(*Args)
	l := bp.L()

	if a.Address == "" || a.Username == "" {
		return nil, fmt.Errorf("address and username are required")
	}
	address, err := normalizeAddress(a.Address, a.TLS)
	if err != nil {
		return nil, err
	}
	a.Address = address
	timeout := 2 * time.Second
	if a.Timeout != "" {
		d, err := time.ParseDuration(a.Timeout)
		if err != nil || d <= 0 {
			return nil, fmt.Errorf("invalid timeout %q", a.Timeout)
		}
		timeout = d
	}
	if a.Comment == "" {
		a.Comment = "mosdns"
	}
	if a.MaxRoutes < 0 {
		return nil, fmt.Errorf("max_routes must be greater than zero, or zero for the default")
	}
	if a.Distance < 0 || a.Distance > 255 {
		return nil, fmt.Errorf("distance must be between 1 and 255, or zero for the default")
	}
	if a.Gateway == "" {
		return nil, fmt.Errorf("gateway is required")
	}
	defGw, err := parseGateway(a.Gateway)
	if err != nil {
		return nil, err
	}
	var replace bool
	switch a.Conflict {
	case "", "keep":
	case "replace":
		replace = true
	default:
		return nil, fmt.Errorf("conflict must be keep or replace, got %q", a.Conflict)
	}
	switch a.OnExit {
	case "":
		a.OnExit = "clean"
	case "clean", "keep":
	default:
		return nil, fmt.Errorf("on_exit must be clean or keep, got %q", a.OnExit)
	}
	switch a.OnStart {
	case "":
		a.OnStart = "adopt"
	case "adopt", "clean":
	default:
		return nil, fmt.Errorf("on_start must be adopt or clean, got %q", a.OnStart)
	}
	wait := true
	if a.Wait != nil {
		wait = *a.Wait
	}

	cl := newClient(clientConfig{
		address:            a.Address,
		username:           a.Username,
		password:           a.Password,
		useTLS:             a.TLS,
		insecureSkipVerify: a.InsecureSkipVerify,
		timeout:            timeout,
		comment:            a.Comment,
		routingTable:       a.RoutingTable,
		distance:           a.Distance,
	}, l)

	p := newPool(cl, l, poolConfig{
		maxDyn:       a.MaxRoutes,
		replace:      replace,
		routingTable: a.RoutingTable,
		distance:     a.Distance,
	})
	defIdx, err := p.gateway(defGw)
	if err != nil {
		return nil, err
	}
	for i, g := range a.Static {
		if g.Gateway == "" {
			return nil, fmt.Errorf("static[%d]: gateway is required", i)
		}
		gw, err := parseGateway(g.Gateway)
		if err != nil {
			return nil, fmt.Errorf("static[%d]: %w", i, err)
		}
		prefixes, err := loadPrefixes(l, g.IPs, g.Files)
		if err != nil {
			return nil, fmt.Errorf("static[%d]: %w", i, err)
		}
		for _, pf := range prefixes {
			if err := p.addStatic(pf, gw); err != nil {
				return nil, fmt.Errorf("static[%d]: %w", i, err)
			}
		}
	}
	p.finishStatic()

	startMode := syncAdopt
	if a.OnStart == "clean" {
		startMode = syncClean
	}
	r := &RouterOS{
		logger:       l,
		cl:           cl,
		pool:         p,
		defGw:        defIdx,
		wait:         wait,
		timeout:      timeout,
		onExit:       a.OnExit,
		startMode:    startMode,
		staticGroups: a.Static,
		queue:        make(chan func(), asyncQueueSize),
		queueSlots:   make(chan struct{}, asyncQueueSize),
	}
	cl.onReconnect = r.scheduleConnected

	// Connect. A login rejection is fatal; a network error only logs and the
	// client keeps retrying in the background so DNS service is not held
	// hostage by the switch. The on_start sync runs on whichever connection
	// succeeds first.
	if err := cl.Connect(); err != nil {
		if isTrap(err) {
			return nil, fmt.Errorf("routeros login failed: %w", err)
		}
		l.Warn("routeros api unreachable at startup; will retry in background", zap.Error(err))
		cl.wg.Add(1)
		go func() {
			defer cl.wg.Done()
			cl.reconnectLoop()
		}()
	} else {
		r.onConnected()
	}

	if !wait {
		r.workerWG.Add(1)
		go r.worker()
	}

	bp.RegAPI(r.api())
	l.Info("routeros plugin ready",
		zap.String("address", a.Address), zap.Stringer("gateway", defGw),
		zap.Int("static", len(p.static)), zap.Bool("wait", wait), zap.String("on_exit", a.OnExit))
	return r, nil
}

// Exec installs routes for the A records of the response via the default gateway.
func (r *RouterOS) Exec(ctx context.Context, qCtx *query_context.Context) error {
	return r.exec(ctx, qCtx, r.defGw)
}

// QuickConfigureExec supports `$tag` and `$tag <gateway>`.
func (r *RouterOS) QuickConfigureExec(args string) (any, error) {
	args = strings.TrimSpace(args)
	if args == "" {
		return r, nil
	}
	gw, err := parseGateway(args)
	if err != nil {
		return nil, err
	}
	gi, err := r.pool.gateway(gw)
	if err != nil {
		return nil, err
	}
	return &gwExec{r: r, gw: gi}, nil
}

type gwExec struct {
	r  *RouterOS
	gw uint8
}

func (g *gwExec) Exec(ctx context.Context, qCtx *query_context.Context) error {
	return g.r.exec(ctx, qCtx, g.gw)
}

func (r *RouterOS) exec(_ context.Context, qCtx *query_context.Context, gw uint8) error {
	resp := qCtx.R()
	if resp == nil {
		return nil
	}
	var ips []netip.Addr
	sawAAAA := false
	for _, rr := range resp.Answer {
		switch v := rr.(type) {
		case *dns.A:
			if a, ok := netip.AddrFromSlice(v.A); ok {
				ips = append(ips, a.Unmap())
			}
		case *dns.AAAA:
			sawAAAA = true
		}
	}
	if sawAAAA && len(ips) == 0 && r.warnAAAA.allow() {
		r.logger.Warn("AAAA answers ignored: routeros plugin is IPv4 only", zap.String("qname", qCtx.QQuestion().Name))
	}
	if len(ips) == 0 {
		return nil
	}

	// Preserve the original repeated-hit hot path: no lifecycle lock, queue
	// operation or allocation beyond the already extracted answer slice.
	if r.pool.FastHit(ips, gw) {
		return nil
	}

	slotReserved := false
	if !r.wait {
		// Reserve capacity before mutating the pool. A saturated queue can
		// therefore never require a lossy rollback of eviction state.
		select {
		case r.queueSlots <- struct{}{}:
			slotReserved = true
		default:
			r.logger.Warn("routeros async queue full; dropping route update")
			return nil
		}
	}

	r.lifeMu.RLock()
	if r.paused || r.closed {
		r.lifeMu.RUnlock()
		if slotReserved {
			<-r.queueSlots
		}
		return nil
	}
	acts, removeIDs := r.pool.Learn(ips, gw)
	if len(acts) == 0 && len(removeIDs) == 0 {
		r.lifeMu.RUnlock()
		if slotReserved {
			<-r.queueSlots
		}
		return nil
	}
	r.opsWG.Add(1)

	if !r.wait {
		r.queue <- func() {
			defer r.opsWG.Done()
			defer func() { <-r.queueSlots }()
			r.pool.Apply(acts, removeIDs)
		}
		r.lifeMu.RUnlock()
		return nil
	}

	done := make(chan struct{})
	go func() {
		defer r.opsWG.Done()
		r.pool.Apply(acts, removeIDs)
		close(done)
	}()
	r.lifeMu.RUnlock()
	t := time.NewTimer(r.timeout)
	defer t.Stop()
	select {
	case <-done:
	case <-t.C:
		// The command stays in flight and the entry is updated when the router
		// answers; we just stop holding the DNS response.
		if r.warnWait.allow() {
			r.logger.Warn("routeros did not confirm route within timeout; answering anyway", zap.Duration("timeout", r.timeout))
		}
	}
	return nil
}

func (r *RouterOS) worker() {
	defer r.workerWG.Done()
	for f := range r.queue {
		f()
	}
}

func (r *RouterOS) flush() syncResult {
	r.controlMu.Lock()
	defer r.controlMu.Unlock()

	r.lifeMu.Lock()
	if r.closed {
		r.lifeMu.Unlock()
		return syncResult{Time: time.Now(), Err: "routeros plugin is closed"}
	}
	r.paused = true
	r.lifeMu.Unlock()

	// Existing operations are bounded by the client command watchdog. DNS
	// requests arriving while paused return normally without adding routes.
	r.opsWG.Wait()
	res := r.pool.Flush()

	runReconnect := false
	r.lifeMu.Lock()
	if !r.closed {
		r.paused = false
		if r.reconnectPending {
			r.reconnectPending = false
			r.opsWG.Add(1)
			runReconnect = true
		}
	}
	r.lifeMu.Unlock()
	if runReconnect {
		go func() {
			defer r.opsWG.Done()
			r.onConnected()
		}()
	}
	return res
}

// Close removes our routes (if on_exit=clean) and disconnects.
func (r *RouterOS) Close() error {
	r.controlMu.Lock()
	defer r.controlMu.Unlock()

	r.lifeMu.Lock()
	if r.closed {
		r.lifeMu.Unlock()
		return nil
	}
	r.paused = true
	r.closed = true
	if !r.wait {
		close(r.queue) // no producer can hold lifeMu.RLock now
	}
	r.lifeMu.Unlock()

	r.opsWG.Wait()
	if !r.wait {
		r.workerWG.Wait()
	}
	r.pool.Close(r.onExit == "clean")
	return r.cl.Close()
}

// --- HTTP API (mounted at /plugins/<tag>/) ---

func (r *RouterOS) api() *chi.Mux {
	m := chi.NewRouter()
	m.Get("/routes", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, r.pool.Routes())
	})
	m.Get("/stats", func(w http.ResponseWriter, _ *http.Request) {
		out := struct {
			Connected bool              `json:"connected"`
			Pool      poolStats         `json:"pool"`
			L3HW      map[string]string `json:"l3hw_monitor,omitempty"`
		}{Connected: r.cl.Connected(), Pool: r.pool.Stats()}
		if out.Connected {
			if mon, err := r.l3hwMonitor(); err == nil {
				out.L3HW = mon
			} else {
				out.L3HW = map[string]string{"error": err.Error()}
			}
		}
		writeJSON(w, out)
	})
	m.Post("/flush", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, r.flush())
	})
	return m
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}
