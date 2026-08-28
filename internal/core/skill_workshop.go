// skill_workshop.go — MPM Skill Workshop.
//
// The workshop is a structured skill-formation and validation workflow
// that lives above the existing skill persistence and validation
// architecture. See docs/archive/2026-08-28-mpm-skill-workshop-design.md
// for the full contract.
//
// This file holds:
//   - The single-flight cache (workshopCache, claimOrWait, publishResult)
//   - The pipeline stages (input validation, decision model,
//     when_to_use check, duplicate detection, identity check,
//     change_type mapping, validate, publish)
//   - The two new DatabaseManager methods (ValidateSkill,
//     SaveSkillAndDeprecatePrior) are defined in skill_db.go where
//     their non-mutating / transactional primitives live alongside
//     SaveSkill.

package internal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/mod/semver"
)

const (
	workshopCacheTTL    = 24 * time.Hour
	workshopWaitTimeout = 60 * time.Second
)

var errWorkshopWaitTimeout = errors.New("workshop: cache wait timed out")

// workshopCacheEntry holds the in-flight result of a workshop execution.
// `done` is closed by the first writer when publishResult is called,
// unblocking any concurrent waiters observing the same key.
type workshopCacheEntry struct {
	done     chan struct{}
	response json.RawMessage
	err      error
}

// workshopCache is the in-memory single-flight dedup map.
// Package-level: cleared on daemon restart. The durable identity
// check (see identityCheckStage) provides the cross-restart guarantee;
// this cache is a deduplication convenience, not a correctness
// mechanism.
var workshopCache sync.Map // map[string]*workshopCacheEntry

// claimOrWait atomically claims the workshop_key slot. If the slot is
// unclaimed, the caller is the writer and receives a fresh entry; if
// the slot is already claimed, the caller blocks on entry.done (or
// ctx.Done / workshopWaitTimeout, whichever fires first) and returns
// the same entry.
//
// Returns (entry, isWriter, err). isWriter=true means the caller MUST
// call publishResult on the entry when finished; isWriter=false means
// the caller should consume entry.response / entry.err instead.
func claimOrWait(ctx context.Context, key string) (*workshopCacheEntry, bool, error) {
	newEntry := &workshopCacheEntry{done: make(chan struct{})}
	actual, loaded := workshopCache.LoadOrStore(key, newEntry)
	entry := actual.(*workshopCacheEntry)

	if loaded {
		select {
		case <-entry.done:
			return entry, false, nil
		case <-ctx.Done():
			return nil, false, ctx.Err()
		case <-time.After(workshopWaitTimeout):
			return nil, false, errWorkshopWaitTimeout
		}
	}

	// Schedule cleanup after TTL. CompareAndDelete ensures we only
	// remove the entry we put in (not a newer claim from a later call
	// that happened to share the slot after expiry).
	time.AfterFunc(workshopCacheTTL, func() {
		workshopCache.CompareAndDelete(key, entry)
	})
	return newEntry, true, nil
}

// publishResult stores the response on the entry and unblocks waiters.
// MUST be called by the writer (the caller for which claimOrWait
// returned isWriter=true).
func publishResult(entry *workshopCacheEntry, response json.RawMessage, err error) {
	entry.response = response
	entry.err = err
	close(entry.done)
}

// WorkshopRequest is the parsed workshop invocation payload. The MCP
// handler unmarshals the raw payload map into this struct before
// pipeline stages run.
type WorkshopRequest struct {
	Mode            string           `json:"mode"`
	Intent          string           `json:"intent"`
	ChangeType      string           `json:"change_type"`
	DecisionModel   DecisionModel    `json:"decision_model"`
	Proposal        SkillProposal    `json:"proposal"`
	TaskContext     string           `json:"task_context"`
	WorkflowDesc    string           `json:"workflow_description"`
	FailureRecovery string           `json:"failure_recovery"`
	RecentActions   []string         `json:"recent_actions"`
	Evidence        WorkshopEvidence `json:"evidence"`
	WorkshopKey     string           `json:"workshop_key"`
}

type DecisionModel struct {
	Reusability    int    `json:"reusability"`
	NonObviousness int    `json:"non_obviousness"`
	Stability      int    `json:"stability"`
	Leverage       int    `json:"leverage"`
	Boundary       string `json:"boundary"`
}

type SkillProposal struct {
	Name        string      `json:"name"`
	Version     string      `json:"version"`
	Domain      string      `json:"domain"`
	Description string      `json:"description"`
	WhenToUse   string      `json:"when_to_use"`
	Steps       []SkillStep `json:"steps"`
	Constraints []string    `json:"constraints"`
}

type WorkshopEvidence struct {
	MemoryIDs    []string `json:"memory_ids"`
	LessonIDs    []string `json:"lesson_ids"`
	ReferenceIDs []string `json:"reference_ids"`
}

// size caps from spec §4.1 and §5.1.
const (
	maxTotalRequest     = 256 * 1024
	maxTaskContext      = 50 * 1024
	maxWorkflowDesc     = 50 * 1024
	maxFailureRecovery  = 20 * 1024
	maxRecentActions    = 20
	maxEvidenceMemory   = 10
	maxEvidenceLesson   = 5
	maxEvidenceRef      = 5
)

// DecisionOutcome is the disposition from the decision-model stage.
type DecisionOutcome string

const (
	DecisionPublished DecisionOutcome = "published"
	DecisionCandidate DecisionOutcome = "candidate"
	DecisionRejected  DecisionOutcome = "rejected"
)

// evaluateDecisionModel applies the 4-axis scoring + boundary check
// (spec §6). For refine mode, it bumps reusability/non_obviousness
// when failure_recovery is supplied (spec §9 step 3). Returns the
// disposition + a populated DecisionModel with high/medium/low labels
// + the total (used in the response payload).
func evaluateDecisionModel(req *WorkshopRequest) (DecisionOutcome, DecisionModel, string) {
	dm := req.DecisionModel

	if req.Mode == "refine" && req.FailureRecovery != "" {
		// Bumps per spec §9 step 3: reusability +=1 if recurring
		// failure mode (proxy: any non-empty failure_recovery),
		// non_obviousness +=1 if non-obvious gap. We treat the
		// presence of failure_recovery as both signals — the
		// operator wouldn't have supplied it otherwise.
		if dm.Reusability < 2 {
			dm.Reusability++
		}
		if dm.NonObviousness < 2 {
			dm.NonObviousness++
		}
	}

	total := dm.Reusability + dm.NonObviousness + dm.Stability + dm.Leverage

	// Boundary gate (spec §6).
	if dm.Boundary != "procedure" {
		return DecisionRejected, dm, fmt.Sprintf("non-procedure boundary: %s", dm.Boundary)
	}
	// Total threshold.
	if total <= 3 {
		return DecisionRejected, dm, "decision_total_below_threshold"
	}
	if total >= 4 && total <= 5 {
		return DecisionCandidate, dm, "decision_total_medium"
	}
	// total >= 6 and boundary = procedure → publish candidate.
	// Downgrades from later stages (when_to_use warnings, duplicate
	// detection, validation) will convert this to candidate before
	// the publication stage.
	return DecisionPublished, dm, "decision_total_high"
}

// validateInput enforces size caps and shape checks. Returns
// (warnings, errors, err). On hard limit violation (size cap
// exceeded), err is non-nil; soft warnings are returned separately.
func validateInput(req *WorkshopRequest) (warnings []string, errors []string, err error) {
	if req == nil {
		return nil, nil, fmt.Errorf("validateInput: nil request")
	}
	if req.Mode != "form" && req.Mode != "refine" {
		return nil, nil, fmt.Errorf("validateInput: mode must be 'form' or 'refine', got %q", req.Mode)
	}
	if req.Mode == "refine" && req.ChangeType == "" {
		return nil, nil, fmt.Errorf("validateInput: refine mode requires change_type")
	}
	if len(req.TaskContext) > maxTaskContext {
		return nil, nil, fmt.Errorf("validateInput: task_context exceeds %d bytes", maxTaskContext)
	}
	if len(req.WorkflowDesc) > maxWorkflowDesc {
		return nil, nil, fmt.Errorf("validateInput: workflow_description exceeds %d bytes", maxWorkflowDesc)
	}
	if len(req.FailureRecovery) > maxFailureRecovery {
		return nil, nil, fmt.Errorf("validateInput: failure_recovery exceeds %d bytes", maxFailureRecovery)
	}
	if len(req.RecentActions) > maxRecentActions {
		return nil, nil, fmt.Errorf("validateInput: recent_actions has %d entries, max %d", len(req.RecentActions), maxRecentActions)
	}
	if len(req.Evidence.MemoryIDs) > maxEvidenceMemory {
		return nil, nil, fmt.Errorf("validateInput: evidence.memory_ids has %d entries, max %d", len(req.Evidence.MemoryIDs), maxEvidenceMemory)
	}
	if len(req.Evidence.LessonIDs) > maxEvidenceLesson {
		return nil, nil, fmt.Errorf("validateInput: evidence.lesson_ids has %d entries, max %d", len(req.Evidence.LessonIDs), maxEvidenceLesson)
	}
	if len(req.Evidence.ReferenceIDs) > maxEvidenceRef {
		return nil, nil, fmt.Errorf("validateInput: evidence.reference_ids has %d entries, max %d", len(req.Evidence.ReferenceIDs), maxEvidenceRef)
	}
	// Soft warnings (don't block).
	if req.Proposal.Name == "" {
		warnings = append(warnings, "missing_proposal_name")
	}
	if req.Proposal.Version == "" {
		warnings = append(warnings, "missing_proposal_version")
	}
	return warnings, errors, nil
}

var (
	verbIngRE  = regexp.MustCompile(`\b\w+ing\b`)
	verbToRE   = regexp.MustCompile(`\bto\s+\w+`)
	commaSplit = regexp.MustCompile(`[,;]| and | or `)
)

// WhenToUseCheck holds validation findings for a skill's when_to_use field.
type WhenToUseCheck struct {
	Warnings []string
	Errors   []string
}

// checkWhenToUse applies the spec §7 validation rules.
func checkWhenToUse(proposed string, mode string) WhenToUseCheck {
	var result WhenToUseCheck
	if proposed == "" {
		result.Errors = append(result.Errors, "when_to_use_empty")
		return result
	}
	if len(proposed) < 30 {
		result.Warnings = append(result.Warnings, "when_to_use_short")
	}
	if !verbIngRE.MatchString(proposed) && !verbToRE.MatchString(proposed) {
		result.Warnings = append(result.Warnings, "when_to_use_no_verb")
	}
	parts := commaSplit.Split(proposed, -1)
	if len(parts) < 2 {
		result.Warnings = append(result.Warnings, "when_to_use_single_phrase")
	}
	return result
}

// whenToUseEqualsName is called separately by the orchestrator with
// the proposed name (since checkWhenToUse doesn't have it).
func whenToUseEqualsName(proposed, name string) bool {
	return proposed != "" && proposed == name
}

// DuplicateMatch describes a skill that overlaps with the proposal.
type DuplicateMatch struct {
	Name         string  `json:"name"`
	ID           string  `json:"id"`
	OverlapScore float64 `json:"overlap_score"`
	Reason       string  `json:"reason"`
}

// DuplicateCheckResult summarises the duplicate-detection outcome.
type DuplicateCheckResult struct {
	ExactMatch   bool            `json:"exact_match"`
	CloseMatches []DuplicateMatch `json:"close_matches"`
}

// detectDuplicates implements spec §8: token-set Jaccard overlap of
// proposal.WhenToUse against every existing skill's WhenToUse,
// combined with a FTS5 score (using the overlap score directly per
// the spec note that FTS5 is best-effort; the substring path is the
// primary signal in the small skill catalog). Returns the top 5 matches
// by combined score. Excludes the skill named `excludeName` (used in
// refine mode to avoid flagging the skill being refined).
func detectDuplicates(dm *DatabaseManager, proposal SkillProposal, excludeName string) (DuplicateCheckResult, error) {
	existing, err := dm.ListSkills("all")
	if err != nil {
		return DuplicateCheckResult{}, fmt.Errorf("detectDuplicates: list: %w", err)
	}
	proposedLower := strings.ToLower(proposal.WhenToUse)
	var matches []DuplicateMatch
	for _, s := range existing {
		if s.Name == excludeName {
			continue
		}
		if s.WhenToUse == "" {
			continue
		}
		existingLower := strings.ToLower(s.WhenToUse)
		// Token-set Jaccard overlap.
		proposedTokens := tokenize(proposedLower)
		existingTokens := tokenize(existingLower)
		overlap := jaccard(proposedTokens, existingTokens)
		if overlap < 0.3 {
			continue
		}
		// For FTS5 we use the overlap score directly (the spec notes
		// FTS5 is best-effort; the substring path is the primary signal
		// in the small skill catalog).
		combined := overlap
		matches = append(matches, DuplicateMatch{
			Name:         s.Name,
			ID:           s.ID,
			OverlapScore: combined,
			Reason:       fmt.Sprintf("when_to_use token overlap %f", overlap),
		})
	}
	sort.Slice(matches, func(i, j int) bool {
		return matches[i].OverlapScore > matches[j].OverlapScore
	})
	if len(matches) > 5 {
		matches = matches[:5]
	}
	// Exact match: combined >= 0.95 AND name matches.
	exact := false
	for _, m := range matches {
		if m.OverlapScore >= 0.95 && m.Name == proposal.Name {
			exact = true
			break
		}
	}
	return DuplicateCheckResult{ExactMatch: exact, CloseMatches: matches}, nil
}

// tokenize splits s on non-alphanumeric runes and returns a set of
// lowercase tokens as a map for efficient Jaccard computation.
func tokenize(s string) map[string]bool {
	out := map[string]bool{}
	for _, f := range strings.FieldsFunc(s, func(r rune) bool {
		return !(r >= 'a' && r <= 'z') && !(r >= 'A' && r <= 'Z') && !(r >= '0' && r <= '9')
	}) {
		out[strings.ToLower(f)] = true
	}
	return out
}

// jaccard computes the set Jaccard coefficient |A ∩ B| / |A ∪ B|.
func jaccard(a, b map[string]bool) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	inter := 0
	for k := range a {
		if b[k] {
			inter++
		}
	}
	union := len(a) + len(b) - inter
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}

// IdentityCheckOutcome is the disposition from the durable (name, version)
// idempotence check.
type IdentityCheckOutcome string

const (
	IdentityNone      IdentityCheckOutcome = "none"     // no existing row
	IdentitySame     IdentityCheckOutcome = "same"     // same content, return existing
	IdentityDifferent IdentityCheckOutcome = "different" // version collision
)

// identityCheck performs spec §5.1 stage 5: look up (name, version)
// before validation/publish. If the row exists with identical
// content_hash, return IdentitySame so the workshop returns the
// existing skill_id without re-writing (cache-loss-safe idempotence).
// If the row exists with a different hash, return IdentityDifferent
// so the workshop surfaces version_collision to the agent.
func identityCheck(dm *DatabaseManager, proposal SkillProposal, content string) (IdentityCheckOutcome, *Skill, error) {
	id, err := SkillIDForNameAndVersion(proposal.Name, proposal.Version)
	if err != nil {
		return IdentityNone, nil, fmt.Errorf("identityCheck: %w", err)
	}
	existing, err := dm.ReadSkill(id, "")
	if err != nil {
		// Not found is fine — new skill path.
		if strings.Contains(err.Error(), "not found") {
			return IdentityNone, nil, nil
		}
		return IdentityNone, nil, fmt.Errorf("identityCheck: read: %w", err)
	}
	// Compare content hashes.
	newHash := contentHash(content)
	if existing.ContentHash == newHash {
		return IdentitySame, existing, nil
	}
	return IdentityDifferent, existing, nil
}

// bumpVersion applies the deterministic spec §9 mapping:
//   correction     → patch
//   extension      → minor
//   restructuring  → minor
//   purpose_change → major
func bumpVersion(currentVersion, changeType string) (string, error) {
	base := "v" + currentVersion
	if !semver.IsValid(base) {
		return "", fmt.Errorf("bumpVersion: invalid current version %q", currentVersion)
	}
	// Strip the leading 'v' for parsing.
	ver := strings.TrimPrefix(base, "v")
	parts := strings.Split(ver, ".")
	if len(parts) != 3 {
		return "", fmt.Errorf("bumpVersion: invalid version format %q", currentVersion)
	}
	var major, minor, patch int
	if _, err := fmt.Sscanf(ver, "%d.%d.%d", &major, &minor, &patch); err != nil {
		return "", fmt.Errorf("bumpVersion: cannot parse version %q: %w", currentVersion, err)
	}
	switch changeType {
	case "correction":
		patch++
	case "extension":
		minor++
	case "restructuring":
		minor++
	case "purpose_change":
		major++
	default:
		return "", fmt.Errorf("bumpVersion: unknown change_type %q", changeType)
	}
	return fmt.Sprintf("%d.%d.%d", major, minor, patch), nil
}

// checkVersionBump verifies proposedVersion matches the deterministic
// bump from changeType applied to currentVersion. Returns nil on
// match; non-nil error on mismatch (caller surfaces
// version_bump_mismatch to the agent).
func checkVersionBump(proposedVersion, currentVersion, changeType string) error {
	expected, err := bumpVersion(currentVersion, changeType)
	if err != nil {
		return err
	}
	if proposedVersion != expected {
		return fmt.Errorf("version_bump_mismatch: proposed %q, expected %q for change_type %q",
			proposedVersion, expected, changeType)
	}
	return nil
}
