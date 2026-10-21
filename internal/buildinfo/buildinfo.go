// Package buildinfo carries the build provenance embedded in every MPM
// binary and the opt-in update check that compares it against upstream.
//
// The provenance values are stamped at LINK time by the canonical build
// (see scripts/build-provenance.sh and the Makefile). Nothing here reads
// git at runtime: a binary must be able to report its own provenance with
// no git checkout present, which is the normal case for an installed
// binary.
//
// ONE authoritative source of the product version: the VERSION file at the
// repository root. The build reads it once and stamps the result into
// every binary; the binary never re-derives it.
package buildinfo

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Linker-stamped values. Every one of these is a `var` (never a `const`):
// a `const` would bake the placeholder into the binary and the linker's
// -X flag would silently no-op.
var (
	// Version is the product version, from the repository's VERSION file.
	Version = "dev"

	// Commit is the full source commit SHA the binary was built from.
	Commit = "unknown"

	// ShortCommit is the abbreviated source commit SHA.
	ShortCommit = "unknown"

	// Dirty is the stamped dirty state of the build: "true" when the
	// build contained uncommitted source changes, "false" when the tree
	// was clean, and "unknown" when the state could not be established
	// (a build from an exported source tree with no Git metadata, or a
	// binary that was never stamped at all).
	Dirty = "unknown"

	// BuildType is "development" for ordinary builds and "release" for a
	// build made from a formally tagged release.
	BuildType = "development"

	// Repository is the upstream "owner/name" the update check compares
	// against.
	Repository = "flowbyte-com/mpm"
)

// The dirty states a build can be in.
//
// "clean" and "unknown" are different facts and are reported as such.
// A build whose source tree could not be compared against a revision
// has NOT been verified clean, and the distinction has to survive all
// the way to the operator's screen.
const (
	// DirtyStateClean means the built tree matched its commit exactly.
	DirtyStateClean = "clean"

	// DirtyStateDirty means the built tree differed from its commit.
	DirtyStateDirty = "dirty"

	// DirtyStateUnknown means the dirty state could not be established.
	DirtyStateUnknown = "unknown"
)

// shortCommitLength is the width of the abbreviated commit SHA shown to
// operators. Eight characters matches `git rev-parse --short=8` and the
// conventional width used in MPM's own documentation.
const shortCommitLength = 8

// Info is the provenance of one binary, as reported to an operator.
//
// The JSON keys are the documented machine-readable contract; see
// docs/SPEC.md.
//
// `dirty` and `dirty_state` are two views of one fact, kept side by
// side for compatibility. `dirty` is the boolean an existing consumer
// already reads: true only when the build is KNOWN to contain
// uncommitted source changes. `dirty_state` is the precise answer —
// "clean", "dirty" or "unknown" — and it is what distinguishes a
// verified-clean build from a build nobody was able to verify.
type Info struct {
	Version     string `json:"version"`
	Commit      string `json:"commit"`
	ShortCommit string `json:"short_commit"`
	Dirty       bool   `json:"dirty"`
	DirtyState  string `json:"dirty_state"`
	Release     bool   `json:"release"`
	BuildType   string `json:"build_type"`
	Repository  string `json:"repository"`
	Binary      string `json:"binary"`
}

// Current returns the provenance embedded in this binary.
//
// binary names the executable ("mpm", "mpm-mcp", ...) so an operator can
// tell which of the five binaries answered.
func Current(binary string) Info {
	short := ShortCommit
	if isPlaceholder(short) && isFullSHA(Commit) {
		short = Commit[:shortCommitLength]
	}

	state := dirtyState(Dirty)

	return Info{
		Version:     Version,
		Commit:      Commit,
		ShortCommit: short,
		Dirty:       state == DirtyStateDirty,
		DirtyState:  state,
		// A release is a build from a formally tagged revision AND from
		// a clean tree. A locally modified tree on a release tag is not
		// the released artifact, and must never be presented as one.
		Release:    BuildType == "release" && state == DirtyStateClean,
		BuildType:  BuildType,
		Repository: Repository,
		Binary:     binary,
	}
}

// dirtyState maps the stamped dirty value onto the three-state
// contract.
//
// Anything that is not exactly "true" or "false" — including an empty
// string and an unstamped binary — is UNKNOWN. Guessing "clean" from an
// unrecognised value would turn a missing stamp into a claim of
// verified-clean source.
func dirtyState(stamp string) string {
	switch stamp {
	case "true":
		return DirtyStateDirty
	case "false":
		return DirtyStateClean
	default:
		return DirtyStateUnknown
	}
}

// isPlaceholder reports whether a stamped value is the "not stamped"
// sentinel rather than a real value.
func isPlaceholder(v string) bool {
	return v == "" || v == "unknown"
}

// ArgvRequestsVersion reports whether an argument vector asks for the
// version surface instead of normal operation.
//
// Only the FIRST argument decides. `mpm recall version` searches for
// the word "version"; it is not a version request.
//
// The recognised spellings are the ones MPM already used (`-v`,
// `--version`), the explicit `version` subcommand, and the `-V` form
// the telemetry sidecar has always accepted.
func ArgvRequestsVersion(argv []string) bool {
	if len(argv) == 0 {
		return false
	}
	switch argv[0] {
	case "version", "-v", "--version", "-V":
		return true
	}
	return false
}

// isFullSHA reports whether v looks like a full 40-character git SHA.
func isFullSHA(v string) bool {
	if len(v) != 40 {
		return false
	}
	for _, c := range v {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return false
		}
	}
	return true
}

// FormatJSON renders the provenance as the documented JSON object.
//
// Machine-readable output must not depend on terminal rendering, so this
// returns bytes rather than printing.
func FormatJSON(info Info) ([]byte, error) {
	return json.MarshalIndent(info, "", "  ")
}

// FormatHuman renders the provenance as the documented human-readable
// block.
//
// The first line is stable in shape — "MPM v<version> (<short sha>)" — so
// it can be scraped, and the detail lines carry the rest.
//
// The dirty line renders the three states distinctly: "no" for a clean
// tree, "yes" for a modified one, and "unknown" for a build whose
// source could not be verified.
func FormatHuman(info Info) string {
	dirty := "unknown"
	switch info.DirtyState {
	case DirtyStateClean:
		dirty = "no"
	case DirtyStateDirty:
		dirty = "yes"
	}

	return fmt.Sprintf(
		"MPM v%s (%s)\n"+
			"  binary:  %s\n"+
			"  commit:  %s\n"+
			"  build:   %s\n"+
			"  dirty:   %s\n",
		info.Version, info.ShortCommit,
		info.Binary, info.Commit,
		info.BuildType, dirty,
	)
}
