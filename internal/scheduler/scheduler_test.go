package scheduler

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Seppia-AI/vigil-agent/internal/collector"
)

// fakeCollector is the minimum collector.Collector implementation needed
// to drive the scheduler in tests without touching real /proc.
type fakeCollector struct {
	called atomic.Uint64
}

func (*fakeCollector) Name() string { return "fake" }
func (f *fakeCollector) Collect(_ context.Context) ([]collector.Sample, error) {
	f.called.Add(1)
	return []collector.Sample{
		{Name: "cpu.usage", Value: 1},
		{Name: "mem.used", Value: 2},
	}, nil
}

// recordingSink stores every batch it sees and (optionally) returns an
// error for the first N calls before becoming healthy. Lets us assert
// both happy-path delivery and back-off+retry behaviour without sleeping
// for real ingest timeouts.
type recordingSink struct {
	mu          sync.Mutex
	got         []collector.Batch
	failFirst   int
	failedSoFar int
	failErr     error
	// result overrides the SendResult returned on SUCCESS (used to
	// drive the throttle path via DroppedQuota>0).
	result SendResult
}

func (r *recordingSink) Send(_ context.Context, b collector.Batch) (SendResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failedSoFar < r.failFirst {
		r.failedSoFar++
		return SendResult{}, r.failErr
	}
	r.got = append(r.got, b)
	return r.result, nil
}

func (r *recordingSink) Count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.got)
}

func newTestScheduler(t *testing.T, sink Sink, interval time.Duration) (*Scheduler, *fakeCollector, *bytes.Buffer) {
	t.Helper()
	fc := &fakeCollector{}
	reg := collector.NewRegistry(
		[]collector.Collector{fc},
		collector.HostInfo{Hostname: "test", OS: "linux", Arch: "amd64", AgentVersion: "test"},
		nil, nil,
	)
	// Wrap a bytes.Buffer in a slog text handler so tests can grep
	// the captured output AND exercise the structured-logging path
	// the production daemon uses.
	logBuf := &bytes.Buffer{}
	logger := slog.New(slog.NewTextHandler(logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	s := New(Options{
		Interval:      interval,
		Registry:      reg,
		Sink:          sink,
		BufferBatches: 4,
		Logger:        logger,
	})
	return s, fc, logBuf
}

func TestScheduler_FirstScrapeIsImmediate(t *testing.T) {
	// Even with a 10s interval, the first scrape must happen within
	// a few hundred ms — that's the whole point of the immediate
	// kickoff in collectLoop.
	sink := &recordingSink{}
	s, fc, _ := newTestScheduler(t, sink, 10*time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() { _ = s.Run(ctx) }()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if fc.called.Load() >= 1 && sink.Count() >= 1 {
			cancel()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("first scrape did not happen within 2s; collector calls=%d sink count=%d",
		fc.called.Load(), sink.Count())
}

func TestScheduler_RetriesAfterSinkFailures(t *testing.T) {
	// Sink errors twice, then succeeds. The same batch should be
	// retried (peek-then-pop semantics) and eventually delivered.
	sink := &recordingSink{failFirst: 2, failErr: errors.New("simulated outage")}
	s, _, logBuf := newTestScheduler(t, sink, 50*time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()

	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if sink.Count() >= 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done

	if sink.Count() < 1 {
		t.Fatalf("no successful send after retries; log:\n%s", logBuf.String())
	}
	st := s.Stats()
	if st.BatchesFailed < 2 {
		t.Errorf("BatchesFailed=%d want >=2", st.BatchesFailed)
	}
	if st.BatchesSent < 1 {
		t.Errorf("BatchesSent=%d want >=1", st.BatchesSent)
	}
	if !strings.Contains(logBuf.String(), "sink error") {
		t.Errorf("expected sink error to be logged; got:\n%s", logBuf.String())
	}
}

func TestScheduler_FatalErrorStopsAgent(t *testing.T) {
	// An ErrFatal-wrapped sink error must make Run return with a
	// non-nil error (wrapping ErrFatal) and must NOT leave any
	// goroutines spinning. That's the 404-from-ingest path: token
	// is revoked, no retry will ever succeed, exit red.
	fatalSink := &fatalSinkStub{
		err: fmt.Errorf("%w: 404 Not Found", ErrFatal),
	}
	s, _, logBuf := newTestScheduler(t, fatalSink, 30*time.Millisecond)

	// Generous timeout: fatal should trigger on the very first Send
	// (after the first scrape lands in the buffer), which is a few
	// tens of ms.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	start := time.Now()
	err := s.Run(ctx)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("Run returned nil; want ErrFatal. log:\n%s", logBuf.String())
	}
	if !errors.Is(err, ErrFatal) {
		t.Errorf("Run err=%v, want errors.Is(err, ErrFatal)==true", err)
	}
	if elapsed > 1500*time.Millisecond {
		t.Errorf("fatal-path shutdown took %s, want <1.5s", elapsed)
	}
	if !strings.Contains(logBuf.String(), "FATAL") {
		t.Errorf("expected FATAL in log; got:\n%s", logBuf.String())
	}
}

// fatalSinkStub returns the same fatal error on every call. The
// scheduler should see it, short-circuit the send loop, and bring the
// agent down. Returning the same error repeatedly is fine — the
// scheduler shouldn't CALL it repeatedly.
type fatalSinkStub struct {
	err  error
	hits atomic.Uint64
}

func (f *fatalSinkStub) Send(_ context.Context, _ collector.Batch) (SendResult, error) {
	f.hits.Add(1)
	return SendResult{}, f.err
}

func TestScheduler_RetryAfterHonorsFloor(t *testing.T) {
	// A RetryAfterError with a 200ms delay must cause the scheduler
	// to wait AT LEAST ~200ms-ish before retrying. We check by
	// measuring the gap between the two Send calls on a sink that
	// returns RetryAfterError the first time and nil the second.
	//
	// Jitter in sleepWithJitter (non-full-jitter path) sleeps in
	// [delay/2, delay], so the lower bound we can safely assert is
	// delay/2 minus a small scheduling slack.
	sink := &retryAfterSink{delay: 200 * time.Millisecond}
	s, _, _ := newTestScheduler(t, sink, 10*time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { _ = s.Run(ctx); close(done) }()

	// Wait until we've observed the second call (the successful one).
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if sink.successCalls.Load() >= 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done

	if sink.successCalls.Load() < 1 {
		t.Fatal("sink never saw a successful call")
	}
	gap := sink.secondCallAt.Load() - sink.firstCallAt.Load()
	// delay/2 = 100ms. Give 40ms slack for the runtime. Still a
	// clear signal we honoured the floor.
	if gap < 60*time.Millisecond.Nanoseconds() {
		t.Errorf("retry gap=%dns want >=60ms (delay was 200ms, floor is delay/2)", gap)
	}
}

type retryAfterSink struct {
	delay        time.Duration
	calls        atomic.Uint64
	successCalls atomic.Uint64
	firstCallAt  atomic.Int64 // unix nanos
	secondCallAt atomic.Int64
}

func (s *retryAfterSink) Send(_ context.Context, _ collector.Batch) (SendResult, error) {
	n := s.calls.Add(1)
	nowNs := time.Now().UnixNano()
	if n == 1 {
		s.firstCallAt.Store(nowNs)
		return SendResult{}, &RetryAfterError{Delay: s.delay}
	}
	if n == 2 {
		s.secondCallAt.Store(nowNs)
	}
	s.successCalls.Add(1)
	return SendResult{}, nil
}

func TestScheduler_ThrottleEngagesOnDroppedQuota(t *testing.T) {
	// When Send returns DroppedQuota>0 the scheduler should engage the
	// throttle. With a 20ms interval, an un-throttled run in 300ms
	// should produce ~15 scrapes; a throttled run should produce ~7-8.
	// We tolerate a wide band because timers are fuzzy in CI.
	sink := &recordingSink{result: SendResult{DroppedQuota: 42}}
	s, fc, logBuf := newTestScheduler(t, sink, 20*time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	go func() { _ = s.Run(ctx); close(done) }()
	<-done

	if !strings.Contains(logBuf.String(), "throttle engaged") {
		t.Errorf("expected 'throttle engaged' in log; got:\n%s", logBuf.String())
	}
	calls := fc.called.Load()
	// Un-throttled upper bound (15) with a wide margin for CI noise.
	// A throttled run should be distinctly lower.
	if calls == 0 {
		t.Fatal("collector never called")
	}
	if calls >= 14 {
		t.Errorf("collector called %d times in 300ms @ 20ms interval; throttle likely did NOT engage", calls)
	}
	st := s.Stats()
	if st.DroppedQuotaSamples == 0 {
		t.Errorf("DroppedQuotaSamples=0; want >0 after engaging throttle")
	}
}

func TestScheduler_StatsAccumulate(t *testing.T) {
	sink := &recordingSink{}
	// Tight interval so we get several scrapes inside the test
	// window; nothing in this scheduler costs more than ms.
	s, fc, _ := newTestScheduler(t, sink, 30*time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	go func() { _ = s.Run(ctx); close(done) }()
	<-done

	st := s.Stats()
	if st.ScrapesOK < 2 {
		t.Errorf("ScrapesOK=%d want >=2 over 250ms with 30ms interval", st.ScrapesOK)
	}
	if fc.called.Load() != st.ScrapesOK+st.ScrapesEmpty+st.ScrapesPartial {
		t.Errorf("collector called %d times but stats sum to %d",
			fc.called.Load(), st.ScrapesOK+st.ScrapesEmpty+st.ScrapesPartial)
	}
	// 2 samples per scrape (see fakeCollector).
	if st.SamplesCollected != 2*st.ScrapesOK {
		t.Errorf("SamplesCollected=%d want %d", st.SamplesCollected, 2*st.ScrapesOK)
	}
	if st.LastFlushUnix == 0 {
		t.Error("LastFlushUnix should be set after at least one Send")
	}
}

func TestScheduler_GracefulShutdown(t *testing.T) {
	// Cancelling the context must make Run return promptly. We give
	// it a generous 1s but on a healthy machine this is microseconds.
	sink := &recordingSink{}
	s, _, _ := newTestScheduler(t, sink, time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = s.Run(ctx); close(done) }()

	cancel()
	select {
	case <-done:
		// good
	case <-time.After(time.Second):
		t.Fatal("Run did not return within 1s of cancellation")
	}
}

// gatedSink blocks every Send until `release` is closed (or ctx
// cancels). Lets a test pre-load the buffer with N batches that the
// sender CANNOT consume until the test says go — exactly the shape
// needed to exercise the drain phase deterministically.
type gatedSink struct {
	mu      sync.Mutex
	got     []collector.Batch
	release chan struct{}
}

func newGatedSink() *gatedSink { return &gatedSink{release: make(chan struct{})} }

func (g *gatedSink) Send(ctx context.Context, b collector.Batch) (SendResult, error) {
	select {
	case <-g.release:
	case <-ctx.Done():
		return SendResult{}, ctx.Err()
	}
	g.mu.Lock()
	g.got = append(g.got, b)
	g.mu.Unlock()
	return SendResult{}, nil
}

func (g *gatedSink) Count() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.got)
}

func TestScheduler_DrainEmptiesBufferOnShutdown(t *testing.T) {
	// Exercise the happy drain path:
	//   1. Sink is gated, so batches accumulate in the buffer.
	//   2. We cancel the parent ctx (= "SIGTERM").
	//   3. Drain begins. We then release the gate; every queued
	//      Send completes back-to-back.
	//   4. Run must return only AFTER all buffered batches have
	//      been delivered (or fast-enough that the drain timer
	//      didn't expire). buf.Len() must end at zero.
	//
	// This is the contract that makes "graceful restart" worth
	// running — without it, every systemctl restart would lose up to
	// `buffer_cap` batches of metrics.
	sink := newGatedSink()
	fc := &fakeCollector{}
	reg := collector.NewRegistry(
		[]collector.Collector{fc},
		collector.HostInfo{Hostname: "test", OS: "linux", Arch: "amd64", AgentVersion: "test"},
		nil, nil,
	)
	logBuf := &bytes.Buffer{}
	logger := slog.New(slog.NewTextHandler(logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	s := New(Options{
		Interval:      20 * time.Millisecond,
		Registry:      reg,
		Sink:          sink,
		BufferBatches: 10, // generous so we don't hit overflow during accumulation
		Logger:        logger,
		DrainTimeout:  2 * time.Second,
	})

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	go func() {
		if err := s.Run(ctx); err != nil {
			t.Errorf("Run returned err=%v want nil", err)
		}
		close(runDone)
	}()

	// Let several scrapes accumulate in the buffer behind the
	// gated sink. With interval=20ms and gate held closed, we
	// expect Len() to climb to ~4-5 within 100ms.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) && s.buf.Len() < 3 {
		time.Sleep(10 * time.Millisecond)
	}
	if s.buf.Len() < 2 {
		t.Fatalf("buffer never accumulated: len=%d (collector called=%d)",
			s.buf.Len(), fc.called.Load())
	}
	preBufLen := s.buf.Len()

	// Trigger SIGTERM-equivalent. Drain phase begins; collect loop
	// exits; sendLoop is still blocked on the gate.
	cancel()

	// Give the drain phase a moment to reach the close(shutdownSend)
	// path, then release the gate. Sends complete in rapid sequence;
	// the loop pops each and re-peeks until empty, then returns.
	time.Sleep(50 * time.Millisecond)
	close(sink.release)

	select {
	case <-runDone:
	case <-time.After(3 * time.Second):
		t.Fatalf("Run did not return after drain release; buf.Len=%d sink.Count=%d log:\n%s",
			s.buf.Len(), sink.Count(), logBuf.String())
	}

	if s.buf.Len() != 0 {
		t.Errorf("buffer not empty after drain: len=%d", s.buf.Len())
	}
	if sink.Count() < preBufLen {
		t.Errorf("sink received %d batches; want >= %d (everything in buffer at shutdown)",
			sink.Count(), preBufLen)
	}
	if !strings.Contains(logBuf.String(), "drain.start") {
		t.Errorf("log missing event=drain.start; got:\n%s", logBuf.String())
	}
	if !strings.Contains(logBuf.String(), "drain.complete") {
		t.Errorf("log missing event=drain.complete; got:\n%s", logBuf.String())
	}
}

func TestScheduler_DrainDeadlineExceeded(t *testing.T) {
	// Sad-path drain: the sink stays blocked, so the drain timer
	// fires and we abandon whatever's still buffered. The test must
	// observe BOTH that Run returned within drainTimeout (+ slack)
	// AND that we logged the deadline event so operators have a
	// trail.
	sink := newGatedSink() // never released
	fc := &fakeCollector{}
	reg := collector.NewRegistry(
		[]collector.Collector{fc},
		collector.HostInfo{Hostname: "test", OS: "linux", Arch: "amd64", AgentVersion: "test"},
		nil, nil,
	)
	logBuf := &bytes.Buffer{}
	logger := slog.New(slog.NewTextHandler(logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	s := New(Options{
		Interval:      20 * time.Millisecond,
		Registry:      reg,
		Sink:          sink,
		BufferBatches: 10,
		Logger:        logger,
		DrainTimeout:  150 * time.Millisecond,
	})

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	go func() {
		if err := s.Run(ctx); err != nil {
			t.Errorf("Run returned err=%v want nil (no fatal)", err)
		}
		close(runDone)
	}()

	// Wait for at least one batch in the buffer so the deadline
	// has something to abandon.
	for d := time.Now().Add(500 * time.Millisecond); time.Now().Before(d); {
		if s.buf.Len() >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if s.buf.Len() < 1 {
		t.Fatalf("no batch in buffer to drain (collector called=%d)", fc.called.Load())
	}

	cancel()
	start := time.Now()
	select {
	case <-runDone:
	case <-time.After(time.Second):
		t.Fatalf("Run did not return within 1s after drain deadline")
	}
	elapsed := time.Since(start)
	// drainTimeout=150ms; allow generous slack for goroutine
	// scheduling + the second sendWG wait after cancelSend.
	if elapsed > 600*time.Millisecond {
		t.Errorf("Run took %s after cancel; expected ~drainTimeout (150ms)", elapsed)
	}
	if !strings.Contains(logBuf.String(), "drain.deadline_exceeded") {
		t.Errorf("log missing event=drain.deadline_exceeded; got:\n%s", logBuf.String())
	}
}

func TestScheduler_DrainSkippedWhenZero(t *testing.T) {
	// DrainTimeout=-1 (or any negative) opts out: shutdown should
	// be IMMEDIATE, no drain log lines emitted, batches lost.
	sink := newGatedSink()
	fc := &fakeCollector{}
	reg := collector.NewRegistry(
		[]collector.Collector{fc},
		collector.HostInfo{Hostname: "test", OS: "linux", Arch: "amd64", AgentVersion: "test"},
		nil, nil,
	)
	logBuf := &bytes.Buffer{}
	logger := slog.New(slog.NewTextHandler(logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	s := New(Options{
		Interval:      20 * time.Millisecond,
		Registry:      reg,
		Sink:          sink,
		BufferBatches: 4,
		Logger:        logger,
		DrainTimeout:  -1, // explicit no-drain
	})

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	go func() { _ = s.Run(ctx); close(runDone) }()

	time.Sleep(80 * time.Millisecond)
	cancel()

	start := time.Now()
	select {
	case <-runDone:
	case <-time.After(time.Second):
		t.Fatal("Run did not return after cancel with no-drain opt-out")
	}
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Errorf("immediate-shutdown took %s; want <200ms", elapsed)
	}
	if strings.Contains(logBuf.String(), "drain.start") {
		t.Errorf("drain.start logged with DrainTimeout=-1; got:\n%s", logBuf.String())
	}
	_ = fc
}

func TestLogSink_AlwaysSucceeds(t *testing.T) {
	// LogSink returning nil is a load-bearing property: the scheduler
	// pops on success, so a Sink that lies about success would
	// silently leak batches. Pin the contract.
	buf := &bytes.Buffer{}
	sink := NewLogSink(buf)
	for i := 0; i < 5; i++ {
		if _, err := sink.Send(context.Background(), mkBatch("h")); err != nil {
			t.Fatalf("LogSink.Send returned error: %v", err)
		}
	}
	if got := strings.Count(buf.String(), "[vigil-agent] flush"); got != 5 {
		t.Errorf("flush lines=%d want 5; got:\n%s", got, buf.String())
	}
}

func TestRetryAfterError_UnwrapsCause(t *testing.T) {
	// errors.Is/As should work transparently so the scheduler (or
	// tests) can detect a RetryAfterError AND the original cause.
	cause := errors.New("rate limited")
	e := &RetryAfterError{Delay: time.Second, Cause: cause}
	if !errors.Is(e, cause) {
		t.Errorf("errors.Is(RetryAfterError, cause) = false; want true")
	}
	var target *RetryAfterError
	if !errors.As(e, &target) {
		t.Errorf("errors.As(RetryAfterError, *RetryAfterError) = false; want true")
	}
	if target.Delay != time.Second {
		t.Errorf("unwrapped Delay=%s want 1s", target.Delay)
	}
}
