package fallback

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chinmay28/hole-balancer/internal/config"
	"github.com/chinmay28/hole-balancer/internal/dnsclient"
	"github.com/chinmay28/hole-balancer/internal/dnsmsg"
	"github.com/chinmay28/hole-balancer/internal/metrics"
	"github.com/chinmay28/hole-balancer/internal/testdns"
)

// fakeClock lets the tracker's day-long windows be tested in microseconds.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newTestTracker(t *testing.T, servers ...string) (*Tracker, *fakeClock, *bytes.Buffer) {
	t.Helper()

	cfg := config.Default()
	cfg.Fallback.Servers = servers
	cfg.Fallback.SummaryInterval = config.Duration(24 * time.Hour)

	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))

	clock := &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	tr := NewTracker(&cfg, log)
	tr.now = clock.Now
	tr.windowStart = clock.Now()
	return tr, clock, &logs
}

func newTestResolver(t *testing.T, tune func(*config.Config), servers ...*testdns.Server) (*Resolver, *Tracker, *bytes.Buffer) {
	t.Helper()

	cfg := config.Default()
	cfg.Fallback.Timeout = config.Duration(500 * time.Millisecond)
	cfg.Fallback.Servers = nil
	for _, s := range servers {
		cfg.Fallback.Servers = append(cfg.Fallback.Servers, s.Addr())
	}
	cfg.Upstreams = []config.Upstream{{Name: "p1", Endpoints: []config.Endpoint{{Addr: "10.0.0.1"}}}}
	if tune != nil {
		tune(&cfg)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("test config is invalid: %v", err)
	}

	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	tr := NewTracker(&cfg, log)
	return NewResolver(&cfg, tr, metrics.New(), log), tr, &logs
}

func mustQuery(t *testing.T) []byte {
	t.Helper()
	q, err := dnsmsg.BuildQuery(0x5151, "example.com.", dnsmsg.TypeA)
	if err != nil {
		t.Fatal(err)
	}
	return q
}

func TestResolverAnswersFromPublicDNS(t *testing.T) {
	up := testdns.Start(t)
	r, tr, _ := newTestResolver(t, nil, up)

	resp, server, err := r.Resolve(context.Background(), mustQuery(t), dnsclient.ProtoUDP)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if err := dnsmsg.ValidateResponse(resp, 0x5151); err != nil {
		t.Errorf("invalid response: %v", err)
	}
	if server != up.Addr() {
		t.Errorf("server = %q, want %q", server, up.Addr())
	}
	if got := tr.Snapshot().Queries; got != 1 {
		t.Errorf("tracker recorded %d queries, want 1", got)
	}
}

func TestResolverTriesEveryServerBeforeGivingUp(t *testing.T) {
	dead, alive := testdns.Start(t), testdns.Start(t)
	dead.SetDrop(true)
	r, _, _ := newTestResolver(t, nil, dead, alive)

	// Whichever the rotation starts on, the working resolver must be reached.
	for i := 0; i < 4; i++ {
		if _, server, err := r.Resolve(context.Background(), mustQuery(t), dnsclient.ProtoUDP); err != nil {
			t.Fatalf("attempt %d: %v", i, err)
		} else if server != alive.Addr() {
			t.Errorf("attempt %d answered by %s, want the live resolver", i, server)
		}
	}
}

func TestResolverRotatesAcrossServers(t *testing.T) {
	a, b := testdns.Start(t), testdns.Start(t)
	r, _, _ := newTestResolver(t, nil, a, b)

	for i := 0; i < 20; i++ {
		if _, _, err := r.Resolve(context.Background(), mustQuery(t), dnsclient.ProtoUDP); err != nil {
			t.Fatal(err)
		}
	}
	if a.Queries() == 0 || b.Queries() == 0 {
		t.Errorf("rotation did not reach both resolvers: a=%d b=%d", a.Queries(), b.Queries())
	}
}

func TestResolverFailsWhenNoServerAnswers(t *testing.T) {
	a, b := testdns.Start(t), testdns.Start(t)
	a.SetDrop(true)
	b.SetDrop(true)
	r, tr, _ := newTestResolver(t, nil, a, b)

	if _, _, err := r.Resolve(context.Background(), mustQuery(t), dnsclient.ProtoUDP); err == nil {
		t.Fatal("Resolve succeeded with every resolver dead")
	}
	snap := tr.Snapshot()
	if snap.Failures != 2 {
		t.Errorf("failures = %d, want 2", snap.Failures)
	}
}

func TestResolverDisabled(t *testing.T) {
	r, _, _ := newTestResolver(t, func(cfg *config.Config) {
		cfg.Fallback.Enabled = false
	}, testdns.Start(t))

	if r.Enabled() {
		t.Error("resolver should report itself disabled")
	}
	if _, _, err := r.Resolve(context.Background(), mustQuery(t), dnsclient.ProtoUDP); err != ErrDisabled {
		t.Errorf("err = %v, want ErrDisabled", err)
	}

	// A nil resolver is the same as a disabled one, so callers need no guard.
	var nilResolver *Resolver
	if nilResolver.Enabled() {
		t.Error("a nil resolver should report itself disabled")
	}
}

func TestResolverMovesOnFromServfail(t *testing.T) {
	broken, good := testdns.Start(t), testdns.Start(t)
	broken.SetRCode(dnsmsg.RCodeServFail)
	r, _, _ := newTestResolver(t, nil, broken, good)

	for i := 0; i < 4; i++ {
		_, server, err := r.Resolve(context.Background(), mustQuery(t), dnsclient.ProtoUDP)
		if err != nil {
			t.Fatalf("attempt %d: %v", i, err)
		}
		if server != good.Addr() {
			t.Errorf("attempt %d answered by the SERVFAILing resolver", i)
		}
	}
}

// The core reporting requirement: usage is summarised, never logged per query.
func TestTrackerLogsOneSummaryPerWindow(t *testing.T) {
	tr, clock, logs := newTestTracker(t, "8.8.8.8:53")

	tr.Observe(true)
	for i := 0; i < 5000; i++ {
		tr.RecordQuery("8.8.8.8:53", true)
	}
	clock.Advance(30 * time.Minute)
	tr.Observe(false)

	if logs.Len() != 0 {
		t.Fatalf("nothing should be logged before the window closes:\n%s", logs.String())
	}

	clock.Advance(23 * time.Hour)
	tr.Report()

	out := logs.String()
	if n := strings.Count(out, "public DNS fallback"); n != 1 {
		t.Fatalf("expected exactly one summary line, got %d:\n%s", n, out)
	}
	for _, want := range []string{
		"queries=5000",
		"outages=1",
		"outage_total=30m0s",
		`resolvers="8.8.8.8:53=5000"`,
		"still_down=false",
		"NOT filtered",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("summary is missing %q\n---\n%s", want, out)
		}
	}
}

func TestTrackerStaysSilentWhenNothingHappened(t *testing.T) {
	tr, clock, logs := newTestTracker(t, "8.8.8.8:53")

	for i := 0; i < 3; i++ {
		clock.Advance(24 * time.Hour)
		tr.Report()
	}
	if logs.Len() != 0 {
		t.Errorf("a quiet day should produce no log line:\n%s", logs.String())
	}
}

func TestTrackerCountsSeparateOutages(t *testing.T) {
	tr, clock, _ := newTestTracker(t, "8.8.8.8:53")

	tr.Observe(true)
	clock.Advance(2 * time.Minute)
	tr.Observe(false)

	clock.Advance(time.Hour)

	tr.Observe(true)
	clock.Advance(10 * time.Minute)
	tr.Observe(false)

	s := tr.Snapshot()
	if s.Episodes != 2 {
		t.Errorf("episodes = %d, want 2", s.Episodes)
	}
	if s.TotalOutage != 12*time.Minute {
		t.Errorf("total = %v, want 12m", s.TotalOutage)
	}
	if s.LongestOutage != 10*time.Minute {
		t.Errorf("longest = %v, want 10m", s.LongestOutage)
	}
}

func TestObserveIsIdempotent(t *testing.T) {
	tr, clock, _ := newTestTracker(t, "8.8.8.8:53")

	// The health checker and endpoint state changes both report the same
	// outage; it must be counted once.
	for i := 0; i < 10; i++ {
		tr.Observe(true)
		clock.Advance(time.Minute)
	}
	tr.Observe(false)

	s := tr.Snapshot()
	if s.Episodes != 1 {
		t.Errorf("episodes = %d, want 1", s.Episodes)
	}
	if s.TotalOutage != 10*time.Minute {
		t.Errorf("total = %v, want 10m", s.TotalOutage)
	}
}

func TestOngoingOutageIsReportedAndCarriesOver(t *testing.T) {
	tr, clock, logs := newTestTracker(t, "8.8.8.8:53")

	tr.Observe(true)
	tr.RecordQuery("8.8.8.8:53", true)
	clock.Advance(24 * time.Hour)
	tr.Report()

	if !strings.Contains(logs.String(), "still_down=true") {
		t.Errorf("an outage in progress should be flagged:\n%s", logs.String())
	}
	if !strings.Contains(logs.String(), "outage_total=24h0m0s") {
		t.Errorf("elapsed time of an ongoing outage should be counted:\n%s", logs.String())
	}

	// The next window starts fresh but still knows the outage continues, and
	// must not double-count the time already reported.
	logs.Reset()
	clock.Advance(2 * time.Hour)
	tr.Report()

	out := logs.String()
	if !strings.Contains(out, "outage_total=2h0m0s") {
		t.Errorf("second window should report only its own 2h:\n%s", out)
	}
	if !strings.Contains(out, "outages=1") {
		t.Errorf("the continuing outage should still be counted once:\n%s", out)
	}
}

func TestReportResetsTheWindow(t *testing.T) {
	tr, clock, logs := newTestTracker(t, "8.8.8.8:53")

	tr.Observe(true)
	tr.RecordQuery("8.8.8.8:53", true)
	clock.Advance(time.Minute)
	tr.Observe(false)

	clock.Advance(24 * time.Hour)
	tr.Report()
	logs.Reset()

	clock.Advance(24 * time.Hour)
	tr.Report()
	if logs.Len() != 0 {
		t.Errorf("counters should have been cleared by the first report:\n%s", logs.String())
	}
}

func TestRunEmitsFinalSummaryOnShutdown(t *testing.T) {
	cfg := config.Default()
	cfg.Fallback.SummaryInterval = config.Duration(time.Hour)

	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	tr := NewTracker(&cfg, log)

	tr.Observe(true)
	tr.RecordQuery("8.8.8.8:53", true)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		tr.Run(ctx)
		close(done)
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}

	// Without this, a process that restarts daily would never report anything.
	if !strings.Contains(logs.String(), "public DNS fallback") {
		t.Errorf("shutdown should flush the pending summary:\n%s", logs.String())
	}
}

func TestTrackerIsConcurrencySafe(t *testing.T) {
	tr, _, _ := newTestTracker(t, "8.8.8.8:53")

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 300; j++ {
				tr.RecordQuery("8.8.8.8:53", j%4 != 0)
				tr.Observe(j%2 == 0)
				tr.Snapshot()
			}
		}(i)
	}
	wg.Wait()

	if got := tr.Snapshot().Queries; got != 8*300 {
		t.Errorf("queries = %d, want %d", got, 8*300)
	}
}

func TestFormatServersIsStable(t *testing.T) {
	got := formatServers(map[string]uint64{"8.8.4.4:53": 2, "8.8.8.8:53": 10})
	if want := "8.8.4.4:53=2 8.8.8.8:53=10"; got != want {
		t.Errorf("formatServers = %q, want %q", got, want)
	}
	if got := formatServers(nil); got != "none" {
		t.Errorf("formatServers(nil) = %q, want none", got)
	}
}
