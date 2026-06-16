package main

import "testing"

// TestC1_ReplayReturnsInIDOrder_AfterWrap verifies that ringBuffer.Replay
// returns events in ascending ID order even after the buffer has wrapped.
//
// Before the C1 fix, Replay iterated the events slice directly. After wrap,
// the slice order diverged from chronological order, so a reconnecting
// client received events out of order. The fix sorts the filtered slice by
// ID before returning.
func TestC1_ReplayReturnsInIDOrder_AfterWrap(t *testing.T) {
	rb := newRingBuffer(3) // small buffer to force wrap quickly

	// Push 5 events into a size-3 buffer.
	// After wrap, the live IDs in chronological order are 3, 4, 5.
	// The underlying slice order is [e4, e5, e3] (head wrapped past 0).
	for i := 1; i <= 5; i++ {
		rb.Push("type", "data")
	}

	events := rb.Replay(0)
	if len(events) != 3 {
		t.Fatalf("expected 3 live events after wrap, got %d", len(events))
	}

	// Verify ascending ID order — the contract SSE clients depend on.
	for i := 1; i < len(events); i++ {
		if events[i].ID <= events[i-1].ID {
			t.Errorf("events not in ascending ID order at index %d: %d came before %d",
				i, events[i-1].ID, events[i].ID)
		}
	}

	// Specifically assert the chronological order: 3, 4, 5.
	expectedIDs := []int64{3, 4, 5}
	for i, ev := range events {
		if ev.ID != expectedIDs[i] {
			t.Errorf("event[%d].ID = %d, want %d (slice order would give 4, 5, 3)",
				i, ev.ID, expectedIDs[i])
		}
	}
}

// TestC1_ReplayPreservesOrder_LargeBufferNoWrap verifies the sort doesn't
// disturb the natural slice order when the buffer hasn't wrapped.
func TestC1_ReplayPreservesOrder_LargeBufferNoWrap(t *testing.T) {
	rb := newRingBuffer(50)
	for i := 1; i <= 10; i++ {
		rb.Push("type", "data")
	}

	events := rb.Replay(0)
	if len(events) != 10 {
		t.Fatalf("expected 10 events, got %d", len(events))
	}
	for i := 1; i < len(events); i++ {
		if events[i].ID != events[i-1].ID+1 {
			t.Errorf("events not contiguous at index %d: %d, %d",
				i, events[i-1].ID, events[i].ID)
		}
	}
}

// TestC1_ReplayRespectsLastSeen verifies the filter is still applied.
func TestC1_ReplayRespectsLastSeen(t *testing.T) {
	rb := newRingBuffer(3)
	for i := 1; i <= 5; i++ {
		rb.Push("type", "data")
	}

	// After wrap, live IDs are 3, 4, 5. lastSeen=3 should return only 4, 5.
	events := rb.Replay(3)
	if len(events) != 2 {
		t.Fatalf("expected 2 events with ID > 3, got %d", len(events))
	}
	if events[0].ID != 4 || events[1].ID != 5 {
		t.Errorf("expected IDs 4, 5; got %d, %d", events[0].ID, events[1].ID)
	}
}

// TestC1_ReplayReturnsNilForFutureLastSeen verifies early return.
func TestC1_ReplayReturnsNilForFutureLastSeen(t *testing.T) {
	rb := newRingBuffer(3)
	for i := 1; i <= 3; i++ {
		rb.Push("type", "data")
	}
	if events := rb.Replay(100); events != nil {
		t.Errorf("expected nil for lastSeen > count, got %d events", len(events))
	}
}

// TestH1_CountIsThreadSafe runs concurrent Push and Count under -race.
// The race detector will fail the test if Count() reads rb.count without
// the lock. (Verify with `go test -race ./cmd/mpm/`.)
func TestH1_CountIsThreadSafe(t *testing.T) {
	rb := newRingBuffer(100)

	// Producer: push 1000 events
	done := make(chan struct{})
	go func() {
		for i := 0; i < 1000; i++ {
			rb.Push("type", "data")
		}
		close(done)
	}()

	// Concurrent reader: spin on Count() until producer finishes
	for {
		select {
		case <-done:
			// Final read
			if got := rb.Count(); got != 1000 {
				t.Errorf("Count() = %d, want 1000", got)
			}
			return
		default:
			_ = rb.Count()
		}
	}
}
