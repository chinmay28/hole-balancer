// Package pool holds the set of upstream Pi-holes, tracks their health, and
// decides which one answers each query.
package pool

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chinmay28/hole-balancer/internal/config"
)

// ewmaAlpha weights the newest latency sample. Low enough that one slow query
// does not knock an endpoint out of the least-latency ordering.
const ewmaAlpha = 0.2

// Endpoint is one network path to an upstream: a Pi-hole's LAN address, its
// Tailscale address, and so on.
type Endpoint struct {
	Name string
	Addr string
	// Upstream is the Pi-hole this path leads to. Two endpoints of the same
	// upstream are the same machine, which is why selection happens at the
	// upstream level and only then descends to a path.
	Upstream *Upstream

	up atomic.Bool

	mu           sync.Mutex
	successes    int // consecutive
	failures     int // consecutive
	latencyEWMA  time.Duration
	lastChange   time.Time
	lastErr      string
	totalQueries uint64
	totalFails   uint64
}

// Healthy reports whether this path is currently in rotation.
func (e *Endpoint) Healthy() bool { return e.up.Load() }

// Latency returns the exponentially weighted mean round-trip time, or zero if
// nothing has been measured yet.
func (e *Endpoint) Latency() time.Duration {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.latencyEWMA
}

// Upstream is a single Pi-hole server together with every path to it.
type Upstream struct {
	Name      string
	Weight    int
	Endpoints []*Endpoint

	drained atomic.Bool
}

// Healthy reports whether any path to this Pi-hole is answering.
func (u *Upstream) Healthy() bool {
	for _, e := range u.Endpoints {
		if e.Healthy() {
			return true
		}
	}
	return false
}

// Active returns the most preferred healthy path, which is the first healthy
// entry in configuration order. Listing the LAN address first therefore keeps
// traffic off Tailscale while the LAN is up.
func (u *Upstream) Active() *Endpoint {
	for _, e := range u.Endpoints {
		if e.Healthy() {
			return e
		}
	}
	return nil
}

// Drained reports whether the operator has taken this Pi-hole out of rotation
// for maintenance.
func (u *Upstream) Drained() bool { return u.drained.Load() }

// SetDrained takes an upstream out of, or back into, rotation.
func (u *Upstream) SetDrained(v bool) { u.drained.Store(v) }

// StateChangeFunc is called whenever an endpoint transitions between up and
// down, so the caller can log it and update metrics.
type StateChangeFunc func(e *Endpoint, up bool, reason string)

// Pool is the collection of upstreams and the selection policy over them.
//
// The membership and the strategy can both change while queries are in flight,
// so that a Pi-hole can be added or removed from the management interface
// without a restart. mu guards those two; per-endpoint health lives on the
// endpoints themselves and is not covered by it.
type Pool struct {
	mu        sync.RWMutex
	upstreams []*Upstream
	endpoints []*Endpoint
	strategy  string

	rise    int
	fall    int
	passive bool

	rr       atomic.Uint64
	onChange StateChangeFunc
}

// New builds a pool from configuration. Every endpoint starts down; the
// initial probe sweep in package health is what brings the pool up, and the
// last-resort tier in Plan keeps queries flowing in the meantime.
func New(cfg *config.Config, onChange StateChangeFunc) *Pool {
	p := &Pool{
		strategy: cfg.Strategy,
		rise:     cfg.Health.Rise,
		fall:     cfg.Health.Fall,
		passive:  cfg.Health.Passive.Enabled,
		onChange: onChange,
	}
	if p.onChange == nil {
		p.onChange = func(*Endpoint, bool, string) {}
	}
	for _, uc := range cfg.Upstreams {
		u := buildUpstream(uc)
		p.upstreams = append(p.upstreams, u)
		p.endpoints = append(p.endpoints, u.Endpoints...)
	}
	return p
}

func buildUpstream(uc config.Upstream) *Upstream {
	u := &Upstream{Name: uc.Name, Weight: uc.Weight}
	for _, ec := range uc.Endpoints {
		u.Endpoints = append(u.Endpoints, &Endpoint{Name: ec.Name, Addr: ec.Addr, Upstream: u})
	}
	return u
}

// Errors returned when the pool's membership cannot be changed as asked.
var (
	ErrDuplicateName = errors.New("pool: an upstream with that name already exists")
	ErrNotFound      = errors.New("pool: no such upstream")
	ErrLastUpstream  = errors.New("pool: refusing to remove the only upstream")
)

// Add brings a new Pi-hole into rotation. Its endpoints start down and are
// picked up by the next health sweep, so a wrong address never silently
// swallows queries — though the fail-open tier will still try it if nothing
// else is healthy.
func (p *Pool) Add(uc config.Upstream) (*Upstream, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	for _, u := range p.upstreams {
		if u.Name == uc.Name {
			return nil, fmt.Errorf("%w: %s", ErrDuplicateName, uc.Name)
		}
		for _, e := range u.Endpoints {
			for _, ec := range uc.Endpoints {
				if e.Addr == ec.Addr {
					return nil, fmt.Errorf("address %s is already used by upstream %q", ec.Addr, u.Name)
				}
			}
		}
	}

	u := buildUpstream(uc)
	p.upstreams = append(p.upstreams, u)
	p.endpoints = append(p.endpoints, u.Endpoints...)
	return u, nil
}

// Remove takes a Pi-hole out of the pool entirely.
//
// Removing the last one is refused: an empty pool has nothing to fail over to
// and nothing to recover, which is a configuration mistake rather than a
// deployment anyone wants.
func (p *Pool) Remove(name string) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	idx := -1
	for i, u := range p.upstreams {
		if u.Name == name {
			idx = i
			break
		}
	}
	if idx < 0 {
		return fmt.Errorf("%w: %s", ErrNotFound, name)
	}
	if len(p.upstreams) == 1 {
		return ErrLastUpstream
	}

	removed := p.upstreams[idx]
	p.upstreams = append(p.upstreams[:idx:idx], p.upstreams[idx+1:]...)

	kept := make([]*Endpoint, 0, len(p.endpoints))
	for _, e := range p.endpoints {
		if e.Upstream != removed {
			kept = append(kept, e)
		}
	}
	p.endpoints = kept
	return nil
}

// Strategy returns the selection strategy currently in force.
func (p *Pool) Strategy() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.strategy
}

// SetStrategy switches how upstreams are chosen. The name must be one the
// configuration package recognises.
func (p *Pool) SetStrategy(name string) error {
	if !config.ValidStrategy(name) {
		return fmt.Errorf("pool: unknown strategy %q", name)
	}
	p.mu.Lock()
	p.strategy = name
	p.mu.Unlock()
	return nil
}

// Upstreams returns the configured upstreams in configuration order. The slice
// is a copy, so callers may range over it while the pool changes underneath.
func (p *Pool) Upstreams() []*Upstream {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return append([]*Upstream(nil), p.upstreams...)
}

// Endpoints returns a copy of every endpoint across every upstream.
func (p *Pool) Endpoints() []*Endpoint {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return append([]*Endpoint(nil), p.endpoints...)
}

// Lookup finds an upstream by name.
func (p *Pool) Lookup(name string) *Upstream {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, u := range p.upstreams {
		if u.Name == name {
			return u
		}
	}
	return nil
}

// HealthyUpstreams counts the Pi-holes currently answering.
func (p *Pool) HealthyUpstreams() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	n := 0
	for _, u := range p.upstreams {
		if u.Healthy() && !u.Drained() {
			n++
		}
	}
	return n
}

// Plan returns the endpoints to try for one query, best first.
//
// The ordering is tiered so that a retry moves to a different Pi-hole before
// it falls back to a slower path to the same one:
//
//  1. the preferred path of each healthy upstream, ordered by the configured
//     strategy, so the first entry is the selected upstream;
//  2. the remaining healthy paths, shuffled;
//  3. every path currently believed to be down, shuffled. This is the
//     fail-open tier: when nothing is healthy the balancer still tries, since
//     a stale health verdict is a worse outcome than a wasted round trip.
//
// Drained upstreams are skipped entirely unless they are all that is left.
func (p *Pool) Plan() []*Endpoint {
	p.mu.RLock()
	defer p.mu.RUnlock()

	var healthy, down, drained []*Upstream
	for _, u := range p.upstreams {
		switch {
		case u.Drained():
			drained = append(drained, u)
		case u.Healthy():
			healthy = append(healthy, u)
		default:
			down = append(down, u)
		}
	}

	ordered := p.orderLocked(healthy)
	plan := make([]*Endpoint, 0, len(p.endpoints))

	for _, u := range ordered {
		if e := u.Active(); e != nil {
			plan = append(plan, e)
		}
	}

	var secondary []*Endpoint
	for _, u := range ordered {
		active := u.Active()
		for _, e := range u.Endpoints {
			if e != active && e.Healthy() {
				secondary = append(secondary, e)
			}
		}
	}
	shuffle(secondary)
	plan = append(plan, secondary...)

	var lastResort []*Endpoint
	for _, u := range ordered {
		for _, e := range u.Endpoints {
			if !e.Healthy() {
				lastResort = append(lastResort, e)
			}
		}
	}
	for _, u := range down {
		lastResort = append(lastResort, u.Endpoints...)
	}
	shuffle(lastResort)
	plan = append(plan, lastResort...)

	if len(plan) == 0 {
		// Everything is drained. Answering from a draining Pi-hole beats
		// answering nothing at all.
		for _, u := range drained {
			plan = append(plan, u.Endpoints...)
		}
		shuffle(plan)
	}
	return plan
}

// order applies the selection strategy to the healthy upstreams. The entry it
// returns first is the one that serves the query; the rest are the retry
// order.
// orderLocked applies the selection strategy. The caller holds at least a read
// lock, which is what makes reading p.strategy here safe.
func (p *Pool) orderLocked(healthy []*Upstream) []*Upstream {
	if len(healthy) < 2 {
		return healthy
	}
	out := make([]*Upstream, len(healthy))
	copy(out, healthy)

	switch p.strategy {
	case config.StrategyFailover:
		// Configuration order already expresses the priority.

	case config.StrategyRoundRobin:
		n := len(out)
		start := int(p.rr.Add(1)-1) % n
		rotated := make([]*Upstream, 0, n)
		for i := 0; i < n; i++ {
			rotated = append(rotated, out[(start+i)%n])
		}
		out = rotated

	case config.StrategyLeastLatency:
		sort.SliceStable(out, func(i, j int) bool {
			return effectiveLatency(out[i]) < effectiveLatency(out[j])
		})

	default: // config.StrategyRandom
		// Weighted draw for the winner, then a shuffle so that retries are
		// spread evenly rather than always landing on the same second choice.
		shuffle(out)
		if i := weightedPick(out); i > 0 {
			out[0], out[i] = out[i], out[0]
		}
	}
	return out
}

// effectiveLatency is the measured latency of an upstream's preferred path.
// Endpoints with no sample yet sort last so that a single unmeasured upstream
// does not soak up all traffic before it has proved itself.
func effectiveLatency(u *Upstream) time.Duration {
	e := u.Active()
	if e == nil {
		return time.Hour
	}
	if l := e.Latency(); l > 0 {
		return l
	}
	return time.Hour
}

// weightedPick chooses an index in proportion to upstream weight.
func weightedPick(ups []*Upstream) int {
	total := 0
	for _, u := range ups {
		total += u.Weight
	}
	if total <= 0 {
		return rand.IntN(len(ups))
	}
	n := rand.IntN(total)
	for i, u := range ups {
		n -= u.Weight
		if n < 0 {
			return i
		}
	}
	return len(ups) - 1
}

func shuffle[T any](s []T) {
	rand.Shuffle(len(s), func(i, j int) { s[i], s[j] = s[j], s[i] })
}

// ReportProbe records the outcome of an active health check.
func (p *Pool) ReportProbe(e *Endpoint, ok bool, latency time.Duration, err error) {
	p.record(e, ok, latency, err, true)
}

// ReportQuery records the outcome of a real client query. Latency always
// feeds the moving average; whether the outcome moves the health verdict
// depends on health.passive.enabled.
func (p *Pool) ReportQuery(e *Endpoint, ok bool, latency time.Duration, err error) {
	e.mu.Lock()
	e.totalQueries++
	if !ok {
		e.totalFails++
	}
	e.mu.Unlock()
	p.record(e, ok, latency, err, p.passive)
}

func (p *Pool) record(e *Endpoint, ok bool, latency time.Duration, err error, affectsHealth bool) {
	e.mu.Lock()
	if ok && latency > 0 {
		if e.latencyEWMA == 0 {
			e.latencyEWMA = latency
		} else {
			e.latencyEWMA = time.Duration(float64(e.latencyEWMA)*(1-ewmaAlpha) + float64(latency)*ewmaAlpha)
		}
	}
	if !affectsHealth {
		e.mu.Unlock()
		return
	}

	var (
		flip   bool
		newUp  bool
		reason string
	)
	if ok {
		e.failures = 0
		e.successes++
		if !e.up.Load() && e.successes >= p.rise {
			flip, newUp = true, true
			reason = "recovered"
		}
	} else {
		e.successes = 0
		e.failures++
		if err != nil {
			e.lastErr = err.Error()
		}
		if e.up.Load() && e.failures >= p.fall {
			flip, newUp = true, false
			reason = e.lastErr
		}
	}
	if flip {
		e.lastChange = time.Now()
		if newUp {
			e.lastErr = ""
		}
	}
	e.mu.Unlock()

	if flip {
		e.up.Store(newUp)
		p.onChange(e, newUp, reason)
	}
}

// SetInitial records the very first verdict for an endpoint, bypassing the
// rise and fall thresholds.
//
// Those thresholds exist to damp flapping between two known states. At
// startup there is no previous state to protect, and making an operator wait
// several probe intervals before the pool comes up would be a worse default
// than trusting the first answer.
func (p *Pool) SetInitial(e *Endpoint, up bool, latency time.Duration, err error) {
	e.mu.Lock()
	if up {
		e.successes, e.failures = p.rise, 0
		e.latencyEWMA = latency
		e.lastErr = ""
	} else {
		e.successes, e.failures = 0, p.fall
		if err != nil {
			e.lastErr = err.Error()
		}
	}
	e.lastChange = time.Now()
	e.mu.Unlock()

	if e.up.Swap(up) != up {
		reason := "initial probe"
		if !up && err != nil {
			reason = err.Error()
		}
		p.onChange(e, up, reason)
	}
}

// EndpointStatus is a point-in-time view of one path, for the status API.
type EndpointStatus struct {
	Name         string  `json:"name"`
	Addr         string  `json:"addr"`
	Healthy      bool    `json:"healthy"`
	LatencyMS    float64 `json:"latency_ms"`
	Queries      uint64  `json:"queries"`
	Failures     uint64  `json:"failures"`
	LastChange   string  `json:"last_change,omitempty"`
	LastError    string  `json:"last_error,omitempty"`
	ConsecFails  int     `json:"consecutive_failures"`
	ConsecOKs    int     `json:"consecutive_successes"`
	IsPreferred  bool    `json:"preferred"`
	UpstreamName string  `json:"-"`
}

// UpstreamStatus is a point-in-time view of one Pi-hole.
type UpstreamStatus struct {
	Name      string           `json:"name"`
	Weight    int              `json:"weight"`
	Healthy   bool             `json:"healthy"`
	Drained   bool             `json:"drained"`
	Endpoints []EndpointStatus `json:"endpoints"`
}

// Status snapshots the whole pool.
func (p *Pool) Status() []UpstreamStatus {
	p.mu.RLock()
	defer p.mu.RUnlock()

	out := make([]UpstreamStatus, 0, len(p.upstreams))
	for _, u := range p.upstreams {
		active := u.Active()
		us := UpstreamStatus{
			Name:    u.Name,
			Weight:  u.Weight,
			Healthy: u.Healthy(),
			Drained: u.Drained(),
		}
		for _, e := range u.Endpoints {
			e.mu.Lock()
			es := EndpointStatus{
				Name:         e.Name,
				Addr:         e.Addr,
				Healthy:      e.up.Load(),
				LatencyMS:    float64(e.latencyEWMA) / float64(time.Millisecond),
				Queries:      e.totalQueries,
				Failures:     e.totalFails,
				LastError:    e.lastErr,
				ConsecFails:  e.failures,
				ConsecOKs:    e.successes,
				IsPreferred:  e == active,
				UpstreamName: u.Name,
			}
			if !e.lastChange.IsZero() {
				es.LastChange = e.lastChange.UTC().Format(time.RFC3339)
			}
			e.mu.Unlock()
			us.Endpoints = append(us.Endpoints, es)
		}
		out = append(out, us)
	}
	return out
}
