package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chinmay28/hole-balancer/internal/config"
	"github.com/chinmay28/hole-balancer/internal/stats"
)

func send(t *testing.T, s *Server, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, target, nil)
	} else {
		r = httptest.NewRequest(method, target, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, r)
	return rec
}

func decodeBody(t *testing.T, rec *httptest.ResponseRecorder, v any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), v); err != nil {
		t.Fatalf("response is not valid JSON (%d): %s", rec.Code, rec.Body.String())
	}
}

func TestOverviewDescribesTheWholePage(t *testing.T) {
	s, p := newTestAdmin(t, true)
	p.SetInitial(p.Endpoints()[0], true, 3*time.Millisecond, nil)

	rec := send(t, s, http.MethodGet, "/api/overview", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d", rec.Code)
	}

	var got overviewResponse
	decodeBody(t, rec, &got)
	if got.Total != 2 || got.Healthy != 1 {
		t.Errorf("healthy/total = %d/%d, want 1/2", got.Healthy, got.Total)
	}
	if got.Strategy != config.StrategyRandom {
		t.Errorf("strategy = %q", got.Strategy)
	}
	if len(got.Strategies) != 4 {
		t.Errorf("available strategies = %v", got.Strategies)
	}
	if !got.Control.Enabled {
		t.Error("control should report as enabled")
	}
	if len(got.Upstreams) != 2 {
		t.Errorf("upstreams = %d, want 2", len(got.Upstreams))
	}
}

// With control off the interface must be able to explain why, rather than
// offering buttons that will be refused.
func TestOverviewExplainsReadOnly(t *testing.T) {
	s, _ := newTestAdmin(t, false)

	var got overviewResponse
	decodeBody(t, send(t, s, http.MethodGet, "/api/overview", ""), &got)
	if got.Control.Enabled {
		t.Error("control should be disabled")
	}
	if !strings.Contains(got.Control.Reason, "allow_control") {
		t.Errorf("reason = %q, want it to name the setting to change", got.Control.Reason)
	}
}

func TestStatsEndpoint(t *testing.T) {
	s, _, collector := newTestAdminFull(t, true, "")
	for i := 0; i < 3; i++ {
		collector.Record(stats.Record{
			Proto: "udp", QType: "A", Upstream: "pihole-1",
			RCode: "NOERROR", Latency: time.Millisecond, Attempts: 1,
		})
	}

	var got stats.Snapshot
	decodeBody(t, send(t, s, http.MethodGet, "/api/stats", ""), &got)
	if got.Total != 3 {
		t.Errorf("total = %d, want 3", got.Total)
	}
	if got.TopNode != "pihole-1" {
		t.Errorf("top node = %q", got.TopNode)
	}
	if len(got.Minutes) == 0 {
		t.Error("history should be present for the chart")
	}
}

func TestAddAndRemoveUpstreamThroughTheAPI(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	s, p, _ := newTestAdminFull(t, true, path)

	rec := send(t, s, http.MethodPost, "/api/upstreams",
		`{"name":"pihole-3","weight":2,"endpoints":["10.0.0.3","10.0.0.4:5353"]}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("add: code = %d, body = %s", rec.Code, rec.Body.String())
	}
	u := p.Lookup("pihole-3")
	if u == nil {
		t.Fatal("upstream is not in the pool")
	}
	if u.Weight != 2 || len(u.Endpoints) != 2 {
		t.Errorf("upstream = weight %d, %d endpoints", u.Weight, len(u.Endpoints))
	}

	if rec := send(t, s, http.MethodDelete, "/api/upstreams/pihole-3", ""); rec.Code != http.StatusOK {
		t.Fatalf("remove: code = %d, body = %s", rec.Code, rec.Body.String())
	}
	if p.Lookup("pihole-3") != nil {
		t.Error("upstream survived removal")
	}
}

func TestAPIErrorsCarryUsefulStatusCodes(t *testing.T) {
	s, _, _ := newTestAdminFull(t, true, "")

	cases := []struct {
		name, method, target, body string
		want                       int
	}{
		{"duplicate name", http.MethodPost, "/api/upstreams", `{"name":"pihole-1","endpoints":["10.9.9.9"]}`, http.StatusConflict},
		{"bad address", http.MethodPost, "/api/upstreams", `{"name":"x","endpoints":["not:a:host"]}`, http.StatusBadRequest},
		{"no endpoints", http.MethodPost, "/api/upstreams", `{"name":"x","endpoints":[]}`, http.StatusBadRequest},
		{"unknown field", http.MethodPost, "/api/upstreams", `{"nmae":"x"}`, http.StatusBadRequest},
		{"malformed json", http.MethodPost, "/api/upstreams", `{`, http.StatusBadRequest},
		{"missing upstream", http.MethodDelete, "/api/upstreams/nope", "", http.StatusNotFound},
		{"bad strategy", http.MethodPut, "/api/strategy", `{"strategy":"sticky"}`, http.StatusBadRequest},
		{"fallback with none", http.MethodPut, "/api/fallback", `{"enabled":true,"servers":[]}`, http.StatusBadRequest},
		{"bad resolver", http.MethodPut, "/api/fallback", `{"enabled":true,"servers":["not:a:host"]}`, http.StatusBadRequest},
	}
	for _, tc := range cases {
		rec := send(t, s, tc.method, tc.target, tc.body)
		if rec.Code != tc.want {
			t.Errorf("%s: code = %d, want %d (%s)", tc.name, rec.Code, tc.want, rec.Body.String())
		}
		var e apiError
		decodeBody(t, rec, &e)
		if e.Error == "" {
			t.Errorf("%s: no message for the interface to show", tc.name)
		}
	}
}

// Every mutating route must refuse in read-only mode — a client that skips the
// interface and posts directly must not get further than the buttons would.
func TestReadOnlyRefusesEveryMutatingRoute(t *testing.T) {
	s, p := newTestAdmin(t, false)

	cases := []struct{ method, target, body string }{
		{http.MethodPost, "/api/upstreams", `{"name":"x","endpoints":["10.9.9.9"]}`},
		{http.MethodDelete, "/api/upstreams/pihole-1", ""},
		{http.MethodPost, "/api/upstreams/pihole-1/drain", `{"drained":true}`},
		{http.MethodPut, "/api/strategy", `{"strategy":"failover"}`},
		{http.MethodPut, "/api/fallback", `{"enabled":false,"servers":[]}`},
		{http.MethodPost, "/api/stats/reset", ""},
	}
	for _, tc := range cases {
		rec := send(t, s, tc.method, tc.target, tc.body)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s %s: code = %d, want 403", tc.method, tc.target, rec.Code)
		}
	}
	if len(p.Upstreams()) != 2 || p.Lookup("pihole-1").Drained() {
		t.Error("a read-only server was changed anyway")
	}
}

func TestDrainThroughTheAPI(t *testing.T) {
	s, p, _ := newTestAdminFull(t, true, "")

	if rec := send(t, s, http.MethodPost, "/api/upstreams/pihole-1/drain", `{"drained":true}`); rec.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !p.Lookup("pihole-1").Drained() {
		t.Error("upstream was not drained")
	}

	send(t, s, http.MethodPost, "/api/upstreams/pihole-1/drain", `{"drained":false}`)
	if p.Lookup("pihole-1").Drained() {
		t.Error("upstream was not returned to rotation")
	}
}

func TestStrategyAndFallbackThroughTheAPI(t *testing.T) {
	s, p, _ := newTestAdminFull(t, true, "")

	if rec := send(t, s, http.MethodPut, "/api/strategy", `{"strategy":"round-robin"}`); rec.Code != http.StatusOK {
		t.Fatalf("strategy: code = %d", rec.Code)
	}
	if got := p.Strategy(); got != config.StrategyRoundRobin {
		t.Errorf("strategy = %q", got)
	}

	rec := send(t, s, http.MethodPut, "/api/fallback", `{"enabled":true,"servers":["1.1.1.1","1.0.0.1"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("fallback: code = %d, body = %s", rec.Code, rec.Body.String())
	}
	var fs fallbackStatus
	decodeBody(t, rec, &fs)
	if len(fs.Servers) != 2 || fs.Servers[0] != "1.1.1.1:53" {
		t.Errorf("servers = %v", fs.Servers)
	}
	// The response reflects live state, so the interface never shows a value
	// the balancer is not actually using.
	var ov overviewResponse
	decodeBody(t, send(t, s, http.MethodGet, "/api/overview", ""), &ov)
	if len(ov.Fallback.Servers) != 2 {
		t.Errorf("overview still reports %v", ov.Fallback.Servers)
	}
}

func TestAddingAnUpstreamTriggersAProbe(t *testing.T) {
	s, _, _ := newTestAdminFull(t, true, "")
	probed := make(chan struct{}, 1)
	s.probe = func() { probed <- struct{}{} }

	send(t, s, http.MethodPost, "/api/upstreams", `{"name":"x","endpoints":["10.9.9.9"]}`)
	select {
	case <-probed:
	default:
		t.Error("adding a Pi-hole should trigger an immediate health check")
	}
}

func TestStatsReset(t *testing.T) {
	s, _, collector := newTestAdminFull(t, true, "")
	collector.Record(stats.Record{Proto: "udp", RCode: "NOERROR", Upstream: "pihole-1"})

	if rec := send(t, s, http.MethodPost, "/api/stats/reset", ""); rec.Code != http.StatusOK {
		t.Fatalf("code = %d", rec.Code)
	}
	if got := collector.Snapshot().Total; got != 0 {
		t.Errorf("total after reset = %d", got)
	}
}

func TestUIIsSelfContained(t *testing.T) {
	s, _ := newTestAdmin(t, true)

	rec := send(t, s, http.MethodGet, "/", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d", rec.Code)
	}
	body := rec.Body.String()

	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("content type = %q", ct)
	}
	if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'none'") {
		t.Errorf("missing a restrictive CSP, got %q", csp)
	}

	// The page must load on a network whose DNS is broken, so nothing may be
	// *fetched* from anywhere else. A hyperlink to an off-origin page is fine —
	// following it is the reader's choice and happens after the page has
	// loaded — so this checks resource positions specifically rather than
	// matching "https://" anywhere in the document.
	for _, forbidden := range []string{
		`src="http`, `src='http`, `src="//`, // scripts, images, frames
		`url(http`, `url("http`, `url(//`, // stylesheet references
		"@import", // ditto
	} {
		if strings.Contains(body, forbidden) {
			t.Errorf("page fetches %q — it must load with no network at all", forbidden)
		}
	}
	// <link> is a fetch too, so its href has to be same-origin.
	for _, tag := range strings.Split(body, "<link ")[1:] {
		head := tag
		if i := strings.Index(head, ">"); i >= 0 {
			head = head[:i]
		}
		if strings.Contains(head, "href=\"http") || strings.Contains(head, "href=\"//") {
			t.Errorf("page links an off-origin resource: <link %s>", head)
		}
	}
	for _, want := range []string{"<style>", "<script>", "hole-balancer", "/api/overview"} {
		if !strings.Contains(body, want) {
			t.Errorf("page is missing %q", want)
		}
	}
}

func TestTextSummaryStillServedSeparately(t *testing.T) {
	s, _ := newTestAdmin(t, true)

	rec := send(t, s, http.MethodGet, "/summary", "")
	if !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/plain") {
		t.Errorf("summary should stay plain text for curl, got %q", rec.Header().Get("Content-Type"))
	}
	if !strings.Contains(rec.Body.String(), "hole-balancer test") {
		t.Errorf("summary body = %q", rec.Body.String())
	}
}

// The dashboard gets opened from a phone — often *because* the laptop stopped
// resolving — so the page must declare a mobile viewport and must not disable
// pinch zoom.
func TestUIIsMobileReady(t *testing.T) {
	s, _ := newTestAdmin(t, true)
	body := send(t, s, http.MethodGet, "/", "").Body.String()

	if !strings.Contains(body, `name="viewport"`) ||
		!strings.Contains(body, "width=device-width") {
		t.Error("page is missing a mobile viewport declaration")
	}
	// Blocking zoom is an accessibility failure, and the usual reason a page
	// gets away with tiny text.
	for _, banned := range []string{"user-scalable=no", "maximum-scale=1"} {
		if strings.Contains(body, banned) {
			t.Errorf("viewport must not disable zoom, found %q", banned)
		}
	}

	// The layout is mobile-first: these are the rules that make it work, and
	// losing any of them silently degrades the phone experience.
	for _, want := range []string{
		"@media (min-width: 560px)", // the first widen-up breakpoint
		"@media (pointer: coarse)",  // thumb-sized tap targets
		"env(safe-area-inset-left)", // notch and home-indicator insets
		"overflow-x: hidden",        // the page itself never scrolls sideways
	} {
		if !strings.Contains(body, want) {
			t.Errorf("stylesheet is missing %q", want)
		}
	}
}

func TestIconIsServedAndCacheable(t *testing.T) {
	s, _ := newTestAdmin(t, true)

	rec := send(t, s, http.MethodGet, "/icon.svg", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "image/svg+xml" {
		t.Errorf("content type = %q", ct)
	}
	if !strings.Contains(rec.Body.String(), "<svg") {
		t.Error("body is not an SVG")
	}

	// A content-hash ETag lets a browser revalidate cheaply while a new build
	// invalidates on its own, with no cache-busting query string to maintain.
	etag := rec.Header().Get("ETag")
	if etag == "" {
		t.Fatal("no ETag")
	}
	r := httptest.NewRequest(http.MethodGet, "/icon.svg", nil)
	r.Header.Set("If-None-Match", etag)
	again := httptest.NewRecorder()
	s.Handler().ServeHTTP(again, r)
	if again.Code != http.StatusNotModified {
		t.Errorf("revalidation returned %d, want 304", again.Code)
	}
}

func TestPageCarriesBothMarks(t *testing.T) {
	s, _ := newTestAdmin(t, true)
	body := send(t, s, http.MethodGet, "/", "").Body.String()

	// The icon is linked, so the browser caches one copy for the tab and the
	// header.
	for _, want := range []string{`rel="icon"`, `href="icon.svg"`, `src="icon.svg"`} {
		if !strings.Contains(body, want) {
			t.Errorf("page is missing %q", want)
		}
	}

	// The author's mark is inlined, which is the only way its currentColor
	// strokes can take the footer's text colour in both themes.
	if strings.Contains(body, "<!--DEV_MARK-->") {
		t.Error("the DEV_MARK placeholder was not substituted")
	}
	if !strings.Contains(body, `stroke="currentColor"`) {
		t.Error("the author's mark is not inlined into the page")
	}
	if !strings.Contains(body, "chinmay28") {
		t.Error("the footer credit is missing")
	}
}
