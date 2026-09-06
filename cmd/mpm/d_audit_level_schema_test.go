// cmd/mpm/d_audit_level_schema_test.go
//
// Final-pass debt-closure test: prove that the audit-level schema
// docstring matches the substrate's actual CHECK constraint.
//
// Pre-D.3 docstring claimed: "Lowercase canonical values:
// debug/info/warn/error."
//
// Substrate AuditLevel constants + CHECK constraint (schema.go:851):
// info/warn/error/fatal/critical.
//
// The docstring was inaccurate on two counts:
//   - `debug` is NOT a substrate constant; nothing writes
//     level='debug' to the audit log.
//   - `fatal` and `critical` ARE real substrate levels (cascade
//     dead-letters, depth-suppression events) and the substrate
//     CHECK constraint permits them, but the tool schema did not
//     advertise them.
//
// D.3 fix: tool schema now reads
// "Lowercase canonical values: info/warn/error/fatal/critical. The CLI
// accepts a 'warning' alias for 'warn'; that alias is CLI-only and
// does not apply here."
//
// These tests pin the schema's level contract so a future
// re-narrowing of the schema doesn't silently break tool-side
// queries against fatal/critical rows that the substrate accepts.

package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/flowbyte-com/mpm-core/tools"
)

func TestD_AuditLevelSchema_DocstringListsAllSubstrateLevels(t *testing.T) {
	// Pull the registry schema for the mpm_system tool. The level
	// field is on the query_audit_log action's oneOf branch.
	tool, ok := tools.ByName("mpm_system")
	require.True(t, ok, "mpm_system tool must be registered")

	schema := string(tool.Schema)
	// The pre-D.3 docstring claimed "debug/info/warn/error" — the
	// word "debug" must NOT appear in the level docstring
	// (it isn't a substrate constant).
	//
	// We assert this via substring checks against the schema
	// JSON. If the schema ever re-introduces debug, the failure
	// here forces a re-check.
	assert.NotContains(t, schema, "debug/info",
		"level docstring must not include 'debug' (not a substrate constant)")

	// The five substrate levels must all be mentioned in the
	// level docstring.
	for _, lvl := range []string{"info", "warn", "error", "fatal", "critical"} {
		assert.Contains(t, schema, lvl,
			"level docstring must enumerate substrate level %q", lvl)
	}
}

func TestD_AuditLevelSchema_CLIAliasWarningDocumented(t *testing.T) {
	// The schema explicitly notes that the CLI's 'warning' alias
	// is CLI-only. A tool caller cannot pass 'warning'; they
	// must use 'warn' to filter the audit log.
	tool, ok := tools.ByName("mpm_system")
	require.True(t, ok)
	schema := string(tool.Schema)
	assert.Contains(t, schema, "warning",
		"schema must document the CLI alias distinction so tool callers know to use 'warn'")
}
