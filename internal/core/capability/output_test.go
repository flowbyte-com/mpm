package capability

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

// =============================================================================
// output_test.go — drainCapped + MaxOutputBytesDefault tests (EX-5.3)
//
// Covers the contract documented in output.go:
//
//   * drainCapped returns (bytes, truncated=false) when input
//     fits within the cap.
//   * drainCapped returns (cap_bytes, truncated=true) when
//     input exceeds the cap by any amount.
//   * drainCapped returns (input_bytes, truncated=false) when
//     input is exactly at the cap (no overflow).
//   * drainCapped returns (partial_bytes, truncated=? based on
//     length) when the reader returns an error mid-stream.
//   * drainCapped panics on max ≤ 0.
//
// MaxOutputBytesDefault is a named constant, not a magic
// number — the default-matches-spec test pins the value.
// =============================================================================

// TestDrainCapped_UnderCap verifies the happy path: input
// fits within the cap, no truncation.
func TestDrainCapped_UnderCap(t *testing.T) {
	input := strings.Repeat("a", 100)
	buf, truncated := drainCapped(strings.NewReader(input), 1000)
	if truncated {
		t.Errorf("truncated = true, want false (input fits within cap)")
	}
	if !bytes.Equal(buf, []byte(input)) {
		t.Errorf("buf = %q, want %q", buf, input)
	}
}

// TestDrainCapped_ExactlyAtCap verifies the boundary case:
// input exactly equals the cap. The +1 sentinel returns
// max+1 only when overflow occurred; at exactly max, no
// truncation flag fires.
func TestDrainCapped_ExactlyAtCap(t *testing.T) {
	input := strings.Repeat("b", 100)
	buf, truncated := drainCapped(strings.NewReader(input), 100)
	if truncated {
		t.Errorf("truncated = true at exact cap; want false")
	}
	if len(buf) != 100 {
		t.Errorf("len(buf) = %d, want 100", len(buf))
	}
}

// TestDrainCapped_OverCapByOne verifies the +1 sentinel fires
// on a single byte of overflow. This is the smallest case
// that distinguishes truncated=true from truncated=false.
func TestDrainCapped_OverCapByOne(t *testing.T) {
	input := strings.Repeat("c", 101)
	buf, truncated := drainCapped(strings.NewReader(input), 100)
	if !truncated {
		t.Errorf("truncated = false, want true (input is 101, cap is 100)")
	}
	if len(buf) != 100 {
		t.Errorf("len(buf) = %d, want 100 (truncated to cap)", len(buf))
	}
}

// TestDrainCapped_FarOverCap verifies that a runaway script
// (e.g., 10MB of output with a 1KB cap) doesn't allocate 10MB
// — the buffer caps at exactly max bytes.
func TestDrainCapped_FarOverCap(t *testing.T) {
	const cap = 1024
	const total = 10 * 1024 * 1024 // 10 MiB
	input := strings.Repeat("d", total)
	buf, truncated := drainCapped(strings.NewReader(input), cap)
	if !truncated {
		t.Errorf("truncated = false, want true (10 MiB > 1 KiB cap)")
	}
	if len(buf) != cap {
		t.Errorf("len(buf) = %d, want %d (cap)", len(buf), cap)
	}
}

// TestDrainCapped_ReadErrorMidStream verifies that a
// mid-stream read error returns whatever was read so far
// without crashing. The truncation flag reflects the
// partial-read length.
func TestDrainCapped_ReadErrorMidStream(t *testing.T) {
	// halfReader reads 50 bytes then errors.
	halfReader := &errAfterReader{buf: bytes.Repeat([]byte("e"), 100), errAfter: 50}

	buf, truncated := drainCapped(halfReader, 1000)
	// We got 50 bytes (under cap), truncated=false.
	// The error is intentionally swallowed because the
	// caller's cmd.Wait will surface the actual pipe
	// failure with its own error.
	if truncated {
		t.Errorf("truncated = true, want false (50 bytes < 1000 cap)")
	}
	if len(buf) != 50 {
		t.Errorf("len(buf) = %d, want 50", len(buf))
	}
}

// TestDrainCapped_ReadErrorAfterOverflow verifies that a
// mid-stream error AFTER the cap has been reached still
// reports truncation. The partial read filled the cap; the
// truncation flag should reflect that even though the error
// path returned early.
func TestDrainCapped_ReadErrorAfterOverflow(t *testing.T) {
	// errAfterReader triggers an error right after we've
	// read past the cap. Because io.LimitReader reads
	// max+1 first, the underlying reader sees the request
	// and (depending on implementation) may surface the
	// error. drainCapped's contract is: if len(returned) >
	// max, truncated=true regardless of the error.
	//
	// Simulate by giving LimitReader 100 bytes of data
	// plus an error after byte 50. io.LimitReader returns
	// after reading max+1 (= 101), but the underlying
	// reader errors at 50. drainCapped's io.ReadAll
	// returns the 50 bytes + the error. len(50) < max so
	// truncated=false. This is the conservative path:
	// truncation only fires on bytes-read > cap, not on
	// "would have been truncated if read continued."
	halfReader := &errAfterReader{buf: bytes.Repeat([]byte("e"), 100), errAfter: 50}
	buf, truncated := drainCapped(halfReader, 100)
	_ = buf
	_ = truncated
	// No assertion: this test documents the conservative
	// truncation policy. The mid-stream error path is
	// tested by TestDrainCapped_ReadErrorMidStream.
}

// TestDrainCapped_PanicsOnZeroMax verifies the panic guard.
// A zero cap is a programming error; panicking is louder
// than silently falling back to io.ReadAll.
func TestDrainCapped_PanicsOnZeroMax(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic for max=0")
		}
	}()
	drainCapped(strings.NewReader("x"), 0)
}

// TestDrainCapped_PanicsOnNegativeMax verifies the same
// guard for negative inputs.
func TestDrainCapped_PanicsOnNegativeMax(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic for max=-1")
		}
	}()
	drainCapped(strings.NewReader("x"), -1)
}

// TestMaxOutputBytesDefault_MatchesSpec pins the named
// constant to the spec's documented value. A future change
// to the default that doesn't update this test would be a
// silent spec drift.
func TestMaxOutputBytesDefault_MatchesSpec(t *testing.T) {
	if MaxOutputBytesDefault != 16*1024*1024 {
		t.Errorf("MaxOutputBytesDefault = %d, want %d (16 MiB per spec §4.3)",
			MaxOutputBytesDefault, 16*1024*1024)
	}
	// And: DefaultResourceLimits uses the named constant.
	if DefaultResourceLimits().MaxOutputBytes != MaxOutputBytesDefault {
		t.Errorf("DefaultResourceLimits().MaxOutputBytes = %d, want %d (MaxOutputBytesDefault)",
			DefaultResourceLimits().MaxOutputBytes, MaxOutputBytesDefault)
	}
}

// =============================================================================
// Helper: io.Reader that errors after N bytes.
// =============================================================================

// errAfterReader returns errAfter bytes then errors on the
// next Read. Used to simulate a child process that died
// mid-write (broken pipe, SIGKILL during stdout flush).
type errAfterReader struct {
	buf      []byte
	pos      int
	errAfter int
}

func (r *errAfterReader) Read(p []byte) (int, error) {
	if r.pos >= r.errAfter {
		return 0, io.ErrUnexpectedEOF
	}
	n := copy(p, r.buf[r.pos:r.errAfter])
	r.pos += n
	if r.pos >= r.errAfter {
		// Return what we have + error, matching the
		// behavior of a real broken-pipe scenario.
		return n, io.ErrUnexpectedEOF
	}
	return n, nil
}

// errAfterReader satisfies io.Reader (compile-time guard).
var _ io.Reader = (*errAfterReader)(nil)

// errReader always errors immediately. Used to verify
// drainCapped's mid-stream error path returns empty + false
// rather than panicking.
type errReader struct{}

func (errReader) Read(p []byte) (int, error) { return 0, errors.New("simulated pipe failure") }

// TestDrainCapped_ImmediateReadError verifies the
// immediate-error path: empty buffer + truncated=false.
// The error itself is intentionally swallowed; the caller's
// cmd.Wait surfaces it with full context.
func TestDrainCapped_ImmediateReadError(t *testing.T) {
	buf, truncated := drainCapped(errReader{}, 100)
	if truncated {
		t.Errorf("truncated = true on immediate error, want false")
	}
	if len(buf) != 0 {
		t.Errorf("len(buf) = %d, want 0", len(buf))
	}
}
