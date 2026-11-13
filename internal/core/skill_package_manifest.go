package internal

// skill_package_manifest.go — Skill Packages, stage P2: the manifest
// type, its validation, the canonical JSON encoding, and the stable
// `package_hash`.
//
// Design of record:
//   docs/dev/skill-package-format-design-2026-11-04.md
//   §5.1 (identity), §5.2 (version handling), §5.4 (canonical paths and
//   display-only `raw_path`), §5.5 (the file manifest), §9 (the row
//   columns the manifest must fill), §10 P2 (this stage).
//   Approved: PR #222, comment 6658245185.
//
// This file is PURE. It performs no import, no export, no scan and no
// database write. It describes the manifest and hashes it.
//
// ─────────────────────────────────────────────────────────────────────────────
// WHAT THE HASH COVERS, AND WHY `raw_path` IS NOT IN IT
//
// `package_hash` is the SHA-256 of the canonical manifest JSON (§5.5):
// object keys sorted, no insignificant whitespace, UTF-8, `files` sorted
// by path. It deliberately does not depend on the archive container, so
// export → import is stable across ZIP writers, compression levels and
// timestamps.
//
// Two consequences follow from the design and are honoured here:
//
//  1. `package_hash` CANNOT BE A MEMBER OF THE DOCUMENT IT HASHES. A
//     hash of a document that contains the hash is circular. So the
//     hashed document omits the `package_hash` key, and the STORED
//     manifest adds it back. Because canonicalisation sorts keys, the
//     stored manifest is exactly the canonical form of the complete
//     manifest — see StoredJSON.
//
//  2. `raw_path` IS NOT IN THE HASHED DOCUMENT. §5.4 and T48 require
//     that the same package imported from a flat archive and from a
//     wrapped archive produce "the same canonical paths, the same file
//     rows and the same `package_hash` — only `raw_path` differs". If
//     `raw_path` were hashed, those two imports would hash differently.
//     `raw_path` therefore lives in the `skill_package_files.raw_path`
//     column (§9) and in no hashed document. It is display-only: it
//     never addresses a file and is never compared for identity.
//
// The `scanned` block IS part of the manifest and therefore part of the
// hash: two packages with identical bytes but different scan outcomes
// are not the same package (§5.5).
//
// ─────────────────────────────────────────────────────────────────────────────
// THE SCAN STATUS NEVER CLAIMS MORE THAN HAPPENED
//
// `scanned` records what actually ran, per file (§5.5). It is not a
// constant `true`: it is set from the outcome of the scan, and it is
// false for a scanner that did not run. A binary asset has
// `poison: false` — a DISCLOSED SKIP, not a claim of safety.
//
// P2 does not scan anything. It models the two booleans and validates
// them; it never sets one to true on its own.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"golang.org/x/mod/semver"
)

// PackageFormatV1 is the manifest format identifier of this stage
// (§5.5, and the `format` column's default in §9).
const PackageFormatV1 = "mpm-skill-package/1"

// PackageRole is the role a file plays inside a package.
//
// The vocabulary is closed at the four values §5.5 names: `entrypoint`,
// `reference`, `asset`, `script`. The §9 column is NOT NULL, so the
// derivation has to be total.
type PackageRole string

const (
	// PackageRoleEntrypoint — the package's entrypoint, special-cased
	// out of the path convention (§5.5).
	PackageRoleEntrypoint PackageRole = "entrypoint"

	// PackageRoleReference — supporting reference material. Derived
	// from the `references/` convention.
	PackageRoleReference PackageRole = "reference"

	// PackageRoleAsset — package assets. Derived from the `assets/`
	// convention.
	PackageRoleAsset PackageRole = "asset"

	// PackageRoleScript — package scripts. Derived from the `scripts/`
	// convention. Scripts are STORED and FLAGGED, never executed (§7
	// threat 16).
	PackageRoleScript PackageRole = "script"
)

// PackageRoleDefault is the role given to a file that is neither the
// entrypoint nor under one of the three named content directories.
//
// §5.5 names the four roles and the three path conventions, but does
// not say what a file at the package root beside the entrypoint is —
// `README.md`, say. The role vocabulary is closed at four values and the
// §9 column is NOT NULL, so a total derivation is required.
//
// The fallback is `reference`: `reference` is the role for supporting
// material of the package, whereas `asset` is the role the design
// associates with binary, non-textual content (O3, §7 threat 16). A
// root-level text file is not a binary asset, so labelling it `asset`
// would misinform a reader of the manifest.
//
// This is a RESOLVED GAP, not a rule read out of the design. It is
// stated here, pinned by a test, and is the one place in this stage
// where the design left the mapping open.
const PackageRoleDefault = PackageRoleReference

// ValidPackageRoles returns the closed set of role values, in a stable
// order.
func ValidPackageRoles() []PackageRole {
	return []PackageRole{
		PackageRoleEntrypoint,
		PackageRoleReference,
		PackageRoleAsset,
		PackageRoleScript,
	}
}

// IsValidPackageRole reports whether role is one of the four values
// §5.5 names.
func IsValidPackageRole(role PackageRole) bool {
	for _, r := range ValidPackageRoles() {
		if r == role {
			return true
		}
	}
	return false
}

// PackageScanStatus records which scanners actually ran on a file
// (§5.5, §7 threat 14, and the `secret_scanned` / `poison_scanned`
// columns in §9).
//
// A `false` here means "this scanner did not run" — a disclosed skip,
// never a claim of safety. A scan that could not run at all is an
// IMPORT REFUSAL, not a `false` entry: nothing is written, so no row
// can ever record a scan that did not happen (R10).
type PackageScanStatus struct {
	// Secret — the secret scanner ran on this file.
	Secret bool `json:"secret"`

	// Poison — the poison scanner ran on this file.
	Poison bool `json:"poison"`
}

// PackageManifestFile is one entry of the manifest's `files` array
// (§5.5).
//
// Path is the CANONICAL path and is the file's identity (§5.4). There is
// deliberately no raw_path field: the original name is display-only
// metadata and it lives in its own column, outside the hashed document.
type PackageManifestFile struct {
	// Path is the canonical, NFC-normalised, POSIX-separated path.
	Path string `json:"path"`

	// SHA256 is the lowercase hex SHA-256 of the file's bytes.
	SHA256 string `json:"sha256"`

	// SizeBytes is the length of the file's bytes.
	SizeBytes int64 `json:"size_bytes"`

	// ContentType comes from a small, explicit allow-list keyed on
	// extension plus a content sniff — never a general MIME guesser
	// (§5.5). P2 does not derive it: the allow-list is not enumerated
	// in the design, so deriving one here would be invention. The field
	// is carried and validated; derivation lands with the import
	// stages.
	ContentType string `json:"content_type"`

	// Role is the file's role inside the package.
	Role PackageRole `json:"role"`

	// Executable flags a script. A flagged script is STORED and never
	// executed (§7 threat 16).
	Executable bool `json:"executable"`

	// Scanned records which scanners actually ran on this file.
	Scanned PackageScanStatus `json:"scanned"`
}

// PackageVersionOverride records HOW the effective version was arrived
// at (§5.2 and §9's `declared_version` / `version_source`).
//
// A package that declares no version is not rewritten: MPM assigns an
// effective version and records the override SEPARATELY, here and in the
// package row — never in the third-party file.
type PackageVersionOverride struct {
	// Declared is the version the package itself declared, or nil when
	// it declared none.
	Declared *string `json:"declared"`

	// Effective is the version the package is known by.
	Effective string `json:"effective"`

	// Reason is why the effective version was chosen. §9 names two
	// values: `declared` and `package_declares_no_version`.
	Reason string `json:"reason"`
}

// PackageVersionSourceDeclared is the version_source value for a
// package whose entrypoint carried a `version:` frontmatter field.
const PackageVersionSourceDeclared = "declared"

// PackageVersionSourceUnversioned is the version_source value for a
// package that declares no version — the pre-release default of §5.2,
// or an operator override (§9).
const PackageVersionSourceUnversioned = "package_declares_no_version"

// PackageManifest is the manifest of a skill package (§5.5).
//
// There is deliberately NO PackageHash field. The hash is OF this
// document, so it cannot be a member of it; StoredJSON adds the key back
// for storage.
type PackageManifest struct {
	// Format is the manifest format identifier.
	Format string `json:"format"`

	// Name is the skill's name. The package is a VERSION of an existing
	// skill — there is no second identity space (§5.1).
	Name string `json:"name"`

	// Version is the effective version.
	Version string `json:"version"`

	// DeclaredVersion is the version the package declared, or nil when
	// it declared none.
	DeclaredVersion *string `json:"declared_version"`

	// VersionOverride records how the effective version was reached.
	VersionOverride PackageVersionOverride `json:"version_override"`

	// Entrypoint is the canonical path of the package's entrypoint.
	Entrypoint string `json:"entrypoint"`

	// EntrypointSHA256 is the SHA-256 of the ORIGINAL entrypoint bytes,
	// as supplied (§5.2).
	EntrypointSHA256 string `json:"entrypoint_sha256"`

	// ProjectionSHA256 is the SHA-256 of the bytes written to
	// `memories.content`. It lives inside the manifest because it is a
	// property of the derived projection, not of the package (§9).
	ProjectionSHA256 string `json:"projection_sha256"`

	// Files are the package's files. Their order in this slice does not
	// matter: the canonical form sorts them by path.
	Files []PackageManifestFile `json:"files"`
}

// PackageManifestReason is a stable, machine-readable refusal code for
// a rejected manifest.
//
// The vocabulary is closed. The codes name the field that failed, in
// lower_snake_case, in the same shape as the path reasons.
type PackageManifestReason string

const (
	// PackageManifestReasonFormat — the `format` field is missing or is
	// not the format this stage understands.
	PackageManifestReasonFormat PackageManifestReason = "manifest_format_invalid"

	// PackageManifestReasonName — the `name` field is not a valid
	// skill name.
	PackageManifestReasonName PackageManifestReason = "manifest_name_invalid"

	// PackageManifestReasonVersion — the `version` field is not a
	// valid semver version.
	PackageManifestReasonVersion PackageManifestReason = "manifest_version_invalid"

	// PackageManifestReasonDeclaredVersion — `declared_version` and
	// `version_override.declared` disagree, or the declared version is
	// not the effective version (§5.2).
	PackageManifestReasonDeclaredVersion PackageManifestReason = "manifest_declared_version_invalid"

	// PackageManifestReasonVersionOverride — the version override record
	// is incomplete or does not agree with the effective version.
	PackageManifestReasonVersionOverride PackageManifestReason = "manifest_version_override_invalid"

	// PackageManifestReasonEntrypoint — the `entrypoint` field is not a
	// canonical path.
	PackageManifestReasonEntrypoint PackageManifestReason = "manifest_entrypoint_invalid"

	// PackageManifestReasonEntrypointMissing — no file carries the
	// entrypoint path.
	PackageManifestReasonEntrypointMissing PackageManifestReason = "manifest_entrypoint_missing"

	// PackageManifestReasonEntrypointAmbiguous — more than one file
	// carries the entrypoint role.
	PackageManifestReasonEntrypointAmbiguous PackageManifestReason = "manifest_entrypoint_ambiguous"

	// PackageManifestReasonEntrypointHash — the entrypoint file's hash
	// does not match `entrypoint_sha256`.
	PackageManifestReasonEntrypointHash PackageManifestReason = "manifest_entrypoint_hash_mismatch"

	// PackageManifestReasonHash — a SHA-256 field is not 64 lowercase
	// hex characters.
	PackageManifestReasonHash PackageManifestReason = "manifest_hash_invalid"

	// PackageManifestReasonFilePath — a file's path is not a canonical
	// path.
	PackageManifestReasonFilePath PackageManifestReason = "manifest_file_path_invalid"

	// PackageManifestReasonInvalidUTF8 — a path in the manifest is not
	// valid UTF-8, so it cannot be represented in a JSON manifest.
	//
	// The code is the SAME string as the path primitive's
	// `invalid_utf8`, so one refusal code names the condition wherever
	// it is caught.
	PackageManifestReasonInvalidUTF8 PackageManifestReason = "invalid_utf8"

	// PackageManifestReasonDuplicatePath — two files carry the same
	// canonical path.
	PackageManifestReasonDuplicatePath PackageManifestReason = "duplicate_path"

	// PackageManifestReasonFileSize — a file's size is negative.
	PackageManifestReasonFileSize PackageManifestReason = "manifest_file_size_invalid"

	// PackageManifestReasonContentType — a file's content type is
	// empty.
	PackageManifestReasonContentType PackageManifestReason = "manifest_file_content_type_invalid"

	// PackageManifestReasonRole — a file's role is not one of the four
	// values §5.5 names.
	PackageManifestReasonRole PackageManifestReason = "manifest_file_role_invalid"

	// PackageManifestReasonHashMismatch — a stored manifest's
	// `package_hash` is not the hash of its canonical form.
	PackageManifestReasonHashMismatch PackageManifestReason = "manifest_hash_mismatch"

	// PackageManifestReasonMalformed — the stored bytes are not a
	// manifest at all.
	PackageManifestReasonMalformed PackageManifestReason = "manifest_malformed"
)

// PackageManifestError is the typed refusal returned by the manifest
// primitives.
//
// Like PackagePathError it is recognised through errors.As, so a caller
// may wrap it and the classification still holds.
type PackageManifestError struct {
	// Reason is the stable machine-readable code.
	Reason PackageManifestReason

	// Field names the offending field. It never carries the offending
	// value.
	Field string

	// Message is the caller-facing text.
	Message string
}

// Error implements the error interface.
func (e *PackageManifestError) Error() string {
	if e == nil {
		return "package manifest rejected"
	}
	if e.Message != "" {
		return e.Message
	}
	if e.Field != "" {
		return fmt.Sprintf("%s: %s", e.Field, e.Reason)
	}
	return string(e.Reason)
}

// OutcomeCode returns the stable outcome_code for this error.
func (e *PackageManifestError) OutcomeCode() string {
	if e == nil || e.Reason == "" {
		return "unclassified"
	}
	return string(e.Reason)
}

// manifestErrorf builds a PackageManifestError with a formatted message.
func manifestErrorf(reason PackageManifestReason, field, format string, args ...any) *PackageManifestError {
	return &PackageManifestError{
		Reason:  reason,
		Field:   field,
		Message: fmt.Sprintf(format, args...),
	}
}

// PackageRoleForPath derives a file's role from the path convention
// (§5.5): `references/` → reference, `assets/` → asset, `scripts/` →
// script, with the entrypoint special-cased.
//
// The convention is read from the FIRST segment, so `assets/img/logo.png`
// is an asset and `scripts/lib/x.sh` is a script.
//
// A file that matches no convention and is not the entrypoint gets
// PackageRoleDefault; see that constant for why.
func PackageRoleForPath(entrypoint, path string) PackageRole {
	if entrypoint != "" && path == entrypoint {
		return PackageRoleEntrypoint
	}
	first := path
	if idx := strings.IndexByte(path, '/'); idx >= 0 {
		first = path[:idx]
	}
	switch first {
	case "references":
		return PackageRoleReference
	case "assets":
		return PackageRoleAsset
	case "scripts":
		return PackageRoleScript
	}
	return PackageRoleDefault
}

// CanonicalBytes returns the canonical JSON encoding of the manifest.
//
// Canonical means, per §5.5:
//
//   - object keys sorted
//   - no insignificant whitespace
//   - UTF-8
//   - `files` sorted by path
//
// The `package_hash` key is NOT present — it is the hash OF this
// document, so it cannot be inside it. See StoredJSON for the stored
// form.
//
// The encoding is stable across Go releases and platforms: the object is
// built as a map (whose keys encoding/json sorts), the file list is
// sorted by path before encoding, and HTML escaping is disabled so that
// `<`, `>` and `&` survive verbatim rather than becoming \u003c, \u003e
// and \u0026 — which is what "UTF-8 canonical representation" requires.
func (m PackageManifest) CanonicalBytes() ([]byte, error) {
	doc, err := m.canonicalDocument()
	if err != nil {
		return nil, err
	}
	return marshalCanonicalJSON(doc)
}

// ComputePackageHash returns the SHA-256 of the canonical manifest
// JSON, as lowercase hex.
//
// The result does not depend on the archive container, the compression
// level, the timestamps or the layout the package arrived in (§5.5). It
// does depend on the scan status, because the scan status is part of the
// manifest.
func (m PackageManifest) ComputePackageHash() (string, error) {
	canonical, err := m.CanonicalBytes()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}

// StoredJSON returns the canonical manifest JSON with the
// `package_hash` key added — the exact bytes to store in
// `skill_packages.manifest` (§9).
//
// Because canonicalisation sorts keys, the result is exactly the
// canonical form of the complete manifest. The hash inside it is the
// hash of the same document WITHOUT that key, which is the only
// non-circular definition available.
func (m PackageManifest) StoredJSON() (string, error) {
	doc, err := m.canonicalDocument()
	if err != nil {
		return "", err
	}
	hash, err := m.ComputePackageHash()
	if err != nil {
		return "", err
	}
	doc["package_hash"] = hash
	encoded, err := marshalCanonicalJSON(doc)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

// canonicalDocument builds the map that is encoded as the canonical
// JSON.
//
// The manifest is marshalled through encoding/json first and decoded
// into a map, because a map's keys are sorted on encode while a struct's
// fields are emitted in DECLARATION order. The decode uses UseNumber so
// that an integer survives the round trip as its exact literal rather
// than as a float64 that might be re-encoded differently.
func (m PackageManifest) canonicalDocument() (map[string]any, error) {
	// The document must not misrepresent the manifest. See
	// checkEncodable for what that means and why it matters here.
	if err := m.checkEncodable(); err != nil {
		return nil, err
	}

	// Sort a COPY of the file list: the caller's manifest is not
	// mutated, and the canonical form is independent of the order the
	// files were supplied in.
	files := make([]PackageManifestFile, len(m.Files))
	copy(files, m.Files)
	sort.SliceStable(files, func(i, j int) bool {
		return files[i].Path < files[j].Path
	})
	sorted := m
	sorted.Files = files

	encoded, err := json.Marshal(sorted)
	if err != nil {
		return nil, fmt.Errorf("package manifest: encode: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	var doc map[string]any
	if err := decoder.Decode(&doc); err != nil {
		return nil, fmt.Errorf("package manifest: decode: %w", err)
	}
	return doc, nil
}

// marshalCanonicalJSON encodes a document as canonical JSON: sorted
// keys, no insignificant whitespace, UTF-8 preserved, and no trailing
// newline.
func marshalCanonicalJSON(doc map[string]any) ([]byte, error) {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	// HTML escaping would rewrite `<`, `>` and `&` as \u003c, \u003e
	// and \u0026. The manifest is data, not markup, and the design
	// requires a UTF-8 canonical representation.
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(doc); err != nil {
		return nil, fmt.Errorf("package manifest: encode canonical json: %w", err)
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// Validate checks the manifest for internal consistency.
//
// It is a pure check: it never touches the database and never scans
// anything. It refuses, among other things, a manifest whose entrypoint
// is not among its files, a manifest with two entrypoints, a manifest
// whose entrypoint hash disagrees with the entrypoint file's hash, and
// a file whose path is not a canonical path.
func (m PackageManifest) Validate() error {
	if m.Format != PackageFormatV1 {
		return manifestErrorf(PackageManifestReasonFormat, "format",
			"format: manifest format %q is not %q", m.Format, PackageFormatV1)
	}

	// The package is a version of an existing skill, so the name and
	// version are validated by the EXISTING identity primitive rather
	// than by a second rule set (§5.1).
	//
	// The semver check runs FIRST so that a bad version is reported as a
	// bad version. Once the version is valid semver it cannot also be
	// empty, colon-bearing or whitespace-bearing, so any remaining
	// identity error belongs to the name.
	if !semver.IsValid("v" + m.Version) {
		return manifestErrorf(PackageManifestReasonVersion, "version",
			"version: %q is not a valid semver version", m.Version)
	}
	if err := validateSkillNameAndVersion(m.Name, m.Version); err != nil {
		return manifestErrorf(PackageManifestReasonName, "name",
			"name: %v", err)
	}

	if err := m.validateVersionOverride(); err != nil {
		return err
	}
	if err := validateSHA256Hex("entrypoint_sha256", m.EntrypointSHA256); err != nil {
		return err
	}
	if err := validateSHA256Hex("projection_sha256", m.ProjectionSHA256); err != nil {
		return err
	}

	// The entrypoint is a canonical path. It is compared against its own
	// canonical form, so a non-canonical spelling is refused rather
	// than quietly repaired.
	canonicalEntrypoint, err := CanonicalPackagePath(m.Entrypoint)
	if err != nil {
		return manifestErrorf(manifestPathReason(err, PackageManifestReasonEntrypoint), "entrypoint",
			"entrypoint: %v", err)
	}
	if canonicalEntrypoint != m.Entrypoint {
		return manifestErrorf(PackageManifestReasonEntrypoint, "entrypoint",
			"entrypoint: %q is not in canonical form", m.Entrypoint)
	}

	seen := make(map[string]struct{}, len(m.Files))
	entrypoints := 0
	for i := range m.Files {
		file := &m.Files[i]

		canonical, err := CanonicalPackagePath(file.Path)
		if err != nil {
			return manifestErrorf(manifestPathReason(err, PackageManifestReasonFilePath), "files",
				"files[%d].path: %v", i, err)
		}
		if canonical != file.Path {
			return manifestErrorf(PackageManifestReasonFilePath, "files",
				"files[%d].path: %q is not in canonical form", i, file.Path)
		}
		if _, dup := seen[file.Path]; dup {
			return manifestErrorf(PackageManifestReasonDuplicatePath, "files",
				"files[%d].path: %q appears more than once", i, file.Path)
		}
		seen[file.Path] = struct{}{}

		if err := validateSHA256Hex("files[].sha256", file.SHA256); err != nil {
			return err
		}
		if file.SizeBytes < 0 {
			return manifestErrorf(PackageManifestReasonFileSize, "files",
				"files[%d].size_bytes: must not be negative", i)
		}
		if file.ContentType == "" {
			return manifestErrorf(PackageManifestReasonContentType, "files",
				"files[%d].content_type: must not be empty", i)
		}
		if !IsValidPackageRole(file.Role) {
			return manifestErrorf(PackageManifestReasonRole, "files",
				"files[%d].role: %q is not a valid role", i, file.Role)
		}

		if file.Path == m.Entrypoint {
			entrypoints++
			if file.Role != PackageRoleEntrypoint {
				return manifestErrorf(PackageManifestReasonRole, "files",
					"files[%d].role: the entrypoint must carry role %q", i, PackageRoleEntrypoint)
			}
			if file.SHA256 != m.EntrypointSHA256 {
				return manifestErrorf(PackageManifestReasonEntrypointHash, "files",
					"files[%d].sha256: does not match entrypoint_sha256", i)
			}
		}
	}

	switch {
	case entrypoints == 0:
		return manifestErrorf(PackageManifestReasonEntrypointMissing, "files",
			"files: no file carries the entrypoint path %q", m.Entrypoint)
	case entrypoints > 1:
		return manifestErrorf(PackageManifestReasonEntrypointAmbiguous, "files",
			"files: %d files carry the entrypoint path %q", entrypoints, m.Entrypoint)
	}
	return nil
}

// validateVersionOverride checks the version override record against the
// manifest's own version fields (§5.2, §9).
func (m PackageManifest) validateVersionOverride() error {
	override := m.VersionOverride
	if override.Effective == "" {
		return manifestErrorf(PackageManifestReasonVersionOverride, "version_override",
			"version_override.effective: must not be empty")
	}
	if override.Effective != m.Version {
		return manifestErrorf(PackageManifestReasonVersionOverride, "version_override",
			"version_override.effective: %q does not match version %q", override.Effective, m.Version)
	}
	switch override.Reason {
	case PackageVersionSourceDeclared, PackageVersionSourceUnversioned:
	case "":
		return manifestErrorf(PackageManifestReasonVersionOverride, "version_override",
			"version_override.reason: must not be empty")
	default:
		return manifestErrorf(PackageManifestReasonVersionOverride, "version_override",
			"version_override.reason: %q is not a known version source", override.Reason)
	}
	if !semver.IsValid("v" + override.Effective) {
		return manifestErrorf(PackageManifestReasonVersionOverride, "version_override",
			"version_override.effective: %q is not a valid semver version", override.Effective)
	}
	// The provenance record and the declared version must agree.
	if err := m.validateVersionProvenance(); err != nil {
		return err
	}

	// The declared version, where the package declared one, must BE the
	// effective version (§5.2: "It must be valid semver and must equal
	// the effective version").
	if m.DeclaredVersion != nil {
		if !semver.IsValid("v" + *m.DeclaredVersion) {
			return manifestErrorf(PackageManifestReasonDeclaredVersion, "declared_version",
				"declared_version: %q is not a valid semver version", *m.DeclaredVersion)
		}
		if *m.DeclaredVersion != m.Version {
			return manifestErrorf(PackageManifestReasonDeclaredVersion, "declared_version",
				"declared_version: %q does not match the effective version %q", *m.DeclaredVersion, m.Version)
		}
	}
	if override.Declared != nil {
		if m.DeclaredVersion == nil {
			return manifestErrorf(PackageManifestReasonDeclaredVersion, "version_override",
				"version_override.declared: set while declared_version is absent")
		}
		if *override.Declared != *m.DeclaredVersion {
			return manifestErrorf(PackageManifestReasonDeclaredVersion, "version_override",
				"version_override.declared: %q does not match declared_version %q", *override.Declared, *m.DeclaredVersion)
		}
	}
	return nil
}

// validateVersionProvenance enforces the equivalence between the
// version SOURCE and the presence of a declared version (§9:
// "A row therefore always says whether its version came from the
// package or from MPM").
//
//   - reason `declared` means the package carried a `version:` field, so
//     a declared version must be present — in the manifest and in the
//     override record, and they must be the same version.
//   - reason `package_declares_no_version` means the package carried no
//     version, so no declared version may be present anywhere.
//
// The two are checked as an equivalence rather than as two independent
// optionalities, because a record that says one thing and carries
// another is a row that lies about its own provenance.
//
// It is deliberately separate from the rest of validateVersionOverride:
// this one invariant is also enforced at the construction boundary (so
// a manifest can never be STORED with a contradictory record) and at the
// parsing boundary (so a stored manifest that contradicts itself is
// refused rather than handed back as though it were sound).
func (m PackageManifest) validateVersionProvenance() error {
	override := m.VersionOverride
	switch override.Reason {
	case PackageVersionSourceDeclared:
		if m.DeclaredVersion == nil {
			return manifestErrorf(PackageManifestReasonDeclaredVersion, "version_override.reason",
				"version_override.reason: %q requires a declared version, but declared_version is absent",
				override.Reason)
		}
		if override.Declared == nil {
			return manifestErrorf(PackageManifestReasonDeclaredVersion, "version_override.declared",
				"version_override.declared: must be present when the version source is %q",
				override.Reason)
		}
	case PackageVersionSourceUnversioned:
		if m.DeclaredVersion != nil {
			return manifestErrorf(PackageManifestReasonDeclaredVersion, "declared_version",
				"declared_version: must be absent when the version source is %q",
				override.Reason)
		}
		if override.Declared != nil {
			return manifestErrorf(PackageManifestReasonDeclaredVersion, "version_override.declared",
				"version_override.declared: must be absent when the version source is %q",
				override.Reason)
		}
	}
	return nil
}

// checkEncodable refuses a manifest whose canonical form would
// MISREPRESENT it.
//
// Two things make a manifest dishonest when it is encoded:
//
//   - a path that is not valid UTF-8. encoding/json REPLACES every
//     invalid byte with U+FFFD, so two distinct malformed paths would
//     encode to the same document and therefore to the same
//     `package_hash`, and the stored manifest would no longer name the
//     path the file row carries. The path primitive already refuses
//     such a name; this is the second line, at the boundary where the
//     document is actually built.
//   - a version provenance record that contradicts itself. A manifest
//     that says its version came from the package while carrying no
//     declared version lies about its own provenance.
//
// The rest of the contract — the path grammar, the digests, the
// entrypoint — is Validate's job. This guard is only about what the
// ENCODING would do to the document, so that a manifest can never be
// hashed or stored while misrepresenting itself.
func (m PackageManifest) checkEncodable() error {
	if !utf8.ValidString(m.Entrypoint) {
		return manifestErrorf(PackageManifestReasonInvalidUTF8, "entrypoint",
			"entrypoint: is not valid UTF-8 and cannot be represented in a manifest")
	}
	for i := range m.Files {
		if !utf8.ValidString(m.Files[i].Path) {
			return manifestErrorf(PackageManifestReasonInvalidUTF8, "files",
				"files[%d].path: is not valid UTF-8 and cannot be represented in a manifest", i)
		}
	}
	return m.validateVersionProvenance()
}

// manifestPathReason maps a path refusal onto the manifest's own
// vocabulary.
//
// A malformed UTF-8 path keeps its own code, because "this path is not
// representable" and "this path is not canonical" are different
// failures with different fixes. Every OTHER refusal keeps the
// caller's own reason, so the manifest's vocabulary is not flattened.
func manifestPathReason(err error, fallback PackageManifestReason) PackageManifestReason {
	if PackagePathReasonOf(err) == PackagePathReasonInvalidUTF8 {
		return PackageManifestReasonInvalidUTF8
	}
	return fallback
}

// validateSHA256Hex checks that a hash field carries 64 lowercase hex
// characters.
func validateSHA256Hex(field, value string) error {
	if len(value) != 64 {
		return manifestErrorf(PackageManifestReasonHash, field,
			"%s: must be 64 hex characters", field)
	}
	for i := 0; i < len(value); i++ {
		c := value[i]
		if (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') {
			continue
		}
		return manifestErrorf(PackageManifestReasonHash, field,
			"%s: must be lowercase hexadecimal", field)
	}
	return nil
}

// packageManifestWire is the stored form of a manifest: the manifest
// itself plus the `package_hash` key.
//
// The embedded struct's fields are promoted, so the wire form has the
// manifest's keys AND `package_hash`.
type packageManifestWire struct {
	PackageManifest
	PackageHash string `json:"package_hash"`
}

// ParsePackageManifest parses a stored manifest.
//
// The parser is strict: an unknown key is refused, because the manifest
// carries its own `format` field and a document that does not match the
// format this stage understands is not silently reinterpreted.
//
// It also refuses a document whose version provenance contradicts
// itself. A manifest that says its version came from the package while
// carrying no declared version is not a manifest — it is a record that
// lies about its own provenance (§9) — so it is refused here rather
// than handed back as though it were sound. The parser's REMAINING
// strictness (trailing bytes after the document, a zero `format`) is a
// separate concern and is not addressed here.
//
// The `package_hash` key is read and returned separately; it is not part
// of the manifest proper, because it is the hash OF the manifest.
func ParsePackageManifest(data []byte) (*PackageManifest, string, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var wire packageManifestWire
	if err := decoder.Decode(&wire); err != nil {
		return nil, "", manifestErrorf(PackageManifestReasonMalformed, "",
			"package manifest: %v", err)
	}
	// A document that contradicts itself about its own version
	// provenance is refused here, not handed back as sound.
	if err := wire.PackageManifest.validateVersionProvenance(); err != nil {
		return nil, "", err
	}
	return &wire.PackageManifest, wire.PackageHash, nil
}

// VerifyPackageHash parses a stored manifest and checks that the
// `package_hash` it carries is the hash of its own canonical form.
//
// It returns the recomputed hash. A mismatch is a refusal with reason
// `manifest_hash_mismatch`: a stored manifest whose hash does not
// describe it is corrupt, and saying so is better than carrying it.
func VerifyPackageHash(stored []byte) (string, error) {
	manifest, claimed, err := ParsePackageManifest(stored)
	if err != nil {
		return "", err
	}
	recomputed, err := manifest.ComputePackageHash()
	if err != nil {
		return "", err
	}
	if claimed == "" {
		return "", manifestErrorf(PackageManifestReasonHashMismatch, "package_hash",
			"package_hash: the stored manifest carries no package_hash")
	}
	if claimed != recomputed {
		return "", manifestErrorf(PackageManifestReasonHashMismatch, "package_hash",
			"package_hash: the stored manifest's hash does not describe its content")
	}
	return recomputed, nil
}
