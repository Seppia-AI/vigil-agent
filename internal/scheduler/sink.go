package scheduler

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/Seppia-AI/vigil-agent/internal/collector"
)

// Sink is the contract the scheduler uses to ship a single Batch
// somewhere. See internal/ingest for the production HTTP implementation
// and this file for the LogSink placeholder used by `--dry-run`.
//
// The interface deliberately takes ONE batch at a time rather than a
// slice. That keeps retry semantics simple — the sender peeks the head
// of the buffer, calls Send, and on success Pops; on failure it leaves
// the batch in place and applies back-off. A multi-batch Send would
// have to report partial success (which N succeeded?) and complicate
// every implementation.
//
// Error semantics — READ CAREFULLY before implementing a new Sink:
//
//   - Return nil for any outcome that means "the server has accepted
//     responsibility for this batch and the agent must move on",
//     INCLUDING 4xx-other which drops the batch on the floor. The
//     scheduler pops on nil; a retry-forever loop on something the
//     server rejects would be worse than losing one batch.
//
//   - Return an error wrapping ErrFatal for unrecoverable problems
//     (token revoked, probe deleted). The scheduler will STOP THE
//     AGENT and propagate a non-zero exit so systemd shows red. Never
//     wrap ErrFatal around something a human could fix at runtime —
//     e.g. a DNS blip should be a plain error, not fatal.
//
//   - Return a *RetryAfterError for 429 / server-rate-limit responses
//     so the scheduler waits at least the server-requested duration
//     before retrying instead of its own exponential back-off.
//
//   - Return any other non-nil error for retriable failures (5xx,
//     network errors, timeouts). The scheduler applies exponential
//     back-off with full jitter, capped at maxSendBackoff.
type Sink interface {
	Send(ctx context.Context, batch collector.Batch) (SendResult, error)
}

// SendResult carries per-send information back from the sink so the
// scheduler can act on server-side signals that aren't errors. A "result"
// struct rather than an extra return argument leaves room to grow
// without changing the Sink interface signature.
//
// The scheduler itself only consumes DroppedQuota; the other fields
// are populated by HTTPSink for callers that want to surface the full
// server breakdown (today: `vigil-agent --once --send`).
type SendResult struct {
	// Count is the number of samples the server reported as accepted
	// (i.e. stored). Always <= the number of samples in the batch we
	// sent; the difference is captured in the Dropped* / Stripped*
	// fields below.
	Count int

	// DroppedQuota is the number of samples the server dropped because
	// the per-minute sample cap was exceeded. Non-zero means "slow
	// down" — the scheduler halves
	// the effective scrape rate for one minute so the backlog has
	// time to clear and the agent doesn't hammer against the cap.
	DroppedQuota int

	// DroppedUnsupported is the number of samples the server dropped
	// because the metric name is not in its allowlist (e.g. a custom
	// metric name on a plan that doesn't permit them). Informational
	// only from the scheduler's perspective; the verify path prints it.
	DroppedUnsupported int

	// DroppedCardinality is the number of samples the server dropped
	// because the per-probe series-cardinality cap was hit. Informational
	// only from the scheduler's perspective.
	DroppedCardinality int

	// StrippedLabels is the number of label keys the server removed
	// from accepted samples (high-cardinality keys like request_id).
	// The samples themselves were stored; only the offending labels
	// were dropped. Informational only.
	StrippedLabels int
}

// ErrFatal is a sentinel meaning "do not retry, terminate the agent".
// Wrap it with %w when returning from Sink.Send. Exactly one response
// code — 404 Not Found — currently produces this: it means the token
// is revoked or the probe has been deleted, and no amount of waiting
// will make it valid again.
//
//	return scheduler.SendResult{}, fmt.Errorf("%w: token invalid", scheduler.ErrFatal)
//
// The scheduler uses errors.Is(err, ErrFatal) to detect it.
var ErrFatal = errors.New("fatal ingest error")

// RetryAfterError wraps a retriable failure and tells the scheduler
// "wait AT LEAST this long before retrying". Returned by the HTTP sink
// on 429 responses, using the Retry-After header value. If the server
// gives us nothing, Delay will be minSendBackoff.
//
// The scheduler treats the server's hint as a floor, not a ceiling —
// if the next exponential-backoff step is longer than Delay, we use
// the exponential one. That way a misconfigured server that replies
// "Retry-After: 1" during a real outage doesn't DoS us.
type RetryAfterError struct {
	Delay time.Duration
	Cause error
}

func (e *RetryAfterError) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("retry after %s: %v", e.Delay, e.Cause)
	}
	return fmt.Sprintf("retry after %s", e.Delay)
}

// Unwrap lets errors.Is/As work transparently so callers can still
// detect the underlying cause (e.g. is the 429 actually a "quota
// exhausted" vs a generic rate-limit).
func (e *RetryAfterError) Unwrap() error { return e.Cause }

// LogSink is a Sink that just writes a one-line human-readable summary
// of each batch to the given writer. It always succeeds (returns nil),
// so the scheduler will Pop after every Send — i.e. it never builds up
// a backlog.
//
// This exists for two reasons:
//
//  1. It lets the scheduler/buffer be exercised in isolation (in tests
//     and during development) without the agent ever opening a socket.
//
//  2. `vigil-agent --dry-run` uses LogSink instead of HTTPSink for
//     debugging config + labels on a host that can't reach the ingest
//     server (air-gapped test box, CI runner, offline demo).
//
// The format is deliberately terse — one line per scrape — because the
// daemon emits one of these every `scrape_interval_s` seconds and a
// long-lived agent would otherwise spam logs.
type LogSink struct {
	w io.Writer
}

// NewLogSink returns a Sink that writes summary lines to w.
func NewLogSink(w io.Writer) *LogSink { return &LogSink{w: w} }

// Send implements Sink. Always returns SendResult{}, nil — see the
// LogSink doc-comment for why a logging sink must never look like a
// retriable failure.
func (l *LogSink) Send(_ context.Context, batch collector.Batch) (SendResult, error) {
	// A typical line:
	//   [vigil-agent] flush ts=2026-04-18T13:39:13Z host=web-01 metrics=142
	fmt.Fprintf(l.w, "[vigil-agent] flush ts=%s host=%s metrics=%d\n",
		batch.Ts.UTC().Format("2006-01-02T15:04:05Z"),
		batch.Hostname,
		len(batch.Metrics),
	)
	return SendResult{}, nil
}
