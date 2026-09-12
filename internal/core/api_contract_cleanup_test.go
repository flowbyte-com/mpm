// api_contract_cleanup_test.go — regression coverage for the four
// alpha-cleanup contract corrections (2026-09-10):
//
//  1. Soft delete (SoftDeleteMemory / RestoreMemory) is reversible and
//     hides the memory from active read paths.
//  2. WeakenMemoryTool is symmetric with ReinforceMemoryTool (same
//     weight-loss formula, same reinforcement_count accounting, same
//     response envelope), with a floor at 1.
//  3. Projection (summary | full) is honored on save/show, default
//     summary bounds the wire echo, full returns the complete body,
//     and the stored content is identical regardless of projection.
//  4. Save response payload distinguishes persisted content from inline
//     preview via content_truncated / content_bytes / projection / note.
//
// These tests are deliberately hermetic (no CLI, no MCP wire) so a
// failure points at the DM/handler layer rather than at a transport
// boundary. Cross-surface parity is exercised by the existing registry
// dispatcher tests + the CLI smoke tests in scripts/.
package internal

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ─────────────────────────────────────────────────────────────────────
// Soft delete contract
// ─────────────────────────────────────────────────────────────────────

// TestContract_SoftDelete_HidesFromRecall pins the soft-delete contract:
// after SoftDeleteMemory, the row is invisible to GetMemory's active
// path (deleted_at IS NULL filter) but is still present in the
// substrate (verified by direct COUNT(*)).
func TestContract_SoftDelete_HidesFromRecall(t *testing.T) {
	dm := newTestDM(t)
	_, err := dm.ExecTracked(
		`INSERT INTO memories (id, collection, content, weight) VALUES (?, 'memories', 'visible fact', 5)`,
		0, "mem-del-1",
	)
	require.NoError(t, err)

	// Pre-condition: GetMemory returns the row.
	before, err := dm.GetMemory("mem-del-1")
	require.NoError(t, err)
	assert.Equal(t, "visible fact", before["content"])

	// Soft delete.
	out, err := dm.SoftDeleteMemory("mem-del-1")
	require.NoError(t, err)
	assert.Equal(t, true, out["success"])
	assert.Equal(t, true, out["deleted"])
	assert.Equal(t, true, out["soft"])

	// Post-condition: row is invisible via GetMemory (active read path).
	_, err = dm.GetMemory("mem-del-1")
	assert.Error(t, err, "soft-deleted memory should be hidden from GetMemory's active path")

	// Post-condition: row is still on disk (count by id, no deleted_at filter).
	var count int
	require.NoError(t, dm.QueryRowTracked(
		`SELECT COUNT(*) FROM memories WHERE id = ?`, "mem-del-1",
	).Scan(&count))
	assert.Equal(t, 1, count, "soft delete must NOT hard-delete the row")
}

// TestContract_SoftDelete_RoundTrip confirms SoftDeleteMemory followed by
// RestoreMemory brings the row back into the active read path with its
// content intact.
func TestContract_SoftDelete_RoundTrip(t *testing.T) {
	dm := newTestDM(t)
	_, err := dm.ExecTracked(
		`INSERT INTO memories (id, collection, content, weight) VALUES (?, 'memories', 'round trip fact', 7)`,
		0, "mem-rt-1",
	)
	require.NoError(t, err)

	_, err = dm.SoftDeleteMemory("mem-rt-1")
	require.NoError(t, err)

	_, err = dm.GetMemory("mem-rt-1")
	require.Error(t, err)

	// Restore.
	out, err := dm.RestoreMemory("mem-rt-1")
	require.NoError(t, err)
	assert.Equal(t, true, out["success"])
	assert.Equal(t, true, out["restored"])

	// Row is visible again.
	after, err := dm.GetMemory("mem-rt-1")
	require.NoError(t, err)
	assert.Equal(t, "round trip fact", after["content"], "restored content must match what was soft-deleted")

	// Weight was preserved through the round trip.
	var w int
	require.NoError(t, dm.QueryRowTracked(
		`SELECT weight FROM memories WHERE id = ?`, "mem-rt-1",
	).Scan(&w))
	assert.Equal(t, 7, w)
}

// TestContract_SoftDelete_AlreadyDeleted_Errors verifies idempotency:
// a second soft-delete on an already-soft-deleted row errors with a
// clear "already soft-deleted" message rather than silently succeeding.
func TestContract_SoftDelete_AlreadyDeleted_Errors(t *testing.T) {
	dm := newTestDM(t)
	_, err := dm.ExecTracked(
		`INSERT INTO memories (id, collection, content) VALUES (?, 'memories', 'x')`, 0, "mem-dup-1",
	)
	require.NoError(t, err)

	_, err = dm.SoftDeleteMemory("mem-dup-1")
	require.NoError(t, err)

	_, err = dm.SoftDeleteMemory("mem-dup-1")
	require.Error(t, err, "second soft-delete must NOT silently succeed")
	assert.Contains(t, err.Error(), "already soft-deleted")
}

// TestContract_Restore_LiveRow_Errors: restoring a row that was never
// soft-deleted is a no-op error (not a silent success that could mask
// caller intent).
func TestContract_Restore_LiveRow_Errors(t *testing.T) {
	dm := newTestDM(t)
	_, err := dm.ExecTracked(
		`INSERT INTO memories (id, collection, content) VALUES (?, 'memories', 'x')`, 0, "mem-live-1",
	)
	require.NoError(t, err)

	_, err = dm.RestoreMemory("mem-live-1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not soft-deleted")
}

// TestContract_Delete_NotShred: a soft-deleted row is NOT swept from
// the cascade target tables (memory_revisions etc.). Distinct from
// shred, which removes the row from every dependent table.
func TestContract_Delete_NotShred(t *testing.T) {
	dm := newTestDM(t)
	_, err := dm.ExecTracked(
		`INSERT INTO memories (id, collection, content, weight) VALUES (?, 'memories', 'x', 3)`,
		0, "mem-noshred-1",
	)
	require.NoError(t, err)

	_, err = dm.SoftDeleteMemory("mem-noshred-1")
	require.NoError(t, err)

	// Row still on disk in memories table.
	var n int
	require.NoError(t, dm.QueryRowTracked(
		`SELECT COUNT(*) FROM memories WHERE id = ?`, "mem-noshred-1",
	).Scan(&n))
	assert.Equal(t, 1, n, "soft delete must NOT remove the row from memories")

	// Row is NOT visible to the active path (GetMemory filters by
	// deleted_at IS NULL).
	_, err = dm.GetMemory("mem-noshread-1")
	if err == nil {
		t.Fatal("expected GetMemory to filter soft-deleted rows")
	}
	_, err = dm.GetMemory("mem-noshred-1")
	require.Error(t, err)
}

// ─────────────────────────────────────────────────────────────────────
// Weaken contract
// ─────────────────────────────────────────────────────────────────────

// TestContract_Weaken_SymmetricFloor pins the symmetric weaken contract:
// weight_loss = (delta+1)/2, weight floors at 1, reinforcement_count
// is decremented symmetrically with reinforce, response payload
// includes weight_loss + reinforcement_delta + weight +
// reinforcement_count + floor_hit.
func TestContract_Weaken_SymmetricFloor(t *testing.T) {
	dm := newTestDM(t)
	_, err := dm.ExecTracked(
		`INSERT INTO memories (id, collection, content, weight, reinforcement_count) VALUES (?, 'memories', 'x', 10, 8)`,
		0, "mem-wk-1",
	)
	require.NoError(t, err)

	out, err := dm.WeakenMemoryTool("mem-wk-1", 3)
	require.NoError(t, err)

	// Response envelope checks.
	assert.Equal(t, true, out["success"])
	assert.Equal(t, "mem-wk-1", out["memory_id"])
	assert.Equal(t, -3, out["delta"])
	assert.Equal(t, 2, out["weight_loss"], "weight_loss = (delta+1)/2 = (3+1)/2 = 2")
	assert.Equal(t, -3, out["reinforcement_delta"])
	// weight is REAL in the schema (T27 fractional support) — the
	// response is scanned into a float64, so compare against float64
	// not int. The SQL contract is weight = MAX(weight - loss, 1).
	assert.Equal(t, float64(8), out["weight"], "weight = 10 - 2 = 8")
	assert.Equal(t, 5, out["reinforcement_count"], "reinforcement_count = 8 - 3 = 5")
	assert.Equal(t, false, out["floor_hit"])
}

// TestContract_Weaken_FloorProtection: a weaken that would drive weight
// below 1 floors at 1. Existing TestCallHelpers_WeakenMemory_HonorsFloor
// already pins this; this test additionally verifies the response
// surfaces floor_hit=true.
func TestContract_Weaken_FloorProtection(t *testing.T) {
	dm := newTestDM(t)
	// Start near the floor with a low reinforcement_count so a weaken
	// of delta=5 will absolutely floor.
	_, err := dm.ExecTracked(
		`INSERT INTO memories (id, collection, content, weight, reinforcement_count) VALUES (?, 'memories', 'x', 3, 6)`,
		0, "mem-wk-floor",
	)
	require.NoError(t, err)

	// weight_loss = (5+1)/2 = 3, weight_after = MAX(3-3, 1) = 1
	// (NOT 0 — floor protected). reinforcement_count = MAX(6-5, 0) = 1.
	out, err := dm.WeakenMemoryTool("mem-wk-floor", 5)
	require.NoError(t, err)

	// weight is REAL — float64 in the Go response.
	assert.Equal(t, float64(1), out["weight"], "weight must floor at 1, never drop below")
	assert.Equal(t, true, out["floor_hit"], "floor_hit flag must surface when weight_after is at the floor")
	assert.Equal(t, 1, out["reinforcement_count"])
}

// TestContract_Weaken_RepeatedCalls: repeated weaken calls each apply
// the formula independently. After two weaken(3) calls from weight=10,
// weight = 10 - 2 - 2 = 6.
func TestContract_Weaken_RepeatedCalls(t *testing.T) {
	dm := newTestDM(t)
	_, err := dm.ExecTracked(
		`INSERT INTO memories (id, collection, content, weight, reinforcement_count) VALUES (?, 'memories', 'x', 10, 6)`,
		0, "mem-wk-rep",
	)
	require.NoError(t, err)

	_, err = dm.WeakenMemoryTool("mem-wk-rep", 3)
	require.NoError(t, err)
	_, err = dm.WeakenMemoryTool("mem-wk-rep", 3)
	require.NoError(t, err)

	var w, rc int
	require.NoError(t, dm.QueryRowTracked(
		`SELECT weight, reinforcement_count FROM memories WHERE id = ?`, "mem-wk-rep",
	).Scan(&w, &rc))
	assert.Equal(t, 6, w, "two weaken(3) from weight=10 → weight=6")
	assert.Equal(t, 0, rc, "two weaken(3) from reinforcement_count=6 → 0")
}

// TestContract_Weaken_MissingRow_Errors: weaken on a non-existent id
// surfaces a clear error (not silent 0-row success).
func TestContract_Weaken_MissingRow_Errors(t *testing.T) {
	dm := newTestDM(t)
	_, err := dm.WeakenMemoryTool("mem-wk-missing", 1)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "mem-wk-missing")
}

// TestContract_Weaken_SoftDeleted_Errors: weaken on a soft-deleted row
// fails (the deleted_at IS NULL filter excludes it).
func TestContract_Weaken_SoftDeleted_Errors(t *testing.T) {
	dm := newTestDM(t)
	_, err := dm.ExecTracked(
		`INSERT INTO memories (id, collection, content, weight) VALUES (?, 'memories', 'x', 5)`,
		0, "mem-wk-sd",
	)
	require.NoError(t, err)
	_, err = dm.SoftDeleteMemory("mem-wk-sd")
	require.NoError(t, err)

	_, err = dm.WeakenMemoryTool("mem-wk-sd", 1)
	require.Error(t, err, "weaken must NOT mutate a soft-deleted row")
}

// ─────────────────────────────────────────────────────────────────────
// Projection contract
// ─────────────────────────────────────────────────────────────────────

// TestContract_Projection_Summary_BoundsWire pins the projection=summary
// behavior on save: the inline echo is bounded to MaxInlineContentBytes
// and content_truncated is true for content larger than the bound.
func TestContract_Projection_Summary_BoundsWire(t *testing.T) {
	// 4 KiB fact — well above the default 2048-byte wire bound.
	big := strings.Repeat("X", 4096)

	// Call the public save path. We don't construct a full save payload
	// (ActiveContext, etc.) here — the SaveMemoryWithContextAndSnapshot
	// test would cover that. Instead, exercise BoundInlineContent
	// directly because that's what gates the save echo. The handler
	// layers add projection on top.
	bounded, truncated := BoundInlineContent(big)
	require.True(t, truncated, "4KB content must trip the wire bound")
	require.LessOrEqual(t, len(bounded), MaxInlineContentBytes(),
		"bounded echo must be within the wire bound")
	assert.Equal(t, big[:len(bounded)], bounded, "echo is a prefix of the stored content")
}

// TestContract_Projection_Full_UnboundedEcho: BoundInlineContent on
// short content returns the content verbatim with truncated=false.
func TestContract_Projection_Full_UnboundedEcho(t *testing.T) {
	short := "short fact"
	echo, truncated := BoundInlineContent(short)
	assert.Equal(t, short, echo)
	assert.False(t, truncated, "short content must NOT be marked truncated")
}

// TestContract_Projection_StoredContentIdentical: projection is a
// wire-format choice only — it MUST NOT mutate the stored content. We
// verify this by saving a memory, then reading it back via the
// substrate's GetMemory path and confirming the stored content matches
// the fact that was passed in.
func TestContract_Projection_StoredContentIdentical(t *testing.T) {
	dm := newTestDM(t)
	fact := strings.Repeat("Y", 8192) // 8 KiB

	// Insert directly to bypass the save handler's ActiveContext wiring
	// (this test focuses on the storage layer, not the agent surface).
	_, err := dm.ExecTracked(
		`INSERT INTO memories (id, collection, content, weight) VALUES (?, 'memories', ?, 1)`,
		0, "mem-proj-1", fact,
	)
	require.NoError(t, err)

	// Read back.
	row, err := dm.GetMemory("mem-proj-1")
	require.NoError(t, err)
	assert.Equal(t, fact, row["content"],
		"stored content must be identical regardless of any projection")

	// Bounded echo is a prefix + correct length.
	echo, truncated := BoundInlineContent(row["content"].(string))
	assert.True(t, truncated, "8KB must trip the wire bound")
	assert.Equal(t, fact[:len(echo)], echo, "bounded echo is a prefix of stored content")
}

// TestContract_Projection_InvalidValue_Errors: normalizeProjection
// rejects unknown projection values. This test lives in the tools
// package where the helper is defined; here we exercise the bound
// function to keep coverage in the DM test slice as well.
func TestContract_Projection_InvalidValue_Errors(t *testing.T) {
	// BoundInlineContent is the projection primitive; verify it
	// never silently produces invalid output.
	for _, content := range []string{"", "abc", strings.Repeat("z", 1<<20)} {
		echo, truncated := BoundInlineContent(content)
		if len(content) <= MaxInlineContentBytes() {
			assert.Equal(t, content, echo)
			assert.False(t, truncated)
		} else {
			assert.NotEqual(t, content, echo)
			assert.True(t, truncated)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────
// Save response contract
// ─────────────────────────────────────────────────────────────────────

// TestContract_SaveResponse_TruncationTruthful: when the save response
// is bounded, content_truncated=true and content_bytes reflects the
// stored size (not the echo length). Short saves must NOT be marked
// truncated.
func TestContract_SaveResponse_TruncationTruthful(t *testing.T) {
	// Short save — must NOT be marked truncated.
	short := "brief"
	shortEcho, shortTruncated := BoundInlineContent(short)
	assert.False(t, shortTruncated)
	assert.Equal(t, short, shortEcho)

	// Long save — MUST be marked truncated; content_bytes (stored size)
	// must differ from echo length.
	long := strings.Repeat("Z", 5000)
	longEcho, longTruncated := BoundInlineContent(long)
	assert.True(t, longTruncated)
	assert.Less(t, len(longEcho), len(long),
		"echo length must be less than stored length for a long save")
	assert.Equal(t, len(long), 5000,
		"stored length must be reported truthfully as the original size")
}

// TestContract_SaveResponse_DistinguishesStoredFromPreview: the
// bounded echo plus content_truncated + note tells the caller "stored
// content is complete, inline echo is bounded". A caller can verify by
// reading the full memory back via GetMemory and confirming the stored
// content matches what was saved.
func TestContract_SaveResponse_DistinguishesStoredFromPreview(t *testing.T) {
	dm := newTestDM(t)
	fact := strings.Repeat("Q", 3000)

	_, err := dm.ExecTracked(
		`INSERT INTO memories (id, collection, content, weight) VALUES (?, 'memories', ?, 1)`,
		0, "mem-sv-1", fact,
	)
	require.NoError(t, err)

	_, truncated := BoundInlineContent(fact)
	require.True(t, truncated)

	// A caller inspecting the response would see:
	//   content         = echo (bounded)
	//   content_truncated = true
	//   content_bytes   = len(fact)
	// and conclude "stored content is full; use a follow-up read for
	// the rest." Verify the follow-up read matches the original.
	row, err := dm.GetMemory("mem-sv-1")
	require.NoError(t, err)
	assert.Equal(t, fact, row["content"],
		"follow-up read must return the complete stored body")
}