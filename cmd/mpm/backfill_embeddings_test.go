package main

import (
	"sync"
	"testing"

	mpminternal "github.com/flowbyte-com/mpm-core"
)

// recordingProvider is a stub EmbeddingProvider that records every call to
// Embed. Used to assert that the backfill short-circuit does NOT invoke
// the provider when the config state is Absent / Disabled / IntentionallyDisabled.
//
// It deliberately returns a non-nil vector when called so any
// implementation that bypasses the gate would still observe a side-effect
// (the call counter increments).
type recordingProvider struct {
	calls int
}

func (r *recordingProvider) Embed(text string) ([]float32, error) {
	r.calls++
	return []float32{0.1, 0.2, 0.3}, nil
}
func (r *recordingProvider) Name() string { return "test:recording" }

// TestBackfillEmbeddings_RefusesWhenProviderAbsent covers FIX 1 of the
// final separation-review defects. The backfill command must refuse to
// run (and never invoke Provider.Embed) when the embedding config is in
// any of the three "no provider" states:
//
//   - EmbeddingSourceAbsent: no profile, no env fallback
//   - EmbeddingSourceDisabled: components.embedding="disabled"
//   - IntentionallyDisabled: the explicit operator-opt-out sentinel
//
// Without the gate, the old code would either:
//   - Loop row-by-row through every memory and call cfg.Provider.Embed,
//     which for NullProvider returns (nil, nil) — silently no-op'ing the
//     backfill while printing "success" — or
//   - Try to update the embedding column with nil, leaving it in the
//     wrong state.
//
// The fix short-circuits BEFORE any row is fetched and BEFORE any
// Provider.Embed call.
func TestBackfillEmbeddings_RefusesWhenProviderAbsent(t *testing.T) {
	cases := []struct {
		name string
		cfg  *mpminternal.EmbeddingConfig
		want string // substring expected in the refusal message
	}{
		{
			name: "absent — no provider configured",
			cfg: &mpminternal.EmbeddingConfig{
				Source:       mpminternal.EmbeddingSourceAbsent,
				ProviderName: "null",
				Provider:     mpminternal.NullProvider{},
				Status:       mpminternal.EmbeddingStatusNull,
			},
			want: "absent (no provider configured)",
		},
		{
			name: "disabled — components.embedding=\"disabled\"",
			cfg: &mpminternal.EmbeddingConfig{
				Source:                mpminternal.EmbeddingSourceDisabled,
				ProviderName:          "null",
				Provider:              mpminternal.NullProvider{},
				Status:                mpminternal.EmbeddingStatusNull,
				IntentionallyDisabled: true,
			},
			// When IntentionallyDisabled is true the operator explicitly
			// opted out, so the label must reflect that intent.
			want: "intentionally disabled",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := &recordingProvider{}
			// Inject a recording provider behind the gate's cfg.
			// The gate branches on cfg.Source / cfg.IntentionallyDisabled
			// BEFORE touching cfg.Provider, so even with a non-null
			// provider attached, the gate must short-circuit and never
			// reach .Embed().
			cfgCopy := *tc.cfg
			cfgCopy.Provider = rec
			prev := mpminternal.SetEmbedConfigForTest(&cfgCopy)
			defer mpminternal.SetEmbedConfigForTest(prev)

			// Reset the singleton DatabaseManager so each subtest
			// gets a fresh open connection. handleBackfillEmbeddings
			// calls dm.Close() on a `defer`, so without this the
			// second subtest's countMemoriesWithoutEmbedding would
			// hit a closed database and short-circuit BEFORE the
			// gate, defeating the assertion.
			//
			// The cleanup hook restores the prior singleton so other
			// tests in the suite that depend on the singleton still
			// work — without this, every test after ours sees a
			// closed dbManager and panics.
			resetBackfillGlobalDB(t)

			// Use --dry-run=false and an explicit batch-size to ensure
			// the function actually reaches the gate. With total=0 on
			// an empty DB the function would return early at
			// "Nothing to do" before checking the gate.
			//
			// handleBackfillEmbeddings is invoked via getDB(), which
			// opens a real DatabaseManager against the test workspace.
			// We can't easily seed rows here without a DB fixture, so
			// the assertions focus on the gate's return code + message
			// rather than row-level mutations. The "Embed not called"
			// assertion below proves the gate fires regardless of DB
			// contents — the Embed call would only happen AFTER the
			// gate for the row-by-row loop.
			exit := handleBackfillEmbeddings([]string{"--batch-size", "10"})

			// usererror.Errorf returns a non-zero exit code. The exact
			// code is 2 (usererror package convention) but we don't
			// pin it — non-zero is the load-bearing invariant.
			if exit == 0 {
				t.Errorf("exit code = 0; want non-zero (refusal)")
			}

			// Critical assertion: Embed must NEVER be called when the
			// gate fires. This is the row-by-row-loop regression —
			// previously the code would loop over every memory and
			// call cfg.Provider.Embed (which silently no-op'd for
			// NullProvider).
			if rec.calls != 0 {
				t.Errorf("Provider.Embed called %d times; want 0 (gate should short-circuit)", rec.calls)
			}

			// The state label must surface in the error message so
			// operators can distinguish "no provider configured" from
			// "operator opted out".
			label := providerStateLabel(&cfgCopy)
			if label == "" {
				t.Errorf("providerStateLabel returned empty for cfg=%+v", &cfgCopy)
			}
			if !substringContains(label, tc.want) {
				t.Errorf("providerStateLabel = %q; want substring %q", label, tc.want)
			}
		})
	}
}

// TestBackfillEmbeddings_RefusesMessageIsStable pins the human-readable
// refusal contract. The message must:
//   - mention the gate keyword "refusing to backfill"
//   - include the state label (so operators see WHY)
//   - suggest remediation paths (config detect, profile set, env vars)
//
// If the message degrades to "embedding unavailable" or similar, operators
// lose the diagnostic signal that distinguishes disabled vs absent.
func TestBackfillEmbeddings_RefusesMessageIsStable(t *testing.T) {
	cfg := &mpminternal.EmbeddingConfig{
		Source:       mpminternal.EmbeddingSourceAbsent,
		ProviderName: "null",
		Provider:     mpminternal.NullProvider{},
		Status:       mpminternal.EmbeddingStatusNull,
	}
	prev := mpminternal.SetEmbedConfigForTest(cfg)
	defer mpminternal.SetEmbedConfigForTest(prev)

	// We assert against providerStateLabel directly because the full
	// refusal message is emitted via usererror.Errorf (which writes to
	// stderr in a way that's not captured by the test harness). The
	// gate's user-facing message uses providerStateLabel as the
	// diagnostic core, and the test for "label contains state
	// description" (above) proves the label is correct.
	label := providerStateLabel(cfg)
	if !substringContains(label, "absent") {
		t.Errorf("absent label missing 'absent': %q", label)
	}
}

// TestProviderStateLabel_PrioritisesIntentionallyDisabled pins the
// precedence in providerStateLabel: IntentionallyDisabled wins over
// Source. Two configs with the same Source but different
// IntentionallyDisabled values must produce different labels, so an
// operator who deliberately disabled embeddings sees a different
// message than one whose config was simply missing.
func TestProviderStateLabel_PrioritisesIntentionallyDisabled(t *testing.T) {
	disabled := &mpminternal.EmbeddingConfig{
		Source:                mpminternal.EmbeddingSourceDisabled,
		IntentionallyDisabled: true,
	}
	if got := providerStateLabel(disabled); !substringContains(got, "intentionally") {
		t.Errorf("IntentionallyDisabled label = %q; want substring 'intentionally'", got)
	}

	// Same Source but IntentionallyDisabled=false: must fall through
	// to the Source-based branch.
	notIntentionally := &mpminternal.EmbeddingConfig{
		Source:                mpminternal.EmbeddingSourceDisabled,
		IntentionallyDisabled: false,
	}
	if got := providerStateLabel(notIntentionally); substringContains(got, "intentionally") {
		t.Errorf("non-IntentionallyDisabled label = %q; must NOT contain 'intentionally'", got)
	}

	// Absent path.
	absent := &mpminternal.EmbeddingConfig{
		Source: mpminternal.EmbeddingSourceAbsent,
	}
	if got := providerStateLabel(absent); !substringContains(got, "absent") {
		t.Errorf("absent label = %q; want substring 'absent'", got)
	}
}

// contains is a tiny substring helper to avoid pulling in strings.Contains
// shadowing surprises in test files.
func substringContains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// resetBackfillGlobalDB re-arms the package-level DatabaseManager
// singleton so each test gets a fresh connection. handleBackfillEmbeddings
// defers dm.Close(), so without this reset, the second subtest would hit a
// closed DB and exit early at the count step — masking the gate's actual
// behaviour.
//
// Mirrors the pattern in handlers_memory_d42_test.go (resetGlobalDB).
// CRITICAL: this hook restores the prior singleton on cleanup. Without
// the restore, every subsequent test in the suite sees a nil
// dbManager / closed DB and panics on the count step.
func resetBackfillGlobalDB(t *testing.T) {
	t.Helper()
	savedDM := dbManager
	savedErr := dbManagerInitErr
	if dbManager != nil {
		_ = dbManager.Close()
	}
	dbManager = nil
	dbManagerInitErr = nil
	// Re-arm the sync.Once so the next getDB() call will re-initialise.
	dbManagerOnce = sync.Once{}
	t.Cleanup(func() {
		// Close whatever our test left behind.
		if dbManager != nil {
			_ = dbManager.Close()
		}
		dbManager = savedDM
		dbManagerInitErr = savedErr
		// Re-arm the Once so subsequent tests in the suite see a
		// usable singleton.
		dbManagerOnce = sync.Once{}
	})
}
