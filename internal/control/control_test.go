package control

import (
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chinmay28/hole-balancer/internal/config"
	"github.com/chinmay28/hole-balancer/internal/fallback"
	"github.com/chinmay28/hole-balancer/internal/metrics"
	"github.com/chinmay28/hole-balancer/internal/pool"
	"github.com/chinmay28/hole-balancer/internal/stats"
)

type harness struct {
	mgr      *Manager
	pool     *pool.Pool
	resolver *fallback.Resolver
	stats    *stats.Collector
	path     string
}

func newHarness(t *testing.T, allowWrite bool, persist bool) *harness {
	t.Helper()

	cfg := config.Default()
	cfg.Admin.AllowControl = allowWrite
	cfg.Fallback.Servers = []string{"8.8.8.8"}
	cfg.Upstreams = []config.Upstream{
		{Name: "pihole-1", Weight: 1, Endpoints: []config.Endpoint{{Addr: "10.0.0.1"}}},
		{Name: "pihole-2", Weight: 1, Endpoints: []config.Endpoint{{Addr: "10.0.0.2"}}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("test config is invalid: %v", err)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	p := pool.New(&cfg, nil)
	m := metrics.New()
	resolver := fallback.NewResolver(&cfg, fallback.NewTracker(&cfg, log), m, log)
	collector := stats.New()

	path := ""
	if persist {
		path = filepath.Join(t.TempDir(), "config.yaml")
		if err := cfg.Save(path); err != nil {
			t.Fatal(err)
		}
	}

	return &harness{
		mgr: New(Options{
			Config: &cfg, Path: path, Pool: p, Fallback: resolver,
			Stats: collector, Log: log, AllowWrite: allowWrite,
		}),
		pool: p, resolver: resolver, stats: collector, path: path,
	}
}

// reload reads back what was written, which is the only proof that a change
// will still be there after a restart.
func (h *harness) reload(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := config.Load(h.path)
	if err != nil {
		t.Fatalf("saved config does not reload: %v", err)
	}
	return cfg
}

func TestAddUpstreamAppliesLiveAndPersists(t *testing.T) {
	h := newHarness(t, true, true)

	err := h.mgr.AddUpstream(config.Upstream{
		Name:      "pihole-3",
		Endpoints: []config.Endpoint{{Addr: "10.0.0.3"}, {Addr: "10.0.0.4:5353"}},
	})
	if err != nil {
		t.Fatalf("AddUpstream: %v", err)
	}

	if u := h.pool.Lookup("pihole-3"); u == nil {
		t.Fatal("upstream is not in the running pool")
	} else if len(u.Endpoints) != 2 {
		t.Errorf("endpoints = %d, want 2", len(u.Endpoints))
	}

	saved := h.reload(t)
	if len(saved.Upstreams) != 3 {
		t.Fatalf("saved config has %d upstreams, want 3", len(saved.Upstreams))
	}
	last := saved.Upstreams[2]
	if last.Name != "pihole-3" || last.Weight != 1 {
		t.Errorf("saved upstream = %+v", last)
	}
	if last.Endpoints[0].Addr != "10.0.0.3:53" {
		t.Errorf("addr = %q, want the default port filled in", last.Endpoints[0].Addr)
	}
	if last.Endpoints[1].Addr != "10.0.0.4:5353" {
		t.Errorf("addr = %q, want the explicit port kept", last.Endpoints[1].Addr)
	}
}

func TestAddUpstreamValidation(t *testing.T) {
	h := newHarness(t, true, false)

	cases := map[string]config.Upstream{
		"no name":           {Endpoints: []config.Endpoint{{Addr: "10.0.0.9"}}},
		"no endpoints":      {Name: "x"},
		"bad address":       {Name: "x", Endpoints: []config.Endpoint{{Addr: "not:a:host"}}},
		"negative weight":   {Name: "x", Weight: -2, Endpoints: []config.Endpoint{{Addr: "10.0.0.9"}}},
		"duplicate name":    {Name: "pihole-1", Endpoints: []config.Endpoint{{Addr: "10.0.0.9"}}},
		"address in use":    {Name: "x", Endpoints: []config.Endpoint{{Addr: "10.0.0.2"}}},
		"repeated endpoint": {Name: "x", Endpoints: []config.Endpoint{{Addr: "10.0.0.9"}, {Addr: "10.0.0.9:53"}}},
		"name with slash":   {Name: "a/b", Endpoints: []config.Endpoint{{Addr: "10.0.0.9"}}},
	}
	for name, u := range cases {
		if err := h.mgr.AddUpstream(u); err == nil {
			t.Errorf("%s: AddUpstream succeeded, want an error", name)
		}
	}
	if got := len(h.pool.Upstreams()); got != 2 {
		t.Errorf("pool has %d upstreams; a rejected add must change nothing", got)
	}
}

func TestRemoveUpstream(t *testing.T) {
	h := newHarness(t, true, true)
	h.stats.Record(stats.Record{Upstream: "pihole-2", RCode: "NOERROR"})

	if err := h.mgr.RemoveUpstream("pihole-2"); err != nil {
		t.Fatalf("RemoveUpstream: %v", err)
	}
	if h.pool.Lookup("pihole-2") != nil {
		t.Error("upstream is still in the running pool")
	}
	if saved := h.reload(t); len(saved.Upstreams) != 1 {
		t.Errorf("saved config has %d upstreams, want 1", len(saved.Upstreams))
	}
	for _, n := range h.stats.Snapshot().Nodes {
		if n.Name == "pihole-2" {
			t.Error("a removed Pi-hole should stop appearing in the statistics")
		}
	}

	if err := h.mgr.RemoveUpstream("pihole-2"); !errors.Is(err, pool.ErrNotFound) {
		t.Errorf("removing it twice = %v, want ErrNotFound", err)
	}
}

// An empty pool has nothing to fail over to, so the last one cannot go.
func TestRemoveLastUpstreamIsRefused(t *testing.T) {
	h := newHarness(t, true, true)
	if err := h.mgr.RemoveUpstream("pihole-1"); err != nil {
		t.Fatal(err)
	}
	if err := h.mgr.RemoveUpstream("pihole-2"); !errors.Is(err, pool.ErrLastUpstream) {
		t.Errorf("err = %v, want ErrLastUpstream", err)
	}
	if len(h.pool.Upstreams()) != 1 {
		t.Error("the refused removal still took effect")
	}
}

func TestSetStrategy(t *testing.T) {
	h := newHarness(t, true, true)

	if err := h.mgr.SetStrategy(config.StrategyLeastLatency); err != nil {
		t.Fatalf("SetStrategy: %v", err)
	}
	if got := h.pool.Strategy(); got != config.StrategyLeastLatency {
		t.Errorf("live strategy = %q", got)
	}
	if got := h.reload(t).Strategy; got != config.StrategyLeastLatency {
		t.Errorf("saved strategy = %q", got)
	}

	if err := h.mgr.SetStrategy("sticky"); err == nil {
		t.Error("an unknown strategy should be refused")
	}
	if got := h.pool.Strategy(); got != config.StrategyLeastLatency {
		t.Errorf("a refused change altered the live strategy to %q", got)
	}
}

func TestSetFallback(t *testing.T) {
	h := newHarness(t, true, true)

	if err := h.mgr.SetFallback(true, []string{"1.1.1.1", "9.9.9.9:5353"}); err != nil {
		t.Fatalf("SetFallback: %v", err)
	}
	want := []string{"1.1.1.1:53", "9.9.9.9:5353"}
	got := h.resolver.Servers()
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("live resolvers = %v, want %v", got, want)
	}
	if saved := h.reload(t); len(saved.Fallback.Servers) != 2 || !saved.Fallback.Enabled {
		t.Errorf("saved fallback = %+v", saved.Fallback)
	}

	// Disabling clears the live list, so Enabled has one source of truth.
	if err := h.mgr.SetFallback(false, []string{"1.1.1.1"}); err != nil {
		t.Fatal(err)
	}
	if h.resolver.Enabled() {
		t.Error("resolver should report itself disabled")
	}
	if saved := h.reload(t); saved.Fallback.Enabled {
		t.Error("saved config should record fallback as disabled")
	}
}

func TestSetFallbackValidation(t *testing.T) {
	h := newHarness(t, true, false)
	before := h.resolver.Servers()

	cases := map[string][]string{
		"bad address": {"not:a:host"},
		"duplicate":   {"1.1.1.1", "1.1.1.1:53"},
		"empty list":  {},
	}
	for name, servers := range cases {
		if err := h.mgr.SetFallback(true, servers); err == nil {
			t.Errorf("%s: SetFallback succeeded, want an error", name)
		}
	}
	if got := h.resolver.Servers(); len(got) != len(before) {
		t.Error("a rejected change altered the live resolver list")
	}
}

func TestDrainIsLiveButNotPersisted(t *testing.T) {
	h := newHarness(t, true, true)

	if err := h.mgr.SetDrained("pihole-1", true); err != nil {
		t.Fatalf("SetDrained: %v", err)
	}
	if !h.pool.Lookup("pihole-1").Drained() {
		t.Error("upstream was not drained")
	}
	// Draining is a "while I work on this box" state. Surviving a reboot would
	// leave a Pi-hole quietly out of rotation with nothing to explain why.
	if strings.Contains(readFile(t, h.path), "drained") {
		t.Error("drain state should not be written to the configuration file")
	}

	if err := h.mgr.SetDrained("nope", true); !errors.Is(err, pool.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestReadOnlyRefusesEveryChange(t *testing.T) {
	h := newHarness(t, false, true)

	changes := map[string]error{
		"add":      h.mgr.AddUpstream(config.Upstream{Name: "x", Endpoints: []config.Endpoint{{Addr: "10.0.0.9"}}}),
		"remove":   h.mgr.RemoveUpstream("pihole-1"),
		"drain":    h.mgr.SetDrained("pihole-1", true),
		"strategy": h.mgr.SetStrategy(config.StrategyFailover),
		"fallback": h.mgr.SetFallback(true, []string{"1.1.1.1"}),
	}
	for name, err := range changes {
		if !errors.Is(err, ErrReadOnly) {
			t.Errorf("%s: err = %v, want ErrReadOnly", name, err)
		}
	}
	if h.mgr.CanWrite() {
		t.Error("CanWrite should be false")
	}
	if len(h.pool.Upstreams()) != 2 || h.pool.Lookup("pihole-1").Drained() {
		t.Error("a read-only manager changed the pool")
	}
}

// Without a config file the interface still works; changes just do not outlive
// the process.
func TestWorksWithoutAConfigFile(t *testing.T) {
	h := newHarness(t, true, false)

	if h.mgr.ConfigPath() != "" {
		t.Errorf("ConfigPath = %q, want empty", h.mgr.ConfigPath())
	}
	if err := h.mgr.AddUpstream(config.Upstream{Name: "x", Endpoints: []config.Endpoint{{Addr: "10.0.0.9"}}}); err != nil {
		t.Fatalf("AddUpstream: %v", err)
	}
	if h.pool.Lookup("x") == nil {
		t.Error("the change did not apply")
	}
}

// A save that fails must not leave the running server disagreeing with disk.
func TestFailedSaveRollsTheChangeBack(t *testing.T) {
	h := newHarness(t, true, true)

	// Point the manager at a directory that cannot be written to.
	dir := filepath.Join(t.TempDir(), "readonly")
	if err := os.Mkdir(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	h.mgr.path = filepath.Join(dir, "config.yaml")

	err := h.mgr.AddUpstream(config.Upstream{Name: "doomed", Endpoints: []config.Endpoint{{Addr: "10.0.0.9"}}})
	if err == nil {
		t.Skip("running as a user that can write to a read-only directory")
	}
	if h.pool.Lookup("doomed") != nil {
		t.Error("the upstream stayed in the pool after the save failed")
	}
	if got := len(h.mgr.Snapshot().Upstreams); got != 2 {
		t.Errorf("config has %d upstreams after a failed save, want 2", got)
	}
}

func TestSnapshotIsACopy(t *testing.T) {
	h := newHarness(t, true, false)

	snap := h.mgr.Snapshot()
	snap.Upstreams[0].Name = "mutated"
	snap.Fallback.Servers[0] = "6.6.6.6"

	fresh := h.mgr.Snapshot()
	if fresh.Upstreams[0].Name == "mutated" || fresh.Fallback.Servers[0] == "6.6.6.6" {
		t.Error("Snapshot handed out a reference to the live configuration")
	}
}

func TestBackupIsKept(t *testing.T) {
	h := newHarness(t, true, true)
	if err := h.mgr.SetStrategy(config.StrategyFailover); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(h.path + ".bak"); err != nil {
		t.Errorf("no backup written alongside the config: %v", err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
