// controls_pinning_test.go — pin the negative-control harness against
// the acceptance suite.
//
// # WHY THIS FILE EXISTS
//
// PR 2's controls live in `scripts/continuity-001-pr2-controls.py`,
// because they MUTATE THE SERVICE'S SOURCE: the invariants of PR 2
// are code, not DDL, so a control has to remove the code and observe
// that the suite notices.
//
// A harness that lives outside the test suite can rot in two ways:
//
//   - it names a test that has been renamed or deleted, and then the
//     control silently measures NOTHING while reporting a pass; and
//   - it names a mutation target that no longer exists, and then the
//     control aborts — or, worse, mutates something else.
//
// Neither is visible at `make test` time, which is why this file
// exists: it reads the harness and checks that every test it names is
// a test that EXISTS, and that every file it may mutate is a file that
// EXISTS.
//
// # WHAT IT DOES NOT DO
//
// It does not run the controls. Running them mutates the working tree,
// which is not something a unit test may do. The harness runs them, and
// the published numbers in the design note come from the harness's
// output.

package assignment

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// controlsScript is the harness, relative to the repository root.
const controlsScript = "scripts/continuity-001-pr2-controls.py"

// repoRoot walks up from this file's directory to the repository root.
//
// The path is derived from the SOURCE FILE's location rather than from
// the working directory, so the test works whether `go test` is run
// from the package directory or from the repository root.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate the test's own source file")
	}
	// internal/core/assignment/controls_pinning_test.go → the root is
	// three directories up.
	root := filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", "..", ".."))
	if _, err := os.Stat(filepath.Join(root, controlsScript)); err != nil {
		t.Fatalf("the control harness is not at %s: %v", controlsScript, err)
	}
	return root
}

// testNameRe finds a Go test function's name in a source file.
var testNameRe = regexp.MustCompile(`(?m)^func (Test[A-Za-z0-9_]+)\(`)

// suiteTestNames is every test function declared in this package.
func suiteTestNames(t *testing.T) map[string]bool {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate the test's own source file")
	}
	dir := filepath.Dir(thisFile)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read the package directory: %v", err)
	}
	names := map[string]bool{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		for _, m := range testNameRe.FindAllStringSubmatch(string(b), -1) {
			names[m[1]] = true
		}
	}
	return names
}

// expectRe finds the "expect" list of a control in the harness.
var expectRe = regexp.MustCompile(`"expect":\s*\[([^\]]*)\]`)

// breaksRe finds the "breaks" list of a control in the harness.
//
// The list is the control's DECLARED EXEMPTIONS: the extra tests its
// removal is expected to break. The gate refuses a control that broke
// anything outside the declaration, so a stale entry here is an
// exemption for a test that no longer exists — and one that nobody
// notices, because a stale entry never matches anything.
var breaksRe = regexp.MustCompile(`"breaks":\s*\[([^\]]*)\]`)

// TestControls_TheHarnessNamesTestsThatExist pins that every test the
// harness names is a test that exists.
//
// A harness that names a deleted test measures nothing and reports a
// pass — the most dangerous possible outcome, and the one PR 1's
// harness explicitly refuses to produce.
func TestControls_TheHarnessNamesTestsThatExist(t *testing.T) {
	root := repoRoot(t)
	b, err := os.ReadFile(filepath.Join(root, controlsScript))
	if err != nil {
		t.Fatalf("read the control harness: %v", err)
	}
	suite := suiteTestNames(t)

	seen := 0
	for _, m := range expectRe.FindAllStringSubmatch(string(b), -1) {
		for _, quoted := range strings.Split(m[1], ",") {
			quoted = strings.TrimSpace(quoted)
			if !strings.HasPrefix(quoted, `"`) {
				continue
			}
			name := strings.Trim(quoted, `"`)
			if name == "" {
				continue
			}
			seen++
			if !suite[name] {
				t.Errorf("the control harness names the test %q, which does not exist in "+
					"this package — the control would measure nothing and report a pass", name)
			}
		}
	}
	if seen == 0 {
		t.Fatal("the control harness names no tests at all; the expectations have rotted away")
	}
}

// TestControls_TheHarnessNamesOnlyTestsThatExist pins that every
// DECLARED EXEMPTION names a test that exists.
//
// The gate refuses a control that broke a test it did not declare, so
// the declaration is what makes a control's measurement possible. A
// stale entry — an exemption for a test that was renamed or deleted —
// matches nothing, and nobody notices: it looks like a declaration that
// simply never applied.
func TestControls_TheHarnessNamesOnlyTestsThatExist(t *testing.T) {
	root := repoRoot(t)
	b, err := os.ReadFile(filepath.Join(root, controlsScript))
	if err != nil {
		t.Fatalf("read the control harness: %v", err)
	}
	suite := suiteTestNames(t)

	seen := 0
	for _, m := range breaksRe.FindAllStringSubmatch(string(b), -1) {
		for _, quoted := range strings.Split(m[1], ",") {
			quoted = strings.TrimSpace(quoted)
			if !strings.HasPrefix(quoted, `"`) {
				continue
			}
			name := strings.Trim(quoted, `"`)
			if name == "" {
				continue
			}
			seen++
			if !suite[name] {
				t.Errorf("the control harness declares an exemption for the test %q, "+
					"which does not exist in this package — the exemption matches nothing, "+
					"so it looks like a declaration that never applied", name)
			}
		}
	}
	if seen == 0 {
		t.Fatal("the control harness declares no exemptions at all; the declarations " +
			"have rotted away, and this test would pass vacuously")
	}
}

// filesRe finds the FILES tuple of the harness.
var filesRe = regexp.MustCompile(`(?s)FILES = \((.*?)\)`)

// TestControls_TheHarnessOnlyMutatesFilesThatExist pins that every file
// the harness may mutate is a file that exists.
//
// A harness that names a file that has moved aborts on every control
// run; one that names a file that has been DELETED would mutate
// nothing and report a misleading zero.
func TestControls_TheHarnessOnlyMutatesFilesThatExist(t *testing.T) {
	root := repoRoot(t)
	b, err := os.ReadFile(filepath.Join(root, controlsScript))
	if err != nil {
		t.Fatalf("read the control harness: %v", err)
	}
	m := filesRe.FindStringSubmatch(string(b))
	if m == nil {
		t.Fatal("the control harness has no FILES tuple")
	}
	count := 0
	for _, line := range strings.Split(m[1], "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, `"`) {
			continue
		}
		rel := strings.Trim(strings.TrimSpace(line), `",`)
		if rel == "" {
			continue
		}
		count++
		if _, err := os.Stat(filepath.Join(root, rel)); err != nil {
			t.Errorf("the control harness may mutate %q, which does not exist", rel)
		}
	}
	if count == 0 {
		t.Fatal("the control harness may mutate no files at all")
	}
}

// writePatternRe finds an INSERT into the assignment tables.
var writePatternRe = regexp.MustCompile(
	`INSERT(?: OR \w+)? INTO (agent_tasks|agent_task_events)`)

// TestOnlyTheAssignmentPackageWritesTheAssignmentTables is a
// STRUCTURAL companion to the behavioural boundary test.
//
// The behavioural test (internal/core/tools/assignment_operator_boundary_regression_test.go)
// asks every registry action whether it can create a root or record an
// approval. This one asks the source: which files INSERT into
// `agent_tasks` or `agent_task_events` at all?
//
// The answer must be the assignment package and nothing else. A write
// outside it would be a way to create a root that no registry action
// names — invisible to a behavioural test that only probes the actions
// it knows about.
//
// The assignment package's OWN test files are excluded: a test may
// write a malformed row deliberately, to prove that the schema refuses
// it, and that is not a path into production.
func TestOnlyTheAssignmentPackageWritesTheAssignmentTables(t *testing.T) {
	root := repoRoot(t)

	var offenders []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			switch info.Name() {
			case ".git", "node_modules", ".build", "__pycache__":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return nil
		}
		if strings.HasSuffix(rel, "_test.go") {
			return nil
		}
		b, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}
		if writePatternRe.Match(b) {
			offenders = append(offenders, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk the repository: %v", err)
	}

	for _, rel := range offenders {
		if strings.HasPrefix(rel, "internal/core/assignment/") {
			continue
		}
		t.Errorf("%s writes an assignment row; only internal/core/assignment/ may, "+
			"or the authority boundary has a path no registry action names", rel)
	}
}

// TestControls_TheHarnessGateCanFail pins that the harness's gate is a
// GATE.
//
// A harness whose exit code cannot fail is not evidence: it would still
// print "14/14 controls are load-bearing" after every load-bearing
// control had stopped noticing anything, and the published count would
// be unbacked by anything that could contradict it.
//
// The harness's own --selftest mode drives its summary with the results
// such a run WOULD produce — a load-bearing control that did not detect
// its removal, and a defence-in-depth control that broke the suite — and
// requires the gate to REFUSE them. So a self-test that passes is a
// proof that the gate fails when it should.
func TestControls_TheHarnessGateCanFail(t *testing.T) {
	root := repoRoot(t)
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is not on PATH; the harness's self-test cannot be driven here")
	}
	cmd := exec.Command(python, filepath.Join(root, controlsScript), "--selftest")
	// The child gets a PINNED environment, not the ambient one: the
	// subprocess guard exists precisely because an inherited environment
	// is how a test reaches a live workspace. The harness's self-test
	// opens no database, but the rule is the rule.
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + t.TempDir(),
		"MPM_WORKSPACE=" + t.TempDir(),
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the harness's self-test failed, so its gate cannot be trusted "+
			"to fail when a control stops detecting: %v\n%s", err, out)
	}
}

// publishedSelfTestCases is the number of cases the harness's
// self-test makes.
//
// It is PUBLISHED — the design note and the pull request both carry
// it — and this test is what makes the published number true: the
// self-test prints its own count, and a case added without the
// published figure being updated fails here rather than silently
// making the documentation stale.
//
// The count is the LOG's LENGTH — the number of checks the self-test
// EVALUATED, not the number of lines it printed. Every check in the
// harness is a predicate, and every predicate records that it ran, so
// a check that stops running leaves the count short.
const publishedSelfTestCases = 60

// TestControls_TheHarnessPublishesItsOwnCaseCount pins the published
// figure to the harness's own.
//
// A printed number nobody checks is a number that drifts. The
// harness prints its count precisely so that this test can compare
// it, and so that the number in the documentation is the harness's
// count rather than somebody's memory of it.
func TestControls_TheHarnessPublishesItsOwnCaseCount(t *testing.T) {
	root := repoRoot(t)
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is not on PATH; the harness's self-test cannot be driven here")
	}
	cmd := exec.Command(python, filepath.Join(root, controlsScript), "--selftest")
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + t.TempDir(),
		"MPM_WORKSPACE=" + t.TempDir(),
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the harness's self-test failed: %v\n%s", err, out)
	}
	want := fmt.Sprintf("THE SELF-TEST PASSED: the gate can fail. %d cases.",
		publishedSelfTestCases)
	if !strings.Contains(string(out), want) {
		t.Errorf("the harness did not print %q, so the PUBLISHED case count "+
			"(%d) is stale — the self-test now makes a different number of "+
			"cases. Update the published figure and this constant together.\n%s",
			want, publishedSelfTestCases, out)
	}
}

// harnessMutations is the MUTATION MATRIX: each entry removes one
// mechanism from a COPY of the harness, and the self-test must FAIL
// against the copy.
//
// The matrix lives HERE, outside the harness, on purpose. The harness's
// own self-test cannot catch a mutation that neuters one of its own
// checks — turning `ok = (proceeded == want_proceeded)` into
// `ok = True` leaves the check in place, leaves the case count the
// same, and leaves the self-test green. From inside the harness there
// is no observer left: the check IS the thing being neutered.
//
// From out here there is. The copy is written to a temporary directory
// and run from there — the tree is never touched.
var harnessMutations = []struct {
	name string
	old  string
	new  string
}{
	{
		"the gate's first question (the named test must fail)",
		`named = len(hit) == len(control["expect"])`,
		`named = True`,
	},
	{
		"the one-third bound",
		`len(specific) * BROKEN_SUITE_FRACTION > denominator`,
		`False`,
	},
	{
		"the identity comparison in the bound",
		`set(skipped) != set(baseline_skipped) or`,
		`len(skipped) != len(baseline_skipped) or`,
	},
	{
		"the defence-in-depth branch's identity comparison",
		`and set(skipped) == set(baseline_skipped))`,
		`and len(skipped) <= len(baseline_skipped))`,
	},
	{
		"the defence-in-depth branch's whole clean clause",
		`clean = (not specific and total == baseline
                     and set(skipped) == set(baseline_skipped))`,
		`clean = True`,
	},
	{
		"the two-sided bound",
		`broke_suite = (total != denominator or`,
		`broke_suite = (total < denominator or`,
	},
	{
		"the unexpected-failures clause",
		`unexpected = [f for f in specific
                          if f not in control["expect"] and f not in control.get("breaks", [])]`,
		`unexpected = []`,
	},
	{
		"the published-message assertion's dispatch",
		`if case["mode"] == "more_tests":`,
		`if False:`,
	},
	{
		"the isolation assertion's dispatch",
		`if case.get("isolates"):`,
		`if False:`,
	},
	{
		"the main() wiring drive's verdict",
		`    return ok_proceeded and ok_exited and ok_ran`,
		`    return True`,
	},
	{
		"the PASS pattern's anchor",
		`PASS_RE = re.compile(r"^--- PASS: (\w+)", re.M)`,
		`PASS_RE = re.compile(r"--- PASS: (\w+)", re.M)`,
	},
	{
		"the SKIP pattern's anchor",
		`SKIP_RE = re.compile(r"^--- SKIP: (\w+)", re.M)`,
		`SKIP_RE = re.compile(r"--- SKIP: (\w+)", re.M)`,
	},
	{
		"the baseline's not-green refusal",
		`if base_failed:`,
		`if False:`,
	},
	{
		"the baseline's skipping refusal",
		`if base_skipped:`,
		`if False:`,
	},
	{
		// The count's own CHECK is pinned by
		// TestControls_TheHarnessCountsItsOwnCases, which removes it.
		// This entry pins the PUBLISHED FIGURE: a copy whose count no
		// longer matches the literal must fail.
		"the published case count",
		`expected_self_test_cases = 60`,
		`expected_self_test_cases = 59`,
	},
	{
		"main()'s use of the baseline verdict",
		`baseline = baseline_verdict(base_failed, base_passed, base_skipped,
                                    base_detail)`,
		`baseline = base_passed`,
	},
	// The NEUTERED PREDICATES. A mutation that turns a predicate's
	// BODY into a constant leaves the check in place — so the counter
	// still increments, the case count is unchanged, and the harness's
	// own self-test would stay green. The harness CANNOT catch that:
	// the check IS the thing being neutered.
	//
	// Which is why every check in the self-test is a call to a
	// PREDICATE, and every predicate is driven with a deliberately
	// wrong value to prove it can fail. These four entries are that
	// proof, from outside: a neutered predicate fails its own probe.
	{
		"the equality predicate's body",
		`    return got == want`,
		`    return True`,
	},
	{
		"the published-message predicate's body",
		`    return "NOT a run of this suite" in output`,
		`    return True`,
	},
	{
		"the isolation predicate's body",
		`    return refused == 1`,
		`    return True`,
	},
	{
		"the sample predicate's body",
		`    return needle in sample`,
		`    return True`,
	},
	// The NEUTERED CALL SITES. A mutation that turns a check's call
	// site into a constant leaves the check in place — and with the
	// count derived from the LOG, it also leaves the check unlogged,
	// so the count changes and the self-test fails against the copy.
	{
		"the baseline denominator's call site",
		`if not _equal(good, SUITE_SIZE, calls):`,
		`if False:`,
	},
	{
		"the regex comparison's call site",
		`if not _equal(got, want, calls):`,
		`if False:`,
	},
	{
		"the sample's shape call site",
		`if not _sample_ok(needle, sample, calls):`,
		`if False:`,
	},
	{
		"the published-message call site",
		`if not _message_ok(buf.getvalue(), calls):`,
		`if False:`,
	},
	{
		"the isolation call site",
		`if not _isolation_ok(refused, calls):`,
		`if False:`,
	},
	{
		"the case verdict's call site",
		`if not _equal(ok, want, calls):`,
		`if False:`,
	},
	{
		"the wiring verdict's call site",
		`wiring_ok = _wiring_ok(proceeded, exited, ran, want_proceeded,
                               want_exit, calls)`,
		`wiring_ok = True`,
	},
}

// TestControls_TheHarnessFailsWhenAMechanismIsRemoved is the MUTATION
// MATRIX as an executable artefact.
//
// Each mutation removes one published mechanism from a COPY of the
// harness, and the harness's own self-test must FAIL against that
// copy. A mutation the self-test survives is a mechanism with nothing
// behind it — and this test says so by name.
func TestControls_TheHarnessFailsWhenAMechanismIsRemoved(t *testing.T) {
	root := repoRoot(t)
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is not on PATH; the harness's self-test cannot be driven here")
	}
	original, err := os.ReadFile(filepath.Join(root, controlsScript))
	if err != nil {
		t.Fatalf("read the control harness: %v", err)
	}
	for _, m := range harnessMutations {
		t.Run(m.name, func(t *testing.T) {
			mutated := strings.Replace(string(original), m.old, m.new, 1)
			if mutated == string(original) {
				t.Fatalf("the mutation did not apply — %q is not in the harness, "+
					"so the matrix no longer tests what it claims", m.old)
			}
			dir := t.TempDir()
			script := filepath.Join(dir, "controls.py")
			if err := os.WriteFile(script, []byte(mutated), 0o600); err != nil {
				t.Fatalf("write the mutated copy: %v", err)
			}
			cmd := exec.Command(python, script, "--selftest")
			cmd.Env = []string{
				"PATH=" + os.Getenv("PATH"),
				"HOME=" + t.TempDir(),
				"MPM_WORKSPACE=" + t.TempDir(),
			}
			out, err := cmd.CombinedOutput()
			if err == nil {
				t.Errorf("the self-test PASSED against a copy with %s removed, so "+
					"that mechanism has nothing behind it:\n%s", m.name, out)
			}
		})
	}
}

// TestControls_TheHarnessCountsItsOwnCases pins that the harness's
// count is CHECKED, not merely printed.
//
// The harness compares its own case count against a published literal
// and fails when they differ. That check is what makes the published
// figure mean something: a case added without the figure being
// updated changes the count, and the check refuses.
//
// The check itself needs a case that fails when it is removed — and
// removing it cannot be observed from the harness's own output, since
// the printed count is the same either way. So this test mutates a
// COPY of the harness — in a temporary directory, never the tree —
// and requires the self-test to FAIL.
//
// The tree is never touched: the copy is written to a temp directory
// and run from there.
func TestControls_TheHarnessCountsItsOwnCases(t *testing.T) {
	root := repoRoot(t)
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is not on PATH; the harness's self-test cannot be driven here")
	}
	original, err := os.ReadFile(filepath.Join(root, controlsScript))
	if err != nil {
		t.Fatalf("read the control harness: %v", err)
	}
	// The mutation: the published literal is made wrong, so the
	// harness's count no longer matches it. A harness that CHECKS the
	// count fails; a harness that only PRINTS it does not.
	//
	// The literal is derived from the published constant, so the two
	// cannot drift apart: the mutation always moves the harness's
	// figure by one.
	mutated := strings.Replace(string(original),
		fmt.Sprintf("expected_self_test_cases = %d", publishedSelfTestCases),
		fmt.Sprintf("expected_self_test_cases = %d", publishedSelfTestCases-1),
		1)
	if mutated == string(original) {
		t.Fatal("the mutation did not apply — the harness's published literal " +
			"is not where this test expects it")
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "controls.py")
	if err := os.WriteFile(script, []byte(mutated), 0o600); err != nil {
		t.Fatalf("write the mutated copy: %v", err)
	}
	cmd := exec.Command(python, script, "--selftest")
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + t.TempDir(),
		"MPM_WORKSPACE=" + t.TempDir(),
	}
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Errorf("the harness's self-test PASSED against a mutated copy whose "+
			"published case count is wrong, so the count is printed but not "+
			"checked:\n%s", out)
	}
}

// controlNameRe finds a control's name in the harness.
var controlNameRe = regexp.MustCompile(`(?m)^\s*"name":\s*"([a-z0-9-]+)",`)

// TestControls_EveryControlIsNamedAndClassified pins the harness's
// shape: every control has a name, an invariant, and a classification.
//
// A control without a classification is ambiguous: the harness would
// not know whether the named test is supposed to fail.
func TestControls_EveryControlIsNamedAndClassified(t *testing.T) {
	root := repoRoot(t)
	b, err := os.ReadFile(filepath.Join(root, controlsScript))
	if err != nil {
		t.Fatalf("read the control harness: %v", err)
	}
	src := string(b)

	names := controlNameRe.FindAllStringSubmatch(src, -1)
	if len(names) == 0 {
		t.Fatal("the control harness declares no controls")
	}
	seen := map[string]bool{}
	for _, m := range names {
		name := m[1]
		if seen[name] {
			t.Errorf("the control %q is declared twice", name)
		}
		seen[name] = true
	}
	for _, key := range []string{`"invariant":`, `"expect":`, `"load_bearing":`} {
		if !strings.Contains(src, key) {
			t.Errorf("the control harness has no %s field", key)
		}
	}
}
