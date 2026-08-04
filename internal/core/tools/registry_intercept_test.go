// registry_intercept_test.go — pins the interceptor's contract:
//   - cognitive verbs pass through without recording
//   - non-cognitive verbs record into the global buffer
//   - recording happens on both success and error
//
// The interceptor wraps every entry in Registry via init(), so a
// direct unit test exercises the same code path that mcp-mcp and
// `mpm call` will hit at runtime.

package tools

import (
	"errors"
	"testing"

	mpminternal "github.com/flowbyte-com/mpm-core"
)

// fakeHandler returns a fixed result and (optionally) error.
func fakeHandler(result interface{}, err error) HandlerFunc {
	return func(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, payload map[string]interface{}) (interface{}, error) {
		return result, err
	}
}

// resetBuffer wipes the global tool buffer between tests so each
// starts from a clean state. Lives in the tools package test file
// because it needs to reach across the package boundary into
// internal/core for ResetForTest().
func resetBuffer() {
	mpminternal.ResetForTest()
}

func TestIntercept_CognitiveVerb_NoBufferRecord(t *testing.T) {
	resetBuffer()

	called := false
	wrapped := Intercept("save_to_memory", fakeHandler("ok", nil))

	ac := mpminternal.ActiveContext{SessionID: "cog-session"}
	_, err := wrapped(nil, ac, map[string]interface{}{})
	if err != nil {
		t.Fatalf("wrap returned err: %v", err)
	}

	called = true
	_ = called

	// Cognitive verbs must NOT push to the buffer — the observation
	// window would otherwise be polluted by the save itself.
	if got := mpminternal.GlobalToolBuffer().Head("cog-session"); got != nil {
		t.Errorf("cognitive verb %q recorded a buffer entry: %+v", "save_to_memory", got)
	}
}

func TestIntercept_NonCognitiveVerb_RecordsBuffer(t *testing.T) {
	resetBuffer()

	wrapped := Intercept("read_file", fakeHandler("file contents", nil))

	ac := mpminternal.ActiveContext{SessionID: "obs-session"}
	_, err := wrapped(nil, ac, map[string]interface{}{})
	if err != nil {
		t.Fatalf("wrap returned err: %v", err)
	}

	rec := mpminternal.GlobalToolBuffer().Head("obs-session")
	if rec == nil {
		t.Fatal("observation tool did not record into buffer")
	}
	if rec.ToolName != "read_file" {
		t.Errorf("ToolName: want read_file, got %q", rec.ToolName)
	}
	if rec.CallID == "" {
		t.Error("CallID should be auto-filled by interceptor")
	}
	if rec.ResultHash == "" {
		t.Error("ResultHash should be set on successful tool execution")
	}
}

func TestIntercept_FailedToolStillRecorded(t *testing.T) {
	// A failed tool is still an observation — the agent may save
	// "this file doesn't exist" as a memory later. Recording the
	// failure preserves the audit trail.
	resetBuffer()

	wrapped := Intercept("read_file", fakeHandler(nil, errors.New("not found")))

	ac := mpminternal.ActiveContext{SessionID: "fail-session"}
	_, err := wrapped(nil, ac, nil)
	if err == nil {
		t.Fatal("expected the wrapped handler's error to propagate")
	}

	rec := mpminternal.GlobalToolBuffer().Head("fail-session")
	if rec == nil {
		t.Fatal("failed tool was not recorded — observation is lost")
	}
	if rec.ResultHash != "" {
		t.Errorf("ResultHash should be empty on error, got %q", rec.ResultHash)
	}
}

func TestIntercept_EmptySessionIDSilentlySkips(t *testing.T) {
	// CLI invocations and certain unit tests don't carry a session_id.
	// The interceptor must not crash; it just doesn't record.
	resetBuffer()

	wrapped := Intercept("read_file", fakeHandler("ok", nil))

	ac := mpminternal.ActiveContext{SessionID: ""} // empty
	_, _ = wrapped(nil, ac, nil)

	if mpminternal.GlobalToolBuffer().Len() != 0 {
		t.Errorf("empty SessionID should produce no entry, got Len=%d",
			mpminternal.GlobalToolBuffer().Len())
	}
}

// TestRegistryIsWrappedAtInit confirms init() actually fired — every
// tool in Registry should now have its Handler wrapped. We test this
// indirectly by checking that Registry tool handlers exist and that
// the four cognitive-verb / non-cognitive-verb unit tests above pass.
// (Running a real Registry handler with a nil DatabaseManager would
// panic on first DB access; that's not what this test guards.)
func TestRegistryIsWrappedAtInit(t *testing.T) {
	// init() runs at package load time. If it didn't fire, every
	// Handler in Registry would be nil (registry_list.go declares
	// them but init() is what assigns the wrapping).
	for _, tool := range Registry {
		if tool.Handler == nil {
			t.Errorf("Registry tool %q has nil Handler — init() did not run", tool.Name)
		}
	}

	// All known cognitive verbs must remain registered (otherwise
	// the exclusion list is silently incomplete).
	required := []string{"save_to_memory", "record_decision", "propose_theory", "save_lesson", "commit_milestone"}
	for _, name := range required {
		if _, ok := ByName(name); !ok {
			t.Errorf("Registry missing %q — registry_list.go out of sync with cognitiveVerbs", name)
		}
	}
}