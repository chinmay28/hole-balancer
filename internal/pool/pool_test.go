package pool

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/chinmay28/hole-balancer/internal/config"
)

// newTestPool builds a pool of n upstreams, each with the given number of
// endpoints, and marks every endpoint healthy.
func newTestPool(t *testing.T, strategy string, upstreams, endpointsEach int) *Pool {
	t.Helper()

	cfg := config.Default()
	cfg.Strategy = strategy
	cfg.Listen = config.Listen{UDP: ":0"}
	for i := 0; i < upstreams; i++ {
		u := config.Upstream{Name: fmt.Sprintf("pihole-%d", i+1), Weight: 1}
		for j := 0; j < endpointsEach; j++ {
			u.Endpoints = append(u.Endpoints, config.Endpoint{
				Name: fmt.Sprintf("path-%d", j+1),
				Addr: fmt.Sprintf("10.0.%d.%d:53", i+1, j+1),
			})
		}
		cfg.Upstreams = append(cfg.Upstreams, u)
	}

	if err := cfg.Validate(); err != nil {
		t.Fatalf("test config is invalid: %v", err)
	}

	p := New(&cfg, nil)
	for _, e := range p.Endpoints() {
		e.up.Store(true)
	}
	return p
}

func planNames(plan []*Endpoint) []string {
	out := make([]string, len(plan))
	for i, e := range plan {
		out[i] = e.Upstream.Name + "/" + e.Name
	}
	return out
}

func TestPlanPrefersFirstHealthyEndpointPerUpstream(t *testing.T) {
	p := newTestPool(t, config.StrategyFailover, 2, 2)

	plan := p.Plan()
	if len(plan) != 4 {
		t.Fatalf("plan has %d entries, want 4: %v", len(plan), planNames(plan))
	}
	// Tier 1 is one entry per upstream, using each upstream's preferred path.
	if plan[0].Upstream.Name != "pihole-1" || plan[0].Name != "path-1" {
		t.Errorf("plan[0] = %s, want pihole-1/path-1", planNames(plan)[0])
	}
	if plan[1].Upstream.Name != "pihole-2" || plan[1].Name != "path-1" {
		t.Errorf("plan[1] = %s, want pihole-2/path-1", planNames(plan)[1])
	}
	// Tier 2 is the alternate paths, so a retry reaches a different machine
	// before it falls back to a second route to the same one.
	for _, e := range plan[2:] {
		if e.Name != "path-2" {
			t.Errorf("tier 2 entry %s should be an alternate path", e.Upstream.Name+"/"+e.Name)
		}
	}
}

func TestPlanSkipsDownEndpointsButKeepsThemAsLastResort(t *testing.T) {
	p := newTestPool(t, config.StrategyFailover, 2, 2)
	// Take down pihole-1's LAN path only.
	p.Upstreams()[0].Endpoints[0].up.Store(false)

	plan := p.Plan()
	if plan[0].Name != "path-2" || plan[0].Upstream.Name != "pihole-1" {
		t.Errorf("plan[0] = %s, want pihole-1 to fall through to its second path",
			planNames(plan)[0])
	}
	if got := planNames(plan)[len(plan)-1]; got != "pihole-1/path-1" {
		t.Errorf("down endpoint should sort last, plan = %v", planNames(plan))
	}
}

// Fault tolerance matters more than an out-of-date health verdict: when the
// balancer believes nothing is up it must still try rather than SERVFAIL.
func TestPlanFailsOpenWhenNothingIsHealthy(t *testing.T) {
	p := newTestPool(t, config.StrategyRandom, 3, 2)
	for _, e := range p.Endpoints() {
		e.up.Store(false)
	}

	plan := p.Plan()
	if len(plan) != 6 {
		t.Fatalf("plan has %d entries, want all 6 endpoints: %v", len(plan), planNames(plan))
	}
	if p.HealthyUpstreams() != 0 {
		t.Errorf("HealthyUpstreams = %d, want 0", p.HealthyUpstreams())
	}
}

func TestPlanSkipsDrainedUpstreams(t *testing.T) {
	p := newTestPool(t, config.StrategyFailover, 2, 1)
	p.Upstreams()[0].SetDrained(true)

	plan := p.Plan()
	for _, e := range plan {
		if e.Upstream.Name == "pihole-1" {
			t.Fatalf("drained upstream appeared in plan: %v", planNames(plan))
		}
	}
	if p.HealthyUpstreams() != 1 {
		t.Errorf("HealthyUpstreams = %d, want 1", p.HealthyUpstreams())
	}

	// Draining everything must not black-hole DNS for the whole network.
	p.Upstreams()[1].SetDrained(true)
	if plan := p.Plan(); len(plan) != 2 {
		t.Errorf("with everything drained the plan should still list every endpoint, got %v",
			planNames(plan))
	}
}

func TestRandomStrategySpreadsLoad(t *testing.T) {
	p := newTestPool(t, config.StrategyRandom, 4, 1)

	const runs = 4000
	counts := map[string]int{}
	for i := 0; i < runs; i++ {
		counts[p.Plan()[0].Upstream.Name]++
	}

	expected := runs / 4
	for name, got := range counts {
		if got < expected*7/10 || got > expected*13/10 {
			t.Errorf("%s selected %d times, want roughly %d (distribution: %v)", name, got, expected, counts)
		}
	}
	if len(counts) != 4 {
		t.Errorf("only %d of 4 upstreams were ever selected: %v", len(counts), counts)
	}
}

func TestRandomStrategyRespectsWeights(t *testing.T) {
	p := newTestPool(t, config.StrategyRandom, 2, 1)
	p.Upstreams()[0].Weight = 3 // 3:1 split

	const runs = 4000
	counts := map[string]int{}
	for i := 0; i < runs; i++ {
		counts[p.Plan()[0].Upstream.Name]++
	}

	ratio := float64(counts["pihole-1"]) / float64(counts["pihole-2"])
	if ratio < 2.5 || ratio > 3.6 {
		t.Errorf("selection ratio = %.2f, want about 3.0 (distribution: %v)", ratio, counts)
	}
}

func TestRoundRobinStrategyIsExactlyEven(t *testing.T) {
	p := newTestPool(t, config.StrategyRoundRobin, 3, 1)

	var order []string
	for i := 0; i < 6; i++ {
		order = append(order, p.Plan()[0].Upstream.Name)
	}
	want := []string{"pihole-1", "pihole-2", "pihole-3", "pihole-1", "pihole-2", "pihole-3"}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("round-robin order = %v, want %v", order, want)
		}
	}
}

func TestFailoverStrategyPrefersConfigOrder(t *testing.T) {
	p := newTestPool(t, config.StrategyFailover, 3, 1)
	for i := 0; i < 20; i++ {
		if got := p.Plan()[0].Upstream.Name; got != "pihole-1" {
			t.Fatalf("failover picked %s, want the first configured upstream", got)
		}
	}

	p.Upstreams()[0].Endpoints[0].up.Store(false)
	if got := p.Plan()[0].Upstream.Name; got != "pihole-2" {
		t.Errorf("with the primary down, failover picked %s, want pihole-2", got)
	}
}

func TestLeastLatencyStrategy(t *testing.T) {
	p := newTestPool(t, config.StrategyLeastLatency, 3, 1)
	p.ReportProbe(p.Upstreams()[0].Endpoints[0], true, 50*time.Millisecond, nil)
	p.ReportProbe(p.Upstreams()[1].Endpoints[0], true, 5*time.Millisecond, nil)
	p.ReportProbe(p.Upstreams()[2].Endpoints[0], true, 20*time.Millisecond, nil)

	plan := p.Plan()
	want := []string{"pihole-2", "pihole-3", "pihole-1"}
	for i, name := range want {
		if plan[i].Upstream.Name != name {
			t.Fatalf("least-latency order = %v, want %v", planNames(plan), want)
		}
	}
}

func TestFallAndRiseThresholds(t *testing.T) {
	p := newTestPool(t, config.StrategyRandom, 1, 1)
	// Default rise and fall are both 2.
	e := p.Endpoints()[0]
	boom := errors.New("connection refused")

	p.ReportQuery(e, false, 0, boom)
	if !e.Healthy() {
		t.Fatal("one failure should not take an endpoint out of rotation")
	}
	p.ReportQuery(e, false, 0, boom)
	if e.Healthy() {
		t.Fatal("two consecutive failures should mark the endpoint down")
	}

	p.ReportProbe(e, true, time.Millisecond, nil)
	if e.Healthy() {
		t.Fatal("one success should not be enough to bring an endpoint back")
	}
	p.ReportProbe(e, true, time.Millisecond, nil)
	if !e.Healthy() {
		t.Fatal("two consecutive successes should restore the endpoint")
	}
}

func TestIntermittentFailuresDoNotAccumulate(t *testing.T) {
	p := newTestPool(t, config.StrategyRandom, 1, 1)
	e := p.Endpoints()[0]

	// A failure every other query is packet loss, not an outage.
	for i := 0; i < 20; i++ {
		p.ReportQuery(e, false, 0, errors.New("timeout"))
		p.ReportQuery(e, true, time.Millisecond, nil)
	}
	if !e.Healthy() {
		t.Error("alternating success and failure should not take an endpoint down")
	}
}

func TestPassiveDisabledIgnoresQueryOutcomes(t *testing.T) {
	cfg := config.Default()
	cfg.Health.Passive.Enabled = false
	cfg.Upstreams = []config.Upstream{{
		Name: "pihole-1", Weight: 1,
		Endpoints: []config.Endpoint{{Name: "lan", Addr: "10.0.0.1:53"}},
	}}

	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}

	p := New(&cfg, nil)
	e := p.Endpoints()[0]
	e.up.Store(true)

	for i := 0; i < 10; i++ {
		p.ReportQuery(e, false, 0, errors.New("timeout"))
	}
	if !e.Healthy() {
		t.Error("with passive checks off, query failures must not change health")
	}

	// Latency is still tracked, since it feeds the least-latency strategy.
	p.ReportQuery(e, true, 10*time.Millisecond, nil)
	if e.Latency() != 10*time.Millisecond {
		t.Errorf("latency = %v, want it recorded even when passive health is off", e.Latency())
	}

	// Active probes still apply.
	p.ReportProbe(e, false, 0, errors.New("timeout"))
	p.ReportProbe(e, false, 0, errors.New("timeout"))
	if e.Healthy() {
		t.Error("active probes should still be able to take an endpoint down")
	}
}

func TestStateChangeCallbackFiresOncePerTransition(t *testing.T) {
	cfg := config.Default()
	cfg.Upstreams = []config.Upstream{{
		Name: "pihole-1", Weight: 1,
		Endpoints: []config.Endpoint{{Name: "lan", Addr: "10.0.0.1:53"}},
	}}

	var flips []bool
	p := New(&cfg, func(_ *Endpoint, up bool, _ string) { flips = append(flips, up) })
	e := p.Endpoints()[0]
	e.up.Store(true)

	for i := 0; i < 5; i++ {
		p.ReportProbe(e, false, 0, errors.New("timeout"))
	}
	for i := 0; i < 5; i++ {
		p.ReportProbe(e, true, time.Millisecond, nil)
	}

	if len(flips) != 2 || flips[0] != false || flips[1] != true {
		t.Errorf("state changes = %v, want exactly [false true]", flips)
	}
}

func TestSetInitialBypassesThresholds(t *testing.T) {
	p := newTestPool(t, config.StrategyRandom, 1, 1)
	e := p.Endpoints()[0]
	e.up.Store(false)

	p.SetInitial(e, true, 3*time.Millisecond, nil)
	if !e.Healthy() {
		t.Fatal("the first probe result should apply immediately at startup")
	}
	if e.Latency() != 3*time.Millisecond {
		t.Errorf("latency = %v, want 3ms", e.Latency())
	}

	// And the endpoint should still need a full `fall` run to go back down.
	p.ReportQuery(e, false, 0, errors.New("timeout"))
	if !e.Healthy() {
		t.Error("thresholds should apply normally after the initial verdict")
	}
}

func TestLatencyEWMASmoothsSpikes(t *testing.T) {
	p := newTestPool(t, config.StrategyRandom, 1, 1)
	e := p.Endpoints()[0]

	p.ReportProbe(e, true, 10*time.Millisecond, nil)
	if e.Latency() != 10*time.Millisecond {
		t.Fatalf("first sample = %v, want it taken verbatim", e.Latency())
	}
	p.ReportProbe(e, true, 1000*time.Millisecond, nil)
	if got := e.Latency(); got > 250*time.Millisecond {
		t.Errorf("latency = %v, a single spike should not dominate the average", got)
	}
}

func TestStatusReflectsPool(t *testing.T) {
	p := newTestPool(t, config.StrategyFailover, 2, 2)
	p.Upstreams()[1].SetDrained(true)
	p.ReportQuery(p.Endpoints()[0], true, 5*time.Millisecond, nil)

	status := p.Status()
	if len(status) != 2 {
		t.Fatalf("status has %d upstreams, want 2", len(status))
	}
	if !status[0].Healthy || status[0].Drained {
		t.Errorf("pihole-1 status = %+v, want healthy and not drained", status[0])
	}
	if !status[1].Drained {
		t.Error("pihole-2 should report as drained")
	}
	if !status[0].Endpoints[0].IsPreferred {
		t.Error("the first healthy endpoint should be marked preferred")
	}
	if status[0].Endpoints[0].Queries != 1 {
		t.Errorf("query count = %d, want 1", status[0].Endpoints[0].Queries)
	}
	if status[0].Endpoints[0].LatencyMS != 5 {
		t.Errorf("latency = %v ms, want 5", status[0].Endpoints[0].LatencyMS)
	}
}

func TestLookup(t *testing.T) {
	p := newTestPool(t, config.StrategyRandom, 2, 1)
	if u := p.Lookup("pihole-2"); u == nil || u.Name != "pihole-2" {
		t.Errorf("Lookup(pihole-2) = %v", u)
	}
	if u := p.Lookup("nope"); u != nil {
		t.Errorf("Lookup(nope) = %v, want nil", u)
	}
}

func TestConcurrentReportsAndPlans(t *testing.T) {
	p := newTestPool(t, config.StrategyRandom, 3, 2)
	done := make(chan struct{})

	for i := 0; i < 8; i++ {
		go func() {
			for j := 0; j < 500; j++ {
				plan := p.Plan()
				if len(plan) > 0 {
					p.ReportQuery(plan[0], j%3 != 0, time.Millisecond, errors.New("x"))
				}
				p.Status()
			}
			done <- struct{}{}
		}()
	}
	for i := 0; i < 8; i++ {
		<-done
	}
}
