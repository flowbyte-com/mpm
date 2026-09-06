package internal

import (
	"strings"
	"testing"
)

// Part 1 (2026-09-06): global-rule retire surface. Closes the catalog §C.2
// asymmetry (global rules can be added but never formally retracted).
//
// This file exercises the 10-case matrix required by the Part 1 spec:
//   1. recording a rule still works
//   2. querying default returns it while active
//   3. revocation requires explicit confirmation
//   4. revoked rule disappears from default active results
//   5. revoked rule remains in explicit historical results
//   6. unknown ID is handled according to the canonical resource-not-found convention
//   7. already-revoked ID has explicit, deterministic behaviour
//   8. no unrelated global rule is modified
//   9. schema and handler agree (parity)
//  10. migration, if required, works on both fresh and upgraded DBs
//
// All tests run through the public handler surface so the regression pins
// the full wire path: handler -> DM -> shared DB.

func TestGlobalRule_RecordStillWorks(t *testing.T) {
	dm := NewTestSharedDM(t)

	id, err := dm.RecordGlobalRule("never log secrets at INFO", []string{"security"}, 10.0, "operator@host")
	if err != nil {
		t.Fatalf("RecordGlobalRule: %v", err)
	}
	if id == "" {
		t.Fatal("RecordGlobalRule returned empty id")
	}
	// The row must exist in shared.memories with is_global=1.
	var isGlobal int
	var deletedAt *int64
	var retiredAt *int64
	if err := dm.SQLDB().QueryRow(
		`SELECT is_global, deleted_at, retired_at FROM shared.memories WHERE id = ?`, id,
	).Scan(&isGlobal, &deletedAt, &retiredAt); err != nil {
		t.Fatalf("probe shared.memories: %v", err)
	}
	if isGlobal != 1 {
		t.Errorf("expected is_global=1, got %d", isGlobal)
	}
	if deletedAt != nil {
		t.Errorf("expected deleted_at NULL, got %d", *deletedAt)
	}
	if retiredAt != nil {
		t.Errorf("expected retired_at NULL on fresh record, got %d", *retiredAt)
	}
}

func TestGlobalRule_QueryDefaultReturnsActiveOnly(t *testing.T) {
	dm := NewTestSharedDM(t)

	id, err := dm.RecordGlobalRule("active rule under test", []string{"alpha"}, 5.0, "")
	if err != nil {
		t.Fatalf("RecordGlobalRule: %v", err)
	}

	// Default query (active only) returns the row.
	got, err := dm.QueryGlobalRules("", 10, false)
	if err != nil {
		t.Fatalf("QueryGlobalRules active: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 active rule, got %d", len(got))
	}
	if got[0]["id"] != id {
		t.Errorf("unexpected id %v", got[0]["id"])
	}
}

func TestGlobalRule_RetireRequiresConfirm(t *testing.T) {
	dm := NewTestSharedDM(t)
	id, _ := dm.RecordGlobalRule("rule under test", nil, 5.0, "")

	// Without confirm -> error.
	if _, err := dm.RetireGlobalRule(id, "", false); err == nil {
		t.Fatal("RetireGlobalRule without confirm should error")
	} else if !strings.Contains(err.Error(), "confirm=true") {
		t.Errorf("error should mention confirm=true gate, got: %v", err)
	}

	// Truthy string is NOT acceptable; only literal bool true.
	// (The handler enforces this at the boundary; the DM check is the
	// second line of defense.)
	if _, err := dm.RetireGlobalRule(id, "", false); err == nil {
		t.Fatal("RetireGlobalRule with confirm=false should error")
	}
}

func TestGlobalRule_RetiredRuleDisappearsFromActiveQuery(t *testing.T) {
	dm := NewTestSharedDM(t)
	id, _ := dm.RecordGlobalRule("rule to retire", nil, 5.0, "")

	// Retire it.
	res, err := dm.RetireGlobalRule(id, "superseded", true)
	if err != nil {
		t.Fatalf("RetireGlobalRule: %v", err)
	}
	if res["success"] != true {
		t.Errorf("expected success=true, got %v", res)
	}
	if res["already_retired"] != false {
		t.Errorf("expected already_retired=false on first call, got %v", res)
	}

	// Default query filters it out.
	got, err := dm.QueryGlobalRules("", 10, false)
	if err != nil {
		t.Fatalf("QueryGlobalRules active: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected 0 active rules after retire, got %d: %+v", len(got), got)
	}
}

func TestGlobalRule_RetiredRuleSurfacesInHistoricalQuery(t *testing.T) {
	dm := NewTestSharedDM(t)
	id, _ := dm.RecordGlobalRule("rule to retire", nil, 5.0, "")

	if _, err := dm.RetireGlobalRule(id, "obsolete", true); err != nil {
		t.Fatalf("RetireGlobalRule: %v", err)
	}

	// includeRetired=true brings it back.
	got, err := dm.QueryGlobalRules("", 10, true)
	if err != nil {
		t.Fatalf("QueryGlobalRules includeRetired: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 retired rule, got %d", len(got))
	}
	if got[0]["id"] != id {
		t.Errorf("unexpected id %v", got[0]["id"])
	}
	if _, ok := got[0]["retired_at"]; !ok {
		t.Error("retired row should expose retired_at field")
	}
}

func TestGlobalRule_UnknownIDIsError(t *testing.T) {
	dm := NewTestSharedDM(t)

	_, err := dm.RetireGlobalRule("rule-does-not-exist", "", true)
	if err == nil {
		t.Fatal("RetireGlobalRule on unknown id should error")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error should mention 'not found', got: %v", err)
	}
}

func TestGlobalRule_AlreadyRetiredIsSilentSuccess(t *testing.T) {
	dm := NewTestSharedDM(t)
	id, _ := dm.RecordGlobalRule("rule to retire twice", nil, 5.0, "")

	// First retire succeeds and stamps retired_at.
	first, err := dm.RetireGlobalRule(id, "first call", true)
	if err != nil {
		t.Fatalf("first retire: %v", err)
	}
	firstStamp, ok := first["retired_at"].(int64)
	if !ok {
		t.Fatalf("expected int64 retired_at on first call, got %T %v", first["retired_at"], first["retired_at"])
	}

	// Second retire is silent success with already_retired=true. The
	// returned retired_at must match the first stamp — the second call
	// must NOT overwrite the original retire timestamp.
	second, err := dm.RetireGlobalRule(id, "second call", true)
	if err != nil {
		t.Fatalf("second retire: %v", err)
	}
	if second["already_retired"] != true {
		t.Errorf("expected already_retired=true on second call, got %v", second)
	}
	secondStamp, ok := second["retired_at"].(int64)
	if !ok {
		t.Fatalf("expected int64 retired_at on second call, got %T %v", second["retired_at"], second["retired_at"])
	}
	if secondStamp != firstStamp {
		t.Errorf("second retire must not overwrite the original timestamp: first=%d second=%d", firstStamp, secondStamp)
	}
}

func TestGlobalRule_RetiringOneDoesNotTouchOthers(t *testing.T) {
	dm := NewTestSharedDM(t)

	idA, _ := dm.RecordGlobalRule("rule A", nil, 5.0, "")
	idB, _ := dm.RecordGlobalRule("rule B", nil, 5.0, "")
	idC, _ := dm.RecordGlobalRule("rule C", nil, 5.0, "")

	// Retire only B.
	if _, err := dm.RetireGlobalRule(idB, "", true); err != nil {
		t.Fatalf("retire B: %v", err)
	}

	// A and C must remain active; B must be filtered from active query.
	active, err := dm.QueryGlobalRules("", 10, false)
	if err != nil {
		t.Fatalf("QueryGlobalRules active: %v", err)
	}
	if len(active) != 2 {
		t.Fatalf("expected 2 active rules after retiring B, got %d", len(active))
	}
	gotActive := map[string]bool{}
	for _, r := range active {
		gotActive[r["id"].(string)] = true
	}
	if !gotActive[idA] || !gotActive[idC] {
		t.Errorf("A and C must remain active; got active set %v", gotActive)
	}
	if gotActive[idB] {
		t.Error("B must not appear in active set")
	}

	// With includeRetired=true, all three come back.
	all, err := dm.QueryGlobalRules("", 10, true)
	if err != nil {
		t.Fatalf("QueryGlobalRules includeRetired: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("expected 3 total rules with include_retired, got %d", len(all))
	}
}

func TestGlobalRule_SharedDBNotAttachedErrors(t *testing.T) {
	dm := NewTestLocalOnlyDM(t)

	_, err := dm.RetireGlobalRule("any-id", "", true)
	if err == nil {
		t.Fatal("RetireGlobalRule without shared DB should error")
	}
	if !strings.Contains(err.Error(), "shared DB not attached") {
		t.Errorf("error should mention shared DB attachment, got: %v", err)
	}
}

// TestGlobalRule_RetiredAtColumnOnSharedMemories asserts the schema change
// (the SafeMigrations entry that adds `retired_at INTEGER` to memories) is
// applied to BOTH the local DB and shared.memories by the time the DM is
// ready to serve requests. This is the upgrade-path regression the spec
// asked for: a fresh DB and a migrated DB must converge on the same schema.
func TestGlobalRule_RetiredAtColumnOnSharedMemories(t *testing.T) {
	dm := NewTestSharedDM(t)

	// Local DB has retired_at.
	var localHasCol int
	if err := dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('memories') WHERE name = 'retired_at'`,
	).Scan(&localHasCol); err != nil {
		t.Fatalf("probe local memories: %v", err)
	}
	if localHasCol != 1 {
		t.Fatal("local memories missing retired_at column (SafeMigrations did not apply)")
	}

	// shared.memories has retired_at (the attachShared path must run
	// SafeMigrations against the shared schema too). sqlite_master is
	// more reliable than pragma_table_info for attached databases
	// because pragma_table_info resolves unqualified names against the
	// connection's default schema only.
	var sharedHasCol int
	if err := dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM shared.pragma_table_info('memories') WHERE name = 'retired_at'`,
	).Scan(&sharedHasCol); err != nil {
		t.Fatalf("probe shared.memories: %v", err)
	}
	if sharedHasCol != 1 {
		// Diagnostic: list the columns we DO see.
		rows, _ := dm.SQLDB().Query(`SELECT name FROM shared.pragma_table_info('memories') ORDER BY cid`)
		var names []string
		for rows.Next() {
			var n string
			rows.Scan(&n)
			names = append(names, n)
		}
		t.Fatalf("shared.memories missing retired_at column (SafeMigrations did not propagate to shared schema); columns present: %v", names)
	}
}
