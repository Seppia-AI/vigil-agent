// Command vigil-agent is the metrics-shipping daemon for Seppia Vigil Vitals.
//
// It scrapes a fixed set of host metrics (cpu / mem / swap / disk / net /
// uptime) and POSTs them as JSON to /vigil/vitals/<token> on the
// configured ingest URL.
//
// CLI shape:
//
//	vigil-agent                 # run the daemon (collect → buffer → ship)
//	vigil-agent run             # explicit alias of the above
//	vigil-agent --version       # print build info
//	vigil-agent --check-config  # validate config and exit
//	vigil-agent --config PATH   # use a non-default config file
//	vigil-agent --once          # scrape once, print JSON batch, exit
//	vigil-agent --dry-run       # daemon, but log batches instead of POSTing
//	vigil-agent --insecure      # DEV ONLY: skip TLS verification
//	vigil-agent --log-format=text|json  # default text
//	vigil-agent --log-level=debug|info|warn|error  # default info
//	vigil-agent --metrics-addr=127.0.0.1:9090  # opt-in /metrics endpoint
//	vigil-agent --drain-timeout=5s  # bound the SIGTERM drain window
//	vigil-agent --no-update-check   # suppress the once-a-day "newer release?" check
//
// Exit codes (stable; consumed by systemd unit + install script,
// canonical source: internal/exitcode):
//
//	0   OK        normal exit — SIGTERM after clean drain, --once / --check-config success, --version
//	1   Config    config error or fatal sink (missing token, bad URL, token revoked → 404)
//	2   Runtime   unexpected scheduler / runtime error (should not happen under normal operation)
//	3   Usage     bad CLI flag or unknown positional
//
// systemd should set `RestartPreventExitStatus=1 3` so the unit only
// auto-retries transient Runtime errors; Config/Usage errors require
// operator action and looping on them just spams the journal.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Seppia-AI/vigil-agent/internal/collector"
	"github.com/Seppia-AI/vigil-agent/internal/config"
	"github.com/Seppia-AI/vigil-agent/internal/exitcode"
	"github.com/Seppia-AI/vigil-agent/internal/ingest"
	"github.com/Seppia-AI/vigil-agent/internal/observ"
	"github.com/Seppia-AI/vigil-agent/internal/scheduler"
	"github.com/Seppia-AI/vigil-agent/internal/updater"
	"github.com/Seppia-AI/vigil-agent/internal/version"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run is split out from main so it's testable: pass args + writers,
// get back the exit code. main() just wires it to the real os/* and
// exits.
func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("vigil-agent", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var (
		showVersion   = fs.Bool("version", false, "print version information and exit")
		checkConfig   = fs.Bool("check-config", false, "load and validate config, then exit")
		once          = fs.Bool("once", false, "run the collectors once, print the JSON batch to stdout, exit")
		dryRun        = fs.Bool("dry-run", false, "run the daemon but log batches to stderr instead of POSTing them")
		insecure      = fs.Bool("insecure", false, "DEV ONLY: skip TLS certificate verification when POSTing to the ingest URL")
		configPath    = fs.String("config", "", "path to YAML config file (default: "+config.DefaultConfigPath+")")
		logFormat     = fs.String("log-format", "text", "log format: text|json")
		logLevel      = fs.String("log-level", "info", "log level: debug|info|warn|error")
		metricsAddr   = fs.String("metrics-addr", "", "expose /metrics on this addr (e.g. 127.0.0.1:9090); empty = disabled")
		drainTimeout  = fs.Duration("drain-timeout", scheduler.DefaultDrainTimeout, "max time to flush buffered batches after SIGTERM before exit (0 = drop immediately)")
		noUpdateCheck = fs.Bool("no-update-check", false, "disable the once-a-day check for newer agent releases (override of disable_update_check in config)")
	)

	fs.Usage = func() {
		fmt.Fprintf(stderr, "%s\n\n", version.String())
		fmt.Fprintf(stderr, "Usage: vigil-agent [flags] [run]\n\n")
		fmt.Fprintf(stderr, "Subcommands:\n")
		fmt.Fprintf(stderr, "  run    explicit alias for the bare daemon invocation (default)\n\n")
		fmt.Fprintf(stderr, "Flags:\n")
		fs.PrintDefaults()
		fmt.Fprintf(stderr, "\nExit codes: 0=OK, 1=Config, 2=Runtime, 3=Usage.\n")
		fmt.Fprintf(stderr, "Documentation: https://github.com/Seppia-AI/vigil-agent\n")
	}

	if err := fs.Parse(args); err != nil {
		// flag.ContinueOnError already printed the usage to stderr.
		// Translate to our usage exit code so systemd can distinguish
		// "you passed me a bad flag" from "config is wrong".
		return exitcode.Usage
	}

	// Subcommand parsing is intentionally minimal: the only positional
	// we accept is the literal `run` (a documentation hook so a
	// systemd ExecStart line can read `… vigil-agent run` without
	// surprising the operator). Anything else is a typo we should
	// reject loudly rather than silently treat as the daemon.
	if positional := fs.Args(); len(positional) > 0 {
		if len(positional) != 1 || positional[0] != "run" {
			fmt.Fprintf(stderr, "unexpected argument(s): %v\n\n", positional)
			fs.Usage()
			return exitcode.Usage
		}
	}

	if *showVersion {
		fmt.Fprintln(stdout, version.String())
		return exitcode.OK
	}

	// Build the structured logger up front so even sub-commands that
	// do their own thing (--check-config, --once) get the same format
	// guarantees. ParseLog* turn typos into a startup error rather
	// than silently mis-formatting an entire run.
	format, err := observ.ParseLogFormat(*logFormat)
	if err != nil {
		fmt.Fprintf(stderr, "flag --log-format: %v\n", err)
		return exitcode.Usage
	}
	level, err := observ.ParseLogLevel(*logLevel)
	if err != nil {
		fmt.Fprintf(stderr, "flag --log-level: %v\n", err)
		return exitcode.Usage
	}
	logger := observ.NewLogger(stderr, format, level, version.Version)

	if *checkConfig {
		return runCheckConfig(*configPath, stdout, stderr)
	}

	if *once {
		return runOnce(*configPath, stdout, stderr)
	}

	return runDaemon(*configPath, *dryRun, *insecure, *metricsAddr, *drainTimeout, *noUpdateCheck, logger, stderr)
}

// runDaemon is the body of `vigil-agent` (no flags). It loads config,
// builds the collector registry + scheduler, wires SIGINT/SIGTERM to
// graceful shutdown, optionally exposes /metrics, and blocks until a
// signal arrives.
//
// `--dry-run` swaps the HTTPSink for the LogSink — useful for testing
// config and verifying labels on a host that doesn't have network
// access to the ingest server (e.g. a CI runner, air-gapped test box).
//
// `drainTimeout` is the bounded post-SIGTERM flush window. See
// scheduler.DefaultDrainTimeout for sizing rationale; 0/-1 disables
// drain entirely (immediate exit, in-flight batches lost).
func runDaemon(path string, dryRun, insecure bool, metricsAddr string, drainTimeout time.Duration, noUpdateCheck bool, logger *slog.Logger, stderr io.Writer) int {
	cfg, src, err := config.Load(path)
	if err != nil {
		fmt.Fprintf(stderr, "config error: %v\n", err)
		return exitcode.Config
	}

	// Banner stays on stderr (NOT through the structured logger)
	// because it's a multi-line human-friendly block; logfmt/JSON
	// would mangle it.
	fmt.Fprintln(stderr, version.String())
	printSource(stderr, src, cfg)

	hi := collector.CollectHostInfo()
	hi.AgentVersion = version.Version

	registry := collector.NewRegistry(collector.All(), hi, cfg.Labels, cfg.MetricsAllowlist)

	// Pick a sink. Default (no flags) is the HTTPSink against the
	// configured ingest URL; --dry-run forces the LogSink for air-
	// gapped test hosts, CI runners, and label-debugging sessions.
	var sink scheduler.Sink
	if dryRun {
		logger.Info("dry-run: batches will be logged, not POSTed",
			slog.String("event", "daemon.dry_run"))
		sink = scheduler.NewLogSink(stderr)
	} else {
		if insecure {
			// Logged loudly because letting this slip into
			// production bricks the transport security of the
			// token — not a subtle footgun.
			logger.Warn("--insecure: TLS certificate verification DISABLED — use only in dev",
				slog.String("event", "daemon.insecure"))
		}
		httpSink, err := ingest.New(ingest.Options{
			IngestURL:    cfg.IngestURL,
			Token:        cfg.Token,
			Insecure:     insecure,
			Logger:       logger,
			AgentVersion: version.Version,
		})
		if err != nil {
			fmt.Fprintf(stderr, "ingest sink: %v\n", err)
			return exitcode.Config
		}
		sink = httpSink
	}

	sched := scheduler.New(scheduler.Options{
		Interval:     time.Duration(cfg.ScrapeIntervalS) * time.Second,
		Registry:     registry,
		Sink:         sink,
		Logger:       logger,
		DrainTimeout: drainTimeout,
	})

	// Optional /metrics endpoint. Empty addr = disabled (the default
	// for production VMs that don't run a local Prometheus). Operators
	// who do can pass --metrics-addr=127.0.0.1:9090. Bind failure is
	// fatal — silently losing observability is worse than refusing to
	// start.
	var metricsSrv *observ.MetricsServer
	if metricsAddr != "" {
		metricsSrv, err = observ.StartMetricsServer(metricsAddr,
			schedStatsAdapter{sched}, version.Version, logger)
		if err != nil {
			fmt.Fprintf(stderr, "metrics endpoint: %v\n", err)
			return exitcode.Config
		}
		defer metricsSrv.Stop()
	}

	// Wire SIGINT/SIGTERM to context cancellation so cancelling either
	// signal stops the scheduler cleanly. signal.NotifyContext gives us
	// the standard "first signal cancels, second forces exit" semantics
	// for free — the runtime kills us if a second signal arrives while
	// we're still draining.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Optional once-a-day update check. Best-effort, never fatal: if
	// updater.New rejects the configured URL we log it once and carry
	// on without an updater rather than refuse to start the daemon —
	// shipping metrics is more important than checking for upgrades.
	//
	// The CLI flag wins over the config field: --no-update-check forces
	// it off even if the file says enabled. There is intentionally no
	// override in the other direction (a config that disables it cannot
	// be re-enabled via flag) — easier to reason about for ops.
	updateCheckDisabled := cfg.DisableUpdateCheck || noUpdateCheck
	if !updateCheckDisabled {
		up, uerr := updater.New(updater.Options{
			CurrentVersion: version.Version,
			URL:            cfg.ResolvedUpdateCheckURL(),
			Logger:         logger,
		})
		if uerr != nil {
			logger.Warn("update check disabled: bad config",
				slog.String("event", "update.disabled"),
				slog.String("err", uerr.Error()),
			)
		} else {
			go func() {
				// Run only returns nil; ignore for symmetry.
				_ = up.Run(ctx)
			}()
		}
	} else {
		logger.Debug("update check disabled by config / flag",
			slog.String("event", "update.disabled"),
		)
	}

	runErr := sched.Run(ctx)

	// On exit, log a structured summary of what happened during the
	// run. JSON/logfmt-friendly so it can be ingested by the same
	// pipeline as the in-flight events.
	st := sched.Stats()
	logger.Info("daemon summary",
		slog.String("event", "daemon.summary"),
		slog.Uint64("scrapes_ok", st.ScrapesOK),
		slog.Uint64("scrapes_partial", st.ScrapesPartial),
		slog.Uint64("scrapes_empty", st.ScrapesEmpty),
		slog.Uint64("samples_collected", st.SamplesCollected),
		slog.Uint64("batches_sent", st.BatchesSent),
		slog.Uint64("batches_failed", st.BatchesFailed),
		slog.Uint64("dropped_overflow", st.DroppedOverflow),
		slog.Uint64("dropped_quota_samples", st.DroppedQuotaSamples),
	)

	if runErr != nil {
		// ErrFatal from the sink (e.g. 404 = token revoked) maps to
		// exitcode.Config so systemd flags it red AND the operator
		// knows it's a config-level problem, not a transient one.
		// Anything else is Runtime.
		if errors.Is(runErr, scheduler.ErrFatal) {
			fmt.Fprintf(stderr, "fatal: %v\n", runErr)
			return exitcode.Config
		}
		fmt.Fprintf(stderr, "scheduler error: %v\n", runErr)
		return exitcode.Runtime
	}
	return exitcode.OK
}

// schedStatsAdapter bridges scheduler.Stats (the agent's internal
// counter struct) to observ.StatsSnapshot (the Prom-handler-friendly
// shape). Defined here, not in scheduler, so scheduler doesn't take
// an import on observ — keeping the dependency arrow strictly
// observ → scheduler-callers, never the reverse.
type schedStatsAdapter struct{ s *scheduler.Scheduler }

func (a schedStatsAdapter) StatsSnapshot() observ.StatsSnapshot {
	st := a.s.Stats()
	return observ.StatsSnapshot{
		ScrapesOK:           st.ScrapesOK,
		ScrapesPartial:      st.ScrapesPartial,
		ScrapesEmpty:        st.ScrapesEmpty,
		SamplesCollected:    st.SamplesCollected,
		BatchesSent:         st.BatchesSent,
		BatchesFailed:       st.BatchesFailed,
		DroppedOverflow:     st.DroppedOverflow,
		DroppedQuotaSamples: st.DroppedQuotaSamples,
		LastFlushUnix:       st.LastFlushUnix,
	}
}

// runOnce is the body of `vigil-agent --once`. It loads config (so static
// labels and the metrics_allowlist are honoured), runs every collector
// exactly once, and prints the resulting Batch as pretty-printed JSON to
// stdout — i.e. EXACTLY the bytes the daemon would POST, minus HTTP
// framing.
//
// We deliberately use the cpu collector twice with a short delay between
// scrapes: gopsutil's cpu.Percent(0, *) needs a baseline call before it
// returns meaningful numbers, otherwise the first sample is always 0.
// 250ms is short enough to feel instant on the CLI but long enough for
// most kernels to accumulate a believable delta. See cpu.go for context.
//
// Per-collector errors are reported on stderr but do NOT change the exit
// code unless EVERY collector failed (in which case the batch would be
// empty and we surface a Runtime error). Partial scrapes are normal.
func runOnce(path string, stdout, stderr io.Writer) int {
	cfg, _, err := config.LoadWith(path, config.LoadOptions{RequireToken: false})
	if err != nil {
		fmt.Fprintf(stderr, "config error: %v\n", err)
		return exitcode.Config
	}

	hi := collector.CollectHostInfo()
	hi.AgentVersion = version.Version

	reg := collector.NewRegistry(collector.All(), hi, cfg.Labels, cfg.MetricsAllowlist)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Warm-up scrape: discard the result. This primes gopsutil's
	// internal CPU baseline so the second scrape has a real delta to
	// report. Cheap (microseconds for everything except cpu.Percent
	// itself, which is also non-blocking) and well worth the
	// always-zero-on-first-scrape avoidance.
	_, _ = reg.Scrape(ctx, time.Now())
	time.Sleep(250 * time.Millisecond)

	batch, errs := reg.Scrape(ctx, time.Now())

	if len(batch.Metrics) == 0 {
		// Total wipeout — nothing to ship. This is the only case
		// where --once is a hard failure; even one good sample
		// means the agent is doing its job.
		fmt.Fprintln(stderr, "no samples produced; collector errors:")
		fmt.Fprintln(stderr, collector.FormatErrors(errs))
		return exitcode.Runtime
	}

	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(batch); err != nil {
		fmt.Fprintf(stderr, "encode batch: %v\n", err)
		return exitcode.Runtime
	}

	if msg := collector.FormatErrors(errs); msg != "" {
		fmt.Fprintln(stderr, "collector warnings (non-fatal):")
		fmt.Fprintln(stderr, msg)
	}
	return exitcode.OK
}

// runCheckConfig is the body of `vigil-agent --check-config`. Loads the
// merged config (file → env), runs Validate(), and prints either:
//
//   - on success: a one-block summary of where each value came from and
//     the redacted final config
//   - on failure: the error and where we got as far as before failing
//
// Exit code: OK on success, Config on any failure.
func runCheckConfig(path string, stdout, stderr io.Writer) int {
	cfg, src, err := config.Load(path)
	if err != nil {
		fmt.Fprintf(stderr, "config: INVALID\n")
		printSource(stderr, src, cfg)
		fmt.Fprintf(stderr, "error: %v\n", err)
		return exitcode.Config
	}

	fmt.Fprintln(stdout, "config: OK")
	printSource(stdout, src, cfg)
	return exitcode.OK
}

// printSource writes a short, copy-pasteable summary of where the merged
// config came from and what it ended up looking like. Token is always
// redacted (see config.Redacted) — never log the raw token.
func printSource(w io.Writer, src config.Source, cfg config.Config) {
	r := cfg.Redacted()
	switch {
	case src.FileExisted:
		fmt.Fprintf(w, "  file:           %s (loaded)\n", src.FilePath)
	case src.FilePath != "":
		fmt.Fprintf(w, "  file:           %s (not present, env-only)\n", src.FilePath)
	default:
		fmt.Fprintln(w, "  file:           (none)")
	}
	fmt.Fprintf(w, "  env overrides:  %d\n", src.EnvVarsApplied)
	fmt.Fprintf(w, "  ingest_url:     %s\n", r.IngestURL)
	fmt.Fprintf(w, "  token:          %s\n", r.Token)
	fmt.Fprintf(w, "  scrape_interval_s: %d\n", r.ScrapeIntervalS)
	fmt.Fprintf(w, "  labels:         %v\n", r.Labels)
	fmt.Fprintf(w, "  metrics_allowlist: %v\n", r.MetricsAllowlist)
	fmt.Fprintf(w, "  log_level:      %s\n", r.LogLevel)
	fmt.Fprintf(w, "  log_format:     %s\n", r.LogFormat)
}
