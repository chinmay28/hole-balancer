// Package health keeps the pool's view of each upstream path current by
// probing every endpoint on a fixed interval.
//
// Probing runs against every endpoint, including ones already believed to be
// down: live client traffic never reaches a down endpoint, so active probes
// are the only thing that can bring one back.
package health

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/chinmay28/hole-balancer/internal/config"
	"github.com/chinmay28/hole-balancer/internal/dnsclient"
	"github.com/chinmay28/hole-balancer/internal/dnsmsg"
	"github.com/chinmay28/hole-balancer/internal/metrics"
	"github.com/chinmay28/hole-balancer/internal/pool"
)

// Checker probes upstream endpoints on a schedule.
type Checker struct {
	cfg     *config.Config
	pool    *pool.Pool
	metrics *metrics.Metrics
	log     *slog.Logger
}

// New creates a health checker for the given pool.
func New(cfg *config.Config, p *pool.Pool, m *metrics.Metrics, log *slog.Logger) *Checker {
	return &Checker{cfg: cfg, pool: p, metrics: m, log: log}
}

// Bootstrap probes every endpoint once and applies the result directly, so
// the balancer starts with an accurate view instead of an empty one. It is
// bounded by health.startup_timeout and never returns an error: an upstream
// that does not answer in time is simply marked down and picked up by the
// next sweep.
func (c *Checker) Bootstrap(ctx context.Context) {
	timeout := c.cfg.Health.StartupTimeout.D()
	if timeout <= 0 {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	c.forEachEndpoint(func(e *pool.Endpoint) {
		latency, err := c.probe(ctx, e)
		c.countCheck(e, err)
		c.pool.SetInitial(e, err == nil, latency, err)
	})

	c.log.Info("initial health sweep complete",
		"healthy_upstreams", c.pool.HealthyUpstreams(),
		"total_upstreams", len(c.pool.Upstreams()))
}

// Run probes on health.interval until ctx is cancelled.
func (c *Checker) Run(ctx context.Context) {
	interval := c.cfg.Health.Interval.D()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.Sweep(ctx)
		}
	}
}

// Sweep probes every endpoint once, in parallel, and feeds the results into
// the pool's rise/fall tracking.
func (c *Checker) Sweep(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, c.cfg.Health.Interval.D())
	defer cancel()

	c.forEachEndpoint(func(e *pool.Endpoint) {
		latency, err := c.probe(ctx, e)
		c.countCheck(e, err)
		c.pool.ReportProbe(e, err == nil, latency, err)
	})
}

func (c *Checker) forEachEndpoint(fn func(*pool.Endpoint)) {
	var wg sync.WaitGroup
	for _, e := range c.pool.Endpoints() {
		wg.Add(1)
		go func(e *pool.Endpoint) {
			defer wg.Done()
			fn(e)
		}(e)
	}
	wg.Wait()
}

func (c *Checker) countCheck(e *pool.Endpoint, err error) {
	result := "success"
	if err != nil {
		result = "failure"
	}
	c.metrics.HealthChecks.Inc(
		metrics.Label{Name: "upstream", Value: e.Upstream.Name},
		metrics.Label{Name: "endpoint", Value: e.Name},
		metrics.Label{Name: "result", Value: result},
	)
}

// probe sends one query to an endpoint and judges the reply.
//
// Anything that comes back as a well-formed DNS response counts as alive,
// including NXDOMAIN. That matters here: the probe name may well be on a
// blocklist on one Pi-hole and not another, and a filtered name is proof the
// server is working, not proof that it is broken. Only a server that cannot
// answer at all — timeout, refusal, or SERVFAIL — is treated as down.
func (c *Checker) probe(ctx context.Context, e *pool.Endpoint) (time.Duration, error) {
	// #nosec G404 -- transaction IDs need to be unpredictable to off-path
	// observers only; math/rand/v2 is seeded from the runtime's secure source.
	query, err := dnsmsg.BuildQuery(uint16(rand.UintN(1<<16)), c.cfg.Health.Probe.Name, c.cfg.Health.Probe.QType())
	if err != nil {
		return 0, fmt.Errorf("building probe query: %w", err)
	}

	resp, rtt, err := dnsclient.Exchange(ctx, dnsclient.ProtoUDP, e.Addr, query, c.cfg.Health.Timeout.D())
	if err != nil {
		return rtt, err
	}

	hdr, err := dnsmsg.ParseHeader(resp)
	if err != nil {
		return rtt, fmt.Errorf("probe response: %w", err)
	}
	switch hdr.RCode {
	case dnsmsg.RCodeServFail, dnsmsg.RCodeRefused, dnsmsg.RCodeNotImp:
		return rtt, fmt.Errorf("probe returned %s", hdr.RCode)
	}
	if c.cfg.Health.Probe.RequireAnswer && hdr.ANCount == 0 {
		return rtt, fmt.Errorf("probe returned %s with no answer records", hdr.RCode)
	}
	return rtt, nil
}
