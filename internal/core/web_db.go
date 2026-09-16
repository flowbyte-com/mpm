package internal

import (
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"
)

// isUniqueConstraintError reports whether err is a SQLite UNIQUE/PRIMARY KEY
// constraint violation. The modernc.org/sqlite driver returns these errors
// with the message "constraint failed: UNIQUE constraint failed: <col>" or
// "constraint failed: PRIMARY KEY constraint failed: <col>". A substring
// match is sufficient — we only need to distinguish "already exists" from
// other write failures for the AddReference error path.

// Memory read-path SQL fragments.
//
// MemoryExpireClause filters out rows whose expires_at is in the past.
// Always include this in any SELECT against `memories` that surfaces user
// data — SetMemoryTTL sets the flag but enforcement lives in the read paths.
// (PruneExpired is the bulk delete; this is the per-row filter.)
//
// expires_at is stored as a Unix timestamp (REAL) — see SetMemoryTTL. The
// clause uses strftime('%s','now') so the comparison is numeric; comparing
// against CURRENT_TIMESTAMP (a string) leaks expired rows because SQLite
// does lexicographic comparison and 'T' > ' ' in ASCII. All comparisons
// against expires_at MUST use strftime('%s','now'), not CURRENT_TIMESTAMP.
//
// deleted_at follows the same convention as expires_at — INTEGER Unix
// epoch. The two are unified so SQLite comparisons are always numeric and
// never mix types (INTEGER < TEXT is always TRUE in SQLite, which would
// shred migrated rows on rollback — see audit.md 2026-07-23 finding 1
// for the rollback procedure).
//
// MemoryExpireClauseM is the alias-qualified variant for queries that
// SELECT FROM `memories m` (e.g. FTS5 joins).
const (
	MemoryExpireClause  = " AND (expires_at IS NULL OR expires_at > strftime('%s','now'))"
	MemoryExpireClauseM = " AND (m.expires_at IS NULL OR m.expires_at > strftime('%s','now'))"
)

// ==================== Lightweight reference types for cross-ref display ====================

// TopicRef is a lightweight topic reference for cross-reference display
type TopicRef struct {
	ID   string
	Name string
	Role string // "manual", "auto", "related"
}

// ReferenceDocRef is a lightweight reference doc reference
type ReferenceDocRef struct {
	ID       string
	Title    string
	FilePath string
}

// MemoryRef is a lightweight memory reference (truncated content for lists)
type MemoryRef struct {
	ID         string
	Content    string
	Collection string
	Weight     int
}

// ==================== Memory queries (for web UI) ====================

// QueryMemories returns memories matching the criteria
func (dm *DatabaseManager) QueryMemories(collection string, primeOnly bool, limit, offset int) ([]map[string]interface{}, error) {
	if limit <= 0 {
		limit = 50
	}

	// Alpha-4 ledger audit D-001 fix: QueryMemories was the lone memory
	// read path that omitted `deleted_at IS NULL`. Every other read
	// surface (GetMemory, SearchMemories, HybridSearch, GetMemoriesForExport,
	// recentMemories, recentMilestones) filters soft-deleted rows. Without
	// this predicate the web UI returned 90 phantom memories on the live
	// DB; CLI/MCP would never surface those rows. The MemoryExpireClause
	// is applied below for the same parity reason.
	query := `SELECT id, collection, content, session_id, tags, metadata, created_at, source_db, source_id, promoted_at FROM memories WHERE deleted_at IS NULL`
	args := []interface{}{}

	if collection != "" {
		query += " AND collection = ?"
		args = append(args, collection)
	}

	if primeOnly {
		query += " AND (metadata LIKE '%is_prime_directive%' OR tags LIKE '%is_prime_directive%' OR collection = 'directives')"
	}

	// Apply expiry parity with the rest of the read surface. Without
	// this, an expired-but-not-deleted memory appears in QueryMemories
	// but is invisible to HybridSearch/SearchMemories/GetMemory — same
	// population-divergence class as the missing deleted_at filter.
	query += MemoryExpireClause

	query += " ORDER BY created_at DESC LIMIT ? OFFSET ?"
	args = append(args, limit, offset)

	rows, err := dm.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var mems []map[string]interface{}
	for rows.Next() {
		var id, collection, content, tagsJSON, metadataJSON, createdAt string
		var sessionID, sourceDB, sourceID *string
		var promotedAt *float64

		if err := rows.Scan(&id, &collection, &content, &sessionID, &tagsJSON, &metadataJSON, &createdAt, &sourceDB, &sourceID, &promotedAt); err != nil {
			return nil, fmt.Errorf("scanning query memory row: %w", err)
		}

		m := map[string]interface{}{
			"id":         id,
			"collection": collection,
			"content":    content,
			"tags":       tagsJSON,
			"metadata":   metadataJSON,
			"created_at": createdAt,
		}
		if sessionID != nil {
			m["session_id"] = *sessionID
		}
		if sourceDB != nil {
			m["source_db"] = *sourceDB
		}
		if sourceID != nil {
			m["source_id"] = *sourceID
		}
		if promotedAt != nil {
			m["promoted_at"] = *promotedAt
		}
		mems = append(mems, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return mems, nil
}

// SearchMemories searches memories using FTS5 or LIKE fallback
func (dm *DatabaseManager) SearchMemories(q, collection string, primeOnly bool, limit, offset int) ([]map[string]interface{}, error) {
	// 2026-09-05 audit remediation pass 2: the previous shape coerced
	// `limit <= 0` to 50. The public handlers now validate limit via
	// parseLimitStrict before calling this method, so any value
	// reaching here is intentional (0 → 0, negative was already
	// rejected upstream). Internal callers must pass an explicit
	// limit.

	escaped := strings.ReplaceAll(q, "\"", "\"\"")
	ftsQuery := "\"" + escaped + "\"*"

	// Try FTS5 first
	mems := []map[string]interface{}{}
	found := false
	if testRows, err := dm.db.Query(`SELECT 1 FROM memories_fts WHERE memories_fts MATCH ? LIMIT 1`, ftsQuery); err == nil && testRows != nil {
		defer testRows.Close()
		if testRows.Next() {
			found = true
		}
	}

	var query string
	var args []interface{}

	if found {
		query = `SELECT m.id, m.collection, m.content, m.session_id, m.tags, m.metadata, m.created_at, m.source_db, m.source_id, m.promoted_at FROM memories m JOIN memories_fts f ON m.rowid = f.rowid WHERE memories_fts MATCH ? AND m.deleted_at IS NULL` + MemoryExpireClauseM
		args = []interface{}{ftsQuery}
	} else {
		query = `SELECT id, collection, content, session_id, tags, metadata, created_at, source_db, source_id, promoted_at FROM memories WHERE deleted_at IS NULL AND content LIKE ?` + MemoryExpireClause
		args = []interface{}{"%" + q + "%"}
	}

	if collection != "" {
		if found {
			query += " AND m.collection = ?"
		} else {
			query += " AND collection = ?"
		}
		args = append(args, collection)
	}

	if primeOnly {
		if found {
			query += " AND (m.metadata LIKE '%is_prime_directive%' OR m.tags LIKE '%is_prime_directive%' OR m.collection = 'directives')"
		} else {
			query += " AND (metadata LIKE '%is_prime_directive%' OR tags LIKE '%is_prime_directive%' OR collection = 'directives')"
		}
	}

	if found {
		query += " ORDER BY rank LIMIT ? OFFSET ?"
	} else {
		query += " ORDER BY created_at DESC LIMIT ? OFFSET ?"
	}
	args = append(args, limit, offset)

	rows, err := dm.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var id, coll, content, tagsJSON, metadataJSON, createdAt string
		var sessionID, sourceDB, sourceID *string
		var promotedAt *float64

		if err := rows.Scan(&id, &coll, &content, &sessionID, &tagsJSON, &metadataJSON, &createdAt, &sourceDB, &sourceID, &promotedAt); err != nil {
			return nil, fmt.Errorf("scanning search memory row: %w", err)
		}

		m := map[string]interface{}{
			"id":         id,
			"collection": coll,
			"content":    content,
			"tags":       tagsJSON,
			"metadata":   metadataJSON,
			"created_at": createdAt,
		}
		if sessionID != nil {
			m["session_id"] = *sessionID
		}
		if sourceDB != nil {
			m["source_db"] = *sourceDB
		}
		if sourceID != nil {
			m["source_id"] = *sourceID
		}
		if promotedAt != nil {
			m["promoted_at"] = *promotedAt
		}
		mems = append(mems, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return mems, nil
}

// GetMemory returns a single memory by ID. NULL tags (which can occur
// if a write path bypasses AddMemory's tags validation) are normalized
// to "" so the returned map's "tags" value is always a string. Same
// treatment for metadata and the nullable pointer columns: callers
// can rely on type-stable access (string-or-empty) without per-call
// nil checks. Production never sees this branch because every write
// path populates tags; the normalization is here for safety against
// future write paths and for tests that construct rows directly.
func (dm *DatabaseManager) GetMemory(id string) (map[string]interface{}, error) {
	var collection, content, createdAt string
	var tagsNS, metadataNS sql.NullString
	var sessionID, sourceDB, sourceID *string
	var promotedAt *float64
	var weight float64
	var confidence sql.NullFloat64

	// BEGIN IMMEDIATE ensures a consistent read snapshot. Without a transaction,
	// a concurrent GC batch UPDATE commit can cause GetMemory to observe a
	// partially-applied weight change. IMMEDIATE acquires a write lock at start
	// but GetMemory itself is read-only, so this only prevents the GC batch
	// from committing mid-scan — it does not block concurrent reads.
	tx, err := dm.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	err = tx.QueryRow(`
		SELECT collection, content, session_id, tags, metadata, created_at, weight, confidence, source_db, source_id, promoted_at
		FROM memories WHERE id = ? AND deleted_at IS NULL`+MemoryExpireClause+`
	    `, id).Scan(&collection, &content, &sessionID, &tagsNS, &metadataNS, &createdAt, &weight, &confidence, &sourceDB, &sourceID, &promotedAt)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("memory not found: %s", id)
	}
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}

	tags := ""
	if tagsNS.Valid {
		tags = tagsNS.String
	}
	metadata := ""
	if metadataNS.Valid {
		metadata = metadataNS.String
	}

	m := map[string]interface{}{
		"id":         id,
		"collection": collection,
		"content":    content,
		"tags":       tags,
		"metadata":   metadata,
		"created_at": createdAt,
		"weight":     int(weight),
	}
	if confidence.Valid {
		m["confidence"] = confidence.Float64
	}
	if sessionID != nil {
		m["session_id"] = *sessionID
	}
	if sourceDB != nil {
		m["source_db"] = *sourceDB
	}
	if sourceID != nil {
		m["source_id"] = *sourceID
	}
	if promotedAt != nil {
		m["promoted_at"] = *promotedAt
	}
	return m, nil
}

// GetMemoryByExternalID returns a memory by its source_db + source_id combination.
// Used for deduplication when polling external databases.
func (dm *DatabaseManager) GetMemoryByExternalID(sourceDB, sourceID string) (map[string]interface{}, error) {
	var id string
	var collection, content, tagsJSON, metadataJSON, createdAt string
	var sessionID *string
	var promotedAt *float64

	err := dm.db.QueryRow(`
		SELECT id, collection, content, session_id, tags, metadata, created_at, promoted_at
		FROM memories WHERE source_db = ? AND source_id = ?
	`, sourceDB, sourceID).Scan(&id, &collection, &content, &sessionID, &tagsJSON, &metadataJSON, &createdAt, &promotedAt)
	if err != nil {
		return nil, err
	}

	m := map[string]interface{}{
		"id":         id,
		"collection": collection,
		"content":    content,
		"tags":       tagsJSON,
		"metadata":   metadataJSON,
		"created_at": createdAt,
	}
	if sessionID != nil {
		m["session_id"] = *sessionID
	}
	if promotedAt != nil {
		m["promoted_at"] = *promotedAt
	}
	return m, nil
}

// GetMemoryTopics returns all topics linked to a memory, ordered by role DESC (manual first) then name
func (dm *DatabaseManager) GetMemoryTopics(memoryID string) ([]TopicRef, error) {
	rows, err := dm.db.Query(`
		SELECT t.id, t.name, tm.role
		FROM topic_memberships tm
		JOIN topics t ON t.id = tm.topic_id
		WHERE tm.memory_id = ?
		ORDER BY tm.role DESC, t.name
	`, memoryID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var refs []TopicRef
	for rows.Next() {
		var r TopicRef
		if err := rows.Scan(&r.ID, &r.Name, &r.Role); err != nil {
			// Diagnostic — skip this row but keep going.
			dm.LogAudit(AuditWarn, "web_db", fmt.Sprintf("GetMemoryTopics: scan failed, skipping row: %v", err), "", AuditContext{"memory_id": memoryID})
			continue
		}
		refs = append(refs, r)
	}
	if refs == nil {
		refs = []TopicRef{}
	}
	return refs, rows.Err()
}

// GetReferenceDoc returns a reference doc by ID (lightweight, no chunks)
func (dm *DatabaseManager) GetReferenceDoc(docID string) (*ReferenceDocRef, error) {
	if docID == "" {
		return nil, nil
	}
	var title, filePath string
	err := dm.db.QueryRow(`SELECT title, file_path FROM reference_docs WHERE id = ?`, docID).Scan(&title, &filePath)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &ReferenceDocRef{ID: docID, Title: title, FilePath: filePath}, nil
}

// GetTopicTopMemories returns top-N memories for a topic (by weight DESC, created_at DESC)
// plus the total count of all linked memories (for the count chip).
// The content field is truncated to 120 chars for list display.
func (dm *DatabaseManager) GetTopicTopMemories(topicID string, limit int) ([]MemoryRef, int, error) {
	if limit <= 0 {
		limit = 3
	}

	rows, err := dm.db.Query(`
		SELECT m.id, m.content, m.collection, m.weight
		FROM topic_memberships tm
		JOIN memories m ON m.id = tm.memory_id
		WHERE tm.topic_id = ? AND tm.memory_id IS NOT NULL AND m.deleted_at IS NULL
		ORDER BY m.weight DESC, m.created_at DESC
		LIMIT ?
	`, topicID, limit)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var refs []MemoryRef
	for rows.Next() {
		var r MemoryRef
		var content string
		var weight float64
		if err := rows.Scan(&r.ID, &content, &r.Collection, &weight); err != nil {
			// Diagnostic — skip this row but keep going.
			dm.LogAudit(AuditWarn, "web_db", fmt.Sprintf("GetTopicTopMemories: scan failed, skipping row: %v", err), "", AuditContext{"topic_id": topicID})
			continue
		}
		r.Weight = int(weight)
		if len(content) > 120 {
			r.Content = content[:120] + "…"
		} else {
			r.Content = content
		}
		refs = append(refs, r)
	}
	if refs == nil {
		refs = []MemoryRef{}
	}

	var total int
	if err := dm.db.QueryRow(`SELECT COUNT(*) FROM topic_memberships WHERE topic_id = ? AND memory_id IS NOT NULL`, topicID).Scan(&total); err != nil {
		dm.LogAudit(AuditWarn, "web_db", fmt.Sprintf("GetTopicTopMemories: total count failed, defaulting to 0: %v", err), "", AuditContext{"topic_id": topicID})
	}

	return refs, total, rows.Err()
}

// UpdateMemory updates an existing memory's content and metadata.
// Errors with ErrMemoryNotFound-equivalent when the id doesn't exist —
// a silent 0-row success would let callers believe a typo'd id was
// updated (Defense Triad rule 3: verify persistence).
//
// Embedding contract on update:
//   - When EmbedText succeeds, the new embedding is stored.
//   - When EmbedText fails (provider error), the new embedding is
//     NOT stored (preserves any prior valid embedding rather than
//     overwriting with NULL) and the failure is surfaced as an
//     AuditWarn. This mirrors the AddMemory "embedding failure must
//     not become a silent semantic degradation" rule.
//   - When EmbedText returns (nil, nil) because the embedding
//     provider is absent/disabled (NullProvider), the embedding column
//     is preserved as-is (NOT overwritten with the literal string
//     "null") so a previously-stored embedding survives.
func (dm *DatabaseManager) UpdateMemory(id, content string, tags map[string]interface{}, metadata map[string]interface{}) error {
	tagsJSON, _ := json.Marshal(tags)
	metadataJSON, _ := json.Marshal(metadata)
	embedding, embedErr := EmbedText(content)
	if embedErr != nil {
		// Provider error: do NOT overwrite the embedding column. Log
		// to audit so the failure is visible without blocking the
		// durable update of content/tags/metadata.
		dm.LogAudit(AuditWarn, "memory", fmt.Sprintf("UpdateMemory: embedding failed, prior embedding preserved (memory_id=%s): %v", id, embedErr), "", AuditContext{})
		embedding = nil
	}
	contentHash := fmt.Sprintf("%x", sha256.Sum256([]byte(content)))

	// If we have a fresh embedding, write it. Otherwise preserve the
	// existing embedding column by NOT including it in the UPDATE.
	// This protects against:
	//   (a) provider error — see above, audit logged
	//   (b) NullProvider (absent/disabled) — embedding column preserved
	var res sql.Result
	var err error
	if embedding != nil {
		embeddingJSON, _ := json.Marshal(embedding)
		res, err = dm.db.Exec(`
			UPDATE memories
			SET content = ?, tags = ?, metadata = ?, embedding = ?, content_hash = ?,
			    updated_at = CAST(strftime('%s','now') AS INTEGER)
			WHERE id = ?
		`, content, string(tagsJSON), string(metadataJSON), string(embeddingJSON), contentHash, id)
	} else {
		res, err = dm.db.Exec(`
			UPDATE memories
			SET content = ?, tags = ?, metadata = ?, content_hash = ?,
			    updated_at = CAST(strftime('%s','now') AS INTEGER)
			WHERE id = ?
		`, content, string(tagsJSON), string(metadataJSON), contentHash, id)
	}
	if err != nil {
		return err
	}
	if affected, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("update memory rows-affected: %w", err)
	} else if affected == 0 {
		return fmt.Errorf("update memory: no row with id %s (not found, deleted, or expired)", id)
	}
	return nil
}

// ReinforceMemory increments the reinforcement count and weight.
// Errors when the id matches no row (no silent no-op success).
//
// Atomicity (alpha remediation 2026-08-27): the mutation is a single
// SQL UPDATE so SQLite WAL mode serializes concurrent writers — no
// read-modify-write window exists at the database boundary. The
// previous implementation already used single-statement atomic SQL,
// but the validation run observed a stale final weight under
// contention; this revision keeps the atomic shape and adds the
// Defense Triad rule 3 read-back assertion (post-update, not within
// the mutation statement) so a silent-promotion class failure cannot
// pass through to the caller. The arithmetic invariant — `weight` and
// `reinforcement_count` advance by exactly `weightGain` and `delta`
// modulo the clamping rules — is now provable from the read-back,
// not just inferred from the absence of an error.
func (dm *DatabaseManager) ReinforceMemory(id string, delta int) error {
	if delta <= 0 {
		delta = 1
	}
	weightGain := (delta + 1) / 2
	if weightGain == 0 {
		weightGain = 1
	}

	res, err := dm.db.Exec(`
		UPDATE memories
		SET reinforcement_count = reinforcement_count + ?, weight = MIN(weight + ?, 100),
		    last_accessed_at = CAST(strftime('%s','now') AS INTEGER), runtime_seconds_since_access = 0, runtime_last_accrued_at = CAST(strftime('%s','now') AS INTEGER)
		WHERE id = ? AND deleted_at IS NULL
	`, delta, weightGain, id)
	if err != nil {
		if isBusyError(err) {
			// Surface the contention signal rather than swallowing
			// it; the caller's retry budget decides what to do.
			return fmt.Errorf("reinforce memory: write contended: %w", err)
		}
		return fmt.Errorf("reinforce memory: %w", err)
	}
	if affected, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("reinforce memory rows-affected: %w", err)
	} else if affected == 0 {
		return fmt.Errorf("reinforce memory: no row with id %s (not found, deleted, or expired)", id)
	}

	// Defense Triad rule 3: read-back proves persistence. The SQL
	// UPDATE above is already atomic at the SQLite layer; this
	// read-back exists to surface the silent-promotion class (a
	// trigger swallowing the UPDATE, a view with INSTEAD OF, etc.)
	// that `RowsAffected()` alone could miss. Under concurrent
	// weaken we cannot pin a precise post-state here, but we can
	// still confirm the row is on disk and within the model bounds.
	//
	// weight is REAL — fractional values are valid (post-fix T27;
	// pre-fix this scanned into int which panicked on any non-integer
	// value). reinforcement_count is INTEGER.
	var postWeight float64
	var postReinf int
	if err := dm.db.QueryRow(
		`SELECT weight, reinforcement_count FROM memories WHERE id = ?`, id,
	).Scan(&postWeight, &postReinf); err != nil {
		return fmt.Errorf("reinforce memory: post-update read-back: %w", err)
	}
	if postWeight < 0 || postWeight > 100 {
		return fmt.Errorf("reinforce memory: read-back weight out of bounds (%g)", postWeight)
	}
	if postReinf < 0 {
		return fmt.Errorf("reinforce memory: read-back reinforcement_count negative (%d)", postReinf)
	}
	return nil
}

// AdjustMemoryWeight adjusts weight by delta with a hard floor of 1.
// Used by the +/- feedback shortcuts. Errors when the id matches no row.
func (dm *DatabaseManager) AdjustMemoryWeight(id string, delta int) error {
	res, err := dm.db.Exec(`
		UPDATE memories
		SET weight = MAX(weight + ?, 1),
		    last_accessed_at = CAST(strftime('%s','now') AS INTEGER), runtime_seconds_since_access = 0, runtime_last_accrued_at = CAST(strftime('%s','now') AS INTEGER)
		WHERE id = ?
	`, delta, id)
	if err != nil {
		return err
	}
	if affected, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("adjust memory weight rows-affected: %w", err)
	} else if affected == 0 {
		return fmt.Errorf("adjust memory weight: no row with id %s (not found, deleted, or expired)", id)
	}
	return nil
}

// ChallengeAndReinforce clears the challenged status (if any) and applies
// reinforcement in a single transaction. Used when +<id> hits a challenged memory.
func (dm *DatabaseManager) ChallengeAndReinforce(id string, delta int) error {
	if delta <= 0 {
		delta = 1
	}
	weightGain := (delta + 1) / 2
	if weightGain == 0 {
		weightGain = 1
	}
	tx, err := dm.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Fetch challenged_theory_id from memory metadata
	var metaStr string
	err = tx.QueryRow(`SELECT metadata FROM memories WHERE id = ?`, id).Scan(&metaStr)
	if err != nil {
		return err
	}
	var theoryID string
	if metaStr != "" {
		var meta map[string]interface{}
		if json.Unmarshal([]byte(metaStr), &meta) == nil {
			if t, ok := meta["challenged_theory_id"].(string); ok {
				theoryID = t
			}
		}
	}

	// 1. Resolve theory if present
	if theoryID != "" {
		resolvePatch := map[string]interface{}{"status": "disproven", "memory_id": nil}
		resolveJSON, _ := json.Marshal(resolvePatch)
		_, err = tx.Exec(
			`UPDATE memories SET metadata = json_patch(COALESCE(metadata,'{}'), ?) WHERE id = ?`,
			string(resolveJSON), theoryID)
		if err != nil {
			return err
		}
	}

	// 2. Clear challenged status from memory (including the recorded
	// pre-challenge weight — the user's +<id> is an explicit endorsement
	// that supersedes the challenge, so the challenge bookkeeping goes).
	clearPatch := map[string]interface{}{"status": nil, "challenged_theory_id": nil, "challenged_prior_weight": nil}
	clearJSON, _ := json.Marshal(clearPatch)
	_, err = tx.Exec(
		`UPDATE memories SET metadata = json_patch(COALESCE(metadata,'{}'), ?) WHERE id = ?`,
		string(clearJSON), id)
	if err != nil {
		return err
	}

	// 3. Apply reinforcement and bump last_accessed_at
	_, err = tx.Exec(`
		UPDATE memories
		SET reinforcement_count = reinforcement_count + ?,
		    weight = MIN(weight + ?, 100),
		    last_accessed_at = CAST(strftime('%s','now') AS INTEGER), runtime_seconds_since_access = 0, runtime_last_accrued_at = CAST(strftime('%s','now') AS INTEGER)
		WHERE id = ?
	`, delta, weightGain, id)
	if err != nil {
		return err
	}

	return tx.Commit()
}

// WeakenMemory decrements reinforcement count and reduces weight.
// Errors when the id matches no row (no silent no-op success).
//
// Weight floor contract (T24 2026-09-11): the floor is 1.0 across every
// weight-modifying primitive. AdjustMemoryWeight, WeakenMemoryTool,
// and the legacy_weight view all use MAX(weight - ..., 1); ReinforceMemory
// only adds weight so the floor doesn't apply, but a fractional starting
// weight could decay via the synthesis / lifecycle paths and still
// never cross below 1.0. The pre-fix WeakenMemory used MAX(weight - ?, 0),
// which let a large weaken pass produce weight=0.0 — a state that's
// incompatible with the scoring model (which assumes weight>=1) and
// downstream filters (e.g. embedding_migration.go's `weight < 1.0`
// check for synthetic memories).
//
// Atomicity (alpha remediation 2026-08-27): the mutation is a single
// SQL UPDATE so SQLite WAL mode serializes concurrent writers. The
// read-back assertion (Defense Triad rule 3) confirms the post-update
// state landed and surfaces the silent-promotion class of failures
// (trigger swallowing the UPDATE, view with INSTEAD OF, etc.).
func (dm *DatabaseManager) WeakenMemory(id string, delta int) error {
	if delta <= 0 {
		delta = 1
	}
	weightLoss := (delta + 1) / 2

	res, err := dm.db.Exec(`
		UPDATE memories
		SET reinforcement_count = MAX(reinforcement_count - ?, 0),
		    weight = MAX(weight - ?, 1),
		    last_accessed_at = CAST(strftime('%s','now') AS INTEGER), runtime_seconds_since_access = 0, runtime_last_accrued_at = CAST(strftime('%s','now') AS INTEGER)
		WHERE id = ? AND deleted_at IS NULL
	`, delta, weightLoss, id)
	if err != nil {
		if isBusyError(err) {
			return fmt.Errorf("weaken memory: write contended: %w", err)
		}
		return fmt.Errorf("weaken memory: %w", err)
	}
	if affected, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("weaken memory rows-affected: %w", err)
	} else if affected == 0 {
		return fmt.Errorf("weaken memory: no row with id %s (not found, deleted, or expired)", id)
	}

	// Defense Triad rule 3: read-back proves persistence.
	// weight is REAL — fractional values are valid (T27 fix).
	// Floor check mirrors the SQL MAX(weight - ?, 1) — the post-state
	// must land >= 1.0. A post-state < 1.0 means either the SQL floor
	// is misconfigured or an INSTEAD OF trigger swallowed the UPDATE.
	var postWeight float64
	var postReinf int
	if err := dm.db.QueryRow(
		`SELECT weight, reinforcement_count FROM memories WHERE id = ?`, id,
	).Scan(&postWeight, &postReinf); err != nil {
		return fmt.Errorf("weaken memory: post-update read-back: %w", err)
	}
	if postWeight < 1.0 {
		return fmt.Errorf("weaken memory: read-back weight below floor (%g)", postWeight)
	}
	if postReinf < 0 {
		return fmt.Errorf("weaken memory: read-back reinforcement_count out of bounds (%d)", postReinf)
	}
	return nil
}

// SetMemoryTTL sets an expiration time on a memory.
//
// expires_at is stored as a Unix timestamp (REAL) so it can be compared
// against CURRENT_TIMESTAMP in SQL. Do not switch to RFC3339 strings
// here without also updating every read-path clause — SQLite will
// lexicographically compare the two and the filter will leak expired
// rows (T > space in ASCII).
func (dm *DatabaseManager) SetMemoryTTL(id string, expiresAt time.Time) error {
	var res sql.Result
	var err error
	if expiresAt.IsZero() {
		res, err = dm.db.Exec(`UPDATE memories SET expires_at = NULL WHERE id = ?`, id)
	} else {
		res, err = dm.db.Exec(`
			UPDATE memories SET expires_at = ? WHERE id = ?
		`, expiresAt.Unix(), id)
	}
	if err != nil {
		return err
	}
	if affected, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("set memory ttl rows-affected: %w", err)
	} else if affected == 0 {
		return fmt.Errorf("set memory ttl: no row with id %s (not found, deleted, or expired)", id)
	}
	return nil
}

// PruneExpired removes memories that have passed their expires_at time.
func (dm *DatabaseManager) PruneExpired() (int, error) {
	result, err := dm.db.Exec(`
		DELETE FROM memories WHERE expires_at IS NOT NULL AND expires_at < strftime('%s','now')
	`)
	if err != nil {
		return 0, err
	}
	rows, _ := result.RowsAffected()
	return int(rows), nil
}

// GetMemoriesByRelevance returns memories ordered by composite relevance score.
func (dm *DatabaseManager) GetMemoriesByRelevance(collection string, limit int) ([]map[string]interface{}, error) {
	if limit <= 0 {
		limit = 20
	}

	query := `
		SELECT id, collection, content, session_id, tags, metadata, created_at,
		       COALESCE(reinforcement_count, 0) as reinforcement_count,
		       COALESCE(weight, 1) as weight,
		       last_accessed_at, expires_at
		FROM memories
		WHERE deleted_at IS NULL
		  AND (expires_at IS NULL OR expires_at > strftime('%s','now'))
		  AND (? = '' OR collection = ?)
		ORDER BY (COALESCE(reinforcement_count, 0) * 2) + (COALESCE(weight, 1) * 1.5) DESC,
		         COALESCE(last_accessed_at, created_at) DESC
		LIMIT ?
	`

	rows, err := dm.db.Query(query, collection, collection, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []map[string]interface{}
	for rows.Next() {
		var id, collection, content, tagsJSON, metadataJSON, createdAt string
		var sessionID *string
		var reinforcementCount int
		var weight float64
		var lastAccessedAt, expiresAt *string

		err := rows.Scan(&id, &collection, &content, &sessionID, &tagsJSON, &metadataJSON,
			&createdAt, &reinforcementCount, &weight, &lastAccessedAt, &expiresAt)
		if err != nil {
			return nil, fmt.Errorf("scanning relevance memory row: %w", err)
		}

		m := map[string]interface{}{
			"id":                  id,
			"collection":          collection,
			"content":             content,
			"tags":                tagsJSON,
			"metadata":            metadataJSON,
			"created_at":          createdAt,
			"reinforcement_count": reinforcementCount,
			"weight":              int(weight),
		}
		if sessionID != nil {
			m["session_id"] = *sessionID
		}
		if lastAccessedAt != nil {
			m["last_accessed_at"] = *lastAccessedAt
		}
		if expiresAt != nil {
			m["expires_at"] = *expiresAt
		}
		results = append(results, m)
	}
	return results, rows.Err()
}

// ShredMemory wraps the standalone ShredMemory function for DatabaseManager.
// Fires stale-foundation wakes for any theory whose dependencies reference
// this memory — the hard-delete equivalent of the soft-delete hook in
// MemoryStore.DeleteMemory.
//
// 2026-08-04 (Task 4): cascade invalidation hook. The standalone
// wrapper now enqueues cascade intents for every downstream
// decision/theory that cited the shredded memory (via explicit
// `dependencies` JSON or typed `epistemic_provenance` citations).
// The intent enqueue and the root DELETE share a single tx so a
// partial failure rolls back both. The lesson-aware behavior (the
// lessons view INSTEAD OF trigger + lessons_base + lessons_fts
// atomic delete) is preserved by routing through the existing
// standalone `ShredMemory(db, id)` function. The cascade hook
// only fires on the memory path because the lesson path has no
// reasoning dependents.
//
// Why this is now `error` returning: the cascade enqueue runs
// inside the same tx as the memory DELETE, so a partial failure
// surfaces as a non-nil error rather than silently no-op'ing. The
// legacy contract was permissive (`return nil` on success, error
// otherwise); that contract is preserved. The FireStaleFoundationWakes
// hook is unchanged — it still fires AFTER the tx commits because
// wakes are an out-of-band wake path, not a transactionally-coupled
// invariant.
//
// Idempotency: the standalone ShredMemory(db, id) helper already
// treats a missing row as a clean no-op (idempotent). The cascade
// intent write is also idempotent on the schema's UNIQUE key.
// Calling ShredMemory twice on the same id returns success on
// both calls and does not duplicate intents.
func (dm *DatabaseManager) ShredMemory(id string) error {
	if id == "" {
		return fmt.Errorf("shred: id is required")
	}

	// Probe lessons_base. Lessons path has no cascade hook (no
	// reasoning dependents); use the existing standalone helper
	// which already handles the lessons INSTEAD OF DELETE trigger.
	var lessonCount int
	if err := dm.db.QueryRow(`SELECT COUNT(*) FROM lessons_base WHERE id = ?`, id).Scan(&lessonCount); err != nil {
		return fmt.Errorf("shred: probe lessons_base: %w", err)
	}
	if lessonCount > 0 {
		if err := ShredMemory(dm.db, id); err != nil {
			return err
		}
		_, _ = dm.FireStaleFoundationWakes(id)
		return nil
	}

	// Memory path: DELETE + cascade intent enqueue in one tx. The
	// standalone ShredMemory helper opens its own tx internally; we
	// can't reuse it for the memory path because we need our own tx
	// to add the cascade hook. Inline the DELETE here so the cascade
	// hook lives in the same transaction.
	tx, err := dm.db.Begin()
	if err != nil {
		return fmt.Errorf("shred: begin: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	// Cascade invalidation hook first — must happen BEFORE the
	// DELETE so the discoverCascadeTargets helper can still observe
	// the dependencies JSON + epistemic_provenance rows pointing at
	// this id.
	if _, err := dm.EnqueueCascadeInvalidation(
		tx,
		id, "memory",
		"memory_shredded", "", 0,
	); err != nil {
		return fmt.Errorf("shred: cascade enqueue: %w", err)
	}

	if _, err := tx.Exec(`DELETE FROM memories WHERE id = ?`, id); err != nil {
		return fmt.Errorf("shred: delete memory: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("shred: commit: %w", err)
	}
	committed = true

	// Fire stale-foundation wakes AFTER commit. Wakes are an
	// out-of-band notification path (not transactionally-coupled),
	// so they live outside the tx.
	_, _ = dm.FireStaleFoundationWakes(id)
	return nil
}

// GetNegativeWeightMemories returns all non-deleted memories with weight < 0.
func (dm *DatabaseManager) GetNegativeWeightMemories() ([]map[string]interface{}, error) {
	rows, err := dm.db.Query(`
		SELECT id, collection, content, weight, metadata, created_at
		FROM memories WHERE weight < 0 AND deleted_at IS NULL
		ORDER BY weight ASC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var results []map[string]interface{}
	for rows.Next() {
		var id, collection, content, metadata, createdAt string
		var weight float64
		if err := rows.Scan(&id, &collection, &content, &weight, &metadata, &createdAt); err != nil {
			return nil, fmt.Errorf("scanning negative-weight memory row: %w", err)
		}
		results = append(results, map[string]interface{}{
			"id": id, "collection": collection, "content": content,
			"weight": int(weight), "metadata": metadata, "created_at": createdAt,
		})
	}
	return results, rows.Err()
}

// GetProvenTheoryForMemory searches for a resolved theory whose content
// references the given memory ID. Returns nil if no proven theory exists.
func (dm *DatabaseManager) GetProvenTheoryForMemory(memoryID string) (map[string]interface{}, error) {
	var id, content, metadata string
	err := dm.db.QueryRow(`
		SELECT id, content, metadata FROM memories
		WHERE collection = 'theories' AND deleted_at IS NULL
		AND json_extract(metadata, '$.status') = 'resolved'
		AND content LIKE '%' || ? || '%'
		ORDER BY created_at DESC LIMIT 1
	`, memoryID).Scan(&id, &content, &metadata)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{"id": id, "content": content, "metadata": metadata}, nil
}

// ==================== Reference queries (for web UI) ====================

// ListReferences returns all reference documents
func (dm *DatabaseManager) ListReferences(limit, offset int) ([]map[string]interface{}, error) {
	if limit <= 0 {
		limit = 50
	}
	query := `SELECT id, title, file_path, source_type, tags, import_reason, total_chunks, last_indexed, created_at FROM reference_docs ORDER BY created_at DESC LIMIT ? OFFSET ?`
	rows, err := dm.db.Query(query, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	refs := []map[string]interface{}{}
	for rows.Next() {
		var id, title, filePath, sourceType, tags, importReason, lastIndexed, createdAt string
		var totalChunks int
		if err := rows.Scan(&id, &title, &filePath, &sourceType, &tags, &importReason, &totalChunks, &lastIndexed, &createdAt); err != nil {
			return nil, fmt.Errorf("scanning reference list row: %w", err)
		}
		refs = append(refs, map[string]interface{}{
			"id":            id,
			"title":         title,
			"file_path":     filePath,
			"source_type":   sourceType,
			"tags":          tags,
			"import_reason": importReason,
			"total_chunks":  totalChunks,
			"last_indexed":  lastIndexed,
			"created_at":    createdAt,
			// Freshness: derived from existing fields (no schema change).
			// See internal/core/reference_freshness.go + protocol §8.
			"freshness": string(ClassifyReferenceFreshnessFromFields(tags, importReason, lastIndexed, time.Now())),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return refs, nil
}

// GetReference returns a reference document with its chunks
func (dm *DatabaseManager) GetReference(refID string) (map[string]interface{}, error) {
	var id, title, filePath, sourceType, tags, lastIndexed, createdAt, importReason string
	var totalChunks int
	err := dm.db.QueryRow(`
		SELECT id, title, file_path, source_type, tags, total_chunks, last_indexed, created_at, import_reason
		FROM reference_docs WHERE id = ?
	`, refID).Scan(&id, &title, &filePath, &sourceType, &tags, &totalChunks, &lastIndexed, &createdAt, &importReason)
	if err != nil {
		return nil, err
	}

	ref := map[string]interface{}{
		"id":            id,
		"title":         title,
		"file_path":     filePath,
		"source_type":   sourceType,
		"tags":          tags,
		"import_reason": importReason,
		"total_chunks":  totalChunks,
		"last_indexed":  lastIndexed,
		"created_at":    createdAt,
		// Freshness: derived from existing fields (no schema change).
		// See internal/core/reference_freshness.go + protocol §8.
		"freshness": string(ClassifyReferenceFreshnessFromFields(tags, importReason, lastIndexed, time.Now())),
	}

	// Get chunks
	rows, err := dm.db.Query(`SELECT id, chunk_index, section, content FROM reference_chunks WHERE doc_id = ? ORDER BY chunk_index`, refID)
	if err == nil {
		defer rows.Close()
		chunks := []map[string]interface{}{}
		for rows.Next() {
			var chunkID, section, content string
			var chunkIndex int
			if err := rows.Scan(&chunkID, &chunkIndex, &section, &content); err != nil {
				dm.LogAudit(AuditWarn, "web_db", fmt.Sprintf("GetReference: scan failed for chunk, skipping: %v", err), "", AuditContext{"ref_id": refID})
				continue
			}
			chunks = append(chunks, map[string]interface{}{
				"id":          chunkID,
				"chunk_index": chunkIndex,
				"section":     section,
				"content":     content,
			})
		}
		ref["chunks"] = chunks
	}

	return ref, nil
}

// SearchReferences searches reference documents by title/content
func (dm *DatabaseManager) SearchReferences(q string, limit int) ([]map[string]interface{}, error) {
	if limit <= 0 {
		limit = 20
	}

	// Try FTS5 on references table first, fallback to LIKE
	found := false
	if testRows, err := dm.db.Query(`SELECT 1 FROM sqlite_master WHERE type='table' AND name='references_fts'`); err == nil && testRows != nil {
		defer testRows.Close()
		if testRows.Next() {
			found = true
		}
	}

	var refs []map[string]interface{}
	var rows *sql.Rows
	var err error

	if found {
		escaped := strings.ReplaceAll(q, "\"", "\"\"")
		ftsQuery := "\"" + escaped + "\"*"
		// CTE pattern: bm25() is only in scope inside the FTS5 virtual
		// table query, so the score subquery must wrap the FTS5 match.
		// We join reference_docs on its implicit rowid (NOT id, which is
		// TEXT — see schema.go:622), because the references_ai trigger
		// (db.go:2456) populates references_fts.rowid = new.rowid from
		// reference_docs' implicit rowid. The previous pattern of
		// `WHERE id IN (SELECT rowid FROM references_fts)` failed
		// because id (TEXT) could never match rowid (INTEGER). Mirrors
		// the proven pattern from SearchReferenceChunks below.
		rows, err = dm.db.Query(`
			WITH scores AS (
				SELECT rowid, bm25(references_fts) AS s
				FROM references_fts
				WHERE references_fts MATCH ?
			)
			SELECT rd.id, rd.title, rd.file_path, rd.source_type, rd.tags,
			       rd.total_chunks, rd.last_indexed, rd.created_at
			FROM reference_docs rd
			JOIN scores ON rd.rowid = scores.rowid
			ORDER BY scores.s
			LIMIT ?`, ftsQuery, limit)
	} else {
		rows, err = dm.db.Query(`SELECT id, title, file_path, source_type, tags, total_chunks, last_indexed, created_at FROM reference_docs WHERE title LIKE ? OR content LIKE ? ORDER BY created_at DESC LIMIT ?`, "%"+q+"%", "%"+q+"%", limit)
	}

	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var id, title, filePath, sourceType, tags, lastIndexed, createdAt string
		var totalChunks int
		if err := rows.Scan(&id, &title, &filePath, &sourceType, &tags, &totalChunks, &lastIndexed, &createdAt); err != nil {
			dm.LogAudit(AuditWarn, "web_db", fmt.Sprintf("SearchReferences: scan failed for ref, skipping: %v", err), "", AuditContext{})
			continue
		}
		refs = append(refs, map[string]interface{}{
			"id":           id,
			"title":        title,
			"file_path":    filePath,
			"source_type":  sourceType,
			"tags":         tags,
			"total_chunks": totalChunks,
			"last_indexed": lastIndexed,
			"created_at":   createdAt,
			// Freshness: derived from existing fields. SearchReferences
			// doesn't query import_reason, so the signal here comes from
			// tags + last_indexed only — that's intentional, not a bug.
			"freshness": string(ClassifyReferenceFreshnessFromFields(tags, "", lastIndexed, time.Now())),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return refs, nil
}

// SearchReferenceChunks searches chunks within reference documents
func (dm *DatabaseManager) SearchReferenceChunks(q string, limit int) ([]map[string]interface{}, error) {
	if limit <= 0 {
		limit = 20
	}

	// Try FTS5 on chunks first, fallback to LIKE
	found := false
	if testRows, err := dm.db.Query(`SELECT 1 FROM sqlite_master WHERE type='table' AND name='reference_chunks_fts'`); err == nil && testRows != nil {
		defer testRows.Close()
		if testRows.Next() {
			found = true
		}
	}

	var chunks []map[string]interface{}
	var rows *sql.Rows
	var err error
	searchKind := "chunk_like"
	if found {
		searchKind = "chunk_fts"
		escaped := strings.ReplaceAll(q, "\"", "\"\"")
		ftsQuery := "\"" + escaped + "\"*"
		rows, err = dm.db.Query(`WITH scores AS (SELECT rowid, bm25(reference_chunks_fts) as s FROM reference_chunks_fts WHERE reference_chunks_fts MATCH ?) SELECT rc.id, rc.doc_id, rc.chunk_index, rc.section, rc.content, r.title, scores.s FROM reference_chunks rc JOIN scores ON rc.rowid = scores.rowid JOIN reference_docs r ON rc.doc_id = r.id ORDER BY scores.s LIMIT ?`, ftsQuery, limit)
	} else {
		rows, err = dm.db.Query(`SELECT rc.id, rc.doc_id, rc.chunk_index, rc.section, rc.content, r.title FROM reference_chunks rc JOIN reference_docs r ON rc.doc_id = r.id WHERE rc.content LIKE ? ORDER BY rc.chunk_index LIMIT ?`, "%"+q+"%", limit)
	}

	if err != nil {
		return nil, err
	}
	defer rows.Close()

	rank := 0
	for rows.Next() {
		rank++
		var id, docID, section, content, title string
		var chunkIndex int
		var score sql.NullFloat64
		if found {
			if err := rows.Scan(&id, &docID, &chunkIndex, &section, &content, &title, &score); err != nil {
				return nil, fmt.Errorf("scanning reference chunk row: %w", err)
			}
		} else {
			if err := rows.Scan(&id, &docID, &chunkIndex, &section, &content, &title); err != nil {
				return nil, fmt.Errorf("scanning reference chunk row: %w", err)
			}
		}
		chunks = append(chunks, map[string]interface{}{
			"id":          id,
			"doc_id":      docID,
			"chunk_index": chunkIndex,
			"section":     section,
			"content":     content,
			"doc_title":   title,
		})
		// Record the interaction (audit trail for admission function).
		// Skip very short queries — they are likely exploratory clicks and would
		// pollute the audit table with noise.
		if len(strings.TrimSpace(q)) >= 3 {
			dm.recordReferenceInteraction(docID, &id, q, searchKind, rank, score)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return chunks, nil
}

// recordReferenceInteraction writes one row to reference_interactions.
// Errors are swallowed: a failure to record an interaction must not break
// the search results, and the audit table is not on the critical path.
func (dm *DatabaseManager) recordReferenceInteraction(docID string, chunkID *string, query, searchKind string, rank int, score sql.NullFloat64) {
	_, _ = dm.db.Exec(`
		INSERT INTO reference_interactions (id, doc_id, chunk_id, query, search_kind, rank, score, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`, GenerateID(), docID, chunkID, query, searchKind, rank, score, time.Now().Unix())
}

// GetRecentInteractions returns the N most recent reference interactions.
// Used by the admission function (Phase 3) to surface what is being retrieved
// and how often. Cheap to query: the created_at index isn't needed for LIMIT N
// on the natural insert order, but if usage grows, add `created_at` to the index.
func (dm *DatabaseManager) GetRecentInteractions(limit int) ([]map[string]interface{}, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := dm.db.Query(`
		SELECT i.id, i.doc_id, i.chunk_id, i.query, i.search_kind, i.rank, i.score, i.created_at,
		       r.title, r.import_reason
		FROM reference_interactions i
		LEFT JOIN reference_docs r ON r.id = i.doc_id
		ORDER BY i.created_at DESC
		LIMIT ?
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]interface{}{}
	for rows.Next() {
		var id, docID, query, searchKind, createdAt, title string
		var chunkID, importReason sql.NullString
		var rank int
		var score sql.NullFloat64
		if err := rows.Scan(&id, &docID, &chunkID, &query, &searchKind, &rank, &score, &createdAt, &title, &importReason); err != nil {
			return nil, fmt.Errorf("scanning recent interaction row: %w", err)
		}
		entry := map[string]interface{}{
			"id":            id,
			"doc_id":        docID,
			"query":         query,
			"search_kind":   searchKind,
			"rank":          rank,
			"score":         score.Float64,
			"created_at":    createdAt,
			"doc_title":     title,
			"import_reason": importReason.String,
		}
		if chunkID.Valid {
			entry["chunk_id"] = chunkID.String
		}
		out = append(out, entry)
	}
	return out, rows.Err()
}

// GetInteractionsForDoc returns all interactions for a single reference doc,
// in reverse chronological order. Used by the admission function to evaluate
// whether a reference has been actively used, and in what context.
func (dm *DatabaseManager) GetInteractionsForDoc(docID string, limit int) ([]map[string]interface{}, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := dm.db.Query(`
		SELECT id, chunk_id, query, search_kind, rank, score, created_at
		FROM reference_interactions
		WHERE doc_id = ?
		ORDER BY created_at DESC
		LIMIT ?
	`, docID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]interface{}{}
	for rows.Next() {
		var id, query, searchKind, createdAt string
		var chunkID sql.NullString
		var rank int
		var score sql.NullFloat64
		if err := rows.Scan(&id, &chunkID, &query, &searchKind, &rank, &score, &createdAt); err != nil {
			return nil, fmt.Errorf("scanning interaction row: %w", err)
		}
		entry := map[string]interface{}{
			"id":          id,
			"query":       query,
			"search_kind": searchKind,
			"rank":        rank,
			"score":       score.Float64,
			"created_at":  createdAt,
		}
		if chunkID.Valid {
			entry["chunk_id"] = chunkID.String
		}
		out = append(out, entry)
	}
	return out, rows.Err()
}

// GetMostUsedReferences returns the references that have been retrieved most
// often, in descending order of interaction count. The admission function
// uses this to prioritize which references are candidates for memory
// extraction (a reference used 50 times is more likely to yield useful
// memories than one used once).
func (dm *DatabaseManager) GetMostUsedReferences(limit int) ([]map[string]interface{}, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := dm.db.Query(`
		SELECT r.id, r.title, r.import_reason, count(i.id) as hits, count(DISTINCT i.query) as distinct_queries
		FROM reference_docs r
		JOIN reference_interactions i ON i.doc_id = r.id
		GROUP BY r.id
		ORDER BY hits DESC
		LIMIT ?
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]interface{}{}
	for rows.Next() {
		var id, title, importReason string
		var hits, distinctQueries int
		if err := rows.Scan(&id, &title, &importReason, &hits, &distinctQueries); err != nil {
			return nil, fmt.Errorf("scanning most-used reference row: %w", err)
		}
		out = append(out, map[string]interface{}{
			"id":                id,
			"title":             title,
			"import_reason":     importReason,
			"hits":              hits,
			"distinct_queries":  distinctQueries,
		})
	}
	return out, rows.Err()
}

// FindAdmissionCandidates returns chunks that have been retrieved enough
// times to be worth evaluating as memory candidates. The trigger is
// "3+ hits across 2+ distinct queries" per chunk — frequent enough to
// suggest real value, varied enough to suggest the chunk carries a
// pattern that survives different framings.
//
// For each candidate, also returns:
//   - the import_reason of the parent reference (so the LLM sees the seed)
//   - the recent queries that surfaced it (so the LLM sees the framings)
//   - the chunk content (so the LLM can extract the pattern)
//
// Excludes chunks from references that already produced an admitted
// memory in the last 7 days for the same chunk — repeated admission is
// wasteful and the existing memory's reinforcement is the right signal.
// instead of the hand-rolled tx.Begin/Commit pattern they had here.

// SearchTopics searches topics using FTS5 or LIKE fallback
func (dm *DatabaseManager) SearchTopics(q string, limit int) ([]map[string]interface{}, error) {
	if limit <= 0 {
		limit = 20
	}

	escaped := strings.ReplaceAll(q, "\"", "\"\"")
	ftsQuery := "\"" + escaped + "\"*"

	found := false
	if testRows, err := dm.db.Query(`SELECT 1 FROM topics_fts WHERE topics_fts MATCH ? LIMIT 1`, ftsQuery); err == nil && testRows != nil {
		defer testRows.Close()
		if testRows.Next() {
			found = true
		}
	}

	var query string
	var args []interface{}

	if found {
		query = `WITH scores AS (SELECT rowid, bm25(topics_fts) as s FROM topics_fts WHERE topics_fts MATCH ?) SELECT t.id, t.name, COALESCE(t.description,''), t.created_at, COALESCE(t.tags,'{}'), scores.s FROM topics t JOIN scores ON t.rowid = scores.rowid`
		args = []interface{}{ftsQuery}
	} else {
		query = `SELECT id, name, COALESCE(description,''), created_at, COALESCE(tags,'{}') FROM topics WHERE is_active = 1 AND (name LIKE ? OR description LIKE ?)`
		args = []interface{}{"%" + q + "%", "%" + q + "%"}
	}

	if found {
		query += " ORDER BY scores.s LIMIT ?"
	} else {
		query += " ORDER BY created_at DESC LIMIT ?"
	}
	args = append(args, limit)

	rows, err := dm.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	topics := []map[string]interface{}{}
	for rows.Next() {
		var id, name, description, createdAt, tags string
		if err := rows.Scan(&id, &name, &description, &createdAt, &tags); err != nil {
			dm.LogAudit(AuditWarn, "web_db", fmt.Sprintf("SearchTopics: scan failed, skipping: %v", err), "", AuditContext{})
			continue
		}
		topics = append(topics, map[string]interface{}{
			"id": id, "name": name, "description": description,
			"created_at": createdAt, "tags": tags,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return topics, nil
}

// GetMemoryStats returns comprehensive memory statistics.
//
// 2026-09-16 fresh-profile fix: directives are stored in the
// `memories` table (collection='directives' for the canonical path,
// is_prime_directive=1 for legacy rows). They are surfaced as their
// own row on the dashboard / in `mpm status` and must NOT inflate
// the canonical active-memory count surfaced here. Pre-fix this
// method lumped directives into the `active` / `ltm` / `never_accessed`
// counts, so `mpm info` reported a larger memory population than
// `mpm status` — and a fresh install reported 5 "active memories"
// that were actually the baseline-cognitive-bootstrap directives.
//
// The exclusion is NULL-safe via COALESCE so NULL values in the
// flag column (imported, legacy, or manually-created rows) are
// treated as "not a directive" rather than UNKNOWN. See
// `countMemories` in cmd/mpm/handlers_status.go for the matching
// dashboard predicate; this is the same canonical form so the
// two surfaces can never disagree.
func (dm *DatabaseManager) GetMemoryStats() (map[string]interface{}, error) {
	stats := make(map[string]interface{})

	// Basic counts. Each one is best-effort — log audit and default to 0 on failure.
	var total, active, deleted, ltm, reinforced, neverAccessed, expired int
	// Canonical NULL-safe directive-exclusion used by every count below.
	const directiveExcl = `NOT (collection = 'directives' OR COALESCE(is_prime_directive, 0) = 1)`
	err := dm.db.QueryRow(`SELECT COUNT(*) FROM memories WHERE ` + directiveExcl).Scan(&total)
	if err != nil {
		return nil, err
	}
	if err := dm.db.QueryRow(`SELECT COUNT(*) FROM memories WHERE deleted_at IS NULL AND ` + directiveExcl).Scan(&active); err != nil {
		dm.LogAudit(AuditWarn, "web_db", fmt.Sprintf("GetMemoryStats: active count failed, defaulting to 0: %v", err), "", AuditContext{})
	}
	if err := dm.db.QueryRow(`SELECT COUNT(*) FROM memories WHERE deleted_at IS NOT NULL AND ` + directiveExcl).Scan(&deleted); err != nil {
		dm.LogAudit(AuditWarn, "web_db", fmt.Sprintf("GetMemoryStats: deleted count failed, defaulting to 0: %v", err), "", AuditContext{})
	}
	// CLI acceptance 2026-09-12: LTM uses the canonical IsLTMMemory
	// definition (is_long_term flag OR weight>=10, see ltm.go) so this
	// agrees with `mpm status` (countMemories weight>=10). Pre-fix this
	// counted only the flag and disagreed with status on every
	// high-weight non-promoted row.
	if err := dm.db.QueryRow(`SELECT COUNT(*) FROM memories WHERE (is_long_term = 1 OR weight >= 10) AND deleted_at IS NULL AND ` + directiveExcl).Scan(&ltm); err != nil {
		dm.LogAudit(AuditWarn, "web_db", fmt.Sprintf("GetMemoryStats: ltm count failed, defaulting to 0: %v", err), "", AuditContext{})
	}
	if err := dm.db.QueryRow(`SELECT COUNT(*) FROM memories WHERE reinforcement_count > 0 AND deleted_at IS NULL AND ` + directiveExcl).Scan(&reinforced); err != nil {
		dm.LogAudit(AuditWarn, "web_db", fmt.Sprintf("GetMemoryStats: reinforced count failed, defaulting to 0: %v", err), "", AuditContext{})
	}
	if err := dm.db.QueryRow(`SELECT COUNT(*) FROM memories WHERE last_accessed_at IS NULL AND reinforcement_count = 0 AND deleted_at IS NULL AND ` + directiveExcl).Scan(&neverAccessed); err != nil {
		dm.LogAudit(AuditWarn, "web_db", fmt.Sprintf("GetMemoryStats: never_accessed count failed, defaulting to 0: %v", err), "", AuditContext{})
	}
	if err := dm.db.QueryRow(`SELECT COUNT(*) FROM memories WHERE expires_at IS NOT NULL AND expires_at < strftime('%s','now') AND deleted_at IS NULL AND ` + directiveExcl).Scan(&expired); err != nil {
		dm.LogAudit(AuditWarn, "web_db", fmt.Sprintf("GetMemoryStats: expired count failed, defaulting to 0: %v", err), "", AuditContext{})
	}

	stats["total"] = total
	stats["active"] = active
	stats["deleted"] = deleted
	stats["ltm"] = ltm
	stats["reinforced"] = reinforced
	stats["never_accessed"] = neverAccessed
	stats["expired"] = expired

	// By collection — applies the same canonical directiveExcl
	// predicate as the scalar counters above so the breakdown
	// agrees with `total` (no directive rows leaking via their
	// collection name).
	collRows, err := dm.db.Query(`
		SELECT collection, COUNT(*) as count FROM memories
		WHERE deleted_at IS NULL AND ` + directiveExcl + `
		GROUP BY collection ORDER BY count DESC
	`)
	if err == nil {
		defer collRows.Close()
		byCollection := []map[string]interface{}{}
		for collRows.Next() {
			var coll string
			var count int
			if err := collRows.Scan(&coll, &count); err != nil {
				dm.LogAudit(AuditWarn, "web_db", fmt.Sprintf("GetMemoryStats: by_collection scan failed, skipping: %v", err), "", AuditContext{})
				continue
			}
			byCollection = append(byCollection, map[string]interface{}{"collection": coll, "count": count})
		}
		stats["by_collection"] = byCollection
	} else {
		stats["by_collection"] = []map[string]interface{}{}
	}

	// By tag (top 20) — applies directiveExcl so directive-derived
	// tags (e.g. prime_directive, directive) don't pollute the
	// ordinary-memory tag distribution.
	tagRows, err := dm.db.Query(`
		SELECT json_each.value as tag, COUNT(*) as count
		FROM memories, json_each(memories.tags)
		WHERE deleted_at IS NULL AND ` + directiveExcl + `
		GROUP BY json_each.value
		ORDER BY count DESC
		LIMIT 20
	`)
	if err == nil {
		defer tagRows.Close()
		byTag := []map[string]interface{}{}
		for tagRows.Next() {
			var tag string
			var count int
			if err := tagRows.Scan(&tag, &count); err != nil {
				dm.LogAudit(AuditWarn, "web_db", fmt.Sprintf("GetMemoryStats: by_tag scan failed, skipping: %v", err), "", AuditContext{})
				continue
			}
			byTag = append(byTag, map[string]interface{}{"tag": tag, "count": count})
		}
		stats["by_tag"] = byTag
	} else {
		stats["by_tag"] = []map[string]interface{}{}
	}

	// Reinforcement distribution — applies directiveExcl.
	distRows, err := dm.db.Query(`
		SELECT reinforcement_count, COUNT(*) as count
		FROM memories WHERE deleted_at IS NULL AND ` + directiveExcl + `
		GROUP BY reinforcement_count ORDER BY reinforcement_count
	`)
	if err == nil {
		defer distRows.Close()
		dist := []map[string]interface{}{}
		for distRows.Next() {
			var rc, count int
			if err := distRows.Scan(&rc, &count); err != nil {
				dm.LogAudit(AuditWarn, "web_db", fmt.Sprintf("GetMemoryStats: reinforce_dist scan failed, skipping: %v", err), "", AuditContext{})
				continue
			}
			dist = append(dist, map[string]interface{}{"reinforcement_count": rc, "count": count})
		}
		stats["reinforce_dist"] = dist
	} else {
		stats["reinforce_dist"] = []map[string]interface{}{}
	}

	// ── Epistemic Provenance Registry (ISR Telemetry) ──────────────────────
	// Nested grouping: agent → model/compute → persona. The
	// directiveExcl predicate must apply here too: the bootstrap
	// `mpm_ops_init` agent writes the directive rows, and without
	// the exclusion its registry entry would surface in
	// `mpm info`'s Database block with `total_memories` /
	// `active_memories` counts that contradict the
	// aggregate `total: 0` on a directive-only database.
	isrRows, err := dm.db.Query(`
		SELECT COALESCE(json_extract(metadata, '$.provenance.agent'), 'unknown') as agent,
		       COALESCE(json_extract(metadata, '$.provenance.model'), 'unknown') as model,
		       COALESCE(json_extract(metadata, '$.provenance.compute'), 'unknown') as compute,
		       COALESCE(json_extract(metadata, '$.provenance.persona'), 'unknown') as persona,
		       COUNT(*) as total,
		       SUM(CASE WHEN weight > 1 THEN 1 ELSE 0 END) as active
		FROM memories
		WHERE deleted_at IS NULL
		  AND ` + directiveExcl + `
		  AND json_extract(metadata, '$.provenance.agent') IS NOT NULL
		GROUP BY json_extract(metadata, '$.provenance.agent'),
		         json_extract(metadata, '$.provenance.model'),
		         json_extract(metadata, '$.provenance.compute'),
		         json_extract(metadata, '$.provenance.persona')
		ORDER BY agent, total DESC
	`)
	if err == nil {
		defer isrRows.Close()

		type isrPersona struct {
			persona string
			total   int
			active  int
		}
		type isrModel struct {
			model    string
			compute  string
			personas []isrPersona
		}
		type isrAgent struct {
			agent  string
			models []isrModel
		}

		var agents []isrAgent
		agentIndex := make(map[string]int)

		for isrRows.Next() {
			var agent, model, compute, persona sql.NullString
			var total, active int
			if err := isrRows.Scan(&agent, &model, &compute, &persona, &total, &active); err != nil {
				dm.LogAudit(AuditWarn, "web_db", fmt.Sprintf("GetMemoryStats: ISR scan failed, skipping: %v", err), "", AuditContext{})
				continue
			}
			a := agent.String
			ai, ok := agentIndex[a]
				if !ok {
					ai = len(agents)
					agentIndex[a] = ai
					agents = append(agents, isrAgent{agent: a})
				}
				var mi int
				found := false
				for i, m := range agents[ai].models {
					if m.model == model.String && m.compute == compute.String {
						mi = i
						found = true
						break
					}
				}
				if !found {
					mi = len(agents[ai].models)
					agents[ai].models = append(agents[ai].models, isrModel{
						model:   model.String,
						compute: compute.String,
					})
				}
				agents[ai].models[mi].personas = append(agents[ai].models[mi].personas, isrPersona{
					persona: persona.String,
					total:   total,
					active:  active,
				})
		}

		// Serialize to nested maps for JSON/display compatibility.
		// Initialise as an empty (non-nil) slice so the empty-stats
		// case marshals to `[]` rather than `null` — JSON consumers
		// expect a stable list shape.
		registry := []map[string]interface{}{}
		for _, a := range agents {
			var models []map[string]interface{}
			for _, m := range a.models {
				var personas []map[string]interface{}
				for _, p := range m.personas {
					isrVal := 0.0
					if p.total > 0 {
						isrVal = float64(p.active) / float64(p.total) * 100.0
					}
					personas = append(personas, map[string]interface{}{
						"persona":          p.persona,
						"total_memories":   p.total,
						"active_memories":  p.active,
						"decayed_to_floor": p.total - p.active,
						"isr":              math.Round(isrVal*10) / 10,
					})
				}
				models = append(models, map[string]interface{}{
					"model":    m.model,
					"compute":  m.compute,
					"personas": personas,
				})
			}
			registry = append(registry, map[string]interface{}{
				"agent":  a.agent,
				"models": models,
			})
		}

		// Always set provenance_registry — even when no ordinary
		// memories remain. The empty-slice encoding (``[]`` in JSON)
		// is part of the documented wire shape, not an
		// "absent means empty" convention. Consumers that look up
		// the key expect to find a stable list-shaped value.
		stats["provenance_registry"] = registry
	} else {
		stats["provenance_registry"] = []map[string]interface{}{}
	}

	return stats, nil
}

// PruneOlderThan deletes memories created before the given Unix-epoch seconds.
// Boundary accepts int64 directly (created_at is INTEGER seconds since epoch).
// Callers in cmd/mpm pass `time.Now().Add(-duration).Unix()`.
func (dm *DatabaseManager) PruneOlderThan(beforeUnixSec int64) (int, error) {
	result, err := dm.db.Exec(`
		DELETE FROM memories WHERE created_at < ? AND deleted_at IS NULL
	`, beforeUnixSec)
	if err != nil {
		return 0, err
	}
	rows, _ := result.RowsAffected()
	return int(rows), nil
}

// PruneNeverAccessed deletes memories that were never accessed
func (dm *DatabaseManager) PruneNeverAccessed() (int, error) {
	result, err := dm.db.Exec(`
		DELETE FROM memories
		WHERE last_accessed_at IS NULL
		  AND reinforcement_count = 0
		  AND weight = 1
		  AND is_long_term = 0
		  AND deleted_at IS NULL
	`)
	if err != nil {
		return 0, err
	}
	rows, _ := result.RowsAffected()
	return int(rows), nil
}

// GetMemoriesForExport retrieves memories with optional filters
func (dm *DatabaseManager) GetMemoriesForExport(collection, since, until string) ([]map[string]interface{}, error) {
	query := `
		SELECT id, collection, content, COALESCE(tags,'[]'), COALESCE(metadata,'{}'),
		       COALESCE(created_at, 0),
		       COALESCE(reinforcement_count, 0) as reinforcement_count,
		       COALESCE(weight, 1) as weight,
		       COALESCE(is_long_term, 0) is_long_term,
		       COALESCE(last_accessed_at, '') as last_accessed_at,
		       COALESCE(expires_at, '') as expires_at
		FROM memories
		WHERE deleted_at IS NULL
	`
	args := []interface{}{}

	if collection != "" {
		query += " AND collection = ?"
		args = append(args, collection)
	}
	if since != "" {
		query += " AND created_at >= ?"
		args = append(args, since)
	}
	if until != "" {
		query += " AND created_at <= ?"
		args = append(args, until+" 23:59:59")
	}

	// Alpha-4 ledger audit D-002 fix: GetMemoriesForExport is the read
	// path behind `mpm ls`. Every other read surface (GetMemory,
	// SearchMemories, HybridSearch, QueryMemories) applies
	// MemoryExpireClause. Without it, `mpm ls` surfaces expired rows
	// that `mpm show` and `mpm_memory query` will never find — a
	// population-divergence class identical to the deleted_at fix.
	query += MemoryExpireClause

	query += " ORDER BY created_at DESC"

	rows, err := dm.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var memories []map[string]interface{}
	for rows.Next() {
		var id, coll, content, tags, metadata, createdAt, lastAccessed, expiresAt string
		// Numeric columns: SQLite INTEGER scans to int64, REAL scans to float64.
		// Keep these types consistent with the schema and document them here
		// so downstream consumers don't silently fall through their default
		// values when type-asserting (See D-001 / W-008 in the alpha-4 audit).
		var rc, isLongTerm int64
		var weight float64
		err := rows.Scan(&id, &coll, &content, &tags, &metadata, &createdAt, &rc, &weight, &isLongTerm, &lastAccessed, &expiresAt)
		if err != nil {
			return nil, fmt.Errorf("scanning export memory row: %w", err)
		}
		mem := map[string]interface{}{
			"id":                  id,
			"collection":          coll,
			"content":             content,
			"tags":                tags,
			"metadata":            metadata,
			"created_at":          createdAt,
			"reinforcement_count": rc,
			"weight":              weight,
			"is_long_term":        isLongTerm,
			"last_accessed_at":    lastAccessed,
			"expires_at":          expiresAt,
		}
		memories = append(memories, mem)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return memories, nil
}

// =============================================================================
// Self-Improving Memory System (DatabaseManager wrappers)
// =============================================================================

// RunSelfMaintenance runs all self-improvement processes on DatabaseManager.
func (dm *DatabaseManager) RunSelfMaintenance() (map[string]interface{}, error) {
	store, err := dm.getSharedStore()
	if err != nil {
		return nil, err
	}

	stats, err := store.RunSelfMaintenance()
	if err != nil {
		return nil, err
	}

	return map[string]interface{}{
		"decayed_weights":          stats.DecayedWeights,
		"pruned_total":             stats.PrunedTotal,
		"consolidated":             stats.Consolidated,
		"never_accessed":           stats.NeverAccessed,
		"low_weight":               stats.LowWeight,
		"for_spaced_reinforcement": stats.ForReview,
	}, nil
}

// GetSpacedReinforcementReview returns memories for spaced reinforcement review.
func (dm *DatabaseManager) GetSpacedReinforcementReview(daysSinceAccess, limit int) ([]map[string]interface{}, error) {
	store, err := dm.getSharedStore()
	if err != nil {
		return nil, err
	}

	memories, err := store.SpacedReinforcementReview(daysSinceAccess, limit)
	if err != nil {
		return nil, err
	}

	result := make([]map[string]interface{}, 0, len(memories))
	for _, mem := range memories {
		result = append(result, map[string]interface{}{
			"id":                  mem.ID,
			"collection":          mem.Collection,
			"content":             mem.Content,
			"tags":                mem.Tags,
			"weight":              mem.Weight,
			"reinforcement_count": mem.ReinforcementCount,
			"last_accessed_at":    mem.LastAccessedAt,
		})
	}
	return result, nil
}

// GetContextualMemories returns memories relevant to a given context.
func (dm *DatabaseManager) GetContextualMemories(contextTags []string, sessionContext string, limit int) ([]map[string]interface{}, error) {
	store, err := dm.getSharedStore()
	if err != nil {
		return nil, err
	}

	memories, err := store.GetContextualMemories(contextTags, sessionContext, limit)
	if err != nil {
		return nil, err
	}

	result := make([]map[string]interface{}, 0, len(memories))
	for _, mem := range memories {
		result = append(result, map[string]interface{}{
			"id":                  mem.ID,
			"collection":          mem.Collection,
			"content":             mem.Content,
			"tags":                mem.Tags,
			"weight":              mem.Weight,
			"reinforcement_count": mem.ReinforcementCount,
		})
	}
	return result, nil
}
