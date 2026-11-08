// deferred_write_shutdown_test.go — the MCP server's deferred-write
// shutdown path must REPORT what it could not land.
//
// WHY THIS FILE EXISTS.
//
// issue #178 (F2 remediation): the independent review found that the
// shutdown path
//
//   - logged a message with NO NUMBER when the flush budget expired,
//     and
//   - never called the one API that could report the count — so
//     `CloseDeferredWrites` was dead code in production.
//
// The path is exercised through a small seam, because the process-wide
// queue cannot be closed in a test: every later test in this package
// would lose it.
//
// The test pins the two behaviours that matter:
//
//   1. The queue is ALWAYS closed, whether or not the flush drained.
//   2. What was lost is reported — with the numbers, and at ERROR level
//      when accepted obligations did not land.

package main

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	core "github.com/flowbyte-com/mpm-core"
)

// shutdownCaptureHandler records every slog record's message and its
// attributes.
type shutdownCaptureHandler struct {
	mu    sync.Mutex
	msgs  []string
	attrs []slog.Attr
}

func (h *shutdownCaptureHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *shutdownCaptureHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.msgs = append(h.msgs, r.Message)
	r.Attrs(func(a slog.Attr) bool {
		h.attrs = append(h.attrs, a)
		return true
	})
	return nil
}

func (h *shutdownCaptureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *shutdownCaptureHandler) WithGroup(string) slog.Handler      { return h }

func (h *shutdownCaptureHandler) contains(sub string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, m := range h.msgs {
		if strings.Contains(m, sub) {
			return true
		}
	}
	return false
}

func (h *shutdownCaptureHandler) attr(name string) (slog.Value, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, a := range h.attrs {
		if a.Key == name {
			return a.Value, true
		}
	}
	return slog.Value{}, false
}

// TestDeferredWriteShutdown_ClosesTheQueueAndCountsWhatItLost pins the
// shutdown path's two obligations.
func TestDeferredWriteShutdown_ClosesTheQueueAndCountsWhatItLost(t *testing.T) {
	h := &shutdownCaptureHandler{}
	previous := slog.Default()
	slog.SetDefault(slog.New(h))
	t.Cleanup(func() { slog.SetDefault(previous) })

	closed := false
	flushCalled := false
	stats := core.DeferredWriteStats{
		Accepted:      7,
		Landed:        2,
		Pending:       3,
		LostEvidence:  2,
		DroppedClosed: 3,
	}

	deferredWriteShutdown(deferredWriteShutdownDeps{
		flush: func(ctx context.Context) error {
			flushCalled = true
			// The budget expired: the queue could not drain.
			return context.DeadlineExceeded
		},
		close: func() core.DeferredWriteStats {
			closed = true
			return stats
		},
	})

	if !flushCalled {
		t.Error("the shutdown did not flush the queue")
	}
	if !closed {
		t.Fatal("the shutdown did not close the queue; the pending obligations would be " +
			"discarded with no count")
	}

	// The flush failure is reported.
	if !h.contains("did not all land") {
		t.Errorf("the shutdown did not report the flush timeout; captured: %v", h.msgs)
	}
	// The loss is reported, with the numbers.
	if !h.contains("LOST at shutdown") {
		t.Errorf("the shutdown did not report the loss; captured: %v", h.msgs)
	}
	if v, ok := h.attr("discarded_at_close"); !ok || v.Int64() != 3 {
		t.Errorf("the shutdown did not report how many obligations were discarded; captured: %v", h.attrs)
	}
	if v, ok := h.attr("lost_evidence"); !ok || v.Int64() != 2 {
		t.Errorf("the shutdown did not report how many EVIDENCE obligations were lost; captured: %v", h.attrs)
	}
	if v, ok := h.attr("durability"); !ok || !strings.Contains(v.String(), "not crash-durable") {
		t.Errorf("the shutdown did not state that the queue is not crash-durable; captured: %v", h.attrs)
	}
}

// TestDeferredWriteShutdown_CleanDrainReportsNothing is the control.
//
// A clean drain loses nothing, so the shutdown must NOT report a loss.
func TestDeferredWriteShutdown_CleanDrainReportsNothing(t *testing.T) {
	h := &shutdownCaptureHandler{}
	previous := slog.Default()
	slog.SetDefault(slog.New(h))
	t.Cleanup(func() { slog.SetDefault(previous) })

	closed := false
	deferredWriteShutdown(deferredWriteShutdownDeps{
		flush: func(ctx context.Context) error { return nil },
		close: func() core.DeferredWriteStats {
			closed = true
			return core.DeferredWriteStats{Accepted: 3, Landed: 3}
		},
	})

	if !closed {
		t.Fatal("the shutdown did not close the queue")
	}
	if h.contains("LOST at shutdown") {
		t.Errorf("the shutdown reported a loss on a clean drain; captured: %v", h.msgs)
	}
	if h.contains("did not all land") {
		t.Errorf("the shutdown reported a flush failure on a clean drain; captured: %v", h.msgs)
	}
}

// The shutdown's flush budget must be bounded: a shutdown must not hang
// on a contended database.
func TestDeferredWriteShutdown_FlushBudgetIsBounded(t *testing.T) {
	h := &shutdownCaptureHandler{}
	previous := slog.Default()
	slog.SetDefault(slog.New(h))
	t.Cleanup(func() { slog.SetDefault(previous) })

	deferredWriteShutdown(deferredWriteShutdownDeps{
		flush: func(ctx context.Context) error {
			// A flush that never finishes on its own must still be
			// bounded by the context.
			<-ctx.Done()
			return ctx.Err()
		},
		close: func() core.DeferredWriteStats { return core.DeferredWriteStats{} },
	})

	// The flush returned within the budget, so the call completed.
	if !h.contains("did not all land") {
		t.Errorf("the shutdown did not report the exhausted flush budget; captured: %v", h.msgs)
	}
	if deferredWriteFlushBudget <= 0 || deferredWriteFlushBudget > 30*time.Second {
		t.Errorf("the flush budget is %s; it must be a short, bounded pause", deferredWriteFlushBudget)
	}
}
