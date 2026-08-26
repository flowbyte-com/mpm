// f18_wake_format_parity_test.go — F18 regression at the tool surface.
//
// JSON (default) and format=system-prompt must derive from the same
// underlying wake data: when a handoff exists, BOTH representations carry
// it; when none exists, both represent absence cleanly.
package tools

import (
	"encoding/json"
	"strings"
	"testing"

	mpminternal "github.com/flowbyte-com/mpm-core"
)

func TestF18_WakeFormatsAgreeOnLastHandoff(t *testing.T) {
	dm := newTestIsolatedDM(t)

	if _, err := dm.EndSession("sess-parity", "parity handoff payload", "clean", nil, nil); err != nil {
		t.Fatalf("EndSession: %v", err)
	}

	// system-prompt branch (ReadWakeContext → prose).
	sp, err := handleReadWakeContext(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"format": "system-prompt",
	})
	if err != nil {
		t.Fatalf("system-prompt wake failed: %v", err)
	}
	spMap := sp.(map[string]interface{})
	content, _ := spMap["content"].(string)
	if !strings.Contains(content, "Previous Session Handoff") || !strings.Contains(content, "parity handoff payload") {
		t.Fatalf("F18 REGRESSION: system-prompt format omitted last_handoff:\n%s", content)
	}

	// JSON branch on a fresh DB + fresh handoff (the first call consumed
	// the previous one by design — read-once delivery).
	dm2 := newTestIsolatedDM(t)
	if _, err := dm2.EndSession("sess-parity-2", "json parity handoff", "clean", nil, nil); err != nil {
		t.Fatalf("EndSession: %v", err)
	}
	j, err := handleReadWakeContext(dm2, mpminternal.ActiveContext{}, map[string]interface{}{})
	if err != nil {
		t.Fatalf("json wake failed: %v", err)
	}
	raw, err := json.Marshal(j)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var wire struct {
		LastHandoff *struct {
			Summary string `json:"summary"`
		} `json:"last_handoff"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if wire.LastHandoff == nil || wire.LastHandoff.Summary != "json parity handoff" {
		t.Fatalf("F18 REGRESSION: json format omitted last_handoff: %s", raw)
	}
}
