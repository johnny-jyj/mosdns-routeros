package routeros

import (
	"errors"
	"net/netip"
	"sync"
	"testing"

	ros "github.com/go-routeros/routeros/v3"
	"github.com/go-routeros/routeros/v3/proto"
	"go.uber.org/zap"
)

// fakeBackend is an in-memory RouterOS route table.
type fakeBackend struct {
	mu        sync.Mutex
	connected bool
	nextID    uint32
	routes    map[uint32]remoteRoute
	adds      int
	sets      int
	removes   int // number of ids removed
	removeCmd int // number of RemoveRoutes calls
	addErr    error
}

func newFake() *fakeBackend {
	return &fakeBackend{connected: true, nextID: 1, routes: map[uint32]remoteRoute{}}
}

func (f *fakeBackend) Connected() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.connected }

func (f *fakeBackend) AddRoute(dst netip.Prefix, gw netip.Addr) (uint32, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.connected {
		return 0, errNotConnected
	}
	if f.addErr != nil {
		return 0, f.addErr
	}
	f.adds++
	id := f.nextID
	f.nextID++
	f.routes[id] = remoteRoute{id: id, dst: dst, gw: gw}
	return id, nil
}

func (f *fakeBackend) SetGateway(id uint32, gw netip.Addr) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.connected {
		return errNotConnected
	}
	r, ok := f.routes[id]
	if !ok {
		return deviceErr("no such item")
	}
	r.gw = gw
	f.routes[id] = r
	f.sets++
	return nil
}

func (f *fakeBackend) RemoveRoutes(ids []uint32) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.connected {
		return errNotConnected
	}
	f.removeCmd++
	for _, id := range ids {
		delete(f.routes, id)
		f.removes++
	}
	return nil
}

func (f *fakeBackend) ListRoutes() ([]remoteRoute, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.connected {
		return nil, errNotConnected
	}
	out := make([]remoteRoute, 0, len(f.routes))
	for _, r := range f.routes {
		out = append(out, r)
	}
	return out, nil
}

// inject adds a route directly to the "router" (as if created by someone else / a previous run).
func (f *fakeBackend) inject(dst string, gw string) uint32 {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := f.nextID
	f.nextID++
	f.routes[id] = remoteRoute{id: id, dst: netip.MustParsePrefix(dst), gw: netip.MustParseAddr(gw)}
	return id
}

func (f *fakeBackend) count() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.routes) }

func (f *fakeBackend) find(dst string) (remoteRoute, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := netip.MustParsePrefix(dst)
	for _, r := range f.routes {
		if r.dst == p {
			return r, true
		}
	}
	return remoteRoute{}, false
}

func deviceErr(msg string) error {
	s := proto.NewSentence()
	s.Word = "!trap"
	s.Map["message"] = msg
	return &ros.DeviceError{Sentence: s}
}

var (
	gwA = netip.MustParseAddr("192.168.88.254")
	gwB = netip.MustParseAddr("192.168.88.253")
)

func newTestPool(t *testing.T, be backend, maxDyn int, replace bool) (*pool, uint8, uint8) {
	t.Helper()
	p := newPool(be, zap.NewNop(), poolConfig{maxDyn: maxDyn, replace: replace})
	a, _ := p.gateway(gwA)
	b, _ := p.gateway(gwB)
	return p, a, b
}

func learn(p *pool, gw uint8, ips ...string) {
	addrs := make([]netip.Addr, len(ips))
	for i, s := range ips {
		addrs[i] = netip.MustParseAddr(s)
	}
	acts, rm := p.Learn(addrs, gw)
	p.Apply(acts, rm)
}

func TestLearnAddAndDedup(t *testing.T) {
	f := newFake()
	p, a, _ := newTestPool(t, f, 100, false)
	p.finishStatic()

	learn(p, a, "1.1.1.1", "1.1.1.1", "8.8.8.8")
	if f.adds != 2 || f.count() != 2 {
		t.Fatalf("adds=%d count=%d", f.adds, f.count())
	}
	// second time: fast path, no API
	acts, rm := p.Learn([]netip.Addr{netip.MustParseAddr("1.1.1.1")}, a)
	if len(acts) != 0 || len(rm) != 0 {
		t.Fatalf("expected no actions, got %d/%d", len(acts), len(rm))
	}
	st := p.Stats()
	if st.Dynamic != 2 || st.Synced != 2 {
		t.Fatalf("stats %+v", st)
	}
	r, _ := f.find("1.1.1.1/32")
	if r.gw != gwA {
		t.Fatalf("gw %v", r.gw)
	}
}

func TestInflightDedup(t *testing.T) {
	f := newFake()
	p, a, b := newTestPool(t, f, 100, true)
	p.finishStatic()

	ip := []netip.Addr{netip.MustParseAddr("1.1.1.1")}
	acts1, _ := p.Learn(ip, a) // inflight now
	if len(acts1) != 1 {
		t.Fatal("expected add")
	}
	acts2, _ := p.Learn(ip, a) // same gw while inflight -> nothing
	acts3, _ := p.Learn(ip, b) // different gw while inflight -> ignored even with replace
	if len(acts2) != 0 || len(acts3) != 0 {
		t.Fatalf("expected no actions while inflight: %d %d", len(acts2), len(acts3))
	}
	p.Apply(acts1, nil)
	if f.adds != 1 {
		t.Fatalf("adds=%d", f.adds)
	}
}

func TestStaticCoverage(t *testing.T) {
	f := newFake()
	p, a, b := newTestPool(t, f, 100, false)
	if err := p.addStatic(netip.MustParsePrefix("10.0.0.0/8"), gwA); err != nil {
		t.Fatal(err)
	}
	if err := p.addStatic(netip.MustParsePrefix("10.0.0.0/8"), gwA); err != nil {
		t.Fatal("dedupe should not error:", err)
	}
	if err := p.addStatic(netip.MustParsePrefix("10.0.0.0/8"), gwB); err == nil {
		t.Fatal("expected static conflict error")
	}
	p.finishStatic()
	p.Sync(syncClean) // installs the static route
	if f.adds != 1 {
		t.Fatalf("static add expected, adds=%d", f.adds)
	}
	learn(p, a, "10.1.2.3") // covered, same gw -> nothing
	learn(p, b, "10.1.2.4") // covered, other gw -> warning, nothing
	if f.adds != 1 || p.Stats().Dynamic != 0 {
		t.Fatalf("static coverage not honoured: adds=%d dyn=%d", f.adds, p.Stats().Dynamic)
	}
}

func TestConflictKeepAndReplace(t *testing.T) {
	for _, replace := range []bool{false, true} {
		f := newFake()
		p, a, b := newTestPool(t, f, 100, replace)
		p.finishStatic()
		learn(p, a, "1.1.1.1")
		learn(p, b, "1.1.1.1")
		r, _ := f.find("1.1.1.1/32")
		want := gwA
		wantSets := 0
		if replace {
			want = gwB
			wantSets = 1
		}
		if r.gw != want || f.sets != wantSets || f.adds != 1 {
			t.Fatalf("replace=%v gw=%v sets=%d adds=%d", replace, r.gw, f.sets, f.adds)
		}
	}
}

func TestEviction(t *testing.T) {
	f := newFake()
	p, a, _ := newTestPool(t, f, 10, false)
	p.finishStatic()
	for i := 0; i < 10; i++ {
		learn(p, a, netip.AddrFrom4([4]byte{1, 1, 1, byte(i)}).String())
	}
	// touch 1.1.1.0 so it is "recently used"
	learn(p, a, "1.1.1.0")
	learn(p, a, "2.2.2.2") // forces eviction
	st := p.Stats()
	if st.Dynamic > 10 {
		t.Fatalf("pool exceeded max: %d", st.Dynamic)
	}
	if f.removes == 0 || f.count() != st.Dynamic {
		t.Fatalf("removes=%d router=%d pool=%d", f.removes, f.count(), st.Dynamic)
	}
	if _, ok := f.find("1.1.1.0/32"); !ok {
		t.Fatal("recently referenced entry was evicted")
	}
	if _, ok := f.find("2.2.2.2/32"); !ok {
		t.Fatal("new entry missing")
	}
}

func TestOfflineThenSync(t *testing.T) {
	f := newFake()
	f.connected = false
	p, a, _ := newTestPool(t, f, 100, false)
	p.finishStatic()
	learn(p, a, "1.1.1.1", "2.2.2.2")
	st := p.Stats()
	if st.Offline != 2 || f.adds != 0 {
		t.Fatalf("expected 2 offline entries, got %+v adds=%d", st, f.adds)
	}
	f.mu.Lock()
	f.connected = true
	f.mu.Unlock()
	res := p.Sync(syncClean)
	if res.Added != 2 || f.count() != 2 || p.Stats().Synced != 2 {
		t.Fatalf("sync did not add offline entries: %+v count=%d", res, f.count())
	}
}

func TestAddRejectedByDevice(t *testing.T) {
	f := newFake()
	f.addErr = deviceErr("invalid gateway")
	p, a, _ := newTestPool(t, f, 100, false)
	p.finishStatic()
	learn(p, a, "1.1.1.1")
	if p.Stats().Dynamic != 0 {
		t.Fatal("rejected entry should be forgotten")
	}
	f.addErr = errors.New("transport")
	learn(p, a, "1.1.1.1")
	if p.Stats().Offline != 1 {
		t.Fatal("transport failure should leave entry offline for sync")
	}
}

func TestSyncAdoptAndClean(t *testing.T) {
	for _, mode := range []syncMode{syncAdopt, syncClean} {
		f := newFake()
		p, a, _ := newTestPool(t, f, 100, false)
		_ = p.addStatic(netip.MustParsePrefix("10.0.0.0/8"), gwA)
		_ = p.addStatic(netip.MustParsePrefix("172.16.0.0/12"), gwA)
		p.finishStatic()

		idStatic := f.inject("10.0.0.0/8", "192.168.88.254")     // matches -> adopt
		idWrongGw := f.inject("172.16.0.0/12", "192.168.88.253") // static, wrong gw -> set
		idDyn := f.inject("1.1.1.1/32", "192.168.88.254")        // leftover dynamic
		idCIDR := f.inject("9.9.9.0/24", "192.168.88.254")       // not ours -> remove

		res := p.Sync(mode)
		if res.Set != 1 || res.Removed == 0 {
			t.Fatalf("mode=%d res=%+v", mode, res)
		}
		if r := f.routes[idWrongGw]; r.gw != gwA {
			t.Fatalf("static gw not fixed: %v", r.gw)
		}
		if _, ok := f.routes[idCIDR]; ok {
			t.Fatal("foreign CIDR not removed")
		}
		if _, ok := f.routes[idStatic]; !ok {
			t.Fatal("static route removed")
		}
		_, dynKept := f.routes[idDyn]
		if mode == syncAdopt {
			if !dynKept || p.Stats().Synced != 1 {
				t.Fatalf("adopt: dyn kept=%v stats=%+v", dynKept, p.Stats())
			}
			// adopted entry is now a fast-path hit
			acts, _ := p.Learn([]netip.Addr{netip.MustParseAddr("1.1.1.1")}, a)
			if len(acts) != 0 {
				t.Fatal("adopted entry should not be re-added")
			}
		} else if dynKept || p.Stats().Dynamic != 0 {
			t.Fatalf("clean: dyn kept=%v stats=%+v", dynKept, p.Stats())
		}
	}
}

func TestSyncAddsMissingStatic(t *testing.T) {
	f := newFake()
	p, _, _ := newTestPool(t, f, 100, false)
	_ = p.addStatic(netip.MustParsePrefix("10.0.0.0/8"), gwA)
	p.finishStatic()
	res := p.Sync(syncClean)
	if res.Added != 1 || f.count() != 1 {
		t.Fatalf("%+v", res)
	}
	// second sync is a no-op
	res = p.Sync(syncClean)
	if res.Added+res.Set+res.Removed != 0 || res.Adopted != 1 {
		t.Fatalf("second sync not idempotent: %+v", res)
	}
}

func TestFlushAndClose(t *testing.T) {
	f := newFake()
	p, a, _ := newTestPool(t, f, 100, false)
	_ = p.addStatic(netip.MustParsePrefix("10.0.0.0/8"), gwA)
	p.finishStatic()
	p.Sync(syncClean)
	learn(p, a, "1.1.1.1", "2.2.2.2")
	if f.count() != 3 {
		t.Fatalf("count=%d", f.count())
	}
	p.Flush()
	if f.count() != 1 || p.Stats().Dynamic != 0 {
		t.Fatalf("flush: count=%d stats=%+v", f.count(), p.Stats())
	}
	learn(p, a, "3.3.3.3")
	p.Close(true)
	if f.count() != 0 {
		t.Fatalf("close(clean) left %d routes", f.count())
	}
	// after close, Learn is a no-op
	acts, _ := p.Learn([]netip.Addr{netip.MustParseAddr("4.4.4.4")}, a)
	if len(acts) != 0 {
		t.Fatal("learn after close")
	}
}

func TestCloseKeep(t *testing.T) {
	f := newFake()
	p, a, _ := newTestPool(t, f, 100, false)
	p.finishStatic()
	learn(p, a, "1.1.1.1")
	p.Close(false)
	if f.count() != 1 {
		t.Fatal("close(keep) removed routes")
	}
}

func TestRemoveBatching(t *testing.T) {
	f := newFake()
	p, a, _ := newTestPool(t, f, 2000, false)
	p.finishStatic()
	for i := 0; i < 1200; i++ {
		learn(p, a, netip.AddrFrom4([4]byte{10, byte(i >> 8), byte(i), 1}).String())
	}
	p.Flush()
	// fake counts commands, not chunks; pool hands all ids in one call and the
	// real client chunks by removeBatch. Just make sure everything is gone.
	if f.count() != 0 {
		t.Fatalf("left %d", f.count())
	}
}

func TestConcurrentLearn(t *testing.T) {
	f := newFake()
	p, a, b := newTestPool(t, f, 500, true)
	p.finishStatic()
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			gw := a
			if g%2 == 1 {
				gw = b
			}
			for i := 0; i < 300; i++ {
				learn(p, gw, netip.AddrFrom4([4]byte{10, 0, byte(i >> 8), byte(i)}).String())
			}
		}(g)
	}
	wg.Wait()
	st := p.Stats()
	if st.Inflight != 0 || st.Dynamic != 300 || f.count() != 300 {
		t.Fatalf("stats=%+v router=%d", st, f.count())
	}
}

// Review #1: an entry left offline by a transport error must be retried by the
// next query while the API is up, without waiting for a reconnect sync.
func TestOfflineRetriedOnNextLearn(t *testing.T) {
	f := newFake()
	p, a, _ := newTestPool(t, f, 100, false)
	p.finishStatic()
	f.addErr = errors.New("transport")
	learn(p, a, "1.1.1.1")
	if p.Stats().Offline != 1 || f.count() != 0 {
		t.Fatalf("setup: %+v", p.Stats())
	}
	f.addErr = nil
	learn(p, a, "1.1.1.1")
	if st := p.Stats(); st.Synced != 1 || st.Offline != 0 || f.count() != 1 {
		t.Fatalf("offline entry not retried: %+v router=%d", st, f.count())
	}
}

// Review #1b: an entry that is offline but still holds a router id (a set
// failed on transport) must be repaired with set, never re-added.
func TestOfflineWithStaleIDUsesSet(t *testing.T) {
	f := newFake()
	p, a, b := newTestPool(t, f, 100, true)
	p.finishStatic()
	learn(p, a, "1.1.1.1") // synced via A
	f.mu.Lock()
	f.connected = false
	f.mu.Unlock()
	// replace while disconnected -> offline with stale id, gw now B
	learn(p, b, "1.1.1.1")
	if st := p.Stats(); st.Offline != 1 {
		t.Fatalf("expected offline: %+v", st)
	}
	f.mu.Lock()
	f.connected = true
	f.mu.Unlock()
	learn(p, b, "1.1.1.1")
	if f.adds != 1 || f.sets != 1 || f.count() != 1 {
		t.Fatalf("expected one set, no duplicate add: adds=%d sets=%d count=%d", f.adds, f.sets, f.count())
	}
	if r, _ := f.find("1.1.1.1/32"); r.gw != gwB {
		t.Fatalf("gw=%v", r.gw)
	}
}

// A retry-set on a route that was deleted behind our back must re-add it in
// the same Apply, so the query that triggered it ends with a route in place.
func TestRetrySetOnDeletedRouteReadds(t *testing.T) {
	f := newFake()
	p, a, _ := newTestPool(t, f, 100, false)
	p.finishStatic()
	learn(p, a, "1.1.1.1")
	r, _ := f.find("1.1.1.1/32")
	// leave the entry offline with a stale id, then delete the route on the router
	f.mu.Lock()
	f.connected = false
	f.mu.Unlock()
	p.mu.Lock()
	p.dyn[0x01010101].state = stateOffline
	p.mu.Unlock()
	f.mu.Lock()
	delete(f.routes, r.id)
	f.connected = true
	f.mu.Unlock()

	learn(p, a, "1.1.1.1") // retry -> set fails (no such item) -> immediate add
	if st := p.Stats(); st.Synced != 1 || f.count() != 1 || f.adds != 2 {
		t.Fatalf("stats=%+v count=%d adds=%d", st, f.count(), f.adds)
	}
}

// Review #3: max_routes is a hard cap even when nothing is evictable.
func TestMaxRoutesHardCap(t *testing.T) {
	f := newFake()
	p, a, _ := newTestPool(t, f, 3, false)
	p.finishStatic()
	// fill with inflight entries (never applied) so eviction cannot free anything
	for _, ip := range []string{"1.1.1.1", "1.1.1.2", "1.1.1.3"} {
		p.Learn([]netip.Addr{netip.MustParseAddr(ip)}, a)
	}
	acts, _ := p.Learn([]netip.Addr{netip.MustParseAddr("1.1.1.4")}, a)
	if len(acts) != 0 || p.Stats().Dynamic != 3 {
		t.Fatalf("cap exceeded: acts=%d dyn=%d", len(acts), p.Stats().Dynamic)
	}
}

// Review #4: a keep-conflict hit still marks the kept route as recently used.
func TestKeepConflictKeepsRouteWarm(t *testing.T) {
	f := newFake()
	p, a, b := newTestPool(t, f, 4, false)
	p.finishStatic()
	learn(p, a, "1.1.1.1", "1.1.1.2", "1.1.1.3", "1.1.1.4")
	learn(p, b, "1.1.1.1") // conflict on .1 via other gateway -> must set ref
	learn(p, a, "2.2.2.2") // forces eviction of one cold entry
	if _, ok := f.find("1.1.1.1/32"); !ok {
		t.Fatal("kept route was evicted although it was just referenced")
	}
}

// #8: flush must not orphan routes whose add is still in flight.
func TestFlushKeepsInflight(t *testing.T) {
	f := newFake()
	p, a, _ := newTestPool(t, f, 100, false)
	p.finishStatic()
	learn(p, a, "1.1.1.1")
	acts, _ := p.Learn([]netip.Addr{netip.MustParseAddr("2.2.2.2")}, a) // inflight, not applied yet
	p.Flush()
	p.Apply(acts, nil) // the late add completes
	if st := p.Stats(); st.Dynamic != 1 || st.Synced != 1 {
		t.Fatalf("inflight entry lost: %+v", st)
	}
	if f.count() != 1 {
		t.Fatalf("router has %d routes, want 1", f.count())
	}
	if _, ok := f.find("2.2.2.2/32"); !ok {
		t.Fatal("inflight route missing")
	}
}

// #10: residual /32 covered by a static CIDR is removed, not adopted.
func TestAdoptSkipsStaticCovered(t *testing.T) {
	f := newFake()
	p, _, _ := newTestPool(t, f, 100, false)
	_ = p.addStatic(netip.MustParsePrefix("10.0.0.0/8"), gwA)
	p.finishStatic()
	f.inject("10.0.0.0/8", "192.168.88.254")
	f.inject("10.1.1.1/32", "192.168.88.253") // conflicts with static
	f.inject("1.1.1.1/32", "192.168.88.254")  // genuine leftover
	res := p.Sync(syncAdopt)
	if res.Removed != 1 || p.Stats().Dynamic != 1 || f.count() != 2 {
		t.Fatalf("res=%+v stats=%+v count=%d", res, p.Stats(), f.count())
	}
}

// #11: eviction and close must remove routes of offline entries that still
// hold a router id.
func TestCloseRemovesStaleIDs(t *testing.T) {
	f := newFake()
	p, a, b := newTestPool(t, f, 100, true)
	p.finishStatic()
	learn(p, a, "1.1.1.1")
	f.mu.Lock()
	f.connected = false
	f.mu.Unlock()
	learn(p, b, "1.1.1.1") // offline with stale id
	f.mu.Lock()
	f.connected = true
	f.mu.Unlock()
	p.Close(true)
	if f.count() != 0 {
		t.Fatalf("stale-id route left on router: %d", f.count())
	}
}

func TestIDHelpers(t *testing.T) {
	for _, s := range []string{"*1", "*1A3F", "*FFFFFFFF"} {
		id, err := parseID(s)
		if err != nil {
			t.Fatal(err)
		}
		back, err := parseID(formatID(id))
		if err != nil || back != id {
			t.Fatalf("roundtrip %s -> %d -> %s", s, id, formatID(id))
		}
	}
	if _, err := parseID(""); err == nil {
		t.Fatal("empty id accepted")
	}
	if p, _ := parseDst("1.2.3.4"); p != netip.MustParsePrefix("1.2.3.4/32") {
		t.Fatal(p)
	}
	if p, _ := parseDst("1.2.3.4/24"); p != netip.MustParsePrefix("1.2.3.0/24") {
		t.Fatal(p)
	}
}
