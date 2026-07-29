package health

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/chinmay28/hole-balancer/internal/config"
	"github.com/chinmay28/hole-balancer/internal/dnsmsg"
	"github.com/chinmay28/hole-balancer/internal/metrics"
	"github.com/chinmay28/hole-balancer/internal/pool"
	"github.com/chinmay28/hole-balancer/internal/testdns"
)

func newChecker(t *testing.T, tune func(*config.Config), servers ...*testdns.Server) (*Checker, *pool.Pool) {
	t.Helper()

	cfg := config.Default()
	cfg.Health.Timeout = config.Duration(300 * time.Millisecond)
	cfg.Health.Interval = config.Duration(50 * time.Millisecond)
	cfg.Health.StartupTimeout = config.Duration(time.Second)
	cfg.Health.Probe.Name = "probe.test."
	cfg.Admin.Listen = ""
	for i, s := range servers {
		cfg.Upstreams = append(cfg.Upstreams, config.Upstream{
			Name:      string(rune('a'+i)) + "-pihole",
			Weight:    1,
			Endpoints: []config.Endpoint{{Name: "lan", Addr: s.Addr()}},
		})
	}
	if tune != nil {
		tune(&cfg)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("test config is invalid: %v", err)
	}

	p := pool.New(&cfg, nil)
	log := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	return New(&cfg, p, metrics.New(), log), p
}

func TestBootstrapMarksReachableUpstreamsHealthyImmediately(t *testing.T) {
	up, down := testdns.Start(t), testdns.Start(t)
	down.SetDrop(true)

	c, p := newChecker(t, nil, up, down)
	c.Bootstrap(context.Background())

	if !p.Upstreams()[0].Healthy() {
		t.Error("a responding upstream should be usable after the first sweep, not after `rise` sweeps")
	}
	if p.Upstreams()[1].Healthy() {
		t.Error("a silent upstream should start down")
	}
	if p.HealthyUpstreams() != 1 {
		t.Errorf("HealthyUpstreams = %d, want 1", p.HealthyUpstreams())
	}
}

// A probe name on one Pi-hole's blocklist and not another's must not be read
// as an outage: NXDOMAIN proves the server is working.
func TestProbeTreatsNxdomainAsHealthy(t *testing.T) {
	blocking := testdns.Start(t)
	blocking.SetRCode(dnsmsg.RCodeNXDomain)

	c, p := newChecker(t, nil, blocking)
	c.Bootstrap(context.Background())

	if !p.Upstreams()[0].Healthy() {
		t.Error("NXDOMAIN means the server answered; it must count as healthy")
	}
}

func TestProbeTreatsServerFailuresAsUnhealthy(t *testing.T) {
	for _, rcode := range []dnsmsg.RCode{dnsmsg.RCodeServFail, dnsmsg.RCodeRefused, dnsmsg.RCodeNotImp} {
		s := testdns.Start(t)
		s.SetRCode(rcode)

		c, p := newChecker(t, nil, s)
		c.Bootstrap(context.Background())

		if p.Upstreams()[0].Healthy() {
			t.Errorf("%v should mark the upstream down", rcode)
		}
	}
}

func TestProbeRequireAnswer(t *testing.T) {
	empty := testdns.Start(t)
	empty.SetAnswers(0)

	c, p := newChecker(t, func(cfg *config.Config) {
		cfg.Health.Probe.RequireAnswer = true
	}, empty)
	c.Bootstrap(context.Background())

	if p.Upstreams()[0].Healthy() {
		t.Error("with require_answer set, an empty NOERROR should not count as healthy")
	}

	// And with the default (off), the same reply is fine.
	c2, p2 := newChecker(t, nil, empty)
	c2.Bootstrap(context.Background())
	if !p2.Upstreams()[0].Healthy() {
		t.Error("by default an empty NOERROR should count as healthy")
	}
}

func TestSweepDetectsAnOutageAndTheRecovery(t *testing.T) {
	s := testdns.Start(t)
	c, p := newChecker(t, nil, s)
	c.Bootstrap(context.Background())
	if !p.Upstreams()[0].Healthy() {
		t.Fatal("upstream should start healthy")
	}

	// Default fall is 2, so two sweeps take it out.
	s.SetDrop(true)
	c.Sweep(context.Background())
	if !p.Upstreams()[0].Healthy() {
		t.Error("one failed sweep should not be enough to declare an outage")
	}
	c.Sweep(context.Background())
	if p.Upstreams()[0].Healthy() {
		t.Error("two failed sweeps should mark the upstream down")
	}

	// Probing continues against down endpoints, which is the only way back.
	s.SetDrop(false)
	c.Sweep(context.Background())
	c.Sweep(context.Background())
	if !p.Upstreams()[0].Healthy() {
		t.Error("a recovered upstream should be brought back into rotation")
	}
}

func TestRunProbesOnInterval(t *testing.T) {
	s := testdns.Start(t)
	c, p := newChecker(t, nil, s)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)

	deadline := time.Now().Add(3 * time.Second)
	for !p.Upstreams()[0].Healthy() {
		if time.Now().After(deadline) {
			t.Fatal("Run never brought the upstream up")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if s.Queries() == 0 {
		t.Error("Run never sent a probe")
	}
}

func TestBootstrapIsBoundedByStartupTimeout(t *testing.T) {
	slow := testdns.Start(t)
	slow.SetDrop(true)

	c, p := newChecker(t, func(cfg *config.Config) {
		cfg.Health.Timeout = config.Duration(5 * time.Second)
		cfg.Health.StartupTimeout = config.Duration(200 * time.Millisecond)
	}, slow)

	start := time.Now()
	c.Bootstrap(context.Background())
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("bootstrap took %v; it must be bounded by startup_timeout", elapsed)
	}
	if p.Upstreams()[0].Healthy() {
		t.Error("an upstream that did not answer in time should start down")
	}
}

func TestProbesRunAgainstEveryEndpointOfAnUpstream(t *testing.T) {
	lan, tailscale := testdns.Start(t), testdns.Start(t)

	cfg := config.Default()
	cfg.Health.Timeout = config.Duration(300 * time.Millisecond)
	cfg.Health.StartupTimeout = config.Duration(time.Second)
	cfg.Upstreams = []config.Upstream{{
		Name:   "pihole-1",
		Weight: 1,
		Endpoints: []config.Endpoint{
			{Name: "lan", Addr: lan.Addr()},
			{Name: "tailscale", Addr: tailscale.Addr()},
		},
	}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}

	p := pool.New(&cfg, nil)
	log := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	New(&cfg, p, metrics.New(), log).Bootstrap(context.Background())

	if lan.Queries() == 0 || tailscale.Queries() == 0 {
		t.Errorf("both paths should be probed; got lan=%d tailscale=%d",
			lan.Queries(), tailscale.Queries())
	}
	if p.Upstreams()[0].Active().Name != "lan" {
		t.Error("the first configured path should be preferred while it is healthy")
	}

	// With the LAN path down, the upstream stays usable over Tailscale.
	lan.SetDrop(true)
	c := New(&cfg, p, metrics.New(), log)
	c.Sweep(context.Background())
	c.Sweep(context.Background())

	if !p.Upstreams()[0].Healthy() {
		t.Fatal("an upstream with one working path must remain healthy")
	}
	if p.Upstreams()[0].Active().Name != "tailscale" {
		t.Error("traffic should move to the surviving path")
	}
}
