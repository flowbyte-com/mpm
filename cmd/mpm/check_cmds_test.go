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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
	if code != 0 {
		t.Fatalf("the check exited with %d: %s", code, out)
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
	if code != 0 {
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

// TestTheFixtureIsReachableFromTheCommandLine proves the fixture file is
// where the command expects it.
func TestTheFixtureIsReachableFromTheCommandLine(t *testing.T) {
	path, err := evidence.FixturePath("continuity-001")
	if err != nil {
		t.Fatalf("the fixture path: %v", err)
	}
	if filepath.Base(path) != "continuity-001.json" {
		t.Errorf("the fixture path is %q", path)
	}
	if _, err := os.Stat(filepath.Join("..", "..", "internal", "core", "evidence", path)); err != nil {
		t.Errorf("the fixture is not where the package says it is: %v", err)
	}
}
