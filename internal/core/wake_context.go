// wake_context.go - Single source of truth for wake context data and formatting.
// Both the CLI tool (`mpm call read_wake_context`) and the Go MCP server
// (cmd/mpm-mcp) call into this file so the data-gathering logic and the
// human-readable format stay in lockstep.

package internal

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"
)

// WakeContextData is the bounded orientation surface for an agent.
// It answers four questions: Who am I? What just happened? What is waiting
// for me? What constraints apply? Wake context is orientation — not
// investigation; not retrieval; not durable state. Deliberate retrieval
// happens via mpm_memory query / mpm_lessons / mpm_skills get, scheduler
// activation happens via the wakes surface, durable state lives in
// memories/lessons/decisions/theories tables. Wake context bootstraps.
// That's it.
//
// INVARIANTS (every field added to this struct must respect these):
//   1. Humans get prose. Agents get typed signals. The same data can be
//      rendered both ways (see formatWakeContext for prose, the struct
//      itself for JSON), but the JSON wire form must always be typed.
//   2. If a subsystem can create work an agent is expected to perform,
//      wake_context MUST expose the resulting pending state. This is the
//      invariant that the 2026-08-13 overdue_wakes patch closed.
//   3. Lists MUST NOT omit empty values (no omitempty on slice fields).
//      Absence in the JSON means "schema does not support this signal";
//      empty array means "checked, none found". The two are different
//      states and the agent must be able to distinguish them.
//   4. Payload size is strictly bounded (MaxWakeContextBytes = 32 KB).
//      If budget is exceeded, fields are truncated in a documented
//      priority order and *flagged via Truncated booleans*. Wake context
//      must never silently grow past budget.
//
// Wire format version: bump ContextVersion on any non-additive change.
// Pure additions (new fields, new truncation flags) are safe under the
// same version. Rename, semantic change, or removal of an existing
// field is a major-version bump.
type WakeContextData struct {
	// Metadata — emitted on every read so consumers can detect stale
	// payloads and (in v5+) reason about which schema they got.
	ContextVersion string `json:"context_version"` // e.g., "wake-context-v5"
	GeneratedAt    int64  `json:"generated_at"`     // unix seconds, when the struct was assembled
	AsOf           int64  `json:"as_of"`            // unix seconds, the substrate-state timestamp

	// Identity — strict superset over v3's session_id field. v3 callers
	// keep working because SessionID is preserved. v4 callers can use
	// the split fields.
	//
	// SessionCurrentID — the session that is currently waking.
	// SessionPreviousID — the session that last wrote state (often the
	//   one whose handoff we just marked read). Empty when no prior
	//   session exists or when GetPreviousSession hasn't been wired yet
	//   (filled in by a follow-up; today the field stays "").
	// SessionStartedAt — when SessionCurrentID began (unix epoch).
	// SessionPreviousEndedAt — when SessionPreviousID ended (unix epoch,
	//   0 when no previous).
	SessionID               string `json:"session_id"`
	SessionCurrentID        string `json:"session_current_id"`
	SessionPreviousID       string `json:"session_previous_id"`
	// MPMSessionID — MPM-owned session identity (sticky across
	// CLI/MCP/process boundaries within one interaction lifecycle).
	// Populated by active.json (wip/session-identity-rework) and by
	// the canonical session-identity surface. Empty string on a fresh
	// workspace until the first handoff write allocates it.
	MPMSessionID            string `json:"mpm_session_id,omitempty"`
	// FrameworkSessionID — host-owned session identifier. Optional;
	// empty when the calling host has no native session id (Pi, Hermes
	// without hooks, Claude Code without MPM_SESSION_ID).
	FrameworkSessionID      string `json:"framework_session_id,omitempty"`
	SessionStartedAt        int64  `json:"session_started_at"`
	SessionPreviousEndedAt int64  `json:"session_previous_ended_at"`

	// Orientation — the "what just happened" half.
	//
	// ActiveMode is the legacy singular form, kept for back-compat with
	// pre-2026-09-11 consumers. Definition (v spec 2026-09-11):
	//   * zero modes           → ""
	//   * one or more modes    → FIRST resolved mode (NOT comma-joined)
	// ActiveModes is the canonical plural form for the 0..N contract.
	// ActiveModeSource is the aggregate resolution source for the
	// multi-mode selection as a whole ("explicit" / "fallback" / "empty");
	// see active_state.go for the source vocabulary.
	ActiveMode            string   `json:"active_mode"`
	ActiveModes           []string `json:"active_modes"`
	ActiveModeSource      string   `json:"active_mode_source"`
	// ActivePersonaSource mirrors the per-persona resolution source
	// ("explicit" / "fallback" / "empty"). New consumers should consult
	// this to distinguish explicit user selection from system default
	// fallback from explicit clear.
	ActivePersona         string `json:"active_persona"`
	ActivePersonaSource   string `json:"active_persona_source"`
	RecentTopics          []string `json:"recent_topics"`
	// RecentTopicsTruncated is set by enforceSizeLimit when the topics
	// array had to be shed to stay under MaxWakeContextBytes. The
	// agent sees the flag and knows recent_topics is empty *because of
	// the cap*, not because there were no recent topics.
	RecentTopicsTruncated bool                `json:"recent_topics_truncated,omitempty"`
	RecentMemories       []WakeContextMemory `json:"recent_memories"`
	RecentMilestones     []WakeContextMemory `json:"recent_milestones"`

	// Attention & Pending Work — the "what is waiting for me" half.
	// LastHandoff is the most recent unread handoff from a previous
	// session, or nil if there is none. Marked-as-read on surfacing;
	// the same handoff is never shown twice in a row.
	LastHandoff *Handoff `json:"last_handoff,omitempty"`
	// OverdueWakes: see struct field comment above. Added 2026-08-13.
	// Always-on (no omitempty) per invariant 3.
	OverdueWakes []OverdueWake `json:"overdue_wakes"`
	// ScratchpadOrphans: human-readable summary of unpromoted
	// scratchpad rows. v5+ may carry structured entries; v4 keeps the
	// prose form for compatibility with existing call sites.
	ScratchpadOrphans string `json:"scratchpad_orphans"`
	// OpenWorks are work items with status='open'. Bounded to 5 items,
	// ordered by updated_at DESC (most recently touched first, so fresh
	// and actively-updated work outranks stale history — see F3). The
	// tiebreak is created_at DESC for deterministic ordering. Non-nil
	// empty slice always emitted per WakeContextData invariant 3.
	OpenWorks []WakeContextWork `json:"open_works"`
	// CompletedWorks are work items with status='done', bounded to 5
	// items, ordered by completed_at DESC (most recently completed
	// first). RECOMMENDED 10: gives the agent a "what did I just ship"
	// surface so a fresh session can re-orient against recent
	// completions without opening the full work history. Cancelled
	// items are excluded — those belong to a different cognitive
	// channel ("what got rejected and why").
	CompletedWorks []WakeContextWork `json:"completed_works"`

	// RecentActivity — Stage 2 cross-surface activity summary. Pulled
	// from tool_invocations through the canonical EffectiveActorKind
	// + ClassifyAction filters. Default scope: agent+human mutating
	// actions, newest first, capped at 10 events to fit the wake-context
	// byte budget. Each event identifies its actor_kind/framework so
	// the consumer can distinguish agent vs human vs system writes.
	// The section is read-only at gather time and never mutates the
	// underlying tool_invocations row for itself (the recent_activity
	// query is classified as read_only and so is excluded from the
	// default semantic activity feed).
	RecentActivity []WakeContextActivity `json:"recent_activity"`
	// RecentActivityTruncated is set by enforceSizeLimit when the
	// recent_activity array had to be shed to stay under
	// MaxWakeContextBytes. The agent sees the flag and knows the
	// absence was a cap-induced drop, not "checked, none found".
	RecentActivityTruncated bool `json:"recent_activity_truncated,omitempty"`

	// Constraints & Capabilities — the "what rules apply, what tools".
	// GlobalRules only populated when MPM_SHARED_DB is attached.
	GlobalRules []WakeContextRule `json:"global_rules"`
	// EpistemicPressure is kept as EpistemicPressureData (existing
	// type alias kept intentionally to avoid an unnecessary rename).
	EpistemicPressure EpistemicPressureData `json:"epistemic_pressure"`
	// AvailableSkills: at most TopSkillsInWake entries, weight-sorted.
	AvailableSkills []SkillSummary `json:"available_skills"`
	// AvailableSkillsTruncated: see RecentTopicsTruncated above.
	AvailableSkillsTruncated bool `json:"available_skills_truncated,omitempty"`

	// System Health.
	// AuditSummary: prose form (humans get prose). Future v5 may add
	// a structured audit_block alongside this; for v4 prose stays.
	AuditSummary string `json:"audit_summary"`
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
const ScratchpadThesisPreviewMax = 200

// WakeContextMemory is the trimmed memory reference shown in wake context.
// Phase 2C: changed from Content to Summary + Pointer for bounded orientation.
type WakeContextMemory struct {
	ID        string `json:"id"`
	Summary   string `json:"summary"`  // first 256 chars via SummarizeMemory
	Pointer   string `json:"pointer"`  // "mpm://memory/<id>"
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

// WakeContextActivity is the bounded per-event projection of
// RecentActivityEvent used in the wake-context payload. Trims
// the public wire shape to a glance-friendly subset so the wake
// payload stays inside the 32 KB byte budget even on a busy agent.
type WakeContextActivity struct {
	ID            string `json:"id"`
	Timestamp     int64  `json:"timestamp"`
	ActorKind     string `json:"actor_kind"`
	FrameworkName string `json:"framework_name,omitempty"`
	Tool          string `json:"tool"`
	Action        string `json:"action"`
	ArtifactID    string `json:"artifact_id,omitempty"`
	Summary       string `json:"summary"`
}

// OverdueWake is a single row in the wake-context overdue-wakes
// surface. Derived from scheduled_wakes where fired=0 AND
// target_time <= now. Slim payload: id, when it was due, what kind
// it is, the reason (truncated to 100 chars at the render layer),
// and the overdue duration in seconds (always >= 0 because the
// gather query filters out future-dated rows; the negative
// clamp at scan time is defensive against timezone skew between
// the SQLite-side strftime and the renderer).
type OverdueWake struct {
	ID          string `json:"id"`
	TargetTime  int64  `json:"target_time"`
	Reason      string `json:"reason"`
	Kind        string `json:"kind,omitempty"`
	OverdueSecs int64  `json:"overdue_secs"`
}

// EnforceSizeLimit marshals the wake-context data, checks it against
// the MaxWakeContextBytes cap, and sheds the heaviest arrays first if
// the limit is breached. The shed order is documented in
// wakeContextTruncatedFieldNames (declared above) — heaviest first so
// the agent loses the lowest-priority information first.
//
// This function is a guardrail, not a normal path: under steady-state
// operation, the wake-context payload is bounded by the gather-layer
// limits (5 memories, 5 milestones, 5 overdue wakes, 5 skills, 10
// rules, 200-char scratchpad previews). A size-cap hit indicates either
// a subsystem growing past its allocation or a regression in the
// gather-layer limits — both warrant operator attention.
//
// The shed-and-flush loop returns the marshalled bytes so callers
// that produce wire form from WakeContextData (e.g. ReadWakeContext
// which returns prose alongside JSON) can ship the post-cap bytes
// without re-marshalling. The function also mutates *data to set
// Truncated flags.
func EnforceSizeLimit(data *WakeContextData) ([]byte, error) {
	b, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	if len(b) <= MaxWakeContextBytes {
		return b, nil
	}

	// Shed tiers. Each tier is gated on the field being non-empty
	// (the slot might already have been dropped by an earlier tier)
	// and sets the Truncated flag so the agent can tell that the
	// absence was a cap-induced drop and not "checked, none found".
	for _, tier := range wakeContextTruncatedFieldNames { //nolint:gocritic // range is fine, switch is the actual control flow
		switch tier {
		case "available_skills":
			if len(data.AvailableSkills) > 0 {
				data.AvailableSkills = make([]SkillSummary, 0)
				data.AvailableSkillsTruncated = true
				b, _ = json.Marshal(data)
				if len(b) <= MaxWakeContextBytes {
					return b, nil
				}
			}
		case "recent_activity":
			if len(data.RecentActivity) > 0 {
				data.RecentActivity = make([]WakeContextActivity, 0)
				data.RecentActivityTruncated = true
				b, _ = json.Marshal(data)
				if len(b) <= MaxWakeContextBytes {
					return b, nil
				}
			}
		case "recent_topics":
			if len(data.RecentTopics) > 0 {
				data.RecentTopics = make([]string, 0)
				data.RecentTopicsTruncated = true
				b, _ = json.Marshal(data)
				if len(b) <= MaxWakeContextBytes {
					return b, nil
				}
			}
		}
	}

	// Pathological case: even after shedding both tiers, the payload
	// still exceeds budget. Likely a regression in the render layer
	// (e.g. a debug field that's a megabyte). Don't silently lie; warn
	// so operators can find it. Wake context must still return — the
	// agent's bootstrap is non-optional.
	slog.Warn("wake_context: size cap exceeded even after shedding all tiers",
		"size_bytes", len(b),
		"budget_bytes", MaxWakeContextBytes,
		"context_version", data.ContextVersion)
	return b, nil
}

// readActiveState reads {MPM_DIR}/active.json via the canonical loader
// and resolves persona and modes through the pointer-aware resolver.
// Returns the singular-string projections for back-compat with tests and
// callers that pre-date the 0..N mode contract:
//
//	mode    = first resolved mode name (or "")
//	persona = resolved persona name (or "")
//
// For the full multi-mode collection + per-field source metadata use
// readActiveStateFull.
//
// v spec 2026-09-11 (selector hardening): the resolver distinguishes:
//
//	1. explicit selection  → SourceExplicit
//	2. explicit clear      → SourceEmpty
//	3. absent / uninit    → default fallback (SourceFallback or SourceEmpty)
//	4. stale / invalid    → drop (modes) or fallback (persona); SourceFallback
//
// Persona is 0..1, modes are 0..N. Multi-mode storage is honoured here:
// each entry is validated independently and stale entries are dropped
// rather than being collapsed into a single invalid value (the
// pre-refactor bug was: comma-joined "x,y" → os.Stat("x,y.md") → default).
func readActiveState(dm *DatabaseManager) (mode, persona string) {
	active, err := LoadActiveJSON()
	if err != nil {
		return "", ""
	}
	mr := ResolveActiveModes(dm, active.Modes)
	pr := ResolveActivePersonaIntent(dm, active.Persona)
	if names := mr.Names(); len(names) > 0 {
		mode = names[0]
	}
	persona = pr.Name
	return mode, persona
}

// readActiveStateFull returns the rich resolver output for both
// selector dimensions. Use this when you need the multi-mode
// collection, per-entry sources, or the Missing diagnostics. The
// wake context renderer uses this internally to populate
// ActiveModeSource / ActivePersonaSource / ActiveModes.
func readActiveStateFull(dm *DatabaseManager) (ModeResolution, Resolution) {
	active, err := LoadActiveJSON()
	if err != nil {
		return ModeResolution{Modes: []ResolvedMode{}, Source: SourceEmpty},
			Resolution{Name: "", Source: SourceEmpty}
	}
	modes := ResolveActiveModes(dm, active.Modes)
	persona := ResolveActivePersonaIntent(dm, active.Persona)
	return modes, persona
}

// GatherWakeContext returns the wake context, populating active mode/persona
// from active.json and recent memories/topics from the database even when
// there is no prior session row. Callers can detect a missing session via
// SessionID == "". The latest unread handoff (if any) is also surfaced
// and marked as read.
// wakeContextCurrentVersion is set on every GatherWakeContext so consumers
// can detect schema drift. Bumped only on non-additive changes to
// WakeContextData (rename, semantic change, removal). Pure additions —
// new fields, new truncation flags — stay at the same version because
// the JSON wire form remains a strict superset of older versions.
const wakeContextCurrentVersion = "wake-context-v5"

// MaxWakeContextBytes is the hard cap on the JSON wire form of the
// wake-context payload. 32 KB is enough for the bounded orientation
// surface (5 tactical memories + 5 milestones + ≤5 overdue wakes +
// ≤5 skills + ≤10 rules + small prose summaries). If a future
// subsystem crosses this budget, enforceSizeLimit sheds the heaviest
// arrays first and sets the corresponding Truncated flag.
const MaxWakeContextBytes = 32 * 1024

// wakeContextTruncatedFieldNames lists the JSON field names whose
// truncation we surface. Used by tests and by future operator
// surfaces. Order = shedding priority (first listed = shed first).
// recent_activity sheds before recent_topics because it is bounded
// already at 10 entries and is the heaviest single addition to the
// payload under the Stage 2 cross-surface activity summary.
var wakeContextTruncatedFieldNames = []string{
	"available_skills",
	"recent_activity",
	"recent_topics",
}

// GatherWakeContext assembles the wake context and CONSUMES the latest
// unread handoff (marks it read). This is the agent-facing contract: a wake
// delivers the handoff once.
//
// Presentation-only callers (dashboards, `mpm continue` composition) must
// use GatherWakeContextReadOnly instead — rendering is not delivery, and a
// consuming render steals the handoff from the agent's actual boot read
// (audit finding F18: `mpm status` marked the handoff read, so
// read_wake_context later saw none).
func (dm *DatabaseManager) GatherWakeContext() (WakeContextData, error) {
	return dm.gatherWakeContext(true)
}

// GatherWakeContextReadOnly assembles the same wake context WITHOUT marking
// the handoff read. Pure projection — safe to call for display.
func (dm *DatabaseManager) GatherWakeContextReadOnly() (WakeContextData, error) {
	return dm.gatherWakeContext(false)
}

func (dm *DatabaseManager) gatherWakeContext(markHandoffRead bool) (WakeContextData, error) {
	var data WakeContextData

	// Metadata — emitted on every read. GeneratedAt and AsOf are the
	// same instant for now (event-time = processing-time); the
	// separation exists so future event-time backfills can be
	// distinguished cleanly.
	nowUnix := time.Now().Unix()
	data.ContextVersion = wakeContextCurrentVersion
	data.GeneratedAt = nowUnix
	data.AsOf = nowUnix

	session, err := dm.GetLastSession()
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return data, fmt.Errorf("get last session: %w", err)
	}
	if err == nil && session != nil {
		data.SessionID, _ = session["session_id"].(string)
		data.SessionCurrentID = data.SessionID
		// SessionStartedAt is a unix epoch (int64) on the sessions table.
		if startedAt, ok := session["started_at"].(int64); ok {
			data.SessionStartedAt = startedAt
		}
		// SessionPreviousID + SessionPreviousEndedAt remain zero values
		// until a future iteration wires GetPreviousSession(); until
		// then the agent sees "" / 0 and the absence is captured in
		// the v4 wire format. Future work, not future regression.
	}

	// Ensures every list field on data is a non-nil empty slice —
	// see invariant 3 on WakeContextData. Variables like
	// `var x []string` would serialize as null; `make([]string, 0)`
	// serializes as []. The GatherWakeContext body below populates
	// most of these from query helpers (each `GetX()` in this file
	// already uses `make([]T, 0)` internally), but this explicit
	// init here guarantees the contract for the fresh-database case
	// before the queries run.
	data.RecentTopics = make([]string, 0)
	data.RecentMemories = make([]WakeContextMemory, 0)
	data.RecentMilestones = make([]WakeContextMemory, 0)
	data.OverdueWakes = make([]OverdueWake, 0)
	data.GlobalRules = make([]WakeContextRule, 0)
	data.AvailableSkills = make([]SkillSummary, 0)
	data.OpenWorks = make([]WakeContextWork, 0)
	data.CompletedWorks = make([]WakeContextWork, 0)
	data.RecentActivity = make([]WakeContextActivity, 0)

	// Pull the latest unread handoff. In consume mode the mark-read happens
	// here so re-reading wake context (e.g. in the same session) doesn't
	// re-show the same handoff. Wake-context timestamp is the read-by
	// token — distinct from any session_id since the agent may not have one.
	// Read-only mode peeks: the handoff stays unread for the real consumer.
	var h *Handoff
	var herr error
	if markHandoffRead {
		h, herr = dm.MarkLatestHandoffRead("wake-context")
	} else {
		h, herr = dm.GetLatestUnreadHandoff()
	}
	if herr != nil && !errors.Is(herr, sql.ErrNoRows) {
		// Non-fatal: log the handoff read failure to audit but continue
		// with wake context. The handoff is bootstrap data; the agent
		// can still wake up without it.
		dm.LogAudit(AuditWarn, "wake_context", "handoff read failed: "+herr.Error(), "", AuditContext{})
	}
	if h != nil {
		data.LastHandoff = h
	}

	modeRes, personaRes := readActiveStateFull(dm)
	// Legacy singular field: first mode or empty (NOT comma-joined —
	// singular back-compat must not invent a multi-mode string).
	if names := modeRes.Names(); len(names) > 0 {
		data.ActiveMode = names[0]
	}
	data.ActiveModeSource = modeRes.Source
	data.ActiveModes = modeRes.Names()
	data.ActivePersona = personaRes.Name
	data.ActivePersonaSource = personaRes.Source
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
	data.OpenWorks = dm.gatherOpenWorks()
	data.CompletedWorks = dm.gatherCompletedWorks()

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
	if rules, _ := dm.QueryGlobalRules("", 10, false); len(rules) > 0 {
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
	data.OverdueWakes = dm.gatherOverdueWakes()
	data.AvailableSkills = populateAvailableSkills(dm, "all")
	data.RecentActivity = dm.gatherRecentActivity(10)

	return data, nil
}

// gatherRecentActivity returns the bounded cross-surface activity
// projection used in the wake-context payload. Defaults: 10 events,
// agent+human scope, mutating actions only, newest-first. The
// gathering function is purely a projection — it never mutates
// durable state (its own tool_invocations row is classified as
// read_only and excluded from the default semantic activity feed).
func (dm *DatabaseManager) gatherRecentActivity(limit int) []WakeContextActivity {
	if dm == nil || dm.db == nil {
		return nil
	}
	if limit <= 0 {
		limit = 10
	}
	events, err := dm.RecentActivity(RecentActivityQueryParams{
		Limit: limit,
	})
	if err != nil {
		dm.LogAudit(AuditWarn, "wake_context", "gatherRecentActivity: "+err.Error(), "", AuditContext{})
		return nil
	}
	out := make([]WakeContextActivity, 0, len(events))
	for _, ev := range events {
		out = append(out, WakeContextActivity{
			ID:            ev.ID,
			Timestamp:     ev.Timestamp,
			ActorKind:     ev.ActorKind,
			FrameworkName: ev.FrameworkName,
			Tool:          ev.Tool,
			Action:        ev.Action,
			ArtifactID:    ev.ArtifactID,
			Summary:       ev.Summary,
		})
	}
	return out
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

// gatherOverdueWakes returns the most-overdue un-fired scheduled
// wakes, capped at 5 rows by overdue severity (most-overdue first).
// Returns nil (no error) when no overdue wakes exist.
//
// Symmetric to gatherEpistemicPressure: a single query against
// scheduled_wakes, sub-millisecond at realistic queue sizes thanks
// to the idx_scheduled_wakes_due(fired, target_time) index. The
// kind column is parsed out of metadata so the agent can branch on
// it (task / reminder / drill / etc) without parsing the reason
// string. OverdueSecs is computed in SQL so the renderer never
// needs to call time.Now() — keeping this surface pure data.
//
// NULL handling: rows with NULL or empty reason are skipped
// (defensive — would never be created via the canonical paths but
// a future ingest path could insert manually).
//
// Error handling: non-fatal, log to audit and return nil. The agent
// still has mpm_call mpm_wakes list overdue_only=true as the
// canonical surface if the wake-context summary is incomplete.
//
// Bootstrap context (2026-08-13): this method was added to
// eliminate the silent-failure gap where read_wake_context never
// surfaced scheduled_wakes at all — agents relying on wake-context
// for "what should I do today" were structurally blind to reminders,
// deferred check-ins, and drill launches.
func (dm *DatabaseManager) gatherOverdueWakes() []OverdueWake {
	if dm == nil || dm.db == nil {
		return nil
	}
	rows, err := dm.db.Query(`
		SELECT id, target_time, reason,
		       COALESCE(json_extract(metadata, '$.kind'), '') AS kind,
		       CAST(strftime('%s','now') AS INTEGER) - target_time AS overdue_secs
		FROM scheduled_wakes
		WHERE fired = 0
		  AND target_time <= CAST(strftime('%s','now') AS INTEGER)
		  AND reason != ''
		ORDER BY target_time ASC
		LIMIT 5`)
	if err != nil {
		dm.LogAudit(AuditWarn, "wake_context", "overdue_wakes read failed: "+err.Error(), "", AuditContext{})
		return nil
	}
	defer rows.Close()

	out := make([]OverdueWake, 0, 5)
	for rows.Next() {
		var w OverdueWake
		if err := rows.Scan(&w.ID, &w.TargetTime, &w.Reason, &w.Kind, &w.OverdueSecs); err != nil {
			dm.LogAudit(AuditWarn, "wake_context", "scan overdue wake row: "+err.Error(), "", AuditContext{})
			continue
		}
		if w.OverdueSecs < 0 {
			w.OverdueSecs = 0
		}
		out = append(out, w)
	}
	return out
}

// recentMemories returns up to `limit` non-deleted memories ordered newest first.
//
// RECOMMENDED 9: structural epistemology collections ('decisions',
// 'theories') are excluded here because the wake-context render layer
// surfaces them in their own dedicated sections (audit_summary lists
// pending/resolved theories and recent decisions inline). Including
// them in recent_memories would duplicate the same row across two
// sections of the wake payload, inflating the byte budget and
// confusing the agent into thinking they were independent entries.
// The allowlist here MUST stay in lockstep with the structural-topic
// filter in GetRecentUserTopics (which excludes the 'decisions' /
// 'theories' anchors from the topics surface) — both filters target
// the same architectural invariant: structural artifacts get their own
// section, not mixed into the general-purpose recent stream.
func (dm *DatabaseManager) recentMemories(limit int) ([]WakeContextMemory, error) {
	rows, err := dm.SQLDB().Query(`
		SELECT id, content, created_at FROM memories
		WHERE deleted_at IS NULL
		  AND (expires_at IS NULL OR expires_at > strftime('%s','now'))
		  AND collection NOT IN ('decisions', 'theories')
		ORDER BY created_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []WakeContextMemory
	for rows.Next() {
		var id, content, createdAt string
		if err := rows.Scan(&id, &content, &createdAt); err != nil {
			return nil, fmt.Errorf("scanning wake context recent memory row: %w", err)
		}
		// Phase 2C: bounded summary instead of full content.
		summary := SummarizeMemory(content, 256)
		out = append(out, WakeContextMemory{
			ID:        id,
			Summary:   summary,
			Pointer:   "mpm://memory/" + id,
			CreatedAt: createdAt,
		})
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
		  AND (expires_at IS NULL OR expires_at > strftime('%s','now'))
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
		var id, content, createdAt string
		if err := rows.Scan(&id, &content, &createdAt); err != nil {
			return nil, fmt.Errorf("scanning wake context recent milestone row: %w", err)
		}
		// Phase 2C: bounded summary instead of full content.
		summary := SummarizeMemory(content, 256)
		out = append(out, WakeContextMemory{
			ID:        id,
			Summary:   summary,
			Pointer:   "mpm://memory/" + id,
			CreatedAt: createdAt,
		})
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
	// Multi-mode rendering. Each resolved mode is listed; if any have a
	// non-explicit source (fallback / stale-dropped), we surface the
	// source tag inline so the agent can distinguish explicit user
	// selection from system fallback. Singular-mode and zero-mode are
	// both rendered honestly — the singular back-compat field is NOT
	// used to derive display.
	modeLine := "**Modes:**"
	if len(d.ActiveModes) == 0 {
		modeLine += " <none>"
		if d.ActiveModeSource == SourceEmpty {
			modeLine += " [empty]"
		}
	} else {
		for i, m := range d.ActiveModes {
			if i > 0 {
				modeLine += ","
			}
			modeLine += " " + m
			if d.ActiveModeSource == SourceFallback {
				modeLine += " [fallback]"
			}
		}
	}
	lines = append(lines, modeLine)
	persona := d.ActivePersona
	personaLine := "**Persona:**"
	if persona == "" {
		personaLine += " <none>"
	} else {
		personaLine += " " + persona
	}
	if d.ActivePersonaSource == SourceFallback {
		personaLine += " [fallback]"
	} else if d.ActivePersonaSource == SourceEmpty {
		personaLine += " [empty]"
	}
	lines = append(lines, personaLine)
	if len(d.RecentTopics) > 0 {
		lines = append(lines, "**Recent Topics:** "+strings.Join(d.RecentTopics, ", "))
	}
	if len(d.RecentMemories) > 0 {
		lines = append(lines, "**Recent Memories:**")
		for i, m := range d.RecentMemories {
			if i >= 5 {
				break
			}
			summary := m.Summary
			if len(summary) > 80 {
				summary = summary[:80]
			}
			ageSuffix := ""
			if len(m.CreatedAt) >= 10 {
				ageSuffix = " (" + m.CreatedAt[:10] + ")"
			}
			lines = append(lines, fmt.Sprintf("  - %s%s", summary, ageSuffix))
		}
	}
	if len(d.RecentMilestones) > 0 {
		lines = append(lines, "**Recent Milestones:**")
		for i, m := range d.RecentMilestones {
			if i >= 5 {
				break
			}
			summary := m.Summary
			if len(summary) > 120 {
				summary = summary[:120] + "…"
			}
			ageSuffix := ""
			if len(m.CreatedAt) >= 10 {
				ageSuffix = " (" + m.CreatedAt[:10] + ")"
			}
			lines = append(lines, fmt.Sprintf("  - %s%s", summary, ageSuffix))
		}
	}
	if d.AuditSummary != "" {
		lines = append(lines, "**"+d.AuditSummary+"**")
	}
	if len(d.GlobalRules) > 0 {
		lines = append(lines, "**Global Rules (shared across all agents on this workstation):**")
		for _, r := range d.GlobalRules {
			content := r.Content
			// F3-2 (alpha-final): rune-safe truncation; see the
			// matching site in gatherOpenWorks.
			if utf8.RuneCountInString(content) > 100 {
				runes := []rune(content)
				content = string(runes[:100]) + "…"
			}
			lines = append(lines, fmt.Sprintf("  - [w=%v] %s", r.Weight, content))
		}
	}
	if d.ScratchpadOrphans != "" {
		lines = append(lines, d.ScratchpadOrphans)
	}
	if len(d.OverdueWakes) > 0 {
		lines = append(lines, fmt.Sprintf("**Overdue Wakes (%d, capped at 5):**", len(d.OverdueWakes)))
		for _, w := range d.OverdueWakes {
			reason := w.Reason
			if len(reason) > 100 {
				reason = reason[:100] + "…"
			}
			kindLabel := ""
			if w.Kind != "" {
				kindLabel = " [" + w.Kind + "]"
			}
			lines = append(lines, fmt.Sprintf("  - %s%s overdue by %s — %s",
				w.ID, kindLabel, humanizeOverdueSecs(w.OverdueSecs), reason))
		}
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
	if len(d.OpenWorks) > 0 {
		lines = append(lines, fmt.Sprintf("**Pending Work (%d):**", len(d.OpenWorks)))
		for _, w := range d.OpenWorks {
			verif := ""
			if w.Verification != "" {
				verif = " [" + string(w.Verification) + "]"
			}
			lines = append(lines, fmt.Sprintf("  - %s%s [%s]", w.Title, verif, w.Pointer))
		}
	}
	if len(d.CompletedWorks) > 0 {
		// RECOMMENDED 10: completed-work section complements pending
		// work so the wake context is orientation-balanced. The agent
		// sees both "what's waiting on me" and "what I just shipped"
		// without having to query the work-item surface separately.
		lines = append(lines, fmt.Sprintf("**Recently Completed Work (%d):**", len(d.CompletedWorks)))
		for _, w := range d.CompletedWorks {
			verif := ""
			if w.Verification != "" {
				verif = " [" + string(w.Verification) + "]"
			}
			lines = append(lines, fmt.Sprintf("  - %s%s [%s]", w.Title, verif, w.Pointer))
		}
	}
	return strings.Join(lines, "\n")
}

// formatHandoff renders a Handoff as a structured block. Designed to be
// scannable but informative — the agent needs to know (1) when the last
// session was, (2) what it was doing, and (3) what state it left behind.
//
// F13: commitments and open questions round-trip through write → storage →
// wake. They render here (bounded) so the system-prompt projection carries
// the same continuity data the JSON projection does.
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
	summary := h.Summary
	if len(summary) > 400 {
		summary = summary[:400] + "…"
	}
	lines = append(lines, "  - Summary: "+summary)
	if len(h.Commitments) > 0 {
		lines = append(lines, "  - Commitments:")
		for i, c := range h.Commitments {
			if i >= 5 {
				break // bounded like every other wake block
			}
			if len(c) > 120 {
				c = c[:120] + "…"
			}
			lines = append(lines, "    - "+c)
		}
	}
	if len(h.OpenQuestions) > 0 {
		lines = append(lines, "  - Open questions:")
		for i, q := range h.OpenQuestions {
			if i >= 5 {
				break
			}
			if len(q) > 120 {
				q = q[:120] + "…"
			}
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
// scratchpadLifecycle classifies a scratchpad's decay state.
// These labels drive the operator-facing diagnostic output.
type scratchpadLifecycle struct {
	Status string // "active" | "expired" | "orphaned"
	Tag    string // "[Active]" | "[Expired]" | "[Orphaned]"
}

// scrubDecayAt converts the raw scan result (NULL, int, or time.Time)
// into a Unix timestamp seconds integer, or -1 if unparseable.
// Handles both integer Unix timestamps (test fixtures) and
// time.Time ISO8601 strings (production DB) and time.Time values.
func scrubDecayAt(raw interface{}) (int, bool) {
	if raw == nil {
		return 0, false // nil → no TTL
	}
	switch v := raw.(type) {
	case int64:
		return int(v), true
	case float64:
		return int(v), true
	case time.Time:
		return int(v.Unix()), true
	case string:
		t, err := time.Parse("2006-01-02 15:04:05", v)
		if err != nil {
			t2, err2 := time.Parse(time.RFC3339, v)
			if err2 != nil {
				return -1, false
			}
			return int(t2.Unix()), true
		}
		return int(t.Unix()), true
	default:
		return -1, false
	}
}

func classifyScratchpad(decayAtRaw interface{}, now int, sessionExists bool) scratchpadLifecycle {
	decaySecs, ok := scrubDecayAt(decayAtRaw)
	// nil or unparseable → no TTL set → permanently active
	if !ok || decaySecs > now {
		return scratchpadLifecycle{Status: "active", Tag: "[Active]"}
	}
	// TTL has crossed. If no session record exists, it cannot be
	// recovered through the normal promote/discard path — that is
	// the orphan condition: broken reference, not merely old content.
	if !sessionExists {
		return scratchpadLifecycle{Status: "orphaned", Tag: "[Orphaned]"}
	}
	return scratchpadLifecycle{Status: "expired", Tag: "[Expired]"}
}

// ScratchpadOrphansSummary returns the agent-facing wake-context block
// for scratchpads from previous sessions. Empty string when none exist.
//
// Classification contract (locked 2026-08-22):
//   - active   : decay_at > now  — still within TTL, not yet recoverable
//   - expired  : decay_at <= now AND session record exists — TTL crossed
//               normally; can be promoted or discarded via normal path
//   - orphaned : decay_at <= now AND no session record — broken reference;
//               cannot auto-recover; operator action required
//
// The orphan condition is an integrity anomaly, not a lifecycle state.
// Expired is a normal TTL completion — the vacuum path (gc_run
// --scratchpads) handles these. Active scratchpads are working state
// and do not require action.
//
// Internal query is deliberately broad (all previous-session scratchpads,
// ignoring decay_at at the WHERE layer) to surface orphans that might
// otherwise be hidden by a decay_at filter. Classification happens in
// the presentation layer, not the query.
func (dm *DatabaseManager) ScratchpadOrphansSummary() (string, error) {
	if dm == nil || dm.db == nil {
		return "", nil
	}

	currentSession := ""
	if s, err := dm.GetLastSession(); err == nil && s != nil {
		currentSession, _ = s["session_id"].(string)
	}

	now := int(time.Now().Unix())

	// LEFT JOIN against session_handoffs to detect the orphan condition:
	// a scratchpad with no corresponding session record cannot be
	// recovered through the normal promote/discard path.
	rows, err := dm.db.Query(`
		SELECT
			es.session_id,
			es.thesis,
			es.decay_at,
			(CAST(strftime('%s','now') AS INTEGER) - es.updated_at) / 3600.0 AS age_hours,
			sh.session_id IS NOT NULL AS session_exists
		FROM ephemeral_scratchpad es
		LEFT JOIN session_handoffs sh ON es.session_id = sh.session_id
		WHERE es.session_id != ?
		ORDER BY
			CASE
				WHEN es.decay_at IS NULL THEN 0
				WHEN es.decay_at > CAST(strftime('%s','now') AS INTEGER) THEN 0
				WHEN sh.session_id IS NOT NULL THEN 1
				ELSE 2
			END,
			es.updated_at DESC`,
		currentSession)
	if err != nil {
		return "", fmt.Errorf("scratchpad orphans query: %w", err)
	}
	defer rows.Close()

	counts := map[string]int{"active": 0, "expired": 0, "orphaned": 0}
	var lines []string

	for rows.Next() {
		var id, thesis string
		var decayAtRaw interface{} // NULL, int Unix timestamp, or time.Time
		var ageHours float64
		var sessionExists bool
		if err := rows.Scan(&id, &thesis, &decayAtRaw, &ageHours, &sessionExists); err != nil {
			return "", fmt.Errorf("scanning scratchpad row: %w", err)
		}

		lc := classifyScratchpad(decayAtRaw, now, sessionExists)
		counts[lc.Status]++

		line := fmt.Sprintf("  * %s session %s: %s",
			lc.Tag, id, previewThesisTruncated(thesis))

		// Orphaned scratchpads need operator attention and cannot be
		// auto-recovered. Expired-but-session-backed ones are normal
		// TTL completions — note the TTL crossing for provenance.
		if lc.Status == "orphaned" {
			line += "  ⚠ no session record — promote_scratchpad or discard required"
		} else if lc.Status == "expired" {
			line += "  (TTL expired)"
		}

		lines = append(lines, line)
	}

	if len(lines) == 0 {
		return "", nil
	}

	// Header mirrors the auditSummary pattern: three-way count gives
	// the operator an immediate Glance/Defer/Act signal.
	// "orphaned" is the only count that demands immediate attention.
	header := fmt.Sprintf(
		"- Ephemeral Scratchpads (active=%d, expired=%d, orphaned=%d):",
		counts["active"], counts["expired"], counts["orphaned"])

	// If any orphaned entries exist, surface the action reminder inline.
	// Expired entries are normal lifecycle — no action required from the
	// wake-context glance. Active entries are working state — leave them
	// quiet until TTL crosses.
	if counts["orphaned"] > 0 {
		header += "  Action Required: promote_scratchpad or discard for orphaned entries."
	}

	out := []string{header}
	out = append(out, lines...)
	return strings.Join(out, "\n"), nil
}

// humanizeOverdueSecs renders an overdue duration in seconds as the
// coarsest meaningful unit (s < 60 → "Ns"; m < 60 → "Nm"; h < 24 →
// "Nh"; else "Nd"). Coarse-grained on purpose — wake-context is a
// glance surface, not a clock. Used by the OverdueWakes renderer.
// Negative input is clamped to zero (defensive — gatherOverdueWakes
// filters future-dated rows but a renderer-only defense costs
// nothing).
func humanizeOverdueSecs(s int64) string {
	if s <= 0 {
		return "0s"
	}
	if s < 60 {
		return fmt.Sprintf("%ds", s)
	}
	if s < 3600 {
		return fmt.Sprintf("%dm", s/60)
	}
	if s < 86400 {
		return fmt.Sprintf("%dh", s/3600)
	}
	return fmt.Sprintf("%dd", s/86400)
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

// gatherOpenWorks returns up to 5 open work items for wake context,
// most-recently-touched first: ORDER BY updated_at DESC, created_at DESC
// as the determinism tiebreak.
//
// Ordering rationale (audit finding F3): created_at ASC surfaced the five
// OLDEST open items — stale historical work — while fresh work a few rows
// down never appeared. updated_at DESC puts current/relevant work at the
// top: a newly created work and an old-but-just-updated work both surface,
// while genuinely untouched old work sinks below the bound instead of
// permanently occupying it. Closed/cancelled work is excluded by the
// status='open' filter. Titles are truncated to 120 chars.
func (dm *DatabaseManager) gatherOpenWorks() []WakeContextWork {
	rows, err := dm.db.Query(`
		SELECT id, title, status, verification, created_at
		FROM works WHERE status = 'open'
		ORDER BY updated_at DESC, created_at DESC
		LIMIT 5
	`)
	if err != nil {
		dm.LogAudit(AuditWarn, "wake_context", "gatherOpenWorks: "+err.Error(), "", AuditContext{})
		return nil
	}
	defer rows.Close()

	out := make([]WakeContextWork, 0, 5)
	for rows.Next() {
		var w WakeContextWork
		var verification sql.NullString
		if err := rows.Scan(&w.ID, &w.Title, &w.Status, &verification, &w.CreatedAt); err != nil {
			dm.LogAudit(AuditWarn, "wake_context", "gatherOpenWorks scan: "+err.Error(), "", AuditContext{})
			continue
		}
		if verification.Valid {
			w.Verification = WorkVerification(verification.String)
		}
		w.Pointer = "mpm://work/" + w.ID
		// F3-2 (alpha-final): rune-safe truncation. A naive `len()` and
		// byte slice would mid-rune-slice titles containing CJK or emoji
		// characters, producing invalid UTF-8 that downstream JSON
		// parsers reject. Walk the string counting runes, then advance
		// the same number of bytes so the resulting slice is on a
		// valid boundary. Both gatherOpenWorks and gatherCompletedWorks
		// apply this fix.
		if utf8.RuneCountInString(w.Title) > 120 {
			runes := []rune(w.Title)
			w.Title = string(runes[:120])
		}
		out = append(out, w)
	}
	return out
}

// gatherCompletedWorks returns up to 5 done-status work items for wake
// context, most-recently-completed first: ORDER BY completed_at DESC,
// updated_at DESC as the determinism tiebreak.
//
// RECOMMENDED 10: completed work complements the open_works surface.
// Without it, a freshly-started session can see "5 things waiting on
// me" but cannot easily see "5 things I just shipped" — the orientation
// bias is asymmetric. completed_at is preferred over updated_at
// because a work item can be re-noted (and thus re-updated) without
// being re-completed; the completion event timestamp is the
// architecturally meaningful one.
//
// Cancelled items are excluded on purpose — they belong to a separate
// cognitive channel ("what got rejected and why") that the v1 wake
// context doesn't surface. Adding them here would conflate two
// distinct outcomes (shipped vs. abandoned). Titles truncated to 120
// chars to match gatherOpenWorks.
//
// Audit pattern: errors are logged but non-fatal (matches
// gatherOpenWorks). Wake context must always emit a non-nil slice
// (invariant 3) even on query failure, so the slice is initialized
// with make at the top.
func (dm *DatabaseManager) gatherCompletedWorks() []WakeContextWork {
	out := make([]WakeContextWork, 0, 5)
	rows, err := dm.db.Query(`
		SELECT id, title, status, verification, created_at
		FROM works WHERE status = 'done'
		ORDER BY COALESCE(completed_at, updated_at) DESC, updated_at DESC
		LIMIT 5
	`)
	if err != nil {
		dm.LogAudit(AuditWarn, "wake_context", "gatherCompletedWorks: "+err.Error(), "", AuditContext{})
		return out
	}
	defer rows.Close()

	for rows.Next() {
		var w WakeContextWork
		var verification sql.NullString
		if err := rows.Scan(&w.ID, &w.Title, &w.Status, &verification, &w.CreatedAt); err != nil {
			dm.LogAudit(AuditWarn, "wake_context", "gatherCompletedWorks scan: "+err.Error(), "", AuditContext{})
			continue
		}
		if verification.Valid {
			w.Verification = WorkVerification(verification.String)
		}
		w.Pointer = "mpm://work/" + w.ID
		// F3-2 (alpha-final): rune-safe truncation. A naive `len()` and
		// byte slice would mid-rune-slice titles containing CJK or emoji
		// characters, producing invalid UTF-8 that downstream JSON
		// parsers reject. Walk the string counting runes, then advance
		// the same number of bytes so the resulting slice is on a
		// valid boundary. Both gatherOpenWorks and gatherCompletedWorks
		// apply this fix.
		if utf8.RuneCountInString(w.Title) > 120 {
			runes := []rune(w.Title)
			w.Title = string(runes[:120])
		}
		out = append(out, w)
	}
	return out
}
