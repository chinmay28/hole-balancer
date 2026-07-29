// Package dnsmsg implements the small slice of DNS wire format that the
// balancer actually needs: reading headers and questions off incoming
// messages, building health-probe queries, and synthesising error responses.
//
// The balancer forwards queries byte-for-byte, so it deliberately does not
// implement a full message parser. Anything it cannot understand is passed
// through untouched rather than re-encoded.
package dnsmsg

import (
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
)

// HeaderLen is the fixed size of a DNS message header.
const HeaderLen = 12

// MaxMessageSize is the largest DNS message that can be framed over TCP, and
// therefore the largest we ever need to buffer.
const MaxMessageSize = 65535

// Common resource record types.
const (
	TypeA     uint16 = 1
	TypeNS    uint16 = 2
	TypeCNAME uint16 = 5
	TypeSOA   uint16 = 6
	TypePTR   uint16 = 12
	TypeMX    uint16 = 15
	TypeTXT   uint16 = 16
	TypeAAAA  uint16 = 28
	TypeSRV   uint16 = 33
	TypeOPT   uint16 = 41
	TypeANY   uint16 = 255
)

// ClassINET is the only class we care about.
const ClassINET uint16 = 1

// RCode is a DNS response code.
type RCode uint8

// Response codes defined by RFC 1035 and RFC 2136.
const (
	RCodeNoError  RCode = 0
	RCodeFormErr  RCode = 1
	RCodeServFail RCode = 2
	RCodeNXDomain RCode = 3
	RCodeNotImp   RCode = 4
	RCodeRefused  RCode = 5
)

var rcodeNames = map[RCode]string{
	RCodeNoError:  "NOERROR",
	RCodeFormErr:  "FORMERR",
	RCodeServFail: "SERVFAIL",
	RCodeNXDomain: "NXDOMAIN",
	RCodeNotImp:   "NOTIMP",
	RCodeRefused:  "REFUSED",
	6:             "YXDOMAIN",
	7:             "YXRRSET",
	8:             "NXRRSET",
	9:             "NOTAUTH",
	10:            "NOTZONE",
}

// String returns the mnemonic for the response code, falling back to a
// numeric form for codes we do not name.
func (r RCode) String() string {
	if n, ok := rcodeNames[r]; ok {
		return n
	}
	return fmt.Sprintf("RCODE%d", uint8(r))
}

// ParseRCode resolves a response code mnemonic such as "SERVFAIL". Parsing is
// case-insensitive.
func ParseRCode(s string) (RCode, error) {
	want := strings.ToUpper(strings.TrimSpace(s))
	for code, name := range rcodeNames {
		if name == want {
			return code, nil
		}
	}
	return 0, fmt.Errorf("unknown rcode %q", s)
}

var typeNames = map[string]uint16{
	"A": TypeA, "NS": TypeNS, "CNAME": TypeCNAME, "SOA": TypeSOA,
	"PTR": TypePTR, "MX": TypeMX, "TXT": TypeTXT, "AAAA": TypeAAAA,
	"SRV": TypeSRV, "ANY": TypeANY,
}

// ParseType resolves a record type mnemonic such as "AAAA".
func ParseType(s string) (uint16, error) {
	if t, ok := typeNames[strings.ToUpper(strings.TrimSpace(s))]; ok {
		return t, nil
	}
	return 0, fmt.Errorf("unknown record type %q", s)
}

// TypeString renders a record type for logs and metrics.
func TypeString(t uint16) string {
	for name, v := range typeNames {
		if v == t {
			return name
		}
	}
	return fmt.Sprintf("TYPE%d", t)
}

// Errors returned when a message cannot be understood.
var (
	ErrShortMessage = errors.New("dnsmsg: message shorter than header")
	ErrNoQuestion   = errors.New("dnsmsg: message has no question")
	ErrMalformed    = errors.New("dnsmsg: malformed message")
	ErrNameTooLong  = errors.New("dnsmsg: name exceeds 255 bytes")
	ErrLabelTooLong = errors.New("dnsmsg: label exceeds 63 bytes")
)

// Header is the decoded fixed-size portion of a DNS message.
type Header struct {
	ID      uint16
	QR      bool // true for responses
	Opcode  uint8
	AA      bool // authoritative answer
	TC      bool // truncated
	RD      bool // recursion desired
	RA      bool // recursion available
	RCode   RCode
	QDCount uint16
	ANCount uint16
	NSCount uint16
	ARCount uint16
}

// ParseHeader decodes the 12-byte header at the start of msg.
func ParseHeader(msg []byte) (Header, error) {
	if len(msg) < HeaderLen {
		return Header{}, ErrShortMessage
	}
	flags := binary.BigEndian.Uint16(msg[2:4])
	return Header{
		ID:      binary.BigEndian.Uint16(msg[0:2]),
		QR:      flags&0x8000 != 0,
		Opcode:  uint8(flags>>11) & 0x0f,
		AA:      flags&0x0400 != 0,
		TC:      flags&0x0200 != 0,
		RD:      flags&0x0100 != 0,
		RA:      flags&0x0080 != 0,
		RCode:   RCode(flags & 0x000f),
		QDCount: binary.BigEndian.Uint16(msg[4:6]),
		ANCount: binary.BigEndian.Uint16(msg[6:8]),
		NSCount: binary.BigEndian.Uint16(msg[8:10]),
		ARCount: binary.BigEndian.Uint16(msg[10:12]),
	}, nil
}

// ID returns the transaction ID of msg, or 0 if msg is too short.
func ID(msg []byte) uint16 {
	if len(msg) < 2 {
		return 0
	}
	return binary.BigEndian.Uint16(msg[0:2])
}

// Question is the first entry of a message's question section.
type Question struct {
	Name  string // fully qualified, with trailing dot
	Type  uint16
	Class uint16
}

// String renders the question the way dig would, for logging.
func (q Question) String() string {
	return q.Name + " " + TypeString(q.Type)
}

// ParseQuestion decodes the first question of msg. Compression pointers are
// rejected: nothing precedes the question section, so a pointer there can only
// be malformed or hostile.
func ParseQuestion(msg []byte) (Question, error) {
	hdr, err := ParseHeader(msg)
	if err != nil {
		return Question{}, err
	}
	if hdr.QDCount == 0 {
		return Question{}, ErrNoQuestion
	}
	name, off, err := parseName(msg, HeaderLen)
	if err != nil {
		return Question{}, err
	}
	if off+4 > len(msg) {
		return Question{}, ErrMalformed
	}
	return Question{
		Name:  name,
		Type:  binary.BigEndian.Uint16(msg[off : off+2]),
		Class: binary.BigEndian.Uint16(msg[off+2 : off+4]),
	}, nil
}

// parseName decodes an uncompressed domain name starting at off and returns
// the name in presentation form along with the offset just past it.
func parseName(msg []byte, off int) (string, int, error) {
	var sb strings.Builder
	total := 0
	for {
		if off >= len(msg) {
			return "", 0, ErrMalformed
		}
		l := int(msg[off])
		switch {
		case l == 0:
			off++
			if sb.Len() == 0 {
				return ".", off, nil
			}
			return sb.String(), off, nil
		case l&0xc0 != 0:
			// Compression pointer or reserved label type.
			return "", 0, ErrMalformed
		}
		off++
		if off+l > len(msg) {
			return "", 0, ErrMalformed
		}
		total += l + 1
		if total > 255 {
			return "", 0, ErrNameTooLong
		}
		writeEscaped(&sb, msg[off:off+l])
		sb.WriteByte('.')
		off += l
	}
}

// writeEscaped appends a label, escaping bytes that are not safe to print in
// presentation format.
func writeEscaped(sb *strings.Builder, label []byte) {
	for _, c := range label {
		switch {
		case c == '.' || c == '\\':
			sb.WriteByte('\\')
			sb.WriteByte(c)
		case c < '!' || c > '~':
			fmt.Fprintf(sb, "\\%03d", c)
		default:
			sb.WriteByte(c)
		}
	}
}

// packName encodes a presentation-format domain name into wire format.
func packName(name string) ([]byte, error) {
	if name == "" || name == "." {
		return []byte{0}, nil
	}
	name = strings.TrimSuffix(name, ".")
	out := make([]byte, 0, len(name)+2)
	for _, label := range strings.Split(name, ".") {
		if len(label) == 0 {
			return nil, fmt.Errorf("dnsmsg: empty label in %q", name)
		}
		if len(label) > 63 {
			return nil, ErrLabelTooLong
		}
		out = append(out, byte(len(label)))
		out = append(out, label...)
	}
	out = append(out, 0)
	if len(out) > 255 {
		return nil, ErrNameTooLong
	}
	return out, nil
}

// BuildQuery assembles a standard recursive query for name and qtype. It is
// used for health probes; client queries are never rebuilt.
func BuildQuery(id uint16, name string, qtype uint16) ([]byte, error) {
	qname, err := packName(name)
	if err != nil {
		return nil, err
	}
	msg := make([]byte, HeaderLen, HeaderLen+len(qname)+4)
	binary.BigEndian.PutUint16(msg[0:2], id)
	binary.BigEndian.PutUint16(msg[2:4], 0x0100) // RD
	binary.BigEndian.PutUint16(msg[4:6], 1)      // QDCOUNT
	msg = append(msg, qname...)
	msg = binary.BigEndian.AppendUint16(msg, qtype)
	msg = binary.BigEndian.AppendUint16(msg, ClassINET)
	return msg, nil
}

// ErrorResponse builds a minimal response to req carrying the given response
// code. The question section is echoed back but every other section is
// dropped, including any EDNS0 OPT record the client sent: an OPT belongs to
// the responder, and we have no opinion to express in one.
//
// It returns nil if req is too malformed to answer, in which case the caller
// should simply drop the query.
func ErrorResponse(req []byte, rcode RCode) []byte {
	hdr, err := ParseHeader(req)
	if err != nil || hdr.QR {
		return nil
	}

	end := HeaderLen
	if hdr.QDCount > 0 {
		_, off, err := parseName(req, HeaderLen)
		if err != nil || off+4 > len(req) {
			return nil
		}
		end = off + 4
	}

	resp := make([]byte, end)
	copy(resp, req[:end])

	// QR=1, RA=1, preserve Opcode and RD, clear AA/TC/Z.
	flags := uint16(0x8000) | uint16(0x0080) | uint16(rcode&0x0f)
	flags |= uint16(hdr.Opcode&0x0f) << 11
	if hdr.RD {
		flags |= 0x0100
	}
	binary.BigEndian.PutUint16(resp[2:4], flags)

	qd := uint16(0)
	if hdr.QDCount > 0 {
		qd = 1
	}
	binary.BigEndian.PutUint16(resp[4:6], qd)
	binary.BigEndian.PutUint16(resp[6:8], 0)  // ANCOUNT
	binary.BigEndian.PutUint16(resp[8:10], 0) // NSCOUNT
	binary.BigEndian.PutUint16(resp[10:12], 0)
	return resp
}

// ValidateResponse checks that resp is a plausible answer to a query with the
// given transaction ID. It guards against truncated reads and stray datagrams
// but deliberately does not compare the question section, which some
// forwarders normalise.
func ValidateResponse(resp []byte, wantID uint16) error {
	hdr, err := ParseHeader(resp)
	if err != nil {
		return err
	}
	if hdr.ID != wantID {
		return fmt.Errorf("dnsmsg: response id %d does not match query id %d", hdr.ID, wantID)
	}
	if !hdr.QR {
		return errors.New("dnsmsg: response has query bit clear")
	}
	return nil
}
