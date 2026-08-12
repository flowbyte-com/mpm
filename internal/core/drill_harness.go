// drill_harness.go — synthetic harness for behavioural drill runs.
//
// Real frameworks (Hermes, OpenCode, Pi) will eventually be wrapped by
// a separate harness that spawns the framework as a subprocess and
// captures its `mpm call` invocations via the tool_invocations table.
// Until v1.1, the only path is `synthetic` — a deterministic in-process
// emitter that demonstrates both a PASS (compliant=true) and a FAIL
// (compliant=false) so the drill pipeline has proof-of-life end to end.

package internal

import (
	"context"

	"github.com/google/uuid"
)

// ToolCall is the minimal record of one dispatch — what tool was called,
// with what action, and which invocation UUID gates the audit row.
// Payload carries the JSON payload from `mpm call <tool> --payload ...`
// so the audit row's payload_hash field can be reconstructed.
type ToolCall struct {
	ToolName     string
	Action       string
	Payload      map[string]any
	InvocationID string
}

// SyntheticHarness produces a deterministic sequence of ToolCalls for a
// given drill. The compliant=true path emits Expect.Sequence in order;
// compliant=false skips the first step to demonstrate a FAIL.
//
// Real-framework harness (planned for v1.1) returns ToolCalls derived
// from tool_invocations rows; the contract is identical so the scorer
// stays source-agnostic.
type SyntheticHarness interface {
	Run(ctx context.Context, drill DrillSpec, compliant bool) ([]ToolCall, string, error)
}

// syntheticHarness is the trivial in-process implementation. It does
// NOT execute tool handlers — that's the scheduler's job (drill_handler.go
// shells out to `mpm call` and lets the audit hook persist the row).
type syntheticHarness struct{}

// NewSyntheticHarness returns a fresh SyntheticHarness. Stateful fields
// are scoped to a single Run call so concurrent drills do not interleave.
func NewSyntheticHarness() SyntheticHarness { return &syntheticHarness{} }

// Run produces the tool-call sequence for one drill. sessionID is a
// fresh UUID used by the scheduler to scope tool_invocations queries.
func (h *syntheticHarness) Run(ctx context.Context, drill DrillSpec, compliant bool) ([]ToolCall, string, error) {
	sessionID := uuid.NewString()
	var calls []ToolCall

	sequence := drill.Expect.Sequence
	if !compliant && len(sequence) > 1 {
		// Skip the first step to violate the order contract. This
		// deliberate violation is what makes the non-compliant path
		// detectably different from the compliant one — both paths
		// call the same set of tools, just in different orders.
		sequence = sequence[1:]
	}

	for _, step := range sequence {
		// Bounded by ctx so a stuck harness doesn't hold the scheduler
		// forever. With Len(sequence) <= a few steps the ctx check is
		// effectively never triggered, but the contract is documented.
		if err := ctx.Err(); err != nil {
			return calls, sessionID, err
		}
		calls = append(calls, ToolCall{
			ToolName:     step.Tool,
			Action:       step.Action,
			Payload:      map[string]any{"action": step.Action, "session_id": sessionID},
			InvocationID: uuid.NewString(),
		})
	}
	return calls, sessionID, nil
}
