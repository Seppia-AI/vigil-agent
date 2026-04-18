// Package config defines the agent's runtime configuration: how it's shaped,
// what the defaults are, and what counts as a valid value.
//
// Loading (file → env overlay) lives in load.go; this file is purely
// definitions + validation so it stays cheap to test and easy to read.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// DefaultConfigPath is where the agent looks for its YAML config when no
// --config flag and no VIGIL_CONFIG env var is set. Matches the path the
// installer drops the file at.
const DefaultConfigPath = "/etc/seppia/vigil.yml"

// DefaultIngestURL is the production Vigil ingest base URL. The agent
// appends "/vigil/vitals/<token>" to it.
//
// Override with the `ingest_url:` YAML field or the VIGIL_INGEST_URL env
// var. Self-hosters / staging environments are the main reason this is
// configurable.
const DefaultIngestURL = "https://api.seppia.ai"

// UpdateCheckPath is appended to the IngestURL (or to UpdateCheckURL if the
// operator overrode it to a bare base) to form the full URL the updater
// polls. Single source of truth so the agent and the ingest endpoint stay
// in sync.
const UpdateCheckPath = "/vigil/agent/latest_version"

// Scrape interval bounds.
//
//   - Min 1s is the absolute floor allowed by the schema. The server may
//     enforce a tighter per-account floor on first POST and the agent
//     clamps locally to whatever the server returns; this minimum exists
//     only so a config like `scrape_interval_s: 0` fails loudly.
//   - Max 1h is a sanity cap: anything slower than hourly is almost
//     certainly a typo (a probe scraping every 24h would look "stale" the
//     entire time it's healthy because freshness is computed as 6×
//     scrape_interval_s on the server).
const (
	MinScrapeIntervalS     = 1
	MaxScrapeIntervalS     = 3600
	DefaultScrapeIntervalS = 60
)

// MaxStaticLabels is the agent-side cap on the number of static labels
// (the `labels:` map in vigil.yml that gets merged into every sample).
//
// The server caps total labels per sample at 16. We cap the static map at
// half that to leave headroom for per-sample labels emitted by collectors
// — e.g. a
// disk.* sample carrying `device=sda1` or a net.* sample carrying
// `iface=eth0`. Without this headroom, a probe with 16 static labels
// would silently drop every per-sample label at ingest time.
const MaxStaticLabels = 8

// MaxLabelValueBytes mirrors the server's per-label-value limit. We
// validate up front so a too-long label fails at --check-config time
// rather than silently being truncated on every batch.
const MaxLabelValueBytes = 256

// validLabelKey allows the same identifier shape Prometheus / OpenTelemetry
// use: starts with a letter or underscore, then letters / digits /
// underscores. Matches what the server's label sanitizer accepts without
// stripping characters.
var validLabelKey = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// ValidLogLevels and ValidLogFormats are exported so tests and the example
// YAML generator can stay in sync with what Validate accepts.
var (
	ValidLogLevels  = []string{"debug", "info", "warn", "error"}
	ValidLogFormats = []string{"text", "json"}
)

// Config is the full set of knobs the operator can set, in any of: a YAML
// file at DefaultConfigPath, a YAML file at the path given by --config /
// VIGIL_CONFIG, or per-field environment variables.
//
// Fields are flat on purpose. Nested config encourages premature taxonomy
// (a `tls:` group, a `retry:` group, …) that we don't have anything to
// put in yet. Promote to nested struct when the field count actually
// justifies it.
type Config struct {
	// IngestURL is the base URL of the Vigil API. The agent POSTs
	// metric batches to <IngestURL>/vigil/vitals/<Token>. Required;
	// defaulted to DefaultIngestURL.
	IngestURL string `yaml:"ingest_url"`

	// Token is the per-probe ingest secret. Created in the Vigil UI on
	// the probe's detail page; rotatable via "Regenerate token". REQUIRED
	// — there is no demo / no-auth mode.
	Token string `yaml:"token"`

	// ScrapeIntervalS is the desired seconds-between-scrapes. Subject to
	// a per-account minimum the server returns on first POST; the agent
	// clamps locally if its config is faster than that minimum and logs
	// once.
	ScrapeIntervalS int `yaml:"scrape_interval_s"`

	// Labels are static key/value pairs merged into every sample sent by
	// this agent. Useful for `env=prod`, `region=eu-west-1`, etc.
	// Capped at MaxStaticLabels to leave per-sample label headroom under
	// the server's 16-label-per-sample cap.
	Labels map[string]string `yaml:"labels"`

	// MetricsAllowlist, if non-empty, restricts what the agent emits to
	// just the listed metric names. The server filters too — this exists
	// for noisy hosts where you want to avoid even building the rejected
	// samples. Empty = ship everything the collectors produce.
	MetricsAllowlist []string `yaml:"metrics_allowlist"`

	// LogLevel is one of ValidLogLevels.
	LogLevel string `yaml:"log_level"`

	// LogFormat is one of ValidLogFormats. "text" is human-readable;
	// "json" is structured for log aggregators.
	LogFormat string `yaml:"log_format"`

	// DisableUpdateCheck, when true, suppresses the agent's once-a-day
	// "is there a newer release?" GET against UpdateCheckURL. Defaults
	// to false (check is on). The CLI flag --no-update-check sets this
	// true at runtime without touching the file.
	//
	// Operators in air-gapped environments, or those who tightly pin
	// agent versions via configuration management, will want this on.
	DisableUpdateCheck bool `yaml:"disable_update_check"`

	// UpdateCheckURL, if non-empty, overrides the URL the updater polls.
	// Empty (the default) means "derive from IngestURL" — i.e. the agent
	// hits <IngestURL>/vigil/agent/latest_version. Self-hosters who
	// proxy releases through a private mirror are the main reason for
	// this knob.
	UpdateCheckURL string `yaml:"update_check_url"`
}

// Defaults returns a Config pre-filled with the values the agent uses
// when nothing is specified. Callers (LoadFile, --check-config tests,
// the daemon main loop) start from Defaults and overlay file + env on
// top, so any unset field gets the documented default.
//
// Notably, Token is NOT defaulted — Validate() requires it. We refuse
// to ever start with a placeholder token because that would silently
// dead-letter into a 404 loop on the server side.
func Defaults() Config {
	return Config{
		IngestURL:        DefaultIngestURL,
		ScrapeIntervalS:  DefaultScrapeIntervalS,
		Labels:           map[string]string{},
		MetricsAllowlist: []string{},
		LogLevel:         "info",
		LogFormat:        "text",
	}
}

// Validate checks that every field holds a usable value, returning the
// first problem encountered. Errors are wrapped with %w so callers can
// inspect them with errors.Is / errors.As if they ever need to (today,
// `vigil-agent --check-config` just prints them and exits non-zero).
//
// Order of checks is "most likely to be wrong first" so the operator
// fixing a bad config sees the obvious problem (missing token) before
// the subtle one (label value over the byte limit).
func (c Config) Validate() error {
	return c.validate(true)
}

// ValidateWithoutToken is the same as Validate but accepts a missing /
// empty Token. Used by `vigil-agent --once`, which prints a JSON batch
// to stdout and never attempts to POST — so the absence of a token is
// not a real problem there. The daemon and --check-config still call
// Validate() and fail loudly on a missing token.
func (c Config) ValidateWithoutToken() error {
	return c.validate(false)
}

func (c Config) validate(requireToken bool) error {
	if requireToken {
		if strings.TrimSpace(c.Token) == "" {
			return errors.New("token: required (set `token:` in config or VIGIL_TOKEN env var)")
		}
	}
	// Whitespace check still runs even when the token is optional —
	// a present-but-malformed token is always an operator error,
	// regardless of whether we're about to POST with it.
	if c.Token != "" && c.Token != strings.TrimSpace(c.Token) {
		return errors.New("token: must not contain leading or trailing whitespace")
	}

	if err := validateURL(c.IngestURL); err != nil {
		return fmt.Errorf("ingest_url: %w", err)
	}

	if c.ScrapeIntervalS < MinScrapeIntervalS || c.ScrapeIntervalS > MaxScrapeIntervalS {
		return fmt.Errorf("scrape_interval_s: must be between %d and %d (got %d)",
			MinScrapeIntervalS, MaxScrapeIntervalS, c.ScrapeIntervalS)
	}

	if err := validateLabels(c.Labels); err != nil {
		return fmt.Errorf("labels: %w", err)
	}

	for i, m := range c.MetricsAllowlist {
		if strings.TrimSpace(m) == "" {
			return fmt.Errorf("metrics_allowlist[%d]: empty entry", i)
		}
	}

	if !contains(ValidLogLevels, c.LogLevel) {
		return fmt.Errorf("log_level: must be one of %v (got %q)", ValidLogLevels, c.LogLevel)
	}
	if !contains(ValidLogFormats, c.LogFormat) {
		return fmt.Errorf("log_format: must be one of %v (got %q)", ValidLogFormats, c.LogFormat)
	}

	// UpdateCheckURL is optional (empty = derive from IngestURL). When
	// the operator sets it explicitly, validate the same way IngestURL
	// is validated — otherwise a typo in the YAML would only surface as
	// a debug-level "update.check_failed" once a day, very hard to spot.
	if c.UpdateCheckURL != "" {
		if err := validateUpdateCheckURL(c.UpdateCheckURL); err != nil {
			return fmt.Errorf("update_check_url: %w", err)
		}
	}

	return nil
}

// ResolvedUpdateCheckURL returns the absolute URL the updater should poll.
// If UpdateCheckURL is set, it's returned as-is; otherwise it's derived
// from IngestURL by appending UpdateCheckPath.
//
// Centralised here so the daemon doesn't have to know how the two fields
// compose, and so a future change (e.g. moving the path) is one edit.
func (c Config) ResolvedUpdateCheckURL() string {
	if strings.TrimSpace(c.UpdateCheckURL) != "" {
		return c.UpdateCheckURL
	}
	base := strings.TrimRight(c.IngestURL, "/")
	return base + UpdateCheckPath
}

// validateUpdateCheckURL is like validateURL but tolerant of a path: an
// operator who points at a non-default mirror will typically include the
// full URL ("https://mirror.example.com/path/to/latest_version"), unlike
// IngestURL which is a base URL the agent appends to.
func validateUpdateCheckURL(s string) error {
	u, err := url.Parse(strings.TrimSpace(s))
	if err != nil {
		return fmt.Errorf("not a valid URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("scheme must be http or https (got %q)", u.Scheme)
	}
	if u.Host == "" {
		return errors.New("missing host")
	}
	return nil
}

// validateURL accepts http(s) URLs with a host but no path / query /
// fragment. The agent will append "/vigil/vitals/<token>" itself, so a
// configured base URL with a trailing path would silently produce
// double-slashes or wrong endpoints.
func validateURL(s string) error {
	if strings.TrimSpace(s) == "" {
		return errors.New("required")
	}
	u, err := url.Parse(s)
	if err != nil {
		return fmt.Errorf("not a valid URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("scheme must be http or https (got %q)", u.Scheme)
	}
	if u.Host == "" {
		return errors.New("missing host")
	}
	// Accept "/" as the path (some callers will write `https://api.seppia.ai/`)
	// but reject anything more specific — the agent owns the path segment.
	if u.Path != "" && u.Path != "/" {
		return fmt.Errorf("must not include a path (got %q)", u.Path)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return errors.New("must not include a query string or fragment")
	}
	return nil
}

// validateLabels enforces the static-label cap, key shape, and value
// length. Empty map is fine.
func validateLabels(labels map[string]string) error {
	if len(labels) > MaxStaticLabels {
		return fmt.Errorf("at most %d static labels allowed (got %d) — server caps total labels per sample at 16; we reserve headroom for per-sample collector labels",
			MaxStaticLabels, len(labels))
	}
	for k, v := range labels {
		if !validLabelKey.MatchString(k) {
			return fmt.Errorf("invalid label key %q: must match [a-zA-Z_][a-zA-Z0-9_]*", k)
		}
		if len(v) > MaxLabelValueBytes {
			return fmt.Errorf("label %q value too long: %d bytes (max %d)", k, len(v), MaxLabelValueBytes)
		}
	}
	return nil
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// Redacted returns a copy of the config safe to log: the Token is replaced
// with a fingerprint (first 4 chars + length) so operators can correlate
// logs with the right probe without a `.token` value ever hitting log
// storage. Use this everywhere we print Config — never print c directly.
func (c Config) Redacted() Config {
	out := c
	out.Token = redactToken(c.Token)
	return out
}

func redactToken(t string) string {
	t = strings.TrimSpace(t)
	if t == "" {
		return ""
	}
	if len(t) <= 4 {
		return fmt.Sprintf("***(len=%d)", len(t))
	}
	return fmt.Sprintf("%s***(len=%d)", t[:4], len(t))
}
