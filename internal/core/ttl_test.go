package internal

import (
	"strings"
	"testing"
	"time"
)

// These tests are not parallel. Each one builds a hermetic manager via
// newTestDM, which pins MPM_WORKSPACE via t.Setenv; t.Setenv panics
// under t.Parallel. A hermetic manager is a process-global environment
// change, and a test that changes the environment cannot run
// concurrently with others doing the same. The tests are microseconds
// each, so nothing is lost by serialising them.

func TestSetMemoryTTL_ExpiresAtColumn(t *testing.T) {
	dm := newTestDM(t)
	id, err := dm.SaveMemory("default", "ttl-test-content", "", nil, nil, nil, false, 1)
	if err != nil {
		t.Fatalf("SaveMemory: %v", err)
	}

	// Set a TTL one second in the past so it's already expired.
	past := time.Now().Add(-1 * time.Second)
	if err := dm.SetMemoryTTL(id, past); err != nil {
		t.Fatalf("SetMemoryTTL: %v", err)
	}

	// GetMemory must NOT return an expired memory. The "memory not found"
	// domain error (mapped from sql.ErrNoRows at the DB layer — see
	// F7 regression test) is the expected "not found" signal — both
	// deleted and expired rows.
	mem, err := dm.GetMemory(id)
	if err == nil || err.Error() != "memory not found: "+id {
		t.Fatalf("expected 'memory not found: %s', got err=%v mem=%+v", id, err, mem)
	}

	// SearchMemories must NOT return it either.
	hits, err := dm.SearchMemories("ttl-test-content", "", false, 10, 0)
	if err != nil {
		t.Fatalf("SearchMemories: %v", err)
	}
	for _, h := range hits {
		if h["id"] == id {
			t.Fatalf("expected expired memory to be hidden from SearchMemories")
		}
	}
}

func TestSetMemoryTTL_FutureExpiryStillVisible(t *testing.T) {
	dm := newTestDM(t)
	id, err := dm.SaveMemory("default", "future-ttl-content", "", nil, nil, nil, false, 1)
	if err != nil {
		t.Fatalf("SaveMemory: %v", err)
	}

	// TTL 1 hour in the future — still readable.
	future := time.Now().Add(1 * time.Hour)
	if err := dm.SetMemoryTTL(id, future); err != nil {
		t.Fatalf("SetMemoryTTL: %v", err)
	}

	mem, err := dm.GetMemory(id)
	if err != nil {
		t.Fatalf("GetMemory: %v", err)
	}
	if mem == nil {
		t.Fatalf("expected future-TTL memory to be readable")
	}
	if !strings.Contains(mem["content"].(string), "future-ttl-content") {
		t.Fatalf("content mismatch: %v", mem["content"])
	}
}

func TestSetMemoryTTL_ClearWithZeroTime(t *testing.T) {
	dm := newTestDM(t)
	id, err := dm.SaveMemory("default", "clear-ttl-content", "", nil, nil, nil, false, 1)
	if err != nil {
		t.Fatalf("SaveMemory: %v", err)
	}

	// Set then clear.
	if err := dm.SetMemoryTTL(id, time.Now().Add(-time.Second)); err != nil {
		t.Fatalf("SetMemoryTTL: %v", err)
	}
	if err := dm.SetMemoryTTL(id, time.Time{}); err != nil {
		t.Fatalf("SetMemoryTTL clear: %v", err)
	}

	mem, err := dm.GetMemory(id)
	if err != nil {
		t.Fatalf("GetMemory: %v", err)
	}
	if mem == nil {
		t.Fatalf("expected cleared-TTL memory to be readable")
	}
}

func TestPruneExpired_RemovesExpiredRows(t *testing.T) {
	dm := newTestDM(t)

	liveID, err := dm.SaveMemory("default", "live", "", nil, nil, nil, false, 1)
	if err != nil {
		t.Fatalf("SaveMemory live: %v", err)
	}
	deadID, err := dm.SaveMemory("default", "dead", "", nil, nil, nil, false, 1)
	if err != nil {
		t.Fatalf("SaveMemory dead: %v", err)
	}

	if err := dm.SetMemoryTTL(deadID, time.Now().Add(-time.Hour)); err != nil {
		t.Fatalf("SetMemoryTTL: %v", err)
	}

	pruned, err := dm.PruneExpired()
	if err != nil {
		t.Fatalf("PruneExpired: %v", err)
	}
	if pruned < 1 {
		t.Fatalf("expected at least 1 pruned row, got %d", pruned)
	}

	// Live row still readable.
	mem, err := dm.GetMemory(liveID)
	if err != nil {
		t.Fatalf("GetMemory live: %v", err)
	}
	if mem == nil {
		t.Fatalf("live memory was incorrectly pruned")
	}
}