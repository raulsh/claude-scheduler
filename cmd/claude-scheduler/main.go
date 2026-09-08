// Command claude-scheduler runs scheduled Claude Code tasks behind a local
// web UI, checking each task's dependencies before it fires.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"github.com/raulsh/claude-scheduler/internal/api"
	"github.com/raulsh/claude-scheduler/internal/config"
	"github.com/raulsh/claude-scheduler/internal/executor"
	"github.com/raulsh/claude-scheduler/internal/health"
	"github.com/raulsh/claude-scheduler/internal/notify"
	"github.com/raulsh/claude-scheduler/internal/scheduler"
	"github.com/raulsh/claude-scheduler/internal/sdnotify"
	"github.com/raulsh/claude-scheduler/internal/store"
)

// Build metadata, set via -ldflags.
var (
	version = "dev"
	commit  = "none"
)

const defaultConfigPath = "/etc/claude-scheduler/config.yaml"

func main() {
	api.Version = version

	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	var err error
	switch os.Args[1] {
	case "serve":
		err = runServe(os.Args[2:])
	case "version", "--version", "-v":
		fmt.Printf("claude-scheduler %s (commit %s, %s)\n", version, commit, runtime.Version())
	case "help", "--help", "-h":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}

	if err != nil {
		fmt.Fprintln(os.Stderr, "error: "+err.Error())
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `claude-scheduler - scheduled Claude Code tasks with dependency healthchecks

Usage:
  claude-scheduler serve [flags]   Run the scheduler and web UI
  claude-scheduler version         Print version information
  claude-scheduler help            Show this message

Serve flags:
  --config <path>   Configuration file (default `+defaultConfigPath+`)
  --bind <addr>     Override the listen address
  --port <n>        Override the listen port
  --log-level <l>   debug, info, warn or error (default info)
`)
}

func runServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	configPath := fs.String("config", defaultConfigPath, "configuration file path")
	bind := fs.String("bind", "", "override the listen address")
	port := fs.Int("port", 0, "override the listen port")
	logLevel := fs.String("log-level", "info", "log level: debug, info, warn or error")
	if err := fs.Parse(args); err != nil {
		return err
	}

	log := newLogger(*logLevel)

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	// Flags win over both the file and the environment.
	if *bind != "" {
		cfg.Server.Bind = *bind
	}
	if *port != 0 {
		cfg.Server.Port = *port
	}
	if err := cfg.Validate(); err != nil {
		return err
	}

	log.Info("starting claude-scheduler",
		"version", version, "config", *configPath, "addr", cfg.Server.Addr())

	if err := ensureDirs(cfg); err != nil {
		return err
	}

	st, err := store.Open(cfg.Paths.DBPath())
	if err != nil {
		return err
	}
	defer st.Close()

	// Executions that were in flight when the service stopped would otherwise
	// appear to run forever.
	if n, err := st.ReapOrphanedExecutions(context.Background()); err != nil {
		log.Warn("could not reap orphaned executions", "error", err)
	} else if n > 0 {
		log.Info("reaped executions interrupted by a previous shutdown", "count", n)
	}

	warnAboutEnvironment(cfg, log)

	// The run context bounds background executions so shutdown stops them.
	runCtx, stopRuns := context.WithCancel(context.Background())
	defer stopRuns()

	broker := executor.NewBroker()
	exe := executor.New(runCtx, cfg, st, broker, log)

	registry, logins := buildHealth(cfg, st, log)
	// Dependencies are checked before every run, so a task never executes
	// against an expired credential.
	exe.SetPreflight(registry.Preflight())

	desktop := notify.NewDesktop(cfg.Notify.Desktop.Enabled)
	defer desktop.Close()
	notifier := notify.NewFanout(st, log,
		desktop,
		notify.NewSlack(cfg.Notify.Slack.Enabled, cfg.Notify.Slack.WebhookURL),
	)
	log.Info("notification channels enabled", "channels", notifier.Channels())

	exe.SetNotifier(func(task *store.Task, exec *store.Execution) {
		if ev, worth := notify.ForExecution(task, exec); worth {
			notifier.Notify(context.Background(), ev)
		}
	})
	exe.SetPauseNotifier(func(task *store.Task, reason string) {
		notifier.Notify(context.Background(), notify.ForAutoPause(task, reason))
	})

	sched := scheduler.New(cfg, st, exe, log)
	if err := sched.Start(runCtx); err != nil {
		return err
	}
	defer sched.Stop()

	srv := &http.Server{
		Addr: cfg.Server.Addr(),
		Handler: api.New(cfg, st, log, api.Deps{
			Executor: exe,
			Health:   registry,
			Logins:   logins,
			Reloader: sched,
		}).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// No WriteTimeout: SSE responses are intentionally long-lived.
		IdleTimeout: 120 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errs := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errs <- err
			return
		}
		errs <- nil
	}()

	if err := sdnotify.Ready(); err != nil {
		log.Warn("could not notify systemd of readiness", "error", err)
	}
	_ = sdnotify.Status(fmt.Sprintf("listening on %s, %d task(s) scheduled",
		cfg.Server.Addr(), sched.Count()))
	log.Info("listening", "url", "http://"+cfg.Server.Addr())

	select {
	case err := <-errs:
		if err != nil {
			return fmt.Errorf("http server: %w", err)
		}
		return nil
	case <-ctx.Done():
		log.Info("shutting down")
		_ = sdnotify.Stopping()
		sched.Stop()
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("graceful shutdown: %w", err)
	}

	// Stop in-flight executions and give them a moment to record an outcome,
	// so they are reported as cancelled rather than reaped on next start.
	if running := exe.Running(); len(running) > 0 {
		log.Info("stopping in-flight executions", "count", len(running))
		stopRuns()
		waitForRuns(exe, 10*time.Second, log)
	}

	log.Info("stopped")
	return nil
}

func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(level)); err != nil {
		lvl = slog.LevelInfo
	}
	// Text output: these logs are read through journalctl, which already
	// supplies its own structure and timestamps.
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
}

func ensureDirs(cfg config.Config) error {
	for _, dir := range []string{
		cfg.Paths.DataDir,
		cfg.Paths.LogDir,
		cfg.Paths.TranscriptDir(),
		filepath.Dir(cfg.Paths.DBPath()),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
	}
	return nil
}

// warnAboutEnvironment surfaces conditions that leave the service running but
// unable to do its job, so they appear in the journal at startup rather than
// as a mystery failure on the first scheduled run.
func warnAboutEnvironment(cfg config.Config, log *slog.Logger) {
	if cfg.Binaries.Claude == "" {
		log.Warn("the claude CLI was not found; no task can run until binaries.claude is set",
			"config", defaultConfigPath)
	} else {
		log.Info("claude CLI located", "path", cfg.Binaries.Claude)
	}
	if cfg.Binaries.AWS == "" {
		log.Warn("the aws CLI was not found; AWS profile checks will report unavailable")
	}
	if os.Getenv("HOME") == "" {
		log.Warn("HOME is unset; the claude and aws CLIs will not find their credentials")
	}
	if !cfg.Server.IsLoopback() {
		log.Warn("listening beyond loopback: the API can trigger code execution as this user",
			"bind", cfg.Server.Bind, "auth", cfg.Server.Token != "")
	}
}

// waitForRuns gives cancelled executions a chance to persist their outcome.
func waitForRuns(exe *executor.Executor, timeout time.Duration, log *slog.Logger) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if len(exe.Running()) == 0 {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	log.Warn("executions still running at shutdown; they will be reaped on next start",
		"count", len(exe.Running()))
}

// buildHealth assembles the dependency checkers this service knows about.
func buildHealth(cfg config.Config, st *store.Store, log *slog.Logger) (*health.Registry, *health.LoginManager) {
	awsChecker := &health.AWSChecker{
		Binary:   cfg.Binaries.AWS,
		Timeout:  cfg.Health.CheckTimeout.Std(),
		CacheFor: cfg.Health.AWSTTL.Std(),
	}
	mcpChecker := &health.MCPChecker{
		Binary:   cfg.Binaries.Claude,
		Timeout:  cfg.Health.CheckTimeout.Std(),
		CacheFor: cfg.Health.MCPTTL.Std(),
	}
	binaryChecker := &health.BinaryChecker{
		Timeout:  cfg.Health.CheckTimeout.Std(),
		CacheFor: cfg.Health.BinaryTTL.Std(),
		Probes: map[string]health.BinaryProbe{
			"aws":    {Path: cfg.Binaries.AWS},
			"claude": {Path: cfg.Binaries.Claude},
			"git":    {},
		},
	}

	registry := health.NewRegistry(st, log, awsChecker, mcpChecker, binaryChecker)
	return registry, health.NewLoginManager(awsChecker, registry, log)
}
