// Package seed (engine_capabilities.go) — ApplyCapabilities runtime.
//
// Mirrors ApplyDirectives (engine.go) and ApplySkills (engine.go)
// for the capability subsystem. Walks the SeedCapabilities
// registry and idempotently inserts each entry into the
// capabilities table. Contract:
//
//   - Row absent (no live id matching SavedID)        → Created
//   - Row present, source_code matches the seed       → Skipped
//   - Row present, source_code drifted from the seed → Drifted
//     (operator's local edit preserved; surfaced for visibility)
//
// ApplyCapabilities NEVER overwrites an operator's local edit.
// The Drifted bucket is purely for visibility — the operator
// sees the divergence and decides whether to reconcile.
//
// Initial state on insert is "validated" (not "active") so
// the lifecycle is exercised: probation → active is earned
// on first real invocations. See capabilities.go file header
// for the rationale.
//
// Routing:
//   ApplyCapabilities goes through capability.Store.InsertCapabilityProposal
//   (the canonical writer) so the scanner, linter-exempt path for
//   curated source, name-uniqueness check, and dependency check all
//   run — same guarantees any operator proposal gets. The seed does
//   NOT short-circuit with a raw INSERT.
package seed

import (
	"database/sql"
	"fmt"
	"strings"

	"github.com/flowbyte-com/mpm-core/capability"
)

// capabilityStoreSeeding is the slice of the capability.Store
// interface that ApplyCapabilities depends on. Defined as a
// local interface so tests can inject a fake without depending
// on the full Store surface (and so the seed package stays
// decoupled from any future capability API drift).
//
// Why InsertCapabilityProposalWithID (not InsertCapabilityProposal):
// the seed needs a deterministic id so "lookup, compare source_code,
// insert-or-skip" is idempotent. InsertCapabilityProposal generates
// a fresh UUID per call, which would break the Skipped/Drifted
// triage on every re-run. InsertCapabilityProposalWithID uses the
// canonical "cap.<name>" id from SeedCapability.SavedID() so the
// on-disk row is stable across re-runs.
//
// InitialState="validated" is set on the converted Proposal so the
// scanner + name-uniqueness + dependency checks all run via the
// canonical writer — same guarantees an operator proposal gets.
type capabilityStoreSeeding interface {
	InsertCapabilityProposalWithID(p *capability.Proposal, id string) (string, error)
	GetCapability(id string) (*capability.Capability, error)
}

// ApplyCapabilities walks the SeedCapabilities registry and
// idempotently inserts each entry into the capabilities table.
//
// store must be a connected *capability.Store (production calls
// pass NewStore(dm); tests pass a fake).
//
// This is a convenience wrapper around ApplyCapabilitiesFromBundle
// that uses the compiled-in SeedCapabilities slice. CLI / MCP
// surfaces that resolve a sidecar bundle (via LoadBundledCapabilities)
// call ApplyCapabilitiesFromBundle directly so the merged result
// (compiled + overrides + additions) is what actually gets seeded.
//
// The function is safe to call concurrently from multiple
// processes against the same DB only if SQLite's locking is
// properly serialized (which it is by default in WAL mode for
// single-writer).
//
// Returns a SeedSummary whose buckets reflect the lifecycle:
//   Created — capability rows newly inserted (validated state).
//   Skipped — row present, source_code matches the seed.
//   Drifted — row present, source_code differs from seed
//             (operator's edit preserved; flagged for visibility).
func ApplyCapabilities(store capabilityStoreSeeding) (SeedSummary, error) {
	return ApplyCapabilitiesFromBundle(store, SeedCapabilities)
}

// ApplyCapabilitiesFromBundle walks the supplied bundle and
// idempotently inserts each entry into the capabilities table.
// Same contract as ApplyCapabilities — Created / Skipped /
// Drifted buckets, scanner + name-uniqueness + dependency
// checks all run via InsertCapabilityProposal, no operator
// edits overwritten.
//
// Use this when the seed source isn't the compiled-in
// SeedCapabilities slice — typically after LoadBundledCapabilities
// has merged a sidecar JSON on top of the compiled registry.
// The CLI command `mpm capability seed` is the canonical
// caller; tests use it to seed custom bundles.
func ApplyCapabilitiesFromBundle(store capabilityStoreSeeding, bundle []SeedCapability) (SeedSummary, error) {
	summary := SeedSummary{
		Created: []string{},
		Skipped: []string{},
		Updated: []string{},
	}

	for _, sc := range bundle {
		savedID, err := sc.SavedID()
		if err != nil {
			return summary, fmt.Errorf("seed %s: SavedID: %w", sc.StableID, err)
		}
		if err := sc.Validate(); err != nil {
			return summary, fmt.Errorf("seed %s: Validate: %w", sc.StableID, err)
		}

		// 1. Look up existing row by saved id. Soft-deleted
		// rows are treated as absent (mirrors ApplySkills /
		// ApplyDirectives) so a re-init after accidental
		// shred can recover them.
		existing, lookupErr := store.GetCapability(savedID)
		switch {
		case lookupErr != nil && !isCapabilityNotFound(lookupErr):
			// Real DB error (not just "no row").
			return summary, fmt.Errorf("seed %s: lookup %s: %w", sc.StableID, savedID, lookupErr)

		case lookupErr != nil && isCapabilityNotFound(lookupErr):
			// 2a. No existing row — insert with the
			// deterministic seed id so the next seed run
			// finds the row and compares source_code.
			proposal := seedCapabilityToProposal(sc, savedID)
			if _, err := store.InsertCapabilityProposalWithID(proposal, savedID); err != nil {
				return summary, fmt.Errorf("seed %s: insert: %w", sc.StableID, err)
			}
			summary.Created = append(summary.Created, savedID)

		default:
			// 2b. Row exists — compare source_code against seed.
			// TrimSpace so trailing-newline drift (SaveSkill-
			// style canonicalization vs registry string
			// literals) doesn't trigger false drift reports.
			if strings.TrimSpace(existing.SourceCode) == strings.TrimSpace(sc.SourceCode) {
				summary.Skipped = append(summary.Skipped, savedID)
			} else {
				summary.Drifted = append(summary.Drifted, savedID)
			}
		}
	}

	return summary, nil
}

// seedCapabilityToProposal converts a SeedCapability into the
// canonical capability.Proposal shape. Lives as a free function
// rather than a SeedCapability method so the seed package stays
// the only consumer of capability.Proposal's typed API (keeps
// the test surface small).
//
// Conversion rules:
//
//   Name            ← sc.Name
//   Purpose         ← sc.Purpose
//   SourceCode      ← sc.SourceCode
//   SourceLanguage  ← sc.SourceLanguage
//   Tags            ← sc.Tags (StringSlice, JSON-encoded at the
//                     store layer)
//   RequestedDomain ← sc.RequestedDomain (cast to ExecutionDomain)
//   DependsOn       ← sc.DependsOn (resolved to cap.<name> ids;
//                     empty slice for the Tier 1 primitives)
//   AuthorAgent     ← "seed:baseline" (matches skills precedent)
//   InitialState    ← StateValidated (post-lint, post-dry-run;
//                     the forge tick promotes to probation on
//                     first invocation)
//   Metadata        ← sc.Metadata merged with seed-tracking fields:
//                       content_hash  : sc.ContentHash()
//                       stable_id     : sc.StableID
//                       tier          : "1" (for the Tier 1 set)
//                       primitive     : "true"
//                       risk_class    : "low"
//                     Operator-added metadata overrides the seed-
//                     tracking fields if it conflicts (rare; a
//                     metadata.content_hash override would be
//                     surprising but not catastrophic).
func seedCapabilityToProposal(sc SeedCapability, savedID string) *capability.Proposal {
	// Resolve DependsOn from names to cap.<name> ids. Empty
	// input → empty output (no normalization needed).
	depIDs := make([]string, 0, len(sc.DependsOn))
	for _, dep := range sc.DependsOn {
		depIDs = append(depIDs, "cap."+dep)
	}

	// Build metadata by merging operator-supplied with
	// seed-tracking. Operator metadata wins on conflict so
	// a future operator-added content_hash override isn't
	// silently clobbered.
	meta := capability.CapabilityMetadata{
		"content_hash": sc.ContentHash(),
		"stable_id":    sc.StableID,
		"tier":         "1",
		"primitive":    "true",
		"risk_class":   "low",
	}
	for k, v := range sc.Metadata {
		meta[k] = v
	}
	// savedID is the on-disk primary key. Stash it in
	// metadata too so audit / drift detection has a single
	// lookup target (the row id changes if Name changes;
	// the stable_id doesn't).
	meta["seed_id"] = savedID

	domain := capability.ExecutionDomain(sc.RequestedDomain)

	return &capability.Proposal{
		Name:            sc.Name,
		Purpose:         sc.Purpose,
		SourceCode:      sc.SourceCode,
		SourceLanguage:  sc.SourceLanguage,
		Tags:            capability.StringSlice(sc.Tags),
		RequestedDomain: domain,
		DependsOn:       depIDs,
		AuthorAgent:     "seed:baseline",
		InitialState:    capability.StateValidated,
		Metadata:        meta,
	}
}

// isCapabilityNotFound reports whether err is the canonical
// "no row matched" signal from the capability Store. Uses
// errors.Is-style matching via a SQL no-rows fallback so the
// seed engine doesn't depend on the capability package's
// exported ErrNotFound directly (matches the engine's other
// decoupled-from-implementation choices).
func isCapabilityNotFound(err error) bool {
	if err == nil {
		return false
	}
	if err == capability.ErrNotFound {
		return true
	}
	if err == sql.ErrNoRows {
		return true
	}
	// Defensive: the capability.Store.GetCapability path
	// also surfaces capability.ErrNotFound via fmt.Errorf
	// wrapping, so check the message too. Cheap regex
	// against the canonical sentinel substring.
	return strings.Contains(err.Error(), "capability: not found")
}