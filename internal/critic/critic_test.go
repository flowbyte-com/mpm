// Tests for the critic audit cycle.
//
// Each hunt is exercised against an isolated in-memory schema with seeded
// rows that exercise the rule boundary (threshold, sample size, collection
// filter, delete semantics). The Audit orchestrator is tested with a
// fake CLIRunner so emit-via-mpm-call is a pure in-process assertion.

package critic

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

const criticTestSchema = `
CREATE TABLE memories (
    id TEXT PRIMARY KEY,
    collection TEXT NOT NULL,
    content TEXT NOT NULL,
    tags JSON,
    metadata JSON,
    confidence REAL NOT NULL DEFAULT 0.8,
    is_long_term INTEGER NOT NULL DEFAULT 0,
    deleted_at INTEGER,
    updated_at INTEGER,
    created_at INTEGER
);
`

// newTestAudit returns an Audit with a temp-file DB and a fake CLI runner.
// The fake recorder captures every (tool, payload) the cycle emits so the
// test can assert without spawning the real mpm binary.
func newTestAudit(t *testing.T) (*Audit, *fakeCLI) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")
	db, err := sql.Open("sqlite3", path+"?_busy_timeout=5000&_journal_mode=WAL&_synchronous=NORMAL")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if _, err := db.Exec(criticTestSchema); err != nil {
		t.Fatalf("install schema: %v", err)
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	a := &Audit{
		db:    db,
		log:   log,
		cycle: 0,
		cli:   &fakeCLI{},
		hunts: []Hunt{
			&SurvivalAsymmetryHunt{},
			&StaleMemoryHunt{MaxAge: 30 * 24 * time.Hour},
			&WeakTheoryHunt{MaxConfidence: 0.6},
		},
	}
	return a, a.cli.(*fakeCLI)
}

// fakeCLI records every Call. Set failTool to make the runner error on
// a specific tool (testing emit failure paths).
type fakeCLI struct {
	mu       sync.Mutex
	calls    []fakeCall
	failTool string // when set, calls to this tool return error
}

type fakeCall struct {
	Tool    string
	Action  string
	Payload map[string]interface{}
}

func (f *fakeCLI) Call(_ context.Context, tool, action string, payload map[string]interface{}) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fakeCall{Tool: tool, Action: action, Payload: payload})
	if f.failTool != "" && tool == f.failTool {
		return errors.New("synthetic failure: " + tool)
	}
	return nil
}

func (f *fakeCLI) Calls() []fakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]fakeCall, len(f.calls))
	copy(out, f.calls)
	return out
}

// seedMemory inserts a memory row with the given fields. deletedAt=="" means alive.
func seedMemory(t *testing.T, a *Audit, id, collection, content, source string, confidence float64, deletedAt string, updatedAt time.Time) {
	t.Helper()
	metadata := `{}`
	if source != "" {
		metadata = `{"source":"` + source + `"}`
	}
	var updArg interface{}
	if updatedAt.IsZero() {
		updArg = nil
	} else {
		updArg = updatedAt.Unix()
	}
	var delArg interface{}
	if deletedAt != "" {
		delArg = deletedAt
	}
	_, err := a.db.Exec(
		`INSERT INTO memories (id, collection, content, metadata, confidence, deleted_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		id, collection, content, metadata, confidence, delArg, updArg,
	)
	if err != nil {
		t.Fatalf("seed memory: %v", err)
	}
}

// ── SurvivalAsymmetryHunt ────────────────────────────────────────────────

func TestSurvivalAsymmetryHunt_FiresAboveThreshold(t *testing.T) {
	a, cli := newTestAudit(t)
	ctx := context.Background()

	// Reproduce the original asymmetry direction: call-sourced memories
	// die at much higher rate than direct-sourced. The hunt tests
	// direct/call ratio > threshold (calling out call-sourced degradation).
	// 100 call-source, 25 alive (0.25). 50 direct, 47 alive (0.94).
	// ratio = 0.94 / 0.25 = 3.76x → fires.
	for i := 0; i < 100; i++ {
		alive := i < 25
		del := ""
		if !alive {
			del = "2026-07-01 00:00:00"
		}
		seedMemory(t, a, mkID("call", i), "memories", "call test", "call", 0.8, del, time.Now())
	}
	for i := 0; i < 50; i++ {
		alive := i < 47
		del := ""
		if !alive {
			del = "2026-07-01 00:00:00"
		}
		seedMemory(t, a, mkID("direct", i), "memories", "direct test", "direct", 0.8, del, time.Now())
	}

	findings, err := (&SurvivalAsymmetryHunt{}).Run(ctx, a)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(findings))
	}
	if findings[0].Tool != "mpm_lessons" || findings[0].Action != "save" {
		t.Errorf("expected mpm_lessons/save, got %s/%s", findings[0].Tool, findings[0].Action)
	}
	if findings[0].Priority != 2 {
		t.Errorf("expected priority 2, got %d", findings[0].Priority)
	}
	if len(cli.Calls()) != 0 {
		t.Errorf("hunt.Run should NOT emit directly; got %d calls", len(cli.Calls()))
	}
}

func TestSurvivalAsymmetryHunt_BelowThreshold_NoFire(t *testing.T) {
	a, _ := newTestAudit(t)
	ctx := context.Background()

	// Equal survival: 50 call / 50 alive, 50 direct / 50 alive. ratio=1.0.
	for i := 0; i < 50; i++ {
		seedMemory(t, a, mkID("call", i), "memories", "call", "call", 0.8, "", time.Now())
		seedMemory(t, a, mkID("direct", i), "memories", "direct", "direct", 0.8, "", time.Now())
	}
	findings, err := (&SurvivalAsymmetryHunt{}).Run(ctx, a)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(findings) != 0 {
		t.Errorf("expected 0 findings, got %d (dormant prey should not fire)", len(findings))
	}
}

func TestSurvivalAsymmetryHunt_BelowMinSample_NoFire(t *testing.T) {
	a, _ := newTestAudit(t)
	ctx := context.Background()

	// Only 5 per source (below default min=10). Even with extreme ratio,
	// the hunt should refuse to fire to avoid p-hacking on small N.
	for i := 0; i < 5; i++ {
		seedMemory(t, a, mkID("call", i), "memories", "call", "call", 0.8, "2026-07-01 00:00:00", time.Now())
		seedMemory(t, a, mkID("direct", i), "memories", "direct", "direct", 0.8, "", time.Now())
	}
	findings, err := (&SurvivalAsymmetryHunt{}).Run(ctx, a)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(findings) != 0 {
		t.Errorf("expected 0 findings (sample size 5 < min 10), got %d", len(findings))
	}
}

func TestSurvivalAsymmetryHunt_MissingDirect_NoFire(t *testing.T) {
	a, _ := newTestAudit(t)
	ctx := context.Background()

	// Only call-sourced memories. Hunt needs both sources to compute ratio.
	for i := 0; i < 50; i++ {
		seedMemory(t, a, mkID("call", i), "memories", "call", "call", 0.8, "", time.Now())
	}
	findings, err := (&SurvivalAsymmetryHunt{}).Run(ctx, a)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(findings) != 0 {
		t.Errorf("expected 0 findings (missing direct source), got %d", len(findings))
	}
}

// ── StaleMemoryHunt ─────────────────────────────────────────────────────

func TestStaleMemoryHunt_FlagsOldMemory(t *testing.T) {
	a, _ := newTestAudit(t)
	ctx := context.Background()

	oldDate := time.Now().Add(-90 * 24 * time.Hour) // 90 days ago, > 30d default
	recent := time.Now().Add(-1 * 24 * time.Hour)  // 1 day ago

	seedMemory(t, a, "old-1", "memories", "outdated info", "", 0.8, "", oldDate)
	seedMemory(t, a, "old-2", "memories", "another outdated", "", 0.7, "", oldDate)
	seedMemory(t, a, "recent-1", "memories", "fresh data", "", 0.9, "", recent)
	// Deleted memories: should NOT be flagged.
	seedMemory(t, a, "old-deleted", "memories", "old gone", "", 0.8, "2026-07-01 00:00:00", oldDate)
	// No updated_at: should not be flagged (NULL).
	seedMemory(t, a, "no-updated", "memories", "no timestamp", "", 0.8, "", time.Time{})

	findings, err := (&StaleMemoryHunt{MaxAge: 30 * 24 * time.Hour}).Run(ctx, a)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(findings) != 2 {
		t.Fatalf("expected 2 findings (old-1 + old-2), got %d", len(findings))
	}
	for _, f := range findings {
		if f.Tool != "mpm_memory" {
			t.Errorf("expected mpm_memory, got %s", f.Tool)
		}
		if f.Action != "challenge" {
			t.Errorf("expected challenge action, got %s", f.Action)
		}
		if f.Priority != 1 {
			t.Errorf("expected priority 1, got %d", f.Priority)
		}
	}
}

func TestStaleMemoryHunt_DefaultMaxAge(t *testing.T) {
	a, _ := newTestAudit(t)
	ctx := context.Background()

	// 31 days old: just past the default 30-day threshold. Should fire.
	stale := time.Now().Add(-31 * 24 * time.Hour)
	seedMemory(t, a, "stale", "memories", "old memory", "", 0.8, "", stale)

	// Hunt with default MaxAge=0 → code uses 30 days.
	findings, err := (&StaleMemoryHunt{}).Run(ctx, a)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding (default 30d), got %d", len(findings))
	}
}

// ── WeakTheoryHunt ──────────────────────────────────────────────────────

func TestWeakTheoryHunt_FlagsBelowThreshold(t *testing.T) {
	a, _ := newTestAudit(t)
	ctx := context.Background()

	// Theories in collection='theories'. Confidence < 0.6 should fire.
	seedMemory(t, a, "weak-1", "theories", "weak theory A", "", 0.3, "", time.Now())
	seedMemory(t, a, "weak-2", "theories", "weak theory B", "", 0.4, "", time.Now())
	seedMemory(t, a, "strong-1", "theories", "strong theory", "", 0.9, "", time.Now())
	// Memory NOT in theories collection: should NOT fire.
	seedMemory(t, a, "weak-mem", "memories", "weak memory", "", 0.3, "", time.Now())
	// Deleted weak theory: should NOT fire.
	seedMemory(t, a, "weak-deleted", "theories", "deleted weak", "", 0.3, "2026-07-01 00:00:00", time.Now())

	findings, err := (&WeakTheoryHunt{MaxConfidence: 0.6}).Run(ctx, a)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(findings) != 2 {
		t.Fatalf("expected 2 findings (weak-1 + weak-2), got %d", len(findings))
	}
	for _, f := range findings {
		if f.Tool != "mpm_lessons" || f.Action != "save" {
			t.Errorf("expected mpm_lessons/save, got %s/%s", f.Tool, f.Action)
		}
	}
}

func TestWeakTheoryHunt_DefaultThreshold(t *testing.T) {
	a, _ := newTestAudit(t)
	ctx := context.Background()

	// 0.59 confidence: just below default 0.6. Should fire.
	seedMemory(t, a, "almost", "theories", "almost strong", "", 0.59, "", time.Now())
	findings, err := (&WeakTheoryHunt{}).Run(ctx, a)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding (default 0.6), got %d", len(findings))
	}
}

// ── PoisonPillHunt ──────────────────────────────────────────────────────

func TestPoisonPillHunt_NoHighConfidenceMemory_NoFire(t *testing.T) {
	a, _ := newTestAudit(t)
	ctx := context.Background()

	// Only low-confidence memories.
	seedMemory(t, a, "low-1", "memories", "low", "", 0.5, "", time.Now())
	findings, err := (&PoisonPillHunt{Cycle: 5}).Run(ctx, a)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(findings) != 0 {
		t.Errorf("expected 0 findings (no high-conf memory), got %d", len(findings))
	}
}

func TestPoisonPillHunt_PicksHighestConfidenceMemory(t *testing.T) {
	a, _ := newTestAudit(t)
	ctx := context.Background()

	seedMemory(t, a, "mid", "memories", "mid", "", 0.85, "", time.Now())
	seedMemory(t, a, "high-1", "memories", "high A", "", 0.95, "", time.Now())
	seedMemory(t, a, "high-2", "memories", "high B", "", 0.9, "", time.Now())
	// Theory with high confidence: should NOT be picked (collection != memories).
	seedMemory(t, a, "high-theory", "theories", "high theory", "", 0.95, "", time.Now())

	findings, err := (&PoisonPillHunt{Cycle: 5}).Run(ctx, a)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("expected 1 poison pill, got %d", len(findings))
	}
	if findings[0].Tool != "mpm_theories" || findings[0].Action != "propose" {
		t.Errorf("expected mpm_theories/propose, got %s/%s", findings[0].Tool, findings[0].Action)
	}
	hyp, ok := findings[0].Payload["hypothesis"].(string)
	if !ok || !contains(hyp, "high-1") {
		t.Errorf("expected hypothesis to target high-1 (highest conf), got %q", hyp)
	}
	if !contains(hyp, "POISON PILL cycle 5") {
		t.Errorf("expected hypothesis to tag cycle 5, got %q", hyp)
	}
}

// ── Audit orchestrator ─────────────────────────────────────────────────

func TestAudit_Run_EmitsFindingsViaCLI(t *testing.T) {
	a, cli := newTestAudit(t)
	ctx := context.Background()

	// Seed data that fires WeakTheory. Survival asymmetry and stale
	// memory hunts need more data than we want to set up here; we
	// verify the orchestration path with just one finding-emitting
	// hunt so the test stays focused.
	seedMemory(t, a, "weak-1", "theories", "weak A", "", 0.3, "", time.Now())

	if err := a.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if a.Cycle() != 1 {
		t.Errorf("expected cycle=1, got %d", a.Cycle())
	}
	calls := cli.Calls()
	if len(calls) < 1 {
		t.Fatalf("expected ≥1 emitted findings, got %d", len(calls))
	}
	foundSaveLesson := false
	for _, c := range calls {
		if c.Tool == "mpm_lessons" && c.Action == "save" {
			foundSaveLesson = true
		}
	}
	if !foundSaveLesson {
		t.Errorf("expected mpm_lessons/save emission in calls=%v", calls)
	}
}

func TestAudit_Run_PoisonPillOnCycleFive(t *testing.T) {
	a, cli := newTestAudit(t)
	ctx := context.Background()

	// Seed a high-confidence memory so poison pill has a target.
	seedMemory(t, a, "target", "memories", "high conf", "", 0.95, "", time.Now())

	// Run cycles 1..5. Cycle 5 should fire poison pill.
	for i := 1; i <= 5; i++ {
		if err := a.Run(ctx); err != nil {
			t.Fatalf("Run cycle %d: %v", i, err)
		}
	}

	// Find an mpm_theories/propose call (poison pill). Should be exactly 1.
	proposeCount := 0
	for _, c := range cli.Calls() {
		if c.Tool == "mpm_theories" && c.Action == "propose" {
			proposeCount++
		}
	}
	if proposeCount != 1 {
		t.Errorf("expected exactly 1 mpm_theories/propose (poison pill on cycle 5), got %d", proposeCount)
	}
}

func TestAudit_Run_HuntFailureDoesNotStopCycle(t *testing.T) {
	a, cli := newTestAudit(t)
	ctx := context.Background()

	// Replace one hunt with a broken one that always errors.
	a.hunts = []Hunt{
		&brokenHunt{},
		&WeakTheoryHunt{MaxConfidence: 0.6},
	}
	seedMemory(t, a, "weak", "theories", "weak", "", 0.3, "", time.Now())

	if err := a.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if a.Cycle() != 1 {
		t.Errorf("cycle should advance even with broken hunt, got %d", a.Cycle())
	}
	// The WeakTheory hunt should still have emitted its finding.
	found := false
	for _, c := range cli.Calls() {
		if c.Tool == "mpm_lessons" && c.Action == "save" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected WeakTheory finding to emit despite prior hunt failure")
	}
}

func TestAudit_Run_EmitFailureLogsButContinues(t *testing.T) {
	a, _ := newTestAudit(t)
	a.cli.(*fakeCLI).failTool = "mpm_lessons" // every lesson-save call errors
	ctx := context.Background()

	seedMemory(t, a, "weak", "theories", "weak", "", 0.3, "", time.Now())
	if err := a.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Run completes despite CLI errors — that's the failure-isolation guarantee.
	if a.Cycle() != 1 {
		t.Errorf("cycle should still advance, got %d", a.Cycle())
	}
}

// ── helpers ───────────────────────────────────────────────────────────

type brokenHunt struct{}

func (b *brokenHunt) Name() string { return "broken" }
func (b *brokenHunt) Run(_ context.Context, _ *Audit) ([]Finding, error) {
	return nil, errors.New("synthetic hunt failure")
}

func mkID(prefix string, i int) string {
	return prefix + "-" + strconv.Itoa(i)
}

func contains(haystack, needle string) bool {
	if len(needle) == 0 {
		return true
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

// TestCriticCanPublishFinding is the contract test that pins the
// critic's publishing path. It runs the orchestrator end-to-end with
// the fake CLI and asserts the captured Call payload matches the live
// `mpm call` envelope:
//
//	mpm call <Tool> --payload '{"action":"<Action>","params":<Payload>}'
//
// If a future rename breaks `mpm_memory` → `mpm_mind`, or `challenge` →
// `flag_stale`, the captured envelope drifts and this test fails. The
// whole point of this test is to prevent the failure mode where every
// critic emit silently errors with "unknown tool" — exactly the bug
// the live critic had before this stage-1 fix.
func TestCriticCanPublishFinding(t *testing.T) {
	a, cli := newTestAudit(t)
	ctx := context.Background()

	// Seed a stale memory so StaleMemoryHunt fires with the full
	// challenge envelope.
	old := time.Now().Add(-60 * 24 * time.Hour)
	seedMemory(t, a, "stale-1", "memories", "ancient content", "", 0.8, "", old)

	if err := a.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Find the challenge emit. The fake captures every Call verbatim
	// — Tool + Action + Payload are the contract.
	var challenge *fakeCall
	for i := range cli.Calls() {
		c := &cli.Calls()[i]
		if c.Tool == "mpm_memory" && c.Action == "challenge" {
			challenge = c
			break
		}
	}
	if challenge == nil {
		t.Fatalf("no challenge emit captured; calls=%v", cli.Calls())
	}

	// Payload MUST carry the params the live `mpm_memory challenge`
	// action expects — memoryId + evidence. If a future rename moves
	// these field names, the test fails before the bug ships.
	memoryID, ok := challenge.Payload["memoryId"].(string)
	if !ok || memoryID != "stale-1" {
		t.Errorf("payload.memoryId = %v, want \"stale-1\"", challenge.Payload["memoryId"])
	}
	evidence, ok := challenge.Payload["evidence"].(string)
	if !ok || evidence == "" {
		t.Errorf("payload.evidence missing or empty: %v", challenge.Payload["evidence"])
	}
}

// TestCriticEnvelopesAreCurrent is the meta-contract: every Tool/Action
// the critic publishes must reference a tool that exists in the live
// `mpm call` envelope. We can't shell out to `mpm` from a unit test
// reliably, but we can assert the Finding's Tool+Action combo isn't
// one of the historically-broken legacy names. This is the test that
// fails immediately if someone reintroduces "challenge_memory" (which
// the live envelope rejected with "unknown tool").
func TestCriticEnvelopesAreCurrent(t *testing.T) {
	forbidden := map[string]string{
		"challenge_memory":  "legacy — use mpm_memory + challenge",
		"save_lesson":       "legacy — use mpm_lessons + save",
		"propose_theory":    "legacy — use mpm_theories + propose",
	}

	// Drain every published Finding across all hunts by running each
	// in isolation. We can't enumerate hunts from outside, but the
	// public Run() path emits them — so seed the DB to fire all three.
	a, cli := newTestAudit(t)
	ctx := context.Background()

	seedMemory(t, a, "old-mem", "memories", "old", "", 0.8, "", time.Now().Add(-60*24*time.Hour))
	seedMemory(t, a, "weak-theory", "theories", "weak", "", 0.3, "", time.Now())

	if err := a.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, c := range cli.Calls() {
		if why, bad := forbidden[c.Tool]; bad {
			t.Errorf("critic published legacy tool name %q (%s) — the live envelope rejects it as unknown", c.Tool, why)
		}
		if c.Action == "" {
			t.Errorf("critic published Finding with empty Action for tool=%s — the mpm call envelope requires action+params", c.Tool)
		}
	}
}
