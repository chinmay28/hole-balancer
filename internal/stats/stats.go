// Package stats accumulates the query statistics the management interface
// shows: totals, per-node shares, response-code mix, and a short history.
//
// This overlaps the Prometheus metrics on purpose. Those exist to be scraped by
// something else; these exist so the built-in dashboard works on a home network
// with no monitoring stack at all, and they carry a time series, which plain
// counters cannot.
//
// Nothing here records a domain name or a client address. The dashboard answers
// "how much and from where in the pool", not "who looked up what" — that is
// Pi-hole's job, and keeping it out of this process means the management page
// cannot leak a browsing history.
package stats

import (
	"sort"
	"sync"
	"time"
)

// History sizes: an hour at minute resolution and a day at hour resolution.
// Both are fixed-size ring buffers, so memory use does not grow with uptime.
const (
	minuteBuckets = 60
	hourBuckets   = 24
)

// Record is one completed client query, as the forwarder saw it.
type Record struct {
	Proto string // "udp" or "tcp"
	QType string // "A", "AAAA", …
	// Upstream and Endpoint name the Pi-hole that answered. Both are empty when
	// nothing did.
	Upstream string
	Endpoint string
	RCode    string
	Latency  time.Duration
	// Attempts counts every upstream tried, so anything above 1 is a retry.
	Attempts int
	// Fallback marks a query answered by a public resolver, which means it was
	// not filtered.
	Fallback bool
	// Failed marks a query no upstream and no fallback could answer.
	Failed bool
}

type nodeStat struct {
	Queries   uint64
	Failures  uint64
	Retries   uint64
	LatencySu time.Duration
	LatencyN  uint64
	Last      time.Time
}

type bucket struct {
	at       time.Time // start of the bucket's period; zero means unused
	queries  uint64
	fallback uint64
	failed   uint64
	blocked  uint64
}

// Collector is the running tally. It is safe for concurrent use.
type Collector struct {
	now func() time.Time // injectable for tests

	mu      sync.Mutex
	started time.Time

	total     uint64
	fallback  uint64
	failed    uint64
	retried   uint64
	latencySu time.Duration
	latencyN  uint64

	byProto    map[string]uint64
	byRCode    map[string]uint64
	byQType    map[string]uint64
	byUpstream map[string]*nodeStat

	minutes [minuteBuckets]bucket
	hours   [hourBuckets]bucket
}

// New creates an empty collector.
func New() *Collector {
	c := &Collector{
		now:        time.Now,
		byProto:    make(map[string]uint64),
		byRCode:    make(map[string]uint64),
		byQType:    make(map[string]uint64),
		byUpstream: make(map[string]*nodeStat),
	}
	c.started = c.now()
	return c
}

// Record adds one query to the tally.
func (c *Collector) Record(r Record) {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := c.now()
	c.total++
	if r.Proto != "" {
		c.byProto[r.Proto]++
	}
	if r.QType != "" {
		c.byQType[r.QType]++
	}
	if r.RCode != "" {
		c.byRCode[r.RCode]++
	}
	if r.Attempts > 1 {
		c.retried += uint64(r.Attempts - 1)
	}
	if r.Latency > 0 {
		c.latencySu += r.Latency
		c.latencyN++
	}
	switch {
	case r.Failed:
		c.failed++
	case r.Fallback:
		c.fallback++
	}

	if r.Upstream != "" {
		n := c.byUpstream[r.Upstream]
		if n == nil {
			n = &nodeStat{}
			c.byUpstream[r.Upstream] = n
		}
		n.Queries++
		n.Last = now
		if r.Attempts > 1 {
			n.Retries += uint64(r.Attempts - 1)
		}
		if r.Latency > 0 {
			n.LatencySu += r.Latency
			n.LatencyN++
		}
	}

	// NXDOMAIN is how Pi-hole reports a blocked domain, so this doubles as a
	// rough "how much was filtered" figure. It is approximate: a genuinely
	// non-existent name looks the same from here.
	blocked := r.RCode == "NXDOMAIN"
	c.addToBucket(&c.minutes[minuteIndex(now)], now.Truncate(time.Minute), r, blocked)
	c.addToBucket(&c.hours[hourIndex(now)], now.Truncate(time.Hour), r, blocked)
}

func (c *Collector) addToBucket(b *bucket, start time.Time, r Record, blocked bool) {
	// A ring slot is reused every hour (or day). If the slot belongs to an
	// older period, it is stale history and gets reset rather than added to.
	if !b.at.Equal(start) {
		*b = bucket{at: start}
	}
	b.queries++
	if r.Fallback {
		b.fallback++
	}
	if r.Failed {
		b.failed++
	}
	if blocked {
		b.blocked++
	}
}

func minuteIndex(t time.Time) int { return t.Minute() % minuteBuckets }
func hourIndex(t time.Time) int   { return t.Hour() % hourBuckets }

// ForgetUpstream drops a removed Pi-hole's counters, so a name that is no
// longer in the pool stops appearing in the dashboard.
func (c *Collector) ForgetUpstream(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.byUpstream, name)
}

// Reset clears every counter and the history.
func (c *Collector) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.total, c.fallback, c.failed, c.retried = 0, 0, 0, 0
	c.latencySu, c.latencyN = 0, 0
	c.byProto = make(map[string]uint64)
	c.byRCode = make(map[string]uint64)
	c.byQType = make(map[string]uint64)
	c.byUpstream = make(map[string]*nodeStat)
	c.minutes = [minuteBuckets]bucket{}
	c.hours = [hourBuckets]bucket{}
	c.started = c.now()
}

// Count is one labelled tally, sorted largest first when returned in a slice.
type Count struct {
	Name  string  `json:"name"`
	Count uint64  `json:"count"`
	Share float64 `json:"share"` // fraction of the total, 0..1
}

// NodeStat is one Pi-hole's contribution.
type NodeStat struct {
	Name     string  `json:"name"`
	Queries  uint64  `json:"queries"`
	Share    float64 `json:"share"`
	Retries  uint64  `json:"retries"`
	AvgMS    float64 `json:"avg_latency_ms"`
	LastSeen string  `json:"last_seen,omitempty"`
}

// Point is one bucket of the history.
type Point struct {
	At       string `json:"at"`
	Queries  uint64 `json:"queries"`
	Fallback uint64 `json:"fallback"`
	Failed   uint64 `json:"failed"`
	Blocked  uint64 `json:"blocked"`
}

// Snapshot is everything the dashboard needs in one JSON document.
type Snapshot struct {
	Since        string     `json:"since"`
	UptimeSec    float64    `json:"uptime_seconds"`
	Total        uint64     `json:"total_queries"`
	Fallback     uint64     `json:"fallback_queries"`
	Failed       uint64     `json:"failed_queries"`
	Retries      uint64     `json:"retries"`
	AvgMS        float64    `json:"avg_latency_ms"`
	QPS          float64    `json:"queries_per_second"`
	QPM          float64    `json:"queries_per_minute_recent"`
	BlockedShare float64    `json:"blocked_share"`
	TopNode      string     `json:"top_node,omitempty"`
	TopNodeCount uint64     `json:"top_node_queries"`
	Nodes        []NodeStat `json:"nodes"`
	Protos       []Count    `json:"protocols"`
	RCodes       []Count    `json:"rcodes"`
	QTypes       []Count    `json:"query_types"`
	Minutes      []Point    `json:"last_hour"`
	Hours        []Point    `json:"last_day"`
}

// Snapshot renders the current tally.
func (c *Collector) Snapshot() Snapshot {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := c.now()
	uptime := now.Sub(c.started)

	s := Snapshot{
		Since:     c.started.UTC().Format(time.RFC3339),
		UptimeSec: uptime.Seconds(),
		Total:     c.total,
		Fallback:  c.fallback,
		Failed:    c.failed,
		Retries:   c.retried,
		Protos:    counts(c.byProto, c.total),
		RCodes:    counts(c.byRCode, c.total),
		QTypes:    counts(c.byQType, c.total),
		Minutes:   series(c.minutes[:], now, time.Minute, minuteBuckets),
		Hours:     series(c.hours[:], now, time.Hour, hourBuckets),
	}

	if c.latencyN > 0 {
		s.AvgMS = float64(c.latencySu) / float64(c.latencyN) / float64(time.Millisecond)
	}
	if secs := uptime.Seconds(); secs > 0 {
		s.QPS = float64(c.total) / secs
	}
	if c.total > 0 {
		s.BlockedShare = float64(c.byRCode["NXDOMAIN"]) / float64(c.total)
	}

	// The recent rate is more useful than the lifetime average on a dashboard
	// that is open right now.
	var recent uint64
	for _, p := range s.Minutes {
		recent += p.Queries
	}
	if n := len(s.Minutes); n > 0 {
		s.QPM = float64(recent) / float64(n)
	}

	for name, n := range c.byUpstream {
		ns := NodeStat{Name: name, Queries: n.Queries, Retries: n.Retries}
		if c.total > 0 {
			ns.Share = float64(n.Queries) / float64(c.total)
		}
		if n.LatencyN > 0 {
			ns.AvgMS = float64(n.LatencySu) / float64(n.LatencyN) / float64(time.Millisecond)
		}
		if !n.Last.IsZero() {
			ns.LastSeen = n.Last.UTC().Format(time.RFC3339)
		}
		s.Nodes = append(s.Nodes, ns)
	}
	sort.Slice(s.Nodes, func(i, j int) bool {
		if s.Nodes[i].Queries != s.Nodes[j].Queries {
			return s.Nodes[i].Queries > s.Nodes[j].Queries
		}
		return s.Nodes[i].Name < s.Nodes[j].Name
	})
	if len(s.Nodes) > 0 && s.Nodes[0].Queries > 0 {
		s.TopNode = s.Nodes[0].Name
		s.TopNodeCount = s.Nodes[0].Queries
	}
	return s
}

// counts turns a tally map into a slice sorted largest first, with ties broken
// by name so the dashboard does not reshuffle between refreshes.
func counts(m map[string]uint64, total uint64) []Count {
	out := make([]Count, 0, len(m))
	for name, n := range m {
		c := Count{Name: name, Count: n}
		if total > 0 {
			c.Share = float64(n) / float64(total)
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// series unrolls a ring buffer into oldest-to-newest order, filling periods
// that saw no traffic with zeroes so the chart's x-axis is evenly spaced.
func series(ring []bucket, now time.Time, step time.Duration, n int) []Point {
	out := make([]Point, 0, n)
	newest := now.Truncate(step)

	for i := n - 1; i >= 0; i-- {
		at := newest.Add(-time.Duration(i) * step)
		p := Point{At: at.UTC().Format(time.RFC3339), Queries: 0}
		var idx int
		if step == time.Minute {
			idx = minuteIndex(at)
		} else {
			idx = hourIndex(at)
		}
		if b := ring[idx]; b.at.Equal(at) {
			p.Queries, p.Fallback, p.Failed, p.Blocked = b.queries, b.fallback, b.failed, b.blocked
		}
		out = append(out, p)
	}
	return out
}
