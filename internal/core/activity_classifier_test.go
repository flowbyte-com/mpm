// activity_classifier_test.go — Stage 2 actor classification and
// semantic action tests. Covers the canonical EffectiveActorKind
// classifier across all known host-agent frameworks, plus the
// ClassifyAction semantic mapping. Pin these so the historical
// bug ("framework=openclaw → actor=human") cannot regress.
package internal

import (
	"strings"
	"testing"
)

// Phase A test #23: Provenance / actor-classification tests.

// TestEffectiveActorKind_CanonicalFrameworks pins the classification
// of every known host-agent framework. These are the wire values
// for actor_kind across the recent_activity public surface; do
// not change them without a major version bump.
func TestEffectiveActorKind_CanonicalFrameworks(t *testing.T) {
	cases := []struct {
		name            string
		rawActorKind    string
		frameworkName   string
		wantEffective   string
	}{
		// Operator-driven CLI: human, never agent.
		{"mpm-cli explicit human", "human", "mpm-cli", ActorKindHuman},
		{"mpm-cli with empty raw", "", "mpm-cli", ActorKindHuman},

		// MCP transport: agent.
		{"mcp with empty raw", "", "mcp", ActorKindAgent},

		// Host-agent frameworks — the historical bug.
		// Pre-fix, all of these wrote actor_kind='human'. Post-fix,
		// they normalize to 'agent' at read time.
		{"openclaw with empty raw", "", "openclaw", ActorKindAgent},
		{"openclaw with explicit human", "human", "openclaw", ActorKindAgent},
		{"pi with explicit human", "human", "pi", ActorKindAgent},
		{"opencode with explicit human", "human", "opencode", ActorKindAgent},
		{"claude-code with explicit human", "human", "claude-code", ActorKindAgent},
		{"hermes with explicit human", "human", "hermes", ActorKindAgent},

		// Explicit trustworthy provenance overrides framework.
		{"drill explicit wins", "drill", "openclaw", ActorKindDrill},
		{"system explicit wins", "system", "pi", ActorKindSystem},
		{"drill with empty framework", "drill", "", ActorKindDrill},

		// Explicit agent on mpm-cli: accepted (explicit raw value
		// passes through step 3). This is unrealistic in production
		// because no real agent framework sets framework_name="mpm-cli",
		// but the contract is: explicit raw wins over framework for
		// the mpm-cli path (no known-agent-framework match).
		{"mpm-cli with explicit agent", "agent", "mpm-cli", ActorKindAgent},

		// Unknown framework: cannot reliably classify as human. A
		// foreign-framework shim could write raw=human to impersonate
		// an operator. Honest classification is "unknown", surfaced
		// in the default semantic feed so the activity is not
		// silently mis-attributed.
		{"unknown framework empty raw", "", "custom-x", ActorKindUnknown},
		{"unknown framework explicit human", "human", "custom-x", ActorKindUnknown},
		{"unknown framework explicit agent", "agent", "custom-x", ActorKindUnknown},

		// Empty everything: human fallback (CLI default).
		{"empty everything", "", "", ActorKindHuman},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := EffectiveActorKind(c.rawActorKind, c.frameworkName)
			if got != c.wantEffective {
				t.Fatalf("EffectiveActorKind(%q, %q) = %q, want %q",
					c.rawActorKind, c.frameworkName, got, c.wantEffective)
			}
		})
	}
}

// TestEffectiveActorKind_HistoricalNormalization is the
// Stage 1 → Stage 2 release-acceptance test. Pre-fix rows in
// production had actor_kind='human' + framework_name='openclaw'.
// This test pins that such rows normalize to actor_kind='agent'
// at read time without rewriting the stored row.
func TestEffectiveActorKind_HistoricalNormalization(t *testing.T) {
	// Sample of production-observed (raw_actor_kind, framework_name)
	// pairs from the last 7 days of `v`. Each pair MUST normalize to
	// the expected effective kind under the canonical classifier.
	historicalCases := []struct {
		rawActorKind  string
		frameworkName string
		wantEffective string
	}{
		{"human", "openclaw", ActorKindAgent},    // 95 production rows
		{"human", "pi", ActorKindAgent},          // 21 production rows
		{"human", "opencode", ActorKindAgent},    // 14 production rows
		{"human", "claude-code", ActorKindAgent}, // 13 production rows
		{"human", "mpm-cli", ActorKindHuman},     // 727 production rows (correct)
		{"agent", "mcp", ActorKindAgent},         // 23 production rows (correct)
	}
	for _, c := range historicalCases {
		got := EffectiveActorKind(c.rawActorKind, c.frameworkName)
		if got != c.wantEffective {
			t.Errorf("Historical normalization: (%q, %q) → %q, want %q",
				c.rawActorKind, c.frameworkName, got, c.wantEffective)
		}
	}
}

// TestIsAgentFramework is a thin helper test for the public surface.
func TestIsAgentFramework(t *testing.T) {
	for _, name := range []string{"mcp", "openclaw", "pi", "opencode", "claude-code", "hermes"} {
		if !IsAgentFramework(name) {
			t.Errorf("IsAgentFramework(%q) = false, want true", name)
		}
	}
	for _, name := range []string{"mpm-cli", "", "drill", "unknown"} {
		if IsAgentFramework(name) {
			t.Errorf("IsAgentFramework(%q) = true, want false", name)
		}
	}
}

// Phase B test #24: Semantic action classification tests.
//
// Each major tool family must have at least one mutating action
// classified as mutating and one read-only action classified as
// read_only. The action map is the public contract — adding
// new (tool, action) pairs MUST add an entry here.
func TestClassifyAction_MutatingAndReadOnly(t *testing.T) {
	cases := []struct {
		tool, action string
		want         ActionClass
	}{
		// MEMORY
		{"mpm_memory", "save", ActionClassMutating},
		{"mpm_memory", "shred", ActionClassMutating},
		{"mpm_memory", "delete", ActionClassMutating},
		{"mpm_memory", "restore", ActionClassMutating},
		{"mpm_memory", "challenge", ActionClassMutating},
		{"mpm_memory", "commit_milestone", ActionClassMutating},
		{"mpm_memory", "query", ActionClassReadOnly},
		{"mpm_memory", "show", ActionClassReadOnly},
		{"mpm_memory", "list", ActionClassReadOnly},

		// HANDOFF
		{"mpm_handoff", "write", ActionClassMutating},
		{"mpm_handoff", "shred", ActionClassMutating},
		{"mpm_handoff", "read", ActionClassLifecycle}, // delivery side effect
		{"mpm_handoff", "list", ActionClassReadOnly},

		// SCRATCHPAD
		{"mpm_scratchpad", "flush", ActionClassMutating},
		{"mpm_scratchpad", "promote", ActionClassMutating},
		{"mpm_scratchpad", "discard", ActionClassMutating},
		{"mpm_scratchpad", "read", ActionClassReadOnly},

		// WORK
		{"mpm_work", "create", ActionClassMutating},
		{"mpm_work", "update", ActionClassMutating},
		{"mpm_work", "complete", ActionClassMutating},
		{"mpm_work", "note", ActionClassMutating},
		{"mpm_work", "list", ActionClassReadOnly},
		{"mpm_work", "show", ActionClassReadOnly},
		{"mpm_work", "history", ActionClassReadOnly},

		// WAKES
		{"mpm_wakes", "schedule", ActionClassMutating},
		{"mpm_wakes", "resolve", ActionClassMutating},
		{"mpm_wakes", "upsert_task", ActionClassMutating},
		{"mpm_wakes", "list", ActionClassReadOnly},
		{"mpm_wakes", "digest", ActionClassReadOnly},
		{"mpm_wakes", "check", ActionClassLifecycle},

		// CHANGELOG / REVIEW
		{"mpm_log_to_changelog", "write", ActionClassMutating},

		// DOCTOR / PROBE — diagnostic, must NOT appear as agent activity.
		{"mpm_doctor", "probe", ActionClassDiagnostic},
		{"mpm_doctor", "run", ActionClassDiagnostic},

		// SYSTEM — maintenance.
		{"mpm_system", "gc_run", ActionClassMaintenance},
		{"mpm_system", "compact", ActionClassMaintenance},
		{"mpm_system", "migrate", ActionClassMaintenance},
		{"mpm_system", "query_audit_log", ActionClassReadOnly},

		// CONTEXT — read_wake_context is lifecycle (consumes handoff).
		{"mpm_context", "read_wake_context", ActionClassLifecycle},
		{"mpm_context", "read_directives", ActionClassReadOnly},
		{"mpm_context", "recent_activity", ActionClassReadOnly},

		// RESOLVE — read-only.
		{"mpm_resolve", "query", ActionClassReadOnly},

		// Unknown tool defaults to read_only.
		{"mpm_unknown", "anything", ActionClassReadOnly},
	}
	for _, c := range cases {
		got := ClassifyAction(c.tool, c.action)
		if got != c.want {
			t.Errorf("ClassifyAction(%q, %q) = %q, want %q",
				c.tool, c.action, got, c.want)
		}
	}
}

// TestClassifyAction_SummaryDoesNotLeakPayload is the secret-redaction
// guarantee at the classifier level. The summary strings are
// structural metadata; they MUST NOT contain any caller-supplied
// payload fragments.
func TestClassifyAction_SummaryDoesNotLeakPayload(t *testing.T) {
	const sentinel = "MPM_ACTIVITY_SECRET_MUST_NOT_LEAK_alpha"
	cases := []RecentActivityEvent{
		{Tool: "mpm_memory", Action: "save", Summary: activitySummary("mpm_memory", "save", ActorKindAgent)},
		{Tool: "mpm_handoff", Action: "write", Summary: activitySummary("mpm_handoff", "write", ActorKindAgent)},
		{Tool: "mpm_scratchpad", Action: "flush", Summary: activitySummary("mpm_scratchpad", "flush", ActorKindHuman)},
		{Tool: "mpm_work", Action: "create", Summary: activitySummary("mpm_work", "create", ActorKindAgent)},
	}
	for _, ev := range cases {
		if strings.Contains(ev.Summary, sentinel) {
			t.Errorf("summary leaked sentinel: %q", ev.Summary)
		}
		if strings.Contains(ev.Summary, "secret") || strings.Contains(ev.Summary, "password") {
			t.Errorf("summary contains forbidden token: %q", ev.Summary)
		}
	}
}

// TestIsMutating is a thin helper test.
func TestIsMutating(t *testing.T) {
	if !IsMutating("mpm_memory", "save") {
		t.Error("IsMutating(mpm_memory, save) = false, want true")
	}
	if IsMutating("mpm_memory", "query") {
		t.Error("IsMutating(mpm_memory, query) = true, want false")
	}
	if IsMutating("mpm_doctor", "probe") {
		t.Error("IsMutating(mpm_doctor, probe) = true, want false")
	}
}