// contradiction_verification_test.go — regression tests for issue #10.
//
// The defect these pin: `mpm ops resolve-contradictions --apply` reached
// the slash path for UNVERIFIED similarity candidates. `detected_by` was
// selected by LoadContradictionQueue and read by nobody, so a row written
// by `--propose` — which establishes only that two memories are RELATED,
// never that their claims conflict — was applied exactly like a human-
// established conflict. On a duplicate-heavy store that penalises
// memories that never contradicted anything.
//
// The policy pinned here:
//
//	Similarity is relatedness, not logical contradiction.
//	Similarity-generated proposals must NEVER automatically penalise.
//	Explicitly adjudicated contradictions MAY be applied.
//	Anything else — missing, unknown, legacy detected_by — fails closed.
//
// Every test here uses an isolated in-memory DatabaseManager
// (newTestDMWithShared). None of them touches an installed MPM database.

package internal

import (
	"errors"
	"strings"
	"testing"
	"unicode/utf8"
)

// memoryState is a snapshot of the columns --apply is allowed to move.
// Tests compare it before and after so "nothing was penalised" is a
// measured assertion rather than an inference from an exit code.
type memoryState struct {
	weight            float64
	retrievalPriority float64
	metadata          string
	resolvedAtNull    bool
}

func snapshotMemory(t *testing.T, dm *DatabaseManager, id string) memoryState {
	t.Helper()
	var ms memoryState
	var meta *string
	err := dm.db.QueryRow(`
		SELECT COALESCE(weight,0), COALESCE(retrieval_priority,0), metadata
		FROM shared.memories WHERE id = ?`, id).Scan(&ms.weight, &ms.retrievalPriority, &meta)
	if err != nil {
		t.Fatalf("snapshot %s: %v", id, err)
	}
	if meta != nil {
		ms.metadata = *meta
	}
	return ms
}

func queueRowIsUnresolved(t *testing.T, dm *DatabaseManager, id int64) bool {
	t.Helper()
	var resolvedAt *string
	if err := dm.db.QueryRow(`SELECT resolved_at FROM shared.contradiction_log WHERE id = ?`, id).Scan(&resolvedAt); err != nil {
		t.Fatalf("read resolved_at for row %d: %v", id, err)
	}
	return resolvedAt == nil
}

func countQueueTableRows(t *testing.T, dm *DatabaseManager, query string, args ...interface{}) int {
	t.Helper()
	var n int
	if err := dm.db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("count query failed: %v", err)
	}
	return n
}

// seedDecisivePair inserts two memories with a wide provenance gap — the
// configuration that reaches the decisive (slash) branch — and returns
// them. margin must exceed ProvenanceThreshold, which is the whole point:
// the defect is reachable on exactly the rows that would be slashed.
func seedDecisivePair(t *testing.T, dm *DatabaseManager, a, b string) {
	t.Helper()
	if _, err := dm.db.Exec(`INSERT INTO shared.memories (id, collection, content, confidence, retrieval_priority, importance, weight)
		VALUES (?, 'memories', 'strong claim', 0.95, 0.9, 0.9, 5)`, a); err != nil {
		t.Fatalf("seed %s: %v", a, err)
	}
	if _, err := dm.db.Exec(`INSERT INTO shared.memories (id, collection, content, confidence, retrieval_priority, importance, weight)
		VALUES (?, 'memories', 'weak claim', 0.10, 0.2, 0.2, 5)`, b); err != nil {
		t.Fatalf("seed %s: %v", b, err)
	}
}

func enqueueWithSource(t *testing.T, dm *DatabaseManager, a, b, detectedBy string) int64 {
	t.Helper()
	res, err := dm.db.Exec(`INSERT INTO shared.contradiction_log
		(memory_id_a, memory_id_b, evidence, similarity, detected_by)
		VALUES (?, ?, 'unverified similarity candidate (cosine=0.91)', 0.91, ?)`, a, b, detectedBy)
	if err != nil {
		t.Fatalf("enqueue %s/%s: %v", a, b, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("last insert id: %v", err)
	}
	return id
}

func rowFor(t *testing.T, dm *DatabaseManager, id int64) map[string]interface{} {
	t.Helper()
	queue, err := dm.LoadContradictionQueue(100)
	if err != nil {
		t.Fatalf("load queue: %v", err)
	}
	for _, row := range queue {
		if row["id"] == queueIDToStr(int(id)) {
			return row
		}
	}
	t.Fatalf("row %d not found in queue", id)
	return nil
}

// TestApply_UnverifiedSimilarityCandidate_DoesNotPenalise is the direct
// reproduction of issue #10. The row is exactly what `--propose` writes,
// the pair has a decisive margin, and --apply is set. Nothing about the
// memory may move.
func TestApply_UnverifiedSimilarityCandidate_DoesNotPenalise(t *testing.T) {
	dm := newTestDMWithShared(t)
	seedDecisivePair(t, dm, "mem-a", "mem-b")

	id := enqueueWithSource(t, dm, "mem-a", "mem-b", similarityCandidateSource)
	row := rowFor(t, dm, id)

	before := snapshotMemory(t, dm, "mem-b")

	dec, err := dm.ResolveOneContradiction(row, true)

	// The refusal must be explicit, not silent.
	if err == nil {
		t.Fatalf("apply of an unverified similarity candidate must be refused; got nil error (decision=%+v)", dec)
	}
	var unv *UnverifiedContradictionError
	if !errors.As(err, &unv) {
		t.Fatalf("error must be *UnverifiedContradictionError so a caller can classify it; got %T: %v", err, err)
	}
	if unv.QueueRowID != queueIDToStr(int(id)) {
		t.Errorf("error must name the queue row, got %q", unv.QueueRowID)
	}
	if unv.Reason == "" {
		t.Error("error must explain why the row was refused, got empty reason")
	}

	// And the durable state must be untouched.
	after := snapshotMemory(t, dm, "mem-b")
	if after.weight != before.weight {
		t.Errorf("loser weight moved: %.4f -> %.4f", before.weight, after.weight)
	}
	if after.retrievalPriority != before.retrievalPriority {
		t.Errorf("loser retrieval_priority moved: %.4f -> %.4f", before.retrievalPriority, after.retrievalPriority)
	}
	if after.metadata != before.metadata {
		t.Errorf("loser metadata mutated:\n before: %s\n after:  %s", before.metadata, after.metadata)
	}
	if !queueRowIsUnresolved(t, dm, id) {
		t.Error("refused row must stay in the queue, not be silently discarded")
	}
	if n := countQueueTableRows(t, dm, `SELECT COUNT(*) FROM shared.memories WHERE collection='resolutions'`); n != 0 {
		t.Errorf("no resolution memory should be written for a refused row, got %d", n)
	}
	if n := countQueueTableRows(t, dm, `SELECT COUNT(*) FROM shared.memories WHERE collection='theories'`); n != 0 {
		t.Errorf("no arbitration theory should be written for a refused row, got %d", n)
	}
}

// TestApply_AdjudicatedContradiction_Applies is the other half: the fix
// must not break the path that legitimately exists. An operator who has
// adjudicated the pair gets the full resolution behaviour.
func TestApply_AdjudicatedContradiction_Applies(t *testing.T) {
	dm := newTestDMWithShared(t)
	seedDecisivePair(t, dm, "mem-a", "mem-b")

	id := enqueueWithSource(t, dm, "mem-a", "mem-b", similarityCandidateSource)
	if err := dm.AdjudicateContradiction(id, "mem-a asserts the rule, mem-b asserts its negation"); err != nil {
		t.Fatalf("adjudicate: %v", err)
	}

	row := rowFor(t, dm, id)
	before := snapshotMemory(t, dm, "mem-b")

	dec, err := dm.ResolveOneContradiction(row, true)
	if err != nil {
		t.Fatalf("an adjudicated contradiction must apply; got %v", err)
	}
	if !dec.Adjudicated {
		t.Error("decision must report Adjudicated=true for an adjudicated row")
	}
	if dec.LoserID != "mem-b" {
		t.Errorf("expected loser mem-b, got %q", dec.LoserID)
	}
	if queueRowIsUnresolved(t, dm, id) {
		t.Error("applied row must be marked resolved")
	}
	after := snapshotMemory(t, dm, "mem-b")
	if after.weight >= before.weight {
		t.Errorf("loser weight should be reduced on an adjudicated resolution: %.4f -> %.4f", before.weight, after.weight)
	}
	if n := countQueueTableRows(t, dm, `SELECT COUNT(*) FROM shared.memories WHERE collection='resolutions'`); n != 1 {
		t.Errorf("expected 1 resolution memory, got %d", n)
	}
}

// TestApply_MixedQueue_DoesNotPromoteCandidates covers the realistic
// batch: one adjudicated conflict and one unverified candidate in the
// same run. The candidate must not ride along.
func TestApply_MixedQueue_DoesNotPromoteCandidates(t *testing.T) {
	dm := newTestDMWithShared(t)
	seedDecisivePair(t, dm, "mem-a", "mem-b")
	seedDecisivePair(t, dm, "mem-c", "mem-d")

	adjudicatedID := enqueueWithSource(t, dm, "mem-a", "mem-b", similarityCandidateSource)
	candidateID := enqueueWithSource(t, dm, "mem-c", "mem-d", similarityCandidateSource)
	if err := dm.AdjudicateContradiction(adjudicatedID, "operator confirmed the negation"); err != nil {
		t.Fatalf("adjudicate: %v", err)
	}

	candidateBefore := snapshotMemory(t, dm, "mem-d")
	adjudicatedBefore := snapshotMemory(t, dm, "mem-b")

	applied, refused := 0, 0
	queue, err := dm.LoadContradictionQueue(100)
	if err != nil {
		t.Fatalf("load queue: %v", err)
	}
	for _, row := range queue {
		if _, err := dm.ResolveOneContradiction(row, true); err != nil {
			var unv *UnverifiedContradictionError
			if !errors.As(err, &unv) {
				t.Fatalf("unexpected error for row %v: %v", row["id"], err)
			}
			refused++
			continue
		}
		applied++
	}

	if applied != 1 || refused != 1 {
		t.Fatalf("expected exactly 1 applied / 1 refused, got %d / %d", applied, refused)
	}

	if got := snapshotMemory(t, dm, "mem-d"); got.weight != candidateBefore.weight ||
		got.retrievalPriority != candidateBefore.retrievalPriority || got.metadata != candidateBefore.metadata {
		t.Errorf("unverified candidate was penalised:\n before: %+v\n after:  %+v", candidateBefore, got)
	}
	if !queueRowIsUnresolved(t, dm, candidateID) {
		t.Error("unverified candidate row must remain unresolved")
	}
	if got := snapshotMemory(t, dm, "mem-b"); got.weight >= adjudicatedBefore.weight {
		t.Errorf("adjudicated row should have been applied: %.4f -> %.4f", adjudicatedBefore.weight, got.weight)
	}
	if queueRowIsUnresolved(t, dm, adjudicatedID) {
		t.Error("adjudicated row should be resolved")
	}
}

// TestClassifyContradictionVerification_FailsClosed pins the classifier
// directly. Every value that is not an explicit operator adjudication
// must be unverified — including the shapes a legacy or hand-written row
// can carry.
func TestClassifyContradictionVerification_FailsClosed(t *testing.T) {
	cases := []struct {
		name       string
		detectedBy string
		want       bool
	}{
		{"empty string", "", false},
		{"whitespace only", "   ", false},
		{"similarity candidate", similarityCandidateSource, false},
		{"candidate that went to arbitration", similarityCandidateSource + "|theory:arbitration-1", false},
		{"bare theory linkage", "|theory:arbitration-1", false},
		{"legacy session id", "session-abc123", false},
		{"legacy agent id", "agent-7", false},
		{"unknown token", "something-else-entirely", false},
		{"near miss on the marker", "adjudicated", false},
		{"marker with no reason", "adjudicated:", false},
		{"adjudication", "adjudicated:mem-a negates mem-b", true},
		{"candidate promoted by adjudication", similarityCandidateSource + "|adjudicated:operator checked both", true},
		{"arbitration then adjudication", similarityCandidateSource + "|theory:t1|adjudicated:operator checked", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ClassifyContradictionVerification(tc.detectedBy)
			if got.Adjudicated != tc.want {
				t.Errorf("detectedBy=%q: Adjudicated=%v, want %v (reason: %q)", tc.detectedBy, got.Adjudicated, tc.want, got.Reason)
			}
			if !got.Adjudicated && got.Reason == "" {
				t.Errorf("detectedBy=%q: an unverified row must carry a reason the operator can act on", tc.detectedBy)
			}
			if got.Adjudicated && got.Reason != "" {
				t.Errorf("detectedBy=%q: verified row should carry no refusal reason, got %q", tc.detectedBy, got.Reason)
			}
		})
	}
}

// TestApply_RepeatedRuns_IntroduceNoExtraMutations: running the whole
// queue twice must converge. The first run refuses the candidate; the
// second must refuse it identically and apply nothing new.
func TestApply_RepeatedRuns_IntroduceNoExtraMutations(t *testing.T) {
	dm := newTestDMWithShared(t)
	seedDecisivePair(t, dm, "mem-a", "mem-b")
	seedDecisivePair(t, dm, "mem-c", "mem-d")

	adjudicatedID := enqueueWithSource(t, dm, "mem-a", "mem-b", similarityCandidateSource)
	candidateID := enqueueWithSource(t, dm, "mem-c", "mem-d", similarityCandidateSource)
	if err := dm.AdjudicateContradiction(adjudicatedID, "operator confirmed the negation"); err != nil {
		t.Fatalf("adjudicate: %v", err)
	}

	runOnce := func() (applied, refused int) {
		queue, err := dm.LoadContradictionQueue(100)
		if err != nil {
			t.Fatalf("load queue: %v", err)
		}
		for _, row := range queue {
			if _, err := dm.ResolveOneContradiction(row, true); err != nil {
				refused++
				continue
			}
			applied++
		}
		return applied, refused
	}

	if applied, refused := runOnce(); applied != 1 || refused != 1 {
		t.Fatalf("first run: expected 1 applied / 1 refused, got %d / %d", applied, refused)
	}
	afterFirstA := snapshotMemory(t, dm, "mem-b")
	afterFirstC := snapshotMemory(t, dm, "mem-d")
	resolutionsAfterFirst := countQueueTableRows(t, dm, `SELECT COUNT(*) FROM shared.memories WHERE collection='resolutions'`)
	theoriesAfterFirst := countQueueTableRows(t, dm, `SELECT COUNT(*) FROM shared.memories WHERE collection='theories'`)

	if applied, refused := runOnce(); applied != 0 || refused != 1 {
		t.Fatalf("second run: expected 0 applied / 1 refused, got %d / %d", applied, refused)
	}
	if got := snapshotMemory(t, dm, "mem-b"); got != afterFirstA {
		t.Errorf("adjudicated loser moved on the second run:\n first: %+v\n second: %+v", afterFirstA, got)
	}
	if got := snapshotMemory(t, dm, "mem-d"); got != afterFirstC {
		t.Errorf("candidate loser moved on the second run:\n first: %+v\n second: %+v", afterFirstC, got)
	}
	if n := countQueueTableRows(t, dm, `SELECT COUNT(*) FROM shared.memories WHERE collection='resolutions'`); n != resolutionsAfterFirst {
		t.Errorf("resolution memories grew on the second run: %d -> %d", resolutionsAfterFirst, n)
	}
	if n := countQueueTableRows(t, dm, `SELECT COUNT(*) FROM shared.memories WHERE collection='theories'`); n != theoriesAfterFirst {
		t.Errorf("arbitration theories grew on the second run: %d -> %d", theoriesAfterFirst, n)
	}
	if !queueRowIsUnresolved(t, dm, candidateID) {
		t.Error("refused candidate must still be in the queue after repeated runs")
	}
}

// TestAdjudicateContradiction_RequiresReasonAndTargetsOneRow: the
// mechanism has to be deliberate. A blank reason, or a row that does not
// exist, must be an error rather than a silent no-op.
func TestAdjudicateContradiction_RequiresReasonAndTargetsOneRow(t *testing.T) {
	dm := newTestDMWithShared(t)
	seedDecisivePair(t, dm, "mem-a", "mem-b")
	id := enqueueWithSource(t, dm, "mem-a", "mem-b", similarityCandidateSource)

	if err := dm.AdjudicateContradiction(id, "   "); err == nil {
		t.Error("blank adjudication reason must be rejected")
	}
	if err := dm.AdjudicateContradiction(99999, "no such row"); err == nil {
		t.Error("adjudicating a row that does not exist must be an error, not a silent no-op")
	}
	if got := ClassifyContradictionVerification(mustDetectedBy(t, dm, id)); got.Adjudicated {
		t.Error("a rejected adjudication must not have marked the row verified")
	}

	if err := dm.AdjudicateContradiction(id, "operator read both memories"); err != nil {
		t.Fatalf("adjudicate: %v", err)
	}
	if got := ClassifyContradictionVerification(mustDetectedBy(t, dm, id)); !got.Adjudicated {
		t.Error("a successful adjudication must mark the row verified")
	}
	// Adjudication must be additive, not destructive: the detection
	// provenance stays legible.
	if got := mustDetectedBy(t, dm, id); !textContains(got, similarityCandidateSource) {
		t.Errorf("adjudication must not erase detection provenance, detected_by is now %q", got)
	}

	// Adjudicating twice must not double-apply.
	if err := dm.AdjudicateContradiction(id, "operator read both memories again"); err != nil {
		t.Fatalf("re-adjudicate: %v", err)
	}
	if n := countQueueTableRows(t, dm, `SELECT COUNT(*) FROM shared.contradiction_log WHERE id = ? AND detected_by LIKE '%adjudicated:%'`, id); n != 1 {
		t.Errorf("expected exactly 1 adjudication segment after 2 calls, got %d", n)
	}
}

// TestAdjudicateContradiction_RefusesResolvedRow keeps adjudication
// pointed at live work. Once a row is resolved, re-adjudicating it is a
// mistake worth reporting.
func TestAdjudicateContradiction_RefusesResolvedRow(t *testing.T) {
	dm := newTestDMWithShared(t)
	seedDecisivePair(t, dm, "mem-a", "mem-b")
	id := enqueueWithSource(t, dm, "mem-a", "mem-b", similarityCandidateSource)
	if _, err := dm.db.Exec(`UPDATE shared.contradiction_log SET resolved_at = CURRENT_TIMESTAMP WHERE id = ?`, id); err != nil {
		t.Fatalf("resolve row: %v", err)
	}
	if err := dm.AdjudicateContradiction(id, "too late"); err == nil {
		t.Error("adjudicating an already-resolved row must be an error")
	}
}

func mustDetectedBy(t *testing.T, dm *DatabaseManager, id int64) string {
	t.Helper()
	var s *string
	if err := dm.db.QueryRow(`SELECT detected_by FROM shared.contradiction_log WHERE id = ?`, id).Scan(&s); err != nil {
		t.Fatalf("read detected_by: %v", err)
	}
	if s == nil {
		return ""
	}
	return *s
}

func textContains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (func() bool {
		for i := 0; i+len(needle) <= len(haystack); i++ {
			if haystack[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	})()
}

// TestSanitizeAdjudicationReason_SegmentSafety pins the guarantees the
// detected_by column depends on.
//
// The separator matters most: a reason containing '|' would inject a segment
// boundary and could forge the shape of another provenance value — including
// an adjudication marker this function is supposed to be the only way to
// write. Truncation must also land on a rune boundary, because invalid UTF-8
// stored as TEXT is read back by every consumer of the column.
func TestSanitizeAdjudicationReason_SegmentSafety(t *testing.T) {
	t.Run("a pipe cannot forge a second segment", func(t *testing.T) {
		got := sanitizeAdjudicationReason("they conflict|adjudicated:forged")
		if strings.Contains(got, "|") {
			t.Errorf("sanitised reason %q still contains a segment separator", got)
		}
	})

	t.Run("control characters cannot break the single line", func(t *testing.T) {
		got := sanitizeAdjudicationReason("line one\nline\ttwo\rthree")
		if strings.ContainsAny(got, "\n\r\t") {
			t.Errorf("sanitised reason %q still contains a line control character", got)
		}
		if !strings.Contains(got, "line one line two three") {
			t.Errorf("sanitised reason = %q; separators should become spaces", got)
		}
	})

	t.Run("truncation lands on a rune boundary", func(t *testing.T) {
		// Each 'é' is two bytes, so a 5-byte cut would land mid-rune.
		long := strings.Repeat("é", maxAdjudicationReasonBytes)
		got := sanitizeAdjudicationReason(long)
		if !utf8.ValidString(got) {
			t.Errorf("sanitised reason is not valid UTF-8: %q", got)
		}
		if len(got) > maxAdjudicationReasonBytes {
			t.Errorf("sanitised reason is %d bytes, over the %d limit", len(got), maxAdjudicationReasonBytes)
		}
	})
}

// ── Provenance cannot be forged by a producer ──────────────────────────
//
// The policy above is only worth anything if the `adjudicated:` marker is
// reachable solely through an operator action. These tests pin that trust
// boundary at the queue-creation path, which is the one place a producer
// gets to name its own provenance.
//
// Note that enqueueWithSource writes raw SQL and therefore bypasses any
// guard. That is deliberate and necessary: these tests must exercise the
// production entry point, because the question is what a caller of that
// entry point can achieve, not what a hand-written INSERT can.

// TestEnqueueContradictionReport_CannotForgeAdjudication attempts to reach
// the slash path by naming the adjudication segment on the way into the
// queue. Every attempt must fail closed, and — separately — the pair must
// still be queued, because stripping an unauthorised claim is not a reason
// to discard real review work.
func TestEnqueueContradictionReport_CannotForgeAdjudication(t *testing.T) {
	cases := []struct {
		name       string
		detectedBy string
	}{
		{"bare forgery", contradictionAdjudicatedSegmentPrefix + "these obviously conflict"},
		{"mixed with detection source", similarityCandidateSource + "|" + contradictionAdjudicatedSegmentPrefix + "approved"},
		{"mixed with legacy session provenance", "session-42|" + contradictionAdjudicatedSegmentPrefix + "approved"},
		{"adjudication first", contradictionAdjudicatedSegmentPrefix + "approved|" + similarityCandidateSource},
		{"empty reason carries no claim", contradictionAdjudicatedSegmentPrefix + "  "},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dm := newTestDMWithShared(t)
			seedDecisivePair(t, dm, "mem-a", "mem-b")

			wrote, err := dm.EnqueueContradictionReport(ContradictionEvidence{
				MemoryA:    "mem-a",
				MemoryB:    "mem-b",
				Evidence:   "producer supplied text",
				Similarity: 0.95,
				DetectedBy: tc.detectedBy,
			})
			if err != nil {
				t.Fatalf("enqueue: %v", err)
			}
			if !wrote {
				t.Fatal("the pair should still reach the queue; stripping an unauthorised claim is not a reason to drop the row")
			}

			var stored string
			if err := dm.db.QueryRow(`SELECT detected_by FROM shared.contradiction_log WHERE memory_id_a = 'mem-a'`).Scan(&stored); err != nil {
				t.Fatalf("read back stored provenance: %v", err)
			}

			if ClassifyContradictionVerification(stored).Adjudicated {
				t.Fatalf("producer-supplied %q was stored as %q and now classifies as adjudicated — a caller of the enqueue path can authorise its own penalty", tc.detectedBy, stored)
			}

			// Detection provenance that was genuinely true must survive the
			// strip: the row should become unverified, not nonsense.
			if tc.name == "mixed with detection source" && strings.TrimSpace(stored) != similarityCandidateSource {
				t.Errorf("true detection provenance was lost: stored %q, want %q", stored, similarityCandidateSource)
			}
		})
	}
}

// TestApply_ProducerForgedRow_DoesNotPenalise is the end-to-end consequence
// of the bypass attempt above. A producer that names its own adjudication
// must still not reach the slash path, and must still be refused in the
// typed way the CLI counts and explains.
func TestApply_ProducerForgedRow_DoesNotPenalise(t *testing.T) {
	dm := newTestDMWithShared(t)
	seedDecisivePair(t, dm, "mem-a", "mem-b")

	if _, err := dm.EnqueueContradictionReport(ContradictionEvidence{
		MemoryA:    "mem-a",
		MemoryB:    "mem-b",
		Evidence:   "producer supplied text",
		Similarity: 0.95,
		DetectedBy: similarityCandidateSource + "|" + contradictionAdjudicatedSegmentPrefix + "trust me",
	}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	var id int64
	if err := dm.db.QueryRow(`SELECT id FROM shared.contradiction_log WHERE memory_id_a = 'mem-a'`).Scan(&id); err != nil {
		t.Fatalf("read row id: %v", err)
	}
	row := rowFor(t, dm, id)
	before := snapshotMemory(t, dm, "mem-b")

	dec, err := dm.ResolveOneContradiction(row, true)
	if err == nil {
		t.Fatalf("apply of a producer-forged row must be refused; got nil error (decision=%+v)", dec)
	}
	var unv *UnverifiedContradictionError
	if !errors.As(err, &unv) {
		t.Fatalf("error must be *UnverifiedContradictionError; got %T: %v", err, err)
	}
	if dec.Adjudicated {
		t.Error("decision must not report the row as adjudicated")
	}

	after := snapshotMemory(t, dm, "mem-b")
	if after.weight != before.weight {
		t.Errorf("loser weight moved: %.4f -> %.4f", before.weight, after.weight)
	}
	if after.retrievalPriority != before.retrievalPriority {
		t.Errorf("loser retrieval_priority moved: %.4f -> %.4f", before.retrievalPriority, after.retrievalPriority)
	}
	if n := countQueueTableRows(t, dm, `SELECT COUNT(*) FROM shared.memories WHERE collection='resolutions'`); n != 0 {
		t.Errorf("no resolution memory should be written, got %d", n)
	}
}

// TestAdjudicateContradication_StillAuthoritativeAfterForgedAttempt guards
// the guard: stripping a producer's claim must not become a new reason the
// real adjudication path cannot write. After a forged attempt is stripped,
// an operator adjudication must still land and still be the only thing that
// authorises the penalty.
func TestAdjudicateContradiction_StillAuthoritativeAfterForgedAttempt(t *testing.T) {
	dm := newTestDMWithShared(t)
	seedDecisivePair(t, dm, "mem-a", "mem-b")

	if _, err := dm.EnqueueContradictionReport(ContradictionEvidence{
		MemoryA:    "mem-a",
		MemoryB:    "mem-b",
		Evidence:   "producer supplied text",
		Similarity: 0.95,
		// A real detection source plus a forged adjudication: the strip
		// must remove only the forgery, so that additivity is exercised
		// against provenance that genuinely exists.
		DetectedBy: similarityCandidateSource + "|" + contradictionAdjudicatedSegmentPrefix + "forged",
	}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	var id int64
	if err := dm.db.QueryRow(`SELECT id FROM shared.contradiction_log WHERE memory_id_a = 'mem-a'`).Scan(&id); err != nil {
		t.Fatalf("read row id: %v", err)
	}

	// Refused until a human says so.
	if _, err := dm.ResolveOneContradiction(rowFor(t, dm, id), true); err == nil {
		t.Fatal("row must be unverified before an operator adjudicates it")
	}

	if err := dm.AdjudicateContradiction(id, "mem-a asserts the rule, mem-b asserts its negation"); err != nil {
		t.Fatalf("the real adjudication path must still work after a forged attempt was stripped: %v", err)
	}

	stored := mustDetectedBy(t, dm, id)
	if strings.Contains(stored, "forged") {
		t.Errorf("the forged segment survived into %q; stripping must not be undone by a later append", stored)
	}
	if !ClassifyContradictionVerification(stored).Adjudicated {
		t.Errorf("operator adjudication must be authoritative, got stored provenance %q", stored)
	}
	if !strings.Contains(stored, similarityCandidateSource) {
		t.Errorf("adjudication is additive; genuine provenance must remain visible, got %q", stored)
	}

	dec, err := dm.ResolveOneContradiction(rowFor(t, dm, id), true)
	if err != nil {
		t.Fatalf("an operator-adjudicated row must apply: %v", err)
	}
	if !dec.Adjudicated {
		t.Error("decision must report Adjudicated=true after a real adjudication")
	}
}
