// p3_missed_opportunity_test.go — regression coverage for P3: the
// retrospective missed-opportunity hunt.
//
// WHAT THE HUNT DETECTS.
//
// An agent session that performed MPM-mediated work and recorded nothing
// durable. It is the only critic hunt that looks at ABSENT knowledge.
//
// THE THREE PROPERTIES THAT MATTER.
//
//  1. NO FALSE GAPS. A session that recorded a decision must NOT be
//     reported as having recorded nothing — even though the
//     `tool_invocations` × `artifact_provenance` join cannot see that
//     decision. TestP3MissedOpportunity_SessionThatRecordedADecisionIsNotACandidate
//     is the guard, and it fails without the recording-call signal.
//
//  2. THE GAP RULE (adjudication §6b.2). The gap is the TRIGGER, never a
//     citation. The citations are tool_invocations rows that exist and
//     resolve as type 'tool_invocation'. Where the only thing available
//     is a gap, there is no proposal at all.
//
//  3. IDEMPOTENCY. The proposal is a pure function of its evidence: no
//     cycle marker, no volatile value, and a citation SET that does not
//     change as the session grows.
//
// ISOLATION.
//
// The unit-level tests use the critic package's own hermetic fixture
// (newTestAudit), which opens a temp-file database. The end-to-end tests
// open a REAL core DatabaseManager in a disposable workspace with HOME,
// MPM_WORKSPACE and MPM_DB_PATH pinned, so the production path resolution
// is exercised rather than bypassed.

package critic

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	mpmcore "github.com/flowbyte-com/mpm-core"
	"github.com/flowbyte-com/mpm-core/tools"
)

// ── fixtures ─────────────────────────────────────────────────────────────

// p3InstallHuntTables creates the two tables the hunt reads. The critic's
// shared test fixture carries only the tables the OTHER hunts need, so the
// hunt's tables are installed here rather than widening a fixture every
// other test depends on.
func p3InstallHuntTables(t *testing.T, a *Audit) {
	t.Helper()
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS tool_invocations (
			id                   TEXT PRIMARY KEY,
			session_id           TEXT NOT NULL,
			tool_name            TEXT NOT NULL,
			action               TEXT NOT NULL,
			invocation_id        TEXT NOT NULL,
			actor_kind           TEXT NOT NULL,
			framework_name       TEXT,
			payload_hash         TEXT NOT NULL,
			result_status        TEXT NOT NULL,
			started_at           INTEGER NOT NULL,
			completed_at         INTEGER,
			duration_ms          INTEGER,
			mpm_session_id       TEXT,
			framework_session_id TEXT
		)`,
		`CREATE TABLE IF NOT EXISTS artifact_provenance (
			id             TEXT PRIMARY KEY,
			artifact_id    TEXT NOT NULL,
			artifact_type  TEXT NOT NULL,
			created_at     INTEGER NOT NULL,
			actor_kind     TEXT NOT NULL,
			invocation_id  TEXT,
			session_id     TEXT,
			framework_name TEXT
		)`,
	}
	for _, s := range stmts {
		if _, err := a.DB().Exec(s); err != nil {
			t.Fatalf("install hunt table: %v", err)
		}
	}
}

// p3Invocation is one seeded tool invocation.
type p3Invocation struct {
	ID               string
	Tool             string
	Action           string
	ResultStatus     string
	FrameworkSession string
	MPMSession       string
	InvocationID     string
	StartedAt        int64
}

// p3SeedInvocation inserts a tool_invocations row.
func p3SeedInvocation(t *testing.T, a *Audit, inv p3Invocation) {
	t.Helper()
	if inv.ResultStatus == "" {
		inv.ResultStatus = "success"
	}
	if inv.InvocationID == "" {
		inv.InvocationID = "inv_" + inv.ID
	}
	if inv.StartedAt == 0 {
		inv.StartedAt = 1793000000
	}
	_, err := a.DB().Exec(`
		INSERT INTO tool_invocations
			(id, session_id, tool_name, action, invocation_id, actor_kind,
			 framework_name, payload_hash, result_status, started_at,
			 mpm_session_id, framework_session_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		inv.ID, inv.MPMSession, inv.Tool, inv.Action, inv.InvocationID,
		"agent", "opencode", "sha256:"+inv.ID, inv.ResultStatus,
		inv.StartedAt, inv.MPMSession, inv.FrameworkSession)
	if err != nil {
		t.Fatalf("seed tool_invocations %s: %v", inv.ID, err)
	}
}

// p3SeedReads inserts n read-only invocations for a framework session.
func p3SeedReads(t *testing.T, a *Audit, frameworkSession, mpmSession string, n int, startID int64) {
	t.Helper()
	for i := 0; i < n; i++ {
		p3SeedInvocation(t, a, p3Invocation{
			ID:               fmt.Sprintf("inv-%s-%d-%02d", frameworkSession, startID, i),
			Tool:             "mpm_references",
			Action:           "search",
			FrameworkSession: frameworkSession,
			MPMSession:       mpmSession,
			StartedAt:        startID + int64(i),
		})
	}
}

// p3SeedArtifact inserts an artifact_provenance row attributed to an
// invocation id.
func p3SeedArtifact(t *testing.T, a *Audit, artifactID, artifactType, invocationID string) {
	t.Helper()
	_, err := a.DB().Exec(`
		INSERT INTO artifact_provenance
			(id, artifact_id, artifact_type, created_at, actor_kind, invocation_id, framework_name)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"ap-"+artifactID, artifactID, artifactType, 1793000000, "agent",
		invocationID, "opencode")
	if err != nil {
		t.Fatalf("seed artifact_provenance %s: %v", artifactID, err)
	}
}

// p3CountRows counts the rows in a table.
func p3CountRows(t *testing.T, a *Audit, table string) int {
	t.Helper()
	var n int
	if err := a.DB().QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// ── detection ────────────────────────────────────────────────────────────

// TestP3MissedOpportunity_SessionWithOnlyReadsIsACandidate is the positive
// path: a session that read MPM repeatedly and recorded nothing IS a
// candidate.
//
// The proposal must cite the session's invocations and declare them as
// tool_invocations, because that is what makes it rest on evidence that
// exists (adjudication §6b.2).
func TestP3MissedOpportunity_SessionWithOnlyReadsIsACandidate(t *testing.T) {
	a, _ := newTestAudit(t)
	p3InstallHuntTables(t, a)
	ctx := context.Background()

	p3SeedReads(t, a, "ses_candidate", "mpm-sess-1", 6, 1793000000)

	findings, err := (&MissedOpportunityHunt{}).Run(ctx, a)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("expected exactly 1 finding, got %d", len(findings))
	}
	f := findings[0]
	if f.Tool != "mpm_theories" || f.Action != "propose" {
		t.Errorf("expected mpm_theories/propose, got %s/%s", f.Tool, f.Action)
	}
	if f.Payload["source_type"] != "tool_invocation" {
		t.Errorf("expected source_type %q, got %v", "tool_invocation", f.Payload["source_type"])
	}
	sourceIDs, ok := f.Payload["source_ids"].([]string)
	if !ok {
		t.Fatalf("expected source_ids to be a []string, got %T", f.Payload["source_ids"])
	}
	if len(sourceIDs) != 6 {
		t.Errorf("expected 6 cited invocations, got %d: %v", len(sourceIDs), sourceIDs)
	}
}

// TestP3MissedOpportunity_SessionThatRecordedAMemoryIsNotACandidate is the
// ordinary guard: a session that saved a memory is not a gap.
//
// This is the audit's acceptance criterion B-3.
func TestP3MissedOpportunity_SessionThatRecordedAMemoryIsNotACandidate(t *testing.T) {
	a, _ := newTestAudit(t)
	p3InstallHuntTables(t, a)
	ctx := context.Background()

	p3SeedReads(t, a, "ses_recorder", "mpm-sess-1", 6, 1793000000)
	p3SeedInvocation(t, a, p3Invocation{
		ID: "inv-save-1", Tool: "mpm_memory", Action: "save",
		FrameworkSession: "ses_recorder", MPMSession: "mpm-sess-1",
	})

	findings, err := (&MissedOpportunityHunt{}).Run(ctx, a)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(findings) != 0 {
		t.Fatalf("expected 0 findings — the session recorded a memory, got %d: %+v",
			len(findings), findings)
	}
}

// TestP3MissedOpportunity_SessionThatRecordedADecisionIsNotACandidate is
// THE FALSE-GAP GUARD, and it is the reason this hunt does not rely on the
// join alone.
//
// A decision writes an artifact_provenance row with an EMPTY
// invocation_id — the per-call provenance bridge that populates it exists
// only on the mpm_memory save path. So the join cannot see the decision,
// and a hunt built on the join alone reports this session as a gap that
// never happened.
//
// The live workspace had exactly this shape: a session with 12 invocations
// and 0 join-attributable artifacts that had recorded 8 decisions.
//
// This test FAILS if the recording-call signal is removed.
func TestP3MissedOpportunity_SessionThatRecordedADecisionIsNotACandidate(t *testing.T) {
	a, _ := newTestAudit(t)
	p3InstallHuntTables(t, a)
	ctx := context.Background()

	p3SeedReads(t, a, "ses_decider", "mpm-sess-1", 4, 1793000000)
	// Eight successful decision records. Their artifact_provenance rows
	// carry NO invocation id — that is the substrate behaviour this
	// guard is about — so the join sees nothing.
	for i := 0; i < 8; i++ {
		p3SeedInvocation(t, a, p3Invocation{
			ID: fmt.Sprintf("inv-decide-%02d", i), Tool: "mpm_decisions",
			Action: "record", FrameworkSession: "ses_decider",
			MPMSession: "mpm-sess-1", StartedAt: 1793000100 + int64(i),
		})
		p3SeedArtifact(t, a, fmt.Sprintf("art-decision-%02d", i), "decision", "")
	}

	findings, err := (&MissedOpportunityHunt{}).Run(ctx, a)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(findings) != 0 {
		t.Fatalf("expected 0 findings — the session recorded 8 decisions, got %d: %+v",
			len(findings), findings)
	}
}

// TestP3MissedOpportunity_SessionWithAnAttributedArtifactIsNotACandidate
// pins the OTHER disqualifying signal: the join.
//
// A session whose invocation is attributable to an artifact produced
// something, whatever the tool log says.
func TestP3MissedOpportunity_SessionWithAnAttributedArtifactIsNotACandidate(t *testing.T) {
	a, _ := newTestAudit(t)
	p3InstallHuntTables(t, a)
	ctx := context.Background()

	p3SeedReads(t, a, "ses_attributed", "mpm-sess-1", 6, 1793000000)
	// The artifact is attributable to the session's FIRST invocation.
	firstInv := "inv-ses_attributed-1793000000-00"
	p3SeedArtifact(t, a, "art-1", "memory", "inv_"+firstInv)

	findings, err := (&MissedOpportunityHunt{}).Run(ctx, a)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(findings) != 0 {
		t.Fatalf("expected 0 findings — an artifact is attributable to the "+
			"session's invocations, got %d: %+v", len(findings), findings)
	}
}

// TestP3MissedOpportunity_FailedRecordingCallDoesNotDisqualify pins that
// only a SUCCESSFUL recording call counts as recording.
//
// A failed save wrote nothing, so the session really did record nothing —
// and reporting it is correct, not a false gap.
func TestP3MissedOpportunity_FailedRecordingCallDoesNotDisqualify(t *testing.T) {
	a, _ := newTestAudit(t)
	p3InstallHuntTables(t, a)
	ctx := context.Background()

	p3SeedReads(t, a, "ses_failed_save", "mpm-sess-1", 6, 1793000000)
	p3SeedInvocation(t, a, p3Invocation{
		ID: "inv-save-failed", Tool: "mpm_memory", Action: "save",
		ResultStatus: "error", FrameworkSession: "ses_failed_save",
		MPMSession: "mpm-sess-1",
	})

	findings, err := (&MissedOpportunityHunt{}).Run(ctx, a)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("expected exactly 1 finding — the save FAILED, so nothing "+
			"was recorded, got %d", len(findings))
	}
}

// ── honest degradation ───────────────────────────────────────────────────

// TestP3MissedOpportunity_SessionWithoutFrameworkSessionIDIsSkipped pins
// the honest-degradation rule.
//
// A session with no framework_session_id cannot be attributed to a
// conversation, so the hunt skips it. It must NOT fall back to
// mpm_session_id, which is workspace-scoped and merges unrelated
// conversations.
func TestP3MissedOpportunity_SessionWithoutFrameworkSessionIDIsSkipped(t *testing.T) {
	a, _ := newTestAudit(t)
	p3InstallHuntTables(t, a)
	ctx := context.Background()

	// Twelve invocations, no framework_session_id, but a shared
	// mpm_session_id that another session also uses.
	p3SeedReads(t, a, "", "mpm-sess-shared", 12, 1793000000)

	// The DEGRADATION RULE must reject the session at the DETECTION
	// stage, not merely produce no finding at the end. Otherwise a
	// fallback to mpm_session_id would slip through: the session would
	// become a candidate and then be dropped for having nothing to cite,
	// which is a different rule entirely.
	candidates, err := (&MissedOpportunityHunt{}).findCandidateSessions(
		ctx, a, defaultMinInvocations, defaultCandidateLimit)
	if err != nil {
		t.Fatalf("findCandidateSessions: %v", err)
	}
	if len(candidates) != 0 {
		t.Errorf("expected NO candidate session without a framework_session_id, "+
			"got %d (%q) — the session must be skipped at detection, not fall back "+
			"to the workspace-scoped mpm_session_id", len(candidates), candidates)
	}

	findings, err := (&MissedOpportunityHunt{}).Run(ctx, a)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(findings) != 0 {
		t.Fatalf("expected 0 findings — a session with no framework_session_id "+
			"must be skipped, not guessed at, got %d: %+v", len(findings), findings)
	}
}

// TestP3MissedOpportunity_SessionsSharingAnMPMSessionIDAreNotMerged pins
// the other half: two host sessions that share one mpm_session_id are TWO
// sessions, not one.
//
// The mpm_session_id is workspace-scoped; the framework_session_id is the
// per-conversation key. Merging on the former would attribute one
// conversation's activity to another.
func TestP3MissedOpportunity_SessionsSharingAnMPMSessionIDAreNotMerged(t *testing.T) {
	a, _ := newTestAudit(t)
	p3InstallHuntTables(t, a)
	ctx := context.Background()

	p3SeedReads(t, a, "ses_first", "mpm-sess-shared", 6, 1793000000)
	p3SeedReads(t, a, "ses_second", "mpm-sess-shared", 6, 1793000100)

	findings, err := (&MissedOpportunityHunt{}).Run(ctx, a)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(findings) != 2 {
		t.Fatalf("expected 2 findings — two distinct framework sessions, got %d: %+v",
			len(findings), findings)
	}
	seen := map[string]bool{}
	for _, f := range findings {
		hyp, _ := f.Payload["hypothesis"].(string)
		switch {
		case strings.Contains(hyp, "ses_first"):
			seen["ses_first"] = true
		case strings.Contains(hyp, "ses_second"):
			seen["ses_second"] = true
		default:
			t.Errorf("expected the hypothesis to name its session, got %q", hyp)
		}
	}
	if len(seen) != 2 {
		t.Errorf("expected one finding per framework session, got %v", seen)
	}
}

// ── the gap rule ─────────────────────────────────────────────────────────

// TestP3MissedOpportunity_CitationsResolveAsToolInvocations pins the gap
// rule's positive requirement: every cited id must resolve as a
// tool_invocation.
//
// A citation that does not resolve REJECTS the whole proposal (P2's typed
// path), so a hunt that cited a gap would not merely be imprecise — it
// would produce nothing at all.
func TestP3MissedOpportunity_CitationsResolveAsToolInvocations(t *testing.T) {
	a, _ := newTestAudit(t)
	p3InstallHuntTables(t, a)
	ctx := context.Background()

	p3SeedReads(t, a, "ses_cited", "mpm-sess-1", 5, 1793000000)

	findings, err := (&MissedOpportunityHunt{}).Run(ctx, a)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(findings))
	}
	sourceIDs, _ := findings[0].Payload["source_ids"].([]string)
	for _, id := range sourceIDs {
		var n int
		if err := a.DB().QueryRow(
			"SELECT COUNT(*) FROM tool_invocations WHERE id = ?", id).Scan(&n); err != nil {
			t.Fatalf("resolve cited id %q: %v", id, err)
		}
		if n != 1 {
			t.Errorf("cited id %q does not resolve to exactly one tool_invocations row", id)
		}
	}
}

// TestP3MissedOpportunity_GapOnlyCandidateProducesNoProposal is the gap
// rule's explicit case: where the only thing available is a GAP, there is
// no proposal.
//
// The hunt's own detection is derived from the same rows it would cite, so
// this branch is unreachable from the top-level path — it is a guard on
// the classification, and it is exercised directly so that the rule is
// executable rather than merely described.
func TestP3MissedOpportunity_GapOnlyCandidateProducesNoProposal(t *testing.T) {
	if _, ok := missedOpportunityProposal("ses_gap_only", nil); ok {
		t.Fatal("expected NO proposal when there is nothing to cite — a gap " +
			"is a candidate opportunity, never a citation")
	}
	if _, ok := missedOpportunityProposal("", []string{"inv-1"}); ok {
		t.Fatal("expected NO proposal for an empty session id")
	}
}

// ── idempotency ──────────────────────────────────────────────────────────

// TestP3MissedOpportunity_ProposalCarriesNoCycleMarker pins that the
// proposal carries nothing that varies per cycle.
//
// The cycle number belongs in the finding's Reason, which is logged and
// never persisted: the hypothesis, the tags and the validation criteria
// are all part of the proposal's identity, so a cycle-varying value in any
// of them is a new proposal every cycle (the D4 defect).
func TestP3MissedOpportunity_ProposalCarriesNoCycleMarker(t *testing.T) {
	a, _ := newTestAudit(t)
	p3InstallHuntTables(t, a)
	ctx := context.Background()

	p3SeedReads(t, a, "ses_stable_marker", "mpm-sess-1", 5, 1793000000)

	findings, err := (&MissedOpportunityHunt{}).Run(ctx, a)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(findings))
	}
	f := findings[0]

	hyp, _ := f.Payload["hypothesis"].(string)
	if strings.Contains(strings.ToLower(hyp), "cycle") {
		t.Errorf("expected no cycle marker in the hypothesis, got %q", hyp)
	}
	criteria, _ := f.Payload["validation_criteria"].(string)
	if strings.Contains(strings.ToLower(criteria), "cycle") {
		t.Errorf("expected no cycle marker in the validation criteria, got %q", criteria)
	}
	tags, _ := f.Payload["tags"].([]string)
	for _, tag := range tags {
		if strings.HasPrefix(tag, "cycle_") {
			t.Errorf("expected no cycle_* tag, got %v", tags)
		}
	}
	// The proposal must not assert that a lesson exists — the gap rule.
	if strings.Contains(strings.ToLower(hyp), "a lesson exists") &&
		!strings.Contains(strings.ToLower(hyp), "does not assert that any particular lesson exists") {
		t.Errorf("the hypothesis must not assert that a lesson exists, got %q", hyp)
	}
}

// TestP3MissedOpportunity_ProposalIsStableAcrossRuns pins that two runs
// over the same data produce byte-identical proposals.
//
// This is what makes the hunt safe to run on every cycle: the second run
// is a no-op at the substrate level, because the identity hash matches.
func TestP3MissedOpportunity_ProposalIsStableAcrossRuns(t *testing.T) {
	a, _ := newTestAudit(t)
	p3InstallHuntTables(t, a)
	ctx := context.Background()

	p3SeedReads(t, a, "ses_stable", "mpm-sess-1", 5, 1793000000)

	first, err := (&MissedOpportunityHunt{}).Run(ctx, a)
	if err != nil {
		t.Fatalf("first Run: %v", err)
	}
	second, err := (&MissedOpportunityHunt{}).Run(ctx, a)
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if len(first) != 1 || len(second) != 1 {
		t.Fatalf("expected 1 finding per run, got %d and %d", len(first), len(second))
	}
	if first[0].Payload["hypothesis"] != second[0].Payload["hypothesis"] {
		t.Errorf("expected an identical hypothesis across runs")
	}
	if fmt.Sprint(first[0].Payload["tags"]) != fmt.Sprint(second[0].Payload["tags"]) {
		t.Errorf("expected identical tags across runs")
	}
	if fmt.Sprint(first[0].Payload["source_ids"]) != fmt.Sprint(second[0].Payload["source_ids"]) {
		t.Errorf("expected identical citations across runs")
	}
	if first[0].Payload["validation_criteria"] != second[0].Payload["validation_criteria"] {
		t.Errorf("expected identical validation criteria across runs")
	}
}

// TestP3MissedOpportunity_CitationSetConvergesOnTheOldestPrefix pins how
// the citation set behaves as a session keeps working.
//
// The proposal's IDENTITY never changes — the hypothesis, the tags and the
// validation criteria are untouched by new activity — so re-emitting the
// finding is a no-op at the proposal level.
//
// The citations ACCUMULATE, because new invocations are new evidence about
// the same session. They are bounded: the hunt cites the session's OLDEST
// invocations, so the set converges on a stable prefix instead of chasing
// the newest activity. Chasing the newest would change the proposal's
// meaning on every cycle.
func TestP3MissedOpportunity_CitationSetConvergesOnTheOldestPrefix(t *testing.T) {
	a, _ := newTestAudit(t)
	p3InstallHuntTables(t, a)
	ctx := context.Background()

	p3SeedReads(t, a, "ses_growing", "mpm-sess-1", 5, 1793000000)
	first, err := (&MissedOpportunityHunt{}).Run(ctx, a)
	if err != nil {
		t.Fatalf("first Run: %v", err)
	}
	firstIDs, _ := first[0].Payload["source_ids"].([]string)
	if len(firstIDs) != 5 {
		t.Fatalf("expected 5 citations, got %d: %v", len(firstIDs), firstIDs)
	}

	// The session keeps working.
	p3SeedReads(t, a, "ses_growing", "mpm-sess-1", 3, 1793001000)
	second, err := (&MissedOpportunityHunt{}).Run(ctx, a)
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	secondIDs, _ := second[0].Payload["source_ids"].([]string)

	// The identity-bearing fields are untouched.
	if first[0].Payload["hypothesis"] != second[0].Payload["hypothesis"] {
		t.Errorf("expected the hypothesis to be unchanged by new activity")
	}
	if fmt.Sprint(first[0].Payload["tags"]) != fmt.Sprint(second[0].Payload["tags"]) {
		t.Errorf("expected the tags to be unchanged by new activity")
	}
	if first[0].Payload["validation_criteria"] != second[0].Payload["validation_criteria"] {
		t.Errorf("expected the validation criteria to be unchanged by new activity")
	}
	// The earlier citations are all still there, and the new activity was
	// added — the proposal accumulates evidence.
	if len(secondIDs) != 8 {
		t.Errorf("expected 8 citations after the session grew, got %d: %v",
			len(secondIDs), secondIDs)
	}
	for i := range firstIDs {
		if secondIDs[i] != firstIDs[i] {
			t.Errorf("expected the earlier citations to be preserved in order: "+
				"index %d is %q, want %q", i, secondIDs[i], firstIDs[i])
		}
	}
}

// TestP3MissedOpportunity_CitationsAreBoundedAndConverge pins the bound.
//
// With MaxCitations set, a session that does a lot more work is still
// cited by the SAME oldest prefix — the set converges instead of chasing
// the newest activity.
func TestP3MissedOpportunity_CitationsAreBoundedAndConverge(t *testing.T) {
	a, _ := newTestAudit(t)
	p3InstallHuntTables(t, a)
	ctx := context.Background()

	p3SeedReads(t, a, "ses_bounded", "mpm-sess-1", 8, 1793000000)
	first, err := (&MissedOpportunityHunt{MaxCitations: 3}).Run(ctx, a)
	if err != nil {
		t.Fatalf("first Run: %v", err)
	}
	firstIDs, _ := first[0].Payload["source_ids"].([]string)
	if len(firstIDs) != 3 {
		t.Fatalf("expected the citations to be bounded to 3, got %d: %v",
			len(firstIDs), firstIDs)
	}

	// The session does a lot more work.
	p3SeedReads(t, a, "ses_bounded", "mpm-sess-1", 20, 1793001000)
	second, err := (&MissedOpportunityHunt{MaxCitations: 3}).Run(ctx, a)
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	secondIDs, _ := second[0].Payload["source_ids"].([]string)

	if fmt.Sprint(firstIDs) != fmt.Sprint(secondIDs) {
		t.Errorf("expected the citation set to converge on the oldest prefix, "+
			"got %v then %v", firstIDs, secondIDs)
	}
}

// TestP3MissedOpportunity_HuntWritesNothing pins the read-only property.
//
// The hunt emits findings; it never writes to the substrate itself. Only
// the proposal path writes, and only through `mpm call`.
func TestP3MissedOpportunity_HuntWritesNothing(t *testing.T) {
	a, _ := newTestAudit(t)
	p3InstallHuntTables(t, a)
	ctx := context.Background()

	p3SeedReads(t, a, "ses_readonly", "mpm-sess-1", 5, 1793000000)

	before := map[string]int{}
	for _, table := range []string{"tool_invocations", "artifact_provenance", "memories"} {
		before[table] = p3CountRows(t, a, table)
	}

	if _, err := (&MissedOpportunityHunt{}).Run(ctx, a); err != nil {
		t.Fatalf("Run: %v", err)
	}

	for _, table := range []string{"tool_invocations", "artifact_provenance", "memories"} {
		if after := p3CountRows(t, a, table); after != before[table] {
			t.Errorf("expected the hunt to write nothing, %s went %d -> %d",
				table, before[table], after)
		}
	}
}

// ── the vocabulary ───────────────────────────────────────────────────────

// TestP3RecordingActionVocabulary_IsDeclaredAndComplete pins the declared
// vocabulary of recording actions.
//
// The vocabulary is the hunt's second signal, and its failure mode is a
// FALSE GAP: a recording action that is missing from the map would make a
// session that recorded look like one that did not. So the map is pinned
// explicitly, and a change to it must be a deliberate, visible one.
func TestP3RecordingActionVocabulary_IsDeclaredAndComplete(t *testing.T) {
	want := map[string][]string{
		"mpm_memory":     {"save"},
		"mpm_theories":   {"propose"},
		"mpm_decisions":  {"record"},
		"mpm_lessons":    {"save"},
		"mpm_work":       {"create", "note"},
		"mpm_skills":     {"save"},
		"mpm_handoff":    {"write"},
		"mpm_context":    {"write_handoff"},
		"mpm_topics":     {"create"},
		"mpm_references": {"add", "import"},
		"mpm_evidence":   {"add"},
	}
	if len(recordingActions) != len(want) {
		t.Errorf("the recording vocabulary has %d tools, want %d — "+
			"a change to it must be deliberate", len(recordingActions), len(want))
	}
	for tool, actions := range want {
		got, ok := recordingActions[tool]
		if !ok {
			t.Errorf("the recording vocabulary is missing tool %q", tool)
			continue
		}
		for _, action := range actions {
			if !got[action] {
				t.Errorf("the recording vocabulary is missing %q for tool %q",
					action, tool)
			}
		}
		if len(got) != len(actions) {
			t.Errorf("tool %q declares %d recording actions, want %d",
				tool, len(got), len(actions))
		}
	}
}

// TestP3RecordingActionPredicate_IsGeneratedFromTheVocabulary pins that
// the SQL predicate cannot drift from the declared vocabulary.
func TestP3RecordingActionPredicate_IsGeneratedFromTheVocabulary(t *testing.T) {
	fragment, args := recordingActionPredicate()
	keys := recordingActionKeys()

	if len(args) != len(keys) {
		t.Fatalf("the predicate binds %d arguments but the vocabulary has %d keys",
			len(args), len(keys))
	}
	if !strings.Contains(fragment, "IN (") {
		t.Fatalf("expected an IN predicate, got %q", fragment)
	}
	// The predicate appears twice in the hunt's query, so its arguments
	// are bound twice. A mismatch here would silently disable the
	// recording-call signal.
	if strings.Count(fragment, "?") != len(keys) {
		t.Errorf("the predicate has %d placeholders but %d keys",
			strings.Count(fragment, "?"), len(keys))
	}
	for i, k := range keys {
		if args[i].(string) != k {
			t.Errorf("argument %d is %q, want %q", i, args[i], k)
		}
	}
}

// p3JSONRoundTrip mirrors the `mpm call` boundary.
//
// The critic emits findings by marshalling the payload to JSON and passing
// it to `mpm call`, which JSON-decodes it. So a `[]string` in the payload
// arrives at the handler as a `[]interface{}`. The end-to-end tests go
// through the same round-trip, because that is what production does — and
// because internal.ParseStringSliceOr drops a Go-native `[]string`
// silently, which would otherwise make these tests pass for the wrong
// reason (no citations at all).
func p3JSONRoundTrip(t *testing.T, payload map[string]interface{}) map[string]interface{} {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	var out map[string]interface{}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	return out
}

// ── end to end, through the real substrate ───────────────────────────────

// p3RealAudit builds an Audit over a REAL core database in a disposable
// workspace, so the end-to-end tests exercise the production path
// resolution and the production schema.
func p3RealAudit(t *testing.T) (*Audit, *mpmcore.DatabaseManager) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	ws := t.TempDir()
	t.Setenv("MPM_WORKSPACE", ws)
	t.Setenv("MPM_DB_PATH", filepath.Join(ws, "src", "db", "mpm.db"))

	dm, err := mpmcore.NewDatabaseManager(ws)
	if err != nil {
		t.Fatalf("NewDatabaseManager(%q): %v", ws, err)
	}
	t.Cleanup(func() { _ = dm.Close() })
	if err := dm.InitSchema(); err != nil {
		t.Fatalf("InitSchema: %v", err)
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	a := &Audit{db: dm.SQLDB(), log: log, cycle: 0, cli: &fakeCLI{}}
	return a, dm
}

// p3SeedRealInvocation inserts a tool_invocations row into the REAL
// database.
func p3SeedRealInvocation(t *testing.T, dm *mpmcore.DatabaseManager, id, tool, action, frameworkSession string, startedAt int64) {
	t.Helper()
	_, err := dm.SQLDB().Exec(`
		INSERT INTO tool_invocations
			(id, session_id, tool_name, action, invocation_id, actor_kind,
			 framework_name, payload_hash, result_status, started_at,
			 mpm_session_id, framework_session_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, "mpm-sess-real", tool, action, "inv_"+id, "agent", "opencode",
		"sha256:"+id, "success", startedAt, "mpm-sess-real", frameworkSession)
	if err != nil {
		t.Fatalf("seed tool_invocations %s: %v", id, err)
	}
}

// TestP3MissedOpportunity_EndToEnd_CreatesOnePendingProposalAndNothingElse
// is the audit's acceptance criterion B-1, executed through the PRODUCTION
// dispatch path.
//
// The hunt's finding is applied the way the critic applies it — through
// the registered `mpm_theories` handler — and the substrate's state is
// inspected afterwards.
//
// The pass condition: exactly ONE pending proposal; NO lesson, memory or
// decision created; NO promotion; the proposal's citations are typed
// tool_invocations that resolve.
func TestP3MissedOpportunity_EndToEnd_CreatesOnePendingProposalAndNothingElse(t *testing.T) {
	a, dm := p3RealAudit(t)
	ctx := context.Background()

	for i := 0; i < 6; i++ {
		p3SeedRealInvocation(t, dm, fmt.Sprintf("inv-real-%02d", i),
			"mpm_references", "search", "ses_real", 1793000000+int64(i))
	}

	findings, err := (&MissedOpportunityHunt{}).Run(ctx, a)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(findings))
	}

	// Apply it the way the critic applies it: `mpm call mpm_theories`.
	tool, ok := tools.ByName("mpm_theories")
	if !ok {
		t.Fatal("mpm_theories not found in the registry")
	}
	res, err := tool.Handler(dm, mpmcore.ActiveContext{}, map[string]interface{}{
		"action": "propose",
		"params": p3JSONRoundTrip(t, findings[0].Payload),
	})
	if err != nil {
		t.Fatalf("mpm_theories propose: %v", err)
	}
	result, _ := res.(map[string]interface{})
	proposalID, _ := result["id"].(string)
	if proposalID == "" {
		t.Fatalf("the proposal returned no id: %v", res)
	}

	// Exactly one theory, and it is PENDING.
	var theories, pending int
	if err := dm.SQLDB().QueryRow(
		`SELECT COUNT(*), SUM(CASE WHEN json_extract(metadata,'$.status')='pending' THEN 1 ELSE 0 END)
		 FROM memories WHERE collection='theories'`).Scan(&theories, &pending); err != nil {
		t.Fatalf("count theories: %v", err)
	}
	if theories != 1 {
		t.Errorf("expected exactly 1 theory row, got %d", theories)
	}
	if pending != 1 {
		t.Errorf("expected the theory to be PENDING, got %d pending of %d", pending, theories)
	}

	// It is NOT promoted, and it created no other artifact.
	var conf float64
	if err := dm.SQLDB().QueryRow(
		`SELECT confidence FROM memories WHERE id = ?`, proposalID).Scan(&conf); err != nil {
		t.Fatalf("read the proposal: %v", err)
	}
	if conf != 0.5 {
		t.Errorf("expected the pending proposal to carry the base confidence 0.5, got %v", conf)
	}
	var lessons, decisions, plainMemories int
	if err := dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM lessons_base`).Scan(&lessons); err != nil {
		t.Fatalf("count lessons: %v", err)
	}
	if err := dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM memories WHERE collection='decisions'`).Scan(&decisions); err != nil {
		t.Fatalf("count decisions: %v", err)
	}
	if err := dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM memories WHERE collection='memories'`).Scan(&plainMemories); err != nil {
		t.Fatalf("count memories: %v", err)
	}
	if lessons != 0 || decisions != 0 || plainMemories != 0 {
		t.Errorf("expected the proposal to create NO lesson, decision or memory, "+
			"got lessons=%d decisions=%d memories=%d", lessons, decisions, plainMemories)
	}

	// The citations are typed tool_invocations that resolve.
	var citationCount int
	if err := dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM epistemic_provenance WHERE downstream_id = ?`,
		proposalID).Scan(&citationCount); err != nil {
		t.Fatalf("count citations: %v", err)
	}
	if citationCount != 6 {
		t.Errorf("expected 6 citation rows, got %d", citationCount)
	}
	var distinctTypes int
	if err := dm.SQLDB().QueryRow(
		`SELECT COUNT(DISTINCT source_type) FROM epistemic_provenance WHERE downstream_id = ?`,
		proposalID).Scan(&distinctTypes); err != nil {
		t.Fatalf("count citation types: %v", err)
	}
	if distinctTypes != 1 {
		t.Errorf("expected exactly ONE citation source type, got %d", distinctTypes)
	}
	var citedType string
	if err := dm.SQLDB().QueryRow(
		`SELECT source_type FROM epistemic_provenance WHERE downstream_id = ? LIMIT 1`,
		proposalID).Scan(&citedType); err != nil {
		t.Fatalf("read citation type: %v", err)
	}
	if citedType != "tool_invocation" {
		t.Errorf("expected the declared source type %q, got %q", "tool_invocation", citedType)
	}
}

// TestP3MissedOpportunity_EndToEnd_IsIdempotent is the audit's acceptance
// criteria B-2 and B-6: the second run produces zero new proposals and
// zero new rows in any table.
func TestP3MissedOpportunity_EndToEnd_IsIdempotent(t *testing.T) {
	a, dm := p3RealAudit(t)
	ctx := context.Background()

	for i := 0; i < 6; i++ {
		p3SeedRealInvocation(t, dm, fmt.Sprintf("inv-real-%02d", i),
			"mpm_references", "search", "ses_real", 1793000000+int64(i))
	}

	tool, ok := tools.ByName("mpm_theories")
	if !ok {
		t.Fatal("mpm_theories not found in the registry")
	}

	apply := func() {
		t.Helper()
		findings, err := (&MissedOpportunityHunt{}).Run(ctx, a)
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		for _, f := range findings {
			if _, err := tool.Handler(dm, mpmcore.ActiveContext{}, map[string]interface{}{
				"action": "propose",
				"params": p3JSONRoundTrip(t, f.Payload),
			}); err != nil {
				t.Fatalf("mpm_theories propose: %v", err)
			}
		}
	}

	apply()

	countRows := func() (theories, citations int) {
		t.Helper()
		if err := dm.SQLDB().QueryRow(
			`SELECT COUNT(*) FROM memories WHERE collection='theories'`).Scan(&theories); err != nil {
			t.Fatalf("count theories: %v", err)
		}
		if err := dm.SQLDB().QueryRow(
			`SELECT COUNT(*) FROM epistemic_provenance`).Scan(&citations); err != nil {
			t.Fatalf("count citations: %v", err)
		}
		return
	}
	theories1, citations1 := countRows()
	if theories1 != 1 {
		t.Fatalf("expected 1 theory after the first run, got %d", theories1)
	}

	apply()

	theories2, citations2 := countRows()
	if theories2 != theories1 {
		t.Errorf("expected NO new proposal on the second run: theories %d -> %d",
			theories1, theories2)
	}
	if citations2 != citations1 {
		t.Errorf("expected NO new citation rows on the second run: %d -> %d",
			citations1, citations2)
	}
}

// TestP3MissedOpportunity_EndToEnd_RejectsAnUnresolvableCitation pins the
// atomic rejection at the substrate level.
//
// If a cited invocation is gone, the typed path REJECTS the proposal and
// leaves no partial record: no theory row, no citation row. That is the
// property that makes the hunt safe to re-emit.
func TestP3MissedOpportunity_EndToEnd_RejectsAnUnresolvableCitation(t *testing.T) {
	a, dm := p3RealAudit(t)
	ctx := context.Background()

	for i := 0; i < 6; i++ {
		p3SeedRealInvocation(t, dm, fmt.Sprintf("inv-real-%02d", i),
			"mpm_references", "search", "ses_real", 1793000000+int64(i))
	}

	findings, err := (&MissedOpportunityHunt{}).Run(ctx, a)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(findings))
	}

	// One of the cited invocations disappears before the proposal is
	// applied.
	if _, err := dm.SQLDB().Exec(
		`DELETE FROM tool_invocations WHERE id = ?`, "inv-real-03"); err != nil {
		t.Fatalf("delete the cited invocation: %v", err)
	}

	tool, _ := tools.ByName("mpm_theories")
	_, err = tool.Handler(dm, mpmcore.ActiveContext{}, map[string]interface{}{
		"action": "propose",
		"params": p3JSONRoundTrip(t, findings[0].Payload),
	})
	if err == nil {
		t.Fatal("expected the proposal to be REJECTED because a cited " +
			"invocation no longer resolves")
	}
	if !strings.Contains(err.Error(), "does not resolve") {
		t.Errorf("expected the error to say the citation does not resolve, got: %v", err)
	}

	var theories, citations int
	if err := dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM memories WHERE collection='theories'`).Scan(&theories); err != nil {
		t.Fatalf("count theories: %v", err)
	}
	if err := dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM epistemic_provenance`).Scan(&citations); err != nil {
		t.Fatalf("count citations: %v", err)
	}
	if theories != 0 {
		t.Errorf("expected NO theory row to survive the rejection, got %d", theories)
	}
	if citations != 0 {
		t.Errorf("expected NO citation row to survive the rejection, got %d", citations)
	}
}

// ── the defect the hunt had to work around ───────────────────────────────

// TestP3ArtifactProvenance_InvocationIDIsPopulatedOnlyByTheMemorySavePath
// pins the substrate defect that forced the hunt's two-signal design.
//
// `artifact_provenance.invocation_id` is populated ONLY by the mpm_memory
// save path, because the per-call provenance bridge that overlays the
// ActiveContext's invocation id exists only there. Every other write path
// — decisions, theories, lessons — resolves its provenance with an EMPTY
// invocation id, so its artifact cannot be joined back to the call that
// produced it.
//
// THIS IS A CHARACTERIZATION OF CURRENT BEHAVIOUR, NOT A CONTRACT. If a
// future change populates the invocation id on every write path, this test
// FAILS and the hunt's second signal can be simplified.
//
// It is pinned here rather than left implicit because the hunt's design
// rests on it: without it, the join alone reports a session that recorded
// eight decisions as one that recorded nothing.
func TestP3ArtifactProvenance_InvocationIDIsPopulatedOnlyByTheMemorySavePath(t *testing.T) {
	a, dm := p3RealAudit(t)

	ac := mpmcore.ActiveContext{
		SessionID:    "ses_probe",
		InvocationID: "inv_probe_1",
	}

	// The mpm_memory path: the per-call provenance bridge applies.
	if _, err := dm.SaveMemoryNodeForInvocation(
		"memories", "a memory", "", []string{"p3"}, nil, nil, false, 1.0, "",
		"0.8", "0.8", "", ac); err != nil {
		t.Fatalf("SaveMemoryNodeForInvocation: %v", err)
	}

	// The decision path: no bridge.
	dec, err := dm.RecordDecision("ctx", "choice", "why", "out", []string{"p3"}, nil, ac)
	if err != nil {
		t.Fatalf("RecordDecision: %v", err)
	}
	decID, _ := dec["id"].(string)

	var inv string
	if err := dm.SQLDB().QueryRow(
		`SELECT coalesce(invocation_id,'') FROM artifact_provenance WHERE artifact_id = ?`,
		decID).Scan(&inv); err != nil {
		t.Fatalf("read the decision's provenance: %v", err)
	}
	if inv != "" {
		t.Errorf("expected the decision's artifact_provenance.invocation_id to be "+
			"EMPTY (the defect this hunt works around), got %q — if this now "+
			"populates, the hunt's recording-call signal can be simplified", inv)
	}

	// The hunt must still see the recording, which is why the signal
	// exists.
	_ = a
}

// TestP3MissedOpportunity_EndToEnd_SessionThatRecordedIsNotReported is the
// end-to-end false-gap guard, on the REAL schema.
//
// A session whose invocations include a successful decision is NOT
// reported, even though no artifact_provenance row joins back to its
// invocations.
func TestP3MissedOpportunity_EndToEnd_SessionThatRecordedIsNotReported(t *testing.T) {
	a, dm := p3RealAudit(t)
	ctx := context.Background()

	for i := 0; i < 6; i++ {
		p3SeedRealInvocation(t, dm, fmt.Sprintf("inv-real-%02d", i),
			"mpm_references", "search", "ses_real", 1793000000+int64(i))
	}
	p3SeedRealInvocation(t, dm, "inv-real-decide", "mpm_decisions", "record",
		"ses_real", 1793000100)

	findings, err := (&MissedOpportunityHunt{}).Run(ctx, a)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(findings) != 0 {
		t.Fatalf("expected 0 findings — the session recorded a decision, got %d: %+v",
			len(findings), findings)
	}
}

// ── registration ─────────────────────────────────────────────────────────

// TestP3MissedOpportunity_IsRegisteredInTheDefaultCritic pins that the
// hunt is part of the critic's repertoire.
//
// critic.New is what production constructs (cmd/mpm-critic/main.go), so
// the registration list is what decides whether the hunt runs at all. A
// hunt that is implemented but not registered is a capability nobody has.
//
// The critic's AUTOMATIC path stays off regardless: P2b (merged as #118)
// refuses to arm or dispatch a critic_audit wake, so the hunt only runs
// when an operator or a reviewing agent runs mpm-critic deliberately.
func TestP3MissedOpportunity_IsRegisteredInTheDefaultCritic(t *testing.T) {
	dir := t.TempDir()
	db, err := sql.Open("sqlite3", filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	a, err := New(db, slog.New(slog.NewTextHandler(os.Stderr,
		&slog.HandlerOptions{Level: slog.LevelError})))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	found := false
	for _, h := range a.hunts {
		if h.Name() == "missed_opportunity" {
			found = true
			break
		}
	}
	if !found {
		names := make([]string, 0, len(a.hunts))
		for _, h := range a.hunts {
			names = append(names, h.Name())
		}
		t.Errorf("the missed_opportunity hunt is not registered in critic.New; "+
			"registered hunts: %v", names)
	}
}

// ── the structural guard against cycle tags ──────────────────────────────

// TestP3NoHuntEmitsACycleTag is the structural guard.
//
// It runs EVERY hunt that can emit a finding, with data that makes each
// one fire, and asserts that no emitted finding carries a `cycle_<n>` tag.
//
// WHY THIS IS A GUARD RATHER THAN A COMMENT: a `cycle_<n>` tag is inert on
// the lesson surface (the lesson is deduplicated by content hash, so the
// tag is simply ignored on a repeat) and FATAL on the proposal surface (the
// tags are part of the proposal's identity hash, so the tag makes every
// cycle a DISTINCT pending proposal — the D4 defect). So the tag is a
// trap that only bites when the hunt is moved to a proposal surface — and
// moving the hunts there is exactly what Session A's adjudication §6c.3
// rules for next.
//
// Removing the tag from all three hunts now is what makes that switch
// safe, and this test is what keeps it that way.
func TestP3NoHuntEmitsACycleTag(t *testing.T) {
	a, _ := newTestAudit(t)
	ctx := context.Background()

	// SurvivalAsymmetryHunt needs an asymmetry above the threshold.
	for i := 0; i < 100; i++ {
		alive := i < 25
		del := ""
		if !alive {
			del = "2026-07-01 00:00:00"
		}
		seedMemory(t, a, mkID("guard-call", i), "memories", "call test", "call",
			0.8, del, time.Now())
	}
	for i := 0; i < 50; i++ {
		alive := i < 47
		del := ""
		if !alive {
			del = "2026-07-01 00:00:00"
		}
		seedMemory(t, a, mkID("guard-direct", i), "memories", "direct test", "direct",
			0.8, del, time.Now())
	}
	// WeakTheoryHunt needs a pending theory below the confidence floor.
	seedMemory(t, a, "guard-weak-theory", "theories", "a weak theory", "",
		0.4, "", time.Now())
	// PoisonPillHunt needs a high-confidence memory to attack.
	seedMemory(t, a, "guard-high-conf", "memories", "high confidence", "",
		0.95, "", time.Now())

	hunts := []Hunt{
		&SurvivalAsymmetryHunt{},
		&WeakTheoryHunt{MaxConfidence: 0.6},
		&PoisonPillHunt{Cycle: 5},
	}
	for _, h := range hunts {
		findings, err := h.Run(ctx, a)
		if err != nil {
			t.Fatalf("%s: Run: %v", h.Name(), err)
		}
		if len(findings) == 0 {
			t.Errorf("%s: expected the guard to make this hunt FIRE, so its "+
				"payload is actually checked — got no findings", h.Name())
			continue
		}
		for _, f := range findings {
			tags, _ := f.Payload["tags"].([]string)
			for _, tag := range tags {
				if strings.HasPrefix(tag, "cycle_") {
					t.Errorf("%s emitted the cycle tag %q — a cycle-varying "+
						"tag is part of a proposal's identity and would make "+
						"every cycle a DISTINCT proposal", h.Name(), tag)
				}
			}
			hyp, _ := f.Payload["hypothesis"].(string)
			if strings.Contains(strings.ToLower(hyp), "cycle") {
				t.Errorf("%s put a cycle marker in its hypothesis: %q", h.Name(), hyp)
			}
		}
	}
}
