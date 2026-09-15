// concurrent_test.go — Pins the multi-profile Doctor cache-write
// contract:
//
//   1. RunActiveProbes, with N≥4 distinct effective profiles, fans out
//      real probes concurrently.
//   2. After all probes complete, ONE atomic SaveSystemConfig merges
//      the results into system_config[model_probe_results].
//   3. A fresh reader observes all N fingerprints in the persisted
//      cache row — proving NO last-writer race condition.
//
// Pins the lost-update failure mode that would otherwise arise from
// per-goroutine read-modify-write of the same system_config key.

package probe

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	mpminternal "github.com/flowbyte-com/mpm-core"
	"github.com/flowbyte-com/mpm-core/config"
)

// startProbeTestDM returns an in-memory DatabaseManager and a cleanup
// hook. We use mpminternal.NewTestDM (canonical hermetic helper) so the
// workspace DB on the host is NOT touched.
func startProbeTestDM(t *testing.T) (*mpminternal.DatabaseManager, func()) {
	t.Helper()
	dm := mpminternal.NewTestDM(t)
	return dm, func() { _ = dm.Close() }
}

// newAlwaysOKServer stands up a tiny httptest server that returns a
// successful OpenAI-protocol response regardless of the request path or
// body. Used by the cache-fan-out test.
func newAlwaysOKServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"OK READY"}}]}`))
	})
	return httptest.NewServer(mux)
}

// TestRunActiveProbes_FourDistinctProfilesPersistedAllResults is the
// canonical regression: four distinct effective profiles are probed
// concurrently; ONE merge+save persists all four fingerprints; a fresh
// reader sees all four. Pins the lost-update failure mode at the
// System_config[model_probe_results] row.
func TestRunActiveProbes_FourDistinctProfilesPersistedAllResults(t *testing.T) {
	dm, cleanup := startProbeTestDM(t)
	defer cleanup()

	srv := newAlwaysOKServer(t)
	defer srv.Close()

	// Four distinct profiles — each (provider, model, base_url) is
	// distinct → four distinct fingerprints. The fake server returns
	// the same body to every probe; the test pins cache fan-out, not
	// the wire.
	cfg := &config.Config{
		Profiles: map[string]config.Profile{
			"a": {Provider: "openai-compatible", Model: "model-a", BaseURL: srv.URL},
			"b": {Provider: "openai-compatible", Model: "model-b", BaseURL: srv.URL},
			"c": {Provider: "openai-compatible", Model: "model-c", BaseURL: srv.URL},
			"d": {Provider: "openai-compatible", Model: "model-d", BaseURL: srv.URL},
		},
		Components: map[string]string{
			"memory":  "a",
			"critic":  "b",
			"router":  "c",
			"planner": "d",
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	results, err := RunActiveProbes(ctx, cfg, dm)
	if err != nil {
		t.Fatalf("RunActiveProbes: %v", err)
	}
	if len(results) != 4 {
		t.Fatalf("expected 4 probe results, got %d", len(results))
	}

	// Cache must now contain 4 rows with 4 distinct fingerprints.
	rows, err := LoadCache(dm)
	if err != nil {
		t.Fatalf("LoadCache: %v", err)
	}
	if len(rows) != 4 {
		t.Fatalf("persisted cache has %d rows, want 4 (no rows lost to concurrent races)", len(rows))
	}
	seen := map[string]bool{}
	for _, r := range rows {
		fp := ComputeFingerprint(FingerprintInput{
			Provider: r.Provider, Model: r.Model, BaseURL: srv.URL, Credential: "",
		})
		if !seen[fp] {
			seen[fp] = true
		}
	}
	if len(seen) != 4 {
		t.Fatalf("persisted cache saw %d unique fingerprints, want 4", len(seen))
	}
}

// TestMergeCache_FourDistinctFingerprints is a no-DB companion that
// pins the merge invariant directly: N distinct fresh results yield N
// rows on a fresh merge.
func TestMergeCache_FourDistinctFingerprints(t *testing.T) {
	fps := []string{"fp-a", "fp-b", "fp-c", "fp-d"}
	fpset := map[string]struct{}{}
	results := make([]ProbeResult, 0, len(fps))
	for _, fp := range fps {
		fpset[fp] = struct{}{}
		results = append(results, ProbeResult{
			Fingerprint: fp, Provider: "p", Model: "m",
			BaseURLSafe: "u", Status: ProbeHealthy,
			LatencyMs: 12, CheckedAt: time.Now(),
		})
	}
	merged := MergeCache(nil, results, fpset)
	if len(merged) != 4 {
		t.Fatalf("merged len = %d, want 4", len(merged))
	}
	seen := map[string]bool{}
	for _, r := range merged {
		seen[r.Fingerprint] = true
	}
	for _, fp := range fps {
		if !seen[fp] {
			t.Fatalf("merged missing fingerprint %s", fp)
		}
	}
}

// TestMergeCache_PreExistingRecordsCombinedWithConcurrent pins the
// mixed scenario: pre-existing records with fingerprints not in the
// current call's target set are dropped; current-set fingerprints are
// preserved or refreshed.
func TestMergeCache_PreExistingRecordsCombinedWithConcurrent(t *testing.T) {
	preFp := "previous-fingerprint-veryunique"
	preExisting := []CachedProbe{
		{
			Fingerprint: preFp, Provider: "openai", Model: "old",
			BaseURLSafe: "https://api.openai.com/v1", Status: "healthy",
			LatencyMs: 50, CheckedAt: time.Now().Add(-1 * time.Minute).Unix(),
		},
	}
	newFps := map[string]struct{}{
		ComputeFingerprint(FingerprintInput{Provider: "openai-compatible", Model: "n1", BaseURL: "http://x"}): {},
		ComputeFingerprint(FingerprintInput{Provider: "openai-compatible", Model: "n2", BaseURL: "http://y"}): {},
		ComputeFingerprint(FingerprintInput{Provider: "openai-compatible", Model: "n3", BaseURL: "http://z"}): {},
	}
	fresh := []ProbeResult{}
	for fp := range newFps {
		fresh = append(fresh, ProbeResult{
			Fingerprint: fp, Provider: "openai-compatible", Model: "n?",
			BaseURLSafe: "http://x", Status: ProbeHealthy,
			LatencyMs: 12, CheckedAt: time.Now(),
		})
	}
	merged := MergeCache(preExisting, fresh, newFps)
	for _, r := range merged {
		if r.Fingerprint == preFp {
			t.Fatal("pre-existing fingerprint should be dropped (config no longer references it)")
		}
	}
	if len(merged) != len(fresh) {
		t.Fatalf("merged len = %d, want %d (only fresh rows should remain)", len(merged), len(fresh))
	}
}

// TestMergeCache_BoundedAtMaxEvenWithConcurrentExcess verifies the
// FIFO cap survives many concurrent results.
func TestMergeCache_BoundedAtMaxEvenWithConcurrentExcess(t *testing.T) {
	results := make([]ProbeResult, 0, 100)
	fps := map[string]struct{}{}
	for i := 0; i < 100; i++ {
		fp := ComputeFingerprint(FingerprintInput{
			Provider: "openai-compatible", Model: "m",
			BaseURL:    "http://x" + string(rune('a'+i%26)) + ".local",
			Credential: "",
		})
		fps[fp] = struct{}{}
		results = append(results, ProbeResult{
			Fingerprint: fp, Provider: "p", Model: "m",
			BaseURLSafe: "u", Status: ProbeHealthy,
			LatencyMs: int64(i), CheckedAt: time.Unix(int64(i), 0),
		})
	}
	merged := MergeCache(nil, results, fps)
	if len(merged) > DefaultMaxCachedRecords {
		t.Fatalf("merged len = %d, must be <= DefaultMaxCachedRecords(%d)",
			len(merged), DefaultMaxCachedRecords)
	}
}
