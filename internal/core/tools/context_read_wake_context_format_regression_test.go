// context_read_wake_context_format_regression_test.go — Residual
// pass §I-C.16.
//
// The 2026-09-05 audit found mpm_context.read_wake_context silently
// fell through to the JSON envelope when format was anything other
// than "system-prompt". A caller asking format="bogus" got the JSON
// envelope back without any indication their explicit format
// choice was ignored — the handler's `if format == "system-prompt"`
// branch was the only check, and the default path was the
// fall-through.
//
// Pre-fix reproduction (live CLI):
//
//   $ mpm call mpm_context --payload '{"action":"read_wake_context","params":{"format":"bogus"}}'
//     # wanted: error mentioning the format enum
//     # actual: success, JSON envelope returned as if format was omitted
//
// Canonical contract (per registry schema enum [system-prompt]
// and the §I-C.16 brief):
//
//   omitted / nil / ""  → JSON envelope (the existing default)
//   "system-prompt"     → prose envelope (existing positive path)
//   any other string    → ERROR: format must be one of [system-prompt]
//   non-string          → ERROR (type mismatch)

package tools

import (
	"strings"
	"testing"

	mpminternal "github.com/flowbyte-com/mpm-core"
)

// TestWakeContextFormat_OmittedDefaultsToJSON pins: omitting
// format falls through to the JSON envelope, preserving the
// existing default for back-compat.
func TestWakeContextFormat_OmittedDefaultsToJSON(t *testing.T) {
	dm := newTestSharedDM(t)

	for _, params := range []map[string]interface{}{
		{},                                       // omitted
		{"format": nil},                          // explicit null
		{"format": ""},                           // explicit empty
	} {
		res, err := handleMpmContext(dm, mpminternal.ActiveContext{}, map[string]interface{}{
			"action": "read_wake_context",
			"params": params,
		})
		if err != nil {
			t.Errorf("omitted/empty/null format must not error: %v", err)
		}
		if res == nil {
			t.Errorf("omitted/empty/null format must produce a context payload")
		}
	}
}

// TestWakeContextFormat_SystemPromptAccepted pins the positive
// path: format="system-prompt" returns the prose envelope.
func TestWakeContextFormat_SystemPromptAccepted(t *testing.T) {
	dm := newTestSharedDM(t)

	res, err := handleMpmContext(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"action": "read_wake_context",
		"params": map[string]interface{}{"format": "system-prompt"},
	})
	if err != nil {
		t.Fatalf("format=system-prompt must succeed: %v", err)
	}
	m, _ := res.(map[string]interface{})
	if m["format"] != "system-prompt" {
		t.Errorf("expected format=system-prompt in envelope, got %v", m["format"])
	}
	if _, ok := m["content"]; !ok {
		t.Errorf("system-prompt envelope must contain 'content' key")
	}
}

// TestWakeContextFormat_InvalidStringRejected pins the headline
// §I-C.16 invariant: a non-canonical format value errors rather
// than silently falling through to JSON.
func TestWakeContextFormat_InvalidStringRejected(t *testing.T) {
	dm := newTestSharedDM(t)

	for _, bad := range []string{"bogus", "json", "markdown", "System-Prompt", "SYSTEM-PROMPT", "0"} {
		t.Run("format="+bad, func(t *testing.T) {
			_, err := handleMpmContext(dm, mpminternal.ActiveContext{}, map[string]interface{}{
				"action": "read_wake_context",
				"params": map[string]interface{}{"format": bad},
			})
			if err == nil {
				t.Fatalf("invalid format %q must error", bad)
			}
			if !strings.Contains(err.Error(), "format") {
				t.Errorf("error must mention 'format', got: %v", err)
			}
			if !strings.Contains(err.Error(), "system-prompt") {
				t.Errorf("error must mention the allowed value, got: %v", err)
			}
		})
	}
}

// TestWakeContextFormat_NonStringRejected pins: a non-string
// value for format errors with a clear type message.
func TestWakeContextFormat_NonStringRejected(t *testing.T) {
	dm := newTestSharedDM(t)

	for _, bad := range []interface{}{123, true, []interface{}{"system-prompt"}} {
		t.Run("type", func(t *testing.T) {
			_, err := handleMpmContext(dm, mpminternal.ActiveContext{}, map[string]interface{}{
				"action": "read_wake_context",
				"params": map[string]interface{}{"format": bad},
			})
			if err == nil {
				t.Fatalf("non-string format (%T) must error", bad)
			}
			if !strings.Contains(err.Error(), "format") {
				t.Errorf("error must mention 'format', got: %v", err)
			}
		})
	}
}

// TestWakeContextFormat_InvalidDoesNotMutate pins the
// no-side-effect invariant: an invalid format returns an error
// without producing a context payload the caller could mistake for
// valid content.
func TestWakeContextFormat_InvalidDoesNotMutate(t *testing.T) {
	dm := newTestSharedDM(t)

	res, err := handleMpmContext(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"action": "read_wake_context",
		"params": map[string]interface{}{"format": "bogus"},
	})
	if err == nil {
		t.Fatal("invalid format must error")
	}
	if res != nil {
		t.Errorf("invalid format must return nil result, got %v", res)
	}
}
