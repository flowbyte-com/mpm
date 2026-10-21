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

	// Dirty is "true" when the build contained uncommitted source changes.
	Dirty = "false"

	// BuildType is "development" for ordinary builds and "release" for a
	// build made from a formally tagged release.
	BuildType = "development"

	// Repository is the upstream "owner/name" the update check compares
	// against.
	Repository = "flowbyte-com/mpm"
)

// shortCommitLength is the width of the abbreviated commit SHA shown to
// operators. Eight characters matches `git rev-parse --short=8` and the
// conventional width used in MPM's own documentation.
const shortCommitLength = 8

// Info is the provenance of one binary, as reported to an operator.
//
// The JSON keys are the documented machine-readable contract; see
// docs/SPEC.md.
type Info struct {
	Version     string `json:"version"`
	Commit      string `json:"commit"`
	ShortCommit string `json:"short_commit"`
	Dirty       bool   `json:"dirty"`
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

	return Info{
		Version:     Version,
		Commit:      Commit,
		ShortCommit: short,
		Dirty:       Dirty == "true",
		Release:     BuildType == "release",
		BuildType:   BuildType,
		Repository:  Repository,
		Binary:      binary,
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
func FormatHuman(info Info) string {
	dirty := "no"
	if info.Dirty {
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
