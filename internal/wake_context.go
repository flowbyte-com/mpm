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

// GatherWakeContext returns the most recent session's wake context.
// Returns a zero-value struct (no error) when there is no prior session —
// callers can detect this via SessionID == "".
func (dm *DatabaseManager) GatherWakeContext() (WakeContextData, error) {
	var data WakeContextData

	session, err := dm.GetLastSession()
	if errors.Is(err, sql.ErrNoRows) {
		return data, nil
	}
	if err != nil {
		return data, fmt.Errorf("get last session: %w", err)
	}
	if session == nil {
		return data, nil
	}

	data.SessionID, _ = session["session_id"].(string)
	data.ActiveMode, data.ActivePersona = readActiveState()
	data.RecentMemories = dm.recentMemories(10)
	data.RecentTopics = dm.recentTopicNames(5)
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

// recentTopicNames returns up to `limit` topic names ordered newest first.
func (dm *DatabaseManager) recentTopicNames(limit int) []string {
	rows, err := dm.SQLDB().Query(
		`SELECT name FROM topics ORDER BY created_at DESC LIMIT ?`, limit)
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
// there is no prior session — the MCP server interprets that as the "wake
// context is empty" state.
func (dm *DatabaseManager) ReadWakeContext() (string, error) {
	data, err := dm.GatherWakeContext()
	if err != nil {
		return "", err
	}
	if data.SessionID == "" {
		return "", nil
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
	return strings.Join(lines, "\n")
}
