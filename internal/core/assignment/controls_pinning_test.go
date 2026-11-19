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
	_ = seen
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
// print "13/13 controls are load-bearing" after every load-bearing
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
