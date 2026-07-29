// Package seed (skills.go) — the Baseline Skill Library.
//
// Mirrors directives.go: a curated set of skills that close the
// substrate's most-used procedural loops. Idempotent — `mpm ops init
// skills` detects existing rows by saved id and skips them; local
// edits are preserved and flagged as drift in the engine summary.
//
// MPM doesn't ship skills automatically. Operators run:
//
//	mpm ops init skills
//
// to seed them. The seed file is intentionally small — a starting
// point, not a complete library. Each operator's actual skill
// catalogue should grow from their own usage.

package seed

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"unicode"
)

// SeedSkill is one entry in the Baseline Skill Library. Each entry
// maps a stable ID to the skill content the operator will see.
//
// The deterministic storage id (SavedID) is derived from Name and
// Version — that's the contract ReadSkill / ListSkills read against.
// The StableID is a seed-lifecycle handle (distinct from the row id)
// that survives renames: if Name changes in a future registry update
// the row still has provenance back to the original seed entry, and
// the old row stays readable by its deterministic id until the
// operator reconciles.
type SeedSkill struct {
	StableID  string   // seed-lifecycle handle (audit / drift)
	Name      string   // skill frontmatter name (required)
	Version   string   // skill frontmatter version (semver, required)
	Domain    string   // optional taxonomy bucket
	Tags      []string // optional, must include "skill" if used
	WhenToUse string   // surfaced by list_skills / proactive_recall_hint
	Content   string   // full skill body (frontmatter + markdown)
}

// ErrInvalidSeedSkill is returned when a SeedSkill entry is missing
// required fields. Caught at SavedID() time so registry authors see
// the error at seed initialization rather than mid-ApplySkills.
var ErrInvalidSeedSkill = errors.New("seed: invalid SeedSkill")

// skillIDSuffix is the canonical separator used to build skill
// row ids: `skill:<name>-v<version>`. Mirrors the internal/core
// contract; duplicated here because the seed package can't import
// internal/core (separate Go modules — same directory hierarchy
// but different module roots).
const skillIDSuffix = "-v"

// SavedID returns the deterministic skill row id
// (`skill:<name>-v<semver>`) that the upstream SaveSkill writes
// and ReadSkill resolves against. It is distinct from StableID —
// StableID is for seed lifecycle, SavedID is the on-disk primary key.
//
// Validates Name and Version are non-empty (and name/version are
// free of separators that would break id parsing) so a half-filled
// struct doesn't silently produce a malformed row id.
func (s SeedSkill) SavedID() (string, error) {
	if err := validateNameVersion(s.Name, s.Version); err != nil {
		return "", fmt.Errorf("%w: %w", ErrInvalidSeedSkill, err)
	}
	return fmt.Sprintf("skill:%s%s%s", s.Name, skillIDSuffix, s.Version), nil
}

// validateNameVersion mirrors the constraint used by the upstream
// id-builder. Kept local so this package stays decoupled.
func validateNameVersion(name, version string) error {
	if name == "" {
		return fmt.Errorf("skill name must not be empty")
	}
	if version == "" {
		return fmt.Errorf("skill version must not be empty")
	}
	if strings.ContainsRune(name, ':') || strings.IndexFunc(name, unicode.IsSpace) >= 0 {
		return fmt.Errorf("invalid skill name %q: must not contain colons or whitespace", name)
	}
	if strings.ContainsRune(version, ':') || strings.IndexFunc(version, unicode.IsSpace) >= 0 {
		return fmt.Errorf("invalid skill version %q: must not contain colons or whitespace", version)
	}
	return nil
}

// ContentHash returns a stable SHA-256 fingerprint of the skill's
// content. Used by ApplySkills to detect when an existing row has
// drifted from the seed (operator's edit preserved; flagged for
// visibility rather than overwritten).
func (s SeedSkill) ContentHash() string {
	sum := sha256.Sum256([]byte(s.Content))
	return fmt.Sprintf("%x", sum)
}

// SeedSkills is the canonical registry of baseline skills. Order
// is preserved at seed time but does not affect runtime retrieval
// (read_skill returns the latest-version row by name).
//
// Edit this slice to add or deprecate baseline skills. Existing
// rows in operator DBs are never modified by `mpm ops init skills`
// unless the row was deleted by the operator.
var SeedSkills = []SeedSkill{
	{
		StableID:  "mpm-seed-skill-mpm-status",
		Name:      "mpm-status",
		Version:   "1.0.0",
		WhenToUse: "mpm status, mpm health, basic CLI navigation",
		Tags:      []string{"skill", "mpm", "bootstrap"},
		Content: `---
name: mpm-status
version: 1.0.0
when_to_use: mpm status, mpm health, basic CLI navigation
domain: mpm
---
# mpm-status

Use this skill when you need to check the substrate's state.

1. Call ` + "`mpm_stats`" + ` for memory counts and lifecycle posture.
2. Call ` + "`mpm_health_check`" + ` for SQLite integrity + busy-retry counters.
3. If errors are surfaced, escalate to the operator with full context.

Constraints:
- Read-only diagnostics first; never run mutating commands without explicit user consent.
- If the busy-retry counter is climbing, check the WAL writer mode before deeper investigation.
`,
	},
}
