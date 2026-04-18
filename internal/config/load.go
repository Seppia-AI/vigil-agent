package config

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Env-var names. Listed as constants so they're greppable and so the
// example YAML / docs generator can iterate over them. Matches the table
// in README.md — keep these in sync.
const (
	EnvConfig          = "VIGIL_CONFIG"
	EnvIngestURL       = "VIGIL_INGEST_URL"
	EnvToken           = "VIGIL_TOKEN" //nolint:gosec // env var name, not a credential
	EnvScrapeIntervalS = "VIGIL_SCRAPE_INTERVAL_S"
	EnvLabels          = "VIGIL_LABELS"
	EnvMetricsAllow    = "VIGIL_METRICS_ALLOWLIST"
	EnvLogLevel        = "VIGIL_LOG_LEVEL"
	EnvLogFormat       = "VIGIL_LOG_FORMAT"

	// EnvDisableUpdateCheck — any of "1", "true", "yes", "on" (case-
	// insensitive) sets DisableUpdateCheck=true. Anything else, including
	// empty / unset, leaves the file value alone. Same convention as
	// other "disable" boolean envs (Docker DOCKER_BUILDKIT=0/1, etc.).
	EnvDisableUpdateCheck = "VIGIL_DISABLE_UPDATE_CHECK"

	// EnvUpdateCheckURL overrides the URL the updater polls. Useful for
	// staging / mirror setups; defaults to <IngestURL>/vigil/agent/latest_version.
	EnvUpdateCheckURL = "VIGIL_UPDATE_CHECK_URL"
)

// Source describes where the loaded config came from. Useful in
// --check-config output ("config: file=/etc/seppia/vigil.yml, env vars
// applied: 2") and in the daemon's startup banner so an operator looking
// at logs can tell why the agent is using the values it's using.
type Source struct {
	// FilePath is the YAML file that was read, or "" if no file was loaded.
	FilePath string

	// FileExisted is true iff a config file was actually read. False when
	// the default path was tried and not found (acceptable: env-only
	// config). When the operator passes --config explicitly and the file
	// is missing, Load returns an error instead.
	FileExisted bool

	// EnvVarsApplied is the count of env vars that overrode a field.
	// Surfaced in --check-config so misconfigured shells (typos like
	// `VIGIL_TOEKN`) are spotted immediately.
	EnvVarsApplied int
}

// Load builds a Config by:
//
//  1. starting from Defaults()
//  2. overlaying values from the YAML file at `path` (if any)
//  3. overlaying values from VIGIL_* environment variables
//  4. validating the result
//
// Path selection (highest priority first):
//
//   - explicit `path` argument (typically from `--config`)
//   - $VIGIL_CONFIG env var
//   - DefaultConfigPath ("/etc/seppia/vigil.yml")
//
// A missing file at the DEFAULT path is allowed (the operator may run
// env-only). A missing file at an EXPLICIT path is an error — they
// asked for that file by name and a typo there should be loud.
func Load(path string) (Config, Source, error) {
	return LoadWith(path, LoadOptions{RequireToken: true})
}

// LoadOptions tweaks Load's strictness. The zero value is intentionally
// strict-ish: only RequireToken needs an explicit opt-out (Load itself
// passes RequireToken=true to preserve the daemon's "fail fast" behaviour).
type LoadOptions struct {
	// RequireToken, when false, allows a missing or empty Token to
	// pass validation. Used by `vigil-agent --once`, which prints a
	// JSON batch to stdout and never POSTs anything — so demanding
	// a real token there is just developer friction. The token's
	// SHAPE is still validated when present (no whitespace etc.).
	RequireToken bool
}

// LoadWith is the explicit-options form of Load. Most callers want
// Load(); --once and any future "diagnostics" subcommands want this.
func LoadWith(path string, opts LoadOptions) (Config, Source, error) {
	cfg := Defaults()
	src := Source{}

	resolved, explicit := resolveConfigPath(path)
	src.FilePath = resolved

	if resolved != "" {
		fileCfg, exists, err := readYAMLFile(resolved)
		// Set FileExisted as soon as we know the file is on disk —
		// even if parsing failed. Otherwise --check-config's "file:
		// ... (not present)" line lies about a present-but-malformed
		// file, which is exactly the case the operator most needs
		// to debug.
		if exists {
			src.FileExisted = true
		}
		switch {
		case err != nil:
			return cfg, src, fmt.Errorf("read %s: %w", resolved, err)
		case !exists && explicit:
			// Operator explicitly named a file that doesn't exist.
			// Refuse to silently fall back to env-only — they'd
			// never spot the typo.
			return cfg, src, fmt.Errorf("config file not found: %s", resolved)
		case exists:
			cfg = mergeFromFile(cfg, fileCfg)
		}
	}

	cfg, applied, err := applyEnvOverrides(cfg)
	if err != nil {
		return cfg, src, err
	}
	src.EnvVarsApplied = applied

	var vErr error
	if opts.RequireToken {
		vErr = cfg.Validate()
	} else {
		vErr = cfg.ValidateWithoutToken()
	}
	if vErr != nil {
		return cfg, src, vErr
	}
	return cfg, src, nil
}

// resolveConfigPath returns the file path to read and whether the
// operator named it explicitly (vs us falling back to a default).
//
// "explicit" matters because we treat a missing file differently in the
// two cases: a missing default path is fine (env-only config), a missing
// explicit path is an error.
func resolveConfigPath(arg string) (path string, explicit bool) {
	if strings.TrimSpace(arg) != "" {
		return arg, true
	}
	if env := strings.TrimSpace(os.Getenv(EnvConfig)); env != "" {
		return env, true
	}
	return DefaultConfigPath, false
}

// readYAMLFile reads and parses a YAML file into a Config. The exists
// return distinguishes "not on disk" (acceptable for the default path)
// from "exists but unreadable / unparseable" (always an error).
func readYAMLFile(path string) (Config, bool, error) {
	raw, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return Config{}, false, nil
	case err != nil:
		return Config{}, false, err
	}

	// Strict mode: unknown YAML keys are an error. Catches `tokn:` typos
	// that would otherwise silently leave Token unset and fall through
	// to the env-overlay path.
	var cfg Config
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		// yaml.v3 returns io.EOF on an empty document — that's
		// equivalent to "no overrides", not a parse error. Useful
		// for env-only configs that still want a placeholder file
		// on disk for systemd to template against.
		if errors.Is(err, io.EOF) {
			return Config{}, true, nil
		}
		return Config{}, true, fmt.Errorf("parse YAML: %w", err)
	}
	return cfg, true, nil
}

// mergeFromFile overlays only the fields the YAML file actually set on
// top of `base`. We can't just `cfg = fileCfg` because that would zero
// out defaults the YAML file omitted (e.g. an empty file would wipe
// LogLevel back to "").
//
// Heuristic: any non-zero field in `file` overrides `base`. This works
// for every type in Config because a deliberate "set this to empty"
// doesn't make sense for any current field — Token can't be the empty
// string, IngestURL can't, ScrapeIntervalS can't be 0, Labels and
// MetricsAllowlist falling back to defaults (both empty) is a no-op.
//
// If we ever add a boolean that defaults to true and needs to be
// overridable to false, switch to *bool or a `setFields []string`
// approach. For now this keeps the merge logic readable.
func mergeFromFile(base, file Config) Config {
	out := base
	if file.IngestURL != "" {
		out.IngestURL = file.IngestURL
	}
	if file.Token != "" {
		out.Token = file.Token
	}
	if file.ScrapeIntervalS != 0 {
		out.ScrapeIntervalS = file.ScrapeIntervalS
	}
	if len(file.Labels) > 0 {
		out.Labels = file.Labels
	}
	if len(file.MetricsAllowlist) > 0 {
		out.MetricsAllowlist = file.MetricsAllowlist
	}
	if file.LogLevel != "" {
		out.LogLevel = file.LogLevel
	}
	if file.LogFormat != "" {
		out.LogFormat = file.LogFormat
	}
	// DisableUpdateCheck is the only boolean field; treat ANY explicit
	// `true` as an override (we can't distinguish "unset" from "false"
	// for plain bool YAML, but only the true case differs from the
	// default). If a future maintainer needs an explicit `false`
	// override they should switch this field to *bool.
	if file.DisableUpdateCheck {
		out.DisableUpdateCheck = true
	}
	if file.UpdateCheckURL != "" {
		out.UpdateCheckURL = file.UpdateCheckURL
	}
	return out
}

// applyEnvOverrides walks every supported VIGIL_* env var and, if set,
// overrides the corresponding Config field. Returns the count of
// applied overrides so Source can report it.
//
// Empty-string env vars are TREATED AS UNSET. This matches the systemd /
// shell convention where `VIGIL_FOO=` typically means "unset" and avoids
// the surprise of `export VIGIL_TOKEN=""` silently wiping a file-loaded
// token. If an operator genuinely wants to clear a field, they should
// remove it from the YAML.
func applyEnvOverrides(cfg Config) (Config, int, error) {
	applied := 0

	if v := os.Getenv(EnvIngestURL); v != "" {
		cfg.IngestURL = v
		applied++
	}
	if v := os.Getenv(EnvToken); v != "" {
		cfg.Token = v
		applied++
	}
	if v := os.Getenv(EnvScrapeIntervalS); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return cfg, applied, fmt.Errorf("%s: not an integer (%q)", EnvScrapeIntervalS, v)
		}
		cfg.ScrapeIntervalS = n
		applied++
	}
	if v := os.Getenv(EnvLabels); v != "" {
		labels, err := parseLabelsString(v)
		if err != nil {
			return cfg, applied, fmt.Errorf("%s: %w", EnvLabels, err)
		}
		cfg.Labels = labels
		applied++
	}
	if v := os.Getenv(EnvMetricsAllow); v != "" {
		cfg.MetricsAllowlist = parseCSVList(v)
		applied++
	}
	if v := os.Getenv(EnvLogLevel); v != "" {
		cfg.LogLevel = v
		applied++
	}
	if v := os.Getenv(EnvLogFormat); v != "" {
		cfg.LogFormat = v
		applied++
	}
	if v := os.Getenv(EnvDisableUpdateCheck); v != "" {
		if parseBoolish(v) {
			cfg.DisableUpdateCheck = true
		}
		applied++
	}
	if v := os.Getenv(EnvUpdateCheckURL); v != "" {
		cfg.UpdateCheckURL = v
		applied++
	}

	return cfg, applied, nil
}

// parseBoolish accepts the loose set of truthy strings shells / config
// management tools tend to emit. We deliberately do NOT treat unknown
// values as `true` — typos like `VIGIL_DISABLE_UPDATE_CHECK=ture` should
// land as "false" (i.e. not silently disable a feature the operator
// thought they were enabling).
func parseBoolish(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// parseLabelsString turns "k1=v1,k2=v2" into map[k1]v1, [k2]v2. Whitespace
// around keys / values is trimmed so `VIGIL_LABELS="env=prod, region=eu"`
// works. Empty entries (e.g. trailing comma) are skipped silently —
// they're harmless and almost always shell concatenation artefacts.
func parseLabelsString(s string) (map[string]string, error) {
	out := map[string]string{}
	for _, pair := range strings.Split(s, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		eq := strings.IndexByte(pair, '=')
		if eq <= 0 {
			return nil, fmt.Errorf("invalid label %q: expected key=value", pair)
		}
		k := strings.TrimSpace(pair[:eq])
		v := strings.TrimSpace(pair[eq+1:])
		if k == "" {
			return nil, fmt.Errorf("invalid label %q: empty key", pair)
		}
		out[k] = v
	}
	return out, nil
}

// parseCSVList turns "a,b,c" into ["a","b","c"], trimming whitespace and
// skipping empty entries.
func parseCSVList(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		out = append(out, p)
	}
	return out
}
