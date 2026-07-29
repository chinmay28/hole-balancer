// Command hole-balancer forwards DNS queries to a pool of Pi-hole servers,
// spreading load across the ones that are answering and routing around the
// ones that are not.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/chinmay28/hole-balancer/internal/admin"
	"github.com/chinmay28/hole-balancer/internal/config"
	"github.com/chinmay28/hole-balancer/internal/health"
	"github.com/chinmay28/hole-balancer/internal/metrics"
	"github.com/chinmay28/hole-balancer/internal/pool"
	"github.com/chinmay28/hole-balancer/internal/proxy"
)

// version is overridden at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "hole-balancer:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		configPath  = flag.String("config", "/etc/hole-balancer/config.yaml", "path to the configuration file")
		validate    = flag.Bool("validate", false, "check the configuration and exit")
		showVersion = flag.Bool("version", false, "print the version and exit")
		healthcheck = flag.Bool("healthcheck", false, "query the admin health endpoint and exit non-zero if it is unhealthy")
		logFormat   = flag.String("log-format", "text", "log output format: text or json")
	)
	flag.Parse()

	if *showVersion {
		fmt.Println(buildVersion())
		return nil
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	if *healthcheck {
		return runHealthcheck(cfg.Admin.Listen)
	}
	if *validate {
		fmt.Printf("%s: ok (%d upstreams, strategy %s)\n", *configPath, len(cfg.Upstreams), cfg.Strategy)
		return nil
	}

	log, err := newLogger(cfg.Log.Level, *logFormat)
	if err != nil {
		return err
	}
	log.Info("starting hole-balancer",
		"version", buildVersion(),
		"strategy", cfg.Strategy,
		"upstreams", len(cfg.Upstreams),
		"config", *configPath)

	m := metrics.New()
	p := pool.New(cfg, func(e *pool.Endpoint, up bool, reason string) {
		state := "down"
		level := slog.LevelWarn
		if up {
			state, level = "up", slog.LevelInfo
		}
		log.Log(context.Background(), level, "endpoint state changed",
			"upstream", e.Upstream.Name, "endpoint", e.Addr, "state", state, "reason", reason)
		m.StateFlips.Inc(
			metrics.Label{Name: "upstream", Value: e.Upstream.Name},
			metrics.Label{Name: "endpoint", Value: e.Name},
			metrics.Label{Name: "state", Value: state},
		)
	})

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Learn the state of the pool before opening the listeners, so the first
	// client query is routed on fact rather than on an assumption.
	checker := health.New(cfg, p, m, log)
	checker.Bootstrap(ctx)
	if p.HealthyUpstreams() == 0 {
		log.Warn("no upstream answered the initial probe; serving anyway and will retry every interval",
			"interval", cfg.Health.Interval.String())
	}

	var (
		wg     sync.WaitGroup
		errMu  sync.Mutex
		errRet error
	)
	fail := func(err error) {
		if err == nil {
			return
		}
		errMu.Lock()
		if errRet == nil {
			errRet = err
		}
		errMu.Unlock()
		stop() // one failed component brings the process down for the supervisor
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		checker.Run(ctx)
	}()

	adminSrv := admin.New(cfg, p, m, log, buildVersion())
	wg.Add(1)
	go func() {
		defer wg.Done()
		fail(adminSrv.ListenAndServe(ctx))
	}()

	dnsSrv := proxy.New(cfg, p, m, log)
	wg.Add(1)
	go func() {
		defer wg.Done()
		fail(dnsSrv.ListenAndServe(ctx))
	}()

	<-ctx.Done()
	log.Info("shutting down")
	wg.Wait()

	if errRet != nil && isPermissionError(errRet) {
		return fmt.Errorf("%w\n\nbinding port 53 needs privileges. Either run as root, grant the "+
			"capability with `setcap cap_net_bind_service=+ep /usr/local/bin/hole-balancer`, or "+
			"set listen.udp/listen.tcp to a high port", errRet)
	}
	return errRet
}

// runHealthcheck asks the running instance's admin interface whether it can
// still reach a Pi-hole. It exists so that container runtimes can probe an
// image that ships without a shell or an HTTP client.
func runHealthcheck(adminAddr string) error {
	if adminAddr == "" {
		return errors.New("-healthcheck needs admin.listen to be set in the configuration")
	}
	host, port, err := net.SplitHostPort(adminAddr)
	if err != nil {
		return fmt.Errorf("admin.listen %q: %w", adminAddr, err)
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get("http://" + net.JoinHostPort(host, port) + "/healthz")
	if err != nil {
		return fmt.Errorf("health check: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("health check returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	fmt.Print(string(body))
	return nil
}

func newLogger(level, format string) (*slog.Logger, error) {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "info":
		lvl = slog.LevelInfo
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		return nil, fmt.Errorf("unknown log level %q", level)
	}

	opts := &slog.HandlerOptions{Level: lvl}
	switch format {
	case "json":
		return slog.New(slog.NewJSONHandler(os.Stderr, opts)), nil
	case "text":
		return slog.New(slog.NewTextHandler(os.Stderr, opts)), nil
	default:
		return nil, fmt.Errorf("unknown log format %q: want text or json", format)
	}
}

func isPermissionError(err error) bool {
	return errors.Is(err, os.ErrPermission) || errors.Is(err, syscall.EACCES) || errors.Is(err, syscall.EPERM)
}

// buildVersion prefers the linker-supplied version and falls back to the VCS
// stamp the Go toolchain embeds for `go install` builds.
func buildVersion() string {
	if version != "dev" {
		return version
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return version
	}
	var rev, dirty string
	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			if len(setting.Value) > 12 {
				rev = setting.Value[:12]
			} else {
				rev = setting.Value
			}
		case "vcs.modified":
			if setting.Value == "true" {
				dirty = "-dirty"
			}
		}
	}
	if rev == "" {
		return version
	}
	return version + "+" + rev + dirty
}
