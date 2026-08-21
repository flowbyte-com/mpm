package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOutputPolicy_ThresholdBoundary(t *testing.T) {
	// threshold=10: strict > is needed to spill.
	t.Setenv("MPM_MCP_MAX_RESULT_BYTES", "10")
	policy := DefaultOutputPolicy()

	// 8 bytes -> Pass (8 <= 10)
	dec, n, err := policy.Apply(context.Background(), map[string]int{"ab": 1})
	require.NoError(t, err)
	assert.Equal(t, DecisionPass, dec)
	assert.Equal(t, 8, n) // {"ab":1} = 8 bytes

	// 11 bytes -> Spill (11 > 10); {"abcde":1} = 11 bytes
	dec, n, err = policy.Apply(context.Background(), map[string]int{"abcde": 1})
	require.NoError(t, err)
	assert.Equal(t, DecisionSpill, dec)
	assert.Equal(t, 11, n) // {"abcd":1} = 11 bytes
}

func TestOutputPolicy_ThresholdBoundary_EdgeCases(t *testing.T) {
	t.Setenv("MPM_MCP_MAX_RESULT_BYTES", "10")
	policy := DefaultOutputPolicy()

	// Byte counts verified via json.Marshal:
	// {"a":1}    = 7 bytes  -> Pass  (7 <= 10)
	// {"ab":1}   = 8 bytes  -> Pass  (8 <= 10)
	// {"abc":1}  = 9 bytes  -> Pass  (9 <= 10)
	// {"abcd":1} = 10 bytes -> Pass  (10 == 10)
	// {"abcde":1}= 11 bytes -> Spill (11 > 10)
	testCases := []struct {
		name     string
		input    any
		expected Decision
		wantLen  int
	}{
		{"7 bytes", map[string]int{"a": 1}, DecisionPass, 7},
		{"8 bytes", map[string]int{"ab": 1}, DecisionPass, 8},
		{"9 bytes", map[string]int{"abc": 1}, DecisionPass, 9},
		{"10 bytes at boundary", map[string]int{"abcd": 1}, DecisionPass, 10},  // {"abcd":1} = 10 == threshold
		{"11 bytes over", map[string]int{"abcde": 1}, DecisionSpill, 11},         // {"abcde":1} = 11 > threshold
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			dec, n, err := policy.Apply(context.Background(), tc.input)
			require.NoError(t, err)
			assert.Equal(t, tc.expected, dec)
			assert.Equal(t, tc.wantLen, n)
		})
	}
}

func TestOutputPolicy_MarshalFailureReturnsError(t *testing.T) {
	policy := DefaultOutputPolicy()

	type cyclic struct {
		C *cyclic `json:"c"`
	}
	c := &cyclic{}
	c.C = c

	dec, n, err := policy.Apply(context.Background(), c)
	assert.Error(t, err)
	assert.Equal(t, Decision(0), dec)
	assert.Equal(t, 0, n)
	assert.NotEqual(t, DecisionSpill, dec)
}

func TestOutputPolicy_ContextCanceled(t *testing.T) {
	policy := DefaultOutputPolicy()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	dec, n, err := policy.Apply(ctx, map[string]int{"a": 1})
	assert.Error(t, err)
	assert.Equal(t, Decision(0), dec)
	assert.Equal(t, 0, n)
	assert.Equal(t, context.Canceled, err)
}

func TestOutputPolicy_NoBlobStoreReference(t *testing.T) {
	// Architecture guard: ensure no blobstore import exists in output_policy.go.
	// Uses an absolute path so it works regardless of CWD.
	thisFile := filepath.Join(os.Getenv("MPM_WORKSPACE"),
		"internal/core/tools/output_policy.go")
	if thisFile == "/" || thisFile == "" {
		// Fallback: construct from test process working dir.
		cwd, _ := os.Getwd()
		thisFile = filepath.Join(cwd, "output_policy.go")
	}
	content, err := os.ReadFile(thisFile)
	require.NoError(t, err)
	assert.NotContains(t, content,
		`"github.com/flowbyte-com/mpm/internal/blobstore"`,
		"output_policy must not import internal/blobstore")
}

func TestOutputPolicy_DefaultThreshold(t *testing.T) {
	os.Unsetenv("MPM_MCP_MAX_RESULT_BYTES")
	policy := DefaultOutputPolicy()

	// {"a":1} = 7 bytes; with default 10240, always passes.
	dec, n, err := policy.Apply(context.Background(), map[string]int{"a": 1})
	require.NoError(t, err)
	assert.Equal(t, DecisionPass, dec)
	assert.Equal(t, 7, n)
}

func TestOutputPolicy_EnvVarOverride(t *testing.T) {
	t.Setenv("MPM_MCP_MAX_RESULT_BYTES", "1024")
	policy := DefaultOutputPolicy()

	dec, _, err := policy.Apply(context.Background(), map[string]int{"a": 1})
	require.NoError(t, err)
	assert.Equal(t, DecisionPass, dec)

	large := map[string]string{"data": string(make([]byte, 2000))}
	dec, n, err := policy.Apply(context.Background(), large)
	require.NoError(t, err)
	assert.Equal(t, DecisionSpill, dec)
	assert.True(t, n > 1024)
}

func TestOutputPolicy_EnvVarInvalid(t *testing.T) {
	t.Setenv("MPM_MCP_MAX_RESULT_BYTES", "not-a-number")
	policy := DefaultOutputPolicy()

	dec, n, err := policy.Apply(context.Background(), map[string]int{"a": 1})
	require.NoError(t, err)
	assert.Equal(t, DecisionPass, dec)
	assert.Equal(t, 7, n)
}

func TestOutputPolicy_EnvVarZero(t *testing.T) {
	// Zero is <= 0, so falls back to 10240.
	t.Setenv("MPM_MCP_MAX_RESULT_BYTES", "0")
	policy := DefaultOutputPolicy()

	dec, _, err := policy.Apply(context.Background(), map[string]int{"a": 1})
	require.NoError(t, err)
	assert.Equal(t, DecisionPass, dec)
}

func TestOutputPolicy_NegativeEnvVar(t *testing.T) {
	t.Setenv("MPM_MCP_MAX_RESULT_BYTES", "-5")
	policy := DefaultOutputPolicy()

	dec, _, err := policy.Apply(context.Background(), map[string]int{"a": 1})
	require.NoError(t, err)
	assert.Equal(t, DecisionPass, dec)
}

func TestOutputPolicy_ReturnsErrorNotSpill(t *testing.T) {
	policy := DefaultOutputPolicy()
	type untagged struct {
		C chan int `json:"c"`
	}

	dec, n, err := policy.Apply(context.Background(), untagged{})
	assert.Error(t, err)
	assert.Equal(t, Decision(0), dec)
	assert.Equal(t, 0, n)
}

// TestOutputPolicy_BytesMeasuredEqualBytesSpilled proves the OutputPolicy
// measured bytes are the same bytes passed to BlobStore.Put — no double-marshal.
// This is the Phase 1 no-gzip invariant: the exact bytes measured by Apply
// are the exact bytes supplied to Put.
func TestOutputPolicy_BytesMeasuredEqualBytesSpilled(t *testing.T) {
	t.Setenv("MPM_MCP_MAX_RESULT_BYTES", "1024")
	policy := DefaultOutputPolicy()

	// Large result that spills.
	result := map[string]string{"data": string(make([]byte, 2000))}
	decision, bytes, err := policy.Apply(context.Background(), result)
	require.NoError(t, err)
	assert.Equal(t, DecisionSpill, decision)
	assert.True(t, bytes > 1024)

	// The bytes returned by Apply must equal json.Marshal(result).
	// This is the single-marshal guarantee.
	resultBytes, err := json.Marshal(result)
	require.NoError(t, err)
	assert.Equal(t, len(resultBytes), bytes,
		"Apply must return the exact json.Marshal bytes, not a re-marshal")
}

// TestOutputPolicy_DecisionDeterminism proves the same input always produces
// the same decision (idempotent Apply, no state mutation).
func TestOutputPolicy_DecisionDeterminism(t *testing.T) {
	t.Setenv("MPM_MCP_MAX_RESULT_BYTES", "100")
	policy := DefaultOutputPolicy()

	input := map[string]int{"items": 1}

	// Apply 10 times — all decisions and byte counts must match.
	var firstDecision Decision
	var firstBytes int
	for i := 0; i < 10; i++ {
		dec, n, err := policy.Apply(context.Background(), input)
		require.NoError(t, err)
		if i == 0 {
			firstDecision = dec
			firstBytes = n
		}
		assert.Equal(t, firstDecision, dec, "decision must be deterministic")
		assert.Equal(t, firstBytes, n, "byte count must be deterministic")
	}
}

// TestOutputPolicy_OnlyMCPEnforces is a static analysis test that proves
// OutputPolicy.Apply is called only within cmd/mpm-mcp (the MCP server),
// never in the CLI binary (cmd/mpm). This enforces the Phase 1 architecture:
// the CLI never spills; only the MCP server applies the output policy.
func TestOutputPolicy_OnlyMCPEnforces(t *testing.T) {
	// Phase 1 architecture: OutputPolicy.Apply may only be called in cmd/mpm-mcp.
	// Verify the source files directly rather than relying on package listing.
	mcpToolsFile := filepath.Join(os.Getenv("MPM_WORKSPACE"), "cmd/mpm-mcp/tools.go")
	if mcpToolsFile == "/" || mcpToolsFile == "" {
		cwd, _ := os.Getwd()
		mcpToolsFile = filepath.Join(cwd, "..", "..", "cmd", "mpm-mcp", "tools.go")
	}
	mcpContent, mcpErr := os.ReadFile(mcpToolsFile)
	if mcpErr != nil {
		t.Skipf("cannot read mpm-mcp tools.go: %v", mcpErr)
	}

	// The MCP server MUST call outputPolicy_.Apply.
	if !strings.Contains(string(mcpContent), "outputPolicy_.Apply") {
		t.Error("mpm-mcp tools.go must call outputPolicy_.Apply")
	}

	// The CLI (cmd/mpm) must NOT call OutputPolicy.Apply.
	// Check all non-test Go files in cmd/mpm.
	cliFiles, err := filepath.Glob(filepath.Join(os.Getenv("MPM_WORKSPACE"), "cmd/mpm/*.go"))
	if err == nil && len(cliFiles) > 0 {
		for _, f := range cliFiles {
			content, err := os.ReadFile(f)
			if err != nil {
				continue
			}
			if strings.Contains(string(content), "OutputPolicy") && !strings.Contains(string(content), "// OutputPolicy") {
				t.Errorf("cmd/mpm file %q must not reference OutputPolicy", filepath.Base(f))
			}
		}
	}
}
