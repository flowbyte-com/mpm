package main

import (
	"testing"

	mpminternal "github.com/flowbyte-com/mpm-core"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests for handleMemoryAdd — specifically the --fact/--tags/--weight
// flag handling that landed as a 2026-08-13 incident fix. Pre-fix: the
// CLI silently passed unrecognized flags through as positional content,
// producing rows like content="--fact X --weight 85 --tags a,b,c"
// with tags=null and weight=1. The function returned success and an
// id, so the bug was undetectable without a direct sqlite3 query —
// the exact shape the always-cross-check-with-sqlite3 rule exists to catch.
//
// Test strategy: invoke handleMemoryAdd against a real DatabaseManager
// (not a mock — the bug was in the integration between the CLI handler
// and the persistence layer), then assert on the persisted row's actual
// columns via direct SQL.

// setupMemoryAddTest wires up the globals that handleMemoryAdd reaches
// into (dbManager singleton, MPM workspace root) and redirects them at
// a hermetic in-memory DB. The CLI handler's globals are
// process-global, so tests that run in sequence would leak between
// each other without this reset. Returns the DM so tests can query
// back to verify persistence.
//
// Pattern adapted from capability_seed_test.go::installTestDM: trigger
// lazy init so the sync.Once is consumed, snapshot the prior values,
// swap in the test DM, restore on cleanup.
func setupMemoryAddTest(t *testing.T) *mpminternal.DatabaseManager {
	t.Helper()
	dm := newTestDMForCmd(t)
	// Point the workspace at a temp dir so handlers that read the
	// mode/persona files don't blow up. The temp dir is empty, which
	// means resolveActiveContext() short-circuits (no current_mode /
	// current_persona files) and activeMode/activePersona stay empty —
	// the handler is fine with that.
	t.Setenv("MPM_WORKSPACE", t.TempDir())
	// Override the dbManager singleton so getMemoryStore() and
	// getDBConcrete() return our hermetic DM instead of the
	// canonical workspace DB.
	_ = getDB() // consume sync.Once so the lazy-init doesn't fire later
	prev := dbManager
	prevErr := dbManagerInitErr
	dbManager = dm
	dbManagerInitErr = nil
	t.Cleanup(func() {
		dbManager = prev
		dbManagerInitErr = prevErr
	})
	return dm
}

// TestMemoryAdd_FactTagsWeightFlagsAllApply is the regression test
// for the 2026-08-13 silent-failure bug. Pre-fix: invoking with
// --fact/--tags/--weight landed flag literals as content and ignored
// tags/weight. Post-fix: each flag is parsed and persisted.
func TestMemoryAdd_FactTagsWeightFlagsAllApply(t *testing.T) {
	dm := setupMemoryAddTest(t)

	code := handleMemoryAdd([]string{
		"--fact", "test-content-via-flag",
		"--tags", "alpha,beta,gamma",
		"--weight", "85",
	})
	require.Equal(t, 0, code, "handleMemoryAdd should return 0 on success")

	// Cross-check the persisted row directly via SQL — this is the
	// always-cross-check-with-sqlite3 rule the bug exposed as a silent
	// failure. If the handler returns success but the row differs,
	// this catches it.
	var dbContent string
	var dbWeight float64
	var dbTagsJSON string
	row := dm.SQLDB().QueryRow(
		`SELECT content, weight, tags FROM memories WHERE content = 'test-content-via-flag'`,
	)
	require.NoError(t, row.Scan(&dbContent, &dbWeight, &dbTagsJSON),
		"memory row should exist after handleMemoryAdd returned 0")
	assert.Equal(t, "test-content-via-flag", dbContent,
		"persisted content must be the --fact value (no flag literals)")
	assert.InDelta(t, 85.0, dbWeight, 0.001,
		"weight must reflect --weight arg, not the silent default of 1.0")
	for _, want := range []string{"alpha", "beta", "gamma"} {
		assert.Contains(t, dbTagsJSON, want,
			"tags must include %q (--tags arg), not the silent default of null", want)
	}
}

// TestMemoryAdd_PositionalContentBackwardCompat verifies that the
// pre-fix invocation pattern (`mpm memory add <content>`) still
// works after the flag-recognizing changes. Backward compat is
// required — scripts in the wild invoke the CLI without flags.
func TestMemoryAdd_PositionalContentBackwardCompat(t *testing.T) {
	dm := setupMemoryAddTest(t)

	code := handleMemoryAdd([]string{"plain positional content"})
	require.Equal(t, 0, code)

	var dbContent string
	var dbWeight float64
	var dbTagsJSON string
	row := dm.SQLDB().QueryRow(
		`SELECT content, weight, tags FROM memories WHERE content = 'plain positional content'`,
	)
	require.NoError(t, row.Scan(&dbContent, &dbWeight, &dbTagsJSON))
	assert.Equal(t, "plain positional content", dbContent)
	assert.InDelta(t, 1.0, dbWeight, 0.001,
		"no --weight → default 1.0 (normalized to 1 in the column via the legacy *10 conversion rule; pre-fix this silently became 10)")
	assert.NotContains(t, dbTagsJSON, "alpha", "no --tags → tags null, not empty string")
}

// TestMemoryAdd_FactWinsOverPositional verifies that --fact takes
// precedence when both are provided (mirrors mpm_memory action=save's
// `params.fact` field semantics). The positional arg is dropped so the
// silent-failure shape doesn't recur.
func TestMemoryAdd_FactWinsOverPositional(t *testing.T) {
	dm := setupMemoryAddTest(t)

	code := handleMemoryAdd([]string{
		"--fact", "via-fact",
		"this-positional-is-dropped",
	})
	require.Equal(t, 0, code)

	var dbContent string
	row := dm.SQLDB().QueryRow(
		`SELECT content FROM memories WHERE content = 'via-fact'`,
	)
	require.NoError(t, row.Scan(&dbContent))
	assert.Equal(t, "via-fact", dbContent,
		"--fact should win the content-precedence rule")

	// The positional arg must NOT have been concatenated or appended.
	var count int
	row = dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM memories WHERE content LIKE '%this-positional-is-dropped%'`,
	)
	require.NoError(t, row.Scan(&count))
	assert.Equal(t, 0, count,
		"positional arg must be dropped, not stored as content")
}

// TestMemoryAdd_InvalidWeightRejected verifies that an unparseable
// --weight is a hard error, not a silent fallback to the default.
// Pre-fix couldn't hit this because --weight wasn't recognized — it
// landed as content. Post-fix must reject explicitly so the operator
// notices a typo.
func TestMemoryAdd_InvalidWeightRejected(t *testing.T) {
	dm := setupMemoryAddTest(t)

	code := handleMemoryAdd([]string{
		"--fact", "x",
		"--weight", "notanumber",
	})
	assert.Equal(t, 1, code, "invalid --weight should return exit code 1")

	// And no row should have been created.
	var count int
	row := dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM memories WHERE content = 'x'`,
	)
	require.NoError(t, row.Scan(&count))
	assert.Equal(t, 0, count,
		"rejected invocation must NOT have created a row")
}

// TestMemoryAdd_TagsTrimsAndDropsEmpties verifies that --tags parsing
// trims whitespace and drops empty entries from comma-separated input.
func TestMemoryAdd_TagsTrimsAndDropsEmpties(t *testing.T) {
	dm := setupMemoryAddTest(t)

	code := handleMemoryAdd([]string{
		"--fact", "trim-test",
		"--tags", "  spaced1 , spaced2 ,,   spaced3  ,,",
	})
	require.Equal(t, 0, code)

	var dbTagsJSON string
	row := dm.SQLDB().QueryRow(
		`SELECT tags FROM memories WHERE content = 'trim-test'`,
	)
	require.NoError(t, row.Scan(&dbTagsJSON))
	for _, want := range []string{"spaced1", "spaced2", "spaced3"} {
		assert.Contains(t, dbTagsJSON, want)
	}
}

// TestMemoryAdd_ForensicMetadataForDefaultedFlags verifies that when
// --weight OR --tags are NOT provided, the metadata does NOT lie by
// recording a default value under the cli_weight/cli_tags keys. The
// always-cross-check-with-sqlite3 rule needs to distinguish "user
// passed --weight 1 explicitly" from "user didn't pass anything and we
// defaulted to 1"; these cli_* metadata keys are the discriminator.
func TestMemoryAdd_ForensicMetadataForDefaultedFlags(t *testing.T) {
	dm := setupMemoryAddTest(t)

	// (a) no flags at all
	handleMemoryAdd([]string{"positional only"})
	// (b) --weight only
	handleMemoryAdd([]string{"--fact", "with-weight", "--weight", "42"})
	// (c) --tags only
	handleMemoryAdd([]string{"--fact", "with-tags", "--tags", "a,b"})

	rows, err := dm.SQLDB().Query(
		`SELECT content, metadata FROM memories WHERE collection = 'memories' ORDER BY created_at`,
	)
	require.NoError(t, err)
	defer rows.Close()

	type memrow struct {
		content  string
		metadata string
	}
	var all []memrow
	for rows.Next() {
		var r memrow
		require.NoError(t, rows.Scan(&r.content, &r.metadata))
		all = append(all, r)
	}
	require.Len(t, all, 3)

	// (a) positional-only: NO cli_weight, NO cli_tags
	assert.NotContains(t, all[0].metadata, "cli_weight",
		"positional-only invocation must NOT record cli_weight (would lie)")
	assert.NotContains(t, all[0].metadata, "cli_tags")

	// (b) --weight supplied: cli_weight present, cli_tags absent
	assert.Contains(t, all[1].metadata, `"cli_weight":42`,
		"--weight supplied → cli_weight metadata must echo the explicit value")
	assert.NotContains(t, all[1].metadata, "cli_tags")

	// (c) --tags supplied: cli_tags present, cli_weight absent
	assert.Contains(t, all[2].metadata, `"cli_tags":["a","b"]`,
		"--tags supplied → cli_tags metadata must echo the explicit values")
	assert.NotContains(t, all[2].metadata, "cli_weight")
}

// TestMemoryAdd_DashDashSeparatorStripped is the regression test for
// the D3 defect. `mpm memory add` documented the `-- ` prefix as the
// way to terminate flag parsing for content that begins with `-`,
// but pre-fix the handler did NOT consume `--` — it joined it with the
// rest of the positional args verbatim. So `mpm memory add -- -flaglike`
// stored `-- -flaglike` (with the leading `-- ` literal) instead of
// `-flaglike`. The audit reported this as "mpm add rejects content
// beginning with `-`"; on inspection, the actual defect is that the
// escape hatch printed in the help text did not actually work for the
// `mpm memory add` path.
//
// Fix: when the first positional arg is the literal token `--`, drop
// it from contentArgs so the remaining content is stored verbatim,
// matching POSIX-utility convention. Does NOT change flag parsing for
// tokens that begin with a single dash — those continue to be rejected
// at parse time as ambiguous (preserves the 2026-08-13 silent-failure
// invariant).
func TestMemoryAdd_DashDashSeparatorStripped(t *testing.T) {
	dm := setupMemoryAddTest(t)

	// The audit's reported form: content begins with `-` after the
	// separator. Pre-fix: stored as `-- ---yaml-front-matter`.
	// Post-fix: stored as `---yaml-front-matter`.
	code := handleMemoryAdd([]string{"--", "---yaml-front-matter"})
	require.Equal(t, 0, code)

	var gotContent string
	row := dm.SQLDB().QueryRow(
		`SELECT content FROM memories WHERE content = '---yaml-front-matter' ORDER BY created_at DESC LIMIT 1`,
	)
	require.NoError(t, row.Scan(&gotContent),
		"mpm memory add -- ---yaml-front-matter should have stored `---yaml-front-matter`")
	assert.Equal(t, "---yaml-front-matter", gotContent,
		"the leading `--` separator must be consumed, not stored verbatim")

	// Negative control: a single-dash leading token in positional
	// content (without the `--` separator) is preserved verbatim.
	// Pre-fix behaviour accepted `-foo` and stored it as-is. The
	// fix must not change that behaviour — only consume `--` when
	// it appears in the separator position.
	handleMemoryAdd([]string{"-leading-single-dash"})
	var single string
	row2 := dm.SQLDB().QueryRow(
		`SELECT content FROM memories WHERE content = '-leading-single-dash' LIMIT 1`,
	)
	require.NoError(t, row2.Scan(&single),
		"single-dash leading content must still be stored verbatim (no rejection)")
	assert.Equal(t, "-leading-single-dash", single)
}

// TestMemoryAdd_DashDashSeparatorWithFlagsBefore is the S1-era
// strengthening of the D3 regression. Verifies that flags appearing
// before the `--` separator are still parsed correctly, while tokens
// after the separator are stored verbatim as content. This exercises
// the shared `splitOnDashDash` helper end-to-end through the public
// `handleMemoryAdd` path, not just the helper in isolation.
func TestMemoryAdd_DashDashSeparatorWithFlagsBefore(t *testing.T) {
	dm := setupMemoryAddTest(t)

	// Recognised flags before separator + raw content after.
	// `--weight 85` and `--tags alpha,beta` must be honoured; the
	// remaining tokens (`--`, `---yaml-front-matter`, `--looks-like-flag`)
	// are raw content.
	code := handleMemoryAdd([]string{
		"--weight", "85",
		"--tags", "alpha,beta",
		"--", "---yaml-front-matter", "--looks-like-flag",
	})
	require.Equal(t, 0, code)

	// Find the row by its unique weight + tag combo to avoid matching
	// rows left by earlier tests in this file.
	row := dm.SQLDB().QueryRow(
		`SELECT content, weight, tags FROM memories
		 WHERE weight = 85 AND tags LIKE '%alpha%beta%'
		 ORDER BY created_at DESC LIMIT 1`,
	)
	var gotContent string
	var gotWeight float64
	var gotTags string
	require.NoError(t, row.Scan(&gotContent, &gotWeight, &gotTags),
		"row with weight=85 and tags=alpha,beta should exist")
	assert.Equal(t, "---yaml-front-matter --looks-like-flag", gotContent,
		"content after `--` must be joined verbatim, with all dash-prefixed tokens preserved")
	assert.Equal(t, 85.0, gotWeight, "--weight before separator must be honoured")
}

// TestMemoryAdd_RepeatedDashDashAfterSeparator verifies that repeated
// `--` tokens after the first separator are kept in raw content
// (per POSIX: only the first `--` ends option processing). The
// pre-S1 implementation stored `-- ---yaml-front-matter` for input
// `["--", "--", "---yaml-front-matter"]`; the S1 fix returns the
// second `--` and the dash-prefixed token as content.
func TestMemoryAdd_RepeatedDashDashAfterSeparator(t *testing.T) {
	dm := setupMemoryAddTest(t)

	code := handleMemoryAdd([]string{"--", "--", "---yaml-front-matter"})
	require.Equal(t, 0, code)

	// The content must be `-- ---yaml-front-matter` (both `--`
	// tokens preserved verbatim) — NOT `---yaml-front-matter` (which
	// would mean the second `--` was silently consumed).
	row := dm.SQLDB().QueryRow(
		`SELECT content FROM memories
		 WHERE content = '-- ---yaml-front-matter'
		 ORDER BY created_at DESC LIMIT 1`,
	)
	var gotContent string
	require.NoError(t, row.Scan(&gotContent),
		"row with content `-- ---yaml-front-matter` (both `--` tokens preserved) should exist")
	assert.Equal(t, "-- ---yaml-front-matter", gotContent,
		"only the FIRST `--` is the separator; subsequent `--` tokens are content")
}
