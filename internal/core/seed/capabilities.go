// Package seed (capabilities.go) — Baseline Capability Library.
//
// Mirrors skills.go: a curated set of capabilities that ship with
// the mpm binary and provide day-1 utility to agents working with
// the capability subsystem. The set is intentionally small — a
// starting point, not a complete library.
//
// Why these ship as "validated" (not "draft"):
//
//	Capabilities go through a lifecycle — draft → linted →
//	validated → probation → active → ... — to earn trust via
//	empirical evidence. Seeded capabilities skip the linter and
//	dry-run (their source is already reviewed and curated) but
//	still enter at "validated" so the forge tick promotes them
//	to probation on first invocation. The lifecycle is fully
//	exercised — probation is earned normally.
//
//	Inserting at "active" would bypass probation entirely, which
//	defeats the "earned trust" contract for the most-used
//	primitives. Inserting at "draft" would waste forge cycles on
//	lint+scan of curated source. "Validated" is the right seam.
//
// Drift detection:
//
//	ApplyCapabilities computes sha256(content) for each seeded
//	capability and compares to metadata.content_hash on the
//	existing row. A mismatch means an operator has edited the
//	seeded source — the edit is preserved (we never overwrite)
//	and surfaced in the engine summary as a drift bucket.
//
// Idempotency:
//
//	ApplyCapabilities is safe to re-run. Rows already in the
//	"validated" state for a given stable_id are skipped; rows in
//	other states (active, degraded, fractured, etc.) are left
//	alone (the operator has been using them — don't reset).
package seed

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"unicode"
)

// SeedCapability is one entry in the Baseline Capability Library.
// Mirrors the ForgePayload shape (§3.1 of the spec) but adds
// seed-lifecycle metadata that the forge path doesn't carry
// (StableID for drift, ContentHash for idempotent seeding).
//
// Field semantics:
//
//	StableID  — seed-lifecycle handle. Distinct from the on-disk
//	            row id (cap.<name>) so a Name change in a future
//	            registry update still has provenance back to the
//	            original seed entry.
//	Name      — canonical capability name (no spaces; lowercase;
//	            dot-separated namespace).
//	Purpose   — natural-language description surfaced by the
//	            list_by_state read path and the auto-route plugin.
//	SourceCode — bash/python/jq source. Max 64KB / 500 lines
//	             (matches forge policy).
//	SourceLanguage — "bash" | "python" | "jq". Must be in the
//	                  allowedLanguages set; enforced at forge time.
//	RequestedDomain — "sandbox" | "restricted" | "trusted" |
//	                   "operator". Seeded primitives are all
//	                   "sandbox" (no host access; network none).
//	Tags      — discovery tags; first tag should be "capability"
//	            so the list-by-state path groups them.
//	DependsOn — capability names this entry depends on. Validated
//	            at seed time (each must exist or be co-seeded).
//	Metadata  — free-form JSON. Captured into capabilities.metadata.
//	            Conventionally carries {tier: 1, primitive: true,
//	            risk_class: "low"} for the Tier 1 set.
//	Notes     — author-facing rationale. NOT stored on the row —
//	            surfaced only in the seed report so operators
//	            know why a primitive exists.
type SeedCapability struct {
	StableID        string            `json:"stable_id"`
	Name            string            `json:"name"`
	Purpose         string            `json:"purpose"`
	SourceCode      string            `json:"source_code"`
	SourceLanguage  string            `json:"source_language"`
	RequestedDomain string            `json:"requested_domain"`
	Tags            []string          `json:"tags"`
	DependsOn       []string          `json:"depends_on,omitempty"`
	Metadata        map[string]string `json:"metadata,omitempty"`
	Notes           string            `json:"notes,omitempty"`
}

// ErrInvalidSeedCapability is returned when a SeedCapability
// entry is missing required fields or violates constraints.
// Caught at SavedID() / Validate() time so registry authors
// see the error at seed initialization rather than mid-seed.
var ErrInvalidSeedCapability = errors.New("seed: invalid SeedCapability")

// SavedID returns the canonical capability row id (`cap.<name>`)
// that the forge / read paths use. Mirrors the convention in
// the spec §1.2 (names are dot-namespaced, lowercase, no spaces).
//
// Validates Name and SourceCode at the same time so a half-filled
// struct doesn't silently produce a malformed row id.
func (s SeedCapability) SavedID() (string, error) {
	if err := s.validateForID(); err != nil {
		return "", fmt.Errorf("%w: %w", ErrInvalidSeedCapability, err)
	}
	return "cap." + s.Name, nil
}

// validateForID checks the name + source constraints that the
// row id depends on. Field-level validation that doesn't depend
// on the full set (e.g., tags / metadata) happens in Validate.
func (s SeedCapability) validateForID() error {
	if strings.TrimSpace(s.Name) == "" {
		return fmt.Errorf("capability name must not be empty")
	}
	if strings.ContainsRune(s.Name, ':') || strings.IndexFunc(s.Name, unicode.IsSpace) >= 0 {
		return fmt.Errorf("invalid capability name %q: must not contain colons or whitespace", s.Name)
	}
	for _, r := range s.Name {
		if !(r == '.' || r == '_' || r == '-' || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')) {
			return fmt.Errorf("invalid capability name %q: must be lowercase alphanumeric with . _ -", s.Name)
		}
	}
	if strings.TrimSpace(s.SourceCode) == "" {
		return fmt.Errorf("source_code must not be empty")
	}
	if len(s.SourceCode) > 65536 {
		return fmt.Errorf("source_code exceeds 64KB cap (%d bytes)", len(s.SourceCode))
	}
	return nil
}

// Validate runs all field-level checks. Use this from the
// registry loader so seed initialization surfaces problems
// before any SQL is written.
func (s SeedCapability) Validate() error {
	if err := s.validateForID(); err != nil {
		return err
	}
	switch s.SourceLanguage {
	case "bash", "python", "jq":
	default:
		return fmt.Errorf("invalid source_language %q (must be bash|python|jq)", s.SourceLanguage)
	}
	switch s.RequestedDomain {
	case "sandbox", "restricted", "trusted", "operator":
	default:
		return fmt.Errorf("invalid requested_domain %q (must be sandbox|restricted|trusted|operator)", s.RequestedDomain)
	}
	if strings.TrimSpace(s.Purpose) == "" {
		return fmt.Errorf("purpose must not be empty")
	}
	return nil
}

// ContentHash returns a stable SHA-256 fingerprint of the
// seeded source code. ApplyCapabilities uses this to detect
// when an existing row has drifted from the seed (operator's
// edit preserved; flagged for visibility rather than overwritten).
//
// Hash is over source_code only (not metadata/tags) — matches
// the forge's source_hash contract and lets drift detection
// focus on "did the operator edit the executable code?"
func (s SeedCapability) ContentHash() string {
	sum := sha256.Sum256([]byte(s.SourceCode))
	return fmt.Sprintf("%x", sum)
}

// =============================================================================
// Tier 1 — read-only primitive set
//
// Four starter capabilities that cover the most common
// capability-discovery + health queries an agent would make.
// All sandbox domain: read-only filesystem, no network, /tmp
// only, max output 16MB. The source uses `mpm call` to read
// the substrate (not direct sqlite access) so the access path
// matches what an operator's hand-rolled capability would use.
//
// All four scripts:
//
//   * Accept --json flag for machine-readable output; default
//     human-readable. (Convention, not enforced.)
//   * Shell out to `mpm call` rather than reading sqlite
//     directly — keeps the capability substrate-agnostic.
//   * Filter inputs via standard --key=value pairs, never
//     positional args beyond a leading target id.
//   * Exit 0 on success, 1 on missing input, 2 on substrate
//     error. (Convention, surfaced in metadata.)
//
// The four primitives:
// =============================================================================

// SeedCapabilities is the canonical registry of baseline
// capabilities. Order is preserved at seed time but does not
// affect runtime retrieval (GetLiveVersion returns the latest-
// state row by name).
//
// Edit this slice to add or deprecate baseline capabilities.
// Existing rows in operator DBs are never modified by
// `mpm capability seed` unless the row was deleted.
var SeedCapabilities = []SeedCapability{
	// ---------------------------------------------------------------
	// Tier 1 / Primitive 1: list_capabilities
	//
	// Lists all live (state='active', superseded_by_id IS NULL)
	// capabilities, optionally filtered by --state and --domain.
	// The agent's day-1 "what can I call?" query.
	//
	// Risk class: low — read-only, no args, no network.
	// Output shape: one capability per line, or JSON array
	// when --json is passed.
	// ---------------------------------------------------------------
	{
		StableID:       "cap-seed-list-capabilities",
		Name:           "list_capabilities",
		Purpose:        "List all live capabilities, optionally filtered by --state and --domain.",
		SourceLanguage: "bash",
		RequestedDomain: "sandbox",
		Tags:           []string{"capability", "discovery", "tier-1", "read-only"},
		SourceCode: `#!/bin/bash
# list_capabilities — Tier 1 read-only primitive
# Usage: list_capabilities [--state=<s>] [--domain=<d>] [--json]
set -euo pipefail

state=""
domain=""
json=0
for arg in "$@"; do
  case "$arg" in
    --state=*)  state="${arg#--state=}";;
    --domain=*) domain="${arg#--domain=}";;
    --json)     json=1;;
    -h|--help)
      echo "Usage: list_capabilities [--state=<s>] [--domain=<d>] [--json]" >&2
      exit 0;;
    *)
      echo "unknown arg: $arg" >&2; exit 1;;
  esac
done

# Build filter payload. Pass an empty payload to list everything.
payload='{}'
if [[ -n "$state" || -n "$domain" ]]; then
  payload=$(mpm call capability-list-bundled --payload "$(printf '{"state":"%s","domain":"%s"}' "${state:-}" "${domain:-}")" 2>/dev/null || echo '{}')
fi

if [[ $json -eq 1 ]]; then
  mpm call capability-list-bundled --payload "$payload"
else
  mpm call capability-list-bundled --payload "$payload" | jq -r '.[] | "\(.name)\t\(.state)\t\(.execution_domain)"' 2>/dev/null \
    || mpm call capability-list-bundled --payload "$payload"
fi
`,
		Metadata: map[string]string{
			"tier":       "1",
			"primitive":  "true",
			"risk_class": "low",
		},
		Notes: "Day-1 discovery query. Read-only, no args required.",
	},

	// ---------------------------------------------------------------
	// Tier 1 / Primitive 2: get_capability
	//
	// Fetches a single capability by id or name. Returns the full
	// row (state, domain, source_hash, lineage, metrics). Used by
	// the agent to inspect before invoking.
	//
	// Risk class: low — read-only, single target id.
	// ---------------------------------------------------------------
	{
		StableID:       "cap-seed-get-capability",
		Name:           "get_capability",
		Purpose:        "Fetch a single capability by id or name with full state, lineage, and metrics.",
		SourceLanguage: "bash",
		RequestedDomain: "sandbox",
		Tags:           []string{"capability", "discovery", "tier-1", "read-only"},
		SourceCode: `#!/bin/bash
# get_capability — Tier 1 read-only primitive
# Usage: get_capability <id|name> [--json]
set -euo pipefail

target=""
json=0
for arg in "$@"; do
  case "$arg" in
    --json) json=1;;
    --help|-h)
      echo "Usage: get_capability <id|name> [--json]" >&2
      exit 0;;
    -*)
      echo "unknown flag: $arg" >&2; exit 1;;
    *)
      if [[ -z "$target" ]]; then target="$arg"; else echo "extra arg: $arg" >&2; exit 1; fi;;
  esac
done

if [[ -z "$target" ]]; then
  echo "Usage: get_capability <id|name> [--json]" >&2
  exit 1
fi

# Prefer the live version by name; fall back to by id. Both
# paths go through mpm call so the capability sees the same
# surface as a hand-rolled invocation would.
if mpm call capability-get --payload "$(printf '{"name":"%s"}' "$target")" 2>/dev/null | jq -e '.id' >/dev/null 2>&1; then
  result=$(mpm call capability-get --payload "$(printf '{"name":"%s"}' "$target")")
else
  result=$(mpm call capability-get --payload "$(printf '{"id":"%s"}' "$target")")
fi

if [[ $json -eq 1 ]]; then
  echo "$result"
else
  echo "$result" | jq -r '
    "name:        \(.name)
state:       \(.state)
domain:      \(.execution_domain)
source_lang: \(.source_language)
source_hash: \(.source_hash)
successes:   \(.success_count)
failures:    \(.failure_count)
fractures:   \(.fracture_count)
last_invoked: \(.last_invoked_at // "never")
author:      \(.author_agent // "—")
created:     \(.created_at)
"
' 2>/dev/null || echo "$result"
fi
`,
		Metadata: map[string]string{
			"tier":       "1",
			"primitive":  "true",
			"risk_class": "low",
		},
		Notes: "Inspect-before-invoke. Read-only, single target.",
	},

	// ---------------------------------------------------------------
	// Tier 1 / Primitive 3: capability_lineage
	//
	// Walks capability_dependencies in both directions for a given
	// capability — what it depends on (upstream) and what depends
	// on it (downstream). Used before invocation to surface the
	// blast radius; used after a fracture to see what else is at
	// risk.
	//
	// Risk class: low — read-only.
	// ---------------------------------------------------------------
	{
		StableID:       "cap-seed-capability-lineage",
		Name:           "capability_lineage",
		Purpose:        "Show dependencies (upstream) and dependents (downstream) for a capability.",
		SourceLanguage: "bash",
		RequestedDomain: "sandbox",
		Tags:           []string{"capability", "discovery", "lineage", "tier-1", "read-only"},
		SourceCode: `#!/bin/bash
# capability_lineage — Tier 1 read-only primitive
# Usage: capability_lineage <id|name> [--direction=up|down|both] [--json]
set -euo pipefail

target=""
direction="both"
json=0
for arg in "$@"; do
  case "$arg" in
    --direction=*) direction="${arg#--direction=}";;
    --json) json=1;;
    --help|-h)
      echo "Usage: capability_lineage <id|name> [--direction=up|down|both] [--json]" >&2
      exit 0;;
    -*)
      echo "unknown flag: $arg" >&2; exit 1;;
    *)
      if [[ -z "$target" ]]; then target="$arg"; else echo "extra arg: $arg" >&2; exit 1; fi;;
  esac
done

if [[ -z "$target" ]]; then
  echo "Usage: capability_lineage <id|name> [--direction=up|down|both] [--json]" >&2
  exit 1
fi

if [[ ! "$direction" =~ ^(up|down|both)$ ]]; then
  echo "invalid --direction: $direction (must be up|down|both)" >&2; exit 1
fi

result=$(mpm call capability-lineage --payload "$(printf '{"target":"%s","direction":"%s"}' "$target" "$direction")")

if [[ $json -eq 1 ]]; then
  echo "$result"
else
  echo "$result" | jq -r '
    if .dependencies then "UPSTREAM (depends on):", (.dependencies[] | "  \(.name) [\(.state)]") else empty end,
    if .dependents then "DOWNSTREAM (depended on by):", (.dependents[] | "  \(.name) [\(.state)]") else empty end
  ' 2>/dev/null || echo "$result"
fi
`,
		Metadata: map[string]string{
			"tier":       "1",
			"primitive":  "true",
			"risk_class": "low",
		},
		Notes: "Pre-invocation blast-radius check; post-fracture impact analysis.",
	},

	// ---------------------------------------------------------------
	// Tier 1 / Primitive 4: capability_health
	//
	// Quick health summary: failure rate, fracture count, last
	// invocation, last failure stderr (truncated). The agent's
	// "is this thing safe to call?" pre-flight.
	//
	// Risk class: low — read-only.
	// ---------------------------------------------------------------
	{
		StableID:       "cap-seed-capability-health",
		Name:           "capability_health",
		Purpose:        "Health summary: failure rate, fracture count, last invocation, last failure trace.",
		SourceLanguage: "bash",
		RequestedDomain: "sandbox",
		Tags:           []string{"capability", "discovery", "health", "tier-1", "read-only"},
		SourceCode: `#!/bin/bash
# capability_health — Tier 1 read-only primitive
# Usage: capability_health <id|name> [--json]
set -euo pipefail

target=""
json=0
for arg in "$@"; do
  case "$arg" in
    --json) json=1;;
    --help|-h)
      echo "Usage: capability_health <id|name> [--json]" >&2
      exit 0;;
    -*)
      echo "unknown flag: $arg" >&2; exit 1;;
    *)
      if [[ -z "$target" ]]; then target="$arg"; else echo "extra arg: $arg" >&2; exit 1; fi;;
  esac
done

if [[ -z "$target" ]]; then
  echo "Usage: capability_health <id|name> [--json]" >&2
  exit 1
fi

result=$(mpm call capability-health --payload "$(printf '{"target":"%s"}' "$target")")

if [[ $json -eq 1 ]]; then
  echo "$result"
else
  echo "$result" | jq -r '
    "target:           \(.name)
state:            \(.state)
success_count:    \(.success_count)
failure_count:    \(.failure_count)
failure_rate:     \(.failure_rate)
fracture_count:   \(.fracture_count)
last_invoked_at:  \(.last_invoked_at // "never")
last_failure_at:  \(.last_failure_at // "never")
avg_latency_ms:   \(.avg_latency_ms)
last_failure_stderr: \(.last_failure_stderr // "—")
"
' 2>/dev/null || echo "$result"
fi
`,
		Metadata: map[string]string{
			"tier":       "1",
			"primitive":  "true",
			"risk_class": "low",
		},
		Notes: "Pre-invocation safety check.",
	},
}

// SeedCapabilitiesByName returns the seeded capability with the
// given name (canonical lookup for tests + drift detection).
// O(N) scan; the slice is small and called once per seed.
func SeedCapabilitiesByName(name string) (SeedCapability, bool) {
	for _, s := range SeedCapabilities {
		if s.Name == name {
			return s, true
		}
	}
	return SeedCapability{}, false
}

// SeedCapabilitiesByStableID is the same lookup keyed on the
// seed-lifecycle handle. Distinct from SavedID() so a Name
// change in the registry still maps back to the right row.
func SeedCapabilitiesByStableID(stableID string) (SeedCapability, bool) {
	for _, s := range SeedCapabilities {
		if s.StableID == stableID {
			return s, true
		}
	}
	return SeedCapability{}, false
}