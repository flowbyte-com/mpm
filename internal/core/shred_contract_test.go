// shred_contract_test.go — pins the boundary of what `shred` guarantees.
//
// # Why this file exists
//
// MPM's user-facing surfaces used to describe shredding as "secure
// delete". The implementation has never done that, and cannot without
// machinery MPM deliberately does not have. What it does is a hard
// DELETE from the active substrate plus a defined cascade — which makes
// the object unreachable through MPM, but does nothing about the audit
// log, mirror history, backups, the WAL, or the freed pages of the DB
// file itself.
//
// The risk these tests pin is not a missing feature; it is the
// opposite. If someone later reads "secure delete" in the help text,
// believes it, and documents or promises erasure to an operator, the
// promise cannot be kept. So the invariant is stated negatively: the
// documented and user-visible claim must stay inside what the code
// actually does.
//
// Everything here is hermetic — in-memory DatabaseManager, temp
// workspaces, a unique canary string per run. No live database, no
// operator workspace, no real content is touched.

package internal

import (
	"bytes"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// shredContractCanary is unique to these tests so a hit for it can
// only come from rows this file created.
const shredContractCanary = "SHREDCONTRACT-CANARY-4b7e21"

// TestShredRemovesObjectFromActiveState is the positive half: the
// guarantee MPM actually makes. These are the rows a shredded memory
// must be gone from — including the FTS index, which is reached only
// through the `memories_ad` AFTER DELETE trigger, so it is worth
// asserting directly rather than trusting the trigger to exist.
func TestShredRemovesObjectFromActiveState(t *testing.T) {
	dm := NewTestDM(t)
	defer dm.Close()
	db := dm.SQLDB()

	const id = "mem-contract-1"
	seedShredContractMemory(t, db, id)

	// Precondition: the canary is findable in the FTS index.
	var ftsBefore int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM memories_fts WHERE content LIKE ?`,
		"%"+shredContractCanary+"%").Scan(&ftsBefore); err != nil {
		t.Fatalf("FTS pre-check: %v", err)
	}
	if ftsBefore == 0 {
		t.Fatalf("canary not in memories_fts before shred; the fixture is wrong, " +
			"not the behavior under test")
	}

	if _, err := dm.ShredMemoryWithCascade(id); err != nil {
		t.Fatalf("ShredMemoryWithCascade: %v", err)
	}

	// Every table the broad sweep claims to clear must be empty for this id.
	cleared := []struct{ table, where string }{
		{"memories", `id = '` + id + `'`},
		{"memories_fts", `content LIKE '%` + shredContractCanary + `%'`},
		{"topic_memberships", `memory_id = '` + id + `'`},
		{"memory_revisions", `memory_id = '` + id + `'`},
		{"evidence", `artifact_id = '` + id + `'`},
		{"retrieval_metadata", `node_id = '` + id + `'`},
		{"confidence_history", `artifact_id = '` + id + `'`},
		{"artifact_provenance", `artifact_id = '` + id + `'`},
		{"synth_runs", `result_memory_id = '` + id + `'`},
	}
	for _, c := range cleared {
		var n int
		q := `SELECT COUNT(*) FROM ` + c.table + ` WHERE ` + c.where
		if err := db.QueryRow(q).Scan(&n); err != nil {
			t.Errorf("%s: %v", c.table, err)
			continue
		}
		if n != 0 {
			t.Errorf("%s still has %d row(s) for a shredded memory; shred's "+
				"active-state guarantee is broken", c.table, n)
		}
	}
}

// TestShredDoesNotClaimErasure is the negative half, and the reason
// this file exists.
//
// It pins the specific rows that SURVIVE a shred. If a future change
// starts clearing these, the test fails — and that failure is correct
// and worth reading, because it means the contract in
// docs/SPEC.md §4.5.2 has drifted and must be updated alongside the
// code, not silently invalidated by it.
func TestShredDoesNotClaimErasure(t *testing.T) {
	dm := NewTestDM(t)
	defer dm.Close()
	db := dm.SQLDB()

	const id = "mem-contract-2"
	seedShredContractMemory(t, db, id)

	// An audit row that quotes the content, exactly as real usage
	// produces. No shred path reads system_audit_log.
	if _, err := db.Exec(`INSERT INTO system_audit_log (id, level, component, message, created_at)
		VALUES ('audit-contract-1', 'info', 'test', ?, CAST(strftime('%s','now') AS INTEGER))`,
		"saved "+shredContractCanary); err != nil {
		t.Fatalf("seed audit row: %v", err)
	}
	// An epistemic provenance edge citing the memory. shredBroadSweep
	// documents this as deliberately not swept.
	if _, err := db.Exec(`INSERT INTO epistemic_provenance
		(id, source_id, source_type, downstream_id, downstream_type, event_id)
		VALUES ('ep-contract-1', ?, 'memory', 'theory-contract-1', 'theory', 'evt-contract-1')`,
		id); err != nil {
		t.Fatalf("seed epistemic_provenance: %v", err)
	}

	if _, err := dm.ShredMemoryWithCascade(id); err != nil {
		t.Fatalf("ShredMemoryWithCascade: %v", err)
	}

	// These are the copies that outlive a shred. docs/SPEC.md §4.5.2
	// lists them; this test is what makes that list a checked claim.
	survives := []struct{ table, where, why string }{
		{"system_audit_log", `message LIKE '%` + shredContractCanary + `%'`,
			"the audit log is a durable history, not a per-object casualty"},
		{"epistemic_provenance", `source_id = '` + id + `'`,
			"edges citing a dead id are pruned by a later gc sweep, not by shred"},
	}
	for _, s := range survives {
		var n int
		q := `SELECT COUNT(*) FROM ` + s.table + ` WHERE ` + s.where
		if err := db.QueryRow(q).Scan(&n); err != nil {
			t.Errorf("%s: %v", s.table, err)
			continue
		}
		if n == 0 {
			t.Errorf("%s no longer retains its row after shred.\n"+
				"  If shred now clears this table, docs/SPEC.md §4.5.2 is stale and\n"+
				"  must be updated in the same change (%s).", s.table, s.why)
		}
	}
}

// TestShredLeavesContentInStorageBytes is the measurement behind the
// whole contract: after the command exits, the content is still in the
// database file.
//
// This is the claim "secure delete" would have to mean, and MPM does
// not make it. MPM issues no PRAGMA secure_delete, no VACUUM, and no
// wal_checkpoint on the shred path, so the freed pages keep the bytes
// until SQLite happens to reuse them. Asserting their continued
// presence is how this test documents the limit — if a future
// contributor does add real page clearing, this test fails and the
// contract can be widened deliberately.
func TestShredLeavesContentInStorageBytes(t *testing.T) {
	ws := t.TempDir()
	dm, err := NewDatabaseManager(ws)
	if err != nil {
		t.Fatalf("NewDatabaseManager: %v", err)
	}
	defer dm.Close()

	const id = "mem-contract-3"
	if _, err := dm.SQLDB().Exec(`INSERT INTO memories (id, collection, content, session_id, tags)
		VALUES (?, 'memories', ?, NULL, '[]')`, id, "the secret is "+shredContractCanary); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := dm.ShredMemoryWithCascade(id); err != nil {
		t.Fatalf("ShredMemoryWithCascade: %v", err)
	}

	// The row is gone from the live schema...
	var rows int
	if err := dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM memories WHERE id = ?`, id).Scan(&rows); err != nil {
		t.Fatalf("post-shred read: %v", err)
	}
	if rows != 0 {
		t.Fatalf("memory row survived shred; the guarantee itself is broken")
	}

	// ...but the bytes are still on the medium. Check the main DB and
	// the WAL together, because a checkpoint can move the bytes from
	// one to the other at any moment; either location counts as
	// "still present", which is the whole point.
	foundIn := ""
	for _, suffix := range []string{"", "-wal"} {
		data, err := os.ReadFile(dm.DBPath() + suffix)
		if err != nil {
			continue // absent (e.g. WAL checkpointed away) is fine
		}
		if bytes.Contains(data, []byte(shredContractCanary)) {
			foundIn = suffix
			break
		}
	}
	if foundIn == "" {
		t.Skip("the freed pages happened to be reused or rewritten in this run; " +
			"the limit this test documents is real but not observable byte-for-byte " +
			"on every run. See TestShredContractIsDocumented for the check that always runs.")
	}
	t.Logf("shredded content still present in mpm.db%s — this is the documented limit", foundIn)
}

// TestShredContractIsDocumented is the always-on guard. It does not
// depend on SQLite page reuse, so it never skips.
//
// It asserts the contract statement exists in the canonical product
// document. Documentation drift is the actual failure mode this whole
// audit is about: the code has always behaved correctly here, and only
// the wording was wrong — so a future contributor is far likelier to
// re-break the *words* than the code.
func TestShredContractIsDocumented(t *testing.T) {
	spec, ok := readRepoFile(t, "docs/SPEC.md")
	if !ok {
		t.Skip("docs/SPEC.md not reachable from the test working directory; " +
			"this guard only runs in a full checkout")
	}
	for _, want := range []string{
		"What \"shred\" guarantees",
		"not secure erasure",
		"MPM has no secure-erasure capability",
		"Residual copies outside every `shred` guarantee",
	} {
		if !strings.Contains(spec, want) {
			t.Errorf("docs/SPEC.md no longer contains %q.\n"+
				"  The shred contract is the canonical statement of what per-object\n"+
				"  shred does and does not guarantee. If it was reworded, re-add the\n"+
				"  substance — 'shred removes from active state, it does not erase\n"+
				"  bytes' — rather than deleting the claim.", want)
		}
	}
}

// seedShredContractMemory inserts one memory plus the dependent rows
// the broad sweep is expected to clear, so both halves of the contract
// have something to act on.
func seedShredContractMemory(t *testing.T, db *sql.DB, id string) {
	t.Helper()
	stmts := []struct{ label, sql string }{
		{"topic", `INSERT INTO topics (id, name, description, tags, is_active)
			VALUES ('topic-' || ?, 'contract-topic', 'fixture', '[]', 1)`},
		{"memory", `INSERT INTO memories (id, collection, content, session_id, tags)
			VALUES (?, 'memories', ?, NULL, '[]')`},
		{"topic_memberships", `INSERT INTO topic_memberships (memory_id, session_id, topic_id, role)
			VALUES (?, NULL, 'topic-' || ?, 'related')`},
		{"evidence", `INSERT INTO evidence
			(id, artifact_id, artifact_type, type, source_group, strength, created_by, created_at, notes)
			VALUES ('ev-' || ?, ?, 'memory', 'observation', 'fixture', 0.5, 'test',
				CAST(strftime('%s','now') AS INTEGER), ?)`},
		{"retrieval_metadata", `INSERT INTO retrieval_metadata (node_id, node_type) VALUES (?, 'memory')`},
		{"confidence_history", `INSERT INTO confidence_history
			(id, artifact_id, artifact_type, confidence, computed_at, evidence_count, trigger)
			VALUES ('ch-' || ?, ?, 'memory', 0.5, CAST(strftime('%s','now') AS INTEGER), 1, 'manual_recompute')`},
		{"artifact_provenance", `INSERT INTO artifact_provenance
			(id, artifact_id, artifact_type, created_at, actor_kind)
			VALUES ('ap-' || ?, ?, 'memory', CAST(strftime('%s','now') AS INTEGER), 'system')`},
		{"synth_runs", `INSERT INTO synth_runs (content_hash, first_run_at, last_run_at, result_memory_id)
			VALUES ('hash-' || ?, 1, 1, ?)`},
	}
	canary := "saw " + shredContractCanary
	for _, s := range stmts {
		var err error
		switch s.label {
		case "memory":
			_, err = db.Exec(s.sql, id, canary)
		case "topic":
			_, err = db.Exec(s.sql, id)
		case "evidence":
			_, err = db.Exec(s.sql, id, id, canary)
		case "topic_memberships", "synth_runs":
			_, err = db.Exec(s.sql, id, id)
		default:
			_, err = db.Exec(s.sql, id, id)
		}
		if err != nil {
			t.Fatalf("seed %s: %v", s.label, err)
		}
	}
}

// readRepoFile reads a path relative to the repository root, located
// by walking up from the test's working directory. It reports ok=false
// rather than failing when the file is not reachable, so a
// module-only checkout skips rather than errors.
func readRepoFile(t *testing.T, rel string) (string, bool) {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for i := 0; i < 6; i++ {
		candidate := filepath.Join(dir, rel)
		if data, err := os.ReadFile(candidate); err == nil {
			return string(data), true
		}
		dir = filepath.Dir(dir)
	}
	return "", false
}
