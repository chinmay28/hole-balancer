// Package dnsclient performs a single DNS exchange with an upstream server
// over UDP or TCP, and provides the RFC 7766 framing helpers both the
// forwarder and the health checker need.
package dnsclient

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/chinmay28/hole-balancer/internal/dnsmsg"
)

// Transports understood by the balancer. A query is forwarded over the same
// transport the client used, so an EDNS0 buffer negotiated over UDP still
// means what the client intended.
const (
	ProtoUDP = "udp"
	ProtoTCP = "tcp"
)

// bufPool recycles the 64 KiB scratch buffers used to receive DNS messages.
// 64 KiB is the largest message TCP framing can describe, so a single size
// covers every case.
var bufPool = sync.Pool{
	New: func() any {
		b := make([]byte, dnsmsg.MaxMessageSize)
		return &b
	},
}

// GetBuf borrows a receive buffer from the shared pool.
func GetBuf() *[]byte { return bufPool.Get().(*[]byte) }

// PutBuf returns a buffer borrowed from GetBuf.
func PutBuf(b *[]byte) { bufPool.Put(b) }

// Exchange sends query to addr and returns the response together with the
// round-trip time.
//
// Each exchange uses its own connected socket. That costs an ephemeral port
// per query but means the kernel filters off-path replies for us and the
// client's transaction ID can be forwarded untouched, with no multiplexing
// table to get wrong.
func Exchange(ctx context.Context, proto, addr string, query []byte, timeout time.Duration) ([]byte, time.Duration, error) {
	dialCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	start := time.Now()
	var d net.Dialer
	conn, err := d.DialContext(dialCtx, proto, addr)
	if err != nil {
		return nil, time.Since(start), fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()

	deadline := start.Add(timeout)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return nil, time.Since(start), err
	}

	// The socket deadline bounds how long this can take, but it does not react
	// to cancellation. Without this watcher an in-flight read would keep a
	// shutdown waiting for the full query timeout.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			// Moving the deadline into the past unblocks any pending I/O at
			// once, and is safe to do concurrently with a read.
			_ = conn.SetDeadline(time.Now())
		case <-done:
		}
	}()

	var resp []byte
	if proto == ProtoTCP {
		resp, err = exchangeTCP(conn, query)
	} else {
		resp, err = exchangeUDP(conn, query)
	}
	rtt := time.Since(start)
	if err != nil {
		// Report cancellation as cancellation rather than as the I/O timeout
		// the watcher used to force it.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, rtt, ctxErr
		}
		return nil, rtt, err
	}
	if err := dnsmsg.ValidateResponse(resp, dnsmsg.ID(query)); err != nil {
		return nil, rtt, err
	}
	return resp, rtt, nil
}

func exchangeUDP(conn net.Conn, query []byte) ([]byte, error) {
	if _, err := conn.Write(query); err != nil {
		return nil, fmt.Errorf("write: %w", err)
	}

	buf := GetBuf()
	defer PutBuf(buf)
	wantID := dnsmsg.ID(query)

	// A connected UDP socket only receives datagrams from the peer, but the
	// peer itself can send a late reply to an earlier query on a reused port.
	// Skip anything whose transaction ID does not match until the deadline.
	for {
		n, err := conn.Read(*buf)
		if err != nil {
			return nil, fmt.Errorf("read: %w", err)
		}
		if n < dnsmsg.HeaderLen {
			continue
		}
		if dnsmsg.ID((*buf)[:n]) != wantID {
			continue
		}
		out := make([]byte, n)
		copy(out, (*buf)[:n])
		return out, nil
	}
}

func exchangeTCP(conn net.Conn, query []byte) ([]byte, error) {
	if len(query) > dnsmsg.MaxMessageSize {
		return nil, errors.New("query exceeds maximum DNS message size")
	}
	framed := make([]byte, 2+len(query))
	binary.BigEndian.PutUint16(framed[:2], uint16(len(query)))
	copy(framed[2:], query)
	if _, err := conn.Write(framed); err != nil {
		return nil, fmt.Errorf("write: %w", err)
	}
	return ReadMessage(conn)
}

// ReadMessage reads one length-prefixed DNS message (RFC 7766 framing).
func ReadMessage(r io.Reader) ([]byte, error) {
	var lenBuf [2]byte
	if _, err := io.ReadFull(r, lenBuf[:]); err != nil {
		return nil, err
	}
	n := int(binary.BigEndian.Uint16(lenBuf[:]))
	if n == 0 {
		return nil, errors.New("zero-length DNS message")
	}
	msg := make([]byte, n)
	if _, err := io.ReadFull(r, msg); err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	return msg, nil
}

// WriteMessage writes one length-prefixed DNS message.
func WriteMessage(w io.Writer, msg []byte) error {
	if len(msg) > dnsmsg.MaxMessageSize {
		return errors.New("response exceeds maximum DNS message size")
	}
	framed := make([]byte, 2+len(msg))
	binary.BigEndian.PutUint16(framed[:2], uint16(len(msg)))
	copy(framed[2:], msg)
	_, err := w.Write(framed)
	return err
}

// Classify reduces a transport error to a short, low-cardinality label for
// metrics and logs.
func Classify(err error) string {
	if err == nil {
		return "none"
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "timeout"
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return "eof"
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		if opErr.Op == "dial" {
			return "dial"
		}
		return "network"
	}
	if errors.Is(err, dnsmsg.ErrShortMessage) || errors.Is(err, dnsmsg.ErrMalformed) {
		return "malformed"
	}
	return "error"
}
