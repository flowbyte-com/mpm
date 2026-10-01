// partial_result_envelope_test.go — regression for the loss of partial
// results at the MCP transport.
//
// A handler may return a structured result ALONGSIDE a non-nil error.
// The compact action does this on a mid-drain batch failure, and its
// diagnostic is the only way an operator learns which batch failed and
// how much work already committed.
//
// The MCP transport used to return
// mcp.NewToolResultErrorFromErr(name+" failed", err), which carries the
// error string and nothing else — the diagnostic was discarded at the
// point of loss.
//
// The contract pinned here is deliberately narrow, because MCP has a
// protocol obligation that the CLI does not:
//
//   - IsError MUST stay true. A failed operation is never laundered
//     into a successful tool result in order to carry metadata.
//   - The FIRST content block MUST stay the "<tool> failed: <err>" text,
//     so a client that reads only that block sees exactly what it saw
//     before this change.
//   - The structured diagnostic is APPENDED as an additional block.
//
// The helper's own contract is pinned in
// internal/core/tools/partial_result_regression_test.go.

package main

import (
	"encoding/json"
	"testing"

	"github.com/flowbyte-com/mpm-core/tools"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type partialErr struct{}

func (partialErr) Error() string { return "synthesis timeout" }

func partialResultFixture() map[string]interface{} {
	return map[string]interface{}{
		"success":           false,
		"batches_processed": 3,
		"raw_processed":     150,
		"lessons_created":   3,
		"raw_remaining":     110,
		"stop_reason":       "failure",
		"failed_batch":      4,
		"failure_reason":    "synthesis timeout",
	}
}

// TestPartialResultPayload_CarriesDiagnostics: the appended block must
// contain the full diagnostic the handler returned.
func TestPartialResultPayload_CarriesDiagnostics(t *testing.T) {
	content := partialResultPayload(partialResultFixture(), partialErr{})
	require.NotNil(t, content, "a partial result must produce a content block")

	text, ok := content.(mcp.TextContent)
	require.True(t, ok, "expected a TextContent block")
	assert.Contains(t, text.Text, "partial_result", "block must be distinguishable from a primary payload")

	var envelope struct {
		PartialResult map[string]interface{} `json:"partial_result"`
	}
	require.NoError(t, json.Unmarshal([]byte(text.Text), &envelope))

	for field, want := range map[string]interface{}{
		"batches_processed": float64(3),
		"raw_remaining":     float64(110),
		"stop_reason":       "failure",
		"failed_batch":      float64(4),
		"failure_reason":    "synthesis timeout",
	} {
		assert.Equal(t, want, envelope.PartialResult[field], "field %q lost at the MCP transport", field)
	}
}

// TestPartialResultPayload_MarksFailure: `success` is false inside the
// diagnostic block, matching the tool result's IsError=true. A client
// that reads only the appended block must not conclude the call worked.
func TestPartialResultPayload_MarksFailure(t *testing.T) {
	content := partialResultPayload(partialResultFixture(), partialErr{})
	require.NotNil(t, content)

	text := content.(mcp.TextContent)
	var envelope struct {
		PartialResult map[string]interface{} `json:"partial_result"`
	}
	require.NoError(t, json.Unmarshal([]byte(text.Text), &envelope))
	assert.Equal(t, false, envelope.PartialResult["success"])
}

// TestPartialResultPayload_ForcesFailureVerdict: a handler that claims
// success:true alongside its error must not be able to launder the
// failure into a success through the diagnostic block.
//
// The fixture pairs the hostile claim with a real diagnostic field, so
// the block is emitted at all — otherwise this test would be measuring
// the suppression path rather than the verdict.
func TestPartialResultPayload_ForcesFailureVerdict(t *testing.T) {
	hostile := map[string]interface{}{
		"success":       true,
		"error":         "all good",
		"stop_reason":   "failure",
		"failed_batch":  4,
		"raw_remaining": 110,
	}

	content := partialResultPayload(hostile, partialErr{})
	require.NotNil(t, content)

	text := content.(mcp.TextContent)
	var envelope struct {
		PartialResult map[string]interface{} `json:"partial_result"`
	}
	require.NoError(t, json.Unmarshal([]byte(text.Text), &envelope))
	assert.Equal(t, false, envelope.PartialResult["success"],
		"transport verdict must override a handler's self-reported success")
	assert.Equal(t, "synthesis timeout", envelope.PartialResult["error"])
}

// TestPartialResultPayload_NoResultIsSuppressed: the common (nil, err)
// case must produce NO extra block, so the response is byte-identical to
// what it was before this change.
func TestPartialResultPayload_NoResultIsSuppressed(t *testing.T) {
	assert.Nil(t, partialResultPayload(nil, partialErr{}),
		"a nil result carries no diagnostic and must not add a block")
}

// TestPartialResultPayload_ReservedOnlyResultIsSuppressed pins the
// boundary of the suppression rule.
//
// A result made up ENTIRELY of reserved keys collapses to the two-key
// envelope, so the block is suppressed. This is the right outcome: after
// the transport overwrites those keys, the envelope says exactly what
// the error text already says, and a duplicate block would be noise.
//
// The subtle part, and the reason this is pinned: a naive `len(env) > 2`
// check gets the same answer here BY ACCIDENT, but only because the
// reserved keys happen to number two. A result with one reserved key and
// one diagnostic field would be wrongly suppressed by that check while
// carrying real information. tools.HasDiagnosticFields tests for extra
// keys, which is the actual question.
func TestPartialResultPayload_ReservedOnlyResultIsSuppressed(t *testing.T) {
	assert.Nil(t, partialResultPayload(
		map[string]interface{}{"success": true, "error": "all good"},
		partialErr{}),
		"a result carrying only reserved keys adds nothing to the error text")
}

// TestErrorTextForResult_Contract: the primary error text is unchanged.
func TestErrorTextForResult_Contract(t *testing.T) {
	assert.Equal(t, "mpm_memory failed: synthesis timeout",
		tools.ErrorTextForResult("mpm_memory", partialErr{}))
}
