package admin

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/chinmay28/hole-balancer/internal/config"
	"github.com/chinmay28/hole-balancer/internal/control"
	"github.com/chinmay28/hole-balancer/internal/pool"
)

// maxBodyBytes bounds request bodies. Every request here is a handful of
// addresses, so anything larger is a mistake or an attack.
const maxBodyBytes = 64 << 10

// apiError is the shape every failed request returns, so the interface has one
// place to read a message from.
type apiError struct {
	Error string `json:"error"`
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// fail maps an error to the status code that describes it, so the interface can
// tell "you asked for something impossible" apart from "the server broke".
func (s *Server) fail(w http.ResponseWriter, err error) {
	code := http.StatusInternalServerError
	switch {
	case errors.Is(err, control.ErrReadOnly):
		code = http.StatusForbidden
	case errors.Is(err, pool.ErrNotFound):
		code = http.StatusNotFound
	case errors.Is(err, pool.ErrDuplicateName):
		code = http.StatusConflict
	case errors.Is(err, pool.ErrLastUpstream):
		code = http.StatusConflict
	}
	if code == http.StatusInternalServerError {
		// Anything unclassified is either a validation failure or a failed
		// save. Both are worth showing verbatim: the operator typed the input.
		code = http.StatusBadRequest
	}
	writeJSON(w, code, apiError{Error: err.Error()})
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeJSON(w, http.StatusBadRequest, apiError{Error: "invalid request body: " + err.Error()})
		return false
	}
	return true
}

// apiRoutes registers the management API. Read endpoints are always available;
// the mutating ones are registered regardless but refuse with 403 when control
// is off, so the interface can explain why rather than showing a bare 404.
func (s *Server) apiRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/overview", s.handleOverview)
	mux.HandleFunc("GET /api/stats", s.handleStats)
	mux.HandleFunc("GET /api/config", s.handleConfig)

	mux.HandleFunc("POST /api/upstreams", s.handleAddUpstream)
	mux.HandleFunc("DELETE /api/upstreams/{name}", s.handleRemoveUpstream)
	mux.HandleFunc("POST /api/upstreams/{name}/drain", s.handleSetDrain)
	mux.HandleFunc("PUT /api/fallback", s.handleSetFallback)
	mux.HandleFunc("PUT /api/strategy", s.handleSetStrategy)
	mux.HandleFunc("POST /api/stats/reset", s.handleResetStats)
}

// overviewResponse is the single document the dashboard polls. Keeping the
// whole page's state in one request means the panels can never disagree with
// each other the way separate polls would.
type overviewResponse struct {
	Version    string                `json:"version"`
	UptimeSec  float64               `json:"uptime_seconds"`
	Strategy   string                `json:"strategy"`
	Strategies []string              `json:"available_strategies"`
	Healthy    int                   `json:"healthy_upstreams"`
	Total      int                   `json:"total_upstreams"`
	Fallback   fallbackStatus        `json:"fallback"`
	Upstreams  []pool.UpstreamStatus `json:"upstreams"`
	Control    controlStatus         `json:"control"`
}

// controlStatus tells the interface what it is allowed to do, so it can disable
// the controls instead of offering buttons that will be refused.
type controlStatus struct {
	Enabled    bool   `json:"enabled"`
	ConfigPath string `json:"config_path,omitempty"`
	Reason     string `json:"reason,omitempty"`
}

func (s *Server) controlStatus() controlStatus {
	cs := controlStatus{Enabled: s.control.CanWrite()}
	if !cs.Enabled {
		cs.Reason = "Set admin.allow_control: true in the configuration to make changes here."
		return cs
	}
	cs.ConfigPath = s.control.ConfigPath()
	if cs.ConfigPath == "" {
		cs.Reason = "Changes apply immediately but are not saved: no configuration file to write to."
	}
	return cs
}

func (s *Server) handleOverview(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, overviewResponse{
		Version:    s.version,
		UptimeSec:  s.uptime().Seconds(),
		Strategy:   s.pool.Strategy(),
		Strategies: config.Strategies(),
		Healthy:    s.pool.HealthyUpstreams(),
		Total:      len(s.pool.Upstreams()),
		Fallback:   s.fallbackStatus(),
		Upstreams:  s.pool.Status(),
		Control:    s.controlStatus(),
	})
}

func (s *Server) handleStats(w http.ResponseWriter, _ *http.Request) {
	if s.stats == nil {
		writeJSON(w, http.StatusOK, struct{}{})
		return
	}
	writeJSON(w, http.StatusOK, s.stats.Snapshot())
}

func (s *Server) handleConfig(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.control.Snapshot())
}

// upstreamRequest is the add-a-Pi-hole payload. Endpoints are plain address
// strings; the label defaults to the address, matching the config file's
// shorthand form.
type upstreamRequest struct {
	Name      string   `json:"name"`
	Weight    int      `json:"weight"`
	Endpoints []string `json:"endpoints"`
}

func (s *Server) handleAddUpstream(w http.ResponseWriter, r *http.Request) {
	var req upstreamRequest
	if !decode(w, r, &req) {
		return
	}

	u := config.Upstream{Name: req.Name, Weight: req.Weight}
	for _, addr := range req.Endpoints {
		if strings.TrimSpace(addr) == "" {
			continue
		}
		u.Endpoints = append(u.Endpoints, config.Endpoint{Addr: addr})
	}

	if err := s.control.AddUpstream(u); err != nil {
		s.fail(w, err)
		return
	}
	// A new Pi-hole starts down until a probe says otherwise. Checking it now
	// rather than at the next interval means the interface reflects reality by
	// the time the operator has finished reading the success message.
	s.probeNow()
	writeJSON(w, http.StatusCreated, map[string]string{"status": "added", "upstream": u.Name})
}

func (s *Server) handleRemoveUpstream(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := s.control.RemoveUpstream(name); err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "removed", "upstream": name})
}

func (s *Server) handleSetDrain(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Drained bool `json:"drained"`
	}
	if !decode(w, r, &req) {
		return
	}
	name := r.PathValue("name")
	if err := s.control.SetDrained(name, req.Drained); err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"upstream": name, "drained": req.Drained})
}

func (s *Server) handleSetFallback(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Enabled bool     `json:"enabled"`
		Servers []string `json:"servers"`
	}
	if !decode(w, r, &req) {
		return
	}

	servers := make([]string, 0, len(req.Servers))
	for _, srv := range req.Servers {
		if strings.TrimSpace(srv) != "" {
			servers = append(servers, srv)
		}
	}
	if err := s.control.SetFallback(req.Enabled, servers); err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.fallbackStatus())
}

func (s *Server) handleSetStrategy(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Strategy string `json:"strategy"`
	}
	if !decode(w, r, &req) {
		return
	}
	if err := s.control.SetStrategy(req.Strategy); err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"strategy": req.Strategy})
}

func (s *Server) handleResetStats(w http.ResponseWriter, _ *http.Request) {
	if err := s.control.CanWriteErr(); err != nil {
		s.fail(w, err)
		return
	}
	if s.stats != nil {
		s.stats.Reset()
		s.log.Info("query statistics reset from the management interface")
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "reset"})
}

// probeNow asks for an out-of-band health sweep, if one was wired up.
func (s *Server) probeNow() {
	if s.probe != nil {
		s.probe()
	}
}
