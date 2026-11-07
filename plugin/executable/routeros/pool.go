package routeros

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/IrineSistiana/mosdns/v5/pkg/matcher/netlist"
	"go.uber.org/zap"
)

const (
	removeBatch    = 500 // max .ids per /ip/route/remove command
	maxGateways    = 255 // gateway index is a uint8
	evictBatchMax  = 64
	defaultMaxDyn  = 40000
	warnRateLimit  = 10 * time.Second
	asyncQueueSize = 1024
)

// dynamic entry states
const (
	stateInflight uint8 = iota // an add/set for this entry is in flight
	stateSynced                // exists on the router with id
	stateOffline               // learned while disconnected; sync will add it
)

type dynEntry struct {
	ip    uint32
	id    uint32
	slot  int32 // index in pool.clock
	gw    uint8
	state uint8
	ref   atomic.Bool // CLOCK reference bit
}

type staticEntry struct {
	prefix netip.Prefix
	gw     uint8
	id     uint32
	synced bool
}

// syncMode selects what to do with router routes that are not in the pool.
type syncMode uint8

const (
	syncAdopt syncMode = iota // startup, on_start=adopt: take them over as dynamic
	syncClean                 // startup on_start=clean / reconnect / flush: remove
)

type syncResult struct {
	Time    time.Time `json:"time"`
	Router  int       `json:"router"`
	Pool    int       `json:"pool"`
	Adopted int       `json:"adopted"`
	Set     int       `json:"set"`
	Added   int       `json:"added"`
	Removed int       `json:"removed"`
	Err     string    `json:"err,omitempty"`
}

// pool owns every route this plugin created on the router.
//
// Static routes are immutable after construction and need no lock. Dynamic
// routes live in dyn/clock under mu; the hot path (an IP already present) only
// takes the shared read lock and flips one atomic bit.
type pool struct {
	be     backend
	logger *zap.Logger

	// gateway table; index is the uint8 stored in entries
	gwMu  sync.RWMutex
	gws   []netip.Addr
	gwIdx map[netip.Addr]uint8

	// static (immutable after newPool)
	static      []staticEntry
	staticExact map[netip.Prefix]int // prefix -> index in static
	staticByGw  map[uint8]*netlist.List
	staticAny   *netlist.List

	// dynamic
	mu        sync.RWMutex
	dyn       map[uint32]*dynEntry
	clock     []*dynEntry
	free      []int32
	hand      int
	maxDyn    int
	replace   bool // conflict=replace
	closed    bool
	syncMu    sync.Mutex // serialises Sync/Flush/Close
	lastSync  atomic.Pointer[syncResult]
	syncFails atomic.Int64

	// rate-limited warnings
	warnOffline  rateLimiter
	warnConflict rateLimiter
	warnEvict    rateLimiter
}

type poolConfig struct {
	maxDyn  int
	replace bool
}

func newPool(be backend, logger *zap.Logger, cfg poolConfig) *pool {
	if cfg.maxDyn <= 0 {
		cfg.maxDyn = defaultMaxDyn
	}
	return &pool{
		be:          be,
		logger:      logger,
		gwIdx:       make(map[netip.Addr]uint8),
		staticExact: make(map[netip.Prefix]int),
		staticByGw:  make(map[uint8]*netlist.List),
		staticAny:   netlist.NewList(),
		dyn:         make(map[uint32]*dynEntry),
		maxDyn:      cfg.maxDyn,
		replace:     cfg.replace,
	}
}

// gateway returns the index of gw, registering it if new.
func (p *pool) gateway(gw netip.Addr) (uint8, error) {
	p.gwMu.RLock()
	i, ok := p.gwIdx[gw]
	p.gwMu.RUnlock()
	if ok {
		return i, nil
	}
	p.gwMu.Lock()
	defer p.gwMu.Unlock()
	if i, ok := p.gwIdx[gw]; ok {
		return i, nil
	}
	if len(p.gws) >= maxGateways {
		return 0, fmt.Errorf("too many distinct gateways (max %d)", maxGateways)
	}
	i = uint8(len(p.gws))
	p.gws = append(p.gws, gw)
	p.gwIdx[gw] = i
	return i, nil
}

func (p *pool) gwAddr(i uint8) netip.Addr {
	p.gwMu.RLock()
	defer p.gwMu.RUnlock()
	return p.gws[i]
}

// addStatic registers a static prefix. Must be called before the pool is used
// concurrently (i.e. during Init). Same prefix with the same gateway is
// de-duplicated; with a different gateway it is an error.
func (p *pool) addStatic(prefix netip.Prefix, gw netip.Addr) error {
	prefix = prefix.Masked()
	gi, err := p.gateway(gw)
	if err != nil {
		return err
	}
	if idx, ok := p.staticExact[prefix]; ok {
		if p.static[idx].gw != gi {
			return fmt.Errorf("static route conflict for %s: gateway %s vs %s",
				prefix, p.gwAddr(p.static[idx].gw), gw)
		}
		return nil
	}
	p.staticExact[prefix] = len(p.static)
	p.static = append(p.static, staticEntry{prefix: prefix, gw: gi})
	l := p.staticByGw[gi]
	if l == nil {
		l = netlist.NewList()
		p.staticByGw[gi] = l
	}
	l.Append(prefix)
	p.staticAny.Append(prefix)
	return nil
}

// finishStatic sorts the static matchers. Call once after the last addStatic.
func (p *pool) finishStatic() {
	for _, l := range p.staticByGw {
		l.Sort()
	}
	p.staticAny.Sort()
}

// action is one API operation produced by Learn, executed outside the lock.
type action struct {
	kind   uint8 // actAdd / actSet
	e      *dynEntry
	gw     netip.Addr
	oldGw  uint8 // for actSet rollback
	oldSyn bool
}

const (
	actAdd uint8 = iota
	actSet
)

// Learn is the hot path: make sure every ip in ips has a route via gateway gi.
// It returns the pending actions (API calls) the caller must run via Apply.
// removeIDs are routes evicted to stay under max_routes.
func (p *pool) Learn(ips []netip.Addr, gi uint8) (acts []action, removeIDs []uint32) {
	for _, addr := range ips {
		if !addr.Is4() {
			continue
		}
		ip := binary.BigEndian.Uint32(addr.AsSlice())

		// 1. read-only fast path
		p.mu.RLock()
		e := p.dyn[ip]
		if e != nil && e.gw == gi && e.state == stateSynced {
			e.ref.Store(true)
			p.mu.RUnlock()
			continue
		}
		p.mu.RUnlock()

		// 2. static coverage (immutable, no lock)
		if l := p.staticByGw[gi]; l != nil && l.Contains(addr) {
			continue // already covered by a static CIDR to the same gateway
		}
		if p.staticAny.Contains(addr) {
			if p.warnConflict.allow() {
				p.logger.Warn("dynamic route ignored: covered by a static route with another gateway",
					zap.Stringer("ip", addr), zap.Stringer("gateway", p.gwAddr(gi)))
			}
			continue
		}

		// 3. write path
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			return acts, removeIDs
		}
		e = p.dyn[ip]
		switch {
		case e == nil:
			if len(p.dyn) >= p.maxDyn {
				removeIDs = p.evictLocked(removeIDs)
			}
			if len(p.dyn) >= p.maxDyn {
				// Hard cap: eviction found nothing removable (everything is
				// in flight or hot). Drop this request; it will be retried by
				// the next query for the same IP.
				if p.warnEvict.allow() {
					p.logger.Warn("max_routes reached and nothing evictable; route dropped", zap.Stringer("ip", addr))
				}
				break
			}
			// New entries start unreferenced: an IP that is learned once and
			// never resolved again is the first eviction candidate.
			e = &dynEntry{ip: ip, gw: gi}
			p.insertLocked(e)
			if p.be.Connected() {
				e.state = stateInflight
				acts = append(acts, action{kind: actAdd, e: e, gw: p.gwAddr(gi)})
			} else {
				e.state = stateOffline
				if p.warnOffline.allow() {
					p.logger.Warn("routeros api offline; route queued until reconnect", zap.Stringer("ip", addr))
				}
			}
		case e.gw == gi:
			// Same gateway. Inflight: someone else is adding, nothing to do.
			// Offline (a previous add/set failed on transport, or learned while
			// disconnected): retry now if the API is back.
			e.ref.Store(true)
			if e.state == stateOffline && p.be.Connected() {
				e.state = stateInflight
				acts = append(acts, p.retryAction(e))
			}
		case e.state == stateInflight:
			if p.warnConflict.allow() {
				p.logger.Warn("gateway conflict while add in flight; ignored",
					zap.Stringer("ip", addr), zap.Stringer("existing", p.gwAddr(e.gw)), zap.Stringer("requested", p.gwAddr(gi)))
			}
		default: // synced or offline with a different gateway
			if p.warnConflict.allow() {
				p.logger.Warn("gateway conflict",
					zap.Stringer("ip", addr), zap.Stringer("existing", p.gwAddr(e.gw)), zap.Stringer("requested", p.gwAddr(gi)),
					zap.String("policy", map[bool]string{true: "replace", false: "keep"}[p.replace]))
			}
			// The kept route is still the one in use: keep it warm for CLOCK.
			e.ref.Store(true)
			if p.replace {
				old, wasSynced := e.gw, e.state == stateSynced
				e.gw = gi
				if !p.be.Connected() {
					e.state = stateOffline // fixed by the reconnect sync
				} else if wasSynced {
					e.state = stateInflight
					acts = append(acts, action{kind: actSet, e: e, gw: p.gwAddr(gi), oldGw: old, oldSyn: true})
				} else {
					e.state = stateInflight
					acts = append(acts, p.retryAction(e))
				}
			}
		}
		p.mu.Unlock()
	}
	return acts, removeIDs
}

// retryAction builds the API call that brings an offline entry back in sync:
// an add if the router never gave us an id, otherwise a set on the existing
// route (a stale id means an earlier set failed on transport; re-adding would
// create a duplicate route). Caller holds mu and has set e.state=stateInflight.
func (p *pool) retryAction(e *dynEntry) action {
	if e.id == 0 {
		return action{kind: actAdd, e: e, gw: p.gwAddr(e.gw)}
	}
	return action{kind: actSet, e: e, gw: p.gwAddr(e.gw), oldGw: e.gw}
}

// insertLocked places e into dyn and the CLOCK ring. Caller holds mu.
func (p *pool) insertLocked(e *dynEntry) {
	p.dyn[e.ip] = e
	if n := len(p.free); n > 0 {
		e.slot = p.free[n-1]
		p.free = p.free[:n-1]
		p.clock[e.slot] = e
		return
	}
	e.slot = int32(len(p.clock))
	p.clock = append(p.clock, e)
}

// removeLocked drops e from dyn and frees its ring slot. Caller holds mu.
func (p *pool) removeLocked(e *dynEntry) {
	delete(p.dyn, e.ip)
	if int(e.slot) < len(p.clock) && p.clock[e.slot] == e {
		p.clock[e.slot] = nil
		p.free = append(p.free, e.slot)
	}
}

// evictLocked runs the CLOCK hand to free a small batch of entries. Returns
// removeIDs extended with the router ids of evicted synced entries.
func (p *pool) evictLocked(removeIDs []uint32) []uint32 {
	batch := max(1, min(evictBatchMax, p.maxDyn/100))
	evicted := 0
	n := len(p.clock)
	if n == 0 {
		return removeIDs
	}
	for steps := 0; steps < 2*n && evicted < batch; steps++ {
		e := p.clock[p.hand]
		p.hand = (p.hand + 1) % n
		if e == nil {
			continue
		}
		if e.ref.Load() {
			e.ref.Store(false)
			continue
		}
		if e.state == stateInflight {
			continue
		}
		if e.id != 0 { // synced, or offline with a stale id still on the router
			removeIDs = append(removeIDs, e.id)
		}
		p.removeLocked(e)
		evicted++
	}
	if evicted > 0 && p.warnEvict.allow() {
		p.logger.Warn("max_routes reached; evicting least recently used dynamic routes",
			zap.Int("max_routes", p.maxDyn), zap.Int("evicted", evicted))
	}
	return removeIDs
}

// Apply executes the actions produced by Learn and updates entry state when
// the router replies. It must be called outside the lock.
func (p *pool) Apply(acts []action, removeIDs []uint32) {
	for _, a := range acts {
		switch a.kind {
		case actAdd:
			id, err := p.be.AddRoute(netip.PrefixFrom(ipFromU32(a.e.ip), 32), a.gw)
			p.mu.Lock()
			if a.e.state == stateInflight && p.dyn[a.e.ip] == a.e {
				switch {
				case err == nil:
					a.e.id, a.e.state = id, stateSynced
				case errors.Is(err, errNotConnected) || !isDeviceError(err):
					a.e.state = stateOffline // transport problem: retry via sync
				default:
					p.removeLocked(a.e) // router rejected it; forget and retry on next query
				}
			}
			p.mu.Unlock()
			if err != nil {
				p.logger.Warn("add route failed", zap.Stringer("ip", ipFromU32(a.e.ip)), zap.Stringer("gateway", a.gw), zap.Error(err))
			}
		case actSet:
			err := p.be.SetGateway(a.e.id, a.gw)
			readd := false
			p.mu.Lock()
			if a.e.state == stateInflight && p.dyn[a.e.ip] == a.e {
				switch {
				case err == nil:
					a.e.state = stateSynced
				case !isDeviceError(err):
					a.e.state = stateOffline // transport: retried on next query / sync
				case a.oldSyn:
					a.e.gw, a.e.state = a.oldGw, stateSynced // conflict replace rejected; router kept the old gateway
				default:
					// Retry-set rejected: the route is gone from the router (e.g.
					// deleted by hand). Forget the stale id and re-add right now
					// so this query does not leave the IP without a route.
					a.e.id = 0
					readd = true
				}
			}
			p.mu.Unlock()
			if err != nil {
				p.logger.Warn("set gateway failed", zap.Stringer("ip", ipFromU32(a.e.ip)), zap.Stringer("gateway", a.gw), zap.Error(err))
			}
			if readd {
				p.Apply([]action{{kind: actAdd, e: a.e, gw: a.gw}}, nil)
			}
		}
	}
	if len(removeIDs) > 0 {
		if err := p.be.RemoveRoutes(removeIDs); err != nil {
			p.logger.Warn("remove evicted routes failed (will be cleaned by next sync)", zap.Int("count", len(removeIDs)), zap.Error(err))
		}
	}
}

// dropInflight undoes the pool side of actions that will never be executed
// (async queue full): adds are forgotten, sets are rolled back.
func (p *pool) dropInflight(acts []action) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, a := range acts {
		if p.dyn[a.e.ip] != a.e || a.e.state != stateInflight {
			continue
		}
		switch a.kind {
		case actAdd:
			p.removeLocked(a.e)
		case actSet:
			a.e.gw, a.e.state = a.oldGw, stateSynced
		}
	}
}

// Sync reconciles the router with the pool. See the plan: adopt matching
// routes, fix gateways, add missing ones, and either adopt or remove unknown
// ones depending on mode. Returns the summary; logs exactly one line.
func (p *pool) Sync(mode syncMode) syncResult {
	p.syncMu.Lock()
	defer p.syncMu.Unlock()

	res := syncResult{Time: time.Now()}
	remote, err := p.be.ListRoutes()
	if err != nil {
		res.Err = err.Error()
		p.syncFails.Add(1)
		p.lastSync.Store(&res)
		p.logger.Error("routeros sync: list routes failed", zap.Error(err))
		return res
	}
	res.Router = len(remote)

	type setOp struct {
		id uint32
		gw netip.Addr
		e  *dynEntry
		s  *staticEntry
	}
	var (
		sets      []setOp
		addStatic []*staticEntry
		addDyn    []*dynEntry
		removeIDs []uint32
		adoptDyn  []remoteRoute
	)
	seenStatic := make(map[int]bool)
	seenDyn := make(map[uint32]bool)

	p.mu.Lock()
	res.Pool = len(p.static) + len(p.dyn)
	for _, r := range remote {
		// static?
		if idx, ok := p.staticExact[r.dst]; ok {
			s := &p.static[idx]
			if seenStatic[idx] {
				removeIDs = append(removeIDs, r.id) // duplicate on router
				continue
			}
			seenStatic[idx] = true
			s.id, s.synced = r.id, true
			if r.gw != p.gwAddr(s.gw) {
				sets = append(sets, setOp{id: r.id, gw: p.gwAddr(s.gw), s: s})
			} else {
				res.Adopted++
			}
			continue
		}
		// dynamic /32?
		if r.dst.Bits() != 32 || !r.dst.Addr().Is4() {
			removeIDs = append(removeIDs, r.id)
			continue
		}
		ip := binary.BigEndian.Uint32(r.dst.Addr().AsSlice())
		e := p.dyn[ip]
		switch {
		case e == nil:
			// Adopt only when it is a plausible dynamic route: valid IP gateway,
			// not a duplicate, and not covered by a static CIDR (static wins).
			if mode == syncAdopt && r.gw.IsValid() && !seenDyn[ip] && !p.staticAny.Contains(r.dst.Addr()) {
				adoptDyn = append(adoptDyn, r)
				seenDyn[ip] = true
			} else {
				removeIDs = append(removeIDs, r.id)
			}
		case e.state == stateInflight:
			// its own goroutine will settle it; do not touch
			seenDyn[ip] = true
		case seenDyn[ip]:
			removeIDs = append(removeIDs, r.id) // duplicate on router
		default:
			seenDyn[ip] = true
			e.id, e.state = r.id, stateSynced
			if r.gw != p.gwAddr(e.gw) {
				e.state = stateInflight
				sets = append(sets, setOp{id: r.id, gw: p.gwAddr(e.gw), e: e})
			} else {
				res.Adopted++
			}
		}
	}
	// pool entries missing on the router
	for i := range p.static {
		if !seenStatic[i] {
			addStatic = append(addStatic, &p.static[i])
		}
	}
	for _, e := range p.dyn {
		if !seenDyn[e.ip] && e.state != stateInflight {
			e.state = stateInflight
			addDyn = append(addDyn, e)
		}
	}
	// adopt unknown /32s as dynamic (startup only)
	for _, r := range adoptDyn {
		gi, err := p.gateway(r.gw)
		if err != nil || len(p.dyn) >= p.maxDyn {
			removeIDs = append(removeIDs, r.id)
			continue
		}
		ip := binary.BigEndian.Uint32(r.dst.Addr().AsSlice())
		e := &dynEntry{ip: ip, id: r.id, gw: gi, state: stateSynced}
		p.insertLocked(e)
		res.Adopted++
	}
	p.mu.Unlock()

	// --- API calls outside the lock ---
	var failed bool
	for _, op := range sets {
		err := p.be.SetGateway(op.id, op.gw)
		if op.e != nil {
			p.mu.Lock()
			if p.dyn[op.e.ip] == op.e && op.e.state == stateInflight {
				if err == nil {
					op.e.state = stateSynced
				} else {
					op.e.state = stateOffline
				}
			}
			p.mu.Unlock()
		}
		if err != nil {
			failed = true
			p.logger.Debug("sync: set gateway failed", zap.Uint32("id", op.id), zap.Error(err))
		} else {
			res.Set++
		}
	}
	for _, s := range addStatic {
		id, err := p.be.AddRoute(s.prefix, p.gwAddr(s.gw))
		if err != nil {
			failed = true
			p.logger.Debug("sync: add static failed", zap.Stringer("dst", s.prefix), zap.Error(err))
			continue
		}
		p.mu.Lock()
		s.id, s.synced = id, true
		p.mu.Unlock()
		res.Added++
	}
	for _, e := range addDyn {
		id, err := p.be.AddRoute(netip.PrefixFrom(ipFromU32(e.ip), 32), p.gwAddr(e.gw))
		p.mu.Lock()
		if p.dyn[e.ip] == e && e.state == stateInflight {
			switch {
			case err == nil:
				e.id, e.state = id, stateSynced
			case isDeviceError(err):
				p.removeLocked(e)
			default:
				e.state = stateOffline
			}
		}
		p.mu.Unlock()
		if err != nil {
			failed = true
			p.logger.Debug("sync: add dynamic failed", zap.Stringer("ip", ipFromU32(e.ip)), zap.Error(err))
		} else {
			res.Added++
		}
	}
	if len(removeIDs) > 0 {
		// never remove an id some entry claimed meanwhile
		removeIDs = p.filterOwnedIDs(removeIDs)
		if err := p.be.RemoveRoutes(removeIDs); err != nil {
			failed = true
			p.logger.Debug("sync: remove failed", zap.Error(err))
		} else {
			res.Removed = len(removeIDs)
		}
	}

	if failed {
		res.Err = "some operations failed"
		p.syncFails.Add(1)
		p.logger.Error("routeros sync finished with errors", syncFields(res)...)
	} else if res.Set+res.Added+res.Removed > 0 {
		p.logger.Warn("routeros sync repaired", syncFields(res)...)
	} else {
		p.logger.Info("routeros sync ok", syncFields(res)...)
	}
	p.lastSync.Store(&res)
	return res
}

func syncFields(r syncResult) []zap.Field {
	return []zap.Field{
		zap.Int("router", r.Router), zap.Int("pool", r.Pool), zap.Int("adopted", r.Adopted),
		zap.Int("set", r.Set), zap.Int("added", r.Added), zap.Int("removed", r.Removed),
	}
}

// filterOwnedIDs drops ids currently held by any pool entry.
func (p *pool) filterOwnedIDs(ids []uint32) []uint32 {
	p.mu.RLock()
	defer p.mu.RUnlock()
	owned := make(map[uint32]bool, len(ids))
	for _, s := range p.static {
		if s.synced {
			owned[s.id] = true
		}
	}
	for _, e := range p.dyn {
		if e.id != 0 {
			owned[e.id] = true
		}
	}
	out := ids[:0]
	for _, id := range ids {
		if !owned[id] {
			out = append(out, id)
		}
	}
	return out
}

// Flush drops every dynamic entry and removes it from the router, then runs a
// clean sync to catch anything left over. Entries whose add/set is still in
// flight are kept: their goroutine will settle them and the following sync
// sees them as regular pool members instead of orphans on the router.
func (p *pool) Flush() syncResult {
	p.mu.Lock()
	ids := make([]uint32, 0, len(p.dyn))
	for _, e := range p.dyn {
		if e.state == stateInflight {
			continue
		}
		if e.id != 0 {
			ids = append(ids, e.id)
		}
		p.removeLocked(e)
	}
	p.mu.Unlock()
	if len(ids) > 0 {
		if err := p.be.RemoveRoutes(ids); err != nil {
			p.logger.Warn("flush: remove failed", zap.Error(err))
		}
	}
	return p.Sync(syncClean)
}

// Close marks the pool closed. If clean is true every route the plugin owns is
// removed from the router, with a final list-by-comment sweep.
func (p *pool) Close(clean bool) {
	p.mu.Lock()
	p.closed = true
	ids := make([]uint32, 0, len(p.dyn)+len(p.static))
	for _, s := range p.static {
		if s.synced {
			ids = append(ids, s.id)
		}
	}
	for _, e := range p.dyn {
		if e.id != 0 {
			ids = append(ids, e.id)
		}
	}
	p.mu.Unlock()
	if !clean {
		return
	}
	if !p.be.Connected() {
		p.logger.Warn("routeros offline at shutdown; routes left on router", zap.Int("count", len(ids)))
		return
	}
	if len(ids) > 0 {
		if err := p.be.RemoveRoutes(ids); err != nil {
			p.logger.Warn("shutdown: remove routes failed", zap.Error(err))
		}
	}
	// sweep anything we lost track of (e.g. inflight adds that completed late)
	remote, err := p.be.ListRoutes()
	if err != nil {
		p.logger.Warn("shutdown: final list failed", zap.Error(err))
		return
	}
	if len(remote) > 0 {
		left := make([]uint32, len(remote))
		for i, r := range remote {
			left[i] = r.id
		}
		if err := p.be.RemoveRoutes(left); err != nil {
			p.logger.Warn("shutdown: final sweep failed", zap.Error(err))
		}
	}
	p.logger.Info("routeros routes cleaned up", zap.Int("removed", len(ids)+len(remote)))
}

// --- introspection for the HTTP API ---

type routeView struct {
	Dst     string `json:"dst"`
	Gateway string `json:"gateway"`
	ID      string `json:"id,omitempty"`
	State   string `json:"state"`
	Static  bool   `json:"static,omitempty"`
}

func (p *pool) Routes() []routeView {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]routeView, 0, len(p.static)+len(p.dyn))
	for _, s := range p.static {
		v := routeView{Dst: s.prefix.String(), Gateway: p.gwAddr(s.gw).String(), Static: true, State: "offline"}
		if s.synced {
			v.ID, v.State = formatID(s.id), "synced"
		}
		out = append(out, v)
	}
	for _, e := range p.dyn {
		v := routeView{Dst: ipFromU32(e.ip).String(), Gateway: p.gwAddr(e.gw).String()}
		switch e.state {
		case stateSynced:
			v.ID, v.State = formatID(e.id), "synced"
		case stateInflight:
			v.State = "inflight"
		default:
			v.State = "offline"
		}
		out = append(out, v)
	}
	return out
}

type poolStats struct {
	Static       int         `json:"static"`
	Dynamic      int         `json:"dynamic"`
	Synced       int         `json:"dynamic_synced"`
	Inflight     int         `json:"dynamic_inflight"`
	Offline      int         `json:"dynamic_offline"`
	MaxRoutes    int         `json:"max_routes"`
	LastSync     *syncResult `json:"last_sync,omitempty"`
	SyncFailures int64       `json:"sync_failures"`
}

func (p *pool) Stats() poolStats {
	p.mu.RLock()
	st := poolStats{Static: len(p.static), Dynamic: len(p.dyn), MaxRoutes: p.maxDyn}
	for _, e := range p.dyn {
		switch e.state {
		case stateSynced:
			st.Synced++
		case stateInflight:
			st.Inflight++
		default:
			st.Offline++
		}
	}
	p.mu.RUnlock()
	st.LastSync = p.lastSync.Load()
	st.SyncFailures = p.syncFails.Load()
	return st
}

func ipFromU32(v uint32) netip.Addr {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], v)
	return netip.AddrFrom4(b)
}

// rateLimiter allows one event per warnRateLimit.
type rateLimiter struct{ last atomic.Int64 }

func (r *rateLimiter) allow() bool {
	now := time.Now().UnixNano()
	last := r.last.Load()
	if now-last < int64(warnRateLimit) {
		return false
	}
	return r.last.CompareAndSwap(last, now)
}
