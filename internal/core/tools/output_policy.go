package tools

import (
	"context"
	"encoding/json"
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
	Apply(ctx context.Context, result any) (Decision, int, error)
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

func (p *defaultOutputPolicy) Apply(ctx context.Context, result any) (Decision, int, error) {
	// Check context cancellation before marshaling.
	if err := ctx.Err(); err != nil {
		return 0, 0, err
	}

	data, err := json.Marshal(result)
	if err != nil {
		// Marshal failure returns error, not DecisionSpill.
		return 0, 0, err
	}

	n := len(data)
	if n > p.threshold {
		return DecisionSpill, n, nil
	}
	return DecisionPass, n, nil
}
