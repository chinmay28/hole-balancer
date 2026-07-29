// Package config loads and validates the balancer's YAML configuration.
package config

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/chinmay28/hole-balancer/internal/dnsmsg"
)

// DefaultDNSPort is appended to upstream addresses that omit a port.
const DefaultDNSPort = "53"

// Duration wraps time.Duration so durations can be written as "2s" in YAML.
type Duration time.Duration

// UnmarshalYAML accepts either a duration string ("500ms") or a bare number of
// seconds.
func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	var s string
	if err := node.Decode(&s); err != nil {
		return fmt.Errorf("line %d: duration must be a string like \"2s\"", node.Line)
	}
	if secs, err := strconv.ParseFloat(s, 64); err == nil {
		*d = Duration(time.Duration(secs * float64(time.Second)))
		return nil
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("line %d: invalid duration %q", node.Line, s)
	}
	*d = Duration(parsed)
	return nil
}

// MarshalYAML writes a duration back as the string form it was read in.
//
// Without this, saving would emit a raw nanosecond count, which the parser
// above would then read back as that many *seconds* — a config the balancer
// wrote would not survive its own restart.
func (d Duration) MarshalYAML() (any, error) { return time.Duration(d).String(), nil }

// D returns the wrapped time.Duration.
func (d Duration) D() time.Duration { return time.Duration(d) }

func (d Duration) String() string { return time.Duration(d).String() }

// Config is the top-level configuration document.
type Config struct {
	Listen    Listen     `yaml:"listen"`
	Strategy  string     `yaml:"strategy"`
	Query     Query      `yaml:"query"`
	Health    Health     `yaml:"health"`
	Fallback  Fallback   `yaml:"fallback"`
	Admin     Admin      `yaml:"admin"`
	Log       Log        `yaml:"log"`
	Upstreams []Upstream `yaml:"upstreams"`
}

// Listen describes the client-facing sockets.
type Listen struct {
	UDP string `yaml:"udp"`
	TCP string `yaml:"tcp"`
}

// Query controls how client queries are forwarded and retried.
type Query struct {
	// Timeout bounds a single attempt against a single endpoint, not the
	// whole query.
	Timeout     Duration `yaml:"timeout"`
	MaxAttempts int      `yaml:"max_attempts"`
	// MaxConcurrent caps queries in flight. Beyond it the balancer sheds load
	// with SERVFAIL rather than growing its memory use without bound.
	MaxConcurrent int `yaml:"max_concurrent"`
	// RetryRCodes are response codes that indicate the upstream itself is
	// unwell, so the query is worth re-asking elsewhere. Blocked domains come
	// back as NXDOMAIN or an answer, never as one of these, so retrying here
	// cannot defeat Pi-hole's filtering.
	RetryRCodes []string `yaml:"retry_rcodes"`

	retryRCodes map[dnsmsg.RCode]bool
}

// ShouldRetryRCode reports whether a response with this code should be
// re-asked against another upstream.
func (q *Query) ShouldRetryRCode(r dnsmsg.RCode) bool { return q.retryRCodes[r] }

// Health configures active probing and passive failure tracking.
type Health struct {
	Interval Duration `yaml:"interval"`
	Timeout  Duration `yaml:"timeout"`
	// Rise is the number of consecutive successes needed to bring a down
	// endpoint back into rotation.
	Rise int `yaml:"rise"`
	// Fall is the number of consecutive failures that take an endpoint out.
	Fall int `yaml:"fall"`
	// StartupTimeout bounds the initial probe sweep performed before the
	// listeners start accepting traffic.
	StartupTimeout Duration `yaml:"startup_timeout"`
	Probe          Probe    `yaml:"probe"`
	Passive        Passive  `yaml:"passive"`
}

// Probe is the query used to test whether an endpoint is answering.
type Probe struct {
	Name string `yaml:"name"`
	Type string `yaml:"type"`
	// RequireAnswer demands a non-empty answer section. Off by default so that
	// a probe name landing on a blocklist does not mark a healthy Pi-hole down.
	RequireAnswer bool `yaml:"require_answer"`

	qtype uint16
}

// QType returns the resolved probe record type.
func (p *Probe) QType() uint16 { return p.qtype }

// Passive turns live client traffic into a health signal, which detects a
// dead upstream far sooner than the probe interval alone.
type Passive struct {
	Enabled bool `yaml:"enabled"`
}

// Fallback is the last line of defence: public resolvers used only when no
// Pi-hole can answer a query.
//
// These queries are NOT filtered — a public resolver knows nothing about your
// blocklists — so this trades ad blocking for the network staying usable
// during a total Pi-hole outage. Set enabled to false if you would rather DNS
// fail than go unfiltered.
type Fallback struct {
	Enabled bool     `yaml:"enabled"`
	Servers []string `yaml:"servers"`
	// Timeout bounds one attempt against one public resolver.
	Timeout Duration `yaml:"timeout"`
	// SummaryInterval is how often the accumulated fallback usage is written
	// to the log. Fallback is never logged per query: during an outage that
	// would be thousands of identical lines.
	SummaryInterval Duration `yaml:"summary_interval"`
}

// Admin configures the HTTP status, metrics, and control endpoints.
type Admin struct {
	Listen string `yaml:"listen"`
	// AllowControl enables the endpoints that drain and restore upstreams.
	AllowControl bool `yaml:"allow_control"`
}

// Log configures diagnostic output.
type Log struct {
	Level string `yaml:"level"`
	// Queries logs one line per client query. Useful when tuning, noisy in
	// steady state, and a privacy consideration since it records every domain
	// every device on the network looks up.
	Queries bool `yaml:"queries"`
}

// Upstream is a single Pi-hole, reachable through one or more network paths.
type Upstream struct {
	Name      string     `yaml:"name"`
	Weight    int        `yaml:"weight"`
	Endpoints []Endpoint `yaml:"endpoints"`
}

// Endpoint is one address for an upstream. In YAML it may be written as a
// bare address or as a mapping with a label:
//
//	endpoints:
//	  - 192.168.1.10
//	  - {name: tailscale, addr: 100.64.0.5}
type Endpoint struct {
	Name string `yaml:"name"`
	Addr string `yaml:"addr"`
}

// UnmarshalYAML accepts either the scalar or the mapping form.
func (e *Endpoint) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode {
		var s string
		if err := node.Decode(&s); err != nil {
			return err
		}
		e.Addr = s
		return nil
	}
	type raw Endpoint // avoid recursing into this method
	var r raw
	if err := node.Decode(&r); err != nil {
		return err
	}
	*e = Endpoint(r)
	return nil
}

// Selection strategies.
const (
	StrategyRandom       = "random"
	StrategyRoundRobin   = "round-robin"
	StrategyFailover     = "failover"
	StrategyLeastLatency = "least-latency"
)

var validStrategies = []string{StrategyRandom, StrategyRoundRobin, StrategyFailover, StrategyLeastLatency}

// Default returns a configuration with every optional field populated.
func Default() Config {
	return Config{
		Listen:   Listen{UDP: ":53", TCP: ":53"},
		Strategy: StrategyRandom,
		Query: Query{
			Timeout:       Duration(2 * time.Second),
			MaxAttempts:   3,
			MaxConcurrent: 1024,
			RetryRCodes:   []string{"SERVFAIL", "REFUSED"},
		},
		Health: Health{
			Interval:       Duration(10 * time.Second),
			Timeout:        Duration(2 * time.Second),
			Rise:           2,
			Fall:           2,
			StartupTimeout: Duration(3 * time.Second),
			Probe: Probe{
				Name: "dns.google.",
				Type: "A",
			},
			Passive: Passive{Enabled: true},
		},
		Fallback: Fallback{
			Enabled:         true,
			Servers:         []string{"8.8.8.8", "8.8.4.4"},
			Timeout:         Duration(2 * time.Second),
			SummaryInterval: Duration(24 * time.Hour),
		},
		Admin: Admin{Listen: "127.0.0.1:8053"},
		Log:   Log{Level: "info"},
	}
}

// ValidStrategy reports whether name is a selection strategy the balancer
// implements.
func ValidStrategy(name string) bool { return contains(validStrategies, name) }

// Strategies lists the selection strategies, for the management interface.
func Strategies() []string { return append([]string(nil), validStrategies...) }

// NormaliseAddr validates a DNS server address and supplies the default port
// when one is not given. It is exported so the management interface can check
// an address the moment it is typed, using exactly the rules the config file
// obeys.
func NormaliseAddr(addr string) (string, error) { return normaliseAddr(addr) }

// PrepareUpstream fills in an upstream's defaults and normalises its
// addresses, returning the cleaned copy. It is the single definition of what a
// well-formed upstream is, shared by the config file and the management API so
// the two can never drift.
func PrepareUpstream(u Upstream) (Upstream, error) {
	u.Name = strings.TrimSpace(u.Name)
	if u.Name == "" {
		return u, fmt.Errorf("name must not be empty")
	}
	if strings.ContainsAny(u.Name, "/?#\\") {
		return u, fmt.Errorf("name must not contain /, ?, # or backslash")
	}
	if u.Weight == 0 {
		u.Weight = 1
	}
	if u.Weight < 0 {
		return u, fmt.Errorf("weight must not be negative")
	}
	if len(u.Endpoints) == 0 {
		return u, fmt.Errorf("at least one endpoint is required")
	}

	seen := make(map[string]bool, len(u.Endpoints))
	cleaned := make([]Endpoint, 0, len(u.Endpoints))
	for i, ep := range u.Endpoints {
		addr, err := normaliseAddr(ep.Addr)
		if err != nil {
			return u, fmt.Errorf("endpoint %d: %w", i+1, err)
		}
		if seen[addr] {
			return u, fmt.Errorf("endpoint %s is listed twice", addr)
		}
		seen[addr] = true
		ep.Addr = addr
		if ep.Name == "" {
			ep.Name = addr
		}
		cleaned = append(cleaned, ep)
	}
	u.Endpoints = cleaned
	return u, nil
}

// managedHeader is prepended to any configuration the balancer writes itself.
const managedHeader = `# hole-balancer configuration.
#
# This file is written by the management interface. Comments are not preserved
# across a save, so keep notes elsewhere if you edit it by hand.
#
# The full annotated reference lives in config.example.yaml.
`

// Save writes the configuration to path in YAML.
//
// The write goes to a temporary file in the same directory and is then
// renamed, so a crash midway leaves the previous configuration intact rather
// than a half-written one that will not parse on the next boot. The previous
// contents are kept alongside as <path>.bak.
func (c *Config) Save(path string) error {
	body, err := yaml.Marshal(c)
	if err != nil {
		return fmt.Errorf("encoding config: %w", err)
	}
	out := append([]byte(managedHeader), body...)

	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".hole-balancer-config-*.yaml")
	if err != nil {
		return fmt.Errorf("creating temp file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename succeeds

	if _, err := tmp.Write(out); err != nil {
		tmp.Close()
		return fmt.Errorf("writing config: %w", err)
	}
	// Flush to disk before the rename, so the rename cannot expose an empty file.
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("syncing config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return err
	}

	if prev, err := os.ReadFile(path); err == nil {
		if err := os.WriteFile(path+".bak", prev, 0o600); err != nil {
			return fmt.Errorf("writing backup: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("reading existing config: %w", err)
	}

	if err := os.Chmod(tmpName, 0o644); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// Clone returns a deep copy, so a caller can stage edits without disturbing
// the configuration the running server is reading.
func (c *Config) Clone() *Config {
	out := *c
	out.Query.RetryRCodes = append([]string(nil), c.Query.RetryRCodes...)
	out.Fallback.Servers = append([]string(nil), c.Fallback.Servers...)
	out.Upstreams = make([]Upstream, len(c.Upstreams))
	for i, u := range c.Upstreams {
		u.Endpoints = append([]Endpoint(nil), u.Endpoints...)
		out.Upstreams[i] = u
	}
	if c.Query.retryRCodes != nil {
		out.Query.retryRCodes = make(map[dnsmsg.RCode]bool, len(c.Query.retryRCodes))
		for k, v := range c.Query.retryRCodes {
			out.Query.retryRCodes[k] = v
		}
	}
	return &out
}

// Load reads, parses, and validates the configuration file at path.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(data)
}

// Parse decodes a configuration document, layering it over the defaults.
func Parse(data []byte) (*Config, error) {
	cfg := Default()
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true) // typos in key names should fail loudly
	if err := dec.Decode(&cfg); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("config is empty: at least one upstream must be configured")
		}
		return nil, fmt.Errorf("parsing config: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// Validate checks the configuration and resolves the fields derived from it,
// such as the parsed retry response codes and probe record type.
//
// Parse calls it for you. A Config built in code — in a test, or by an
// embedder — must call it before use: until it runs, the derived fields are
// empty and behaviour that depends on them is silently wrong.
func (c *Config) Validate() error {
	if c.Listen.UDP == "" && c.Listen.TCP == "" {
		return fmt.Errorf("listen: at least one of udp or tcp must be set")
	}

	if !contains(validStrategies, c.Strategy) {
		return fmt.Errorf("strategy: %q is not one of %s", c.Strategy, strings.Join(validStrategies, ", "))
	}

	if c.Query.Timeout <= 0 {
		return fmt.Errorf("query.timeout must be positive")
	}
	if c.Query.MaxAttempts < 1 {
		return fmt.Errorf("query.max_attempts must be at least 1")
	}
	if c.Query.MaxConcurrent < 1 {
		return fmt.Errorf("query.max_concurrent must be at least 1")
	}
	c.Query.retryRCodes = make(map[dnsmsg.RCode]bool, len(c.Query.RetryRCodes))
	for _, name := range c.Query.RetryRCodes {
		code, err := dnsmsg.ParseRCode(name)
		if err != nil {
			return fmt.Errorf("query.retry_rcodes: %w", err)
		}
		c.Query.retryRCodes[code] = true
	}

	if c.Health.Interval <= 0 {
		return fmt.Errorf("health.interval must be positive")
	}
	if c.Health.Timeout <= 0 {
		return fmt.Errorf("health.timeout must be positive")
	}
	if c.Health.Rise < 1 {
		return fmt.Errorf("health.rise must be at least 1")
	}
	if c.Health.Fall < 1 {
		return fmt.Errorf("health.fall must be at least 1")
	}
	qtype, err := dnsmsg.ParseType(c.Health.Probe.Type)
	if err != nil {
		return fmt.Errorf("health.probe.type: %w", err)
	}
	c.Health.Probe.qtype = qtype
	if c.Health.Probe.Name == "" {
		return fmt.Errorf("health.probe.name must not be empty")
	}
	if !strings.HasSuffix(c.Health.Probe.Name, ".") {
		c.Health.Probe.Name += "."
	}

	if c.Fallback.Enabled {
		if len(c.Fallback.Servers) == 0 {
			return fmt.Errorf("fallback.servers must list at least one resolver when fallback is enabled")
		}
		if c.Fallback.Timeout <= 0 {
			return fmt.Errorf("fallback.timeout must be positive")
		}
		if c.Fallback.SummaryInterval <= 0 {
			return fmt.Errorf("fallback.summary_interval must be positive")
		}
		seen := make(map[string]bool, len(c.Fallback.Servers))
		for i, raw := range c.Fallback.Servers {
			addr, err := normaliseAddr(raw)
			if err != nil {
				return fmt.Errorf("fallback.servers[%d]: %w", i, err)
			}
			if seen[addr] {
				return fmt.Errorf("fallback.servers: duplicate resolver %s", addr)
			}
			seen[addr] = true
			c.Fallback.Servers[i] = addr
		}
	}

	if !contains([]string{"debug", "info", "warn", "error"}, c.Log.Level) {
		return fmt.Errorf("log.level: %q is not one of debug, info, warn, error", c.Log.Level)
	}

	if len(c.Upstreams) == 0 {
		return fmt.Errorf("upstreams: at least one upstream must be configured")
	}

	seenName := make(map[string]bool, len(c.Upstreams))
	seenAddr := make(map[string]string, len(c.Upstreams))
	for i := range c.Upstreams {
		u := &c.Upstreams[i]
		if u.Name == "" {
			u.Name = fmt.Sprintf("upstream-%d", i+1)
		}
		if seenName[u.Name] {
			return fmt.Errorf("upstreams: duplicate name %q", u.Name)
		}
		seenName[u.Name] = true

		if u.Weight == 0 {
			u.Weight = 1
		}
		if u.Weight < 0 {
			return fmt.Errorf("upstreams[%s]: weight must not be negative", u.Name)
		}
		if len(u.Endpoints) == 0 {
			return fmt.Errorf("upstreams[%s]: at least one endpoint is required", u.Name)
		}

		for j := range u.Endpoints {
			ep := &u.Endpoints[j]
			addr, err := normaliseAddr(ep.Addr)
			if err != nil {
				return fmt.Errorf("upstreams[%s].endpoints[%d]: %w", u.Name, j, err)
			}
			ep.Addr = addr
			if ep.Name == "" {
				ep.Name = addr
			}
			if owner, dup := seenAddr[addr]; dup {
				return fmt.Errorf("upstreams[%s]: address %s is already used by upstream %q; "+
					"list every path to one Pi-hole under a single upstream so it is not "+
					"weighted twice in selection", u.Name, addr, owner)
			}
			seenAddr[addr] = u.Name
		}
	}
	return nil
}

// normaliseAddr validates an upstream address and supplies the default DNS
// port when one is not given.
func normaliseAddr(addr string) (string, error) {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return "", fmt.Errorf("address must not be empty")
	}

	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		// No port, or an unbracketed IPv6 literal such as "fd00::1".
		if ip := net.ParseIP(addr); ip != nil {
			return net.JoinHostPort(addr, DefaultDNSPort), nil
		}
		if strings.Contains(addr, ":") {
			return "", fmt.Errorf("%q is not a valid address; bracket IPv6 literals as [::1]:53", addr)
		}
		return net.JoinHostPort(addr, DefaultDNSPort), nil
	}
	if host == "" {
		return "", fmt.Errorf("%q has no host part", addr)
	}
	if _, err := strconv.ParseUint(port, 10, 16); err != nil {
		return "", fmt.Errorf("%q has an invalid port", addr)
	}
	return net.JoinHostPort(host, port), nil
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
