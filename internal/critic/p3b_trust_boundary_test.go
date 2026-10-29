package critic

// p3b_trust_boundary_test.go — the critic's trust-boundary transition (P3b).
//
// WHAT P3b CHANGED.
//
// The critic used to write TRUSTED LESSONS. `mpm_lessons save` lands in
// lessons_base, is indexed in lessons_fts, is served by the wake-context
// payload and is returned by retrieval — with a full confidence value and
// no review step, because lessons_base has no status column. Two hunts did
// that on every cycle: SurvivalAsymmetryHunt and WeakTheoryHunt.
//
// P3b moves both to `mpm_theories propose`: the only existing UNTRUSTED
// proposal surface (adjudication §6c.1), which writes
// `collection='theories'` with `metadata.status='pending'` and becomes
// terminal only through an explicit resolve. No schema change; no pending
// state is added to lessons_base.
//
// THE SEVEN PROPERTIES THIS SUITE PROVES.
//
//  1. Both hunts emit PENDING THEORIES, not trusted lessons.
//  2. Their typed citations resolve to the correct source artifacts.
//  3. An invalid citation REJECTS the proposal atomically — no partial
//     proposal record survives.
//  4. Repeating an identical hunt does NOT generate another proposal.
//  5. A changed underlying observation CAN generate a genuinely distinct
//     proposal.
//  6. A pending proposal is explicitly marked pending when it is surfaced
//     in the wake context.
//  7. The hunts' existing DETECTION behaviour is intact.
//
// Plus the structural guard: NO hunt in the critic's default repertoire
// targets the trusted lesson surface.
//
// THE PROMOTED CONTRACT.
//
// TestP3b_CriticFindingIsNotTrustedWithoutReview in this file is the
// PROMOTED form of the M5 contract test
// TestM5Contract_CriticFindingIsNotTrustedWithoutReview, which lived in
// internal/critic/m5_trust_contract_test.go behind the `m5contract` build
// tag and FAILED against production behaviour. The defect it covers —
// defect D2 — is fixed, so the contract now passes, and a passing
// contract belongs in ORDINARY CI COVERAGE rather than in a file that no
// green gate compiles.
//
// ISOLATION.
//
// Every test pins HOME, MPM_WORKSPACE and MPM_DB_PATH to disposable
// locations and opens a REAL core DatabaseManager at the canonical
// <workspace>/src/db/mpm.db — the path MPM_DB_PATH names — so the
// production path resolution and the production schema are exercised
// rather than bypassed.

import (
	"context"
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

// p3bRealAudit builds an Audit over a REAL core database in a disposable
// workspace, with the critic's DEFAULT hunt repertoire.
func p3bRealAudit(t *testing.T) (*Audit, *mpmcore.DatabaseManager) {
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
	a := &Audit{
		db:    dm.SQLDB(),
		log:   log,
		cycle: 0,
		cli:   &fakeCLI{},
		hunts: []Hunt{
			&SurvivalAsymmetryHunt{},
			&StaleMemoryHunt{MaxAge: 30 * 24 * time.Hour},
			&WeakTheoryHunt{MaxConfidence: 0.6},
			&MissedOpportunityHunt{},
		},
	}
	return a, dm
}

// p3bSeedMemory inserts a memories row directly, with full control over
// the provenance `source` label, the confidence and the deletion state.
//
// The direct INSERT is deliberate: the hunts read `metadata.source`, and
// the production save layer stamps that label from its own provenance
// plumbing. Seeding through the save path would couple these tests to
// that plumbing; seeding directly pins the SUBJECT of the hunt.
func p3bSeedMemory(t *testing.T, dm *mpmcore.DatabaseManager, id, collection, content, source string, confidence float64, deletedAt interface{}, createdAt int64) {
	t.Helper()
	metadata := "{}"
	if source != "" {
		metadata = fmt.Sprintf(`{"source":%q}`, source)
	}
	if _, err := dm.SQLDB().Exec(`
		INSERT INTO memories (id, collection, content, metadata, confidence, deleted_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		id, collection, content, metadata, confidence, deletedAt, createdAt); err != nil {
		t.Fatalf("seed memory %s: %v", id, err)
	}
}

// p3bSeedCohort seeds a provenance cohort: n rows, the first alive rows
// live and the rest soft-deleted.
func p3bSeedCohort(t *testing.T, dm *mpmcore.DatabaseManager, source string, n, alive int, startID int64, startAt int64) {
	t.Helper()
	for i := 0; i < n; i++ {
		var deleted interface{}
		if i >= alive {
			deleted = int64(1790000000)
		}
		p3bSeedMemory(t, dm, fmt.Sprintf("%s-%03d", source, i), "memories",
			fmt.Sprintf("%s sourced memory %d", source, i), source, 0.8,
			deleted, startAt+int64(i))
	}
}

// p3bApplyProposal applies a finding's payload through the PRODUCTION
// dispatch path — the registered `mpm_theories` handler — and returns the
// result.
//
// The payload goes through a JSON round-trip first, because that is what
// production does: the critic marshals the finding to JSON and `mpm call`
// decodes it, so a Go-native []string arrives at the handler as a
// []interface{}.
//
// (Without the round-trip these tests would pass for the wrong reason:
// internal.ParseStringSliceOr silently returns nil for a Go-native
// []string, so the proposal would carry NO citations at all and would
// still "succeed". That silent drop is tracked separately as a defect and
// is deliberately NOT fixed here.)
func p3bApplyProposal(t *testing.T, dm *mpmcore.DatabaseManager, payload map[string]interface{}) (map[string]interface{}, error) {
	t.Helper()
	tool, ok := tools.ByName("mpm_theories")
	if !ok {
		t.Fatal("mpm_theories is not in the registry")
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	var params map[string]interface{}
	if err := json.Unmarshal(raw, &params); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	res, err := tool.Handler(dm, mpmcore.ActiveContext{}, map[string]interface{}{
		"action": "propose",
		"params": params,
	})
	if err != nil {
		return nil, err
	}
	out, _ := res.(map[string]interface{})
	return out, nil
}

// p3bReadWakeContext reads the wake context through the registered
// `mpm_context` handler.
func p3bReadWakeContext(t *testing.T, dm *mpmcore.DatabaseManager) map[string]interface{} {
	t.Helper()
	tool, ok := tools.ByName("mpm_context")
	if !ok {
		t.Fatal("mpm_context is not in the registry")
	}
	res, err := tool.Handler(dm, mpmcore.ActiveContext{}, map[string]interface{}{
		"action": "read_wake_context",
		"params": map[string]interface{}{},
	})
	if err != nil {
		t.Fatalf("read_wake_context: %v", err)
	}
	out, _ := res.(map[string]interface{})
	return out
}

// p3bCount counts the rows in a table.
func p3bCount(t *testing.T, dm *mpmcore.DatabaseManager, query string, args ...interface{}) int {
	t.Helper()
	var n int
	if err := dm.SQLDB().QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("count query %q: %v", query, err)
	}
	return n
}

// p3bPendingTheories counts the pending theory rows.
func p3bPendingTheories(t *testing.T, dm *mpmcore.DatabaseManager) int {
	t.Helper()
	return p3bCount(t, dm, `
		SELECT COUNT(*) FROM memories
		WHERE collection = 'theories'
		  AND deleted_at IS NULL
		  AND json_extract(metadata, '$.status') = 'pending'`)
}

// ── 1. pending proposals, not trusted lessons ─────────────────────────────

// TestP3b_SurvivalAsymmetry_EmitsPendingProposalNotTrustedLesson pins the
// trust boundary for the survival hunt.
//
// The finding must target the untrusted proposal surface, must carry the
// proposal's fields (a hypothesis and validation criteria — not a lesson's
// `fact` and `type`), and must cite its evidence.
//
// Applying it must produce a PENDING theory and NO lesson: a lesson is
// trusted on arrival, so writing one is defect D2.
func TestP3b_SurvivalAsymmetry_EmitsPendingProposalNotTrustedLesson(t *testing.T) {
	a, dm := p3bRealAudit(t)
	ctx := context.Background()

	p3bSeedCohort(t, dm, "call", 100, 25, 0, 1793000000)
	p3bSeedCohort(t, dm, "direct", 50, 47, 0, 1793000100)

	findings, err := (&SurvivalAsymmetryHunt{}).Run(ctx, a)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(findings))
	}
	f := findings[0]

	if f.Tool != "mpm_theories" || f.Action != "propose" {
		t.Fatalf("expected mpm_theories/propose, got %s/%s", f.Tool, f.Action)
	}
	// The lesson payload keys must be GONE. They are the shape of a
	// trusted write.
	for _, forbidden := range []string{"fact", "type"} {
		if _, present := f.Payload[forbidden]; present {
			t.Errorf("the finding still carries the lesson-shaped %q key", forbidden)
		}
	}
	// The proposal shape must be present.
	for _, required := range []string{"hypothesis", "validation_criteria", "source_ids", "source_type"} {
		if _, present := f.Payload[required]; !present {
			t.Errorf("the finding is missing the %q field", required)
		}
	}

	// Apply it through the production dispatch.
	res, err := p3bApplyProposal(t, dm, f.Payload)
	if err != nil {
		t.Fatalf("apply the proposal: %v", err)
	}
	proposalID, _ := res["id"].(string)
	if proposalID == "" {
		t.Fatalf("the proposal returned no id: %v", res)
	}

	// Exactly one PENDING theory, and it is pending.
	if n := p3bPendingTheories(t, dm); n != 1 {
		t.Errorf("expected exactly 1 pending theory, got %d", n)
	}
	var status string
	if err := dm.SQLDB().QueryRow(
		`SELECT json_extract(metadata, '$.status') FROM memories WHERE id = ?`,
		proposalID).Scan(&status); err != nil {
		t.Fatalf("read the proposal's status: %v", err)
	}
	if status != "pending" {
		t.Errorf("expected the proposal to be pending, got %q", status)
	}

	// NO lesson. Not one row, and nothing retrievable through the FTS
	// index that backs retrieval.
	if n := p3bCount(t, dm, `SELECT COUNT(*) FROM lessons_base`); n != 0 {
		t.Errorf("expected NO lesson row, got %d — the critic must not write "+
			"to the trusted lesson surface", n)
	}
	if n := p3bCount(t, dm, `SELECT COUNT(*) FROM lessons_fts`); n != 0 {
		t.Errorf("expected NO lesson_fts row, got %d", n)
	}
}

// TestP3b_WeakTheory_EmitsPendingProposalNotTrustedLesson pins the trust
// boundary for the weak-theory hunt.
func TestP3b_WeakTheory_EmitsPendingProposalNotTrustedLesson(t *testing.T) {
	a, dm := p3bRealAudit(t)
	ctx := context.Background()

	// A pending theory below the confidence floor.
	weak, err := dm.ProposeTheory(
		"A pending hypothesis the critic should notice.",
		"Demonstrate the claim.",
		nil, nil, []string{"p3b", "weak"},
	)
	if err != nil {
		t.Fatalf("ProposeTheory: %v", err)
	}
	weakID, _ := weak["id"].(string)

	findings, err := (&WeakTheoryHunt{MaxConfidence: 0.6}).Run(ctx, a)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(findings))
	}
	f := findings[0]

	if f.Tool != "mpm_theories" || f.Action != "propose" {
		t.Fatalf("expected mpm_theories/propose, got %s/%s", f.Tool, f.Action)
	}
	for _, forbidden := range []string{"fact", "type"} {
		if _, present := f.Payload[forbidden]; present {
			t.Errorf("the finding still carries the lesson-shaped %q key", forbidden)
		}
	}
	for _, required := range []string{"hypothesis", "validation_criteria", "source_ids", "source_type"} {
		if _, present := f.Payload[required]; !present {
			t.Errorf("the finding is missing the %q field", required)
		}
	}

	res, err := p3bApplyProposal(t, dm, f.Payload)
	if err != nil {
		t.Fatalf("apply the proposal: %v", err)
	}
	proposalID, _ := res["id"].(string)
	if proposalID == "" {
		t.Fatalf("the proposal returned no id: %v", res)
	}
	// TWO pending theories: the weak one the hunt observed, and the
	// proposal the hunt made about it.
	if n := p3bPendingTheories(t, dm); n != 2 {
		t.Errorf("expected 2 pending theories (the observed one + the proposal), got %d", n)
	}
	// The proposal must NOT have resolved the theory it observed.
	var observedStatus string
	if err := dm.SQLDB().QueryRow(
		`SELECT json_extract(metadata, '$.status') FROM memories WHERE id = ?`,
		weakID).Scan(&observedStatus); err != nil {
		t.Fatalf("read the observed theory's status: %v", err)
	}
	if observedStatus != "pending" {
		t.Errorf("expected the OBSERVED theory to still be pending — the critic "+
			"must not resolve it, got %q", observedStatus)
	}
	if n := p3bCount(t, dm, `SELECT COUNT(*) FROM lessons_base`); n != 0 {
		t.Errorf("expected NO lesson row, got %d", n)
	}
}

// TestP3b_CriticFindingIsNotTrustedWithoutReview is the PROMOTED M5
// contract for defect D2.
//
// It was `TestM5Contract_CriticFindingIsNotTrustedWithoutReview` in
// internal/critic/m5_trust_contract_test.go, behind the `m5contract` build
// tag, failing against production behaviour. The defect is fixed, so the
// contract passes — and a passing contract belongs in ordinary CI
// coverage.
//
// The assertion M5 makes: a critic-authored finding must not be
// retrievable as TRUSTED knowledge until it has been reviewed. It must be
// pending, or absent.
func TestP3b_CriticFindingIsNotTrustedWithoutReview(t *testing.T) {
	a, dm := p3bRealAudit(t)
	ctx := context.Background()

	if _, err := dm.ProposeTheory(
		"A pending hypothesis the critic should notice.",
		"Demonstrate the claim.",
		nil, nil, []string{"p3b", "weak"},
	); err != nil {
		t.Fatalf("ProposeTheory: %v", err)
	}

	findings, err := (&WeakTheoryHunt{MaxConfidence: 0.6}).Run(ctx, a)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(findings) == 0 {
		t.Fatal("expected the weak-theory hunt to emit a finding")
	}

	if _, err := p3bApplyProposal(t, dm, findings[0].Payload); err != nil {
		t.Fatalf("apply the finding: %v", err)
	}

	// EXPECTED: nothing the critic produced is retrievable as trusted
	// knowledge. The lesson surface must be untouched — and the FTS
	// index that backs retrieval must have no hit for the critic's text.
	const marker = "p3bcriticunreviewedmarker"
	var ftsHits int
	if err := dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM lessons_fts WHERE lessons_fts MATCH ?`, marker).
		Scan(&ftsHits); err != nil {
		t.Fatalf("query lessons_fts: %v", err)
	}
	if ftsHits != 0 {
		t.Errorf("expected a critic-authored finding to NOT be retrievable as "+
			"trusted knowledge without review (defect D2), but lessons_fts "+
			"returned %d hit(s)", ftsHits)
	}
	if n := p3bCount(t, dm, `SELECT COUNT(*) FROM lessons_base`); n != 0 {
		t.Errorf("expected NO critic-authored lesson row, got %d", n)
	}

	// And it IS present, as a PENDING proposal — the other half of the
	// contract: the finding is not lost, it is held for review.
	if n := p3bPendingTheories(t, dm); n != 2 {
		t.Errorf("expected the finding to be held as a pending proposal "+
			"(the observed theory + the proposal), got %d pending theories", n)
	}
}

// ── 2. the typed citations resolve to the right artifacts ────────────────

// TestP3b_SurvivalAsymmetry_CitationsResolveAsMemories pins that the
// survival proposal cites MEMORIES, and that every cited id resolves as a
// memory under P2's typed path.
//
// P2's 'memory' source type resolves against the memories collection, and
// only against rows that are not soft-deleted. A citation that does not
// resolve that way REJECTS the whole proposal.
func TestP3b_SurvivalAsymmetry_CitationsResolveAsMemories(t *testing.T) {
	a, dm := p3bRealAudit(t)
	ctx := context.Background()

	// Rows OUTSIDE the memories collection that carry the same provenance
	// labels, and are OLDER than the cohort. If the citation query were
	// unscoped it would cite THESE first — so this is what makes the
	// test discriminate rather than merely pass.
	p3bSeedMemory(t, dm, "project-call", "project", "a project note", "call",
		0.8, nil, 1792000000)
	p3bSeedMemory(t, dm, "project-direct", "project", "another project note",
		"direct", 0.8, nil, 1792000001)

	p3bSeedCohort(t, dm, "call", 100, 25, 0, 1793000000)
	p3bSeedCohort(t, dm, "direct", 50, 47, 0, 1793000100)

	findings, err := (&SurvivalAsymmetryHunt{}).Run(ctx, a)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(findings))
	}
	f := findings[0]

	if f.Payload["source_type"] != "memory" {
		t.Fatalf("expected source_type %q, got %v", "memory", f.Payload["source_type"])
	}
	sourceIDs, ok := f.Payload["source_ids"].([]string)
	if !ok || len(sourceIDs) == 0 {
		t.Fatalf("expected a non-empty source_ids list, got %v", f.Payload["source_ids"])
	}

	cohorts := map[string]bool{}
	for _, id := range sourceIDs {
		var collection, source string
		var deleted interface{}
		if err := dm.SQLDB().QueryRow(
			`SELECT collection, json_extract(metadata, '$.source'), deleted_at
			   FROM memories WHERE id = ?`, id).
			Scan(&collection, &source, &deleted); err != nil {
			t.Fatalf("cited id %q does not resolve to a memories row: %v", id, err)
		}
		if collection != "" && collection != "memories" {
			t.Errorf("cited id %q has collection %q — it would be REJECTED by "+
				"P2's 'memory' source type", id, collection)
		}
		if deleted != nil {
			t.Errorf("cited id %q is soft-deleted — it would be REJECTED by "+
				"P2's 'memory' source type", id)
		}
		if source != "call" && source != "direct" {
			t.Errorf("cited id %q is not in either measured cohort (source=%q)", id, source)
		}
		cohorts[source] = true
	}
	// The proposal rests on the CONTRAST between the two cohorts, so
	// both sides must be cited.
	if !cohorts["call"] || !cohorts["direct"] {
		t.Errorf("expected citations from BOTH cohorts, got %v", cohorts)
	}
}

// TestP3b_WeakTheory_CitationsResolveAsTheories pins that the weak-theory
// proposal cites THEORIES, and that the cited id is the very theory the
// hunt observed.
func TestP3b_WeakTheory_CitationsResolveAsTheories(t *testing.T) {
	a, dm := p3bRealAudit(t)
	ctx := context.Background()

	weak, err := dm.ProposeTheory(
		"A pending hypothesis the critic should notice.",
		"Demonstrate the claim.",
		nil, nil, []string{"p3b", "weak"},
	)
	if err != nil {
		t.Fatalf("ProposeTheory: %v", err)
	}
	weakID, _ := weak["id"].(string)

	findings, err := (&WeakTheoryHunt{MaxConfidence: 0.6}).Run(ctx, a)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(findings))
	}
	f := findings[0]

	if f.Payload["source_type"] != "theory" {
		t.Fatalf("expected source_type %q, got %v", "theory", f.Payload["source_type"])
	}
	sourceIDs, ok := f.Payload["source_ids"].([]string)
	if !ok || len(sourceIDs) != 1 {
		t.Fatalf("expected exactly 1 cited id, got %v", f.Payload["source_ids"])
	}
	if sourceIDs[0] != weakID {
		t.Errorf("expected the citation to be the observed theory %q, got %q",
			weakID, sourceIDs[0])
	}
	// It must resolve AS A THEORY.
	var collection string
	var deleted interface{}
	if err := dm.SQLDB().QueryRow(
		`SELECT collection, deleted_at FROM memories WHERE id = ?`, sourceIDs[0]).
		Scan(&collection, &deleted); err != nil {
		t.Fatalf("the cited id does not resolve: %v", err)
	}
	if collection != "theories" {
		t.Errorf("expected the cited id to be a theory, got collection %q", collection)
	}
	if deleted != nil {
		t.Errorf("the cited id is soft-deleted — it would be REJECTED")
	}
}

// ── 3. an invalid citation rejects the proposal atomically ───────────────

// TestP3b_InvalidCitationRejectsTheProposalAtomically pins the atomic
// rejection for both hunts.
//
// A cited id that does not resolve REJECTS the whole proposal. Because the
// proposal and its citations are written in ONE transaction, the rejection
// leaves NO partial record: no theory row, no citation row.
//
// This is the property that makes the critic safe to re-emit: a proposal
// whose evidence has gone away fails cleanly instead of landing half-written.
func TestP3b_InvalidCitationRejectsTheProposalAtomically(t *testing.T) {
	const phantom = "no-such-artifact-0000000000000000"

	for _, tc := range []struct {
		name string
		hunt func(a *Audit) ([]Finding, error)
		seed func(t *testing.T, dm *mpmcore.DatabaseManager)
	}{
		{
			name: "survival_asymmetry",
			hunt: func(a *Audit) ([]Finding, error) {
				return (&SurvivalAsymmetryHunt{}).Run(context.Background(), a)
			},
			seed: func(t *testing.T, dm *mpmcore.DatabaseManager) {
				p3bSeedCohort(t, dm, "call", 100, 25, 0, 1793000000)
				p3bSeedCohort(t, dm, "direct", 50, 47, 0, 1793000100)
			},
		},
		{
			name: "weak_theory",
			hunt: func(a *Audit) ([]Finding, error) {
				return (&WeakTheoryHunt{MaxConfidence: 0.6}).Run(context.Background(), a)
			},
			seed: func(t *testing.T, dm *mpmcore.DatabaseManager) {
				if _, err := dm.ProposeTheory(
					"A pending hypothesis the critic should notice.",
					"Demonstrate the claim.",
					nil, nil, []string{"p3b", "weak"},
				); err != nil {
					t.Fatalf("ProposeTheory: %v", err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, dm := p3bRealAudit(t)
			tc.seed(t, dm)

			findings, err := tc.hunt(a)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if len(findings) != 1 {
				t.Fatalf("expected 1 finding, got %d", len(findings))
			}

			// Corrupt the citation: one id that resolves to nothing.
			payload := map[string]interface{}{}
			for k, v := range findings[0].Payload {
				payload[k] = v
			}
			sourceIDs, _ := findings[0].Payload["source_ids"].([]string)
			corrupt := append([]string{}, sourceIDs...)
			corrupt = append(corrupt, phantom)
			payload["source_ids"] = corrupt

			baseline := p3bCount(t, dm, `SELECT COUNT(*) FROM memories WHERE collection = 'theories'`)

			_, err = p3bApplyProposal(t, dm, payload)
			if err == nil {
				t.Fatal("expected the proposal to be REJECTED because a cited " +
					"id does not resolve")
			}
			if !strings.Contains(err.Error(), "does not resolve") {
				t.Errorf("expected the error to say the citation does not resolve, got: %v", err)
			}

			// NOTHING survives: no NEW theory row, no citation row. The
			// weak_theory case seeds one theory of its own (the observed
			// one), so the baseline is not always zero.
			if n := p3bCount(t, dm, `SELECT COUNT(*) FROM memories WHERE collection = 'theories'`); n != baseline {
				t.Errorf("expected NO theory row to survive the rejection, got %d (baseline %d)", n, baseline)
			}
			if n := p3bCount(t, dm, `SELECT COUNT(*) FROM epistemic_provenance`); n != 0 {
				t.Errorf("expected NO citation row to survive the rejection, got %d", n)
			}
			if n := p3bCount(t, dm, `SELECT COUNT(*) FROM lessons_base`); n != 0 {
				t.Errorf("expected NO lesson row, got %d", n)
			}
		})
	}
}

// ── 4. idempotency across cycles ─────────────────────────────────────────

// TestP3b_RepeatingTheHuntDoesNotCreateAnotherProposal pins idempotency.
//
// The critic runs on every cycle. A hunt that re-emitted the same finding
// must land on the SAME proposal, not a new one every cycle — otherwise the
// substrate fills with indistinguishable pending theories.
//
// This is the D4 property, and it is why the cycle number lives in the
// finding's Reason and the volatile figures live there too.
func TestP3b_RepeatingTheHuntDoesNotCreateAnotherProposal(t *testing.T) {
	for _, tc := range []struct {
		name string
		hunt func(a *Audit) ([]Finding, error)
		seed func(t *testing.T, dm *mpmcore.DatabaseManager)
	}{
		{
			name: "survival_asymmetry",
			hunt: func(a *Audit) ([]Finding, error) {
				return (&SurvivalAsymmetryHunt{}).Run(context.Background(), a)
			},
			seed: func(t *testing.T, dm *mpmcore.DatabaseManager) {
				p3bSeedCohort(t, dm, "call", 100, 25, 0, 1793000000)
				p3bSeedCohort(t, dm, "direct", 50, 47, 0, 1793000100)
			},
		},
		{
			name: "weak_theory",
			hunt: func(a *Audit) ([]Finding, error) {
				return (&WeakTheoryHunt{MaxConfidence: 0.6}).Run(context.Background(), a)
			},
			seed: func(t *testing.T, dm *mpmcore.DatabaseManager) {
				if _, err := dm.ProposeTheory(
					"A pending hypothesis the critic should notice.",
					"Demonstrate the claim.",
					nil, nil, []string{"p3b", "weak"},
				); err != nil {
					t.Fatalf("ProposeTheory: %v", err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, dm := p3bRealAudit(t)
			tc.seed(t, dm)

			// Two identical cycles. The second one runs at a different
			// cycle number — which must NOT change the proposal.
			a.cycle = 4
			first, err := tc.hunt(a)
			if err != nil {
				t.Fatalf("first Run: %v", err)
			}
			a.cycle = 9
			second, err := tc.hunt(a)
			if err != nil {
				t.Fatalf("second Run: %v", err)
			}
			if len(first) != 1 || len(second) != 1 {
				t.Fatalf("expected 1 finding per run, got %d and %d", len(first), len(second))
			}
			if fmt.Sprint(first[0].Payload) != fmt.Sprint(second[0].Payload) {
				t.Errorf("expected the two cycles to produce an IDENTICAL proposal — "+
					"the cycle number must not enter an identity-bearing field.\n"+
					"cycle 4: %v\ncycle 9: %v", first[0].Payload, second[0].Payload)
			}

			res1, err := p3bApplyProposal(t, dm, first[0].Payload)
			if err != nil {
				t.Fatalf("apply the first proposal: %v", err)
			}
			res2, err := p3bApplyProposal(t, dm, second[0].Payload)
			if err != nil {
				t.Fatalf("apply the second proposal: %v", err)
			}
			id1, _ := res1["id"].(string)
			id2, _ := res2["id"].(string)
			if id1 == "" || id1 != id2 {
				t.Fatalf("expected the repeated proposal to land on the SAME id, got %q then %q", id1, id2)
			}

			// Exactly one theory (plus the observed one, for weak_theory).
			expectedTheories := 1
			if tc.name == "weak_theory" {
				expectedTheories = 2
			}
			if n := p3bCount(t, dm, `SELECT COUNT(*) FROM memories WHERE collection = 'theories'`); n != expectedTheories {
				t.Errorf("expected %d theory rows after two identical cycles, got %d",
					expectedTheories, n)
			}
			// And no duplicate citations.
			if n := p3bCount(t, dm, `SELECT COUNT(*) FROM epistemic_provenance`); n != len(first[0].Payload["source_ids"].([]string)) {
				t.Errorf("expected %d citation rows (no duplicates), got %d",
					len(first[0].Payload["source_ids"].([]string)), n)
			}
		})
	}
}

// ── 5. a changed observation produces a distinct proposal ───────────────

// TestP3b_ChangedObservationCreatesADistinctProposal pins the other half
// of idempotency: idempotency must not become STALENESS.
//
// A hunt that always produces the same proposal regardless of what it
// observes would be just as wrong as one that produces a new proposal every
// cycle. A genuinely different observation must produce a genuinely
// different proposal.
func TestP3b_ChangedObservationCreatesADistinctProposal(t *testing.T) {
	// (a) weak_theory: a DIFFERENT weak theory is a different observation.
	t.Run("weak_theory", func(t *testing.T) {
		a, dm := p3bRealAudit(t)
		ctx := context.Background()

		first, err := dm.ProposeTheory(
			"The first weak hypothesis.", "Criteria one.",
			nil, nil, []string{"p3b", "weak"})
		if err != nil {
			t.Fatalf("ProposeTheory: %v", err)
		}
		second, err := dm.ProposeTheory(
			"The second weak hypothesis.", "Criteria two.",
			nil, nil, []string{"p3b", "weak"})
		if err != nil {
			t.Fatalf("ProposeTheory: %v", err)
		}
		firstID, _ := first["id"].(string)
		secondID, _ := second["id"].(string)

		findings, err := (&WeakTheoryHunt{MaxConfidence: 0.6}).Run(ctx, a)
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if len(findings) != 2 {
			t.Fatalf("expected 2 findings (two weak theories), got %d", len(findings))
		}

		ids := map[string]bool{}
		for _, f := range findings {
			res, err := p3bApplyProposal(t, dm, f.Payload)
			if err != nil {
				t.Fatalf("apply the proposal: %v", err)
			}
			id, _ := res["id"].(string)
			ids[id] = true
		}
		if len(ids) != 2 {
			t.Errorf("expected 2 DISTINCT proposals for 2 distinct observations, got %d: %v",
				len(ids), ids)
		}
		if ids[firstID] || ids[secondID] {
			t.Errorf("expected the proposals to be new artifacts, not the observed "+
				"theories themselves (observed: %q, %q)", firstID, secondID)
		}
	})

	// (b) survival_asymmetry: a materially different asymmetry is a
	// different observation. The ratio is quantized into a band, so the
	// band crossing — not ordinary decay drift — is what changes the
	// proposal's identity.
	t.Run("survival_asymmetry", func(t *testing.T) {
		a, dm := p3bRealAudit(t)
		ctx := context.Background()

		// Band 2x-4x: call 25/100 alive (0.25), direct 47/50 (0.94) →
		// ratio 3.76.
		p3bSeedCohort(t, dm, "call", 100, 25, 0, 1793000000)
		p3bSeedCohort(t, dm, "direct", 50, 47, 0, 1793000100)

		first, err := (&SurvivalAsymmetryHunt{}).Run(ctx, a)
		if err != nil {
			t.Fatalf("first Run: %v", err)
		}
		if len(first) != 1 {
			t.Fatalf("expected 1 finding, got %d", len(first))
		}
		res1, err := p3bApplyProposal(t, dm, first[0].Payload)
		if err != nil {
			t.Fatalf("apply the first proposal: %v", err)
		}
		id1, _ := res1["id"].(string)

		// The 'call' cohort ages: 100 rows, 5 alive (0.05). direct 49/50
		// (0.98) → ratio 19.6, band 16x-32x. A materially different
		// observation.
		if _, err := dm.SQLDB().Exec(
			`UPDATE memories SET deleted_at = 1790000000
			   WHERE collection = 'memories'
			     AND json_extract(metadata, '$.source') = 'call'
			     AND id NOT IN (SELECT id FROM memories
			                     WHERE collection = 'memories'
			                       AND json_extract(metadata, '$.source') = 'call'
			                       AND deleted_at IS NULL
			                     ORDER BY created_at ASC, id ASC LIMIT 5)`); err != nil {
			t.Fatalf("age the call cohort: %v", err)
		}
		second, err := (&SurvivalAsymmetryHunt{}).Run(ctx, a)
		if err != nil {
			t.Fatalf("second Run: %v", err)
		}
		if len(second) != 1 {
			t.Fatalf("expected 1 finding, got %d", len(second))
		}
		if fmt.Sprint(first[0].Payload["hypothesis"]) == fmt.Sprint(second[0].Payload["hypothesis"]) {
			t.Fatalf("expected the hypothesis to change with the observation — "+
				"the ratio band is in the hypothesis, so a band crossing must "+
				"change it: %v", first[0].Payload["hypothesis"])
		}
		res2, err := p3bApplyProposal(t, dm, second[0].Payload)
		if err != nil {
			t.Fatalf("apply the second proposal: %v", err)
		}
		id2, _ := res2["id"].(string)
		if id1 == id2 {
			t.Errorf("expected a materially different observation to produce a "+
				"DISTINCT proposal, got the same id %q", id1)
		}
	})
}

// TestP3b_RatioBandIsStableUnderDrift pins WHY the ratio is quantized.
//
// The ratio is a live measurement: rows age out of the cohorts, so it
// drifts. An exact ratio in the hypothesis would make every cycle a
// DISTINCT proposal — the D4 defect in a new place. The band is stable
// under drift, and only a crossing changes the proposal's identity.
func TestP3b_RatioBandIsStableUnderDrift(t *testing.T) {
	for _, tc := range []struct {
		ratio float64
		want  string
	}{
		{2.0, "2x-4x"},
		{2.5, "2x-4x"},
		{3.99, "2x-4x"},
		{4.0, "4x-8x"},
		{7.9, "4x-8x"},
		{8.0, "8x-16x"},
		{19.6, "16x-32x"},
		{1.5, "1x-2x"},
	} {
		if got := ratioBand(tc.ratio); got != tc.want {
			t.Errorf("ratioBand(%v) = %q, want %q", tc.ratio, got, tc.want)
		}
	}
	// A drift of 0.5% must NOT change the band.
	if ratioBand(3.7600) != ratioBand(3.7788) {
		t.Errorf("expected a 0.5%% drift to leave the band unchanged")
	}
}

// TestP3b_SurvivalProposalIsStableUnderObservationDrift pins WHY the ratio
// is quantized.
//
// The ratio is a LIVE measurement: rows age out of the cohorts, so the
// exact figure drifts from cycle to cycle. An exact ratio in the
// HYPOTHESIS — an identity-bearing field — would therefore make every
// cycle a DISTINCT proposal, which is the D4 defect in a new place.
//
// The band is what makes the proposal stable: ordinary drift leaves it
// alone, so the proposal's identity-bearing fields are unchanged and the
// hunt is free to re-emit the same finding every cycle.
func TestP3b_SurvivalProposalIsStableUnderObservationDrift(t *testing.T) {
	a, dm := p3bRealAudit(t)
	ctx := context.Background()

	// call 25/100 alive (0.25), direct 47/50 alive (0.94) → ratio 3.76,
	// band 2x-4x.
	p3bSeedCohort(t, dm, "call", 100, 25, 0, 1793000000)
	p3bSeedCohort(t, dm, "direct", 50, 47, 0, 1793000100)

	first, err := (&SurvivalAsymmetryHunt{}).Run(ctx, a)
	if err != nil {
		t.Fatalf("first Run: %v", err)
	}
	if len(first) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(first))
	}
	if !strings.Contains(first[0].Payload["hypothesis"].(string), "2x-4x") {
		t.Fatalf("expected the hypothesis to carry the ratio band, got %v",
			first[0].Payload["hypothesis"])
	}

	// The cohort ages by one row: 24/100 alive (0.24) → ratio 3.92. The
	// band is unchanged; the exact ratio is not.
	if _, err := dm.SQLDB().Exec(`
		UPDATE memories SET deleted_at = 1790000000
		WHERE id = (
			SELECT id FROM memories
			WHERE collection = 'memories'
			  AND json_extract(metadata, '$.source') = 'call'
			  AND deleted_at IS NULL
			ORDER BY created_at DESC, id DESC LIMIT 1)`); err != nil {
		t.Fatalf("age the call cohort by one row: %v", err)
	}

	second, err := (&SurvivalAsymmetryHunt{}).Run(ctx, a)
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if len(second) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(second))
	}
	for _, field := range []string{"hypothesis", "validation_criteria", "tags"} {
		if fmt.Sprint(first[0].Payload[field]) != fmt.Sprint(second[0].Payload[field]) {
			t.Errorf("expected %q to be unchanged by ordinary observation drift — "+
				"a drifting value in an identity-bearing field would create a "+
				"NEW proposal every cycle.\nbefore: %v\nafter:  %v",
				field, first[0].Payload[field], second[0].Payload[field])
		}
	}
}

// TestP3b_SurvivalCitationSetIsStableAsTheCohortGrows pins the citation
// set's stability.
//
// The citations are the cohort's OLDEST live rows, so they are a stable
// PREFIX: as the cohort grows, the same rows are re-cited and the
// proposal's evidence does not churn. Chasing the newest activity would
// change the proposal's meaning on every cycle.
//
// The new rows are also ALIVE, so the survival rate moves a little — but
// the ratio stays in the same band, so the proposal's identity does not
// change either. That is the whole point of quantizing.
func TestP3b_SurvivalCitationSetIsStableAsTheCohortGrows(t *testing.T) {
	a, dm := p3bRealAudit(t)
	ctx := context.Background()

	p3bSeedCohort(t, dm, "call", 100, 25, 0, 1793000000)
	p3bSeedCohort(t, dm, "direct", 50, 47, 0, 1793000100)

	first, err := (&SurvivalAsymmetryHunt{}).Run(ctx, a)
	if err != nil {
		t.Fatalf("first Run: %v", err)
	}
	if len(first) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(first))
	}
	firstIDs, _ := first[0].Payload["source_ids"].([]string)

	// The cohort keeps working: ten more ALIVE rows, all newer.
	for i := 0; i < 10; i++ {
		p3bSeedMemory(t, dm, fmt.Sprintf("call-new-%02d", i), "memories",
			"a newer call-sourced memory", "call", 0.8, nil, 1793001000+int64(i))
	}

	second, err := (&SurvivalAsymmetryHunt{}).Run(ctx, a)
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if len(second) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(second))
	}
	secondIDs, _ := second[0].Payload["source_ids"].([]string)

	if fmt.Sprint(firstIDs) != fmt.Sprint(secondIDs) {
		t.Errorf("expected the citation set to stay a stable PREFIX of the "+
			"cohort as it grew, got %v then %v", firstIDs, secondIDs)
	}
	if first[0].Payload["hypothesis"] != second[0].Payload["hypothesis"] {
		t.Errorf("expected the hypothesis to be unchanged by the cohort's growth")
	}
}

// ── 6. pending proposals are marked pending in the wake context ──────────

// TestP3b_PendingProposalIsMarkedPendingInWakeContext pins that a pending
// proposal that reaches the agent's context is EXPLICITLY marked pending.
//
// Adjudication §6c.2 limitation 2 states that a proposal on the proposal
// surface "would reach the agent's context immediately — labelled pending,
// not promoted". Before P3b that label did not exist: the wake context's
// theory pointer carried an id, a summary and a pointer URI, and nothing
// distinguished a pending proposal from a resolved one.
//
// The trust boundary needs the distinction to be visible where the agent
// actually reads it.
func TestP3b_PendingProposalIsMarkedPendingInWakeContext(t *testing.T) {
	a, dm := p3bRealAudit(t)
	ctx := context.Background()

	if _, err := dm.ProposeTheory(
		"A pending hypothesis the critic should notice.",
		"Demonstrate the claim.",
		nil, nil, []string{"p3b", "weak"},
	); err != nil {
		t.Fatalf("ProposeTheory: %v", err)
	}
	findings, err := (&WeakTheoryHunt{MaxConfidence: 0.6}).Run(ctx, a)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(findings))
	}
	res, err := p3bApplyProposal(t, dm, findings[0].Payload)
	if err != nil {
		t.Fatalf("apply the proposal: %v", err)
	}
	proposalID, _ := res["id"].(string)

	wake := p3bReadWakeContext(t, dm)
	theories, _ := wake["recent_theories"].([]map[string]interface{})
	if len(theories) == 0 {
		t.Fatalf("expected the wake context to surface recent theories, got none: %v",
			wake["recent_theories"])
	}

	var found map[string]interface{}
	for _, th := range theories {
		if id, _ := th["id"].(string); id == proposalID {
			found = th
			break
		}
	}
	if found == nil {
		t.Fatalf("expected the proposal %q to be surfaced in the wake context's "+
			"recent_theories, got %v", proposalID, theories)
	}
	status, present := found["status"]
	if !present {
		t.Fatalf("expected the surfaced proposal to carry an explicit status, "+
			"got %v", found)
	}
	if status != "pending" {
		t.Errorf("expected the surfaced proposal to be marked %q, got %v",
			"pending", status)
	}
}

// ── 7. the detection behaviour is intact ─────────────────────────────────

// TestP3b_ExistingDetectionBehaviourIsIntact pins that the hunts still
// detect exactly what they detected before P3b.
//
// The surface changed; the RULE did not.
func TestP3b_ExistingDetectionBehaviourIsIntact(t *testing.T) {
	ctx := context.Background()

	t.Run("survival_below_threshold_does_not_fire", func(t *testing.T) {
		a, dm := p3bRealAudit(t)
		p3bSeedCohort(t, dm, "call", 50, 50, 0, 1793000000)
		p3bSeedCohort(t, dm, "direct", 50, 50, 0, 1793000100)
		findings, err := (&SurvivalAsymmetryHunt{}).Run(ctx, a)
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if len(findings) != 0 {
			t.Errorf("expected 0 findings (ratio 1.0), got %d", len(findings))
		}
	})

	t.Run("survival_below_min_sample_does_not_fire", func(t *testing.T) {
		a, dm := p3bRealAudit(t)
		p3bSeedCohort(t, dm, "call", 5, 1, 0, 1793000000)
		p3bSeedCohort(t, dm, "direct", 5, 5, 0, 1793000100)
		findings, err := (&SurvivalAsymmetryHunt{}).Run(ctx, a)
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if len(findings) != 0 {
			t.Errorf("expected 0 findings (sample 5 < min 10), got %d", len(findings))
		}
	})

	t.Run("survival_missing_direct_cohort_does_not_fire", func(t *testing.T) {
		a, dm := p3bRealAudit(t)
		p3bSeedCohort(t, dm, "call", 50, 5, 0, 1793000000)
		findings, err := (&SurvivalAsymmetryHunt{}).Run(ctx, a)
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if len(findings) != 0 {
			t.Errorf("expected 0 findings (no direct cohort), got %d", len(findings))
		}
	})

	t.Run("survival_zero_survival_does_not_fire", func(t *testing.T) {
		a, dm := p3bRealAudit(t)
		// Every 'call' row deleted: survival 0 → the hunt must not fire.
		p3bSeedCohort(t, dm, "call", 50, 0, 0, 1793000000)
		p3bSeedCohort(t, dm, "direct", 50, 50, 0, 1793000100)
		findings, err := (&SurvivalAsymmetryHunt{}).Run(ctx, a)
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if len(findings) != 0 {
			t.Errorf("expected 0 findings (call survival is 0), got %d", len(findings))
		}
	})

	t.Run("weak_theory_floor_is_respected", func(t *testing.T) {
		a, dm := p3bRealAudit(t)
		// A theory at 0.59 is below the default floor of 0.6.
		p3bSeedMemory(t, dm, "almost", "theories", "almost strong", "", 0.59, nil, 1793000000)
		p3bSeedMemory(t, dm, "strong", "theories", "strong theory", "", 0.9, nil, 1793000001)
		p3bSeedMemory(t, dm, "not-a-theory", "memories", "a memory", "", 0.3, nil, 1793000002)
		p3bSeedMemory(t, dm, "deleted-theory", "theories", "deleted", "", 0.3,
			int64(1790000000), 1793000003)

		findings, err := (&WeakTheoryHunt{MaxConfidence: 0.6}).Run(ctx, a)
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if len(findings) != 1 {
			t.Fatalf("expected 1 finding (only the 0.59 theory), got %d", len(findings))
		}
		sourceIDs, _ := findings[0].Payload["source_ids"].([]string)
		if len(sourceIDs) != 1 || sourceIDs[0] != "almost" {
			t.Errorf("expected the finding to cite the 0.59 theory, got %v", sourceIDs)
		}
	})
}

// TestP3b_SurvivalCohortIsScopedToTheMemoriesCollection pins the ONE
// deliberate detection correction.
//
// Before P3b the cohort was every row with a provenance `source` label,
// whatever its collection. On the live workspace that mixed 33
// memories-collection rows with 10 agentshell, 6 project, 4 changelog, 1
// lesson and 2 decision rows — all stamped 'call' by the same save layer,
// but living under different lifecycles. It also made the typed citation
// impossible, because P2's 'memory' source type resolves only against
// the memories collection.
//
// The prey claim is about MEMORIES, so the cohort is the memories
// collection. This test pins the correction so it cannot drift back.
func TestP3b_SurvivalCohortIsScopedToTheMemoriesCollection(t *testing.T) {
	a, dm := p3bRealAudit(t)
	ctx := context.Background()

	// Rows OUTSIDE the memories collection, stamped 'call', all alive.
	// They are deliberately numerous and healthy enough to SWAMP the
	// real cohort if it were not scoped.
	//
	// The arithmetic: the memories-collection 'call' cohort is 100 rows
	// with 5 alive (survival 0.05); the 'direct' cohort is 50 rows with
	// 49 alive (survival 0.98).
	//
	//   scoped   (memories only): 0.98 / 0.05 = 19.6  → fires
	//   unscoped (+ 200 healthy): 0.98 / 0.683 = 1.43 → does NOT fire
	//
	// So the hunt's firing is itself the observable: an unscoped cohort
	// silently swallows the asymmetry, which is precisely the defect the
	// scope corrects.
	for i := 0; i < 200; i++ {
		p3bSeedMemory(t, dm, fmt.Sprintf("other-%03d", i), "project",
			"a project note", "call", 0.8, nil, 1793000000+int64(i))
	}
	p3bSeedCohort(t, dm, "call", 100, 5, 0, 1793000100)
	p3bSeedCohort(t, dm, "direct", 50, 49, 0, 1793000200)

	findings, err := (&SurvivalAsymmetryHunt{}).Run(ctx, a)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding — the non-memory rows must not enter the "+
			"cohort, got %d findings", len(findings))
	}
	// The measured figures must be the SCOPED ones.
	reason := findings[0].Reason
	if !strings.Contains(reason, "call=0.050 (100 rows)") {
		t.Errorf("expected the measured cohort to be the 100 memories-collection "+
			"rows, got the reason %q", reason)
	}
	// And the cited ids are all memories-collection rows.
	sourceIDs, _ := findings[0].Payload["source_ids"].([]string)
	for _, id := range sourceIDs {
		var collection string
		if err := dm.SQLDB().QueryRow(
			`SELECT collection FROM memories WHERE id = ?`, id).Scan(&collection); err != nil {
			t.Fatalf("cited id %q does not resolve: %v", id, err)
		}
		if collection != "memories" {
			t.Errorf("cited id %q has collection %q — only the memories "+
				"collection is in the cohort", id, collection)
		}
	}
}

// ── the structural guard ─────────────────────────────────────────────────

// TestP3b_NoHuntTargetsTheTrustedLessonSurface is the structural guard
// over the WHOLE critic repertoire.
//
// It runs every hunt that can emit a finding, with data that makes each
// one fire, and asserts that NONE of them targets `mpm_lessons`.
//
// WHY A GUARD RATHER THAN A REVIEW. The trusted lesson surface has no
// pending state: a lesson is trusted on arrival. So a single hunt that
// emits `mpm_lessons save` re-opens defect D2 — silently, and with no
// test failing. The guard is what keeps the transition complete as more
// hunts are added.
//
// There is NO LONGER an EXCEPTION here.
//
// The `--telemetry-hunt` path in cmd/mpm-critic used to write lessons
// through the mpm repository's telemetry package. It no longer does: it is
// a read-only diagnostic that reports the observation and writes nothing
// (#138, and #150 for the count), and it is outside this repertoire
// either way — the guard above covers the hunts in the critic's default
// repertoire.
//
// This comment used to record that exception as live. It was left
// standing after the exception was closed, which is how a reader comes to
// believe a trust-boundary hole is still open. The guard now covers the
// whole surface, and a hunt that emits `mpm_lessons save` again fails
// this test.
func TestP3b_NoHuntTargetsTheTrustedLessonSurface(t *testing.T) {
	a, dm := p3bRealAudit(t)
	ctx := context.Background()

	// SurvivalAsymmetryHunt: an asymmetry above the threshold.
	p3bSeedCohort(t, dm, "call", 100, 25, 0, 1793000000)
	p3bSeedCohort(t, dm, "direct", 50, 47, 0, 1793000100)
	// WeakTheoryHunt: a pending theory below the floor.
	p3bSeedMemory(t, dm, "guard-weak", "theories", "a weak theory", "", 0.4, nil, 1793000200)
	// StaleMemoryHunt: a wall-clock-stale, non-long-term memory with
	// residency.
	p3bSeedStaleMemory(t, dm, "guard-stale", "a stale memory")
	p3bSettleMemory(t, dm, "guard-stale")
	// MissedOpportunityHunt: a session that did work and recorded nothing.
	for i := 0; i < 6; i++ {
		p3bSeedInvocation(t, dm, fmt.Sprintf("inv-guard-%02d", i),
			"mpm_references", "search", "ses_guard", 1793000400+int64(i))
	}

	for _, h := range a.hunts {
		findings, err := h.Run(ctx, a)
		if err != nil {
			t.Fatalf("%s: Run: %v", h.Name(), err)
		}
		if len(findings) == 0 {
			t.Errorf("%s: expected the guard to make this hunt FIRE, so its "+
				"target is actually checked — got no findings", h.Name())
			continue
		}
		for _, f := range findings {
			if f.Tool == "mpm_lessons" {
				t.Errorf("%s targets the TRUSTED lesson surface (%s/%s) — the "+
					"critic must emit pending proposals", h.Name(), f.Tool, f.Action)
			}
			if f.Tool == "mpm_theories" && f.Action != "propose" {
				t.Errorf("%s targets mpm_theories/%s — the only untrusted "+
					"theory write is propose", h.Name(), f.Action)
			}
		}
	}
}

// p3bSeedStaleMemory seeds a memory that passes the stale-memory hunt's
// FIRST gate: wall-clock stale (updated_at older than the hunt's MaxAge)
// and not long-term.
func p3bSeedStaleMemory(t *testing.T, dm *mpmcore.DatabaseManager, id, content string) {
	t.Helper()
	stale := time.Now().Add(-40 * 24 * time.Hour).Unix()
	if _, err := dm.SQLDB().Exec(`
		INSERT INTO memories (id, collection, content, metadata, confidence,
		                      deleted_at, created_at, updated_at, is_long_term)
		VALUES (?, 'memories', ?, '{}', 0.8, NULL, ?, ?, 0)`,
		id, content, stale, stale); err != nil {
		t.Fatalf("seed stale memory %s: %v", id, err)
	}
}

// p3bSettleMemory grants cumulative-scheduler-active residency to a
// memory, so the stale-memory hunt's SECOND gate (settling) passes.
//
// StaleMemoryHunt requires BOTH a wall-clock-stale memory AND sufficient
// active uptime; the second gate exists so the critic never judges a
// memory the system has barely had the opportunity to observe.
func p3bSettleMemory(t *testing.T, dm *mpmcore.DatabaseManager, memoryID string) {
	t.Helper()
	if _, err := dm.SQLDB().Exec(
		`INSERT OR REPLACE INTO system_config (key, raw_json, content_hash) VALUES (?, ?, '')`,
		mpmcore.ActiveUptimeKey,
		fmt.Sprintf(`{"active_seconds":%d,"last_elapsed_ms":0,"updated_at":0}`,
			int64(30*24*time.Hour/time.Second))); err != nil {
		t.Fatalf("seed active uptime: %v", err)
	}
	if _, err := dm.SQLDB().Exec(
		`INSERT OR REPLACE INTO memory_settling_baselines (memory_id, baseline_active_seconds, captured_at)
		 VALUES (?, 0, 0)`, memoryID); err != nil {
		t.Fatalf("seed baseline for %s: %v", memoryID, err)
	}
}

// p3bSeedInvocation inserts a tool_invocations row, so the
// missed-opportunity hunt has a session to look at.
func p3bSeedInvocation(t *testing.T, dm *mpmcore.DatabaseManager, id, tool, action, frameworkSession string, startedAt int64) {
	t.Helper()
	if _, err := dm.SQLDB().Exec(`
		INSERT INTO tool_invocations
			(id, session_id, tool_name, action, invocation_id, actor_kind,
			 framework_name, payload_hash, result_status, started_at,
			 mpm_session_id, framework_session_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, "mpm-sess-guard", tool, action, "inv_"+id, "agent", "opencode",
		"sha256:"+id, "success", startedAt, "mpm-sess-guard", frameworkSession); err != nil {
		t.Fatalf("seed tool_invocations %s: %v", id, err)
	}
}
