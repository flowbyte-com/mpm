// memory_tools.go — DM methods backing memory read/write/mutation tools.
//
// Routes three surfaces: `mpm call <tool> --payload '{...}'`,
// `<tool>` MCP entry, and the original `mpm <command>` CLI handlers.
// Each method returns a wire-format map[string]interface{} so both
// `mpm call` and the MCP tool can json.Marshal it identically.
//
// Mutations (shred/snooze/set-weight/promote/reinforce/weaken/patch)
// are grouped together because they share the same authorization model
// (any authenticated agent) and the same response shape.
package internal

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/flowbyte-com/mpm-core/synth"
)

// SaveMemoryWithContext persists a fact to memory, injecting provenance
// and active context into metadata. weight accepts BOTH the legacy 0.0-1.0
// float scale AND the 0-100 integer scale — see normalizeWeightToColumn
// in memory.go for the auto-detection rule. The caller's raw value is also
// recorded as meta.weight_intent for forensic tracing (what scale was the
// caller thinking in?).
//
// Theory-resolve hook (2026-07-23, supersedes the 2026-07-22 phantom):
// after the memory insert lands, detect `theory:<id>` + `outcome:<proven|
// disproven>` tags (or `Theory <id> resolved PROVEN|DISPROVEN` content
// regex) and apply the corresponding UPDATE to the theories row. The
// hook is best-effort: failures are logged + skipped, the memory insert
// is never rolled back. Atomicity is sacrificed for simplicity — the
// previous session's "WithTx + SaveMemoryNode" design was never wired in
// (the hook file existed but SaveMemoryWithContext did not call it). The
// 2026-07-23 wiring is non-atomic; production-grade atomicity is a future
// refactor that requires moving SaveMemoryWithContext from AddMemoryWithWeight
// to a WithTx + SaveMemoryNode path. The hook returns the IDs of theories
// it actually resolved in result["theory_resolutions_applied"].
func (dm *DatabaseManager) SaveMemoryWithContext(
	fact, collection string,
	tags []string,
	weight float64,
	ttl string,
	ac ActiveContext,
) (map[string]interface{}, *Memory, error) {
	return dm.saveMemoryWithContextImpl(fact, collection, tags, weight, ttl, ac, nil)
}

// SaveMemoryWithContextAndSnapshot is the snapshot-enabled entry point.
// Same semantics as SaveMemoryWithContext but additionally injects an
// _epistemic_snapshot block into metadata before the INSERT, computed
// from the supplied WrapperContext (agent/session/model + recent tool
// observation + confidence/depth self-assessment).
//
// wc == nil is equivalent to calling SaveMemoryWithContext — the
// legacy path is fully preserved for callers that don't have a
// WrapperContext to supply.
//
// On snapshot resolution failure (resolver returns an error), the
// memory is still saved — the snapshot block is simply omitted and
// a warning is logged. Snapshot instrumentation is best-effort; it
// must never fail the user's save.
func (dm *DatabaseManager) SaveMemoryWithContextAndSnapshot(
	fact, collection string,
	tags []string,
	weight float64,
	ttl string,
	ac ActiveContext,
	wc *WrapperContext,
) (map[string]interface{}, *Memory, error) {
	return dm.saveMemoryWithContextImpl(fact, collection, tags, weight, ttl, ac, wc)
}

// saveMemoryWithContextImpl is the shared body. The wc parameter
// gates the snapshot injection; nil = legacy path, non-nil = snapshot
// path. Single source of truth for both entry points so the two
// never drift apart.
func (dm *DatabaseManager) saveMemoryWithContextImpl(
	fact, collection string,
	tags []string,
	weight float64,
	ttl string,
	ac ActiveContext,
	wc *WrapperContext,
) (map[string]interface{}, *Memory, error) {
	if collection == "" {
		collection = "memories"
	}
	if weight <= 0 {
		weight = 0.5
	}

	meta := ac.withActiveContextMeta(nil)

	// Snapshot injection (alpha, 2026-08-04). For brand-new memories
	// we pass artifactID="" so the resolver omits the validation block
	// (no evidence exists yet for a row that hasn't been written).
	// Single-statement INSERT is preserved — the snapshot is merged
	// into meta before the write, no post-write UPDATE needed.
	if wc != nil {
		snap, err := ResolveSnapshot(context.Background(), dm, *wc, "", "")
		if err != nil {
			slog.Warn("epistemic_snapshot resolution failed (saving without snapshot)",
				"session_id", wc.SessionID, "error", err.Error())
		} else {
			meta = snap.MergeInto(meta)
		}
	}

	store, err := dm.getSharedStore()
	if err != nil {
		return nil, nil, fmt.Errorf("get memory store: %w", err)
	}

	if weight > 0 {
		meta["weight_intent"] = int(weight * 10)
	}

	// Use AddMemoryWithWeight so the caller's weight actually reaches the
	// weight column instead of falling back to the DB default (or Go zero).
	mem, err := store.AddMemoryWithWeight(fact, collection, tags, meta, "", "call", weight)
	if err != nil {
		return nil, nil, fmt.Errorf("add memory: %w", err)
	}

	if ttl != "" {
		if dur, err := parseDurationString(ttl); err == nil && dur > 0 {
			dm.SetMemoryTTL(mem.ID, time.Now().Add(dur))
		}
	}

	// Theory-resolve hook: detect references in this payload and apply
	// resolutions. Best-effort: log + skip on failure, never roll back
	// the memory insert.
	applied, _ := dm.applyTheoryResolutions(fact, tags)

	result := map[string]interface{}{
		"success": true,
		"id":      mem.ID,
		"content": mem.Content,
		"weight":  mem.Weight,
		"tags":    mem.Tags,
	}
	if len(applied) > 0 {
		result["theory_resolutions_applied"] = applied
	}
	return result, mem, nil
}

// applyTheoryResolutions detects theory references in the payload and
// applies them. Detects via detectTheoryResolutions, pre-flights via
// lookupTheoryStatus (only attempt on pending theories), resolves via
// resolveTheoryOnNode. Returns the IDs of theories that were successfully
// resolved. Best-effort: any error path is logged and skipped.
func (dm *DatabaseManager) applyTheoryResolutions(content string, tags []string) ([]string, error) {
	resolutions := detectTheoryResolutions(content, tags)
	if len(resolutions) == 0 {
		return nil, nil
	}

	var applied []string
	for _, r := range resolutions {
		status, found, err := dm.lookupTheoryStatus(r.theoryID)
		if err != nil {
			slog.Warn("theory-resolve hook: pre-flight lookup failed",
				"theory_id", r.theoryID, "error", err.Error())
			continue
		}
		if !found {
			slog.Warn("theory-resolve hook: theory not found",
				"theory_id", r.theoryID)
			continue
		}
		if status != "pending" {
			slog.Debug("theory-resolve hook: theory already resolved, skipping",
				"theory_id", r.theoryID, "status", status)
			continue
		}

		// Map outcome to (conclusion, newStatus).
		var conclusion, newStatus string
		switch r.outcome {
		case "proven":
			conclusion = "confirmed"
			newStatus = "proven"
		case "disproven":
			conclusion = "rejected"
			newStatus = "disproven"
		default:
			slog.Warn("theory-resolve hook: unknown outcome",
				"theory_id", r.theoryID, "outcome", r.outcome)
			continue
		}

		if err := resolveTheoryOnNode(dm, r.theoryID, conclusion, newStatus); err != nil {
			slog.Warn("theory-resolve hook: resolve failed",
				"theory_id", r.theoryID, "error", err.Error())
			continue
		}
		applied = append(applied, r.theoryID)
	}
	return applied, nil
}

// HybridSearchMemories runs hybrid (BM25 + semantic) search with FTS fallback.
// Named to avoid collision with dm.SearchMemories in web_db.go.
//
// scope (Phase 2d, WISHLIST.md multi-agent shared epistemology):
//
//	"all"    (default) — federated local + shared. Shared rows get
//	                       the "Shared Premium" multiplier
//	                       (1.2x; rules collection 1.35x; cap 1.0).
//	                       Result count is capped at `limit` after
//	                       post-retrieval re-ranking across both
//	                       sides. Empty scope or unknown values
//	                       default to "all".
//	"local"  — same behaviour as pre-Phase-2d HybridSearch: hybrid
//	            search on local memories only, no Shared Premium.
//	"shared" — keyword-only QueryGlobalRules path. No vector search,
//	            no Shared Premium (within a single side there is no
//	            local to soften). is_global=1 rows only.
func (dm *DatabaseManager) HybridSearchMemories(query, collection string, limit int, scope string) ([]map[string]interface{}, error) {
	if limit <= 0 {
		limit = 5
	}
	if scope == "" {
		scope = "all"
	}

	switch scope {
	case "local":
		return dm.hybridSearchScopeLocal(query, collection, limit)
	case "shared":
		return dm.hybridSearchScopeShared(query, limit)
	case "all":
		return dm.hybridSearchScopeAll(query, collection, limit)
	default:
		return nil, fmt.Errorf("invalid scope %q (want all|local|shared)", scope)
	}
}

// hybridSearchScopeLocal: the pre-Phase-2d behaviour, isolated so the
// "all" path doesn't branch on local vs shared mid-merge.
func (dm *DatabaseManager) hybridSearchScopeLocal(query, collection string, limit int) ([]map[string]interface{}, error) {
	cfg := DefaultHybridConfig()
	cfg.Limit = limit
	mems, err := HybridSearch(dm, query, collection, cfg)
	if err != nil {
		fb, fbErr := dm.fallbackLocal(query, collection, limit, err)
		if fbErr != nil {
			return nil, fbErr
		}
		return hybridResultsToMaps(fb), nil
	}
	return hybridResultsToMaps(mems), nil
}

// hybridSearchScopeShared: keyword-only retrieval against shared.memories
// via QueryGlobalRules (existing FTS5 + LIKE fallback path).
func (dm *DatabaseManager) hybridSearchScopeShared(query string, limit int) ([]map[string]interface{}, error) {
	rules, err := dm.QueryGlobalRules(query, limit)
	if err != nil {
		return nil, fmt.Errorf("query shared.memories: %w", err)
	}
	items := make([]map[string]interface{}, 0, len(rules))
	for _, r := range rules {
		items = append(items, map[string]interface{}{
			"id":         r["id"],
			"content":    r["content"],
			"weight":     r["weight"],
			"collection": r["collection"],
			"origin":     "shared",
		})
	}
	return items, nil
}

// hybridSearchScopeAll: federated local + shared.
//
// Fetching strategy (the "Federated Fetch Buffer"):
//
//	1. Run HybridSearch against local with cfg.Limit=2*limit.
//	2. Run HybridSearch against shared.memories with cfg.Limit=2*limit.
//	   Same code path, different schema prefix.
//	3. Concat (up to 4*limit rows).
//	4. applySharedPremium: re-rank with the multiplier, slice to `limit`.
//	   The 2x fetch buffer ensures a shared row promoted by the boost
//	   doesn't get truncated at the cfg.Limit gate.
func (dm *DatabaseManager) hybridSearchScopeAll(query, collection string, limit int) ([]map[string]interface{}, error) {
	fetch := limit * 2
	if fetch < 10 {
		fetch = 10
	}

	localCfg := DefaultHybridConfig()
	localCfg.Limit = fetch
	localMems, errLocal := HybridSearch(dm, query, collection, localCfg)
	if errLocal != nil {
		localMems, errLocal = dm.fallbackLocal(query, collection, fetch, errLocal)
		_ = errLocal
	}

	sharedCfg := DefaultHybridConfig()
	sharedCfg.SchemaPrefix = "shared."
	sharedCfg.Origin = "shared"
	sharedCfg.Limit = fetch
	sharedMems, errShared := HybridSearch(dm, query, collection, sharedCfg)
	if errShared != nil {
		// Empty shared side is fine; return whatever local produced.
		sharedMems = nil
	}

	merged := make([]HybridResult, 0, len(localMems)+len(sharedMems))
	merged = append(merged, localMems...)
	merged = append(merged, sharedMems...)

	ranked := applySharedPremium(merged, limit)
	return hybridResultsToMaps(ranked), nil
}

// fallbackLocal: FTS-only path used when hybrid vector side can't be
// exercised (no embedding provider / transient cosine failure).
func (dm *DatabaseManager) fallbackLocal(query, collection string, limit int, prevErr error) ([]HybridResult, error) {
	store, storeErr := dm.getSharedStore()
	if storeErr != nil {
		return nil, fmt.Errorf("hybrid search: %w; fallback store: %v", prevErr, storeErr)
	}
	fallback, fallbackErr := store.FullTextSearch(query, collection, limit)
	if fallbackErr != nil {
		return nil, fmt.Errorf("hybrid search: %w; fulltext fallback: %v", prevErr, fallbackErr)
	}
	out := make([]HybridResult, 0, len(fallback))
	for _, m := range fallback {
		out = append(out, HybridResult{
			ID:         m.ID,
			Content:    m.Content,
			Collection: m.Collection,
			Tags:       strings.Join(m.Tags, ","),
			Weight:     float64(m.Weight),
			Origin:     "local",
		})
	}
	return out, nil
}

// applySharedPremium is the post-merge re-ranker (Phase 2d).
//
// Multiplier logic:
//
//	local rows      → final = combined      (raw relevance)
//	shared non-rule → final = min(combined * 1.20,  1.0)
//	shared rule     → final = min(combined * 1.35,  1.0)
//
// Rationale: a multiplicative boost scales with relevance, so an
// irrelevant shared row sinks (negative or zero scores stay at or
// below their raw ranking) while a relevant shared row edges out a
// similarly-relevant local row. The 1.0 cap prevents a near-perfect
// score from compounding unbounded. Stacking 1.20 (shared) and 1.35
// (rules) preserves the WISHLIST's two-tier hierarchy: house rules
// outrank shared decisions outrank local.
//
// final is stashed on HybridResult.CombinedScore so callers see what
// the ranker actually used. Raw scores remain on FTS5Score and
// VectorSimilarity for diagnostics.
//
// Sort: final-score desc, then weight desc, then collection asc
// (stable + predictable).
func applySharedPremium(merged []HybridResult, targetCount int) []HybridResult {
	type scored struct {
		r     HybridResult
		final float64
	}
	out := make([]scored, len(merged))
	for i, r := range merged {
		final := r.CombinedScore
		if r.Origin == "shared" {
			multiplier := 1.20
			if r.Collection == "rules" {
				multiplier = 1.35
			}
			final = r.CombinedScore * multiplier
			if final > 1.0 {
				final = 1.0
			}
		}
		r.CombinedScore = final
		out[i] = scored{r: r, final: final}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].final != out[j].final {
			return out[i].final > out[j].final
		}
		if out[i].r.Weight != out[j].r.Weight {
			return out[i].r.Weight > out[j].r.Weight
		}
		return out[i].r.Collection < out[j].r.Collection
	})
	if targetCount > 0 && len(out) > targetCount {
		out = out[:targetCount]
	}
	ranked := make([]HybridResult, len(out))
	for i, s := range out {
		ranked[i] = s.r
	}
	return ranked
}

// hybridResultsToMaps projects the HybridResult struct into the
// external map shape handleQueryLongTermMemory returns. Phase 2d
// adds origin + combined_score so agents and tests can distinguish
// local vs shared rows and verify the boosted score landed.
func hybridResultsToMaps(mems []HybridResult) []map[string]interface{} {
	items := make([]map[string]interface{}, 0, len(mems))
	for _, m := range mems {
		var banner string
		if m.IsConceptDrift {
			banner = conceptDriftWarning
		} else if m.IsChallenged {
			banner = challengeWarning
		}
		items = append(items, map[string]interface{}{
			"id":                   m.ID,
			"content":              m.Content,
			"weight":               m.Weight,
			"tags":                 m.Tags,
			"collection":           m.Collection,
			"origin":               m.Origin,
			"banner":               banner,
			"is_concept_drift":     m.IsConceptDrift,
			"is_challenged":        m.IsChallenged,
			"challenged_theory_id": m.ChallengedTheoryID,
			"combined_score":       m.CombinedScore,
		})
	}
	return items
}

// ── Memory feedback / mutation tools (Tier 1) ───────────────────────────────
//
// These close the agent feedback loop on memories. Without them, agents
// have to fall back to shelling `mpm reinforce <id>` etc., which forces
// them to invent CLI quoting and parse text output. Every tool below is
// (a) a DM method, (b) wired into the mpm call registry, (c) registered
// as an MCP tool so agents get typed arguments and JSON responses.

// ShredMemoryWithCascade hard-deletes a memory and any theory it
// challenged. Returns a result map with success flag, the shredded
// memory id, and (if applicable) the purged theory id so callers can
// chain a follow-up decision. Single transaction so a partial failure
// can't leave orphan topic_memberships rows.
//
// Distinct from the legacy `ShredMemory(id) error` (internal/web_db.go)
// which only deletes the memory row and leaves topic_memberships +
// challenged_theory_id orphaned. The CLI's handleShredMem has been doing
// the cascade manually since 2026-06-26; this method captures the same
// logic in the DM API so MCP/`mpm call` tools don't have to reinvent it.
//
// 2026-07-23: collection-aware routing. If the id is a lesson, route
// through the lessons view (which fires the INSTEAD OF DELETE trigger
// and removes from lessons_base + lessons_fts atomically) and return
// `lesson_id` in the result map. Otherwise fall through to the existing
// memory-cascade path. The 2026-07-22 session claimed this fix was
// shipped but the production code was missing — the lesson-handling
// tests (shred_lesson_aware_test.go, shred_cascade_test.go) were
// untracked and the cascade wrapper silently no-op'd on lesson IDs.
//
// 2026-08-04 (Task 4): cascade invalidation hook. The memory path
// enqueues cascade intents for every downstream decision/theory that
// cited the shredded memory (via explicit `dependencies` JSON or
// typed `epistemic_provenance` citations) inside the same tx as the
// root DELETE. The lesson path remains non-cascading: lessons have
// no reasoning dependents and the design spec excludes them from
// the cascade target surface. The number of enqueued intents is
// reported as `cascade_intents` in the result map (zero is a clean
// no-op when no downstream dependents exist).
//
// Idempotency: a shred of a non-existent id is a success no-op
// (preserves the legacy contract). The cascade intent write is
// also idempotent on the schema's UNIQUE key
// (dead, downstream, event), so a re-shred against the same id
// cannot duplicate intents. When the memory row is already gone
// the cascade discovery returns an empty target list (no
// dependents surface a dead id), so the enqueue is a clean zero.
func (dm *DatabaseManager) ShredMemoryWithCascade(memoryID string) (map[string]interface{}, error) {
	if memoryID == "" {
		return nil, fmt.Errorf("memory_id is required")
	}

	// Probe lessons_base first. If the id is a lesson, route through
	// the lessons view (lessons_cascade test) and return lesson_id.
	var lessonCount int
	if err := dm.db.QueryRow(`SELECT COUNT(*) FROM lessons_base WHERE id = ?`, memoryID).Scan(&lessonCount); err != nil {
		return nil, fmt.Errorf("shred: probe lessons_base: %w", err)
	}
	if lessonCount > 0 {
		if _, err := dm.db.Exec(`DELETE FROM lessons WHERE id = ?`, memoryID); err != nil {
			return nil, fmt.Errorf("shred: delete from lessons: %w", err)
		}
		return map[string]interface{}{
			"success":   true,
			"lesson_id": memoryID,
			"shredded":  true,
		}, nil
	}

	// Pull the challenged_theory_id out of metadata BEFORE deleting the
	// row, so we know which theory (if any) to cascade-purge.
	var theoryID string
	if mem, err := dm.GetMemory(memoryID); err == nil && mem != nil {
		if metaStr, ok := mem["metadata"].(string); ok && metaStr != "" {
			var meta map[string]interface{}
			if json.Unmarshal([]byte(metaStr), &meta) == nil {
				theoryID, _ = meta["challenged_theory_id"].(string)
			}
		}
	}

	// Atomicity boundary: topic_memberships cleanup + theory purge +
	// memory DELETE + cascade intent enqueue all live in one tx. A
	// failure in any step rolls back every preceding step so the
	// substrate never sees an orphan topic_memberships row, a half-
	// purged theory, or a memory that vanished without a cascade
	// intent.
	var cascadeIntents int
	tx, err := dm.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("shred: begin: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	if _, err := tx.Exec(`DELETE FROM topic_memberships WHERE memory_id = ?`, memoryID); err != nil {
		return nil, fmt.Errorf("shred: delete memberships: %w", err)
	}
	if theoryID != "" {
		if _, err := tx.Exec(`DELETE FROM memories WHERE id = ?`, theoryID); err != nil {
			return nil, fmt.Errorf("shred: delete theory: %w", err)
		}
	}

	// Cascade invalidation hook: enqueue intents for every downstream
	// decision/theory that cited the about-to-be-deleted memory.
	// Must happen BEFORE the memory DELETE so the
	// discoverCascadeTargets helper can still observe the
	// dependencies JSON + epistemic_provenance rows pointing at this
	// id. The schema's UNIQUE key makes a re-shred a clean no-op even
	// when intents already exist from a prior (now-rolled-back)
	// attempt.
	if n, err := dm.EnqueueCascadeInvalidation(
		tx,
		memoryID, "memory",
		"memory_shredded", "", 0,
	); err != nil {
		return nil, fmt.Errorf("shred: cascade enqueue: %w", err)
	} else {
		cascadeIntents = n
	}

	if _, err := tx.Exec(`DELETE FROM memories WHERE id = ?`, memoryID); err != nil {
		return nil, fmt.Errorf("shred: delete memory: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("shred: commit: %w", err)
	}
	committed = true

	result := map[string]interface{}{
		"success":         true,
		"memory_id":       memoryID,
		"shredded":        true,
		"cascade_intents": cascadeIntents,
	}
	if theoryID != "" {
		result["theory_purged"] = theoryID
	}
	return result, nil
}

// SnoozeMemory bumps a memory's relevance without promoting it to LTM.
// Caps weight at 9 (never reaches the LTM threshold of 10) and refreshes
// last_accessed_at by the given number of days. The days parameter is
// clamped to >= 1 to prevent nonsense values.
func (dm *DatabaseManager) SnoozeMemory(memoryID string, days int) (map[string]interface{}, error) {
	if memoryID == "" {
		return nil, fmt.Errorf("memory_id is required")
	}
	if days <= 0 {
		days = 1
	}
	res, err := dm.db.Exec(`
		UPDATE memories
		SET weight = MIN(weight + 1, 9),
		    last_accessed_at = CAST(strftime('%s','now', '+' || ? || ' days') AS INTEGER)
		WHERE id = ? AND deleted_at IS NULL
	`, days, memoryID)
	if err != nil {
		return nil, fmt.Errorf("snooze: %w", err)
	}
	rows, _ := res.RowsAffected()
	if rows == 0 {
		return nil, fmt.Errorf("snooze: memory %q not found", memoryID)
	}
	return map[string]interface{}{
		"success":   true,
		"memory_id": memoryID,
		"days":      days,
	}, nil
}

// SetMemoryWeight sets a memory's weight directly. Validates the weight
// is in the [0, 100] range so a typo can't blow the scoring model out of
// proportion. Returns an error if the memory doesn't exist.
func (dm *DatabaseManager) SetMemoryWeight(memoryID string, weight int) (map[string]interface{}, error) {
	if memoryID == "" {
		return nil, fmt.Errorf("memory_id is required")
	}
	if weight < 0 || weight > 100 {
		return nil, fmt.Errorf("weight must be 0-100 (got %d)", weight)
	}
	res, err := dm.db.Exec(`UPDATE memories SET weight = ? WHERE id = ? AND deleted_at IS NULL`, weight, memoryID)
	if err != nil {
		return nil, fmt.Errorf("set weight: %w", err)
	}
	rows, _ := res.RowsAffected()
	if rows == 0 {
		return nil, fmt.Errorf("set weight: memory %q not found", memoryID)
	}
	return map[string]interface{}{
		"success":   true,
		"memory_id": memoryID,
		"weight":    weight,
	}, nil
}

// PromoteMemory converts a memory to Long-Term Memory: clears TTL, applies
// a +9 reinforcement, and sets weight=10 + is_long_term=1. The combination
// is what the scoring model treats as "never decay, never garbage-collect."
func (dm *DatabaseManager) PromoteMemory(memoryID string) (map[string]interface{}, error) {
	if memoryID == "" {
		return nil, fmt.Errorf("memory_id is required")
	}
	// Best-effort: clear TTL and reinforce. The hard-promote UPDATE below
	// is what actually guarantees LTM status; the pre-steps just ensure
	// the memory's reinforcement count and TTL reflect "this is permanent."
	_ = dm.SetMemoryTTL(memoryID, time.Time{})
	_ = dm.ReinforceMemory(memoryID, 9)
	res, err := dm.db.Exec(`UPDATE memories SET weight = 10, is_long_term = 1 WHERE id = ? AND deleted_at IS NULL`, memoryID)
	if err != nil {
		return nil, fmt.Errorf("promote: %w", err)
	}
	rows, _ := res.RowsAffected()
	if rows == 0 {
		return nil, fmt.Errorf("promote: memory %q not found", memoryID)
	}
	return map[string]interface{}{
		"success":      true,
		"memory_id":    memoryID,
		"weight":       10,
		"is_long_term": true,
	}, nil
}

// ReinforceMemoryTool is the tool-facing wrapper around dm.ReinforceMemory.
// Lives here (not next to the underlying method) so tool-callers don't
// have to import web_db internals and the call signature stays simple.
func (dm *DatabaseManager) ReinforceMemoryTool(memoryID string, delta int) (map[string]interface{}, error) {
	if memoryID == "" {
		return nil, fmt.Errorf("memory_id is required")
	}
	if delta == 0 {
		delta = 1
	}
	if err := dm.ReinforceMemory(memoryID, delta); err != nil {
		return nil, fmt.Errorf("reinforce: %w", err)
	}
	return map[string]interface{}{
		"success":   true,
		"memory_id": memoryID,
		"delta":     delta,
	}, nil
}

// WeakenMemoryTool mirrors ReinforceMemoryTool for the negative direction.
// Uses AdjustMemoryWeight (which has a hard floor at 1) rather than
// WeakenMemory so a typo can't drive weight negative.
func (dm *DatabaseManager) WeakenMemoryTool(memoryID string, delta int) (map[string]interface{}, error) {
	if memoryID == "" {
		return nil, fmt.Errorf("memory_id is required")
	}
	if delta == 0 {
		delta = 1
	}
	if err := dm.AdjustMemoryWeight(memoryID, -delta); err != nil {
		return nil, fmt.Errorf("weaken: %w", err)
	}
	return map[string]interface{}{
		"success":   true,
		"memory_id": memoryID,
		"delta":     -delta,
	}, nil
}

// PatchMemoryMetadata merges a JSON patch into the existing metadata.
// The patch must be a JSON object string; primitive values are rejected.
// Existing keys not in the patch are preserved (merge, not replace).
func (dm *DatabaseManager) PatchMemoryMetadata(memoryID string, patchJSON string) (map[string]interface{}, error) {
	if memoryID == "" {
		return nil, fmt.Errorf("memory_id is required")
	}
	if err := dm.UpdateMemoryMetadata(memoryID, patchJSON); err != nil {
		return nil, fmt.Errorf("patch memory: %w", err)
	}
	return map[string]interface{}{
		"success":   true,
		"memory_id": memoryID,
	}, nil
}

// SynthesizeMemoryFor runs LLM-driven merge synthesis for a single memory
// against all other non-LTM memories. AutoSynthesize's per-call contract:
// it scans for near-miss clusters and either merges them (writing a new
// synthesized memory row + soft-deleting the originals) or no-ops.
//
// Lazy SynthClient construction: each call instantiates a fresh client
// from env (MINIMAX_API_KEY / OPENAI_API_KEY). The client is stateless
// apart from its config, so per-call creation is fine — the synthesis
// loop is the slow part, not client init.
//
// Returns a result map with success flag and the auto-synthesize stats
// (scanned/merged/no-op counts). Errors from the LLM surface as Go errors.
func (dm *DatabaseManager) SynthesizeMemoryFor(ctx context.Context, memoryID string) (map[string]interface{}, error) {
	if memoryID == "" {
		return nil, fmt.Errorf("memory_id is required")
	}
	// Fetch the memory to get its content for synthesis. GetMemory
	// returns sql.ErrNoRows for missing rows (not nil map) — collapse
	// both to a friendlier "not found" error so callers see one shape.
	mem, err := dm.GetMemory(memoryID)
	if err != nil || mem == nil {
		return nil, fmt.Errorf("synthesize: memory %q not found", memoryID)
	}
	content, _ := mem["content"].(string)
	if content == "" {
		return nil, fmt.Errorf("synthesize: memory %q has no content to synthesize", memoryID)
	}

	client := synth.NewSynthClient()
	AutoSynthesize(ctx, dm, client, memoryID, content)

	return map[string]interface{}{
		"success":   true,
		"memory_id": memoryID,
		"ran_scan":  true,
	}, nil
}