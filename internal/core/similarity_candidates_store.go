// similarity_candidates_store.go — the ONLY path that persists candidates.
//
// Separation of concerns, stated plainly:
//
//   - similarity_candidates.go DISCOVERS candidates and writes nothing.
//   - this file PERSISTS them, and is reachable only from an explicitly
//     invoked operator command.
//
// Ordinary semantic retrieval calls neither. That is the whole guarantee:
// a read cannot add a row to the persistent queue, so a read cannot grow it
// without bound and a diagnostic command cannot leave a durable trace
// behind.
//
// The queue rows themselves are unchanged in shape. Existing
// shared.contradiction_log records are read, resolved and arbitrated exactly
// as before; nothing here migrates, reinterprets or deletes them. What
// changed is only WHO may add to the queue: not a retrieval, only an
// operator who asked.

package internal

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
)

// similarityCandidateSource is set on every row this file writes, so a
// reviewer can tell at a glance which queue items arrived from an explicit
// operator scan and which predate the split. Existing rows carry whatever
// detected_by they already had and are not touched.
const similarityCandidateSource = "similarity-scan"

// LoadSimilarityScanEntries reads the most recent live memories as scan
// inputs. `limit` caps the working set (0 = no cap); pairs are evaluated
// within that set only, so a smaller limit is a cheaper, shallower scan.
//
// A memory with no embedding, a literal 'null' vector, or a synthetic hash
// vector cannot be compared meaningfully and is excluded rather than
// silently scored.
//
// The embedding_model filter is the important one, and it is what every
// retrieval path already does (hybrid_search.go, vector_index.go, db.go all
// compare against CurrentEmbeddingFingerprint().Model). validEmbeddingPair
// only checks that two vectors are the same LENGTH, so two 768-dim vectors
// from different models sail through it and produce a confident cosine
// between unrelated spaces. That was survivable while one model was in use;
// it is not survivable mid-migration, when a scan run over a partially
// re-embedded store would compare all-minilm rows against nomic rows and
// propose pairs derived from a number that means nothing.
func (dm *DatabaseManager) LoadSimilarityScanEntries(limit int, schemaPrefix string) ([]ScanEntry, error) {
	if dm.db == nil {
		return nil, fmt.Errorf("LoadSimilarityScanEntries: dm.db is nil")
	}
	// schemaPrefix is interpolated into the statement, so it is validated the
	// same way both HybridSearch call sites validate theirs. The scan is a
	// WRITE path — it feeds EnqueueSimilarityCandidates — so an unvalidated
	// prefix here is a far worse problem than a bad read.
	if err := ValidateSchemaPrefix(schemaPrefix); err != nil {
		return nil, fmt.Errorf("LoadSimilarityScanEntries: %w", err)
	}
	// An empty active model means no provider is configured, so there is no
	// fingerprint to filter on. Selecting everything then is the honest
	// behaviour: the scan runs, the operator sees candidates, and the
	// absence of a provider is visible in `mpm config show` rather than
	// silently emptying the queue.
	fingerprint := CurrentEmbeddingFingerprint().Model

	q := fmt.Sprintf(`
		SELECT id, embedding, content, COALESCE(metadata, '{}')
		FROM %smemories
		WHERE deleted_at IS NULL
		  AND embedding IS NOT NULL
		  AND embedding != 'null'
		  AND embedding_source != 'hash'
	`, schemaPrefix)
	args := []any{}
	if fingerprint != "" {
		q += " AND embedding_model = ?"
		args = append(args, fingerprint)
	}
	q += " ORDER BY created_at DESC"
	if limit > 0 {
		q += fmt.Sprintf(" LIMIT %d", limit)
	}

	rows, err := dm.db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("LoadSimilarityScanEntries: %w", err)
	}
	defer rows.Close()

	var out []ScanEntry
	for rows.Next() {
		var id, embStr, content, meta string
		if err := rows.Scan(&id, &embStr, &content, &meta); err != nil {
			return nil, fmt.Errorf("LoadSimilarityScanEntries: scan: %w", err)
		}
		var emb []float32
		if embStr == "" || json.Unmarshal([]byte(embStr), &emb) != nil || len(emb) == 0 {
			continue
		}
		out = append(out, ScanEntry{
			ID:           id,
			Content:      content,
			Embedding:    emb,
			IsChallenged: memoryIsChallenged(meta),
			MetadataJSON: meta,
		})
	}
	return out, rows.Err()
}

// LoadSimilarityScanEntriesFederated scans BOTH sides of the corpus — the
// local `memories` table and the ATTACHed `shared.memories` — and returns the
// union. This is the entry point the propose command uses.
//
// Scanning local only would be a silent under-scan on a shared-attached
// install. It is not a subtle one: hybridSearchScope (memory_tools.go:419)
// returns local ∪ shared to every reader, so the memories an operator sees on
// screen are exactly the memories a local-only scan would fail to compare.
// The queue those candidates are enqueued into — shared.contradiction_log —
// lives on the shared side regardless, so the scan and its destination were
// reading from different databases.
//
// The scan degrades to local-only when no shared DB is attached, matching
// hybridSearchScope: an unattached shared side is a normal configuration, not
// an error, and the operator gets a scan of what actually exists.
//
// limit applies PER SIDE, exactly as hybridSearchScope applies its limit per
// side. The union is therefore up to 2x limit. This is deliberate: the limit
// is a per-source cost bound, and a scan that silently saw only half the
// corpus would be worse than a wider one.
func (dm *DatabaseManager) LoadSimilarityScanEntriesFederated(limit int) ([]ScanEntry, error) {
	local, err := dm.LoadSimilarityScanEntries(limit, "")
	if err != nil {
		return nil, err
	}

	shared, err := dm.LoadSimilarityScanEntries(limit, "shared.")
	if err != nil {
		if isSharedUnavailable(err) {
			return local, nil
		}
		return nil, fmt.Errorf("LoadSimilarityScanEntriesFederated: %w", err)
	}

	// A memory can be promoted to shared and remain in local. Both copies are
	// the same claim with the same ID, and FindSimilarityCandidates skips only
	// index pairs (j > i) — it would compare the two copies against each other
	// and propose a memory as a contradiction of itself. Dedupe on ID.
	seen := make(map[string]struct{}, len(local)+len(shared))
	out := make([]ScanEntry, 0, len(local)+len(shared))
	for _, e := range local {
		seen[e.ID] = struct{}{}
		out = append(out, e)
	}
	for _, e := range shared {
		if _, dup := seen[e.ID]; dup {
			continue
		}
		seen[e.ID] = struct{}{}
		out = append(out, e)
	}
	return out, nil
}

// memoryIsChallenged reports whether the metadata carries the challenge flag.
// It READS pre-existing state and never sets it.
//
// The test is the same one the schema's own aggregate uses —
// json_extract(metadata, '$.status') = 'challenged' (schema.go) — rather
// than a substring search over the raw JSON.
//
// A substring search was wrong in four measured ways:
//
//	{"status":"challenged",...}                            -> true   (correct)
//	{"restored_from_challenge":true,"challenged_at":100}    -> true   (WRONG)
//	{"challenged_memory_id":"other"}                        -> true   (WRONG)
//	{"note":"we challenged the rollout last sprint"}        -> true   (WRONG)
//
// The restored case is the one that matters. challenge_restore.go nulls
// status and challenged_theory_id but deliberately KEEPS challenged_at and
// challenged_prior_weight as audit trail — so a substring match reads every
// restored memory as challenged forever. That mis-assigns the
// state_collision candidate kind and swaps MemoryA/MemoryB, so the pair an
// operator reviews names the wrong side.
func memoryIsChallenged(metadataJSON string) bool {
	if metadataJSON == "" {
		return false
	}
	var meta struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal([]byte(metadataJSON), &meta); err != nil {
		return false
	}
	return meta.Status == "challenged"
}

// EnqueueSimilarityCandidates persists candidates to the shared queue.
//
// This is the explicit opt-in write. It is not called by HybridSearch, by
// recall, or by any diagnostic path.
//
// EnqueueContradictionReport is idempotent on the unresolved memory pair, so
// re-running a scan over the same corpus adds nothing the second time. That
// is what keeps a repeated operator scan from growing the queue without
// bound. Idempotency was briefly a claim without a mechanism: the table's
// uniqueness constraint includes a one-second-resolution `detected_at`, so
// OR IGNORE alone suppressed a duplicate only within the same second. See
// the pre-check in contradiction_log.go.
//
// Returns the number of rows ACTUALLY inserted. Candidates already queued
// are not counted, so a repeat run over unchanged data returns 0 rather than
// re-announcing last run's work as new. Callers show this number to an
// operator, and a number that counts writes it did not perform is worse than
// no number at all.
func (dm *DatabaseManager) EnqueueSimilarityCandidates(cands []SimilarityCandidate) (int, error) {
	if len(cands) == 0 {
		return 0, nil
	}
	inserted := 0
	for _, c := range cands {
		ev := ContradictionEvidence{
			MemoryA:    c.MemoryA,
			MemoryB:    c.MemoryB,
			Evidence:   c.Evidence,
			Similarity: c.Similarity,
			DetectedBy: similarityCandidateSource,
		}
		wrote, err := dm.EnqueueContradictionReport(ev)
		if err != nil {
			// Surface the shared-attachment failure rather than swallowing
			// it: a caller that asked for persistence and got silence would
			// reasonably conclude the queue was updated.
			if isSharedUnavailable(err) {
				return inserted, fmt.Errorf(
					"EnqueueSimilarityCandidates: shared database unavailable: %w", err)
			}
			return inserted, err
		}
		if wrote {
			inserted++
		}
	}
	return inserted, nil
}

// isSharedUnavailable reports whether err is the expected "no shared
// database attached" failure rather than a real write error.
func isSharedUnavailable(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "no such table") ||
		strings.Contains(msg, "no such schema") ||
		strings.Contains(msg, "shared")
}

// CountContradictionQueue reports how many unresolved rows the queue holds.
// Used by the explicit review command to show before/after, and by tests
// that assert a retrieval wrote nothing.
func (dm *DatabaseManager) CountContradictionQueue() (int, error) {
	if dm.db == nil {
		return 0, fmt.Errorf("CountContradictionQueue: dm.db is nil")
	}
	var n int
	err := dm.db.QueryRow(
		`SELECT COUNT(*) FROM shared.contradiction_log WHERE resolved_at IS NULL`).Scan(&n)
	if err != nil {
		if err == sql.ErrNoRows {
			return 0, nil
		}
		return 0, err
	}
	return n, nil
}
