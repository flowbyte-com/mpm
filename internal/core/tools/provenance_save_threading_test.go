// provenance_save_threading_test.go — regression for Task 5b:
// handleSaveToMemory must thread ActiveContext.FrameworkName into the
// artifact_provenance.framework_name column.
package tools

import (
	"testing"

	mpminternal "github.com/flowbyte-com/mpm-core"
)

func TestSaveMemory_PersistsFrameworkNameFromActiveContext(t *testing.T) {
	dm := newTestIsolatedDM(t)

	// Route through handleMpmMemory so the full handler chain runs:
	// handleMpmMemory → handleSaveToMemory → SaveMemoryWithContextAndSnapshot
	result, err := handleMpmMemory(dm, mpminternal.ActiveContext{
		SessionID:    "test-session",
		FrameworkName: "opencode",
	}, map[string]interface{}{
		"action": "save",
		"params": map[string]interface{}{
			"fact":      "opencode thread test",
			"collection": "memories",
			"tags":      []interface{}{"provenance-test"},
		},
	})
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	res, ok := result.(map[string]interface{})
	if !ok {
		t.Fatalf("expected map result, got %T", result)
	}
	id, ok := res["id"].(string)
	if !ok {
		t.Fatalf("expected id string in result, got %T", res["id"])
	}

	var fw string
	if err := dm.SQLDB().QueryRow(
		`SELECT framework_name FROM artifact_provenance WHERE artifact_id = ? AND artifact_type = 'memory'`,
		id,
	).Scan(&fw); err != nil {
		t.Fatalf("provenance read-back: %v", err)
	}
	if fw != "opencode" {
		t.Errorf("artifact_provenance.framework_name = %q, want opencode", fw)
	}
}
