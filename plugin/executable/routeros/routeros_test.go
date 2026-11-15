package routeros

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/IrineSistiana/mosdns/v5/pkg/query_context"
	"github.com/miekg/dns"
	"go.uber.org/zap"
)

func TestLoadPrefixes(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "ips.txt")
	if err := os.WriteFile(f, []byte("# comment\n1.1.1.1\n10.0.0.0/8 # trailing\n\n2001:db8::/32\n192.168.1.5/24\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := loadPrefixes(zap.NewNop(), []string{"8.8.8.8", "::1"}, []string{f})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"8.8.8.8/32", "1.1.1.1/32", "10.0.0.0/8", "192.168.1.0/24"}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for i, w := range want {
		if got[i] != netip.MustParsePrefix(w) {
			t.Fatalf("#%d got %v want %v", i, got[i], w)
		}
	}
	if _, err := loadPrefixes(zap.NewNop(), []string{"not-an-ip"}, nil); err == nil {
		t.Fatal("expected error")
	}
	if _, err := loadPrefixes(zap.NewNop(), nil, []string{filepath.Join(dir, "missing")}); err == nil {
		t.Fatal("expected error for missing file")
	}
	tooLong := filepath.Join(dir, "too-long")
	if err := os.WriteFile(tooLong, []byte(strings.Repeat("1", 70<<10)+"\n1.1.1.1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadPrefixes(zap.NewNop(), nil, []string{tooLong}); err == nil {
		t.Fatal("scanner error was silently ignored")
	}
}

func TestParseGateway(t *testing.T) {
	ok := []string{"192.168.88.1", " 10.0.0.1 "}
	bad := []string{"", "0.0.0.0", "127.0.0.1", "255.255.255.255", "224.0.0.1", "169.254.1.1", "fe80::1", "2001:db8::1", "1.2.3", "eth0"}
	for _, s := range ok {
		if _, err := parseGateway(s); err != nil {
			t.Errorf("%q: %v", s, err)
		}
	}
	for _, s := range bad {
		if _, err := parseGateway(s); err == nil {
			t.Errorf("%q accepted", s)
		}
	}
}

func TestNormalizeAddress(t *testing.T) {
	tests := []struct {
		in   string
		tls  bool
		want string
	}{
		{"192.168.88.1", false, "192.168.88.1:8728"},
		{"router.example", true, "router.example:8729"},
		{"router.example:9000", false, "router.example:9000"},
		{"2001:db8::1", false, "[2001:db8::1]:8728"},
		{"[2001:db8::1]:9000", true, "[2001:db8::1]:9000"},
	}
	for _, tt := range tests {
		got, err := normalizeAddress(tt.in, tt.tls)
		if err != nil || got != tt.want {
			t.Errorf("normalizeAddress(%q): got %q, %v; want %q", tt.in, got, err, tt.want)
		}
	}
	for _, bad := range []string{"", "host:bad", "host:70000", "bad host", "bad/name", "bad/name:8728", ":8728"} {
		if got, err := normalizeAddress(bad, false); err == nil {
			t.Errorf("normalizeAddress(%q) accepted as %q", bad, got)
		}
	}
}

// Review #2: the first successful connection applies on_start (adopt), later
// ones clean. startupDiag is skipped here because cl is nil.
func TestOnConnectedFirstTimeAdopts(t *testing.T) {
	f := newFake()
	p, _, _ := newTestPool(t, f, 100, false)
	p.finishStatic()
	f.inject("1.1.1.1/32", "192.168.88.254")
	r := &RouterOS{logger: zap.NewNop(), pool: p, startMode: syncAdopt, paused: true}

	// simulate: first connect happened late (startup dial failed) -> must adopt
	r.onConnected()
	if p.Stats().Dynamic != 1 || f.count() != 1 {
		t.Fatalf("first sync did not adopt: %+v", p.Stats())
	}
	// a later reconnect with an unknown route on the router -> clean
	r.paused = false
	f.inject("2.2.2.2/32", "192.168.88.254")
	r.onConnected()
	if p.Stats().Dynamic != 1 || f.count() != 1 {
		t.Fatalf("reconnect sync did not clean: %+v count=%d", p.Stats(), f.count())
	}
}

func TestOnConnectedRetriesInitialAdoptAfterListFailure(t *testing.T) {
	f := newFake()
	p, _, _ := newTestPool(t, f, 100, false)
	p.finishStatic()
	r := &RouterOS{logger: zap.NewNop(), pool: p, startMode: syncAdopt, paused: true}

	f.mu.Lock()
	f.connected = false
	f.mu.Unlock()
	r.onConnected()
	if r.firstSynced {
		t.Fatal("failed initial list consumed adopt mode")
	}

	f.mu.Lock()
	f.connected = true
	f.mu.Unlock()
	f.inject("1.1.1.1/32", gwA.String())
	r.onConnected()
	if !r.firstSynced || p.Stats().Dynamic != 1 || f.count() != 1 {
		t.Fatalf("retry did not adopt: first=%v stats=%+v count=%d", r.firstSynced, p.Stats(), f.count())
	}
}

func TestOnConnectedDoesNotCompleteAfterRepairTrap(t *testing.T) {
	f := newFake()
	f.addErr = deviceErr("not enough permissions")
	p, _, _ := newTestPool(t, f, 100, false)
	if err := p.addStatic(netip.MustParsePrefix("10.0.0.0/8"), gwA); err != nil {
		t.Fatal(err)
	}
	p.finishStatic()
	r := &RouterOS{logger: zap.NewNop(), pool: p, startMode: syncAdopt, paused: true}

	res := r.onConnected()
	if res.Err == "" || !isTrap(res.cause) {
		t.Fatalf("sync result did not retain trap: %+v cause=%v", res, res.cause)
	}
	if r.firstSynced {
		t.Fatal("failed repair was marked as the completed initial sync")
	}
}

// #7: Exec in async mode must not panic when racing with Close.
func TestAsyncExecAfterClose(t *testing.T) {
	f := newFake()
	p, a, _ := newTestPool(t, f, 100, false)
	p.finishStatic()
	r := &RouterOS{
		logger: zap.NewNop(), cl: newClient(clientConfig{}, zap.NewNop()),
		pool: p, defGw: a, wait: false, timeout: time.Second, onExit: "keep",
		queue: make(chan func(), 4), queueSlots: make(chan struct{}, 4),
	}
	r.workerWG.Add(1)
	go r.worker()

	q := new(dns.Msg)
	q.SetQuestion("example.com.", dns.TypeA)
	mk := func(ip byte) *query_context.Context {
		resp := new(dns.Msg)
		resp.SetReply(q)
		resp.Answer = []dns.RR{&dns.A{Hdr: dns.RR_Header{Name: "example.com.", Rrtype: dns.TypeA, Class: dns.ClassINET}, A: []byte{1, 1, 1, ip}}}
		qc := query_context.NewContext(q)
		qc.SetResponse(resp)
		return qc
	}
	_ = r.Exec(context.Background(), mk(1))

	// Close while another goroutine keeps calling Exec.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 2; i < 60; i++ {
			_ = r.Exec(context.Background(), mk(byte(i)))
		}
	}()
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	<-done
	if st := p.Stats(); st.Inflight != 0 {
		t.Fatalf("inflight entries left after close: %+v", st)
	}
}

func TestExecExtractsARecords(t *testing.T) {
	f := newFake()
	p, a, b := newTestPool(t, f, 100, false)
	p.finishStatic()
	r := &RouterOS{logger: zap.NewNop(), pool: p, defGw: a, wait: true, timeout: time.Second}

	q := new(dns.Msg)
	q.SetQuestion("example.com.", dns.TypeA)
	resp := new(dns.Msg)
	resp.SetReply(q)
	resp.Answer = append(resp.Answer,
		&dns.A{Hdr: dns.RR_Header{Name: "example.com.", Rrtype: dns.TypeA, Class: dns.ClassINET}, A: []byte{1, 1, 1, 1}},
		&dns.AAAA{Hdr: dns.RR_Header{Name: "example.com.", Rrtype: dns.TypeAAAA, Class: dns.ClassINET}, AAAA: make([]byte, 16)},
		&dns.CNAME{Hdr: dns.RR_Header{Name: "example.com.", Rrtype: dns.TypeCNAME, Class: dns.ClassINET}, Target: "x."},
	)
	qc := query_context.NewContext(q)
	qc.SetResponse(resp)

	if err := r.Exec(context.Background(), qc); err != nil {
		t.Fatal(err)
	}
	if f.adds != 1 {
		t.Fatalf("adds=%d", f.adds)
	}
	rt, ok := f.find("1.1.1.1/32")
	if !ok || rt.gw != gwA {
		t.Fatalf("route %+v ok=%v", rt, ok)
	}

	// $tag <gw> form
	ex, err := r.QuickConfigureExec(gwB.String())
	if err != nil {
		t.Fatal(err)
	}
	resp.Answer = []dns.RR{&dns.A{Hdr: dns.RR_Header{Name: "example.com.", Rrtype: dns.TypeA, Class: dns.ClassINET}, A: []byte{2, 2, 2, 2}}}
	if err := ex.(*gwExec).Exec(context.Background(), qc); err != nil {
		t.Fatal(err)
	}
	rt, _ = f.find("2.2.2.2/32")
	if rt.gw != gwB {
		t.Fatalf("gw %v", rt.gw)
	}
	_ = b

	if _, err := r.QuickConfigureExec("not-an-ip"); err == nil {
		t.Fatal("bad gateway accepted")
	}
	// nil response is a no-op
	if err := r.Exec(context.Background(), query_context.NewContext(q)); err != nil {
		t.Fatal(err)
	}
}

func TestAsyncQueuedWorkDoesNotHoldReconcileBarrier(t *testing.T) {
	f := newFake()
	p, a, _ := newTestPool(t, f, 100, false)
	p.finishStatic()
	r := &RouterOS{
		logger: zap.NewNop(), pool: p, defGw: a, wait: false,
		queue: make(chan func(), 1), queueSlots: make(chan struct{}, 1),
	}
	if err := r.Exec(context.Background(), queryWithA(1)); err != nil {
		t.Fatal(err)
	}

	done := make(chan syncResult, 1)
	go func() { done <- p.Sync(syncClean) }()
	select {
	case res := <-done:
		if res.Err != "" {
			t.Fatalf("sync failed: %+v", res)
		}
	case <-time.After(time.Second):
		t.Fatal("queued async work held the reconciliation barrier before execution")
	}

	(<-r.queue)()
	r.opsWG.Wait()
	if f.count() != 1 {
		t.Fatalf("queued update produced %d routes", f.count())
	}
}

func TestAsyncQueueFullDoesNotMutatePool(t *testing.T) {
	f := newFake()
	p, a, _ := newTestPool(t, f, 100, false)
	p.finishStatic()
	r := &RouterOS{
		logger: zap.NewNop(), pool: p, defGw: a, wait: false, timeout: time.Second,
		queue: make(chan func(), 1), queueSlots: make(chan struct{}, 1),
	}
	r.queueSlots <- struct{}{} // reserve the only slot
	if err := r.Exec(context.Background(), queryWithA(1)); err != nil {
		t.Fatal(err)
	}
	if st := p.Stats(); st.Dynamic != 0 || st.Inflight != 0 {
		t.Fatalf("queue-full Exec mutated pool: %+v", st)
	}
}

type blockingBackend struct {
	*fakeBackend
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *blockingBackend) AddRoute(dst netip.Prefix, gw netip.Addr) (uint32, error) {
	b.once.Do(func() { close(b.started) })
	<-b.release
	return b.fakeBackend.AddRoute(dst, gw)
}

type reconnectDuringFlushBackend struct {
	*fakeBackend
	reconnect func()
	listOnce  sync.Once
	added     chan struct{}
	addOnce   sync.Once
}

func (b *reconnectDuringFlushBackend) ListRoutes() ([]remoteRoute, error) {
	first := false
	b.listOnce.Do(func() { first = true })
	if first {
		// Model the connection becoming usable after Flush started, but before
		// its failed list command returned and the pause was released.
		b.mu.Lock()
		b.connected = true
		b.mu.Unlock()
		b.reconnect()
		return nil, errNotConnected
	}
	return b.fakeBackend.ListRoutes()
}

func (b *reconnectDuringFlushBackend) AddRoute(dst netip.Prefix, gw netip.Addr) (uint32, error) {
	id, err := b.fakeBackend.AddRoute(dst, gw)
	if err == nil {
		b.addOnce.Do(func() { close(b.added) })
	}
	return id, err
}

func TestFlushReplaysReconnectReceivedWhilePaused(t *testing.T) {
	be := &reconnectDuringFlushBackend{
		fakeBackend: newFake(),
		added:       make(chan struct{}),
	}
	be.connected = false
	p := newPool(be, zap.NewNop(), poolConfig{maxDyn: 100})
	if err := p.addStatic(netip.MustParsePrefix("10.0.0.0/8"), gwA); err != nil {
		t.Fatal(err)
	}
	p.finishStatic()
	r := &RouterOS{
		logger: zap.NewNop(), pool: p, startMode: syncClean, firstSynced: true,
	}
	be.reconnect = r.scheduleConnected

	res := r.flush()
	if res.Err == "" {
		t.Fatal("setup did not make the Flush reconciliation fail")
	}
	select {
	case <-be.added:
	case <-time.After(time.Second):
		t.Fatal("reconnect received during Flush was not replayed")
	}
	r.opsWG.Wait()
	if _, ok := be.find("10.0.0.0/8"); !ok {
		t.Fatal("replayed reconnect did not restore the static route")
	}
}

func TestCloseWaitsForWaitModeApply(t *testing.T) {
	be := &blockingBackend{fakeBackend: newFake(), started: make(chan struct{}), release: make(chan struct{})}
	p := newPool(be, zap.NewNop(), poolConfig{maxDyn: 100})
	a, _ := p.gateway(gwA)
	p.finishStatic()
	r := &RouterOS{
		logger: zap.NewNop(), cl: newClient(clientConfig{}, zap.NewNop()), pool: p,
		defGw: a, wait: true, timeout: 5 * time.Millisecond, onExit: "clean",
	}
	if err := r.Exec(context.Background(), queryWithA(1)); err != nil {
		t.Fatal(err)
	}
	<-be.started

	closed := make(chan struct{})
	go func() {
		_ = r.Close()
		close(closed)
	}()
	select {
	case <-closed:
		t.Fatal("Close returned before inflight Apply completed")
	case <-time.After(20 * time.Millisecond):
	}
	close(be.release)
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("Close did not finish after Apply was released")
	}
	if be.count() != 0 {
		t.Fatalf("Close(clean) left %d routes", be.count())
	}
}

func TestFlushWaitsForInflightApply(t *testing.T) {
	be := &blockingBackend{fakeBackend: newFake(), started: make(chan struct{}), release: make(chan struct{})}
	p := newPool(be, zap.NewNop(), poolConfig{maxDyn: 100})
	a, _ := p.gateway(gwA)
	p.finishStatic()
	r := &RouterOS{
		logger: zap.NewNop(), pool: p, defGw: a, wait: true, timeout: 5 * time.Millisecond,
	}
	if err := r.Exec(context.Background(), queryWithA(1)); err != nil {
		t.Fatal(err)
	}
	<-be.started

	done := make(chan syncResult, 1)
	go func() { done <- r.flush() }()
	select {
	case <-done:
		t.Fatal("Flush returned before inflight Apply completed")
	case <-time.After(20 * time.Millisecond):
	}
	close(be.release)
	select {
	case res := <-done:
		if res.Err != "" {
			t.Fatalf("flush result: %+v", res)
		}
	case <-time.After(time.Second):
		t.Fatal("Flush did not finish after Apply was released")
	}
	if be.count() != 0 || p.Stats().Dynamic != 0 {
		t.Fatalf("flush left router=%d pool=%+v", be.count(), p.Stats())
	}
}

func queryWithA(ip byte) *query_context.Context {
	q := new(dns.Msg)
	q.SetQuestion("example.com.", dns.TypeA)
	resp := new(dns.Msg)
	resp.SetReply(q)
	resp.Answer = []dns.RR{&dns.A{
		Hdr: dns.RR_Header{Name: "example.com.", Rrtype: dns.TypeA, Class: dns.ClassINET},
		A:   []byte{1, 1, 1, ip},
	}}
	qc := query_context.NewContext(q)
	qc.SetResponse(resp)
	return qc
}

func TestRouterOSAPIAuthorization(t *testing.T) {
	newRouter := func(token string) *RouterOS {
		f := newFake()
		p, _, _ := newTestPool(t, f, 100, false)
		p.finishStatic()
		return &RouterOS{
			logger:   zap.NewNop(),
			cl:       newClient(clientConfig{}, zap.NewNop()),
			pool:     p,
			wait:     true,
			apiToken: token,
		}
	}
	request := func(r *RouterOS, method, path, remote, token, confirm string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, nil)
		req.RemoteAddr = remote
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		if confirm != "" {
			req.Header.Set("X-Mosdns-Confirm", confirm)
		}
		rr := httptest.NewRecorder()
		r.api().ServeHTTP(rr, req)
		return rr
	}
	requestAuth := func(r *RouterOS, auth string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/routes", nil)
		req.RemoteAddr = "192.0.2.10:1234"
		req.Header.Set("Authorization", auth)
		rr := httptest.NewRecorder()
		r.api().ServeHTTP(rr, req)
		return rr
	}

	r := newRouter("")
	if rr := request(r, http.MethodGet, "/routes", "192.0.2.10:1234", "", ""); rr.Code != http.StatusForbidden {
		t.Fatalf("remote request without token status=%d", rr.Code)
	}
	if rr := request(r, http.MethodGet, "/routes", "127.0.0.1:1234", "", ""); rr.Code != http.StatusOK {
		t.Fatalf("loopback request status=%d", rr.Code)
	}
	if rr := request(r, http.MethodPost, "/flush", "127.0.0.1:1234", "", ""); rr.Code != http.StatusForbidden {
		t.Fatalf("unconfirmed flush status=%d", rr.Code)
	}
	if rr := request(r, http.MethodPost, "/flush", "127.0.0.1:1234", "", "flush"); rr.Code != http.StatusOK {
		t.Fatalf("confirmed flush status=%d body=%s", rr.Code, rr.Body.String())
	}

	r = newRouter("secret")
	if rr := request(r, http.MethodGet, "/routes", "192.0.2.10:1234", "", ""); rr.Code != http.StatusUnauthorized {
		t.Fatalf("missing bearer status=%d", rr.Code)
	}
	if rr := request(r, http.MethodGet, "/routes", "192.0.2.10:1234", "wrong", ""); rr.Code != http.StatusUnauthorized {
		t.Fatalf("wrong bearer status=%d", rr.Code)
	}
	if rr := request(r, http.MethodGet, "/routes", "192.0.2.10:1234", "secret", ""); rr.Code != http.StatusOK {
		t.Fatalf("valid bearer status=%d", rr.Code)
	}
	if rr := requestAuth(r, "bearer   secret"); rr.Code != http.StatusOK {
		t.Fatalf("case-insensitive bearer status=%d", rr.Code)
	}
}
