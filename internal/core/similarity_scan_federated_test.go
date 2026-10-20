// similarity_scan_federated_test.go — the federated scan must read BOTH
// sides of the corpus, or say why it cannot.
//
// The defect these pin
//
// LoadSimilarityScanEntriesFederated read the shared side over the POOLED
// database. SQLite's ATTACH is PER CONNECTION, and the pool is not limited
// to one connection (db.go deliberately does not call SetMaxOpenConns(1),
// because a single-connection pool deadlocks against a concurrent
// transaction). The ATTACH made at startup therefore lives on ONE pooled
// connection, and a pooled query can land on another one that has no
// `shared` schema at all.
//
// The failure it produced was silent and total. The read failed with "no
// such table: shared.memories", the failure was classified as "no shared
// database", and the scan returned the LOCAL half as a successful result.
// The operator saw a scan of half the corpus and a successful exit code.
//
// The second defect was the mirror image. A shared source that was
// CONFIGURED but unreachable — an ATTACH that never succeeded, a shared file
// that was removed, a shared file replaced by a directory — produced the
// same local-only success. A broken installation answered exactly like a
// local-only one.
//
// The three installations and their three answers
//
//	no shared source configured      local-only, and that is SUCCESS
//	shared source configured+usable  both sides are read
//	shared source configured+broken  the scan FAILS, loudly
//
// Every test here runs against a disposable database under t.TempDir(). The
// live MPM database is never opened.

package internal

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// seedFederatedMemory inserts a memory on ONE side of the corpus.
//
// The model stamp and the created_at are explicit, because the two filters
// these tests pin — the embedding-model filter and the newest-first
// ordering — can only be observed when the test controls them.
func seedFederatedMemory(t *testing.T, dm *DatabaseManager, schema, id, content, model string, createdAt int64, vec []float32) {
	t.Helper()
	raw, err := json.Marshal(vec)
	require.NoError(t, err)
	_, err = dm.SQLDB().Exec(`
		INSERT INTO `+schema+`memories
			(id, collection, content, metadata, weight, created_at,
			 embedding, embedding_source, embedding_dimension, embedding_model)
		VALUES (?, 'memories', ?, '{}', 1, ?, ?, 'provider', ?, ?)`,
		id, content, createdAt, string(raw), len(vec), model)
	require.NoError(t, err)
}

// federatedIDs is a small readability helper for the assertions.
func federatedIDs(entries []ScanEntry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.ID)
	}
	return out
}

// drainPooledConnections empties the pool's free list, so that every later
// pooled query has to open a BRAND-NEW connection.
//
// SQLite's ATTACH is per connection, so a new connection has no `shared`
// schema attached. The pool retains at most maxIdleConns idle connections
// (2 by default) and hands them out most-recently-used first, so the number
// of idle connections is not knowable from the outside. Holding three
// connections is enough to empty the free list whatever it held.
//
// This is the accident that hides the defect in production: the pool's
// free list usually still holds the connection that performed the startup
// ATTACH, so a pooled read of the shared side usually works — until the
// pool opens one more connection than it had before.
func drainPooledConnections(t *testing.T, dm *DatabaseManager) {
	t.Helper()
	ctx := context.Background()
	held := make([]*sql.Conn, 0, 3)
	for i := 0; i < 3; i++ {
		conn, err := dm.SQLDB().Conn(ctx)
		require.NoError(t, err)
		held = append(held, conn)
	}
	t.Cleanup(func() {
		for _, conn := range held {
			_ = conn.Close()
		}
	})
}

// THE REGRESSION. A fresh pooled connection has no `shared` schema, and the
// federated scan must still read the shared side.
//
// The pooled read is attempted first, to prove the hazard is live rather
// than hypothetical: if that read were to succeed, this test would be
// asserting against a condition that does not exist.
func TestLoadSimilarityScanEntriesFederated_ReadsBothSidesOnAFreshPooledConnection(t *testing.T) {
	dm := NewTestSharedDM(t)
	installStubProvider(t, map[string][]float32{"probe": {1, 0, 0}})
	active := CurrentEmbeddingFingerprint().Model
	require.NotEmpty(t, active, "the fixture must resolve an active fingerprint")

	seedFederatedMemory(t, dm, "", "local-1", "a local claim", active, 100, unitVec(1, 0, 0))
	seedFederatedMemory(t, dm, "shared.", "shared-1", "a shared claim", active, 100, unitVec(0, 1, 0))

	drainPooledConnections(t, dm)

	// The hazard, made visible: a POOLED read of the shared side cannot
	// find the shared schema, because this connection never attached it.
	_, pooledErr := dm.SQLDB().Query(`SELECT id FROM shared.memories LIMIT 1`)
	require.Error(t, pooledErr,
		"a fresh pooled connection must not have the shared schema attached; "+
			"if it does, this test is no longer exercising the hazard")
	require.Contains(t, pooledErr.Error(), "shared",
		"the pooled failure must be the missing shared schema")

	// The federated scan must not inherit that failure. It pins a
	// connection, attaches the shared database to it, and reads both sides.
	entries, err := dm.LoadSimilarityScanEntriesFederated(0)
	require.NoError(t, err,
		"the federated scan must not report success over half the corpus")
	require.ElementsMatch(t, []string{"local-1", "shared-1"}, federatedIDs(entries),
		"both sides of the corpus must be scanned")
}

// A shared source that is CONFIGURED but cannot be reached must fail the
// scan. Reporting a local-only success would tell the operator their queue
// was scanned when half of it was never looked at.
func TestLoadSimilarityScanEntriesFederated_ConfiguredButUnreachableSharedSourceIsAnError(t *testing.T) {
	installStubProvider(t, map[string][]float32{"probe": {1, 0, 0}})
	active := CurrentEmbeddingFingerprint().Model

	t.Run("the attach never succeeded", func(t *testing.T) {
		tmp := t.TempDir()
		// A DIRECTORY where the shared database file should be. SQLite
		// cannot open it, so the startup ATTACH fails — but the operator
		// DID configure a shared source.
		sharedPath := filepath.Join(tmp, "shared-is-a-directory")
		require.NoError(t, os.MkdirAll(sharedPath, 0o700))
		t.Setenv("MPM_SHARED_DB", sharedPath)

		// The workspace root is passed EXPLICITLY. An empty root falls
		// through to config.GetMPMDir(), which is the operator's live
		// directory unless an env var happens to redirect it — and relying
		// on that redirect is exactly the pattern the repository's
		// database-safety guard exists to keep out of the test suite.
		dm, err := NewDatabaseManager(tmp)
		require.NoError(t, err)
		t.Cleanup(func() { dm.Close() })
		require.Equal(t, "", dm.SharedAttached(),
			"the fixture must reproduce an install whose ATTACH failed")

		seedFederatedMemory(t, dm, "", "local-only", "a local claim", active, 100, unitVec(1, 0, 0))

		entries, err := dm.LoadSimilarityScanEntriesFederated(0)
		require.Error(t, err,
			"a configured shared source that cannot be attached must fail the scan, "+
				"not degrade to a successful local-only scan")
		require.Nil(t, entries, "a failed scan must not return a partial result")
		require.Contains(t, err.Error(), "configured",
			"the error must say the shared source is configured")
	})

	t.Run("the shared file was removed", func(t *testing.T) {
		dm := NewTestSharedDM(t)
		seedFederatedMemory(t, dm, "", "local-only", "a local claim", active, 100, unitVec(1, 0, 0))
		seedFederatedMemory(t, dm, "shared.", "shared-1", "a shared claim", active, 100, unitVec(0, 1, 0))

		// The shared file disappears. A fresh connection re-attaches an
		// empty database, which has no `memories` table at all.
		require.NoError(t, os.Remove(dm.SharedAttached()))
		drainPooledConnections(t, dm)

		entries, err := dm.LoadSimilarityScanEntriesFederated(0)
		require.Error(t, err,
			"a shared source whose rows cannot be read must fail the scan")
		require.Nil(t, entries, "a failed scan must not return a partial result")
	})

	t.Run("the shared path is no longer a file", func(t *testing.T) {
		dm := NewTestSharedDM(t)
		seedFederatedMemory(t, dm, "", "local-only", "a local claim", active, 100, unitVec(1, 0, 0))
		seedFederatedMemory(t, dm, "shared.", "shared-1", "a shared claim", active, 100, unitVec(0, 1, 0))

		// The shared file is replaced by a directory, so the ATTACH on a
		// fresh connection cannot succeed.
		require.NoError(t, os.Remove(dm.SharedAttached()))
		require.NoError(t, os.MkdirAll(dm.SharedAttached(), 0o700))
		drainPooledConnections(t, dm)

		entries, err := dm.LoadSimilarityScanEntriesFederated(0)
		require.Error(t, err,
			"a shared source that cannot be attached must fail the scan")
		require.Nil(t, entries, "a failed scan must not return a partial result")
	})
}

// An installation with NO shared source configured is a normal installation.
// The scan is local-only, and it succeeds. This is the one case where a
// missing shared side is not an error, and it is the case the old code and
// the new code must agree on.
func TestLoadSimilarityScanEntriesFederated_UnconfiguredSharedSourceStaysLocalOnly(t *testing.T) {
	dm := NewTestLocalOnlyDM(t)
	installStubProvider(t, map[string][]float32{"probe": {1, 0, 0}})
	active := CurrentEmbeddingFingerprint().Model

	seedFederatedMemory(t, dm, "", "local-1", "a local claim", active, 100, unitVec(1, 0, 0))
	seedFederatedMemory(t, dm, "", "local-2", "another local claim", active, 100, unitVec(0, 1, 0))

	entries, err := dm.LoadSimilarityScanEntriesFederated(0)
	require.NoError(t, err,
		"an installation with no shared source is normal, not broken")
	require.Len(t, entries, 2)
}

// The filters that made the scan meaningful must survive the move to a
// pinned connection: the embedding-model fingerprint, the newest-first
// ordering, the per-side limit and the cross-side ID dedupe.
func TestLoadSimilarityScanEntriesFederated_KeepsTheFiltersOnBothSides(t *testing.T) {
	dm := NewTestSharedDM(t)
	installStubProvider(t, map[string][]float32{"probe": {1, 0, 0}})
	active := CurrentEmbeddingFingerprint().Model

	// Newest first, on each side.
	seedFederatedMemory(t, dm, "", "local-old", "older local claim", active, 100, unitVec(1, 0, 0))
	seedFederatedMemory(t, dm, "", "local-new", "newer local claim", active, 200, unitVec(1, 0, 0))
	seedFederatedMemory(t, dm, "shared.", "shared-old", "older shared claim", active, 100, unitVec(0, 1, 0))
	seedFederatedMemory(t, dm, "shared.", "shared-new", "newer shared claim", active, 200, unitVec(0, 1, 0))

	// A row stamped with a foreign model is not comparable and must not
	// join the scan on either side.
	seedFederatedMemory(t, dm, "", "local-foreign", "foreign local claim", "some-other-model", 300, unitVec(1, 0, 0))
	seedFederatedMemory(t, dm, "shared.", "shared-foreign", "foreign shared claim", "some-other-model", 300, unitVec(0, 1, 0))

	entries, err := dm.LoadSimilarityScanEntriesFederated(0)
	require.NoError(t, err)

	ids := federatedIDs(entries)
	require.NotContains(t, ids, "local-foreign",
		"a row stamped with a different model must be excluded on the local side")
	require.NotContains(t, ids, "shared-foreign",
		"a row stamped with a different model must be excluded on the shared side")

	// The local side keeps its newest-first order, and the shared side is
	// appended after it.
	require.Equal(t, []string{"local-new", "local-old", "shared-new", "shared-old"}, ids,
		"each side is ordered newest first, local before shared")

	// The limit applies PER SIDE.
	limited, err := dm.LoadSimilarityScanEntriesFederated(1)
	require.NoError(t, err)
	require.Len(t, limited, 2,
		"the limit is a per-source bound, so the union is up to 2x the limit")
}

// A memory promoted to shared can remain in local. The two copies are one
// claim, and the scan must not offer it to the comparison pass twice.
func TestLoadSimilarityScanEntriesFederated_DeduplicatesAcrossSources(t *testing.T) {
	dm := NewTestSharedDM(t)
	installStubProvider(t, map[string][]float32{"probe": {1, 0, 0}})
	active := CurrentEmbeddingFingerprint().Model

	seedFederatedMemory(t, dm, "", "promoted", "the same claim on both sides", active, 100, unitVec(1, 0, 0))
	seedFederatedMemory(t, dm, "shared.", "promoted", "the same claim on both sides", active, 100, unitVec(1, 0, 0))
	seedFederatedMemory(t, dm, "shared.", "other", "a different claim", active, 100, unitVec(0, 1, 0))

	entries, err := dm.LoadSimilarityScanEntriesFederated(0)
	require.NoError(t, err)
	require.Equal(t, []string{"promoted", "other"}, federatedIDs(entries),
		"the same claim on both sides is one entry, not two")
}
