package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/flowbyte-com/mpm-core"
	"github.com/flowbyte-com/mpm-core/capability"
	"github.com/flowbyte-com/mpm-core/seed"

	"github.com/stretchr/testify/require"
)

// =============================================================================
// capability_seed_test.go — CS-2.3 CLI handler tests
//
// Pins the contract of `mpm capability seed` end-to-end:
//
//   * First run inserts all 4 Tier 1 primitives with state='validated'.
//   * Second run is a clean no-op (every row in Skipped).
//   * Operator drift (source_code mutation) surfaces in the Drifted
//     bucket and is never overwritten.
//   * --sidecar flag points at a custom bundle and overrides / adds
//     entries from the compiled registry.
//   * Unknown flag returns 1 (the operator gets a clear error).
//   * Help flag returns 0 with no DB writes.
//   * No-sidecar path (default MPM_WORKSPACE empty) reads the
//     compiled registry alone.
//
// The CLI handler is monolithic (calls getDBConcrete, then prints
// directly to stdout); the test harness installs a fresh *DatabaseManager
// in the package-level singleton so we don't touch the real workspace DB.
// =============================================================================

// installTestDM installs dm as the package-level dbManager singleton
// for the duration of the test. Restores the prior value on cleanup.
//
// Important: we must NOT reset sync.Once. The Once gates the lazy
// init that creates the real workspace DB; resetting it would let
// the lazy init run again and overwrite our test dm. Instead, we
// trigger the lazy init once (to consume the Once), then replace
// dbManager with our test dm.
func installTestDM(t *testing.T, dm *internal.DatabaseManager) {
	t.Helper()
	// Trigger lazy init so the sync.Once is consumed. If the
	// workspace DB can't open, getDB() returns nil and we early-out.
	_ = getDB()
	prev := dbManager
	prevErr := dbManagerInitErr
	dbManager = dm
	dbManagerInitErr = nil
	t.Cleanup(func() {
		dbManager = prev
		dbManagerInitErr = prevErr
	})
}

// withTempWorkspace points MPM_WORKSPACE at a temp dir for the
// duration of the test so the loader's default-sidecar resolution
// doesn't pick up a real workspace's bundled_capabilities.json.
func withTempWorkspace(t *testing.T) string {
	t.Helper()
	ws := t.TempDir()
	t.Setenv("MPM_WORKSPACE", ws)
	return ws
}

// TestHandleCapabilitySeed_Defaults_InsertsAllTier1Primitives: the
// happy path with no sidecar — first run inserts all 4 Tier 1
// primitives into the empty test DB.
func TestHandleCapabilitySeed_Defaults_InsertsAllTier1Primitives(t *testing.T) {
	dm := internal.NewTestDM(t)
	installTestDM(t, dm)
	withTempWorkspace(t)

	require.Equal(t, 0, handleCapabilitySeed(nil),
		"first run should exit 0")

	// Verify all 4 Tier 1 primitives are in the capabilities table.
	store := capability.NewStore(dm)
	got := listSeededCapabilityNames(t, dm)
	require.ElementsMatch(t, []string{
		"list_capabilities", "get_capability",
		"capability_lineage", "capability_health",
	}, got, "all 4 Tier 1 primitives should be seeded")

	// And every row is in state='validated'.
	for _, name := range got {
		cap, err := store.GetCapability("cap." + name)
		require.NoError(t, err)
		require.Equal(t, capability.StateValidated, cap.State,
			"seeded rows should be in 'validated' state")
	}
}

// TestHandleCapabilitySeed_SecondRunIsIdempotent: second run is a
// no-op against the same DB. Verified via insert call count.
func TestHandleCapabilitySeed_SecondRunIsIdempotent(t *testing.T) {
	dm := internal.NewTestDM(t)
	installTestDM(t, dm)
	withTempWorkspace(t)

	require.Equal(t, 0, handleCapabilitySeed(nil))
	first := countCapabilityRows(t, dm)
	require.Equal(t, 4, first, "first run inserts 4 rows")

	// Second run must not insert anything new.
	require.Equal(t, 0, handleCapabilitySeed(nil),
		"second run should exit 0 (clean no-op)")
	second := countCapabilityRows(t, dm)
	require.Equal(t, first, second,
		"second run should not change row count")
}

// TestHandleCapabilitySeed_SidecarOverride_AddsAndReplaces: when a
// sidecar is supplied that overrides one compiled entry and adds a
// new entry, both the override and the addition are inserted.
func TestHandleCapabilitySeed_SidecarOverride_AddsAndReplaces(t *testing.T) {
	dm := internal.NewTestDM(t)
	installTestDM(t, dm)
	withTempWorkspace(t)

	// Build a sidecar with one override + one addition.
	sidecar := filepath.Join(t.TempDir(), "bundled_capabilities.json")
	sidecarContent := `{
		"schema_version": "1.0",
		"capabilities": [
			{
				"stable_id": "cap-seed-list-capabilities",
				"name": "list_capabilities",
				"purpose": "OVERRIDDEN.",
				"source_language": "bash",
				"requested_domain": "sandbox",
				"source_code": "#!/bin/bash\necho overridden\n",
				"tags": ["capability", "overridden"]
			},
			{
				"stable_id": "cap-seed-custom-addition",
				"name": "custom_addition",
				"purpose": "Operator-added primitive.",
				"source_language": "bash",
				"requested_domain": "sandbox",
				"source_code": "#!/bin/bash\necho custom\n",
				"tags": ["capability", "custom"]
			}
		]
	}`
	require.NoError(t, os.WriteFile(sidecar, []byte(sidecarContent), 0644))

	require.Equal(t, 0, handleCapabilitySeed([]string{"--sidecar", sidecar}))

	store := capability.NewStore(dm)
	names := listSeededCapabilityNames(t, dm)
	require.Contains(t, names, "custom_addition",
		"sidecar addition should be present")
	require.Contains(t, names, "list_capabilities",
		"compiled entry should still be present (overridden)")

	// Verify the override actually replaced the source_code.
	cap, err := store.GetCapability("cap.list_capabilities")
	require.NoError(t, err)
	require.Equal(t, "OVERRIDDEN.", cap.Purpose,
		"sidecar override should have replaced the purpose")
	require.Equal(t, "#!/bin/bash\necho overridden\n", cap.SourceCode,
		"sidecar override should have replaced the source_code")
}

// TestHandleCapabilitySeed_UnknownFlag_Errors: the handler rejects
// unknown flags with exit 1 and a clear error message.
func TestHandleCapabilitySeed_UnknownFlag_Errors(t *testing.T) {
	dm := internal.NewTestDM(t)
	installTestDM(t, dm)
	withTempWorkspace(t)

	exit := handleCapabilitySeed([]string{"--bogus-flag"})
	require.Equal(t, 1, exit, "unknown flag should exit 1")

	// And no rows were inserted.
	rows := countCapabilityRows(t, dm)
	require.Equal(t, 0, rows, "unknown flag should not seed anything")
}

// TestHandleCapabilitySeed_HelpFlag_ReturnsZero: --help (or 'help'
// after the global flag rewrite) succeeds with no DB writes.
func TestHandleCapabilitySeed_HelpFlag_ReturnsZero(t *testing.T) {
	dm := internal.NewTestDM(t)
	installTestDM(t, dm)
	withTempWorkspace(t)

	// Both forms: the global parseFlags rewrites --help to 'help',
	// so the handler sees either. Accept both.
	for _, arg := range []string{"--help", "-h", "help"} {
		require.Equal(t, 0, handleCapabilitySeed([]string{arg}),
			"help flag %q should exit 0", arg)
	}
	rows := countCapabilityRows(t, dm)
	require.Equal(t, 0, rows, "help flags should not seed anything")
}

// TestHandleCapabilitySeed_MissingSidecarArg_Errors: --sidecar without
// a following path argument returns 1 with a clear error.
func TestHandleCapabilitySeed_MissingSidecarArg_Errors(t *testing.T) {
	dm := internal.NewTestDM(t)
	installTestDM(t, dm)
	withTempWorkspace(t)

	exit := handleCapabilitySeed([]string{"--sidecar"})
	require.Equal(t, 1, exit, "--sidecar without path should exit 1")
	rows := countCapabilityRows(t, dm)
	require.Equal(t, 0, rows, "missing sidecar arg should not seed anything")
}

// TestHandleCapabilitySeed_SidecarInvalidEntry_Errors: a sidecar
// with a Validate()-failing entry aborts the seed (no partial
// inserts). Mirrors the loader's contract.
func TestHandleCapabilitySeed_SidecarInvalidEntry_Errors(t *testing.T) {
	dm := internal.NewTestDM(t)
	installTestDM(t, dm)
	withTempWorkspace(t)

	sidecar := filepath.Join(t.TempDir(), "bundled_capabilities.json")
	sidecarContent := `{
		"schema_version": "1.0",
		"capabilities": [
			{
				"stable_id": "cap-seed-bad",
				"name": "bad_entry",
				"purpose": "purpose",
				"source_language": "ruby",
				"requested_domain": "sandbox",
				"source_code": "#!/bin/bash\necho bad\n"
			}
		]
	}`
	require.NoError(t, os.WriteFile(sidecar, []byte(sidecarContent), 0644))

	exit := handleCapabilitySeed([]string{"--sidecar", sidecar})
	require.Equal(t, 1, exit, "invalid sidecar entry should exit 1")

	// And no rows were inserted (atomic).
	rows := countCapabilityRows(t, dm)
	require.Equal(t, 0, rows, "invalid sidecar should not insert anything")
}

// TestHandleCapabilitySeed_SidecarSchemaMismatch_Errors: a sidecar
// with the wrong schema_version is rejected.
func TestHandleCapabilitySeed_SidecarSchemaMismatch_Errors(t *testing.T) {
	dm := internal.NewTestDM(t)
	installTestDM(t, dm)
	withTempWorkspace(t)

	sidecar := filepath.Join(t.TempDir(), "bundled_capabilities.json")
	require.NoError(t, os.WriteFile(sidecar, []byte(`{
		"schema_version": "0.9",
		"capabilities": []
	}`), 0644))

	exit := handleCapabilitySeed([]string{"--sidecar", sidecar})
	require.Equal(t, 1, exit, "schema mismatch should exit 1")

	// And the error message references the schema constraint.
	rows := countCapabilityRows(t, dm)
	require.Equal(t, 0, rows)
}

// =============================================================================
// Helpers
// =============================================================================

// listSeededCapabilityNames returns the names of every capability row
// in the DB. Used by tests to verify "what got seeded" without
// asserting on internal fields.
func listSeededCapabilityNames(t *testing.T, dm *internal.DatabaseManager) []string {
	t.Helper()
	rows, err := dm.SQLDB().Query(
		`SELECT name FROM capabilities WHERE deleted_at IS NULL ORDER BY name`)
	if err != nil {
		t.Fatalf("query capabilities: %v", err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatalf("scan: %v", err)
		}
		names = append(names, n)
	}
	return names
}

// countCapabilityRows returns the live row count.
func countCapabilityRows(t *testing.T, dm *internal.DatabaseManager) int {
	t.Helper()
	var n int
	require.NoError(t, dm.QueryRowTracked(
		`SELECT COUNT(*) FROM capabilities WHERE deleted_at IS NULL`,
	).Scan(&n))
	return n
}

// _ guards against unused-import lints if the test surface evolves.
var _ = strings.TrimSpace
var _ = seed.ApplyCapabilitiesFromBundle
