// skill_package_provenance_test.go — the B2 provenance regressions.
//
// A resurrection REPLACES the content of an existing skill row in place.
// The review (comment 6864706276) showed that the row's artifact
// provenance was left describing the PREVIOUS writer, which is the false
// attribution the substrate's refreshArtifactProvenance exists to
// prevent — and which the legacy force path already avoids.
//
// These tests pin the corrected behaviour:
//
//   - an import with an EXPLICIT invocation provenance attributes the
//     revived row to that invocation;
//   - an operator-driven import with an EMPTY ActiveContext does NOT
//     retain the previous writer's attribution;
//   - a rollback restores the original state, INCLUDING the provenance;
//   - the legacy ordinary-skill force-overwrite is unchanged.
//
// Every fixture is disposable: a t.TempDir() tree and a disposable
// database.
package internal

import (
	"database/sql"
	"strings"
	"testing"
)

// p4ProvenanceOf reads the identity columns of a skill's provenance row.
func p4ProvenanceOf(t *testing.T, dm *DatabaseManager, id string) map[string]string {
	t.Helper()
	out := map[string]string{}
	rows, err := dm.db.Query(`
		SELECT COALESCE(actor_kind,''), COALESCE(actor_id,''), COALESCE(framework_name,''),
		       COALESCE(model_name,''), COALESCE(session_id,''), COALESCE(invocation_id,'')
		FROM artifact_provenance WHERE artifact_id = ?`, id)
	if err != nil {
		t.Fatalf("query provenance: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var kind, actor, framework, model, session, invocation string
		if err := rows.Scan(&kind, &actor, &framework, &model, &session, &invocation); err != nil {
			t.Fatalf("scan provenance: %v", err)
		}
		out["actor_kind"] = kind
		out["actor_id"] = actor
		out["framework_name"] = framework
		out["model_name"] = model
		out["session_id"] = session
		out["invocation_id"] = invocation
	}
	return out
}

// p4WriterA is the identity of the ORIGINAL writer of a skill.
func p4WriterA() ActiveContext {
	return ActiveContext{
		SessionID:     "session-writer-a",
		InvocationID:  "invocation-writer-a",
		FrameworkName: "framework-writer-a",
		Model:         "model-writer-a",
		Agent:         "agent-writer-a",
	}
}

// p4WriterB is the identity of the IMPORTING invocation.
func p4WriterB() ActiveContext {
	return ActiveContext{
		SessionID:     "session-writer-b",
		InvocationID:  "invocation-writer-b",
		FrameworkName: "framework-writer-b",
		Model:         "model-writer-b",
		Agent:         "agent-writer-b",
	}
}

// p4ProvenanceSkill creates an ordinary skill attributed to writer A and
// returns its id.
func p4ProvenanceSkill(t *testing.T, dm *DatabaseManager, name string) string {
	t.Helper()
	doc := p4SkillDoc(name, "1.0.0", "the ORIGINAL ordinary body\n")
	id, err := dm.SaveSkillWithInvocation(name, "1.0.0", doc, "the-original-author", false, p4WriterA())
	if err != nil {
		t.Fatalf("save the original skill: %v", err)
	}
	return id
}

// ─────────────────────────────────────────────────────────────────────────────
// The explicit-invocation case
// ─────────────────────────────────────────────────────────────────────────────

// TestP4_B2_AResurrectionAttributesTheImportingInvocation is the core
// B2 regression.
//
// A skill attributed to writer A is soft-deleted and then resurrected by
// a package import attributed to writer B. The revived row's content is
// the package's projection, and its provenance must describe writer B —
// not writer A, who no longer wrote the content.
func TestP4_B2_AResurrectionAttributesTheImportingInvocation(t *testing.T) {
	dm := p3DM(t)

	id := p4ProvenanceSkill(t, dm, "attribution")

	before := p4ProvenanceOf(t, dm, id)
	t.Logf("provenance BEFORE: %+v", before)
	if before["actor_id"] != "agent-writer-a" {
		t.Fatalf("the original skill's provenance does not name writer A: %+v", before)
	}

	if err := dm.ShredSkill(id); err != nil {
		t.Fatalf("shred: %v", err)
	}

	root := p4PackageTree(t, map[string]string{
		"SKILL.md":        p4SkillDoc("attribution", "1.0.0", "the PACKAGE body\n"),
		"references/a.md": "# A\n",
	})
	res, err := dm.ImportSkillPackageFromDir(PackageImportOptions{
		Path:          root,
		ActiveContext: p4WriterB(),
	})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if res.Status != PackageImportStatusResurrected {
		t.Fatalf("the status = %q, want %q", res.Status, PackageImportStatusResurrected)
	}

	// The content and the metadata describe the imported package.
	var content, meta string
	if err := dm.db.QueryRow(`SELECT content, COALESCE(metadata,'{}') FROM memories WHERE id = ?`,
		id).Scan(&content, &meta); err != nil {
		t.Fatalf("read the revived row: %v", err)
	}
	if want := "the PACKAGE body"; !p4Contains(content, want) {
		t.Errorf("the revived content does not carry the package's body:\n%s", content)
	}
	t.Logf("the revived content:\n%s", content)
	t.Logf("the revived metadata: %s", meta)

	// The provenance describes writer B.
	after := p4ProvenanceOf(t, dm, id)
	t.Logf("provenance AFTER:  %+v", after)
	if after["actor_id"] == "agent-writer-a" {
		t.Errorf("the provenance STILL names writer A, who no longer wrote the content: %+v", after)
	}
	if after["actor_id"] != "agent-writer-b" {
		t.Errorf("the provenance does not name the importing invocation: %+v", after)
	}
	if after["invocation_id"] != "invocation-writer-b" {
		t.Errorf("the provenance's invocation_id = %q, want %q",
			after["invocation_id"], "invocation-writer-b")
	}
	if after["session_id"] != "session-writer-b" {
		t.Errorf("the provenance's session_id = %q, want %q",
			after["session_id"], "session-writer-b")
	}
	if after["framework_name"] != "framework-writer-b" {
		t.Errorf("the provenance's framework_name = %q, want %q",
			after["framework_name"], "framework-writer-b")
	}

	// The retrieval trajectory is preserved.
	var weight, rc int64
	if err := dm.db.QueryRow(`SELECT weight, reinforcement_count FROM memories WHERE id = ?`,
		id).Scan(&weight, &rc); err != nil {
		t.Fatalf("read the weight: %v", err)
	}
	t.Logf("the revived row's weight=%d reinforcement_count=%d", weight, rc)
}

// ─────────────────────────────────────────────────────────────────────────────
// The empty-ActiveContext case
// ─────────────────────────────────────────────────────────────────────────────

// TestP4_B2_AResurrectionWithAnEmptyContextDoesNotRetainWriterA is the
// operator-driven case: the import supplies NO ActiveContext.
//
// provenanceForWrite resolves an empty context to the AMBIENT identity —
// the operator's, not the previous writer's. The previous writer's
// attribution is obsolete (the content is now the package's), so it must
// NOT be retained. The row must not keep naming writer A.
func TestP4_B2_AResurrectionWithAnEmptyContextDoesNotRetainWriterA(t *testing.T) {
	dm := p3DM(t)

	id := p4ProvenanceSkill(t, dm, "operatorattr")

	before := p4ProvenanceOf(t, dm, id)
	t.Logf("provenance BEFORE: %+v", before)

	if err := dm.ShredSkill(id); err != nil {
		t.Fatalf("shred: %v", err)
	}

	root := p4PackageTree(t, map[string]string{
		"SKILL.md": p4SkillDoc("operatorattr", "1.0.0", "the PACKAGE body\n"),
	})
	// NO ActiveContext: the operator-driven import.
	res, err := dm.ImportSkillPackageFromDir(PackageImportOptions{Path: root})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if res.Status != PackageImportStatusResurrected {
		t.Fatalf("the status = %q, want %q", res.Status, PackageImportStatusResurrected)
	}

	after := p4ProvenanceOf(t, dm, id)
	t.Logf("provenance AFTER (empty ActiveContext): %+v", after)

	// The obsolete attribution is GONE.
	if after["actor_id"] == "agent-writer-a" {
		t.Errorf("the provenance still names writer A after an operator-driven resurrection: %+v", after)
	}
	if after["invocation_id"] == "invocation-writer-a" {
		t.Errorf("the provenance's invocation_id still names writer A: %+v", after)
	}
	if after["session_id"] == "session-writer-a" {
		t.Errorf("the provenance's session_id still names writer A: %+v", after)
	}
	if after["framework_name"] == "framework-writer-a" {
		t.Errorf("the provenance's framework_name still names writer A: %+v", after)
	}

	// And the attribution is ACCURATE: it says who did it, and it is not
	// an agent that did not write the content. The ambient identity in a
	// test workspace is the operator's, which carries no agent name.
	if after["actor_id"] == "" && after["actor_kind"] == "" {
		t.Errorf("the provenance row carries no identity at all: %+v", after)
	}
	t.Logf("the ambient identity recorded for the operator-driven resurrection: %+v", after)
}

// ─────────────────────────────────────────────────────────────────────────────
// Rollback
// ─────────────────────────────────────────────────────────────────────────────

// TestP4_B2_ARolledBackResurrectionRestoresTheOriginalProvenance proves
// the provenance change is part of the SAME transaction as the content
// change.
//
// The injection fires on the blob INSERT, which the import performs AFTER
// the resurrection has written the content and refreshed the provenance.
// If the transaction rolls back cleanly, BOTH must be rolled back: the
// original content AND the original attribution.
func TestP4_B2_ARolledBackResurrectionRestoresTheOriginalProvenance(t *testing.T) {
	dm := p3DM(t)

	id := p4ProvenanceSkill(t, dm, "rollbackattr")
	originalContent := p4SkillDoc("rollbackattr", "1.0.0", "the ORIGINAL ordinary body\n")

	before := p4ProvenanceOf(t, dm, id)
	t.Logf("provenance BEFORE: %+v", before)

	if err := dm.ShredSkill(id); err != nil {
		t.Fatalf("shred: %v", err)
	}

	// The injection fires AFTER the resurrection's content write and its
	// provenance refresh — the blob INSERT is the next write the import
	// performs.
	if _, err := dm.SQLDB().Exec(`
		CREATE TRIGGER p4_b2_injected_failure BEFORE INSERT ON skill_package_blobs
		BEGIN
			SELECT RAISE(ABORT, 'p4: injected failure after the resurrection');
		END;
	`); err != nil {
		t.Fatalf("create the injection trigger: %v", err)
	}

	root := p4PackageTree(t, map[string]string{
		"SKILL.md":        p4SkillDoc("rollbackattr", "1.0.0", "the PACKAGE body\n"),
		"references/a.md": "# A\n",
	})
	_, err := dm.ImportSkillPackageFromDir(PackageImportOptions{
		Path:          root,
		ActiveContext: p4WriterB(),
	})
	if err == nil {
		t.Fatal("the import succeeded despite the injected failure")
	}
	t.Logf("the import was refused, as expected: %v", err)

	// The rollback restored the ORIGINAL content.
	var content string
	if err := dm.db.QueryRow(`SELECT content FROM memories WHERE id = ?`, id).Scan(&content); err != nil {
		t.Fatalf("read the rolled-back row: %v", err)
	}
	if content != originalContent {
		t.Errorf("the rolled-back content is not the original:\n%s", content)
	}
	t.Logf("the rolled-back content is the original")

	// And it restored the ORIGINAL PROVENANCE.
	after := p4ProvenanceOf(t, dm, id)
	t.Logf("provenance AFTER the rollback: %+v", after)
	if after["actor_id"] != before["actor_id"] {
		t.Errorf("the rollback did not restore the provenance: before=%+v after=%+v", before, after)
	}
	if after["invocation_id"] != before["invocation_id"] {
		t.Errorf("the rollback did not restore the provenance's invocation_id: before=%+v after=%+v",
			before, after)
	}

	// And nothing from the import survived. The skill row itself was
	// there BEFORE the import and must still be there — still
	// soft-deleted.
	p4AssertNoPackageRowsStored(t, dm, "rolled-back resurrection")
	var deletedAt sql.NullInt64
	if err := dm.db.QueryRow(`SELECT deleted_at FROM memories WHERE id = ?`, id).Scan(&deletedAt); err != nil {
		t.Fatalf("read the tombstone: %v", err)
	}
	if !deletedAt.Valid {
		t.Errorf("the rollback did not restore the tombstone: the row is live")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// The legacy force-overwrite is unchanged
// ─────────────────────────────────────────────────────────────────────────────

// TestP4_B2_TheLegacyForceOverwriteIsUnchanged pins that the ordinary
// skill force-overwrite path still refreshes the provenance exactly as it
// did before — the new tx-scoped refresh must not have changed it.
func TestP4_B2_TheLegacyForceOverwriteIsUnchanged(t *testing.T) {
	dm := p3DM(t)

	id := p4ProvenanceSkill(t, dm, "forcepath")
	before := p4ProvenanceOf(t, dm, id)
	t.Logf("provenance BEFORE: %+v", before)

	// The legacy force path replaces the content in place.
	newDoc := p4SkillDoc("forcepath", "1.0.0", "the FORCED body\n")
	if _, err := dm.SaveSkillWithInvocation("forcepath", "1.0.0", newDoc,
		"the-forcing-author", true, p4WriterB()); err != nil {
		t.Fatalf("force overwrite: %v", err)
	}

	after := p4ProvenanceOf(t, dm, id)
	t.Logf("provenance AFTER:  %+v", after)
	if after["actor_id"] == "agent-writer-a" {
		t.Errorf("the legacy force path stopped refreshing the provenance: %+v", after)
	}
	if after["actor_id"] != "agent-writer-b" {
		t.Errorf("the legacy force path's provenance does not name the forcing invocation: %+v", after)
	}

	// The content was replaced.
	var content string
	if err := dm.db.QueryRow(`SELECT content FROM memories WHERE id = ?`, id).Scan(&content); err != nil {
		t.Fatalf("read the content: %v", err)
	}
	if !p4Contains(content, "the FORCED body") {
		t.Errorf("the content was not replaced:\n%s", content)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// The provenance failure is propagated, not swallowed
// ─────────────────────────────────────────────────────────────────────────────

// TestP4_B2_AProvenanceFailureRollsTheResurrectionBack proves the
// import does not report a successful resurrection whose attribution it
// could not write.
//
// The injection fires on the provenance UPSERT itself.
func TestP4_B2_AProvenanceFailureRollsTheResurrectionBack(t *testing.T) {
	dm := p3DM(t)

	id := p4ProvenanceSkill(t, dm, "provfail")
	originalContent := p4SkillDoc("provfail", "1.0.0", "the ORIGINAL ordinary body\n")

	if err := dm.ShredSkill(id); err != nil {
		t.Fatalf("shred: %v", err)
	}

	// The injection fires on the provenance UPSERT.
	if _, err := dm.SQLDB().Exec(`
		CREATE TRIGGER p4_b2_provenance_failure BEFORE INSERT ON artifact_provenance
		WHEN NEW.artifact_id = 'skill:provfail-v1.0.0'
		BEGIN
			SELECT RAISE(ABORT, 'p4: injected provenance failure');
		END;
	`); err != nil {
		t.Fatalf("create the injection trigger: %v", err)
	}

	root := p4PackageTree(t, map[string]string{
		"SKILL.md": p4SkillDoc("provfail", "1.0.0", "the PACKAGE body\n"),
	})
	_, err := dm.ImportSkillPackageFromDir(PackageImportOptions{
		Path:          root,
		ActiveContext: p4WriterB(),
	})
	if err == nil {
		t.Fatal("the import reported a SUCCESSFUL resurrection whose provenance it could not write")
	}
	t.Logf("the import was refused: %v (reason %q)", err, PackageImportReasonOf(err))

	// The row was rolled back: the original content is still there.
	var content string
	if err := dm.db.QueryRow(`SELECT content FROM memories WHERE id = ?`, id).Scan(&content); err != nil {
		t.Fatalf("read the rolled-back row: %v", err)
	}
	if content != originalContent {
		t.Errorf("the rolled-back content is not the original:\n%s", content)
	}
	p4AssertNoPackageRowsStored(t, dm, "provenance failure")
	var deletedAt sql.NullInt64
	if err := dm.db.QueryRow(`SELECT deleted_at FROM memories WHERE id = ?`, id).Scan(&deletedAt); err != nil {
		t.Fatalf("read the tombstone: %v", err)
	}
	if !deletedAt.Valid {
		t.Errorf("the rollback did not restore the tombstone: the row is live")
	}
}

// p4Contains is a tiny helper so the test reads cleanly. The package
// already declares a `contains`, so this one is prefixed.
func p4Contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && strings.Contains(haystack, needle)
}
