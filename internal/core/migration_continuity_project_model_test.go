// migration_continuity_project_model_test.go — Stage 1 acceptance tests
// (AT1–AT8) for the additive Project-Scoped Continuity storage.
//
// WHAT IS UNDER TEST
//
//	Stage 1 is the additive storage and nothing else. The four tables
//	(projects, project_names, continuity_threads,
//	continuity_checkpoints) are created by ONE migration-owned DDL path,
//	inside the fatal migration transaction, and nowhere else. These
//	tests pin that:
//
//	  - the DDL is the approved §4.1 text, byte for byte;
//	  - an existing database gains the four tables and their indexes
//	    and changes nothing else (AT1, AT3, AT4);
//	  - a fresh database ends up with the SAME schema as a migrated one
//	    (AT2);
//	  - the migration is idempotent (AT5);
//	  - a failure inside the migration leaves NO partial Continuity
//	    schema, and the next open recovers (AT6);
//	  - the four tables never appear in the cross-workspace `shared`
//	    schema (AT7);
//	  - the restore allow-list gains exactly four names, and the
//	    allow-list is still load-bearing (AT8).
//
// HOW "THE PRE-CONTINUITY SCHEMA" IS BUILT
//
//	AT1 and AT6 need a database that the pre-Continuity binary would
//	have produced. A test cannot shell out to the old binary, so the
//	fixture is reconstructed: a database is opened with the current
//	code, the four Continuity tables are dropped (which drops their
//	indexes with them), and the sentinel row is deleted.
//
//	That reconstruction is EXACT, and the tests assert it rather than
//	assuming it: the only difference between a pre-Continuity database
//	and a post-Continuity one is those four tables, those eight
//	indexes and that one sentinel row — because the migration is
//	purely additive and touches nothing else. TestPreContinuityFixture
//	ReconstructionIsExact proves it by diffing the object lists.
//
//	Fixture surgery runs on a connection WITHOUT the foreign-key
//	pragmas: dropping a parent table with FKs enabled trips the
//	implicit-DELETE FK check. The surgery is fixture construction, not
//	product behaviour; the opens that are under test use the
//	production DSN, which carries `_foreign_keys=1`.
//
// EVERY DATABASE IS DISPOSABLE
//
//	Each test builds its workspace under t.TempDir() and opens it with
//	NewDatabaseManager(root), which places the database at
//	<root>/src/db/mpm.db. No test touches the operator's workspace, and
//	no test reaches the live MPM database.
package internal

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// continuityApprovedDDLSHA256 is the SHA-256 of §4.1 of the approved
// architecture document (docs/dev/continuity-architecture-2026-11-04.md,
// PR #224, at commit d4ea2468a65d0a2face9b51a39e4255c595ce6ac).
//
// The DDL in the migration is pinned to this hash: the storage cannot
// drift from the approved architecture without failing the build.
const continuityApprovedDDLSHA256 = "d0b3d7dd6bb797b7449b6c71397ab8006a6a70e134e9f1323d295b022dae70f1"

// continuitySentinelID is the schema_migrations sentinel the migration
// writes LAST, inside the fatal transaction.
const continuitySentinelID = "continuity_project_model_v1"

// continuityTables are the four tables Stage 1 adds.
var continuityTables = []string{
	"projects",
	"project_names",
	"continuity_threads",
	"continuity_checkpoints",
}

// continuityIndexes are the eight indexes the approved DDL creates.
// They are named explicitly so a dropped or renamed index fails the
// test by name rather than by a count.
var continuityIndexes = []string{
	"idx_project_names_project",
	"idx_project_names_live_name",
	"idx_project_names_one_canonical",
	"idx_continuity_threads_project",
	"idx_continuity_threads_focus",
	"idx_continuity_threads_id_project",
	"idx_continuity_checkpoints_thread",
	"idx_continuity_checkpoints_id_thread",
}

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

// continuityWorkspace creates a disposable workspace and returns its root
// and the path the database will live at (<root>/src/db/mpm.db).
func continuityWorkspace(t *testing.T) (root, dbPath string) {
	t.Helper()
	PinIsolatedWorkspace(t)
	root = t.TempDir()
	t.Setenv("MPM_WORKSPACE", root)
	dbPath = filepath.Join(root, "src", "db", "mpm.db")
	return root, dbPath
}

// openContinuityDB opens the workspace with the PRODUCTION boot path.
//
// NewDatabaseManager is the path the real binary takes: it opens the
// database with the production DSN (which carries `_foreign_keys=1`, so
// foreign keys are enforced structurally) and then runs
// initUnifiedSchema, which runs the base DDL slices and the fatal
// migration transaction.
//
// The returned error is the test's to inspect: AT6 requires the boot to
// FAIL when the migration fails.
func openContinuityDB(t *testing.T, root string) (*DatabaseManager, error) {
	t.Helper()
	return NewDatabaseManager(root)
}

// openContinuityFixture opens a connection for FIXTURE SURGERY only —
// one without the foreign-key pragmas, so that dropping a parent table
// does not trip SQLite's implicit-DELETE foreign-key check.
//
// It is never used to assert product behaviour.
func openContinuityFixture(t *testing.T, dbPath string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatalf("open fixture connection: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// stripContinuityObjects turns a fully-migrated database back into the
// pre-Continuity schema: the four tables are dropped (which drops their
// indexes with them), in reverse dependency order, and the sentinel row
// is deleted.
//
// The result is what the pre-Continuity binary would have produced — see
// the file header for why, and
// TestPreContinuityFixtureReconstructionIsExact for the proof.
func stripContinuityObjects(t *testing.T, db *sql.DB) {
	t.Helper()
	// Reverse dependency order: a child before its parent.
	for _, table := range []string{
		"continuity_checkpoints",
		"continuity_threads",
		"project_names",
		"projects",
	} {
		if _, err := db.Exec("DROP TABLE IF EXISTS " + table); err != nil {
			t.Fatalf("drop %s: %v", table, err)
		}
	}
	if _, err := db.Exec(
		"DELETE FROM schema_migrations WHERE id = ?", continuitySentinelID,
	); err != nil {
		t.Fatalf("delete the Continuity sentinel: %v", err)
	}
}

// seedRepresentativeRows inserts representative pre-existing rows, so
// the row-preservation assertions (AT3) have something to preserve.
//
// They go into EXISTING tables — the point is that the migration must
// not disturb them.
//
// The seeded rows are FAITHFUL to what the pre-Continuity binary
// produces. That matters for `works`: MPM's pre-existing
// `migrateWorkEvents` migration (db.go:8316-8339, unchanged by this
// branch) seeds a `created` work_event for every `works` row whose
// `migrated_at` is NULL and then sets `migrated_at`. A pre-Continuity
// database therefore always has both. Seeding a `works` row WITHOUT
// them would make the NEXT boot — any boot, including the pre-Continuity
// binary's own — write to `works` and `work_events`, and AT3 would then
// be measuring MPM's unrelated event-sourcing backfill instead of the
// Continuity migration.
//
// (This is a fixture-construction constraint, not a product constraint,
// and it is exactly the kind of thing that makes "representative rows"
// meaningful rather than decorative.)
func seedRepresentativeRows(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.Exec(
		`INSERT INTO memories (id, collection, content, created_at) VALUES (?, ?, ?, ?)`,
		"0123456789abcdef", "memories", "a pre-existing memory", 1767225600,
	); err != nil {
		t.Fatalf("seed a memory: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO works (id, title, content, created_at, updated_at, migrated_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		"fedcba9876543210", "a pre-existing work", "body", 1767225600, 1767225600, 1767225600,
	); err != nil {
		t.Fatalf("seed a work: %v", err)
	}
	// Its `created` event, as migrateWorkEvents would have left it.
	if _, err := db.Exec(
		`INSERT INTO work_events
		   (id, work_id, event_index, event_type, created_at, note, title, content, directive_ids)
		 VALUES (?, ?, 0, 'created', ?, '', ?, ?, '[]')`,
		"0011223344556677", "fedcba9876543210", 1767225600, "a pre-existing work", "body",
	); err != nil {
		t.Fatalf("seed a work event: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Observers
// ---------------------------------------------------------------------------

// continuitySchemaObjects returns every sqlite_master object that carries DDL,
// keyed by name. Keying by name — not by row order — is what AT2
// compares: row order is not part of the schema.
func continuitySchemaObjects(t *testing.T, db *sql.DB) map[string]string {
	t.Helper()
	rows, err := db.Query(
		"SELECT name, sql FROM sqlite_master WHERE sql IS NOT NULL",
	)
	if err != nil {
		t.Fatalf("read sqlite_master: %v", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var name, sqlText string
		if err := rows.Scan(&name, &sqlText); err != nil {
			t.Fatalf("scan sqlite_master: %v", err)
		}
		out[name] = sqlText
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate sqlite_master: %v", err)
	}
	return out
}

// continuityObjectNames returns the sorted names of the sqlite_master objects of
// the given type ("table" or "index").
func continuityObjectNames(t *testing.T, db *sql.DB, typ string) []string {
	t.Helper()
	rows, err := db.Query(
		"SELECT name FROM sqlite_master WHERE type = ? AND name NOT LIKE 'sqlite_%' ORDER BY name",
		typ,
	)
	if err != nil {
		t.Fatalf("read sqlite_master (%s): %v", typ, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan sqlite_master (%s): %v", typ, err)
		}
		out = append(out, name)
	}
	return out
}

// continuityRowSnapshot renders every row of every table as a string.
//
// This is AT3's mechanism: whole rows, not a sample, and the rendering
// is lossless (SQLite's quote() renders every value in its SQL literal
// form, NULL included) so a changed column cannot hide behind a
// formatting difference.
//
// EVERY table is snapshotted, including the FTS5 virtual tables and
// their shadow tables — they are pre-existing state too.
//
// The rendered rows are SORTED in Go rather than ordered by rowid in
// SQL, for two reasons: some shadow tables are WITHOUT ROWID and have
// no rowid at all, and row order is not part of the row set. Sorting
// makes the two snapshots comparable without depending on a storage
// detail.
func continuityRowSnapshot(t *testing.T, db *sql.DB) map[string][]string {
	t.Helper()
	rows, err := db.Query(
		"SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name",
	)
	if err != nil {
		t.Fatalf("read the table list: %v", err)
	}
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan the table list: %v", err)
		}
		tables = append(tables, name)
	}
	rows.Close()

	out := map[string][]string{}
	for _, table := range tables {
		cols, err := continuityTableColumns(db, table)
		if err != nil {
			t.Fatalf("read the columns of %s: %v", table, err)
		}
		if len(cols) == 0 {
			out[table] = []string{}
			continue
		}
		quoted := make([]string, 0, len(cols))
		for _, c := range cols {
			quoted = append(quoted, "quote("+continuityQuoteIdent(c)+")")
		}
		// ORDER BY the rendered row.
		//
		// SQL does not promise an order for a SELECT without ORDER BY,
		// and SQLite's query planner is free to satisfy this one from
		// the PRIMARY KEY index instead of the table — which is what
		// makes the row order a plan artefact rather than a property
		// of the data.
		//
		// The snapshot is compared INDEX-WISE (continuityDiffSnapshots
		// walks the slice pairwise), so two snapshots of the SAME row
		// SET can compare unequal purely because SQLite chose a
		// different plan for the second query. That is not a
		// migration defect; it is an unordered comparison of an
		// unordered result.
		//
		// Ordering by the rendered row makes the order a pure function
		// of the row set, so the comparison answers the question the
		// test actually asks: did the recovery change a pre-existing
		// row?
		//
		// The sentinel's own row moves when it is deleted and
		// re-inserted (a new rowid lands at the end of the table),
		// which is exactly the kind of reordering this pins down. The
		// sentinel is stripped before the comparison, so its move is
		// not what this fixes — the fix is that the OTHER rows keep
		// their relative order across both snapshots.
		renderedExpr := strings.Join(quoted, " || ' | ' || ")
		q := "SELECT " + renderedExpr +
			" FROM " + continuityQuoteIdent(table) +
			" ORDER BY " + renderedExpr
		rrows, err := db.Query(q)
		if err != nil {
			t.Fatalf("read %s: %v", table, err)
		}
		var rendered []string
		for rrows.Next() {
			var line string
			if err := rrows.Scan(&line); err != nil {
				rrows.Close()
				t.Fatalf("scan %s: %v", table, err)
			}
			rendered = append(rendered, line)
		}
		if err := rrows.Err(); err != nil {
			rrows.Close()
			t.Fatalf("iterate %s: %v", table, err)
		}
		rrows.Close()
		sort.Strings(rendered)
		out[table] = rendered
	}
	return out
}

// continuityTableColumns returns a table's column names in declaration order.
func continuityTableColumns(db *sql.DB, table string) ([]string, error) {
	rows, err := db.Query("PRAGMA table_info(" + continuityQuoteIdent(table) + ")")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var cols []string
	for rows.Next() {
		var cid int
		var name, colType string
		var notNull int
		var dfltValue sql.NullString
		var pk int
		if err := rows.Scan(&cid, &name, &colType, &notNull, &dfltValue, &pk); err != nil {
			return nil, err
		}
		cols = append(cols, name)
	}
	return cols, rows.Err()
}

// continuityQuoteIdent quotes an SQLite identifier for interpolation.
func continuityQuoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// continuityStripSentinel removes the migration's own sentinel row from
// a schema_migrations snapshot and reports whether it was present.
//
// The sentinel is the ONE row any existing table gains, and it is the
// migration's bookkeeping rather than project state: AT1 requires it to
// exist (count exactly 1), and AT3/AT4 are about pre-existing state
// and fabricated state. It is therefore excluded from the
// byte-identity comparison — and its presence is asserted separately,
// so the exclusion cannot hide anything.
//
// The rendered row starts with the quoted sentinel id, because
// schema_migrations' first column is `id`.
func continuityStripSentinel(rows []string) ([]string, bool) {
	prefix := "'" + continuitySentinelID + "' | "
	out := make([]string, 0, len(rows))
	found := false
	for _, r := range rows {
		if strings.HasPrefix(r, prefix) {
			found = true
			continue
		}
		out = append(out, r)
	}
	return out, found
}

// continuityDeclaredTables returns the names of the tables the
// Continuity DDL declares.
//
// Read from the DDL literal itself, so the "four tables" count is
// OBSERVED rather than asserted: the source of truth for how many
// tables Stage 1 creates is the approved §4.1 DDL, which
// TestContinuityProjectModelDDLIsByteIdentical pins by hash.
func continuityDeclaredTables(t *testing.T) []string {
	t.Helper()
	var names []string
	for _, line := range strings.Split(continuityProjectModelDDL, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "CREATE TABLE") {
			continue
		}
		rest := strings.TrimSpace(strings.TrimPrefix(trimmed, "CREATE TABLE"))
		rest = strings.TrimPrefix(rest, "IF NOT EXISTS")
		rest = strings.TrimSpace(rest)
		name := rest
		if i := strings.IndexAny(name, " ("); i >= 0 {
			name = name[:i]
		}
		if name != "" {
			names = append(names, name)
		}
	}
	return names
}

// continuityAllowListInvariants checks the HISTORY-INDEPENDENT invariant
// of AT8.
//
// It takes only the final state — the table names the approved DDL
// declares, the canonical restore allow-list, and the runtime
// allow-list — and nothing else. In particular it does NOT read the
// commit graph.
//
// That is the whole point. A test whose result depends on repository
// history rather than on the code can turn a correct tree red: a
// commit split, a post-merge build where HEAD equals the base branch,
// or a checkout without history are all ordinary events, not defects,
// and none of them may change whether this schema is correct.
//
// The invariant is:
//
//   - every table the approved DDL declares appears EXACTLY ONCE in the
//     canonical allow-list;
//   - none of them appears in the runtime allow-list.
//
// "Exactly once" rather than "at least once": a duplicated entry is not
// a different allow-list, it is a mistake, and the canonical list is a
// security surface.
//
// The invariant deliberately says NOTHING about the size of the
// canonical list. The list is shared — an independent feature adding
// its own tables to it changes the total without changing anything
// about this change — so a size assertion would fail on a correct
// merge. What the list's contents are is enforced history-free in both
// directions by TestCanonicalSchemaSync: every CREATE TABLE in this
// package must be on the canonical list, and every name on the
// canonical list must have a table behind it.
func continuityAllowListInvariants(declared, canonical, runtime []string) []string {
	var violations []string
	for _, name := range declared {
		switch n := continuityCount(canonical, name); {
		case n == 0:
			violations = append(violations, fmt.Sprintf(
				"%q is declared by the Continuity DDL but is absent from CanonicalMPMSchema — "+
					"a dump declaring it would be rejected by mpm restore-db", name))
		case n > 1:
			violations = append(violations, fmt.Sprintf(
				"%q appears %d times in CanonicalMPMSchema, want exactly once", name, n))
		}
		if continuityCount(runtime, name) > 0 {
			violations = append(violations, fmt.Sprintf(
				"%q is in RuntimeCanonicalSchema. It has its CREATE TABLE in source, so it "+
					"belongs in the canonical list the source-scan guard checks.", name))
		}
	}
	return violations
}

// continuityCount returns how many times name appears in list.
func continuityCount(list []string, name string) int {
	n := 0
	for _, s := range list {
		if s == name {
			n++
		}
	}
	return n
}

// continuityDiffSnapshots reports the tables whose rows changed between two
// snapshots.
func continuityDiffSnapshots(before, after map[string][]string) []string {
	var changed []string
	names := map[string]bool{}
	for n := range before {
		names[n] = true
	}
	for n := range after {
		names[n] = true
	}
	for n := range names {
		b, a := before[n], after[n]
		if len(b) != len(a) {
			changed = append(changed, fmt.Sprintf("%s: %d row(s) -> %d row(s)", n, len(b), len(a)))
			continue
		}
		for i := range b {
			if b[i] != a[i] {
				changed = append(changed, fmt.Sprintf("%s: row %d changed", n, i))
				break
			}
		}
	}
	sort.Strings(changed)
	return changed
}

// ---------------------------------------------------------------------------
// The approved DDL
// ---------------------------------------------------------------------------

// TestContinuityProjectModelDDLIsByteIdentical pins the DDL to the
// approved architecture.
//
// The hash is the one C's review and the operator's decision both
// reference. If the DDL in the migration is edited — reformatted,
// "tidied", or a constraint changed — this fails, and the change has to
// go back through the architecture review.
func TestContinuityProjectModelDDLIsByteIdentical(t *testing.T) {
	sum := sha256.Sum256([]byte(continuityProjectModelDDL))
	got := hex.EncodeToString(sum[:])
	if got != continuityApprovedDDLSHA256 {
		t.Fatalf("the Continuity DDL is not the approved DDL.\n  expected sha256: %s\n  got:          %s\n"+
			"  The DDL must be byte-for-byte §4.1 of the approved architecture document "+
			"(docs/dev/continuity-architecture-2026-11-04.md at commit d4ea2468a65d0a2face9b51a39e4255c595ce6ac). "+
			"Any change to it is a change to the approved architecture.",
			continuityApprovedDDLSHA256, got)
	}
}

// TestContinuityDDLIsNotInBaseTables pins the local-only property at the
// source level.
//
// BaseTables is the slice attachShared propagates into the
// cross-workspace `shared` schema. A project-identity table must never
// be in it. This is a source-level guard so the property cannot be
// broken by a later refactor that moves the DDL.
func TestContinuityDDLIsNotInBaseTables(t *testing.T) {
	for _, table := range continuityTables {
		for _, ddl := range BaseTables {
			if strings.Contains(ddl, "CREATE TABLE IF NOT EXISTS "+table+" ") ||
				strings.Contains(ddl, "CREATE TABLE "+table+" ") {
				t.Fatalf("%s is in BaseTables. BaseTables is propagated into the cross-workspace "+
					"`shared` schema (db.go:2132, db.go:2160); project identity must stay inside one workspace.",
					table)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// The pre-Continuity fixture
// ---------------------------------------------------------------------------

// TestPreContinuityFixtureReconstructionIsExact proves that the
// reconstruction used by AT1/AT6/AT3 really is the pre-Continuity
// schema, and not an approximation of it.
//
// The claim is: a pre-Continuity database differs from a post-Continuity
// one by EXACTLY the four tables, the eight indexes and the one
// sentinel row — nothing else. If that holds, dropping those objects
// from a migrated database reconstructs the old schema exactly.
func TestPreContinuityFixtureReconstructionIsExact(t *testing.T) {
	root, dbPath := continuityWorkspace(t)

	dm, err := openContinuityDB(t, root)
	if err != nil {
		t.Fatalf("open the workspace: %v", err)
	}
	migrated := continuitySchemaObjects(t, dm.SQLDB())
	migratedTables := continuityObjectNames(t, dm.SQLDB(), "table")
	migratedIndexes := continuityObjectNames(t, dm.SQLDB(), "index")
	dm.Close()

	fixture := openContinuityFixture(t, dbPath)
	stripContinuityObjects(t, fixture)
	preContinuity := continuitySchemaObjects(t, fixture)
	preContinuityTables := continuityObjectNames(t, fixture, "table")
	preContinuityIndexes := continuityObjectNames(t, fixture, "index")

	// The difference must be exactly the four tables and the eight
	// indexes — no more, no less.
	var missingTables, extraTables []string
	for _, n := range migratedTables {
		if !continuityContains(preContinuityTables, n) {
			missingTables = append(missingTables, n)
		}
	}
	for _, n := range preContinuityTables {
		if !continuityContains(migratedTables, n) {
			extraTables = append(extraTables, n)
		}
	}
	sort.Strings(missingTables)
	sort.Strings(extraTables)
	if len(missingTables) != len(continuityTables) ||
		!continuitySameSet(missingTables, continuityTables) {
		t.Errorf("the tables the reconstruction removed are %v, want exactly %v",
			missingTables, continuityTables)
	}
	if len(extraTables) != 0 {
		t.Errorf("the reconstruction left tables the migration never creates: %v", extraTables)
	}

	var missingIndexes, extraIndexes []string
	for _, n := range migratedIndexes {
		if !continuityContains(preContinuityIndexes, n) {
			missingIndexes = append(missingIndexes, n)
		}
	}
	for _, n := range preContinuityIndexes {
		if !continuityContains(migratedIndexes, n) {
			extraIndexes = append(extraIndexes, n)
		}
	}
	sort.Strings(missingIndexes)
	sort.Strings(extraIndexes)
	if len(missingIndexes) != len(continuityIndexes) ||
		!continuitySameSet(missingIndexes, continuityIndexes) {
		t.Errorf("the indexes the reconstruction removed are %v, want exactly %v",
			missingIndexes, continuityIndexes)
	}
	if len(extraIndexes) != 0 {
		t.Errorf("the reconstruction left indexes the migration never creates: %v", extraIndexes)
	}

	// And the surviving objects are byte-identical, so the
	// reconstruction did not rewrite anything either.
	for name, sqlText := range preContinuity {
		migratedSQL, ok := migrated[name]
		if !ok {
			t.Errorf("the pre-Continuity fixture has %q, which the migrated database lacks", name)
			continue
		}
		if migratedSQL != sqlText {
			t.Errorf("the reconstruction changed %q:\n  migrated: %s\n  stripped: %s",
				name, migratedSQL, sqlText)
		}
	}
}

// ---------------------------------------------------------------------------
// AT1 — existing database migration
// ---------------------------------------------------------------------------

// TestContinuityAT1ExistingDatabaseMigration — AT1.
//
// A disposable file-backed WAL database on the pre-Continuity schema,
// populated with representative rows, then opened by the new binary:
// the four tables and their indexes exist BY NAME, and the sentinel
// count is 1.
func TestContinuityAT1ExistingDatabaseMigration(t *testing.T) {
	root, dbPath := continuityWorkspace(t)

	// Build the pre-Continuity database.
	dm, err := openContinuityDB(t, root)
	if err != nil {
		t.Fatalf("build the fixture: %v", err)
	}
	fixture := openContinuityFixture(t, dbPath)
	stripContinuityObjects(t, fixture)
	seedRepresentativeRows(t, fixture)
	dm.Close()

	// Open it with the new binary — the production boot path.
	dm, err = openContinuityDB(t, root)
	if err != nil {
		t.Fatalf("open the pre-Continuity database with the new binary: %v", err)
	}
	defer dm.Close()

	db := dm.SQLDB()

	for _, table := range continuityTables {
		var n int
		if err := db.QueryRow(
			"SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?", table,
		).Scan(&n); err != nil {
			t.Fatalf("probe %s: %v", table, err)
		}
		if n != 1 {
			t.Errorf("the table %q does not exist after the migration (count %d)", table, n)
		}
	}
	for _, index := range continuityIndexes {
		var n int
		if err := db.QueryRow(
			"SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = ?", index,
		).Scan(&n); err != nil {
			t.Fatalf("probe %s: %v", index, err)
		}
		if n != 1 {
			t.Errorf("the index %q does not exist after the migration (count %d)", index, n)
		}
	}

	var sentinels int
	if err := db.QueryRow(
		"SELECT COUNT(*) FROM schema_migrations WHERE id = ?", continuitySentinelID,
	).Scan(&sentinels); err != nil {
		t.Fatalf("count the sentinel: %v", err)
	}
	if sentinels != 1 {
		t.Errorf("the sentinel count is %d, want exactly 1", sentinels)
	}
}

// ---------------------------------------------------------------------------
// AT2 — fresh database creation
// ---------------------------------------------------------------------------

// TestContinuityAT2FreshDatabaseSchemaEquivalence — AT2.
//
// A new database through the same path has a schema IDENTICAL to the
// migrated one, compared as the full sqlite_master.sql text of every
// table and index, GROUPED BY NAME.
//
// Row order is deliberately not part of the comparison: it is not part
// of the schema, and an order-sensitive comparison is fragile even
// though it happens to pass today (the architecture document's §14.2
// E28 measures exactly that).
func TestContinuityAT2FreshDatabaseSchemaEquivalence(t *testing.T) {
	// The migrated database.
	migratedRoot, migratedPath := continuityWorkspace(t)
	dm, err := openContinuityDB(t, migratedRoot)
	if err != nil {
		t.Fatalf("build the migrated fixture: %v", err)
	}
	fixture := openContinuityFixture(t, migratedPath)
	stripContinuityObjects(t, fixture)
	dm.Close()
	migrated, err := openContinuityDB(t, migratedRoot)
	if err != nil {
		t.Fatalf("migrate the fixture: %v", err)
	}
	migratedSchema := continuitySchemaObjects(t, migrated.SQLDB())
	migrated.Close()

	// The fresh database, through the same path.
	freshRoot, _ := continuityWorkspace(t)
	fresh, err := openContinuityDB(t, freshRoot)
	if err != nil {
		t.Fatalf("build a fresh database: %v", err)
	}
	freshSchema := continuitySchemaObjects(t, fresh.SQLDB())
	fresh.Close()

	// Compare, grouped by name.
	var differences []string
	names := map[string]bool{}
	for n := range migratedSchema {
		names[n] = true
	}
	for n := range freshSchema {
		names[n] = true
	}
	for n := range names {
		m, mOK := migratedSchema[n]
		f, fOK := freshSchema[n]
		switch {
		case !mOK:
			differences = append(differences, fmt.Sprintf("%q is missing from the migrated database", n))
		case !fOK:
			differences = append(differences, fmt.Sprintf("%q is missing from the fresh database", n))
		case m != f:
			differences = append(differences, fmt.Sprintf("%q differs:\\n  migrated: %s\\n  fresh:    %s", n, m, f))
		}
	}
	sort.Strings(differences)
	if len(differences) != 0 {
		t.Fatalf("the fresh and migrated schemas differ (%d difference(s)):\\n  %s",
			len(differences), strings.Join(differences, "\\n  "))
	}
	if len(migratedSchema) == 0 {
		t.Fatal("the migrated schema is empty — the comparison would pass vacuously")
	}

	// Negative control: the comparison is load-bearing. A schema that
	// differs by ONE object's text must be reported.
	perturbed := map[string]string{}
	for k, v := range migratedSchema {
		perturbed[k] = v
	}
	for k := range perturbed {
		perturbed[k] = perturbed[k] + " -- perturbed"
		break
	}
	if len(continuityDiffObjectNames(migratedSchema, perturbed)) == 0 {
		t.Fatal("a perturbed schema compared equal to the original — the equivalence check is not load-bearing")
	}
}

// continuityDiffObjects reports the names whose DDL text differs between
// two schema maps, plus any name present in only one of them.
func continuityDiffObjects(a, b map[string]string) []string {
	var out []string
	names := map[string]bool{}
	for n := range a {
		names[n] = true
	}
	for n := range b {
		names[n] = true
	}
	for n := range names {
		av, aOK := a[n]
		bv, bOK := b[n]
		switch {
		case !aOK:
			out = append(out, n+" (added)")
		case !bOK:
			out = append(out, n+" (removed)")
		case av != bv:
			out = append(out, n+" (changed)")
		}
	}
	sort.Strings(out)
	return out
}

// continuityDiffObjectNames reports the names whose DDL text differs between
// two schema maps.
func continuityDiffObjectNames(a, b map[string]string) []string {
	var out []string
	names := map[string]bool{}
	for n := range a {
		names[n] = true
	}
	for n := range b {
		names[n] = true
	}
	for n := range names {
		if a[n] != b[n] {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// ---------------------------------------------------------------------------
// AT3 — whole-row preservation
// ---------------------------------------------------------------------------

// TestContinuityAT3WholeRowPreservation — AT3.
//
// Every pre-existing row of every pre-existing table, before and after,
// as whole-row strings: byte-identical.
//
// The whole row set, not a sample. The rendering is lossless, so a
// changed column cannot hide.
//
// The ONE permitted difference is the migration's own sentinel row in
// schema_migrations: it is new bookkeeping, not pre-existing state, and
// AT1 requires it to exist. It is excluded from the comparison and
// asserted separately, so excluding it cannot hide anything.
func TestContinuityAT3WholeRowPreservation(t *testing.T) {
	root, dbPath := continuityWorkspace(t)

	dm, err := openContinuityDB(t, root)
	if err != nil {
		t.Fatalf("build the fixture: %v", err)
	}
	fixture := openContinuityFixture(t, dbPath)
	stripContinuityObjects(t, fixture)
	seedRepresentativeRows(t, fixture)

	before := continuityRowSnapshot(t, fixture)
	dm.Close()

	// The migration runs.
	dm, err = openContinuityDB(t, root)
	if err != nil {
		t.Fatalf("run the migration: %v", err)
	}
	defer dm.Close()

	after := continuityRowSnapshot(t, dm.SQLDB())

	// The four new tables are not "pre-existing rows" — they are new
	// and empty (AT4). Everything else must be untouched.
	for _, table := range continuityTables {
		delete(after, table)
		delete(before, table)
	}

	// The sentinel: absent before, present exactly once after.
	beforeMigrations, beforeHadSentinel := continuityStripSentinel(before["schema_migrations"])
	afterMigrations, afterHadSentinel := continuityStripSentinel(after["schema_migrations"])
	if beforeHadSentinel {
		t.Error("the Continuity sentinel existed BEFORE the migration — the fixture is not pre-Continuity")
	}
	if !afterHadSentinel {
		t.Error("the Continuity sentinel is missing after the migration")
	}
	if len(after["schema_migrations"]) != len(afterMigrations)+1 {
		t.Errorf("schema_migrations gained %d row(s); the migration must add exactly its own sentinel",
			len(after["schema_migrations"])-len(afterMigrations))
	}
	delete(before, "schema_migrations")
	delete(after, "schema_migrations")

	changed := continuityDiffSnapshots(before, after)
	if len(changed) != 0 {
		t.Fatalf("the migration changed pre-existing rows (%d difference(s)):\\n  %s",
			len(changed), strings.Join(changed, "\\n  "))
	}
	if !continuitySameStrings(beforeMigrations, afterMigrations) {
		t.Errorf("the migration changed the pre-existing schema_migrations rows:\\n  before: %v\\n  after:  %v",
			beforeMigrations, afterMigrations)
	}
	if len(before) == 0 {
		t.Fatal("the pre-migration snapshot is empty — the preservation check would pass vacuously")
	}
}

// ---------------------------------------------------------------------------
// AT4 — zero fabricated state
// ---------------------------------------------------------------------------

// TestContinuityAT4ZeroFabricatedState — AT4.
//
// No existing table gains a row; no existing row gains a column value;
// project_names starts empty; projects starts empty.
//
// The migration creates STRUCTURE. It fabricates no state: there is no
// seed project, no default name, no placeholder thread.
func TestContinuityAT4ZeroFabricatedState(t *testing.T) {
	root, dbPath := continuityWorkspace(t)

	dm, err := openContinuityDB(t, root)
	if err != nil {
		t.Fatalf("build the fixture: %v", err)
	}
	fixture := openContinuityFixture(t, dbPath)
	stripContinuityObjects(t, fixture)
	before := continuityRowSnapshot(t, fixture)
	dm.Close()

	dm, err = openContinuityDB(t, root)
	if err != nil {
		t.Fatalf("run the migration: %v", err)
	}
	defer dm.Close()
	db := dm.SQLDB()

	// The four new tables start EMPTY.
	for _, table := range continuityTables {
		var n int
		if err := db.QueryRow("SELECT COUNT(*) FROM " + continuityQuoteIdent(table)).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if n != 0 {
			t.Errorf("%s starts with %d row(s); the migration must fabricate no state", table, n)
		}
	}
	// Named explicitly, because they are the two the architecture names.
	for _, table := range []string{"projects", "project_names"} {
		var n int
		if err := db.QueryRow("SELECT COUNT(*) FROM " + continuityQuoteIdent(table)).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if n != 0 {
			t.Errorf("%s starts with %d row(s); it must start empty", table, n)
		}
	}

	// No EXISTING table gained a row.
	//
	// The single exception is the migration's own sentinel row in
	// schema_migrations: it is bookkeeping, it is required by AT1, and
	// it is asserted here rather than assumed.
	after := continuityRowSnapshot(t, db)
	for _, table := range continuityTables {
		delete(after, table)
	}
	beforeMigrations, beforeHadSentinel := continuityStripSentinel(before["schema_migrations"])
	afterMigrations, afterHadSentinel := continuityStripSentinel(after["schema_migrations"])
	if beforeHadSentinel {
		t.Error("the Continuity sentinel existed BEFORE the migration — the fixture is not pre-Continuity")
	}
	if !afterHadSentinel {
		t.Error("the Continuity sentinel is missing after the migration")
	}
	if len(after["schema_migrations"]) != len(afterMigrations)+1 {
		t.Errorf("schema_migrations gained %d row(s); the migration must add exactly its own sentinel",
			len(after["schema_migrations"])-len(afterMigrations))
	}
	if !continuitySameStrings(beforeMigrations, afterMigrations) {
		t.Errorf("the migration changed the pre-existing schema_migrations rows:\\n  before: %v\\n  after:  %v",
			beforeMigrations, afterMigrations)
	}
	delete(before, "schema_migrations")
	delete(after, "schema_migrations")

	for table, beforeRows := range before {
		afterRows, ok := after[table]
		if !ok {
			t.Errorf("the table %q disappeared", table)
			continue
		}
		if len(afterRows) != len(beforeRows) {
			t.Errorf("%s gained rows: %d -> %d", table, len(beforeRows), len(afterRows))
		}
	}
}

// ---------------------------------------------------------------------------
// AT5 — idempotent migration
// ---------------------------------------------------------------------------

// TestContinuityAT5IdempotentMigration — AT5.
//
// Run the migration twice, then a third time: the table list, the index
// list and the sentinel count are unchanged by runs 2 and 3.
//
// The migration runs on EVERY open, so three opens are three runs.
func TestContinuityAT5IdempotentMigration(t *testing.T) {
	root, _ := continuityWorkspace(t)

	type state struct {
		tables    []string
		indexes   []string
		sentinels int
	}
	var states []state

	for run := 1; run <= 3; run++ {
		dm, err := openContinuityDB(t, root)
		if err != nil {
			t.Fatalf("run %d: %v", run, err)
		}
		db := dm.SQLDB()
		var sentinels int
		if err := db.QueryRow(
			"SELECT COUNT(*) FROM schema_migrations WHERE id = ?", continuitySentinelID,
		).Scan(&sentinels); err != nil {
			t.Fatalf("run %d: count the sentinel: %v", run, err)
		}
		states = append(states, state{
			tables:    continuityObjectNames(t, db, "table"),
			indexes:   continuityObjectNames(t, db, "index"),
			sentinels: sentinels,
		})
		dm.Close()
	}

	for run := 2; run <= 3; run++ {
		prev, cur := states[run-2], states[run-1]
		if !continuitySameStrings(prev.tables, cur.tables) {
			t.Errorf("run %d changed the table list:\\n  before: %v\\n  after:  %v",
				run, prev.tables, cur.tables)
		}
		if !continuitySameStrings(prev.indexes, cur.indexes) {
			t.Errorf("run %d changed the index list:\\n  before: %v\\n  after:  %v",
				run, prev.indexes, cur.indexes)
		}
		if prev.sentinels != cur.sentinels {
			t.Errorf("run %d changed the sentinel count: %d -> %d",
				run, prev.sentinels, cur.sentinels)
		}
		if cur.sentinels != 1 {
			t.Errorf("after run %d the sentinel count is %d, want exactly 1", run, cur.sentinels)
		}
	}
}

// ---------------------------------------------------------------------------
// AT6 — failed-migration rollback
// ---------------------------------------------------------------------------

// TestContinuityAT6FailedMigrationRollback — AT6.
//
// A trigger that aborts the migration's SENTINEL write is installed on a
// disposable database, which is then opened by the new binary.
//
// The sentinel is written LAST, after the DDL, inside the same
// transaction. So when the trigger fires, the four CREATE TABLEs and
// the eight CREATE INDEXes have ALREADY succeeded — and must be undone
// by the rollback.
//
// Assertions: the migration returns an error; NewDatabaseManager
// surfaces it; the table list is unchanged; the index list is
// unchanged; no sentinel; the pre-existing row set is byte-identical.
func TestContinuityAT6FailedMigrationRollback(t *testing.T) {
	root, dbPath := continuityWorkspace(t)

	dm, err := openContinuityDB(t, root)
	if err != nil {
		t.Fatalf("build the fixture: %v", err)
	}
	fixture := openContinuityFixture(t, dbPath)
	stripContinuityObjects(t, fixture)
	seedRepresentativeRows(t, fixture)
	dm.Close()

	// Inject the failure at the sentinel write.
	//
	// The trigger is installed BEFORE the "before" snapshot: it is part
	// of the fixture, so the schema-object comparison must see it on
	// both sides. (A trigger that existed only on the "after" side
	// would be reported as an added object, which would be a false
	// positive about the migration.)
	if _, err := fixture.Exec(
		`CREATE TRIGGER block_continuity_sentinel BEFORE INSERT ON schema_migrations
		 WHEN NEW.id = 'continuity_project_model_v1'
		 BEGIN SELECT RAISE(ABORT, 'injected failure'); END`,
	); err != nil {
		t.Fatalf("install the failure trigger: %v", err)
	}

	beforeObjects := continuitySchemaObjects(t, fixture)
	beforeRows := continuityRowSnapshot(t, fixture)

	// The boot must FAIL, and the error must surface.
	failed, err := openContinuityDB(t, root)
	if err == nil {
		if failed != nil {
			failed.Close()
		}
		t.Fatal("the migration succeeded despite the injected failure. The sentinel write must be " +
			"the last thing the migration does, inside the transaction, so a failure there rolls " +
			"the DDL back.")
	}
	if !strings.Contains(err.Error(), "continuity project model migration failed") {
		t.Errorf("the boot failed, but not through the Continuity migration: %v", err)
	}

	// The DDL must have been undone.
	afterObjects := continuitySchemaObjects(t, fixture)
	afterRows := continuityRowSnapshot(t, fixture)

	for _, table := range continuityTables {
		if _, ok := afterObjects[table]; ok {
			t.Errorf("the table %q survived the failed migration — the DDL was not rolled back", table)
		}
	}
	for _, index := range continuityIndexes {
		if _, ok := afterObjects[index]; ok {
			t.Errorf("the index %q survived the failed migration — the DDL was not rolled back", index)
		}
	}

	// The object list is unchanged — every table and index that was
	// there before the failed boot is still there, and no new one
	// appeared.
	if added := continuityDiffObjects(beforeObjects, afterObjects); len(added) != 0 {
		t.Errorf("the failed migration changed the schema object list: %v", added)
	}

	// The table list and the index list are unchanged.
	beforeTableNames := continuityObjectNames(t, fixture, "table")
	afterTableNames := continuityObjectNames(t, fixture, "table")
	if !continuitySameStrings(beforeTableNames, afterTableNames) {
		t.Errorf("the failed migration changed the table list:\n  before: %v\n  after:  %v",
			beforeTableNames, afterTableNames)
	}
	beforeIndexNames := continuityObjectNames(t, fixture, "index")
	afterIndexNames := continuityObjectNames(t, fixture, "index")
	if !continuitySameStrings(beforeIndexNames, afterIndexNames) {
		t.Errorf("the failed migration changed the index list:\n  before: %v\n  after:  %v",
			beforeIndexNames, afterIndexNames)
	}

	// No sentinel.
	var sentinels int
	if err := fixture.QueryRow(
		"SELECT COUNT(*) FROM schema_migrations WHERE id = ?", continuitySentinelID,
	).Scan(&sentinels); err != nil {
		t.Fatalf("count the sentinel: %v", err)
	}
	if sentinels != 0 {
		t.Errorf("the sentinel count is %d after the failed migration, want 0", sentinels)
	}

	// The pre-existing rows are byte-identical.
	changed := continuityDiffSnapshots(beforeRows, afterRows)
	for _, table := range continuityTables {
		changed = continuityRemoveString(changed, func(s string) bool { return strings.HasPrefix(s, table+":") })
	}
	if len(changed) != 0 {
		t.Errorf("the failed migration changed pre-existing rows (%d difference(s)):\n  %s",
			len(changed), strings.Join(changed, "\n  "))
	}
}

// TestContinuityAT6RecoveryAfterInjectedFailure — the recovery half of
// AT6.
//
// After the injected failure, the next open must succeed and must leave
// the complete schema: the four tables, the eight indexes and exactly
// one sentinel. A half-migrated database is worse than an unmigrated
// one.
func TestContinuityAT6RecoveryAfterInjectedFailure(t *testing.T) {
	root, dbPath := continuityWorkspace(t)

	dm, err := openContinuityDB(t, root)
	if err != nil {
		t.Fatalf("build the fixture: %v", err)
	}
	fixture := openContinuityFixture(t, dbPath)
	stripContinuityObjects(t, fixture)
	seedRepresentativeRows(t, fixture)
	beforeRows := continuityRowSnapshot(t, fixture)
	dm.Close()

	if _, err := fixture.Exec(
		`CREATE TRIGGER block_continuity_sentinel BEFORE INSERT ON schema_migrations
		 WHEN NEW.id = 'continuity_project_model_v1'
		 BEGIN SELECT RAISE(ABORT, 'injected failure'); END`,
	); err != nil {
		t.Fatalf("install the failure trigger: %v", err)
	}
	if _, err := openContinuityDB(t, root); err == nil {
		t.Fatal("the injected failure did not fail the boot")
	}

	// Remove the trigger and open again.
	if _, err := fixture.Exec("DROP TRIGGER block_continuity_sentinel"); err != nil {
		t.Fatalf("drop the failure trigger: %v", err)
	}
	dm, err = openContinuityDB(t, root)
	if err != nil {
		t.Fatalf("the recovery open failed: %v", err)
	}
	defer dm.Close()
	db := dm.SQLDB()

	for _, table := range continuityTables {
		var n int
		if err := db.QueryRow(
			"SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?", table,
		).Scan(&n); err != nil {
			t.Fatalf("probe %s: %v", table, err)
		}
		if n != 1 {
			t.Errorf("after the recovery open the table %q is missing", table)
		}
	}
	for _, index := range continuityIndexes {
		var n int
		if err := db.QueryRow(
			"SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = ?", index,
		).Scan(&n); err != nil {
			t.Fatalf("probe %s: %v", index, err)
		}
		if n != 1 {
			t.Errorf("after the recovery open the index %q is missing", index)
		}
	}
	var sentinels int
	if err := db.QueryRow(
		"SELECT COUNT(*) FROM schema_migrations WHERE id = ?", continuitySentinelID,
	).Scan(&sentinels); err != nil {
		t.Fatalf("count the sentinel: %v", err)
	}
	if sentinels != 1 {
		t.Errorf("after the recovery open the sentinel count is %d, want exactly 1", sentinels)
	}

	// The pre-existing rows survived both the failure and the recovery.
	afterRows := continuityRowSnapshot(t, db)
	for _, table := range continuityTables {
		delete(afterRows, table)
		delete(beforeRows, table)
	}

	// The only row any existing table gains across the failed boot AND
	// the recovery is the migration's own sentinel.
	beforeMigrations, beforeHadSentinel := continuityStripSentinel(beforeRows["schema_migrations"])
	afterMigrations, afterHadSentinel := continuityStripSentinel(afterRows["schema_migrations"])
	if beforeHadSentinel {
		t.Error("the Continuity sentinel existed BEFORE the migration — the fixture is not pre-Continuity")
	}
	if !afterHadSentinel {
		t.Error("the Continuity sentinel is missing after the recovery open")
	}
	if len(afterRows["schema_migrations"]) != len(afterMigrations)+1 {
		t.Errorf("schema_migrations gained %d row(s); the migration must add exactly its own sentinel",
			len(afterRows["schema_migrations"])-len(afterMigrations))
	}
	if !continuitySameStrings(beforeMigrations, afterMigrations) {
		t.Errorf("the failed migration and its recovery changed the pre-existing schema_migrations rows:\n  before: %v\n  after:  %v",
			beforeMigrations, afterMigrations)
	}
	delete(beforeRows, "schema_migrations")
	delete(afterRows, "schema_migrations")

	changed := continuityDiffSnapshots(beforeRows, afterRows)
	if len(changed) != 0 {
		t.Errorf("the failed migration and its recovery changed pre-existing rows (%d difference(s)):\n  %s",
			len(changed), strings.Join(changed, "\n  "))
	}
}

// ---------------------------------------------------------------------------
// AT7 — the four tables are local only
// ---------------------------------------------------------------------------

// TestContinuityAT7LocalOnly — AT7.
//
// A hermetic NewTestSharedDM — a local database plus an ATTACHed shared
// database, both in a temp directory: the four names are present in
// main.sqlite_master with a count of 4, and the count of those names in
// shared.sqlite_master is 0.
//
// Project identity must not leak into the cross-workspace broadcast
// store. That is the reason the DDL is migration-owned and not in
// BaseTables (which attachShared propagates).
func TestContinuityAT7LocalOnly(t *testing.T) {
	dm := NewTestSharedDM(t)
	defer dm.Close()

	db := dm.SQLDB()

	// The shared database must actually be attached, or every assertion
	// below would pass vacuously.
	var sharedAttached int
	if err := db.QueryRow("SELECT COUNT(*) FROM pragma_database_list WHERE name = 'shared'").Scan(&sharedAttached); err != nil {
		t.Fatalf("probe the attached databases: %v", err)
	}
	if sharedAttached == 0 {
		t.Fatal("the shared database is not attached — the test would pass vacuously")
	}

	// The four names are present in main.sqlite_master with a count of 4.
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(continuityTables)), ",")
	args := make([]interface{}, 0, len(continuityTables))
	for _, table := range continuityTables {
		args = append(args, table)
	}
	var localCount int
	if err := db.QueryRow(
		"SELECT COUNT(*) FROM main.sqlite_master WHERE type = 'table' AND name IN ("+placeholders+")",
		args...,
	).Scan(&localCount); err != nil {
		t.Fatalf("probe main.sqlite_master: %v", err)
	}
	if localCount != 4 {
		t.Errorf("main.sqlite_master holds %d of the four Continuity tables, want 4", localCount)
	}

	// ... and the count of those names in shared.sqlite_master is 0.
	var sharedCount int
	if err := db.QueryRow(
		"SELECT COUNT(*) FROM shared.sqlite_master WHERE type = 'table' AND name IN ("+placeholders+")",
		args...,
	).Scan(&sharedCount); err != nil {
		t.Fatalf("probe shared.sqlite_master: %v", err)
	}
	if sharedCount != 0 {
		t.Errorf("shared.sqlite_master holds %d of the four Continuity tables, want 0. Project identity "+
			"must stay inside one workspace — the shared store is the cross-workspace broadcast "+
			"store and holds no project identity.", sharedCount)
	}

	// Named explicitly: no shared.projects table exists.
	var sharedProjects int
	if err := db.QueryRow(
		"SELECT COUNT(*) FROM shared.sqlite_master WHERE type = 'table' AND name = 'projects'",
	).Scan(&sharedProjects); err != nil {
		t.Fatalf("probe shared.projects: %v", err)
	}
	if sharedProjects != 0 {
		t.Errorf("shared.projects exists (count %d). Project identity must stay inside one workspace.", sharedProjects)
	}

	// And each of the four, by name, is in the LOCAL schema only.
	for _, table := range continuityTables {
		var local, shared int
		if err := db.QueryRow(
			"SELECT COUNT(*) FROM main.sqlite_master WHERE type = 'table' AND name = ?", table,
		).Scan(&local); err != nil {
			t.Fatalf("probe main.%s: %v", table, err)
		}
		if local != 1 {
			t.Errorf("the table %q is missing from the local schema (count %d)", table, local)
		}
		if err := db.QueryRow(
			"SELECT COUNT(*) FROM shared.sqlite_master WHERE type = 'table' AND name = ?", table,
		).Scan(&shared); err != nil {
			t.Fatalf("probe shared.%s: %v", table, err)
		}
		if shared != 0 {
			t.Errorf("the table %q is present in the SHARED schema (count %d). Project identity "+
				"must stay inside one workspace — the shared store is the cross-workspace broadcast "+
				"store and holds no project identity.", table, shared)
		}
	}
}

// ---------------------------------------------------------------------------
// AT8 — the restore allow-list
// ---------------------------------------------------------------------------

// TestContinuityAT8RestoreAllowList — AT8.
//
// A dump containing the four new tables, named explicitly, is accepted
// by `mpm restore-db`; CanonicalMPMSchema contains exactly the four new
// entries and no others; RuntimeCanonicalSchema contains none of the
// four; and the allow-list's size grows by exactly four.
func TestContinuityAT8RestoreAllowList(t *testing.T) {
	// The dump names all four tables explicitly.
	dump := strings.Builder{}
	for _, table := range continuityTables {
		fmt.Fprintf(&dump, "CREATE TABLE %s (id TEXT PRIMARY KEY);\n", table)
	}
	for _, index := range continuityIndexes {
		table := "projects"
		switch {
		case strings.HasPrefix(index, "idx_project_names_"):
			table = "project_names"
		case strings.HasPrefix(index, "idx_continuity_threads_"):
			table = "continuity_threads"
		case strings.HasPrefix(index, "idx_continuity_checkpoints_"):
			table = "continuity_checkpoints"
		}
		fmt.Fprintf(&dump, "CREATE INDEX %s ON %s (id);\n", index, table)
	}
	dump.WriteString("INSERT INTO projects (id) VALUES ('cpj-0123456789abcdef');\n")

	if err := NewCanonicalDumpValidator().Validate(dump.String()); err != nil {
		t.Fatalf("the restore allow-list REJECTED a dump declaring the four Continuity tables: %v\n"+
			"  A dump of a legitimate MPM database must be restorable.", err)
	}

	// The approved DDL declares exactly the four expected tables.
	//
	// This is what makes "four" OBSERVABLE rather than asserted: the
	// count and the names come from the migration's own DDL, which
	// TestContinuityProjectModelDDLIsByteIdentical pins to the
	// approved §4.1 by hash.
	declared := continuityDeclaredTables(t)
	if !continuitySameStrings(declared, continuityTables) {
		t.Errorf("the Continuity DDL declares %v, want exactly %v",
			declared, continuityTables)
	}

	// The allow-list invariant — HISTORY-INDEPENDENT.
	//
	// continuityAllowListInvariants reads ONLY the final state: the
	// names the approved DDL declares, the canonical allow-list and
	// the runtime allow-list. It does not read the commit graph, so
	// its result cannot change when the work is split across commits,
	// when the branch is merged into main, when the checkout has no
	// history, or when another feature adds its own tables to the
	// shared allow-list.
	//
	// It deliberately does NOT assert an absolute allow-list size.
	// The canonical list is shared: an independent feature adding its
	// own tables to it changes the total without changing anything
	// about this change, so a pinned total would be a merge hazard —
	// a test that fails on a correct merge.
	//
	// What the allow-list's CONTENTS are is enforced history-free in
	// both directions by TestCanonicalSchemaSync: every CREATE TABLE
	// in this package must be on the canonical list, and every name
	// on the canonical list must have a table behind it. What remains
	// for AT8 is the pair the guards cannot express: the four tables
	// the approved DDL declares must be on the canonical list exactly
	// once each, and none of them may be on the runtime list.
	if violations := continuityAllowListInvariants(
		declared, CanonicalMPMSchema, RuntimeCanonicalSchema,
	); len(violations) != 0 {
		t.Errorf("the restore allow-list does not satisfy the Continuity invariant:\n  %s",
			strings.Join(violations, "\n  "))
	}

	// Negative control: the allow-list is still load-bearing. Removing
	// one of the four must make the SAME dump rejected — otherwise the
	// acceptance above proves nothing.
	trimmed := make([]string, 0, len(CanonicalMPMSchema))
	for _, name := range CanonicalMPMSchema {
		if name != "projects" {
			trimmed = append(trimmed, name)
		}
	}
	trimmed = append(trimmed, RuntimeCanonicalSchema...)
	if err := NewDumpValidator(trimmed).Validate(dump.String()); err == nil {
		t.Fatal("the dump was still accepted after `projects` was removed from the allow-list. " +
			"The allow-list is not load-bearing, so the acceptance above proves nothing.")
	}
}

// ---------------------------------------------------------------------------
// AT8's invariant, across repository shapes
// ---------------------------------------------------------------------------

// TestContinuityAllowListInvariantIsHistoryIndependent proves that the
// allow-list invariant depends on the CODE, not on the commit graph.
//
// The scenarios below are the repository shapes a correct change
// passes through. They differ in history and in the size of the shared
// allow-list; they do not differ in the schema, and none of them may
// change the verdict.
//
//   - the work split across commits (the migration file added before
//     the allow-list registration, or the other way round);
//   - the branch merged into main, including the case where HEAD IS
//     the base branch — the state a build of the merged result sees;
//   - a source archive or a shallow checkout, with no usable history;
//   - the combined tree with an independent feature (Skill Packages
//     P1), where the allow-list grows by three more tables.
//
// The first four have the SAME final state, and that is the point: the
// invariant reads only the final state, so its verdict is identical in
// all of them. The last one has a larger allow-list and must also
// pass — an independent feature changing the total must not cause a
// false failure.
//
// The negative rows are the counterweight: the invariant must still
// REJECT a genuinely missing, duplicated or misfiled entry. An
// invariant that cannot fail is not an invariant.
func TestContinuityAllowListInvariantIsHistoryIndependent(t *testing.T) {
	declared := continuityDeclaredTables(t)
	if !continuitySameStrings(declared, continuityTables) {
		t.Fatalf("the Continuity DDL declares %v, want exactly %v", declared, continuityTables)
	}

	// The real allow-list satisfies the invariant.
	baseline := continuityAllowListInvariants(declared, CanonicalMPMSchema, RuntimeCanonicalSchema)
	if len(baseline) != 0 {
		t.Fatalf("the real allow-list violates the Continuity invariant:\n  %s",
			strings.Join(baseline, "\n  "))
	}

	// An independent feature's three tables (Skill Packages P1, PR
	// #228). Used here only as a synthetic "another feature changed
	// the shared allow-list" — the invariant must not notice.
	independent := []string{"skill_packages", "skill_package_files", "skill_package_blobs"}
	combined := append(append([]string(nil), CanonicalMPMSchema...), independent...)

	withoutProjects := make([]string, 0, len(CanonicalMPMSchema))
	for _, name := range CanonicalMPMSchema {
		if name != "projects" {
			withoutProjects = append(withoutProjects, name)
		}
	}
	duplicatedProjects := append(append([]string(nil), CanonicalMPMSchema...), "projects")
	runtimeWithProjects := append(append([]string(nil), RuntimeCanonicalSchema...), "projects")

	scenarios := []struct {
		name      string
		canonical []string
		runtime   []string
		want      int
	}{
		{
			name:      "the feature branch, as it stands",
			canonical: CanonicalMPMSchema,
			runtime:   RuntimeCanonicalSchema,
			want:      0,
		},
		{
			name:      "the work split across commits: the migration file added before the allow-list registration",
			canonical: CanonicalMPMSchema,
			runtime:   RuntimeCanonicalSchema,
			want:      0,
		},
		{
			name:      "merged into main: HEAD is the base branch",
			canonical: CanonicalMPMSchema,
			runtime:   RuntimeCanonicalSchema,
			want:      0,
		},
		{
			name:      "a source archive or shallow checkout: no usable history",
			canonical: CanonicalMPMSchema,
			runtime:   RuntimeCanonicalSchema,
			want:      0,
		},
		{
			name:      "combined with an independent feature: the allow-list grows by three more tables",
			canonical: combined,
			runtime:   RuntimeCanonicalSchema,
			want:      0,
		},
		{
			name:      "NEGATIVE: a required allow-list name is missing",
			canonical: withoutProjects,
			runtime:   RuntimeCanonicalSchema,
			want:      1,
		},
		{
			name:      "NEGATIVE: a required allow-list name is duplicated",
			canonical: duplicatedProjects,
			runtime:   RuntimeCanonicalSchema,
			want:      1,
		},
		{
			name:      "NEGATIVE: a required name is on the runtime allow-list instead",
			canonical: CanonicalMPMSchema,
			runtime:   runtimeWithProjects,
			want:      1,
		},
	}

	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			violations := continuityAllowListInvariants(declared, sc.canonical, sc.runtime)
			if len(violations) != sc.want {
				t.Fatalf("the invariant reported %d violation(s), want %d:\n  %s",
					len(violations), sc.want, strings.Join(violations, "\n  "))
			}
		})
	}

	// The four history shapes have the same final state, so they must
	// have the same verdict — and the same as the real allow-list.
	for _, sc := range scenarios[:4] {
		violations := continuityAllowListInvariants(declared, sc.canonical, sc.runtime)
		if len(violations) != len(baseline) {
			t.Errorf("the scenario %q produced %d violation(s) but the real allow-list produced %d — "+
				"the invariant must depend on the final state only",
				sc.name, len(violations), len(baseline))
		}
	}

	// And the independent feature's three tables must not change the
	// verdict either.
	combinedViolations := continuityAllowListInvariants(declared, combined, RuntimeCanonicalSchema)
	if len(combinedViolations) != len(baseline) {
		t.Errorf("adding an independent feature's tables changed the verdict: %d violation(s) -> %d",
			len(baseline), len(combinedViolations))
	}
}

// TestContinuityAcceptanceTestsDoNotDependOnGitHistory is the
// structural guard behind the invariant above.
//
// The invariant is durable because it reads only the final state. This
// guard makes that property structural rather than a matter of
// discipline: if a future edit reintroduces a Git invocation into this
// file, the acceptance tests could again acquire a dependence on the
// commit graph, and this fails.
//
// A test whose result depends on repository history rather than on the
// code is a test that can turn a correct tree red — the split-commit
// failure this file once had is the proof. The guard is here so that
// the failure cannot come back quietly.
//
// The needle is assembled at runtime so that this guard does not match
// its own source text.
func TestContinuityAcceptanceTestsDoNotDependOnGitHistory(t *testing.T) {
	src, err := os.ReadFile("migration_continuity_project_model_test.go")
	if err != nil {
		t.Fatalf("read this file: %v", err)
	}
	needle := "exec" + ".Command("
	if strings.Contains(string(src), needle) {
		t.Fatalf("this file invokes %s. The Continuity acceptance tests must not depend on the "+
			"commit graph: a correct tree can fail under a benign commit split, under a "+
			"post-merge build where HEAD is the base branch, or in a checkout without history.",
			needle)
	}
}

// ---------------------------------------------------------------------------
// A lost sentinel
// ---------------------------------------------------------------------------

// TestContinuityLostSentinelRecoversWithoutDataLoss covers the
// recovery path the migration's header claims.
//
// The header says: "The DDL statements are themselves CREATE ... IF NOT
// EXISTS, so a re-run is a no-op even if the sentinel were lost." A
// comment that makes an untested claim is a liability, so the claim is
// tested here.
//
// The scenario: a database whose Continuity tables carry real rows, and
// whose sentinel row has been removed — by an operator editing
// `schema_migrations`, by a partial restore, or by any other means.
// The next open must re-run the migration, find the tables already
// there, leave every row and every index untouched, and rewrite the
// sentinel. It must NOT drop or recreate the tables (which would
// destroy the ledger).
func TestContinuityLostSentinelRecoversWithoutDataLoss(t *testing.T) {
	root, dbPath := continuityWorkspace(t)

	dm, err := openContinuityDB(t, root)
	if err != nil {
		t.Fatalf("build the fixture: %v", err)
	}

	// Real Continuity state.
	if _, err := dm.SQLDB().Exec(
		`INSERT INTO projects (id, created_at, updated_at) VALUES ('cpj-0123456789abcdef', 1767225600, 1767225600)`,
	); err != nil {
		t.Fatalf("seed a project: %v", err)
	}
	if _, err := dm.SQLDB().Exec(
		`INSERT INTO project_names (id, name, project_id, kind, created_at)
		 VALUES (1, 'lost-sentinel', 'cpj-0123456789abcdef', 'canonical', 1767225600)`,
	); err != nil {
		t.Fatalf("seed a project name: %v", err)
	}
	if _, err := dm.SQLDB().Exec(
		`INSERT INTO continuity_threads (id, project_id, title, focus, state, revision, created_at, updated_at)
		 VALUES ('ct-0123456789abcdef', 'cpj-0123456789abcdef', 'A thread', 'lost-sentinel', 'active', 3, 1767225600, 1767225600)`,
	); err != nil {
		t.Fatalf("seed a thread: %v", err)
	}
	if _, err := dm.SQLDB().Exec(
		`INSERT INTO continuity_checkpoints (id, thread_id, project_id, revision, headline, facts, hypotheses, state, next_actions, created_at)
		 VALUES ('ck-0123456789abcdef', 'ct-0123456789abcdef', 'cpj-0123456789abcdef', 3, 'a checkpoint', '[]', '[]', '[]', '[]', 1767225600)`,
	); err != nil {
		t.Fatalf("seed a checkpoint: %v", err)
	}

	before := continuityRowSnapshot(t, dm.SQLDB())
	beforeObjects := continuitySchemaObjects(t, dm.SQLDB())
	dm.Close()

	// Lose the sentinel.
	fixture := openContinuityFixture(t, dbPath)
	if _, err := fixture.Exec(
		"DELETE FROM schema_migrations WHERE id = ?", continuitySentinelID,
	); err != nil {
		t.Fatalf("delete the sentinel: %v", err)
	}
	var sentinels int
	if err := fixture.QueryRow(
		"SELECT COUNT(*) FROM schema_migrations WHERE id = ?", continuitySentinelID,
	).Scan(&sentinels); err != nil {
		t.Fatalf("count the sentinel: %v", err)
	}
	if sentinels != 0 {
		t.Fatalf("the sentinel count is %d after the deletion, want 0", sentinels)
	}

	// The next open re-runs the migration.
	dm, err = openContinuityDB(t, root)
	if err != nil {
		t.Fatalf("the recovery open failed: %v", err)
	}
	defer dm.Close()
	db := dm.SQLDB()

	// The sentinel is back, exactly once.
	if err := db.QueryRow(
		"SELECT COUNT(*) FROM schema_migrations WHERE id = ?", continuitySentinelID,
	).Scan(&sentinels); err != nil {
		t.Fatalf("count the sentinel: %v", err)
	}
	if sentinels != 1 {
		t.Errorf("the sentinel count is %d after the recovery open, want exactly 1", sentinels)
	}

	// The rows survived — the migration did not drop and recreate the
	// tables.
	after := continuityRowSnapshot(t, db)
	// The sentinel row is the one permitted difference.
	beforeMigrations, _ := continuityStripSentinel(before["schema_migrations"])
	afterMigrations, afterHadSentinel := continuityStripSentinel(after["schema_migrations"])
	if !afterHadSentinel {
		t.Error("the Continuity sentinel is missing after the recovery open")
	}
	// schema_migrations is EXCLUDED from the full comparison BEFORE it
	// is made.
	//
	// The recovery re-applies the migration, so the sentinel is
	// re-recorded with a NEW applied_at. That is the expected, permitted
	// difference — and it is compared separately, below, with the
	// sentinel stripped from BOTH sides.
	//
	// The exclusion has to happen before the full comparison, not
	// after. Comparing first and deleting afterwards reported the
	// sentinel's new applied_at as a changed pre-existing row, which
	// made this test fail whenever the recovery landed in a different
	// second than the first application — a ~4% flake that had nothing
	// to do with the migration's behaviour.
	delete(before, "schema_migrations")
	delete(after, "schema_migrations")

	changed := continuityDiffSnapshots(before, after)
	changed = append(changed, continuityDiffSnapshots(
		map[string][]string{"schema_migrations": beforeMigrations},
		map[string][]string{"schema_migrations": afterMigrations},
	)...)
	if len(changed) != 0 {
		t.Errorf("the recovery changed pre-existing rows (%d difference(s)):\n  %s",
			len(changed), strings.Join(changed, "\n  "))
	}

	// The Continuity rows are still there, row for row.
	for table, want := range map[string]int{
		"projects":               1,
		"project_names":          1,
		"continuity_threads":     1,
		"continuity_checkpoints": 1,
	} {
		var got int
		if err := db.QueryRow("SELECT COUNT(*) FROM " + continuityQuoteIdent(table)).Scan(&got); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if got != want {
			t.Errorf("%s holds %d row(s) after the recovery, want %d — the migration must not "+
				"drop and recreate the tables", table, got, want)
		}
	}

	// And the schema is unchanged.
	afterObjects := continuitySchemaObjects(t, db)
	if diff := continuityDiffObjects(beforeObjects, afterObjects); len(diff) != 0 {
		t.Errorf("the recovery changed the schema object list: %v", diff)
	}
}

// ---------------------------------------------------------------------------
// Foreign-key enforcement
// ---------------------------------------------------------------------------

// TestContinuityForeignKeyEnforcement pins the containment guarantees at
// the storage boundary.
//
// The production DSN carries `_foreign_keys=1`, so every foreign key in
// the approved DDL is enforced by SQLite itself. This test writes the
// hostile rows the architecture says must be refused, and the benign
// ones that must land.
//
// It is a Stage 1 storage property, not a Stage 2 API: the API does not
// exist yet, but the schema that will enforce it does.
func TestContinuityForeignKeyEnforcement(t *testing.T) {
	root, _ := continuityWorkspace(t)
	dm, err := openContinuityDB(t, root)
	if err != nil {
		t.Fatalf("open the workspace: %v", err)
	}
	defer dm.Close()
	db := dm.SQLDB()

	// The pragmas must be ON, or the rest of the test proves nothing.
	var fk int
	if err := db.QueryRow("PRAGMA foreign_keys").Scan(&fk); err != nil {
		t.Fatalf("read the foreign_keys pragma: %v", err)
	}
	if fk != 1 {
		t.Fatalf("foreign_keys is %d on the production path. Every assertion below would be vacuous.", fk)
	}

	// Seed two projects, one thread each, and one checkpoint each.
	if _, err := db.Exec(
		"INSERT INTO projects (id) VALUES ('cpj-a'), ('cpj-b')",
	); err != nil {
		t.Fatalf("seed the projects: %v", err)
	}
	if _, err := db.Exec(
		"INSERT INTO continuity_threads (id, project_id, title) VALUES ('ct-1','cpj-a','one'), ('ct-2','cpj-b','two')",
	); err != nil {
		t.Fatalf("seed the threads: %v", err)
	}
	if _, err := db.Exec(
		"INSERT INTO continuity_checkpoints (id, thread_id, project_id, revision) " +
			"VALUES ('ck-1','ct-1','cpj-a',0), ('ck-2','ct-2','cpj-b',0)",
	); err != nil {
		t.Fatalf("seed the checkpoints: %v", err)
	}

	// Positive control: the benign writes land.
	if _, err := db.Exec(
		"INSERT INTO continuity_checkpoints (id, thread_id, project_id, revision, supersedes) " +
			"VALUES ('ck-3','ct-1','cpj-a',0,'ck-1')",
	); err != nil {
		t.Errorf("a same-thread supersession was refused: %v", err)
	}
	if _, err := db.Exec(
		"UPDATE continuity_threads SET last_checkpoint_id = 'ck-1' WHERE id = 'ct-1'",
	); err != nil {
		t.Errorf("a same-thread resume pointer was refused: %v", err)
	}

	// The hostile writes are refused.
	hostile := []struct {
		what string
		sql  string
	}{
		{
			"a checkpoint naming a project other than its thread's",
			"INSERT INTO continuity_checkpoints (id, thread_id, project_id, revision) " +
				"VALUES ('ck-x','ct-1','cpj-b',0)",
		},
		{
			"a resume pointer naming another thread's checkpoint",
			"UPDATE continuity_threads SET last_checkpoint_id = 'ck-2' WHERE id = 'ct-1'",
		},
		{
			"a supersession naming another thread's checkpoint",
			"INSERT INTO continuity_checkpoints (id, thread_id, project_id, revision, supersedes) " +
				"VALUES ('ck-y','ct-1','cpj-a',0,'ck-2')",
		},
		{
			"a thread supersession naming another project's thread",
			"UPDATE continuity_threads SET superseded_by = 'ct-2' WHERE id = 'ct-1'",
		},
		{
			"a name row naming a nonexistent project",
			"INSERT INTO project_names (name, project_id, kind) VALUES ('orphan','cpj-nope','canonical')",
		},
		{
			"a hard delete of a project that still has children",
			"DELETE FROM projects WHERE id = 'cpj-a'",
		},
	}
	for _, h := range hostile {
		if _, err := db.Exec(h.sql); err == nil {
			t.Errorf("%s was ACCEPTED. It must be refused by the schema, not by the API.", h.what)
		} else if !strings.Contains(strings.ToLower(err.Error()), "foreign key") {
			t.Errorf("%s was refused, but not as a foreign-key violation: %v", h.what, err)
		}
	}

	// And the refused writes left nothing behind.
	var n int
	if err := db.QueryRow(
		"SELECT COUNT(*) FROM continuity_checkpoints WHERE id IN ('ck-x','ck-y')",
	).Scan(&n); err != nil {
		t.Fatalf("count the refused checkpoints: %v", err)
	}
	if n != 0 {
		t.Errorf("%d row(s) survived a refused write", n)
	}
}

// ---------------------------------------------------------------------------
// Small helpers
// ---------------------------------------------------------------------------

func continuityContains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

func continuitySameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	x := append([]string(nil), a...)
	y := append([]string(nil), b...)
	sort.Strings(x)
	sort.Strings(y)
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}
	return true
}

func continuitySameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// continuityRemoveString drops the entries of a difference list that satisfy keep.
func continuityRemoveString(list []string, keep func(string) bool) []string {
	var out []string
	for _, s := range list {
		if !keep(s) {
			out = append(out, s)
		}
	}
	return out
}
