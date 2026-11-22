package evidence

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// THE NEGATIVE CONTROLS.
//
// A control REMOVES an invariant from the implementation and requires the
// acceptance suite to FAIL. A control that removes nothing measures
// nothing, and a suite that does not notice is a suite that does not
// test the invariant.
//
// # WHY THEY ARE IN GO AND NOT IN A SEPARATE HARNESS
//
// PR 2's controls live in a Python harness because they mutate a package
// the acceptance suite observes from another module. This package is
// OBSERVABLE BY ITSELF, and it depends on nothing but the standard
// library — so a control can copy the package into a temporary module,
// mutate it there, and run the suite against the copy.
//
// The copy needs no network and no module cache, which means the control
// runs anywhere the suite runs.
//
// # WHAT A CONTROL PROVES
//
// It proves the named acceptance test is load-bearing: with the
// invariant removed, that test fails. If the test still passes, the
// invariant was not being tested, and the control says so.

// control is one negative control.
type control struct {
	// name is the control's name.
	name string
	// invariant is what the control removes.
	invariant string
	// file is the file the mutation lands in.
	file string
	// from is the text the mutation replaces.
	from string
	// to is the text it replaces it with.
	to string
	// test is the acceptance test that must then FAIL.
	test string
}

// The controls.
//
// Every mutation is a REMOVAL of a refusal: a stale head that would be
// accepted, a failed job that would be called green, a forged verdict
// that would be believed, a pointer written before the records it names,
// and a record whose bytes do not match its name.
var controls = []control{
	{
		name:      "stale-head acceptance",
		invariant: "a head that advanced invalidates the review of the old one",
		file:      "verify.go",
		from:      "out.add(CheckReviewedSHA, CheckStale,",
		to:        "out.add(CheckReviewedSHA, CheckVerified,",
		test:      "TestANewerHeadInvalidatesTheReviewedHeadClaim",
	},
	{
		name:      "false CI success",
		invariant: "a failed job is not a green run",
		file:      "build.go",
		from:      "if job.Conclusion != \"success\" {",
		to:        "if false {",
		test:      "TestAFailedOrPendingCIJobCannotBeGreen",
	},
	{
		name:      "forged review approval",
		invariant: "a verdict that no reviewer recorded is not a verdict",
		file:      "verify.go",
		from:      "out.add(CheckReviewVerdict, CheckFailed, fmt.Sprintf(\n\t\t\t\"the record carries the verdict %q but its review disposition is %q\",",
		to:        "out.add(CheckReviewVerdict, CheckVerified, fmt.Sprintf(\n\t\t\t\"the record carries the verdict %q but its review disposition is %q\",",
		test:      "TestAForgedReviewVerdictIsRefused",
	},
	{
		name:      "prohibited write order",
		invariant: "the pointer is published after the records it names",
		file:      "publish.go",
		from:      "\tfor _, f := range plan.Files {\n\t\tif err := p.Writer.PutFile(ctx, f.Path, f.Content); err != nil {",
		to:        "\tfor i := len(plan.Files) - 1; i >= 0; i-- {\n\t\tf := plan.Files[i]\n\t\tif err := p.Writer.PutFile(ctx, f.Path, f.Content); err != nil {",
		test:      "TestAnInterruptedPublicationLeavesThePointerAlone",
	},
	{
		name:      "a record's own digest",
		invariant: "a record's bytes must hash to the identifier the index gave them",
		file:      "check.go",
		from:      "digestOK := SameDigest(DigestBytes(recordRaw), entry.ID)",
		to:        "digestOK := true",
		test:      "TestARecordWhoseBytesDoNotMatchItsNameIsRefused",
	},
	{
		name:      "the observed side is screened",
		invariant: "third-party text from GitHub must not reach the published bytes",
		file:      "build.go",
		from:      "\tr.redactObserved(rec.GitHub, rec.CI)",
		to:        "\t_ = r.Withheld // the observed side is not screened",
		test:      "TestTheObservedSideIsScreenedLikeTheClaims",
	},
	{
		name:      "a finished task with no verdict",
		invariant: "work with no verdict is waiting for a reviewer, not for CI",
		file:      "build.go",
		from:      "\t\t\treturn DispositionAwaitingReview",
		to:        "\t\t\treturn DispositionAwaitingCI",
		test:      "TestAFinishedTaskWithNoVerdictIsAwaitingAReview",
	},
	{
		name:      "the oldest review is published",
		invariant: "a superseded verdict must not be published as the current one",
		file:      "build.go",
		from:      "\t\tout[rev.ReviewTarget] = info",
		to:        "\t\tif _, seen := out[rev.ReviewTarget]; !seen {\n\t\t\tout[rev.ReviewTarget] = info\n\t\t}",
		test:      "TestTheNewestReviewOfASubjectIsPublished",
	},
	{
		name:      "the newest candidate is not chosen",
		invariant: "the newest review of a task is the one that is published",
		file:      "build.go",
		from:      "\t\treturn a.UpdatedAt > b.UpdatedAt",
		to:        "\t\treturn a.UpdatedAt < b.UpdatedAt",
		test:      "TestTheNewestCandidateWinsOverTheTreesOwn",
	},
	{
		name:      "a refuted digest is published",
		invariant: "a digest the publisher REFUTED must not be published as true",
		file:      "build.go",
		from:      "\t\tif rec.ReviewEvidence.Status == CaptureContradicted {",
		to:        "\t\tif false {",
		test:      "TestARefutedDigestIsNotPublished",
	},
	{
		name:      "the ledger's account of a review is lost",
		invariant: "two accounts of ONE review are merged, not chosen between",
		file:      "build.go",
		from:      "\tif candidate.Verdict != \"\" {\n\t\tout.Verdict = candidate.Verdict\n\t\tout.LocallyReported = candidate.LocallyReported\n\t}",
		to:        "\tif false {\n\t\tout.Verdict = candidate.Verdict\n\t\tout.LocallyReported = candidate.LocallyReported\n\t}",
		test:      "TestTheLedgerAccountOfAReviewIsNotLostToTheTreesOwn",
	},
	{
		name:      "a finished review is filed as waiting for a review",
		invariant: "a reviewer's own record is filed by the review, not by the subject's rules",
		file:      "build.go",
		from:      "\tif r.Role == \"reviewer\" {\n\t\tif finishedState(r.State) {\n\t\t\treturn DispositionCompleted\n\t\t}",
		to:        "\tif r.Role == \"reviewer\" {\n\t\tif finishedState(r.State) {\n\t\t\treturn DispositionAwaitingReview\n\t\t}",
		test:      "TestAFinishedReviewIsNotFiledAsWaitingForAReview",
	},
}

// TestTheNegativeControlsAreLoadBearing runs every control.
//
// Each one copies the package, removes one invariant, and requires the
// named acceptance test to FAIL. A control that does not make its test
// fail is reported as NOT LOAD-BEARING, and the test fails with it.
func TestTheNegativeControlsAreLoadBearing(t *testing.T) {
	for _, c := range controls {
		c := c
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			copyPackage(t, dir)
			mutate(t, dir, c)
			out, code := runSuite(t, dir, c.test)
			if code == 0 {
				t.Fatalf("NOT LOAD-BEARING: removing %s left %s PASSING\n%s",
					c.invariant, c.test, out)
			}
			if !strings.Contains(out, "--- FAIL: "+c.test) {
				t.Fatalf("the control removed %s, but %s is not the test that failed:\n%s",
					c.invariant, c.test, out)
			}
		})
	}
}

// copyPackage copies the package's sources into a temporary module.
//
// The fixtures come with them: the acceptance suite reads one, and a
// control that broke the suite by removing a fixture would measure
// nothing.
func copyPackage(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read the package: %v", err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") {
			continue
		}
		raw, err := os.ReadFile(entry.Name())
		if err != nil {
			t.Fatalf("read %s: %v", entry.Name(), err)
		}
		if err := os.WriteFile(filepath.Join(dir, entry.Name()), raw, 0o600); err != nil {
			t.Fatalf("write %s: %v", entry.Name(), err)
		}
	}
	fixtures, err := os.ReadDir("fixtures")
	if err == nil {
		if err := os.MkdirAll(filepath.Join(dir, "fixtures"), 0o750); err != nil {
			t.Fatalf("make the fixtures directory: %v", err)
		}
		for _, entry := range fixtures {
			raw, err := os.ReadFile(filepath.Join("fixtures", entry.Name()))
			if err != nil {
				t.Fatalf("read the fixture: %v", err)
			}
			if err := os.WriteFile(filepath.Join(dir, "fixtures", entry.Name()), raw, 0o600); err != nil {
				t.Fatalf("write the fixture: %v", err)
			}
		}
	}
	// A MODULE of its own, with no dependencies: the package imports
	// nothing but the standard library, so the copy needs no network
	// and no module cache.
	mod := "module evidencecontrol\n\ngo 1.26\n"
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(mod), 0o600); err != nil {
		t.Fatalf("write the go.mod: %v", err)
	}
}

// mutate applies the control's mutation to the copied package.
func mutate(t *testing.T, dir string, c control) {
	t.Helper()
	path := filepath.Join(dir, c.file)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", c.file, err)
	}
	body := string(raw)
	if strings.Count(body, c.from) != 1 {
		t.Fatalf("the control's target is not in %s (%d matches, expected exactly 1); "+
			"a control that cannot find its target measures nothing",
			c.file, strings.Count(body, c.from))
	}
	body = strings.Replace(body, c.from, c.to, 1)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", c.file, err)
	}
}

// runSuite runs one acceptance test in the copied package.
func runSuite(t *testing.T, dir string, test string) (string, int) {
	t.Helper()
	cmd := exec.Command("go", "test", "-count=1", "-run", "^"+test+"$", ".")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GOFLAGS=-mod=mod",
		"GOPROXY=off",
		"GOWORK=off",
	)
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			code = exit.ExitCode()
		} else {
			t.Fatalf("run the suite: %v", err)
		}
	}
	return string(out), code
}
