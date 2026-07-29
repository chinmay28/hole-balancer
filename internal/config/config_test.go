package config

import (
	"strings"
	"testing"
	"time"

	"github.com/chinmay28/hole-balancer/internal/dnsmsg"
)

const minimal = `
upstreams:
  - name: pihole-1
    endpoints: [192.168.1.10]
`

func TestParseAppliesDefaults(t *testing.T) {
	cfg, err := Parse([]byte(minimal))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.Strategy != StrategyRandom {
		t.Errorf("strategy = %q, want %q", cfg.Strategy, StrategyRandom)
	}
	if cfg.Listen.UDP != ":53" || cfg.Listen.TCP != ":53" {
		t.Errorf("listen = %+v, want :53 on both", cfg.Listen)
	}
	if cfg.Query.Timeout.D() != 2*time.Second {
		t.Errorf("query.timeout = %v, want 2s", cfg.Query.Timeout)
	}
	if !cfg.Health.Passive.Enabled {
		t.Error("passive health checks should default to on")
	}
	if cfg.Upstreams[0].Weight != 1 {
		t.Errorf("weight = %d, want 1", cfg.Upstreams[0].Weight)
	}
	if got := cfg.Upstreams[0].Endpoints[0].Addr; got != "192.168.1.10:53" {
		t.Errorf("addr = %q, want the default DNS port appended", got)
	}
	if !cfg.Query.ShouldRetryRCode(dnsmsg.RCodeServFail) {
		t.Error("SERVFAIL should be retried by default")
	}
	if cfg.Query.ShouldRetryRCode(dnsmsg.RCodeNXDomain) {
		t.Error("NXDOMAIN must never be retried: it is how Pi-hole reports a blocked domain")
	}
}

func TestParseEndpointForms(t *testing.T) {
	cfg, err := Parse([]byte(`
upstreams:
  - name: pihole-1
    endpoints:
      - 192.168.1.10
      - addr: 100.64.0.5:5353
        name: tailscale
      - "[fd7a:115c::1]:53"
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	eps := cfg.Upstreams[0].Endpoints
	want := []Endpoint{
		{Name: "192.168.1.10:53", Addr: "192.168.1.10:53"},
		{Name: "tailscale", Addr: "100.64.0.5:5353"},
		{Name: "[fd7a:115c::1]:53", Addr: "[fd7a:115c::1]:53"},
	}
	if len(eps) != len(want) {
		t.Fatalf("got %d endpoints, want %d", len(eps), len(want))
	}
	for i := range want {
		if eps[i] != want[i] {
			t.Errorf("endpoint %d = %+v, want %+v", i, eps[i], want[i])
		}
	}
}

func TestNormaliseAddr(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{in: "192.168.1.10", want: "192.168.1.10:53"},
		{in: "192.168.1.10:5353", want: "192.168.1.10:5353"},
		{in: "pihole.lan", want: "pihole.lan:53"},
		{in: "fd7a:115c::1", want: "[fd7a:115c::1]:53"},
		{in: "[fd7a:115c::1]:5353", want: "[fd7a:115c::1]:5353"},
		{in: "  192.168.1.10  ", want: "192.168.1.10:53"},
		{in: "", wantErr: true},
		{in: ":53", wantErr: true},
		{in: "host:notaport", wantErr: true},
		{in: "not:a:valid:thing", wantErr: true},
	}
	for _, tc := range cases {
		got, err := normaliseAddr(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("normaliseAddr(%q) = %q, want error", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("normaliseAddr(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("normaliseAddr(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// The same Pi-hole listed as two upstreams would take two slots in the random
// draw and defeat the point of the grouping, so it has to be an error.
func TestDuplicateAddressAcrossUpstreamsIsRejected(t *testing.T) {
	_, err := Parse([]byte(`
upstreams:
  - name: lan
    endpoints: [192.168.1.10]
  - name: tailscale
    endpoints: [192.168.1.10:53]
`))
	if err == nil {
		t.Fatal("Parse accepted the same address under two upstreams")
	}
	if !strings.Contains(err.Error(), "single upstream") {
		t.Errorf("error should explain the grouping rule, got: %v", err)
	}
}

func TestValidationErrors(t *testing.T) {
	cases := map[string]string{
		"no upstreams":     "strategy: random\n",
		"unknown strategy": "strategy: sticky\n" + minimal,
		"unknown key":      "stratergy: random\n" + minimal,
		"bad rcode":        "query:\n  retry_rcodes: [BOOM]\n" + minimal,
		"bad probe type":   "health:\n  probe:\n    type: XYZZY\n" + minimal,
		"bad log level":    "log:\n  level: chatty\n" + minimal,
		"zero attempts":    "query:\n  max_attempts: 0\n" + minimal,
		"zero interval":    "health:\n  interval: 0s\n" + minimal,
		"negative weight":  "upstreams:\n  - name: a\n    weight: -1\n    endpoints: [1.2.3.4]\n",
		"no endpoints":     "upstreams:\n  - name: a\n    endpoints: []\n",
		"duplicate name":   "upstreams:\n  - name: a\n    endpoints: [1.2.3.4]\n  - name: a\n    endpoints: [1.2.3.5]\n",
		"no listeners":     "listen:\n  udp: \"\"\n  tcp: \"\"\n" + minimal,
		"bad duration":     "query:\n  timeout: soon\n" + minimal,
	}
	for name, doc := range cases {
		if _, err := Parse([]byte(doc)); err == nil {
			t.Errorf("%s: Parse succeeded, want an error", name)
		}
	}
}

func TestProbeNameGetsTrailingDot(t *testing.T) {
	cfg, err := Parse([]byte("health:\n  probe:\n    name: pi.hole\n" + minimal))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.Health.Probe.Name != "pi.hole." {
		t.Errorf("probe name = %q, want %q", cfg.Health.Probe.Name, "pi.hole.")
	}
	if cfg.Health.Probe.QType() != dnsmsg.TypeA {
		t.Errorf("probe qtype = %d, want A", cfg.Health.Probe.QType())
	}
}

func TestDurationForms(t *testing.T) {
	cfg, err := Parse([]byte("query:\n  timeout: 1500ms\nhealth:\n  interval: \"30\"\n" + minimal))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.Query.Timeout.D() != 1500*time.Millisecond {
		t.Errorf("timeout = %v, want 1.5s", cfg.Query.Timeout)
	}
	if cfg.Health.Interval.D() != 30*time.Second {
		t.Errorf("interval = %v, want 30s", cfg.Health.Interval)
	}
}

func TestUnnamedUpstreamsGetNames(t *testing.T) {
	cfg, err := Parse([]byte("upstreams:\n  - endpoints: [1.2.3.4]\n  - endpoints: [1.2.3.5]\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.Upstreams[0].Name != "upstream-1" || cfg.Upstreams[1].Name != "upstream-2" {
		t.Errorf("generated names = %q, %q", cfg.Upstreams[0].Name, cfg.Upstreams[1].Name)
	}
}
