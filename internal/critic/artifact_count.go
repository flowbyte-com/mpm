package critic

import (
	"context"
	"database/sql"
	"fmt"
)

// SessionArtifactCount returns the number of artifacts attributed to a
// framework session.
//
// WHY THIS EXISTS. The critic's telemetry hunt used to obtain this count
// by shelling out to `mpm call mpm_provenance --payload
// {"action":"count_by_session"}`. **That tool does not exist.** The
// registry publishes mpm_memory, mpm_theories, mpm_decisions, mpm_lessons,
// mpm_topics, mpm_references, mpm_evidence, mpm_confidence,
// mpm_retrieval_diagnose, mpm_context, mpm_skills, mpm_wakes, mpm_handoff,
// mpm_scratchpad, mpm_system, mpm_log_to_changelog, mpm_request_review,
// mpm_resolve, mpm_blob_read, mpm_blob_search and mpm_work — and no
// mpm_provenance. The hunt therefore FAILED on every qualifying session
// ("mpm_provenance: exit status 1"), and `mpm-critic --telemetry-hunt`
// exited 1.
//
// The count does not need a subprocess at all: the critic already holds
// the substrate's connection, and the attribution rule it needs is the
// one the missed-opportunity hunt already applies. This function applies
// exactly that rule, so the two hunts cannot drift apart.
//
// THE RULE. An artifact counts as belonging to a session when the
// substrate attributed it to one of that session's invocations — the
// `artifact_provenance` row's `invocation_id` matches the invocation's
// id. This mirrors the missed-opportunity hunt's
// `COUNT(DISTINCT ap.artifact_id)` over the same LEFT JOIN.
//
// A session with no invocations has no artifacts, and reports 0.
func SessionArtifactCount(ctx context.Context, db *sql.DB, frameworkSessionID string) (int, error) {
	if db == nil {
		return 0, fmt.Errorf("critic: SessionArtifactCount: db is required")
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
		return 0, fmt.Errorf("critic: SessionArtifactCount(%q): %w", frameworkSessionID, err)
	}
	return count, nil
}
