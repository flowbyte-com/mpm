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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/flowbyte-com/mpm-core/config"
	"github.com/flowbyte-com/mpm-core/synth"
)

// nearMissCandidate represents a single FTS5 match that is semantically close
// to the newly ingested memory.
type nearMissCandidate struct {
	ID      string `json:"id"`
	Content string `json:"content"`
	Score   int    `json:"score"`
	Tags    string `json:"tags,omitempty"`
}

// sanitiseFTS5Tokens splits content into FTS5-safe tokens. Each token
// is wrapped in FTS5 phrase quotes ("token") so FTS5 treats them as
// literal phrases rather than column-filter syntax.
//
// Why this matters: FTS5 supports the column-filter syntax `col:term`.
// A bare token that happens to match a column name (e.g. "07" matches
// no column, but any token that DOES match a column name without the
// `:`) triggers a "no such column: X" error from the FTS5 engine.
// The watchdog before this fix showed 164 synthesize_skip events all
// with errors like "no such column: 07" / "no such column: CHOICE" —
// those are memory-content tokens being misinterpreted as column names.
//
// FTS5 operators (AND, OR, NOT, NEAR) are skipped. FTS5 special
// characters (* " ( ) : - ^ +) are stripped from word boundaries so
// the literal phrase itself is clean. Outputs are wrapped in FTS5
// phrase quotes so FTS5 treats the whole token as a literal term.
//
// Note: this is NOT a parameterised SQL query — the FTS5 MATCH term
// is inherently a search-engine string and must be quoted per-token.
// Parameterised queries (the parameter binding at the SQL layer) are
// already correct in DetectNearMiss; the bug is at the FTS5 MATCH
// term level, not the SQL parameter level.
func sanitiseFTS5Tokens(content string) []string {
	words := strings.Fields(content)
	tokens := make([]string, 0, len(words))
	for _, w := range words {
		w = strings.TrimFunc(w, func(r rune) bool {
			return r == '*' || r == '"' || r == '(' || r == ')' ||
				r == ':' || r == '-' || r == '^' || r == '+'
		})
		up := strings.ToUpper(w)
		if up == "AND" || up == "OR" || up == "NOT" || up == "NEAR" || w == "" {
			continue
		}
		// FTS5 phrase quoting: each token becomes a literal phrase.
		// %q handles any embedded double-quotes (none should
		// survive the TrimFunc above, but be defensive).
		tokens = append(tokens, fmt.Sprintf("%q", w))
		if len(tokens) >= 100 {
			break
		}
	}
	return tokens
}

// synthesisEnabled is the kill switch for the background synthesis
// engine. Reads the substrate config and returns whether synthesis
// should fire. Default true (a missing field in mpm_config.json
// doesn't accidentally disable synthesis). Pointer-to-bool in the
// config struct distinguishes "not set" (default on) from explicitly
// false (kill switch engaged).
//
// Cheap on every call: file load + JSON parse. AutoSynthesize is
// async fire-and-forget so this isn't on the user-facing hot path.
// Operators can disable synthesis via:
//
//	mpm config set synthesis_enabled false
func synthesisEnabled() bool {
	cfg, err := config.LoadConfig()
	if err != nil || cfg == nil {
		return true
	}
	if cfg.SynthesisEnabled == nil {
		return true
	}
	return *cfg.SynthesisEnabled
}

// synthHashContent returns the lower-case hex SHA-256 of content.
// Format matches the existing memories.content_hash column
// (BackfillContentHash in memory.go uses the same expression), so
// the dedup table and the memories table share the same identity
// key. Length is always 64 hex chars.
//
// Cheap on every call: SHA-256 of <10KB content is sub-millisecond
// on any modern CPU. Faster than the FTS5 query, so this is the
// fast-skip path — compute the hash first, look it up, skip the
// expensive query and the LLM call.
func synthHashContent(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// synthCooldownSeconds is the minimum time (in seconds) between
// re-synthesizing the same content. Looked up via the typed
// GetConfigInt helper from system_config (key
// "synth.cooldown_seconds"), with env-var override
// MPM_SYNTH_COOLDOWN_SECONDS, default 3600 (one hour).
//
// Default 1h is the conservative baseline: long enough to
// stop token burn under burst writes, short enough that a
// re-ingestion 30 minutes later still gets a fresh synthesis
// if the surrounding context has changed meaningfully.
//
// AutoSynthesize does not pollute this surface; the cooldown
// is per-row on memories.last_synthesized_at and per-hash on
// synth_runs. The combination means:
//   - same content within cooldown: skipped (synth_runs hit)
//   - same content after cooldown: re-synthesized (synth_runs
//     upserted, last_run_at bumped)
//   - different content but overlapping candidates: filtered
//     out by DetectNearMiss cooldown on last_synthesized_at
func synthCooldownSeconds() int {
	cfg, err := config.LoadConfig()
	if err != nil || cfg == nil {
		return 3600
	}
	if v := osGetenvInt("synth.cooldown_seconds", cfg); v > 0 {
		return v
	}
	return 3600
}

// osGetenvInt reads MPM_<KEY> env var with dots->underscores
// lookup. Returns 0 if unset or unparseable; caller handles
// default semantics. Mirrors GetConfigInt's env-var fallback.
func osGetenvInt(key string, _ *config.Config) int {
	envKey := "MPM_" + strings.ToUpper(strings.ReplaceAll(key, ".", "_"))
	v := os.Getenv(envKey)
	if v == "" {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0
	}
	return n
}

// synthHasContentHash checks the persistent synth_runs ledger
// for the given content hash. Returns (true, runCount) on hit,
// (false, 0) on miss. Cheap O(1) lookup keyed by content_hash.
//
// The ledger is the authoritative "has this content ever been
// involved in synthesis" record. Updates atomically bump
// last_run_at and run_count; first_run_at and
// result_memory_id are preserved from the original synthesis.
func synthHasContentHash(dm CoreDB, contentHash string) (bool, int) {
	if dm == nil {
		return false, 0
	}
	var runCount int
	err := dm.SQLDB().QueryRow(
		`SELECT run_count FROM synth_runs WHERE content_hash = ?`,
		contentHash,
	).Scan(&runCount)
	if err != nil {
		return false, 0
	}
	return true, runCount
}

// recordSynthRun upserts a synth_runs row. On first synthesis
// for this content_hash: insert with run_count=1. On re-sights
// (shouldn't normally happen given the fast-skip, but defensive):
// bump last_run_at and run_count. first_run_at and
// result_memory_id stay locked to the original synthesis.
//
// resultMemoryID is the id of the synthesized memory written
// from this content; surfaced for forensic trails.
func recordSynthRun(dm CoreDB, contentHash, resultMemoryID string) {
	if dm == nil {
		return
	}
	now := time.Now().Unix()
	dm.SQLDB().Exec(`
		INSERT INTO synth_runs (content_hash, first_run_at, last_run_at, run_count, result_memory_id)
		VALUES (?, ?, ?, 1, ?)
		ON CONFLICT(content_hash) DO UPDATE SET
			last_run_at = excluded.last_run_at,
			run_count = run_count + 1
	`, contentHash, now, now, resultMemoryID)
}

// bumpSynthDedupCounter increments run_count and refreshes
// last_run_at on a synth_runs row without changing the
// result_memory_id. Used when a dedup HIT (the content was
// already synthesized) but the caller wants a watchdog-visible
// signal that the same content was re-sighted. run_count is the
// cumulative number of times this content was seen across the
// lifetime of the ledger.
func bumpSynthDedupCounter(dm CoreDB, contentHash string) {
	if dm == nil {
		return
	}
	now := time.Now().Unix()
	dm.SQLDB().Exec(`
		UPDATE synth_runs
		SET last_run_at = ?, run_count = run_count + 1
		WHERE content_hash = ?
	`, now, contentHash)
}

// markMemorySynthCooldown stamps last_synthesized_at on a
// surviving memory (typically the synthesized merge result,
// R). Makes the new memory's cooldown visible to
// DetectNearMiss for the next ingest: candidates that were
// just produced from synthesis won't themselves be
// immediately re-merged into another synthesis run.
//
// Originals (newID + soft-deleted candidates) are NOT
// stamped — they're being deleted; their last_synthesized_at
// would be moot.
func markMemorySynthCooldown(dm CoreDB, memoryID string) {
	if dm == nil || memoryID == "" {
		return
	}
	dm.SQLDB().Exec(
		`UPDATE memories SET last_synthesized_at = CAST(strftime('%s','now') AS INTEGER) WHERE id = ?`,
		memoryID,
	)
}

// DetectNearMiss uses FTS5 bm25 to find memories whose content overlaps
// semantically with the given text. Returns up to 10 candidates ordered by
// relevance, filtered by a bm25 score threshold AND by the per-memory
// last_synthesized_at cooldown.
//
// bm25 in SQLite returns negative values where lower (more negative) = more
// relevant. The threshold of -10 means "at least somewhat relevant". The
// cooldownSeconds parameter excludes memories stamped with a recent
// last_synthesized_at — pass 0 to disable the cooldown filter (back-compat
// for callers that want the raw FTS5 result).
//
// "Exact duplicates (same content_hash) are excluded via the newID
// parameter" — kept from the prior version but note the AutoSynthesize
// fast-skip on synth_runs handles duplicates BEFORE we get here.
func DetectNearMiss(dm CoreDB, content string, newID string, threshold float64, cooldownSeconds int) ([]nearMissCandidate, error) {
	tokens := sanitiseFTS5Tokens(content)
	if len(tokens) == 0 {
		return nil, nil
	}
	query := strings.Join(tokens, " ")

	// Cooldown predicate. If cooldownSeconds <= 0 the filter is a
	// no-op (the IS NULL OR < cutoff becomes IS NULL OR 1, which
	// matches all rows).
	var cooldownClause string
	var cooldownArgs []interface{}
	if cooldownSeconds > 0 {
		cutoff := time.Now().Unix() - int64(cooldownSeconds)
		cooldownClause = ` AND (m.last_synthesized_at IS NULL OR m.last_synthesized_at < ?) `
		cooldownArgs = []interface{}{cutoff}
	}

	args := append([]interface{}{query, newID}, cooldownArgs...)

	rows, err := dm.SQLDB().Query(`
		SELECT m.id, m.content, bm25(memories_fts) as score, COALESCE(m.tags, '') as tags
		FROM memories m
		JOIN memories_fts fts ON m.rowid = fts.rowid
		WHERE memories_fts MATCH ?
		  AND m.deleted_at IS NULL
		  AND m.id != ?
		  AND m.collection NOT IN ('theories', 'decisions')`+cooldownClause+`
		ORDER BY score
		LIMIT 10
	`, args...)
	if err != nil {
		return nil, fmt.Errorf("FTS5 near-miss query failed: %w", err)
	}
	defer rows.Close()

	var candidates []nearMissCandidate
	for rows.Next() {
		var c nearMissCandidate
		var score float64
		if err := rows.Scan(&c.ID, &c.Content, &score, &c.Tags); err != nil {
			return nil, fmt.Errorf("scanning near-miss candidate row: %w", err)
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
func AutoSynthesize(ctx context.Context, dm CoreDB, client *synth.SynthClient, newID, content string) {
	if client == nil || dm == nil {
		return
	}
	// Kill switch: operators can disable the background synthesis
	// engine entirely via `mpm config set synthesis_enabled false`.
	// Without this, the engine fires an LLM call on every memory
	// write chain that finds near-miss candidates — that's the
	// root cause of token burn spikes. The switch is on top of
	// the nil checks so we don't error if config is unloadable.
	if !synthesisEnabled() {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}

	// 0. content_hash fast-skip (BEFORE FTS5 query).
	// sha256(content) → check synth_runs ledger. Hit means this
	// exact content has already been synthesized (or attempted)
	// in some prior session; bumping last_run_at is enough for
	// observability, but the LLM call is unnecessary. This is
	// the layer that stops the token burn from re-ingestion of
	// already-synthesized content (e.g. a tui save that re-saves
	// the same memory because the user hit cmd-s twice).
	contentHash := synthHashContent(content)
	if hit, runCount := synthHasContentHash(dm, contentHash); hit {
		bumpSynthDedupCounter(dm, contentHash)
		logWatchdogOp(dm, "synthesize_skip", map[string]interface{}{
			"reason":       "content_hash already synthesized",
			"content_hash": contentHash,
			"prior_runs":   runCount,
			"timestamp":    time.Now().UTC().Format(time.RFC3339),
		})
		return
	}

	// 1. FTS5 pre-filter (with candidate cooldown via last_synthesized_at).
	threshold := -10.0
	cooldown := synthCooldownSeconds()
	candidates, err := DetectNearMiss(dm, content, newID, threshold, cooldown)
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
	if err := dm.SQLDB().QueryRowContext(ctx,
		`SELECT MIN(created_at) FROM memories WHERE id IN (`+strings.Join(idPlaceholders, ",")+`)`,
		idArgs...,
	).Scan(&oldestCreatedAt); err != nil {
		// MIN over an empty set returns NULL (not ErrNoRows), so an
		// error here is a real query failure — log it rather than
		// silently swallowing. The oldestCreatedAt = nil fallback
		// degrades gracefully (the synthetic memory gets its own
		// created_at from the SaveMemory call earlier) so this path
		// is best-effort by design, but a silent miss would mask
		// genuine DB trouble from the audit log.
		dm.LogAudit(AuditWarn, "synthesis", fmt.Sprintf("AutoSynthesize: oldest created_at query failed, falling back to synthetic memory's own created_at: %v", err), "", AuditContext{})
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
	embedding, embedErr := EmbedText(result.Content)
	if embedErr != nil {
		dm.LogAudit(AuditWarn, "synthesis", fmt.Sprintf("synthesis succeeded but embedding failed: %v", embedErr), "", AuditContext{})
		// Spec §4.4: persist the embedding failure on the memory itself so
		// a later query or backfill can find it without scanning the
		// audit ledger. Mirrors the structured-error contract that the
		// MCP save_to_memory surface exposes (Task 14).
		metadata["embedding_error"] = embedErr.Error()
		metadata["embedding_status"] = "unavailable"
	}

	// 8. Save the synthesized LTM
	newSynthID, err := dm.SaveMemory("memories", result.Content, "", allTags, metadata, embedding, true, 10)
	if err != nil {
		logWatchdogOp(dm, "synthesize_failed", map[string]interface{}{
			"content": truncatedContent(content),
			"error":   fmt.Sprintf("save failed: %v", err),
		})
		return
	}

	// 9. Preserve oldest created_at from the source fragments — canonical INTEGER
	if oldestCreatedAt != nil && *oldestCreatedAt != "" {
		if _, err := dm.SQLDB().ExecContext(ctx, "UPDATE memories SET created_at = CAST(? AS INTEGER) WHERE id = ?", *oldestCreatedAt, newSynthID); err != nil {
			dm.LogAudit(AuditWarn, "synthesis", fmt.Sprintf("AutoSynthesize: created_at preservation UPDATE failed (synth=%s): %v", newSynthID, err), "", AuditContext{})
		}
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
	if _, err := dm.SQLDB().ExecContext(ctx,
		`INSERT OR IGNORE INTO topic_memberships (memory_id, topic_id, role, created_at)
		 SELECT ?, topic_id, role, created_at
		 FROM topic_memberships
		 WHERE memory_id IN (`+strings.Join(topicPlaceholders, ",")+`)`,
		topicArgs...,
	); err != nil {
		dm.LogAudit(AuditWarn, "synthesis", fmt.Sprintf("AutoSynthesize: topic_memberships transfer failed (synth=%s): %v", newSynthID, err), "", AuditContext{})
	}

	// 11. Soft-delete originals (candidates + triggering memory)
	for _, c := range toMerge {
		if _, err := dm.SQLDB().ExecContext(ctx, "UPDATE memories SET deleted_at = CAST(strftime('%s','now') AS INTEGER) WHERE id = ?", c.ID); err != nil {
			dm.LogAudit(AuditWarn, "synthesis", fmt.Sprintf("AutoSynthesize: soft-delete UPDATE failed (orig=%s): %v", c.ID, err), "", AuditContext{})
		}
	}
	if _, err := dm.SQLDB().ExecContext(ctx, "UPDATE memories SET deleted_at = CAST(strftime('%s','now') AS INTEGER) WHERE id = ?", newID); err != nil {
		dm.LogAudit(AuditWarn, "synthesis", fmt.Sprintf("AutoSynthesize: soft-delete UPDATE failed (orig=%s): %v", newID, err), "", AuditContext{})
	}

	// 12. Persist the synth_runs ledger entry + stamp cooldown on R.
	// recordSynthRun: content_hash → first/last_run_at + run_count.
	// markMemorySynthCooldown: stamps R.last_synthesized_at so the
	// next FTS5 query won't re-pick R as a candidate within the
	// cooldown window. Originals (newID + toMerge) are NOT stamped:
	// their last_synthesized_at would be moot (they're being
	// soft-deleted on the next step).
	recordSynthRun(dm, contentHash, newSynthID)
	markMemorySynthCooldown(dm, newSynthID)

	// 13. Log success to watchdog
	logWatchdogOp(dm, "synthesize", map[string]interface{}{
		"new_id":       newSynthID,
		"old_ids":      sourceIDs,
		"content_hash": contentHash,
		"cooldown_s":   cooldown,
		"timestamp":    time.Now().UTC().Format(time.RFC3339),
	})
}

// logWatchdogOp writes a structured synthesis event to watchdog.jsonl.
// Uses logWatchdogRaw to write a single entry with synthesis-specific fields
// (op, content, error, new_id, old_ids, timestamp). Does NOT use the
// watchdogOp format (which is for query timing) to avoid schema fragmentation.
func logWatchdogOp(dm CoreDB, op string, data map[string]interface{}) {
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
	// Type-assert to access unexported logWatchdogRaw. If the
	// CoreDB wasn't created by NewDatabaseManager/NewSession,
	// the watchdog entry is silently dropped — acceptable for
	// best-effort observability.
	if dm, ok := dm.(*DatabaseManager); ok {
		dm.logWatchdogRaw(line)
	}
}

func truncatedContent(s string) string {
	if len(s) > 120 {
		return s[:120] + "..."
	}
	return s
}
