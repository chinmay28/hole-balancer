package dnsclient

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/chinmay28/hole-balancer/internal/dnsmsg"
	"github.com/chinmay28/hole-balancer/internal/testdns"
)

func query(t *testing.T, id uint16) []byte {
	t.Helper()
	q, err := dnsmsg.BuildQuery(id, "example.com.", dnsmsg.TypeA)
	if err != nil {
		t.Fatal(err)
	}
	return q
}

func TestExchangeOverBothTransports(t *testing.T) {
	up := testdns.Start(t)

	for _, proto := range []string{ProtoUDP, ProtoTCP} {
		resp, rtt, err := Exchange(context.Background(), proto, up.Addr(), query(t, 0x1111), time.Second)
		if err != nil {
			t.Fatalf("%s: %v", proto, err)
		}
		if err := dnsmsg.ValidateResponse(resp, 0x1111); err != nil {
			t.Errorf("%s: %v", proto, err)
		}
		if rtt <= 0 {
			t.Errorf("%s: rtt = %v, want a positive measurement", proto, rtt)
		}
	}
}

func TestExchangeTimesOut(t *testing.T) {
	up := testdns.Start(t)
	up.SetDrop(true)

	start := time.Now()
	_, _, err := Exchange(context.Background(), ProtoUDP, up.Addr(), query(t, 1), 200*time.Millisecond)
	if err == nil {
		t.Fatal("Exchange succeeded against a server that never answers")
	}
	if Classify(err) != "timeout" {
		t.Errorf("Classify(%v) = %q, want timeout", err, Classify(err))
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("took %v, want the deadline to be honoured", elapsed)
	}
}

func TestExchangeRespectsContextCancellation(t *testing.T) {
	up := testdns.Start(t)
	up.SetDrop(true)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	if _, _, err := Exchange(ctx, ProtoUDP, up.Addr(), query(t, 1), 10*time.Second); err == nil {
		t.Fatal("Exchange should fail once its context is cancelled")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("took %v, want cancellation to cut it short", elapsed)
	}
}

func TestExchangeFailsOnUnreachableUpstream(t *testing.T) {
	up := testdns.Start(t)
	addr := up.Addr()
	up.Stop()

	_, _, err := Exchange(context.Background(), ProtoTCP, addr, query(t, 1), 500*time.Millisecond)
	if err == nil {
		t.Fatal("dialling a closed port should fail")
	}
	if reason := Classify(err); reason != "dial" && reason != "network" {
		t.Errorf("Classify = %q, want a connection failure", reason)
	}
}

// A stray datagram carrying a different transaction ID must not be mistaken
// for the answer.
func TestExchangeUDPIgnoresMismatchedIDs(t *testing.T) {
	server, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()

	go func() {
		buf := make([]byte, 4096)
		n, addr, err := server.ReadFrom(buf)
		if err != nil {
			return
		}
		req := make([]byte, n)
		copy(req, buf[:n])

		// First a decoy with the wrong ID, then the real answer.
		decoy := dnsmsg.ErrorResponse(req, dnsmsg.RCodeNoError)
		binary.BigEndian.PutUint16(decoy[0:2], 0xdead)
		_, _ = server.WriteTo(decoy, addr)
		_, _ = server.WriteTo(dnsmsg.ErrorResponse(req, dnsmsg.RCodeNoError), addr)
	}()

	resp, _, err := Exchange(context.Background(), ProtoUDP, server.LocalAddr().String(), query(t, 0x2222), 2*time.Second)
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if got := dnsmsg.ID(resp); got != 0x2222 {
		t.Errorf("returned a response with id %#x, want 0x2222", got)
	}
}

func TestMessageFramingRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	msg := query(t, 0x3333)

	if err := WriteMessage(&buf, msg); err != nil {
		t.Fatalf("WriteMessage: %v", err)
	}
	if got := binary.BigEndian.Uint16(buf.Bytes()[:2]); int(got) != len(msg) {
		t.Errorf("length prefix = %d, want %d", got, len(msg))
	}

	got, err := ReadMessage(&buf)
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	if !bytes.Equal(got, msg) {
		t.Error("round trip changed the message")
	}
}

func TestReadMessageRejectsBadFraming(t *testing.T) {
	if _, err := ReadMessage(bytes.NewReader([]byte{0x00, 0x00})); err == nil {
		t.Error("a zero-length frame should be rejected")
	}
	if _, err := ReadMessage(bytes.NewReader([]byte{0x00})); err == nil {
		t.Error("a truncated length prefix should be rejected")
	}
	// Prefix promises 10 bytes, only 3 follow.
	short := append([]byte{0x00, 0x0a}, 1, 2, 3)
	if _, err := ReadMessage(bytes.NewReader(short)); err == nil {
		t.Error("a frame shorter than its prefix should be rejected")
	}
}

func TestClassify(t *testing.T) {
	cases := map[string]error{
		"none":      nil,
		"timeout":   context.DeadlineExceeded,
		"canceled":  context.Canceled,
		"eof":       io.ErrUnexpectedEOF,
		"malformed": dnsmsg.ErrMalformed,
		"error":     errors.New("something else"),
	}
	for want, err := range cases {
		if got := Classify(err); got != want {
			t.Errorf("Classify(%v) = %q, want %q", err, got, want)
		}
	}
}

func TestBufPoolReturnsFullSizeBuffers(t *testing.T) {
	b := GetBuf()
	if len(*b) != dnsmsg.MaxMessageSize {
		t.Errorf("buffer is %d bytes, want %d", len(*b), dnsmsg.MaxMessageSize)
	}
	PutBuf(b)
}
