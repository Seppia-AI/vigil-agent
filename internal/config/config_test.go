package config

import (
	"strings"
	"testing"
)

// validBase returns a Config that passes Validate(). Tests start from
// this and mutate one field to assert the failure path.
func validBase() Config {
	c := Defaults()
	c.Token = "test-token-1234567890"
	return c
}

func TestDefaults_Reasonable(t *testing.T) {
	d := Defaults()

	if d.IngestURL != DefaultIngestURL {
		t.Errorf("IngestURL = %q, want %q", d.IngestURL, DefaultIngestURL)
	}
	if d.ScrapeIntervalS != DefaultScrapeIntervalS {
		t.Errorf("ScrapeIntervalS = %d, want %d", d.ScrapeIntervalS, DefaultScrapeIntervalS)
	}
	if d.LogLevel != "info" {
		t.Errorf("LogLevel = %q, want %q", d.LogLevel, "info")
	}
	if d.LogFormat != "text" {
		t.Errorf("LogFormat = %q, want %q", d.LogFormat, "text")
	}
	// Critical: defaults MUST NOT supply a token. We never want a
	// placeholder token to silently let the agent start.
	if d.Token != "" {
		t.Errorf("Token unexpectedly defaulted to %q (must be empty)", d.Token)
	}
	// Defaults() must pass nothing through Validate without a token.
	if err := d.Validate(); err == nil {
		t.Error("Defaults() should fail Validate() because Token is empty")
	}
}

func TestValidate_Token(t *testing.T) {
	tests := []struct {
		name  string
		token string
		want  string // substring expected in error; empty = no error
	}{
		{"valid", "abc123", ""},
		{"empty", "", "token: required"},
		{"whitespace only", "   ", "token: required"},
		{"leading whitespace", " abc", "leading or trailing"},
		{"trailing whitespace", "abc ", "leading or trailing"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := validBase()
			c.Token = tt.token
			err := c.Validate()
			assertErrContains(t, err, tt.want)
		})
	}
}

func TestValidate_IngestURL(t *testing.T) {
	tests := []struct {
		name string
		url  string
		want string
	}{
		{"https with host", "https://api.seppia.ai", ""},
		{"http with host", "http://localhost:3000", ""},
		{"trailing slash allowed", "https://api.seppia.ai/", ""},
		{"empty", "", "ingest_url"},
		{"missing scheme", "api.seppia.ai", "scheme"},
		{"ftp", "ftp://api.seppia.ai", "scheme"},
		{"no host", "https://", "missing host"},
		{"with path", "https://api.seppia.ai/v1", "must not include a path"},
		{"with query", "https://api.seppia.ai?x=1", "query string"},
		{"with fragment", "https://api.seppia.ai#frag", "query string or fragment"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := validBase()
			c.IngestURL = tt.url
			err := c.Validate()
			assertErrContains(t, err, tt.want)
		})
	}
}

func TestValidate_ScrapeIntervalS(t *testing.T) {
	tests := []struct {
		name string
		v    int
		want string
	}{
		{"min", MinScrapeIntervalS, ""},
		{"max", MaxScrapeIntervalS, ""},
		{"middle", 60, ""},
		{"zero", 0, "scrape_interval_s"},
		{"negative", -5, "scrape_interval_s"},
		{"too large", MaxScrapeIntervalS + 1, "scrape_interval_s"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := validBase()
			c.ScrapeIntervalS = tt.v
			err := c.Validate()
			assertErrContains(t, err, tt.want)
		})
	}
}

func TestValidate_Labels(t *testing.T) {
	t.Run("empty ok", func(t *testing.T) {
		c := validBase()
		c.Labels = nil
		if err := c.Validate(); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("at cap", func(t *testing.T) {
		c := validBase()
		c.Labels = map[string]string{}
		for i := 0; i < MaxStaticLabels; i++ {
			c.Labels[charKey(i)] = "v"
		}
		if err := c.Validate(); err != nil {
			t.Fatalf("unexpected error at cap: %v", err)
		}
	})

	t.Run("over cap", func(t *testing.T) {
		c := validBase()
		c.Labels = map[string]string{}
		for i := 0; i < MaxStaticLabels+1; i++ {
			c.Labels[charKey(i)] = "v"
		}
		assertErrContains(t, c.Validate(), "static labels")
	})

	t.Run("invalid key", func(t *testing.T) {
		c := validBase()
		c.Labels = map[string]string{"bad-key!": "v"}
		assertErrContains(t, c.Validate(), "invalid label key")
	})

	t.Run("digit-leading key", func(t *testing.T) {
		c := validBase()
		c.Labels = map[string]string{"1foo": "v"}
		assertErrContains(t, c.Validate(), "invalid label key")
	})

	t.Run("value too long", func(t *testing.T) {
		c := validBase()
		c.Labels = map[string]string{"k": strings.Repeat("x", MaxLabelValueBytes+1)}
		assertErrContains(t, c.Validate(), "value too long")
	})
}

func TestValidate_MetricsAllowlist(t *testing.T) {
	c := validBase()
	c.MetricsAllowlist = []string{"cpu.usage", "  ", "mem.used"}
	assertErrContains(t, c.Validate(), "metrics_allowlist[1]")

	c.MetricsAllowlist = []string{"cpu.usage", "mem.used"}
	if err := c.Validate(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidate_LogLevelFormat(t *testing.T) {
	c := validBase()
	c.LogLevel = "loud"
	assertErrContains(t, c.Validate(), "log_level")

	c = validBase()
	c.LogFormat = "xml"
	assertErrContains(t, c.Validate(), "log_format")
}

func TestRedacted_HidesToken(t *testing.T) {
	c := validBase()
	c.Token = "secret-token-abcdef12345"
	r := c.Redacted()

	if strings.Contains(r.Token, "secret") {
		t.Errorf("Redacted token leaks the secret part: %q", r.Token)
	}
	if !strings.HasPrefix(r.Token, "secr") {
		t.Errorf("Redacted token should keep first 4 chars, got %q", r.Token)
	}
	// Original config must be untouched (Redacted returns a copy).
	if c.Token != "secret-token-abcdef12345" {
		t.Errorf("Redacted mutated the receiver: Token = %q", c.Token)
	}
}

func TestRedacted_ShortToken(t *testing.T) {
	c := validBase()
	c.Token = "abc"
	r := c.Redacted()
	if strings.Contains(r.Token, "abc") {
		t.Errorf("Short token leaked: %q", r.Token)
	}
}

// ── helpers ──────────────────────────────────────────────────────────────────

func assertErrContains(t *testing.T, err error, substr string) {
	t.Helper()
	if substr == "" {
		if err != nil {
			t.Fatalf("expected no error, got: %v", err)
		}
		return
	}
	if err == nil {
		t.Fatalf("expected error containing %q, got nil", substr)
	}
	if !strings.Contains(err.Error(), substr) {
		t.Fatalf("error %q does not contain %q", err.Error(), substr)
	}
}

func charKey(i int) string {
	// "a", "b", … "h" — enough unique single-char keys for our tests.
	return string(rune('a' + i))
}
