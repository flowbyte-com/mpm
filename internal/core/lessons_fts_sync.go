// lessons_fts_sync.go — F2 alpha-blocker fix: lessons_fts ↔ lessons_base
// rowid/content desync detection and one-shot repair.
//
// Audit finding F2: searching lessons for exact content terms ("nginx",
// "logstats") returned ONE UNRELATED lesson. Root cause: the production
// database's `lessons_fts` rows were desynced from `lessons_base` — e.g.
// fts rowid 145 held the nginx lesson's text while lessons_base rowid 145
// held a completely different lesson. SearchLessons JOINs on
// `l.rowid = fts.rowid`, so a MATCH on the indexed text paired with the
// WRONG base row and returned it as a confident, wrong result.
//
// How the desync arises: `lessons` was historically a real table whose FTS
// was maintained by AFTER-triggers + a one-shot backfill (backfillFTSTables,
// which only fills EMPTY fts tables). The later migration to
// lessons_base + view rebuilt rows under NEW rowids while the populated
// lessons_fts kept the OLD mapping. Direct writes into lessons_base that
// bypass the view's INSTEAD OF triggers desync it the same way.
//
// Repair strategy: verify every lessons_fts row against lessons_base at the
// same rowid (existence + exact content/tags match), rebuild the whole index
// inside one transaction when any inconsistency is found, then record a
// schema_migrations sentinel so the O(n) verification does not run on every
// CLI invocation. All steady-state write paths go through the view's INSTEAD
// OF triggers (atomic with the base write), so after the one-time repair the
// index cannot silently drift again.
package internal

import (
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
)

// lessonsFTSSyncSentinel gates the one-shot verification/repair.
const lessonsFTSSyncSentinel = "lessons_fts_resync_v1"

// EnsureLessonsFTSInSync verifies lessons_fts against lessons_base and
// rebuilds the index if they disagree. Safe to call on every init: once the
// sentinel row exists it is a no-op. Non-FTS5 builds (lessons_fts absent)
// are also a no-op so LIKE-fallback databases keep booting.
func (dm *DatabaseManager) EnsureLessonsFTSInSync() error {
	// Sentinel check first — cheap and covers the overwhelmingly common case.
	var sentinel int
	sentinelErr := dm.db.QueryRow(
		`SELECT COUNT(*) FROM schema_migrations WHERE id = ?`, lessonsFTSSyncSentinel,
	).Scan(&sentinel)
	if sentinelErr != nil && !errors.Is(sentinelErr, sql.ErrNoRows) {
		return fmt.Errorf("lessons fts sentinel probe: %w", sentinelErr)
	}
	if sentinelErr == nil && sentinel > 0 {
		return nil
	}

	// lessons_fts only exists on FTS5 builds; skip otherwise.
	var ftsExists int
	if err := dm.db.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='lessons_fts'`,
	).Scan(&ftsExists); err != nil || ftsExists == 0 {
		return dm.recordLessonsFTSSentinel() // nothing to keep in sync
	}

	desynced, err := dm.lessonsFTSDesynced()
	if err != nil {
		// If we cannot even verify (e.g. legacy DB without lessons_base),
		// record the sentinel and move on — search falls back to LIKE.
		slog.Warn("EnsureLessonsFTSInSync: verification failed; skipping repair", "error", err.Error())
		return dm.recordLessonsFTSSentinel()
	}
	if desynced {
		if err := dm.rebuildLessonsFTS(); err != nil {
			return fmt.Errorf("rebuild lessons_fts: %w", err)
		}
		slog.Info("EnsureLessonsFTSInSync: lessons_fts was out of sync with lessons_base; index rebuilt")
	}
	return dm.recordLessonsFTSSentinel()
}

// lessonsFTSDesynced reports whether lessons_fts disagrees with lessons_base:
// orphaned fts rows, missing fts rows, or same-rowid content/tags mismatch.
func (dm *DatabaseManager) lessonsFTSDesynced() (bool, error) {
	// Orphans: fts rows whose rowid has no live base lesson.
	var orphans int
	if err := dm.db.QueryRow(`
		SELECT COUNT(*) FROM lessons_fts f
		WHERE NOT EXISTS (SELECT 1 FROM lessons_base b WHERE b.rowid = f.rowid)
	`).Scan(&orphans); err != nil {
		return false, fmt.Errorf("orphan probe: %w", err)
	}
	if orphans > 0 {
		return true, nil
	}

	// Missing or stale: any base lesson without an exactly matching fts row.
	var mismatched int
	if err := dm.db.QueryRow(`
		SELECT COUNT(*) FROM lessons_base b
		WHERE NOT EXISTS (
			SELECT 1 FROM lessons_fts f
			WHERE f.rowid = b.rowid AND f.content = b.content AND f.tags = COALESCE(b.tags,'[]')
		)
	`).Scan(&mismatched); err != nil {
		return false, fmt.Errorf("mismatch probe: %w", err)
	}
	return mismatched > 0, nil
}

// rebuildLessonsFTS atomically clears and repopulates lessons_fts from the
// lessons view (which reads lessons_base). Single transaction: readers see
// either the old index or the new complete one.
func (dm *DatabaseManager) rebuildLessonsFTS() error {
	tx, err := dm.db.Begin()
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`DELETE FROM lessons_fts`); err != nil {
		return fmt.Errorf("clear: %w", err)
	}
	if _, err := tx.Exec(`
		INSERT INTO lessons_fts(rowid, content, tags)
		SELECT rowid, content, COALESCE(tags,'[]') FROM lessons
	`); err != nil {
		return fmt.Errorf("repopulate: %w", err)
	}
	return tx.Commit()
}

func (dm *DatabaseManager) recordLessonsFTSSentinel() error {
	if _, err := dm.db.Exec(
		`INSERT OR IGNORE INTO schema_migrations (id, applied_at) VALUES (?, CAST(strftime('%s','now') AS INTEGER))`,
		lessonsFTSSyncSentinel,
	); err != nil {
		return fmt.Errorf("record sentinel: %w", err)
	}
	return nil
}
