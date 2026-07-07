// Package seed holds the Baseline Cognitive Bootstrap — the reference
// prime directives that close MPM's advanced cognitive loops.
//
// These directives are NOT auto-loaded at install time. The "Truth is
// external" and "no auto-noise" principles forbid silent state
// mutation just because the binary booted. Instead, the operator runs
//
//	mpm ops init directives
//
// to seed them. The command is idempotent (existing directives are
// detected by exact-content match and skipped), so it's safe to re-run.
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
		Content: "Before session_end: call list_active_clusters. For each unknown cluster that represents an unresolved critical failure, include its cluster_key and a one-line context note in the open_questions payload of session_end. This is how clusters survive the 7-day rolling-window decay — the agent explicitly carries forward what matters across session boundaries (and across vacations). Do NOT carry forward clusters that are snoozed (status=snoozed), resolved (status=resolved), or whose cluster_key appears in an active theory/decision (those are already tracked; re-mentioning is noise). Do NOT mix cluster_keys into commitments — commitments are promises of future labor, clusters are unresolved state of the system. The two fields have different semantics.",
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
}