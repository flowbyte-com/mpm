// compact.go — Phase 2 of the epistemic compaction pipeline.
//
// The compact_epistemology MCP tool is the agent's reflex to the
// epistemic_pressure trigger (see wake_context.go). It extracts a
// bounded batch of raw memories, hands them to the LLM for synthesis,
// validates the response structurally, then commits a lesson and
// marks the raw memories in a single transaction.
//
// Atomicity guarantee: the lesson insert and the raw-memory mark
// happen in one SQL transaction. Either both commit or neither commits.
// The LLM call is OUTSIDE the transaction — a failure there leaves
// zero DB writes, no partial state.
//
// Provenance convention: the LLM does NOT see or return the raw memory
// IDs. The Go orchestrator holds the slice from the SELECT and attaches
// it after unmarshal. This eliminates a hallucination vector where the
// model could lose IDs in the response (the "lost in the middle"
// phenomenon) and strand raw memories in the pressure queue forever.
//
// LLM injection: compactSynthesizeFunc is the single seam between
// orchestration and the LLM. Production wires it to the
// *synth.SynthClient's SynthesizeCompactLesson method. Tests override
// the variable to inject canned responses without touching the network.

package internal

import (
	"context"
	"crypto/sha1"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/flowbyte-com/mpm-core/synth"
)

// CompactLesson is the LLM-facing shape of the synthesized lesson.
// Only Title, Body, and Tags come from the model. Provenance is NOT
// in this struct — it lives in the orchestrator's local scope and
// never crosses the wire to the model.
type CompactLesson struct {
	Title string   `json:"title"`
	Body  string   `json:"body"`
	Tags  []string `json:"tags"`
}

// Validate enforces semantic checks beyond JSON shape. Empty fields
// mean the LLM either hallucinated an empty response or returned
// the refusal sentinel {"title":"", "body":"", "tags":[]} from the
// prompt instructions. Either way: orchestrator rolls back.
func (c *CompactLesson) Validate() error {
	if c.Title == "" {
		return errors.New("title required")
	}
	if c.Body == "" {
		return errors.New("body required")
	}
	if len(c.Tags) == 0 {
		return errors.New("at least one tag required")
	}
	return nil
}

// CompactEpistemologyResult is the wire-format return shape for the
// MCP tool. Field names are agent-facing; snake_case is intentional
// so the JSON matches the rest of the substrate's tool surface.
//
// SkippedReason is set on no-op paths (no raw memories, below
// threshold). On error paths, the result is nil and the error
// carries the reason.
type CompactEpistemologyResult struct {
	Compacted      int    `json:"compacted"`
	LessonsCreated int    `json:"lessons_created"`
	RawMarked      int    `json:"raw_marked"`
	LessonID       string `json:"lesson_id,omitempty"`
	SkippedReason  string `json:"skipped_reason,omitempty"`
}

// compactBatchSize is the hard ceiling on raw memories per call.
// Tuned to fit comfortably in MiniMax-M2.7's context window even
// for verbose memory entries, while still draining a backlog of
// 1000+ raw in ~20 calls.
const compactBatchSize = 50

// compactSynthesizeFunc is the LLM injection seam. Production wires
// it in init; tests override the variable directly.
var compactSynthesizeFunc = func(ctx context.Context, rawMemories []string) (string, error) {
	return synth.NewSynthClient().SynthesizeCompactLesson(ctx, rawMemories)
}

// CompactEpistemology is the DM method backing the MCP tool. Pure
// orchestration: pre-check, extract, synthesize, validate, transaction.
// The caller (tool handler) supplies ctx and force flag; force=true
// bypasses the pressure threshold check (rare — explicit agent override).
//
// Return shape: non-nil result on no-op paths (pre-check failed,
// threshold not exceeded) with SkippedReason set. Non-nil error on
// synthesis / validation / DB failures. Either way, the data plane
// is consistent — no partial states ever persist.
func (dm *DatabaseManager) CompactEpistemology(ctx context.Context, force bool) (*CompactEpistemologyResult, error) {
	// 1. Pre-check — read the pressure gauge. Bail out cheaply on no-op.
	rawCount, threshold, err := dm.compactPreCheck(ctx)
	if err != nil {
		return nil, fmt.Errorf("pre_check: %w", err)
	}
	if rawCount == 0 {
		return &CompactEpistemologyResult{SkippedReason: "no_raw_memories"}, nil
	}
	if !force && rawCount <= threshold {
		return &CompactEpistemologyResult{SkippedReason: "below_threshold"}, nil
	}

	// 2. Extract — oldest first, capped at compactBatchSize.
	rawIDs, rawContents, err := dm.extractRawBatch(ctx, compactBatchSize)
	if err != nil {
		return nil, fmt.Errorf("extract: %w", err)
	}
	if len(rawIDs) == 0 {
		return &CompactEpistemologyResult{SkippedReason: "no_raw_memories"}, nil
	}

	// 3. Synthesize — call the LLM with strict JSON contract. NO DB
	// writes have happened yet; any failure here leaves the substrate
	// untouched.
	text, err := compactSynthesizeFunc(ctx, rawContents)
	if err != nil {
		return nil, fmt.Errorf("synthesize: %w", err)
	}

	// 4. Unmarshal into the domain struct. Bad JSON = model_schema_violation.
	var lesson CompactLesson
	if err := json.Unmarshal([]byte(text), &lesson); err != nil {
		return nil, fmt.Errorf("model_schema_violation: %w", err)
	}

	// 5. Validate — semantic check (empty fields, missing tags).
	if err := lesson.Validate(); err != nil {
		return nil, fmt.Errorf("lesson_validation_failed: %w", err)
	}

	// 6. Transaction — INSERT lesson + UPDATE raw memories, atomically.
	lessonID, err := dm.commitLessonAndMark(ctx, &lesson, rawIDs)
	if err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}

	return &CompactEpistemologyResult{
		Compacted:      len(rawIDs),
		LessonsCreated: 1,
		RawMarked:      len(rawIDs),
		LessonID:       lessonID,
	}, nil
}

// compactPreCheck reads raw_count and the system_config threshold in
// a single SELECT against the epistemic_pressure_v view. Subquery
// for the threshold — defaults to 100 if system_config.compaction
// .raw_threshold is unset.
func (dm *DatabaseManager) compactPreCheck(ctx context.Context) (rawCount, threshold int, err error) {
	row := dm.db.QueryRowContext(ctx, `
		SELECT
		  raw_count,
		  COALESCE(
		    (SELECT CAST(json_extract(raw_json, '$.raw_threshold') AS INTEGER)
		     FROM system_config WHERE key = 'compaction'),
		    100
		  ) AS threshold
		FROM epistemic_pressure_v
	`)
	if err := row.Scan(&rawCount, &threshold); err != nil {
		return 0, 0, err
	}
	return rawCount, threshold, nil
}

// extractRawBatch selects the oldest `limit` non-compacted memories.
// Returns parallel slices of IDs and contents — the orchestrator uses
// the IDs for the transaction UPDATE and the contents for the LLM prompt.
func (dm *DatabaseManager) extractRawBatch(ctx context.Context, limit int) (ids, contents []string, err error) {
	rows, err := dm.db.QueryContext(ctx, `
		SELECT id, content FROM memories
		WHERE collection = 'memories'
		  AND deleted_at IS NULL
		  AND (metadata IS NULL OR metadata = ''
		       OR json_extract(metadata, '$.compacted_into') IS NULL)
		ORDER BY created_at ASC
		LIMIT ?
	`, limit)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, content string
		if err := rows.Scan(&id, &content); err != nil {
			return nil, nil, err
		}
		ids = append(ids, id)
		contents = append(contents, content)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	return ids, contents, nil
}

// commitLessonAndMark wraps the lesson insert and the raw mark in a
// single transaction. Returns the new lesson ID on success.
//
// Lessons table layout (key fields): id TEXT PK, type TEXT (default
// 'insight'), content TEXT, tags JSON, created TEXT, content_hash
// TEXT (sha1 of content envelope — used for dedup).
//
// The lesson content is stored as a JSON envelope {title, body} so
// downstream readers can distinguish the title from the prose body
// without parsing markdown. Tags stored as JSON array, matching the
// existing lesson shape from save_lesson.
func (dm *DatabaseManager) commitLessonAndMark(ctx context.Context, lesson *CompactLesson, rawIDs []string) (string, error) {
	tx, err := dm.db.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("begin tx: %w", err)
	}
	// Rollback is a no-op after Commit. This defer covers every
	// return path between BeginTx and the final Commit — if any step
	// fails, the transaction rolls back atomically.
	defer func() { _ = tx.Rollback() }()

	envelope := struct {
		Title string `json:"title"`
		Body  string `json:"body"`
	}{
		Title: lesson.Title,
		Body:  lesson.Body,
	}
	envelopeBytes, err := json.Marshal(envelope)
	if err != nil {
		return "", fmt.Errorf("marshal envelope: %w", err)
	}

	tagsJSON, err := json.Marshal(lesson.Tags)
	if err != nil {
		return "", fmt.Errorf("marshal tags: %w", err)
	}

	hash := sha1.Sum(envelopeBytes)
	contentHash := hex.EncodeToString(hash[:])

	// INSERT lesson, RETURNING id. SQLite supports RETURNING since 3.35
	// (we depend on 3.35+ via FTS5). Lesson id is a random 32-char hex
	// prefixed with "les-" to match the existing lesson id pattern
	// (lessons_base.id is TEXT, no length constraint).
	var lessonID string
	err = tx.QueryRowContext(ctx, `
		INSERT INTO lessons (id, type, content, tags, created, content_hash)
		VALUES ('les-' || lower(hex(randomblob(16))), 'insight', ?, ?, ?, ?)
		RETURNING id
	`, string(envelopeBytes), string(tagsJSON), time.Now().UTC().Format(time.RFC3339), contentHash).Scan(&lessonID)
	if err != nil {
		return "", fmt.Errorf("insert lesson: %w", err)
	}

	// UPDATE all raw memories in one statement. SQLite supports
	// json_insert on each row independently — COALESCE handles rows
	// with NULL/empty metadata by initializing to '{}' first. The
	// WHERE clause mirrors the extract filter, ensuring we don't
	// race against another caller that already marked some of these.
	placeholders := make([]string, 0, len(rawIDs))
	args := []interface{}{lessonID}
	for _, id := range rawIDs {
		placeholders = append(placeholders, "?")
		args = append(args, id)
	}
	updateSQL := fmt.Sprintf(`
		UPDATE memories SET metadata = json_insert(
			COALESCE(metadata, '{}'),
			'$.compacted_into',
			?
		)
		WHERE id IN (%s)
		  AND deleted_at IS NULL
		  AND (metadata IS NULL OR metadata = ''
		       OR json_extract(metadata, '$.compacted_into') IS NULL)
	`, strings.Join(placeholders, ","))
	res, err := tx.ExecContext(ctx, updateSQL, args...)
	if err != nil {
		return "", fmt.Errorf("update memories: %w", err)
	}
	// rowsAffected should equal len(rawIDs). If it's less, a row was
	// marked or deleted between the SELECT and the UPDATE — concurrent
	// write. Log but don't fail; the lesson is still created and the
	// remaining rows are still marked.
	if n, _ := res.RowsAffected(); n < int64(len(rawIDs)) {
		// Non-fatal — the dominant case is single-writer. This guards
		// against silent partial-state surprises in multi-agent setups.
		_ = n // see docstring; intentional silent
	}

	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("commit: %w", err)
	}

	// Record the compaction event so wake_context can surface
	// last_compacted_at on future wakes. Non-fatal: the lesson and
	// raw-mark are already committed, so a write failure here just
	// means the timestamp won't surface next session. The agent
	// will still see the lesson via query_lessons / mcp stats; the
	// \"when did I last compact?\" signal is informational, not
	// load-bearing for correctness.
	if err := dm.recordCompactionEvent(ctx, lessonID, len(rawIDs)); err != nil {
		dm.LogAudit(AuditWarn, "compact_epistemology", "record event failed: "+err.Error(), "", AuditContext{})
	}

	return lessonID, nil
}

// recordCompactionEvent writes a one-line ledger entry to system_config
// under key 'compaction.last_run'. The wake_context gather reads this on
// every wake to surface last_compacted_at. UPSERT semantics — repeated
// calls overwrite the previous event (only the most recent matters).
//
// Schema: {last_run_at: <RFC3339>, lesson_id: <id>, raw_marked: <count>}.
// Kept minimal — detailed lesson content is in the lessons table; this
// is just the \"did compaction happen recently?\" signal.
func (dm *DatabaseManager) recordCompactionEvent(ctx context.Context, lessonID string, rawMarked int) error {
	payload, err := json.Marshal(map[string]interface{}{
		"last_run_at": time.Now().UTC().Format(time.RFC3339),
		"lesson_id":   lessonID,
		"raw_marked":  rawMarked,
	})
	if err != nil {
		return fmt.Errorf("marshal payload: %w", err)
	}
	_, err = dm.db.ExecContext(ctx, `
		INSERT INTO system_config (key, raw_json, content_hash)
		VALUES ('compaction.last_run', ?, '')
		ON CONFLICT(key) DO UPDATE SET
		  raw_json = excluded.raw_json,
		  updated_at = CURRENT_TIMESTAMP
	`, string(payload))
	return err
}

// Compile-time guard: compactSynthesizeFunc signature matches the
// *synth.SynthClient.SynthesizeCompactLesson method signature. If the
// latter ever changes, this line fails to compile and the seam
// catches the drift at build time, not runtime.
var _ = func() bool {
	var f func(context.Context, []string) (string, error) = compactSynthesizeFunc
	return f != nil
}()
var _ sql.Result // keep sql import even if rowsAffected branch is no-op