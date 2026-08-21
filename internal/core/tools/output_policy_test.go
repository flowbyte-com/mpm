package tools

import (
	"context"
	"os"
	"path/filepath"
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
