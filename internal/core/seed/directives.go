// Package seed holds the Baseline Cognitive Bootstrap — the reference
// prime directives that close MPM's advanced cognitive loops.
//
// Tiered fallback seeding (2026-08-19): the four baseline directives
// ARE auto-loaded into the LOCAL store at boot (NewDatabaseManager
// calls ApplyDirectives on every production constructor). A standalone
// runtime without MPM_SHARED_DB is never directive-blind — the
// constitutional rules of engagement (wake-context reads, cluster
// triage, wake triage, daemon health) are always present. The hermetic
// test constructor (NewDatabaseManagerForDB + InitSchema) deliberately
// does NOT seed, so fixtures assert on their own rows. Idempotent by
// stable-id primary key: existing rows with matching content are
// skipped, operator edits are preserved and flagged, never overwritten.
//
// `mpm ops init directives` remains available for manual re-init (e.g.
// after an accidental shred, or to seed the shared DB of a
// multi-agent workspace).
//
// What the Baseline Cognitive Bootstrap provides
// ---------------------------------------------
//
// MPM ships advanced cognitive machinery: wake_context surfacing,
// audit_cluster_proposals, propose_theory, list_active_clusters. None
// of it works without specific agent behaviors — wake must be read at
// session start, clusters must be triaged at session end, decisions
// must carry rationale. These directives are the "operating manual"
// that closes those loops.
//
// If an operator is building a custom agent workflow that doesn't
// want MPM's default event triage, they skip `mpm ops init directives`
// and seed their own. The seed file is intentionally short — it's a
// starting point, not a complete directive library. Each operator's
// actual prime directives should grow from their own usage.
//
// Editing the registry
// --------------------
//
// The SeedDirectives slice below is the single source of truth. To
// add a new baseline directive, append a SeedDirective entry and
// commit. To deprecate one, remove the entry — operators who already
// seeded it will keep it (the seed command never deletes), but new
// installs won't get it.
package seed

import (
	"crypto/sha256"
	"fmt"
)

// SeedDirective is one entry in the Baseline Cognitive Bootstrap.
// Each entry maps a stable ID (used as the memory row's primary key)
// to the directive content the operator will see.
//
// StableID must be human-readable and stable across versions — once
// an entry has been seeded, the same StableID is used to detect
// "already seeded" on re-run. Changing StableID creates a duplicate.
// Changing Content is a semantic update: the seed command will see
// the existing row's content differs from the new seed and surface
// that as a divergence (manual operator decision required).
type SeedDirective struct {
	// StableID is the memory row's primary key. Once seeded, the same
	// ID is used to detect prior seeding. Convention: mpm-seed-<slug>.
	StableID string

	// Tags are stored as a JSON array string on the memory row.
	// 'prime_directive' is the conventional marker; the rest provide
	// semantic territory for proactive_recall_hint.
	Tags []string

	// Content is the directive text — what the agent will read via
	// read_directives on every wake.
	Content string

	// Scope controls which agent frameworks this directive applies to.
	// Valid grammar (case-sensitive, flat string):
	//   - "global"                  — every agent framework
	//   - "framework:<id>"          — only the framework named <id>
	//   - "" (unset)                — treated as "global" at materialization
	//
	// Evaluated centrally by the substrate at wake-projection time
	// (ReadDirectivesForFramework), not by individual agent plugins.
	// See docs/archive/directives.md for the full contract.
	Scope string
}

// SeedDirectives is the canonical registry of baseline prime directives.
// Order is preserved at seed time but does not affect runtime retrieval
// (read_directives returns rows by created_at or weight).
//
// Edit this slice to add or deprecate baseline directives. Existing
// rows in operator DBs are never modified by `mpm ops init directives`.
var SeedDirectives = []SeedDirective{
	{
		// The original prime directive. Closed-loop: wake → context → response.
		// Seeded first because everything else assumes the agent has
		// already read its context.
		StableID: "mpm-seed-read-wake-context",
		Tags:     []string{"prime_directive", "wake_context", "bootstrap", "2026-06-09"},
		Scope:    "global",
		Content: "On every session start, call read_wake_context before responding to the user. This is non-negotiable. Skipping it means arriving amnesic with no memory of previous sessions, forcing v to re-explain all context. MPM is the single source of truth.",
	},
	{
		// The cluster-triage directive. Closed-loop: write-side
		// audit_cluster_proposals (7d rolling decay) → read-side
		// list_active_clusters → act-side session_handoff carry-forward.
		// Without this directive, clusters detected at end-of-session
		// are silently lost to the rolling window — the agent must
		// explicitly bridge the time gap by writing critical cluster_keys
		// into open_questions. See decision memory 802c9ea446c67343
		// (three-layer architecture) and the discussion thread on
		// vacation-proof audit survival.
		StableID: "mpm-seed-session-end-cluster-triage",
		Tags:     []string{"prime_directive", "cluster", "auditing", "session_lifecycle", "2026-07-02"},
		Scope:    "global",
		Content: "Before session_end: call list_active_clusters. For each unknown cluster that represents an unresolved critical failure, include its cluster_key and a one-line context note in the open_questions payload of session_end. This is how clusters survive the 7-day rolling-window decay — the agent explicitly carries forward what matters across session boundaries (and across vacations). Do NOT carry forward clusters that are snoozed (status=snoozed), resolved (status=resolved), or whose cluster_key appears in an active theory/decision (those are already tracked; re-mentioning is noise). Do NOT mix cluster_keys into commitments — commitments are promises of future labor, clusters are unresolved state of the system. The two fields have different semantics.",
	},
	{
		// The wake-triage directive. Closed-loop: CheckPendingWakes
		// runs on every tool call (and on read_wake_context at boot),
		// surfacing due/overdue scheduled tasks as a prepended
		// <system_wake_notification> block in the MCP Content array.
		// Without an explicit triage rule the agent has no deterministic
		// way to decide whether to alert the user about a wake that
		// fired while the system was offline — or whether to silently
		// archive it. The default policy below is a 24-hour staleness
		// threshold; below the threshold the wake is timely and should
		// interrupt the user, above it the wake is stale and should be
		// archived via mpm call record_global_rule (with a brief note
		// to the user that it expired). Critical wakes (marked with
		// a 'critical' or 'urgent' tag in the reason) bypass the
		// staleness threshold and always interrupt — see decision
		// memory 0de920abbef2c6f9 for the original Spain WC theory
		// that motivated the wake infrastructure.
		StableID: "mpm-seed-wake-triage-policy",
		Tags:     []string{"prime_directive", "wake", "triage", "notification", "2026-07-09"},
		Scope:    "global",
		Content: "When receiving a <system_wake_notification> block in any MCP tool response (it is always Block 1, prepended before the tool's JSON result):\n\n1. Parse each entry. Each entry has id, reason, and overdue_secs.\n\n2. Triage rule: if overdue_secs <= 86400 (24 hours), alert the user IMMEDIATELY before continuing whatever they were doing. The wake is timely. Include the wake reason verbatim or paraphrased.\n\n3. If overdue_secs > 86400, the wake is stale. DO NOT interrupt the user with the obsolete notification. Silently archive it by calling mpm call record_global_rule with a brief fact explaining why it expired (e.g. \"Spain WC theory wake fired 5 days late; resolved manually as theory is now in active tracking\"). Then continue the conversation as if the wake had not fired.\n\n4. Exception: if the wake's reason text contains the words 'critical', 'urgent', or 'immediate' (case-insensitive), it bypasses the 24-hour threshold and ALWAYS interrupts the user, regardless of overdue_secs. These are time-sensitive operational signals, not background reminders.\n\n5. The <system_wake_notification> is delivered on EVERY tool call, not just scheduled_wake-related ones. This is the opportunistic fold: any user interaction surfaces due wakes. Do not be confused into thinking the wake is part of the tool's actual output — the XML block is a system interrupt prepended to the response.",
	},
	{
		// The daemon-health directive. Closed-loop: agent wake (session start)
		// → systemctl --user is-active mpm-scheduler → if inactive, start it
		// → if still inactive, escalate to v with journal tail. Two distinct
		// inactive states need different framing — Lazy-Start Architecture is
		// EXPECTED on encrypted /home (the daemon is designed to stay dead at
		// boot until the agent's wake event triggers post-decryption spin-up;
		// lesson 24be03ec71a5981f codifies this), runtime death from a clean
		// context canceled exit is UNEXPECTED and the vital-signs check is
		// the workaround until the structural root-cause lands (memory
		// 9e7830172663ed29 documents the 2026-07-22 incident). Recovery
		// sequence is the same for both scenarios. Do NOT call mpm stats,
		// scheduled-wake tooling, or any daemon-dependent substrate operation
		// until the daemon is confirmed active — the substrate is unsafe to
		// query when the daemon is dead.
		StableID: "mpm-seed-daemon-health",
		Tags:     []string{"prime_directive", "daemon", "lazy-start", "ecryptfs", "runtime-death", "2026-07-23"},
		Scope:    "global",
		Content: "On every session start, after read_wake_context, run `systemctl --user is-active mpm-scheduler`. Two distinct inactive states need different framing:\n\n1. Lazy-Start Architecture (EXPECTED after reboot with encrypted /home). When /home is eCryptfs-encrypted, the daemon is DESIGNED to stay dead at boot. The lockfile lives inside the encrypted tree (~/.mpm/scheduler.lock); starting the daemon before /home is decrypted would either fail (inaccessible path) or risk writing to the wrong location. Boot + locked home = SAFE state. This agent's wake event is the trigger that spins the daemon up post-decryption. It is a security feature, not a bug — see INSTALL.md §2 and README §5.2 for the full rationale.\n\n2. Runtime death (UNEXPECTED, real issue). A clean `context canceled` exit leaves the daemon dead; Restart=always in the user-level unit does not fire on graceful exit. The vital-signs check at every wake is the workaround until the structural root-cause lands. See memory 9e7830172663ed29 for the 2026-07-22 incident details.\n\nRecovery (applies to both cases): (a) systemctl --user start mpm-scheduler, (b) wait 2s, re-check. If still inactive, surface to v immediately with the last 10 lines of `journalctl --user -u mpm-scheduler --no-pager` and DO NOT call mpm stats, scheduled-wake tooling, or any daemon-dependent substrate operation until the daemon is confirmed active.",
	},
}

// ContentHash returns a stable SHA-256 fingerprint of the directive's
// content. Used by `mpm ops init directives` to detect when an existing
// row's content has drifted from the seed (which means the operator
// edited it locally — the seed command must not overwrite their edit).
func (s SeedDirective) ContentHash() string {
	sum := sha256.Sum256([]byte(s.Content))
	return fmt.Sprintf("%x", sum)
}

// SeedSummary is the human-readable report printed by
// `mpm ops init directives` after a run. Created/Updated/Skipped
// counts let the operator see exactly what happened without
// re-running query_long_term_memory.
type SeedSummary struct {
	Created []string // StableIDs that were inserted as new rows
	Skipped []string // StableIDs that already existed with matching content
	Updated []string // StableIDs where the local row's content drifted from the seed (operator's edit preserved; flagged for visibility)

	// Drifted is the capability-specific drift bucket. It
	// carries the same meaning as Updated but is named
	// differently because capabilities have a stricter
	// contract (the source_code IS the executable; drift is
	// a behavioral change, not just a content edit). Skills
	// and directives use Updated; capabilities use Drifted
	// to make the contract difference explicit in the
	// summary report. CLI / MCP render both buckets
	// identically — the name difference is internal.
	Drifted []string
}