package capability

import (
	"fmt"
	"io"
)

// =============================================================================
// output.go — capability output capture + truncation (spec §4.3 / EX-5)
//
// Capability invocations can produce arbitrarily large output.
// A misbehaving script — an infinite debug loop, a runaway
// jq pipeline, a debug-print on every iteration — can dump
// gigabytes into stdout or stderr in seconds. Without a
// cap-aware read, the agent's memory grows unbounded and the
// host OOM-kills the agent itself.
//
// drainCapped is the canonical cap-aware reader. It reads up
// to max+1 bytes from r and reports whether the read was
// truncated (truncated=true if the child wrote more than max
// bytes). The +1 trick detects overflow without allocating an
// unbounded buffer for the runaway case: we stop reading as
// soon as we know there's "at least max+1 bytes."
//
// Failure semantics:
//
//   * max ≤ 0 → panic. The Executor resolves limits through
//               resolveLimits + ClampLimits, both of which
//               guarantee max is positive and at-or-below
//               the documented ceiling. A max ≤ 0 here is
//               a programming-error invariant violation;
//               panicking is louder than silently falling
//               back to io.ReadAll (which would be a memory
//               allocation attack vector).
//
//   * Read errors mid-stream → return what we have + truncated
//               status is preserved (the partial read may
//               still be at the cap). The caller's cmd.Wait
//               surfaces the actual error.
//
// Spec: docs/archive/capability-lifecycle.md §4.3 (Output size:
//      16 MB hard cap).
// =============================================================================

// MaxOutputBytesDefault is the default stdout/stderr ceiling
// when neither the request nor the capability metadata
// specifies max_output_bytes. Matches the spec §4.3 default
// of 16 MB. ClampLimits caps this further at 64 MiB to
// defend against absurd metadata values.
const MaxOutputBytesDefault int64 = 16 * 1024 * 1024

// drainCapped reads from r until EOF or until max+1 bytes
// have been consumed. Returns the bytes read (truncated to
// max) and whether the read was truncated (true if r had
// more than max bytes available).
//
// The +1 byte technique: by reading max+1, we know the
// stream was truncated if and only if we got back max+1
// bytes — without needing a separate "is there more" probe.
// The +1 byte is dropped before returning, so the buffer
// length is exactly min(actual, max).
//
// Panics if max ≤ 0. This is intentional: a Driver calling
// drainCapped with a zero or negative cap has violated its
// own contract (the Executor guarantees a positive value).
// Allowing it to fall through to io.ReadAll would silently
// re-introduce the unbounded allocation we're trying to
// prevent.
func drainCapped(r io.Reader, max int64) ([]byte, bool) {
	if max <= 0 {
		panic(fmt.Sprintf(
			"capability: drainCapped: max=%d is non-positive; the Executor must pass a positive MaxOutputBytes",
			max,
		))
	}

	// Step 1: try to read max+1 bytes. If we get exactly
	// max+1, the stream was truncated (there were at least
	// max+1 bytes available). If we get ≤ max, the stream
	// fit entirely within the cap.
	limited := io.LimitReader(r, max+1)

	// Loop until EOF or until we hit the +1 boundary. ReadAll
	// on a LimitReader drains up to the limit and returns nil
	// (no error) when the limit is hit — so the read below
	// returns the full max+1 bytes for a truncated stream.
	full, err := io.ReadAll(limited)
	if err != nil {
		// Mid-stream read error (broken pipe, child died,
		// process killed mid-write). Return what we have so
		// the caller's cmd.Wait can surface the real error;
		// the truncation status is whatever the partial read
		// tells us.
		if int64(len(full)) > max {
			return full[:max], true
		}
		return full, false
	}

	if int64(len(full)) > max {
		// Truncated: the child wrote more than max bytes.
		// Drop the +1 sentinel and report truncation.
		return full[:max], true
	}

	// Not truncated: the entire stream fit within max bytes.
	// Return the buffer unchanged.
	return full, false
}
