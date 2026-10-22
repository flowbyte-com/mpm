// update_test.go — contracts for the opt-in update check.
//
// The check answers one question: is the commit this binary was built
// from behind the upstream main branch, and by how much? The answer must
// come from VERIFIED ancestry (the GitHub comparison API computes it from
// the repository's commit graph) — never from comparing SHA strings,
// timestamps or version numbers.
//
// Every network outcome must degrade to a clean "unknown" rather than a
// guess. These tests use httptest servers; nothing here reaches the
// public internet.
package buildinfo

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const testCommit = "06c029ba11ce18c7f10aa60b0f0497ece2edda7c"

// upstreamServer serves a canned git-ref response and a canned compare
// response, recording the paths it was asked for.
func upstreamServer(t *testing.T, upstreamSHA string, compareStatus string, aheadBy, behindBy int) (*httptest.Server, *[]string) {
	t.Helper()
	var seen []string

	mux := http.NewServeMux()
	mux.HandleFunc("/repos/"+defaultTestRepository+"/git/ref/heads/main", func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"ref":"refs/heads/main","object":{"sha":%q,"type":"commit"}}`, upstreamSHA)
	})
	mux.HandleFunc("/repos/"+defaultTestRepository+"/compare/", func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"status":%q,"ahead_by":%d,"behind_by":%d,"total_commits":%d}`,
			compareStatus, aheadBy, behindBy, aheadBy+behindBy)
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &seen
}

func newTestChecker(base string) Checker {
	return Checker{
		BaseURL:    base,
		Repository: defaultTestRepository,
		HTTPClient: &http.Client{Timeout: 2 * time.Second},
		Now:        func() time.Time { return time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC) },
	}
}

func TestCheck_UpstreamAheadMeansInstalledIsBehind(t *testing.T) {
	// GitHub reports "ahead" when the COMPARE HEAD (upstream main) is
	// ahead of the base (the installed commit). The installed binary is
	// therefore behind by ahead_by.
	srv, _ := upstreamServer(t, "aaaaaaa1111111", "ahead", 3, 0)

	res := newTestChecker(srv.URL).Check(context.Background(), testCommit)

	if res.Status != UpdateBehind {
		t.Fatalf("Status = %q, want %q", res.Status, UpdateBehind)
	}
	if res.BehindBy != 3 {
		t.Errorf("BehindBy = %d, want 3", res.BehindBy)
	}
	if res.AheadBy != 0 {
		t.Errorf("AheadBy = %d, want 0", res.AheadBy)
	}
	if res.UpstreamCommit != "aaaaaaa1111111" {
		t.Errorf("UpstreamCommit = %q, want %q", res.UpstreamCommit, "aaaaaaa1111111")
	}
}

func TestCheck_UpstreamBehindMeansInstalledIsAhead(t *testing.T) {
	srv, _ := upstreamServer(t, "bbbbbbb2222222", "behind", 0, 2)

	res := newTestChecker(srv.URL).Check(context.Background(), testCommit)

	if res.Status != UpdateAhead {
		t.Fatalf("Status = %q, want %q", res.Status, UpdateAhead)
	}
	if res.AheadBy != 2 {
		t.Errorf("AheadBy = %d, want 2", res.AheadBy)
	}
	if res.BehindBy != 0 {
		t.Errorf("BehindBy = %d, want 0", res.BehindBy)
	}
}

func TestCheck_IdenticalMeansCurrent(t *testing.T) {
	srv, _ := upstreamServer(t, testCommit, "identical", 0, 0)

	res := newTestChecker(srv.URL).Check(context.Background(), testCommit)

	if res.Status != UpdateCurrent {
		t.Fatalf("Status = %q, want %q", res.Status, UpdateCurrent)
	}
	if res.BehindBy != 0 || res.AheadBy != 0 {
		t.Errorf("current build must report 0/0, got behind=%d ahead=%d", res.BehindBy, res.AheadBy)
	}
}

func TestCheck_DivergedReportsBothDistances(t *testing.T) {
	srv, _ := upstreamServer(t, "ccccccc3333333", "diverged", 4, 2)

	res := newTestChecker(srv.URL).Check(context.Background(), testCommit)

	if res.Status != UpdateDiverged {
		t.Fatalf("Status = %q, want %q", res.Status, UpdateDiverged)
	}
	// GitHub's ahead_by counts commits head is ahead of base; head is
	// upstream main, so the installed commit is BEHIND by that many.
	if res.BehindBy != 4 {
		t.Errorf("BehindBy = %d, want 4", res.BehindBy)
	}
	if res.AheadBy != 2 {
		t.Errorf("AheadBy = %d, want 2", res.AheadBy)
	}
}

func TestCheck_UnknownInstalledCommitMakesNoNetworkCall(t *testing.T) {
	srv, seen := upstreamServer(t, "aaaaaaa1111111", "ahead", 3, 0)

	res := newTestChecker(srv.URL).Check(context.Background(), "unknown")

	if res.Status != UpdateUnknown {
		t.Fatalf("Status = %q, want %q", res.Status, UpdateUnknown)
	}
	if len(*seen) != 0 {
		t.Errorf("an unknown installed commit must not reach the network, saw %v", *seen)
	}
	if res.Error == "" {
		t.Error("an unknown result must carry an explanation")
	}
}

func TestCheck_RateLimitedIsUnknownNotAFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"message":"API rate limit exceeded"}`)
	}))
	defer srv.Close()

	res := newTestChecker(srv.URL).Check(context.Background(), testCommit)

	if res.Status != UpdateUnknown {
		t.Fatalf("Status = %q, want %q", res.Status, UpdateUnknown)
	}
	if !strings.Contains(strings.ToLower(res.Error), "rate limit") {
		t.Errorf("Error = %q, want it to mention the rate limit", res.Error)
	}
}

func TestCheck_UnknownUpstreamCommitIsUnknown(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/"+defaultTestRepository+"/git/ref/heads/main", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"ref":"refs/heads/main","object":{"sha":%q}}`, "aaaaaaa1111111")
	})
	mux.HandleFunc("/repos/"+defaultTestRepository+"/compare/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"message":"Not Found"}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	res := newTestChecker(srv.URL).Check(context.Background(), testCommit)

	if res.Status != UpdateUnknown {
		t.Fatalf("Status = %q, want %q", res.Status, UpdateUnknown)
	}
	if res.Error == "" {
		t.Error("an unknown result must carry an explanation")
	}
}

func TestCheck_OfflineIsUnknownNotACrash(t *testing.T) {
	// A closed server stands in for an offline host.
	srv := httptest.NewServer(http.NotFoundHandler())
	base := srv.URL
	srv.Close()

	res := newTestChecker(base).Check(context.Background(), testCommit)

	if res.Status != UpdateUnknown {
		t.Fatalf("Status = %q, want %q", res.Status, UpdateUnknown)
	}
	if res.Error == "" {
		t.Error("an offline result must carry an explanation")
	}
}

func TestCheck_HonoursTheBoundedTimeout(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	defer func() {
		close(release)
		srv.Close()
	}()

	c := newTestChecker(srv.URL)
	c.HTTPClient = &http.Client{Timeout: 150 * time.Millisecond}

	start := time.Now()
	res := c.Check(context.Background(), testCommit)
	elapsed := time.Since(start)

	if res.Status != UpdateUnknown {
		t.Fatalf("Status = %q, want %q", res.Status, UpdateUnknown)
	}
	if elapsed > 5*time.Second {
		t.Errorf("the check took %s; the timeout is not bounded", elapsed)
	}
}

// TestCheck_TheWholeCheckIsBoundedNotJustEachRequest pins the
// guarantee the SPEC states: the check as a whole is bounded.
//
// A per-request timeout alone is not that guarantee. The check makes
// TWO requests (resolve the upstream head, then compare). If the first
// one is slow but still under the per-request timeout, a check bounded
// only per request would let the SECOND one run for another full
// timeout — the operator waits for the sum.
//
// This test makes the first request take most of the timeout and stalls
// the second one. Bounded as a whole, the check returns at the
// deadline (~one timeout). Bounded only per request, it would take
// roughly two.
func TestCheck_TheWholeCheckIsBoundedNotJustEachRequest(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/git/ref/heads/main") {
			// Slow, but still under the per-request timeout.
			time.Sleep(600 * time.Millisecond)
			fmt.Fprintf(w, `{"ref":"refs/heads/main","object":{"sha":"aaaaaaa1111111"}}`)
			return
		}
		<-release
	}))
	defer func() {
		close(release)
		srv.Close()
	}()

	const clientTimeout = 800 * time.Millisecond

	c := newTestChecker(srv.URL)
	c.HTTPClient = &http.Client{Timeout: clientTimeout}

	start := time.Now()
	res := c.Check(context.Background(), testCommit)
	elapsed := time.Since(start)

	if res.Status != UpdateUnknown {
		t.Fatalf("Status = %q, want %q", res.Status, UpdateUnknown)
	}
	// The whole-check bound is ONE timeout. Waiting out a second full
	// timeout would mean only the individual requests were bounded.
	if elapsed > 1200*time.Millisecond {
		t.Errorf("the check took %s; only the individual requests were bounded, not the check as a whole", elapsed)
	}
}

func TestCheck_SendsNoCredentials(t *testing.T) {
	var sawAuth bool
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/"+defaultTestRepository+"/git/ref/heads/main", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			sawAuth = true
		}
		fmt.Fprintf(w, `{"ref":"refs/heads/main","object":{"sha":%q}}`, "aaaaaaa1111111")
	})
	mux.HandleFunc("/repos/"+defaultTestRepository+"/compare/", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			sawAuth = true
		}
		fmt.Fprint(w, `{"status":"identical","ahead_by":0,"behind_by":0}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	newTestChecker(srv.URL).Check(context.Background(), testCommit)

	if sawAuth {
		t.Error("the update check sent an Authorization header; it must never require credentials")
	}
}

func TestCheck_RequestsOnlyTheRepositoryAndTwoRevisions(t *testing.T) {
	srv, seen := upstreamServer(t, "aaaaaaa1111111", "identical", 0, 0)

	newTestChecker(srv.URL).Check(context.Background(), testCommit)

	for _, path := range *seen {
		if !strings.HasPrefix(path, "/repos/"+defaultTestRepository+"/") {
			t.Errorf("the check touched an unexpected path: %s", path)
		}
	}
}
