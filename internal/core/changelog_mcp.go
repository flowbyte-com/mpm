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
	"time"
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

// ConfirmationSpec is one (artifact_id, artifact_type) pair the caller
// asserts is validated by a changelog commit. Used by
// LogChangelogEntryWithConfirmations; the artifact_type must be one of
// lesson / decision / theory (the canonical artifact types that
// participate in the confirmation flow per docs/epistemic-confirmation.md).
//
// Confirmation is explicit-only: no inference, no keyword matching. The
// caller asserts the relationship by naming the id; the substrate
// records an evidence row with type='reproduction' against the
// asserted artifact and runs RecomputeConfidence in the same
// transaction as the changelog memory write.
type ConfirmationSpec struct {
	ArtifactID   string
	ArtifactType string
}

// LogChangelogEntryWithConfirmations is the confirmation variant of
// LogChangelogEntry. It writes a changelog memory tied to a commit
// AND, for each ConfirmationSpec, fires one evidence row + recompute
// against the asserted artifact — all inside a single transaction so a
// failure anywhere rolls back the entire operation.
//
// Atomicity contract: either the changelog memory AND every
// confirmation evidence row land, or none of them do. This matches
// docs/epistemic-confirmation.md §"What confirmation produces" +
// §"Failure handling".
//
// The evidence row uses the registry defaults:
//   - type='reproduction' (default strength 0.85)
//   - source_group='git' (the confirmation is commit-anchored)
//   - independence_factor=1.0
//   - created_by='log_to_changelog:<commit_hash>'
//   - notes='confirmed by changelog entry <commit_hash>'
//
// These are deliberately not caller-tunable in this design. The
// confirmation mechanism has one job — record an explicit assertion
// that this commit validates the named artifact — and exposes no
// tunable knobs (per docs/epistemic-confirmation.md §"Configuration
// knobs"). A future arc that needs finer-grained trigger taxonomy or
// caller-strength override is a separate design and would land in its
// own docs.
//
// Cascade cross-fire check: positive-strength evidence on a
// confidence=0.7-default lesson can never drop it below the cascade
// threshold (0.3). RecomputeConfidence's hard-confidence invalidation
// hook fires only on threshold-crossing transitions, so this path
// cannot trigger any downstream re-evaluation cascade (per
// evidence_store.go:328-340 + docs/archive/epistemic-cascades.md).
func (dm *DatabaseManager) LogChangelogEntryWithConfirmations(
	fact, commitHash string,
	extraTags []string,
	confirmations []ConfirmationSpec,
) (string, error) {
	// Mirror LogChangelogEntry validation so the contract surface is
	// identical for the no-confirmations case.
	if dm == nil || dm.db == nil {
		return "", fmt.Errorf("LogChangelogEntryWithConfirmations: database not initialized")
	}
	if strings.TrimSpace(fact) == "" {
		return "", fmt.Errorf("LogChangelogEntryWithConfirmations: fact is required")
	}
	if strings.TrimSpace(commitHash) == "" {
		return "", fmt.Errorf("LogChangelogEntryWithConfirmations: commit_hash is required (strict retrospective contract: every changelog memory must reference an existing commit)")
	}
	if !fullSHARe.MatchString(commitHash) {
		return "", fmt.Errorf("LogChangelogEntryWithConfirmations: commit_hash %q is not a full 40-character SHA-1; run `git rev-parse HEAD` to get the canonical form", commitHash)
	}

	// Validate every confirmation BEFORE any write so a partial-state
	// batch never lands. Per docs/epistemic-confirmation.md §"Failure
	// handling", invalid input rolls back the entire transaction.
	for i, conf := range confirmations {
		switch conf.ArtifactType {
		case "lesson", "decision", "theory":
			// ok
		default:
			return "", fmt.Errorf("LogChangelogEntryWithConfirmations: confirmation[%d] artifact_type %q invalid (must be lesson, decision, or theory)", i, conf.ArtifactType)
		}
		if strings.TrimSpace(conf.ArtifactID) == "" {
			return "", fmt.Errorf("LogChangelogEntryWithConfirmations: confirmation[%d] artifact_id is required", i)
		}
	}

	// Build tags + body identical to LogChangelogEntry so the
	// synthesis-engine join key is unchanged. The synthesized-engine
	// (or any downstream consumer reading commit:<hash>) sees the
	// same tag set whether the call carried confirmations or not.
	commitTag := CommitTagPrefix + strings.ToLower(commitHash)
	tags := []string{ChangelogTag, commitTag}
	seen := map[string]bool{ChangelogTag: true, commitTag: true}
	for _, t := range extraTags {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		if strings.EqualFold(t, ChangelogTag) {
			continue
		}
		if seen[t] {
			continue
		}
		seen[t] = true
		tags = append(tags, t)
	}
	body := fmt.Sprintf("[commit: %s]\n\n%s", commitHash, fact)

	var memoryID string
	err := dm.WithTx(func(node DBNode) error {
		// 1. Write the changelog memory inside the tx so the scanner
		// boundary + INSERT stay atomic with the confirmation writes.
		// SaveMemoryNode is the tx-aware primitive that runs the same
		// security scanners as SaveMemoryWithContext (validation +
		// isSensitiveContent + isPoisoned).
		var err error
		memoryID, err = dm.SaveMemoryNode(node, "changelog", body, "", tags, nil, nil, false, 1.0, "", "0.5", "0.5", "")
		if err != nil {
			return fmt.Errorf("write changelog memory: %w", err)
		}

		// 2. Fire one evidence row + recompute per confirmation. The
		// in-tx helper (addEvidenceInTx) reuses the exact INSERT +
		// RecomputeConfidence path AddEvidence uses, so the two
		// surfaces never drift.
		for i, conf := range confirmations {
			if err := writeConfirmationInTx(node, conf, commitHash); err != nil {
				return fmt.Errorf("confirmation[%d] %s/%s: %w", i, conf.ArtifactType, conf.ArtifactID, err)
			}
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return memoryID, nil
}

// writeConfirmationInTx is the tx-aware helper that inserts one
// confirmation evidence row + runs RecomputeConfidence, all within the
// caller's existing transaction. Extracted so the body of
// LogChangelogEntryWithConfirmations stays readable as the canonical
// "atomic changelog + N confirmations" recipe.
//
// The artifact-existence check is performed here (rather than inside
// addEvidenceInTx) so we surface the "named artifact does not exist"
// error with the specific (artifact_type, artifact_id) pair in the
// message — this is what docs/epistemic-confirmation.md §"Failure
// handling" promises, and it's why the validation happens BEFORE the
// INSERT (no orphan evidence row, no orphan history row, no orphan
// cascade intent).
func writeConfirmationInTx(node DBNode, conf ConfirmationSpec, commitHash string) error {
	exists, err := artifactExists(node, conf.ArtifactID, conf.ArtifactType)
	if err != nil {
		return fmt.Errorf("artifact existence check: %w", err)
	}
	if !exists {
		return fmt.Errorf("artifact %s/%s does not exist", conf.ArtifactType, conf.ArtifactID)
	}
	in := EvidenceInput{
		ArtifactID:         conf.ArtifactID,
		ArtifactType:       conf.ArtifactType,
		Type:               "reproduction",
		SourceGroup:        "git",
		Strength:           0.85, // DefaultStrength("reproduction") at evidence.go:23
		IndependenceFactor: 1.0,
		CreatedBy:          fmt.Sprintf("log_to_changelog:%s", commitHash),
		CreatedAt:          time.Now(),
		Notes:              fmt.Sprintf("confirmed by changelog entry %s", commitHash),
	}
	return addEvidenceInTx(node, GenerateID(), in, nil)
}
