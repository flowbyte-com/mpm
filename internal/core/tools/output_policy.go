package tools

import (
	"context"
	"log/slog"
	"os"
	"strconv"
)

type Decision int

const (
	DecisionPass  Decision = iota // serialized_bytes <= MPM_MCP_MAX_RESULT_BYTES
	DecisionSpill                 // serialized_bytes > MPM_MCP_MAX_RESULT_BYTES
)

type OutputPolicy interface {
	// Apply decides whether a serialized result should be returned directly
	// (DecisionPass) or spilled to blob store (DecisionSpill). The caller
	// is responsible for marshaling once and passing the serialized bytes;
	// this method only measures length — it does not marshal.
	Apply(ctx context.Context, serialized []byte) (Decision, int, error)
}

type defaultOutputPolicy struct {
	threshold int
}

// DefaultOutputPolicy returns an OutputPolicy with the threshold sourced from
// MPM_MCP_MAX_RESULT_BYTES (parsed as base-10 int). Values <= 0 trigger a
// warning and fall back to the new default of 20480. The default was
// raised from 10240 → 20480 after the pointer-indirection audit
// (docs/pointer-indirection-audit-2026-09-05.md Item 3b) showed that
// 73 % of historical spills landed within 2× the 10240 boundary: the
// old default was forcing a round-trip-and-resolve on response payloads
// that fit comfortably in one inline return.
func DefaultOutputPolicy() OutputPolicy {
	const defaultThreshold = 20480
	threshold := defaultThreshold
	if e := os.Getenv("MPM_MCP_MAX_RESULT_BYTES"); e != "" {
		v, err := strconv.ParseInt(e, 10, 64)
		if err != nil {
			slog.Warn("invalid MPM_MCP_MAX_RESULT_BYTES, using default",
				"value", e, "fallback", defaultThreshold)
		} else if v <= 0 {
			slog.Warn("non-positive MPM_MCP_MAX_RESULT_BYTES, using default",
				"value", e, "fallback", defaultThreshold)
		} else {
			threshold = int(v)
		}
	}
	return &defaultOutputPolicy{threshold: threshold}
}

func (p *defaultOutputPolicy) Apply(ctx context.Context, serialized []byte) (Decision, int, error) {
	// Check context cancellation before measuring.
	if err := ctx.Err(); err != nil {
		return 0, 0, err
	}

	n := len(serialized)
	if n > p.threshold {
		return DecisionSpill, n, nil
	}
	return DecisionPass, n, nil
}

// DefaultOutputThresholdBytes exposes the configured MCP output boundary
// (same env var and fallback as DefaultOutputPolicy) so handlers with their
// own pagination can choose default page sizes that cooperate with the
// transport boundary instead of deterministically tripping it.
// D4 fix (2026-08-25): mpm_blob_read's 50 KB default page exceeded this
// boundary (default 10240), so every unbounded read of a mid-size blob
// self-spilled into a recursive pointer envelope. This helper does NOT
// weaken the boundary — Apply remains the enforcement point.
// (2026-09-05 pointer-audit follow-up: default raised to 20480.)
func DefaultOutputThresholdBytes() int {
	const fallback = 20480
	p := DefaultOutputPolicy()
	dp, ok := p.(*defaultOutputPolicy)
	if !ok {
		return fallback
	}
	return dp.threshold
}
