package capability

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// =============================================================================
// forge.go — capability proposal pipeline (spec §3.2)
//
// The Forge is the orchestration layer. It owns the 12-step validation
// pipeline in spec §3.2 and is the only path through which a
// proposal becomes a capabilities row. The Store owns the load-bearing
// transaction (InsertCapabilityProposal); the Forge owns the
// pre-insert decisions and the post-insert state shortcut
// (insert directly with state='validated' when the dry-run passed,
// per spec §3.2 step 11).
//
// The pipeline order matters:
//
//   1. Schema validation       — fast, no IO
//   2. Scanner pass             — fast, no IO (regex)
//   3. Linter pass              — medium, sub-process
//   4. Dedup check              — medium, DB scan
//   5. Dry-run                  — slow, sub-process + bwrap
//   6. Store insert             — atomic transaction
//
// Steps 1-4 are accumulated (all errors reported at once);
// step 5 only runs if 1-4 are clean; step 6 is final.
//
// The Forge is pluggable on every external dependency (linter,
// dedup, dry-run). Tests use the Fake* types. Production wires
// DefaultLinter + DefaultDedup + BwrapDryRunner.
// =============================================================================

// Forge is the proposal pipeline. Construct with NewForge; the
// returned value is safe for concurrent use (no per-call state).
type Forge struct {
	store  *Store
	linter Linter
	dedup  DedupChecker
	dryRun DryRunner
	clock  Clock
}

// NewForge builds a Forge. All three external dependencies
// (linter, dedup, dryRun) are required. A nil clock falls back
// to realClock (production default; tests should pass a frozen
// clock to make audit-field assertions deterministic).
func NewForge(store *Store, linter Linter, dedup DedupChecker, dryRun DryRunner, clock Clock) *Forge {
	if clock == nil {
		clock = realClock
	}
	return &Forge{
		store:  store,
		linter: linter,
		dedup:  dedup,
		dryRun: dryRun,
		clock:  clock,
	}
}

// ForgeResult is the outcome of one Propose call. Rejected=true
// means the proposal did NOT become a capabilities row; Reasons
// contains every step that failed (structured for the CLI/MCP
// surface to render as a `reasons` array). Rejected=false means
// the row is inserted; ID and State are populated; Reasons is nil.
type ForgeResult struct {
	Rejected bool
	ID       string
	Name     string
	State    CapabilityState
	Reasons  []FieldError
}

// Propose runs the §3.2 pipeline. Returns a ForgeResult; the
// error return is reserved for catastrophic failures (e.g. the
// database is unreachable). All proposal-level rejections surface
// as Rejected=true with a populated Reasons slice.
//
// The Forge accumulates all check failures (not fail-fast) so
// the agent can fix every problem in one round-trip. The dedup
// check and the dry-run ARE order-dependent — if the dedup
// matches, the dry-run is skipped (we don't waste 30s validating
// a duplicate).
func (f *Forge) Propose(ctx context.Context, payload *ForgePayload) (*ForgeResult, error) {
	if payload == nil {
		return nil, fmt.Errorf("capability: forge: payload is nil")
	}
	if f.store == nil {
		return nil, fmt.Errorf("capability: forge: store is nil")
	}
	if f.linter == nil || f.dedup == nil || f.dryRun == nil {
		return nil, fmt.Errorf("capability: forge: missing required dependency (linter/dedup/dryRun)")
	}

	// 1. Schema validation.
	var reasons []FieldError
	if err := payload.Validate(); err != nil {
		var pve *PayloadValidationError
		if errors.As(err, &pve) {
			reasons = append(reasons, pve.Errors...)
		} else {
			reasons = append(reasons, FieldError{Step: "schema", Message: err.Error()})
		}
	}

	// 2. Scanner pass. Only run if source_code is non-empty
	// (the schema check above may have already flagged it).
	//
	// scannerBlocked is the explicit "DO NOT LINT POISON" short-circuit
	// signal. If the scanner matched any §7.1 pattern, the linter is
	// skipped entirely — we never feed known-malicious source into a
	// third-party subprocess. Linters are static analysis tools, but
	// the linter binary itself is un-sandboxed attack surface; an
	// attacker who knew shellcheck's parser had a buffer overflow
	// could craft a proposal that the scanner wouldn't catch but the
	// linter would. The scanner is the security boundary; the linter
	// is the quality boundary. Crossing the two is the bug.
	scannerBlocked := false
	if payload.SourceCode != "" {
		if findings := ScanSource(payload.SourceCode); len(findings) > 0 {
			scannerBlocked = true
			reasons = append(reasons, findings...)
		}
	}

	// 3. Linter pass. ONLY runs if the scanner did not block. This
	// is the "do not lint poison" boundary — preserved here as an
	// explicit boolean (scannerBlocked) rather than a side-channel
	// reason-list scan, so a future maintainer can't accidentally
	// drop the gate by reordering the reasons slice.
	if !scannerBlocked && payload.SourceCode != "" {
		lintCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
		report, err := f.linter.Lint(lintCtx, payload.SourceLanguage, payload.SourceCode)
		cancel()
		if err != nil {
			// Linter infrastructure failure. Hard-reject
			// (fail-closed for toolchain) — a missing or
			// broken linter is a security boundary violation.
			reasons = append(reasons, FieldError{
				Step:    "linter",
				Field:   "source_code",
				Message: fmt.Sprintf("linter infrastructure error: %v", err),
			})
		} else if !report.OK {
			for _, finding := range report.Findings {
				if finding.Severity != "error" {
					continue // warnings don't block
				}
				reasons = append(reasons, FieldError{
					Step: "linter:" + report.Linter,
					Field: "source_code",
					Message: fmt.Sprintf("%s [%s]: %s",
						finding.Code, report.Linter, finding.Message),
					Line:    finding.Line,
					Snippet: fmt.Sprintf("col %d", finding.Column),
				})
			}
		}
	}

	// Early-exit if steps 1-3 found any issue. Steps 4-5
	// (dedup, dry-run) are order-dependent: a dedup match
	// means we should NOT spend 30s on a dry-run of a
	// duplicate. Errors in linter/scanner/schema are
	// deterministic — the agent has to fix them anyway.
	if len(reasons) > 0 {
		return &ForgeResult{Rejected: true, Name: payload.Name, Reasons: reasons}, nil
	}

	// 4. Dedup check. The skip list contains the replaces_id
	// (if any) — that's the fork-flow bypass from spec §3.6.
	var skipList []string
	if payload.ReplacesID != nil {
		skipList = append(skipList, *payload.ReplacesID)
	}
	dedupRes, err := f.dedup.Check(ctx, payload.Purpose, skipList)
	if err != nil {
		return nil, fmt.Errorf("capability: forge: dedup: %w", err)
	}
	if dedupRes.Matched {
		reasons = append(reasons, FieldError{
			Step: "dedup",
			Field: "purpose",
			Message: fmt.Sprintf(
				"purpose matches existing capability %q (id=%s, similarity=%.3f); "+
					"either reuse that capability or set replaces_id to fork it",
				dedupRes.MatchedName, dedupRes.MatchedID, dedupRes.Similarity),
		})
		return &ForgeResult{Rejected: true, Name: payload.Name, Reasons: reasons}, nil
	}

	// 5. Dry-run. Only if all prior steps are clean.
	dryCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	dryRes, err := f.dryRun.Run(dryCtx, payload.SourceLanguage, payload.SourceCode)
	cancel()
	if err != nil {
		reasons = append(reasons, FieldError{
			Step:    "dryrun",
			Field:   "source_code",
			Message: fmt.Sprintf("dry-run infrastructure error: %v", err),
		})
		return &ForgeResult{Rejected: true, Name: payload.Name, Reasons: reasons}, nil
	}
	if !dryRes.Pass {
		reasons = append(reasons, FieldError{
			Step: "dryrun",
			Field: "source_code",
			Message: fmt.Sprintf("dry-run failed: %s", dryRes.Reason),
		})
		return &ForgeResult{Rejected: true, Name: payload.Name, Reasons: reasons}, nil
	}

	// 6. Insert via the Store. The Store runs the remaining
	// checks (size, name uniqueness, dep liveness, domain
	// policy, source hash) atomically with the insert.
	proposal := payload.ToProposal()
	if dryRes.Pass {
		// Spec §3.2 step 11: insert with state='validated'
		// when dry-run passed (skipping the draft→linted
		// transitions that don't apply — the lint + dry-run
		// already happened).
		proposal.InitialState = StateValidated
	} else {
		// Defensive: this branch is unreachable because we
		// returned above on !dryRes.Pass. Kept for clarity
		// in case the early return is ever removed.
		proposal.InitialState = StateLinted
	}

	capID, err := f.store.InsertCapabilityProposal(proposal)
	if err != nil {
		return nil, fmt.Errorf("capability: forge: insert: %w", err)
	}
	return &ForgeResult{
		Rejected: false,
		ID:       capID,
		Name:     proposal.Name,
		State:    proposal.InitialState,
		Reasons:  nil,
	}, nil
}

// hasStep returns true if any FieldError in reasons has a Step
// prefix that matches. Used to skip the linter when the scanner
// already rejected (the linter would re-report the same lines).
func hasStep(reasons []FieldError, prefix string) bool {
	for _, r := range reasons {
		if len(r.Step) >= len(prefix) && r.Step[:len(prefix)] == prefix {
			return true
		}
	}
	return false
}
