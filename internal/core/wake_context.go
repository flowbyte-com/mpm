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
	SessionID        string              `json:"session_id"`
	ActiveMode       string              `json:"active_mode"`
	ActivePersona    string              `json:"active_persona"`
	RecentTopics     []string            `json:"recent_topics"`
	RecentMemories   []WakeContextMemory `json:"recent_memories"`
	RecentMilestones []WakeContextMemory `json:"recent_milestones"`
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
	// ScratchpadOrphans is the agent-facing summary of unpromoted
	// scratchpad rows from previous sessions. Empty when no orphans
	// exist (the common case). Populated by GatherWakeContext via
	// ScratchpadOrphansSummary(). Each orphan carries [Fresh] /
	// [Dormant] / [Expired] age-tag, the session_id, and a 200-char
	// truncated thesis preview. The agent's options are: promote
	// (promote_scratchpad), amend (flush_scratchpad with same
	// session_id), or discard (discard_scratchpad).
	ScratchpadOrphans string `json:"scratchpad_orphans,omitempty"`
	// EpistemicPressure summarises the substrate's cognitive load:
	// raw memories pending compaction vs durable lessons present.
	// Surfaced on every wake so the agent feels its own cognitive
	// pressure without polling (proprioception). Threshold lives in
	// system_config under key "compaction.raw_threshold" (default 100).
	// Exceeded=true is the agent's signal that a compact_epistemology
	// call is warranted; Ratio is a secondary signal (raw:lesson
	// density) clamped to 0 when LessonCount == 0 to avoid
	// divide-by-zero NaN/Inf in the JSON response.
	EpistemicPressure EpistemicPressureData `json:"epistemic_pressure"`
	// AvailableSkills is the lightweight catalogue of skills the agent
	// has access to. At most TopSkillsInWake entries are surfaced in
	// weight order. Populated by GatherWakeContext when the skill feature
	// is enabled; empty otherwise.
	AvailableSkills []SkillSummary `json:"available_skills,omitempty"`
}

// EpistemicPressureData is the structured payload of the substrate's
// cognitive load surface. All fields are scalar and cheap to compute;
// no joins, no cluster extraction (that's deferred to the
// compact_epistemology tool in Phase 2 of the compaction pipeline).
type EpistemicPressureData struct {
	RawCount    int     `json:"raw_count"`
	LessonCount int     `json:"lesson_count"`
	Ratio       float64 `json:"ratio"`
	Threshold   int     `json:"threshold"`
	Exceeded    bool    `json:"exceeded"`
	// LastCompactedAt is the RFC3339 timestamp of the most recent
	// compact_epistemology commit. Empty string when no compaction
	// has happened yet — agent can branch on that without a separate
	// "is this field present?" check. Set via system_config.compaction
	// .last_run by the compact tool after each successful commit.
	LastCompactedAt string `json:"last_compacted_at"`
}

// Scratchpad age-tag thresholds. Tunable from one place. The
// presentation thresholds (wake context) are independent of the
// TTL threshold (decay_at, set on flush + reset on UPSERT). The
// surface uses presentation thresholds; gc_run --scratchpads uses
// the TTL. Decoupling is the "no invisible orphans" mandate.
const (
	ScratchpadFreshHours       = 24.0
	ScratchpadDormantDays      = 7.0
	ScratchpadThesisPreviewMax = 200
)

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
//
// v spec 2026-08-04: each requested name is run through
// ResolveActivePersona / ResolveActiveMode so a stale active.json
// pointing at a deleted .md file falls back to system/standard with
// an audit-log entry, instead of booting the agent with a blank
// context window. dm is passed through so the fallback path can log.
func readActiveState(dm *DatabaseManager) (mode, persona string) {
	active, err := LoadActiveJSON()
	if err != nil {
		return "", ""
	}
	if len(active.Modes) > 0 {
		rawMode := strings.Join(active.Modes, ", ")
		mode = ResolveActiveMode(dm, rawMode)
	}
	persona = ResolveActivePersona(dm, active.Persona)
	return mode, persona
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

	data.ActiveMode, data.ActivePersona = readActiveState(dm)
	// Budget envelope (locked 2026-07-06): 5 tactical + 5 strategic.
	// Recent Memories was pulled at limit=10 but the renderer caps at 5,
	// so the 10→5 swap at the DB layer matches the rendering budget and
	// reclaims 5 slots for Recent Milestones. The total wake-context
	// payload size is unchanged.
	memories, err := dm.recentMemories(5)
	if err != nil {
		return data, fmt.Errorf("gather recent memories: %w", err)
	}
	data.RecentMemories = memories

	milestones, err := dm.recentMilestones(5)
	if err != nil {
		return data, fmt.Errorf("gather recent milestones: %w", err)
	}
	data.RecentMilestones = milestones

	topics, err := dm.GetRecentUserTopics(5)
	if err != nil {
		return data, fmt.Errorf("gather recent user topics: %w", err)
	}
	data.RecentTopics = topics
	data.AuditSummary = dm.AuditSummary()

	// Epistemic pressure — single COUNT query against the view plus
	// the system_config threshold lookup, folded into one sub-millisecond
	// SELECT. Defaults are non-fatal: a missing compaction key in
	// system_config falls back to 100; a view query failure is logged
	// to audit and the field is left as the zero value (the agent
	// sees absent → no compaction pressure, which is the safe
	// default).
	data.EpistemicPressure = dm.gatherEpistemicPressure()

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

	orphans, err := dm.ScratchpadOrphansSummary()
	if err != nil {
		return data, fmt.Errorf("gather scratchpad orphans: %w", err)
	}
	data.ScratchpadOrphans = orphans
	data.AvailableSkills = populateAvailableSkills(dm, "all")

	return data, nil
}

// gatherEpistemicPressure returns the cognitive-load snapshot the agent
// sees on every wake. Single SELECT against the epistemic_pressure_v
// view, joined with a subquery on system_config for the threshold.
// Designed to be sub-millisecond at realistic substrate sizes (the
// two COUNT(*) subqueries in the view hit the (collection, deleted_at,
// ...) composite index on memories and the lessons view directly).
//
// Threshold default: 100 (configurable via system_config.compaction.raw_threshold).
// Ratio default when LessonCount == 0: 0.0 (not NaN, not +Inf — those
// break downstream JSON parsers). The 0.0 represents "no ratio
// information" — a fresh agent has no lessons, but the metric is
// still meaningful via RawCount alone.
func (dm *DatabaseManager) gatherEpistemicPressure() EpistemicPressureData {
	var (
		rawCount    int
		lessonCount int
		threshold   int
	)
	err := dm.SQLDB().QueryRow(`
		SELECT
		  raw_count,
		  lesson_count,
		  COALESCE(
		    (SELECT CAST(json_extract(raw_json, '$.raw_threshold') AS INTEGER)
		     FROM system_config WHERE key = 'compaction'),
		    100
		  ) AS threshold
		FROM epistemic_pressure_v
	`).Scan(&rawCount, &lessonCount, &threshold)
	if err != nil {
		// Non-fatal: log to audit and return zero value. The agent sees
		// absent pressure (raw_count=0, exceeded=false) which is the
		// safe default — no spurious compaction triggers from a
		// substrate glitch.
		dm.LogAudit(AuditWarn, "wake_context", "epistemic_pressure read failed: "+err.Error(), "", AuditContext{})
		return EpistemicPressureData{Threshold: 100}
	}

	// Last compaction timestamp. Read separately so a missing
	// system_config row doesn't kill the whole gauge read. Empty
	// string when no compaction has happened — agents branch on
	// non-empty rather than parsing the string.
	var lastCompacted string
	if err := dm.SQLDB().QueryRow(`
		SELECT COALESCE(json_extract(raw_json, '$.last_run_at'), '')
		FROM system_config WHERE key = 'compaction.last_run'
	`).Scan(&lastCompacted); err != nil {
		lastCompacted = ""
	}

	// Divide-by-zero guard. With LessonCount == 0 the ratio is undefined;
	// emit 0.0 so the JSON marshaller produces a valid number instead of
	// NaN or +Inf that downstream parsers reject. RawCount alone is the
	// signal the agent acts on; ratio is a secondary density metric.
	ratio := 0.0
	if lessonCount > 0 {
		ratio = float64(rawCount) / float64(lessonCount)
	}

	return EpistemicPressureData{
		RawCount:        rawCount,
		LessonCount:     lessonCount,
		Ratio:           ratio,
		Threshold:       threshold,
		Exceeded:        rawCount > threshold,
		LastCompactedAt: lastCompacted,
	}
}

// recentMemories returns up to `limit` non-deleted memories ordered newest first.
func (dm *DatabaseManager) recentMemories(limit int) ([]WakeContextMemory, error) {
	rows, err := dm.SQLDB().Query(`
		SELECT id, content, created_at FROM memories
		WHERE deleted_at IS NULL
		ORDER BY created_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []WakeContextMemory
	for rows.Next() {
		var m WakeContextMemory
		if err := rows.Scan(&m.ID, &m.Content, &m.CreatedAt); err != nil {
			return nil, fmt.Errorf("scanning wake context recent memory row: %w", err)
		}
		out = append(out, m)
	}
	return out, nil
}

// recentMilestones returns up to `limit` non-deleted memories tagged with
// the type:milestone-* prefix, scoped to a 30-day rolling window. This is
// the wake-context narrative-arc surface — complementary to recentMemories
// (tactical: what was I just doing?) with the deliberate commitment-ceremony
// gate encoded by the milestone tag (strategic: what was the actual
// narrative arc?).
//
// The query uses substring-on-JSON-encoded tags: `type:milestone-%"` where
// the trailing quote is the JSON string close. This avoids false positives
// like user-content containing the literal substring 'type:milestone-'.
//
// Milestones are session-decoupled by design (a milestone is a cognitive
// artifact, not an operational log) — there is no session_id filter here.
// Across all sessions, the agent wakes up seeing the same recent narrative
// arc regardless of which session_id it is currently in. Cross-session
// narrative continuity is the whole point of the architecture.
func (dm *DatabaseManager) recentMilestones(limit int) ([]WakeContextMemory, error) {
	if limit <= 0 {
		limit = 5
	}
	rows, err := dm.SQLDB().Query(`
		SELECT id, content, created_at FROM memories
		WHERE deleted_at IS NULL
		  AND tags LIKE ?
		  AND created_at > CAST(strftime('%s','now', '-30 days') AS INTEGER)
		ORDER BY created_at DESC LIMIT ?`,
		`%type:milestone-%`+`"`+`%`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []WakeContextMemory
	for rows.Next() {
		var m WakeContextMemory
		if err := rows.Scan(&m.ID, &m.Content, &m.CreatedAt); err != nil {
			return nil, fmt.Errorf("scanning wake context recent milestone row: %w", err)
		}
		out = append(out, m)
	}
	return out, nil
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
func (dm *DatabaseManager) GetRecentUserTopics(limit int) ([]string, error) {
	rows, err := dm.SQLDB().Query(
		`SELECT name FROM topics
		 WHERE NOT (
		   (description IS NULL OR description = '' OR description = '{}')
		   AND (tags IS NULL OR tags = '' OR tags = '[]')
		 )
		 AND name NOT IN ('decisions', 'theories')
		 ORDER BY created_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("scanning user topic name row: %w", err)
		}
		out = append(out, name)
	}
	return out, nil
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
	if len(d.RecentMilestones) > 0 {
		lines = append(lines, "**Recent Milestones:**")
		for i, m := range d.RecentMilestones {
			if i >= 5 {
				break
			}
			content := m.Content
			if len(content) > 120 {
				content = content[:120] + "…"
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
			lines = append(lines, fmt.Sprintf("  - [w=%v] %s", r.Weight, content))
		}
	}
	if d.ScratchpadOrphans != "" {
		lines = append(lines, d.ScratchpadOrphans)
	}
	if len(d.AvailableSkills) > 0 {
		lines = append(lines, fmt.Sprintf("**Available Skills (count=%d):**", len(d.AvailableSkills)))
		for _, s := range d.AvailableSkills {
			marker := ""
			if s.IsGlobal {
				marker = " [shared]"
			}
			lines = append(lines, fmt.Sprintf("  - %s v%s: %s%s", s.Name, s.Version, s.WhenToUse, marker))
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
//
// Handoff timestamp fields are stored as INTEGER Unix-epoch seconds (see
// migration timestamps_unified_v1); FormatUnixSeconds renders them at the
// display boundary.
func formatHandoff(h *Handoff) string {
	var lines []string
	header := fmt.Sprintf("**Previous Session Handoff** (%s, %s)", h.SessionID, h.EndedState)
	if h.EndedAt > 0 {
		header = fmt.Sprintf("**Previous Session Handoff** (%s, ended %s, %s)",
			h.SessionID, FormatUnixSeconds(h.EndedAt), h.EndedState)
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
		WHERE created_at >= CAST(strftime('%s','now', '-7 days') AS INTEGER)`)
	if err := row.Scan(&errCount, &warnCount); err != nil {
		// Non-fatal: degrade to silent so a transient DB hiccup never
		// floods wake context. The agent still has query_audit_log.
		return ""
	}
	// --- Fetch + dedup via shared helper (same source as list_active_clusters) ---
	knownClusters, unknownClusters, err := dm.ActiveClusters()
	if err != nil {
		// Non-fatal: degrade to silent so a transient DB hiccup never
		// floods wake context. The agent still has query_audit_log +
		// list_active_clusters for structured retrieval.
		return ""
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
			firstSeen := c.FirstSeen
			if len(firstSeen) >= 10 {
				firstSeen = firstSeen[:10]
			}
			lines = append(lines, fmt.Sprintf("  * [%s] %d events since %s (ID: %s)",
				c.Component, c.Count, firstSeen, c.Key))
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

// previewThesisTruncated strips embedded newlines and truncates to
// ScratchpadThesisPreviewMax characters with an ellipsis suffix. Used
// by ScratchpadOrphansSummary to keep the wake-context payload bounded
// — a 5KB thesis flush would otherwise inflate every wake by 5KB+
// regardless of how stale the row is.
func previewThesisTruncated(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= ScratchpadThesisPreviewMax {
		return s
	}
	return s[:ScratchpadThesisPreviewMax] + "..."
}

// scratchpadAgeTag maps an age-in-hours to a presentation tag using
// the ScratchpadFreshHours / ScratchpadDormantDays thresholds. The
// thresholds are presentation-only; the wake-context query does NOT
// filter by them (orphan-surfacing mandate — see schema.go comment).
// gc_run --scratchpads is the one true vacuum path.
func scratchpadAgeTag(ageHours float64) string {
	if ageHours > ScratchpadDormantDays*24 {
		return "[Expired]"
	}
	if ageHours > ScratchpadFreshHours {
		return "[Dormant]"
	}
	return "[Fresh]"
}

// ScratchpadOrphansSummary returns the agent-facing wake-context block
// for unpromoted scratchpad rows from previous sessions. Empty string
// when no orphans exist (caller skips the line entirely).
//
// Surface rule (locked 2026-07-05): every row in ephemeral_scratchpad
// whose session_id != the current session is surfaced, regardless of
// decay_at. The age tag ([Fresh]/[Dormant]/[Expired]) tells the
// agent how stale the row is; filtering it out at the query layer
// would defeat orphan-recovery — exactly the case we want to catch
// (trashed session whose thesis never made it to memory).
//
// Telemetry mirrors auditSummaryRich: a transient DB hiccup returns
// "" so wake context isn't flooded by error noise.
func (dm *DatabaseManager) ScratchpadOrphansSummary() (string, error) {
	if dm == nil || dm.db == nil {
		return "", nil
	}

	currentSession := ""
	if s, err := dm.GetLastSession(); err == nil && s != nil {
		currentSession, _ = s["session_id"].(string)
	}

	rows, err := dm.db.Query(`
		SELECT session_id, thesis,
		       (CAST(strftime('%s','now') AS INTEGER) - updated_at) / 3600.0 AS age_hours
		FROM ephemeral_scratchpad
		WHERE session_id != ?`,
		currentSession)
	if err != nil {
		return "", err
	}
	defer rows.Close()

	var lines []string
	tagCounts := map[string]int{"[Fresh]": 0, "[Dormant]": 0, "[Expired]": 0}
	for rows.Next() {
		var id, thesis string
		var ageHours float64
		if err := rows.Scan(&id, &thesis, &ageHours); err != nil {
			return "", fmt.Errorf("scanning scratchpad orphan row: %w", err)
		}
		tag := scratchpadAgeTag(ageHours)
		tagCounts[tag]++
		lines = append(lines, fmt.Sprintf("  * %s session %s: %s", tag, id, previewThesisTruncated(thesis)))
	}
	if len(lines) == 0 {
		return "", nil
	}

	// Aggregate header summary. Mirrors the auditSummaryRich pattern
	// ("N events since DATE"): a glance tells v whether to act now or
	// defer. Break down by age tag so stale orphans (the ones most
	// likely to be safely discardable) are visually separable.
	total := tagCounts["[Fresh]"] + tagCounts["[Dormant]"] + tagCounts["[Expired]"]
	header := fmt.Sprintf(
		"- Ephemeral Scratchpads (%d orphan%s pending; Action Required: promote_scratchpad, amend, or discard): "+
			"Fresh=%d, Dormant=%d, Expired=%d",
		total,
		ternaryPlural(total),
		tagCounts["[Fresh]"], tagCounts["[Dormant]"], tagCounts["[Expired]"])
	out := []string{header}
	out = append(out, lines...)
	return strings.Join(out, "\n"), nil
}

// ternaryPlural returns "s" unless count == 1 (matches English
// pluralization for "1 orphan pending" vs "0/N orphans pending").
// Extracted so the orphan header reads naturally without inline
// conditionals in fmt format strings.
func ternaryPlural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
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
				  AND created_at >= CAST(strftime('%s','now', '-30 days') AS INTEGER)
				  AND content LIKE ? ESCAPE '\'
			)`, pattern, pattern, pattern)
	if err := row.Scan(&matched); err != nil {
		return false, err
	}
	return matched != 0, nil
}
