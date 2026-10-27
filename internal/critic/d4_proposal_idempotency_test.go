// d4_proposal_idempotency_test.go — the CRITIC half of the D4 contract.
//
// THE DEFECT D4 DESCRIBES.
//
// "Repeated critic cycles generate duplicate theories because of
// cycle-specific tags."
//
// The PoisonPillHunt used to tag its theory with `cycle_<n>`. The
// proposal's identity hash folds json(tags) (internal/core/db.go,
// MemoryIdentityHash), so a cycle-varying tag made every cycle's
// poison pill a DISTINCT proposal — and the hunt defeated its own
// idempotency. The substrate filled with near-identical pending
// theories, one per cycle, each needing its own review.
//
// The correction (Session B's P3 surface switch) moved the cycle number
// out of every identity-bearing field and into the finding's Reason,
// which is logged and never persisted.
//
// WHAT THIS FILE PROVES.
//
// The production path, end to end:
//
//   the hunt runs at cycle N and at cycle N+k  →  the two findings carry
//   IDENTICAL identity-bearing fields  →  the real `mpm_theories propose`
//   dispatch writes ONE pending proposal  →  the second cycle returns the
//   FIRST proposal's id.
//
// The PoisonPillHunt is the hunt D4 is ABOUT, and it is the one the
// P3b suite does not cover (that suite covers the survival-asymmetry and
// weak-theory hunts; the missed-opportunity hunt has its own suite). This
// file closes that gap.
//
// ISOLATION.
//
// HOME, MPM_WORKSPACE and MPM_DB_PATH are pinned to disposable locations
// and the database is a disposable file-backed database at the canonical
// <workspace>/src/db/mpm.db path.

package critic

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	mpmcore "github.com/flowbyte-com/mpm-core"
)

// d4RealAudit builds an Audit over a REAL core database in a disposable
// workspace, and returns the workspace root so a test can reopen the
// database file against it — the "process restart" shape.
func d4RealAudit(t *testing.T) (*Audit, *mpmcore.DatabaseManager, string) {
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
			&StaleMemoryHunt{MaxAge: 30 * 24 * 1e9},
			&WeakTheoryHunt{MaxConfidence: 0.6},
			&MissedOpportunityHunt{},
		},
	}
	return a, dm, ws
}

// d4SeedPoisonPillTarget inserts a memory the poison pill will attack.
//
// The hunt targets the highest-confidence live row in the memories
// collection, so the target must exist and must be confident enough to
// be eligible.
func d4SeedPoisonPillTarget(t *testing.T, dm *mpmcore.DatabaseManager) {
	t.Helper()
	if _, err := dm.SaveMemoryNodeForInvocation(
		"memories",
		"A high-confidence memory the poison pill will target.",
		"", []string{"d4", "target"}, nil, nil,
		false, 1.0, "", "", "", "",
		mpmcore.ActiveContext{},
	); err != nil {
		t.Fatalf("SaveMemoryNodeForInvocation: %v", err)
	}
}

// d4TheoryRows counts the live theory rows.
func d4TheoryRows(t *testing.T, dm *mpmcore.DatabaseManager) int {
	t.Helper()
	var n int
	if err := dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM memories WHERE collection = 'theories' AND deleted_at IS NULL`).Scan(&n); err != nil {
		t.Fatalf("count theories: %v", err)
	}
	return n
}

// TestD4_PoisonPillRepeatedCyclesProduceOnePendingProposal is the
// production-path D4 contract.
//
// Two poison-pill cycles — five apart, exactly as the durable cycle
// counter would advance them — must produce ONE pending proposal, not
// two.
//
// This is the test the D4 contract has always meant: "A periodic reviewer
// re-finding the same thing must not accumulate a new pending proposal
// every cycle."
func TestD4_PoisonPillRepeatedCyclesProduceOnePendingProposal(t *testing.T) {
	a, dm, _ := d4RealAudit(t)
	d4SeedPoisonPillTarget(t, dm)

	first, err := (&PoisonPillHunt{Cycle: 5}).Run(context.Background(), a)
	if err != nil {
		t.Fatalf("PoisonPillHunt cycle 5: %v", err)
	}
	if len(first) == 0 {
		t.Skip("no eligible poison-pill target in this workspace")
	}
	second, err := (&PoisonPillHunt{Cycle: 10}).Run(context.Background(), a)
	if err != nil {
		t.Fatalf("PoisonPillHunt cycle 10: %v", err)
	}
	if len(second) == 0 {
		t.Skip("no eligible poison-pill target in this workspace")
	}

	// The identity-bearing fields are identical across the cycles.
	if fmt.Sprint(first[0].Payload) != fmt.Sprint(second[0].Payload) {
		t.Errorf("expected the two cycles to carry IDENTICAL identity-bearing fields — "+
			"the cycle number must not enter the hypothesis, the tags or the "+
			"validation criteria.\ncycle 5:  %v\ncycle 10: %v",
			first[0].Payload, second[0].Payload)
	}

	// And the production dispatch writes ONE proposal.
	res1, err := p3bApplyProposal(t, dm, first[0].Payload)
	if err != nil {
		t.Fatalf("apply the cycle-5 proposal: %v", err)
	}
	res2, err := p3bApplyProposal(t, dm, second[0].Payload)
	if err != nil {
		t.Fatalf("apply the cycle-10 proposal: %v", err)
	}
	id1, _ := res1["id"].(string)
	id2, _ := res2["id"].(string)
	if id1 == "" || id2 == "" {
		t.Fatalf("a proposal returned no id: %+v / %+v", res1, res2)
	}
	if id1 != id2 {
		t.Errorf("the cycle-10 proposal is %q and the cycle-5 proposal is %q — repeated "+
			"cycles must not accumulate a second pending proposal", id2, id1)
	}
	if n := d4TheoryRows(t, dm); n != 1 {
		t.Errorf("theory rows = %d, want 1 — repeated cycles must not accumulate proposals", n)
	}
}

// TestD4_CriticCyclesAcrossAProcessRestartProduceOnePendingProposal
// answers matrix question 5 on the critic side.
//
// The critic is a ONE-SHOT PROCESS per scheduled audit, so its cycle
// number is durable (system_config) and its proposal must survive a
// process boundary. Nothing but the database file survives a restart, so
// the proposal's identity must be recomputable from what is stored.
func TestD4_CriticCyclesAcrossAProcessRestartProduceOnePendingProposal(t *testing.T) {
	a, dm, ws := d4RealAudit(t)
	d4SeedPoisonPillTarget(t, dm)

	// Cycle 5: the hunt fires and its finding is applied.
	findings, err := (&PoisonPillHunt{Cycle: 5}).Run(context.Background(), a)
	if err != nil {
		t.Fatalf("PoisonPillHunt cycle 5: %v", err)
	}
	if len(findings) == 0 {
		t.Skip("no eligible poison-pill target in this workspace")
	}
	res1, err := p3bApplyProposal(t, dm, findings[0].Payload)
	if err != nil {
		t.Fatalf("apply the cycle-5 proposal: %v", err)
	}
	before, _ := res1["id"].(string)

	// Close the database entirely: nothing but the file survives.
	if err := dm.Close(); err != nil {
		t.Fatalf("close dm: %v", err)
	}
	reopened, err := mpmcore.NewDatabaseManager(ws)
	if err != nil {
		t.Fatalf("reopen NewDatabaseManager(%q): %v", ws, err)
	}
	t.Cleanup(func() { _ = reopened.Close() })

	// The next cycle — in a NEW process, against the SAME file — must
	// find the same proposal.
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	a2 := &Audit{
		db:    reopened.SQLDB(),
		log:   log,
		cycle: 10,
		cli:   &fakeCLI{},
		hunts: []Hunt{
			&SurvivalAsymmetryHunt{},
			&StaleMemoryHunt{MaxAge: 30 * 24 * 1e9},
			&WeakTheoryHunt{MaxConfidence: 0.6},
			&MissedOpportunityHunt{},
		},
	}
	findings2, err := (&PoisonPillHunt{Cycle: 10}).Run(context.Background(), a2)
	if err != nil {
		t.Fatalf("PoisonPillHunt cycle 10 after restart: %v", err)
	}
	if len(findings2) == 0 {
		t.Skip("no eligible poison-pill target in this workspace")
	}
	res2, err := p3bApplyProposal(t, reopened, findings2[0].Payload)
	if err != nil {
		t.Fatalf("apply the cycle-10 proposal after the restart: %v", err)
	}
	after, _ := res2["id"].(string)

	if before != after {
		t.Errorf("the proposal id changed across a process restart: %q -> %q — the identity "+
			"must be recomputable from what is stored", before, after)
	}
	if n := d4TheoryRows(t, reopened); n != 1 {
		t.Errorf("theory rows = %d, want 1 — a process restart must not duplicate the proposal", n)
	}
}
