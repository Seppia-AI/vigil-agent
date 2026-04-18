package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// All env vars Load() reads. Saved + restored around each test so the
// suite is order-independent and doesn't leak state.
var allEnvVars = []string{
	EnvConfig,
	EnvIngestURL,
	EnvToken,
	EnvScrapeIntervalS,
	EnvLabels,
	EnvMetricsAllow,
	EnvLogLevel,
	EnvLogFormat,
}

// withCleanEnv unsets every VIGIL_* env var Load reads before the test
// body, and restores them after. Tests that want to set specific env
// vars do so inside the body.
func withCleanEnv(t *testing.T) {
	t.Helper()
	saved := map[string]string{}
	for _, k := range allEnvVars {
		if v, ok := os.LookupEnv(k); ok {
			saved[k] = v
		}
		_ = os.Unsetenv(k)
	}
	t.Cleanup(func() {
		for _, k := range allEnvVars {
			if v, ok := saved[k]; ok {
				_ = os.Setenv(k, v)
			} else {
				_ = os.Unsetenv(k)
			}
		}
	})
}

func writeYAML(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "vigil.yml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write temp config: %v", err)
	}
	return p
}

func TestLoad_FileOnly(t *testing.T) {
	withCleanEnv(t)

	path := writeYAML(t, `
ingest_url: https://api.example.com
token: file-token
scrape_interval_s: 30
labels:
  env: prod
  region: eu-west-1
metrics_allowlist:
  - cpu.usage
  - mem.used
log_level: debug
log_format: json
`)

	cfg, src, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if !src.FileExisted {
		t.Error("Source.FileExisted = false, want true")
	}
	if src.EnvVarsApplied != 0 {
		t.Errorf("Source.EnvVarsApplied = %d, want 0", src.EnvVarsApplied)
	}
	if cfg.Token != "file-token" {
		t.Errorf("Token = %q, want file-token", cfg.Token)
	}
	if cfg.IngestURL != "https://api.example.com" {
		t.Errorf("IngestURL = %q", cfg.IngestURL)
	}
	if cfg.ScrapeIntervalS != 30 {
		t.Errorf("ScrapeIntervalS = %d", cfg.ScrapeIntervalS)
	}
	wantLabels := map[string]string{"env": "prod", "region": "eu-west-1"}
	if !reflect.DeepEqual(cfg.Labels, wantLabels) {
		t.Errorf("Labels = %v, want %v", cfg.Labels, wantLabels)
	}
	wantAllow := []string{"cpu.usage", "mem.used"}
	if !reflect.DeepEqual(cfg.MetricsAllowlist, wantAllow) {
		t.Errorf("MetricsAllowlist = %v, want %v", cfg.MetricsAllowlist, wantAllow)
	}
	if cfg.LogLevel != "debug" || cfg.LogFormat != "json" {
		t.Errorf("LogLevel/LogFormat = %q/%q", cfg.LogLevel, cfg.LogFormat)
	}
}

func TestLoad_EnvOnly_DefaultPathMissing(t *testing.T) {
	withCleanEnv(t)
	t.Setenv(EnvToken, "env-token")

	// Pass empty path AND make sure VIGIL_CONFIG isn't set: Load will
	// try DefaultConfigPath, find nothing, and fall back to env-only.
	// In the test sandbox /etc/seppia/vigil.yml typically doesn't
	// exist, so this exercises the "default missing is OK" branch.
	cfg, src, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if src.FileExisted {
		t.Errorf("Source.FileExisted = true, want false (default path should be absent in sandbox)")
	}
	if src.EnvVarsApplied != 1 {
		t.Errorf("Source.EnvVarsApplied = %d, want 1", src.EnvVarsApplied)
	}
	if cfg.Token != "env-token" {
		t.Errorf("Token = %q, want env-token", cfg.Token)
	}
	if cfg.IngestURL != DefaultIngestURL {
		t.Errorf("IngestURL = %q, want default %q", cfg.IngestURL, DefaultIngestURL)
	}
}

func TestLoad_EnvOverridesFile(t *testing.T) {
	withCleanEnv(t)

	path := writeYAML(t, `
token: file-token
scrape_interval_s: 30
log_level: info
`)

	t.Setenv(EnvToken, "env-token")
	t.Setenv(EnvScrapeIntervalS, "120")
	t.Setenv(EnvLogLevel, "debug")

	cfg, src, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if !src.FileExisted {
		t.Error("FileExisted = false")
	}
	if src.EnvVarsApplied != 3 {
		t.Errorf("EnvVarsApplied = %d, want 3", src.EnvVarsApplied)
	}
	if cfg.Token != "env-token" {
		t.Errorf("env should win: Token = %q", cfg.Token)
	}
	if cfg.ScrapeIntervalS != 120 {
		t.Errorf("env should win: ScrapeIntervalS = %d", cfg.ScrapeIntervalS)
	}
	if cfg.LogLevel != "debug" {
		t.Errorf("env should win: LogLevel = %q", cfg.LogLevel)
	}
}

func TestLoad_ExplicitMissingFileIsError(t *testing.T) {
	withCleanEnv(t)
	t.Setenv(EnvToken, "env-token")

	_, _, err := Load("/nonexistent/vigil.yml")
	if err == nil {
		t.Fatal("expected error for explicit missing config, got nil")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error %q should mention 'not found'", err.Error())
	}
}

func TestLoad_VIGIL_CONFIG_Env(t *testing.T) {
	withCleanEnv(t)

	path := writeYAML(t, `
token: from-env-config-path
`)
	t.Setenv(EnvConfig, path)

	cfg, src, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !src.FileExisted {
		t.Error("FileExisted = false")
	}
	if src.FilePath != path {
		t.Errorf("FilePath = %q, want %q", src.FilePath, path)
	}
	if cfg.Token != "from-env-config-path" {
		t.Errorf("Token = %q", cfg.Token)
	}
}

func TestLoad_UnknownYAMLKeyIsError(t *testing.T) {
	withCleanEnv(t)

	// `tokn` instead of `token` — the kind of typo we want to catch.
	path := writeYAML(t, `
tokn: oops
ingest_url: https://api.seppia.ai
`)

	_, _, err := Load(path)
	if err == nil {
		t.Fatal("expected error for unknown YAML key, got nil")
	}
	if !strings.Contains(err.Error(), "tokn") {
		t.Errorf("error should mention the unknown key, got: %v", err)
	}
}

func TestLoad_EmptyFileIsAccepted(t *testing.T) {
	withCleanEnv(t)
	t.Setenv(EnvToken, "env-token")

	// Empty YAML file: treated as no overrides (env still wins).
	path := writeYAML(t, "")
	cfg, src, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !src.FileExisted {
		t.Error("FileExisted should be true for an empty file that exists on disk")
	}
	if cfg.Token != "env-token" {
		t.Errorf("Token = %q, want env-token", cfg.Token)
	}
}

func TestLoad_MalformedYAML(t *testing.T) {
	withCleanEnv(t)
	path := writeYAML(t, "token: [unterminated")

	_, src, err := Load(path)
	if err == nil {
		t.Fatal("expected parse error, got nil")
	}
	if !strings.Contains(err.Error(), "parse YAML") {
		t.Errorf("error should be a parse error, got: %v", err)
	}
	// The file IS on disk — Source must reflect that even when parse
	// fails, so --check-config's "file: ... (not present)" line doesn't
	// lie about the present-but-malformed file the operator is debugging.
	if !src.FileExisted {
		t.Error("Source.FileExisted = false, want true (file is on disk)")
	}
	if src.FilePath != path {
		t.Errorf("Source.FilePath = %q, want %q", src.FilePath, path)
	}
}

func TestLoad_BadScrapeIntervalEnv(t *testing.T) {
	withCleanEnv(t)
	t.Setenv(EnvToken, "t")
	t.Setenv(EnvScrapeIntervalS, "not-an-int")

	_, _, err := Load("")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), EnvScrapeIntervalS) {
		t.Errorf("error should name the env var, got: %v", err)
	}
}

func TestLoad_EmptyEnvDoesNotOverride(t *testing.T) {
	withCleanEnv(t)

	path := writeYAML(t, `
token: file-token
log_level: debug
`)
	// Explicitly empty — must NOT clobber the file value (matches the
	// systemd convention that an empty env var means "unset").
	t.Setenv(EnvLogLevel, "")

	cfg, src, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.LogLevel != "debug" {
		t.Errorf("empty env should not override file: LogLevel = %q", cfg.LogLevel)
	}
	if src.EnvVarsApplied != 0 {
		t.Errorf("EnvVarsApplied = %d, want 0", src.EnvVarsApplied)
	}
}

func TestParseLabelsString(t *testing.T) {
	tests := []struct {
		in    string
		want  map[string]string
		isErr bool
	}{
		{"env=prod", map[string]string{"env": "prod"}, false},
		{"env=prod,region=eu", map[string]string{"env": "prod", "region": "eu"}, false},
		{"env = prod , region = eu ", map[string]string{"env": "prod", "region": "eu"}, false},
		{"env=prod,", map[string]string{"env": "prod"}, false},
		{",", map[string]string{}, false},
		{"=value", nil, true},
		{"keyonly", nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := parseLabelsString(tt.in)
			if tt.isErr {
				if err == nil {
					t.Fatalf("expected error, got nil (got=%v)", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestParseCSVList(t *testing.T) {
	got := parseCSVList("a, b ,, c,")
	want := []string{"a", "b", "c"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}

	if got := parseCSVList(""); len(got) != 0 {
		t.Errorf("empty input should yield empty slice, got %v", got)
	}
}
