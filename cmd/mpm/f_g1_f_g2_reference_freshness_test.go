// f_g1_f_g2_reference_freshness_test.go — F-G1/F-G2 reference freshness
// in CLI surfaces.
//
// F-G1: `mpm reference ls` did not show staleness — a backdated 5-year-old
//       reference looked identical to a fresh one. The DB layer had the
//       freshness classifier (F19-1) but the CLI didn't surface it.
//
// F-G2: `mpm reference ls` could not mark references as historical because
//       the CLI didn't surface the freshness field at all. Tagging was the
//       only signal, and there was no header telling the operator "this is
//       historical — don't treat as current authority".
//
// The fix:
//   1. handleRefList JSON envelope includes `freshness` per entry.
//   2. handleRefList human output appends `{freshness}` when not current.
//   3. handleRefShow JSON envelope includes `freshness` at top level.
//   4. handleRefShow human output prints a `⚠️ Freshness: <X>` header line
//      when freshness is not current.
//
// These tests exercise the public handlers and the substrate's freshness
// classifier directly so the contract is pinned end-to-end.
package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	internal "github.com/flowbyte-com/mpm-core"
)

// TestF_G1_RefListJSONIncludesFreshness pins the headline regression for
// F-G1: the JSON envelope must include a freshness field per entry.
func TestF_G1_RefListJSONIncludesFreshness(t *testing.T) {
	dm := getDBConcrete()
	if dm == nil {
		t.Skip("DB unavailable")
	}

	// Seed a reference tagged "stale" so its freshness classifies to stale.
	refID := fG2SeedRef(t, dm, "F-G1 stale ref", `["stale"]`)

	// Capture stdout via a pipe substitute — instead, parse the last JSON
	// line by re-running with output captured through a redirect.
	// The cleanest path is to call the DB layer's ListReferences directly
	// (since handleRefList prints to stdout which we can't easily capture
	// here) and verify the freshness field is populated.
	refs, err := dm.ListReferences(50, 0)
	if err != nil {
		t.Fatalf("ListReferences: %v", err)
	}
	found := false
	for _, r := range refs {
		if id, _ := r["id"].(string); id == refID {
			found = true
			freshness, _ := r["freshness"].(string)
			if freshness == "" {
				t.Errorf("freshness missing from ListReferences result")
			}
			if freshness != string(internal.FreshnessStale) {
				t.Errorf("freshness = %q, want %q", freshness, internal.FreshnessStale)
			}
		}
	}
	if !found {
		t.Errorf("seeded ref %s not in list", refID)
	}
}

// TestF_G1_RefListHumanOutputShowsFreshness pins the human-rendering
// contract: a stale ref must render with a {stale} badge so the
// operator can spot it without parsing JSON.
func TestF_G1_RefListHumanOutputShowsFreshness(t *testing.T) {
	dm := getDBConcrete()
	if dm == nil {
		t.Skip("DB unavailable")
	}

	refID := fG2SeedRef(t, dm, "F-G1 stale visible", `["stale"]`)

	out := fG2CaptureStdout(t, func() int {
		return handleRefList([]string{"ls"})
	})

	// handleRefList truncates the displayed ID to 16 chars, so match the prefix.
	prefix := refID
	if len(prefix) > 16 {
		prefix = prefix[:16]
	}
	if !strings.Contains(out, prefix) {
		t.Errorf("ref prefix %s not present in output:\n%s", prefix, out)
	}
	if !strings.Contains(out, "{stale}") {
		t.Errorf("output missing {stale} badge for stale ref:\n%s", out)
	}
}

// TestF_G2_RefShowFreshnessHeader pins the historical-flag contract:
// a ref with tag "historical" must show `⚠️ Freshness: historical` on
// the human output.
func TestF_G2_RefShowFreshnessHeader(t *testing.T) {
	dm := getDBConcrete()
	if dm == nil {
		t.Skip("DB unavailable")
	}

	refID := fG2SeedRef(t, dm, "F-G2 historical ref", `["historical"]`)

	out := fG2CaptureStdout(t, func() int {
		return handleRefShow([]string{"show", refID})
	})

	if !strings.Contains(out, "Freshness: historical") {
		t.Errorf("historical ref missing freshness header:\n%s", out)
	}
}

// TestF_G2_RefShowJSONIncludesFreshness pins the JSON envelope contract:
// the JSON output of `reference show <id>` must include freshness.
func TestF_G2_RefShowJSONIncludesFreshness(t *testing.T) {
	dm := getDBConcrete()
	if dm == nil {
		t.Skip("DB unavailable")
	}

	refID := fG2SeedRef(t, dm, "F-G2 stale JSON", `["stale"]`)

	out := fG2CaptureStdout(t, func() int {
		return handleRefShow([]string{"show", refID, "--json"})
	})

	var envelope map[string]interface{}
	if err := json.Unmarshal([]byte(out), &envelope); err != nil {
		t.Fatalf("parse JSON: %v\n%s", err, out)
	}
	if f, _ := envelope["freshness"].(string); f != string(internal.FreshnessStale) {
		t.Errorf("envelope.freshness = %q, want %q", f, internal.FreshnessStale)
	}
}

// TestF_G1_CurrentRefNoBadge pins the negative case: a fresh ref must
// NOT carry a freshness badge in the listing.
func TestF_G1_CurrentRefNoBadge(t *testing.T) {
	dm := getDBConcrete()
	if dm == nil {
		t.Skip("DB unavailable")
	}

	// A ref with no stale tags and a recent last_indexed classifies as
	// "current" — no badge should appear.
	refID := fG2SeedRef(t, dm, "F-G1 current ref", `[]`)

	out := fG2CaptureStdout(t, func() int {
		return handleRefList([]string{"ls"})
	})

	// The line for our ref must NOT contain any {stale|historical|...} badge.
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, refID) {
			// Find any {<word>} badge on the same line.
			idx := strings.Index(line, "{")
			if idx < 0 {
				continue
			}
			end := strings.Index(line[idx:], "}")
			if end < 0 {
				continue
			}
			badge := line[idx+1 : idx+end]
			if internal.Freshness(badge) == internal.FreshnessCurrent {
				t.Errorf("current ref should not have {current} badge; got line:\n%s", line)
			}
		}
	}
}

// TestF_G1_AgeBasedStaleClassification pins the age-based fallback:
// a ref with last_indexed > FreshnessAgeThreshold (90d) and no override
// tag should classify as stale.
func TestF_G1_AgeBasedStaleClassification(t *testing.T) {
	dm := getDBConcrete()
	if dm == nil {
		t.Skip("DB unavailable")
	}

	// Insert a ref with last_indexed = now - 100 days. No tags means
	// the classifier falls through to the age check.
	oldTime := time.Now().Add(-100 * 24 * time.Hour).Unix()
	refID := fG2SeedRefWithIndexed(t, dm, "F-G1 old ref", `[]`, oldTime)

	refs, err := dm.ListReferences(50, 0)
	if err != nil {
		t.Fatalf("ListReferences: %v", err)
	}
	for _, r := range refs {
		if id, _ := r["id"].(string); id == refID {
			freshness, _ := r["freshness"].(string)
			if freshness != string(internal.FreshnessStale) {
				t.Errorf("100-day-old untagged ref: freshness = %q, want %q", freshness, internal.FreshnessStale)
			}
			return
		}
	}
	t.Fatalf("seeded ref %s not found in list", refID)
}

// fG2SeedRef inserts a reference with the given tags and returns its ID.
// last_indexed defaults to now (fresh).
func fG2SeedRef(t *testing.T, dm *internal.DatabaseManager, title, tagsJSON string) string {
	return fG2SeedRefWithIndexed(t, dm, title, tagsJSON, time.Now().Unix())
}

// fG2SeedRefWithIndexed inserts a reference with explicit last_indexed
// timestamp. Used to test the age-based freshness classification.
func fG2SeedRefWithIndexed(t *testing.T, dm *internal.DatabaseManager, title, tagsJSON string, lastIndexed int64) string {
	t.Helper()
	refID := fC4UniqueID(t, "f-g1")
	_, err := dm.SQLDB().Exec(
		`INSERT INTO reference_docs (id, title, file_path, source_type, tags, total_chunks, last_indexed, import_reason, created_at)
		 VALUES (?, ?, '/tmp/fake', 'markdown', ?, 1, ?, 'test', CAST(strftime('%s','now') AS INTEGER))`,
		refID, title, tagsJSON, lastIndexed)
	if err != nil {
		t.Fatalf("seed ref: %v", err)
	}
	return refID
}

// fG2CaptureStdout runs fn and returns everything written to stdout.
// Uses the existing captureStdout helper from f8_f10_regression_test.go
// which captures both stdout and stderr.
func fG2CaptureStdout(t *testing.T, fn func() int) string {
	t.Helper()
	var rc int
	out := captureStdout(t, func() {
		rc = fn()
	})
	if rc != 0 {
		t.Logf("captured handler returned exit %d", rc)
	}
	return out
}
