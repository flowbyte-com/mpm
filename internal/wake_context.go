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
	// GlobalRules is the list of shared (cross-agent) house rules that
	// apply to every agent on this workstation. Populated by
	// GatherWakeContext when MPM_SHARED_DB is attached and the shared
	// DB has any is_global rows. Empty in local-only mode (the default).
	// The agent reads these on wake to know the conventions, voice
	// rules, and operator overrides before it starts working.
	GlobalRules []WakeContextRule `json:"global_rules,omitempty"`
}

// WakeContextMemory is the trimmed memory reference shown in wake context.
type WakeContextMemory struct {
	ID        string `json:"id"`
	Content   string `json:"content"`
	CreatedAt string `json:"created_at"`
}

// WakeContextRule is a single shared house rule. Trimmed to content +
// weight so the wake context payload stays small even with hundreds
// of rules. The agent calls query_global_rules for the full row when
// it needs the metadata / weight context.
type WakeContextRule struct {
	Content string `json:"content"`
	Weight  int    `json:"weight"`
}

// readActiveState reads {MPM_DIR}/active.json via the shared loader in
// xitl.go. Missing/unreadable file returns empty strings with no error —
// the wake context is still useful without mode/persona metadata.
func readActiveState() (mode, persona string) {
	active, err := LoadActiveJSON()
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

	// Phase 2c (this commit): surface shared global rules in wake
	// context. Cheap to query (lazy-backfilled FTS) and high-signal —
	// every agent on the workstation sees the same house rules on
	// wake. Skipped silently when no shared DB is attached (local-only
	// mode is the default).
	if rules, _ := dm.QueryGlobalRules("", 10); len(rules) > 0 {
		data.GlobalRules = make([]WakeContextRule, 0, len(rules))
		for _, r := range rules {
			content, _ := r["content"].(string)
			weight := 0
			if w, ok := r["weight"].(int); ok {
				weight = w
			}
			data.GlobalRules = append(data.GlobalRules, WakeContextRule{
				Content: content,
				Weight:  weight,
			})
		}
	}

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
	if len(d.GlobalRules) > 0 {
		lines = append(lines, "**Global Rules (shared across all agents on this workstation):**")
		for _, r := range d.GlobalRules {
			content := r.Content
			if len(content) > 100 {
				content = content[:100] + "…"
			}
			lines = append(lines, fmt.Sprintf("  - [w=%d] %s", r.Weight, content))
		}
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

// auditSummaryRich is the source of truth for wake-context audit
// surfacing. See AuditSummary() in audit.go for the contract.
//
// Shape:
//
//	Audit Summary (Last 7 Days):
//	  - N errors, M warnings logged.
//	  - Active Clusters (Unknown):
//	    * [component] K events since YYYY-MM-DD (ID: cluster_key)
//	  - (X known cluster(s) already tracked in active theories/decisions).
//
// Or, if nothing to surface: "" (caller skips the line entirely).
func (dm *DatabaseManager) auditSummaryRich() string {
	if dm == nil || dm.db == nil {
		return ""
	}

	// --- Headline: raw error/warning counts in 7d window ---
	// COALESCE(SUM(...), 0) is required: when the 7d window has zero
	// matching rows, SUM() returns NULL and `Scan(&int)` errors. This
	// was the bug that made the entire summary return "" on a fresh
	// DB or post-prune state. NULL → 0 is the correct semantic.
	var errCount, warnCount int
	row := dm.db.QueryRow(`
		SELECT
			COALESCE(SUM(CASE WHEN level = 'error' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN level = 'warn'  THEN 1 ELSE 0 END), 0)
		FROM system_audit_log
		WHERE created_at >= datetime('now', '-7 days')`)
	if err := row.Scan(&errCount, &warnCount); err != nil {
		// Non-fatal: degrade to silent so a transient DB hiccup never
		// floods wake context. The agent still has query_audit_log.
		return ""
	}

	// --- Fetch active clusters (incl. expired snoozes) above threshold ---
	clusterRows, err := dm.db.Query(`
		SELECT cluster_key, component, count, first_seen
		FROM audit_cluster_proposals
		WHERE count >= ?
		  AND (status = 'active'
		       OR (status = 'snoozed' AND snooze_until < datetime('now')))
		ORDER BY count DESC, component ASC`,
		ClusterThreshold)
	if err != nil {
		return ""
	}
	type clusterRow struct {
		key       string
		component string
		count     int
		firstSeen string
	}
	var clusters []clusterRow
	for clusterRows.Next() {
		var c clusterRow
		if err := clusterRows.Scan(&c.key, &c.component, &c.count, &c.firstSeen); err != nil {
			continue
		}
		clusters = append(clusters, c)
	}
	clusterRows.Close()

	// --- Dedup: split clusters into known vs unknown ---
	// Known = cluster_key appears in:
	//   - pending theories (status='pending')
	//   - recent decisions (last 30d)
	//   - resolved theories (status='proven' or 'disproven') —
	//     keep these known so a closed loop isn't re-investigated.
	// The LIKE lookup is on memories.content (free text) because the
	// schema doesn't have a cluster_key tag column. Escape % and _
	// from cluster_key to harden against future component names that
	// contain SQL LIKE wildcards.
	var knownClusters []clusterRow
	var unknownClusters []clusterRow
	if len(clusters) > 0 {
		for _, c := range clusters {
			matched, err := dm.clusterKeyKnownByEpistemology(c.key)
			if err != nil {
				// Treat lookup failures as 'unknown' — surfacing more
				// is safer than hiding a real cluster due to a query bug.
				unknownClusters = append(unknownClusters, c)
				continue
			}
			if matched {
				knownClusters = append(knownClusters, c)
			} else {
				unknownClusters = append(unknownClusters, c)
			}
		}
	}

	// --- Build output ---
	var totalEvents = errCount + warnCount
	hasHeadline := totalEvents > 0
	hasUnknowns := len(unknownClusters) > 0
	hasKnowns := len(knownClusters) > 0
	if !hasHeadline && !hasUnknowns && !hasKnowns {
		return ""
	}

	var lines []string
	lines = append(lines, "Audit Summary (Last 7 Days):")

	if hasHeadline {
		lines = append(lines, fmt.Sprintf("- %d errors, %d warnings logged.", errCount, warnCount))
	} else {
		lines = append(lines, "- No errors or warnings logged.")
	}

	if hasUnknowns {
		lines = append(lines, "- Active Clusters (Unknown):")
		for _, c := range unknownClusters {
			firstSeen := c.firstSeen
			if len(firstSeen) >= 10 {
				firstSeen = firstSeen[:10]
			}
			lines = append(lines, fmt.Sprintf("  * [%s] %d events since %s (ID: %s)",
				c.component, c.count, firstSeen, c.key))
		}
	}

	if hasKnowns {
		// Single low-noise summary line — the per-cluster detail for
		// known clusters is deliberately omitted because the agent
		// can look up the theory/decision by cluster_key if it wants
		// more. This is the "less auto-noise" voice from handoff.go.
		if len(knownClusters) == 1 {
			lines = append(lines, "- (1 known cluster already tracked in active theories/decisions.)")
		} else {
			lines = append(lines, fmt.Sprintf("- (%d known clusters already tracked in active theories/decisions.)", len(knownClusters)))
		}
	}

	return strings.Join(lines, "\n")
}

// clusterKeyKnownByEpistemology returns true if cluster_key appears in
// the content of any pending theory, recent decision (last 30d), or
// resolved theory. Uses LIKE with explicit ESCAPE to harden against
// future component names that contain SQL LIKE wildcards (% or _).
func (dm *DatabaseManager) clusterKeyKnownByEpistemology(clusterKey string) (bool, error) {
	if dm == nil || dm.db == nil {
		return false, fmt.Errorf("db not initialized")
	}
	if clusterKey == "" {
		return false, nil
	}
	// Escape SQL LIKE wildcards: % and _. Go's strings.NewReplacer is
	// faster than regex for this trivial substitution.
	escaped := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(clusterKey)
	pattern := "%" + escaped + "%"

	var matched int
	// Single query covers all three categories. Pending theories are
	// status='pending'. Resolved theories are status IN ('proven',
	// 'disproven'). Decisions are 'recent' (last 30d). The LIKE is on
	// content because theories/decisions don't carry cluster_key as a
	// structured field — the agent pastes the cluster_key into the
	// rationale/context when proposing.
	row := dm.db.QueryRow(`
		SELECT
			EXISTS(
				SELECT 1 FROM memories
				WHERE collection = 'theories'
				  AND json_extract(metadata, '$.status') = 'pending'
				  AND content LIKE ? ESCAPE '\'
			)
			OR EXISTS(
				SELECT 1 FROM memories
				WHERE collection = 'theories'
				  AND json_extract(metadata, '$.status') IN ('proven','disproven')
				  AND content LIKE ? ESCAPE '\'
			)
			OR EXISTS(
				SELECT 1 FROM memories
				WHERE collection = 'decisions'
				  AND created_at >= datetime('now', '-30 days')
				  AND content LIKE ? ESCAPE '\'
			)`, pattern, pattern, pattern)
	if err := row.Scan(&matched); err != nil {
		return false, err
	}
	return matched != 0, nil
}
