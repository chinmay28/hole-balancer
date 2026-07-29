// Package testdns provides a controllable stand-in for a Pi-hole, used by the
// balancer's tests to simulate servers that answer, refuse, stall, or vanish.
//
// It is deliberately not part of any shipped code path.
package testdns

import (
	"encoding/binary"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chinmay28/hole-balancer/internal/dnsmsg"
)

// Server is a fake DNS server listening on loopback.
type Server struct {
	udp *net.UDPConn
	tcp net.Listener

	rcode   atomic.Int32
	answers atomic.Int32
	drop    atomic.Bool
	delay   atomic.Int64
	queries atomic.Int64
}

// Start brings up a fake server on an arbitrary loopback port, answering both
// UDP and TCP. It answers NOERROR with one synthetic answer record until told
// otherwise, and shuts down when the test finishes.
func Start(t *testing.T) *Server {
	t.Helper()

	udp, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("listen udp: %v", err)
	}
	// Bind TCP to the same port so one address describes both transports.
	tcp, err := net.Listen("tcp", udp.LocalAddr().String())
	if err != nil {
		udp.Close()
		t.Fatalf("listen tcp: %v", err)
	}

	s := &Server{udp: udp, tcp: tcp}
	s.answers.Store(1)

	go s.serveUDP()
	go s.serveTCP()
	t.Cleanup(func() {
		udp.Close()
		tcp.Close()
	})
	return s
}

// Addr is the host:port both transports listen on.
func (s *Server) Addr() string { return s.udp.LocalAddr().String() }

// SetRCode changes the response code the server returns.
func (s *Server) SetRCode(r dnsmsg.RCode) { s.rcode.Store(int32(r)) }

// SetAnswers changes the ANCOUNT the server reports, so tests can exercise
// probes configured with require_answer.
func (s *Server) SetAnswers(n int) { s.answers.Store(int32(n)) }

// SetDrop makes the server swallow queries without replying, which is what a
// powered-off Pi-hole looks like from the balancer's side.
func (s *Server) SetDrop(v bool) { s.drop.Store(v) }

// SetDelay makes the server wait before answering.
func (s *Server) SetDelay(d time.Duration) { s.delay.Store(int64(d)) }

// Queries counts the queries received since start.
func (s *Server) Queries() int64 { return s.queries.Load() }

// Stop closes both listeners, simulating a host that has gone away.
func (s *Server) Stop() {
	s.udp.Close()
	s.tcp.Close()
}

func (s *Server) serveUDP() {
	buf := make([]byte, dnsmsg.MaxMessageSize)
	for {
		n, addr, err := s.udp.ReadFromUDP(buf)
		if err != nil {
			return
		}
		req := make([]byte, n)
		copy(req, buf[:n])
		go func() {
			resp := s.respond(req)
			if resp != nil {
				_, _ = s.udp.WriteToUDP(resp, addr)
			}
		}()
	}
}

func (s *Server) serveTCP() {
	for {
		conn, err := s.tcp.Accept()
		if err != nil {
			return
		}
		go func() {
			defer conn.Close()
			for {
				var lenBuf [2]byte
				if _, err := io.ReadFull(conn, lenBuf[:]); err != nil {
					return
				}
				req := make([]byte, binary.BigEndian.Uint16(lenBuf[:]))
				if _, err := io.ReadFull(conn, req); err != nil {
					return
				}
				resp := s.respond(req)
				if resp == nil {
					return
				}
				framed := make([]byte, 2+len(resp))
				binary.BigEndian.PutUint16(framed[:2], uint16(len(resp)))
				copy(framed[2:], resp)
				if _, err := conn.Write(framed); err != nil {
					return
				}
			}
		}()
	}
}

// respond builds the reply, or returns nil when the server is dropping.
func (s *Server) respond(req []byte) []byte {
	s.queries.Add(1)
	if d := time.Duration(s.delay.Load()); d > 0 {
		time.Sleep(d)
	}
	if s.drop.Load() {
		return nil
	}

	resp := dnsmsg.ErrorResponse(req, dnsmsg.RCode(s.rcode.Load()))
	if resp == nil {
		return nil
	}
	// ErrorResponse zeroes ANCOUNT; restore whatever the test asked for so
	// require_answer probes can be exercised. The records themselves are not
	// appended: nothing under test parses past the header.
	if n := s.answers.Load(); n > 0 && dnsmsg.RCode(s.rcode.Load()) == dnsmsg.RCodeNoError {
		binary.BigEndian.PutUint16(resp[6:8], uint16(n))
	}
	return resp
}
