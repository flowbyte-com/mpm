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
	"database/sql"
	"encoding/hex"
	"errors"
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

// synthExecNode is the write surface the synthesis ledger and
// cooldown helpers need: a statement with a statement-level retry
// budget.
//
// It is satisfied by a transaction node (DBNode), by a
// DatabaseManager, and by the CoreDB interface — so the SAME helper
// serves both the transactional path (inside the merge) and the
// standalone path, and there is no second SQL spelling to drift.
type synthExecNode interface {
	ExecTracked(query string, retries int, args ...interface{}) (sql.Result, error)
}

// synthWriteBudget is the statement-level retry budget for the
// synthesis writes.
//
// It is the documented budget (5) every other write in the substrate
// carries. Inside the merge transaction it is the ORDINARY busy class
// the statement layer owns; the transaction layer owns the
// stale-snapshot class. The two are different classes, so neither
// budget is multiplied.
const synthWriteBudget = 5

// recordSynthRun upserts a synth_runs row. On first synthesis
// for this content_hash: insert with run_count=1. On re-sights
// (shouldn't normally happen given the fast-skip, but defensive):
// bump last_run_at and run_count. first_run_at and
// result_memory_id stay locked to the original synthesis.
//
// resultMemoryID is the id of the synthesized memory written
// from this content; surfaced for forensic trails.
//
// It RETURNS its error (issue #178). The previous version discarded
// it, and a discarded ledger write is one half of the state that
// stops the same content from being synthesized twice.
func recordSynthRun(node synthExecNode, contentHash, resultMemoryID string) error {
	if node == nil {
		return nil
	}
	now := time.Now().Unix()
	_, err := node.ExecTracked(`
		INSERT INTO synth_runs (content_hash, first_run_at, last_run_at, run_count, result_memory_id)
		VALUES (?, ?, ?, 1, ?)
		ON CONFLICT(content_hash) DO UPDATE SET
			last_run_at = excluded.last_run_at,
			run_count = run_count + 1
	`, synthWriteBudget, contentHash, now, now, resultMemoryID)
	return err
}

// bumpSynthDedupCounter increments run_count and refreshes
// last_run_at on a synth_runs row without changing the
// result_memory_id. Used when a dedup HIT (the content was
// already synthesized) but the caller wants a watchdog-visible
// signal that the same content was re-sighted. run_count is the
// cumulative number of times this content was seen across the
// lifetime of the ledger.
//
// It returns its error (issue #178): a lost counter increment is a
// lost observation, and the caller reports it rather than pretending
// the re-sight was recorded.
func bumpSynthDedupCounter(node synthExecNode, contentHash string) error {
	if node == nil {
		return nil
	}
	now := time.Now().Unix()
	_, err := node.ExecTracked(`
		UPDATE synth_runs
		SET last_run_at = ?, run_count = run_count + 1
		WHERE content_hash = ?
	`, synthWriteBudget, now, contentHash)
	return err
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
//
// It RETURNS its error (issue #178). This is the write whose loss
// made already-processed knowledge eligible for repeated synthesis:
// with no stamp, DetectNearMiss picks the merged memory again as a
// near-miss candidate, and the merge runs a second time — duplicated
// evidence and a second artifact asserting a conclusion the first one
// already asserted.
//
// The caller passes the node of the merge transaction, so the stamp
// and the artifact it describes have the SAME FATE: they commit
// together or neither commits.
func markMemorySynthCooldown(node synthExecNode, memoryID string) error {
	if node == nil || memoryID == "" {
		return nil
	}
	_, err := node.ExecTracked(
		`UPDATE memories SET last_synthesized_at = CAST(strftime('%s','now') AS INTEGER) WHERE id = ?`,
		synthWriteBudget, memoryID,
	)
	return err
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
		// The counter bump is an OBSERVATION, not the dedup decision:
		// the decision was the ledger row's existence, which is
		// already durable. A failed bump loses a count, so it is
		// REPORTED rather than discarded (issue #178) — but it does
		// not fail the skip, because the skip itself is still
		// correct.
		if err := bumpSynthDedupCounter(dm, contentHash); err != nil {
			dm.LogAudit(AuditWarn, "synthesis", fmt.Sprintf(
				"AutoSynthesize: the synthesis re-sight counter could not be incremented (content_hash=%s): %v",
				contentHash, err), "", AuditContext{})
		}
		logSynthesizeEvent(dm, "synthesize_skip", map[string]interface{}{
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
		logSynthesizeEvent(dm, "synthesize_skip", map[string]interface{}{
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
		logSynthesizeEvent(dm, "synthesize_skip", map[string]interface{}{
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

	result, err := client.SynthesizeWithPlan(llmCtx, fragments, synth.NewPerCallPlan())
	if err != nil {
		// On failure: log to watchdog, do not block ingestion.
		// 2026-09-14 release-pass: the bounded-execution
		// safeguard tags its stops with a stable reason; the
		// watchdog records the reason for telemetry and
		// operator diagnosis (without leaking secrets — the
		// watchdog stores machine-stable fields, not the
		// raw prompt body).
		// No content excerpt: the pre-v1 record carried 120 bytes of the
		// body being synthesized, which is row content in an operational
		// log. The error and the safeguard's reason are what an operator
		// diagnoses from, and the body is retrievable from mpm.db.
		fields := map[string]interface{}{
			"error": err.Error(),
		}
		if errors.Is(err, synth.ErrBoundedPlanExceeded) {
			fields["bounded_safeguard"] = "stopped"
			fields["safeguard_reason"] = err.Error()
		}
		logSynthesizeEvent(dm, "synthesize_failed", fields)
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
		applyEmbeddingFailureToMetadata(dm, metadata, embedErr)
	}

	// 8-12. The durable outcome of the synthesis, written in ONE
	// transaction. See commitSynthesisMerge for why the writes have to
	// share a transaction — and what they cost each other when they
	// do not.
	newSynthID, err := commitSynthesisMerge(dm, ctx, synthesisMerge{
		Content:         result.Content,
		Tags:            allTags,
		Metadata:        metadata,
		Embedding:       embedding,
		SourceIDs:       allIDs,
		OldestCreatedAt: oldestCreatedAt,
		ContentHash:     contentHash,
	})
	if err != nil {
		// NOTHING was written: the transaction rolled back. The
		// originals are still live and the merge is still pending, so
		// the next ingest can try again — with no duplicated evidence,
		// no half-applied merge to clean up, and no inflated
		// confidence. The failure is reported, not swallowed.
		logSynthesizeEvent(dm, "synthesize_failed", map[string]interface{}{
			"error":        fmt.Sprintf("the merge transaction failed and was rolled back: %v", err),
			"content_hash": contentHash,
		})
		dm.LogAudit(AuditWarn, "synthesis", fmt.Sprintf(
			"AutoSynthesize: the merge transaction failed and was rolled back; no memory was written (content_hash=%s): %v",
			contentHash, err), "", AuditContext{})
		return
	}

	// 13. Log success to watchdog
	logSynthesizeEvent(dm, "synthesize", map[string]interface{}{
		"new_id":       newSynthID,
		"old_ids":      sourceIDs,
		"content_hash": contentHash,
		"cooldown_s":   cooldown,
		"timestamp":    time.Now().UTC().Format(time.RFC3339),
	})
}

// synthesisMerge is the durable OUTCOME of one synthesis: what the
// merged memory is, which memories it replaces, and the content hash
// that identifies this synthesis in the ledger.
//
// It exists so the durable write can be exercised on its own. The
// synthesis path reaches this write through an LLM call, which a unit
// test cannot make; the WRITE is where the reliability properties
// live, so it is a named, callable unit.
type synthesisMerge struct {
	// Content is the merged memory's content.
	Content string
	// Tags are the merged memory's tags.
	Tags []string
	// Metadata is the merged memory's metadata.
	Metadata map[string]interface{}
	// Embedding is the merged memory's embedding. It may be nil.
	Embedding []float32
	// SourceIDs are the memories this merge replaces: the near-miss
	// candidates plus the memory that triggered the synthesis. They
	// are soft-deleted, not destroyed.
	SourceIDs []string
	// OldestCreatedAt preserves the oldest source created_at on the
	// merged memory. It may be nil or empty.
	OldestCreatedAt *string
	// ContentHash identifies this synthesis in the synth_runs ledger.
	ContentHash string
}

// commitSynthesisMerge writes the durable outcome of one synthesis, and
// returns the id of the merged memory.
//
// WHY ONE TRANSACTION (issue #178)
// ---------------------------------
// The writes are not independent. They are one FACT: "these memories
// were merged into that one, and that one must not be merged again for
// the next hour". Split across separate transactions, the substrate can
// reach states in which the fact is half-true:
//
//   - the merged memory exists but the ledger row does not → the
//     content_hash fast-skip misses, so the same content is synthesized
//     again;
//   - the merged memory exists but the cooldown stamp does not →
//     DetectNearMiss picks it as a candidate again, so it is merged a
//     SECOND time: duplicated evidence, and a second artifact asserting
//     a conclusion the first one already asserted;
//   - the merged memory exists but the originals were not soft-deleted →
//     the evidence is duplicated in the corpus, and the next ingest sees
//     both the originals and their own replacement.
//
// The previous implementation DISCARDED the error of the last two
// writes, so those states were not even observable, let alone
// prevented.
//
// In one transaction the fact is atomic: either the merge and its dedup
// state both land, or neither does.
//
//   - A CRASH before the commit leaves nothing behind. The originals
//     are still live and the merge is still pending, so the next ingest
//     redoes the whole merge cleanly.
//   - An AMBIGUOUS COMMIT leaves the artifact and its cooldown in the
//     same fate: if the merged memory is there, it is stamped, so it is
//     not merged again; if it is not there, nothing happened. Either
//     way there is no duplicated evidence and no inflated confidence.
//
// WHY THE FIRST STATEMENT IN THE TRANSACTION IS A WRITE
// ------------------------------------------------------
// This is not a style preference; it is what makes the transaction
// survive contention.
//
// SQLite does not invoke the busy handler when a connection that
// already holds a READ lock tries to take the WRITE lock: the busy
// handler is skipped to avoid a deadlock between two connections that
// each wait for the other. Measured on this substrate's connection
// settings (WAL, `_busy_timeout=5000`):
//
//	pool write, no transaction      → waits 5.005s, then SQLITE_BUSY
//	transaction, first stmt a write → waits 5.005s, then SQLITE_BUSY
//	transaction, read then write     → returns IMMEDIATELY, SQLITE_BUSY
//
// So a transaction that READS before it WRITES turns ordinary
// contention from a WAIT into an immediate failure, and the
// statement-level retry cannot clear it (re-running the statement in
// place cannot succeed, because the transaction's read lock is what
// blocks the upgrade). The old implementation was immune because every
// one of its writes went to the POOL, where the busy handler is always
// invoked.
//
// The merge's first statement is therefore the soft-delete of the
// sources — a write, which takes the write lock at the transaction's
// first statement, where the busy handler IS invoked. Every statement
// after it runs uncontended, because the transaction already holds the
// write lock.
//
// The merged memory's own creation reads first (the substrate's F19
// identity probe), so it CANNOT be the transaction's first statement.
// That is why the soft-delete leads.
//
// CALLERS MUST NOT WRITE TO THE POOL INSIDE THE CLOSURE. A pool write
// inside a transaction that holds the write lock waits out the busy
// timeout against itself. The audit rows and the watchdog event belong
// outside it.
//
// CANCELLATION: the DBNode seam has no context-aware write (the
// established in-transaction pattern throughout the substrate is
// node.ExecTracked), so the context is honoured by CHECKING it before
// the closure's first write; a cancelled context aborts the
// transaction through its error, and the transaction rolls back.
func commitSynthesisMerge(dm CoreDB, ctx context.Context, m synthesisMerge) (string, error) {
	if dm == nil {
		return "", fmt.Errorf("commit synthesis merge: no database")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if len(m.SourceIDs) == 0 {
		return "", fmt.Errorf("commit synthesis merge: a merge with no source memories is not a merge")
	}

	// The topic_memberships transfer arguments are built BEFORE the
	// transaction: the SQL is assembled from the source id list, and
	// the list does not change inside the transaction.
	topicArgs := make([]interface{}, len(m.SourceIDs)+1)
	topicPlaceholders := make([]string, len(m.SourceIDs))
	for i, id := range m.SourceIDs {
		topicArgs[i+1] = id
		topicPlaceholders[i] = "?"
	}

	var newSynthID string
	err := dm.WithTx(func(node DBNode) error {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("the synthesis was cancelled before it was written: %w", ctxErr)
		}

		// 1. Soft-delete the originals (candidates + triggering memory).
		//
		//    FIRST, and it is a WRITE, on purpose: see the note above
		//    on SQLite's busy handler and a transaction that reads
		//    before it writes. The order inside the transaction is not
		//    observable to any other connection — the whole merge
		//    commits or none of it does.
		for _, id := range m.SourceIDs {
			if _, err := node.ExecTracked(
				"UPDATE memories SET deleted_at = CAST(strftime('%s','now') AS INTEGER) WHERE id = ?",
				synthWriteBudget, id,
			); err != nil {
				return fmt.Errorf("soft-delete the merged original %s: %w", id, err)
			}
		}

		// 2. The merged memory itself.
		id, err := dm.SaveMemoryNode(node, "memories", m.Content, "", m.Tags, m.Metadata, m.Embedding, true, 10, "", "0.5", "0.5", "")
		if err != nil {
			return fmt.Errorf("save the synthesized memory: %w", err)
		}
		newSynthID = id

		// 3. Preserve oldest created_at from the source fragments — canonical INTEGER
		if m.OldestCreatedAt != nil && *m.OldestCreatedAt != "" {
			if _, err := node.ExecTracked(
				"UPDATE memories SET created_at = CAST(? AS INTEGER) WHERE id = ?",
				synthWriteBudget, *m.OldestCreatedAt, newSynthID,
			); err != nil {
				return fmt.Errorf("preserve the merged memory's created_at: %w", err)
			}
		}

		// 4. Transfer topic_memberships from all source IDs to the merged memory
		topicArgs[0] = newSynthID
		if _, err := node.ExecTracked(
			`INSERT OR IGNORE INTO topic_memberships (memory_id, topic_id, role, created_at)
			 SELECT ?, topic_id, role, created_at
			 FROM topic_memberships
			 WHERE memory_id IN (`+strings.Join(topicPlaceholders, ",")+`)`,
			synthWriteBudget, topicArgs...,
		); err != nil {
			return fmt.Errorf("transfer topic memberships to the merged memory: %w", err)
		}

		// 5. The ledger and the cooldown, in the SAME transaction as
		// the artifact they describe. This is the pair that used to be
		// able to disagree with the artifact.
		if err := recordSynthRun(node, m.ContentHash, newSynthID); err != nil {
			return fmt.Errorf("record the synthesis ledger row: %w", err)
		}
		if err := markMemorySynthCooldown(node, newSynthID); err != nil {
			return fmt.Errorf("stamp the synthesis cooldown: %w", err)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return newSynthID, nil
}

// applyEmbeddingFailureToMetadata records a synthesis-time embedding
// failure onto both the audit ledger and the synthesized memory's
// metadata. Extracted from AutoSynthesize so the contract is testable
// in isolation — the AutoSynthesize integration path is hard to drive
// from a unit test (FTS5 bm25 threshold gating makes reaching the
// embedding step conditional on corpus shape).
//
// Contract (regression-pinned by TestApplyEmbeddingFailureToMetadata):
//
//   - AuditWarn row written with component="synthesis" and the canonical
//     phrase "synthesis succeeded but embedding failed".
//   - metadata["embedding_error"] = embedErr.Error() (verbatim provider text).
//   - metadata["embedding_status"] = "unavailable" (canonical sentinel,
//     matches the MCP save_to_memory structured-response contract from
//     spec §4.4).
//
// Callers MUST pass a non-nil dm and a non-nil metadata map. nil values
// panic — these are programming errors, not runtime degradation paths.
func applyEmbeddingFailureToMetadata(dm CoreDB, metadata map[string]interface{}, embedErr error) {
	dm.LogAudit(AuditWarn, "synthesis", fmt.Sprintf("synthesis succeeded but embedding failed: %v", embedErr), "", AuditContext{})
	metadata["embedding_error"] = embedErr.Error()
	metadata["embedding_status"] = "unavailable"
}
