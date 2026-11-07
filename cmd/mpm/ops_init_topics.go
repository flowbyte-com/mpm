// ops_init_topics.go — `mpm ops init topics` command.
//
// Explicit, idempotent, error-reporting initialization of the
// epistemology topic membership table (the
// backfillEpistemologyTopics() that the doctor used to call on
// every run, lifted into an operator-visible maintenance command).
//
// The command links existing theories/decisions memories to their
// topics (one topic per collection). The topic_memberships table
// uses INSERT OR IGNORE on the (memory_id, topic_id) pair, so
// re-runs are safe no-ops. The doctor's previous implicit
// backfill is removed; the operator runs this command when they
// want the topic memberships seeded.
//
// Per-collection error counters trigger exit 1: any error in
// GetOrCreateTopic or in the INSERT OR IGNORE step is accumulated
// per-collection and reported in the structured summary. A clean
// run exits 0 with the same "📚" message the old backfill printed
// plus a per-collection breakdown.
//
// Architecture:
//
//	cli:       this file               (handleOpsInitTopics)
//	router:    cmd/mpm/router.go       (case "init": "topics")
//	contract:  per-collection counters, exit 1 on any error
package main

import (
	"fmt"
	"time"

	"github.com/flowbyte-com/mpm-core/usererror"
)

// initTopicsCollections is the canonical list of collections whose
// memories get linked to their eponymous topic. Mirrors the
// previous implicit backfill in backfillEpistemologyTopics
// (handlers_epistemology.go).
var initTopicsCollections = []string{"theories", "decisions"}

// handleOpsInitTopics links existing theories/decisions memories to
// their topics. Idempotent: re-runs report zero linked and exit 0
// with no new rows. Per-collection error counters trigger exit 1
// on any partial failure.
//
// The command acquires the writable singleton (NOT the read-only
// handle) because topic_memberships is a write operation. The
// doctor's read-only path must not silently link memberships —
// that would be a write, not an inspection.
func handleOpsInitTopics(args []string) int {
	dm := getDB()
	if dm == nil {
		usererror.Error("init topics: database unavailable")
		return 1
	}

	now := time.Now().UTC().Format(time.RFC3339)

	// Per-collection counters. The summary prints every collection
	// whether or not it had rows, so a clean run on a workspace
	// with no theories or decisions still reports
	// "theories 0, decisions 0" with exit 0.
	type collectionResult struct {
		linked int
		errors int
	}
	results := make(map[string]*collectionResult, len(initTopicsCollections))
	for _, c := range initTopicsCollections {
		results[c] = &collectionResult{}
	}

	totalLinked := 0
	totalErrors := 0

	// Per-collection SELECT. A failure in the SELECT aborts that
	// collection's contribution (we cannot link rows we cannot
	// enumerate) but does not stop the other collections from
	// running.
	for _, collection := range initTopicsCollections {
		res := results[collection]
		rows, qerr := dm.SQLDB().Query(
			`SELECT id FROM memories WHERE collection = ? AND deleted_at IS NULL`,
			collection,
		)
		if qerr != nil {
			res.errors++
			totalErrors++
			usererror.Warn("init topics: select %s: %v", collection, qerr)
			continue
		}
		// mpm-lint closes rule: every *sql.Rows must be
		// Close()d before the enclosing function returns.
		// Defer directly in the enclosing function so the lint
		// classifier sees the defer in a covering position.
		defer rows.Close()
		for rows.Next() {
			var id string
			if scanE := rows.Scan(&id); scanE != nil {
				res.errors++
				totalErrors++
				usererror.Warn("init topics: scanning memory row for %s: %v", collection, scanE)
				continue
			}
			topicID, tErr := dm.GetOrCreateTopic(collection)
			if tErr != nil {
				res.errors++
				totalErrors++
				usererror.Warn("init topics: GetOrCreateTopic(%s): %v", collection, tErr)
				continue
			}
			execRes, execErr := dm.ExecTracked(
				`INSERT OR IGNORE INTO topic_memberships (memory_id, topic_id, created_at, role) VALUES (?, ?, ?, ?)`,
				5,
				id, topicID, now, "primary",
			)
			if execErr != nil {
				res.errors++
				totalErrors++
				usererror.Warn("init topics: insert membership %s → %s: %v", id, topicID, execErr)
				continue
			}
			if ra, _ := execRes.RowsAffected(); ra > 0 {
				res.linked++
				totalLinked++
			}
		}
	}

	// Render summary. The canonical "📚" emoji is preserved for
	// back-compat with scripts that grep the old backfill's
	// message. Per-collection breakdown and the error count are
	// additive fields.
	parts := make([]string, 0, len(initTopicsCollections))
	for _, c := range initTopicsCollections {
		r := results[c]
		if r.errors > 0 {
			parts = append(parts, fmt.Sprintf("%s %d linked, %d errors", c, r.linked, r.errors))
		} else {
			parts = append(parts, fmt.Sprintf("%s %d linked", c, r.linked))
		}
	}
	fmt.Printf("📚 Epistemology topics initialized: %d memories linked (%s)\n", totalLinked, joinComma(parts))

	if totalErrors > 0 {
		usererror.Error("init topics: %d error(s) across collections; see warnings above", totalErrors)
		return 1
	}
	return 0
}

// joinComma joins a slice with ", " for the per-collection
// breakdown. Kept local to this file because no other init command
// needs the same shape.
func joinComma(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += ", "
		}
		out += p
	}
	return out
}
