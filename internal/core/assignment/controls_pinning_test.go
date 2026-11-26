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
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"sort"
	"strconv"
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
	// The harness is read as CODE: a commented-out expectation looks
	// exactly like a control that names a test.
	for _, m := range expectRe.FindAllStringSubmatch(
		stripPythonComments(string(b)), -1) {
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
	for _, m := range breaksRe.FindAllStringSubmatch(
		stripPythonComments(string(b)), -1) {
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
const publishedSelfTestCases = 82

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
		`named = (len(control["expect"]) > 0
                     and len(hit) == len(control["expect"]))`,
		`named = True`,
	},
	{
		// The first question must not be VACUOUS: a control that
		// names no test asks nothing, so a control whose removal
		// detected nothing would be reported as load-bearing.
		"the gate's first question loses its non-vacuity guard",
		`            named = (len(control["expect"]) > 0
                     and len(hit) == len(control["expect"]))`,
		`            named = (len(hit) == len(control["expect"]))`,
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
		`    return ok_proceeded and ok_exited and ok_gate and ok_ran`,
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
		// The INCONCLUSIVE set names the failures that say nothing
		// about the invariant: a build that failed to compile and a
		// run that timed out. Both are excluded from the specific
		// set, and the set is pinned on BOTH members.
		"the inconclusive set's build failure",
		`    "<BUILD FAILED>",`,
		``,
	},
	{
		"the inconclusive set's timeout",
		`    "<TIMEOUT>",`,
		``,
	},
	{
		// The count's own CHECK is pinned by
		// TestControls_TheHarnessCountsItsOwnCases, which removes it.
		// This entry pins the PUBLISHED FIGURE: a copy whose count no
		// longer matches the literal must fail.
		"the published case count",
		`expected_self_test_cases = 82`,
		`expected_self_test_cases = 81`,
	},
	{
		"main()'s use of the baseline verdict",
		`baseline = baseline_verdict(base_failed, base_passed, base_skipped,
                                    base_detail)`,
		`baseline = base_passed`,
	},
	{
		// The FLOOR. "Every control that ran was load-bearing" is
		// only a statement when the set of controls that ran is the
		// set that was supposed to run — and the production path
		// states the floor as every control, unless `--only` chose
		// one.
		"the floor's check removed",
		`    if len(results) != expected_controls:`,
		`    if False:`,
	},
	{
		"the floor's comparison weakened",
		`    if len(results) != expected_controls:`,
		`    if len(results) > expected_controls:`,
	},
	{
		"main()'s floor",
		`    expected_controls = 1 if args.only else len(CONTROLS)`,
		`    expected_controls = 1`,
	},
	{
		// The BASELINE the bound is measured over must be the
		// PUBLISHED suite size. A selection that measured something
		// else moves the published line.
		"the baseline's size check removed",
		`        if baseline != SUITE_SIZE:`,
		`        if False:`,
	},
	{
		"the production path's predicate",
		`    return (proceeded is True and exited is None and gate == 0
            and ran_count == controls)`,
		`    return True`,
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
		`wiring_ok = _wiring_ok(proceeded, exited, gate, ran, want_proceeded,
                               want_exit, want_gate, calls)`,
		`wiring_ok = True`,
	},
	// The MEASUREMENT's arithmetic. It used to be inline in main()'s
	// loop, where nothing but the real controls exercised it — and the
	// real controls were not in anyone's test run. The measurement
	// probe catches these.
	{
		"the measurement's hit",
		`    hit = [t for t in control["expect"] if t in specific]`,
		`    hit = list(control["expect"])`,
	},
	{
		"the measurement's total",
		`    total = len(specific) + passed + len(skipped)`,
		`    total = len(specific) + passed`,
	},
	{
		"the measurement's specific set",
		`    specific = [f for f in failed if f not in INCONCLUSIVE]`,
		`    specific = list(failed)`,
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

// gateMutations is the matrix for the GATE'S OWN VERDICT PATH.
//
// The matrix above covers the summary's mechanisms, and the self-test
// drives the summary directly. main()'s path — the arithmetic that
// turns a run into a measurement, the verdict, the exit code and the
// module-level exit — was exercised by NOTHING in the tree: every
// invocation of the harness used --selftest, which returns before
// main() is ever called. So a mutation there left every test green.
//
// Each entry below must make the harness's --selfcheck-gate mode
// ACCEPT the measurement — that is, exit 0 where it must exit
// non-zero.
var gateMutations = []struct {
	name string
	old  string
	new  string
}{
	{
		"main()'s use of the verdict",
		`    ok = summarise(results, baseline, baseline_skipped, expected_controls)`,
		`    ok = True`,
	},
	{
		"main()'s exit code",
		`    return 0 if ok else 1`,
		`    return 0`,
	},
	{
		"main()'s exit code, inverted",
		`    return 0 if ok else 1`,
		`    return 1 if ok else 0`,
	},
	{
		"the module-level exit",
		`    sys.exit(main())`,
		`    sys.exit(0)`,
	},
}

// TestControls_TheHarnessGateRefusesAndExitsNonZero drives the
// harness's gate in --selfcheck-gate mode, where the suite and the
// controls are stubbed and the gate's verdict is the PROCESS's exit
// code.
//
// The stub control declares a test the stubbed run does NOT fail, so
// the gate must REFUSE. A refusal that exited 0 would be a green
// answer to a measurement that was refused, and the exit code is the
// gate.
//
// And every mutation of the gate's own verdict path must make it
// ACCEPT — that is, exit 0 — or it would not be a mutation of the
// gate at all.
func TestControls_TheHarnessGateRefusesAndExitsNonZero(t *testing.T) {
	root := repoRoot(t)
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is not on PATH; the harness's gate cannot be driven here")
	}
	original, err := os.ReadFile(filepath.Join(root, controlsScript))
	if err != nil {
		t.Fatalf("read the control harness: %v", err)
	}

	// The mode must not touch the tree. Its stubs replace snapshot()
	// and restore(), and the observable consequence is that it leaves
	// NO pristine copy behind — so TMPDIR is a fresh directory and it
	// must still be EMPTY afterwards. And the mode's output must
	// carry the gate's REFUSAL, not the pristine-copy error, so
	// restore() cannot have run either.
	run := func(t *testing.T, script string) (int, string) {
		t.Helper()
		tmp := t.TempDir()
		cmd := exec.Command(python, script, "--selfcheck-gate")
		cmd.Env = []string{
			"PATH=" + os.Getenv("PATH"),
			"HOME=" + t.TempDir(),
			"MPM_WORKSPACE=" + t.TempDir(),
			"TMPDIR=" + tmp,
		}
		out, err := cmd.CombinedOutput()
		if err == nil {
			return 0, string(out)
		}
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("run the harness's gate: %v\n%s", err, out)
		}
		entries, readErr := os.ReadDir(tmp)
		if readErr != nil {
			t.Fatalf("read the harness's TMPDIR: %v", readErr)
		}
		if len(entries) != 0 {
			t.Errorf("the harness's gate mode left %d entr(ies) in TMPDIR "+
				"(%v) — it snapshotted the tree, so its stubs are not in "+
				"place", len(entries), entries)
		}
		return exitErr.ExitCode(), string(out)
	}

	// The unmutated harness: the gate must refuse, and the process
	// must exit non-zero.
	dir := t.TempDir()
	pristine := filepath.Join(dir, "controls.py")
	if err := os.WriteFile(pristine, original, 0o600); err != nil {
		t.Fatalf("write the harness copy: %v", err)
	}
	code, out := run(t, pristine)
	if code == 0 {
		t.Errorf("the harness's gate ACCEPTED a measurement it must refuse, " +
			"and the process exited 0 — the exit code is the gate")
	}
	if !strings.Contains(out, "THE HARNESS FAILED") {
		t.Errorf("the harness's gate mode did not report the gate's refusal "+
			"in its output, so it did not get as far as the gate:\n%s", out)
	}
	if strings.Contains(out, "pristine copy") {
		t.Errorf("the harness's gate mode tried to RESTORE the tree, so its "+
			"restore() stub is not in place:\n%s", out)
	}

	for _, m := range gateMutations {
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
			code, _ := run(t, script)
			if code != 0 {
				t.Errorf("the gate still REFUSED with %s changed, so the "+
					"mutation does not touch the gate's verdict — the matrix "+
					"no longer tests what it claims", m.name)
			}
		})
	}
}

// TestControls_TheHarnessSuiteSizeMatchesTheSuite pins the harness's
// SUITE_SIZE — and the bound's line — against the suite that exists.
//
// SUITE_SIZE is the denominator of the harness's SYNTHETIC fixtures:
// the number of tests the `-run TestAPI` selection runs. If it drifts
// from the real suite, every case that used it is measuring nothing.
//
// The gate's own denominator is the MEASURED baseline, not this
// constant — and the link between the two is the harness's
// SELECTION, which is pinned separately by
// TestControls_TheHarnessSelectionIsTheAcceptanceSuite.
//
// The bound's LINE is pinned with it, and it is pinned as a literal
// against the suite's size rather than derived from the harness's own
// constant — a case that derived its boundary from the constant under
// test moves with it and pins nothing.
func TestControls_TheHarnessSuiteSizeMatchesTheSuite(t *testing.T) {
	root := repoRoot(t)
	b, err := os.ReadFile(filepath.Join(root, controlsScript))
	if err != nil {
		t.Fatalf("read the control harness: %v", err)
	}
	// The acceptance suite's own count, by name.
	count := 0
	for name := range suiteTestNames(t) {
		if strings.HasPrefix(name, "TestAPI") {
			count++
		}
	}
	if count == 0 {
		t.Fatal("the acceptance suite has no TestAPI functions at all, so " +
			"the harness's `-run TestAPI` selection would run nothing")
	}
	for _, c := range []struct {
		constant string
		want     int
	}{
		{"SUITE_SIZE", count},
		{"BOUND_AT_THE_LINE", count / 3},
		{"BOUND_ONE_PAST_THE_LINE", count/3 + 1},
	} {
		declared := harnessConstant(t, string(b), c.constant)
		if declared != c.want {
			t.Errorf("the harness's %s is %d, and the suite has %d "+
				"TestAPI functions — the harness's bound is measured against "+
				"a suite that does not exist", c.constant, declared, c.want)
		}
	}
}

// harnessConstant reads a module-level integer constant out of the
// harness. It is a SCAN, not an evaluation: the harness is Python and
// this is Go, so the constant is read the way it is written.
func harnessConstant(t *testing.T, harness, name string) int {
	t.Helper()
	pattern := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(name) +
		` = ([0-9]+)$`)
	match := pattern.FindStringSubmatch(harness)
	if match == nil {
		t.Fatalf("the harness does not declare %s as a module-level integer "+
			"constant — the pinning test no longer tests what it claims", name)
	}
	n, err := strconv.Atoi(match[1])
	if err != nil {
		t.Fatalf("read the harness's %s: %v", name, err)
	}
	return n
}

// TestControls_EveryLoadBearingControlNamesATest pins the structural
// statement the gate's first question now relies on.
//
// The gate asks "did the NAMED test fail?" — and a control that names
// no test asks nothing: `len(hit) == len(expect)` is `0 == 0`, which
// is True. So a control whose removal detected NOTHING would be
// reported as load-bearing. The gate now refuses a control that names
// no test, and this pins that every load-bearing control DOES name
// one — which is what makes the refusal unreachable in practice.
func TestControls_EveryLoadBearingControlNamesATest(t *testing.T) {
	root := repoRoot(t)
	b, err := os.ReadFile(filepath.Join(root, controlsScript))
	if err != nil {
		t.Fatalf("read the control harness: %v", err)
	}
	// Each control is a flat `{...}` block: a name, an invariant, an
	// expect list, a load_bearing flag and a breaks list. None of them
	// contains a nested brace, so the block is bounded by them.
	blockRe := regexp.MustCompile(`(?s)\{[^{}]*\}`)
	// The regexes are TOLERANT of the spacing around the colons. A
	// strict one silently drops a control whose key was respaced, and
	// a pin that quietly checks fewer controls than it claims is the
	// failure this file exists to prevent.
	nameRe := regexp.MustCompile(`"name"\s*:\s*"([^"]+)"`)
	expectRe := regexp.MustCompile(`"expect"\s*:\s*\[([^\]]*)\]`)
	loadBearingRe := regexp.MustCompile(`"load_bearing"\s*:\s*True`)
	// The CONTROLS LIST is the region the pin measures. The helper
	// blocks elsewhere in the harness — the drive's stub, the gate
	// mode's stub, the measurement probe's control — are NOT controls
	// of the harness, and counting them would make the floor a
	// coincidence instead of a statement.
	region := controlsRegion(t, string(b))
	controls := 0
	seen := 0
	for _, match := range blockRe.FindAllString(region, -1) {
		name := nameRe.FindStringSubmatch(match)
		if name == nil {
			continue
		}
		controls++
		if !loadBearingRe.MatchString(match) {
			continue
		}
		expect := expectRe.FindStringSubmatch(match)
		if expect == nil {
			continue
		}
		seen++
		if strings.TrimSpace(expect[1]) == "" {
			t.Errorf("the load-bearing control %q names NO test, so the "+
				"gate's first question asks nothing about it — a control "+
				"whose removal detected nothing would be reported as "+
				"load-bearing", name[1])
		}
	}
	// The counts are EXACT, not a floor: the harness publishes sixteen
	// controls of which fourteen are load-bearing, and a control that
	// dropped out of the pin — for any reason — must make the pin
	// fail rather than pass over a smaller set.
	if controls != publishedControls {
		t.Errorf("the pin saw %d control(s), and the harness publishes %d — "+
			"a control dropped out of the pin, so the pin no longer covers "+
			"every control", controls, publishedControls)
	}
	if seen != publishedLoadBearingControls {
		t.Errorf("the pin saw %d load-bearing control(s), and the harness "+
			"publishes %d — a control dropped out of the pin, so the pin "+
			"no longer covers every control", seen, publishedLoadBearingControls)
	}
}

// controlsRegion returns the harness's CONTROLS list — the region the
// control pins measure. It is bounded by the assignment and by the
// closing bracket at the start of a line, which is where the list
// ends.
func controlsRegion(t *testing.T, harness string) string {
	t.Helper()
	// The region is read as CODE: a commented-out control would look
	// exactly like a declared one.
	harness = stripPythonComments(harness)
	start := strings.Index(harness, "\nCONTROLS = [\n")
	if start < 0 {
		t.Fatal("the harness does not declare a CONTROLS list at the start of a line")
	}
	rest := harness[start+1:]
	end := strings.Index(rest, "\n]\n")
	if end < 0 {
		t.Fatal("the harness's CONTROLS list has no closing bracket at the start of a line")
	}
	return rest[:end]
}

// stripPythonComments removes the comments from a fragment of Python.
//
// The harness's structure is read as TEXT by several pins, and a
// COMMENT can satisfy a substring or a regex: a commented-out
// `"expect": [...]` looks exactly like a control that names a test, and
// a commented-out command looks exactly like the command the harness
// runs. The seventeenth round's verification found that shape — a dead
// literal satisfying a live-looking pin — so the pins that read the
// harness's structure read its CODE.
//
// The scanner understands single-quoted, double-quoted and triple-quoted
// strings, and escapes, so a '#' inside a string is not a comment.
func stripPythonComments(src string) string {
	const (
		inCode = iota
		inSingle
		inDouble
		inTripleSingle
		inTripleDouble
	)
	state := inCode
	var out strings.Builder
	i := 0
	for i < len(src) {
		c := src[i]
		switch state {
		case inCode:
			switch {
			case c == '#':
				for i < len(src) && src[i] != '\n' {
					i++
				}
			case c == '\'':
				if strings.HasPrefix(src[i:], `'''`) {
					out.WriteString(src[i : i+3])
					i += 3
					state = inTripleSingle
				} else {
					out.WriteByte(c)
					i++
					state = inSingle
				}
			case c == '"':
				if strings.HasPrefix(src[i:], `"""`) {
					out.WriteString(src[i : i+3])
					i += 3
					state = inTripleDouble
				} else {
					out.WriteByte(c)
					i++
					state = inDouble
				}
			default:
				out.WriteByte(c)
				i++
			}
		case inSingle, inDouble:
			if c == '\\' && i+1 < len(src) {
				out.WriteString(src[i : i+2])
				i += 2
				continue
			}
			if (state == inSingle && c == '\'') ||
				(state == inDouble && c == '"') {
				out.WriteByte(c)
				i++
				state = inCode
				continue
			}
			out.WriteByte(c)
			i++
		default: // the triple-quoted states
			if c == '\\' && i+1 < len(src) {
				out.WriteString(src[i : i+2])
				i += 2
				continue
			}
			q := byte('\'')
			if state == inTripleDouble {
				q = '"'
			}
			if strings.HasPrefix(src[i:], strings.Repeat(string(q), 3)) {
				out.WriteString(src[i : i+3])
				i += 3
				state = inCode
				continue
			}
			out.WriteByte(c)
			i++
		}
	}
	return out.String()
}

// publishedLoadBearingControls is the number of controls the harness
// publishes as load-bearing. It is the FLOOR of the pin that checks
// every load-bearing control names a test: a control that dropped out
// of the pin must make the pin fail, not pass over a smaller set.
const publishedLoadBearingControls = 14

// publishedTestFunctions is the number of test functions in the four
// suites this PR adds. It is checked against the tree — a published
// number nobody checks drifts.
const publishedTestFunctions = 77

// publishedControls is the number of controls the harness publishes in
// its CONTROLS list: sixteen, of which fourteen are load-bearing and
// two measure the defence in depth.
const publishedControls = 16

// TestControls_TheHarnessSelectionIsTheAcceptanceSuite pins the
// harness's SELECTION.
//
// The selection decides what the harness measures, and the bound's
// denominator is the MEASURED baseline — so the published line ("a
// third of the baseline") is only meaningful relative to the selection.
// A selection one edit wider moves the line: the same thirteen
// failures are REFUSED at the published line and ACCEPTED one edit
// later, and nothing else in the tree notices.
//
// This is the link between the published suite size and the population
// the gate actually measures, and the link was the hole.
func TestControls_TheHarnessSelectionIsTheAcceptanceSuite(t *testing.T) {
	root := repoRoot(t)
	b, err := os.ReadFile(filepath.Join(root, controlsScript))
	if err != nil {
		t.Fatalf("read the control harness: %v", err)
	}
	pattern := harnessStringConstant(t, string(b), "TEST_PATTERN")
	re, err := regexp.Compile(pattern)
	if err != nil {
		t.Fatalf("the harness's TEST_PATTERN %q does not compile: %v", pattern, err)
	}
	// The acceptance suite: the tests the bound is published against.
	acceptance := acceptanceTestNames(t)
	if len(acceptance) == 0 {
		t.Fatal("the acceptance suite has no tests, so the harness's " +
			"selection cannot be compared with anything")
	}
	names := suiteTestNames(t)
	var selected []string
	for name := range names {
		// `go test -run` matches the pattern UNANCHORED, and the
		// harness's pattern has no "/" in it, so this is the whole
		// of Go's selection semantics for it.
		if re.MatchString(name) {
			selected = append(selected, name)
		}
	}
	sort.Strings(selected)
	sort.Strings(acceptance)
	if !reflect.DeepEqual(selected, acceptance) {
		t.Errorf("the harness's selection %q selects %d test(s), and the "+
			"acceptance suite has %d — the selection is what the bound's "+
			"MEASURED baseline is taken over, so a selection that is "+
			"wider moves the published line and weakens the bound\n"+
			"  selected:   %v\n  acceptance: %v",
			pattern, len(selected), len(acceptance), selected, acceptance)
	}
}

// acceptanceTestNames returns the test functions in the acceptance
// suite — the file that holds the PR's ten properties.
func acceptanceTestNames(t *testing.T) []string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate the test's own source file")
	}
	path := filepath.Join(filepath.Dir(thisFile), "assignment_test.go")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the acceptance suite: %v", err)
	}
	var names []string
	for _, m := range testNameRe.FindAllStringSubmatch(string(b), -1) {
		names = append(names, m[1])
	}
	return names
}

// harnessStringConstant reads a module-level string constant out of
// the harness.
func harnessStringConstant(t *testing.T, harness, name string) string {
	t.Helper()
	pattern := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(name) +
		` = "([^"]*)"$`)
	match := pattern.FindStringSubmatch(harness)
	if match == nil {
		t.Fatalf("the harness does not declare %s as a module-level string "+
			"constant — the pinning test no longer tests what it claims", name)
	}
	return match[1]
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
// TestControls_ThePublishedCountsAreInTheDocuments pins the published
// counts against the DOCUMENTS that state them.
//
// The harness's counts are published in the design note and in the
// SPEC, in ENGLISH WORDS: "Sixteen controls: fourteen are
// load-bearing". A number nobody checks drifts — and here the number
// is in two places at once, the code and the prose, and one of them
// will be right and the other wrong.
//
// This is the link between them, and it is checked from outside the
// harness.
func TestControls_ThePublishedCountsAreInTheDocuments(t *testing.T) {
	root := repoRoot(t)
	note := readDocument(t, filepath.Join(root,
		"docs/dev/continuity-001-assignment-api.md"))
	spec := readDocument(t, filepath.Join(root, "docs/SPEC.md"))

	controls := numberWords(publishedControls)
	loadBearing := numberWords(publishedLoadBearingControls)
	phrase := controls + " controls: " + loadBearing + " are load-bearing"
	for name, doc := range map[string]string{
		"the design note": note,
		"the SPEC":        spec,
	} {
		// The prose is written in sentence case — "Sixteen controls:
		// fourteen are load-bearing" — and the numbers are spelled in
		// upper case. The comparison is case-insensitive so that the
		// pin checks the WORDS, not the typesetting.
		if !strings.Contains(strings.ToUpper(doc), strings.ToUpper(phrase)) {
			t.Errorf("%s does not state %q — the published counts and "+
				"the documents' counts have drifted", name, phrase)
		}
	}

	count := numberWords(publishedSelfTestCases)
	if !strings.Contains(strings.ToUpper(note),
		strings.ToUpper("THE COUNT IS "+count+" CHECKS")) {
		t.Errorf("the design note does not state that the count is %s "+
			"checks — the published figure and the note's have drifted",
			count)
	}

	// The four suites this PR adds, and the number of test functions
	// in them. The count is computed from the tree, not from a
	// constant — a constant that says 65 while the tree holds 64 is
	// a published number nobody checked.
	suites := []string{
		"internal/core/assignment/assignment_test.go",
		"internal/core/tools/assignment_operator_boundary_regression_test.go",
		"cmd/mpm/assignment_cmds_test.go",
		"internal/core/assignment/controls_pinning_test.go",
	}
	total := 0
	for _, rel := range suites {
		total += len(testNameRe.FindAllStringSubmatch(
			readDocument(t, filepath.Join(root, rel)), -1))
	}
	if total != publishedTestFunctions {
		t.Errorf("the four suites hold %d test function(s), and the "+
			"published figure is %d — the published number and the "+
			"tree's have drifted", total, publishedTestFunctions)
	}
	functions := numberWords(publishedTestFunctions)
	if !strings.Contains(strings.ToUpper(note),
		strings.ToUpper("THE FOUR SUITES HOLD "+functions+" TEST FUNCTIONS")) {
		t.Errorf("the design note does not state that the four suites "+
			"hold %s test functions — the published figure and the "+
			"note's have drifted", functions)
	}

	first := numberWords(len(harnessMutations))
	second := numberWords(len(gateMutations))
	matrices := "THE MATRICES ARE " + first + " AND " + second + " ENTRIES"
	if !strings.Contains(strings.ToUpper(note), strings.ToUpper(matrices)) {
		t.Errorf("the design note does not state %q — the published "+
			"matrices and the note's have drifted", matrices)
	}
}

// readDocument reads a document, and fails the test if it cannot.
func readDocument(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// numberWords spells a number the way the documents spell it: in
// English words, in upper case. It covers one to ninety-nine, which is
// every number these documents publish.
//
// A number spelled in digits would be a different document: the prose
// reads "Sixteen controls", not "16 controls", and the pin has to
// check the prose that is actually there.
func numberWords(n int) string {
	ones := []string{
		"ZERO", "ONE", "TWO", "THREE", "FOUR", "FIVE", "SIX", "SEVEN",
		"EIGHT", "NINE", "TEN", "ELEVEN", "TWELVE", "THIRTEEN",
		"FOURTEEN", "FIFTEEN", "SIXTEEN", "SEVENTEEN", "EIGHTEEN",
		"NINETEEN",
	}
	tens := []string{
		"", "", "TWENTY", "THIRTY", "FORTY", "FIFTY", "SIXTY",
		"SEVENTY", "EIGHTY", "NINETY",
	}
	if n < 0 || n > 99 {
		return ""
	}
	switch {
	case n < 20:
		return ones[n]
	case n%10 == 0:
		return tens[n/10]
	default:
		return tens[n/10] + "-" + ones[n%10]
	}
}

// TestControls_TheInvocationAsksForEveryControl pins the INVOCATION.
//
// The floor (round fifteen) guarantees the harness measures every
// control WHEN ASKED TO. One line asks: the Makefile's target.
//
// Nothing pinned that, so a target that asked for ONE control would
// measure one control and pass — the gate's own floor is one when
// `--only` chose a control — and every other gate in the tree would
// stay green: the Python suite, docs_check, the guard suites and the
// harness's own self-test.
//
// So the target must invoke the harness with NO `--only`, and the
// workflow's step must invoke the TARGET.
func TestControls_TheInvocationAsksForEveryControl(t *testing.T) {
	root := repoRoot(t)
	makefile := readDocument(t, filepath.Join(root, "Makefile"))

	// The target's recipe: the lines that run the harness.
	target := makefileTarget(t, makefile, "test-continuity-controls")
	if target == "" {
		t.Fatal("the Makefile has no test-continuity-controls target")
	}
	if !strings.Contains(target, "continuity-001-pr2-controls.py") {
		t.Errorf("the test-continuity-controls target does not run the "+
			"negative-control harness:\n%s", target)
	}
	// `--only` narrows the measurement to one control, and the floor
	// is one when it does — so a target that asked for one control
	// would certify one control and pass.
	if strings.Contains(target, "--only") {
		t.Errorf("the test-continuity-controls target asks for a single "+
			"control (--only), so it measures one control out of %d "+
			"and certifies it:\n%s", publishedControls, target)
	}
	// `--selfcheck-gate` stubs the suite and the controls: it is the
	// gate's own self-check, not the measurement.
	if strings.Contains(target, "--selfcheck-gate") {
		t.Errorf("the test-continuity-controls target runs the "+
			"harness's self-check mode, which stubs the suite and the "+
			"controls, so it measures nothing:\n%s", target)
	}
}

// makefileTarget returns the recipe of a named target.
func makefileTarget(t *testing.T, makefile, name string) string {
	t.Helper()
	lines := strings.Split(makefile, "\n")
	start := -1
	for i, line := range lines {
		if strings.TrimSpace(line) == name+":" {
			start = i
			break
		}
	}
	if start < 0 {
		return ""
	}
	var recipe []string
	for _, line := range lines[start+1:] {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if !strings.HasPrefix(line, "\t") {
			break
		}
		recipe = append(recipe, line)
	}
	return strings.Join(recipe, "\n")
}

// TestControls_TheHarnessRunsTheAcceptanceSuite pins `run_tests`.
//
// `run_tests` is the harness's ENTIRE contact with Go: it builds the
// command, runs it, and parses the output into the four numbers the
// gate reasons about. No test exercised it — ten mutations of it
// survived everything.
//
// It must carry the SELECTION and the PACKAGE: a command that dropped
// `-run` would measure the whole package, and one that changed the
// package path would measure a different suite.
func TestControls_TheHarnessRunsTheAcceptanceSuite(t *testing.T) {
	root := repoRoot(t)
	harness := readDocument(t, filepath.Join(root, controlsScript))

	// The command lives in `run_tests` — the harness's ENTIRE contact
	// with Go. The search is scoped to that function's body: a whole-
	// file search is satisfied by a DEAD LITERAL elsewhere in the file,
	// so the pin would report "the command carries the selection" while
	// the command that actually runs does not.
	body := harnessFunctionBody(t, harness, "run_tests")

	// The command's arguments, as the harness writes them.
	for _, want := range []string{
		`["go", "test", "-tags", "fts5", "-count=1", "-v"`,
		`"-run", TEST_PATTERN, "./assignment/..."]`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("run_tests does not contain %s — the harness's contact "+
				"with Go carries the selection and the package, and a command "+
				"that did not would measure something else", want)
		}
	}
	// The flags that carry the FTS5 build, without which the suite
	// does not even link.
	for _, want := range []string{
		`env["CGO_CFLAGS"] = "-DSQLITE_ENABLE_FTS5=1"`,
		`env["CGO_LDFLAGS"] = "-lm"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("run_tests does not set %s — without it the suite does "+
				"not link", want)
		}
	}
}

// TestControls_EveryControlMutatesInsideTheSelection pins the
// selection's REACH.
//
// The harness's FILES tuple lists every file a control MAY mutate —
// the service, the agent's door, the operator's door, and PR 1's DDL.
// But the acceptance suite the gate measures lives in ONE package, so
// a control that mutated a file outside that package would remove
// code the suite cannot see: the control would measure nothing and
// the gate would report it as load-bearing.
//
// So every control's mutation must land inside the selection's reach.
func TestControls_EveryControlMutatesInsideTheSelection(t *testing.T) {
	root := repoRoot(t)
	harness := readDocument(t, filepath.Join(root, controlsScript))
	defined := harnessDefinedFunctions(harness)

	// The files each control's mutator touches.
	mutators := map[string][]string{}
	for _, m := range regexp.MustCompile(
		`"mutate":\s*(control_\w+)`).FindAllStringSubmatch(harness, -1) {
		fn := m[1]
		mutators[fn] = mutatorFiles(t, harness, defined, fn)
	}
	if len(mutators) == 0 {
		t.Fatal("the harness declares no control mutators, so this pin " +
			"no longer tests what it claims")
	}
	for fn, files := range mutators {
		if len(files) == 0 {
			t.Errorf("the mutator %s touches no file, so this pin "+
				"cannot say where it removes code", fn)
			continue
		}
		for _, f := range files {
			if !strings.HasPrefix(f, "internal/core/assignment/") {
				t.Errorf("the mutator %s removes code from %s, which "+
					"is OUTSIDE the acceptance suite's package — the "+
					"suite cannot see that code, so the control would "+
					"measure nothing and the gate would report it as "+
					"load-bearing", fn, f)
			}
		}
	}
}

// harnessDefinedFunctions returns the names of every function the
// harness defines.
func harnessDefinedFunctions(harness string) map[string]bool {
	out := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)^def ([a-z_][a-z0-9_]*)\(`).
		FindAllStringSubmatch(harness, -1) {
		out[m[1]] = true
	}
	return out
}

// mutatorFiles returns every source file a mutator reaches — its own
// body's literals, and the literals of every harness function it calls,
// TRANSITIVELY.
//
// The seventeenth review found the hole this closes: the pin read the
// mutator's own body, and a mutator could reach a file outside the
// acceptance suite's package through a HELPER, whose body named the
// file. The tree then passed a pin whose own statement was false. So
// the pin follows the calls.
func mutatorFiles(t *testing.T, harness string, defined map[string]bool,
	fn string) []string {
	t.Helper()
	seen := map[string]bool{}
	var files []string
	var walk func(string)
	walk = func(name string) {
		if seen[name] {
			return
		}
		seen[name] = true
		body := harnessFunctionBody(t, harness, name)
		for _, f := range regexp.MustCompile(
			`"((?:internal|cmd)/[^"]*\.go)"`).FindAllStringSubmatch(body, -1) {
			files = append(files, f[1])
		}
		for _, c := range regexp.MustCompile(`\b([a-z_][a-z0-9_]*)\(`).
			FindAllStringSubmatch(body, -1) {
			if defined[c[1]] {
				walk(c[1])
			}
		}
	}
	walk(fn)
	return files
}

// harnessFunctionBody returns the body of a module-level function.
//
// The body is read as CODE: a commented-out line inside a function
// looks exactly like a line the function executes.
func harnessFunctionBody(t *testing.T, harness, name string) string {
	t.Helper()
	harness = stripPythonComments(harness)
	marker := "\ndef " + name + "("
	i := strings.Index(harness, marker)
	if i < 0 {
		t.Fatalf("the harness has no function %s — this pin no longer "+
			"tests what it claims", name)
	}
	rest := harness[i+1:]
	j := strings.Index(rest, "\ndef ")
	if j < 0 {
		return rest
	}
	return rest[:j]
}

// TestControls_TheNotePublishesTheControls ties the note to the
// harness.
//
// The design note publishes every control and the test it must fail —
// a table a reader can check. Nothing tied that table to the
// harness's CONTROLS: a control could be renamed, or its expectation
// changed, and the note would keep publishing the old pair.
func TestControls_TheNotePublishesTheControls(t *testing.T) {
	root := repoRoot(t)
	harness := readDocument(t, filepath.Join(root, controlsScript))
	note := readDocument(t, filepath.Join(root,
		"docs/dev/continuity-001-assignment-api.md"))

	region := controlsRegion(t, harness)
	nameRe := regexp.MustCompile(`"name"\s*:\s*"([^"]+)"`)
	expectRe := regexp.MustCompile(`"expect"\s*:\s*\[([^\]]*)\]`)
	testRe := regexp.MustCompile(`"(Test[A-Za-z0-9_]+)"`)
	published := 0
	for _, block := range regexp.MustCompile(`(?s)\{[^{}]*\}`).
		FindAllString(region, -1) {
		name := nameRe.FindStringSubmatch(block)
		if name == nil {
			continue
		}
		published++
		// The note publishes the control's name.
		if !strings.Contains(note, "`"+name[1]+"`") {
			t.Errorf("the design note does not publish the control "+
				"%q, so the table a reader can check does not "+
				"describe the harness", name[1])
		}
		// And the test it must fail — in the note's OWN ROW for this
		// control, so a row that names a different test is caught rather
		// than satisfied by the same name appearing elsewhere.
		expect := expectRe.FindStringSubmatch(block)
		if expect == nil {
			continue
		}
		row := noteRow(t, note, name[1])
		if row == "" {
			t.Errorf("the design note has no row for the control %q",
				name[1])
			continue
		}
		for _, m := range testRe.FindAllStringSubmatch(expect[1], -1) {
			if !strings.Contains(row, "`"+m[1]+"`") {
				t.Errorf("the design note's row for the control %q "+
					"names %q, and the harness requires %q — the "+
					"table a reader can check does not describe the "+
					"harness", name[1], row, m[1])
			}
		}
	}
	if published != publishedControls {
		t.Errorf("the harness publishes %d control(s) and the pin saw "+
			"%d — a control dropped out of the pin", publishedControls,
			published)
	}
}

// noteRow returns the design note's table row for a control: the
// whole line, so the third cell can be read.
func noteRow(t *testing.T, note, name string) string {
	t.Helper()
	rows := noteControlsRows(t, note)
	cells, ok := rows[name]
	if !ok || len(cells) < 4 {
		return ""
	}
	return strings.TrimSpace(cells[3])
}

// noteControlsRows returns the design note's CONTROL table: the
// control's name, and the cells of its row.
//
// The table is found by its HEADER — "| control | what it removes |"
// — so the note's other tables (the declared exemptions, whose first
// cell is also a control's name) cannot be mistaken for it. Finding
// the first row that merely starts with the control's name was good
// enough while the note had one such table; the seventeenth review's
// finding is that "the table a reader can check" must be THE table.
func noteControlsRows(t *testing.T, note string) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	inTable := false
	for _, line := range strings.Split(note, "\n") {
		cells := strings.Split(line, "|")
		if len(cells) < 4 {
			inTable = false
			continue
		}
		first := strings.TrimSpace(cells[1])
		second := strings.TrimSpace(cells[2])
		if first == "control" && second == "what it removes" {
			inTable = true
			continue
		}
		if !inTable {
			continue
		}
		if strings.HasPrefix(first, "---") {
			continue
		}
		if !strings.HasPrefix(first, "`") || !strings.HasSuffix(first, "`") {
			inTable = false
			continue
		}
		out[strings.Trim(first, "`")] = cells
	}
	return out
}

// noteExemptionRows returns the design note's DECLARED-EXEMPTIONS
// table: the control, and the tests it also breaks.
func noteExemptionRows(t *testing.T, note string) map[string]string {
	t.Helper()
	out := map[string]string{}
	inTable := false
	for _, line := range strings.Split(note, "\n") {
		cells := strings.Split(line, "|")
		if len(cells) < 4 {
			inTable = false
			continue
		}
		first := strings.TrimSpace(cells[1])
		second := strings.TrimSpace(cells[2])
		if first == "control" && second == "it also breaks" {
			inTable = true
			continue
		}
		if !inTable {
			continue
		}
		if strings.HasPrefix(first, "---") {
			continue
		}
		if !strings.HasPrefix(first, "`") || !strings.HasSuffix(first, "`") {
			inTable = false
			continue
		}
		out[strings.Trim(first, "`")] = second
	}
	return out
}

// noteSection returns the text of a section of the design note,
// between two headings.
func noteSection(t *testing.T, note, from, to string) string {
	t.Helper()
	i := strings.Index(note, from)
	if i < 0 {
		t.Fatalf("the design note has no section %q", from)
	}
	j := strings.Index(note[i:], to)
	if j < 0 {
		t.Fatalf("the design note has no section %q after %q", to, from)
	}
	return note[i : i+j]
}

// TestControls_TheNotePublishesTheProperties ties the note's property
// table to the acceptance suite.
//
// §5.1 publishes the ten properties and the tests that carry them —
// the table a reader checks to see what the suite covers. The
// seventeenth review found that nothing tied it to the tree: a test
// could be RENAMED and the table would keep publishing the old name; a
// row could name a test that does not exist; a row could drop a test
// and the suite would grow past the table. None of those was caught by
// anything.
//
// So the table must name EXACTLY the acceptance suite's tests: no
// phantoms, and no orphans.
func TestControls_TheNotePublishesTheProperties(t *testing.T) {
	root := repoRoot(t)
	note := readDocument(t, filepath.Join(root,
		"docs/dev/continuity-001-assignment-api.md"))
	suite := readDocument(t, filepath.Join(root,
		"internal/core/assignment/assignment_test.go"))

	section := noteSection(t, note, "### 5.1", "### 5.2")
	named := map[string]bool{}
	for _, m := range regexp.MustCompile("`(Test[A-Za-z0-9_]+)`").
		FindAllStringSubmatch(section, -1) {
		named[m[1]] = true
	}
	inSuite := testNameRe.FindAllStringSubmatch(suite, -1)
	if len(inSuite) == 0 {
		t.Fatal("the acceptance suite holds no test functions, so this " +
			"pin no longer tests what it claims")
	}
	known := map[string]bool{}
	for _, m := range inSuite {
		known[m[1]] = true
		if !named[m[1]] {
			t.Errorf("the note's property table does not name the test %q, "+
				"so the table a reader checks does not describe the suite "+
				"the gate measures", m[1])
		}
	}
	for name := range named {
		if !known[name] {
			t.Errorf("the note's property table names the test %q, which is "+
				"not in the acceptance suite — the table publishes a test "+
				"that does not exist", name)
		}
	}
}

// TestControls_TheNotePublishesTheExemptions ties the note's declared
// exemptions to the harness.
//
// The note publishes, per control, the EXTRA tests its removal breaks
// — the declaration the gate refuses a control for exceeding. The
// seventeenth review found that nothing tied that table to the
// harness: the table could drop an exemption, or name one the harness
// does not declare, and the published declaration and the real one
// would drift apart.
func TestControls_TheNotePublishesTheExemptions(t *testing.T) {
	root := repoRoot(t)
	note := readDocument(t, filepath.Join(root,
		"docs/dev/continuity-001-assignment-api.md"))
	harness := readDocument(t, filepath.Join(root, controlsScript))

	// What the harness declares, per control.
	declared := map[string][]string{}
	region := controlsRegion(t, harness)
	for _, block := range regexp.MustCompile(`(?s)\{[^{}]*\}`).
		FindAllString(region, -1) {
		name := regexp.MustCompile(`"name":\s*"([^"]+)"`).
			FindStringSubmatch(block)
		if name == nil {
			continue
		}
		m := breaksRe.FindStringSubmatch(block)
		if m == nil {
			continue
		}
		var tests []string
		for _, quoted := range strings.Split(m[1], ",") {
			quoted = strings.TrimSpace(quoted)
			if !strings.HasPrefix(quoted, `"`) {
				continue
			}
			tests = append(tests, strings.Trim(quoted, `"`))
		}
		declared[name[1]] = tests
	}
	published := 0
	for _, tests := range declared {
		if len(tests) > 0 {
			published++
		}
	}
	if published == 0 {
		t.Fatal("the harness declares no exemptions, so this pin no longer " +
			"tests what it claims")
	}

	// What the note publishes.
	rows := noteExemptionRows(t, note)
	for control, tests := range declared {
		if len(tests) == 0 {
			continue
		}
		row, ok := rows[control]
		if !ok {
			t.Errorf("the design note publishes no exemptions for the "+
				"control %q — the gate refuses a control for breaking more "+
				"than it declared, and the declaration is not published",
				control)
			continue
		}
		for _, want := range tests {
			if !strings.Contains(row, "`"+want+"`") {
				t.Errorf("the design note's exemptions for the control %q do "+
					"not name %q, and the harness declares it — the published "+
					"declaration and the harness's have drifted", control, want)
			}
		}
	}
	// And the other direction: what the note publishes, the harness must
	// declare. A one-way check would pass over a control whose
	// declaration had been REMOVED — the pin would simply check less,
	// and say nothing.
	for control, row := range rows {
		named := regexp.MustCompile("`(Test[A-Za-z0-9_]+)`").
			FindAllStringSubmatch(row, -1)
		if len(named) == 0 {
			t.Errorf("the design note publishes an exemptions row for the "+
				"control %q that names no test — a row that declares nothing "+
				"is not a declaration", control)
			continue
		}
		for _, m := range named {
			found := false
			for _, want := range declared[control] {
				if want == m[1] {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("the design note publishes the exemption %q for the "+
					"control %q, and the harness does not declare it — the "+
					"published declaration and the harness's have drifted",
					m[1], control)
			}
		}
	}
}

// TestControls_TheNotePublishesTheInvariant ties a control to the
// invariant it removes, where the harness names one.
//
// The harness's CONTROLS carry an `invariant` field, and four of them
// name PR 1's deferred invariants by their identifiers — N1, N2, N3,
// N6. The note's table said what each control removes in PROSE, and
// nothing tied the prose to the harness's field: a control could be
// re-pointed at a different invariant and the note would keep
// publishing the old pair.
//
// This closes PART of the seventeenth round's nit n2 — the part where
// the invariant has a NAME. The rest is still prose, and §5.20 says so.
func TestControls_TheNotePublishesTheInvariant(t *testing.T) {
	root := repoRoot(t)
	note := readDocument(t, filepath.Join(root,
		"docs/dev/continuity-001-assignment-api.md"))
	harness := readDocument(t, filepath.Join(root, controlsScript))

	rows := noteControlsRows(t, note)
	region := controlsRegion(t, harness)
	seen := 0
	for _, block := range regexp.MustCompile(`(?s)\{[^{}]*\}`).
		FindAllString(region, -1) {
		name := regexp.MustCompile(`"name":\s*"([^"]+)"`).
			FindStringSubmatch(block)
		invariant := regexp.MustCompile(`"invariant":\s*"([^"]+)"`).
			FindStringSubmatch(block)
		if name == nil || invariant == nil {
			continue
		}
		code := regexp.MustCompile(`^(N[0-9]+)\b`).FindStringSubmatch(invariant[1])
		if code == nil {
			continue
		}
		seen++
		cells, ok := rows[name[1]]
		if !ok || len(cells) < 3 {
			t.Errorf("the design note has no row for the control %q", name[1])
			continue
		}
		if !strings.Contains(cells[2], code[1]) {
			t.Errorf("the control %q removes the invariant %q, and the note's "+
				"row does not name %s — the table a reader checks does not "+
				"say which invariant the control removes", name[1],
				invariant[1], code[1])
		}
	}
	if seen == 0 {
		t.Fatal("no control names an invariant by its identifier, so this pin " +
			"no longer tests what it claims")
	}
}

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
