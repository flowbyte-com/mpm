// Package seed (capabilities_loader.go) — bundled_capabilities.json
// sidecar loader.
//
// Lets operators ship a custom capability bundle by placing
// bundled_capabilities.json in their MPM_WORKSPACE (or any
// explicit path). The loader merges the sidecar entries with
// the compiled-in SeedCapabilities registry; sidecar entries
// with the same stable_id override the compiled ones so an
// operator can patch a shipped primitive without forking the
// mpm binary.
//
// Why this exists:
//
//	Capabilities are stateful artifacts with a lifecycle.
//	The compiled-in SeedCapabilities is the baseline
//	("Tier 1 read-only primitive set"). An operator who
//	wants to add a custom primitive — or override the
//	source_code of a shipped primitive (e.g., to point at
//	a different substrate tool) — should not need to
//	recompile mpm. The sidecar JSON is the seam.
//
// Resolution order at seed time:
//
//	1. Compiled-in SeedCapabilities (always present, even
//	   when the sidecar is absent).
//	2. Sidecar overrides keyed by stable_id. A sidecar
//	   entry with stable_id matching a compiled entry
//	   replaces the compiled source_code + metadata;
//	   the compiled Notes / struct-level fields stay
//	   unless the sidecar overrides them too.
//	3. Sidecar additions (stable_id not in the compiled
//	   registry) are appended.
//
// The compiled registry is the source of truth for
// structural invariants (name format, source_language set,
// domain set, etc.). Sidecar entries that violate those
// invariants are rejected at load time — the seed refuses
// to start with a bad bundle.
package seed

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// bundledCapabilitiesSchemaVersion is the only schema
// version this loader accepts. Bumping it is a contract
// change that requires updating both this constant and the
// bundled_capabilities.json sidecar shipped with mpm.
const bundledCapabilitiesSchemaVersion = "1.0"

// bundledCapabilitiesDoc is the wire shape of
// bundled_capabilities.json. Mirrors the on-disk JSON;
// field tags match the JSON keys exactly. The struct is
// internal — callers go through LoadBundledCapabilities.
type bundledCapabilitiesDoc struct {
	SchemaVersion string             `json:"schema_version"`
	Description   string             `json:"description,omitempty"`
	Capabilities  []SeedCapability   `json:"capabilities"`
}

// LoadBundledCapabilitiesResult bundles the merged registry
// with the audit info the seed report prints. Operator-facing
// surfaces (CLI / MCP) walk this struct to surface "N entries
// loaded from the sidecar; M entries compiled in."
type LoadBundledCapabilitiesResult struct {
	// Merged is the resolved registry: compiled-in
	// entries with sidecar overrides applied, plus any
	// sidecar additions. Pass this to ApplyCapabilities
	// (or walk SeedCapabilities as usual).
	Merged []SeedCapability

	// SidecarPath is the absolute path the loader read
	// from, or "" if no sidecar was found.
	SidecarPath string

	// CompiledCount is len(SeedCapabilities) at load time.
	CompiledCount int

	// SidecarOverridesCount is how many sidecar entries
	// overrode compiled entries (stable_id collision).
	SidecarOverridesCount int

	// SidecarAdditionsCount is how many sidecar entries
	// were appended (no stable_id collision).
	SidecarAdditionsCount int
}

// LoadBundledCapabilities reads bundled_capabilities.json
// from `path` (or the default MPM_WORKSPACE/bundled_capabilities.json
// when path is empty) and returns a merged registry.
//
// When `path` is empty AND no default sidecar is found,
// returns LoadBundledCapabilitiesResult.Merged = a copy of
// SeedCapabilities (no overrides, no additions) and
// SidecarPath = "". The caller can detect "no sidecar" via
// result.SidecarPath == "".
//
// Errors:
//   * schema_version mismatch → typed ErrSchemaVersionMismatch
//   * file present but malformed JSON → wrapped parse error
//   * sidecar entry fails SeedCapability.Validate() → wrapped
//     validation error (the seed refuses to start with a bad
//     bundle rather than silently dropping the entry)
func LoadBundledCapabilities(path string) (LoadBundledCapabilitiesResult, error) {
	var result LoadBundledCapabilitiesResult
	result.CompiledCount = len(SeedCapabilities)

	// Start from a copy of the compiled registry so we
	// never mutate SeedCapabilities in place. The seed
	// engine is the only consumer; in-place mutation would
	// leak sidecar state across calls.
	result.Merged = make([]SeedCapability, len(SeedCapabilities))
	copy(result.Merged, SeedCapabilities)

	// Resolve the sidecar path: explicit arg wins, then
	// MPM_WORKSPACE/bundled_capabilities.json, then the
	// empty path → "no sidecar" sentinel.
	resolved, err := resolveBundledPath(path)
	if err != nil {
		return result, err
	}
	if resolved == "" {
		return result, nil
	}
	result.SidecarPath = resolved

	// Read + parse the JSON.
	data, err := os.ReadFile(resolved)
	if err != nil {
		return result, fmt.Errorf("seed: LoadBundledCapabilities: read %s: %w", resolved, err)
	}
	var doc bundledCapabilitiesDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		return result, fmt.Errorf("seed: LoadBundledCapabilities: parse %s: %w", resolved, err)
	}

	// Schema guard. Bumping the version is a deliberate
	// operator action; the loader refuses to silently
	// interpret an unknown shape.
	if doc.SchemaVersion != bundledCapabilitiesSchemaVersion {
		return result, fmt.Errorf("%w: got %q, want %q (path: %s)",
			ErrSchemaVersionMismatch, doc.SchemaVersion,
			bundledCapabilitiesSchemaVersion, resolved)
	}

	// Merge: for each sidecar entry, look up the
	// compiled entry by stable_id. Match → override.
	// Miss → append.
	for i := range doc.Capabilities {
		side := &doc.Capabilities[i]

		// Field-level validation BEFORE merging so a
		// bad sidecar entry aborts the whole load.
		if err := side.Validate(); err != nil {
			return result, fmt.Errorf("seed: LoadBundledCapabilities: sidecar entry %q: %w",
				side.StableID, err)
		}

		idx := findSeedCapabilityByStableID(result.Merged, side.StableID)
		if idx >= 0 {
			// Override. Preserve the original
			// Notes when the sidecar doesn't supply
			// one (lets operators patch source_code
			// without re-typing the rationale).
			if side.Notes == "" {
				side.Notes = result.Merged[idx].Notes
			}
			result.Merged[idx] = *side
			result.SidecarOverridesCount++
			continue
		}
		result.Merged = append(result.Merged, *side)
		result.SidecarAdditionsCount++
	}

	return result, nil
}

// resolveBundledPath picks the sidecar path: explicit arg
// → MPM_WORKSPACE/bundled_capabilities.json → "" (no
// sidecar). Returns an empty string (NOT an error) when no
// sidecar is configured; that's a valid state — the loader
// just returns the compiled registry unchanged.
//
// An explicit path that doesn't exist is an error (the
// operator asked for a file that isn't there — silent
// fallback would be surprising).
func resolveBundledPath(path string) (string, error) {
	if path != "" {
		abs, err := filepath.Abs(path)
		if err != nil {
			return "", fmt.Errorf("seed: resolveBundledPath: abs %q: %w", path, err)
		}
		if _, err := os.Stat(abs); err != nil {
			return "", fmt.Errorf("seed: resolveBundledPath: stat %q: %w", abs, err)
		}
		return abs, nil
	}

	// Default: $MPM_WORKSPACE/bundled_capabilities.json.
	// MPM_WORKSPACE may be unset — that means the
	// operator hasn't run `mpm start` yet, and there's
	// no sidecar to read. Treat as "no sidecar."
	ws := strings.TrimSpace(os.Getenv("MPM_WORKSPACE"))
	if ws == "" {
		return "", nil
	}
	defaultPath := filepath.Join(ws, "bundled_capabilities.json")
	if _, err := os.Stat(defaultPath); err != nil {
		if os.IsNotExist(err) {
			return "", nil // sidecar is optional
		}
		return "", fmt.Errorf("seed: resolveBundledPath: stat %q: %w", defaultPath, err)
	}
	return defaultPath, nil
}

// findSeedCapabilityByStableID scans the slice (O(N)) and
// returns the index of the entry with matching stable_id,
// or -1 if absent. Inline so the loader doesn't drag in
// the SeedCapabilitiesByStableID closure (which scans the
// compiled registry, not a caller-supplied slice).
func findSeedCapabilityByStableID(slice []SeedCapability, stableID string) int {
	for i := range slice {
		if slice[i].StableID == stableID {
			return i
		}
	}
	return -1
}

// ErrSchemaVersionMismatch is returned by
// LoadBundledCapabilities when the sidecar's schema_version
// doesn't match the loader's compiled-in version. The
// operator must either upgrade mpm or downgrade the
// sidecar; silent interpretation of an unknown shape would
// risk surfacing broken capabilities.
var ErrSchemaVersionMismatch = fmt.Errorf("seed: bundled_capabilities.json schema_version mismatch")