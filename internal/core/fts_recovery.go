// fts_recovery.go — Pre-init FTS5 integrity repair.
//
// This is the Stage 0 (Pi alpha) follow-on: existing MPM databases
// can enter a partially-initialized FTS5 state in which base tables
// remain intact but the FTS virtual tables are missing while their
// shadow tables remain. This state arises when a `mpm backup`-then-
// `mpm ops restore-db` round-trip in older MPM alpha releases replayed
// a `sqlite3 .dump` output that contained `CREATE TABLE 'sessions_fts_*'(...)`
// (the FTS5 shadow tables) but NOT the `INSERT INTO sqlite_schema(...)
// VALUES(..., 'CREATE VIRTUAL TABLE sessions_fts ...')` line that
// registers the virtual table in sqlite_master. The dump is exactly
// that — see sqlite3 .dump output of any FTS5-enabled DB for the
// canonical shape:
//
//	PRAGMA writable_schema=ON;
//	INSERT INTO sqlite_schema(...) VALUES(..., 'CREATE VIRTUAL TABLE ...');
//	CREATE TABLE 'sessions_fts_data'(...);
//	INSERT INTO sessions_fts_data VALUES(...);
//	...
//	PRAGMA writable_schema=OFF;
//
// The validator's reject-CREATE-VIRTUAL-TABLE + driver-compat strip of
// INSERT-INTO-sqlite_schema combination turns this into an orphan:
//
//   - shadow tables exist (replayed as CREATE TABLE)
//   - virtual table entry does NOT exist in sqlite_master
//   - triggers `<base>_ai/_ad/_au` reference the non-existent virtual
//     table → every INSERT/UPDATE/DELETE on the base table fails with
//     "no such table: main.<base>_fts"
//
// Before the fix, initFTSTables saw `CREATE VIRTUAL TABLE IF NOT EXISTS
// <base>_fts` fail with "error creating shadow table <base>_fts_data:
// table '<base>_fts_data' already exists" and returned the error. The
// caller (line 2159) logged a WARN and moved on. But the triggers
// remained active in sqlite_master, so every subsequent memory/save
// and wake/schedule crashed the transaction with the FTS error.
//
// The repair:
//
//  1. Discover every FTS domain (memories, sessions, topics, lessons,
//     references, reference_chunks, scheduled_wakes, future additions).
//  2. For each, check whether the virtual table exists in sqlite_master.
//  3. If the virtual table is missing AND the shadow tables exist:
//     a. DROP every shadow table (the search projection is disposable;
//     we will rebuild from canonical data on recreate).
//     b. CREATE VIRTUAL TABLE with the canonical schema.
//     c. Re-run the canonical FTS sync trigger DDL.
//     d. Rebuild from canonical data.
//  4. If the virtual table is missing AND no shadow tables exist
//     (fresh corruption): just CREATE VIRTUAL TABLE + triggers.
//  5. If the virtual table exists but is broken (e.g. shape mismatch):
//     drop + recreate.
//  6. Every operation is idempotent. The helper can be called any
//     number of times — it converges to a state where every advertised
//     FTS index is online.
//
// Safety:
//
//   - We never DROP canonical base tables. The repair operates ONLY on
//     FTS projection objects (virtual tables + shadow tables + triggers)
//     which are disposable: the canonical content lives in the base
//     table and is reindexed on rebuild.
//   - The shadow-table DROP is bounded to the known FTS5 shadow-table
//     suffix set (`_fts_data`, `_fts_content`, `_fts_config`,
//     `_fts_docsize`, `_fts_idx`). No other table name in MPM ends
//     with those suffixes.
//   - The rebuild runs only on tables we KNOW how to backfill from
//     canonical data; new FTS domains must add themselves here.
//
// This helper is called by initFTSTables BEFORE the existing
// CREATE-VIRTUAL-TABLE-IF-NOT-EXISTS loop, so the loop sees a clean
// slate. After the repair runs, initFTSTables' existing IF-NOT-EXISTS
// statements are no-ops on the now-clean state.
package internal

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"
)

// ftsShadowSuffixes is the canonical list of FTS5 shadow-table
// suffixes that SQLite creates internally when a virtual table is
// built with `USING fts5(...)`. See
// https://www.sqlite.org/fts5.html section 4.5 "Shadow Tables".
//
// We DROP only tables whose name ends with one of these suffixes AND
// whose base name (without the suffix) matches a known FTS module.
// No canonical MPM table ends in any of these strings.
var ftsShadowSuffixes = []string{
	// Trailing halves of shadow-table names. The vtab name itself
	// ends in `_fts` (e.g. `sessions_fts`); the shadow-table names
	// SQLite creates are `<vtab>_<suffix>` (e.g. `sessions_fts_data`).
	// So the suffix here is the trailing portion AFTER `_fts_`. The
	// earlier draft listed `_fts_data` etc. — that produced
	// `<vtab>_fts_fts_data` and matched nothing.
	"_data",
	"_content",
	"_config",
	"_docsize",
	"_idx",
}

// ftsDomain describes one FTS5 module: its base table, the virtual
// table name, the column list (so the recreated virtual table
// matches the canonical schema), and the trigger DDL strings.
//
// Column lists MUST match the canonical schema in initFTSTables
// (db.go lines ~2790 onward). Drift between this list and initFTSTables
// will cause the recreated virtual table to have a different shape
// from what the triggers expect — surfacing as "no such column"
// errors at write time. Keep the two in lockstep.
//
// Triggers are the same strings initFTSTables builds (copied here so
// the helper is self-contained — extracting the trigger DDL into a
// shared constant is a separate cleanup).
type ftsDomain struct {
	name     string // short name for logs ("memories")
	vtab     string // virtual table name ("memories_fts")
	base     string // canonical base table ("memories")
	cols     string // column list for CREATE VIRTUAL TABLE
	triggers []string
	rebuild  string // INSERT INTO vtab SELECT FROM base WHERE ...
}

// canonicalFtsDomains is the list of FTS5 modules MPM manages. Every
// domain MUST have a corresponding entry here; the helper is intentionally
// closed-world (no auto-detection) so adding a new FTS module is a
// deliberate code change rather than a runtime discovery. Drift is
// caught at the linter level when someone adds a CREATE VIRTUAL TABLE
// in initFTSTables without also listing it here.
var canonicalFtsDomains = []ftsDomain{
	{
		name: "sessions",
		vtab: "sessions_fts",
		base: "sessions",
		cols: "content, session_id, content_hash UNINDEXED",
		triggers: []string{
			`CREATE TRIGGER IF NOT EXISTS sessions_ai AFTER INSERT ON sessions BEGIN INSERT INTO sessions_fts(rowid, content, session_id, content_hash) VALUES (new.rowid, new.content, new.session_id, new.content_hash); END;`,
			`CREATE TRIGGER IF NOT EXISTS sessions_ad AFTER DELETE ON sessions BEGIN DELETE FROM sessions_fts WHERE rowid = old.rowid; END;`,
			`CREATE TRIGGER IF NOT EXISTS sessions_au AFTER UPDATE ON sessions BEGIN DELETE FROM sessions_fts WHERE rowid = old.rowid; INSERT INTO sessions_fts(rowid, content, session_id, content_hash) VALUES (new.rowid, new.content, new.session_id, new.content_hash); END;`,
		},
		rebuild: `INSERT INTO sessions_fts(rowid, content, session_id, content_hash) SELECT rowid, content, COALESCE(session_id,''), COALESCE(content_hash,'') FROM sessions`,
	},
	{
		name: "memories",
		vtab: "memories_fts",
		base: "memories",
		cols: "content, collection, session_id UNINDEXED, tags",
		triggers: []string{
			`CREATE TRIGGER IF NOT EXISTS memories_ai AFTER INSERT ON memories BEGIN INSERT INTO memories_fts(rowid, content, collection, session_id, tags) VALUES (new.rowid, new.content, new.collection, new.session_id, new.tags); END;`,
			`CREATE TRIGGER IF NOT EXISTS memories_ad AFTER DELETE ON memories BEGIN DELETE FROM memories_fts WHERE rowid = old.rowid; END;`,
			`CREATE TRIGGER IF NOT EXISTS memories_au AFTER UPDATE ON memories WHEN old.deleted_at IS NULL AND new.deleted_at IS NOT NULL BEGIN DELETE FROM memories_fts WHERE rowid = old.rowid; END;`,
			`CREATE TRIGGER IF NOT EXISTS memories_au_content AFTER UPDATE ON memories WHEN NOT (old.deleted_at IS NULL AND new.deleted_at IS NOT NULL) BEGIN DELETE FROM memories_fts WHERE rowid = old.rowid; INSERT INTO memories_fts(rowid, content, collection, session_id, tags) VALUES (new.rowid, new.content, new.collection, new.session_id, new.tags); END;`,
		},
		rebuild: `INSERT INTO memories_fts(rowid, content, collection, session_id, tags) SELECT rowid, content, collection, COALESCE(session_id,''), COALESCE(tags,'[]') FROM memories WHERE deleted_at IS NULL`,
	},
	{
		name: "topics",
		vtab: "topics_fts",
		base: "topics",
		cols: "name, description",
		triggers: []string{
			`CREATE TRIGGER IF NOT EXISTS topics_ai AFTER INSERT ON topics BEGIN INSERT INTO topics_fts(rowid, name, description) VALUES (new.rowid, new.name, new.description); END;`,
			`CREATE TRIGGER IF NOT EXISTS topics_ad AFTER DELETE ON topics BEGIN DELETE FROM topics_fts WHERE rowid = old.rowid; END;`,
			`CREATE TRIGGER IF NOT EXISTS topics_au AFTER UPDATE ON topics BEGIN DELETE FROM topics_fts WHERE rowid = old.rowid; INSERT INTO topics_fts(rowid, name, description) VALUES (new.rowid, new.name, new.description); END;`,
		},
		rebuild: `INSERT INTO topics_fts(rowid, name, description) SELECT rowid, name, COALESCE(description,'') FROM topics`,
	},
	{
		name: "lessons",
		vtab: "lessons_fts",
		base: "lessons",
		cols: "content, tags",
		// lessons uses the view+INSTEAD OF path, not raw AFTER triggers.
		// The pre-fix-on-restore orphans this domain differently than
		// memories/sessions/etc., but the repair is the same: detect
		// missing virtual table, drop shadows, recreate, rebuild.
		// No trigger recreation needed here — the view's INSTEAD OF
		// triggers handle sync. The rebuild reads through the view so
		// it joins lessons_base correctly.
		triggers: nil,
		rebuild:  `INSERT INTO lessons_fts(rowid, content, tags) SELECT rowid, content, COALESCE(tags,'[]') FROM lessons_base WHERE deleted_at IS NULL`,
	},
	{
		name: "references",
		vtab: "references_fts",
		base: "reference_docs",
		cols: "title, content, tags",
		triggers: []string{
			`CREATE TRIGGER IF NOT EXISTS references_ai AFTER INSERT ON reference_docs BEGIN INSERT INTO references_fts(rowid, title, content, tags) VALUES (new.rowid, new.title, new.content, new.tags); END;`,
			`CREATE TRIGGER IF NOT EXISTS references_ad AFTER DELETE ON reference_docs BEGIN DELETE FROM references_fts WHERE rowid = old.rowid; END;`,
			`CREATE TRIGGER IF NOT EXISTS references_au AFTER UPDATE ON reference_docs BEGIN DELETE FROM references_fts WHERE rowid = old.rowid; INSERT INTO references_fts(rowid, title, content, tags) VALUES (new.rowid, new.title, new.content, new.tags); END;`,
		},
		rebuild: `INSERT INTO references_fts(rowid, title, content, tags) SELECT rowid, title, COALESCE(content,''), COALESCE(tags,'[]') FROM reference_docs`,
	},
	{
		name: "reference_chunks",
		vtab: "reference_chunks_fts",
		base: "reference_chunks",
		cols: "section, content",
		triggers: []string{
			`CREATE TRIGGER IF NOT EXISTS reference_chunks_ai AFTER INSERT ON reference_chunks BEGIN INSERT INTO reference_chunks_fts(rowid, section, content) VALUES (new.rowid, new.section, new.content); END;`,
			`CREATE TRIGGER IF NOT EXISTS reference_chunks_ad AFTER DELETE ON reference_chunks BEGIN DELETE FROM reference_chunks_fts WHERE rowid = old.rowid; END;`,
			`CREATE TRIGGER IF NOT EXISTS reference_chunks_au AFTER UPDATE ON reference_chunks BEGIN DELETE FROM reference_chunks_fts WHERE rowid = old.rowid; INSERT INTO reference_chunks_fts(rowid, section, content) VALUES (new.rowid, new.section, new.content); END;`,
		},
		rebuild: `INSERT INTO reference_chunks_fts(rowid, section, content) SELECT rowid, COALESCE(section,''), content FROM reference_chunks`,
	},
	{
		name: "scheduled_wakes",
		vtab: "scheduled_wakes_fts",
		base: "scheduled_wakes",
		cols: "reason, theory_id UNINDEXED",
		triggers: []string{
			`CREATE TRIGGER IF NOT EXISTS scheduled_wakes_ai AFTER INSERT ON scheduled_wakes BEGIN INSERT INTO scheduled_wakes_fts(rowid, reason, theory_id) VALUES (new.rowid, new.reason, COALESCE(new.theory_id,'')); END;`,
			`CREATE TRIGGER IF NOT EXISTS scheduled_wakes_ad AFTER DELETE ON scheduled_wakes BEGIN DELETE FROM scheduled_wakes_fts WHERE rowid = old.rowid; END;`,
			`CREATE TRIGGER IF NOT EXISTS scheduled_wakes_au AFTER UPDATE ON scheduled_wakes BEGIN DELETE FROM scheduled_wakes_fts WHERE rowid = old.rowid; INSERT INTO scheduled_wakes_fts(rowid, reason, theory_id) VALUES (new.rowid, new.reason, COALESCE(new.theory_id,'')); END;`,
		},
		rebuild: `INSERT INTO scheduled_wakes_fts(rowid, reason, theory_id) SELECT rowid, reason, COALESCE(theory_id,'') FROM scheduled_wakes`,
	},
}

// repairOrphanedFTS5 indexes walks every canonical FTS domain and
// repairs any orphaned shadow-table state. Safe to call multiple
// times — every operation is idempotent. Called from initFTSTables
// before the CREATE-VIRTUAL-TABLE-IF-NOT-EXISTS loop.
//
// Returns the number of domains that were actually repaired (drops +
// recreates). A return value of 0 means "everything was already clean
// or had no recoverable state". Errors are aggregated per-domain and
// returned as a single joined error so one failure doesn't block
// other domains from being repaired.
func (dm *DatabaseManager) repairOrphanedFTS5() (int, error) {
	repaired := 0
	var errs []error

	for _, d := range canonicalFtsDomains {
		state, err := dm.ftsDomainState(d)
		if err != nil {
			errs = append(errs, fmt.Errorf("fts %s: probe state: %w", d.name, err))
			continue
		}
		switch state {
		case ftsStateClean:
			// Nothing to do.
		case ftsStateOrphanShadows:
			slog.Warn("fts_recovery: repairing orphaned shadow tables", "domain", d.name)
			if err := dm.repairOrphanDomain(d); err != nil {
				errs = append(errs, fmt.Errorf("fts %s: repair orphan: %w", d.name, err))
				continue
			}
			repaired++
		case ftsStateMissing:
			// No virtual table and no shadow tables, but triggers may still
			// reference `<d.vtab>` and break writes. Walk every trigger
			// whose body references the missing vtab and DROP it —
			// `initFTSTables` below will recreate the virtual table and
			// the canonical sync triggers. Idempotent (DROP IF EXISTS).
			//
			// This is the load-bearing branch for the Pi alpha failure:
			// without it, dropping the orphan shadows succeeds but the
			// next INSERT into memories fails with "no such table:
			// main.memories_fts" because the `<d.vtab>`-referencing
			// trigger is still active in sqlite_master.
			if dropped, derr := dm.dropStaleFTSyncTriggers(d); derr != nil {
				errs = append(errs, fmt.Errorf("fts %s: drop stale triggers: %w", d.name, derr))
			} else if dropped > 0 {
				slog.Warn("fts_recovery: dropped stale FTS sync triggers", "domain", d.name, "triggers", dropped)
				repaired++
			}
		case ftsStatePresent:
			// Virtual table exists — defer to initFTSTables' shape-mismatch
			// detection (db.go:2891-2915). We don't second-guess a working
			// virtual table.
		default:
			errs = append(errs, fmt.Errorf("fts %s: unknown state %d", d.name, state))
		}
	}

	if len(errs) > 0 {
		return repaired, errors.Join(errs...)
	}
	return repaired, nil
}

// dropStaleFTSyncTriggers removes any trigger whose body references
// the given domain's virtual table. Used in the ftsStateMissing
// branch to prevent a partially-restored DB from surfacing write
// failures of the form "no such table: main.<vtab>".
//
// Returns the number of triggers actually dropped (0 is not an error).
// This scan is conservative: it only DROPs a trigger when the
// domain's virtual table is missing. On a fresh install where the
// trigger doesn't exist yet (also "missing" state), `DROP TRIGGER
// IF EXISTS` is a no-op.
//
// 2026-09-22 release-blocker D-2: this scan must NOT drop
// INSTEAD OF triggers attached to views. Those triggers (the
// `lessons_instead_of_*` triple on the `lessons` view in
// particular) are owned by `migrateLessonsToView` and re-created by
// `finishOrRepairLessonsView` or by the `ftsStatements` loop, NOT
// by this recovery path. Letting the substring matcher drop them
// creates a coupling where a successful FTS recovery leaves the
// lesson writer surface wedged. The fix is to skip INSTEAD OF
// triggers here so the canonical recovery path
// (`migrateLessonsToView`/`finishOrRepairLessonsView`) owns them.
func (dm *DatabaseManager) dropStaleFTSyncTriggers(d ftsDomain) (int, error) {
	rows, err := dm.db.Query(
		`SELECT name, sql FROM sqlite_master WHERE type='trigger' AND sql LIKE ?`,
		"%"+d.vtab+"%",
	)
	if err != nil {
		return 0, fmt.Errorf("scan triggers: %w", err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var n, body string
		if err := rows.Scan(&n, &body); err != nil {
			return 0, err
		}
		// Skip INSTEAD OF triggers — they belong to a view whose
		// writer surface is owned by migrateLessonsToView /
		// finishOrRepairLessonsView. They cannot reference a
		// missing virtual table in a way that fails a writer
		// (SQLite parses triggers lazily; an attempt to fire
		// would be what's wedged, but the canonical migration
		// owns re-creation). See D-2 fix rationale in
		// fts_recovery_test.go.
		if strings.Contains(body, "INSTEAD OF") {
			continue
		}
		names = append(names, n)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, n := range names {
		if _, err := dm.db.Exec("DROP TRIGGER IF EXISTS " + n); err != nil {
			return 0, fmt.Errorf("drop %s: %w", n, err)
		}
	}
	return len(names), nil
}

// ftsDomainState is the diagnostic state of one FTS5 domain.
type ftsDomainState int

const (
	ftsStateClean         ftsDomainState = iota // virtual table present and healthy
	ftsStateMissing                             // virtual table absent, no shadow tables
	ftsStateOrphanShadows                       // virtual table absent, shadow tables exist
	ftsStatePresent                             // virtual table present (handled by initFTSTables)
)

// ftsDomainState reports the current state of one FTS5 domain.
//
// State determination:
//   - Virtual table present in sqlite_master → ftsStatePresent (clean).
//   - Virtual table absent AND any shadow table present → ftsStateOrphanShadows (recoverable).
//   - Virtual table absent AND no shadow tables → ftsStateMissing (fresh-install state; initFTSTables' IF NOT EXISTS will handle).
func (dm *DatabaseManager) ftsDomainState(d ftsDomain) (ftsDomainState, error) {
	// 1. Does the virtual table exist in sqlite_master?
	//
	// sqlite_master.type='table' is what we want here. The CREATE
	// VIRTUAL TABLE statement registers the virtual table with
	// type='table' (FTS5 module owns the shadow-table kinds but the
	// virtual table itself is a single sqlite_master row of type='table').
	var vtabCount int
	if err := dm.db.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE name = ? AND type = 'table'`,
		d.vtab,
	).Scan(&vtabCount); err != nil {
		return 0, fmt.Errorf("probe vtab: %w", err)
	}
	if vtabCount > 0 {
		slog.Info("fts_recovery: domain vtab present", "domain", d.name, "vtab", d.vtab)
		return ftsStatePresent, nil
	}

	// 2. Does ANY shadow table exist?
	//
	// Shadow tables are also stored as `type='table'` rows in
	// sqlite_master (the FTS5 module registers them as ordinary tables
	// for catalog purposes; only the parent virtual table is queried
	// via the FTS5 module's xFilter/xBestIndex). The orphan state is
	// therefore visible: shadows present, virtual table absent.
	//
	// The probe is rowid-anchored on shadow-table presence by name; we
	// list each known suffix explicitly so the query is inspectable
	// (rather than a `LIKE '%_fts_%'` substring match that could fire
	// on unrelated tables).
	var names []string
	for _, sfx := range ftsShadowSuffixes {
		names = append(names, d.vtab+sfx)
	}
	shadowQuery := `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name IN (`
	for i := range names {
		if i > 0 {
			shadowQuery += ", "
		}
		shadowQuery += "?"
	}
	shadowQuery += ")"
	args := make([]interface{}, len(names))
	for i, n := range names {
		args[i] = n
	}
	var shadowCount int
	if err := dm.db.QueryRow(shadowQuery, args...).Scan(&shadowCount); err != nil {
		return 0, fmt.Errorf("probe shadows: %w", err)
	}
	slog.Info("fts_recovery: domain probe", "domain", d.name, "vtab", d.vtab, "vtab_count", vtabCount, "shadow_count", shadowCount, "query", shadowQuery, "names", strings.Join(names, ","))
	if shadowCount > 0 {
		return ftsStateOrphanShadows, nil
	}
	return ftsStateMissing, nil
}

// ftsShadowSuffixes helper — the actual suffix list (separated from
// the package-level constant for use in ftsDomainState's query).
var ftShadowSuffixes = ftsShadowSuffixes

// repairOrphanDomain repairs a single FTS domain that has shadow
// tables but no virtual table entry. The repair is:
//  1. DROP every shadow table (disposable projection).
//  2. CREATE VIRTUAL TABLE with the canonical schema.
//  3. Reattach sync triggers (the FTS5 module owns the projection;
//     canonical writes continue via the existing base-table triggers).
//  4. Rebuild from the canonical base table.
func (dm *DatabaseManager) repairOrphanDomain(d ftsDomain) error {
	// 1. Drop every shadow table. Idempotent — uses IF EXISTS so
	//    running the repair twice is safe.
	for _, sfx := range ftsShadowSuffixes {
		shadowName := d.vtab + sfx
		if _, err := dm.db.Exec(`DROP TABLE IF EXISTS ` + shadowName); err != nil {
			return fmt.Errorf("drop shadow %s: %w", shadowName, err)
		}
	}

	// 2. Create the virtual table with the canonical schema.
	createSQL := fmt.Sprintf(
		`CREATE VIRTUAL TABLE %s USING fts5(%s, tokenize='porter unicode61')`,
		d.vtab, d.cols,
	)
	if _, err := dm.db.Exec(createSQL); err != nil {
		return fmt.Errorf("create virtual table %s: %w", d.vtab, err)
	}

	// 3. Reattach sync triggers. These are the same triggers
	//    initFTSTables would build on a fresh install. The rebuild
	//    IF NOT EXISTS clauses make this idempotent against repeated
	//    calls (the helper can be run on every init).
	for _, trig := range d.triggers {
		if _, err := dm.db.Exec(trig); err != nil {
			return fmt.Errorf("create trigger for %s: %w", d.vtab, err)
		}
	}

	// 4. Rebuild from the canonical base table. For lessons, the
	//    rebuild reads through the `lessons` view so it JOINs correctly
	//    with the lessons_base backing table; for memories it filters
	//    out soft-deleted rows so the search index doesn't surface them.
	if d.rebuild != "" {
		if _, err := dm.db.Exec(d.rebuild); err != nil {
			return fmt.Errorf("rebuild %s: %w", d.vtab, err)
		}
	}

	return nil
}
