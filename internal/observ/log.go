// Package observ wires the agent's self-observability: structured
// logging via log/slog and an optional local Prometheus endpoint that
// exposes scheduler.Stats. Both are deliberately stdlib-only — no
// prometheus/client_golang, no logrus/zap. The full surface is
// ~200 lines; the trade is "we own the format" vs "we own the deps".
//
// Both knobs are off-by-default-friendly: the logger always exists
// (text format, info level), the metrics endpoint is opt-in via a
// non-empty `--metrics-addr`. Operators who don't care about either
// won't notice they exist.
package observ

import (
	"fmt"
	"io"
	"log/slog"
	"strings"
)

// LogFormat enumerates the wire format the slog handler produces.
// "text" is logfmt-ish (slog.NewTextHandler), suitable for journalctl
// and human eyes. "json" is one JSON object per line, suitable for
// Loki/Vector/Datadog ingestion. We deliberately do NOT support "pretty"
// — we ship with text as the default precisely because it's already
// readable in a terminal.
type LogFormat string

const (
	LogFormatText LogFormat = "text"
	LogFormatJSON LogFormat = "json"
)

// ParseLogFormat is the lenient parser used by the CLI flag handler.
// Empty string → text (matches the default behaviour). Unknown value
// → error so a typo at startup is immediately visible instead of
// silently mis-formatting an entire run.
func ParseLogFormat(s string) (LogFormat, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "text", "logfmt":
		return LogFormatText, nil
	case "json":
		return LogFormatJSON, nil
	default:
		return "", fmt.Errorf("unknown log format %q (want text|json)", s)
	}
}

// ParseLogLevel maps the human-friendly level names accepted by
// `--log-level` to slog.Level values. Empty → info (the default).
// Unknown → error.
//
// We don't expose slog.LevelError-and-up as separate options because
// the agent's error path is small enough that "warn" and "error"
// would collapse to the same set of events in practice; less noise
// in the help text is worth not having a level we'd rarely touch.
func ParseLogLevel(s string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "info":
		return slog.LevelInfo, nil
	case "debug":
		return slog.LevelDebug, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error", "err":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("unknown log level %q (want debug|info|warn|error)", s)
	}
}

// NewLogger builds a *slog.Logger writing to w with the given format
// and level. All agent code uses this — never call slog.Default()
// directly, because tests need to swap the handler and we want a
// single point where the format is decided.
//
// We bind the agent build version onto the logger as a top-level
// attribute so every log line is correlatable to a release without
// requiring the operator to also tail the startup banner. The cost
// is tiny (one extra k/v per line) and the operational value at
// 3 a.m. is large.
func NewLogger(w io.Writer, format LogFormat, level slog.Level, agentVersion string) *slog.Logger {
	if w == nil {
		w = io.Discard
	}
	opts := &slog.HandlerOptions{Level: level}

	var h slog.Handler
	switch format {
	case LogFormatJSON:
		h = slog.NewJSONHandler(w, opts)
	default:
		// LogFormatText also catches the zero value, which is what
		// internal callers use when they don't have a CLI flag in
		// scope (tests, --once).
		h = slog.NewTextHandler(w, opts)
	}
	logger := slog.New(h)
	if agentVersion != "" {
		// `With` returns a new logger that prepends the attribute
		// to every record; cheaper than setting it per-call site.
		logger = logger.With(slog.String("agent_version", agentVersion))
	}
	return logger
}

// Discard returns a logger that drops every record. Used by tests that
// don't care about log output and by code paths (e.g. --once) that run
// briefly enough that the startup banner is the only useful signal.
func Discard() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
