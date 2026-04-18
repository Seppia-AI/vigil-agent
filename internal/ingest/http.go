// Package ingest contains the HTTP sink that ships collector.Batch
// values to the Seppia Vigil Vitals ingest endpoint.
//
// Protocol: POST {ingest_url}/vigil/vitals/{token}
//
// Request body: the JSON-encoded collector.Batch (see Batch doc-comment
// for the wire shape). Content-Type is application/json.
//
// Response: 200 with JSON body  { received: bool, count: int,
//   dropped_quota?: int, dropped_unsupported?: int, dropped_cardinality?: int,
//   stripped_labels?: int }
// The agent currently only acts on `dropped_quota`; the other counters
// are logged at debug level for operator visibility.
//
// Status code handling:
//
//	200 → success. If body.dropped_quota > 0 → engage server-side
//	      throttle (scheduler halves the rate for 1 min).
//	404 → token revoked / probe deleted → ErrFatal. Scheduler stops
//	      the agent. Do not retry.
//	429 → RetryAfterError with the server's Retry-After header (or
//	      a sensible default if the header is missing/unparseable).
//	5xx + network/timeout → plain error → scheduler exponential
//	      back-off (min 1s, max 5min).
//	Other 4xx → log the body and return nil. The batch is dropped
//	      and the scheduler moves on. Retrying a 400 would loop
//	      forever on the same bad data.
package ingest

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/Seppia-AI/vigil-agent/internal/collector"
	"github.com/Seppia-AI/vigil-agent/internal/scheduler"
)

// DefaultHTTPTimeout bounds a single POST. Deliberately longer than the
// server's expected response time (sub-200ms on a healthy path) so a
// briefly-overloaded server isn't mis-classified as a network error.
// Shorter than the default scrape interval (60s) so a stuck POST can't
// delay the next scrape indefinitely.
const DefaultHTTPTimeout = 30 * time.Second

// maxBodyReadBytes caps how much of an error response body we read
// before giving up. The server returns small JSON objects; a 1 MiB
// limit is plenty and protects us from a misconfigured reverse proxy
// that returns an HTML error page the size of a PDF.
const maxBodyReadBytes = 1 << 20 // 1 MiB

// HTTPSink is the production scheduler.Sink implementation. Construct
// with New, pass to scheduler.Options.Sink.
type HTTPSink struct {
	// endpoint is the FULL URL including /vigil/vitals/<token>, built
	// once at construction so hot-path Send calls don't re-parse the
	// base URL. The token is baked in; don't log this string as-is.
	endpoint string

	client    *http.Client
	userAgent string

	// log is where the sink writes status-code details for operators:
	// the body of a dropped 4xx, the Retry-After of a 429 we couldn't
	// parse, etc. Separate from the scheduler log because the sink may
	// be used in isolation (e.g. a future CLI "send one batch" tool).
	log *slog.Logger
}

// Options configures an HTTPSink. IngestURL and Token are required.
type Options struct {
	// IngestURL is the base URL, e.g. "https://api.seppia.ai". Agent
	// appends "/vigil/vitals/<token>". Must not include a path.
	IngestURL string

	// Token is the probe ingest secret. Baked into the URL path and
	// never sent in a header.
	Token string

	// Timeout is the single-request deadline. Zero = DefaultHTTPTimeout.
	Timeout time.Duration

	// Insecure disables TLS certificate verification. SET ONLY FOR
	// LOCAL DEV against a self-signed ingest. Production MUST never
	// run with this — the token would be interceptible.
	Insecure bool

	// UserAgent overrides the default "vigil-agent/<ver> (<os>/<arch>)"
	// UA. Tests use this to distinguish test runs in server logs.
	UserAgent string

	// Logger receives per-response operator-facing log records. Nil
	// is silent; tests pass a *slog.Logger that writes to a buffer.
	// Event keys are stable: ingest.4xx_drop, ingest.marshal_error,
	// ingest.body_invalid, ingest.server_drop.
	Logger *slog.Logger

	// AgentVersion is stitched into the default User-Agent. Caller
	// passes internal/version.Version so the sink doesn't need to
	// import that package.
	AgentVersion string
}

// New builds an HTTPSink. Returns an error for invalid URLs (empty,
// unparseable, wrong scheme, or a URL that already contains a path —
// we refuse to guess how to merge it with "/vigil/vitals/<token>").
func New(opts Options) (*HTTPSink, error) {
	if strings.TrimSpace(opts.Token) == "" {
		return nil, errors.New("ingest: token required")
	}
	u, err := url.Parse(opts.IngestURL)
	if err != nil {
		return nil, fmt.Errorf("ingest: parse URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("ingest: URL must be http or https (got %q)", u.Scheme)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("ingest: URL missing host: %q", opts.IngestURL)
	}
	// Allow a trailing slash on the base URL but no real path — we
	// own the URL shape from here on. This matches the validation
	// already done in internal/config.validateURL.
	trimmed := strings.TrimRight(u.Path, "/")
	if trimmed != "" {
		return nil, fmt.Errorf("ingest: base URL must not include a path (got %q)", u.Path)
	}
	u.Path = "/vigil/vitals/" + url.PathEscape(opts.Token)

	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultHTTPTimeout
	}

	tr := http.DefaultTransport.(*http.Transport).Clone()
	if opts.Insecure {
		// Single switch to make it obvious in code review where
		// certificate validation is being disabled.
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // deliberate opt-in via --insecure
	}

	ua := opts.UserAgent
	if ua == "" {
		ver := opts.AgentVersion
		if ver == "" {
			ver = "dev"
		}
		ua = fmt.Sprintf("vigil-agent/%s (%s/%s)", ver, runtime.GOOS, runtime.GOARCH)
	}

	log := opts.Logger
	if log == nil {
		// Same dance as scheduler: discard logger so tests don't
		// have to plumb one through. Cheap.
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	return &HTTPSink{
		endpoint: u.String(),
		client: &http.Client{
			Transport: tr,
			Timeout:   timeout,
		},
		userAgent: ua,
		log:       log,
	}, nil
}

// ingestResponse mirrors the JSON shape of the server's 200 body. Only
// dropped_quota influences agent behaviour (engages throttle); the
// other counters are logged at WARN with event=ingest.server_drop for
// operator visibility but don't change scheduler decisions.
type ingestResponse struct {
	Received           bool `json:"received"`
	Count              int  `json:"count"`
	DroppedQuota       int  `json:"dropped_quota"`
	DroppedUnsupported int  `json:"dropped_unsupported"`
	DroppedCardinality int  `json:"dropped_cardinality"`
	StrippedLabels     int  `json:"stripped_labels"`
}

// Send implements scheduler.Sink. See the package doc-comment for the
// full status-code table.
func (h *HTTPSink) Send(ctx context.Context, batch collector.Batch) (scheduler.SendResult, error) {
	// Marshal first so we can bail WITHOUT touching the network on a
	// malformed batch. A serialisation error is a programmer bug
	// (Batch fields are always safe to marshal) — we drop the batch
	// and log loudly so it shows up once in the journal.
	body, err := json.Marshal(batch)
	if err != nil {
		h.log.Error("HTTPSink: marshal error, dropping batch",
			slog.String("event", "ingest.marshal_error"),
			slog.String("error", err.Error()),
		)
		return scheduler.SendResult{}, nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.endpoint, bytes.NewReader(body))
	if err != nil {
		// URL was validated at New() time; this path is
		// essentially unreachable. Keep it as a plain error so
		// the scheduler treats it as retriable — the next tick
		// might succeed if something external rigged the URL.
		return scheduler.SendResult{}, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", h.userAgent)
	// http.Client's default keep-alive is fine here: reusing the TLS
	// session improves latency on slow links and the reconnection cost
	// at minute-cadence is negligible.

	resp, err := h.client.Do(req)
	if err != nil {
		// Network error or context cancellation. Both are
		// retriable from the sink's perspective — the scheduler
		// applies back-off; a cancelled context would also
		// short-circuit the sleep that follows.
		return scheduler.SendResult{}, fmt.Errorf("POST %s: %w", h.endpoint, err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxBodyReadBytes))

	switch {
	case resp.StatusCode == http.StatusOK:
		return h.handleOK(respBody)

	case resp.StatusCode == http.StatusNotFound:
		// Fatal: token is wrong or the probe was deleted. An agent
		// with a revoked token can never succeed, so we fail loudly
		// rather than retry-forever-silently.
		snippet := truncate(string(respBody), 256)
		return scheduler.SendResult{}, fmt.Errorf(
			"%w: 404 from ingest (token revoked or probe deleted): %s",
			scheduler.ErrFatal, snippet,
		)

	case resp.StatusCode == http.StatusTooManyRequests:
		return h.handleTooMany(resp)

	case resp.StatusCode >= 500:
		// Retriable server-side failure. Could be a real outage,
		// a deploy, or an overloaded queue. All three recover
		// on their own; the scheduler's exponential back-off
		// gives the server breathing room.
		snippet := truncate(string(respBody), 256)
		return scheduler.SendResult{}, fmt.Errorf("server error %d: %s", resp.StatusCode, snippet)

	case resp.StatusCode >= 400:
		// Other 4xx (400 bad JSON, 413 payload too large, 415
		// unsupported media type, …). Retrying the same batch
		// would fail the same way — drop it on the floor and
		// move on. Log at enough detail that the operator can
		// file a bug if this wasn't self-inflicted.
		snippet := truncate(string(respBody), 512)
		h.log.Warn("HTTPSink: dropping batch (no retry)",
			slog.String("event", "ingest.4xx_drop"),
			slog.Int("status", resp.StatusCode),
			slog.String("body", snippet),
		)
		return scheduler.SendResult{}, nil

	default:
		// 1xx, 2xx-other, 3xx. 2xx-other (e.g. 201) probably
		// means the server accepted the data; treat it as success
		// with no further signal. 3xx in a programmatic context
		// is user error — we won't follow redirects for a POST.
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return scheduler.SendResult{}, nil
		}
		snippet := truncate(string(respBody), 256)
		return scheduler.SendResult{}, fmt.Errorf("unexpected status %d: %s", resp.StatusCode, snippet)
	}
}

// handleOK decodes a 200 body and extracts the signals the scheduler
// cares about. A missing/unparseable body is NOT an error — the server
// accepted the data (status 200 said so), we just can't glean any
// refinements from it. Returning (zero-result, nil) means "ship it".
func (h *HTTPSink) handleOK(body []byte) (scheduler.SendResult, error) {
	var ir ingestResponse
	// Tolerate an empty body (some reverse proxies strip it) by not
	// treating unmarshal errors as send failures.
	if len(bytes.TrimSpace(body)) > 0 {
		if err := json.Unmarshal(body, &ir); err != nil {
			h.log.Warn("HTTPSink: 200 body not JSON",
				slog.String("event", "ingest.body_invalid"),
				slog.String("error", err.Error()),
			)
			return scheduler.SendResult{}, nil
		}
	}
	if ir.DroppedUnsupported > 0 || ir.DroppedCardinality > 0 || ir.StrippedLabels > 0 {
		// Operator-fixable but not urgent. Logged at WARN so a
		// dashboard built on `event=ingest.server_drop` counts can
		// alert; not promoted to its own Prom counter yet (would
		// need per-reason buckets).
		h.log.Warn("HTTPSink: server dropped samples",
			slog.String("event", "ingest.server_drop"),
			slog.Int("unsupported", ir.DroppedUnsupported),
			slog.Int("cardinality", ir.DroppedCardinality),
			slog.Int("stripped_labels", ir.StrippedLabels),
		)
	}
	return scheduler.SendResult{DroppedQuota: ir.DroppedQuota}, nil
}

// handleTooMany parses Retry-After from a 429 response and returns a
// scheduler.RetryAfterError so the scheduler waits at least that long
// before retrying. Accepts both "delta-seconds" and HTTP-date forms
// per RFC 9110 §10.2.3. An unparseable header falls back to a sensible
// default (5s) rather than erroring — a 429 with no header at all is
// common from some CDNs.
func (h *HTTPSink) handleTooMany(resp *http.Response) (scheduler.SendResult, error) {
	delay := parseRetryAfter(resp.Header.Get("Retry-After"), time.Now())
	if delay <= 0 {
		delay = 5 * time.Second
	}
	return scheduler.SendResult{}, &scheduler.RetryAfterError{
		Delay: delay,
		Cause: fmt.Errorf("429 Too Many Requests"),
	}
}

// parseRetryAfter returns the duration encoded by a Retry-After header
// value. Supports:
//
//   - delta-seconds form, e.g. "120" → 120s
//   - HTTP-date form,    e.g. "Wed, 21 Oct 2026 07:28:00 GMT"
//
// Returns 0 on empty / unparseable input (caller substitutes its own
// default). Negative / past dates also return 0 — we don't do
// negative-delay retries.
func parseRetryAfter(v string, now time.Time) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs <= 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		d := t.Sub(now)
		if d <= 0 {
			return 0
		}
		return d
	}
	return 0
}

// truncate shortens s to at most n runes, appending "…" if it had to
// cut. Used for log snippets of oversized server responses — keeps
// journal lines reasonable while still showing enough context to
// diagnose "what did the server actually send back?".
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
