// Package routeros implements the `routeros` executable plugin: every IPv4
// address in the DNS answer is installed on a MikroTik RouterOS device as a
// /32 static route (via the binary API) so that a L3HW-capable switch forwards
// traffic to that destination through the configured gateway.
package routeros

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/netip"
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
	Timeout            string `yaml:"timeout"` // duration, default 2s

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

	queue   chan func()
	wg      sync.WaitGroup
	closeMu sync.Mutex
	closed  bool

	warnAAAA rateLimiter
	warnWait rateLimiter
}

var (
	_ sequence.Executable            = (*RouterOS)(nil)
	_ sequence.QuickConfigurableExec = (*RouterOS)(nil)
	_ sequence.Executable            = (*gwExec)(nil)
)

func Init(bp *coremain.BP, args any) (any, error) {
	a := args.(*Args)
	l := bp.L()

	if a.Address == "" || a.Username == "" {
		return nil, fmt.Errorf("address and username are required")
	}
	if !strings.Contains(a.Address, ":") {
		if a.TLS {
			a.Address += ":8729"
		} else {
			a.Address += ":8728"
		}
	}
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

	p := newPool(cl, l, poolConfig{maxDyn: a.MaxRoutes, replace: replace})
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

	r := &RouterOS{
		logger:  l,
		cl:      cl,
		pool:    p,
		defGw:   defIdx,
		wait:    wait,
		timeout: timeout,
		onExit:  a.OnExit,
		queue:   make(chan func(), asyncQueueSize),
	}
	cl.onReconnect = func() {
		r.pool.Sync(syncClean)
	}

	// Connect. A login rejection is fatal; a network error only logs and the
	// client keeps retrying in the background so DNS service is not held
	// hostage by the switch.
	if err := cl.Connect(); err != nil {
		if isDeviceError(err) {
			return nil, fmt.Errorf("routeros login failed: %w", err)
		}
		l.Warn("routeros api unreachable at startup; will retry in background", zap.Error(err))
		cl.wg.Add(1)
		go func() {
			defer cl.wg.Done()
			cl.reconnectLoop()
		}()
	} else {
		mode := syncAdopt
		if a.OnStart == "clean" {
			mode = syncClean
		}
		p.Sync(mode)
		go r.startupDiag(defGw, a.Static)
	}

	if !wait {
		r.wg.Add(1)
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

	acts, removeIDs := r.pool.Learn(ips, gw)
	if len(acts) == 0 && len(removeIDs) == 0 {
		return nil
	}

	if !r.wait {
		select {
		case r.queue <- func() { r.pool.Apply(acts, removeIDs) }:
		default:
			r.logger.Warn("routeros async queue full; dropping route update", zap.Int("routes", len(acts)))
			r.pool.dropInflight(acts)
		}
		return nil
	}

	done := make(chan struct{})
	go func() {
		r.pool.Apply(acts, removeIDs)
		close(done)
	}()
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
	defer r.wg.Done()
	for f := range r.queue {
		f()
	}
}

// Close removes our routes (if on_exit=clean) and disconnects.
func (r *RouterOS) Close() error {
	r.closeMu.Lock()
	if r.closed {
		r.closeMu.Unlock()
		return nil
	}
	r.closed = true
	r.closeMu.Unlock()

	if !r.wait {
		close(r.queue)
		r.wg.Wait()
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
		writeJSON(w, r.pool.Flush())
	})
	return m
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}
