package routeros

import (
	"context"
	"net/netip"
	"os"
	"path/filepath"
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

// Review #2: the first successful connection applies on_start (adopt), later
// ones clean. startupDiag is skipped here because cl is nil.
func TestOnConnectedFirstTimeAdopts(t *testing.T) {
	f := newFake()
	p, _, _ := newTestPool(t, f, 100, false)
	p.finishStatic()
	f.inject("1.1.1.1/32", "192.168.88.254")
	r := &RouterOS{logger: zap.NewNop(), pool: p, startMode: syncAdopt}
	r.firstSynced.Store(false)

	// simulate: first connect happened late (startup dial failed) -> must adopt
	r.pool.Sync(r.startMode)
	r.firstSynced.Store(true)
	if p.Stats().Dynamic != 1 || f.count() != 1 {
		t.Fatalf("first sync did not adopt: %+v", p.Stats())
	}
	// a later reconnect with an unknown route on the router -> clean
	f.inject("2.2.2.2/32", "192.168.88.254")
	r.onConnected()
	if p.Stats().Dynamic != 1 || f.count() != 1 {
		t.Fatalf("reconnect sync did not clean: %+v count=%d", p.Stats(), f.count())
	}
}

// #7: Exec in async mode must not panic when racing with Close.
func TestAsyncExecAfterClose(t *testing.T) {
	f := newFake()
	p, a, _ := newTestPool(t, f, 100, false)
	p.finishStatic()
	r := &RouterOS{logger: zap.NewNop(), pool: p, defGw: a, wait: false, timeout: time.Second, queue: make(chan func(), 4)}
	r.wg.Add(1)
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
	r.closeMu.Lock()
	r.closed = true
	close(r.queue)
	r.closeMu.Unlock()
	r.wg.Wait()
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
