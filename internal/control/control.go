// Package control applies configuration changes made through the management
// interface to the running balancer, and writes them back to disk.
//
// Every change follows the same three steps: validate against exactly the rules
// the config file obeys, apply to the live components, then persist. Applying
// before persisting means a failed write leaves a running server that disagrees
// with its file — noisy, but the alternative is a save that succeeds while the
// change silently did not take, which is worse. A failed save is reported to the
// caller and logged loudly.
package control

import (
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/chinmay28/hole-balancer/internal/config"
	"github.com/chinmay28/hole-balancer/internal/fallback"
	"github.com/chinmay28/hole-balancer/internal/pool"
	"github.com/chinmay28/hole-balancer/internal/stats"
)

// ErrReadOnly is returned when a change is attempted while the management
// interface is in read-only mode.
var ErrReadOnly = errors.New("control: management interface is read-only (set admin.allow_control: true)")

// Manager owns the authoritative configuration and pushes changes into the
// live pool, resolver, and config file.
type Manager struct {
	log      *slog.Logger
	pool     *pool.Pool
	fallback *fallback.Resolver
	stats    *stats.Collector

	mu         sync.Mutex
	cfg        *config.Config
	path       string
	allowWrite bool
	// persist is false when there is no file to write to, which is the case in
	// tests and when the balancer was started without a config path.
	persist bool
}

// Options configures a Manager.
type Options struct {
	Config   *config.Config
	Path     string
	Pool     *pool.Pool
	Fallback *fallback.Resolver
	Stats    *stats.Collector
	Log      *slog.Logger
	// AllowWrite mirrors admin.allow_control. With it false the Manager still
	// answers questions but refuses every change.
	AllowWrite bool
}

// New creates a Manager over a private copy of the configuration, so nothing
// else in the process observes a half-applied edit.
func New(opts Options) *Manager {
	return &Manager{
		log:        opts.Log,
		pool:       opts.Pool,
		fallback:   opts.Fallback,
		stats:      opts.Stats,
		cfg:        opts.Config.Clone(),
		path:       opts.Path,
		allowWrite: opts.AllowWrite,
		persist:    opts.Path != "",
	}
}

// CanWrite reports whether changes are permitted.
func (m *Manager) CanWrite() bool { return m != nil && m.allowWrite }

// CanWriteErr is CanWrite in the form a handler can return directly.
func (m *Manager) CanWriteErr() error { return m.checkWritable() }

// ConfigPath is the file changes are written to, empty if none.
func (m *Manager) ConfigPath() string {
	if m == nil || !m.persist {
		return ""
	}
	return m.path
}

// Snapshot returns a copy of the current configuration.
func (m *Manager) Snapshot() *config.Config {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cfg.Clone()
}

func (m *Manager) checkWritable() error {
	if !m.allowWrite {
		return ErrReadOnly
	}
	return nil
}

// AddUpstream brings a new Pi-hole into rotation.
func (m *Manager) AddUpstream(u config.Upstream) error {
	if err := m.checkWritable(); err != nil {
		return err
	}

	clean, err := config.PrepareUpstream(u)
	if err != nil {
		return fmt.Errorf("invalid upstream: %w", err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if _, err := m.pool.Add(clean); err != nil {
		return err
	}
	m.cfg.Upstreams = append(m.cfg.Upstreams, clean)

	m.log.Info("upstream added", "upstream", clean.Name,
		"endpoints", len(clean.Endpoints), "weight", clean.Weight)
	return m.saveLocked(func() {
		// Undo the live change if the file could not be written, so the running
		// server and the file on disk stay in agreement.
		_ = m.pool.Remove(clean.Name)
		m.cfg.Upstreams = m.cfg.Upstreams[:len(m.cfg.Upstreams)-1]
	})
}

// RemoveUpstream takes a Pi-hole out of the pool.
func (m *Manager) RemoveUpstream(name string) error {
	if err := m.checkWritable(); err != nil {
		return err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	idx := -1
	for i, u := range m.cfg.Upstreams {
		if u.Name == name {
			idx = i
			break
		}
	}
	if idx < 0 {
		return fmt.Errorf("%w: %s", pool.ErrNotFound, name)
	}

	if err := m.pool.Remove(name); err != nil {
		return err
	}
	removed := m.cfg.Upstreams[idx]
	m.cfg.Upstreams = append(m.cfg.Upstreams[:idx:idx], m.cfg.Upstreams[idx+1:]...)

	m.log.Info("upstream removed", "upstream", name)
	if err := m.saveLocked(func() {
		if _, addErr := m.pool.Add(removed); addErr != nil {
			m.log.Error("could not restore upstream after a failed save",
				"upstream", name, "error", addErr)
		}
		m.cfg.Upstreams = append(m.cfg.Upstreams, removed)
	}); err != nil {
		return err
	}

	// Only once the removal is durable do its statistics stop being interesting.
	if m.stats != nil {
		m.stats.ForgetUpstream(name)
	}
	return nil
}

// SetDrained takes an upstream out of, or back into, rotation without removing
// it. Draining is deliberately not persisted: it is a "right now, while I work
// on this box" state, and a Pi-hole that is still drained after an unrelated
// reboot is a trap.
func (m *Manager) SetDrained(name string, drained bool) error {
	if err := m.checkWritable(); err != nil {
		return err
	}
	u := m.pool.Lookup(name)
	if u == nil {
		return fmt.Errorf("%w: %s", pool.ErrNotFound, name)
	}
	u.SetDrained(drained)
	m.log.Info("upstream drain state changed", "upstream", name, "drained", drained)
	return nil
}

// SetStrategy changes how upstreams are selected.
func (m *Manager) SetStrategy(name string) error {
	if err := m.checkWritable(); err != nil {
		return err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	previous := m.cfg.Strategy
	if err := m.pool.SetStrategy(name); err != nil {
		return err
	}
	m.cfg.Strategy = name

	m.log.Info("selection strategy changed", "from", previous, "to", name)
	return m.saveLocked(func() {
		_ = m.pool.SetStrategy(previous)
		m.cfg.Strategy = previous
	})
}

// SetFallback replaces the public-resolver configuration.
func (m *Manager) SetFallback(enabled bool, servers []string) error {
	if err := m.checkWritable(); err != nil {
		return err
	}

	clean := make([]string, 0, len(servers))
	seen := make(map[string]bool, len(servers))
	for _, raw := range servers {
		addr, err := config.NormaliseAddr(raw)
		if err != nil {
			return fmt.Errorf("invalid resolver %q: %w", raw, err)
		}
		if seen[addr] {
			return fmt.Errorf("resolver %s is listed twice", addr)
		}
		seen[addr] = true
		clean = append(clean, addr)
	}
	if enabled && len(clean) == 0 {
		return errors.New("at least one resolver is required to enable fallback")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	prevEnabled, prevServers := m.cfg.Fallback.Enabled, m.cfg.Fallback.Servers
	m.fallback.SetServers(enabled, clean)
	m.cfg.Fallback.Enabled = enabled
	m.cfg.Fallback.Servers = clean

	m.log.Info("fallback configuration changed", "enabled", enabled, "resolvers", len(clean))
	return m.saveLocked(func() {
		m.fallback.SetServers(prevEnabled, prevServers)
		m.cfg.Fallback.Enabled = prevEnabled
		m.cfg.Fallback.Servers = prevServers
	})
}

// saveLocked persists the configuration, calling rollback and returning the
// error if the write fails. The caller holds m.mu.
func (m *Manager) saveLocked(rollback func()) error {
	if !m.persist {
		return nil
	}
	if err := m.cfg.Save(m.path); err != nil {
		m.log.Error("could not save configuration; rolling the change back",
			"path", m.path, "error", err)
		if rollback != nil {
			rollback()
		}
		return fmt.Errorf("saving %s: %w", m.path, err)
	}
	m.log.Info("configuration saved", "path", m.path)
	return nil
}
