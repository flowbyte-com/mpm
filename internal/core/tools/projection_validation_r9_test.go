// projection_validation_r9_test.go — Round 9 T21 regression.
//
// Pin the wake-context projection validation contract:
//
//   - omitted / nil / ""     → default full projection (no error)
//   - "compact"              → compact projection (no error)
//   - any other string       → ERROR (was silent fall-through)
//   - non-string             → ERROR
//
// Pre-fix T21: read_wake_context with projection="bogus" silently
// fell through to the default full projection and the caller had no
// indication their explicit projection was ignored. Round 9 makes
// the boundary validate.

package tools

import (
	"strings"
	"testing"

	mpminternal "github.com/flowbyte-com/mpm-core"
)

// TestWakeContext_Projection_OmittedDefault exercises the omitted
// path. The pre-fix behavior was already correct here — the bug was
// only on EXPLICIT bad values. Keeping the test pins the default
// behavior so a future maintainer doesn't "tighten" the omitted path
// into an error.
func TestWakeContext_Projection_OmittedDefault(t *testing.T) {
	dm := newTestIsolatedDM(t)

	res, err := handleMpmContext(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"action": "read_wake_context",
		"params": map[string]interface{}{},
	})
	if err != nil {
		t.Fatalf("omitted projection: expected default success, got %v", err)
	}
	if res == nil {
		t.Fatalf("omitted projection: nil result")
	}
}

// TestWakeContext_Projection_EmptyStringDefault same as omitted —
// empty string is treated as omitted at the validation boundary.
func TestWakeContext_Projection_EmptyStringDefault(t *testing.T) {
	dm := newTestIsolatedDM(t)

	res, err := handleMpmContext(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"action": "read_wake_context",
		"params": map[string]interface{}{
			"projection": "",
		},
	})
	if err != nil {
		t.Fatalf("empty projection: expected default success, got %v", err)
	}
	if res == nil {
		t.Fatalf("empty projection: nil result")
	}
}

// TestWakeContext_Projection_CompactAccepted pins the one valid
// projection value.
func TestWakeContext_Projection_CompactAccepted(t *testing.T) {
	dm := newTestIsolatedDM(t)

	res, err := handleMpmContext(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"action": "read_wake_context",
		"params": map[string]interface{}{
			"projection": "compact",
		},
	})
	if err != nil {
		t.Fatalf("compact projection: expected success, got %v", err)
	}
	if res == nil {
		t.Fatalf("compact projection: nil result")
	}
}

// TestWakeContext_Projection_InvalidRejected is the T21 headline:
// an explicit invalid projection must surface a structured error.
// Pre-fix this fell through silently to the default projection.
func TestWakeContext_Projection_InvalidRejected(t *testing.T) {
	dm := newTestIsolatedDM(t)

	for _, bad := range []string{"bogus", "FULL", "verbose", "summary", "compact2", "default"} {
		t.Run(bad, func(t *testing.T) {
			_, err := handleMpmContext(dm, mpminternal.ActiveContext{}, map[string]interface{}{
				"action": "read_wake_context",
				"params": map[string]interface{}{
					"projection": bad,
				},
			})
			if err == nil {
				t.Fatalf("projection=%q: expected error, got success", bad)
			}
			if !strings.Contains(err.Error(), "unknown projection") {
				t.Errorf("projection=%q: error should mention 'unknown projection', got: %v", bad, err)
			}
		})
	}
}

// TestWakeContext_Projection_NonStringRejected pins that a non-string
// value (e.g. the caller accidentally passed a list or map) is also
// rejected at the boundary.
func TestWakeContext_Projection_NonStringRejected(t *testing.T) {
	dm := newTestIsolatedDM(t)

	for _, val := range []interface{}{
		[]interface{}{"compact"},
		map[string]interface{}{"name": "compact"},
		42,
		true,
	} {
		_, err := handleMpmContext(dm, mpminternal.ActiveContext{}, map[string]interface{}{
			"action": "read_wake_context",
			"params": map[string]interface{}{
				"projection": val,
			},
		})
		if err == nil {
			t.Fatalf("projection=%v: expected error, got success", val)
		}
		if !strings.Contains(err.Error(), "projection must be a string") {
			t.Errorf("projection=%v: error should mention 'projection must be a string', got: %v", val, err)
		}
	}
}
