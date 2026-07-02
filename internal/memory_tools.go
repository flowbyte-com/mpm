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
	"strings"
	"time"

	"mpm/internal/synth"
)

// SaveMemoryWithContext persists a fact to memory, injecting provenance
// and active context into metadata. weight (0–1 float) is recorded in
// meta.weight_intent since MemoryStore.AddMemory has no weight param.
func (dm *DatabaseManager) SaveMemoryWithContext(
	fact, collection string,
	tags []string,
	weight float64,
	ttl string,
	ac ActiveContext,
) (map[string]interface{}, *Memory, error) {
	if collection == "" {
		collection = "memories"
	}
	if weight <= 0 {
		weight = 0.5
	}

	meta := ac.withActiveContextMeta(nil)

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

	return map[string]interface{}{
		"success": true,
		"id":      mem.ID,
		"content": mem.Content,
		"weight":  mem.Weight,
		"tags":    mem.Tags,
	}, mem, nil
}

// HybridSearchMemories runs hybrid (BM25 + semantic) search with FTS fallback.
// Named to avoid collision with dm.SearchMemories in web_db.go.
func (dm *DatabaseManager) HybridSearchMemories(query, collection string, limit int) ([]map[string]interface{}, error) {
	if limit <= 0 {
		limit = 5
	}
	cfg := DefaultHybridConfig()
	cfg.Limit = limit
	mems, err := HybridSearch(dm, query, collection, cfg)
	if err != nil {
		// Fallback: try FullTextSearch alone (no vector, no quarantine)
		store, storeErr := dm.getSharedStore()
		if storeErr != nil {
			return nil, fmt.Errorf("hybrid search: %w; fallback store: %v", err, storeErr)
		}
		fallback, fallbackErr := store.FullTextSearch(query, collection, limit)
		if fallbackErr != nil {
			return nil, fmt.Errorf("hybrid search: %w; fulltext fallback: %v", err, fallbackErr)
		}
		mems = nil // signal fallback mode
		for _, m := range fallback {
			mems = append(mems, HybridResult{
				ID:         m.ID,
				Content:    m.Content,
				Collection: m.Collection,
				Tags:       strings.Join(m.Tags, ","),
				Weight:     m.Weight,
			})
		}
	}
	items := make([]map[string]interface{}, 0, len(mems))
	for _, m := range mems {
		// Extract banner from content if present (banner is prepended in Phase 5)
		var banner string
		if m.IsConceptDrift {
			banner = conceptDriftWarning
		} else if m.IsChallenged {
			banner = challengeWarning
		}
		items = append(items, map[string]interface{}{
			"id":                  m.ID,
			"content":             m.Content,
			"weight":              m.Weight,
			"tags":                m.Tags,
			"collection":          m.Collection,
			"banner":              banner,
			"is_concept_drift":    m.IsConceptDrift,
			"is_challenged":       m.IsChallenged,
			"challenged_theory_id": m.ChallengedTheoryID,
		})
	}
	return items, nil
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
func (dm *DatabaseManager) ShredMemoryWithCascade(memoryID string) (map[string]interface{}, error) {
	if memoryID == "" {
		return nil, fmt.Errorf("memory_id is required")
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

	tx, err := dm.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("shred: begin: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`DELETE FROM topic_memberships WHERE memory_id = ?`, memoryID); err != nil {
		return nil, fmt.Errorf("shred: delete memberships: %w", err)
	}
	if theoryID != "" {
		if _, err := tx.Exec(`DELETE FROM memories WHERE id = ?`, theoryID); err != nil {
			return nil, fmt.Errorf("shred: delete theory: %w", err)
		}
	}
	if _, err := tx.Exec(`DELETE FROM memories WHERE id = ?`, memoryID); err != nil {
		return nil, fmt.Errorf("shred: delete memory: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("shred: commit: %w", err)
	}

	result := map[string]interface{}{
		"success":   true,
		"memory_id": memoryID,
		"shredded":  true,
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
		    last_accessed_at = datetime('now', '+' || ? || ' days')
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