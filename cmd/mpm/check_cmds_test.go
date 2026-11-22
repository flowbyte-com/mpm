// check_cmds_test.go — the operator's read-only door to the engineering
// queue.
//
// # WHAT IS UNDER TEST
//
// The three things the command promises:
//
//   - it reads the PUBLISHED evidence and GitHub, and needs no local
//     database;
//   - it WRITES nothing, to the substrate or to GitHub;
//   - it does not report stale information as current: an index it
//     cannot retrieve is an ERROR, not an empty queue.
//
// The GitHub side is a LOCAL SERVER, so the test proves the whole read
// path without the network and without a credential.

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/flowbyte-com/mpm-core/evidence"
)

// checkFixture is a published corpus and a server that serves it.
type checkFixture struct {
	server *httptest.Server
	files  map[string][]byte
	writes int32
}

// newCheckFixture publishes one task's evidence and serves it.
func newCheckFixture(t *testing.T) *checkFixture {
	t.Helper()
	head := "1111111111111111111111111111111111111111"
	base := "2222222222222222222222222222222222222222"

	tree := evidence.Tree{RootID: "root-1"}
	tree.Tasks = append(tree.Tasks, evidence.Claims{
		ID: "task-a", RootID: "root-1", ProjectID: "continuity-001",
		Title: "the work", Objective: "do the thing",
		Recipient: "mpm-implementer", Role: "implementer", State: "completed",
		PRRef:   "https://github.com/owner/name/pull/262",
		BaseSHA: base, HeadSHA: head,
	})
	pr := &evidence.GitHubFacts{
		Repository: "owner/name", Number: 262, State: "merged", Title: "the work",
		HeadSHA: head, BaseSHA: base, MergeCommit: "3333333333333333333333333333333333333333",
		MergedAt: "2026-11-22T08:59:00Z",
		URL:      "https://github.com/owner/name/pull/262",
	}
	run := evidence.CIRun{
		ID: 7, Name: "build", HeadSHA: head, Status: "completed", Conclusion: "success",
		URL:  "https://github.com/owner/name/actions/runs/7",
		Jobs: []evidence.CIJob{{Name: "test", Status: "completed", Conclusion: "success"}},
	}
	records, err := evidence.Build(tree, map[string]evidence.Facts{
		"task-a": {GitHub: pr, CI: []evidence.CIRun{run}},
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	plan, err := evidence.NewPlan("owner/name", evidence.EvidenceBranch, records,
		time.Date(2026, 11, 22, 9, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	files := map[string][]byte{}
	for _, f := range plan.Files {
		files[f.Path] = f.Content
	}
	fixture := &checkFixture{files: files}
	fixture.server = httptest.NewServer(http.HandlerFunc(fixture.serve))
	t.Cleanup(fixture.server.Close)
	return fixture
}

// serve answers the reader's requests.
func (f *checkFixture) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		atomic.AddInt32(&f.writes, 1)
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	path := r.URL.Path
	switch {
	case strings.HasSuffix(path, "/contents/index.json"):
		f.writeFile(w, evidence.IndexPath)
	case strings.Contains(path, "/contents/records/"):
		f.writeFile(w, strings.TrimPrefix(path[strings.Index(path, "/contents/")+len("/contents/"):], ""))
	case strings.Contains(path, "/actions/runs/") && strings.HasSuffix(path, "/jobs"):
		writeJSON(w, map[string]interface{}{
			"jobs": []map[string]string{
				{"name": "test", "status": "completed", "conclusion": "success"},
			},
		})
	case strings.Contains(path, "/actions/runs"):
		writeJSON(w, map[string]interface{}{
			"workflow_runs": []map[string]interface{}{
				{
					"id": 7, "name": "build",
					"head_sha":   "1111111111111111111111111111111111111111",
					"status":     "completed",
					"conclusion": "success",
					"html_url":   "https://github.com/owner/name/actions/runs/7",
				},
			},
		})
	case strings.Contains(path, "/pulls/"):
		writeJSON(w, map[string]interface{}{
			"number": 262, "state": "closed", "draft": false, "title": "the work",
			"html_url": "https://github.com/owner/name/pull/262",
			"merged":   true, "merged_at": "2026-11-22T08:59:00Z",
			"merge_commit_sha": "3333333333333333333333333333333333333333",
			"head":             map[string]string{"sha": "1111111111111111111111111111111111111111"},
			"base":             map[string]string{"sha": "2222222222222222222222222222222222222222"},
			"user":             map[string]string{"login": "flowbyte-com"},
		})
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// writeFile serves one published file.
func (f *checkFixture) writeFile(w http.ResponseWriter, path string) {
	raw, ok := f.files[path]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	_, _ = w.Write(raw)
}

func writeJSON(w http.ResponseWriter, body interface{}) {
	raw, err := json.Marshal(body)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(raw)
}

// TestCheckReadsThePublishedQueueWithoutADatabase proves the read path.
//
// The workspace points at a directory with no database in it, and the
// command still works: the queue comes from the PUBLISHED evidence, not
// from the substrate.
func TestCheckReadsThePublishedQueueWithoutADatabase(t *testing.T) {
	fixture := newCheckFixture(t)
	t.Setenv("MPM_WORKSPACE", t.TempDir())
	t.Setenv(checkAPIEnv, fixture.server.URL)

	code, out := runCheckCLI(t, "--repo", "owner/name", "--json")
	// The record carries no review evidence, so the corpus is NOT
	// verified, and the exit status says so. The report is printed
	// either way: the reader needs to see why.
	if code != 1 {
		t.Fatalf("the check exited with %d, want 1 (the corpus is not verified): %s", code, out)
	}
	var report struct {
		Source struct {
			Repository string `json:"repository"`
			Retrieved  bool   `json:"retrieved"`
		} `json:"source"`
		Verified bool `json:"verified"`
		Records  []struct {
			TaskID         string `json:"task_id"`
			Disposition    string `json:"disposition"`
			Verdict        string `json:"verdict"`
			DigestVerified bool   `json:"digest_verified"`
		} `json:"records"`
	}
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("the check's JSON does not parse: %v\n%s", err, out)
	}
	if !report.Source.Retrieved {
		t.Error("the check did not retrieve the index")
	}
	if len(report.Records) != 1 {
		t.Fatalf("the check reported %d records, want 1:\n%s", len(report.Records), out)
	}
	if report.Records[0].TaskID != "task-a" {
		t.Errorf("the check reported %q, want task-a", report.Records[0].TaskID)
	}
	if !report.Records[0].DigestVerified {
		t.Error("the record's digest was not verified")
	}
	// The record carries NO review evidence, so the check says so: the
	// verdict is "missing", not "verified". A report that called this
	// verified would be telling a reader that a review happened.
	if report.Records[0].Verdict != evidence.CheckMissing {
		t.Errorf("the record's verdict is %q, want %q; the record carries no review evidence",
			report.Records[0].Verdict, evidence.CheckMissing)
	}
	if report.Verified {
		t.Errorf("the report says everything is verified, and the record's review evidence is missing:\n%s", out)
	}
	// And the missing evidence is VISIBLE: the section says so.
	if !strings.Contains(out, "missing") {
		t.Errorf("the report does not mention the missing evidence:\n%s", out)
	}
}

// TestCheckWritesNothing proves the read path is read-only.
//
// The server counts every non-GET request. There must be none.
func TestCheckWritesNothing(t *testing.T) {
	fixture := newCheckFixture(t)
	t.Setenv("MPM_WORKSPACE", t.TempDir())
	t.Setenv(checkAPIEnv, fixture.server.URL)

	code, out := runCheckCLI(t, "--repo", "owner/name", "--json")
	// The exit status follows the report's verdict (this corpus is not
	// verified), and that is not what this test is about: what matters
	// is that the path wrote nothing.
	if code != 0 && code != 1 {
		t.Fatalf("the check exited with %d: %s", code, out)
	}
	if writes := atomic.LoadInt32(&fixture.writes); writes != 0 {
		t.Errorf("the check performed %d writes; it must perform none", writes)
	}
}

// TestCheckRefusesWhenTheIndexCannotBeRetrieved proves the condition is
// reported, not hidden.
//
// An unreachable index is an ERROR. The command must not fall back to
// the last thing it saw and call it current.
func TestCheckRefusesWhenTheIndexCannotBeRetrieved(t *testing.T) {
	t.Setenv("MPM_WORKSPACE", t.TempDir())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()
	t.Setenv(checkAPIEnv, server.URL)

	code, out := runCheckCLI(t, "--repo", "owner/name", "--json")
	if code == 0 {
		t.Fatal("the check reported success although the index could not be retrieved")
	}
	if !strings.Contains(out, "could not be retrieved") {
		t.Errorf("the error does not say the index could not be retrieved:\n%s", out)
	}
	if strings.Contains(out, "queue is empty") {
		t.Errorf("the check reported an empty queue instead of the failure:\n%s", out)
	}
}

// TestCheckHelpIsInert proves the help page mutates nothing.
func TestCheckHelpIsInert(t *testing.T) {
	t.Setenv("MPM_WORKSPACE", t.TempDir())
	code, out := runCheckCLI(t, "--help")
	if code != 0 {
		t.Fatalf("the help exited with %d: %s", code, out)
	}
	if !strings.Contains(out, "engineering queue") {
		t.Errorf("the help page does not describe the command:\n%s", out)
	}
}

// TestEngineeringPublishRefusesWithoutAConfiguredTarget proves the
// publication authority is explicit.
//
// A missing configuration is NOT a default: the publisher refuses rather
// than inventing a repository to write to.
func TestEngineeringPublishRefusesWithoutAConfiguredTarget(t *testing.T) {
	t.Setenv("MPM_WORKSPACE", t.TempDir())
	code, out := runEngineeringCLI(t, "publish", "--dry-run")
	if code == 0 {
		t.Fatal("the publication reported success although no target was configured")
	}
	if !strings.Contains(out, "publication configuration") {
		t.Errorf("the error does not say the configuration is missing:\n%s", out)
	}
}

// TestTheCheckCountsNameTheSetTheyDescribe proves the numbers on the
// report's line add up.
//
// The publication holds RecordCount records, the filters left
// ShownCount of them, and the states are the SHOWN ones — so a reader
// who filtered to one project sees the states of the records they are
// looking at, and the count of the whole publication beside it.
//
// Before this the two populations shared one line and one separator, so
// a reader could add the states up against the wrong count.
func TestTheCheckCountsNameTheSetTheyDescribe(t *testing.T) {
	record := &evidence.VerifiedRecord{
		Record: &evidence.Record{
			TaskID: "task-a", Role: "implementer",
			Disposition: evidence.DispositionCompleted,
		},
		Verdict: evidence.CheckFailed,
	}
	report := &evidence.CheckReport{
		Source:      evidence.Source{Repository: "owner/name", Branch: "evidence"},
		RecordCount: 3,
		ShownCount:  1,
		Records:     []*evidence.VerifiedRecord{record},
		Sections: []evidence.Section{{
			Disposition: evidence.DispositionCompleted,
			Records:     []*evidence.VerifiedRecord{record},
		}},
	}
	out := renderCheckReport(report)
	for _, want := range []string{"3 published", "showing 1", "1 failed"} {
		if !strings.Contains(out, want) {
			t.Errorf("the report does not say %q:\n%s", want, out)
		}
	}
	// The states are the SHOWN ones, so they add up to the shown count
	// and not to the published one.
	for _, unwanted := range []string{"2 failed", "3 failed"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("the report counts records the filters hid (%q):\n%s", unwanted, out)
		}
	}
}

// TestTheCheckExitStatusAgreesWithTheReport proves a script that reads
// the exit status is told what the report says.
//
// Before this, `check` exited 0 while its own mark said the corpus was
// not verified: a CI gate passed while the evidence it was gating said it
// did not hold.
func TestTheCheckExitStatusAgreesWithTheReport(t *testing.T) {
	if code := checkExitCode(&evidence.CheckReport{Verified: true}); code != 0 {
		t.Errorf("a verified report's exit status is %d, want 0", code)
	}
	if code := checkExitCode(&evidence.CheckReport{Verified: false}); code != 1 {
		t.Errorf("an unverified report's exit status is %d, want 1", code)
	}
	if code := checkExitCode(nil); code != 1 {
		t.Errorf("a missing report's exit status is %d, want 1", code)
	}
	// And the CLI itself: the fixture's corpus has no review evidence,
	// so the report is not verified, and the command says so.
	fixture := newCheckFixture(t)
	t.Setenv("MPM_WORKSPACE", t.TempDir())
	t.Setenv(checkAPIEnv, fixture.server.URL)
	code, out := runCheckCLI(t, "--repo", "owner/name")
	if code != 1 {
		t.Fatalf("the check exited with %d, want 1:\n%s", code, out)
	}
}

// TestTheCheckReportSaysWhenThePublicationIsPartial proves the truncation
// reaches the human report.
//
// The index says the publication holds part of the work; a report that
// dropped the flag would show a partial queue as the whole one.
func TestTheCheckReportSaysWhenThePublicationIsPartial(t *testing.T) {
	report := &evidence.CheckReport{
		Source:      evidence.Source{Repository: "owner/name", Branch: "evidence"},
		RecordCount: 500,
		ShownCount:  500,
		Truncated:   true,
		Scanned:     500,
		ScanLimit:   499,
	}
	out := renderCheckReport(report)
	for _, want := range []string{"partial", "499", "more work"} {
		if !strings.Contains(out, want) {
			t.Errorf("the report does not say %q:\n%s", want, out)
		}
	}
	// A whole publication says nothing about a partial one.
	whole := renderCheckReport(&evidence.CheckReport{
		Source:      evidence.Source{Repository: "owner/name", Branch: "evidence"},
		RecordCount: 1,
		ShownCount:  1,
	})
	if strings.Contains(whole, "partial") {
		t.Errorf("a whole publication reports itself as partial:\n%s", whole)
	}
}

// TestAFilterThatMatchesNothingIsNotAnEmptyQueue proves the report does
// not say the queue is empty when a filter simply matched nothing.
//
// "The queue is empty" is a claim about the PUBLICATION. A filter that
// matched nothing — including a typo — says nothing about the queue's
// contents.
func TestAFilterThatMatchesNothingIsNotAnEmptyQueue(t *testing.T) {
	report := &evidence.CheckReport{
		Source:      evidence.Source{Repository: "owner/name", Branch: "evidence"},
		Verified:    true,
		RecordCount: 3,
		ShownCount:  0,
	}
	out := renderCheckReport(report)
	if strings.Contains(out, "The queue is empty") {
		t.Errorf("a filtered view with no matches says the queue is empty:\n%s", out)
	}
	for _, want := range []string{"No records match the filters", "holds 3"} {
		if !strings.Contains(out, want) {
			t.Errorf("the report does not say %q:\n%s", want, out)
		}
	}
	// An EMPTY publication is a different fact, and it is said.
	empty := renderCheckReport(&evidence.CheckReport{
		Source: evidence.Source{Repository: "owner/name", Branch: "evidence"},
	})
	if !strings.Contains(empty, "holds no records") {
		t.Errorf("an empty publication is not said to be empty:\n%s", empty)
	}
}

// TestTheReportSaysWhenTheSnapshotIDDoesNotMatch proves the snapshot id
// the report prints is the one it checked.
func TestTheReportSaysWhenTheSnapshotIDDoesNotMatch(t *testing.T) {
	report := &evidence.CheckReport{
		Source:         evidence.Source{Repository: "owner/name", Branch: "evidence", SnapshotID: "sha256:aaaa"},
		RecordCount:    1,
		ShownCount:     1,
		SnapshotDetail: "the index names the snapshot sha256:aaaa, and the records it lists hash to sha256:bbbb",
	}
	out := renderCheckReport(report)
	if !strings.Contains(out, "records it lists hash to sha256:bbbb") {
		t.Errorf("the report prints the snapshot id without saying it does not match:\n%s", out)
	}
}

// TestAFilteredViewSaysWhatItDoesNotShow proves the human report
// explains a ⚠ whose cause the filter excluded.
func TestAFilteredViewSaysWhatItDoesNotShow(t *testing.T) {
	report := &evidence.CheckReport{
		Source:            evidence.Source{Repository: "owner/name", Branch: "evidence"},
		RecordCount:       2,
		ShownCount:        1,
		UnverifiedOutside: 1,
	}
	out := renderCheckReport(report)
	if !strings.Contains(out, "outside this view are not verified") {
		t.Errorf("the report shows ⚠ with no reason:\n%s", out)
	}
}

// TestTheArtefactProblemIsShownEvenWhenNoRecordMatches proves a broken
// artefact is printed on the zero-match path.
//
// A reader whose filter matched nothing must still learn that the
// summary does not match the index: it is as much the reason for the ⚠
// as a broken record.
func TestTheArtefactProblemIsShownEvenWhenNoRecordMatches(t *testing.T) {
	report := &evidence.CheckReport{
		Source:      evidence.Source{Repository: "owner/name", Branch: "evidence"},
		RecordCount: 2,
		ShownCount:  0,
		Renderings: []evidence.RenderingVerification{{
			Path:   evidence.QueueSummaryPath,
			Detail: "the queue summary is not the rendering of the records under the index's snapshot and truncation fields",
		}},
	}
	out := renderCheckReport(report)
	if !strings.Contains(out, "No records match the filters") {
		t.Errorf("the report does not say the filter matched nothing:\n%s", out)
	}
	if !strings.Contains(out, "artefacts not verified") {
		t.Errorf("the report hides the broken artefact behind the empty view:\n%s", out)
	}
	if !strings.Contains(out, evidence.QueueSummaryPath) {
		t.Errorf("the report does not name the broken artefact:\n%s", out)
	}
}

// runCheckCLI runs `mpm check` and captures its output.
//
// The CLI's runner pipes stdout and stderr into ONE buffer, so the error
// text and the output text arrive together.
func runCheckCLI(t *testing.T, argv ...string) (int, string) {
	t.Helper()
	return runCLI(t, append([]string{"check"}, argv...)...)
}

// runEngineeringCLI runs `mpm engineering` and captures its output.
func runEngineeringCLI(t *testing.T, argv ...string) (int, string) {
	t.Helper()
	return runCLI(t, append([]string{"engineering"}, argv...)...)
}

// TestTheFixtureLoadsFromTheCommandLine proves the command can LOAD the
// fixture, not merely that the file is where the package says it is.
//
// The fixture is EMBEDDED, so an installed binary publishes the corpus
// its source names — a path that could point somewhere else is a path
// that eventually will. The old assertion — that the file exists in the
// SOURCE tree — was true of a binary that could not read it.
func TestTheFixtureLoadsFromTheCommandLine(t *testing.T) {
	fixture, err := evidence.LoadFixture("continuity-001")
	if err != nil {
		t.Fatalf("the fixture does not load: %v", err)
	}
	if len(fixture.Tasks) != 4 {
		t.Errorf("the fixture holds %d tasks, want 4", len(fixture.Tasks))
	}
	for _, task := range fixture.Tasks {
		if task.PRRef == "" {
			t.Errorf("the task %s names no pull request", task.TaskID)
		}
	}
	// And the corpus it yields builds records — the publication path
	// reads the OBSERVED facts from GitHub, and a fixture that cannot
	// be published is not a corpus.
	corpus, err := evidence.BuildCorpusFromFixture(context.Background(), nil, fixture)
	if err == nil {
		t.Error("the corpus was built without a GitHub reader")
	}
	if corpus != nil {
		t.Error("the corpus was built without a GitHub reader")
	}
}
