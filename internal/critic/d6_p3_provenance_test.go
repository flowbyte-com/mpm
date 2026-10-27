// d6_p3_provenance_test.go — D6 regression coverage for the missed-
// opportunity hunt (requirement 8).
//
// THE DEFECT D6 FIXED: the hunt could not tell "this session recorded
// nothing" from "this session recorded eight decisions that cannot be
// joined back to their calls", because the decision's
// artifact_provenance row carried no invocation id. The hunt therefore
// needed a second, independent signal — the recording-call vocabulary —
// to avoid reporting sessions that had in fact recorded knowledge.
//
// THE CORRECTION: the join signal works now. An artifact produced by a
// tool invocation joins to that invocation, so the hunt can see the
// recording without consulting the vocabulary.
//
// The recording-call vocabulary is NOT removed. It still covers the
// recording actions that write no artifact_provenance row at all
// (mpm_references.add/import, mpm_topics.create, mpm_evidence.add) and
// it is what protects a session whose recording happened on a path the
// vocabulary does not name. These tests pin BOTH halves.
package critic

import (
	"context"
	"fmt"
	"strings"
	"testing"

	mpmcore "github.com/flowbyte-com/mpm-core"
)

// d6FindingMentions reports whether any finding names the given session.
//
// The hunt's Finding carries the session id inside its Reason and its
// proposal payload (there is no dedicated session field), so the check is
// a containment test over both.
func d6FindingMentions(findings []Finding, sessionID string) bool {
	for _, f := range findings {
		if strings.Contains(f.Reason, sessionID) {
			return true
		}
		for _, v := range f.Payload {
			if s, ok := v.(string); ok && strings.Contains(s, sessionID) {
				return true
			}
		}
	}
	return false
}

// TestD6_MissedOpportunity_SessionThatRecordedIsNotFlagged pins
// requirement 8: a session that recorded knowledge is not flagged.
//
// The isolation matters. `mpm_decisions.supersede` is a REGISTERED
// recording action — it writes a new decision row — but it is NOT in the
// hunt's recording-call vocabulary, which names only `mpm_decisions.record`.
// So the recording-call signal does NOT fire for it. Before D6 the session
// below WAS reported, because the superseding decision's provenance row
// carried no invocation id and the join found nothing.
//
// After D6 the join finds it, and the session is not reported. That is the
// join signal working on its own.
func TestD6_MissedOpportunity_SessionThatRecordedIsNotFlagged(t *testing.T) {
	a, dm := p3RealAudit(t)
	ctx := context.Background()

	// Six read-only invocations — enough activity to be a candidate.
	for i := 0; i < 6; i++ {
		p3SeedRealInvocation(t, dm, fmt.Sprintf("inv-d6-read-%02d", i),
			"mpm_references", "search", "ses_d6_join", 1793000000+int64(i))
	}

	// The recording invocation. Its tool.action is deliberately OUTSIDE the
	// recording-call vocabulary.
	//
	// p3SeedRealInvocation derives the invocation_id as "inv_" + id, so the
	// id passed here is the suffix, not the full invocation id.
	p3SeedRealInvocation(t, dm, "d6_supersede", "mpm_decisions", "supersede",
		"ses_d6_join", 1793000100)

	// The decision itself, written through the PRODUCTION path with the
	// invocation's identity.
	ac := mpmcore.ActiveContext{
		SessionID:    "ses_d6_join",
		InvocationID: "inv_d6_supersede",
	}
	dec, err := dm.RecordDecision("ctx", "a superseded choice", "why", "", nil, nil, ac)
	if err != nil {
		t.Fatalf("RecordDecision: %v", err)
	}
	decID, _ := dec["id"].(string)
	if decID == "" {
		t.Fatalf("no decision id: %+v", dec)
	}

	// The provenance row must join to the invocation — this is the whole
	// mechanism the hunt's second signal depends on.
	var joined int
	if err := dm.SQLDB().QueryRow(`
		SELECT COUNT(*)
		FROM artifact_provenance ap
		JOIN tool_invocations ti ON ti.invocation_id = ap.invocation_id
		WHERE ap.artifact_id = ?`, decID).Scan(&joined); err != nil {
		t.Fatalf("join provenance to invocation: %v", err)
	}
	if joined != 1 {
		t.Fatalf("the decision's provenance joined to %d invocation rows, want 1", joined)
	}

	findings, err := (&MissedOpportunityHunt{}).Run(ctx, a)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if d6FindingMentions(findings, "ses_d6_join") {
		t.Errorf("the session that recorded a decision was reported as a "+
			"missed opportunity: %+v", findings)
	}
}

// TestD6_MissedOpportunity_SessionThatRecordedNothingIsStillFlagged is the
// control for the test above.
//
// The correction must not weaken the hunt. A session that did MPM-mediated
// work and recorded NOTHING must still be reported.
func TestD6_MissedOpportunity_SessionThatRecordedNothingIsStillFlagged(t *testing.T) {
	a, dm := p3RealAudit(t)
	ctx := context.Background()

	for i := 0; i < 6; i++ {
		p3SeedRealInvocation(t, dm, fmt.Sprintf("inv-d6-empty-%02d", i),
			"mpm_references", "search", "ses_d6_empty", 1793000000+int64(i))
	}

	findings, err := (&MissedOpportunityHunt{}).Run(ctx, a)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !d6FindingMentions(findings, "ses_d6_empty") {
		t.Errorf("a session that recorded nothing was NOT reported — the hunt " +
			"was weakened by the provenance correction")
	}
}

// TestD6_MissedOpportunity_LifecycleIngestionIsNotARecording pins the
// other half of requirement 8/9.
//
// `mpm_system event_ingest` is a LIFECYCLE OBSERVATION. It must not make
// a session look like one that preserved knowledge: it is not in the
// recording-call vocabulary, and it writes no artifact_provenance row.
//
// If D6 had widened the provenance bridge to cover the ingest path, an
// ingest-only session would have been protected by the join — which would
// be WRONG: ingesting observations is not preserving knowledge.
func TestD6_MissedOpportunity_LifecycleIngestionIsNotARecording(t *testing.T) {
	a, dm := p3RealAudit(t)
	ctx := context.Background()

	for i := 0; i < 6; i++ {
		p3SeedRealInvocation(t, dm, fmt.Sprintf("inv-d6-ingest-%02d", i),
			"mpm_system", "event_ingest", "ses_d6_ingest", 1793000000+int64(i))
	}

	// The ingest writes no artifact_provenance row.
	var provRows int
	if err := dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM artifact_provenance`).Scan(&provRows); err != nil {
		t.Fatalf("count artifact_provenance: %v", err)
	}
	if provRows != 0 {
		t.Errorf("artifact_provenance rows = %d, want 0 — the lifecycle ingest "+
			"must not write a provenance row", provRows)
	}

	findings, err := (&MissedOpportunityHunt{}).Run(ctx, a)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !d6FindingMentions(findings, "ses_d6_ingest") {
		t.Errorf("an ingest-only session was NOT reported — lifecycle ingestion " +
			"must not masquerade as knowledge preservation")
	}
}
