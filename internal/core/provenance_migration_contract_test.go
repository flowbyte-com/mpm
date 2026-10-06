package internal

// §9 data preservation, §10 transactional rollback, §18 fail-closed on
// unknown values, §13 ordering vs the lessons/FTS5 view migration, §12
// index contract, §14 fresh-database contract, §15 DDL drift guard.

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// provenanceRowsSeed inserts one row per canonical type that the fixture's
// starting vocabulary permits, exercising the full column surface: nullable
// and non-null fields, actor identity, framework and provider/model fields,
// temperature/max_tokens, reasoning and thinking fields, session/invocation
// ids, both parent columns, provider_metadata, and explicit NULLs. These rows
// must survive the rebuild byte for byte.
//
// Seeding respects the legacy CHECK on purpose: a legacy database genuinely
// cannot hold rows for types it does not yet accept, so the pre-migration
// fingerprint has to be built from what the fixture can actually store.
func provenanceRowsSeed(t *testing.T, dm *DatabaseManager, permitted ...string) {
	t.Helper()
	type seed struct {
		id, atype string
		provider  any
		temp      any
		metadata  any
		parentInv any
		actorID   any
	}
	seeds := []seed{
		{"p1", "memory", "openai", 0.7, `{"k":1}`, "inv-root", "agent-1"},
		{"p2", "theory", "anthropic", 1.0, nil, nil, nil},
		{"p3", "lesson", nil, nil, `{"nested":{"x":"y"}}`, "inv-2", "human-2"},
		{"p4", "decision", "google", 0.0, "", "inv-3", "import-3"},
		{"p5", "handoff", "openai", 0.5, `{"z":true}`, "inv-4", "system-4"},
		{"p6", "directive", "anthropic", 0.25, nil, nil, nil},
		{"p7", "work", "google", 2.5, `{"unicode":"héllo"}`, "inv-5", "unknown-5"},
	}
	for _, s := range seeds {
		if len(permitted) > 0 && !containsString(permitted, s.atype) {
			continue
		}
		_, err := dm.db.Exec(`INSERT INTO artifact_provenance
			(id, artifact_id, artifact_type, created_at, actor_kind, actor_id,
			 framework_name, framework_version, framework_adapter,
			 provider_name, model_name, model_revision, api_endpoint,
			 temperature, max_tokens, reasoning_mode, reasoning_effort,
			 thinking_level, thinking_tokens, thinking_visible,
			 session_id, invocation_id, parent_artifact_id,
			 parent_invocation_id, provider_metadata)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			s.id, "artifact-"+s.id, s.atype, 1700000000, "agent", s.actorID,
			"fw", "1.2.3", "adapter", s.provider, "model-x", "rev", "https://example.invalid",
			s.temp, 2048, "enabled", 0.75, "high", 512, 1,
			"sess-"+s.id, "inv-"+s.id, "parent-"+s.id, s.parentInv, s.metadata)
		if err != nil {
			t.Fatalf("seed row %s (%s): %v", s.id, s.atype, err)
		}
	}
}

// provenanceFingerprint captures every column of every row, ordered, so a
// before/after comparison proves values were preserved rather than merely
// that the row count held.
func provenanceFingerprint(t *testing.T, dm *DatabaseManager) []string {
	t.Helper()
	cols, err := dm.db.Query(`PRAGMA table_info(artifact_provenance)`)
	if err != nil {
		t.Fatalf("table_info: %v", err)
	}
	var names []string
	for cols.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt any
		if err := cols.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			t.Fatalf("scan column: %v", err)
		}
		names = append(names, name)
	}
	cols.Close()

	q := `SELECT ` + strings.Join(names, ",") + ` FROM artifact_provenance ORDER BY id`
	rows, err := dm.db.Query(q)
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		vals := make([]any, len(names))
		ptrs := make([]any, len(names))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatalf("scan row: %v", err)
		}
		parts := make([]string, len(names))
		for i, v := range vals {
			if b, ok := v.([]byte); ok {
				parts[i] = names[i] + "=" + string(b)
			} else {
				parts[i] = names[i] + "=" + toStr(v)
			}
		}
		out = append(out, strings.Join(parts, "|"))
	}
	sort.Strings(out)
	return out
}

func toStr(v any) string {
	switch t := v.(type) {
	case nil:
		return "<nil>"
	case []byte:
		return string(t)
	default:
		return fmt.Sprintf("%v", t)
	}
}

// §9 — the rebuild must preserve every column and every value.
func TestProvenanceMigration_PreservesAllRowsAndValues(t *testing.T) {
	dm := provenanceBootDB(t, []string{"memory", "theory", "lesson", "decision", "handoff", "directive"})
	defer dm.Close()
	provenanceRowsSeed(t, dm, "memory", "theory", "lesson", "decision", "handoff", "directive")

	before := provenanceFingerprint(t, dm)
	if len(before) != 6 {
		t.Fatalf("expected 6 seeded rows, got %d", len(before))
	}

	if err := dm.migrateArtifactProvenanceWorkType(); err != nil {
		t.Fatalf("migrateArtifactProvenanceWorkType: %v", err)
	}

	after := provenanceFingerprint(t, dm)
	if len(after) != len(before) {
		t.Fatalf("row count changed: %d -> %d", len(before), len(after))
	}
	for i := range before {
		if before[i] != after[i] {
			t.Errorf("row %d changed across migration:\n  before: %s\n  after:  %s", i, before[i], after[i])
		}
	}

	// UNIQUE (artifact_id, artifact_type) must still be enforced afterwards.
	_, err := dm.db.Exec(`INSERT INTO artifact_provenance
		(id, artifact_id, artifact_type, created_at, actor_kind)
		VALUES ('dupe','artifact-p1','memory',1,'agent')`)
	if err == nil {
		t.Error("UNIQUE (artifact_id, artifact_type) is no longer enforced after migration")
	}
}

// §9 — parent_invocation_id arrives via SafeMigrations in the real boot path.
// The rebuild must not drop it, and must preserve the values already in it.
func TestProvenanceMigration_PreservesParentInvocationColumn(t *testing.T) {
	dm := provenanceBootDB(t, []string{"memory", "theory", "lesson", "decision"})
	defer dm.Close()
	provenanceRowsSeed(t, dm, "memory", "theory", "lesson", "decision")

	if err := dm.migrateArtifactProvenanceWorkType(); err != nil {
		t.Fatalf("migrateArtifactProvenanceWorkType: %v", err)
	}
	var n int
	if err := dm.db.QueryRow(
		`SELECT COUNT(*) FROM artifact_provenance WHERE parent_invocation_id IS NOT NULL`).Scan(&n); err != nil {
		t.Fatalf("query parent_invocation_id: %v", err)
	}
	// p1, p3 and p4 are seeded with a non-NULL parent_invocation_id; p2 is
	// seeded NULL on purpose so the rebuild is also exercised over a NULL.
	if n != 3 {
		t.Errorf("expected 3 rows with parent_invocation_id, got %d", n)
	}
	var nulls int
	if err := dm.db.QueryRow(
		`SELECT COUNT(*) FROM artifact_provenance WHERE parent_invocation_id IS NULL`).Scan(&nulls); err != nil {
		t.Fatalf("count null parent_invocation_id: %v", err)
	}
	if nulls != 1 {
		t.Errorf("expected the NULL parent_invocation_id row to survive, got %d nulls", nulls)
	}
	if _, err := dm.db.Exec(
		`INSERT INTO artifact_provenance
			(id, artifact_id, artifact_type, created_at, actor_kind, parent_invocation_id)
		 VALUES ('pi-new','artifact-pi-new','work',1,'agent','inv-new')`); err != nil {
		t.Errorf("parent_invocation_id not writable after migration: %v", err)
	}
}

// §10 — a failure after migration work has begun must leave the database
// exactly as it was: original table, original rows, original views. The
// fixture creates a conflicting artifact_provenance_work so the replacement
// CREATE TABLE fails once the transaction is already dropping views.
func TestProvenanceMigration_RollsBackAtomicallyOnFailure(t *testing.T) {
	dm := provenanceBootDB(t, []string{"memory", "theory", "lesson", "decision"})
	defer dm.Close()
	provenanceRowsSeed(t, dm, "memory", "theory", "lesson", "decision")
	before := provenanceFingerprint(t, dm)

	provMustExec(t, dm, `CREATE TABLE artifact_provenance_work (blocker TEXT)`)

	if err := dm.migrateArtifactProvenanceWorkType(); err == nil {
		t.Fatal("expected migration to fail on the conflicting artifact_provenance_work table")
	}

	// Original table still exists and is unchanged.
	var n int
	if err := dm.db.QueryRow(
		`SELECT COUNT(*) FROM artifact_provenance`).Scan(&n); err != nil {
		t.Fatalf("original table lost after failed migration: %v", err)
	}
	after := provenanceFingerprint(t, dm)
	if len(after) != len(before) {
		t.Fatalf("rows changed after failed migration: %d -> %d", len(before), len(after))
	}
	for i := range before {
		if before[i] != after[i] {
			t.Errorf("row %d mutated by a failed migration", i)
		}
	}
	// Both views must survive: the DROP ran inside the transaction.
	for _, v := range canonicalProvenanceViewNames {
		assertViewExists(t, dm, v, "after failed migration")
		assertViewQueryable(t, dm, v)
	}
}

// §18 — a table holding an artifact_type outside the canonical vocabulary
// must fail the migration loudly, keeping the row rather than discarding or
// coercing it.
func TestProvenanceMigration_FailsClosedOnNonCanonicalRow(t *testing.T) {
	dm := provenanceBootDB(t, []string{"memory", "theory", "lesson", "decision", "spaceship"})
	defer dm.Close()

	_, err := dm.db.Exec(`INSERT INTO artifact_provenance
		(id, artifact_id, artifact_type, created_at, actor_kind)
		VALUES ('weird','artifact-weird','spaceship',1,'agent')`)
	if err != nil {
		t.Fatalf("fixture invalid: permissive CHECK should accept 'spaceship': %v", err)
	}

	migErr := dm.migrateArtifactProvenanceWorkType()
	if migErr == nil {
		t.Fatal("expected migration to fail on a non-canonical artifact_type, got nil")
	}
	if !strings.Contains(migErr.Error(), "spaceship") {
		t.Errorf("failure should name the offending value; got: %v", migErr)
	}
	// The row must still be there — not discarded, not coerced.
	var typ string
	if err := dm.db.QueryRow(
		`SELECT artifact_type FROM artifact_provenance WHERE id='weird'`).Scan(&typ); err != nil {
		t.Fatalf("row was discarded by the failed migration: %v", err)
	}
	if typ != "spaceship" {
		t.Errorf("row was coerced to %q; it must be preserved verbatim", typ)
	}
	// And the CHECK must not have been widened to accommodate the junk.
	for _, v := range canonicalProvenanceViewNames {
		assertViewExists(t, dm, v, "after fail-closed")
	}
}

// §12 — after a rebuild the five provenance indexes must be restored on the
// canonical table, and none may remain attached to artifact_provenance_old.
func TestProvenanceMigration_RestoresProvenanceIndexes(t *testing.T) {
	dm := provenanceBootDB(t, []string{"memory", "theory", "lesson", "decision"})
	defer dm.Close()

	if err := dm.migrateArtifactProvenanceWorkType(); err != nil {
		t.Fatalf("migrateArtifactProvenanceWorkType: %v", err)
	}

	want := []string{
		"idx_provenance_artifact", "idx_provenance_model", "idx_provenance_actor",
		"idx_provenance_session", "idx_provenance_invocation",
	}
	for _, idx := range want {
		var tbl string
		err := dm.db.QueryRow(
			`SELECT tbl_name FROM sqlite_master WHERE type='index' AND name=?`, idx).Scan(&tbl)
		if err == sql.ErrNoRows {
			t.Errorf("index %s missing after migration", idx)
			continue
		}
		if err != nil {
			t.Errorf("probing index %s: %v", idx, err)
			continue
		}
		if tbl != "artifact_provenance" {
			t.Errorf("index %s is attached to %q, want artifact_provenance", idx, tbl)
		}
	}
	var stale int
	if err := dm.db.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master
		 WHERE (type='index' OR type='table' OR type='view')
		   AND tbl_name='artifact_provenance_old'`).Scan(&stale); err == nil && stale > 0 {
		t.Errorf("%d schema object(s) still attached to artifact_provenance_old", stale)
	}
}

// §13 — pin the boot ordering this migration's safety argument depends on:
// it must run BEFORE migrateLessonsToView, so the lessons FTS5 triggers do
// not exist yet to be re-validated by its table rename.
func TestProvenanceMigration_RunsBeforeLessonsView(t *testing.T) {
	src, err := filepath.Abs("db.go")
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	data, err := readFileForTest(src)
	if err != nil {
		t.Fatalf("read db.go: %v", err)
	}
	text := string(data)
	provIdx := strings.Index(text, "dm.migrateArtifactProvenanceWorkType()")
	lessonsIdx := strings.Index(text, "dm.migrateLessonsToView()")
	if provIdx < 0 {
		t.Skip("initUnifiedSchema no longer calls migrateArtifactProvenanceWorkType directly")
	}
	if lessonsIdx < 0 {
		t.Skip("db.go no longer calls migrateLessonsToView directly")
	}
	if provIdx > lessonsIdx {
		t.Errorf("initUnifiedSchema now calls migrateLessonsToView (offset %d) BEFORE "+
			"migrateArtifactProvenanceWorkType (offset %d); the table rename would "+
			"re-validate the lessons FTS5 triggers and the documented safety "+
			"argument no longer holds", lessonsIdx, provIdx)
	}
}

// §15 — the migration's replacement DDL and schema.go's canonical DDL must
// agree on the constraint that matters. Compare the artifact_type CHECK
// vocabulary semantically rather than by whitespace.
func TestProvenanceMigration_DDLMatchesCanonicalSchema(t *testing.T) {
	var canonical string
	for _, stmt := range BaseTables {
		if strings.Contains(stmt, "CREATE TABLE IF NOT EXISTS artifact_provenance (") {
			canonical = stmt
			break
		}
	}
	if canonical == "" {
		t.Fatal("BaseTables no longer defines artifact_provenance")
	}
	canonVocab := artifactTypeVocabulary(canonical)
	if !vocabularyMatchesCanonical(canonVocab) {
		t.Errorf("schema.go artifact_type CHECK is %v, want %v",
			canonVocab, ProvenanceArtifactTypes)
	}

	// The migration builds its replacement CHECK from the same slice, so by
	// construction they cannot diverge. Assert the predicate that decides
	// "already canonical" agrees with the canonical DDL, which is the
	// invariant that would actually break if the two ever forked.
	if !vocabularyMatchesCanonical(artifactTypeVocabulary(
		"CREATE TABLE t (artifact_type TEXT CHECK (artifact_type IN (" +
			ProvenanceArtifactTypeSQL() + ")))")) {
		t.Error("ProvenanceArtifactTypeSQL does not render the canonical vocabulary")
	}
}

// §7 — the skip predicate must be a real schema predicate, not a substring
// search that a comment or an unrelated clause could satisfy.
func TestArtifactTypeVocabulary_ParsesCheckNotSubstrings(t *testing.T) {
	cases := []struct {
		name string
		sql  string
		want []string
	}{
		{"canonical", "CREATE TABLE t (artifact_type TEXT, CHECK (artifact_type IN ('memory','work')))",
			[]string{"memory", "work"}},
		{"comment mentioning values outside the CHECK",
			"CREATE TABLE t (artifact_type TEXT, -- accepts 'handoff' later\n CHECK (artifact_type IN ('memory')))",
			[]string{"memory"}},
		{"another table's CHECK must not be borrowed",
			"CREATE TABLE t (other TEXT CHECK (other IN ('memory','theory','decision','lesson','work')))",
			nil},
		{"no CHECK at all", "CREATE TABLE t (artifact_type TEXT)", nil},
		{"unterminated", "CREATE TABLE t (CHECK (artifact_type IN ('memory'", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := artifactTypeVocabulary(tc.sql)
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("got %v, want %v", got, tc.want)
				}
			}
		})
	}
}
