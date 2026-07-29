// Package admin exposes the balancer's status, metrics, and maintenance
// endpoints over HTTP.
package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/chinmay28/hole-balancer/internal/config"
	"github.com/chinmay28/hole-balancer/internal/metrics"
	"github.com/chinmay28/hole-balancer/internal/pool"
)

// Server is the HTTP admin interface.
type Server struct {
	cfg     *config.Config
	pool    *pool.Pool
	metrics *metrics.Metrics
	log     *slog.Logger
	version string
	started time.Time
}

// New creates the admin server.
func New(cfg *config.Config, p *pool.Pool, m *metrics.Metrics, log *slog.Logger, version string) *Server {
	return &Server{cfg: cfg, pool: p, metrics: m, log: log, version: version, started: time.Now()}
}

// Handler builds the admin route table.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /status", s.handleStatus)
	mux.HandleFunc("GET /metrics", s.handleMetrics)
	mux.HandleFunc("GET /{$}", s.handleIndex)
	if s.cfg.Admin.AllowControl {
		mux.HandleFunc("POST /drain", s.handleDrain(true))
		mux.HandleFunc("POST /undrain", s.handleDrain(false))
	}
	return mux
}

// ListenAndServe runs the admin server until ctx is cancelled. It is a no-op
// when admin.listen is empty.
func (s *Server) ListenAndServe(ctx context.Context) error {
	if s.cfg.Admin.Listen == "" {
		<-ctx.Done()
		return nil
	}

	srv := &http.Server{
		Addr:              s.cfg.Admin.Listen,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		s.log.Info("admin interface listening", "addr", s.cfg.Admin.Listen,
			"control_enabled", s.cfg.Admin.AllowControl)
		errCh <- srv.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("admin server: %w", err)
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		return nil
	}
}

// handleHealthz is the liveness and readiness probe. It fails once no
// upstream can answer, which is exactly when a supervisor should notice.
func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	healthy := s.pool.HealthyUpstreams()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if healthy == 0 {
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprintln(w, "unhealthy: no upstream Pi-hole is answering")
		return
	}
	fmt.Fprintf(w, "ok: %d/%d upstreams healthy\n", healthy, len(s.pool.Upstreams()))
}

type statusResponse struct {
	Version   string                `json:"version"`
	UptimeSec float64               `json:"uptime_seconds"`
	Strategy  string                `json:"strategy"`
	Healthy   int                   `json:"healthy_upstreams"`
	Total     int                   `json:"total_upstreams"`
	Upstreams []pool.UpstreamStatus `json:"upstreams"`
}

func (s *Server) handleStatus(w http.ResponseWriter, _ *http.Request) {
	resp := statusResponse{
		Version:   s.version,
		UptimeSec: time.Since(s.started).Seconds(),
		Strategy:  s.cfg.Strategy,
		Healthy:   s.pool.HealthyUpstreams(),
		Total:     len(s.pool.Upstreams()),
		Upstreams: s.pool.Status(),
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(resp); err != nil {
		s.log.Debug("writing status response failed", "error", err)
	}
}

func (s *Server) handleMetrics(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	fmt.Fprint(w, s.metrics.Render(s.gauges()))
}

// gauges derives the live-state series from the pool at scrape time, so
// health state is never stale or double-booked with the counters.
func (s *Server) gauges() []metrics.Gauge {
	var out []metrics.Gauge
	out = append(out, metrics.Gauge{
		Name:   "holebalancer_build_info",
		Help:   "Build information; always 1.",
		Labels: []metrics.Label{{Name: "version", Value: s.version}},
		Value:  1,
	})

	for _, u := range s.pool.Status() {
		out = append(out, metrics.Gauge{
			Name:   "holebalancer_upstream_up",
			Help:   "1 if at least one path to this Pi-hole is answering.",
			Labels: []metrics.Label{{Name: "upstream", Value: u.Name}},
			Value:  boolToFloat(u.Healthy),
		})
		out = append(out, metrics.Gauge{
			Name:   "holebalancer_upstream_drained",
			Help:   "1 if this Pi-hole has been drained for maintenance.",
			Labels: []metrics.Label{{Name: "upstream", Value: u.Name}},
			Value:  boolToFloat(u.Drained),
		})
	}

	for _, u := range s.pool.Status() {
		for _, e := range u.Endpoints {
			labels := []metrics.Label{
				{Name: "upstream", Value: u.Name},
				{Name: "endpoint", Value: e.Name},
				{Name: "addr", Value: e.Addr},
			}
			out = append(out, metrics.Gauge{
				Name:   "holebalancer_endpoint_up",
				Help:   "1 if this network path to a Pi-hole is answering.",
				Labels: labels,
				Value:  boolToFloat(e.Healthy),
			})
			out = append(out, metrics.Gauge{
				Name:   "holebalancer_endpoint_latency_seconds",
				Help:   "Exponentially weighted mean round-trip time for this path.",
				Labels: labels,
				Value:  e.LatencyMS / 1000,
			})
		}
	}
	return out
}

func boolToFloat(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// handleDrain takes an upstream out of rotation, or puts it back. Draining is
// how you update a Pi-hole without any client seeing a timeout: queries stop
// going to it immediately, rather than after a health check notices.
func (s *Server) handleDrain(drain bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := r.URL.Query().Get("upstream")
		if name == "" {
			http.Error(w, "missing required query parameter: upstream", http.StatusBadRequest)
			return
		}
		u := s.pool.Lookup(name)
		if u == nil {
			http.Error(w, fmt.Sprintf("unknown upstream %q", name), http.StatusNotFound)
			return
		}
		u.SetDrained(drain)
		s.log.Info("upstream drain state changed", "upstream", name, "drained", drain)

		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		state := "returned to rotation"
		if drain {
			state = "drained"
		}
		fmt.Fprintf(w, "%s %s\n", name, state)
	}
}

func (s *Server) handleIndex(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprintf(w, "hole-balancer %s\n\n", s.version)
	fmt.Fprintf(w, "strategy: %s\n", s.cfg.Strategy)
	fmt.Fprintf(w, "upstreams: %d healthy of %d\n\n", s.pool.HealthyUpstreams(), len(s.pool.Upstreams()))
	for _, u := range s.pool.Status() {
		state := "DOWN"
		if u.Healthy {
			state = "UP"
		}
		if u.Drained {
			state += " (drained)"
		}
		fmt.Fprintf(w, "%-20s %-16s weight=%d\n", u.Name, state, u.Weight)
		for _, e := range u.Endpoints {
			mark := " "
			if e.IsPreferred {
				mark = "*"
			}
			epState := "down"
			if e.Healthy {
				epState = "up"
			}
			fmt.Fprintf(w, "  %s %-24s %-5s %6.1fms  queries=%d failures=%d",
				mark, e.Addr, epState, e.LatencyMS, e.Queries, e.Failures)
			if !e.Healthy && e.LastError != "" {
				// Without this an endpoint can read as "down, failures=0",
				// which is what a path that only ever failed its probes looks
				// like. The reason is the useful part.
				fmt.Fprintf(w, "  reason=%q", e.LastError)
			}
			fmt.Fprintln(w)
		}
	}
	fmt.Fprint(w, "\nendpoints: /healthz  /status  /metrics\n")
	if s.cfg.Admin.AllowControl {
		fmt.Fprint(w, "control:   POST /drain?upstream=NAME  POST /undrain?upstream=NAME\n")
	}
}
