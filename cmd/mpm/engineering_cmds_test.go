// engineering_cmds_test.go — the publication path's own tests.
//
// # WHAT IS UNDER TEST
//
// The store publication is the path the docs call "the right one": the
// assignment store's rows, published with the facts GitHub reports
// about them.
//
// Three things about it are not obvious, and all three were found by
// review rather than by a failure:
//
//   - it must publish an OBSERVED side. A record with no repository, no
//     pull request state and no workflow run cannot be checked by
//     anybody, so a corpus without them is not evidence.
//
//   - it must publish the NEWEST review of a subject. Two reviewers can
//     review one subject, and publishing the older verdict would
//     publish a superseded decision as the current one.
//
//   - it must CHECK a captured digest before it publishes it. The store
//     holds a claim; the publisher is what checks it.
//
// The GitHub side is a LOCAL SERVER, so the whole path is exercised
// without the network and without a credential.

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/flowbyte-com/mpm-core/assignment"
	"github.com/flowbyte-com/mpm-core/evidence"
)

const (
	storeTestRepo = "owner/name"
	storeTestHead = "1111111111111111111111111111111111111111"
	storeTestBase = "2222222222222222222222222222222222222222"
)

// storeGitHub is a local GitHub that answers the reads the publisher
// makes.
type storeGitHub struct {
	server   *httptest.Server
	prCalls  int
	runCalls int
}

func newStoreGitHub(t *testing.T, prState string) *storeGitHub {
	t.Helper()
	g := &storeGitHub{}
	g.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case strings.Contains(path, "/actions/runs/") && strings.HasSuffix(path, "/jobs"):
			writeStoreJSON(w, map[string]interface{}{
				"jobs": []map[string]string{
					{"name": "the suite", "status": "completed", "conclusion": "success"},
				},
			})
		case strings.Contains(path, "/actions/runs"):
			g.runCalls++
			writeStoreJSON(w, map[string]interface{}{
				"workflow_runs": []map[string]interface{}{
					{
						"id": 7, "name": "the suite",
						"head_sha":   storeTestHead,
						"status":     "completed",
						"conclusion": "success",
						"html_url":   "https://github.com/owner/name/actions/runs/7",
					},
				},
			})
		case strings.Contains(path, "/pulls/"):
			g.prCalls++
			writeStoreJSON(w, map[string]interface{}{
				"number": 262, "state": prState, "draft": false, "title": "the work",
				"html_url": "https://github.com/owner/name/pull/262",
				"merged":   prState == "closed",
				"head":     map[string]string{"sha": storeTestHead},
				"base":     map[string]string{"sha": storeTestBase},
				"user":     map[string]string{"login": "flowbyte-com"},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(g.server.Close)
	return g
}

func writeStoreJSON(w http.ResponseWriter, body interface{}) {
	raw, err := json.Marshal(body)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(raw)
}

// storeTestClock is a clock that advances one second per read.
//
// The store stamps every write with it, and the publisher's rule is
// "the newest review wins" — so the test needs two reviews with a
// KNOWN order, and a frozen clock would give them the same stamp.
type storeTestClock struct{ at time.Time }

func (c *storeTestClock) Now() time.Time {
	c.at = c.at.Add(time.Second)
	return c.at
}

// storeFixture builds a tree with one subject and returns the store.
//
// The subject names a pull request, because that is what the publisher
// reads the repository and the facts from.
func storeFixture(t *testing.T, clock assignment.Clock) (*assignment.Store, *assignment.Assignment) {
	t.Helper()
	dm := assignmentCLIFixture(t)
	store := assignment.NewStoreWithClock(dm, clock)
	root, created, err := store.CreateRoot(
		assignment.NewLocalOperatorAuthority("operator"),
		assignment.CreateRootParams{
			ProjectID:           "cpj-cli",
			Title:               "the root",
			Objective:           "the objective",
			Recipient:           "alpha",
			IdempotencyKey:      "idem:pub:root",
			PermittedOperations: []string{"read", "write"},
			EligibleRecipients:  []string{"alpha", "charlie"},
			MaxDepth:            3,
			MaxTasks:            8,
			MaxConcurrency:      2,
			MaxResourceUnits:    100,
		})
	if err != nil || !created {
		t.Fatalf("create the root: %v (created=%v)", err, created)
	}
	subject, created, err := store.Delegate(assignment.DelegateParams{
		ParentID:       root.ID,
		Title:          "the subject",
		Objective:      "the work",
		Recipient:      "alpha",
		Role:           assignment.RoleImplementer,
		IdempotencyKey: "idem:pub:subject",
		IssueRef:       "https://github.com/owner/name/issues/258",
		PRRef:          "https://github.com/owner/name/pull/262",
		BaseSHA:        storeTestBase,
		HeadSHA:        storeTestHead,
	})
	if err != nil || !created {
		t.Fatalf("delegate the subject: %v (created=%v)", err, created)
	}
	return store, subject
}

// storeTestDigest is the digest of some bytes, in the store's own
// vocabulary.
//
// The store's column holds a bare SHA-256 — the evidence package's
// digest carries a "sha256:" prefix — so the test writes what the
// store accepts.
func storeTestDigest(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// storeClient returns a client pointed at the local GitHub.
func storeClient(gh *storeGitHub) *evidence.Client {
	client := evidence.NewClient("")
	client.BaseURL = gh.server.URL
	return client
}

// TestTheStorePublicationCarriesTheObservedFacts proves the store path
// publishes an OBSERVED side.
//
// A record that names no repository, no pull request state and no
// workflow run cannot be checked by anybody — the checks would look in
// "unknown" and find nothing, and the record would say so forever.
func TestTheStorePublicationCarriesTheObservedFacts(t *testing.T) {
	clock := &storeTestClock{at: time.Date(2026, 11, 22, 9, 0, 0, 0, time.UTC)}
	store, subject := storeFixture(t, clock.Now)
	gh := newStoreGitHub(t, "closed")

	corpus, err := corpusFromStore(context.Background(), store, storeClient(gh))
	if err != nil {
		t.Fatalf("build the corpus: %v", err)
	}
	records, err := corpus.Records()
	if err != nil {
		t.Fatalf("the corpus's records: %v", err)
	}
	var rec *evidence.Record
	for _, r := range records {
		if r.TaskID == subject.ID {
			rec = r
		}
	}
	if rec == nil {
		t.Fatalf("the subject's record is missing; the corpus holds %d records", len(records))
	}
	if rec.Repository != storeTestRepo {
		t.Errorf("the record names the repository %q, want %q", rec.Repository, storeTestRepo)
	}
	if rec.GitHub == nil {
		t.Fatal("the record carries no observed pull request")
	}
	if rec.GitHub.State != "merged" {
		t.Errorf("the record's pull request is %q, want %q", rec.GitHub.State, "merged")
	}
	if rec.GitHub.HeadSHA != storeTestHead {
		t.Errorf("the record's head is %q, want %q", rec.GitHub.HeadSHA, storeTestHead)
	}
	if len(rec.CI) != 1 {
		t.Fatalf("the record carries %d workflow runs, want 1", len(rec.CI))
	}
	if rec.CI[0].ID != 7 {
		t.Errorf("the record's workflow run is %d, want 7", rec.CI[0].ID)
	}
	if rec.CI[0].Conclusion != "success" {
		t.Errorf("the record's workflow run concluded %q, want %q",
			rec.CI[0].Conclusion, "success")
	}
	if gh.prCalls == 0 {
		t.Error("the publisher never asked GitHub about the pull request")
	}
	if gh.runCalls == 0 {
		t.Error("the publisher never asked GitHub about the workflow runs")
	}
}

// TestTheStorePublicationCarriesTheNewestReview proves a superseded
// verdict is not published as the current one.
//
// The rows arrive NEWEST FIRST, so a publisher that kept the last one it
// saw would publish the OLDEST review — the opposite of what the
// evidence contract needs.
func TestTheStorePublicationCarriesTheNewestReview(t *testing.T) {
	clock := &storeTestClock{at: time.Date(2026, 11, 22, 9, 0, 0, 0, time.UTC)}
	store, subject := storeFixture(t, clock.Now)

	// Verdicts name host-captured evidence (T84): each reviewer captures
	// its findings before its verdict is recorded.
	capture := func(reviewerID string, epoch int64, tag string) {
		t.Helper()
		body := []byte(`{"findings":"` + tag + `"}`)
		path := filepath.Join(t.TempDir(), "review-"+tag+".json")
		if err := os.WriteFile(path, body, 0o600); err != nil {
			t.Fatalf("write the capture: %v", err)
		}
		if _, err := store.CaptureReviewEvidence(assignment.ReviewEvidenceParams{
			ReviewerID:     reviewerID,
			Epoch:          epoch,
			ReviewedSHA:    storeTestHead,
			FindingsSHA256: storeTestDigest(body),
			EvidenceRef:    path,
			Actor:          assignment.Actor{Address: "charlie", Kind: "agent"},
		}); err != nil {
			t.Fatalf("capture the review evidence: %v", err)
		}
	}

	// The FIRST reviewer refuses the work.
	old, created, err := store.Delegate(assignment.DelegateParams{
		ParentID:       subject.RootID,
		Title:          "the first review",
		Objective:      "review the work",
		Recipient:      "charlie",
		Role:           assignment.RoleReviewer,
		IdempotencyKey: "idem:pub:review-old",
		Payload:        map[string]interface{}{assignment.PayloadKeyReviewTarget: subject.ID},
	})
	if err != nil || !created {
		t.Fatalf("delegate the first reviewer: %v (created=%v)", err, created)
	}
	old = cliAdvance(t, store, old, assignment.StateInProgress)
	capture(old.ID, old.ClaimEpoch, "the first review")
	if _, err := store.RecordReviewResult(assignment.ReviewResultParams{
		ReviewerID: old.ID,
		Verdict:    assignment.VerdictChangesRequested,
		Epoch:      old.ClaimEpoch,
		Actor:      assignment.Actor{Address: "charlie", Kind: "agent"},
	}); err != nil {
		t.Fatalf("record the first verdict: %v", err)
	}

	// The work is finished, and the SECOND reviewer passes it.
	done := cliAdvance(t, store, subject, assignment.StateCompleted)
	fresh, created, err := store.Delegate(assignment.DelegateParams{
		ParentID:       subject.RootID,
		Title:          "the second review",
		Objective:      "review the work again",
		Recipient:      "charlie",
		Role:           assignment.RoleReviewer,
		IdempotencyKey: "idem:pub:review-new",
		Payload:        map[string]interface{}{assignment.PayloadKeyReviewTarget: done.ID},
	})
	if err != nil || !created {
		t.Fatalf("delegate the second reviewer: %v (created=%v)", err, created)
	}
	fresh = cliAdvance(t, store, fresh, assignment.StateInProgress)
	capture(fresh.ID, fresh.ClaimEpoch, "the second review")
	if _, err := store.RecordReviewResult(assignment.ReviewResultParams{
		ReviewerID: fresh.ID,
		Verdict:    assignment.VerdictPassed,
		Epoch:      fresh.ClaimEpoch,
		Actor:      assignment.Actor{Address: "charlie", Kind: "agent"},
	}); err != nil {
		t.Fatalf("record the second verdict: %v", err)
	}

	gh := newStoreGitHub(t, "closed")
	corpus, err := corpusFromStore(context.Background(), store, storeClient(gh))
	if err != nil {
		t.Fatalf("build the corpus: %v", err)
	}
	records, err := corpus.Records()
	if err != nil {
		t.Fatalf("the corpus's records: %v", err)
	}
	var rec *evidence.Record
	for _, r := range records {
		if r.TaskID == done.ID {
			rec = r
		}
	}
	if rec == nil {
		t.Fatalf("the subject's record is missing; the corpus holds %d records", len(records))
	}
	if rec.ReviewVerdict != evidence.VerdictPassed {
		t.Errorf("the record carries the verdict %q, want %q — the OLD review was published",
			rec.ReviewVerdict, evidence.VerdictPassed)
	}
	if rec.Disposition == evidence.DispositionBlocked {
		t.Errorf("the record is %q — a superseded refusal was published as the current one",
			rec.Disposition)
	}
	if rec.Disposition != evidence.DispositionCompleted {
		t.Errorf("the record is %q, want %q", rec.Disposition, evidence.DispositionCompleted)
	}
}

// TestTheStorePublicationChecksTheCapturedDigest proves the publisher
// checks a captured digest instead of copying the store's claim.
//
// The store holds a CLAIM about the reviewer's findings. The publisher
// reads the bytes and hashes them, and a digest that does not match is
// REFUSED — published as the explicit unknown, with the contradiction
// named.
func TestTheStorePublicationChecksTheCapturedDigest(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/review.json"
	body := []byte(`{"findings":"the reviewer said something"}`)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("write the capture: %v", err)
	}
	clock := &storeTestClock{at: time.Date(2026, 11, 22, 9, 0, 0, 0, time.UTC)}
	store, subject := storeFixture(t, clock.Now)

	reviewer, created, err := store.Delegate(assignment.DelegateParams{
		ParentID:       subject.RootID,
		Title:          "the review",
		Objective:      "review the work",
		Recipient:      "charlie",
		Role:           assignment.RoleReviewer,
		IdempotencyKey: "idem:pub:review",
		Payload:        map[string]interface{}{assignment.PayloadKeyReviewTarget: subject.ID},
	})
	if err != nil || !created {
		t.Fatalf("delegate the reviewer: %v (created=%v)", err, created)
	}
	reviewer = cliAdvance(t, store, reviewer, assignment.StateInProgress)
	if _, err := store.CaptureReviewEvidence(assignment.ReviewEvidenceParams{
		ReviewerID:     reviewer.ID,
		Epoch:          reviewer.ClaimEpoch,
		ReviewedSHA:    storeTestHead,
		FindingsSHA256: storeTestDigest(body),
		EvidenceRef:    path,
		Actor:          assignment.Actor{Address: "charlie", Kind: "agent"},
	}); err != nil {
		t.Fatalf("capture the review evidence: %v", err)
	}
	done := cliAdvance(t, store, subject, assignment.StateCompleted)
	reviewer, err = store.Get(reviewer.ID)
	if err != nil {
		t.Fatalf("re-read the reviewer: %v", err)
	}
	if _, err := store.RecordReviewResult(assignment.ReviewResultParams{
		ReviewerID: reviewer.ID,
		Verdict:    assignment.VerdictPassed,
		Epoch:      reviewer.ClaimEpoch,
		Actor:      assignment.Actor{Address: "charlie", Kind: "agent"},
	}); err != nil {
		t.Fatalf("record the verdict: %v", err)
	}

	gh := newStoreGitHub(t, "closed")
	corpus, err := corpusFromStore(context.Background(), store, storeClient(gh))
	if err != nil {
		t.Fatalf("build the corpus: %v", err)
	}
	records, err := corpus.Records()
	if err != nil {
		t.Fatalf("the corpus's records: %v", err)
	}
	var rec *evidence.Record
	var reviewerRec *evidence.Record
	for _, r := range records {
		if r.TaskID == done.ID {
			rec = r
		}
		if r.TaskID == reviewer.ID {
			reviewerRec = r
		}
	}
	if rec == nil {
		t.Fatalf("the subject's record is missing; the corpus holds %d records", len(records))
	}
	if rec.ReviewEvidence == nil {
		t.Fatal("the record carries no review evidence")
	}
	if rec.ReviewEvidence.Status != evidence.CaptureVerified {
		t.Errorf("the capture's status is %q, want %q", rec.ReviewEvidence.Status,
			evidence.CaptureVerified)
	}
	if !evidence.SameDigest(rec.ReviewEvidence.SHA256, storeTestDigest(body)) {
		t.Errorf("the record's digest is %q, want %q",
			rec.ReviewEvidence.SHA256, storeTestDigest(body))
	}
	// THE POINTER IS NOT PUBLISHED.
	//
	// The store recorded the capture where the HOST wrote it, which is
	// an absolute path on this machine. Publishing it would put the
	// operator's disk layout inside the content-addressed bytes, and on
	// a default install the publishable check would then refuse the
	// WHOLE publication. So the pointer is withheld while the digest
	// and the status still say what the publisher established.
	if rec.ReviewEvidence.Ref != evidence.Unknown {
		t.Errorf("the record publishes the host path %q", rec.ReviewEvidence.Ref)
	}
	if err := evidence.AssertPublishable(rec); err != nil {
		t.Errorf("the record is not publishable: %v", err)
	}
	// THE REVIEWER'S OWN RECORD CARRIES THE SAME ANSWER.
	//
	// The publisher checked this digest and published its answer on the
	// subject's record. A reviewer record that said "nothing is known
	// about this capture" would be contradicting the publisher about
	// the same bytes.
	if reviewerRec == nil {
		t.Fatalf("the reviewer's record is missing; the corpus holds %d records", len(records))
	}
	if reviewerRec.ReviewEvidence == nil {
		t.Fatal("the reviewer's record carries no review evidence")
	}
	if reviewerRec.ReviewEvidence.Status != rec.ReviewEvidence.Status {
		t.Errorf("the reviewer's record says the capture is %q and the subject's says %q",
			reviewerRec.ReviewEvidence.Status, rec.ReviewEvidence.Status)
	}
	if reviewerRec.ReviewEvidence.Ref != evidence.Unknown {
		t.Errorf("the reviewer's record publishes the host path %q", reviewerRec.ReviewEvidence.Ref)
	}
}

// TestTheStorePublicationRefusesARefutedDigest proves a digest the
// publisher REFUTED is not published as though it were true.
//
// The store holds a digest; the bytes say something else. The publisher
// reads the bytes, finds the contradiction, and publishes the EXPLICIT
// unknown — with the contradiction named, so a reader knows the digest
// was checked and did not hold.
func TestTheStorePublicationRefusesARefutedDigest(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/review.json"
	body := []byte(`{"findings":"something else entirely"}`)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("write the capture: %v", err)
	}
	clock := &storeTestClock{at: time.Date(2026, 11, 22, 9, 0, 0, 0, time.UTC)}
	store, subject := storeFixture(t, clock.Now)

	reviewer, created, err := store.Delegate(assignment.DelegateParams{
		ParentID:       subject.RootID,
		Title:          "the review",
		Objective:      "review the work",
		Recipient:      "charlie",
		Role:           assignment.RoleReviewer,
		IdempotencyKey: "idem:pub:review-refuted",
		Payload:        map[string]interface{}{assignment.PayloadKeyReviewTarget: subject.ID},
	})
	if err != nil || !created {
		t.Fatalf("delegate the reviewer: %v (created=%v)", err, created)
	}
	reviewer = cliAdvance(t, store, reviewer, assignment.StateInProgress)
	// The store holds a digest for bytes that are NOT these bytes.
	if _, err := store.CaptureReviewEvidence(assignment.ReviewEvidenceParams{
		ReviewerID:     reviewer.ID,
		Epoch:          reviewer.ClaimEpoch,
		ReviewedSHA:    storeTestHead,
		FindingsSHA256: storeTestDigest([]byte("the bytes the store claims")),
		EvidenceRef:    path,
		Actor:          assignment.Actor{Address: "charlie", Kind: "agent"},
	}); err != nil {
		t.Fatalf("capture the review evidence: %v", err)
	}
	done := cliAdvance(t, store, subject, assignment.StateCompleted)
	reviewer, err = store.Get(reviewer.ID)
	if err != nil {
		t.Fatalf("re-read the reviewer: %v", err)
	}
	if _, err := store.RecordReviewResult(assignment.ReviewResultParams{
		ReviewerID: reviewer.ID,
		Verdict:    assignment.VerdictPassed,
		Epoch:      reviewer.ClaimEpoch,
		Actor:      assignment.Actor{Address: "charlie", Kind: "agent"},
	}); err != nil {
		t.Fatalf("record the verdict: %v", err)
	}

	gh := newStoreGitHub(t, "closed")
	corpus, err := corpusFromStore(context.Background(), store, storeClient(gh))
	if err != nil {
		t.Fatalf("build the corpus: %v", err)
	}
	records, err := corpus.Records()
	if err != nil {
		t.Fatalf("the corpus's records: %v", err)
	}
	var rec *evidence.Record
	for _, r := range records {
		if r.TaskID == done.ID {
			rec = r
		}
	}
	if rec == nil {
		t.Fatalf("the subject's record is missing; the corpus holds %d records", len(records))
	}
	if rec.ReviewEvidence == nil {
		t.Fatal("the record carries no review evidence")
	}
	if rec.ReviewEvidence.Status != evidence.CaptureContradicted {
		t.Errorf("the capture's status is %q, want %q", rec.ReviewEvidence.Status,
			evidence.CaptureContradicted)
	}
	if rec.ReviewEvidence.SHA256 != evidence.Unknown {
		t.Errorf("the refuted digest was published: %q", rec.ReviewEvidence.SHA256)
	}
}

// TestTheStorePublicationSaysWhenItIsTruncated proves the truncation is
// published rather than assumed away.
//
// A scan that merely REACHED the bound has dropped nothing, and saying
// that it had would be a false alarm in every report that carries the
// flag.
func TestTheStorePublicationSaysWhenItIsTruncated(t *testing.T) {
	clock := &storeTestClock{at: time.Date(2026, 11, 22, 9, 0, 0, 0, time.UTC)}
	store, _ := storeFixture(t, clock.Now)
	gh := newStoreGitHub(t, "closed")
	corpus, err := corpusFromStore(context.Background(), store, storeClient(gh))
	if err != nil {
		t.Fatalf("build the corpus: %v", err)
	}
	if corpus.ScanLimit != storeScanLimit {
		t.Errorf("the corpus's scan limit is %d, want %d", corpus.ScanLimit, storeScanLimit)
	}
	if corpus.Scanned == 0 {
		t.Error("the corpus does not say how many assignments it read")
	}
	if corpus.Truncated {
		t.Errorf("the corpus says it was truncated after %d rows", corpus.Scanned)
	}
}

// TestTheStorePublicationSaysWhenItDroppedSomething proves the
// truncation flag can be TRUE — and that it is true exactly when
// something was dropped.
//
// The corpus knows whether anything was actually dropped only if it can
// see ONE ROW PAST its bound, and the store clamps every listing to its
// own limit. So the bound has to sit BELOW that limit, and the extra
// row has to be read.
//
// Before this the bound WAS the store's limit: the extra row was
// silently unreachable, the flag could never be true, and a publication
// that held half the queue said it held all of it.
func TestTheStorePublicationSaysWhenItDroppedSomething(t *testing.T) {
	clock := &storeTestClock{at: time.Date(2026, 11, 22, 9, 0, 0, 0, time.UTC)}
	store := storeWithAssignments(t, clock.Now, storeScanLimit+2)
	gh := newStoreGitHub(t, "closed")
	corpus, err := corpusFromStore(context.Background(), store, storeClient(gh))
	if err != nil {
		t.Fatalf("build the corpus: %v", err)
	}
	if !corpus.Truncated {
		t.Errorf("the corpus says it dropped nothing, and the store holds more rows than the bound (%d)",
			storeScanLimit)
	}
	if corpus.Scanned != storeScanLimit {
		t.Errorf("the corpus says it read %d assignments, want %d", corpus.Scanned, storeScanLimit)
	}
	if corpus.ScanLimit != storeScanLimit {
		t.Errorf("the corpus's scan limit is %d, want %d", corpus.ScanLimit, storeScanLimit)
	}
	// And the publication holds the bound's worth, and no more.
	records, err := corpus.Records()
	if err != nil {
		t.Fatalf("the corpus's records: %v", err)
	}
	if len(records) != storeScanLimit {
		t.Errorf("the corpus holds %d records, want %d", len(records), storeScanLimit)
	}
}

// storeWithAssignments builds a store holding a number of assignments.
//
// The tree's ceiling is raised so that the store really holds the rows:
// a test that asked for more than the store could hold would prove
// nothing about the scan.
func storeWithAssignments(t *testing.T, clock assignment.Clock, count int) *assignment.Store {
	t.Helper()
	dm := assignmentCLIFixture(t)
	store := assignment.NewStoreWithClock(dm, clock)
	root, created, err := store.CreateRoot(
		assignment.NewLocalOperatorAuthority("operator"),
		assignment.CreateRootParams{
			ProjectID:           "cpj-cli",
			Title:               "the root",
			Objective:           "the objective",
			Recipient:           "alpha",
			IdempotencyKey:      "idem:pub:many:root",
			PermittedOperations: []string{"read", "write"},
			EligibleRecipients:  []string{"alpha"},
			MaxDepth:            3,
			MaxTasks:            int64(count) + 10,
			MaxConcurrency:      2,
			MaxResourceUnits:    int64(count) + 10,
		})
	if err != nil || !created {
		t.Fatalf("create the root: %v (created=%v)", err, created)
	}
	for i := 0; i < count; i++ {
		_, created, err := store.Delegate(assignment.DelegateParams{
			ParentID:       root.ID,
			Title:          "the work",
			Objective:      "do the thing",
			Recipient:      "alpha",
			Role:           assignment.RoleImplementer,
			IdempotencyKey: fmt.Sprintf("idem:pub:many:%d", i),
			PRRef:          "https://github.com/owner/name/pull/262",
		})
		if err != nil || !created {
			t.Fatalf("delegate %d: %v (created=%v)", i, err, created)
		}
	}
	return store
}

// TestTheFixtureCorpusIsPublishedWithItsObservedFacts proves the FIRST
// corpus — the one the Continuity 001 work itself produced — is
// publishable from an installed binary.
//
// The fixture is EMBEDDED, so the binary carries it; and the publisher
// reads the OBSERVED facts from GitHub at publication time, so a
// fixture cannot state a fact about GitHub even if it wanted to.
func TestTheFixtureCorpusIsPublishedWithItsObservedFacts(t *testing.T) {
	fixture, err := evidence.LoadFixture("continuity-001")
	if err != nil {
		t.Fatalf("the fixture does not load: %v", err)
	}
	if len(fixture.Tasks) != 4 {
		t.Fatalf("the fixture holds %d tasks, want 4", len(fixture.Tasks))
	}
	gh := newStoreGitHub(t, "closed")
	corpus, err := evidence.BuildCorpusFromFixture(context.Background(),
		storeClient(gh), fixture)
	if err != nil {
		t.Fatalf("build the corpus: %v", err)
	}
	records, err := corpus.Records()
	if err != nil {
		t.Fatalf("the corpus's records: %v", err)
	}
	if len(records) != len(fixture.Tasks) {
		t.Fatalf("the corpus holds %d records, want %d", len(records), len(fixture.Tasks))
	}
	for _, rec := range records {
		if rec.GitHub == nil {
			t.Errorf("the record %s carries no observed pull request", rec.TaskID)
			continue
		}
		if rec.Repository == "" || rec.Repository == evidence.Unknown {
			t.Errorf("the record %s names the repository %q", rec.TaskID, rec.Repository)
		}
	}
}

// TestThePublicationHelpNamesEveryOption proves the help page names the
// options the command accepts.
//
// A flag the help does not mention is a flag the operator does not know
// exists.
func TestThePublicationHelpNamesEveryOption(t *testing.T) {
	t.Setenv("MPM_WORKSPACE", t.TempDir())
	code, out := runEngineeringCLI(t, "help")
	if code != 0 {
		t.Fatalf("the help exited with %d: %s", code, out)
	}
	for _, flag := range []string{"--dry-run", "--fixture", "--repo", "--branch", "--json"} {
		if !strings.Contains(out, flag) {
			t.Errorf("the help page does not mention %s:\n%s", flag, out)
		}
	}
}

// TestThePublicationRefusesABranchTheConfigurationDoesNotName proves
// the configured target wins over a flag, and that the flag is not
// SILENTLY ignored.
//
// A write that went to a branch the operator did not ask for would be a
// publication somewhere nobody chose.
func TestThePublicationRefusesABranchTheConfigurationDoesNotName(t *testing.T) {
	workspace := t.TempDir()
	t.Setenv("MPM_WORKSPACE", workspace)
	resetDBSingleton(t)
	config := `{"repository":"owner/name","branch":"evidence"}`
	if err := os.WriteFile(workspace+"/"+evidenceConfigFileName,
		[]byte(config), 0o600); err != nil {
		t.Fatalf("write the configuration: %v", err)
	}
	code, out := runEngineeringCLI(t, "publish", "--dry-run", "--branch", "somewhere-else")
	if code == 0 {
		t.Fatalf("the publication reported success although the branch differs:\n%s", out)
	}
	if !strings.Contains(out, "the configuration wins") {
		t.Errorf("the refusal does not say the configuration wins:\n%s", out)
	}
}
