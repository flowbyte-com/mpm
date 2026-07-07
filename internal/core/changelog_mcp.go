package internal

// changelog_mcp.go provides the persist-and-locate primitives for
// the log_to_changelog MCP tool. The tool itself lives in
// cmd/mpm-mcp/tools.go; this file holds the data layer so the
// function is testable against a per-test in-memory DM without
// standing up an MCP server.
//
// Design contract (strictly retrospective):
//
//   - Every call REQUIRES a commit_hash. The tool refuses to write a
//     changelog memory without a corresponding git commit. This keeps
//     the schema join (commit_hash, mpm_memory_id) one-to-one: a
//     memory exists iff the commit exists.
//
//   - The "changelog" tag is auto-injected if absent. The synthesis
//     engine locates changelog entries by tag (efficiency: one SQL
//     query against the tag index). Agents do not need to remember
//     to add it; the contract is enforced in code so the synthesis
//     engine never silently misses an entry because the agent
//     typoed a tag.
//
//   - The commit_hash is validated as a 40-character hex string.
//     Short hashes and refs are rejected to keep the join key
//     unambiguous — `git rev-parse HEAD` returns full 40-char SHAs
//     by default, so accepting anything else would mean agents and
//     the synthesis engine disagree about which commit a memory
//     corresponds to.
//
//   - Memories are written at weight 1.0 (permanent, high-importance)
//     with TTL "0" (no expiry). Changelog memories are first-class
//     durable records; the agent must not silently forget them.
//
//   - The commit_hash is stored both as a tag (for index lookup) and
//     in the body of the fact (so the synthesis engine can pattern-
//     match if it ever needs to). The tag is the authoritative join
//     key.

import (
	"fmt"
	"regexp"
	"strings"
)

// ChangelogTag is the canonical tag used to identify changelog
// memories in the MPM memory table. The synthesis engine queries by
// this tag — see internal/changelog_synthesis.go (future) or
// cmd/mpm/changelog_synthesis.go (wherever it lands). The constant
// lives here so any code that writes or queries changelog memories
// agrees on the spelling.
const ChangelogTag = "changelog"

// CommitTagPrefix is prepended to a commit hash to form the per-
// commit tag. Example: commit "abc123..." becomes tag "commit:abc123...".
// Including the prefix avoids collisions with other tag namespaces
// (an agent might naturally write the bare hash as a tag for
// unrelated reasons; "commit:" disambiguates).
const CommitTagPrefix = "commit:"

// fullSHARe matches a 40-character lowercase or uppercase hex
// string, i.e. a full git SHA-1 hash. Anchored on both ends so
// substrings do not match.
var fullSHARe = regexp.MustCompile(`^[0-9a-fA-F]{40}$`)

// LogChangelogEntry writes a changelog memory to MPM tied to a
// specific commit. The fact, commit_hash, and (optional) extra
// tags are persisted as a single memory row. The function returns
// the memory ID on success.
//
// Errors are explicit and pre-conditions are enforced strictly:
//   - empty commit_hash -> error (no orphan entries)
//   - malformed commit_hash -> error (no ambiguous joins)
//   - empty fact -> error (no empty memories)
//
// On success the memory is tagged with #changelog (always) and
// #commit:<full-hash> (always) plus any caller-supplied extra tags.
// The #changelog tag is the synthesis engine's lookup key.
func (dm *DatabaseManager) LogChangelogEntry(fact, commitHash string, extraTags []string) (string, error) {
	if dm == nil || dm.db == nil {
		return "", fmt.Errorf("LogChangelogEntry: database not initialized")
	}
	if strings.TrimSpace(fact) == "" {
		return "", fmt.Errorf("LogChangelogEntry: fact is required")
	}
	if strings.TrimSpace(commitHash) == "" {
		return "", fmt.Errorf("LogChangelogEntry: commit_hash is required (strict retrospective contract: every changelog memory must reference an existing commit)")
	}
	if !fullSHARe.MatchString(commitHash) {
		return "", fmt.Errorf("LogChangelogEntry: commit_hash %q is not a full 40-character SHA-1; run `git rev-parse HEAD` to get the canonical form", commitHash)
	}

	// Build the tag set. Order does not matter; the memory store
	// normalises tags on write. We de-dupe against the caller's
	// tags in case the caller already added #changelog.
	tags := []string{ChangelogTag, CommitTagPrefix + strings.ToLower(commitHash)}
	seen := map[string]bool{ChangelogTag: true, CommitTagPrefix + strings.ToLower(commitHash): true}
	for _, t := range extraTags {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		// Normalise "changelog" (any case) so we do not duplicate.
		if strings.EqualFold(t, ChangelogTag) {
			continue
		}
		if seen[t] {
			continue
		}
		seen[t] = true
		tags = append(tags, t)
	}

	// We prepend a structured commit-reference line to the fact so
	// human readers of the memory table can see the join at a
	// glance. The synthesis engine does not need to parse this — it
	// uses the tag index — but having it in the body is cheap and
	// makes debugging easier.
	body := fmt.Sprintf("[commit: %s]\n\n%s", commitHash, fact)

	out, _, err := dm.SaveMemoryWithContext(
		body,
		"changelog", // dedicated collection so synthesis queries can scope cheaply
		tags,
		1.0,  // weight 1.0 — changelog memories are permanent, high-importance
		"0",  // TTL 0 — no expiry
		ActiveContext{Model: "log_to_changelog"}, // provenance: this memory came from the changelog tool
	)
	if err != nil {
		return "", fmt.Errorf("LogChangelogEntry: persist: %w", err)
	}
	id, _ := out["id"].(string)
	if id == "" {
		return "", fmt.Errorf("LogChangelogEntry: persist succeeded but id missing from result: %+v", out)
	}
	return id, nil
}
