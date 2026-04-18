// Package scheduler glues the collector layer (which produces a Batch
// per scrape) to the ingest layer (which POSTs Batches to the server).
//
// The architecture is deliberately small:
//
//	┌────────────────┐  push  ┌────────────┐  pop  ┌──────────┐
//	│ collect loop   │──────► │   Buffer   │ ────► │ send loop│──► Sink
//	│ (1 goroutine)  │        │ FIFO, capN │       │ (1 gor.) │
//	└────────────────┘        └────────────┘       └──────────┘
//
// One goroutine collects, one sends. The Buffer absorbs short ingest
// outages without blocking the collector — when the buffer is full, the
// OLDEST batch is dropped (and counted) so the freshest data always
// wins. That's the right trade-off for monitoring data: a minute-old
// gap is much more useful than a five-minute-old gap.
//
// We deliberately do NOT use a Go channel as the buffer because:
//
//   - Channels block on full, which would back-pressure the collector
//     and make scrapes overlap unpredictably during an outage.
//   - Channels can't drop the *oldest* element; only the newest can be
//     dropped via a non-blocking select. We want the opposite.
//
// So the Buffer is a tiny bounded FIFO under a mutex, with a separate
// notify channel that wakes the sender after a Push.
package scheduler

import (
	"sync"
	"sync/atomic"

	"github.com/Seppia-AI/vigil-agent/internal/collector"
)

// Buffer is a bounded FIFO of collector.Batch values with drop-oldest
// overflow semantics and a 1-slot notify channel for sender wakeups.
//
// Safe for one producer + one consumer (the typical case in this
// package). Multiple concurrent producers/consumers also work but
// aren't a design goal.
type Buffer struct {
	mu    sync.Mutex
	items []collector.Batch
	cap   int

	// dropped is a monotonic counter incremented every time Push had
	// to evict the oldest batch to make room. Atomic so DroppedOverflow
	// can be read without taking the mutex (cheap for a /metrics
	// scrape that may run concurrently with Push/Pop).
	dropped uint64

	// notify is a 1-buffered channel the producer signals on after a
	// successful Push. The sender selects on it (alongside its retry
	// timer + ctx.Done) to wake immediately when fresh data lands.
	// 1-buffered + non-blocking send means we never miss a wakeup
	// even if the sender is mid-flush when Push happens.
	notify chan struct{}
}

// NewBuffer returns a Buffer that holds at most `cap` batches before
// it starts dropping the oldest entry on Push. cap MUST be >= 1; the
// scheduler validates this at construction time and panics on a bad
// value (it would be a programmer error, not an operator one).
func NewBuffer(cap int) *Buffer {
	if cap < 1 {
		panic("scheduler.NewBuffer: cap must be >= 1")
	}
	return &Buffer{
		items:  make([]collector.Batch, 0, cap),
		cap:    cap,
		notify: make(chan struct{}, 1),
	}
}

// Push appends a batch to the tail. If the buffer is already at
// capacity, the OLDEST batch is dropped first and the dropped counter
// bumped. Returns true if an eviction happened (caller may want to log
// at warn level — sustained overflow means the sender is wedged).
//
// Push never blocks. After updating the buffer it sends a non-blocking
// signal on the notify channel so the sender wakes up if it was idle.
func (b *Buffer) Push(batch collector.Batch) (overflow bool) {
	b.mu.Lock()
	if len(b.items) >= b.cap {
		// Drop oldest. Slide everything down by one rather than
		// reslicing (b.items = b.items[1:]) so the underlying
		// array doesn't grow indefinitely as we keep appending
		// to the tail. Cheap at our cap (single-digit batches).
		copy(b.items, b.items[1:])
		b.items = b.items[:len(b.items)-1]
		atomic.AddUint64(&b.dropped, 1)
		overflow = true
	}
	b.items = append(b.items, batch)
	b.mu.Unlock()

	// Non-blocking notify: if there's already a pending signal the
	// sender will see it on its next loop, so a second one is
	// redundant. The buffered-1 channel collapses the notification
	// stream to "data available, go check".
	select {
	case b.notify <- struct{}{}:
	default:
	}
	return overflow
}

// Peek returns the oldest batch without removing it. Used by the sender
// to attempt a flush; on success it calls Pop, on failure it leaves the
// batch in place for the next retry. Returns ok=false on empty.
//
// We return a value (not a pointer) so the caller can't mutate the
// batch in the buffer. That matters for the retry path — a corrupted
// batch must never become un-retryable just because the sender clobbered
// a field on a previous attempt.
func (b *Buffer) Peek() (collector.Batch, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.items) == 0 {
		return collector.Batch{}, false
	}
	return b.items[0], true
}

// Pop removes and returns the oldest batch. Returns ok=false on empty.
// Always called by the sender AFTER a successful Sink.Send — never
// before — so the buffer is the source of truth for "still need to
// ship this".
func (b *Buffer) Pop() (collector.Batch, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.items) == 0 {
		return collector.Batch{}, false
	}
	first := b.items[0]
	copy(b.items, b.items[1:])
	// Zero the now-unused tail slot so the GC can reclaim its
	// referenced metric slice. Without this the underlying array
	// would keep a stale Batch reference alive after Pop returned.
	b.items[len(b.items)-1] = collector.Batch{}
	b.items = b.items[:len(b.items)-1]
	return first, true
}

// Len returns the current number of buffered batches. Useful for the
// /metrics endpoint and for tests.
func (b *Buffer) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.items)
}

// Cap returns the configured capacity (constant for the buffer's
// lifetime). Exported so the scheduler can log "buffer cap: N batches"
// at startup.
func (b *Buffer) Cap() int { return b.cap }

// DroppedOverflow returns the cumulative number of batches dropped
// because the buffer was full at Push time. Reads atomically — safe to
// call from any goroutine (e.g. a future /metrics handler).
func (b *Buffer) DroppedOverflow() uint64 {
	return atomic.LoadUint64(&b.dropped)
}

// Notify returns the channel the sender should select on to wake up
// when new data has been pushed. Receive-only by design — only the
// Buffer itself sends on it.
func (b *Buffer) Notify() <-chan struct{} { return b.notify }
