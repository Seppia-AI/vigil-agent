package scheduler

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Seppia-AI/vigil-agent/internal/collector"
)

// Default capacity multiplier for the in-memory buffer: 5× the scrape
// interval, expressed as a count of batches.
// In practice that means: at the default 60s scrape, the buffer holds
// 5 batches = 5 minutes of pending data before we start dropping the
// oldest. Long enough to weather a brief ingest blip; short enough that
// a wedged sender doesn't quietly accumulate stale data forever.
const DefaultBufferBatches = 5

// DefaultDrainTimeout caps the post-SIGTERM flush window. After the
// collect loop stops, the send loop is given this long to push whatever
// is still in the buffer before the agent exits.
//
// Sized against systemd's TimeoutStopSec: our packaged unit sets that
// to 10s, so 5s leaves a comfortable margin for an in-flight POST plus
// shutdown bookkeeping. If you raise this, raise TimeoutStopSec too —
// otherwise systemd will SIGKILL us mid-drain and the journal will lie
// about why.
const DefaultDrainTimeout = 5 * time.Second

// Back-off bounds for the send loop. Cheap on the happy path (no
// sleeping until the first error), bounded on the sad path so a
// long outage doesn't let the wakeup interval drift into "we noticed
// recovery 10 minutes late" territory.
//
// maxSendBackoff is 5 min: on 5xx / network errors we use exponential
// back-off with jitter, capped here. An agent behind a 6-hour outage
// wakes up every 5 minutes to poke the server — wasteful
// but bounded and quick to recover once the server is back.
const (
	minSendBackoff = 1 * time.Second
	maxSendBackoff = 5 * time.Minute
)

// Rate-halving ("throttle") parameters. When the server responds with
// a non-zero dropped_quota, the scheduler halves the effective scrape
// rate for throttleDuration. This gives the server's per-minute budget
// a chance to reset without the agent continuing to hammer against the
// cap.
//
// Implementation note: we "halve the rate" by skipping every other
// tick while throttled, not by calling ticker.Reset. That's simpler
// (no race on the ticker's internal state) and observably identical.
const throttleDuration = 1 * time.Minute

// Stats are the agent's own counters. They are exposed via the optional
// Prometheus endpoint (see internal/observ); they're tracked
// unconditionally because the cost is one atomic add per scrape and
// they're invaluable for debugging from logs.
//
// All fields are read/written via atomic ops — safe to inspect from any
// goroutine without locking.
type Stats struct {
	ScrapesOK           uint64
	ScrapesPartial      uint64 // scrape returned at least one collector error
	ScrapesEmpty        uint64 // scrape returned zero samples (skipped Push)
	SamplesCollected    uint64
	BatchesSent         uint64
	BatchesFailed       uint64 // sink returned non-nil; will be retried
	DroppedOverflow     uint64 // mirror of buffer.DroppedOverflow at last read
	DroppedQuotaSamples uint64 // sum of res.DroppedQuota across all successful Sends
	LastFlushUnix       int64  // wall-clock seconds of the last successful Send
}

// Snapshot returns a copy of the stats. Used by tests and (later) by
// the /metrics endpoint. Reads are atomic so the snapshot is internally
// consistent per-field, though not across fields — that's fine for
// monitoring purposes.
func (s *Stats) Snapshot() Stats {
	return Stats{
		ScrapesOK:           atomic.LoadUint64(&s.ScrapesOK),
		ScrapesPartial:      atomic.LoadUint64(&s.ScrapesPartial),
		ScrapesEmpty:        atomic.LoadUint64(&s.ScrapesEmpty),
		SamplesCollected:    atomic.LoadUint64(&s.SamplesCollected),
		BatchesSent:         atomic.LoadUint64(&s.BatchesSent),
		BatchesFailed:       atomic.LoadUint64(&s.BatchesFailed),
		DroppedOverflow:     atomic.LoadUint64(&s.DroppedOverflow),
		DroppedQuotaSamples: atomic.LoadUint64(&s.DroppedQuotaSamples),
		LastFlushUnix:       atomic.LoadInt64(&s.LastFlushUnix),
	}
}

// Options configures a Scheduler. Only Interval, Registry, and Sink
// are required; the rest have sensible defaults.
type Options struct {
	// Interval between scrapes. Validated >= 1s by the caller (config
	// already enforces MinScrapeIntervalS).
	Interval time.Duration

	// Registry produces the per-scrape Batch. Required.
	Registry *collector.Registry

	// Sink ships each Batch. Required. See sink.go for the contract.
	Sink Sink

	// BufferBatches caps the in-memory queue. Defaults to
	// DefaultBufferBatches. Setting this very low (1-2) is fine for
	// tests; very high (>100) will allow surprisingly stale data
	// after a long outage.
	BufferBatches int

	// Logger receives structured operational events: startup banner,
	// overflow warnings, sink errors, throttle engagement, etc.
	// Defaults to a no-op logger so the scheduler is silent in tests
	// by default. Pass observ.NewLogger to get text/JSON output.
	//
	// Event keys are stable — see scheduler.go for the canonical list.
	// Operators may grep the journal for `event=sink.error`,
	// `event=buffer.overflow`, etc.
	Logger *slog.Logger

	// DrainTimeout bounds the post-SIGTERM drain phase: after the
	// collect loop stops, the send loop is given this long to flush
	// whatever is still in the buffer before the agent exits.
	// Zero or negative disables drain entirely (immediate exit; any
	// buffered batches are lost).
	//
	// Default is DefaultDrainTimeout (5s). 5s is the systemd
	// TimeoutStopSec default minus a small safety margin, so a slow
	// drain never causes systemd to escalate SIGTERM → SIGKILL on us.
	DrainTimeout time.Duration
}

// Scheduler runs the collect+send loops. Construct with New, start
// with Run (blocks until ctx is cancelled), inspect with Stats().
type Scheduler struct {
	interval     time.Duration
	drainTimeout time.Duration
	registry     *collector.Registry
	sink         Sink
	buf          *Buffer
	log          *slog.Logger
	stats        Stats

	// shutdownSend is closed by Run() when the drain phase begins.
	// The send loop selects on it alongside buf.Notify() so an idle
	// sender (empty buffer at SIGTERM time) wakes up immediately and
	// returns nil instead of blocking forever waiting for a Push that
	// will never come now that the collect loop is gone.
	//
	// Allocated in New; never re-created — the scheduler is single-
	// shot, one call to Run per instance.
	shutdownSend chan struct{}

	// rng is used to add jitter to the back-off so a herd of agents
	// recovering from a shared outage don't synchronise their retries
	// and re-DoS the server. Per-scheduler instance to avoid a global
	// mutex on math/rand.
	rng *rand.Rand

	// throttleUntilNs is the wall-clock nanosecond timestamp until
	// which the collect loop should skip every other tick (see
	// throttleDuration). Zero = not throttled. Updated atomically by
	// sendLoop (on dropped_quota > 0) and read atomically by
	// collectLoop on each tick — no mutex needed.
	throttleUntilNs atomic.Int64

	// throttleSkipNext flips on every check while throttled. Keeps the
	// "halve the rate" logic branch-free and trivially reviewable.
	throttleSkipNext atomic.Bool
}

// New builds a Scheduler from Options. Panics on missing required
// options — those are programmer errors, not runtime conditions.
func New(opts Options) *Scheduler {
	if opts.Registry == nil {
		panic("scheduler.New: Registry required")
	}
	if opts.Sink == nil {
		panic("scheduler.New: Sink required")
	}
	if opts.Interval <= 0 {
		panic("scheduler.New: Interval must be > 0")
	}
	cap := opts.BufferBatches
	if cap <= 0 {
		cap = DefaultBufferBatches
	}
	drain := opts.DrainTimeout
	if drain == 0 {
		drain = DefaultDrainTimeout
	}
	// Negative is the explicit "no drain" opt-out — clamp to zero so
	// the post-SIGTERM path simply skips the drain phase. We don't
	// reject negatives because tests legitimately use them to assert
	// the no-drain branch.
	if drain < 0 {
		drain = 0
	}
	log := opts.Logger
	if log == nil {
		// Discard logger so callers don't have to remember to set
		// one in tests. slog.New(nil) panics — use a real handler
		// pointed at io.Discard via the slog stdlib defaults.
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Scheduler{
		interval:     opts.Interval,
		drainTimeout: drain,
		registry:     opts.Registry,
		sink:         opts.Sink,
		buf:          NewBuffer(cap),
		log:          log,
		shutdownSend: make(chan struct{}),
		// math/rand/v2 is goroutine-safe at the package level but we
		// want a per-scheduler seed so test runs are reproducible
		// when we pin them; use a non-zero seed derived from start
		// time to keep production runs unpredictable enough to
		// break thundering herds. gosec G404: jitter, not crypto.
		rng: rand.New(rand.NewPCG(uint64(time.Now().UnixNano()), 0)), //nolint:gosec
	}
}

// Stats returns a snapshot of the scheduler's counters.
func (s *Scheduler) Stats() Stats {
	snap := s.stats.Snapshot()
	// DroppedOverflow lives on the buffer (it's the source of truth);
	// mirror it here so callers only need to look at one struct.
	snap.DroppedOverflow = s.buf.DroppedOverflow()
	return snap
}

// Run starts the collect and send loops and blocks until either ctx is
// cancelled or the sink returns a fatal error (ErrFatal; typically 404
// meaning "token revoked / probe deleted"). On fatal, the collect loop
// is cancelled and Run returns the wrapped error for main() to surface
// as a non-zero exit.
//
// Draining the buffer with one final flush is the responsibility of the
// process-lifecycle layer (see runDaemon in cmd/vigil-agent), not this
// loop's.
func (s *Scheduler) Run(ctx context.Context) error {
	s.log.Info("scheduler starting",
		slog.String("event", "scheduler.start"),
		slog.Duration("interval", s.interval),
		slog.Int("buffer_cap", s.buf.Cap()),
	)

	// Two contexts so the drain phase has a clear story:
	//   collectCtx: cancelled at first sign of shutdown (SIGTERM or
	//               fatal sink). Stops new scrapes — no fresh data
	//               can arrive while we're trying to drain.
	//   sendCtx:    stays alive through the drain window. Cancelled
	//               immediately on a fatal sink error (no point
	//               draining when the upstream just told us to stop)
	//               or when the drainTimeout deadline expires.
	//
	// We deliberately do NOT use context.WithCancelCause: Go 1.22+
	// signal.NotifyContext sets a non-Canceled cause when SIGTERM
	// arrives, which made `context.Cause(ctx)` unable to reliably
	// distinguish "signal" from "fatal sink error" without a
	// Go-version-fragile errors.Is dance. An explicit fatalErr
	// capture is shorter and version-stable.
	collectCtx, cancelCollect := context.WithCancel(ctx)
	defer cancelCollect()
	// contextcheck: sendCtx is deliberately rooted on context.Background()
	// rather than ctx — the drain phase must outlive the parent context's
	// cancellation (SIGTERM) so the buffer can flush. The cancel is owned
	// by Run() and the deferred cancelSend() guarantees no leak.
	sendCtx, cancelSend := context.WithCancel(context.Background()) //nolint:contextcheck
	defer cancelSend()

	var (
		fatalMu  sync.Mutex
		fatalErr error
	)

	var collectWG, sendWG sync.WaitGroup
	collectWG.Add(1)
	sendWG.Add(1)

	go func() {
		defer collectWG.Done()
		s.collectLoop(collectCtx)
	}()
	go func() {
		defer sendWG.Done()
		if err := s.sendLoop(sendCtx); err != nil {
			// Fatal sink error: no drain (upstream is broken,
			// the buffered batches will fail the same way).
			// Capture the error and yank both contexts.
			fatalMu.Lock()
			fatalErr = err
			fatalMu.Unlock()
			cancelCollect()
			cancelSend()
		}
	}()

	// Wait for the first sign of shutdown — either SIGTERM (parent
	// ctx cancels collectCtx) or a fatal sink error (the goroutine
	// above cancels collectCtx itself).
	<-collectCtx.Done()
	collectWG.Wait() // guarantee no more Pushes can land during drain.

	// Was the shutdown caused by a fatal sink error? If so, sendLoop
	// has already returned and there's nothing to drain.
	fatalMu.Lock()
	hadFatal := fatalErr != nil
	fatalMu.Unlock()

	if !hadFatal && s.drainTimeout > 0 {
		s.drainAndStop(&sendWG, cancelSend)
	} else {
		// Either no drain configured, or a fatal cut us off. Yank
		// the send loop and wait for it to acknowledge.
		cancelSend()
		sendWG.Wait()
	}

	s.log.Info("scheduler stopped",
		slog.String("event", "scheduler.stop"),
		slog.Int("buffer_pending", s.buf.Len()),
	)

	fatalMu.Lock()
	defer fatalMu.Unlock()
	return fatalErr
}

// drainAndStop is the post-SIGTERM bounded flush. Closing shutdownSend
// flips the send loop into "exit when buffer empty" mode without
// needing a flag — the channel close is the signal AND it wakes any
// idle sender that's blocked on buf.Notify().
//
// Invariants on entry: collect loop is finished (no more Pushes), the
// fatalErr capture is unset (hadFatal == false), drainTimeout > 0.
//
// We log both the start (so operators see "drain begun, N pending")
// and the outcome (clean drain vs deadline cut), classified by stable
// event keys so journal aggregations can split "graceful exits" from
// "exits with data left in flight".
func (s *Scheduler) drainAndStop(sendWG *sync.WaitGroup, cancelSend context.CancelFunc) {
	pending := s.buf.Len()
	s.log.Info("draining buffer before exit",
		slog.String("event", "drain.start"),
		slog.Duration("deadline", s.drainTimeout),
		slog.Int("pending", pending),
	)

	close(s.shutdownSend)

	// Channel-bridge the WaitGroup so we can select on it alongside
	// the deadline timer. Done() doesn't return a channel directly.
	done := make(chan struct{})
	go func() {
		sendWG.Wait()
		close(done)
	}()

	select {
	case <-done:
		// Send loop returned voluntarily — buffer is empty (or
		// drained as far as it could before we ran out of time
		// inside the loop's own ctx checks). Either way: clean.
		s.log.Info("buffer drained before deadline",
			slog.String("event", "drain.complete"),
			slog.Int("buffer_pending", s.buf.Len()),
		)
	case <-time.After(s.drainTimeout):
		// Hard deadline. Cancel the send context to interrupt any
		// in-flight POST or back-off sleep, then wait for the
		// goroutine to acknowledge before returning.
		left := s.buf.Len()
		s.log.Warn("drain deadline exceeded; abandoning buffered batches",
			slog.String("event", "drain.deadline_exceeded"),
			slog.Duration("deadline", s.drainTimeout),
			slog.Int("buffer_pending", left),
		)
		cancelSend()
		<-done
	}
}

// collectLoop runs Registry.Scrape on each tick and pushes the result
// into the buffer. The first scrape happens IMMEDIATELY (not after one
// interval) so:
//
//   - the operator sees data within a second of starting the daemon
//   - gopsutil's CPU baseline is established before the first interval
//     elapses, so the second scrape has a real delta
//
// If a scrape returns zero samples (every collector failed — should be
// rare), we skip the Push. Pushing an empty batch would just waste the
// sender's time and confuse the chart.
func (s *Scheduler) collectLoop(ctx context.Context) {
	// Immediate first scrape so the daemon "starts shipping" without
	// waiting a full interval.
	s.scrapeOnce(ctx)

	t := time.NewTicker(s.interval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if s.shouldSkipTick() {
				// Throttled + it's this tick's turn to skip.
				// We don't scrape, we don't push — next tick
				// we try again. Server gets one scrape per
				// 2×interval instead of one per interval.
				continue
			}
			s.scrapeOnce(ctx)
		}
	}
}

// shouldSkipTick returns true if the scheduler is currently throttled
// (dropped_quota recently observed) AND it's this tick's turn to be
// skipped. We alternate skip/scrape while throttled so the effective
// rate is exactly halved — not a fifth, not "whatever the timer felt
// like". The alternation state is a separate bool (not tick-count
// modulo) so leaving and re-entering throttle always behaves the same.
func (s *Scheduler) shouldSkipTick() bool {
	until := s.throttleUntilNs.Load()
	if until == 0 {
		return false
	}
	if time.Now().UnixNano() >= until {
		// Throttle window expired. Clear state so the next tick
		// goes through the fast path above.
		s.throttleUntilNs.Store(0)
		s.throttleSkipNext.Store(false)
		return false
	}
	// Flip the skip flag and return its PREVIOUS value — the current
	// tick is skipped iff the last tick was scraped. Produces the
	// scrape, skip, scrape, skip, … pattern we want.
	skip := s.throttleSkipNext.Load()
	s.throttleSkipNext.Store(!skip)
	return skip
}

// engageThrottle marks the scheduler as throttled for `throttleDuration`
// from now. Idempotent — a second call while already throttled just
// extends the deadline, which is the right semantics (sustained
// dropped_quota = sustained throttle).
//
// CRITICAL: we must NOT reset throttleSkipNext on re-engagement. While
// the server keeps sending dropped_quota > 0, EVERY successful Send
// calls engageThrottle again. If we reset the skip flag to false each
// time, shouldSkipTick's alternation breaks and no ticks ever get
// skipped — defeating the whole point of the throttle. The skip flag
// is owned exclusively by shouldSkipTick once throttling starts.
func (s *Scheduler) engageThrottle() {
	deadline := time.Now().Add(throttleDuration).UnixNano()
	firstTime := s.throttleUntilNs.Swap(deadline) == 0
	if firstTime {
		// Start the alternation cleanly from "scrape first, skip
		// second" so the server gets one more sample before we
		// start cutting rate — less jarring than an instant gap.
		s.throttleSkipNext.Store(false)
		s.log.Warn("throttle engaged (server reported dropped_quota)",
			slog.String("event", "throttle.engage"),
			slog.Duration("duration", throttleDuration),
		)
	}
}

// scrapeOnce runs the registry, updates counters, and pushes into the
// buffer if there's anything to send. Logging is deliberately quiet:
// per-scrape success messages would drown the log. We log only on
// errors and on overflow.
func (s *Scheduler) scrapeOnce(ctx context.Context) {
	batch, errs := s.registry.Scrape(ctx, time.Now())

	if len(errs) > 0 {
		atomic.AddUint64(&s.stats.ScrapesPartial, 1)
		// One concise warning per partial scrape with the full
		// per-collector breakdown attached as a single attribute.
		// Operators can grep `event=scrape.partial` to count
		// flaky-collector incidents over time.
		s.log.Warn("scrape: collectors errored",
			slog.String("event", "scrape.partial"),
			slog.Int("error_count", len(errs)),
			slog.String("detail", collector.FormatErrors(errs)),
		)
	}
	if len(batch.Metrics) == 0 {
		atomic.AddUint64(&s.stats.ScrapesEmpty, 1)
		return
	}
	atomic.AddUint64(&s.stats.ScrapesOK, 1)
	atomic.AddUint64(&s.stats.SamplesCollected, uint64(len(batch.Metrics)))

	if overflow := s.buf.Push(batch); overflow {
		// One log line per eviction is appropriate — sustained
		// overflow is a real operational concern and the operator
		// should see it in their journal.
		s.log.Warn("buffer overflow: dropped oldest batch",
			slog.String("event", "buffer.overflow"),
			slog.Uint64("dropped_total", s.buf.DroppedOverflow()),
		)
	}
}

// sendLoop pops batches from the buffer and ships them via the Sink.
// Empty buffer → block on the buffer's notify channel (or ctx). Send
// failure → exponential back-off with jitter, capped at maxSendBackoff.
//
// We peek-then-pop (rather than pop-then-retry-on-failure) so a
// transient sink error never loses the batch. The batch stays at the
// head of the queue until either Sink.Send returns nil, or the buffer
// fills and Push evicts it for being too old.
//
// Returns non-nil only for ErrFatal-wrapped errors from the sink — i.e.
// "this agent should die so systemd shows red and the operator fixes
// the config". Regular transient errors (5xx, network, 429) retry
// forever and never cause Run to return.
func (s *Scheduler) sendLoop(ctx context.Context) error {
	backoff := time.Duration(0)

	for {
		// Honour ctx first so a cancellation while we're sleeping
		// in back-off returns promptly.
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		batch, ok := s.buf.Peek()
		if !ok {
			// Empty buffer. Three things can wake us:
			//   1. ctx cancellation (drain deadline or fatal).
			//   2. shutdownSend close (drain phase begun AND
			//      buffer empty → we're done; return cleanly).
			//   3. New data lands via buf.Notify().
			//
			// Ordering matters for case 2: we re-check Peek
			// after taking the shutdown signal in case a final
			// Push raced with the close. Without the re-check
			// we'd silently drop the last batch on the way out,
			// which is the exact failure mode "graceful drain"
			// is supposed to prevent.
			select {
			case <-ctx.Done():
				return nil
			case <-s.shutdownSend:
				if _, hasMore := s.buf.Peek(); !hasMore {
					return nil
				}
				continue
			case <-s.buf.Notify():
			}
			continue
		}

		res, err := s.sink.Send(ctx, batch)
		if err == nil {
			// Success: remove the batch and reset back-off. We
			// pop AFTER a successful send so a Send that panics
			// or never returns leaves the data in the buffer
			// for the next attempt — defence in depth against
			// a buggy sink implementation.
			s.buf.Pop()
			atomic.AddUint64(&s.stats.BatchesSent, 1)
			atomic.StoreInt64(&s.stats.LastFlushUnix, time.Now().Unix())
			backoff = 0

			// Server-side throttling signal. Don't conflate this
			// with a sink error: the batch WAS accepted; the
			// server is just telling us it had to drop samples
			// from inside it because we're over budget.
			if res.DroppedQuota > 0 {
				atomic.AddUint64(&s.stats.DroppedQuotaSamples, uint64(res.DroppedQuota))
				s.engageThrottle()
			}
			continue
		}

		// Fatal: unwind the send loop and let Run() propagate. We
		// do NOT pop the batch — the buffer will be drained (or
		// not) by the daemon's graceful-shutdown path. Leaking one
		// batch here is fine; the process is terminating anyway.
		if errors.Is(err, ErrFatal) {
			s.log.Error("FATAL sink error, stopping agent",
				slog.String("event", "sink.fatal"),
				slog.String("error", err.Error()),
			)
			return err
		}

		atomic.AddUint64(&s.stats.BatchesFailed, 1)

		// 429 Retry-After: the server gave us a specific floor
		// we should respect. Use it as the minimum but still
		// apply exponential growth so a misconfigured "Retry-After:
		// 1" on a server that's actually dead doesn't DoS it.
		var retryAfter *RetryAfterError
		if errors.As(err, &retryAfter) {
			s.log.Warn("sink 429 (retry-after honored)",
				slog.String("event", "sink.retry_after"),
				slog.Duration("retry_after", retryAfter.Delay),
				slog.String("error", err.Error()),
			)
			backoff = nextBackoff(backoff)
			if retryAfter.Delay > backoff {
				backoff = retryAfter.Delay
			}
			if backoff > maxSendBackoff {
				backoff = maxSendBackoff
			}
			s.sleepWithJitter(ctx, backoff, false /* full-jitter bounds honor floor */)
			continue
		}

		s.log.Warn("sink error (will retry with backoff)",
			slog.String("event", "sink.error"),
			slog.String("error", err.Error()),
		)

		// Back-off with full jitter (Marc Brooker's variant: sleep a
		// random duration in [0, backoff]). Avoids the worst-case
		// thundering-herd on a shared outage where every agent
		// retries at exactly the same monotonic offsets.
		backoff = nextBackoff(backoff)
		s.sleepWithJitter(ctx, backoff, true /* full jitter */)
	}
}

// sleepWithJitter sleeps for a duration based on `base`. With
// `fullJitter=true` it sleeps a random duration in [0, base] — the
// classic AWS Architecture Blog pattern. With false, it sleeps at
// least half of `base` and at most `base`, which is the right shape
// when we have a server-specified Retry-After floor we don't want to
// accidentally ignore via jitter.
func (s *Scheduler) sleepWithJitter(ctx context.Context, base time.Duration, fullJitter bool) {
	if base <= 0 {
		return
	}
	var sleep time.Duration
	if fullJitter {
		sleep = time.Duration(s.rng.Int64N(int64(base) + 1))
	} else {
		half := int64(base) / 2
		sleep = time.Duration(half + s.rng.Int64N(half+1))
	}
	select {
	case <-ctx.Done():
	case <-time.After(sleep):
	}
}

// nextBackoff doubles the previous back-off, clamped to [min, max].
// First failure: jumps from 0 to minSendBackoff. Subsequent failures:
// 1s → 2s → 4s → 8s → 16s → 32s → 60s (capped).
func nextBackoff(prev time.Duration) time.Duration {
	if prev <= 0 {
		return minSendBackoff
	}
	next := prev * 2
	if next > maxSendBackoff {
		return maxSendBackoff
	}
	if next < minSendBackoff {
		return minSendBackoff
	}
	return next
}
