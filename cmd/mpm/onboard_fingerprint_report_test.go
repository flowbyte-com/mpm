package main

import (
	"strings"
	"testing"

	mpminternal "github.com/flowbyte-com/mpm-core"
)

// Regression tests for the memory-vector fingerprint line in the onboard
// report.
//
// The defect: that line was a hardcoded string asserting "`memories` has no
// embedding_model column". It was true when written and became false when
// the 2026-10-13 migration added the column — and onboarding kept printing
// it, on the live database, after the deploy that performed the migration.
//
// A report that misstates the substrate is worse than one that stays silent,
// because the operator cannot tell which lines to trust. It was invisible to
// the test suite because the string was a constant: no assertion compared it
// against the store it described.
//
// What matters operationally is per-row, not per-schema. The compatibility
// filter compares embedding_model to the ACTIVE model and excludes what it
// cannot prove, so a store whose column exists but whose rows are unlabelled
// still returns nothing from semantic recall. These tests cover all three
// states, so the text cannot silently go stale again in any of them.

func configuredEmbedding() *mpminternal.EmbeddingConfig {
	return &mpminternal.EmbeddingConfig{
		ProviderName: "ollama:test-embed",
	}
}

// TestOnboardFingerprintLineTracksTheLiveStore is the load-bearing one:
// what the report says must be what the database actually contains.
func TestOnboardFingerprintLineTracksTheLiveStore(t *testing.T) {
	dm := newTestDMForCmd(t)
	db := dm.SQLDB()

	if !memoriesHaveModelColumn(db) {
		t.Fatal("memories has no embedding_model column; the migration under test " +
			"has not been applied, so this test cannot tell stale text from correct text")
	}

	t.Run("unlabelled rows are counted and named as a remedy", func(t *testing.T) {
		seedMemories(t, db, []struct {
			id    string
			vec   string
			model interface{}
		}{
			{"onboard-fp-unlabelled-1", "[0.1,0.2,0.3]", nil},
			{"onboard-fp-unlabelled-2", "[0.4,0.5,0.6]", nil},
		})

		got := memoryVectorFingerprintState(dm, configuredEmbedding())
		// The count is the actionable part. "Some rows are unlabelled" is
		// not something an operator can act on.
		if !strings.Contains(got, "2 of 2") {
			t.Errorf("report does not state how many rows are unlabelled: %q", got)
		}
		if !strings.Contains(got, "backfill-embeddings") {
			t.Errorf("report names no remedy for unlabelled rows: %q", got)
		}
		// It must say why they are excluded, not merely that they are
		// missing — "unlabelled" and "excluded from retrieval" are
		// different claims and only the second explains the outage.
		if !strings.Contains(got, "excluded from semantic recall") {
			t.Errorf("report does not say unlabelled rows are excluded from semantic "+
				"recall, so the operator cannot connect this to recall returning nothing: %q", got)
		}
	})

	t.Run("partially labelled reports only the unlabelled remainder", func(t *testing.T) {
		seedMemories(t, db, []struct {
			id    string
			vec   string
			model interface{}
		}{
			{"onboard-fp-mixed-1", "[0.1,0.2]", nil},
			{"onboard-fp-mixed-2", "[0.3,0.4]", "ollama:test-embed"},
		})

		got := memoryVectorFingerprintState(dm, configuredEmbedding())
		if !strings.Contains(got, "1 of 2") {
			t.Errorf("expected exactly the unlabelled remainder to be counted, got: %q", got)
		}
		if strings.Contains(got, "no embedding_model column") {
			t.Errorf("report claims the column is absent on a store that has it: %q", got)
		}
	})

	t.Run("fully labelled reports nothing outstanding", func(t *testing.T) {
		seedMemories(t, db, []struct {
			id    string
			vec   string
			model interface{}
		}{
			{"onboard-fp-done-1", "[0.1,0.2]", "ollama:test-embed"},
		})

		got := memoryVectorFingerprintState(dm, configuredEmbedding())
		if strings.Contains(got, "backfill-embeddings") {
			t.Errorf("a fully labelled store should not tell the operator to backfill: %q", got)
		}
		if strings.Contains(got, "no embedding_model column") {
			t.Errorf("report claims the column is absent on a store that has it: %q", got)
		}
	})
}

// TestOnboardFingerprintLineNeverClaimsAMissingColumnOnAMigratedStore
// pins the exact regression, independently of the counts: once the column
// exists, the report must never say it does not. This is the assertion the
// original constant text would have failed.
func TestOnboardFingerprintLineNeverClaimsAMissingColumnOnAMigratedStore(t *testing.T) {
	dm := newTestDMForCmd(t)
	db := dm.SQLDB()

	if !memoriesHaveModelColumn(db) {
		t.Skip("store predates the embedding_model migration; the missing-column " +
			"wording is correct there")
	}

	// Deliberately empty of vectors: the previous version of this text was a
	// constant, so it printed the same sentence regardless of contents. The
	// empty store is the case that exposed it.
	got := memoryVectorFingerprintState(dm, configuredEmbedding())
	if strings.Contains(got, "has no embedding_model column") {
		t.Errorf("report claims memories has no embedding_model column, but it does: %q", got)
	}
}

// TestOnboardFingerprintLineDegradesWhenTheStoreIsUnavailable — an
// unavailable database must not read as "everything is fine".
func TestOnboardFingerprintLineDegradesWhenTheStoreIsUnavailable(t *testing.T) {
	got := memoryVectorFingerprintState(nil, configuredEmbedding())
	if !strings.Contains(got, "not checked") {
		t.Errorf("an unavailable store must be reported as unchecked, not silently "+
			"assumed healthy: %q", got)
	}
}
