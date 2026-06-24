package internal

// Synthesis orchestration: post-write near-miss detection and consolidation.
// The LLM HTTP client lives in internal/synth; this file is the orchestration
// layer that ties near-miss detection → LLM call → memory persistence together.
//
// Split rationale: the LLM client (synth) is reusable infrastructure that could
// drive other features (admission, summarisation). AutoSynthesize is one
// specific orchestrator that uses it. Keeping them in separate files /
// packages means the orchestrator can be tested with a mock SynthClient and
// the client can evolve independently of how it's invoked here.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"mpm/internal/synth"
)

// nearMissCandidate represents a single FTS5 match that is semantically close
// to the newly ingested memory.
type nearMissCandidate struct {
	ID      string `json:"id"`
	Content string `json:"content"`
	Score   int    `json:"score"`
	Tags    string `json:"tags,omitempty"`
}

// DetectNearMiss uses FTS5 bm25 to find memories whose content overlaps
// semantically with the given text. Returns up to 10 candidates ordered by
// relevance, filtered by a bm25 score threshold.
//
// bm25 in SQLite returns negative values where lower (more negative) = more
// relevant. The threshold of -10 means "at least somewhat relevant". Exact
// duplicates (same content_hash) are excluded via the newID parameter.
func DetectNearMiss(dm *DatabaseManager, content string, newID string, threshold float64) ([]nearMissCandidate, error) {
	words := strings.Fields(content)
	sanitised := make([]string, 0, len(words))
	for _, w := range words {
		w = strings.TrimFunc(w, func(r rune) bool {
			return r == '*' || r == '"' || r == '(' || r == ')'
		})
		up := strings.ToUpper(w)
		if up == "AND" || up == "OR" || up == "NOT" || up == "NEAR" || w == "" {
			continue
		}
		sanitised = append(sanitised, w)
		if len(sanitised) >= 100 {
			break
		}
	}
	if len(sanitised) == 0 {
		return nil, nil
	}
	query := strings.Join(sanitised, " ")

	rows, err := dm.SQLDB().Query(`
		SELECT m.id, m.content, bm25(memories_fts) as score, m.tags
		FROM memories m
		JOIN memories_fts fts ON m.rowid = fts.rowid
		WHERE memories_fts MATCH ?
		  AND m.deleted_at IS NULL
		  AND m.id != ?
		  AND m.collection NOT IN ('theories', 'decisions')
		ORDER BY score
		LIMIT 10
	`, query, newID)
	if err != nil {
		return nil, fmt.Errorf("FTS5 near-miss query failed: %w", err)
	}
	defer rows.Close()

	var candidates []nearMissCandidate
	for rows.Next() {
		var c nearMissCandidate
		var score float64
		if err := rows.Scan(&c.ID, &c.Content, &score, &c.Tags); err != nil {
			continue
		}
		// bm25: more negative = stronger match
		if score < threshold {
			c.Score = int(score)
			candidates = append(candidates, c)
		}
	}
	return candidates, rows.Err()
}

// =============================================================================
// Session-Level Dedup Tracking
// =============================================================================

// synthSessionDedup prevents re-triggering synthesis for the same pair of
// memory IDs within one ingestion session. Entries older than 1 hour are
// evicted on every access to prevent unbounded growth.
var (
	synthSeen   = map[string]time.Time{}
	synthSeenMu sync.Mutex
)

// InitSynthDedupFromDB queries the synthesis collection in the database and
// pre-populates the in-memory dedup map so that previously merged pairs are
// not re-synthesised after a process restart.
//
// Call once during application startup before any call to AutoSynthesize.
func InitSynthDedupFromDB(dm *DatabaseManager) {
	if dm == nil {
		return
	}
	rows, err := dm.SQLDB().Query(`
		SELECT metadata FROM memories
		WHERE collection = 'synthesis'
		  AND deleted_at IS NULL
		ORDER BY created_at DESC
		LIMIT 200
	`)
	if err != nil {
		return
	}
	defer rows.Close()

	now := time.Now()
	synthSeenMu.Lock()
	defer synthSeenMu.Unlock()

	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			continue
		}
		var record struct {
			SourceIDs []string `json:"source_ids"`
		}
		if json.Unmarshal([]byte(raw), &record) != nil {
			// Try parsing as metadata wrapper: collection=synthesis stores the
			// SynthesisRecord JSON in the content column, but the metadata may
			// also contain source_ids directly.
			var meta struct {
				SourceIDs []string `json:"source_ids"`
			}
			if json.Unmarshal([]byte(raw), &meta) == nil {
				record.SourceIDs = meta.SourceIDs
			}
		}
		if len(record.SourceIDs) < 2 {
			continue
		}
		for i := 0; i < len(record.SourceIDs); i++ {
			for j := i + 1; j < len(record.SourceIDs); j++ {
				a, b := record.SourceIDs[i], record.SourceIDs[j]
				if a > b {
					a, b = b, a
				}
				synthSeen[a+"|"+b] = now
			}
		}
	}
}

const synthDedupTTL = 1 * time.Hour

// synthSweepExpired removes entries older than synthDedupTTL.
// Called internally by markSynthPair and hasSynthPair.
func synthSweepExpired() {
	now := time.Now()
	for k, v := range synthSeen {
		if now.Sub(v) > synthDedupTTL {
			delete(synthSeen, k)
		}
	}
}

// markSynthPair records that a pair of IDs has been sent for synthesis.
func markSynthPair(a, b string) {
	synthSeenMu.Lock()
	defer synthSeenMu.Unlock()
	synthSweepExpired()
	key := a + "|" + b
	if a > b {
		key = b + "|" + a
	}
	synthSeen[key] = time.Now()
}

// hasSynthPair checks whether a pair of IDs has already been queued this session.
func hasSynthPair(a, b string) bool {
	synthSeenMu.Lock()
	defer synthSeenMu.Unlock()
	synthSweepExpired()
	key := a + "|" + b
	if a > b {
		key = b + "|" + a
	}
	_, ok := synthSeen[key]
	return ok
}

// =============================================================================
// Auto-Synthesis After Ingestion
// =============================================================================

// AutoSynthesize is called after a new memory has been written. It:
//  1. Runs FTS5 near-miss detection against the new memory content
//  2. If candidates are found, invokes the LLM for consolidation
//  3. On success: writes the synthesized LTM, soft-deletes originals
//  4. On failure: logs to watchdog, writes original without merging
//
// The caller may pass a cancellable ctx. If ctx is cancelled before the API
// call completes, the write degrades gracefully — the original memory remains
// in place and the synthesis is silently abandoned.
func AutoSynthesize(ctx context.Context, dm *DatabaseManager, client *synth.SynthClient, newID, content string) {
	if client == nil || dm == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}

	// 1. FTS5 pre-filter
	threshold := -10.0
	candidates, err := DetectNearMiss(dm, content, newID, threshold)
	if err != nil {
		logWatchdogOp(dm, "synthesize_skip", map[string]interface{}{
			"reason": "near-miss detection failed",
			"error":  err.Error(),
		})
		return
	}
	if len(candidates) == 0 {
		return // no near-misses found — normal write, no synthesis
	}

	// Check cancellation before proceeding to LLM
	if ctx.Err() != nil {
		return
	}

	// 2. Deduplicate at session level: skip pairs already seen
	var toMerge []nearMissCandidate
	for _, c := range candidates {
		if !hasSynthPair(newID, c.ID) {
			markSynthPair(newID, c.ID)
			toMerge = append(toMerge, c)
		}
	}
	if len(toMerge) == 0 {
		logWatchdogOp(dm, "synthesize_skip", map[string]interface{}{
			"reason": "all candidates already seen this session",
		})
		return
	}

	// Limit to first 4 candidates (+ new memory = 5 max fragments)
	if len(toMerge) > 4 {
		toMerge = toMerge[:4]
	}

	// 3. Build fragments list: new memory + candidates
	fragments := make([]string, 0, 1+len(toMerge))
	fragments = append(fragments, content)
	for _, c := range toMerge {
		fragments = append(fragments, c.Content)
	}

	// 4. Invoke LLM with timeout. If the caller's context has a tighter
	// deadline, respect it; otherwise derive from the client timeout.
	llmCtx := ctx
	if _, hasDeadline := llmCtx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		llmCtx, cancel = context.WithTimeout(llmCtx, client.Timeout)
		defer cancel()
	}

	result, err := client.Synthesize(llmCtx, fragments)
	if err != nil {
		// On failure: log to watchdog, do not block ingestion
		truncated := content
		if len(truncated) > 120 {
			truncated = truncated[:120] + "..."
		}
		logWatchdogOp(dm, "synthesize_failed", map[string]interface{}{
			"content":   truncated,
			"error":     err.Error(),
			"timestamp": time.Now().UTC().Format(time.RFC3339),
		})
		return
	}

	// 5. Collect all source IDs (candidates + triggering memory)
	sourceIDs := make([]string, 0, len(toMerge))
	for _, c := range toMerge {
		sourceIDs = append(sourceIDs, c.ID)
	}
	allIDs := make([]string, len(sourceIDs)+1)
	copy(allIDs, sourceIDs)
	allIDs[len(sourceIDs)] = newID

	// 6. Query the oldest created_at from all source memories (before soft-delete)
	var oldestCreatedAt *string
	idPlaceholders := make([]string, len(allIDs))
	idArgs := make([]interface{}, len(allIDs))
	for i, id := range allIDs {
		idPlaceholders[i] = "?"
		idArgs[i] = id
	}
	err = dm.SQLDB().QueryRow(
		`SELECT MIN(created_at) FROM memories WHERE id IN (`+strings.Join(idPlaceholders, ",")+`)`,
		idArgs...,
	).Scan(&oldestCreatedAt)
	if err != nil {
		oldestCreatedAt = nil
	}

	// 7. Build metadata for the new synthetic memory
	metadata := map[string]interface{}{
		"provenance": map[string]interface{}{
			"source":  "synthetic",
			"model":   client.Model,
			"compute": "high",
		},
		"synthesized":    true,
		"source_ids":     sourceIDs,
		"synthesized_at": time.Now().UTC().Format(time.RFC3339),
	}
	allTags := result.Tags
	if allTags == nil {
		allTags = []string{}
	}
	allTags = append(allTags, "synthesized", "ltm")
	embedding := EmbedText(result.Content)

	// 8. Save the synthesized LTM
	newSynthID, err := dm.SaveMemory("memories", result.Content, "", allTags, metadata, embedding, true, 10)
	if err != nil {
		logWatchdogOp(dm, "synthesize_failed", map[string]interface{}{
			"content": truncatedContent(content),
			"error":   fmt.Sprintf("save failed: %v", err),
		})
		return
	}

	// 9. Preserve oldest created_at from the source fragments
	if oldestCreatedAt != nil && *oldestCreatedAt != "" {
		dm.SQLDB().Exec("UPDATE memories SET created_at = ? WHERE id = ?", *oldestCreatedAt, newSynthID)
	}

	// 10. Transfer topic_memberships from all source IDs to the new synthetic memory
	topicArgs := make([]interface{}, len(allIDs)+1)
	topicArgs[0] = newSynthID
	for i, id := range allIDs {
		topicArgs[i+1] = id
	}
	topicPlaceholders := make([]string, len(allIDs))
	for i := range allIDs {
		topicPlaceholders[i] = "?"
	}
	dm.SQLDB().Exec(
		`INSERT OR IGNORE INTO topic_memberships (memory_id, topic_id, role, created_at)
		 SELECT ?, topic_id, role, created_at
		 FROM topic_memberships
		 WHERE memory_id IN (`+strings.Join(topicPlaceholders, ",")+`)`,
		topicArgs...,
	)

	// 11. Soft-delete originals (candidates + triggering memory)
	for _, c := range toMerge {
		dm.SQLDB().Exec("UPDATE memories SET deleted_at = CURRENT_TIMESTAMP WHERE id = ?", c.ID)
	}
	dm.SQLDB().Exec("UPDATE memories SET deleted_at = CURRENT_TIMESTAMP WHERE id = ?", newID)

	// 12. Log success to watchdog
	logWatchdogOp(dm, "synthesize", map[string]interface{}{
		"new_id":    newSynthID,
		"old_ids":   sourceIDs,
		"timestamp": time.Now().UTC().Format(time.RFC3339),
	})
}

// logWatchdogOp writes a structured synthesis event to watchdog.jsonl.
// Uses logWatchdogRaw to write a single entry with synthesis-specific fields
// (op, content, error, new_id, old_ids, timestamp). Does NOT use the
// watchdogOp format (which is for query timing) to avoid schema fragmentation.
func logWatchdogOp(dm *DatabaseManager, op string, data map[string]interface{}) {
	if dm == nil {
		return
	}
	entry := map[string]interface{}{
		"op":        op,
		"timestamp": time.Now().UTC().Format(time.RFC3339),
	}
	for k, v := range data {
		entry[k] = v
	}
	line, _ := json.Marshal(entry)
	dm.logWatchdogRaw(line)
}

func truncatedContent(s string) string {
	if len(s) > 120 {
		return s[:120] + "..."
	}
	return s
}