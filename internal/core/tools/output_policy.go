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
// MPM_MCP_MAX_RESULT_BYTES (parsed as base-10 int).  Values <= 0 trigger a
// warning and fallback to 10240.  Zero means "no limit" (always DecisionPass).
func DefaultOutputPolicy() OutputPolicy {
	threshold := 10240
	if e := os.Getenv("MPM_MCP_MAX_RESULT_BYTES"); e != "" {
		if v, err := strconv.ParseInt(e, 10, 64); err == nil {
			if v <= 0 {
				slog.Warn("invalid MPM_MCP_MAX_RESULT_BYTES, using default", "value", e, "fallback", 10240)
			} else {
				threshold = int(v)
			}
		} else {
			slog.Warn("failed to parse MPM_MCP_MAX_RESULT_BYTES, using default", "value", e, "fallback", 10240)
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
func DefaultOutputThresholdBytes() int {
	p := DefaultOutputPolicy()
	dp, ok := p.(*defaultOutputPolicy)
	if !ok {
		return 10240
	}
	return dp.threshold
}
