package admin

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/chinmay28/hole-balancer/internal/config"
	"github.com/chinmay28/hole-balancer/internal/control"
	"github.com/chinmay28/hole-balancer/internal/fallback"
	"github.com/chinmay28/hole-balancer/internal/metrics"
	"github.com/chinmay28/hole-balancer/internal/pool"
	"github.com/chinmay28/hole-balancer/internal/stats"
)

func newTestAdmin(t *testing.T, allowControl bool) (*Server, *pool.Pool) {
	s, p, _ := newTestAdminFull(t, allowControl, "")
	return s, p
}

// newTestAdminFull also hands back the manager, for the tests that make
// changes. Passing a config path makes those changes persist to that file.
func newTestAdminFull(t *testing.T, allowControl bool, path string) (*Server, *pool.Pool, *stats.Collector) {
	t.Helper()

	cfg := config.Default()
	cfg.Admin.AllowControl = allowControl
	cfg.Upstreams = []config.Upstream{
		{Name: "pihole-1", Weight: 1, Endpoints: []config.Endpoint{
			{Name: "lan", Addr: "10.0.0.1:53"},
			{Name: "tailscale", Addr: "100.64.0.1:53"},
		}},
		{Name: "pihole-2", Weight: 2, Endpoints: []config.Endpoint{{Name: "lan", Addr: "10.0.0.2:53"}}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}

	p := pool.New(&cfg, nil)
	m := metrics.New()
	log := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	tracker := fallback.NewTracker(&cfg, log)
	resolver := fallback.NewResolver(&cfg, tracker, m, log)
	collector := stats.New()
	manager := control.New(control.Options{
		Config: &cfg, Path: path, Pool: p, Fallback: resolver,
		Stats: collector, Log: log, AllowWrite: allowControl,
	})

	srv := New(Options{
		Config: &cfg, Pool: p, Metrics: m, Fallback: tracker, Resolver: resolver,
		Stats: collector, Control: manager, Log: log, Version: "test",
	})
	return srv, p, collector
}

func do(t *testing.T, s *Server, method, target string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(method, target, nil))
	return rec
}

func TestHealthzTracksThePool(t *testing.T) {
	s, p := newTestAdmin(t, false)

	rec := do(t, s, http.MethodGet, "/healthz")
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("code = %d, want 503 while nothing is healthy", rec.Code)
	}

	p.SetInitial(p.Endpoints()[0], true, time.Millisecond, nil)
	rec = do(t, s, http.MethodGet, "/healthz")
	if rec.Code != http.StatusOK {
		t.Errorf("code = %d, want 200 once an upstream is answering", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "1/2") {
		t.Errorf("body = %q, want it to report 1 of 2 healthy", rec.Body.String())
	}
}

func TestStatusJSON(t *testing.T) {
	s, p := newTestAdmin(t, false)
	p.SetInitial(p.Endpoints()[0], true, 7*time.Millisecond, nil)

	rec := do(t, s, http.MethodGet, "/status")
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d", rec.Code)
	}

	var got statusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}
	if got.Total != 2 || got.Healthy != 1 {
		t.Errorf("healthy/total = %d/%d, want 1/2", got.Healthy, got.Total)
	}
	if got.Strategy != config.StrategyRandom {
		t.Errorf("strategy = %q", got.Strategy)
	}
	if len(got.Upstreams) != 2 || len(got.Upstreams[0].Endpoints) != 2 {
		t.Fatalf("upstream shape = %+v", got.Upstreams)
	}
	if got.Upstreams[0].Endpoints[0].LatencyMS != 7 {
		t.Errorf("latency = %v, want 7ms", got.Upstreams[0].Endpoints[0].LatencyMS)
	}
}

func TestMetricsExposition(t *testing.T) {
	s, p := newTestAdmin(t, false)
	p.SetInitial(p.Endpoints()[0], true, 12*time.Millisecond, nil)
	s.metrics.Queries.Inc(metrics.Label{Name: "proto", Value: "udp"})
	s.metrics.Duration.Observe(0.004)

	rec := do(t, s, http.MethodGet, "/metrics")
	body := rec.Body.String()

	for _, want := range []string{
		`holebalancer_queries_total{proto="udp"} 1`,
		`holebalancer_query_duration_seconds_count 1`,
		`holebalancer_query_duration_seconds_bucket{le="0.005"} 1`,
		`holebalancer_upstream_up{upstream="pihole-1"} 1`,
		`holebalancer_upstream_up{upstream="pihole-2"} 0`,
		`holebalancer_endpoint_latency_seconds{upstream="pihole-1",endpoint="lan",addr="10.0.0.1:53"} 0.012`,
		`holebalancer_build_info{version="test"} 1`,
		"# TYPE holebalancer_queries_total counter",
		"# TYPE holebalancer_upstream_up gauge",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics output is missing %q\n---\n%s", want, body)
		}
	}
}

func TestControlEndpointsDisabledByDefault(t *testing.T) {
	s, _ := newTestAdmin(t, false)
	if rec := do(t, s, http.MethodPost, "/drain?upstream=pihole-1"); rec.Code != http.StatusNotFound {
		t.Errorf("code = %d, want 404 when control is not enabled", rec.Code)
	}
}

func TestDrainAndUndrain(t *testing.T) {
	s, p := newTestAdmin(t, true)

	if rec := do(t, s, http.MethodPost, "/drain?upstream=pihole-1"); rec.Code != http.StatusOK {
		t.Fatalf("drain: code = %d, body = %q", rec.Code, rec.Body.String())
	}
	if !p.Lookup("pihole-1").Drained() {
		t.Error("upstream was not drained")
	}

	if rec := do(t, s, http.MethodPost, "/undrain?upstream=pihole-1"); rec.Code != http.StatusOK {
		t.Fatalf("undrain: code = %d", rec.Code)
	}
	if p.Lookup("pihole-1").Drained() {
		t.Error("upstream was not returned to rotation")
	}

	if rec := do(t, s, http.MethodPost, "/drain?upstream=nope"); rec.Code != http.StatusNotFound {
		t.Errorf("unknown upstream: code = %d, want 404", rec.Code)
	}
	if rec := do(t, s, http.MethodPost, "/drain"); rec.Code != http.StatusBadRequest {
		t.Errorf("missing parameter: code = %d, want 400", rec.Code)
	}
}

func TestIndexSummary(t *testing.T) {
	s, p := newTestAdmin(t, true)
	p.SetInitial(p.Endpoints()[0], true, time.Millisecond, nil)

	rec := do(t, s, http.MethodGet, "/summary")
	body := rec.Body.String()
	for _, want := range []string{"hole-balancer test", "pihole-1", "pihole-2", "10.0.0.1:53", "/metrics", "POST /drain"} {
		if !strings.Contains(body, want) {
			t.Errorf("index is missing %q\n---\n%s", want, body)
		}
	}
}

func TestStatusReportsFallback(t *testing.T) {
	s, p := newTestAdmin(t, false)

	rec := do(t, s, http.MethodGet, "/status")
	var got statusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.Fallback.Enabled {
		t.Error("fallback should report as enabled")
	}
	if !got.Fallback.Active {
		t.Error("with no Pi-hole healthy, fallback should report as active")
	}
	if len(got.Fallback.Servers) == 0 {
		t.Error("configured resolvers should be listed")
	}

	p.SetInitial(p.Endpoints()[0], true, time.Millisecond, nil)
	rec = do(t, s, http.MethodGet, "/status")
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Fallback.Active {
		t.Error("fallback should go inactive as soon as a Pi-hole answers")
	}
}

func TestFallbackGaugeTracksPoolState(t *testing.T) {
	s, p := newTestAdmin(t, false)

	if body := do(t, s, http.MethodGet, "/metrics").Body.String(); !strings.Contains(body, "holebalancer_fallback_active 1") {
		t.Errorf("gauge should be 1 while every Pi-hole is down\n---\n%s", body)
	}

	p.SetInitial(p.Endpoints()[0], true, time.Millisecond, nil)
	if body := do(t, s, http.MethodGet, "/metrics").Body.String(); !strings.Contains(body, "holebalancer_fallback_active 0") {
		t.Errorf("gauge should be 0 once a Pi-hole answers\n---\n%s", body)
	}
}

func TestIndexShowsFallbackState(t *testing.T) {
	s, p := newTestAdmin(t, false)

	body := do(t, s, http.MethodGet, "/summary").Body.String()
	if !strings.Contains(body, "ACTIVE - answering unfiltered") {
		t.Errorf("an operator should see at a glance that answers are unfiltered\n---\n%s", body)
	}

	p.SetInitial(p.Endpoints()[0], true, time.Millisecond, nil)
	body = do(t, s, http.MethodGet, "/summary").Body.String()
	if !strings.Contains(body, "standby") {
		t.Errorf("fallback should read as standby once a Pi-hole is up\n---\n%s", body)
	}
}
