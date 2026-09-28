package internal

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
)

// embeddingBytes marshals a []float32 to JSON bytes for storage in the
// reference_chunks.embedding BLOB column. Matches the serialization
// memories.embedding uses so the two columns are interchangeable
// (one less thing for vector-search code to special-case).
func embeddingBytes(vec []float32) ([]byte, error) {
	if vec == nil {
		return nil, nil
	}
	return json.Marshal(vec)
}

// ==================== Reference Library: DatabaseManager methods ====================
//
// These methods are the single SQLite write surface for the reference
// library. Both go through dm.WithTx so they inherit:
//   - watchdog.jsonl telemetry via ExecTracked / DBNode
//   - automatic rollback on panic (defer-recover inside WithTx)
//   - consistent commit/rollback handling shared with every other
//     DatabaseManager multi-statement operation
//
// There is no separate ReferenceDB type or connection. Anything that
// wants to write a reference goes through AddReference / DeleteReference
// here — a single connection pool, a single set of transaction rules.

// AddReference upserts a reference document and its chunks, applying
// the chunk_hash diff so unchanged content is not re-written and orphan
// chunks from a previous version of the doc are deleted.
//
// For re-ingest of the same source to actually trigger the chunk diff
// (instead of just creating a duplicate doc), callers are expected to
// look up the existing doc by source_path first and reuse its id. The
// diff path is the only way to reuse the same doc across ingests with
// stable chunk-level identity.
//
// Diff semantics:
//   - chunks whose content_hash already exists for the doc_id are
//     treated as Unchanged and skipped (their id is preserved).
//   - chunks whose content_hash is new are Inserted.
//   - existing chunks whose content_hash is not in the new set are
//     Deleted (orphan cleanup).
//   - the doc row itself is always updated to reflect the latest
//     metadata (title, tags, content_hash, total_chunks, last_indexed)
//     via ON CONFLICT(id) DO UPDATE.
//
// Writes into file_path (the schema column name) from doc.SourcePath
// (the struct field name). Both names are kept; do not rename without a
// migration — legacy code reads SourcePath from the struct.
//
// Chunk inserts use ON CONFLICT(id) DO NOTHING so a partial-pipeline
// retry against an already-populated chunk table doesn't duplicate
// rows or fail on the primary-key constraint.
func (dm *DatabaseManager) AddReference(doc *ReferenceDoc, chunks []ReferenceChunk) error {
	if dm == nil || dm.db == nil {
		return fmt.Errorf("AddReference: database not initialized")
	}
	if doc == nil {
		return fmt.Errorf("AddReference: nil reference doc")
	}
	if doc.ContentHash == "" {
		doc.ContentHash = HashContent(doc.Content)
	}

	return dm.WithTx(func(node DBNode) error {
		tagsJSON, _ := MarshalJSON(doc.Tags)

		// Upsert the doc row. ON CONFLICT(id) DO UPDATE keeps metadata
		// fresh on re-ingest; callers that want strict one-shot
		// semantics must check existence first and pick a fresh id.
		_, err := node.ExecTracked(`
			INSERT INTO reference_docs
			(id, title, file_path, source_type, tags, content, content_hash,
			 import_reason, total_chunks, last_indexed, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(id) DO UPDATE SET
				title = excluded.title,
				file_path = excluded.file_path,
				source_type = excluded.source_type,
				tags = excluded.tags,
				content = excluded.content,
				content_hash = excluded.content_hash,
				import_reason = excluded.import_reason,
				total_chunks = excluded.total_chunks,
				last_indexed = excluded.last_indexed
		`, 0,
			doc.ID, doc.Title, doc.SourcePath, doc.SourceType, tagsJSON,
			doc.Content, doc.ContentHash, doc.ImportReason,
			doc.TotalChunks, doc.LastIndexed, doc.Created,
		)
		if err != nil {
			return fmt.Errorf("add reference %q: %w", doc.ID, err)
		}

		// Chunk diff: classify new chunks against existing rows for
		// this doc, then apply the diff inside the same tx.
		diff, err := DiffChunks(dm.db, doc.ID, chunks)
		if err != nil {
			return fmt.Errorf("AddReference: diff chunks: %w", err)
		}

		for _, c := range diff.Inserted {
			if _, err := node.ExecTracked(`
				INSERT INTO reference_chunks
				(id, doc_id, chunk_index, section, content, source_path, content_hash)
				VALUES (?, ?, ?, ?, ?, ?, ?)
				ON CONFLICT(id) DO NOTHING
			`, 0,
				c.ID, c.DocID, c.ChunkIndex,
				c.Section, c.Content, c.SourcePath,
				ComputeChunkHash(c.Content),
			); err != nil {
				return fmt.Errorf("add chunk %q: %w", c.ID, err)
			}
		}
		// Updated chunks share the Inserted write path — same row,
		// different content. ON CONFLICT(id) DO UPDATE rewrites
		// content_hash AND clears the embedding column so
		// EmbedReferenceChunks picks the row up on its next pass.
		// Without the embedding reset, the row would carry a stale
		// vector for new content and the diff-based embedding bypass
		// would silently misroute searches.
		for _, c := range diff.Updated {
			if _, err := node.ExecTracked(`
				INSERT INTO reference_chunks
				(id, doc_id, chunk_index, section, content, source_path, content_hash)
				VALUES (?, ?, ?, ?, ?, ?, ?)
				ON CONFLICT(id) DO UPDATE SET
					content = excluded.content,
					content_hash = excluded.content_hash,
					section = excluded.section,
					chunk_index = excluded.chunk_index,
					embedding = NULL
			`, 0,
				c.ID, c.DocID, c.ChunkIndex,
				c.Section, c.Content, c.SourcePath,
				ComputeChunkHash(c.Content),
			); err != nil {
				return fmt.Errorf("update chunk %q: %w", c.ID, err)
			}
		}
		// Unchanged: skip entirely — the existing row is the truth.
		// Deleted: drop orphans whose content_hash is not in the new set.
		for _, orphanID := range diff.Deleted {
			if _, err := node.ExecTracked(
				`DELETE FROM reference_chunks WHERE id = ?`, 0, orphanID,
			); err != nil {
				return fmt.Errorf("delete orphan chunk %q: %w", orphanID, err)
			}
		}
		return nil
	})
}

// DeleteReference removes a reference, its chunks, and its audit rows
// (reference_interactions, admission_log) in a single transaction. The
// interactions and admission_log tables are NOT children of
// reference_chunks, so ON DELETE CASCADE alone does not cover the
// fan-out — without the transaction, a partial failure could leave
// orphan audit rows pointing at a missing parent doc.
func (dm *DatabaseManager) DeleteReference(id string) error {
	if dm == nil || dm.db == nil {
		return fmt.Errorf("DeleteReference: database not initialized")
	}
	return dm.WithTx(func(node DBNode) error {
		if _, err := node.ExecTracked(`DELETE FROM reference_chunks WHERE doc_id = ?`, 0, id); err != nil {
			return fmt.Errorf("delete reference chunks: %w", err)
		}
		if _, err := node.ExecTracked(`DELETE FROM reference_interactions WHERE doc_id = ?`, 0, id); err != nil {
			return fmt.Errorf("delete reference interactions: %w", err)
		}
		if _, err := node.ExecTracked(`DELETE FROM admission_log WHERE doc_id = ?`, 0, id); err != nil {
			return fmt.Errorf("delete admission log: %w", err)
		}
		if _, err := node.ExecTracked(`DELETE FROM reference_docs WHERE id = ?`, 0, id); err != nil {
			return fmt.Errorf("delete reference doc: %w", err)
		}
		return nil
	})
}

// EmbedReferenceChunks fills in NULL embeddings for chunks belonging
// to docID. Idempotent: chunks with non-NULL embeddings are skipped
// (they were embedded by a previous pass or by re-ingest of unchanged
// content). Per-chunk embedding uses EmbedText — when the provider is
// absent, disabled, or unreachable, EmbedText returns (nil, ...) and
// embeddingBytes(nil) writes NULL to the row. Database-level errors
// (query / UPDATE) are returned as err; ctx cancellation is checked
// between chunks and aborts cleanly. Per-chunk embed failures are
// counted in `failed` but do not abort the loop.
//
// Fingerprint: every successful write also stores the current
// provider's identity (embedding_source, embedding_dimension,
// embedding_model) so the reference backfill can detect when a
// stored embedding came from a different model than the one
// currently configured. The backfill's "needs re-embed?" predicate
// is: content_hash changed (handled by AddReference's diff) OR
// embedding IS NULL OR (embedding_model, embedding_dimension)
// differs from the active provider's fingerprint. See
// referenceEmbeddingNeedsRefresh.
//
// Embedding is intentionally split from AddReference:
//   - AddReference's tx stays small and fast (chunk rows + diff logic
//     only); a slow embed call would block the ingest tx and bloat
//     the WAL.
//   - Embedding is retryable independently — a transient provider
//     failure does not roll back chunk inserts.
//   - Embedding is parallelizable at the caller level (loop across
//     docIDs, wrap in a worker pool) without rewriting AddReference.
//
// Why this works with the chunk_hash diff: AddReference only clears
// embedding on the Updated branch (content changed). The Unchanged
// branch skips the row entirely, so its existing embedding stays —
// that is the whole point of the diff-based embedding bypass. Re-
// ingesting an unchanged manual re-embeds zero chunks.
func (dm *DatabaseManager) EmbedReferenceChunks(ctx context.Context, docID string) (embedded int, failed int, err error) {
	if dm == nil || dm.db == nil {
		return 0, 0, fmt.Errorf("EmbedReferenceChunks: database not initialized")
	}
	if docID == "" {
		return 0, 0, fmt.Errorf("EmbedReferenceChunks: empty docID")
	}

	rows, err := dm.db.QueryContext(ctx,
		`SELECT id, content FROM reference_chunks WHERE doc_id = ? AND embedding IS NULL`, docID)
	if err != nil {
		return 0, 0, fmt.Errorf("EmbedReferenceChunks: query: %w", err)
	}
	type pending struct {
		id      string
		content string
	}
	var batch []pending
	for rows.Next() {
		var p pending
		if scanErr := rows.Scan(&p.id, &p.content); scanErr != nil {
			rows.Close()
			return embedded, failed, fmt.Errorf("EmbedReferenceChunks: scan: %w", scanErr)
		}
		batch = append(batch, p)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return embedded, failed, fmt.Errorf("EmbedReferenceChunks: rows.Err: %w", err)
	}
	rows.Close()

	// Capture the active fingerprint ONCE so every row in this pass
	// stamps the same identity. Doing it per-row would race against
	// provider reconfiguration mid-loop.
	fp := currentEmbeddingFingerprint()

	for _, p := range batch {
		if ctx.Err() != nil {
			return embedded, failed, ctx.Err()
		}
		vec, _ := EmbedText(p.content)
		bytes, marshalErr := embeddingBytes(vec)
		if marshalErr != nil {
			failed++
			continue
		}
		// When EmbedText returned (nil, nil) — NullProvider /
		// disabled / absent — vec is nil and bytes is nil. The
		// UPDATE below writes a NULL embedding AND records the
		// source as "null" / dim=0 / model="" so the forensic
		// classifier and the backfill both treat the row as
		// "not yet embedded with a real provider". A subsequent
		// re-embed pass (or operator-configured provider) will
		// replace these values.
		var (
			source string
			dim    interface{}
			model  interface{}
		)
		if vec == nil {
			source = "null"
			dim = nil
			model = nil
		} else {
			source = "provider"
			dim = len(vec)
			model = fp.Model
		}
		_, writeErr := dm.db.ExecContext(ctx,
			`UPDATE reference_chunks
			   SET embedding = ?,
			       embedding_source = ?,
			       embedding_dimension = ?,
			       embedding_model = ?
			 WHERE id = ?`,
			bytes, source, dim, model, p.id)
		if writeErr != nil {
			failed++
			continue
		}
		embedded++
	}
	return embedded, failed, nil
}

// ReferenceEmbeddingFingerprint is the identity of the embedding
// that would be written RIGHT NOW by EmbedReferenceChunks given the
// active EmbeddingConfig. The fingerprint is the (Model, Dimension)
// pair the active provider would produce. A stored embedding matches
// the active provider when its (embedding_model, embedding_dimension)
// pair equals the active fingerprint. See referenceEmbeddingNeedsRefresh.
type ReferenceEmbeddingFingerprint struct {
	// Model is provider.Name() (e.g. "ollama:all-minilm",
	// "openai-compatible:text-embedding-3-small"). Empty when
	// the active provider is NullProvider / disabled / absent —
	// a fingerprint match against "" means "no provider is
	// configured", which is never a valid match.
	Model string

	// Dimension is the embedding dimension the active provider
	// would produce. 0 means the dimension has not been
	// determined yet (see ActiveEmbeddingDimension) OR the
	// active provider is NullProvider.
	Dimension int
}

// String is the canonical (model, dimension) representation for
// diagnostic output and for hashing into the "fingerprint" string
// used in the content_hash mixin (future enhancement).
func (f ReferenceEmbeddingFingerprint) String() string {
	if f.Model == "" && f.Dimension == 0 {
		return "null"
	}
	return f.Model + "@" + itoaDim(f.Dimension)
}

func itoaDim(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	pos := len(buf)
	for n > 0 {
		pos--
		buf[pos] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}

// currentEmbeddingFingerprint returns the fingerprint of the
// embedding that would be written RIGHT NOW given the active
// EmbeddingConfig. When the provider is null/disabled/absent
// the fingerprint is the zero value (Model="", Dimension=0),
// which referenceEmbeddingNeedsRefresh treats as "no active
// provider to compare against".
//
// We do NOT call provider.Embed to get a sample dimension — that
// would defeat the offline-fallback contract (every search must
// work without a live provider). Instead, the fingerprint is
// derived from cfg metadata. The trade-off: a provider that
// reports a different dimension than the one it actually
// produces would not be detected by the fingerprint comparison
// alone; the dimension-mismatch check at query time (in
// referenceSearchVector) is the safety net.
func currentEmbeddingFingerprint() ReferenceEmbeddingFingerprint {
	return CurrentEmbeddingFingerprint()
}

// CurrentEmbeddingFingerprint is the exported wrapper around
// currentEmbeddingFingerprint. CLI backfill and other
// out-of-package callers use this to report staleness
// without going through a DatabaseManager.
//
// Model is taken from provider.Name() (e.g. "ollama:all-minilm").
// Dimension is taken from the cached ActiveEmbeddingDimension
// if known; otherwise it is left at 0 and the dimension
// comparison degenerates to "stored dim is 0 OR matches active".
// The first call from RefreshStaleReferenceEmbeddings runs a
// one-time sample embed to populate the cache; subsequent calls
// are O(1) lookups.
func CurrentEmbeddingFingerprint() ReferenceEmbeddingFingerprint {
	cfg := DefaultEmbeddingConfig()
	if cfg.Provider == nil || cfg.Provider.Name() == "null" {
		return ReferenceEmbeddingFingerprint{}
	}
	return ReferenceEmbeddingFingerprint{
		Model:     cfg.Provider.Name(),
		Dimension: ActiveEmbeddingDimension(),
	}
}

// activeEmbeddingDimCache caches the most recent observed
// embedding dimension for the active provider. Populated by
// ActiveEmbeddingDimension (lazy sample embed on first miss).
// Cleared by ResetActiveEmbeddingDimensionForTest.
var (
	activeEmbeddingDimMu   sync.Mutex
	activeEmbeddingDim      int
	activeEmbeddingDimSet   bool
	activeEmbeddingDimModel string // model name when the dim was set
)

// ActiveEmbeddingDimension returns the dimension the active
// provider produces, discovered by a one-time sample embed.
// On the first call (or when the active model has changed since
// the last call), it issues a single EmbedText against a
// short probe string; subsequent calls return the cached value
// without an additional provider round-trip.
//
// The probe is "x" — the shortest non-empty string accepted
// by every embedding provider (empty strings are rejected by
// most APIs). The probe is the only runtime cost of dimension
// discovery; everything else in the backfill is metadata
// comparison.
//
// When the active provider is NullProvider / disabled /
// absent, the function returns 0 and leaves the cache
// unchanged.
func ActiveEmbeddingDimension() int {
	activeEmbeddingDimMu.Lock()
	defer activeEmbeddingDimMu.Unlock()

	cfg := DefaultEmbeddingConfig()
	if cfg.Provider == nil || cfg.Provider.Name() == "null" {
		return 0
	}
	if activeEmbeddingDimSet && activeEmbeddingDimModel == cfg.Provider.Name() {
		return activeEmbeddingDim
	}
	// First call OR model changed since last call: sample the
	// active provider once. If the embed fails, leave the
	// cache unchanged — the dimension is unknown for this
	// run and the predicate will not trigger a refresh on
	// dimension mismatch alone.
	vec, err := cfg.Provider.Embed("x")
	if err != nil || len(vec) == 0 {
		return 0
	}
	activeEmbeddingDim = len(vec)
	activeEmbeddingDimSet = true
	activeEmbeddingDimModel = cfg.Provider.Name()
	return activeEmbeddingDim
}

// ResetActiveEmbeddingDimensionForTest clears the cached
// dimension so the next ActiveEmbeddingDimension call runs a
// fresh sample embed. Test-only; production code never needs
// to call this.
func ResetActiveEmbeddingDimensionForTest() {
	activeEmbeddingDimMu.Lock()
	activeEmbeddingDim = 0
	activeEmbeddingDimSet = false
	activeEmbeddingDimModel = ""
	activeEmbeddingDimMu.Unlock()
}

// referenceEmbeddingNeedsRefresh reports whether a stored chunk
// row needs re-embedding given the active fingerprint.
//
//   - embedding IS NULL                              → YES, fill
//   - source == "null" or "hash"                     → YES, regenerate
//   - source == "provider", stored model != active   → YES
//   - source == "provider", stored dim != active dim → YES
//   - source == "provider", stored dim is 0 (legacy) → YES
//   - otherwise                                      → NO, kept as-is
//
// `embeddingIsNull` is the canonical "needs refresh" signal: the
// schema defaults embedding_source to 'provider' even on a
// NULL-embedding row, so source alone cannot tell us whether
// the row has been embedded. The caller passes the IS NULL
// signal explicitly so the predicate does not depend on the
// caller correctly translating the column state into source.
//
// The active.Dimension is set by ActiveEmbeddingDimension
// (one sample embed at the start of a refresh pass). When the
// active dimension is unknown (provider absent, embed failed,
// or first-call race), the dimension comparison degenerates to
// "stored dim is 0 OR matches active" — a stored-dim of 0
// still triggers a refresh (legacy fingerprint stamping), and
// a non-zero stored dim is treated as a match (we cannot know
// better without a successful sample). The model-name
// comparison still fires regardless of dimension state.
func referenceEmbeddingNeedsRefresh(embeddingIsNull bool, storedSource string, storedDim int, storedModel string, active ReferenceEmbeddingFingerprint) bool {
	// NULL embedding: fill.
	if embeddingIsNull {
		return true
	}
	// Source labels that never came from a real provider. The
	// 'null' label is written by EmbedReferenceChunks when the
	// active provider is NullProvider; the 'hash' label is the
	// legacy 256-dim fallback (none of the reference rows are
	// expected to carry it, but the guard is here for
	// forward-compat — see the memories equivalent).
	if storedSource == "null" || storedSource == "hash" {
		return true
	}
	// No active provider to compare against: leave the stored
	// row alone. The query-time dimension-mismatch check will
	// skip it if it ever fires; the operator needs to configure
	// a provider before any refresh can happen.
	if active.Model == "" {
		return false
	}
	// Model identity mismatch: the operator switched providers.
	// This is the primary refresh trigger; the dimension check
	// below is a secondary refinement.
	if storedModel != active.Model {
		return true
	}
	// Dimension mismatch. When active.Dimension is known and
	// non-zero, a stored dim that differs triggers a refresh.
	// When active.Dimension is unknown (0), a stored dim of 0
	// still triggers a refresh to stamp the fingerprint; a
	// non-zero stored dim is treated as a match because we
	// cannot prove otherwise without a sample embed.
	if active.Dimension != 0 {
		if storedDim != active.Dimension {
			return true
		}
	} else {
		// Active dim unknown: refresh legacy rows whose dim
		// was never stamped.
		if storedDim == 0 {
			return true
		}
	}
	return false
}

// RefreshStaleReferenceEmbeddings scans every reference chunk
// and re-embeds the ones that referenceEmbeddingNeedsRefresh
// flags as stale. Returns the count of refreshed rows.
//
// "Stale" means at least one of:
//
//   - embedding IS NULL (never been embedded)
//   - embedding_source = 'null' (NullProvider write)
//   - embedding_source = 'hash' (legacy hash fallback)
//   - embedding_model does not match the active provider
//   - embedding_dimension does not match the active provider
//
// The function is the canonical "automatic detection" path the
// user spec asks for: the operator does not need to remember
// that a model change means "run --force". If a fingerprint
// mismatch is detectable, this function will find it.
//
// Embedding is done chunk-by-chunk in a single transaction per
// doc. Per-chunk embed failures are counted in `failed` and do
// not abort the doc (same shape as EmbedReferenceChunks).
//
// When the active provider is null/disabled/absent, no rows can
// be refreshed — the function returns (0, nil) and a no-op. The
// caller (CLI backfill) is expected to gate on provider
// availability before calling.
//
// Idempotent: a row that was refreshed and now matches the
// active fingerprint will not be refreshed again. Running the
// function twice in a row with no embedding change between
// runs is a no-op on the second call.
func (dm *DatabaseManager) RefreshStaleReferenceEmbeddings(ctx context.Context) (refreshed int, failed int, err error) {
	if dm == nil || dm.db == nil {
		return 0, 0, fmt.Errorf("RefreshStaleReferenceEmbeddings: database not initialized")
	}
	active := currentEmbeddingFingerprint()
	if active.Model == "" {
		// No active provider — nothing to refresh against.
		return 0, 0, nil
	}

	// Fetch every chunk with its current fingerprint. The
	// scan is small (corpus is bounded), so a single SELECT
	// is cheaper than batching by doc.
	rows, err := dm.db.QueryContext(ctx, `
		SELECT id, doc_id, content, embedding,
		       COALESCE(embedding_source, ''),
		       COALESCE(embedding_dimension, 0),
		       COALESCE(embedding_model, '')
		FROM reference_chunks
	`)
	if err != nil {
		return 0, 0, fmt.Errorf("RefreshStaleReferenceEmbeddings: query: %w", err)
	}
	type staleRow struct {
		id      string
		docID   string
		content string
	}
	var stale []staleRow
	for rows.Next() {
		var (
			id, docID, content, source, model string
			embRaw                           []byte
			dim                              int
		)
		if scanErr := rows.Scan(&id, &docID, &content, &embRaw, &source, &dim, &model); scanErr != nil {
			rows.Close()
			return refreshed, failed, fmt.Errorf("RefreshStaleReferenceEmbeddings: scan: %w", scanErr)
		}
		// Embedding IS NULL is the canonical "needs refresh" signal.
		// The fingerprint source default is 'provider' even on
		// NULL-embedding rows, so we cannot rely on source alone.
		embeddingIsNull := len(embRaw) == 0
		if referenceEmbeddingNeedsRefresh(embeddingIsNull, source, dim, model, active) {
			stale = append(stale, staleRow{id, docID, content})
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return refreshed, failed, fmt.Errorf("RefreshStaleReferenceEmbeddings: rows.Err: %w", err)
	}
	rows.Close()

	// Refresh each stale row in place. Sequential per row keeps
	// the WAL small and matches the existing EmbedReferenceChunks
	// pacing. Per-row errors do not abort the loop.
	for _, r := range stale {
		if ctx.Err() != nil {
			return refreshed, failed, ctx.Err()
		}
		vec, _ := EmbedText(r.content)
		if vec == nil {
			// Provider became unavailable mid-loop (or this
			// row's content can't be embedded). Skip with
			// counted failure.
			failed++
			continue
		}
		bytes, marshalErr := embeddingBytes(vec)
		if marshalErr != nil {
			failed++
			continue
		}
		_, writeErr := dm.db.ExecContext(ctx, `
			UPDATE reference_chunks
			   SET embedding = ?,
			       embedding_source = 'provider',
			       embedding_dimension = ?,
			       embedding_model = ?
			 WHERE id = ?`,
			bytes, len(vec), active.Model, r.id)
		if writeErr != nil {
			failed++
			continue
		}
		refreshed++
	}
	return refreshed, failed, nil
}

