package proxy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/chinmay28/hole-balancer/internal/dnsclient"
)

// tcpIdleTimeout is how long an idle client connection is held open. RFC 7766
// recommends keeping connections alive for reuse; a few seconds is plenty for
// a resolver that mostly speaks UDP.
const tcpIdleTimeout = 10 * time.Second

// ListenAndServe binds the configured UDP and TCP sockets and serves until
// ctx is cancelled. It returns the first error that stops a listener, or nil
// on a clean shutdown.
func (s *Server) ListenAndServe(ctx context.Context) error {
	var (
		wg      sync.WaitGroup
		errOnce sync.Once
		retErr  error
		closers []func()
	)

	fail := func(err error) {
		errOnce.Do(func() { retErr = err })
	}

	if addr := s.cfg.Listen.UDP; addr != "" {
		conn, err := net.ListenPacket("udp", addr)
		if err != nil {
			close(s.ready)
			return fmt.Errorf("listen udp %s: %w", addr, err)
		}
		s.addrMu.Lock()
		s.udpAddr = conn.LocalAddr().String()
		s.addrMu.Unlock()
		s.log.Info("listening", "proto", "udp", "addr", conn.LocalAddr().String())
		closers = append(closers, func() { conn.Close() })
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.serveUDP(ctx, conn.(*net.UDPConn)); err != nil {
				fail(err)
			}
		}()
	}

	if addr := s.cfg.Listen.TCP; addr != "" {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			for _, c := range closers {
				c()
			}
			close(s.ready)
			return fmt.Errorf("listen tcp %s: %w", addr, err)
		}
		s.addrMu.Lock()
		s.tcpAddr = ln.Addr().String()
		s.addrMu.Unlock()
		s.log.Info("listening", "proto", "tcp", "addr", ln.Addr().String())
		closers = append(closers, func() { ln.Close() })
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.serveTCP(ctx, ln); err != nil {
				fail(err)
			}
		}()
	}

	close(s.ready)

	<-ctx.Done()
	for _, c := range closers {
		c()
	}
	wg.Wait()
	return retErr
}

// serveUDP reads datagrams and answers each one in its own goroutine.
func (s *Server) serveUDP(ctx context.Context, conn *net.UDPConn) error {
	for {
		buf := dnsclient.GetBuf()
		n, addr, err := conn.ReadFromUDP(*buf)
		if err != nil {
			dnsclient.PutBuf(buf)
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			// A transient read error should not take the listener down.
			s.log.Warn("udp read failed", "error", err)
			continue
		}

		req := make([]byte, n)
		copy(req, (*buf)[:n])
		dnsclient.PutBuf(buf)

		if !s.acquire() {
			if resp := s.overloaded(req, dnsclient.ProtoUDP); resp != nil {
				_, _ = conn.WriteToUDP(resp, addr)
			}
			continue
		}
		go func() {
			defer s.release()
			resp := s.Resolve(ctx, req, dnsclient.ProtoUDP)
			if resp == nil {
				return
			}
			if _, err := conn.WriteToUDP(resp, addr); err != nil {
				s.log.Debug("udp write failed", "client", addr.String(), "error", err)
			}
		}()
	}
}

// serveTCP accepts client connections and serves each one.
func (s *Server) serveTCP(ctx context.Context, ln net.Listener) error {
	var wg sync.WaitGroup
	defer wg.Wait()

	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Timeout() {
				continue
			}
			return fmt.Errorf("tcp accept: %w", err)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.serveTCPConn(ctx, conn)
		}()
	}
}

// serveTCPConn handles one client connection, which may carry several queries
// back to back.
func (s *Server) serveTCPConn(ctx context.Context, conn net.Conn) {
	defer conn.Close()

	// Close the connection promptly when the process is shutting down.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			conn.Close()
		case <-done:
		}
	}()

	for {
		if err := conn.SetReadDeadline(time.Now().Add(tcpIdleTimeout)); err != nil {
			return
		}
		req, err := dnsclient.ReadMessage(conn)
		if err != nil {
			return // EOF, idle timeout, or a client that framed badly
		}

		if !s.acquire() {
			resp := s.overloaded(req, dnsclient.ProtoTCP)
			if resp == nil {
				return
			}
			_ = conn.SetWriteDeadline(time.Now().Add(tcpIdleTimeout))
			if dnsclient.WriteMessage(conn, resp) != nil {
				return
			}
			continue
		}

		resp := s.Resolve(ctx, req, dnsclient.ProtoTCP)
		s.release()
		if resp == nil {
			return // unanswerable: drop the connection rather than desync it
		}
		if err := conn.SetWriteDeadline(time.Now().Add(tcpIdleTimeout)); err != nil {
			return
		}
		if err := dnsclient.WriteMessage(conn, resp); err != nil {
			s.log.Debug("tcp write failed", "client", conn.RemoteAddr().String(), "error", err)
			return
		}
	}
}
