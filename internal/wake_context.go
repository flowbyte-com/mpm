// wake_context.go - Single source of truth for wake context data and formatting.
// Both the CLI tool (`mpm call read_wake_context`) and the Go MCP server
// (cmd/mpm-mcp) call into this file so the data-gathering logic and the
// human-readable format stay in lockstep.

package internal

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// WakeContextData is the structured payload produced by GatherWakeContext.
// Callers format it for their own wire protocol (CLI returns a map for
// backward compatibility with the Python plugin; the MCP server returns a
// pre-formatted string).
type WakeContextData struct {
	SessionID      string              `json:"session_id"`
	ActiveMode     string              `json:"active_mode"`
	ActivePersona  string              `json:"active_persona"`
	RecentTopics   []string            `json:"recent_topics"`
	RecentMemories []WakeContextMemory `json:"recent_memories"`
	// AuditSummary is a one-line summary of system_audit_log activity in
	// the last 24h, or empty if no error/fatal events were logged. The
	// agent uses this as a signpost — if present, it should call
	// query_audit_log to investigate.
	AuditSummary string `json:"audit_summary,omitempty"`
	// LastHandoff is the most recent unread handoff from the previous
	// session, or nil if there is none. The handoff is marked as read
	// when surfaced here, so the same handoff is never shown twice in a
	// row. The agent uses this to continue work across restarts — the
	// summary, commitments, and open_questions fields tell it where it
	// left off, what it promised to do next, and what's still unresolved.
	LastHandoff *Handoff `json:"last_handoff,omitempty"`
}

// WakeContextMemory is the trimmed memory reference shown in wake context.
type WakeContextMemory struct {
	ID        string `json:"id"`
	Content   string `json:"content"`
	CreatedAt string `json:"created_at"`
}

// readActiveState reads {MPM_DIR}/active.json via the shared loader in
// xitl.go. Missing/unreadable file returns empty strings with no error —
// the wake context is still useful without mode/persona metadata.
func readActiveState() (mode, persona string) {
	active, err := loadActiveJSON()
	if err != nil {
		return "", ""
	}
	if len(active.Modes) > 0 {
		mode = strings.Join(active.Modes, ", ")
	}
	return mode, active.Persona
}

// GatherWakeContext returns the wake context, populating active mode/persona
// from active.json and recent memories/topics from the database even when
// there is no prior session row. Callers can detect a missing session via
// SessionID == "". The latest unread handoff (if any) is also surfaced
// and marked as read.
func (dm *DatabaseManager) GatherWakeContext() (WakeContextData, error) {
	var data WakeContextData

	session, err := dm.GetLastSession()
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return data, fmt.Errorf("get last session: %w", err)
	}
	if err == nil && session != nil {
		data.SessionID, _ = session["session_id"].(string)
	}

	// Pull the latest unread handoff. The mark-read happens here so
	// re-reading wake context (e.g. in the same session) doesn't re-show
	// the same handoff. Wake-context timestamp is the read-by token —
	// distinct from any session_id since the agent may not have one.
	h, herr := dm.MarkLatestHandoffRead("wake-context")
	if herr != nil && !errors.Is(herr, sql.ErrNoRows) {
		// Non-fatal: log the handoff read failure to audit but continue
		// with wake context. The handoff is bootstrap data; the agent
		// can still wake up without it.
		dm.LogAudit(AuditWarn, "wake_context", "handoff read failed: "+herr.Error(), "", AuditContext{})
	}
	if h != nil {
		data.LastHandoff = h
	}

	data.ActiveMode, data.ActivePersona = readActiveState()
	data.RecentMemories = dm.recentMemories(10)
	data.RecentTopics = dm.GetRecentUserTopics(5)
	data.AuditSummary = dm.AuditSummary()
	return data, nil
}

// recentMemories returns up to `limit` non-deleted memories ordered newest first.
func (dm *DatabaseManager) recentMemories(limit int) []WakeContextMemory {
	rows, err := dm.SQLDB().Query(`
		SELECT id, content, created_at FROM memories
		WHERE deleted_at IS NULL
		ORDER BY created_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil
	}
	defer rows.Close()

	var out []WakeContextMemory
	for rows.Next() {
		var m WakeContextMemory
		if err := rows.Scan(&m.ID, &m.Content, &m.CreatedAt); err != nil {
			continue
		}
		out = append(out, m)
	}
	return out
}

// GetRecentUserTopics returns up to `limit` topic names from the topics
// table, ordered newest first, with structural topics excluded.
//
// A "structural" topic is an auto-created cross-reference anchor for an
// epistemology collection (`decisions`, `theories`, or any future topic
// auto-generated to act as a navigational hub). They are distinguished
// from user topics by two characteristics:
//   - description is empty/null/'{}'   (auto-created, no human description)
//   - tags is empty/null/'[]'          (no curation, no metadata)
//
// The name allowlist ('decisions', 'theories') is defense-in-depth for
// the known structural topics — if a future operator seeds a structural
// topic with a placeholder description, the pattern still filters it.
//
// This is the single source of truth for "what counts as a user topic"
// across the system. Both the human-facing `mpm wake` CLI handler and
// the machine-facing `read_wake_context` MCP tool call into this method,
// so the structural-topic filter applies uniformly.
func (dm *DatabaseManager) GetRecentUserTopics(limit int) []string {
	rows, err := dm.SQLDB().Query(
		`SELECT name FROM topics
		 WHERE NOT (
		   (description IS NULL OR description = '' OR description = '{}')
		   AND (tags IS NULL OR tags = '' OR tags = '[]')
		 )
		 AND name NOT IN ('decisions', 'theories')
		 ORDER BY created_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			continue
		}
		out = append(out, name)
	}
	return out
}

// ReadWakeContext returns the formatted wake context string, matching the
// Python plugin's `_format_wake_context` output. Returns "" (no error) when
// no data is available — the MCP server interprets that as the "wake
// context is empty" state.
func (dm *DatabaseManager) ReadWakeContext() (string, error) {
	data, err := dm.GatherWakeContext()
	if err != nil {
		return "", err
	}
	return formatWakeContext(data), nil
}

// formatWakeContext renders WakeContextData in the human-readable format the
// Python plugin produces. Mirrors _format_wake_context in
// .claude/mpm-mcp/server.py.
func formatWakeContext(d WakeContextData) string {
	var lines []string
	if d.ActiveMode != "" {
		lines = append(lines, "**Mode:** "+d.ActiveMode)
	}
	if d.ActivePersona != "" {
		lines = append(lines, "**Persona:** "+d.ActivePersona)
	}
	if len(d.RecentTopics) > 0 {
		lines = append(lines, "**Recent Topics:** "+strings.Join(d.RecentTopics, ", "))
	}
	if len(d.RecentMemories) > 0 {
		lines = append(lines, "**Recent Memories:**")
		for i, m := range d.RecentMemories {
			if i >= 5 {
				break
			}
			content := m.Content
			if len(content) > 80 {
				content = content[:80]
			}
			ageSuffix := ""
			if len(m.CreatedAt) >= 10 {
				ageSuffix = " (" + m.CreatedAt[:10] + ")"
			}
			lines = append(lines, fmt.Sprintf("  - %s%s", content, ageSuffix))
		}
	}
	if d.AuditSummary != "" {
		lines = append(lines, "**"+d.AuditSummary+"**")
	}
	if d.LastHandoff != nil {
		lines = append(lines, formatHandoff(d.LastHandoff))
	}
	return strings.Join(lines, "\n")
}

// formatHandoff renders a Handoff as a structured block. Designed to be
// scannable but informative — the agent needs to know (1) when the last
// session was, (2) what it was doing, (3) what it committed to do, and
// (4) what's still unresolved. Anything beyond that is excess.
func formatHandoff(h *Handoff) string {
	var lines []string
	header := fmt.Sprintf("**Previous Session Handoff** (%s, %s)", h.SessionID, h.EndedState)
	if !h.EndedAt.IsZero() {
		header = fmt.Sprintf("**Previous Session Handoff** (%s, ended %s, %s)",
			h.SessionID, h.EndedAt.Format("2006-01-02 15:04 UTC"), h.EndedState)
	}
	lines = append(lines, header)
	lines = append(lines, "  - Summary: "+h.Summary)
	if len(h.Commitments) > 0 {
		lines = append(lines, "  - Commitments:")
		for _, c := range h.Commitments {
			lines = append(lines, "    - "+c)
		}
	}
	if len(h.OpenQuestions) > 0 {
		lines = append(lines, "  - Open Questions:")
		for _, q := range h.OpenQuestions {
			lines = append(lines, "    - "+q)
		}
	}
	return strings.Join(lines, "\n")
}
