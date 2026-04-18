package updater

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// newTestLogger returns a slog.Logger writing to the given buffer in JSON
// format, so individual events can be parsed back and asserted on.
func newTestLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// findEvent returns the first JSON log line whose `event` field matches
// `want`, or nil if none. Lets tests assert "the update.available line
// fired with these fields" without caring about the order or count of
// debug noise around it.
func findEvent(t *testing.T, buf *bytes.Buffer, want string) map[string]any {
	t.Helper()
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("unmarshal log line %q: %v", line, err)
		}
		if ev, _ := m["event"].(string); ev == want {
			return m
		}
	}
	return nil
}

func mustJSON(t *testing.T, w http.ResponseWriter, body any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(body); err != nil {
		t.Fatalf("encode response: %v", err)
	}
}

// Tests both branches of New(): valid running version sets canCheck=true,
// invalid version disables the check silently. We deliberately don't
// fail-loud on bad CurrentVersion — see updater.go for the rationale.
func TestNew_DisablesOnUnparseableCurrentVersion(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	u, err := New(Options{
		CurrentVersion: "dev",
		URL:            "https://example.invalid/latest_version",
		Logger:         newTestLogger(&buf),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if u.canCheck {
		t.Fatalf("New(dev): canCheck=true, want false")
	}

	// The disable event is logged at debug, so it must be in the buffer
	// thanks to LevelDebug above.
	if findEvent(t, &buf, "update.disabled") == nil {
		t.Fatalf("expected update.disabled event in logs, got: %s", buf.String())
	}
}

func TestNew_RequiresURL(t *testing.T) {
	t.Parallel()

	if _, err := New(Options{CurrentVersion: "v0.1.0"}); err == nil {
		t.Fatalf("New: want error for empty URL, got nil")
	}
}

// Happy path: server returns a newer version → updater logs update.available
// exactly once, then a second call with the SAME server response stays quiet.
func TestCheckOnce_LogsUpdateAvailableThenSuppresses(t *testing.T) {
	t.Parallel()

	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		mustJSON(t, w, LatestRelease{
			Version:     "v0.1.3",
			ReleasedAt:  "2026-04-15T08:00:00Z",
			DownloadURL: "https://github.com/Seppia-AI/vigil-agent/releases/tag/v0.1.3",
		})
	}))
	defer srv.Close()

	var buf bytes.Buffer
	u, err := New(Options{
		CurrentVersion: "v0.1.0",
		URL:            srv.URL,
		Logger:         newTestLogger(&buf),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	u.checkOnce(ctx)
	first := findEvent(t, &buf, "update.available")
	if first == nil {
		t.Fatalf("expected update.available on first check, got: %s", buf.String())
	}
	if got := first["latest_version"]; got != "v0.1.3" {
		t.Fatalf("update.available latest_version=%v want v0.1.3", got)
	}
	if got := first["running_version"]; got != "v0.1.0" {
		t.Fatalf("update.available running_version=%v want v0.1.0", got)
	}

	// Reset buffer and call again; the suppression flag should keep the
	// line out of the log (still hits the server, that's fine — we only
	// de-dupe on operator-visible events).
	buf.Reset()
	u.checkOnce(ctx)
	if got := findEvent(t, &buf, "update.available"); got != nil {
		t.Fatalf("update.available should NOT re-fire for the same latest version, got: %s", buf.String())
	}

	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Fatalf("server hits = %d, want 2 (de-dupe must be log-side, not network-side)", got)
	}
}

// up-to-date path: server returns the SAME version we're running →
// no update.available, just a debug update.up_to_date.
func TestCheckOnce_NoEventWhenUpToDate(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mustJSON(t, w, LatestRelease{Version: "v0.1.0"})
	}))
	defer srv.Close()

	var buf bytes.Buffer
	u, err := New(Options{
		CurrentVersion: "v0.1.0",
		URL:            srv.URL,
		Logger:         newTestLogger(&buf),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	u.checkOnce(context.Background())

	if got := findEvent(t, &buf, "update.available"); got != nil {
		t.Fatalf("update.available should NOT fire when up to date, got: %s", buf.String())
	}
	if got := findEvent(t, &buf, "update.up_to_date"); got == nil {
		t.Fatalf("expected debug update.up_to_date event, got: %s", buf.String())
	}
}

// Server unhappy path: 503 → no panic, no update.available, just a
// debug update.check_failed line.
func TestCheckOnce_SilentOnUpstream503(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"upstream_unavailable"}`))
	}))
	defer srv.Close()

	var buf bytes.Buffer
	u, err := New(Options{
		CurrentVersion: "v0.1.0",
		URL:            srv.URL,
		Logger:         newTestLogger(&buf),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	u.checkOnce(context.Background())

	if got := findEvent(t, &buf, "update.available"); got != nil {
		t.Fatalf("update.available should NOT fire on 503, got: %s", buf.String())
	}
	if got := findEvent(t, &buf, "update.check_failed"); got == nil {
		t.Fatalf("expected debug update.check_failed event on 503, got: %s", buf.String())
	}
}

// Run respects ctx cancellation immediately during the initial-delay
// sleep. The test sets a long initial delay then cancels — Run should
// return within a few hundred ms.
func TestRun_CancellationDuringInitialDelay(t *testing.T) {
	t.Parallel()

	u, err := New(Options{
		CurrentVersion: "v0.1.0",
		URL:            "https://example.invalid/latest_version",
		InitialDelay:   1 * time.Hour, // we'll cancel long before this fires
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- u.Run(ctx) }()

	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned error: %v", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatalf("Run did not return within 500ms of ctx cancellation")
	}
}

// Run returns immediately on a "dev" build (canCheck=false) once ctx is
// cancelled — making sure the early <-ctx.Done() branch wires up.
func TestRun_DisabledForDevBuild(t *testing.T) {
	t.Parallel()

	u, err := New(Options{
		CurrentVersion: "dev",
		URL:            "https://example.invalid/latest_version",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if u.canCheck {
		t.Fatalf("dev build should disable the check")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	if err := u.Run(ctx); err != nil {
		t.Fatalf("Run(ctx): %v", err)
	}
}
