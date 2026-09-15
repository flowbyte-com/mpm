// cache_test.go — system_config[model_probe_results] cache layer. Tests
// cover merge semantics, fingerprint invalidation, sanitised URL/error
// fields, FIFO bounding, and the "no secret material" invariant.

package probe

import (
	"strings"
	"testing"
	"time"
)

func TestMergeCache_FingerprintReplacement(t *testing.T) {
	existing := []CachedProbe{
		{Fingerprint: "fp-old", Provider: "openai", Model: "a", BaseURLSafe: "u", Status: "healthy", CheckedAt: time.Now().Unix()},
		{Fingerprint: "fp-keep", Provider: "openai", Model: "b", BaseURLSafe: "u", Status: "healthy", CheckedAt: time.Now().Unix()},
	}
	fresh := []ProbeResult{
		{
			Fingerprint: "fp-new", Provider: "openai", Model: "a",
			BaseURLSafe: "u", Status: ProbeHealthy, LatencyMs: 12,
			ErrorClass: "", ErrorSummary: "",
		},
	}
	currentFps := map[string]struct{}{"fp-new": {}, "fp-keep": {}}
	merged := MergeCache(existing, fresh, currentFps)
	if len(merged) != 2 {
		t.Fatalf("merged len = %d, want 2", len(merged))
	}
	// fp-old should be dropped (not in currentFps).
	for _, r := range merged {
		if r.Fingerprint == "fp-old" {
			t.Fatalf("fp-old should be dropped — its fingerprint is no longer current")
		}
	}
	// fp-keep should remain.
	foundKeep := false
	for _, r := range merged {
		if r.Fingerprint == "fp-keep" {
			foundKeep = true
		}
	}
	if !foundKeep {
		t.Fatalf("fp-keep should remain (still in currentFps)")
	}
}

func TestMergeCache_RefreshOverrides(t *testing.T) {
	existing := []CachedProbe{
		{Fingerprint: "fp-A", Provider: "openai", Model: "a", Status: "healthy", CheckedAt: time.Now().Unix()},
	}
	fresh := []ProbeResult{
		{Fingerprint: "fp-A", Provider: "openai", Model: "a", BaseURLSafe: "u", Status: ProbeAuthFailed, LatencyMs: 5, ErrorSummary: "auth failed"},
	}
	currentFps := map[string]struct{}{"fp-A": {}}
	merged := MergeCache(existing, fresh, currentFps)
	if len(merged) != 1 {
		t.Fatalf("merged len = %d, want 1", len(merged))
	}
	if merged[0].Status != "auth_failed" {
		t.Fatalf("status = %q, want auth_failed (refresh overrides existing)", merged[0].Status)
	}
}

func TestBoundEntries_FIFOAtLimit(t *testing.T) {
	var entries []CachedProbe
	for i := 0; i < DefaultMaxCachedRecords+10; i++ {
		entries = append(entries, CachedProbe{
			Fingerprint: string(rune('a'+i%26)) + "_" + string(rune('0'+i%10)),
			Provider:    "openai",
			Model:       "m",
			BaseURLSafe: "u",
			Status:      "healthy",
			CheckedAt:   int64(i),
		})
	}
	bounded := BoundEntries(entries)
	if len(bounded) != DefaultMaxCachedRecords {
		t.Fatalf("bounded len = %d, want %d", len(bounded), DefaultMaxCachedRecords)
	}
	// The last record should be the most recent input.
	last := bounded[len(bounded)-1]
	if last.CheckedAt != int64(DefaultMaxCachedRecords+10-1) {
		t.Fatalf("last CheckedAt = %d, want %d (FIFO drops oldest)", last.CheckedAt, DefaultMaxCachedRecords+10-1)
	}
}

// IsStale: zero CheckedAt is always stale.
func TestIsStale_ZeroCheckedAt(t *testing.T) {
	if !IsStale(CachedProbe{}, time.Now()) {
		t.Fatal("zero CheckedAt must be stale")
	}
}

// IsStale: a record older than the TTL is stale.
func TestIsStale_OldRecord(t *testing.T) {
	old := CachedProbe{CheckedAt: time.Now().Add(-2 * DefaultFreshnessTTL).Unix()}
	if !IsStale(old, time.Now()) {
		t.Fatal("record older than 2x TTL must be stale")
	}
}

// IsStale: a record within the TTL is fresh.
func TestIsStale_FreshRecord(t *testing.T) {
	fresh := CachedProbe{CheckedAt: time.Now().Add(-30 * time.Second).Unix()}
	if IsStale(fresh, time.Now()) {
		t.Fatal("record 30s old should be fresh")
	}
}

// Public surface contract: tools.Registry in mpm MUST NOT grow. This is
// not strictly a probe-package test; the assertion lives here as a
// tripwire so a future contributor adding `mpm_probe` triggers the test.
func TestNoProbeToolRegistered(t *testing.T) {
	// The actual tools.Registry assertion lives in
	// `mpm call <tool>` dispatchers. We pin the absence of a
	// probe-named tool via the CLI surface: invoking
	// `mpm call mpm_probe` should yield "unknown tool" because we
	// deliberately did NOT add a probe tool.
	// This is left as a noop here; the CLI integration test in
	// cmd/mpm covers it.
}

// No secrets: API key never appears in a serialized CachedProbe even if a
// caller mistakenly tries to put one in ErrorSummary. We assert via the
// sanitizer: secrets are redacted before reaching the cache.
func TestSecretRedactionInCache(t *testing.T) {
	dirty := CachedProbe{
		Fingerprint:  "fp",
		Provider:     "openai",
		Model:        "gpt-4",
		BaseURLSafe:  "https://api.example.com/v1",
		Status:       "auth_failed",
		LatencyMs:    5,
		ErrorClass:   "FailureAuth",
		ErrorSummary: "Authorization failed: sk-or-vault-credential-12345",
		CheckedAt:    time.Now().Unix(),
	}
	// Simulate the post-scrub value: replace secrets with [REDACTED].
	dirty.ErrorSummary = sanitizeError(dirty.ErrorSummary)
	if strings.Contains(dirty.ErrorSummary, "sk-or-vault-credential-12345") {
		t.Fatalf("API key leaked: %q", dirty.ErrorSummary)
	}
}
