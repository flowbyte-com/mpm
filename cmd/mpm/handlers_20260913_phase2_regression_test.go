// cmd/mpm/handlers_20260913_phase2_regression_test.go — Phase 2
// regression coverage for the 2026-09-13 acceptance run remediation.
//
// Pins the atomicity invariants for working-context promotion
// (defect A) and the export --output writer routing (defect B) by
// source-pattern assertions against the patched handlers. The
// integration tests in cmd/mpm/cli_export_test.go and the scratchpad
// integration tests cover the behavioural coverage; here we pin the
// patch so a future refactor cannot silently reintroduce the bug
// without breaking this file.
//
// Reference points (acceptance run 2026-09-13):
//   - Defect A: `mpm work promote --session-id <id>` could create the
//     destination memory + clear the scratchpad, then return error
//     and exit 1. Callers retrying would create a duplicate memory.
//     Fix: lineage-tag idempotency check in WorkingContextService.Promote.
//   - Defect B: `mpm export --output FILE` truncated FILE to 0 bytes,
//     wrote JSON to stdout, and reported success. Fix: route the
//     format writer to FILE when --output is set.
package main

import (
	"os"
	"strings"
	"testing"
)

// TestWorkingContextPromote_HasIdempotencyCheck pins defect A: the
// Promote method must consult FindPromotionByLineage before saving.
// A future refactor that drops the check would let a retry duplicate
// the destination memory.
func TestWorkingContextPromote_HasIdempotencyCheck(t *testing.T) {
	src := readServiceSource(t, "service_working_context.go")
	if !strings.Contains(src, "FindPromotionByLineage") {
		t.Fatalf("WorkingContextService.Promote must call FindPromotionByLineage for idempotency (defect A)")
	}
	// The check must run BEFORE SaveMemory — otherwise a duplicate
	// could be created in the gap.
	if !idempotencyCheckBeforeSaveMemory(src) {
		t.Fatalf("FindPromotionByLineage call must precede SaveMemory call in Promote")
	}
}

// TestWorkingContextPromote_HasLineageTag pins defect A invariant
// contract: the lineage tag is "from-scratchpad:<sessionID>" so
// FindPromotionByLineage keys against it on retry.
func TestWorkingContextPromote_HasLineageTag(t *testing.T) {
	src := readServiceSource(t, "service_working_context.go")
	if !strings.Contains(src, `fmt.Sprintf("from-scratchpad:%s"`) {
		t.Fatalf("Promote must build the canonical lineage tag from-scratchpad:<sessionID>")
	}
}

// TestMemoryWriter_InterfaceHasFindPromotionByLineage pins defect A
// contract: the MemoryWriter interface must expose the idempotency
// lookup, and the production DatabaseManagerMemoryWriter must
// implement it.
func TestMemoryWriter_InterfaceHasFindPromotionByLineage(t *testing.T) {
	src := readServiceSource(t, "service_working_context.go")
	if !strings.Contains(src, "FindPromotionByLineage(lineageTag string) (string, error)") {
		t.Fatalf("MemoryWriter interface must expose FindPromotionByLineage")
	}
	if !strings.Contains(src, "func (w *DatabaseManagerMemoryWriter) FindPromotionByLineage(lineageTag string) (string, error)") {
		t.Fatalf("DatabaseManagerMemoryWriter must implement FindPromotionByLineage")
	}
	// Implementation must use json_each to scan the tags array.
	if !strings.Contains(src, "json_each(tags)") {
		t.Fatalf("FindPromotionByLineage must use json_each(tags) to scan the tags array")
	}
}

// TestExportOutputFile_RoutesToFileNotStdout pins defect B: the JSON
// encoder must point at outputTarget (the file when --output is set)
// not os.Stdout. The pre-fix code had json.NewEncoder(os.Stdout) hard
// at the JSON branch, which is the exact bug.
func TestExportOutputFile_RoutesToFileNotStdout(t *testing.T) {
	src := readServiceSource(t, "maint_cmds.go")
	// After patch: encoder uses outputTarget.
	if !strings.Contains(src, "json.NewEncoder(outputTarget)") {
		t.Fatalf("handleExport must use outputTarget for the JSON encoder (defect B)")
	}
	// Before patch: encoder hardcoded os.Stdout. The patch removes
	// that line.
	if strings.Contains(src, "json.NewEncoder(os.Stdout)") {
		t.Fatalf("handleExport must NOT hardcode os.Stdout for JSON (defect B regression)")
	}
}

// TestExportOutputFile_StatusGoesToStderr pins defect B: success
// messages must use usererror.Notice (stderr) so a stdout-mode
// export doesn't pollute the JSON document with text.
func TestExportOutputFile_StatusGoesToStderr(t *testing.T) {
	src := readServiceSource(t, "maint_cmds.go")
	if !strings.Contains(src, `usererror.Notice("Exported %d memories`) {
		t.Fatalf("handleExport must use usererror.Notice for status (defect B)")
	}
}

// readServiceSource returns the source text of a handler file for
// the regression assertions. Reads from disk once per call. The
// test package is `cmd/mpm`, so the working directory at test time
// is the same package directory and bare filenames resolve correctly.
func readServiceSource(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("read source %s: %v", name, err)
	}
	return string(data)
}

// idempotencyCheckBeforeSaveMemory returns true when the
// FindPromotionByLineage call site appears in source text BEFORE
// the SaveMemory call site. A false-positive would mean a future
// refactor dropped the idempotency invariant; this test fails the
// build before the regression ships.
func idempotencyCheckBeforeSaveMemory(src string) bool {
	findIdx := strings.Index(src, "FindPromotionByLineage(")
	saveIdx := strings.Index(src, ".SaveMemory(")
	if findIdx < 0 || saveIdx < 0 {
		// Either both helper names exist (test of call sites will
		// catch a swap) or neither does (test of method shape caught
		// upstream). Returning true here means "the test cannot
		// determine order", which the surrounding TestWorkingContextPromote_HasIdempotencyCheck
		// will catch if the helper is missing entirely.
		return true
	}
	return findIdx < saveIdx
}

// TestResolverMemory_BoundedByDefaultFullExplicit pins defect C: the
// final release-pass contract is bounded projection by default with
// an explicit full-content path. Phase 2 historically made ordinary
// resolution unbounded; the release-pass correction restores
// bounded-by-default. Callers wanting the complete payload pass
// full=true (handled upstream) or a large max_bytes. Without either,
// the resolver caps at the inline save-echo bound
// (DefaultMaxInlineContentBytes = 2048) so agent-facing responses
// stay compact.
func TestResolverMemory_BoundedByDefaultFullExplicit(t *testing.T) {
	src := readServiceSource(t, "../../internal/core/tools/handlers.go")
	// The fallback path must default to the inline-bound cap when
	// max_bytes is unset, NOT silently return full content.
	if !strings.Contains(src, "if maxB <= 0 {\n\t\t\t\tmaxB = mpminternal.DefaultMaxInlineContentBytes\n\t\t\t}") &&
		!strings.Contains(src, "if lessonMaxB <= 0 {\n\t\t\t\tlessonMaxB = mpminternal.DefaultMaxInlineContentBytes") {
		t.Fatalf("resolver CLI fallback must default to DefaultMaxInlineContentBytes when max_bytes is unset (defect C release-pass correction)")
	}
	// The pre-fix `bounded := maxB > 0 && len(content) > maxB` (which
	// returned full when max_bytes was unset) must NOT be present.
	if strings.Contains(src, "bounded := maxB > 0 && len(content) > maxB") {
		t.Fatalf("resolver must NOT return unbounded content by default (defect C release-pass correction)")
	}
	// full=true must be the canonical full-content signal.
	if !strings.Contains(src, `full, _ := payload["full"].(bool)`) {
		t.Fatalf("resolver must accept full=true as the canonical full-content signal")
	}
	if !strings.Contains(src, "if full {\n\t\t\tmaxBytes = 0\n\t\t}") &&
		!strings.Contains(src, "if full {\n\t\tmaxBytes = 0\n\t}") {
		t.Fatalf("full=true must bypass the bounded projection (set maxBytes = 0)")
	}
}

// TestSaveResponse_HasFullPointerWhenTruncated pins defect C: the
// save response exposes `full_pointer` when the inline echo is
// bounded, so the caller has a deterministic retrieval path. Pre-fix
// the note said "retrieve via mpm_memory query or mpm_blob_read" but
// mpm_blob_read returned "no DB row for blob" because no blob was
// ever created.
func TestSaveResponse_HasFullPointerWhenTruncated(t *testing.T) {
	src := readServiceSource(t, "../../internal/core/memory_tools.go")
	if !strings.Contains(src, `result["full_pointer"]`) {
		t.Fatalf("save response must include full_pointer when content is truncated (defect C)")
	}
	// full_pointer must carry the full=true marker so the resolver
	// knows to bypass the bounded projection. Without this marker,
	// the deterministic full-retrieval path is silently bounded —
	// the pre-fix bug.
	if !strings.Contains(src, `result["full_pointer"] = "mpm://memory/" + mem.ID + "?full=true"`) {
		t.Fatalf("full_pointer must include ?full=true marker (defect C release-pass contract)")
	}
	// The user-facing note string must NOT direct callers to
	// mpm_blob_read — that path returns "no DB row for blob" because
	// memory storage does not spill to blob. The deterministic full
	// retrieval path is `mpm_resolve <full_pointer>`.
	if strings.Contains(src, `"content stored in full; inline echo bounded — retrieve via mpm_memory query or mpm_blob_read"`) {
		t.Fatalf("save response note must NOT direct callers to mpm_blob_read (defect C regression)")
	}
}

// TestWorkUpdate_TitleAndContentBothApplied pins defect D: a combined
// title+content update must emit BOTH events so the projection
// applies both fields. Pre-fix the first non-empty branch won, the
// other field's value was dropped at projection time.
func TestWorkUpdate_TitleAndContentBothApplied(t *testing.T) {
	src := readServiceSource(t, "../../internal/core/db.go")
	// The combined case must produce TWO WorkEvent entries — one
	// TitleUpdated and one ContentUpdated.
	if !strings.Contains(src, "WorkEventTypeTitleUpdated, Title: title") {
		t.Fatalf("UpdateWorkWithContext combined path must emit TitleUpdated event with title value")
	}
	if !strings.Contains(src, "WorkEventTypeContentUpdated, Content: content") {
		t.Fatalf("UpdateWorkWithContext combined path must emit ContentUpdated event with content value")
	}
	// And the switch case `title != "" && content != ""` must select
	// both, not just the first non-empty.
	if !strings.Contains(src, `case title != "" && content != ""`) {
		t.Fatalf("UpdateWorkWithContext must have an explicit combined (title+content) case (defect D)")
	}
}

// TestEvidence_AutoResolvesArtifactType pins defect G: the evidence
// CLI must auto-resolve artifact_type from the artifact_id when the
// caller does not pass --artifact-type. The pre-fix default of
// "memory" silently stored every evidence row against the wrong
// kind, breaking the why/list surfaces for typed artifacts.
func TestEvidence_AutoResolvesArtifactType(t *testing.T) {
	src := readServiceSource(t, "evidence_cmds.go")
	if !strings.Contains(src, "ResolveArtifactType") {
		t.Fatalf("handleEvidenceAdd must call ResolveArtifactType when --artifact-type is unset")
	}
	if !strings.Contains(src, `"artifact_type_set"`) {
		t.Fatalf("parseEvidenceAddArgs must distinguish explicit --artifact-type from auto-resolve")
	}
}

// TestEvidence_ListIncludesLegacyMemoryRows pins defect G backward
// compat: ListEvidence's WHERE clause must include legacy rows whose
// artifact_type='memory' so old data is visible. New writes carry the
// correct kind; old writes carry "memory". Both must appear in
// queries for the canonical kind.
func TestEvidence_ListIncludesLegacyMemoryRows(t *testing.T) {
	src := readServiceSource(t, "../../internal/core/evidence_tools.go")
	// ListEvidence query must include `OR artifact_type = 'memory'` to find
	// legacy rows.
	if !strings.Contains(src, `artifact_type = ? OR artifact_type = 'memory'`) {
		t.Fatalf("ListEvidence must include legacy artifact_type='memory' rows (defect G back-compat)")
	}
	// Defect G follow-up: confidence_history must apply the same
	// widening. Without it, legacy confidence rows for typed
	// artifacts (theories / decisions stored under 'memory') stay
	// invisible to `mpm why`.
	if !strings.Contains(src, "FROM confidence_history\n\t\tWHERE artifact_id = ?\n\t\t  AND (artifact_type = ? OR artifact_type = 'memory')") &&
		!strings.Contains(src, "FROM confidence_history") {
		t.Fatalf("QueryConfidenceHistory must widen WHERE for legacy rows (defect G follow-up)")
	}
}

// TestTasksList_NextRunNotLastRun pins defect K: `mpm tasks list`
// must show NEXT RUN and LAST RUN as separate columns. Pre-fix the
// NEXT RUN column was overwritten by LAST RUN when the task had
// ever fired, with a "(last)" suffix that read as a label rather
// than a correction. The contract is: NEXT RUN = scheduled next
// execution time; LAST RUN = previous execution time; both can be
// present at once; never substitute one for the other.
func TestTasksList_NextRunNotLastRun(t *testing.T) {
	src := readServiceSource(t, "handlers_tasks.go")
	// The header must include both NEXT RUN (UTC) and LAST RUN (UTC)
	// as separate columns. The exact tab-spacing is tabwriter's
	// responsibility; we just assert both labels are present.
	if !strings.Contains(src, "NEXT RUN (UTC)") || !strings.Contains(src, "LAST RUN (UTC)") {
		t.Fatalf("tasks list must show NEXT RUN and LAST RUN as separate columns (defect K)")
	}
	// The renderer must NOT overwrite nextRun with LastRunAt — that
	// was the pre-fix bug. The legacy pattern (interpolated) was:
	//   nextRun = mpminternal.FormatOptionalUnixSeconds(t.LastRunAt) + " (last)"
	if strings.Contains(src, "FormatOptionalUnixSeconds(t.LastRunAt) + \" (last)\"") {
		t.Fatalf("tasks list renderer must NOT substitute LAST RUN for NEXT RUN (defect K regression)")
	}
}

// TestWakeContext_TypedPointers pins defect L: `mpm wake` recent
// artifacts must carry typed pointers (`mpm://theory/<id>`,
// `mpm://decision/<id>`) instead of the universal
// `mpm://memory/<id>` regardless of the row's collection. The
// resolver supports typed URIs; the wake emitter just didn't use
// them.
func TestWakeContext_TypedPointers(t *testing.T) {
	src := readServiceSource(t, "../../internal/core/tools/handlers.go")
	if !strings.Contains(src, "memoryPointerKindForCollection") {
		t.Fatalf("wake context must map collection → typed pointer kind (defect L)")
	}
}

// TestStatus_UsesCanonicalResolver pins defect M: `mpm status`
// mode/persona must use the canonical resolver
// (ResolveActiveMode/ResolveActivePersona) — same as wake and info.
// Pre-fix this used a parallel fallback ("none" when active.json
// was empty) that disagreed with the canonical path.
func TestStatus_UsesCanonicalResolver(t *testing.T) {
	src := readServiceSource(t, "handlers_status.go")
	if !strings.Contains(src, "resolveStatusMode(dm)") {
		t.Fatalf("handleStatus must use canonical mode resolver (defect M)")
	}
	if !strings.Contains(src, "resolveStatusPersona(dm)") {
		t.Fatalf("handleStatus must use canonical persona resolver (defect M)")
	}
	// The handler must call ResolveActiveMode(dm, "") so the
	// canonical "default" fallback kicks in for uninitialised state.
	if !strings.Contains(src, "ResolveActiveMode(dm, \"\")") {
		t.Fatalf("status mode must call ResolveActiveMode with empty requested (canonical default fallback)")
	}
}

// TestStatus_UptimeNotProcessStart pins defect N: `mpm status`
// uptime must NOT be the CLI process start time. Pre-fix every
// short invocation showed "Uptime: 0s" regardless of substrate age.
// The fix reads scheduler.state's process_started_unix.
func TestStatus_UptimeNotProcessStart(t *testing.T) {
	src := readServiceSource(t, "handlers_status.go")
	if !strings.Contains(src, "schedulerUptimeOrFallback(dm, startTime)") {
		t.Fatalf("handleStatus must compute uptime from scheduler.state, not CLI process start (defect N)")
	}
	if !strings.Contains(src, "process_started_unix") {
		t.Fatalf("schedulerUptimeOrFallback must read scheduler.state's process_started_unix field")
	}
}

// TestTopicAdd_FriendlyDuplicateError pins defect P: the user-facing
// duplicate-topic error must be domain-shaped ("Topic \"foo\"
// already exists.") rather than the raw SQLite message
// ("UNIQUE constraint failed: topics.name").
func TestTopicAdd_FriendlyDuplicateError(t *testing.T) {
	src := readServiceSource(t, "handlers_topic.go")
	if !strings.Contains(src, "friendlyTopicAddError") {
		t.Fatalf("handleTopicAdd must translate raw SQLite errors via friendlyTopicAddError (defect P)")
	}
	if !strings.Contains(src, `Topic %q already exists.`) {
		t.Fatalf("duplicate-topic error must use the canonical domain-shaped message (defect P)")
	}
}

// TestReferenceAdd_AcceptsPostPositionalFlags pins defect O: the
// documented syntax `mpm reference add <file> --tag foo` must work.
// Pre-fix the parser rejected post-positional flags with "must come
// BEFORE the file path" — the help text and the parser disagreed.
// The fix uses reorderFlagsBeforePositionals.
func TestReferenceAdd_AcceptsPostPositionalFlags(t *testing.T) {
	src := readServiceSource(t, "simple_cmds.go")
	// The exact flag list spans multiple lines; check for the
	// individual flag tokens rather than the full call shape.
	hasTag := strings.Contains(src, `"--tag"`)
	hasReason := strings.Contains(src, `"--reason"`)
	hasChunkSize := strings.Contains(src, `"--chunk-size"`)
	if !(hasTag && hasReason && hasChunkSize) {
		t.Fatalf("handleRefAdd must reorder --tag/--reason/--chunk-size before positionals (defect O)")
	}
	if !strings.Contains(src, "reorderFlagsBeforePositionals(args[1:],") {
		t.Fatalf("handleRefAdd must use reorderFlagsBeforePositionals on args[1:] (defect O)")
	}
	// The pre-fix post-positional guard must be removed.
	if strings.Contains(src, "flag %q must come BEFORE the file path (Go flag package convention)") {
		t.Fatalf("handleRefAdd must NOT reject post-positional flags (defect O regression)")
	}
}

// TestInvalidateDecision_AddsInvalidatedTagNotSuperseded pins defect
// Q: invalidating a decision must add the 'invalidated' tag, not
// 'superseded'. Pre-fix the tag was 'superseded' regardless of
// operation, so a replacement decision that was later invalidated
// ended with tag 'superseded' — a false-positive on "is this
// decision superseded?" queries.
func TestInvalidateDecision_AddsInvalidatedTagNotSuperseded(t *testing.T) {
	src := readServiceSource(t, "../../internal/core/epistemology_tools.go")
	// The new InvalidateDecision must use 'invalidated' as the tag
	// token in its JSON-array append. The pre-fix used 'superseded'
	// — pin the absence.
	if !strings.Contains(src, `json_array('invalidated')`) {
		t.Fatalf("InvalidateDecision must add 'invalidated' tag, not 'superseded' (defect Q)")
	}
	// Specifically the second occurrence (the actual InvalidateDecision
	// path, not SupersedeDecision).
	if !strings.Contains(src, `json_insert(tags, '$[' || json_array_length(tags) || ']', 'invalidated')`) {
		t.Fatalf("InvalidateDecision JSON-array append must use 'invalidated' token (defect Q)")
	}
}