// activity_classifier.go — canonical classifiers for the recent_activity
// surface. The classifier is read-only and used by both:
//   - the writer paths (recordToolInvocation) so future rows are classified
//     correctly at insert time, and
//   - the read path (RecentActivity query) so historical rows can be
//     normalized without rewriting stored rows.
//
// Design principle: preserve the raw stored values (raw_actor_kind,
// raw_framework_name) but expose a single semantic "effective" value
// publicly. The classifier is the single source of truth; both writer
// and reader call it.
package internal

// Actor-kind vocabulary. Stable wire values — do not rename without
// a major version bump on recent_activity events.
const (
	ActorKindHuman  = "human"  // operator on the CLI: mpm-cli, shell, scripts
	ActorKindAgent  = "agent"  // host agent framework invocation: openclaw, pi, opencode, claude-code, hermes, mcp
	ActorKindDrill  = "drill"  // synthetic harness-driven invocation (test fixtures)
	ActorKindSystem = "system" // substrate-internal subsystem (cascade, gc, scheduler) — rarely via tool_invocations
	// ActorKindUnknown: the framework is neither a known agent framework
	// nor the canonical CLI default (empty/mpm-cli), AND the raw
	// actor_kind cannot be trusted as human (because an unknown
	// framework could itself be a shim around an agent we have not
	// catalogued). Honest representation of unresolved provenance —
	// surfaced in the default semantic feed so a future agent doesn't
	// silently mis-attribute a foreign-framework write.
	ActorKindUnknown = "unknown"
)

// Known host-agent frameworks. These are the canonical wire strings
// set via MPM_PROVENANCE_FRAMEWORK / MPM_FRAMEWORK env vars by each
// adapter. Keep this list in lockstep with docs/provenance-env.md.
//
// An invocation from one of these frameworks is an agent, NOT a
// human, regardless of what the CLI default audit_hook wrote before
// the bug was fixed.
var knownAgentFrameworks = map[string]struct{}{
	"mcp":         {}, // mpm-mcp direct transport
	"openclaw":    {}, // OpenClaw adapter
	"pi":          {}, // Pi adapter
	"opencode":    {}, // OpenCode adapter
	"claude-code": {}, // Claude Code adapter
	"hermes":      {}, // Hermes adapter
}

// EffectiveActorKind returns the semantic actor kind for a tool
// invocation, derived from the raw actor_kind (if explicit and
// trustworthy) and the framework_name.
//
// Precedence:
//  1. explicit trustworthy provenance — drill and system overrides
//     that should never be reclassified by framework semantics.
//  2. known framework semantics — knownAgentFrameworks (mcp/openclaw/
//     pi/opencode/claude-code/hermes) always classify as agent,
//     because the framework identity is more reliable than the
//     raw actor_kind value (which had a known historical bug).
//  3. canonical CLI path — empty / "mpm-cli" framework trusts
//     the raw value when explicit, falls back to human. This is
//     the audit_hook default: mpm-cli is operator CLI, never an
//     agent.
//  4. unknown framework — anything outside the known set AND not
//     the canonical CLI path. The framework is foreign or
//     unrecognized, so the raw actor_kind is NOT trustworthy (an
//     agent shim could write raw=human to impersonate an operator).
//     Honest classification: "unknown".
//
// The function is PURE — no DB access, no I/O. Callers must supply
// raw values from the tool_invocations row.
//
// Stable: the wire values are part of the recent_activity public
// contract; do not change them without bumping ContextVersion.
func EffectiveActorKind(rawActorKind, frameworkName string) string {
	// 1. Explicit trustworthy provenance wins.
	switch rawActorKind {
	case ActorKindDrill, ActorKindSystem:
		return rawActorKind
	}

	// 2. Known framework semantics — framework identity wins
	//    over raw actor_kind. This is the historical-bug-fix
	//    path: rows like (raw=human, framework=openclaw) that
	//    were written by the pre-fix audit_hook normalize to
	//    agent here. No agent framework legitimately identifies
	//    as framework_name="mpm-cli".
	if _, ok := knownAgentFrameworks[frameworkName]; ok {
		return ActorKindAgent
	}

	// 3. Canonical CLI path. Empty framework OR explicit
	//    framework_name="mpm-cli" identifies the operator CLI;
	//    raw human/agent is trusted, anything else defaults to
	//    human. This preserves the audit_hook default for the
	//    mpm binary invoked from a shell.
	if frameworkName == "" || frameworkName == "mpm-cli" {
		if rawActorKind == ActorKindAgent || rawActorKind == ActorKindHuman {
			return rawActorKind
		}
		return ActorKindHuman
	}

	// 4. Unknown framework. We have no provenance evidence for
	//    the framework AND cannot trust the raw actor_kind (a
	//    foreign-framework shim could write raw=human to look
	//    human). Honest classification is "unknown" — surfaced
	//    in the default semantic feed so it isn't silently
	//    relabeled as a human operator.
	return ActorKindUnknown
}

// IsAgentFramework returns true if frameworkName is one of the
// canonical host-agent adapters. Used by callers that need the
// classification without going through EffectiveActorKind.
func IsAgentFramework(frameworkName string) bool {
	_, ok := knownAgentFrameworks[frameworkName]
	return ok
}

// ActionClass is the semantic classification of a (tool, action)
// pair for the recent_activity stream.
//
// Excluded:    do not appear in recent_activity at all
// Mutating:    successful semantic durable write — primary feed content
// ReadOnly:    pure read; excluded by default
// Lifecycle:   state-transition side effect (e.g. fired=1 on a wake)
// Diagnostic:  probe cache, health timestamps — substrate bookkeeping
// Maintenance: gc, compact, migrate — system bookkeeping
type ActionClass string

const (
	ActionClassMutating   ActionClass = "mutating"
	ActionClassReadOnly   ActionClass = "read_only"
	ActionClassLifecycle  ActionClass = "lifecycle"
	ActionClassDiagnostic ActionClass = "diagnostic"
	ActionClassMaintenance ActionClass = "maintenance"
)

// ClassifyAction returns the semantic classification for a
// (tool_name, action) pair. Unknown pairs default to read_only —
// the conservative choice that excludes rather than pollutes the
// activity feed.
//
// The map is the single source of truth. Both writer and reader
// paths call it; do not branch on (tool, action) in call sites.
func ClassifyAction(toolName, action string) ActionClass {
	if v, ok := actionClassMap[keyOf(toolName, action)]; ok {
		return v
	}
	// Tool-level defaults (catch-all action names).
	if v, ok := toolClassMap[toolName]; ok {
		return v
	}
	// Unknown tool/action: exclude conservatively.
	return ActionClassReadOnly
}

func keyOf(tool, action string) string { return tool + "/" + action }

// actionClassMap pins the per-(tool,action) classification. Add new
// entries here when introducing new mutating actions; do NOT branch
// on tool/action strings in call sites.
var actionClassMap = map[string]ActionClass{
	// mpm_memory
	"mpm_memory/save":               ActionClassMutating,
	"mpm_memory/delete":             ActionClassMutating, // soft-delete
	"mpm_memory/restore":            ActionClassMutating,
	"mpm_memory/shred":              ActionClassMutating,
	"mpm_memory/reinforce":          ActionClassMutating,
	"mpm_memory/weaken":             ActionClassMutating,
	"mpm_memory/snooze":             ActionClassMutating,
	"mpm_memory/set_weight":         ActionClassMutating,
	"mpm_memory/patch":              ActionClassMutating,
	"mpm_memory/promote":            ActionClassMutating,
	"mpm_memory/synthesize":         ActionClassMutating, // synthesizes a new memory row
	"mpm_memory/challenge":          ActionClassMutating, // weakens + creates theory
	"mpm_memory/commit_milestone":   ActionClassMutating,
	// mpm_memory read paths default via toolClassMap["mpm_memory"]

	// mpm_theories
	"mpm_theories/propose": ActionClassMutating,
	"mpm_theories/resolve": ActionClassMutating,

	// mpm_decisions
	"mpm_decisions/record":      ActionClassMutating,
	"mpm_decisions/supersede":   ActionClassMutating,
	"mpm_decisions/invalidate":  ActionClassMutating,

	// mpm_lessons
	"mpm_lessons/save":   ActionClassMutating,
	"mpm_lessons/delete": ActionClassMutating,
	"mpm_lessons/shred":  ActionClassMutating,

	// mpm_topics
	"mpm_topics/create": ActionClassMutating,
	"mpm_topics/link":   ActionClassMutating,
	"mpm_topics/unlink": ActionClassMutating,

	// mpm_references
	"mpm_references/save":   ActionClassMutating,
	"mpm_references/delete": ActionClassMutating,

	// mpm_evidence
	"mpm_evidence/save":   ActionClassMutating,
	"mpm_evidence/attach": ActionClassMutating,

	// mpm_confidence
	"mpm_confidence/update": ActionClassMutating,

	// mpm_handoff
	"mpm_handoff/write": ActionClassMutating,
	"mpm_handoff/shred": ActionClassMutating,
	// mpm_handoff/read is the wake-context delivery side effect;
	// classifying it as Lifecycle excludes it from agent activity
	// without losing the audit trail.
	"mpm_handoff/read": ActionClassLifecycle,

	// mpm_scratchpad
	"mpm_scratchpad/flush":    ActionClassMutating,
	"mpm_scratchpad/promote":  ActionClassMutating,
	"mpm_scratchpad/discard":  ActionClassMutating,
	"mpm_scratchpad/read":     ActionClassReadOnly,

	// mpm_work
	"mpm_work/create": ActionClassMutating,
	"mpm_work/update": ActionClassMutating,
	"mpm_work/complete": ActionClassMutating,
	"mpm_work/cancel": ActionClassMutating,
	"mpm_work/note": ActionClassMutating,
	"mpm_work/reopen": ActionClassMutating,
	"mpm_work/resolve_contradiction": ActionClassMutating,

	// mpm_wakes
	"mpm_wakes/schedule":    ActionClassMutating,
	"mpm_wakes/resolve":     ActionClassMutating,
	"mpm_wakes/upsert_task": ActionClassMutating,
	"mpm_wakes/delete_task": ActionClassMutating,
	"mpm_wakes/check":       ActionClassLifecycle,  // fired=1 side effect
	"mpm_wakes/check_pending_event": ActionClassLifecycle,

	// mpm_context
	"mpm_context/write_handoff":      ActionClassMutating, // routes to mpm_handoff
	"mpm_context/record_global_rule": ActionClassMutating,
	"mpm_context/retire_global_rule": ActionClassMutating,
	"mpm_context/promote_to_global":  ActionClassMutating,
	"mpm_context/read_wake_context":  ActionClassLifecycle, // consumes handoff
	"mpm_context/read_handoff":       ActionClassReadOnly,
	"mpm_context/read_directives":     ActionClassReadOnly,
	"mpm_context/proactive_recall_hint": ActionClassReadOnly,
	"mpm_context/query_global_rules":  ActionClassReadOnly,
	"mpm_context/route":               ActionClassReadOnly,

	// mpm_skills
	"mpm_skills/save":   ActionClassMutating,
	"mpm_skills/delete": ActionClassMutating,
	"mpm_skills/promote_to_global": ActionClassMutating,
	"mpm_skills/workshop":          ActionClassMutating,

	// log_to_changelog — special tool_name used internally by mpm_handoff
	// sub-routes; classify as mutating if encountered.
	"log_to_changelog/write": ActionClassMutating,

	// mpm_system — all maintenance; never agent-authored.
	"mpm_system/gc_run":         ActionClassMaintenance,
	"mpm_system/compact":        ActionClassMaintenance,
	"mpm_system/migrate":        ActionClassMaintenance,
	"mpm_system/snooze_cluster": ActionClassMaintenance,
	"mpm_system/unsnooze_cluster": ActionClassMaintenance,
	"mpm_system/resolve_cluster":  ActionClassMaintenance,
	"mpm_system/annotate_cluster": ActionClassMaintenance,
	"mpm_system/query_audit_log":   ActionClassReadOnly,
	"mpm_system/list_clusters":     ActionClassReadOnly,
	"mpm_system/health_check":      ActionClassDiagnostic,
	"mpm_system/critic_findings":   ActionClassReadOnly,

	// mpm_resolve — read-only query surface.
	"mpm_resolve/query": ActionClassReadOnly,
	"mpm_resolve/pointer": ActionClassReadOnly,

	// Doctor/probe — diagnostic, must not appear as semantic activity.
	"mpm_doctor/probe": ActionClassDiagnostic,
	"mpm_doctor/run":   ActionClassDiagnostic,
}

// toolClassMap provides the default classification for unknown
// (tool, action) pairs within known tools.
var toolClassMap = map[string]ActionClass{
	"mpm_memory":     ActionClassReadOnly,
	"mpm_theories":   ActionClassReadOnly,
	"mpm_decisions":  ActionClassReadOnly,
	"mpm_lessons":    ActionClassReadOnly,
	"mpm_topics":     ActionClassReadOnly,
	"mpm_references": ActionClassReadOnly,
	"mpm_evidence":   ActionClassMutating, // attach defaults to mutating
	"mpm_confidence": ActionClassMutating,
	"mpm_handoff":    ActionClassReadOnly,
	"mpm_scratchpad": ActionClassReadOnly,
	"mpm_work":       ActionClassReadOnly,
	"mpm_wakes":      ActionClassReadOnly,
	"mpm_context":    ActionClassReadOnly,
	"mpm_skills":     ActionClassReadOnly,
	"mpm_system":     ActionClassMaintenance,
	"mpm_resolve":    ActionClassReadOnly,
}

// IsMutating returns true iff the (tool, action) pair is a semantic
// durable write that belongs in the default recent_activity feed.
func IsMutating(tool, action string) bool {
	return ClassifyAction(tool, action) == ActionClassMutating
}

// ActionClassMapHas reports whether an exact (tool, action) entry
// exists in the explicit actionClassMap. Exported so the tools
// package's registry-completeness test can assert that every
// registered public pair has an intentional classification without
// having to redefine the map.
func ActionClassMapHas(key string) (ActionClass, bool) {
	v, ok := actionClassMap[key]
	return v, ok
}

// ToolClassMapHas reports whether a tool-level default exists in
// toolClassMap. Exported for the same reason as ActionClassMapHas.
func ToolClassMapHas(tool string) (ActionClass, bool) {
	v, ok := toolClassMap[tool]
	return v, ok
}

// KnownAgentFrameworkNames returns the sorted list of canonical
// host-agent framework names. Exported for tests and for
// documentation that needs to enumerate the known set without
// duplicating the list.
func KnownAgentFrameworkNames() []string {
	out := make([]string, 0, len(knownAgentFrameworks))
	for k := range knownAgentFrameworks {
		out = append(out, k)
	}
	return out
}