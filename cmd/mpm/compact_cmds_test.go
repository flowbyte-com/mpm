// compact_cmds_test.go — operator surface of the deferral lifecycle.
//
// The contract these pin is mostly negative space. The compaction
// drain runs unattended on a schedule; if the operator commands on this
// surface can be reached by anything else, or can run more broadly
// than they say they will, the safety argument in
// docs/archive/2026-09-30-compact-refusal-lifecycle.md §3.3 R1/R2 is
// void.
//
// Every test uses an in-memory DM installed over the package
// singleton. No production DB, no provider, no live state.

package main

import (
	"encoding/json"
	"strings"
	"testing"

	internal "github.com/flowbyte-com/mpm-core"
	"github.com/flowbyte-com/mpm-core/tools"
)

// runCaptured runs fn with stdout/stderr captured and returns its exit
// code alongside the combined output. captureBoth in the D-005 test
// discards the return value and asserts on it inside the closure; this
// returns it, because most of these tests read the JSON envelope
// afterwards and want to assert on the exit code from the same place.
func runCaptured(t *testing.T, fn func() int) (int, string) {
	t.Helper()
	var rc int
	out := captureBoth(t, func() { rc = fn() })
	return rc, out
}

// compactActionParamsSchema returns the raw JSON-Schema of the
// mpm_system compact action's params block, straight from the
// registry. Reading the registry rather than a copy of the schema is
// the point: a hand-copied schema would keep passing after the real
// one drifted.
func compactActionParamsSchema(t *testing.T) string {
	t.Helper()
	for _, tl := range tools.Registry {
		if tl.Name != "mpm_system" {
			continue
		}
		var doc struct {
			Properties struct {
				Action struct {
					Enum []string `json:"enum"`
				} `json:"action"`
			} `json:"properties"`
			OneOf []struct {
				Properties struct {
					Action struct {
						Const string `json:"const"`
					} `json:"action"`
					Params json.RawMessage `json:"params"`
				} `json:"properties"`
			} `json:"oneOf"`
		}
		if err := json.Unmarshal(tl.Schema, &doc); err != nil {
			t.Fatalf("parse mpm_system schema: %v", err)
		}
		for _, branch := range doc.OneOf {
			if branch.Properties.Action.Const == "compact" {
				return string(branch.Properties.Params)
			}
		}
		t.Fatal("mpm_system has no compact branch in its oneOf")
	}
	t.Fatal("mpm_system is not in the tool registry")
	return ""
}

// seedDeferredMemories inserts n raw memories already carrying a
// deferral annotation, oldest first.
func seedDeferredMemories(t *testing.T, dm *internal.DatabaseManager, n int) []string {
	t.Helper()
	var ids []string
	for i := 0; i < n; i++ {
		id := "cli-deferred-" + string(rune('a'+i))
		created := "2026-09-30T12:00:00Z"
		meta := `{"compaction_deferred_at":"2026-09-30T21:14:02Z",` +
			`"compaction_deferred_reason":"refusal_sentinel",` +
			`"compaction_deferred_batch":"cdb-1-001",` +
			`"compaction_deferred_sample":"declined"}`
		if _, err := dm.SQLDB().Exec(`
			INSERT INTO memories (id, collection, content, created_at, updated_at, metadata)
			VALUES (?, 'memories', ?, ?, ?, ?)`,
			id, "content of "+id, created, created, meta); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
		ids = append(ids, id)
	}
	return ids
}

func readDeferralKeys(t *testing.T, dm *internal.DatabaseManager, id string) map[string]any {
	t.Helper()
	var raw string
	if err := dm.SQLDB().QueryRow(`SELECT COALESCE(metadata,'{}') FROM memories WHERE id = ?`, id).Scan(&raw); err != nil {
		t.Fatalf("read metadata %s: %v", id, err)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("unmarshal metadata %s: %v", id, err)
	}
	return m
}

// ── mpm compact requeue-deferred ─────────────────────────────────────

func TestCompactRequeueDeferred_ReturnsRowsToThePool(t *testing.T) {
	dm := internal.NewTestDM(t)
	installTestDM(t, dm)
	withTempWorkspace(t)
	ids := seedDeferredMemories(t, dm, 3)

	code, out := runCaptured(t, func() int { return handleCompactRequeueDeferred([]string{"--json"}) })
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (%s)", code, out)
	}

	var res struct {
		Success           bool     `json:"success"`
		Requeued          int      `json:"requeued"`
		RowIDs            []string `json:"row_ids"`
		RemainingDeferred int      `json:"remaining_deferred"`
		AuditID           string   `json:"audit_id"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	if !res.Success {
		t.Error("success = false")
	}
	if res.Requeued != 3 {
		t.Errorf("requeued = %d, want 3", res.Requeued)
	}
	if len(res.RowIDs) != 3 {
		t.Errorf("row_ids = %v, want 3 entries", res.RowIDs)
	}
	if res.RemainingDeferred != 0 {
		t.Errorf("remaining_deferred = %d, want 0", res.RemainingDeferred)
	}
	if res.AuditID == "" {
		t.Error("audit_id is empty — the operator action left no forensic trail (R5)")
	}
	// The deferral keys are gone from the database, not just absent
	// from the response.
	for _, id := range ids {
		m := readDeferralKeys(t, dm, id)
		for _, key := range []string{"compaction_deferred_at", "compaction_deferred_reason", "compaction_deferred_batch", "compaction_deferred_sample"} {
			if _, ok := m[key]; ok {
				t.Errorf("%s: %s survived the requeue", id, key)
			}
		}
	}
}

// R2 at the CLI boundary: a limit is honoured, and there is no value
// of the flag that means "everything".
func TestCompactRequeueDeferred_HonoursTheLimit(t *testing.T) {
	dm := internal.NewTestDM(t)
	installTestDM(t, dm)
	withTempWorkspace(t)
	seedDeferredMemories(t, dm, 6)

	code, out := runCaptured(t, func() int { return handleCompactRequeueDeferred([]string{"2", "--json"}) })
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (%s)", code, out)
	}
	var res struct {
		Requeued int `json:"requeued"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	if res.Requeued != 2 {
		t.Errorf("requeued = %d, want 2", res.Requeued)
	}
	// The rest are still deferred — the command did not flush.
	counts, err := dm.PressureCounts(t.Context())
	if err != nil {
		t.Fatalf("pressure: %v", err)
	}
	if counts.DeferredCount != 4 {
		t.Errorf("deferred_count = %d, want 4", counts.DeferredCount)
	}
}

// --limit N and a bare positional N are the same thing.
func TestCompactRequeueDeferred_LimitFlagAndPositionalAgree(t *testing.T) {
	for _, args := range [][]string{{"--limit", "1", "--json"}, {"--limit=1", "--json"}, {"1", "--json"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			dm := internal.NewTestDM(t)
			installTestDM(t, dm)
			withTempWorkspace(t)
			seedDeferredMemories(t, dm, 3)

			code, out := runCaptured(t, func() int { return handleCompactRequeueDeferred(args) })
			if code != 0 {
				t.Fatalf("exit = %d, want 0 (%s)", code, out)
			}
			var res struct {
				Requeued int `json:"requeued"`
				Limit    int `json:"limit"`
			}
			if err := json.Unmarshal([]byte(out), &res); err != nil {
				t.Fatalf("output is not JSON: %v\n%s", err, out)
			}
			if res.Requeued != 1 || res.Limit != 1 {
				t.Errorf("requeued = %d limit = %d, want 1/1", res.Requeued, res.Limit)
			}
		})
	}
}

// An empty backlog is a clean no-op, not an error. An operator
// scripting a requeue should not have to special-case "already done".
func TestCompactRequeueDeferred_EmptyBacklogSucceeds(t *testing.T) {
	dm := internal.NewTestDM(t)
	installTestDM(t, dm)
	withTempWorkspace(t)

	code, out := runCaptured(t, func() int { return handleCompactRequeueDeferred([]string{"--json"}) })
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (%s)", code, out)
	}
	var res struct {
		Success  bool   `json:"success"`
		Requeued int    `json:"requeued"`
		AuditID  string `json:"audit_id"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	if !res.Success || res.Requeued != 0 {
		t.Errorf("success = %v requeued = %d, want true/0", res.Success, res.Requeued)
	}
	if res.AuditID != "" {
		t.Error("a no-op requeue wrote an audit row — the trail records decisions, not non-events")
	}
}

// ── argument handling ────────────────────────────────────────────────

// A non-numeric limit is rejected and writes nothing. Silently
// treating "abc" as "the default" would requeue 50 rows when the
// operator asked for something they mistyped.
func TestCompactRequeueDeferred_RejectsNonNumericLimit(t *testing.T) {
	dm := internal.NewTestDM(t)
	installTestDM(t, dm)
	withTempWorkspace(t)
	seedDeferredMemories(t, dm, 3)

	code, out := runCaptured(t, func() int { return handleCompactRequeueDeferred([]string{"--limit", "abc"}) })
	if code == 0 {
		t.Errorf("exit = 0, want non-zero for a non-numeric limit (%s)", out)
	}
	counts, err := dm.PressureCounts(t.Context())
	if err != nil {
		t.Fatalf("pressure: %v", err)
	}
	if counts.DeferredCount != 3 {
		t.Errorf("deferred_count = %d, want 3 — a rejected argument must not requeue", counts.DeferredCount)
	}
}

// An unknown flag is rejected (D-005), not ignored.
func TestCompactRequeueDeferred_RejectsUnknownFlag(t *testing.T) {
	dm := internal.NewTestDM(t)
	installTestDM(t, dm)
	withTempWorkspace(t)
	seedDeferredMemories(t, dm, 2)

	code, out := runCaptured(t, func() int { return handleCompactRequeueDeferred([]string{"--force"}) })
	if code == 0 {
		t.Errorf("exit = 0, want non-zero for an unknown flag (%s)", out)
	}
	counts, _ := dm.PressureCounts(t.Context())
	if counts.DeferredCount != 2 {
		t.Errorf("deferred_count = %d, want 2", counts.DeferredCount)
	}
}

// --help is inert: it prints usage and writes nothing. This is the
// property help_parity_test.go sweeps across every registered command,
// and it is worth pinning for this one specifically — it is a mutating
// command, and a --help that requeued 50 rows would be catastrophic.
func TestCompactRequeueDeferred_HelpIsInert(t *testing.T) {
	for _, flag := range []string{"--help", "-h", "help"} {
		t.Run(flag, func(t *testing.T) {
			dm := internal.NewTestDM(t)
			installTestDM(t, dm)
			withTempWorkspace(t)
			seedDeferredMemories(t, dm, 2)

			code, out := runCaptured(t, func() int { return handleCompactRequeueDeferred([]string{flag}) })
			if code != 0 {
				t.Errorf("exit = %d, want 0", code)
			}
			if !strings.Contains(out, "requeue-deferred") {
				t.Errorf("help output does not mention the command:\n%s", out)
			}
			counts, _ := dm.PressureCounts(t.Context())
			if counts.DeferredCount != 2 {
				t.Errorf("deferred_count = %d, want 2 — --help requeued rows", counts.DeferredCount)
			}
		})
	}
}

// ── mpm compact deferred ─────────────────────────────────────────────

func TestCompactDeferred_ListsRefusals(t *testing.T) {
	dm := internal.NewTestDM(t)
	installTestDM(t, dm)
	withTempWorkspace(t)
	seedDeferredMemories(t, dm, 2)

	code, out := runCaptured(t, func() int { return handleCompactDeferred([]string{"--json"}) })
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (%s)", code, out)
	}
	var res struct {
		Success  bool                   `json:"success"`
		Count    int                    `json:"count"`
		Deferred []internal.DeferredRow `json:"deferred"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	if !res.Success || res.Count != 2 || len(res.Deferred) != 2 {
		t.Fatalf("success = %v count = %d rows = %d, want true/2/2", res.Success, res.Count, len(res.Deferred))
	}
	// R5: the reason must be visible. An operator who cannot see WHY
	// a batch was declined cannot decide whether to requeue it.
	if res.Deferred[0].Reason != "refusal_sentinel" {
		t.Errorf("reason = %q, want refusal_sentinel", res.Deferred[0].Reason)
	}
	if res.Deferred[0].Batch == "" {
		t.Error("batch id is empty")
	}
	// Listing must be read-only.
	counts, _ := dm.PressureCounts(t.Context())
	if counts.DeferredCount != 2 {
		t.Errorf("deferred_count = %d, want 2 — listing mutated state", counts.DeferredCount)
	}
}

// An empty backlog marshals to [] rather than null, so a scripting
// operator can iterate without a nil check no other list requires.
func TestCompactDeferred_EmptyListIsAnEmptyArray(t *testing.T) {
	dm := internal.NewTestDM(t)
	installTestDM(t, dm)
	withTempWorkspace(t)

	code, out := runCaptured(t, func() int { return handleCompactDeferred([]string{"--json"}) })
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(out, `"deferred":[]`) {
		t.Errorf("expected an empty array, got:\n%s", out)
	}
}

// ── the noun group ───────────────────────────────────────────────────

// An unknown subcommand exits non-zero (D-005) rather than printing
// help and reporting success, which is what handleCapability's
// default branch does.
func TestCompact_UnknownSubcommandExitsNonZero(t *testing.T) {
	code, out := runCaptured(t, func() int { return handleCompact([]string{"compact", "requeue-everything"}) })
	if code == 0 {
		t.Errorf("exit = 0, want non-zero for a typo'd subcommand (%s)", out)
	}
}

func TestCompact_HelpMentionsBothSubcommands(t *testing.T) {
	code, out := runCaptured(t, func() int { return handleCompact([]string{"compact", "help"}) })
	if code != 0 {
		t.Errorf("exit = %d, want 0", code)
	}
	for _, want := range []string{"deferred", "requeue-deferred"} {
		if !strings.Contains(out, want) {
			t.Errorf("help output does not mention %q:\n%s", want, out)
		}
	}
}

// ── R1: the surface itself ───────────────────────────────────────────

// The load-bearing negative test for the whole design. Requeue must
// not be reachable from the agent-facing compact action — not as a
// parameter, not under an alias, not through a differently-cased key.
// If this ever passes with a reachable path, an agent can undo a
// model refusal on its own judgement and the deferral loop is back.
func TestRequeue_IsNotReachableFromTheMCPAction(t *testing.T) {
	for _, key := range []string{
		"requeue_deferred", "requeueDeferred", "requeue",
		"force_requeue", "requeue_limit", "clear_deferred",
	} {
		schema := compactActionParamsSchema(t)
		if strings.Contains(schema, `"`+key+`"`) {
			t.Errorf("the mpm_system compact params schema declares %q — requeue must be operator-only (R1)", key)
		}
	}
}
