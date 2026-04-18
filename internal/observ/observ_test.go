package observ

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// stubProvider is the trivial StatsProvider used by every metrics test.
// Carrying just a stored snapshot avoids importing scheduler from this
// package (and the import-cycle risk that comes with it).
type stubProvider struct{ snap StatsSnapshot }

func (s stubProvider) StatsSnapshot() StatsSnapshot { return s.snap }

func TestParseLogFormat(t *testing.T) {
	cases := []struct {
		in      string
		want    LogFormat
		wantErr bool
	}{
		{"", LogFormatText, false},
		{"text", LogFormatText, false},
		{"TEXT", LogFormatText, false},
		{"logfmt", LogFormatText, false}, // alias
		{"json", LogFormatJSON, false},
		{"JSON", LogFormatJSON, false},
		{"yaml", "", true},
		{"asdf", "", true},
	}
	for _, c := range cases {
		got, err := ParseLogFormat(c.in)
		if (err != nil) != c.wantErr {
			t.Errorf("ParseLogFormat(%q) err=%v wantErr=%v", c.in, err, c.wantErr)
			continue
		}
		if got != c.want {
			t.Errorf("ParseLogFormat(%q) = %q want %q", c.in, got, c.want)
		}
	}
}

func TestParseLogLevel(t *testing.T) {
	cases := []struct {
		in      string
		want    slog.Level
		wantErr bool
	}{
		{"", slog.LevelInfo, false},
		{"info", slog.LevelInfo, false},
		{"INFO", slog.LevelInfo, false},
		{"debug", slog.LevelDebug, false},
		{"warn", slog.LevelWarn, false},
		{"warning", slog.LevelWarn, false}, // alias
		{"error", slog.LevelError, false},
		{"err", slog.LevelError, false}, // alias
		{"trace", 0, true},
	}
	for _, c := range cases {
		got, err := ParseLogLevel(c.in)
		if (err != nil) != c.wantErr {
			t.Errorf("ParseLogLevel(%q) err=%v wantErr=%v", c.in, err, c.wantErr)
			continue
		}
		if !c.wantErr && got != c.want {
			t.Errorf("ParseLogLevel(%q) = %v want %v", c.in, got, c.want)
		}
	}
}

func TestNewLogger_RespectsFormatAndLevel(t *testing.T) {
	// JSON format → output is one valid JSON object per line.
	// info level → debug records are dropped.
	// AgentVersion → present on every record as a top-level attribute.
	buf := &bytes.Buffer{}
	logger := NewLogger(buf, LogFormatJSON, slog.LevelInfo, "v9.9")
	logger.Debug("noisy", slog.String("key", "v"))
	logger.Info("important", slog.String("k", "v"))
	out := buf.String()
	if strings.Contains(out, "noisy") {
		t.Errorf("debug record leaked into info-level output: %s", out)
	}
	if !strings.Contains(out, `"important"`) {
		t.Errorf("info record missing from output: %s", out)
	}
	if !strings.Contains(out, `"agent_version":"v9.9"`) {
		t.Errorf("agent_version not bound: %s", out)
	}
	// First non-empty line should look like JSON.
	if !strings.HasPrefix(strings.TrimSpace(out), "{") {
		t.Errorf("expected JSON output; got: %s", out)
	}
}

func TestNewLogger_DiscardWhenWriterNil(t *testing.T) {
	// Defensive: NewLogger(nil, …) must not panic. Useful for tests
	// that build a logger before deciding where to send it.
	l := NewLogger(nil, LogFormatText, slog.LevelInfo, "")
	if l == nil {
		t.Fatal("nil logger returned")
	}
	l.Info("should not crash")
}

func TestMetricsHandler_RendersExposition(t *testing.T) {
	snap := StatsSnapshot{
		ScrapesOK:           7,
		ScrapesPartial:      1,
		ScrapesEmpty:        0,
		SamplesCollected:    420,
		BatchesSent:         5,
		BatchesFailed:       3,
		DroppedOverflow:     2,
		DroppedQuotaSamples: 11,
		LastFlushUnix:       1_700_000_000,
	}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	MetricsHandler(stubProvider{snap}, "v1.2.3").ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d want 200", rr.Code)
	}
	ct := rr.Header().Get("Content-Type")
	if !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type=%q want text/plain prefix", ct)
	}
	body := rr.Body.String()

	// Spot-check the must-have lines. We don't compare the whole
	// body byte-for-byte because the order is alphabetical and the
	// HELP text might evolve — but every metric MUST appear with
	// the right value, exactly once.
	mustContain := []string{
		"# TYPE vigil_agent_scrapes_ok_total counter",
		"vigil_agent_scrapes_ok_total 7",
		"vigil_agent_scrapes_partial_total 1",
		"vigil_agent_samples_collected_total 420",
		"vigil_agent_batches_sent_total 5",
		"vigil_agent_batches_failed_total 3",
		"vigil_agent_dropped_overflow_total 2",
		"vigil_agent_dropped_quota_samples_total 11",
		"# TYPE vigil_agent_last_flush_unix gauge",
		"vigil_agent_last_flush_unix 1700000000",
		`vigil_agent_build_info{version="v1.2.3"} 1`,
	}
	for _, want := range mustContain {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q\n--- body ---\n%s", want, body)
		}
	}

	// Sanity: every counter line must be matched by a HELP and TYPE
	// line (Prometheus's text format requires this for consistent
	// scraping). Counting `# HELP` and `# TYPE` is good enough.
	// 9 builtin metrics + 1 build_info = 10 HELP/TYPE pairs.
	if got, want := strings.Count(body, "# HELP "), len(builtin)+1; got != want {
		t.Errorf("# HELP lines = %d, want %d", got, want)
	}
	if got, want := strings.Count(body, "# TYPE "), len(builtin)+1; got != want {
		t.Errorf("# TYPE lines = %d, want %d", got, want)
	}
}

func TestMetricsHandler_RejectsNonGET(t *testing.T) {
	// POST/PUT/DELETE → 405. Defends against a misconfigured client
	// confusing /metrics with /vigil/vitals/<token>.
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/metrics", nil)
	MetricsHandler(stubProvider{}, "v1").ServeHTTP(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /metrics status=%d want 405", rr.Code)
	}
}

func TestMetricsHandler_HEADIsAllowed(t *testing.T) {
	// Some monitoring agents (kube probes, basic uptime checks) use
	// HEAD for liveness pings. Treat it identically to GET.
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodHead, "/metrics", nil)
	MetricsHandler(stubProvider{}, "v1").ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Errorf("HEAD /metrics status=%d want 200", rr.Code)
	}
}

func TestStartMetricsServer_ServesAndShutsDown(t *testing.T) {
	// End-to-end: bind ephemeral port, scrape /metrics over real
	// TCP, scrape /healthz, then Stop should return promptly.
	logBuf := &bytes.Buffer{}
	logger := slog.New(slog.NewTextHandler(logBuf, nil))
	srv, err := StartMetricsServer("127.0.0.1:0",
		stubProvider{StatsSnapshot{ScrapesOK: 42}}, "vtest", logger)
	if err != nil {
		t.Fatalf("StartMetricsServer: %v", err)
	}
	defer srv.Stop()

	addr := srv.Addr()
	if addr == "" {
		t.Fatal("Addr returned empty after Start")
	}

	// Scrape /metrics
	resp, err := http.Get("http://" + addr + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !strings.Contains(string(body), "vigil_agent_scrapes_ok_total 42") {
		t.Errorf("body missing expected counter; got:\n%s", body)
	}

	// /healthz
	hresp, err := http.Get("http://" + addr + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	hbody, _ := io.ReadAll(hresp.Body)
	_ = hresp.Body.Close()
	if hresp.StatusCode != 200 || string(hbody) != "ok\n" {
		t.Errorf("/healthz status=%d body=%q", hresp.StatusCode, hbody)
	}
}

func TestStartMetricsServer_BindError(t *testing.T) {
	// Reserve a port, then try to start a second server on the same
	// address. The second Start MUST return a non-nil error so main
	// can surface it as a config exit code.
	first, err := StartMetricsServer("127.0.0.1:0", stubProvider{}, "v", nil)
	if err != nil {
		t.Fatalf("first StartMetricsServer: %v", err)
	}
	defer first.Stop()

	if _, err := StartMetricsServer(first.Addr(), stubProvider{}, "v", nil); err == nil {
		t.Errorf("second Start on same addr returned nil; want bind error")
	}
}

func TestEscapeLabelValue(t *testing.T) {
	// Three characters need escaping per the Prom text format spec.
	// Forgetting any of them produces output that breaks parsers.
	got := escapeLabelValue(`hello "world"` + "\n" + `back\slash`)
	want := `hello \"world\"\nback\\slash`
	if got != want {
		t.Errorf("escapeLabelValue = %q want %q", got, want)
	}
}
