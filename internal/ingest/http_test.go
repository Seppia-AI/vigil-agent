package ingest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Seppia-AI/vigil-agent/internal/collector"
	"github.com/Seppia-AI/vigil-agent/internal/scheduler"
)

// testBatch returns a small deterministic Batch for ingest tests. Using
// the real struct (not a hand-rolled JSON literal) catches field-name
// drift between collector.Batch and the wire encoding we send.
func testBatch() collector.Batch {
	return collector.Batch{
		Ts:           time.Unix(1_700_000_000, 0).UTC(),
		Hostname:     "h1",
		OS:           "linux",
		Arch:         "amd64",
		AgentVersion: "test",
		Metrics: []collector.Sample{
			{Name: "cpu.usage", Value: 12.5, Labels: map[string]string{"core": "0"}},
			{Name: "mem.used", Value: 4096},
		},
	}
}

// mustNew builds an HTTPSink against `ts.URL` with the common test
// options and fails the test on construction error. Keeps each case
// focused on Send behaviour rather than boilerplate.
func mustNew(t *testing.T, ts *httptest.Server) (*HTTPSink, *bytes.Buffer) {
	t.Helper()
	logBuf := &bytes.Buffer{}
	logger := slog.New(slog.NewTextHandler(logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	s, err := New(Options{
		IngestURL:    ts.URL,
		Token:        "tok-abc",
		Logger:       logger,
		AgentVersion: "test",
		Timeout:      2 * time.Second,
	})
	if err != nil {
		t.Fatalf("ingest.New: %v", err)
	}
	return s, logBuf
}

func TestNew_Validates(t *testing.T) {
	// URL validation should reject empty token, bad scheme, missing
	// host, and non-empty path — construction-time errors, not
	// runtime surprises halfway through a deploy.
	cases := []struct {
		name    string
		opts    Options
		wantErr string
	}{
		{"empty token", Options{IngestURL: "https://example.com"}, "token required"},
		// Parse on "" returns Scheme="" which hits the scheme check
		// before we see the empty-host branch — either error is fine
		// for the caller, so assert on whichever fires first.
		{"empty url", Options{Token: "x"}, "http or https"},
		{"bad scheme", Options{IngestURL: "ftp://example.com", Token: "x"}, "http or https"},
		{"with path", Options{IngestURL: "https://example.com/api", Token: "x"}, "must not include a path"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := New(c.opts)
			if err == nil {
				t.Fatalf("want error containing %q, got nil", c.wantErr)
			}
			if !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("err=%q want substring %q", err.Error(), c.wantErr)
			}
		})
	}
}

func TestNew_BuildsURLAndUA(t *testing.T) {
	// Token is percent-escaped into the path; User-Agent reflects
	// the supplied AgentVersion. Both matter for server log
	// correlation.
	var got *http.Request
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"received":true,"count":2}`))
	}))
	defer ts.Close()

	s, err := New(Options{IngestURL: ts.URL, Token: "a b/c", AgentVersion: "1.2.3"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Send(context.Background(), testBatch()); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if got == nil {
		t.Fatal("server did not receive request")
	}
	if want := "/vigil/vitals/a%20b%2Fc"; got.URL.Path != want {
		t.Errorf("path=%q want %q", got.URL.Path, want)
	}
	if ua := got.Header.Get("User-Agent"); !strings.Contains(ua, "vigil-agent/1.2.3") {
		t.Errorf("User-Agent=%q missing 'vigil-agent/1.2.3'", ua)
	}
	if ct := got.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type=%q want application/json", ct)
	}
}

func TestSend_OK_NoDroppedQuota(t *testing.T) {
	// Happy path: 200 with received=true, dropped_quota=0 → nil error,
	// SendResult zero. Also sanity-check that the JSON body the
	// server receives round-trips back to the same Batch.
	var bodyBytes []byte
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bodyBytes, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"received":true,"count":2,"dropped_quota":0}`))
	}))
	defer ts.Close()

	s, _ := mustNew(t, ts)
	res, err := s.Send(context.Background(), testBatch())
	if err != nil {
		t.Fatalf("Send err=%v", err)
	}
	if res.DroppedQuota != 0 {
		t.Errorf("DroppedQuota=%d want 0", res.DroppedQuota)
	}
	// Round-trip check: decode what the server received and look for
	// one of our metric names. We don't compare the whole struct
	// because time.Time encoding precision makes that fragile.
	var round collector.Batch
	if err := json.Unmarshal(bodyBytes, &round); err != nil {
		t.Fatalf("server-side decode: %v; body=%s", err, bodyBytes)
	}
	if len(round.Metrics) != 2 {
		t.Errorf("metrics=%d want 2", len(round.Metrics))
	}
}

func TestSend_OK_WithDroppedQuota(t *testing.T) {
	// 200 with dropped_quota>0 → nil error (server accepted the
	// batch), SendResult carries the count so the scheduler can
	// engage its rate-halving throttle.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"received":true,"count":2,"dropped_quota":17}`))
	}))
	defer ts.Close()

	s, _ := mustNew(t, ts)
	res, err := s.Send(context.Background(), testBatch())
	if err != nil {
		t.Fatalf("Send err=%v", err)
	}
	if res.DroppedQuota != 17 {
		t.Errorf("DroppedQuota=%d want 17", res.DroppedQuota)
	}
}

func TestSend_OK_EmptyBodyIsTolerated(t *testing.T) {
	// Some reverse proxies (looking at you, certain load balancers)
	// strip 200 response bodies. The sink must treat that as a
	// clean success, not a sink error.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
	}))
	defer ts.Close()

	s, _ := mustNew(t, ts)
	res, err := s.Send(context.Background(), testBatch())
	if err != nil {
		t.Fatalf("Send err=%v", err)
	}
	if res.DroppedQuota != 0 {
		t.Errorf("DroppedQuota=%d want 0", res.DroppedQuota)
	}
}

func TestSend_404IsFatal(t *testing.T) {
	// 404 = token revoked / probe deleted. The scheduler detects
	// this via errors.Is(err, scheduler.ErrFatal) and terminates
	// the agent with a non-zero exit.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(404)
		_, _ = w.Write([]byte(`{"error":"probe not found"}`))
	}))
	defer ts.Close()

	s, _ := mustNew(t, ts)
	_, err := s.Send(context.Background(), testBatch())
	if err == nil {
		t.Fatal("Send returned nil; want fatal error")
	}
	if !errors.Is(err, scheduler.ErrFatal) {
		t.Errorf("err=%v not ErrFatal", err)
	}
	if !strings.Contains(err.Error(), "probe not found") {
		t.Errorf("err=%v should include response body", err)
	}
}

func TestSend_429WithRetryAfterSeconds(t *testing.T) {
	// 429 with "Retry-After: 7" → RetryAfterError{Delay: 7s}.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(429)
	}))
	defer ts.Close()

	s, _ := mustNew(t, ts)
	_, err := s.Send(context.Background(), testBatch())
	if err == nil {
		t.Fatal("Send returned nil; want RetryAfterError")
	}
	var ra *scheduler.RetryAfterError
	if !errors.As(err, &ra) {
		t.Fatalf("err=%v not *RetryAfterError", err)
	}
	if ra.Delay != 7*time.Second {
		t.Errorf("Delay=%s want 7s", ra.Delay)
	}
}

func TestSend_429WithRetryAfterHTTPDate(t *testing.T) {
	// 429 with an HTTP-date Retry-After → parse and return the
	// delta. Using a future date 2s ahead of the server's clock.
	target := time.Now().Add(2 * time.Second).UTC().Format(http.TimeFormat)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", target)
		w.WriteHeader(429)
	}))
	defer ts.Close()

	s, _ := mustNew(t, ts)
	_, err := s.Send(context.Background(), testBatch())
	var ra *scheduler.RetryAfterError
	if !errors.As(err, &ra) {
		t.Fatalf("err=%v not *RetryAfterError", err)
	}
	// HTTP-date precision is 1s so we accept [0, 3s].
	if ra.Delay < 0 || ra.Delay > 3*time.Second {
		t.Errorf("Delay=%s outside [0,3s]", ra.Delay)
	}
}

func TestSend_429NoHeaderFallsBack(t *testing.T) {
	// 429 with NO Retry-After → default to 5s so we back off
	// sensibly on CDNs that rate-limit without hinting.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(429)
	}))
	defer ts.Close()

	s, _ := mustNew(t, ts)
	_, err := s.Send(context.Background(), testBatch())
	var ra *scheduler.RetryAfterError
	if !errors.As(err, &ra) {
		t.Fatalf("err=%v not *RetryAfterError", err)
	}
	if ra.Delay != 5*time.Second {
		t.Errorf("Delay=%s want 5s default", ra.Delay)
	}
}

func TestSend_5xxIsRetriable(t *testing.T) {
	// 500/502/503/504 → plain retriable error (NOT ErrFatal, NOT
	// RetryAfterError). Scheduler applies exponential back-off.
	for _, code := range []int{500, 502, 503, 504} {
		t.Run(fmt.Sprintf("status_%d", code), func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(code)
				_, _ = w.Write([]byte("oops"))
			}))
			defer ts.Close()

			s, _ := mustNew(t, ts)
			_, err := s.Send(context.Background(), testBatch())
			if err == nil {
				t.Fatalf("Send returned nil; want retriable error for %d", code)
			}
			if errors.Is(err, scheduler.ErrFatal) {
				t.Errorf("err=%v is ErrFatal; want retriable for %d", err, code)
			}
			var ra *scheduler.RetryAfterError
			if errors.As(err, &ra) {
				t.Errorf("err=%v is RetryAfterError; want plain for %d", err, code)
			}
		})
	}
}

func TestSend_Other4xxDropsBatch(t *testing.T) {
	// 400/413/415 → return nil (drop the batch) and log. Retrying
	// would loop forever on the same malformed data.
	for _, code := range []int{400, 413, 415, 422} {
		t.Run(fmt.Sprintf("status_%d", code), func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(code)
				_, _ = w.Write([]byte(fmt.Sprintf(`{"error":"bad %d"}`, code)))
			}))
			defer ts.Close()

			s, logBuf := mustNew(t, ts)
			res, err := s.Send(context.Background(), testBatch())
			if err != nil {
				t.Fatalf("Send err=%v; want nil so scheduler drops the batch", err)
			}
			if res.DroppedQuota != 0 {
				t.Errorf("DroppedQuota=%d want 0", res.DroppedQuota)
			}
			// slog text format prints msg="…" then key=value pairs,
			// so we assert both the action ("dropping batch") AND
			// the status attribute, rather than a one-shot string.
			if !strings.Contains(logBuf.String(), "dropping batch") ||
				!strings.Contains(logBuf.String(), fmt.Sprintf("status=%d", code)) {
				t.Errorf("log missing 'dropping batch'/status=%d for %d; got:\n%s", code, code, logBuf.String())
			}
		})
	}
}

func TestSend_NetworkErrorIsRetriable(t *testing.T) {
	// Server closed before we could POST → net/http returns a
	// connection error. Must be retriable (plain error, not fatal).
	ts := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	ts.Close() // close immediately so the next POST fails
	s, _ := mustNew(t, ts)
	_, err := s.Send(context.Background(), testBatch())
	if err == nil {
		t.Fatal("Send returned nil; want network error")
	}
	if errors.Is(err, scheduler.ErrFatal) {
		t.Errorf("err=%v is ErrFatal; want retriable", err)
	}
}

func TestSend_RespectsContextCancellation(t *testing.T) {
	// A cancelled context should abort the in-flight POST promptly.
	// We use a server that sleeps longer than the context timeout
	// and assert Send returns within a short window.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(5 * time.Second):
			w.WriteHeader(200)
		case <-r.Context().Done():
			// client went away
		}
	}))
	defer ts.Close()

	s, _ := mustNew(t, ts)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := s.Send(ctx, testBatch())
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("Send returned nil; want cancellation error")
	}
	if elapsed > 500*time.Millisecond {
		t.Errorf("Send took %s; want <500ms (context should have cancelled)", elapsed)
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 4, 16, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		in   string
		want time.Duration
	}{
		{"", 0},
		{"  ", 0},
		{"0", 0},
		{"-5", 0},
		{"3", 3 * time.Second},
		{"120", 2 * time.Minute},
		{"not-a-number", 0},
		{now.Add(10 * time.Second).Format(http.TimeFormat), 10 * time.Second},
		{now.Add(-10 * time.Second).Format(http.TimeFormat), 0}, // past
	}
	for _, c := range cases {
		if got := parseRetryAfter(c.in, now); got != c.want {
			t.Errorf("parseRetryAfter(%q) = %s want %s", c.in, got, c.want)
		}
	}
}
