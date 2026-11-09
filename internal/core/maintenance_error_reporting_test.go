// maintenance_error_reporting_test.go — RunSelfMaintenance must not report
// a failed maintenance operation as a success.
//
// WHY THIS FILE EXISTS.
//
// `RunSelfMaintenance` used to discard the error from every operation it ran
// and return `nil` unconditionally, so `mpm ops maintain` printed
// "✅ Self-maintenance complete" and exited 0 while decay, pruning or
// consolidation had not run at all.
//
// Each test below injects a DETERMINISTIC failure with a SQLite trigger (or by
// removing one table) and asserts:
//
//   - the operation is named in the returned error and in stats.Errors;
//   - the operations that DID succeed still contribute their real numbers;
//   - the failure is not confused with overall success.
//
// No timing, no sleeps, no flaky contention.

package internal

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// maintSeedRows inserts memories with explicit columns. `tags` is written as
// the JSON `[]` that addMemoryDirect always produces — a NULL tags column is a
// separate, pre-existing scan defect in ConsolidateMemories (reported
// separately), and these fixtures must not depend on it.
type maintSeedRow struct {
	id            string
	weight        float64
	lastAccessed  sql.NullInt64
	isLongTerm    int
	reinforcement int
	createdAt     int64
	updatedAt     int64
	embedding     string // JSON array, or "" for none
}

func seedMaintRows(t *testing.T, db *sql.DB, rows []maintSeedRow) {
	t.Helper()
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare(`
		INSERT INTO memories
			(id, collection, content, tags, weight, metadata, embedding,
			 is_long_term, reinforcement_count, last_accessed_at, created_at, updated_at)
		VALUES (?, 'memories', ?, '[]', ?, '{}', ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	defer stmt.Close()
	for _, r := range rows {
		var emb interface{}
		if r.embedding != "" {
			emb = r.embedding
		}
		if _, err := stmt.Exec(r.id, "maintenance fixture "+r.id, r.weight, emb,
			r.isLongTerm, r.reinforcement, r.lastAccessed, r.createdAt, r.updatedAt); err != nil {
			t.Fatalf("seed %s: %v", r.id, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// newMaintTestStore returns a store over a file-backed WAL database with work
// for every maintenance operation:
//
//   - 20 aged, non-LTM rows                  → decay targets
//   - 10 never-accessed, aged, non-LTM rows  → prune targets
//   - 5 LTM rows with no access record      → the never-accessed count (and
//     exempt from both the decay and the prune predicates)
//   - 1 recent weight-1 row                 → the low-weight count, exempt
//     from the low-weight prune (updated_at is inside the window)
func newMaintTestStore(t *testing.T) (*DatabaseManager, *MemoryStore, *sql.DB) {
	t.Helper()
	PinIsolatedWorkspace(t)
	dbPath := filepath.Join(t.TempDir(), "mpm.db")
	db, err := sql.Open("sqlite3", SqliteWriteDSN(dbPath))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec("PRAGMA journal_mode = WAL"); err != nil {
		t.Fatalf("wal: %v", err)
	}
	dm := NewDatabaseManagerForDB(db)
	if err := dm.InitSchema(); err != nil {
		t.Fatalf("schema: %v", err)
	}
	store, err := dm.getSharedStore()
	if err != nil {
		t.Fatalf("shared store: %v", err)
	}

	now := time.Now().UTC().Unix()
	old := now - 120*86400
	var rows []maintSeedRow
	for i := 0; i < 20; i++ {
		rows = append(rows, maintSeedRow{
			id:           "decay-" + string(rune('a'+i)),
			weight:       10,
			lastAccessed: sql.NullInt64{Int64: old, Valid: true},
			createdAt:    old, updatedAt: old,
		})
	}
	for i := 0; i < 10; i++ {
		rows = append(rows, maintSeedRow{
			id:        "prune-" + string(rune('a'+i)),
			weight:    10,
			createdAt: old, updatedAt: old,
		})
	}
	for i := 0; i < 5; i++ {
		rows = append(rows, maintSeedRow{
			id:         "ltm-" + string(rune('a'+i)),
			weight:     10,
			isLongTerm: 1,
			createdAt:  old, updatedAt: old,
		})
	}
	rows = append(rows, maintSeedRow{
		id:           "lowweight-recent",
		weight:       1,
		lastAccessed: sql.NullInt64{Int64: now, Valid: true},
		createdAt:    now, updatedAt: now,
	})
	seedMaintRows(t, db, rows)
	return dm, store, db
}

// injectFailure installs a trigger that aborts a specific column update.
func injectFailure(t *testing.T, db *sql.DB, name, column, message string) {
	t.Helper()
	if _, err := db.Exec(`CREATE TRIGGER ` + name + ` BEFORE UPDATE OF ` + column + ` ON memories
		BEGIN SELECT RAISE(ABORT, '` + message + `'); END;`); err != nil {
		t.Fatalf("create trigger %s: %v", name, err)
	}
}

// failedOps returns the operation names the run recorded.
func failedOps(stats *MaintenanceStats) map[string]bool {
	out := map[string]bool{}
	for _, e := range stats.Errors {
		out[e] = true
	}
	return out
}

// TestRunSelfMaintenance_SuccessReturnsNoError is the control: a clean run
// must still return nil, with no failure names.
func TestRunSelfMaintenance_SuccessReturnsNoError(t *testing.T) {
	_, store, _ := newMaintTestStore(t)

	stats, err := store.RunSelfMaintenance()
	if err != nil {
		t.Fatalf("a clean run returned an error: %v", err)
	}
	if stats == nil {
		t.Fatal("a clean run returned nil stats")
	}
	if len(stats.Errors) != 0 {
		t.Fatalf("a clean run recorded failures: %v", stats.Errors)
	}
	// The fixture must actually exercise the operations, or the control is
	// vacuous.
	if stats.DecayedWeights == 0 {
		t.Error("the control run decayed nothing; the fixture is not exercising decay")
	}
	if stats.PrunedTotal == 0 {
		t.Error("the control run pruned nothing; the fixture is not exercising pruning")
	}
	if stats.NeverAccessed != 5 {
		t.Errorf("never-accessed = %d, want 5 (the LTM rows)", stats.NeverAccessed)
	}
	if stats.LowWeight != 1 {
		t.Errorf("low-weight = %d, want 1 (the recent weight-1 row)", stats.LowWeight)
	}
}

// TestRunSelfMaintenance_DecayFailureIsReported isolates the decay operation
// and asserts the OTHER statistics survive.
func TestRunSelfMaintenance_DecayFailureIsReported(t *testing.T) {
	_, store, db := newMaintTestStore(t)
	injectFailure(t, db, "t_decay_fail", "weight", "injected decay failure")

	stats, err := store.RunSelfMaintenance()
	if err == nil {
		t.Fatal("a failed decay was reported as success")
	}
	if !strings.Contains(err.Error(), "decay_weights") {
		t.Errorf("the error does not name the decay operation: %v", err)
	}
	if !strings.Contains(err.Error(), "injected decay failure") {
		t.Errorf("the underlying cause was lost: %v", err)
	}
	if !failedOps(stats)["decay_weights"] {
		t.Errorf("stats.Errors does not name decay_weights: %v", stats.Errors)
	}
	if len(stats.Errors) != 1 {
		t.Errorf("only the decay should have failed, got %v", stats.Errors)
	}

	// Partial statistics: the operations that did not depend on decay must
	// still be real. Prune ran (its trigger did not fire).
	if stats.PrunedTotal == 0 {
		t.Error("the prune count was lost even though only decay failed")
	}
	if stats.NeverAccessed != 5 {
		t.Errorf("never-accessed = %d, want 5 even though decay failed", stats.NeverAccessed)
	}
	if stats.LowWeight != 1 {
		t.Errorf("low-weight = %d, want 1 even though decay failed", stats.LowWeight)
	}
}

// TestRunSelfMaintenance_PruneFailureIsReported isolates the prune operation.
func TestRunSelfMaintenance_PruneFailureIsReported(t *testing.T) {
	_, store, db := newMaintTestStore(t)
	injectFailure(t, db, "t_softdelete_fail", "deleted_at", "injected soft-delete failure")

	stats, err := store.RunSelfMaintenance()
	if err == nil {
		t.Fatal("a failed prune was reported as success")
	}
	got := failedOps(stats)
	if !got["auto_prune"] {
		t.Errorf("the error does not name the prune operation: %v", stats.Errors)
	}
	if got["decay_weights"] {
		t.Errorf("decay should have succeeded: %v", stats.Errors)
	}
	if stats.DecayedWeights == 0 {
		t.Error("the decay count was lost even though decay succeeded")
	}
}

// TestRunSelfMaintenance_ConsolidateFailureIsReported isolates the
// consolidation operation with a cluster large enough to trigger a
// soft-delete, and asserts the failure is reported instead of being counted
// as a consolidation that happened.
func TestRunSelfMaintenance_ConsolidateFailureIsReported(t *testing.T) {
	_, store, db := newMaintTestStore(t)

	// 60 rows, all with the same embedding and collection, so the cluster
	// exceeds ConsolidateMemories' maxPerTopic of 50. They are RECENT, so
	// neither prune nor decay touches them and the only soft-deletes in the
	// run are the cluster's.
	now := time.Now().UTC().Unix()
	emb, err := json.Marshal([]float32{1, 0, 0})
	if err != nil {
		t.Fatalf("marshal embedding: %v", err)
	}
	var rows []maintSeedRow
	for i := 0; i < 60; i++ {
		rows = append(rows, maintSeedRow{
			id:           "cluster-" + string(rune('A'+i%26)) + string(rune('a'+i/26)),
			weight:       10,
			lastAccessed: sql.NullInt64{Int64: now, Valid: true},
			createdAt:    now, updatedAt: now,
			embedding: string(emb),
		})
	}
	seedMaintRows(t, db, rows)

	// Remove the never-accessed rows the shared fixture seeds, so the ONLY
	// soft-deletes in this run are the cluster's. Otherwise the deleted_at
	// trigger fires during pruning too and the test cannot tell the two
	// operations apart.
	if _, err := db.Exec(`DELETE FROM memories WHERE id LIKE 'prune-%'`); err != nil {
		t.Fatalf("clear the prunable fixture rows: %v", err)
	}

	injectFailure(t, db, "t_softdelete_fail", "deleted_at", "injected soft-delete failure")

	stats, err := store.RunSelfMaintenance()
	if err == nil {
		t.Fatal("a failed consolidation was reported as success")
	}
	got := failedOps(stats)
	if !got["consolidate"] {
		t.Errorf("the error does not name the consolidation operation: %v", stats.Errors)
	}
	if got["auto_prune"] {
		t.Errorf("prune had nothing to do, so it should not be reported as failed: %v", stats.Errors)
	}
	// The count must not claim work that did not happen. Before this fix the
	// soft-delete result was discarded and every member was counted anyway.
	if stats.Consolidated != 0 {
		t.Errorf("consolidated = %d, want 0: the soft-delete failed, so nothing was consolidated",
			stats.Consolidated)
	}
}

// TestRunSelfMaintenance_ReviewCountFailureIsReported isolates a STATISTIC
// failure: the maintenance itself can still run, but the report is incomplete
// and must say so.
func TestRunSelfMaintenance_ReviewCountFailureIsReported(t *testing.T) {
	_, store, db := newMaintTestStore(t)
	if _, err := db.Exec(`DROP TABLE memory_reviews`); err != nil {
		t.Fatalf("drop memory_reviews: %v", err)
	}

	stats, err := store.RunSelfMaintenance()
	if err == nil {
		t.Fatal("a failed backlog count was reported as success")
	}
	if !failedOps(stats)["review_backlog"] {
		t.Errorf("stats.Errors does not name review_backlog: %v", stats.Errors)
	}
	// The actions still ran.
	if stats.DecayedWeights == 0 {
		t.Error("a statistic failure must not prevent the maintenance actions")
	}
	if stats.PrunedTotal == 0 {
		t.Error("a statistic failure must not prevent pruning")
	}
}

// TestRunSelfMaintenance_EveryFailureIsListed removes the table every
// operation depends on, so each one fails, and asserts the report names them.
func TestRunSelfMaintenance_EveryFailureIsListed(t *testing.T) {
	_, store, db := newMaintTestStore(t)
	if _, err := db.Exec(`DROP TABLE memories`); err != nil {
		t.Fatalf("drop memories: %v", err)
	}

	stats, err := store.RunSelfMaintenance()
	if err == nil {
		t.Fatal("a wholly failed maintenance was reported as success")
	}
	if stats == nil {
		t.Fatal("stats are nil; partial statistics must still be returned")
	}
	want := []string{
		"decay_weights", "auto_prune", "consolidate",
		"review_backlog", "never_accessed_count", "low_weight_count",
	}
	got := failedOps(stats)
	for _, w := range want {
		if !got[w] {
			t.Errorf("failure %q was not reported; got %v", w, stats.Errors)
		}
		if !strings.Contains(err.Error(), w) {
			t.Errorf("the error does not name %q: %v", w, err)
		}
	}
}

// TestDatabaseManagerRunSelfMaintenance_KeepsPartialStatsOnError pins the
// wrapper: an error must NOT discard the statistics that were produced.
func TestDatabaseManagerRunSelfMaintenance_KeepsPartialStatsOnError(t *testing.T) {
	dm, _, db := newMaintTestStore(t)
	injectFailure(t, db, "t_decay_fail", "weight", "injected decay failure")

	out, err := dm.RunSelfMaintenance()
	if err == nil {
		t.Fatal("the wrapper reported a failed maintenance as success")
	}
	if out == nil {
		t.Fatal("the wrapper discarded the partial statistics on error")
	}
	if _, ok := out["decayed_weights"]; !ok {
		t.Errorf("the partial payload is missing decayed_weights: %#v", out)
	}
	failed, ok := out["errors"].([]string)
	if !ok || len(failed) == 0 {
		t.Fatalf("the partial payload does not carry the failed operation names: %#v", out)
	}
	if failed[0] != "decay_weights" {
		t.Errorf("failed operations = %v, want decay_weights first", failed)
	}
}

// TestDatabaseManagerRunSelfMaintenance_SuccessOmitsErrorsKey keeps the
// success payload's shape unchanged.
func TestDatabaseManagerRunSelfMaintenance_SuccessOmitsErrorsKey(t *testing.T) {
	dm, _, _ := newMaintTestStore(t)

	out, err := dm.RunSelfMaintenance()
	if err != nil {
		t.Fatalf("a clean run returned an error: %v", err)
	}
	if _, present := out["errors"]; present {
		t.Errorf("the success payload grew an errors key: %#v", out)
	}
}
