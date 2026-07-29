// Package fallback provides the balancer's last line of defence: public
// resolvers used only when no Pi-hole can answer, together with the reporting
// that keeps that fact visible without flooding the log.
//
// Queries answered here are unfiltered. That is the whole trade: during a
// total Pi-hole outage the network keeps working, and ads stop being blocked
// until a Pi-hole comes back. The tracker exists so this never happens
// quietly — an outage that lasted three days would otherwise look exactly
// like a working network.
package fallback

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chinmay28/hole-balancer/internal/config"
	"github.com/chinmay28/hole-balancer/internal/dnsclient"
	"github.com/chinmay28/hole-balancer/internal/dnsmsg"
	"github.com/chinmay28/hole-balancer/internal/metrics"
)

// ErrDisabled is returned when a resolution is attempted while fallback is
// turned off.
var ErrDisabled = errors.New("fallback: disabled")

// Resolver forwards a query to the configured public resolvers.
type Resolver struct {
	timeout time.Duration
	retry   func(dnsmsg.RCode) bool
	tracker *Tracker
	metrics *metrics.Metrics
	log     *slog.Logger

	// mu guards servers, which the management interface can replace while
	// queries are in flight.
	mu      sync.RWMutex
	servers []string

	rr atomic.Uint64
}

// NewResolver builds the fallback resolver described by the configuration.
func NewResolver(cfg *config.Config, tracker *Tracker, m *metrics.Metrics, log *slog.Logger) *Resolver {
	r := &Resolver{
		timeout: cfg.Fallback.Timeout.D(),
		retry:   cfg.Query.ShouldRetryRCode,
		tracker: tracker,
		metrics: m,
		log:     log,
	}
	if cfg.Fallback.Enabled {
		r.servers = append(r.servers, cfg.Fallback.Servers...)
	}
	return r
}

// Enabled reports whether any public resolver is configured.
func (r *Resolver) Enabled() bool {
	if r == nil {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.servers) > 0
}

// Servers lists the configured public resolvers.
func (r *Resolver) Servers() []string {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]string(nil), r.servers...)
}

// SetServers replaces the resolver list. Passing enabled false clears it, which
// is what makes Enabled report false without a second flag to keep in step.
//
// Addresses are expected to be normalised already; the management layer does
// that so a bad address is rejected before anything is changed.
func (r *Resolver) SetServers(enabled bool, servers []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !enabled {
		r.servers = nil
		return
	}
	r.servers = append([]string(nil), servers...)
}

// Resolve forwards req to the public resolvers, trying each in turn until one
// answers. It returns the response and the resolver that produced it.
//
// The starting point rotates, so an outage that lasts a while spreads across
// the configured resolvers instead of hammering the first one.
func (r *Resolver) Resolve(ctx context.Context, req []byte, proto string) ([]byte, string, error) {
	if !r.Enabled() {
		return nil, "", ErrDisabled
	}

	servers := r.Servers()
	n := len(servers)
	if n == 0 {
		return nil, "", ErrDisabled
	}
	start := int(r.rr.Add(1)-1) % n
	var lastErr error

	for i := 0; i < n; i++ {
		if ctx.Err() != nil {
			return nil, "", ctx.Err()
		}
		server := servers[(start+i)%n]

		resp, _, err := dnsclient.Exchange(ctx, proto, server, req, r.timeout)
		if err != nil {
			lastErr = err
			r.record(server, false)
			r.metrics.FallbackFailures.Inc(
				metrics.Label{Name: "server", Value: server},
				metrics.Label{Name: "reason", Value: dnsclient.Classify(err)},
			)
			continue
		}

		hdr, _ := dnsmsg.ParseHeader(resp)
		if r.retry != nil && r.retry(hdr.RCode) && i < n-1 {
			lastErr = fmt.Errorf("fallback resolver %s returned %s", server, hdr.RCode)
			r.record(server, false)
			r.metrics.FallbackFailures.Inc(
				metrics.Label{Name: "server", Value: server},
				metrics.Label{Name: "reason", Value: "rcode_" + hdr.RCode.String()},
			)
			continue
		}

		r.record(server, true)
		r.metrics.FallbackResponses.Inc(
			metrics.Label{Name: "server", Value: server},
			metrics.Label{Name: "rcode", Value: hdr.RCode.String()},
		)
		return resp, server, nil
	}

	if lastErr == nil {
		lastErr = errors.New("fallback: no resolver answered")
	}
	return nil, "", lastErr
}

func (r *Resolver) record(server string, ok bool) {
	if r.tracker != nil {
		r.tracker.RecordQuery(server, ok)
	}
}

// Tracker accumulates fallback usage and reports it on a schedule.
//
// Nothing here logs per query. A house that loses both Pi-holes for an hour
// generates tens of thousands of fallback queries; one line a day that says so
// is far more useful than a log nobody can read.
type Tracker struct {
	interval time.Duration
	log      *slog.Logger

	// now is injectable so the summary logic can be tested without waiting a
	// day for a ticker.
	now func() time.Time

	mu          sync.Mutex
	windowStart time.Time
	queries     uint64
	failures    uint64
	perServer   map[string]uint64
	episodes    int
	total       time.Duration
	longest     time.Duration
	inOutage    bool
	outageStart time.Time
}

// NewTracker creates a usage tracker reporting every cfg.Fallback.SummaryInterval.
func NewTracker(cfg *config.Config, log *slog.Logger) *Tracker {
	t := &Tracker{
		interval:  cfg.Fallback.SummaryInterval.D(),
		log:       log,
		now:       time.Now,
		perServer: make(map[string]uint64),
	}
	t.windowStart = t.now()
	return t
}

// Observe records whether every Pi-hole is currently unable to answer. It is
// idempotent, so it can be called from the health checker and from endpoint
// state changes without double-counting an outage.
func (t *Tracker) Observe(allDown bool) {
	t.mu.Lock()
	defer t.mu.Unlock()

	now := t.now()
	switch {
	case allDown && !t.inOutage:
		t.inOutage = true
		t.outageStart = now
		t.episodes++
	case !allDown && t.inOutage:
		t.inOutage = false
		t.addOutageLocked(now.Sub(t.outageStart))
	}
}

func (t *Tracker) addOutageLocked(d time.Duration) {
	t.total += d
	if d > t.longest {
		t.longest = d
	}
}

// RecordQuery counts one query sent to a public resolver.
func (t *Tracker) RecordQuery(server string, ok bool) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.queries++
	if ok {
		t.perServer[server]++
	} else {
		t.failures++
	}
}

// Summary is one reporting window's worth of fallback activity.
type Summary struct {
	Start         time.Time         `json:"window_start"`
	End           time.Time         `json:"window_end"`
	Queries       uint64            `json:"queries"`
	Failures      uint64            `json:"failures"`
	PerServer     map[string]uint64 `json:"per_server"`
	Episodes      int               `json:"outages"`
	TotalOutage   time.Duration     `json:"-"`
	LongestOutage time.Duration     `json:"-"`
	Ongoing       bool              `json:"outage_ongoing"`
}

// Empty reports whether the window saw no fallback activity at all, in which
// case there is nothing worth logging.
func (s Summary) Empty() bool {
	return s.Queries == 0 && s.Episodes == 0 && !s.Ongoing
}

// Snapshot returns the current window without clearing it, for the status API.
func (t *Tracker) Snapshot() Summary {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.snapshotLocked(t.now())
}

func (t *Tracker) snapshotLocked(now time.Time) Summary {
	s := Summary{
		Start:         t.windowStart,
		End:           now,
		Queries:       t.queries,
		Failures:      t.failures,
		PerServer:     make(map[string]uint64, len(t.perServer)),
		Episodes:      t.episodes,
		TotalOutage:   t.total,
		LongestOutage: t.longest,
		Ongoing:       t.inOutage,
	}
	for k, v := range t.perServer {
		s.PerServer[k] = v
	}
	// An outage still in progress has not been added to the totals yet, but it
	// is the most interesting thing in the report, so count it so far.
	if t.inOutage {
		d := now.Sub(t.outageStart)
		s.TotalOutage += d
		if d > s.LongestOutage {
			s.LongestOutage = d
		}
	}
	return s
}

// take snapshots the window and starts a new one.
func (t *Tracker) take() Summary {
	t.mu.Lock()
	defer t.mu.Unlock()

	now := t.now()
	s := t.snapshotLocked(now)

	t.windowStart = now
	t.queries, t.failures = 0, 0
	t.perServer = make(map[string]uint64)
	t.episodes, t.total, t.longest = 0, 0, 0
	if t.inOutage {
		// The outage continues into the next window; only the part that has
		// already been reported is discarded.
		t.outageStart = now
		t.episodes = 1
	}
	return s
}

// Run emits a summary every interval until ctx is cancelled, then emits a
// final one so activity since the last report is not lost on shutdown.
func (t *Tracker) Run(ctx context.Context) {
	ticker := time.NewTicker(t.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			t.Report()
			return
		case <-ticker.C:
			t.Report()
		}
	}
}

// Report writes one summary line, unless the window was quiet.
func (t *Tracker) Report() {
	s := t.take()
	if s.Empty() {
		return
	}

	t.log.Warn("public DNS fallback was used: these queries were NOT filtered by Pi-hole",
		"window", s.End.Sub(s.Start).Round(time.Second).String(),
		"since", s.Start.UTC().Format(time.RFC3339),
		"queries", s.Queries,
		"failed", s.Failures,
		"resolvers", formatServers(s.PerServer),
		"outages", s.Episodes,
		"outage_total", s.TotalOutage.Round(time.Second).String(),
		"outage_longest", s.LongestOutage.Round(time.Second).String(),
		"still_down", s.Ongoing,
	)
}

// formatServers renders the per-resolver counts in a stable order.
func formatServers(counts map[string]uint64) string {
	if len(counts) == 0 {
		return "none"
	}
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var sb strings.Builder
	for i, k := range keys {
		if i > 0 {
			sb.WriteByte(' ')
		}
		fmt.Fprintf(&sb, "%s=%d", k, counts[k])
	}
	return sb.String()
}
