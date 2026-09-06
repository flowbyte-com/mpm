package tools

import (
	"strings"
	"testing"

	mpminternal "github.com/flowbyte-com/mpm-core"
)

// Part 2A (2026-09-06): public surface for topic unlink. Exercises the
// handler -> DM -> topic_memberships path. Proves:
//
//   1. valid existing link can be removed
//   2. relationship is actually gone after unlink
//   3. repeated unlink is silent success (removed=false on the second call)
//   4. invalid memory id errors at the boundary (precise envelope)
//   5. invalid topic id errors at the boundary (precise envelope)
//   6. unrelated memberships survive
//   7. missing memory_id / topic_id are errors
func TestHandleUnlinkTopic_ValidLink(t *testing.T) {
	dm := newTestSharedDM(t)
	ctx := mpminternal.ActiveContext{}

	memoryID, err := dm.SaveMemory("memories", "memory under topic", "", []string{"alpha"}, nil, nil, false, 5.0)
	if err != nil {
		t.Fatalf("SaveToMemory: %v", err)
	}
	topicID, err := dm.CreateTopic("test topic", "no description", "", "")
	if err != nil {
		t.Fatalf("CreateTopic: %v", err)
	}
	if err := dm.AddMemoryToTopic(memoryID, topicID, "manual"); err != nil {
		t.Fatalf("AddMemoryToTopic: %v", err)
	}

	res, err := handleUnlinkTopic(dm, ctx, map[string]interface{}{
		"memory_id": memoryID,
		"topic_id":  topicID,
	})
	if err != nil {
		t.Fatalf("handleUnlinkTopic: %v", err)
	}
	m := res.(map[string]interface{})
	if m["success"] != true {
		t.Errorf("expected success=true, got %v", m["success"])
	}
	if m["removed"] != true {
		t.Errorf("expected removed=true on existing membership, got %v", m["removed"])
	}
}

func TestHandleUnlinkTopic_RelationshipActuallyGone(t *testing.T) {
	dm := newTestSharedDM(t)
	ctx := mpminternal.ActiveContext{}

	memoryID, _ := dm.SaveMemory("memories", "memory under topic", "", []string{}, nil, nil, false, 5.0)
	topicID, _ := dm.CreateTopic("test topic", "", "", "")
	dm.AddMemoryToTopic(memoryID, topicID, "manual")

	if _, err := handleUnlinkTopic(dm, ctx, map[string]interface{}{
		"memory_id": memoryID,
		"topic_id":  topicID,
	}); err != nil {
		t.Fatalf("first unlink: %v", err)
	}

	// Probe the underlying table directly to confirm the row is gone.
	var n int
	if err := dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM topic_memberships WHERE memory_id = ? AND topic_id = ?`,
		memoryID, topicID,
	).Scan(&n); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if n != 0 {
		t.Errorf("expected 0 membership rows after unlink, got %d", n)
	}
}

func TestHandleUnlinkTopic_RepeatedUnlinkSilentSuccess(t *testing.T) {
	dm := newTestSharedDM(t)
	ctx := mpminternal.ActiveContext{}

	memoryID, _ := dm.SaveMemory("memories", "memory under topic", "", []string{}, nil, nil, false, 5.0)
	topicID, _ := dm.CreateTopic("test topic", "", "", "")
	dm.AddMemoryToTopic(memoryID, topicID, "manual")

	first, err := handleUnlinkTopic(dm, ctx, map[string]interface{}{
		"memory_id": memoryID,
		"topic_id":  topicID,
	})
	if err != nil {
		t.Fatalf("first unlink: %v", err)
	}
	if first.(map[string]interface{})["removed"] != true {
		t.Errorf("first call should report removed=true")
	}

	second, err := handleUnlinkTopic(dm, ctx, map[string]interface{}{
		"memory_id": memoryID,
		"topic_id":  topicID,
	})
	if err != nil {
		t.Fatalf("second unlink should be silent success, got: %v", err)
	}
	if second.(map[string]interface{})["removed"] != false {
		t.Errorf("second call should report removed=false, got %v", second)
	}
	if second.(map[string]interface{})["success"] != true {
		t.Errorf("second call must still report success=true")
	}
}

func TestHandleUnlinkTopic_UnknownMemoryErrors(t *testing.T) {
	dm := newTestSharedDM(t)
	ctx := mpminternal.ActiveContext{}
	topicID, _ := dm.CreateTopic("test topic", "", "", "")

	_, err := handleUnlinkTopic(dm, ctx, map[string]interface{}{
		"memory_id": "mem-does-not-exist",
		"topic_id":  topicID,
	})
	if err == nil {
		t.Fatal("expected error for unknown memory_id")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error should mention 'not found', got: %v", err)
	}
}

func TestHandleUnlinkTopic_UnknownTopicErrors(t *testing.T) {
	dm := newTestSharedDM(t)
	ctx := mpminternal.ActiveContext{}
	memoryID, _ := dm.SaveMemory("memories", "memory under nonexistent topic", "", []string{}, nil, nil, false, 5.0)

	_, err := handleUnlinkTopic(dm, ctx, map[string]interface{}{
		"memory_id": memoryID,
		"topic_id":  "topic-does-not-exist",
	})
	if err == nil {
		t.Fatal("expected error for unknown topic_id")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error should mention 'not found', got: %v", err)
	}
}

func TestHandleUnlinkTopic_UnrelatedMembershipsSurvive(t *testing.T) {
	dm := newTestSharedDM(t)
	ctx := mpminternal.ActiveContext{}

	memA, _ := dm.SaveMemory("memories", "memory A", "", []string{}, nil, nil, false, 5.0)
	memB, _ := dm.SaveMemory("memories", "memory B", "", []string{}, nil, nil, false, 5.0)
	topicX, _ := dm.CreateTopic("topic X", "", "", "")
	topicY, _ := dm.CreateTopic("topic Y", "", "", "")

	// Build a 4-row membership graph: A-X, A-Y, B-X, B-Y.
	for _, m := range []string{memA, memB} {
		for _, tp := range []string{topicX, topicY} {
			if err := dm.AddMemoryToTopic(m, tp, "manual"); err != nil {
				t.Fatalf("seed: %v", err)
			}
		}
	}

	// Remove only A-X.
	if _, err := handleUnlinkTopic(dm, ctx, map[string]interface{}{
		"memory_id": memA,
		"topic_id":  topicX,
	}); err != nil {
		t.Fatalf("unlink A-X: %v", err)
	}

	// Probe the surviving edges.
	type edge struct{ m, tp string }
	wantSurviving := []edge{{memB, topicX}, {memB, topicY}, {memA, topicY}}
	var surviving []edge
	rows, err := dm.SQLDB().Query(`SELECT memory_id, topic_id FROM topic_memberships`)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	for rows.Next() {
		var m, tp string
		rows.Scan(&m, &tp)
		surviving = append(surviving, edge{m, tp})
	}
	if len(surviving) != len(wantSurviving) {
		t.Fatalf("expected %d surviving edges, got %d: %+v", len(wantSurviving), len(surviving), surviving)
	}
	for _, w := range wantSurviving {
		found := false
		for _, s := range surviving {
			if s == w {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing surviving edge %+v", w)
		}
	}
}

func TestHandleUnlinkTopic_RejectsMissingIDs(t *testing.T) {
	dm := newTestSharedDM(t)
	ctx := mpminternal.ActiveContext{}

	// Missing memory_id.
	_, err := handleUnlinkTopic(dm, ctx, map[string]interface{}{
		"topic_id": "anything",
	})
	if err == nil {
		t.Fatal("expected error for missing memory_id")
	}
	if !strings.Contains(err.Error(), "memory_id is required") {
		t.Errorf("error should mention memory_id, got: %v", err)
	}

	// Missing topic_id.
	_, err = handleUnlinkTopic(dm, ctx, map[string]interface{}{
		"memory_id": "anything",
	})
	if err == nil {
		t.Fatal("expected error for missing topic_id")
	}
	if !strings.Contains(err.Error(), "topic_id is required") {
		t.Errorf("error should mention topic_id, got: %v", err)
	}
}
