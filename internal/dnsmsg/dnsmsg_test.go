package dnsmsg

import (
	"encoding/binary"
	"testing"
)

func TestBuildQueryRoundTrip(t *testing.T) {
	for _, name := range []string{"example.com.", "example.com", "pi.hole.", "a.very.deep.sub.domain.test."} {
		msg, err := BuildQuery(0x1234, name, TypeA)
		if err != nil {
			t.Fatalf("BuildQuery(%q): %v", name, err)
		}

		hdr, err := ParseHeader(msg)
		if err != nil {
			t.Fatalf("ParseHeader: %v", err)
		}
		if hdr.ID != 0x1234 {
			t.Errorf("id = %#x, want 0x1234", hdr.ID)
		}
		if hdr.QR {
			t.Error("query should have QR clear")
		}
		if !hdr.RD {
			t.Error("query should request recursion")
		}
		if hdr.QDCount != 1 {
			t.Errorf("qdcount = %d, want 1", hdr.QDCount)
		}

		q, err := ParseQuestion(msg)
		if err != nil {
			t.Fatalf("ParseQuestion: %v", err)
		}
		want := name
		if want[len(want)-1] != '.' {
			want += "."
		}
		if q.Name != want {
			t.Errorf("name = %q, want %q", q.Name, want)
		}
		if q.Type != TypeA || q.Class != ClassINET {
			t.Errorf("type/class = %d/%d, want %d/%d", q.Type, q.Class, TypeA, ClassINET)
		}
	}
}

func TestBuildQueryRoot(t *testing.T) {
	msg, err := BuildQuery(1, ".", TypeNS)
	if err != nil {
		t.Fatalf("BuildQuery: %v", err)
	}
	q, err := ParseQuestion(msg)
	if err != nil {
		t.Fatalf("ParseQuestion: %v", err)
	}
	if q.Name != "." {
		t.Errorf("name = %q, want %q", q.Name, ".")
	}
}

func TestBuildQueryRejectsBadNames(t *testing.T) {
	long := ""
	for i := 0; i < 64; i++ {
		long += "a"
	}
	for _, name := range []string{long + ".com.", "double..dot.com."} {
		if _, err := BuildQuery(1, name, TypeA); err == nil {
			t.Errorf("BuildQuery(%q) succeeded, want error", name)
		}
	}
}

// A pointer in the question section cannot refer to anything, so it must be
// rejected rather than followed.
func TestParseQuestionRejectsCompressionPointer(t *testing.T) {
	msg := make([]byte, HeaderLen+6)
	binary.BigEndian.PutUint16(msg[4:6], 1) // QDCOUNT
	msg[HeaderLen] = 0xc0                   // pointer marker
	msg[HeaderLen+1] = 0x0c
	if _, err := ParseQuestion(msg); err == nil {
		t.Fatal("ParseQuestion accepted a compression pointer")
	}
}

func TestParseQuestionRejectsTruncated(t *testing.T) {
	full, err := BuildQuery(1, "example.com.", TypeA)
	if err != nil {
		t.Fatal(err)
	}
	for n := 0; n < len(full); n++ {
		if _, err := ParseQuestion(full[:n]); err == nil {
			t.Errorf("ParseQuestion accepted a message truncated to %d bytes", n)
		}
	}
}

func TestParseQuestionNoQuestion(t *testing.T) {
	msg := make([]byte, HeaderLen)
	if _, err := ParseQuestion(msg); err != ErrNoQuestion {
		t.Fatalf("err = %v, want %v", err, ErrNoQuestion)
	}
}

func TestNameEscaping(t *testing.T) {
	// A label containing a dot and a control byte must come back escaped, not
	// silently mangled into extra labels.
	msg := make([]byte, 0, 32)
	msg = append(msg, make([]byte, HeaderLen)...)
	binary.BigEndian.PutUint16(msg[4:6], 1)
	label := []byte{'a', '.', 'b', 0x07}
	msg = append(msg, byte(len(label)))
	msg = append(msg, label...)
	msg = append(msg, 0)
	msg = binary.BigEndian.AppendUint16(msg, TypeA)
	msg = binary.BigEndian.AppendUint16(msg, ClassINET)

	q, err := ParseQuestion(msg)
	if err != nil {
		t.Fatalf("ParseQuestion: %v", err)
	}
	if want := `a\.b\007.`; q.Name != want {
		t.Errorf("name = %q, want %q", q.Name, want)
	}
}

func TestErrorResponse(t *testing.T) {
	req, err := BuildQuery(0xbeef, "blocked.example.", TypeAAAA)
	if err != nil {
		t.Fatal(err)
	}
	// Append an OPT-like trailer to confirm additional sections are dropped.
	req = append(req, 0x00, 0x00, 0x29, 0x10, 0x00)
	binary.BigEndian.PutUint16(req[10:12], 1) // ARCOUNT

	resp := ErrorResponse(req, RCodeServFail)
	if resp == nil {
		t.Fatal("ErrorResponse returned nil for a valid query")
	}

	hdr, err := ParseHeader(resp)
	if err != nil {
		t.Fatalf("ParseHeader: %v", err)
	}
	if hdr.ID != 0xbeef {
		t.Errorf("id = %#x, want 0xbeef", hdr.ID)
	}
	if !hdr.QR {
		t.Error("response must have QR set")
	}
	if !hdr.RA {
		t.Error("response should advertise recursion available")
	}
	if !hdr.RD {
		t.Error("response should mirror the request's RD bit")
	}
	if hdr.RCode != RCodeServFail {
		t.Errorf("rcode = %v, want SERVFAIL", hdr.RCode)
	}
	if hdr.QDCount != 1 || hdr.ANCount != 0 || hdr.NSCount != 0 || hdr.ARCount != 0 {
		t.Errorf("counts = %d/%d/%d/%d, want 1/0/0/0",
			hdr.QDCount, hdr.ANCount, hdr.NSCount, hdr.ARCount)
	}

	q, err := ParseQuestion(resp)
	if err != nil {
		t.Fatalf("ParseQuestion: %v", err)
	}
	if q.Name != "blocked.example." || q.Type != TypeAAAA {
		t.Errorf("question = %v, want blocked.example. AAAA", q)
	}
}

func TestErrorResponseRefusesNonQueries(t *testing.T) {
	resp, err := BuildQuery(1, "example.com.", TypeA)
	if err != nil {
		t.Fatal(err)
	}
	binary.BigEndian.PutUint16(resp[2:4], 0x8180) // set QR

	if got := ErrorResponse(resp, RCodeServFail); got != nil {
		t.Error("ErrorResponse should refuse to answer a response")
	}
	if got := ErrorResponse([]byte{1, 2, 3}, RCodeServFail); got != nil {
		t.Error("ErrorResponse should refuse a message shorter than a header")
	}
}

func TestValidateResponse(t *testing.T) {
	msg, err := BuildQuery(0x0102, "example.com.", TypeA)
	if err != nil {
		t.Fatal(err)
	}

	if err := ValidateResponse(msg, 0x0102); err == nil {
		t.Error("a message with QR clear must not validate as a response")
	}

	binary.BigEndian.PutUint16(msg[2:4], 0x8180)
	if err := ValidateResponse(msg, 0x0102); err != nil {
		t.Errorf("valid response rejected: %v", err)
	}
	if err := ValidateResponse(msg, 0x0999); err == nil {
		t.Error("mismatched transaction id must be rejected")
	}
	if err := ValidateResponse(msg[:5], 0x0102); err == nil {
		t.Error("short response must be rejected")
	}
}

func TestRCodeNames(t *testing.T) {
	if got := RCodeServFail.String(); got != "SERVFAIL" {
		t.Errorf("got %q, want SERVFAIL", got)
	}
	if got := RCode(200).String(); got != "RCODE200" {
		t.Errorf("got %q, want RCODE200", got)
	}
	got, err := ParseRCode(" servfail ")
	if err != nil || got != RCodeServFail {
		t.Errorf("ParseRCode = %v, %v; want SERVFAIL, nil", got, err)
	}
	if _, err := ParseRCode("NOPE"); err == nil {
		t.Error("ParseRCode accepted an unknown mnemonic")
	}
}

func TestParseType(t *testing.T) {
	if got, err := ParseType("aaaa"); err != nil || got != TypeAAAA {
		t.Errorf("ParseType(aaaa) = %d, %v", got, err)
	}
	if _, err := ParseType("QQQQ"); err == nil {
		t.Error("ParseType accepted an unknown type")
	}
	if got := TypeString(TypeAAAA); got != "AAAA" {
		t.Errorf("TypeString = %q, want AAAA", got)
	}
}
