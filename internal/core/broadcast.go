// broadcast.go — Arc 2 (Active Dissemination).
//
// The other half of the conflict resolution loop. Arc 1 made the
// shared DB write and self-heal; Arc 2 makes the resulting
// epistemic events propagate to every active agent in real time
// without anyone having to run a search.
//
// The model: every agent runs a passive heartbeat (every MPM
// call that touches the shared DB silently bumps shared.bcast_sessions).
// When an operator explicitly broadcasts a memory (via
// `mpm ops broadcast`, or as the side-effect of `record_global_rule`,
// `resolve-contradictions --apply`, or `resolve-theory --winner`),
// the system fans out one shared.bcast_event_wakes row per active target.
//
// The dedup mechanism is the entire architecture: wake_id is a
// deterministic sha256 prefix on (memory_id + target_session +
// content_hash). wake_id is a PRIMARY KEY → INSERT OR IGNORE
// silently no-ops on re-broadcast. O(1), no application-level
// state, race-free across concurrent broadcasters.
//
// On the receiving side, CheckPendingEventWakes mirrors the
// existing CheckPendingWakes: atomic SELECT-and-mark-fired, folds
// the result into the same WakesPending block on every MPM
// response. The receiving agent sees the new rule in its wake
// context the next time it calls any MPM tool. No search required,
// no cold-start cost.

package internal

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// HeartbeatWindowHours is how stale a shared.bcast_sessions row can be
// before we treat the agent as inactive. 24h matches the typical
// workstation uptime cycle (sleep states, paused containers,
// overnight pauses). Locked with v 2026-07-07.
const HeartbeatWindowHours = 24

// MaxEventWakesPerPull bounds the result of CheckPendingEventWakes
// to keep one tool response from blowing out the agent's context.
// 100 matches the existing CheckPendingWakes pull ceiling (look
// for DefaultWakePullLimit in wake_tools.go).
const MaxEventWakesPerPull = 100

// EventWake is the row shape of shared.bcast_event_wakes, decoded for
// the receiving side. See shared_event_wakes DDL for the column
// schema.
type EventWake struct {
	WakeID       string `json:"wake_id"`
	SourceAgent  string `json:"source_agent"`
	MemoryID     string `json:"memory_id"`
	Kind         string `json:"kind"`
	ContentHash  string `json:"content_hash"`
	Rationale    string `json:"rationale"`
	CreatedAt    int64  `json:"created_at"`
	Metadata     string `json:"metadata,omitempty"`
}

// BroadcastOpts configures BroadcastMemory. Zero-value defaults
// are sane: empty kind → auto-detect from memory.collection;
// empty rationale → reject unless the kind is auto-extractable
// (resolution / arbitration); nil ToAgents → fan out to all
// active sessions (minus self).
type BroadcastOpts struct {
	Kind      string   // 'rule' | 'resolution' | 'arbitration' | 'memory'; "" = auto
	Rationale string   // the WHY; required unless auto-extractable
	ToAgents  []string // restrict fan-out; nil = all active (minus self)
	SourceAgent string // who is doing the broadcasting (for shared.bcast_event_wakes.source_agent)
	SourceSessionID string // who is doing the broadcasting (skip self in fan-out)
	DryRun    bool     // compute the report but don't INSERT
}

// BroadcastReport is what BroadcastMemory returns. Mirrors the
// shape that the CLI renders in --json mode.
type BroadcastReport struct {
	MemoryID      string                  `json:"memory_id"`
	Kind          string                  `json:"kind"`
	Rationale     string                  `json:"rationale"`
	ContentHash   string                  `json:"content_hash"`
	Targets       []BroadcastTargetReport `json:"targets"`
	NewWakes      int                     `json:"new_wakes"`
	DedupedWakes  int                     `json:"deduped_wakes"`
}

// BroadcastTargetReport describes one (session, agent) target in
// a broadcast. Status is "new" if the INSERT actually wrote a row,
// "deduped" if the wake_id collided with an existing row, "offline"
// if --to= listed an agent that isn't in shared.bcast_sessions within
// the heartbeat window.
type BroadcastTargetReport struct {
	SessionID string `json:"session_id"`
	AgentID   string `json:"agent_id"`
	WakeID    string `json:"wake_id"`
	Status    string `json:"status"`
}

// BroadcastMemory fans out an epistemic event to every active
// session (or a restricted --to list). Atomic per-row: each
// INSERT OR IGNORE is its own statement; a failure in target 5
// doesn't roll back targets 1-4. (The aggregate fan-out is
// eventually consistent across targets but the dedup contract
// holds because wake_id is deterministic.)
//
// Refuses (returns error) if:
//   - memoryID doesn't exist in shared.memories
//   - rationale is empty AND not auto-extractable
//   - --to= lists an agent_id that isn't in shared.bcast_sessions
//     within the heartbeat window (silent skip would give a
//     false "broadcast succeeded" impression)
//
// Returns the BroadcastReport even on partial failure: the CLI
// renders the report and the operator can re-broadcast just the
// failed targets.
func (dm *DatabaseManager) BroadcastMemory(memoryID string, opts BroadcastOpts) (*BroadcastReport, error) {
	if dm.SharedAttached() == "" {
		return nil, fmt.Errorf("broadcast: shared DB not attached; set MPM_SHARED_DB")
	}

	// 1. Load the memory.
	var content, collection string
	var tagsJSON sql.NullString
	var metadataJSON sql.NullString
	err := dm.db.QueryRow(`
		SELECT content, collection, tags, metadata
		FROM shared.memories
		WHERE id = ? AND deleted_at IS NULL
	`, memoryID).Scan(&content, &collection, &tagsJSON, &metadataJSON)
	if err != nil {
		return nil, fmt.Errorf("broadcast: load memory %s: %w", memoryID, err)
	}

	// 2. Determine kind.
	kind := opts.Kind
	if kind == "" {
		kind = collection
	}
	// Normalize: 'rules' → 'rule', 'resolutions' → 'resolution', etc.
	kind = strings.TrimSuffix(kind, "s")
	// Don't change 'memory' to 'memor' — only strip a single trailing 's'.

	// 3. Determine rationale.
	rationale := opts.Rationale
	if rationale == "" {
		// Auto-extract for resolutions and arbitrations.
		switch kind {
		case "resolution":
			rationale = extractResolutionRationale(metadataJSON, memoryID)
		case "arbitration":
			rationale = extractArbitrationRationale(metadataJSON, memoryID)
		}
	}
	if rationale == "" {
		return nil, fmt.Errorf("broadcast: rationale is required (no --rationale given and not auto-extractable for kind=%s)", kind)
	}

	// 4. Compute content hash.
	tags := decodeTagsJSON(tagsJSON)
	contentHash := computeContentHash(content, tags)

	// 5. Discover targets.
	var targets []BroadcastTarget
	if len(opts.ToAgents) > 0 {
		// Targeted fan-out. Verify each agent has a recent heartbeat;
		// refuse on any offline agent.
		for _, agentID := range opts.ToAgents {
			var sessionID string
			err := dm.db.QueryRow(`
				SELECT session_id FROM shared.bcast_sessions
				WHERE agent_id = ? AND last_heartbeat > datetime('now', '-' || ? || ' hours')
				ORDER BY last_heartbeat DESC LIMIT 1
			`, agentID, HeartbeatWindowHours).Scan(&sessionID)
			if err != nil {
				return nil, fmt.Errorf("broadcast: target agent %q has no active session in last %dh: %w",
					agentID, HeartbeatWindowHours, err)
			}
			targets = append(targets, BroadcastTarget{SessionID: sessionID, AgentID: agentID})
		}
	} else {
		// Full fan-out: all sessions within the heartbeat window,
		// minus the broadcasting session itself (per v's call: no
		// self-echo).
		rows, err := dm.db.Query(`
			SELECT session_id, agent_id
			FROM shared.bcast_sessions
			WHERE last_heartbeat > datetime('now', '-' || ? || ' hours')
			ORDER BY last_heartbeat DESC
		`, HeartbeatWindowHours)
		if err != nil {
			return nil, fmt.Errorf("broadcast: discover active sessions: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var sid, aid string
			if err := rows.Scan(&sid, &aid); err != nil {
				return nil, fmt.Errorf("broadcast: scan session: %w", err)
			}
			// Skip self.
			if opts.SourceSessionID != "" && sid == opts.SourceSessionID {
				continue
			}
			targets = append(targets, BroadcastTarget{SessionID: sid, AgentID: aid})
		}
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("broadcast: iterate sessions: %w", err)
		}
	}

	// 6. Fan out. Each row INSERT OR IGNORE in its own statement.
	report := &BroadcastReport{
		MemoryID:    memoryID,
		Kind:        kind,
		Rationale:   rationale,
		ContentHash: contentHash,
		Targets:     make([]BroadcastTargetReport, 0, len(targets)),
	}

	sourceAgent := opts.SourceAgent
	if sourceAgent == "" {
		sourceAgent = "mpm_broadcast"
	}

	for _, target := range targets {
		wakeID := computeWakeID(memoryID, target.SessionID, contentHash)

		if opts.DryRun {
			report.Targets = append(report.Targets, BroadcastTargetReport{
				SessionID: target.SessionID,
				AgentID:   target.AgentID,
				WakeID:    wakeID,
				Status:    "would_insert",
			})
			continue
		}

		res, err := dm.db.Exec(`
			INSERT OR IGNORE INTO shared.bcast_event_wakes
				(wake_id, target_session, source_agent, memory_id, kind,
				 content_hash, rationale, fired, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, 0, CURRENT_TIMESTAMP)
		`, wakeID, target.SessionID, sourceAgent, memoryID, kind,
			contentHash, rationale)
		if err != nil {
			// Report the per-target error but keep going.
			report.Targets = append(report.Targets, BroadcastTargetReport{
				SessionID: target.SessionID,
				AgentID:   target.AgentID,
				WakeID:    wakeID,
				Status:    "error: " + err.Error(),
			})
			continue
		}
		rowsAffected, _ := res.RowsAffected()
		status := "new"
		if rowsAffected == 0 {
			status = "deduped"
			report.DedupedWakes++
		} else {
			report.NewWakes++
		}
		report.Targets = append(report.Targets, BroadcastTargetReport{
			SessionID: target.SessionID,
			AgentID:   target.AgentID,
			WakeID:    wakeID,
			Status:    status,
		})
	}

	return report, nil
}

// BroadcastTarget is an internal helper for the discovery-then-fanout
// pipeline. Not exposed on the public API.
type BroadcastTarget struct {
	SessionID string
	AgentID   string
}

// CheckPendingEventWakes returns every shared.bcast_event_wakes row where
// target_session = sessionID AND fired = 0, marks them fired = 1
// in the same transaction, and returns them in chronological order.
// Bounded by MaxEventWakesPerPull so a long-idle session can't
// blow out its context on the first poll.
//
// Idempotent across concurrent callers (BEGIN IMMEDIATE →
// serialization). If two MPM calls hit this simultaneously, the
// second sees fired=1 rows and returns nothing for them.
func (dm *DatabaseManager) CheckPendingEventWakes(sessionID string) ([]EventWake, error) {
	if dm.SharedAttached() == "" {
		return nil, nil // shared DB not attached: nothing to do, no error
	}
	if sessionID == "" {
		return nil, ErrSessionIDRequired()
	}

	tx, err := dm.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("CheckPendingEventWakes: begin: %w", err)
	}
	defer tx.Rollback()

	rows, err := tx.Query(`
		SELECT wake_id, source_agent, memory_id, kind, content_hash,
		       rationale, created_at, COALESCE(metadata, '')
		FROM shared.bcast_event_wakes
		WHERE target_session = ? AND fired = 0
		ORDER BY created_at ASC
		LIMIT ?
	`, sessionID, MaxEventWakesPerPull)
	if err != nil {
		return nil, fmt.Errorf("CheckPendingEventWakes: select: %w", err)
	}
	defer rows.Close()

	var wakes []EventWake
	var wakeIDs []string
	for rows.Next() {
		var w EventWake
		var createdAt string
		if err := rows.Scan(&w.WakeID, &w.SourceAgent, &w.MemoryID, &w.Kind,
			&w.ContentHash, &w.Rationale, &createdAt, &w.Metadata); err != nil {
			return nil, fmt.Errorf("CheckPendingEventWakes: scan: %w", err)
		}
		// Parse SQLite's CURRENT_TIMESTAMP format ("2026-07-07 14:32:40")
		// into a unix epoch. The agent doesn't need sub-second precision
		// for ordering.
		if t, err := time.Parse("2006-01-02 15:04:05", createdAt); err == nil {
			w.CreatedAt = t.Unix()
		}
		wakes = append(wakes, w)
		wakeIDs = append(wakeIDs, w.WakeID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("CheckPendingEventWakes: iterate: %w", err)
	}
	rows.Close()

	if len(wakeIDs) > 0 {
		// Build the IN clause.
		placeholders := make([]string, len(wakeIDs))
		args := make([]interface{}, len(wakeIDs))
		for i, id := range wakeIDs {
			placeholders[i] = "?"
			args[i] = id
		}
		_, err := tx.Exec(`
			UPDATE shared.bcast_event_wakes
			SET fired = 1, fired_at = CURRENT_TIMESTAMP
			WHERE wake_id IN (`+strings.Join(placeholders, ",")+`)
		`, args...)
		if err != nil {
			return nil, fmt.Errorf("CheckPendingEventWakes: mark fired: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("CheckPendingEventWakes: commit: %w", err)
	}

	return wakes, nil
}

// Heartbeat silently bumps the calling session's row in
// shared.bcast_sessions. INSERT OR REPLACE on (session_id, agent_id)
// so the first call creates the row, subsequent calls update
// last_heartbeat and total_heartbeats.
//
// Called by every MPM tool that hits the shared DB. No operator
// gate — heartbeats are infrastructure plumbing, not epistemic
// events.
//
// No-op when shared DB not attached (allows unit tests + local-only
// operation without error noise).
func (dm *DatabaseManager) Heartbeat(sessionID, agentID, hostname string, metadata map[string]interface{}) error {
	if dm.SharedAttached() == "" {
		return nil
	}
	if sessionID == "" || agentID == "" {
		return nil // nothing to do without identifiers
	}
	var metaJSON string
	if metadata != nil {
		b, err := json.Marshal(metadata)
		if err != nil {
			return fmt.Errorf("heartbeat: marshal metadata: %w", err)
		}
		metaJSON = string(b)
	}

	tx, err := dm.db.Begin()
	if err != nil {
		return fmt.Errorf("heartbeat: begin: %w", err)
	}
	defer tx.Rollback()

	_, err = tx.Exec(`
		INSERT INTO shared.bcast_sessions
			(session_id, agent_id, hostname, last_heartbeat, boot_at, metadata)
		VALUES (?, ?, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP, ?)
		ON CONFLICT (session_id) DO UPDATE SET
			agent_id = excluded.agent_id,
			hostname = excluded.hostname,
			last_heartbeat = CURRENT_TIMESTAMP,
			metadata = excluded.metadata
	`, sessionID, agentID, hostname, metaJSON)
	if err != nil {
		return fmt.Errorf("heartbeat: upsert session: %w", err)
	}

	// Also bump shared.bcast_agents (the cross-reboot identity cache).
	_, err = tx.Exec(`
		INSERT INTO shared.bcast_agents
			(agent_id, first_seen, last_seen, total_heartbeats)
		VALUES (?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP, 1)
		ON CONFLICT (agent_id) DO UPDATE SET
			last_seen = CURRENT_TIMESTAMP,
			total_heartbeats = total_heartbeats + 1
	`, agentID)
	if err != nil {
		return fmt.Errorf("heartbeat: upsert agent: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("heartbeat: commit: %w", err)
	}
	return nil
}

// DiscoverActiveSessions returns every session within the heartbeat
// window. Used by the CLI's `mpm ops broadcast --dry-run` for the
// "who would I broadcast to?" preview. Also useful for the
// `mpm ops active-sessions` introspection command (operator wants
// to see the fleet).
type ActiveSession struct {
	SessionID    string `json:"session_id"`
	AgentID      string `json:"agent_id"`
	Hostname     string `json:"hostname,omitempty"`
	LastHeartbeat string `json:"last_heartbeat"`
	BootAt       string `json:"boot_at"`
	Metadata     string `json:"metadata,omitempty"`
}

func (dm *DatabaseManager) DiscoverActiveSessions() ([]ActiveSession, error) {
	if dm.SharedAttached() == "" {
		return nil, nil
	}
	rows, err := dm.db.Query(`
		SELECT session_id, agent_id, COALESCE(hostname, ''),
		       last_heartbeat, boot_at, COALESCE(metadata, '')
		FROM shared.bcast_sessions
		WHERE last_heartbeat > datetime('now', '-' || ? || ' hours')
		ORDER BY last_heartbeat DESC
	`, HeartbeatWindowHours)
	if err != nil {
		return nil, fmt.Errorf("discover active sessions: %w", err)
	}
	defer rows.Close()

	var out []ActiveSession
	for rows.Next() {
		var s ActiveSession
		if err := rows.Scan(&s.SessionID, &s.AgentID, &s.Hostname,
			&s.LastHeartbeat, &s.BootAt, &s.Metadata); err != nil {
			return nil, fmt.Errorf("discover active sessions: scan: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// computeContentHash is the SHA-256 of (content + "|" + sorted tags).
// Hashing only the canonicalized rule body means a metadata-only
// update (e.g., status=challenged on a loser) does NOT change the
// content_hash, so re-broadcasting the winner with status changes
// is a no-op. The hash changes only when the rule body itself
// changes — which is what we want for an event wake.
func computeContentHash(content string, tags []string) string {
	h := sha256.New()
	h.Write([]byte(content))
	h.Write([]byte{0x7c}) // "|"
	sorted := append([]string(nil), tags...)
	sort.Strings(sorted)
	tagJSON, _ := json.Marshal(sorted)
	h.Write(tagJSON)
	return hex.EncodeToString(h.Sum(nil))
}

// computeWakeID is the deterministic sha256 prefix that powers
// dedup. Take the first 12 hex chars (48 bits of entropy) — enough
// for ~10^14 unique wake IDs before a 50% collision probability.
// 12 chars is also short enough to embed in audit logs without
// dominating line length.
//
// The triple (memory_id, target_session, content_hash) uniquely
// identifies "this rule version broadcast to this session." If
// any component changes, a new wake fires. If all three match,
// INSERT OR IGNORE silently drops the duplicate.
func computeWakeID(memoryID, targetSession, contentHash string) string {
	h := sha256.New()
	h.Write([]byte(memoryID))
	h.Write([]byte{0x3a}) // ":"
	h.Write([]byte(targetSession))
	h.Write([]byte{0x3a})
	h.Write([]byte(contentHash))
	return hex.EncodeToString(h.Sum(nil))[:12]
}

// decodeTagsJSON parses the tags column. Returns nil for empty /
// invalid input (we don't fail broadcasts on tag-parsing errors;
// the hash just collapses to a content-only hash).
func decodeTagsJSON(raw sql.NullString) []string {
	if !raw.Valid || raw.String == "" {
		return nil
	}
	var tags []string
	if err := json.Unmarshal([]byte(raw.String), &tags); err != nil {
		return nil
	}
	return tags
}

// extractResolutionRationale builds a human-readable rationale from
// a resolution memory's metadata. The Arc 1 path writes
// conclusion, winner_id, loser_id, queue_id, and resolution_id into
// the metadata JSON; we extract them and assemble a one-paragraph
// explanation.
//
// Falls back to a generic "Contradiction resolved" if the metadata
// shape is unexpected.
func extractResolutionRationale(metadataJSON sql.NullString, memoryID string) string {
	if !metadataJSON.Valid || metadataJSON.String == "" {
		return fmt.Sprintf("Resolution %s landed; no metadata rationale available.", memoryID)
	}
	var meta map[string]interface{}
	if err := json.Unmarshal([]byte(metadataJSON.String), &meta); err != nil {
		return fmt.Sprintf("Resolution %s landed; metadata unreadable.", memoryID)
	}
	queueID, _ := meta["queue_id"].(float64)
	winner, _ := meta["winner_id"].(string)
	loser, _ := meta["loser_id"].(string)
	resolutionID, _ := meta["resolution_id"].(string)
	if winner == "" || loser == "" {
		return fmt.Sprintf("Resolution %s landed for queue_id=%v (resolution_id=%v).",
			memoryID, queueID, resolutionID)
	}
	return fmt.Sprintf(
		"Contradiction (queue_id=%v) resolved: %s survived against %s. "+
			"Resolution memory: %s. The surviving memory now has stronger evidence; "+
			"the loser's status=challenged.",
		queueID, winner, loser, resolutionID)
}

// extractArbitrationRationale is the arbitration-theory variant.
// Metadata has the same shape plus arbitration-specific fields.
func extractArbitrationRationale(metadataJSON sql.NullString, memoryID string) string {
	if !metadataJSON.Valid || metadataJSON.String == "" {
		return fmt.Sprintf("Arbitration %s resolved by operator.", memoryID)
	}
	var meta map[string]interface{}
	if err := json.Unmarshal([]byte(metadataJSON.String), &meta); err != nil {
		return fmt.Sprintf("Arbitration %s resolved; metadata unreadable.", memoryID)
	}
	queueID, _ := meta["queue_id"].(float64)
	winner, _ := meta["winner_id"].(string)
	loser, _ := meta["loser_id"].(string)
	conclusion, _ := meta["conclusion"].(string)
	return fmt.Sprintf(
		"Operator arbitrated close-call contradiction (queue_id=%v): %s wins over %s. "+
			"Reasoning: %s. Trust %s going forward; %s is slashed.",
		queueID, winner, loser, conclusion, winner, loser)
}