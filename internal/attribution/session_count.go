// Package attribution is the single implementation of the substrate's
// artifact-attribution rule.
//
// WHY THIS PACKAGE EXISTS. The rule — "how many durable artifacts did
// this framework session produce?" — had copies. The critic's telemetry
// hunt carried one (`internal/critic/artifact_count.go`), the
// missed-opportunity hunt carried a second (inline in its aggregate
// query), and the telemetry sidecar needed a third.
//
// Copies of an attribution rule drift. Two of them already answer
// slightly different questions: the critic's helper counts artifacts
// for ONE session, while the missed-opportunity hunt counts them as
// part of a GROUP BY over many sessions. Keeping the single-session
// rule in one leaf package lets the critic and the sidecar share it
// without either of them having to link the whole substrate.
//
// The sidecar in particular NEEDS the rule and must not link the
// substrate to get it: `mpm-telemetry observe` has to decide whether a
// session produced anything durable, and the only source for that
// answer is the substrate's `artifact_provenance` table.
//
// This package deliberately depends on nothing but database/sql. It
// opens no database, holds no connection, and has no init() side
// effects: a caller opens the substrate itself, read-only, and passes
// the handle in.
package attribution

import (
	"context"
	"database/sql"
	"fmt"
)

// SessionArtifactCount returns the number of artifacts attributed to a
// framework session.
//
// THE RULE. An artifact counts as belonging to a session when the
// substrate attributed it to one of that session's invocations — the
// `artifact_provenance` row's `invocation_id` matches the invocation's
// id. This mirrors the missed-opportunity hunt's
// `COUNT(DISTINCT ap.artifact_id)` over the same LEFT JOIN.
//
// A session with no invocations has no artifacts and reports 0. An
// empty session id reports 0 rather than erroring.
//
// The handle is not modified: the query is a plain SELECT and the
// callers open the substrate read-only.
func SessionArtifactCount(ctx context.Context, db *sql.DB, frameworkSessionID string) (int, error) {
	if db == nil {
		return 0, fmt.Errorf("attribution: SessionArtifactCount: db is required")
	}
	if frameworkSessionID == "" {
		return 0, nil
	}
	const q = `
		SELECT COUNT(DISTINCT ap.artifact_id)
		FROM tool_invocations ti
		LEFT JOIN artifact_provenance ap ON ap.invocation_id = ti.invocation_id
		WHERE ti.framework_session_id = ?`
	var count int
	if err := db.QueryRowContext(ctx, q, frameworkSessionID).Scan(&count); err != nil {
		return 0, fmt.Errorf("attribution: SessionArtifactCount(%q): %w", frameworkSessionID, err)
	}
	return count, nil
}
