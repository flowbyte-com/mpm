package tools

import (
	"testing"

	"github.com/flowbyte-com/mpm-core"
)

// cli_acceptance_20260912_handoff_read_test.go — regression pin for the
// 2026-09-12 CLI acceptance pass: `mpm_handoff read` ignored its id
// params and always returned the latest handoff, so a read-after-shred
// returned a DIFFERENT live handoff with success:true.
func TestCLIAcceptance_HandoffReadByID(t *testing.T) {
	dm := newTestSharedDM(t)

	w1, err := handleHandoffWrite(dm, internal.ActiveContext{}, map[string]interface{}{
		"summary": "first handoff",
	})
	if err != nil {
		t.Fatalf("write 1: %v", err)
	}
	id1 := w1.(map[string]interface{})["handoff_id"].(string)
	w2, err := handleHandoffWrite(dm, internal.ActiveContext{}, map[string]interface{}{
		"summary": "second handoff",
	})
	if err != nil {
		t.Fatalf("write 2: %v", err)
	}
	id2 := w2.(map[string]interface{})["handoff_id"].(string)

	// Explicit read must return the REQUESTED handoff, not the latest.
	got, err := handleHandoffRead(dm, internal.ActiveContext{}, map[string]interface{}{
		"handoff_id": id1,
	})
	if err != nil {
		t.Fatalf("read by handoff_id: %v", err)
	}
	h := got.(map[string]interface{})["handoff"].(*internal.Handoff)
	if h.ID != id1 {
		t.Errorf("read handoff_id=%s returned %s (latest %s?) — id param ignored", id1, h.ID, id2)
	}

	// Bare `id` alias per D-8.1 family convention.
	got, err = handleHandoffRead(dm, internal.ActiveContext{}, map[string]interface{}{
		"id": id1,
	})
	if err != nil {
		t.Fatalf("read by id: %v", err)
	}
	h = got.(map[string]interface{})["handoff"].(*internal.Handoff)
	if h.ID != id1 {
		t.Errorf("read id=%s returned %s", id1, h.ID)
	}

	// Unknown id must be clean not-found, never another record.
	got, err = handleHandoffRead(dm, internal.ActiveContext{}, map[string]interface{}{
		"handoff_id": "does-not-exist",
	})
	if err != nil {
		t.Fatalf("read unknown id should not error, got: %v", err)
	}
	m := got.(map[string]interface{})
	if m["handoff"] != nil {
		t.Errorf("read unknown id must yield handoff:nil, got %+v", m["handoff"])
	}

	// No id still returns latest (backward compat).
	got, err = handleHandoffRead(dm, internal.ActiveContext{}, map[string]interface{}{})
	if err != nil {
		t.Fatalf("read latest: %v", err)
	}
	h = got.(map[string]interface{})["handoff"].(*internal.Handoff)
	if h.ID != id2 {
		t.Errorf("read without id must return latest %s, got %s", id2, h.ID)
	}
}
