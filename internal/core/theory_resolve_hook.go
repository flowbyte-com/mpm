// theory_resolve_hook.go — auto-resolve theories when memory writes
// reference them with a definitive outcome tag.
//
// v spec 2026-07-22:
//   Hook location: SaveMemoryWithContext execution path (memory_tools.go).
//   Trigger: explicit `theory:<id>` tags or `Theory <id>` content references,
//            paired with `outcome:proven` / `outcome:disproven` tags
//            (or `Theory <id> resolved PROVEN|DISPROVEN` in content).
//   Action: auto-resolve the referenced theory in the SAME transaction
//            as the memory insert (atomicity via WithTx + SaveMemoryNode).
//   Cleanup: one-shot resolve 351c6325d1ded5fa (Swiatek Wimbledon).
//
// Failure modes (each is soft — log + skip, memory still commits):
//   - Theory reference without outcome tag: no-op (don't infer outcomes).
//   - Theory already resolved: no-op (idempotent).
//   - Theory not found or wrong collection: warn + skip.
//   - Theory resolve fails inside tx: warn + skip; memory commits anyway.
//     (Memory is the primary record; theory resolution is a derived
//     side-effect. The gap is logged for retry.)
package internal

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"
)

// theoryResolution is a (theoryID, outcome) pair extracted from a
// save_to_memory payload. Built by detectTheoryResolutions.
type theoryResolution struct {
	theoryID string // 16-char hex theory ID
	outcome  string // "proven" | "disproven"
}

// theoryIDRegex matches the canonical 16-char hex memory id shape used
// for theories (and all other memory IDs).
var theoryIDRegex = regexp.MustCompile(`^[a-f0-9]{16}$`)

// outcomeFromContentRegex matches "Theory <id> resolved PROVEN|DISPROVEN"
// in memory content. Case-insensitive for "resolved" (matches natural
// writing style), case-sensitive for the outcome (so an accidental
// "Theory X resolved pending" doesn't fire).
var outcomeFromContentRegex = regexp.MustCompile(`(?i)\btheory\s+([a-f0-9]{16})\s+resolved\s+(proven|disproven)\b`)

// detectTheoryResolutions scans a memory's content + tags for theory
// references paired with a definitive outcome. Returns one Resolution
// per unique theory ID; the outcome is shared across all returned pairs
// (we don't infer different outcomes for different theories in the
// same payload — if the payload has both proven and disproven tags,
// the first-seen wins).
//
// Trigger sources:
//   - tags containing `theory:<id>` AND `outcome:proven`/`outcome:disproven`
//   - content matching `Theory <id> resolved PROVEN|DISPROVEN` regex
//
// An outcome is required. A bare `theory:<id>` tag without an outcome
// is NOT auto-resolved — we never infer outcomes from absence of data.
func detectTheoryResolutions(content string, tags []string) []theoryResolution {
	var theoryIDs []string
	var outcome string
	seenIDs := map[string]bool{}

	// Tag scan.
	for _, t := range tags {
		switch {
		case strings.HasPrefix(t, "theory:"):
			id := strings.TrimPrefix(t, "theory:")
			if theoryIDRegex.MatchString(id) && !seenIDs[id] {
				seenIDs[id] = true
				theoryIDs = append(theoryIDs, id)
			}
		case t == "outcome:proven" && outcome == "":
			outcome = "proven"
		case t == "outcome:disproven" && outcome == "":
			outcome = "disproven"
		}
	}

	// Content scan — captures both the theory ID AND the outcome in one match.
	matches := outcomeFromContentRegex.FindAllStringSubmatch(content, -1)
	for _, m := range matches {
		if len(m) != 3 {
			continue
		}
		id := m[1]
		if !theoryIDRegex.MatchString(id) {
			continue
		}
		if !seenIDs[id] {
			seenIDs[id] = true
			theoryIDs = append(theoryIDs, id)
		}
		// Content regex captures outcome in lowercase via ToLower at match site.
		if outcome == "" {
			outcome = strings.ToLower(m[2])
		}
	}

	// An outcome is required. Without it, we don't infer — return empty.
	if outcome == "" {
		return nil
	}

	out := make([]theoryResolution, 0, len(theoryIDs))
	for _, id := range theoryIDs {
		out = append(out, theoryResolution{theoryID: id, outcome: outcome})
	}
	return out
}

// resolveTheoryOnNode applies a theory resolution to the supplied DBNode.
// Works inside or outside a transaction — when called inside WithTx, the
// UPDATE participates in the parent transaction's commit/rollback.
//
// Mirrors the standalone ResolveTheory (epistemology_tools.go) but takes
// a DBNode so the call can join an in-flight tx. Same patch shape:
// {status, conclusion, resolved_at}. The +1 weight reinforcement is
// inlined as part of the UPDATE so it lands in the same statement and
// is also subject to the same idempotency guard.
//
// Idempotent: the WHERE clause filters on status='pending', so a
// second resolve on the same theory is a no-op (rows=0, no error).
// We still return a non-error result with RowsAffected=0 so callers
// can detect "already resolved" and log it.
func resolveTheoryOnNode(node DBNode, theoryID, conclusion, newStatus string) error {
	if newStatus != "proven" && newStatus != "disproven" {
		return fmt.Errorf("newStatus must be 'proven' or 'disproven' (got %q)", newStatus)
	}
	if !theoryIDRegex.MatchString(theoryID) {
		return fmt.Errorf("theoryID %q is not a 16-char hex id", theoryID)
	}

	now := time.Now().UTC().Format(time.RFC3339)
	patch := map[string]interface{}{
		"status":      newStatus,
		"conclusion":  conclusion,
		"resolved_at": now,
		"resolved_by": "save_to_memory:theory_resolve_hook",
		"resolved_by_via": "auto",
	}
	patchJSON, err := json.Marshal(patch)
	if err != nil {
		return fmt.Errorf("marshal patch: %w", err)
	}

	result, err := node.ExecTracked(`
		UPDATE memories
		SET metadata = json_patch(COALESCE(metadata, '{}'), ?),
		    weight = MIN(weight + 1, 100),
		    last_accessed_at = CAST(strftime('%s','now') AS INTEGER), runtime_seconds_since_access = 0, runtime_last_accrued_at = CAST(strftime('%s','now') AS INTEGER)
		WHERE id = ? AND collection = 'theories' AND deleted_at IS NULL
		  AND json_extract(metadata, '$.status') = 'pending'
	`, 0, string(patchJSON), theoryID)
	if err != nil {
		return fmt.Errorf("resolve theory: %w", err)
	}
	rows, _ := result.RowsAffected()
	if rows == 0 {
		// Theory is non-pending (already resolved), deleted, or not found.
		// Returning a sentinel so the caller can distinguish "hard failure"
		// from "already done".
		return fmt.Errorf("theory %s not pending (already resolved, deleted, or not found)", theoryID)
	}

	slog.Info("theory-resolve hook: theory resolved",
		"theory_id", theoryID,
		"new_status", newStatus,
		"conclusion", conclusion,
	)
	return nil
}

// lookupTheoryStatus reads the current status of a theory directly from
// the DB. Used for pre-flight validation in SaveMemoryWithContext — we
// want to know which theories are worth attempting to resolve BEFORE we
// open a transaction.
//
// Returns:
//   - (status, true, nil) on a successful read of an existing row
//   - ("", false, nil) when the memory is missing or soft-deleted
//   - ("", false, err) on real DB errors
//
// The second return is "found" — false means "stop trying to resolve
// this one" (no error worth surfacing).
func (dm *DatabaseManager) lookupTheoryStatus(theoryID string) (string, bool, error) {
	var status string
	var collection string
	err := dm.db.QueryRow(`
		SELECT json_extract(metadata, '$.status'), collection
		FROM memories
		WHERE id = ? AND deleted_at IS NULL
	`, theoryID).Scan(&status, &collection)
	if err != nil {
		if err.Error() == "sql: no rows in result set" {
			return "", false, nil
		}
		return "", false, err
	}
	if collection != "theories" {
		return "", false, fmt.Errorf("memory %s is not a theory (collection: %s)", theoryID, collection)
	}
	return status, true, nil
}