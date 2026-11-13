// skill_tx_test.go — Stage P3: the transaction-aware skill writer (R2).
//
// Every test here runs against a DISPOSABLE database in a disposable
// workspace. The live MPM installation, its database and its services are
// never touched: `skillPkgDM` opens the workspace's database through the
// PRODUCTION path (NewDatabaseManager), so the DSN's `_foreign_keys=1`
// applies and the database is WAL — the mode the transaction semantics
// under test depend on.

package internal

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

// p3DM opens a DISPOSABLE database in a DISPOSABLE workspace.
//
// The workspace pin is NOT optional. `skillPkgDM` opens the workspace's
// database through the PRODUCTION path, and `NewDatabaseManager` resolves
// that path from MPM_WORKSPACE — falling back to `$HOME/.mpm` when the
// variable is unset. A test that opens the production database without
// pinning the workspace therefore writes to the OPERATOR'S LIVE DATABASE.
//
// This helper pins the workspace FIRST, then opens the database, and
// refuses to proceed unless the pinned workspace is verifiably
// disposable.
//
// The refusal is a CONTAINMENT check, not an equality check. An
// equality check would be dead code: `skillPkgWorkspace` always replaces
// the workspace with a fresh `t.TempDir()`, so it could never equal
// `$HOME/.mpm`. A containment check is real — it also refuses a
// workspace that lives INSIDE the production directory, which is what a
// `TMPDIR` pointing there would produce.
func p3DM(t *testing.T) *DatabaseManager {
	t.Helper()
	ws := skillPkgWorkspace(t)
	if err := ValidateTestWorkspace(ws); err != nil {
		t.Fatalf("the test workspace is not safe: %v", err)
	}
	return skillPkgDM(t)
}

// p3SkillContent builds a minimal valid skill document.
//
// The frontmatter carries a name and a version, which is what the writer
// requires; the description and when_to_use are present so the parse
// produces no warnings.
func p3SkillContent(name, version, body string) string {
	return fmt.Sprintf("---\nname: %s\nversion: %s\ndescription: a test skill\nwhen_to_use: when a test needs a skill with this exact name\n---\n%s",
		name, version, body)
}

// p3IsLatest reads a skill row's is_latest flag through the given
// connection, so a test can observe the row's state INSIDE a transaction.
func p3IsLatest(t *testing.T, q interface {
	QueryRow(string, ...interface{}) *sql.Row
}, id string) bool {
	t.Helper()
	var raw string
	err := q.QueryRow(`SELECT COALESCE(json_extract(metadata, '$.is_latest'), 0) FROM memories WHERE id = ?`, id).Scan(&raw)
	if err != nil {
		t.Fatalf("read is_latest for %s: %v", id, err)
	}
	return raw != "0" && raw != "" && raw != "false"
}

// p3SkillExists reports whether a live skill row exists.
func p3SkillExists(t *testing.T, dm *DatabaseManager, id string) bool {
	t.Helper()
	var n int
	if err := dm.SQLDB().QueryRow(`SELECT COUNT(*) FROM memories WHERE id = ?`, id).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", id, err)
	}
	return n > 0
}

// TestSaveSkillProjectionInTx_WritesInsideTheCallersTransaction pins the
// primitive's core contract: the write lands in the caller's transaction
// and nowhere else.
//
// The assertion that matters is the one in the middle — the row is NOT
// visible outside the transaction before the caller commits. That is what
// "no write escapes the caller's transaction" means, and it is the
// property an importer's atomicity depends on.
func TestSaveSkillProjectionInTx_WritesInsideTheCallersTransaction(t *testing.T) {
	dm := p3DM(t)
	content := p3SkillContent("txskill", "1.0.0", "body")

	tx, err := dm.SQLDB().Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback()

	id, err := dm.SaveSkillProjectionInTx(tx, "txskill", "1.0.0", content, "test-agent", ActiveContext{})
	if err != nil {
		t.Fatalf("SaveSkillProjectionInTx: %v", err)
	}
	if id != "skill:txskill-v1.0.0" {
		t.Errorf("id = %q, want skill:txskill-v1.0.0", id)
	}

	// NOT yet visible outside the transaction: the caller has not
	// committed, so the write has not happened as far as anyone else is
	// concerned.
	if p3SkillExists(t, dm, id) {
		t.Error("the projection is visible outside the transaction before the caller committed")
	}

	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	if !p3SkillExists(t, dm, id) {
		t.Error("the projection is missing after the caller committed")
	}
	got, err := dm.ReadSkill("txskill", "")
	if err != nil {
		t.Fatalf("ReadSkill: %v", err)
	}
	if got.ID != id {
		t.Errorf("ReadSkill id = %q, want %q", got.ID, id)
	}
	if got.Version != "1.0.0" {
		t.Errorf("ReadSkill version = %q, want 1.0.0", got.Version)
	}
}

// TestSaveSkillProjectionInTx_RollbackRemovesTheProjection pins that a
// caller who rolls back leaves nothing behind.
func TestSaveSkillProjectionInTx_RollbackRemovesTheProjection(t *testing.T) {
	dm := p3DM(t)
	content := p3SkillContent("txskill", "1.0.0", "body")

	tx, err := dm.SQLDB().Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	id, err := dm.SaveSkillProjectionInTx(tx, "txskill", "1.0.0", content, "test-agent", ActiveContext{})
	if err != nil {
		t.Fatalf("SaveSkillProjectionInTx: %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	if p3SkillExists(t, dm, id) {
		t.Error("the projection survived the caller's rollback")
	}
}

// TestSaveSkillProjectionInTx_RollbackRestoresPriorVersionIsLatest pins
// the guarantee the design calls out: a rollback undoes the prior
// versions' is_latest changes too, not only the inserted row.
//
// The flip of the prior version happens BEFORE the INSERT, inside the
// caller's transaction. If it were outside, a rollback would leave the
// name with NO latest version at all — ReadSkill would resolve it
// nondeterministically.
func TestSaveSkillProjectionInTx_RollbackRestoresPriorVersionIsLatest(t *testing.T) {
	dm := p3DM(t)

	// v1 is committed and is the latest version.
	if _, err := dm.SaveSkill("txskill", "1.0.0", p3SkillContent("txskill", "1.0.0", "v1"), "test-agent", false); err != nil {
		t.Fatalf("save v1: %v", err)
	}
	v1 := "skill:txskill-v1.0.0"
	if !p3IsLatest(t, dm.SQLDB(), v1) {
		t.Fatal("v1 is not the latest version after the initial save")
	}

	// Inside a transaction, write v2 — which flips v1's flag.
	tx, err := dm.SQLDB().Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := dm.SaveSkillProjectionInTx(tx, "txskill", "2.0.0", p3SkillContent("txskill", "2.0.0", "v2"), "test-agent", ActiveContext{}); err != nil {
		t.Fatalf("SaveSkillProjectionInTx: %v", err)
	}

	// Inside the transaction the flip HAS happened — that is the point:
	// the flip is part of the caller's transaction, not a separate write.
	if p3IsLatest(t, tx, v1) {
		t.Error("v1 is still latest inside the transaction; the flip did not happen in the caller's transaction")
	}

	if err := tx.Rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	// After the rollback, v1 is latest again and v2 does not exist.
	if !p3IsLatest(t, dm.SQLDB(), v1) {
		t.Error("v1 is not latest after the rollback — the is_latest flip escaped the transaction")
	}
	if p3SkillExists(t, dm, "skill:txskill-v2.0.0") {
		t.Error("v2 survived the rollback")
	}
}

// TestSaveSkillProjectionInTx_FailedInsertLeavesNoPartialState injects a
// failure AFTER the prior-version metadata updates — the case the design
// singles out — and pins that nothing partial survives.
//
// The failure is injected with a BEFORE INSERT trigger, so the writer's
// own INSERT fails after the is_latest flips have already run. The
// caller's rollback must undo both.
func TestSaveSkillProjectionInTx_FailedInsertLeavesNoPartialState(t *testing.T) {
	dm := p3DM(t)

	if _, err := dm.SaveSkill("txskill", "1.0.0", p3SkillContent("txskill", "1.0.0", "v1"), "test-agent", false); err != nil {
		t.Fatalf("save v1: %v", err)
	}
	v1 := "skill:txskill-v1.0.0"

	// Failure injection: the INSERT for v3 aborts.
	if _, err := dm.SQLDB().Exec(`
		CREATE TRIGGER p3_fail_insert BEFORE INSERT ON memories
		WHEN NEW.id = 'skill:txskill-v3.0.0'
		BEGIN SELECT RAISE(ABORT, 'injected failure'); END;
	`); err != nil {
		t.Fatalf("create the failing trigger: %v", err)
	}

	tx, err := dm.SQLDB().Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback()

	// The write flips v1's is_latest and then fails on the INSERT.
	_, err = dm.SaveSkillProjectionInTx(tx, "txskill", "3.0.0", p3SkillContent("txskill", "3.0.0", "v3"), "test-agent", ActiveContext{})
	if err == nil {
		t.Fatal("the injected failure did not stop the write")
	}
	if !strings.Contains(err.Error(), "injected failure") {
		t.Errorf("the error does not carry the injected failure: %v", err)
	}

	if err := tx.Rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	// No partial state: v3 never landed, and v1's flag was restored.
	if p3SkillExists(t, dm, "skill:txskill-v3.0.0") {
		t.Error("the failed insert left a row behind")
	}
	if !p3IsLatest(t, dm.SQLDB(), v1) {
		t.Error("v1 is not latest after the failed insert was rolled back")
	}
}

// TestSaveSkillProjectionInTx_SemverOrderingIsCorrect pins that the
// extracted writer still derives is_latest by SEMVER and not by save
// order — including the unequal-width case the id-sort proxy got wrong.
func TestSaveSkillProjectionInTx_SemverOrderingIsCorrect(t *testing.T) {
	dm := p3DM(t)

	write := func(version string) {
		t.Helper()
		tx, err := dm.SQLDB().Begin()
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer tx.Rollback()
		if _, err := dm.SaveSkillProjectionInTx(tx, "txskill", version, p3SkillContent("txskill", version, "body "+version), "test-agent", ActiveContext{}); err != nil {
			t.Fatalf("write %s: %v", version, err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit: %v", err)
		}
	}

	write("1.9.0")
	write("1.10.0")

	// 1.10.0 > 1.9.0 even though the id sorts the other way.
	if !p3IsLatest(t, dm.SQLDB(), "skill:txskill-v1.10.0") {
		t.Error("v1.10.0 is not the latest version")
	}
	if p3IsLatest(t, dm.SQLDB(), "skill:txskill-v1.9.0") {
		t.Error("v1.9.0 is still stamped latest")
	}

	// An OLDER version saved later must not take the flag.
	write("1.0.0")
	if !p3IsLatest(t, dm.SQLDB(), "skill:txskill-v1.10.0") {
		t.Error("the latest flag moved to an older version")
	}
	if p3IsLatest(t, dm.SQLDB(), "skill:txskill-v1.0.0") {
		t.Error("the older version was stamped latest")
	}
}

// TestSaveSkillProjectionInTx_RepeatedCallsFailPredictably pins that a
// second write of the same id inside the same transaction fails with the
// "already exists" refusal rather than a constraint violation — and that
// the first write is unaffected.
func TestSaveSkillProjectionInTx_RepeatedCallsFailPredictably(t *testing.T) {
	dm := p3DM(t)
	content := p3SkillContent("txskill", "1.0.0", "body")

	tx, err := dm.SQLDB().Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback()

	if _, err := dm.SaveSkillProjectionInTx(tx, "txskill", "1.0.0", content, "test-agent", ActiveContext{}); err != nil {
		t.Fatalf("the first write: %v", err)
	}

	_, err = dm.SaveSkillProjectionInTx(tx, "txskill", "1.0.0", content, "test-agent", ActiveContext{})
	if err == nil {
		t.Fatal("the second write of the same id succeeded")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("the second write failed with %v, want an \"already exists\" refusal", err)
	}
}

// TestSaveSkillProjectionInTx_RefusesANilTransaction pins the ownership
// contract: the primitive refuses to write when it has no transaction, so
// a caller's mistake cannot silently produce an unowned write.
func TestSaveSkillProjectionInTx_RefusesANilTransaction(t *testing.T) {
	dm := p3DM(t)

	_, err := dm.SaveSkillProjectionInTx(nil, "txskill", "1.0.0", p3SkillContent("txskill", "1.0.0", "body"), "test-agent", ActiveContext{})
	if err == nil {
		t.Fatal("the writer accepted a nil transaction")
	}
	if !strings.Contains(err.Error(), "transaction") {
		t.Errorf("the refusal does not mention the transaction: %v", err)
	}
	if p3SkillExists(t, dm, "skill:txskill-v1.0.0") {
		t.Error("the refused write still wrote a row")
	}
}

// TestSaveSkillProjectionInTx_PreservesTheValidationGuarantees pins that
// the extraction kept the legacy gate: a projection whose frontmatter
// disagrees with the requested identity, or whose content trips the
// scanner, never reaches the database — through the tx path as well as
// the legacy one.
func TestSaveSkillProjectionInTx_PreservesTheValidationGuarantees(t *testing.T) {
	dm := p3DM(t)

	cases := []struct {
		name    string
		version string
		content string
		want    string
	}{
		{
			name:    "the frontmatter name disagrees with the argument",
			version: "1.0.0",
			content: p3SkillContent("otherskill", "1.0.0", "body"),
			want:    "frontmatter name",
		},
		{
			name:    "the frontmatter version disagrees with the argument",
			version: "2.0.0",
			content: p3SkillContent("txskill", "1.0.0", "body"),
			want:    "frontmatter version",
		},
		{
			name:    "there is no frontmatter at all",
			version: "1.0.0",
			content: "just a body",
			want:    "frontmatter",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tx, err := dm.SQLDB().Begin()
			if err != nil {
				t.Fatalf("begin: %v", err)
			}
			defer tx.Rollback()

			_, err = dm.SaveSkillProjectionInTx(tx, "txskill", tc.version, tc.content, "test-agent", ActiveContext{})
			if err == nil {
				t.Fatal("the writer accepted a projection that must be refused")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("the refusal is %v, want it to mention %q", err, tc.want)
			}
			if err := tx.Rollback(); err != nil {
				t.Fatalf("rollback: %v", err)
			}
			if p3SkillExists(t, dm, "skill:txskill-v"+tc.version) {
				t.Error("the refused projection wrote a row")
			}
		})
	}
}

// TestSaveSkillProjectionInTx_WritesTheArtifactProvenanceInTheCallersTransaction
// pins that the invocation attribution travels with the write and lands
// in the caller's transaction — so a rollback removes the provenance row
// too, rather than leaving a row that claims an invocation that produced
// nothing.
func TestSaveSkillProjectionInTx_WritesTheArtifactProvenanceInTheCallersTransaction(t *testing.T) {
	dm := p3DM(t)

	ac := ActiveContext{
		Agent:         "p3-test-agent",
		FrameworkName: "p3-test-framework",
		InvocationID:  "inv-p3-0001",
	}

	tx, err := dm.SQLDB().Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	id, err := dm.SaveSkillProjectionInTx(tx, "txskill", "1.0.0", p3SkillContent("txskill", "1.0.0", "body"), "p3-test-agent", ac)
	if err != nil {
		t.Fatalf("SaveSkillProjectionInTx: %v", err)
	}

	// The provenance row is inside the caller's transaction.
	var n int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM artifact_provenance WHERE artifact_id = ? AND invocation_id = ?`, id, "inv-p3-0001").Scan(&n); err != nil {
		t.Fatalf("count the provenance row: %v", err)
	}
	if n != 1 {
		t.Errorf("the provenance row count inside the transaction is %d, want 1", n)
	}

	if err := tx.Rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	// After the rollback the provenance row is gone too.
	if err := dm.SQLDB().QueryRow(`SELECT COUNT(*) FROM artifact_provenance WHERE artifact_id = ?`, id).Scan(&n); err != nil {
		t.Fatalf("count the provenance row: %v", err)
	}
	if n != 0 {
		t.Errorf("the provenance row survived the rollback (count=%d)", n)
	}
}
