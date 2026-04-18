package scheduler

import (
	"sync"
	"testing"
	"time"

	"github.com/Seppia-AI/vigil-agent/internal/collector"
)

// mkBatch returns a Batch with a recognisable Hostname so FIFO order
// assertions can verify the right batches survived overflow.
func mkBatch(tag string) collector.Batch {
	return collector.Batch{
		Hostname: tag,
		Ts:       time.Now(),
		Metrics:  []collector.Sample{{Name: "n", Value: 1}},
	}
}

func TestBuffer_PushPop_FIFO(t *testing.T) {
	b := NewBuffer(3)
	b.Push(mkBatch("a"))
	b.Push(mkBatch("b"))
	b.Push(mkBatch("c"))

	if b.Len() != 3 {
		t.Fatalf("Len=%d want 3", b.Len())
	}
	for _, want := range []string{"a", "b", "c"} {
		got, ok := b.Pop()
		if !ok || got.Hostname != want {
			t.Errorf("Pop ok=%v hostname=%q want %q", ok, got.Hostname, want)
		}
	}
	if _, ok := b.Pop(); ok {
		t.Error("Pop on empty should return ok=false")
	}
}

func TestBuffer_OverflowDropsOldest(t *testing.T) {
	b := NewBuffer(2)
	if b.Push(mkBatch("a")) {
		t.Error("first push reported overflow")
	}
	if b.Push(mkBatch("b")) {
		t.Error("second push reported overflow")
	}
	if !b.Push(mkBatch("c")) {
		t.Error("third push (cap=2) should report overflow")
	}
	if !b.Push(mkBatch("d")) {
		t.Error("fourth push (cap=2) should report overflow")
	}

	if got, want := b.DroppedOverflow(), uint64(2); got != want {
		t.Errorf("DroppedOverflow=%d want %d", got, want)
	}

	// FIFO with drop-oldest: a and b were evicted; only c, d remain.
	if got, _ := b.Pop(); got.Hostname != "c" {
		t.Errorf("Pop[0]=%q want c", got.Hostname)
	}
	if got, _ := b.Pop(); got.Hostname != "d" {
		t.Errorf("Pop[1]=%q want d", got.Hostname)
	}
}

func TestBuffer_Peek_DoesNotMutate(t *testing.T) {
	b := NewBuffer(2)
	b.Push(mkBatch("a"))

	got1, ok := b.Peek()
	if !ok || got1.Hostname != "a" {
		t.Fatalf("Peek=%+v ok=%v", got1, ok)
	}
	if b.Len() != 1 {
		t.Errorf("Peek mutated buffer; Len=%d want 1", b.Len())
	}
	got2, _ := b.Peek()
	if got2.Hostname != "a" {
		t.Errorf("second Peek=%q want a", got2.Hostname)
	}
}

func TestBuffer_Notify_FiresOnPush(t *testing.T) {
	b := NewBuffer(2)
	// Notify is buffered-1 and never blocks Push, so the wakeup is
	// guaranteed available immediately after Push returns.
	b.Push(mkBatch("a"))

	select {
	case <-b.Notify():
		// good
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Notify did not fire after Push")
	}
}

func TestBuffer_Notify_Coalesces(t *testing.T) {
	// Two Pushes with no Pop in between should produce at most one
	// pending notification — the channel is buffered-1 and we use a
	// non-blocking send. The sender is supposed to drain the buffer
	// in response to a single wakeup, so coalescing is correct.
	b := NewBuffer(2)
	b.Push(mkBatch("a"))
	b.Push(mkBatch("b"))

	<-b.Notify() // first wakeup
	select {
	case <-b.Notify():
		t.Fatal("Notify should coalesce; got a second wakeup")
	case <-time.After(50 * time.Millisecond):
		// good
	}
}

func TestBuffer_ConcurrentPushPop(t *testing.T) {
	// Light correctness check under concurrency. We don't assert
	// specific popped/dropped split (it depends on scheduling) —
	// just the conservation law: every Push is accounted for as
	// EITHER a successful Pop OR a DroppedOverflow eviction.
	//
	// Conservation matters because a buggy lock could double-count or
	// silently drop without bumping the counter, both of which would
	// produce mysterious "where did my data go?" reports in production.
	b := NewBuffer(8)
	const N = 1000

	produced := make(chan struct{})
	popped := 0

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		for i := 0; i < N; i++ {
			b.Push(mkBatch("x"))
		}
		close(produced)
	}()

	go func() {
		defer wg.Done()
		// Drain whatever the producer makes available, then keep
		// going until the producer has signalled "done" AND the
		// buffer is empty. Yielding via a tiny sleep keeps this
		// non-spinny but also non-blocking on Notify (which the
		// producer may not bother to send fast enough to matter
		// under load).
		for {
			if _, ok := b.Pop(); ok {
				popped++
				continue
			}
			select {
			case <-produced:
				// Final drain attempt after producer is done.
				for {
					if _, ok := b.Pop(); !ok {
						return
					}
					popped++
				}
			default:
				time.Sleep(time.Microsecond)
			}
		}
	}()

	wg.Wait()
	if uint64(popped)+b.DroppedOverflow() != N {
		t.Errorf("conservation violated: popped=%d + dropped=%d != N=%d",
			popped, b.DroppedOverflow(), N)
	}
}

func TestBuffer_NewBuffer_PanicsOnZeroCap(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic for cap=0")
		}
	}()
	NewBuffer(0)
}

func TestNextBackoff(t *testing.T) {
	// maxSendBackoff is 5min — doubling from 32s gives 64s, which is
	// well below the cap, so we walk up to a value that IS above the
	// cap to exercise the clamp.
	cases := []struct {
		in, want time.Duration
	}{
		{0, minSendBackoff},
		{minSendBackoff, 2 * time.Second},
		{2 * time.Second, 4 * time.Second},
		{4 * time.Minute, maxSendBackoff}, // 8min capped to 5min
		{maxSendBackoff, maxSendBackoff},
	}
	for _, c := range cases {
		if got := nextBackoff(c.in); got != c.want {
			t.Errorf("nextBackoff(%v)=%v want %v", c.in, got, c.want)
		}
	}
}
