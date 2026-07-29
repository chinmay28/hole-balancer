// Package metrics implements the small counter and histogram types the
// balancer exports, plus rendering in Prometheus text exposition format.
//
// Hand-rolling this keeps the binary free of a client library for what
// amounts to a dozen series.
package metrics

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

// Label is a single metric dimension.
type Label struct {
	Name  string
	Value string
}

// CounterVec is a set of monotonically increasing counters keyed by labels.
type CounterVec struct {
	name string
	help string

	mu     sync.RWMutex
	series map[string]*series
}

type series struct {
	labels []Label
	value  atomic.Uint64
}

// NewCounterVec creates a labelled counter.
func NewCounterVec(name, help string) *CounterVec {
	return &CounterVec{name: name, help: help, series: make(map[string]*series)}
}

// Inc adds one to the counter identified by labels.
func (c *CounterVec) Inc(labels ...Label) { c.Add(1, labels...) }

// Add increases the counter identified by labels.
func (c *CounterVec) Add(n uint64, labels ...Label) {
	key := labelKey(labels)

	c.mu.RLock()
	s, ok := c.series[key]
	c.mu.RUnlock()
	if !ok {
		c.mu.Lock()
		if s, ok = c.series[key]; !ok {
			s = &series{labels: labels}
			c.series[key] = s
		}
		c.mu.Unlock()
	}
	s.value.Add(n)
}

// Value returns the current value for a label set, mainly for tests.
func (c *CounterVec) Value(labels ...Label) uint64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if s, ok := c.series[labelKey(labels)]; ok {
		return s.value.Load()
	}
	return 0
}

func (c *CounterVec) writeTo(sb *strings.Builder) {
	c.mu.RLock()
	keys := make([]string, 0, len(c.series))
	for k := range c.series {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	snapshot := make([]*series, 0, len(keys))
	for _, k := range keys {
		snapshot = append(snapshot, c.series[k])
	}
	c.mu.RUnlock()

	if len(snapshot) == 0 {
		return
	}
	fmt.Fprintf(sb, "# HELP %s %s\n# TYPE %s counter\n", c.name, c.help, c.name)
	for _, s := range snapshot {
		fmt.Fprintf(sb, "%s%s %d\n", c.name, renderLabels(s.labels), s.value.Load())
	}
}

// Histogram is a cumulative histogram with fixed bucket boundaries, in
// seconds.
type Histogram struct {
	name     string
	help     string
	bounds   []float64
	buckets  []atomic.Uint64
	count    atomic.Uint64
	sumMicro atomic.Uint64 // sum in microseconds, to stay in integer atomics
}

// NewHistogram creates a histogram with the given upper bounds in seconds.
func NewHistogram(name, help string, bounds []float64) *Histogram {
	return &Histogram{
		name:    name,
		help:    help,
		bounds:  bounds,
		buckets: make([]atomic.Uint64, len(bounds)),
	}
}

// Observe records one sample, in seconds.
func (h *Histogram) Observe(seconds float64) {
	h.count.Add(1)
	if seconds > 0 {
		h.sumMicro.Add(uint64(seconds * 1e6))
	}
	for i, b := range h.bounds {
		if seconds <= b {
			h.buckets[i].Add(1)
		}
	}
}

func (h *Histogram) writeTo(sb *strings.Builder) {
	fmt.Fprintf(sb, "# HELP %s %s\n# TYPE %s histogram\n", h.name, h.help, h.name)
	for i, b := range h.bounds {
		fmt.Fprintf(sb, "%s_bucket{le=\"%g\"} %d\n", h.name, b, h.buckets[i].Load())
	}
	count := h.count.Load()
	fmt.Fprintf(sb, "%s_bucket{le=\"+Inf\"} %d\n", h.name, count)
	fmt.Fprintf(sb, "%s_sum %f\n", h.name, float64(h.sumMicro.Load())/1e6)
	fmt.Fprintf(sb, "%s_count %d\n", h.name, count)
}

// Metrics is the balancer's full metric set.
type Metrics struct {
	Queries      *CounterVec // by proto
	Responses    *CounterVec // by upstream, endpoint, rcode
	Failures     *CounterVec // by upstream, endpoint, reason
	Retries      *CounterVec // by reason
	Servfails    *CounterVec // queries the balancer could not answer at all
	HealthChecks *CounterVec // by upstream, endpoint, result
	StateFlips   *CounterVec // by upstream, endpoint, state
	Duration     *Histogram
}

// New creates the metric set with all series empty.
func New() *Metrics {
	m := &Metrics{
		Queries:      NewCounterVec("holebalancer_queries_total", "Client queries received, by transport."),
		Responses:    NewCounterVec("holebalancer_responses_total", "Responses returned to clients, by upstream and response code."),
		Failures:     NewCounterVec("holebalancer_upstream_failures_total", "Failed upstream exchanges, by reason."),
		Retries:      NewCounterVec("holebalancer_retries_total", "Query attempts beyond the first, by reason."),
		Servfails:    NewCounterVec("holebalancer_servfail_total", "Queries that exhausted every upstream."),
		HealthChecks: NewCounterVec("holebalancer_health_checks_total", "Active health probes, by result."),
		StateFlips:   NewCounterVec("holebalancer_endpoint_state_changes_total", "Endpoint health transitions."),
		Duration: NewHistogram("holebalancer_query_duration_seconds",
			"End-to-end time to answer a client query, including retries.",
			[]float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2, 5}),
	}

	// Publish the unlabelled counters at zero straight away. A series that
	// only appears after the first failure makes `rate(...)` alerts silently
	// evaluate to nothing for as long as everything is working.
	m.Servfails.Add(0)
	return m
}

// Gauge is a single labelled sample rendered at scrape time.
type Gauge struct {
	Name   string
	Help   string
	Labels []Label
	Value  float64
}

// Render produces the Prometheus exposition text for the metric set plus any
// gauges the caller derives live from the pool.
func (m *Metrics) Render(gauges []Gauge) string {
	var sb strings.Builder
	m.Queries.writeTo(&sb)
	m.Responses.writeTo(&sb)
	m.Failures.writeTo(&sb)
	m.Retries.writeTo(&sb)
	m.Servfails.writeTo(&sb)
	m.HealthChecks.writeTo(&sb)
	m.StateFlips.writeTo(&sb)
	m.Duration.writeTo(&sb)

	byName := map[string][]Gauge{}
	var order []string
	for _, g := range gauges {
		if _, seen := byName[g.Name]; !seen {
			order = append(order, g.Name)
		}
		byName[g.Name] = append(byName[g.Name], g)
	}
	for _, name := range order {
		group := byName[name]
		fmt.Fprintf(&sb, "# HELP %s %s\n# TYPE %s gauge\n", name, group[0].Help, name)
		for _, g := range group {
			fmt.Fprintf(&sb, "%s%s %g\n", name, renderLabels(g.Labels), g.Value)
		}
	}
	return sb.String()
}

func labelKey(labels []Label) string {
	var sb strings.Builder
	for _, l := range labels {
		sb.WriteString(l.Name)
		sb.WriteByte('\x00')
		sb.WriteString(l.Value)
		sb.WriteByte('\x00')
	}
	return sb.String()
}

func renderLabels(labels []Label) string {
	if len(labels) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteByte('{')
	for i, l := range labels {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(l.Name)
		sb.WriteString(`="`)
		sb.WriteString(escapeLabelValue(l.Value))
		sb.WriteByte('"')
	}
	sb.WriteByte('}')
	return sb.String()
}

func escapeLabelValue(v string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	return r.Replace(v)
}
