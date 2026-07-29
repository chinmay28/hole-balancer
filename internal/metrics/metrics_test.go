package metrics

import (
	"strings"
	"sync"
	"testing"
)

func TestCounterVecAccumulatesPerLabelSet(t *testing.T) {
	c := NewCounterVec("test_total", "help")
	udp := Label{Name: "proto", Value: "udp"}
	tcp := Label{Name: "proto", Value: "tcp"}

	c.Inc(udp)
	c.Inc(udp)
	c.Add(5, tcp)

	if got := c.Value(udp); got != 2 {
		t.Errorf("udp = %d, want 2", got)
	}
	if got := c.Value(tcp); got != 5 {
		t.Errorf("tcp = %d, want 5", got)
	}
	if got := c.Value(Label{Name: "proto", Value: "sctp"}); got != 0 {
		t.Errorf("unseen label set = %d, want 0", got)
	}
}

func TestCounterVecIsConcurrencySafe(t *testing.T) {
	c := NewCounterVec("test_total", "help")
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			l := Label{Name: "worker", Value: string(rune('a' + i%4))}
			for j := 0; j < 500; j++ {
				c.Inc(l)
			}
		}(i)
	}
	wg.Wait()

	total := uint64(0)
	for i := 0; i < 4; i++ {
		total += c.Value(Label{Name: "worker", Value: string(rune('a' + i))})
	}
	if total != 16*500 {
		t.Errorf("total = %d, want %d", total, 16*500)
	}
}

func TestHistogramBucketsAreCumulative(t *testing.T) {
	h := NewHistogram("test_seconds", "help", []float64{0.01, 0.1, 1})
	for _, v := range []float64{0.005, 0.05, 0.5, 5} {
		h.Observe(v)
	}

	out := renderOnly(h)
	for _, want := range []string{
		`test_seconds_bucket{le="0.01"} 1`,
		`test_seconds_bucket{le="0.1"} 2`,
		`test_seconds_bucket{le="1"} 3`,
		`test_seconds_bucket{le="+Inf"} 4`,
		`test_seconds_count 4`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q\n---\n%s", want, out)
		}
	}
	if !strings.Contains(out, "test_seconds_sum 5.555") {
		t.Errorf("sum is wrong\n---\n%s", out)
	}
}

func renderOnly(h *Histogram) string {
	var sb strings.Builder
	h.writeTo(&sb)
	return sb.String()
}

// A counter that only materialises after its first failure makes rate() alerts
// evaluate to nothing while everything is working.
func TestUnlabelledCountersArePublishedAtZero(t *testing.T) {
	out := New().Render(nil)
	if !strings.Contains(out, "holebalancer_servfail_total 0") {
		t.Errorf("servfail counter should be present at zero\n---\n%s", out)
	}
}

func TestRenderGroupsGaugesUnderOneHeader(t *testing.T) {
	out := New().Render([]Gauge{
		{Name: "g_up", Help: "help", Labels: []Label{{Name: "u", Value: "a"}}, Value: 1},
		{Name: "g_up", Help: "help", Labels: []Label{{Name: "u", Value: "b"}}, Value: 0},
	})

	if n := strings.Count(out, "# TYPE g_up gauge"); n != 1 {
		t.Errorf("TYPE line appears %d times, want exactly 1\n---\n%s", n, out)
	}
	for _, want := range []string{`g_up{u="a"} 1`, `g_up{u="b"} 0`} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q", want)
		}
	}
}

func TestLabelValuesAreEscaped(t *testing.T) {
	c := NewCounterVec("test_total", "help")
	c.Inc(Label{Name: "reason", Value: `he said "no"` + "\n" + `path\to`})

	var sb strings.Builder
	c.writeTo(&sb)
	out := sb.String()

	if !strings.Contains(out, `reason="he said \"no\"\npath\\to"`) {
		t.Errorf("label value was not escaped for the exposition format:\n%s", out)
	}
}

func TestRenderSkipsEmptyCounters(t *testing.T) {
	// Queries has no series until something is counted, so its header should
	// not be emitted at all.
	out := New().Render(nil)
	if strings.Contains(out, "holebalancer_queries_total{") {
		t.Errorf("an untouched labelled counter should emit no series\n---\n%s", out)
	}
}

func TestRenderIsDeterministic(t *testing.T) {
	m := New()
	for _, p := range []string{"udp", "tcp", "udp"} {
		m.Queries.Inc(Label{Name: "proto", Value: p})
	}
	if a, b := m.Render(nil), m.Render(nil); a != b {
		t.Error("two consecutive renders differ; series ordering is not stable")
	}
}
