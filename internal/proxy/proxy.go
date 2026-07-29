// Package proxy accepts DNS queries from clients and forwards them to the
// healthiest available Pi-hole.
package proxy

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/chinmay28/hole-balancer/internal/config"
	"github.com/chinmay28/hole-balancer/internal/dnsclient"
	"github.com/chinmay28/hole-balancer/internal/dnsmsg"
	"github.com/chinmay28/hole-balancer/internal/metrics"
	"github.com/chinmay28/hole-balancer/internal/pool"
)

// Server forwards client queries to upstream Pi-holes.
type Server struct {
	cfg     *config.Config
	pool    *pool.Pool
	metrics *metrics.Metrics
	log     *slog.Logger

	// sem bounds queries in flight across both transports.
	sem chan struct{}

	// ready is closed once the listeners are bound, and the addresses record
	// what they bound to. Both matter when the configuration asks for port 0.
	ready   chan struct{}
	addrMu  sync.Mutex
	udpAddr string
	tcpAddr string
}

// New creates a forwarding server.
func New(cfg *config.Config, p *pool.Pool, m *metrics.Metrics, log *slog.Logger) *Server {
	return &Server{
		cfg:     cfg,
		pool:    p,
		metrics: m,
		log:     log,
		sem:     make(chan struct{}, cfg.Query.MaxConcurrent),
		ready:   make(chan struct{}),
	}
}

// Ready is closed once every configured listener is bound and serving.
func (s *Server) Ready() <-chan struct{} { return s.ready }

// UDPAddr reports the address the UDP listener bound to, once Ready is closed.
func (s *Server) UDPAddr() string {
	s.addrMu.Lock()
	defer s.addrMu.Unlock()
	return s.udpAddr
}

// TCPAddr reports the address the TCP listener bound to, once Ready is closed.
func (s *Server) TCPAddr() string {
	s.addrMu.Lock()
	defer s.addrMu.Unlock()
	return s.tcpAddr
}

// acquire reserves a slot for one in-flight query. It reports false when the
// balancer is already at its concurrency limit.
func (s *Server) acquire() bool {
	select {
	case s.sem <- struct{}{}:
		return true
	default:
		return false
	}
}

func (s *Server) release() { <-s.sem }

// Resolve answers a single client query and returns the bytes to send back.
// A nil return means the query was unanswerable and should be dropped without
// a reply, which is the correct handling for a malformed datagram.
func (s *Server) Resolve(ctx context.Context, req []byte, proto string) []byte {
	s.metrics.Queries.Inc(metrics.Label{Name: "proto", Value: proto})

	hdr, err := dnsmsg.ParseHeader(req)
	if err != nil {
		s.log.Debug("dropping malformed query", "proto", proto, "error", err)
		return nil
	}
	if hdr.QR {
		// A response arriving on the query port is either a bug elsewhere or
		// an attempt to use us as a reflector. Never reply.
		s.log.Debug("dropping message with response bit set", "proto", proto)
		return nil
	}

	start := time.Now()
	resp := s.forward(ctx, req, proto)
	s.metrics.Duration.Observe(time.Since(start).Seconds())
	return resp
}

func (s *Server) forward(ctx context.Context, req []byte, proto string) []byte {
	question, qErr := dnsmsg.ParseQuestion(req)

	plan := s.pool.Plan()
	if len(plan) == 0 {
		s.log.Error("no upstreams configured to answer query")
		s.metrics.Servfails.Inc()
		return dnsmsg.ErrorResponse(req, dnsmsg.RCodeServFail)
	}

	attempts := s.cfg.Query.MaxAttempts
	if attempts > len(plan) {
		attempts = len(plan)
	}

	var (
		lastResp   []byte
		lastReason = "no_attempt"
	)

	for i := 0; i < attempts; i++ {
		if ctx.Err() != nil {
			lastReason = "canceled"
			break
		}
		ep := plan[i]
		if i > 0 {
			s.metrics.Retries.Inc(metrics.Label{Name: "reason", Value: lastReason})
		}

		resp, rtt, err := dnsclient.Exchange(ctx, proto, ep.Addr, req, s.cfg.Query.Timeout.D())
		if err != nil {
			lastReason = dnsclient.Classify(err)
			s.pool.ReportQuery(ep, false, rtt, err)
			s.metrics.Failures.Inc(
				metrics.Label{Name: "upstream", Value: ep.Upstream.Name},
				metrics.Label{Name: "endpoint", Value: ep.Name},
				metrics.Label{Name: "reason", Value: lastReason},
			)
			s.log.Debug("upstream exchange failed",
				"upstream", ep.Upstream.Name, "endpoint", ep.Addr,
				"attempt", i+1, "reason", lastReason, "error", err)
			continue
		}

		respHdr, _ := dnsmsg.ParseHeader(resp)
		if s.cfg.Query.ShouldRetryRCode(respHdr.RCode) {
			// The upstream answered, but with a code that says it could not do
			// its job. Treat that as a failure of that Pi-hole and ask another.
			// Blocked domains never arrive this way, so filtering still holds.
			lastReason = "rcode_" + respHdr.RCode.String()
			lastResp = resp
			s.pool.ReportQuery(ep, false, rtt, fmt.Errorf("upstream returned %s", respHdr.RCode))
			s.metrics.Failures.Inc(
				metrics.Label{Name: "upstream", Value: ep.Upstream.Name},
				metrics.Label{Name: "endpoint", Value: ep.Name},
				metrics.Label{Name: "reason", Value: lastReason},
			)
			s.log.Debug("upstream returned retryable rcode",
				"upstream", ep.Upstream.Name, "endpoint", ep.Addr,
				"rcode", respHdr.RCode.String(), "attempt", i+1)
			continue
		}

		s.pool.ReportQuery(ep, true, rtt, nil)
		s.metrics.Responses.Inc(
			metrics.Label{Name: "upstream", Value: ep.Upstream.Name},
			metrics.Label{Name: "endpoint", Value: ep.Name},
			metrics.Label{Name: "rcode", Value: respHdr.RCode.String()},
		)
		if s.cfg.Log.Queries {
			s.log.Info("query",
				"question", questionField(question, qErr),
				"proto", proto,
				"upstream", ep.Upstream.Name,
				"endpoint", ep.Addr,
				"rcode", respHdr.RCode.String(),
				"answers", respHdr.ANCount,
				"attempts", i+1,
				"rtt", rtt.Round(time.Microsecond).String(),
			)
		}
		return resp
	}

	s.metrics.Servfails.Inc()
	s.log.Warn("query failed on every attempt",
		"question", questionField(question, qErr),
		"proto", proto,
		"attempts", attempts,
		"reason", lastReason,
		"healthy_upstreams", s.pool.HealthyUpstreams(),
	)

	if lastResp != nil {
		// Prefer a real upstream error response over a synthetic one: it may
		// carry EDNS0 information the client can use.
		return lastResp
	}
	return dnsmsg.ErrorResponse(req, dnsmsg.RCodeServFail)
}

func questionField(q dnsmsg.Question, err error) string {
	if err != nil {
		return "<unparsed>"
	}
	return q.String()
}

// overloaded builds the reply sent when the concurrency limit is reached.
func (s *Server) overloaded(req []byte, proto string) []byte {
	s.metrics.Failures.Inc(
		metrics.Label{Name: "upstream", Value: "none"},
		metrics.Label{Name: "endpoint", Value: "none"},
		metrics.Label{Name: "reason", Value: "overloaded"},
	)
	s.log.Warn("shedding query: concurrency limit reached",
		"proto", proto, "limit", s.cfg.Query.MaxConcurrent)
	return dnsmsg.ErrorResponse(req, dnsmsg.RCodeServFail)
}
