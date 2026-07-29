package config

import (
	"os"
	"path/filepath"
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

func TestFallbackDefaults(t *testing.T) {
	cfg, err := Parse([]byte(minimal))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !cfg.Fallback.Enabled {
		t.Error("fallback should be on by default: losing every Pi-hole should not take DNS down")
	}
	want := []string{"8.8.8.8:53", "8.8.4.4:53"}
	if len(cfg.Fallback.Servers) != len(want) {
		t.Fatalf("servers = %v, want %v", cfg.Fallback.Servers, want)
	}
	for i := range want {
		if cfg.Fallback.Servers[i] != want[i] {
			t.Errorf("servers[%d] = %q, want %q (the default DNS port should be filled in)",
				i, cfg.Fallback.Servers[i], want[i])
		}
	}
	if cfg.Fallback.SummaryInterval.D() != 24*time.Hour {
		t.Errorf("summary_interval = %v, want 24h", cfg.Fallback.SummaryInterval)
	}
}

func TestFallbackCanBeDisabledWithoutServers(t *testing.T) {
	cfg, err := Parse([]byte("fallback:\n  enabled: false\n  servers: []\n" + minimal))
	if err != nil {
		t.Fatalf("disabling fallback should not require servers: %v", err)
	}
	if cfg.Fallback.Enabled {
		t.Error("fallback should be off")
	}
}

func TestFallbackValidation(t *testing.T) {
	cases := map[string]string{
		"enabled with no servers": "fallback:\n  enabled: true\n  servers: []\n" + minimal,
		"bad server address":      "fallback:\n  servers: [\"not:a:host\"]\n" + minimal,
		"duplicate servers":       "fallback:\n  servers: [8.8.8.8, 8.8.8.8:53]\n" + minimal,
		"zero timeout":            "fallback:\n  timeout: 0s\n" + minimal,
		"zero summary interval":   "fallback:\n  summary_interval: 0s\n" + minimal,
	}
	for name, doc := range cases {
		if _, err := Parse([]byte(doc)); err == nil {
			t.Errorf("%s: Parse succeeded, want an error", name)
		}
	}
}

func TestFallbackServersAreOverriddenNotAppended(t *testing.T) {
	cfg, err := Parse([]byte("fallback:\n  servers: [1.1.1.1]\n" + minimal))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(cfg.Fallback.Servers) != 1 || cfg.Fallback.Servers[0] != "1.1.1.1:53" {
		t.Errorf("servers = %v, want the configured list to replace the defaults", cfg.Fallback.Servers)
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

// A config the balancer writes must survive its own restart. The duration
// fields are the trap: written as a raw integer they would read back as that
// many seconds.
func TestSaveReloadsIdentically(t *testing.T) {
	orig, err := Parse([]byte(`
strategy: least-latency
query:
  timeout: 1500ms
health:
  interval: 45s
  probe: {name: pi.hole, type: AAAA}
fallback:
  servers: [1.1.1.1, 9.9.9.9:5353]
  summary_interval: 12h
upstreams:
  - name: a
    weight: 3
    endpoints: [192.168.1.10, "[fd00::1]:5353"]
  - name: b
    endpoints: [192.168.1.11]
`))
	if err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := orig.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}

	back, err := Load(path)
	if err != nil {
		t.Fatalf("saved config does not reload: %v", err)
	}

	checks := []struct {
		name      string
		got, want any
	}{
		{"strategy", back.Strategy, orig.Strategy},
		{"query.timeout", back.Query.Timeout, orig.Query.Timeout},
		{"health.interval", back.Health.Interval, orig.Health.Interval},
		{"probe.name", back.Health.Probe.Name, orig.Health.Probe.Name},
		{"probe.type", back.Health.Probe.QType(), orig.Health.Probe.QType()},
		{"summary_interval", back.Fallback.SummaryInterval, orig.Fallback.SummaryInterval},
		{"upstream count", len(back.Upstreams), len(orig.Upstreams)},
		{"weight", back.Upstreams[0].Weight, orig.Upstreams[0].Weight},
		{"endpoint", back.Upstreams[0].Endpoints[1].Addr, orig.Upstreams[0].Endpoints[1].Addr},
		{"fallback server", back.Fallback.Servers[1], orig.Fallback.Servers[1]},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v after a save/load round trip, want %v", c.name, c.got, c.want)
		}
	}
}

func TestSaveIsAtomicAndKeepsABackup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	first, _ := Parse([]byte(minimal))
	if err := first.Save(path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + ".bak"); !os.IsNotExist(err) {
		t.Error("the first save should not leave a backup: there was nothing to back up")
	}

	second := first.Clone()
	second.Strategy = StrategyFailover
	if err := second.Save(path); err != nil {
		t.Fatal(err)
	}

	prev, err := os.ReadFile(path + ".bak")
	if err != nil {
		t.Fatalf("no backup after the second save: %v", err)
	}
	if !strings.Contains(string(prev), "strategy: random") {
		t.Errorf("backup does not hold the previous version:\n%s", prev)
	}

	// The temporary file used for the atomic rename must not be left behind.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".hole-balancer-config-") {
			t.Errorf("a temporary file was left behind: %s", e.Name())
		}
	}
}

func TestSaveReportsAnUnwritableDirectory(t *testing.T) {
	cfg, _ := Parse([]byte(minimal))
	if err := cfg.Save(filepath.Join(t.TempDir(), "no", "such", "dir", "c.yaml")); err == nil {
		t.Error("Save should fail when the directory does not exist")
	}
}

func TestCloneIsDeep(t *testing.T) {
	orig, err := Parse([]byte("fallback:\n  servers: [1.1.1.1]\n" + minimal))
	if err != nil {
		t.Fatal(err)
	}

	clone := orig.Clone()
	clone.Upstreams[0].Name = "changed"
	clone.Upstreams[0].Endpoints[0].Addr = "9.9.9.9:53"
	clone.Fallback.Servers[0] = "6.6.6.6:53"
	clone.Query.RetryRCodes[0] = "NXDOMAIN"

	if orig.Upstreams[0].Name == "changed" ||
		orig.Upstreams[0].Endpoints[0].Addr == "9.9.9.9:53" ||
		orig.Fallback.Servers[0] == "6.6.6.6:53" ||
		orig.Query.RetryRCodes[0] == "NXDOMAIN" {
		t.Error("Clone shares state with the original")
	}
	// The derived lookup table must come across, or a clone silently stops
	// retrying.
	if !clone.Query.ShouldRetryRCode(dnsmsg.RCodeServFail) {
		t.Error("clone lost its parsed retry codes")
	}
}

func TestPrepareUpstream(t *testing.T) {
	got, err := PrepareUpstream(Upstream{
		Name:      "  pihole-1  ",
		Endpoints: []Endpoint{{Addr: "192.168.1.10"}, {Name: "ts", Addr: "100.64.0.5:5353"}},
	})
	if err != nil {
		t.Fatalf("PrepareUpstream: %v", err)
	}
	if got.Name != "pihole-1" {
		t.Errorf("name = %q, want it trimmed", got.Name)
	}
	if got.Weight != 1 {
		t.Errorf("weight = %d, want the default 1", got.Weight)
	}
	if got.Endpoints[0].Addr != "192.168.1.10:53" || got.Endpoints[0].Name != "192.168.1.10:53" {
		t.Errorf("endpoint 0 = %+v", got.Endpoints[0])
	}
	if got.Endpoints[1].Name != "ts" {
		t.Errorf("an explicit label should be kept, got %q", got.Endpoints[1].Name)
	}
}

func TestPrepareUpstreamRejections(t *testing.T) {
	cases := map[string]Upstream{
		"empty name":        {Endpoints: []Endpoint{{Addr: "1.2.3.4"}}},
		"blank name":        {Name: "   ", Endpoints: []Endpoint{{Addr: "1.2.3.4"}}},
		"slash in name":     {Name: "a/b", Endpoints: []Endpoint{{Addr: "1.2.3.4"}}},
		"no endpoints":      {Name: "a"},
		"negative weight":   {Name: "a", Weight: -1, Endpoints: []Endpoint{{Addr: "1.2.3.4"}}},
		"bad address":       {Name: "a", Endpoints: []Endpoint{{Addr: "not:a:host"}}},
		"repeated endpoint": {Name: "a", Endpoints: []Endpoint{{Addr: "1.2.3.4"}, {Addr: "1.2.3.4:53"}}},
	}
	for name, u := range cases {
		if _, err := PrepareUpstream(u); err == nil {
			t.Errorf("%s: PrepareUpstream succeeded, want an error", name)
		}
	}
}

func TestStrategyHelpers(t *testing.T) {
	if !ValidStrategy(StrategyLeastLatency) || ValidStrategy("sticky") {
		t.Error("ValidStrategy is wrong")
	}
	if len(Strategies()) != 4 {
		t.Errorf("Strategies() = %v", Strategies())
	}
	// The returned slice must be a copy, or a caller could corrupt the list.
	Strategies()[0] = "mutated"
	if Strategies()[0] == "mutated" {
		t.Error("Strategies() hands out the package-level slice")
	}
}

func TestNormaliseAddrIsExported(t *testing.T) {
	got, err := NormaliseAddr("192.168.1.10")
	if err != nil || got != "192.168.1.10:53" {
		t.Errorf("NormaliseAddr = %q, %v", got, err)
	}
}
