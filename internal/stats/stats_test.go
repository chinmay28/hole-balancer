package stats

import (
	"sync"
	"testing"
	"time"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newTestCollector(t *testing.T) (*Collector, *clock) {
	t.Helper()
	c := &clock{t: time.Date(2026, 3, 4, 12, 0, 0, 0, time.UTC)}
	col := New()
	col.now = c.Now
	col.started = c.Now()
	return col, c
}

func answered(node string) Record {
	return Record{
		Proto: "udp", QType: "A", Upstream: node, Endpoint: "lan",
		RCode: "NOERROR", Latency: 2 * time.Millisecond, Attempts: 1,
	}
}

func TestTracksTotalsAndPerNodeShares(t *testing.T) {
	c, _ := newTestCollector(t)
	for i := 0; i < 6; i++ {
		c.Record(answered("pihole-a"))
	}
	for i := 0; i < 2; i++ {
		c.Record(answered("pihole-b"))
	}

	s := c.Snapshot()
	if s.Total != 8 {
		t.Errorf("total = %d, want 8", s.Total)
	}
	if s.TopNode != "pihole-a" || s.TopNodeCount != 6 {
		t.Errorf("top node = %s/%d, want pihole-a/6", s.TopNode, s.TopNodeCount)
	}
	if len(s.Nodes) != 2 {
		t.Fatalf("nodes = %+v", s.Nodes)
	}
	if s.Nodes[0].Share != 0.75 {
		t.Errorf("busiest share = %v, want 0.75", s.Nodes[0].Share)
	}
	if s.AvgMS != 2 {
		t.Errorf("avg latency = %v ms, want 2", s.AvgMS)
	}
}

// Ties must not reshuffle between refreshes, or the dashboard flickers.
func TestNodeOrderIsStableOnTies(t *testing.T) {
	c, _ := newTestCollector(t)
	for _, n := range []string{"zulu", "alpha", "mike"} {
		c.Record(answered(n))
	}
	for i := 0; i < 5; i++ {
		s := c.Snapshot()
		got := []string{s.Nodes[0].Name, s.Nodes[1].Name, s.Nodes[2].Name}
		want := []string{"alpha", "mike", "zulu"}
		for j := range want {
			if got[j] != want[j] {
				t.Fatalf("order = %v, want %v", got, want)
			}
		}
	}
}

func TestCountsFallbackAndFailuresSeparately(t *testing.T) {
	c, _ := newTestCollector(t)
	c.Record(answered("pihole-a"))
	c.Record(Record{Proto: "udp", QType: "A", RCode: "NOERROR", Fallback: true, Attempts: 3})
	c.Record(Record{Proto: "tcp", QType: "A", RCode: "SERVFAIL", Failed: true, Attempts: 3})

	s := c.Snapshot()
	if s.Fallback != 1 {
		t.Errorf("fallback = %d, want 1", s.Fallback)
	}
	if s.Failed != 1 {
		t.Errorf("failed = %d, want 1", s.Failed)
	}
	if s.Retries != 4 {
		t.Errorf("retries = %d, want 4 (two queries at 3 attempts each)", s.Retries)
	}
	// A fallback answer belongs to no Pi-hole, so it must not inflate one.
	if len(s.Nodes) != 1 || s.Nodes[0].Name != "pihole-a" {
		t.Errorf("nodes = %+v, want only pihole-a", s.Nodes)
	}
}

func TestBlockedShareUsesNxdomain(t *testing.T) {
	c, _ := newTestCollector(t)
	for i := 0; i < 3; i++ {
		r := answered("pihole-a")
		r.RCode = "NXDOMAIN"
		c.Record(r)
	}
	c.Record(answered("pihole-a"))

	if got := c.Snapshot().BlockedShare; got != 0.75 {
		t.Errorf("blocked share = %v, want 0.75", got)
	}
}

func TestHistoryBucketsByMinute(t *testing.T) {
	c, clk := newTestCollector(t)

	c.Record(answered("a"))
	c.Record(answered("a"))
	clk.Advance(time.Minute)
	c.Record(answered("a"))

	s := c.Snapshot()
	if len(s.Minutes) != minuteBuckets {
		t.Fatalf("history has %d buckets, want %d", len(s.Minutes), minuteBuckets)
	}
	last := s.Minutes[len(s.Minutes)-1]
	prev := s.Minutes[len(s.Minutes)-2]
	if last.Queries != 1 {
		t.Errorf("newest bucket = %d, want 1", last.Queries)
	}
	if prev.Queries != 2 {
		t.Errorf("previous bucket = %d, want 2", prev.Queries)
	}
	// Every other bucket is a zero, so the chart's x-axis is evenly spaced.
	var sum uint64
	for _, p := range s.Minutes {
		sum += p.Queries
	}
	if sum != 3 {
		t.Errorf("history total = %d, want 3", sum)
	}
}

// The ring is reused every hour. A slot from the previous hour is stale
// history and must be reset, not added to.
func TestHistoryRingDoesNotAccumulateAcrossWraps(t *testing.T) {
	c, clk := newTestCollector(t)

	c.Record(answered("a"))
	clk.Advance(time.Hour) // same minute-of-hour slot, an hour later
	c.Record(answered("a"))

	s := c.Snapshot()
	last := s.Minutes[len(s.Minutes)-1]
	if last.Queries != 1 {
		t.Errorf("bucket = %d, want 1: the previous hour's count should have been discarded", last.Queries)
	}
}

func TestHistoryDropsBucketsOlderThanTheWindow(t *testing.T) {
	c, clk := newTestCollector(t)
	c.Record(answered("a"))

	clk.Advance(2 * time.Hour)
	s := c.Snapshot()
	for _, p := range s.Minutes {
		if p.Queries != 0 {
			t.Fatalf("a query from two hours ago is still in the last-hour history: %+v", p)
		}
	}
}

func TestForgetUpstream(t *testing.T) {
	c, _ := newTestCollector(t)
	c.Record(answered("gone"))
	c.Record(answered("stays"))

	c.ForgetUpstream("gone")
	s := c.Snapshot()
	if len(s.Nodes) != 1 || s.Nodes[0].Name != "stays" {
		t.Errorf("nodes = %+v, want only stays", s.Nodes)
	}
	// The global total is deliberately untouched: those queries did happen.
	if s.Total != 2 {
		t.Errorf("total = %d, want 2", s.Total)
	}
}

func TestReset(t *testing.T) {
	c, _ := newTestCollector(t)
	for i := 0; i < 10; i++ {
		c.Record(answered("a"))
	}
	c.Reset()

	s := c.Snapshot()
	if s.Total != 0 || len(s.Nodes) != 0 || s.TopNode != "" {
		t.Errorf("snapshot after reset = %+v", s)
	}
	for _, p := range s.Minutes {
		if p.Queries != 0 {
			t.Error("history survived a reset")
		}
	}
}

func TestQPSAndRecentRate(t *testing.T) {
	c, clk := newTestCollector(t)
	for i := 0; i < 120; i++ {
		c.Record(answered("a"))
	}
	clk.Advance(60 * time.Second)

	s := c.Snapshot()
	if s.QPS != 2 {
		t.Errorf("qps = %v, want 2", s.QPS)
	}
	if s.QPM != 2 {
		t.Errorf("recent rate = %v, want 2 per minute over the 60-bucket window", s.QPM)
	}
}

func TestConcurrentRecording(t *testing.T) {
	c := New()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 400; j++ {
				c.Record(answered("node-" + string(rune('a'+i%3))))
				if j%50 == 0 {
					c.Snapshot()
				}
			}
		}(i)
	}
	wg.Wait()

	if got := c.Snapshot().Total; got != 8*400 {
		t.Errorf("total = %d, want %d", got, 8*400)
	}
}
