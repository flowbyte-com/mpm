package internal

// changelog_mcp.go provides the persist-and-locate primitives for
// the mpm_log_to_changelog MCP tool. The tool itself lives in
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
		ActiveContext{Model: "mpm_log_to_changelog"}, // provenance: this memory came from the changelog tool
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

// ContradictionSpec is the symmetric negative-direction counterpart to
// ConfirmationSpec. Used by LogChangelogEntryWithAssertions; the
// artifact_type must be one of lesson / decision / theory.
//
// Like ConfirmationSpec, contradiction is explicit-only — no keyword
// matching, no semantic inference. The caller asserts the artifact is
// invalidated (or contradicted) by this commit; the substrate records
// an evidence row with type='challenge' against the asserted artifact.
//
// CRITICAL ASYMMETRY vs ConfirmationSpec: contradiction CAN trigger
// the invalidation cascade. A confirmation at default strength (+0.85)
// raises confidence from 0.7 → ~0.85, which can never cross the cascade
// floor (0.3). A contradiction at default strength (-0.6) lowers
// confidence toward the floor; a strong contradiction (multiple
// challenge rows, or an override-strength challenge) can cross the
// floor and legitimately enqueue cascade intents for downstream
// dependents. Per docs/archive/epistemic-cascades.md, threshold
// crossing is one of three legitimate cascade triggers. The
// contradiction path does NOT suppress this — doing so would silently
// hide the user's intent when they assert "this artifact is wrong."
//
// Per docs/epistemic-confirmation.md §"Cascade cross-fire
// asymmetry", this is the deliberate, documented divergence between
// the two directions.
type ContradictionSpec struct {
	ArtifactID   string
	ArtifactType string
}

// LogChangelogEntryWithAssertions is the canonical method that fires
// the changelog memory + any number of confirmations + any number of
// contradictions, atomically inside a single transaction.
//
// This is the single source of truth for "explicit assertion" wired
// through mpm_log_to_changelog. Per docs/epistemic-confirmation.md, the
// mechanism has two directions:
//
//   - Confirmation (positive evidence, type='reproduction', default
//     strength +0.85). The cascade invalidation hook is unreachable
//     from this path: confirmation cannot drop confidence below the
//     0.3 floor from any 0.7+ baseline.
//   - Contradiction (negative evidence, type='challenge', default
//     strength -0.6). The cascade invalidation hook IS reachable:
//     a strong contradiction (multiple challenge rows, or an override-
//     strength challenge) can cross the 0.3 floor and legitimately
//     enqueue cascade intents for downstream dependents. This is the
//     deliberate, documented divergence between the two directions.
//
// Atomicity contract: either the changelog memory AND every assertion
// evidence row (confirmations + contradictions) land, or none do.
// Per docs/epistemic-confirmation.md §"Failure handling".
//
// Both directions reuse the same INSERT + RecomputeConfidence path
// (via addEvidenceInTx) so the two surfaces never drift; the only
// delta is the evidence-type/strength constants in
// writeConfirmationInTx vs writeContradictionInTx.
func (dm *DatabaseManager) LogChangelogEntryWithAssertions(
	fact, commitHash string,
	extraTags []string,
	confirmations []ConfirmationSpec,
	contradictions []ContradictionSpec,
) (string, error) {
	// Mirror LogChangelogEntry validation so the contract surface is
	// identical for the no-assertions case.
	if dm == nil || dm.db == nil {
		return "", fmt.Errorf("LogChangelogEntryWithAssertions: database not initialized")
	}
	if strings.TrimSpace(fact) == "" {
		return "", fmt.Errorf("LogChangelogEntryWithAssertions: fact is required")
	}
	if strings.TrimSpace(commitHash) == "" {
		return "", fmt.Errorf("LogChangelogEntryWithAssertions: commit_hash is required (strict retrospective contract: every changelog memory must reference an existing commit)")
	}
	if !fullSHARe.MatchString(commitHash) {
		return "", fmt.Errorf("LogChangelogEntryWithAssertions: commit_hash %q is not a full 40-character SHA-1; run `git rev-parse HEAD` to get the canonical form", commitHash)
	}

	// Validate every assertion BEFORE any write so a partial-state
	// batch never lands. Per docs/epistemic-confirmation.md §"Failure
	// handling", invalid input rolls back the entire transaction.
	for i, conf := range confirmations {
		switch conf.ArtifactType {
		case "lesson", "decision", "theory":
			// ok
		default:
			return "", fmt.Errorf("LogChangelogEntryWithAssertions: confirmation[%d] artifact_type %q invalid (must be lesson, decision, or theory)", i, conf.ArtifactType)
		}
		if strings.TrimSpace(conf.ArtifactID) == "" {
			return "", fmt.Errorf("LogChangelogEntryWithAssertions: confirmation[%d] artifact_id is required", i)
		}
	}
	for i, contra := range contradictions {
		switch contra.ArtifactType {
		case "lesson", "decision", "theory":
			// ok
		default:
			return "", fmt.Errorf("LogChangelogEntryWithAssertions: contradiction[%d] artifact_type %q invalid (must be lesson, decision, or theory)", i, contra.ArtifactType)
		}
		if strings.TrimSpace(contra.ArtifactID) == "" {
			return "", fmt.Errorf("LogChangelogEntryWithAssertions: contradiction[%d] artifact_id is required", i)
		}
	}

	// Build tags + body identical to LogChangelogEntry so the
	// synthesis-engine join key is unchanged.
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
		// boundary + INSERT stay atomic with the assertion writes.
		// SaveMemoryNode is the tx-aware primitive that runs the same
		// security scanners as SaveMemoryWithContext (validation +
		// isSensitiveContent + isPoisoned).
		var err error
		memoryID, err = dm.SaveMemoryNode(node, "changelog", body, "", tags, nil, nil, false, 1.0, "", "0.5", "0.5", "")
		if err != nil {
			return fmt.Errorf("write changelog memory: %w", err)
		}

		// 2. Fire one evidence row + recompute per confirmation.
		// Positive direction; cascade hook unreachable.
		for i, conf := range confirmations {
			if err := writeConfirmationInTx(node, conf, commitHash); err != nil {
				return fmt.Errorf("confirmation[%d] %s/%s: %w", i, conf.ArtifactType, conf.ArtifactID, err)
			}
		}
		// 3. Fire one evidence row + recompute per contradiction.
		// Negative direction; cascade hook reachable on threshold
		// crossing — RecomputeConfidence's existing hard-confidence
		// invalidation logic handles enqueue (evidence_store.go:328-340).
		// The cascade path is the same one any other negative-evidence
		// write would trigger; we deliberately do not gate it.
		for i, contra := range contradictions {
			if err := writeContradictionInTx(node, contra, commitHash); err != nil {
				return fmt.Errorf("contradiction[%d] %s/%s: %w", i, contra.ArtifactType, contra.ArtifactID, err)
			}
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return memoryID, nil
}

// LogChangelogEntryWithConfirmations is the back-compat thin wrapper
// over LogChangelogEntryWithAssertions. It carries no contradictions.
// New callers should use LogChangelogEntryWithAssertions directly.
func (dm *DatabaseManager) LogChangelogEntryWithConfirmations(
	fact, commitHash string,
	extraTags []string,
	confirmations []ConfirmationSpec,
) (string, error) {
	return dm.LogChangelogEntryWithAssertions(fact, commitHash, extraTags, confirmations, nil)
}

// writeContradictionInTx is the contradiction counterpart of
// writeConfirmationInTx. Same shape, opposite evidence direction.
//
// Evidence row uses the registry defaults for `challenge`:
//   - type='challenge' (default strength -0.6)
//   - source_group='git' (the contradiction is commit-anchored)
//   - independence_factor=1.0
//   - created_by='mpm_log_to_changelog:<commit_hash>'
//   - notes='contradicted by changelog entry <commit_hash>'
//
// ASYMMETRY vs writeConfirmationInTx: contradiction CAN trigger
// the cascade invalidation hook via RecomputeConfidence's hard-
// confidence crossing detector (evidence_store.go:328-340). This is
// the intended behavior — a strong contradiction legitimately
// invalidates the artifact and its downstream dependents. Per
// docs/epistemic-confirmation.md §"Cascade cross-fire asymmetry",
// we do not gate this path.
func writeContradictionInTx(node DBNode, spec ContradictionSpec, commitHash string) error {
	exists, err := artifactExists(node, spec.ArtifactID, spec.ArtifactType)
	if err != nil {
		return fmt.Errorf("artifact existence check: %w", err)
	}
	if !exists {
		return fmt.Errorf("artifact %s/%s does not exist", spec.ArtifactType, spec.ArtifactID)
	}
	in := EvidenceInput{
		ArtifactID:         spec.ArtifactID,
		ArtifactType:       spec.ArtifactType,
		Type:               "challenge",
		SourceGroup:        "git",
		Strength:           -0.6, // DefaultStrength("challenge") at evidence.go:24
		IndependenceFactor: 1.0,
		CreatedBy:          fmt.Sprintf("mpm_log_to_changelog:%s", commitHash),
		CreatedAt:          time.Now(),
		Notes:              fmt.Sprintf("contradicted by changelog entry %s", commitHash),
	}
	return addEvidenceInTx(node, GenerateID(), in, nil)
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
		CreatedBy:          fmt.Sprintf("mpm_log_to_changelog:%s", commitHash),
		CreatedAt:          time.Now(),
		Notes:              fmt.Sprintf("confirmed by changelog entry %s", commitHash),
	}
	return addEvidenceInTx(node, GenerateID(), in, nil)
}
