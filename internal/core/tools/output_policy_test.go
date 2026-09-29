package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// marshal encodes a value to JSON bytes. Used by tests that need to supply
// pre-serialized bytes to OutputPolicy.Apply.
func marshal(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

func TestOutputPolicy_ThresholdBoundary(t *testing.T) {
	// threshold=10: strict > is needed to spill.
	t.Setenv("MPM_MCP_MAX_RESULT_BYTES", "10")
	policy := DefaultOutputPolicy()

	// 8 bytes -> Pass (8 <= 10)
	dec, n, err := policy.Apply(context.Background(), marshal(map[string]int{"ab": 1}))
	require.NoError(t, err)
	assert.Equal(t, DecisionPass, dec)
	assert.Equal(t, 8, n) // {"ab":1} = 8 bytes

	// 11 bytes -> Spill (11 > 10); {"abcde":1} = 11 bytes
	dec, n, err = policy.Apply(context.Background(), marshal(map[string]int{"abcde": 1}))
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
		input    []byte
		expected Decision
		wantLen  int
	}{
		{"7 bytes", marshal(map[string]int{"a": 1}), DecisionPass, 7},
		{"8 bytes", marshal(map[string]int{"ab": 1}), DecisionPass, 8},
		{"9 bytes", marshal(map[string]int{"abc": 1}), DecisionPass, 9},
		{"10 bytes at boundary", marshal(map[string]int{"abcd": 1}), DecisionPass, 10}, // {"abcd":1} = 10 == threshold
		{"11 bytes over", marshal(map[string]int{"abcde": 1}), DecisionSpill, 11},      // {"abcde":1} = 11 > threshold
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

func TestOutputPolicy_BytesNotModifiedByApply(t *testing.T) {
	// Apply must not modify the serialized bytes passed to it.
	policy := DefaultOutputPolicy()
	original := []byte(`{"key":"value"}`)

	dec, n, err := policy.Apply(context.Background(), original)
	require.NoError(t, err)
	assert.Equal(t, DecisionPass, dec)
	assert.Equal(t, len(original), n)
	assert.Equal(t, `{"key":"value"}`, string(original), "bytes must not be modified by Apply")
}

func TestOutputPolicy_ContextCanceled(t *testing.T) {
	policy := DefaultOutputPolicy()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	dec, n, err := policy.Apply(ctx, marshal(map[string]int{"a": 1}))
	assert.Error(t, err)
	assert.Equal(t, Decision(0), dec)
	assert.Equal(t, 0, n)
	assert.Equal(t, context.Canceled, err)
}

func TestOutputPolicy_NoBlobStoreReference(t *testing.T) {
	// Architecture guard: ensure no blobstore import exists in output_policy.go.
	//
	// 2026-09-14 release-pass: the fallback to cwd-based lookup
	// now also covers the empty-MPM_WORKSPACE case (where the
	// previous fallback only triggered for `/` or `""`). When
	// MPM_WORKSPACE is unset, filepath.Join("", "...path...")
	// produces a relative path that `go test` cannot resolve from
	// the test-binary working directory. Resolve against the source
	// directory via runtime.Caller so the test is hermetic and does
	// not require MPM_WORKSPACE in the environment.
	//
	// MPM_WORKSPACE is honoured when set so operators can verify
	// against a non-standard install layout.
	ws := os.Getenv("MPM_WORKSPACE")
	var thisFile string
	if ws != "" {
		thisFile = filepath.Join(ws, "internal/core/tools/output_policy.go")
	}
	if thisFile == "" {
		// Resolve output_policy.go relative to this test file's
		// source directory. runtime.Caller(0) returns this test
		// file's path; the source file sits next to it.
		_, thisTestFile, _, ok := runtime.Caller(0)
		if !ok {
			t.Fatalf("could not resolve test file path")
		}
		thisFile = filepath.Join(filepath.Dir(thisTestFile), "output_policy.go")
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
	dec, n, err := policy.Apply(context.Background(), marshal(map[string]int{"a": 1}))
	require.NoError(t, err)
	assert.Equal(t, DecisionPass, dec)
	assert.Equal(t, 7, n)
}

func TestOutputPolicy_EnvVarOverride(t *testing.T) {
	t.Setenv("MPM_MCP_MAX_RESULT_BYTES", "1024")
	policy := DefaultOutputPolicy()

	dec, _, err := policy.Apply(context.Background(), marshal(map[string]int{"a": 1}))
	require.NoError(t, err)
	assert.Equal(t, DecisionPass, dec)

	large := map[string]string{"data": string(make([]byte, 2000))}
	dec, n, err := policy.Apply(context.Background(), marshal(large))
	require.NoError(t, err)
	assert.Equal(t, DecisionSpill, dec)
	assert.True(t, n > 1024)
}

func TestOutputPolicy_EnvVarInvalid(t *testing.T) {
	t.Setenv("MPM_MCP_MAX_RESULT_BYTES", "not-a-number")
	policy := DefaultOutputPolicy()

	dec, n, err := policy.Apply(context.Background(), marshal(map[string]int{"a": 1}))
	require.NoError(t, err)
	assert.Equal(t, DecisionPass, dec)
	assert.Equal(t, 7, n) // falls back to 10240
}

func TestOutputPolicy_EnvVarZero(t *testing.T) {
	// Zero is <= 0, so falls back to 10240.
	t.Setenv("MPM_MCP_MAX_RESULT_BYTES", "0")
	policy := DefaultOutputPolicy()

	dec, _, err := policy.Apply(context.Background(), marshal(map[string]int{"a": 1}))
	require.NoError(t, err)
	assert.Equal(t, DecisionPass, dec)
}

func TestOutputPolicy_NegativeEnvVar(t *testing.T) {
	t.Setenv("MPM_MCP_MAX_RESULT_BYTES", "-5")
	policy := DefaultOutputPolicy()

	dec, _, err := policy.Apply(context.Background(), marshal(map[string]int{"a": 1}))
	require.NoError(t, err)
	assert.Equal(t, DecisionPass, dec)
}

// TestOutputPolicy_BytesMeasuredEqualBytesReturned proves the single-marshal
// guarantee: the bytes returned by Apply are exactly the json.Marshal bytes,
// not a second marshal of the result. This is the Phase 1 no-waste invariant.
func TestOutputPolicy_BytesMeasuredEqualsJsonMarshal(t *testing.T) {
	t.Setenv("MPM_MCP_MAX_RESULT_BYTES", "1024")
	policy := DefaultOutputPolicy()

	// Large result that spills.
	result := map[string]string{"data": string(make([]byte, 2000))}
	decision, bytes, err := policy.Apply(context.Background(), marshal(result))
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

	input := marshal(map[string]int{"items": 1})

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
//
// Population this guard inspects
//
//   - cmd/mpm-mcp/tools.go — exactly one file; the MCP server must
//     reference outputPolicy_.Apply.
//   - every NON-TEST .go file directly in cmd/mpm — none may reference
//     OutputPolicy (unless the file carries an explicit `// OutputPolicy`
//     acknowledgement comment, which is the documented opt-out).
//
// # Why the population is resolved, not guessed
//
// The pre-2026-09-29 version of this guard could inspect nothing and
// still report success, in two independent ways:
//
//  1. It read `filepath.Join(os.Getenv("MPM_WORKSPACE"), "cmd/mpm-mcp/tools.go")`.
//     With MPM_WORKSPACE unset that is a RELATIVE path, and the guard's
//     only fallback tested `mcpToolsFile == "/" || mcpToolsFile == ""` —
//     which is never true for a relative path. The read therefore failed,
//     and the failure was answered with t.Skipf. On an ordinary `go test`
//     run the guard reported SKIP: it had never once checked anything.
//     A branch that performs no check and reports success is worse than
//     an absent guard, because the absent guard is visible in review
//     and the skip is not.
//
//  2. It globbed `filepath.Join(os.Getenv("MPM_WORKSPACE"), "cmd/mpm/*.go")`
//     and wrapped the whole loop in `if err == nil && len(cliFiles) > 0`.
//     With MPM_WORKSPACE unset the glob returns zero matches, so the
//     entire CLI half of the guard never executed — with no message at
//     all, not even a skip.
//
// Both halves now resolve through guardRepoRoot (compiled-in source
// location, independent of the process working directory) and
// nonTestGoFilesIn, which returns an error for a missing or empty
// population instead of an empty success. An unreadable scope is a test
// FAILURE. See guard_scopes_test.go for the scope contract and its
// regression tests.
func TestOutputPolicy_OnlyMCPEnforces(t *testing.T) {
	root := guardRepoRoot(t)

	// The MCP server MUST call outputPolicy_.Apply.
	mcpToolsFile := filepath.Join(root, "cmd", "mpm-mcp", "tools.go")
	mcpContent, err := os.ReadFile(mcpToolsFile)
	if err != nil {
		t.Fatalf("cannot read the MCP server's policy enforcement point %s: %v\n"+
			"This guard exists to prove the MCP server applies the output policy. If that file "+
			"moved or was renamed, update this guard to its new location — do not delete or skip it.",
			mcpToolsFile, err)
	}
	if !strings.Contains(string(mcpContent), "outputPolicy_.Apply") {
		t.Errorf("%s must call outputPolicy_.Apply; without it the MCP server does not enforce the "+
			"output policy and oversized tool results are returned unsized", mcpToolsFile)
	}

	// The CLI (cmd/mpm) must NOT call OutputPolicy.Apply.
	cliFiles, err := nonTestGoFilesIn(filepath.Join(root, "cmd", "mpm"))
	if err != nil {
		t.Fatalf("cannot establish the CLI scan population: %v\n"+
			"The CLI half of this guard is now a hard failure rather than a silently skipped loop, "+
			"because a zero-file glob is indistinguishable from a passing guard.", err)
	}

	for _, f := range cliFiles {
		content, err := os.ReadFile(f)
		if err != nil {
			// A file we just listed but cannot read is a broken checkout,
			// not a reason to silently reduce the population.
			t.Errorf("cannot read %s, which is inside the CLI scan population: %v", f, err)
			continue
		}
		if strings.Contains(string(content), "OutputPolicy") && !strings.Contains(string(content), "// OutputPolicy") {
			t.Errorf("cmd/mpm file %q must not reference OutputPolicy; only the MCP server applies the "+
				"output policy (a deliberate reference needs an explicit `// OutputPolicy` comment "+
				"acknowledging it)", filepath.Base(f))
		}
	}
}
