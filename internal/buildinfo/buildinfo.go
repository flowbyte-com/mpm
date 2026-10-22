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
//
// The package also assembles the BUILD IDENTITY: the product version plus
// the build metadata that says which commit the binary came from, whether
// the source tree was clean, and when the binary was built. It is a
// Semantic Versioning version with build metadata, and it is computed
// entirely from the linker-stamped values — never from Git and never from
// the clock at the moment the version command runs.
package buildinfo

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
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

	// BuildEpoch is the UTC Unix epoch second at which the build ran.
	//
	// It is stamped at BUILD time, from SOURCE_DATE_EPOCH when that
	// variable holds a non-negative integer (the reproducible-builds
	// convention) and from the build machine's clock otherwise. The
	// value is captured once, when the binary is linked; it is never
	// re-derived when the version command runs.
	//
	// "unknown" means no build time could be established. It is never a
	// substituted value.
	BuildEpoch = "unknown"

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
//
// `build_identity` is the one-line form of everything the other fields
// say: the product version plus the build metadata that identifies the
// commit, the source state and the build time. `build_epoch` is the UTC
// Unix epoch second the build ran at, and is 0 when no build time could
// be established.
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
	BuildEpoch  int64  `json:"build_epoch"`
	// BuildIdentity is the complete build identity. See Identity.
	BuildIdentity string `json:"build_identity"`
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
	epoch := parseEpoch(BuildEpoch)

	info := Info{
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
		BuildEpoch: epoch,
	}
	info.BuildIdentity = Identity(info)
	return info
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

// The components of the build identity.
//
// The identity is a Semantic Versioning version with BUILD METADATA:
// `<version>+<component>.<component>...`, where every component is a
// dot-separated identifier of `[0-9A-Za-z-]+` — exactly what SemVer
// requires of build metadata.
const (
	// identityCommitPrefix marks the source-commit component.
	identityCommitPrefix = "g"

	// identityEpochPrefix marks the build-time component.
	identityEpochPrefix = "t"

	// identityUnknown is the value used when a fact could not be
	// established.
	//
	// It is a word, not a hash and not a timestamp: a fact the build
	// could not establish is never dressed up as a real one.
	identityUnknown = "unknown"

	// identityDirty marks a build whose source tree differed from the
	// commit it was based on.
	identityDirty = "dirty"

	// identityDirtyUnknown marks a build whose source tree could not be
	// compared against a revision at all.
	//
	// It is deliberately NOT the same as identityDirty, and deliberately
	// NOT the absence of a marker: an unverifiable tree is not a clean
	// tree, and the identity must not let it look like one.
	identityDirtyUnknown = "dirty-unknown"

	// identityMinCommitLength is the shortest abbreviated commit SHA the
	// identity accepts.
	//
	// Eight hexadecimal characters is the width `git rev-parse
	// --short=8` produces and the width MPM's own documentation uses. A
	// shorter abbreviation cannot identify a revision on its own, so
	// such a value is not put into an identity.
	identityMinCommitLength = 8
)

// Identity returns the complete build identity of a binary.
//
// The identity is a Semantic Versioning version carrying build metadata:
//
//	0.1.0-dev+g3f8a21c.t1792679720
//	0.1.0-dev+g3f8a21c.dirty.t1792679720
//
// The components are, in order:
//
//	g<commit>   the abbreviated source commit — at least eight
//	            hexadecimal characters — or "gunknown" when the build had
//	            no Git metadata of its own.
//	.dirty      present only when the source tree differed from that
//	            commit. When the tree could not be compared against a
//	            revision at all, the marker is ".dirty-unknown": an
//	            unverifiable tree is not a clean tree, and the identity
//	            must not present it as one.
//	t<epoch>    the UTC Unix epoch second the build ran at, or
//	            "tunknown".
//
// A RELEASE carries no build metadata at all: the version alone
// identifies it, which is the conventional stable-release output.
//
// Nothing here is ever invented. A fact the build could not establish is
// reported as "unknown", never as a plausible-looking hash or a
// plausible-looking timestamp.
//
// The version part is the product version from the VERSION file, which
// is a Semantic Versioning string for every real build. The package's
// "dev" placeholder — what an unstamped binary carries — is not one, and
// is reported as such.
func Identity(info Info) string {
	version := info.Version
	if version == "" {
		version = "dev"
	}

	// A release is identified by its version. The commit and the build
	// time are still reported in the detail lines and in the JSON form.
	if info.Release {
		return version
	}

	parts := []string{
		identityCommitPrefix + identityCommit(info.ShortCommit, info.Commit),
	}

	switch info.DirtyState {
	case DirtyStateDirty:
		parts = append(parts, identityDirty)
	case DirtyStateUnknown:
		parts = append(parts, identityDirtyUnknown)
	}

	parts = append(parts, identityEpochPrefix+identityEpoch(info.BuildEpoch))

	return version + "+" + strings.Join(parts, ".")
}

// identityCommit returns the commit component of the build identity.
//
// The abbreviated SHA must be at least eight lowercase hexadecimal
// characters. When the stamped abbreviation is shorter than that, the
// eight-character prefix of the full SHA is used instead; when neither
// is usable, the fact is reported as unknown rather than guessed.
func identityCommit(short, full string) string {
	if isHexAtLeast(short, identityMinCommitLength) {
		return short
	}
	if isFullSHA(full) {
		return full[:identityMinCommitLength]
	}
	return identityUnknown
}

// identityEpoch returns the build-time component of the build identity.
//
// A non-positive epoch is treated as unknown: no real build happened at
// the Unix epoch, so such a value carries no information.
func identityEpoch(epoch int64) string {
	if epoch <= 0 {
		return identityUnknown
	}
	return strconv.FormatInt(epoch, 10)
}

// parseEpoch reads the stamped build epoch.
//
// Anything that is not a plain non-negative integer — "unknown", an
// empty string, a value a linker mangled — is an unknown epoch, never a
// fabricated one.
func parseEpoch(stamp string) int64 {
	if stamp == "" {
		return 0
	}
	n, err := strconv.ParseInt(stamp, 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// isHexAtLeast reports whether v is at least n lowercase hexadecimal
// characters.
func isHexAtLeast(v string, n int) bool {
	if len(v) < n {
		return false
	}
	for _, c := range v {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return false
		}
	}
	return true
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
// The first line is the build identity — "MPM <identity>" — so it can be
// scraped, and the detail lines carry the rest.
//
// The dirty line renders the three states distinctly: "no" for a clean
// tree, "yes" for a modified one, and "unknown" for a build whose
// source could not be verified.
//
// The built line renders the captured build time in UTC. It is derived
// from the epoch the build stamped, never from the clock at the moment
// the command runs.
func FormatHuman(info Info) string {
	dirty := "unknown"
	switch info.DirtyState {
	case DirtyStateClean:
		dirty = "no"
	case DirtyStateDirty:
		dirty = "yes"
	}

	identity := info.BuildIdentity
	if identity == "" {
		identity = Identity(info)
	}

	return fmt.Sprintf(
		"MPM %s\n"+
			"  binary:  %s\n"+
			"  commit:  %s\n"+
			"  build:   %s\n"+
			"  built:   %s\n"+
			"  dirty:   %s\n",
		identity,
		info.Binary, info.Commit,
		info.BuildType, formatEpoch(info.BuildEpoch),
		dirty,
	)
}

// formatEpoch renders a build epoch as a UTC timestamp.
//
// The rendering is derived from the epoch alone, so it is deterministic:
// two runs of the same binary print the same build time.
func formatEpoch(epoch int64) string {
	if epoch <= 0 {
		return "unknown"
	}
	return time.Unix(epoch, 0).UTC().Format(time.RFC3339)
}
