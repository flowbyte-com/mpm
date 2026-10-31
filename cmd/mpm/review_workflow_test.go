// review_workflow_test.go — the doctor → review → doctor workflow.
//
// WHAT THIS FILE PINS
// -------------------
// The workflow the operator is told to follow:
//
//	mpm doctor              "N memories due for spaced review"
//	mpm ops review --stale  list them
//	mpm ops review --ack-all record the reviews
//	mpm doctor              "no memories due for review"
//
// The pre-fix workflow did not work:
//
//   - The doctor's recommendation named a command that only LISTED.
//     Nothing was acknowledged, so the backlog never moved and the
//     warning came back. Following the instruction changed nothing.
//   - A memory created a second ago was reported as stale. The rule
//     was "last_accessed_at IS NULL OR < cutoff", so EVERY
//     never-accessed memory was due immediately — including a brand-new
//     one.
//   - A never-accessed memory rendered as "106751d ago" (292 years),
//     because a NULL timestamp went through a formatter that did not
//     understand the *int64 shape.
//   - The doctor's "60 memories due" was the length of a 100-candidate
//     SAMPLE, not a count.
//   - The CLI's window was 30 days and the doctor's was 14, so the
//     command showed a different set than the doctor had counted.
//
// The tests below pin the corrected behaviour, and they pin the
// NEGATIVE properties too: a review must not be a side effect of
// looking, and a "keep" review must not touch the weight.
//
// EVERY test runs against an isolated workspace. The live installation
// is never touched.
package main

import (
	"database/sql"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// reviewFixture is an isolated workspace with the binary under test.
type reviewFixture struct {
	bin string
	ws  string
}

// newReviewFixture builds an isolated workspace with an initialised
// database.
func newReviewFixture(t *testing.T) reviewFixture {
	t.Helper()
	bin := mpmBinForTest()
	if bin == "" {
		t.Skip("mpm binary not found; run `make build` first")
	}
	ws := t.TempDir()
	f := reviewFixture{bin: bin, ws: ws}
	// Initialise the schema so the doctor has something to report on.
	f.run(t, "call", "mpm_system", "--payload", `{"action":"health_check"}`)
	return f
}

// run invokes the binary in the fixture workspace and returns the
// combined output and the exit code.
func (f reviewFixture) run(t *testing.T, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(f.bin, args...)
	cmd.Env = append(os.Environ(),
		"MPM_WORKSPACE="+f.ws,
		"MPM_SHARED_DB="+filepath.Join(f.ws, "shared.db"),
	)
	out, err := cmd.CombinedOutput()
	code := 0
	if exitErr, ok := err.(*exec.ExitError); ok {
		code = exitErr.ExitCode()
	} else if err != nil {
		t.Fatalf("run %v: %v\n%s", args, err, out)
	}
	return string(out), code
}

// seedMemory inserts a memory directly, with an explicit age.
//
// ageDays > 0 backdates created_at by that many days. ageDays == 0
// means "created now".
//
// The insert is deliberate rather than a call to `mpm add`: the
// eligibility rule reads created_at, and a fixture that wants a
// 90-day-old memory has to produce one.
func (f reviewFixture) seedMemory(t *testing.T, id string, weight int, ageDays int, lastAccessed *int64) {
	t.Helper()
	db, err := sql.Open("sqlite3", filepath.Join(f.ws, "src", "db", "mpm.db"))
	if err != nil {
		t.Fatalf("open the fixture database: %v", err)
	}
	defer db.Close()

	createdAt := "CAST(strftime('%s','now') AS INTEGER)"
	if ageDays > 0 {
		createdAt = "CAST(strftime('%s','now','-" + strconv.Itoa(ageDays) + " days') AS INTEGER)"
	}
	var lastAccess interface{}
	if lastAccessed != nil {
		lastAccess = *lastAccessed
	}
	_, err = db.Exec(`INSERT INTO memories
		(id, collection, content, embedding, embedding_source, weight,
		 is_long_term, is_prime_directive, reinforcement_count,
		 created_at, updated_at, last_accessed_at)
		VALUES (?, 'memories', ?, NULL, 'provider', ?, 0, 0, 0, `+createdAt+`, `+createdAt+`, ?)`,
		id, "seeded memory "+id, weight, lastAccess)
	if err != nil {
		t.Fatalf("seed %q: %v", id, err)
	}
}

// seedDirective inserts a prime directive.
//
// A directive is standing policy, never a spaced-review candidate.
func (f reviewFixture) seedDirective(t *testing.T, id string, ageDays int) {
	t.Helper()
	db, err := sql.Open("sqlite3", filepath.Join(f.ws, "src", "db", "mpm.db"))
	if err != nil {
		t.Fatalf("open the fixture database: %v", err)
	}
	defer db.Close()

	createdAt := "CAST(strftime('%s','now') AS INTEGER)"
	if ageDays > 0 {
		createdAt = "CAST(strftime('%s','now','-" + strconv.Itoa(ageDays) + " days') AS INTEGER)"
	}
	_, err = db.Exec(`INSERT INTO memories
		(id, collection, content, embedding, embedding_source, weight,
		 is_long_term, is_prime_directive, reinforcement_count,
		 created_at, updated_at)
		VALUES (?, 'directives', ?, NULL, 'provider', 10, 0, 1, 0, `+createdAt+`, `+createdAt+`)`,
		id, "seeded directive "+id)
	if err != nil {
		t.Fatalf("seed directive %q: %v", id, err)
	}
}

// dueIDs runs `mpm ops review --stale --json` and returns the ids it
// lists.
func (f reviewFixture) dueIDs(t *testing.T, extra ...string) []string {
	t.Helper()
	args := append([]string{"ops", "review", "--stale", "--json"}, extra...)
	out, code := f.run(t, args...)
	if code != 0 {
		t.Fatalf("`mpm %s` exited %d:\n%s", strings.Join(args, " "), code, out)
	}
	return parseReviewIDs(t, out)
}

// parseReviewIDs extracts the memory ids from the review JSON.
func parseReviewIDs(t *testing.T, out string) []string {
	t.Helper()
	// The output may carry a scheduler notice before the JSON. Take
	// the last line that parses as an object.
	var payload map[string]interface{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var m map[string]interface{}
		if err := json.Unmarshal([]byte(line), &m); err == nil {
			payload = m
			break
		}
	}
	if payload == nil {
		t.Fatalf("no JSON payload in the review output:\n%s", out)
	}
	raw, _ := payload["memories"].([]interface{})
	ids := make([]string, 0, len(raw))
	for _, item := range raw {
		m, _ := item.(map[string]interface{})
		id, _ := m["id"].(string)
		ids = append(ids, id)
	}
	return ids
}

// nowEpoch is the current Unix-epoch second.
func nowEpoch() int64 {
	return time.Now().Unix()
}

// ── Eligibility ───────────────────────────────────────────────────────────

// TestReviewEligibility_NewMemoryIsNotInstantlyStale is the defect the
// brief named.
//
// A memory created a moment ago must NOT be reported as stale. Under
// the pre-fix rule every never-accessed memory was due immediately.
func TestReviewEligibility_NewMemoryIsNotInstantlyStale(t *testing.T) {
	f := newReviewFixture(t)
	f.seedMemory(t, "brand-new", 10, 0, nil)

	if ids := f.dueIDs(t); len(ids) != 0 {
		t.Errorf("a memory created a moment ago was reported as due for review: %v\n"+
			"A brand-new memory is not stale.", ids)
	}
}

// TestReviewEligibility_OldNeverAccessedMemoryIsStillDue is the
// converse.
//
// The fix must not silence the queue: a memory that was created long
// ago and never accessed IS overdue, and that is exactly the case the
// old rule got right.
func TestReviewEligibility_OldNeverAccessedMemoryIsStillDue(t *testing.T) {
	f := newReviewFixture(t)
	f.seedMemory(t, "old-untouched", 10, 90, nil)

	ids := f.dueIDs(t)
	if len(ids) != 1 || ids[0] != "old-untouched" {
		t.Errorf("the due list is %v, want [old-untouched]", ids)
	}
}

// TestReviewEligibility_LowWeightMemoryIsNotDue pins the second
// condition: a memory has to be worth reviewing.
func TestReviewEligibility_LowWeightMemoryIsNotDue(t *testing.T) {
	f := newReviewFixture(t)
	f.seedMemory(t, "trivial", 1, 90, nil)

	if ids := f.dueIDs(t); len(ids) != 0 {
		t.Errorf("a weight-1 memory was reported as due for review: %v", ids)
	}
}

// TestReviewEligibility_DirectivesAreNotDue pins the directive
// exclusion.
//
// A directive is standing policy, read at wake time. It is never
// vector-searchable and never a spaced-review candidate.
func TestReviewEligibility_DirectivesAreNotDue(t *testing.T) {
	f := newReviewFixture(t)
	f.seedDirective(t, "policy-1", 90)

	if ids := f.dueIDs(t); len(ids) != 0 {
		t.Errorf("a directive was reported as due for review: %v", ids)
	}
}

// TestReviewEligibility_RecentlyAccessedIsNotDue pins the access
// condition.
func TestReviewEligibility_RecentlyAccessedIsNotDue(t *testing.T) {
	f := newReviewFixture(t)
	now := nowEpoch()
	f.seedMemory(t, "freshly-used", 10, 90, &now)

	if ids := f.dueIDs(t); len(ids) != 0 {
		t.Errorf("a memory accessed just now was reported as due for review: %v", ids)
	}
}

// TestReviewEligibility_ReviewedMemoryLeavesTheBacklog pins the
// acknowledgment's effect: a reviewed memory is no longer due.
func TestReviewEligibility_ReviewedMemoryLeavesTheBacklog(t *testing.T) {
	f := newReviewFixture(t)
	f.seedMemory(t, "reviewed-one", 10, 90, nil)

	if ids := f.dueIDs(t); len(ids) != 1 {
		t.Fatalf("the fixture is not due for review: %v", ids)
	}
	out, code := f.run(t, "ops", "review", "--ack", "reviewed-one")
	if code != 0 {
		t.Fatalf("`mpm ops review --ack reviewed-one` exited %d:\n%s", code, out)
	}
	if ids := f.dueIDs(t); len(ids) != 0 {
		t.Errorf("a reviewed memory is still due for review: %v", ids)
	}
}

// ── Rendering ─────────────────────────────────────────────────────────────

// TestReviewRendering_NeverAccessedSaysNeverAccessed pins the honest
// rendering.
//
// The pre-fix output said "106751d ago" — 292 years, the distance
// from year 1 to now — for every memory that had never been accessed.
func TestReviewRendering_NeverAccessedSaysNeverAccessed(t *testing.T) {
	f := newReviewFixture(t)
	f.seedMemory(t, "untouched", 10, 90, nil)

	out, code := f.run(t, "ops", "review", "--stale")
	if code != 0 {
		t.Fatalf("`mpm ops review --stale` exited %d:\n%s", code, out)
	}
	if !strings.Contains(out, "never accessed") {
		t.Errorf("the review listing must say the memory was never accessed:\n%s", out)
	}
	if strings.Contains(out, "106751d") {
		t.Errorf("the review listing shows the 292-year artifact:\n%s", out)
	}
}

// TestReviewRendering_ShowsStoredWeightAndReinforcementCount pins the
// two labels.
//
// The pre-fix output printed the weight as a DELTA from an unexplained
// threshold of 10, and the reinforcement count as "0x".
func TestReviewRendering_ShowsStoredWeightAndReinforcementCount(t *testing.T) {
	f := newReviewFixture(t)
	f.seedMemory(t, "weighted", 7, 90, nil)

	out, code := f.run(t, "ops", "review", "--stale")
	if code != 0 {
		t.Fatalf("`mpm ops review --stale` exited %d:\n%s", code, out)
	}
	if !strings.Contains(out, "weight 7") {
		t.Errorf("the listing must show the STORED weight (7), not a delta:\n%s", out)
	}
	if !strings.Contains(out, "reinforcement count 0") {
		t.Errorf("the listing must label the reinforcement count:\n%s", out)
	}
}

// TestReviewJSON_NullLastAccessedIsNull pins the machine-readable
// shape: a memory that was never accessed has a null timestamp, not a
// fabricated epoch.
func TestReviewJSON_NullLastAccessedIsNull(t *testing.T) {
	f := newReviewFixture(t)
	f.seedMemory(t, "untouched", 10, 90, nil)

	out, code := f.run(t, "ops", "review", "--stale", "--json")
	if code != 0 {
		t.Fatalf("`mpm ops review --stale --json` exited %d:\n%s", code, out)
	}
	if !strings.Contains(out, `"last_accessed_at":null`) {
		t.Errorf("a never-accessed memory must report a null timestamp:\n%s", out)
	}
	if strings.Contains(out, `"last_accessed_at":0`) {
		t.Errorf("a never-accessed memory must not report a fabricated epoch:\n%s", out)
	}
}

// ── The options are honoured ─────────────────────────────────────────────

// TestReviewHonoursDaysAndLimit pins that --days and --limit actually
// change the result.
//
// The pre-fix handler parsed --limit and then called the query with a
// hardcoded 20.
func TestReviewHonoursDaysAndLimit(t *testing.T) {
	f := newReviewFixture(t)
	f.seedMemory(t, "twenty-days-old", 10, 20, nil)

	// "Due" means the review clock is OLDER than the window, so a
	// 20-day-old memory is overdue under a 14-day window and not
	// yet overdue under a 30-day one.
	if ids := f.dueIDs(t, "--days", "14"); len(ids) != 1 {
		t.Errorf("a 20-day-old memory must be due under a 14-day window: %v", ids)
	}
	if ids := f.dueIDs(t, "--days", "30"); len(ids) != 0 {
		t.Errorf("a 20-day-old memory must not be due under a 30-day window: %v", ids)
	}

	// The limit must bound the listing.
	for _, id := range []string{"a", "b", "c"} {
		f.seedMemory(t, id, 10, 90, nil)
	}
	if ids := f.dueIDs(t, "--limit", "1"); len(ids) != 1 {
		t.Errorf("--limit 1 returned %d memories, want 1: %v", len(ids), ids)
	}
}

// TestReviewDefaultWindowIsTheSubstrateConstant pins that the CLI
// default and the doctor's window are the SAME number.
//
// They used to be 30 and 14. The operator was told to look at a
// different set than the one the doctor had counted.
func TestReviewDefaultWindowIsTheSubstrateConstant(t *testing.T) {
	f := newReviewFixture(t)
	f.seedMemory(t, "twenty-days-old", 10, 20, nil)

	out, code := f.run(t, "ops", "review", "--stale")
	if code != 0 {
		t.Fatalf("`mpm ops review --stale` exited %d:\n%s", code, out)
	}
	if !strings.Contains(out, "window: 14 days") {
		t.Errorf("the review window must be the substrate's constant (14 days):\n%s", out)
	}
}

// ── The acknowledgment ───────────────────────────────────────────────────

// TestReviewAckRejectsAnUnknownDecision pins the decision vocabulary.
func TestReviewAckRejectsAnUnknownDecision(t *testing.T) {
	f := newReviewFixture(t)
	f.seedMemory(t, "candidate", 10, 90, nil)

	out, code := f.run(t, "ops", "review", "--ack", "candidate", "--decision", "bogus")
	if code == 0 {
		t.Errorf("an unknown review decision exited 0:\n%s", out)
	}
	if !strings.Contains(out, "bogus") {
		t.Errorf("the rejection must name the offending decision:\n%s", out)
	}
}

// TestReviewAckAndAckAllAreExclusive pins that the operator cannot
// ask for both at once.
func TestReviewAckAndAckAllAreExclusive(t *testing.T) {
	f := newReviewFixture(t)
	f.seedMemory(t, "candidate", 10, 90, nil)

	out, code := f.run(t, "ops", "review", "--ack", "candidate", "--ack-all")
	if code == 0 {
		t.Errorf("`--ack` together with `--ack-all` exited 0:\n%s", out)
	}
}

// TestReviewAckOfAnUnknownMemoryFails pins that a review of a memory
// that is not there is an error, not a silent no-op.
//
// A silent no-op leaves the operator believing the backlog was cleared.
func TestReviewAckOfAnUnknownMemoryFails(t *testing.T) {
	f := newReviewFixture(t)

	out, code := f.run(t, "ops", "review", "--ack", "no-such-memory")
	if code == 0 {
		t.Errorf("reviewing a memory that does not exist exited 0:\n%s", out)
	}
}

// TestReviewListingDoesNotAcknowledge is the negative control.
//
// Merely DISPLAYING a memory must not count as reviewing it. If it did,
// the backlog would clear itself every time the operator looked at it,
// and the review would be meaningless.
func TestReviewListingDoesNotAcknowledge(t *testing.T) {
	f := newReviewFixture(t)
	f.seedMemory(t, "candidate", 10, 90, nil)

	if _, code := f.run(t, "ops", "review", "--stale"); code != 0 {
		t.Fatalf("`mpm ops review --stale` exited %d", code)
	}
	if ids := f.dueIDs(t); len(ids) != 1 {
		t.Errorf("listing the backlog changed it: %v\n"+
			"Displaying a memory must not count as reviewing it.", ids)
	}
}

// TestReviewKeepDoesNotChangeTheWeight is the negative control for the
// decision.
//
// A "keep" review says "this memory stands". It must not quietly
// reinforce it, promote it or reset its timestamps — the operator
// asked for a decision to be recorded, not for the memory to change.
func TestReviewKeepDoesNotChangeTheWeight(t *testing.T) {
	f := newReviewFixture(t)
	f.seedMemory(t, "candidate", 7, 90, nil)

	before := f.weightOf(t, "candidate")
	if _, code := f.run(t, "ops", "review", "--ack", "candidate", "--decision", "keep"); code != 0 {
		t.Fatalf("the acknowledgment failed")
	}
	after := f.weightOf(t, "candidate")
	if before != after {
		t.Errorf("a `keep` review changed the weight: %d -> %d", before, after)
	}
}

// TestReviewWeakenDecisionWeakens pins that the "weaken" decision
// actually weakens, through the existing WeakenMemory path.
func TestReviewWeakenDecisionWeakens(t *testing.T) {
	f := newReviewFixture(t)
	f.seedMemory(t, "candidate", 7, 90, nil)

	before := f.weightOf(t, "candidate")
	if _, code := f.run(t, "ops", "review", "--ack", "candidate", "--decision", "weaken"); code != 0 {
		t.Fatalf("the acknowledgment failed")
	}
	after := f.weightOf(t, "candidate")
	if after >= before {
		t.Errorf("a `weaken` review did not weaken the memory: %d -> %d", before, after)
	}
}

// weightOf reads a memory's stored weight.
func (f reviewFixture) weightOf(t *testing.T, id string) int {
	t.Helper()
	db, err := sql.Open("sqlite3", filepath.Join(f.ws, "src", "db", "mpm.db"))
	if err != nil {
		t.Fatalf("open the fixture database: %v", err)
	}
	defer db.Close()
	var w int
	if err := db.QueryRow(`SELECT COALESCE(weight, 0) FROM memories WHERE id = ?`, id).Scan(&w); err != nil {
		t.Fatalf("read the weight of %q: %v", id, err)
	}
	return w
}

// ── The whole journey ─────────────────────────────────────────────────────

// TestDoctorReviewJourney is the acceptance test for the workflow the
// doctor recommends.
//
// The doctor says "N memories due"; the operator lists them; the
// operator records the reviews; the doctor says "no memories due".
//
// Every step asserts its own precondition, so a failure names the step
// that broke rather than "the journey failed".
func TestDoctorReviewJourney(t *testing.T) {
	f := newReviewFixture(t)
	f.seedMemory(t, "journey-a", 10, 90, nil)
	f.seedMemory(t, "journey-b", 10, 120, nil)

	// 1. The doctor reports the backlog.
	//
	// The doctor exits nonzero when it has warnings — that is its
	// contract — so the assertion is on the OUTPUT, not on the exit
	// code.
	doctorOut, _ := f.run(t, "doctor")
	if !strings.Contains(doctorOut, "MPM · Doctor") {
		t.Fatalf("`mpm doctor` did not run:\n%s", doctorOut)
	}
	if !strings.Contains(doctorOut, "Review backlog") {
		t.Fatalf("the doctor does not report a review backlog row:\n%s", doctorOut)
	}
	if !strings.Contains(doctorOut, "2 memories due") {
		t.Errorf("the doctor must report the TRUE count (2):\n%s", doctorOut)
	}
	// The recommendation must name the command that actually does the
	// work, and the command that RECORDS the review. A recommendation
	// that only names a lister is the defect this fixes.
	if !strings.Contains(doctorOut, "mpm ops review --stale") {
		t.Errorf("the doctor must name the listing command:\n%s", doctorOut)
	}
	if !strings.Contains(doctorOut, "--ack") {
		t.Errorf("the doctor must name the command that RECORDS a review:\n%s", doctorOut)
	}

	// 2. The operator lists them, and the listing shows both.
	listOut, code := f.run(t, "ops", "review", "--stale")
	if code != 0 {
		t.Fatalf("`mpm ops review --stale` exited %d:\n%s", code, listOut)
	}
	if !strings.Contains(listOut, "journey-a") || !strings.Contains(listOut, "journey-b") {
		t.Errorf("the listing does not show both memories:\n%s", listOut)
	}

	// 3. The operator records the reviews.
	ackOut, code := f.run(t, "ops", "review", "--ack-all")
	if code != 0 {
		t.Fatalf("`mpm ops review --ack-all` exited %d:\n%s", code, ackOut)
	}
	if !strings.Contains(ackOut, "2") {
		t.Errorf("the acknowledgment must report how many were reviewed:\n%s", ackOut)
	}

	// 4. The doctor reports the backlog as CLEARED. This is the step
	// that never happened before: the doctor said "run this to clear
	// the backlog" and the backlog did not move.
	doctorAfter, _ := f.run(t, "doctor")
	row := doctorBacklogRow(t, doctorAfter)
	if strings.Contains(row, "memories due for spaced review") {
		t.Errorf("the backlog did not clear after the reviews were recorded:\n%s", row)
	}
	if !strings.Contains(row, "no memories due for review") {
		t.Errorf("the doctor must report the cleared backlog explicitly:\n%s", row)
	}
}

// doctorBacklogRow extracts the doctor's "Review backlog" row.
//
// The row is asserted on rather than the whole output: the doctor
// prints many rows, and a cleared backlog reads "no memories due for
// review" — which contains the very substring a naive check for "memories
// due" looks for.
func doctorBacklogRow(t *testing.T, out string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "Review backlog") {
			return line
		}
	}
	t.Fatalf("the doctor did not report a review backlog row:\n%s", out)
	return ""
}

// TestDoctorReviewBacklogCountIsATotalNotASample pins that the
// doctor's count is a COUNT.
//
// The pre-fix check took the first 100 candidates and reported
// len(candidates), so a backlog of 160 was reported as "60 due" (the
// sample, truncated by the query's own ordering).
//
// 120 seeded memories is past the old 100-candidate sample limit.
func TestDoctorReviewBacklogCountIsATotalNotASample(t *testing.T) {
	f := newReviewFixture(t)
	for i := 0; i < 120; i++ {
		f.seedMemory(t, "bulk-"+strconv.Itoa(i), 10, 90, nil)
	}

	doctorOut, _ := f.run(t, "doctor")
	if !strings.Contains(doctorOut, "120 memories due") {
		t.Errorf("the doctor must report the TRUE count (120), not a 100-candidate sample:\n%s", doctorOut)
	}
}
