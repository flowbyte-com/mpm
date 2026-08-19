// resolve.go — Arc 1 (Conflict Resolution) decision + apply logic.
//
// Split out of contradiction_log.go (F-009). Owns the "decide who wins,
// then apply the verdict" path:
//
//   - LoadMemoryProvenance    reads the three signals that drive scoring
//   - ResolveOneContradiction the core decision function (dry-run + apply)
//   - applyResolution         the decisive-winner transaction
//   - applyArbitration        the close-call path (proposes a theory;
//                             queue row stays open for human input)
//   - parseContradictionID / SanitizeEvidenceTag  helpers
//
// Queue CRUD itself (EnqueueContradiction, LoadContradictionQueue) lives
// in contradiction_log.go; the operator-driven arbitration loop lives
// in arbitration.go. The three files together implement the Arc 1
// resolution loop (detect → triage → resolve → remember).

package internal

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"
)

// LoadMemoryProvenance loads the three signals (confidence,
// last_reinforced, reinforcement_count) for a single memory. Returns
// a zero-value ProvenanceScore if the memory is missing or deleted —
// the resolver treats missing memories as having the lowest possible
// provenance, so the OTHER side wins by default.
func (dm *DatabaseManager) LoadMemoryProvenance(memoryID string) (ProvenanceScore, error) {
	ps := ProvenanceScore{MemoryID: memoryID}
	var (
		confidence     sql.NullFloat64
		updatedAt      sql.NullString
		retrievalPri   sql.NullFloat64
	)
	err := dm.db.QueryRow(`
		SELECT
			COALESCE(confidence, 0.5),
			COALESCE(updated_at, CURRENT_TIMESTAMP),
			COALESCE(retrieval_priority, 0.5)
		FROM shared.memories
		WHERE id = ? AND deleted_at IS NULL
	`, memoryID).Scan(&confidence, &updatedAt, &retrievalPri)
	if err == sql.ErrNoRows {
		// Missing/deleted memory = lowest provenance. Caller will
		// see score 0 and the other side wins decisively.
		return ps, nil
	}
	if err != nil {
		return ps, fmt.Errorf("LoadMemoryProvenance: %w", err)
	}
	ps.Confidence = confidence.Float64
	if t, perr := time.Parse(time.RFC3339, updatedAt.String); perr == nil {
		ps.LastReinforced = t
		ps.AgeDays = time.Since(t).Hours() / 24.0
	}
	// Reinforcement count isn't a first-class column; we approximate
	// from the evidence count attached to the memory via the
	// evidence_store pipeline. The query is cheap and the
	// approximation is good enough for ranking. A query failure
	// degrades gracefully to 0 reinforcement (rank drops to
	// confidence+age weight), which is acceptable; we log so the
	// shared DB issue surfaces in the audit log.
	var evCount int
	if err := dm.db.QueryRow(`
		SELECT COUNT(*) FROM shared.evidence
		WHERE artifact_id = ? AND artifact_type = 'memory'
	`, memoryID).Scan(&evCount); err != nil {
		dm.LogAudit(AuditWarn, "resolve", fmt.Sprintf("LoadMemoryProvenance: shared.evidence count failed (memory=%s), defaulting to 0: %v", memoryID, err), "", AuditContext{})
		evCount = 0
	}
	ps.Reinforcement = math.Min(float64(evCount)/10.0, 1.0)

	// Weighted sum: confidence 50%, freshness 30%, reinforcement 20%.
	// Freshness decays as 1 / (1 + age_days) — recent memories win.
	// Reinforcement saturates at 10 evidence rows.
	ps.Score = 0.5*ps.Confidence + 0.3*(1.0/(1.0+ps.AgeDays)) + 0.2*ps.Reinforcement
	return ps, nil
}

// ResolveOneContradiction is the core decision function. Given a
// queue row, load both memories' provenance, compute the verdict,
// and (if --apply is set) apply it in a single transaction.
//
// Returns the decision. Caller prints it (dry-run) or applies it
// (--apply). The function itself NEVER mutates state in dry-run
// mode — mutation is gated on the apply flag at the call site.
//
// Three terminal states:
//   - "resolved": margin > threshold, slash applied, resolution
//     memory written, queue row marked resolved.
//   - "arbitration": margin < threshold, theory proposed, queue
//     row NOT marked resolved (operator arbitrates manually).
//   - "missing": one of the memories no longer exists. Default to
//     the survivor; queue row marked resolved.
func (dm *DatabaseManager) ResolveOneContradiction(row map[string]interface{}, apply bool) (ResolutionDecision, error) {
	dec := ResolutionDecision{}

	memoryA, _ := row["memory_id_a"].(string)
	memoryB, _ := row["memory_id_b"].(string)
	idStr, _ := row["id"].(string)
	queueID, _ := parseContradictionID(idStr)

	if memoryA == "" || memoryB == "" {
		return dec, fmt.Errorf("ResolveOneContradiction: missing memory IDs in row")
	}

	scoreA, err := dm.LoadMemoryProvenance(memoryA)
	if err != nil {
		return dec, fmt.Errorf("ResolveOneContradiction: load %s: %w", memoryA, err)
	}
	scoreB, err := dm.LoadMemoryProvenance(memoryB)
	if err != nil {
		return dec, fmt.Errorf("ResolveOneContradiction: load %s: %w", memoryB, err)
	}

	dec.LoserScore = scoreA
	dec.WinnerScore = scoreB
	dec.Margin = math.Abs(scoreA.Score - scoreB.Score)
	dec.IsCloseCall = dec.Margin < ProvenanceThreshold

	// Tie-or-close → propose theory, do not slash.
	if dec.IsCloseCall {
		dec.LoserID = memoryA
		dec.WinnerID = memoryB
		dec.Rationale = fmt.Sprintf(
			"close call: margin=%.3f < threshold=%.2f. Proposing arbitration theory.",
			dec.Margin, ProvenanceThreshold,
		)
		dec.ResolutionTag = fmt.Sprintf("arbitration:contradiction:%s-%s", memoryA, memoryB)
		if apply {
			if err := dm.applyArbitration(queueID, memoryA, memoryB, scoreA, scoreB, dec.Margin); err != nil {
				return dec, fmt.Errorf("apply arbitration: %w", err)
			}
		}
		return dec, nil
	}

	// Decisive: higher score wins. Slash the loser's weight.
	var loser, winner string
	var loserScore, winnerScore ProvenanceScore
	if scoreA.Score > scoreB.Score {
		// A has the higher score, so A wins. B is the loser.
		winner, loser = memoryA, memoryB
		winnerScore, loserScore = scoreA, scoreB
	} else {
		// B has the higher (or equal) score, so B wins. A is the loser.
		winner, loser = memoryB, memoryA
		winnerScore, loserScore = scoreB, scoreA
	}
	dec.LoserID = loser
	dec.WinnerID = winner
	dec.LoserScore = loserScore
	dec.WinnerScore = winnerScore
	dec.SlashAmount = int(math.Round(loserScore.Confidence * SlashFraction * 100))
	dec.Rationale = fmt.Sprintf(
		"decisive: %s (score=%.3f) wins over %s (score=%.3f, margin=%.3f). Slash %d weight units.",
		winner, winnerScore.Score, loser, loserScore.Score, dec.Margin, dec.SlashAmount,
	)
	dec.ResolutionTag = fmt.Sprintf("resolution:contradiction:%s-%s", loser, winner)

	if apply {
		if err := dm.applyResolution(queueID, dec); err != nil {
			return dec, fmt.Errorf("apply resolution: %w", err)
		}
		// Arc 2: auto-broadcast as the side-effect of the operator's
		// explicit `--apply`. Non-fatal: a broadcast failure does
		// NOT roll back the resolution (which is already committed).
		// Resolution is ground truth; broadcast is the notification.
		// The source_session_id is empty here because the resolution
		// is applied by an operator CLI/MCP call without a stable
		// session context — discovery will simply fan out to all
		// active sessions (which is the right behavior for an
		// operator-driven resolution).
		resolutionID := dec.ResolutionMemoryID // populated by applyResolution
		if resolutionID != "" {
			_, _ = dm.BroadcastMemory(resolutionID, BroadcastOpts{
				Kind:        "resolution",
				SourceAgent: "arc1-resolver",
			})
		}
	}
	return dec, nil
}

// applyResolution executes the verdict in a single transaction:
//  1. Slash the loser's weight (via ChallengeMemory, which sets
//     status='challenged' and decrements weight).
//  2. INSERT a resolution memory into shared.memories — the FTS5
//     sync triggers from 61a8418 mirror it to shared.memories_fts.
//  3. UPDATE the queue row: resolved_at + resolution_memory_id.
//
// All three steps in one transaction. Crash safety: either all
// three land or none do. The queue row stays "unresolved" if any
// step fails; the next operator run picks it up.
func (dm *DatabaseManager) applyResolution(queueID int64, dec ResolutionDecision) error {
	tx, err := dm.db.Begin()
	if err != nil {
		return fmt.Errorf("applyResolution: begin: %w", err)
	}
	defer tx.Rollback()

	// Step 2: write the resolution memory first (so we have its ID
	// for the loser's metadata patch). The collection is
	// 'memories' (so it lands in the regular retrieval path), with
	// a tag identifying it as a resolution. The content is the
	// verdict narrative.
	resolutionID := fmt.Sprintf("resolution-%s-%d", dec.LoserID, time.Now().UnixNano())
	content := fmt.Sprintf(
		"Resolution: memory_%s (loser, confidence=%.3f, last_reinforced=%s) "+
			"slashed by %d weight units in favor of memory_%s (winner, confidence=%.3f, "+
			"last_reinforced=%s, %d evidence rows). Rationale: provenance-based scoring, "+
			"margin=%.3f. See shared.contradiction_log id=%d.",
		dec.LoserID, dec.LoserScore.Confidence, dec.LoserScore.LastReinforced.Format(time.RFC3339),
		dec.SlashAmount,
		dec.WinnerID, dec.WinnerScore.Confidence, dec.WinnerScore.LastReinforced.Format(time.RFC3339),
		int(dec.WinnerScore.Reinforcement*10),
		dec.Margin, queueID,
	)
	tagsJSON := fmt.Sprintf(`["%s"]`, dec.ResolutionTag)
	_, err = tx.Exec(`
		INSERT INTO shared.memories
			(id, collection, content, tags, weight, retrieval_priority, importance, confidence, metadata)
		VALUES (?, 'resolutions', ?, ?, 1.0, 0.7, 0.7, 0.9, ?)
	`, resolutionID, content, tagsJSON, fmt.Sprintf(`{"resolution_for_queue_id":%d,"winner":%q,"loser":%q,"margin":%f}`, queueID, dec.WinnerID, dec.LoserID, dec.Margin))
	if err != nil {
		return fmt.Errorf("applyResolution: write resolution memory: %w", err)
	}

	// Step 1: slash the loser. ChallengeMemory has its own
	// transaction; for the atomic guarantee, we duplicate the
	// essential UPDATE here inside our transaction. We do this
	// AFTER the resolution memory write so resolutionID is in
	// scope for the loser's metadata patch.
	var exists bool
	err = tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM shared.memories WHERE id = ? AND deleted_at IS NULL)`, dec.LoserID).Scan(&exists)
	if err != nil {
		return fmt.Errorf("applyResolution: loser check: %w", err)
	}
	if exists {
		// Build a metadata patch that mirrors the existing
		// ChallengeMemory semantics: status='challenged' stored in
		// the JSON metadata column, plus weight/reinforcement
		// updated via the standard columns. Note: there's no
		// "status" column on the memories table; it's part of the
		// metadata JSON (see updateMemoryMetadataTx).
		//
		// We do a proper JSON merge in Go (not string concat with
		// `||`) so existing metadata fields are preserved. The
		// patch's keys win on collision.
		var existingMetaRaw sql.NullString
		if err := tx.QueryRow(`SELECT metadata FROM shared.memories WHERE id = ?`, dec.LoserID).Scan(&existingMetaRaw); err != nil {
			return fmt.Errorf("applyResolution: read loser metadata: %w", err)
		}
		loserPatch := map[string]interface{}{
			"status":                "challenged",
			"challenged_by_resolution": resolutionID,
			"challenged_at":         time.Now().UTC().Format(time.RFC3339),
		}
		merged := map[string]interface{}{}
		if existingMetaRaw.Valid && existingMetaRaw.String != "" && existingMetaRaw.String != "{}" {
			if err := json.Unmarshal([]byte(existingMetaRaw.String), &merged); err != nil {
				return fmt.Errorf("applyResolution: parse existing loser metadata: %w", err)
			}
		}
		for k, v := range loserPatch {
			merged[k] = v
		}
		mergedJSON, _ := json.Marshal(merged)
		_, err = tx.Exec(`
			UPDATE shared.memories
			SET metadata = ?,
			    weight = MAX(1, weight - ?),
			    retrieval_priority = MAX(0.01, retrieval_priority - ?),
			    updated_at = CAST(strftime('%s','now') AS INTEGER)
			WHERE id = ? AND deleted_at IS NULL
		`, string(mergedJSON), dec.SlashAmount, float64(dec.SlashAmount)/100.0, dec.LoserID)
		if err != nil {
			return fmt.Errorf("applyResolution: slash loser: %w", err)
		}
	}

	// Step 3: mark the queue row resolved.
	_, err = tx.Exec(`
		UPDATE shared.contradiction_log
		SET resolved_at = CURRENT_TIMESTAMP,
		    resolution_memory_id = ?
		WHERE id = ? AND resolved_at IS NULL
	`, resolutionID, queueID)
	if err != nil {
		return fmt.Errorf("applyResolution: mark resolved: %w", err)
	}

	// Step 4: write the resolution as evidence FOR the winner.
	// Arc 1 closure: the resolution itself is a strengthening
	// signal for the surviving memory. If any future agent
	// challenges the winner, the fact that it already survived a
	// contradiction (with this provenance-based verdict) adds
	// recursive weight to its confidence. Strength is 1.0 — a
	// successful resolution is the strongest possible positive
	// evidence (it's the system affirming the memory's standing).
	// The source_group is the queue ID so the evidence is
	// traceable back to the contradiction that produced it.
	//
	// Note: this lands in shared.evidence, qualifying the
	// artifact as a 'memory' under the federated scope. Future
	// agents searching for the winner's evidence trail will see
	// the resolution row.
	evidenceID := fmt.Sprintf("ev-resolution-%s-%d", dec.WinnerID, time.Now().UnixNano())
	now := time.Now().Unix()
	notes := fmt.Sprintf("Survived contradiction against %s (queue_id=%d, margin=%.3f, resolution_id=%s).",
		dec.LoserID, queueID, dec.Margin, resolutionID)
	_, err = tx.Exec(`
		INSERT INTO shared.evidence
			(id, artifact_id, artifact_type, type, source_group, strength, independence_factor, created_by, created_at, notes)
		VALUES (?, ?, 'memory', 'resolution_survived', ?, 1.0, 1.0, 'arc1-resolution-system', ?, ?)
	`, evidenceID, dec.WinnerID, fmt.Sprintf("resolution:%d", queueID), now, notes)
	if err != nil {
		return fmt.Errorf("applyResolution: write evidence: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("applyResolution: commit: %w", err)
	}
	// Arc 2: hand the resolution id back to the caller so the
	// auto-broadcast hook in ResolveOneContradiction can fan out.
	dec.ResolutionMemoryID = resolutionID
	return nil
}

// applyArbitration is the close-call path. It writes a pending theory
// (collection='theories') into shared.memories with both memory IDs
// in dependencies. The queue row stays UNRESOLVED — when the
// operator arbitrates and resolves the theory, the resolution
// command can pick up the queue row again.
//
// Note: the queue row is intentionally NOT marked resolved here. A
// close call is not a resolution; it's a triage flag that requires
// human input.
func (dm *DatabaseManager) applyArbitration(queueID int64, memoryA, memoryB string, scoreA, scoreB ProvenanceScore, margin float64) error {
	tx, err := dm.db.Begin()
	if err != nil {
		return fmt.Errorf("applyArbitration: begin: %w", err)
	}
	defer tx.Rollback()

	theoryID := fmt.Sprintf("arbitration-%s-%s-%d", memoryA, memoryB, time.Now().UnixNano())
	hypothesis := fmt.Sprintf(
		"Memory %s and memory %s are in contradiction (margin=%.3f, threshold=%.2f). "+
			"Provenance: A=%.3f (confidence=%.3f, %d ev), B=%.3f (confidence=%.3f, %d ev). "+
			"Human arbitration required.",
		memoryA, memoryB, margin, ProvenanceThreshold,
		scoreA.Score, scoreA.Confidence, int(scoreA.Reinforcement*10),
		scoreB.Score, scoreB.Confidence, int(scoreB.Reinforcement*10),
	)
	deps := []string{memoryA, memoryB}
	depsJSON, _ := json.Marshal(deps)
	tagsJSON := fmt.Sprintf(`["arbitration:contradiction:%s-%s"]`, memoryA, memoryB)

	_, err = tx.Exec(`
		INSERT INTO shared.memories
			(id, collection, content, tags, weight, retrieval_priority, importance, confidence, dependencies, metadata)
		VALUES (?, 'theories', ?, ?, 1.0, 0.5, 0.5, 0.5, ?, ?)
	`, theoryID, hypothesis, tagsJSON, string(depsJSON), fmt.Sprintf(`{"status":"pending","arbitration_for_queue_id":%d,"margin":%f,"score_a":%f,"score_b":%f}`, queueID, margin, scoreA.Score, scoreB.Score))
	if err != nil {
		return fmt.Errorf("applyArbitration: write theory: %w", err)
	}

	// The theory creation is a "marker" but the contradiction is
	// NOT yet resolved. We attach the theory ID as a sidecar so
	// the operator can see the linkage, but the queue row stays
	// open until they resolve the theory.
	_, err = tx.Exec(`
		UPDATE shared.contradiction_log
		SET detected_by = COALESCE(detected_by, '') || '|theory:' || ?
		WHERE id = ? AND resolved_at IS NULL
	`, theoryID, queueID)
	if err != nil {
		return fmt.Errorf("applyArbitration: link theory: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("applyArbitration: commit: %w", err)
	}
	return nil
}

// parseContradictionID is a defensive helper for the queue ID.
// SQLite returns it as a string when scanned into NullString; we
// convert here rather than at the scan site to keep the SELECT
// query portable. (Renamed from parseInt64 to avoid collision
// with wake_tools.go.)
func parseContradictionID(s string) (int64, error) {
	if s == "" {
		return 0, nil
	}
	var n int64
	_, err := fmt.Sscanf(s, "%d", &n)
	return n, err
}

// SanitizeEvidenceTag strips characters that would break the
// resolution_memory tag (which goes into JSON). Used by callers
// that build the tag from arbitrary memory IDs.
func SanitizeEvidenceTag(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '-' || r == '_' || r == '.' || r == ':' ||
			(r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') ||
			(r >= 'A' && r <= 'Z') {
			return r
		}
		return '_'
	}, s)
}