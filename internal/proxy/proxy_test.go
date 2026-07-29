package proxy

import (
	"context"
	"encoding/binary"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/chinmay28/hole-balancer/internal/config"
	"github.com/chinmay28/hole-balancer/internal/dnsclient"
	"github.com/chinmay28/hole-balancer/internal/dnsmsg"
	"github.com/chinmay28/hole-balancer/internal/metrics"
	"github.com/chinmay28/hole-balancer/internal/pool"
	"github.com/chinmay28/hole-balancer/internal/testdns"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// newTestServer wires a proxy in front of the given fake upstreams, with every
// endpoint marked healthy.
func newTestServer(t *testing.T, tune func(*config.Config), upstreams ...*testdns.Server) (*Server, *pool.Pool, *metrics.Metrics) {
	t.Helper()

	cfg := config.Default()
	cfg.Listen = config.Listen{UDP: "127.0.0.1:0", TCP: "127.0.0.1:0"}
	cfg.Query.Timeout = config.Duration(300 * time.Millisecond)
	cfg.Admin.Listen = ""
	for i, u := range upstreams {
		cfg.Upstreams = append(cfg.Upstreams, config.Upstream{
			Name:      string(rune('a'+i)) + "-pihole",
			Weight:    1,
			Endpoints: []config.Endpoint{{Name: "lan", Addr: u.Addr()}},
		})
	}
	if tune != nil {
		tune(&cfg)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("test config is invalid: %v", err)
	}

	m := metrics.New()
	p := pool.New(&cfg, nil)
	for _, e := range p.Endpoints() {
		p.SetInitial(e, true, time.Millisecond, nil)
	}
	return New(&cfg, p, m, discardLogger()), p, m
}

func mustQuery(t *testing.T, name string) []byte {
	t.Helper()
	q, err := dnsmsg.BuildQuery(0x4242, name, dnsmsg.TypeA)
	if err != nil {
		t.Fatalf("BuildQuery: %v", err)
	}
	return q
}

func TestResolveForwardsAndReturnsUpstreamAnswer(t *testing.T) {
	up := testdns.Start(t)
	s, _, m := newTestServer(t, nil, up)

	resp := s.Resolve(context.Background(), mustQuery(t, "example.com."), dnsclient.ProtoUDP)
	if resp == nil {
		t.Fatal("Resolve returned nil for a healthy upstream")
	}
	hdr, err := dnsmsg.ParseHeader(resp)
	if err != nil {
		t.Fatalf("ParseHeader: %v", err)
	}
	if hdr.ID != 0x4242 {
		t.Errorf("transaction id = %#x, want 0x4242", hdr.ID)
	}
	if hdr.RCode != dnsmsg.RCodeNoError {
		t.Errorf("rcode = %v, want NOERROR", hdr.RCode)
	}
	if up.Queries() != 1 {
		t.Errorf("upstream saw %d queries, want 1", up.Queries())
	}
	if got := m.Queries.Value(metrics.Label{Name: "proto", Value: "udp"}); got != 1 {
		t.Errorf("query metric = %d, want 1", got)
	}
}

// The headline requirement: when a Pi-hole stops answering, clients keep
// resolving.
func TestResolveFailsOverToHealthyUpstream(t *testing.T) {
	dead, alive := testdns.Start(t), testdns.Start(t)
	dead.SetDrop(true)
	s, _, _ := newTestServer(t, nil, dead, alive)

	for i := 0; i < 6; i++ {
		resp := s.Resolve(context.Background(), mustQuery(t, "example.com."), dnsclient.ProtoUDP)
		if resp == nil {
			t.Fatalf("attempt %d: no response", i)
		}
		hdr, _ := dnsmsg.ParseHeader(resp)
		if hdr.RCode != dnsmsg.RCodeNoError {
			t.Fatalf("attempt %d: rcode = %v, want NOERROR", i, hdr.RCode)
		}
	}
	if alive.Queries() == 0 {
		t.Error("the healthy upstream never received a query")
	}
}

// Repeated timeouts on live traffic should take an endpoint out of rotation
// without waiting for the next active probe.
func TestPassiveHealthTakesEndpointDown(t *testing.T) {
	dead, alive := testdns.Start(t), testdns.Start(t)
	dead.SetDrop(true)
	s, p, _ := newTestServer(t, func(c *config.Config) {
		c.Strategy = config.StrategyFailover // always try the dead one first
	}, dead, alive)

	for i := 0; i < 4; i++ {
		s.Resolve(context.Background(), mustQuery(t, "example.com."), dnsclient.ProtoUDP)
	}

	if p.Upstreams()[0].Healthy() {
		t.Error("the dropping upstream should have been marked down by passive health")
	}
	if p.HealthyUpstreams() != 1 {
		t.Errorf("HealthyUpstreams = %d, want 1", p.HealthyUpstreams())
	}

	before := dead.Queries()
	for i := 0; i < 3; i++ {
		s.Resolve(context.Background(), mustQuery(t, "example.com."), dnsclient.ProtoUDP)
	}
	if dead.Queries() != before {
		t.Errorf("a down endpoint still received %d queries", dead.Queries()-before)
	}
}

func TestResolveRetriesOnServfailButNotOnNxdomain(t *testing.T) {
	broken, alive := testdns.Start(t), testdns.Start(t)
	broken.SetRCode(dnsmsg.RCodeServFail)
	s, _, _ := newTestServer(t, func(c *config.Config) {
		c.Strategy = config.StrategyFailover
	}, broken, alive)

	resp := s.Resolve(context.Background(), mustQuery(t, "example.com."), dnsclient.ProtoUDP)
	hdr, _ := dnsmsg.ParseHeader(resp)
	if hdr.RCode != dnsmsg.RCodeNoError {
		t.Errorf("rcode = %v, want the retry to reach the healthy upstream", hdr.RCode)
	}

	// NXDOMAIN is how Pi-hole reports a blocked domain. Retrying it elsewhere
	// would quietly undo blocking, so it must be returned as-is.
	blocking := testdns.Start(t)
	blocking.SetRCode(dnsmsg.RCodeNXDomain)
	s2, _, _ := newTestServer(t, func(c *config.Config) {
		c.Strategy = config.StrategyFailover
	}, blocking, alive)

	before := alive.Queries()
	resp = s2.Resolve(context.Background(), mustQuery(t, "ads.example."), dnsclient.ProtoUDP)
	hdr, _ = dnsmsg.ParseHeader(resp)
	if hdr.RCode != dnsmsg.RCodeNXDomain {
		t.Errorf("rcode = %v, want NXDOMAIN passed straight through", hdr.RCode)
	}
	if alive.Queries() != before {
		t.Error("a blocked domain must not be retried against another upstream")
	}
}

func TestResolveServfailsWhenEveryUpstreamIsDown(t *testing.T) {
	a, b := testdns.Start(t), testdns.Start(t)
	a.SetDrop(true)
	b.SetDrop(true)
	s, _, m := newTestServer(t, nil, a, b)

	resp := s.Resolve(context.Background(), mustQuery(t, "example.com."), dnsclient.ProtoUDP)
	if resp == nil {
		t.Fatal("clients must get an answer, even a failure, rather than a hang")
	}
	hdr, _ := dnsmsg.ParseHeader(resp)
	if hdr.RCode != dnsmsg.RCodeServFail {
		t.Errorf("rcode = %v, want SERVFAIL", hdr.RCode)
	}
	if hdr.ID != 0x4242 {
		t.Errorf("id = %#x, want the client's transaction id echoed", hdr.ID)
	}
	if m.Servfails.Value() != 1 {
		t.Errorf("servfail metric = %d, want 1", m.Servfails.Value())
	}
}

func TestMaxAttemptsIsRespected(t *testing.T) {
	a, b, c := testdns.Start(t), testdns.Start(t), testdns.Start(t)
	for _, u := range []*testdns.Server{a, b, c} {
		u.SetDrop(true)
	}
	s, _, _ := newTestServer(t, func(cfg *config.Config) {
		cfg.Query.MaxAttempts = 2
	}, a, b, c)

	s.Resolve(context.Background(), mustQuery(t, "example.com."), dnsclient.ProtoUDP)

	total := a.Queries() + b.Queries() + c.Queries()
	if total != 2 {
		t.Errorf("upstreams saw %d attempts, want max_attempts=2", total)
	}
}

func TestResolveSpreadsLoadAcrossHealthyUpstreams(t *testing.T) {
	servers := []*testdns.Server{testdns.Start(t), testdns.Start(t), testdns.Start(t)}
	s, _, _ := newTestServer(t, nil, servers...)

	const runs = 300
	for i := 0; i < runs; i++ {
		if resp := s.Resolve(context.Background(), mustQuery(t, "example.com."), dnsclient.ProtoUDP); resp == nil {
			t.Fatalf("run %d: no response", i)
		}
	}
	for i, u := range servers {
		if u.Queries() < runs/6 {
			t.Errorf("upstream %d received only %d of %d queries; load is not being spread",
				i, u.Queries(), runs)
		}
	}
}

func TestResolveDropsMalformedAndReflectedMessages(t *testing.T) {
	up := testdns.Start(t)
	s, _, _ := newTestServer(t, nil, up)

	if resp := s.Resolve(context.Background(), []byte{0x01, 0x02}, dnsclient.ProtoUDP); resp != nil {
		t.Error("a truncated datagram should be dropped, not answered")
	}

	// A message with the response bit set would let us be used as a
	// reflector; it must never produce a reply.
	reflected := mustQuery(t, "example.com.")
	binary.BigEndian.PutUint16(reflected[2:4], 0x8180)
	if resp := s.Resolve(context.Background(), reflected, dnsclient.ProtoUDP); resp != nil {
		t.Error("a DNS response arriving as a query should be dropped")
	}
	if up.Queries() != 0 {
		t.Error("malformed input should never reach an upstream")
	}
}

func TestDrainedUpstreamStopsReceivingQueries(t *testing.T) {
	a, b := testdns.Start(t), testdns.Start(t)
	s, p, _ := newTestServer(t, nil, a, b)
	p.Upstreams()[0].SetDrained(true)

	for i := 0; i < 30; i++ {
		s.Resolve(context.Background(), mustQuery(t, "example.com."), dnsclient.ProtoUDP)
	}
	if a.Queries() != 0 {
		t.Errorf("drained upstream received %d queries, want 0", a.Queries())
	}
	if b.Queries() != 30 {
		t.Errorf("remaining upstream received %d queries, want 30", b.Queries())
	}
}

func TestConcurrencyLimitShedsLoad(t *testing.T) {
	up := testdns.Start(t)
	s, _, _ := newTestServer(t, func(c *config.Config) {
		c.Query.MaxConcurrent = 1
	}, up)

	if !s.acquire() {
		t.Fatal("first acquire should succeed")
	}
	if s.acquire() {
		t.Fatal("second acquire should be refused at the limit")
	}
	resp := s.overloaded(mustQuery(t, "example.com."), dnsclient.ProtoUDP)
	hdr, _ := dnsmsg.ParseHeader(resp)
	if hdr.RCode != dnsmsg.RCodeServFail {
		t.Errorf("shed query rcode = %v, want SERVFAIL", hdr.RCode)
	}
	s.release()
	if !s.acquire() {
		t.Error("a released slot should be reusable")
	}
}

// End-to-end through the real sockets: a client speaking UDP and TCP to the
// balancer's listeners gets answers from a real upstream.
func TestListenAndServeEndToEnd(t *testing.T) {
	up := testdns.Start(t)
	s, _, _ := newTestServer(t, nil, up)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- s.ListenAndServe(ctx) }()

	select {
	case <-s.Ready():
	case <-time.After(5 * time.Second):
		t.Fatal("listeners never became ready")
	}

	t.Run("udp", func(t *testing.T) {
		conn, err := net.Dial("udp", s.UDPAddr())
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(3 * time.Second))

		if _, err := conn.Write(mustQuery(t, "example.com.")); err != nil {
			t.Fatalf("write: %v", err)
		}
		buf := make([]byte, 4096)
		n, err := conn.Read(buf)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if err := dnsmsg.ValidateResponse(buf[:n], 0x4242); err != nil {
			t.Errorf("invalid response over udp: %v", err)
		}
	})

	t.Run("tcp", func(t *testing.T) {
		conn, err := net.Dial("tcp", s.TCPAddr())
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(3 * time.Second))

		// Two queries on one connection, to confirm the framing loop keeps
		// the connection usable rather than answering once and desyncing.
		for i := 0; i < 2; i++ {
			if err := dnsclient.WriteMessage(conn, mustQuery(t, "example.com.")); err != nil {
				t.Fatalf("write %d: %v", i, err)
			}
			resp, err := dnsclient.ReadMessage(conn)
			if err != nil {
				t.Fatalf("read %d: %v", i, err)
			}
			if err := dnsmsg.ValidateResponse(resp, 0x4242); err != nil {
				t.Errorf("invalid response over tcp: %v", err)
			}
		}
	})

	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Errorf("ListenAndServe returned %v, want a clean shutdown", err)
		}
	case <-time.After(5 * time.Second):
		t.Error("ListenAndServe did not return after cancellation")
	}
}

func TestListenAndServeReportsBindErrors(t *testing.T) {
	blocker, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Close()

	up := testdns.Start(t)
	s, _, _ := newTestServer(t, func(c *config.Config) {
		c.Listen = config.Listen{UDP: blocker.LocalAddr().String()}
	}, up)

	if err := s.ListenAndServe(context.Background()); err == nil {
		t.Error("binding an address already in use should return an error")
	}
}

func TestSlowUpstreamTimesOutAndRetries(t *testing.T) {
	slow, fast := testdns.Start(t), testdns.Start(t)
	slow.SetDelay(2 * time.Second)
	s, _, _ := newTestServer(t, func(c *config.Config) {
		c.Strategy = config.StrategyFailover
		c.Query.Timeout = config.Duration(150 * time.Millisecond)
	}, slow, fast)

	start := time.Now()
	resp := s.Resolve(context.Background(), mustQuery(t, "example.com."), dnsclient.ProtoUDP)
	elapsed := time.Since(start)

	if resp == nil {
		t.Fatal("no response")
	}
	hdr, _ := dnsmsg.ParseHeader(resp)
	if hdr.RCode != dnsmsg.RCodeNoError {
		t.Errorf("rcode = %v, want the fast upstream's answer", hdr.RCode)
	}
	if elapsed > time.Second {
		t.Errorf("took %v; the slow upstream should have been abandoned after the timeout", elapsed)
	}
}
