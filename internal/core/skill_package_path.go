package internal

// skill_package_path.go — Skill Packages, stage P2: canonical relative
// paths, and the collision rules that make a canonical path an identity.
//
// Design of record:
//   docs/dev/skill-package-format-design-2026-11-04.md
//   §5.4 (canonical relative paths), §7 (threats 1, 2, 4, 6, 7, 8),
//   §10 P2 (this stage).
//   Approved: PR #222, comment 6658245185.
//
// This file is PURE. It opens no file, reads no archive, touches no
// database, and writes nothing to a filesystem. A canonical path is a
// STRING with rules; the rules live here and nowhere else.
//
// ─────────────────────────────────────────────────────────────────────────────
// THE LOAD-BEARING INVARIANT
//
// The canonical path is the identity. `raw_path` is display-only.
//
// Two package files are the same file exactly when their canonical paths
// are equal, and nothing else (§5.4). The original name may be preserved
// as display metadata, but it never addresses a file, is never compared
// for identity, and never becomes an authoritative filesystem path.
//
// ─────────────────────────────────────────────────────────────────────────────
// WHY EVERY RULE IS EXPLICIT RATHER THAN HOST-PLATFORM
//
// archive/zip performs NO path sanitisation: `../evil.md` and
// `/etc/passwd` are read verbatim (§7 threat 1). And filepath.IsAbs
// catches `/etc/passwd` only on the host platform — on Linux,
// `C:\Windows\System32\evil.dll` is not absolute (§7 threat 4). A
// Linux-only absolute-path test would let a Windows path through.
//
// So every check below is written against the PATH GRAMMAR, not against
// the running OS. `path/filepath` is deliberately NOT imported here.
//
// The grammar a canonical path must satisfy (§5.4):
//
//   - relative to the package root
//   - `/`-separated, POSIX style, never a filesystem path
//   - no leading `/`
//   - no `.` or `..` segments
//   - no empty segments (`//`)
//   - no backslashes
//   - no drive-letter prefix
//   - no trailing `/`
//   - no NUL, no control character
//   - at most 255 bytes long
//   - at most 8 segments deep
//   - NFC-normalised
//
// ─────────────────────────────────────────────────────────────────────────────
// THE ORDER OF THE CHECKS IS PART OF THE CONTRACT
//
// A single raw name can violate several rules at once — `C:\a\b` is both
// drive-letter-prefixed and backslash-bearing. The reason code a caller
// sees is therefore decided by a fixed order, documented below and pinned
// by the tests:
//
//   1. empty            → empty_path
//   2. NUL              → nul_byte
//   3. control char     → control_character
//   4. UNC prefix       → unc_path
//   5. drive letter     → drive_letter_prefix
//   6. leading `/`      → absolute_path
//   7. backslash        → backslash_separator
//   8. trailing `/`     → trailing_slash
//   9. empty segment    → empty_path_segment
//  10. `.` segment      → dot_segment
//  11. `..` segment     → path_traversal
//  12. too long         → path_too_long
//  13. too deep         → path_too_deep
//
// The order runs from the most specific structural violation to the most
// general, and from the raw name to the canonical form. NFC normalisation
// happens between phases 7 and 8: normalisation cannot introduce or
// remove a `/`, a backslash, a control character or a `.`/`..` segment
// (it maps characters to canonical equivalents, and no ASCII punctuation
// has a canonical decomposition), so the structural checks are identical
// before and after it. It CAN change the byte length, which is why the
// length limit is measured on the normalised form.

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// Package path limits, from §5.4.
const (
	// MaxPackagePathBytes is the maximum length of a canonical path, in
	// bytes. The design says "at most 255 bytes long". The limit is
	// measured on the NFC-normalised form, because that is the form
	// that is stored, compared and exported — so the limit has to
	// describe the form that actually reaches the database.
	MaxPackagePathBytes = 255

	// MaxPackagePathDepth is the maximum number of path segments in a
	// canonical path. The design says "at most 8 segments deep".
	MaxPackagePathDepth = 8
)

// PackagePathReason is a stable, machine-readable refusal code for a
// rejected package path.
//
// The vocabulary is closed. Codes the design names itself —
// `absolute_path` (§7 threat 2), `duplicate_path` (threat 6),
// `path_case_collision` (threat 7), `path_unicode_collision` (threat 8),
// `path_traversal` (threat 1) — are used verbatim. The remaining codes
// name rules the design states but does not give a code for, and follow
// the same lower_snake_case shape.
type PackagePathReason string

const (
	// PackagePathReasonEmpty — the name was empty.
	PackagePathReasonEmpty PackagePathReason = "empty_path"

	// PackagePathReasonNUL — the name contained a NUL byte.
	PackagePathReasonNUL PackagePathReason = "nul_byte"

	// PackagePathReasonControlCharacter — the name contained a control
	// character (C0, DEL or C1).
	PackagePathReasonControlCharacter PackagePathReason = "control_character"

	// PackagePathReasonUNC — the name began with two backslashes, the
	// UNC shape `\\server\share`.
	PackagePathReasonUNC PackagePathReason = "unc_path"

	// PackagePathReasonDriveLetter — the name began with a drive-letter
	// prefix such as `C:`. Checked before the backslash rule so the
	// Windows shape is named as such rather than as a stray separator.
	PackagePathReasonDriveLetter PackagePathReason = "drive_letter_prefix"

	// PackagePathReasonAbsolute — the name began with `/`, making it an
	// absolute POSIX path (§7 threat 2).
	PackagePathReasonAbsolute PackagePathReason = "absolute_path"

	// PackagePathReasonBackslash — the name contained a backslash in
	// any position (§7 threat 4). A canonical path is POSIX-separated;
	// a backslash is never a separator here.
	PackagePathReasonBackslash PackagePathReason = "backslash_separator"

	// PackagePathReasonTrailingSlash — the name ended with `/`.
	PackagePathReasonTrailingSlash PackagePathReason = "trailing_slash"

	// PackagePathReasonEmptySegment — the name contained an empty path
	// segment, i.e. `//`.
	PackagePathReasonEmptySegment PackagePathReason = "empty_path_segment"

	// PackagePathReasonDotSegment — the name contained a `.` segment.
	PackagePathReasonDotSegment PackagePathReason = "dot_segment"

	// PackagePathReasonTraversal — the name contained a `..` segment
	// (§7 threat 1, ZIP slip).
	PackagePathReasonTraversal PackagePathReason = "path_traversal"

	// PackagePathReasonTooLong — the canonical path exceeded
	// MaxPackagePathBytes bytes.
	PackagePathReasonTooLong PackagePathReason = "path_too_long"

	// PackagePathReasonTooDeep — the canonical path exceeded
	// MaxPackagePathDepth segments.
	PackagePathReasonTooDeep PackagePathReason = "path_too_deep"

	// PackagePathReasonDuplicate — two entries carried the SAME raw
	// name, and therefore the same canonical path (§7 threat 6).
	PackagePathReasonDuplicate PackagePathReason = "duplicate_path"

	// PackagePathReasonUnicodeCollision — two entries carried
	// DIFFERENT raw names that normalise to the same NFC form
	// (§7 threat 8).
	PackagePathReasonUnicodeCollision PackagePathReason = "path_unicode_collision"

	// PackagePathReasonCaseCollision — two canonical paths differ only
	// by ASCII case (§7 threat 7). This is what makes an exported
	// package safe on a case-insensitive filesystem.
	PackagePathReasonCaseCollision PackagePathReason = "path_case_collision"
)

// PackagePathError is the typed refusal returned by every path
// primitive in this file.
//
// It is recognised through errors.As, so a caller may WRAP it
// (fmt.Errorf("...: %w", err)) and the classification still holds — the
// same property PayloadShapeError has.
//
// The refused path is carried for the operator's benefit only. It is
// display context: it is never used to address a file, and it is never
// compared for identity.
type PackagePathError struct {
	// Reason is the stable machine-readable code.
	Reason PackagePathReason

	// Path is the offending name exactly as supplied.
	Path string

	// Detail is optional extra context — for a collision, the path that
	// was already registered under the same key.
	Detail string
}

// Error implements the error interface.
func (e *PackagePathError) Error() string {
	if e == nil {
		return "package path rejected"
	}
	if e.Detail != "" {
		return fmt.Sprintf("package path %q rejected: %s (%s)", e.Path, e.Reason, e.Detail)
	}
	return fmt.Sprintf("package path %q rejected: %s", e.Path, e.Reason)
}

// OutcomeCode returns the stable outcome_code for this error. The
// classification wiring into the tool-dispatch boundary lands with the
// import surfaces (P4/P5); P2 has no CLI or MCP surface to classify.
func (e *PackagePathError) OutcomeCode() string {
	if e == nil || e.Reason == "" {
		return "unclassified"
	}
	return string(e.Reason)
}

// PackagePathReasonOf returns the reason carried by err, if it is a
// PackagePathError (or wraps one). It returns the empty reason when err
// is nil or is some other error type.
func PackagePathReasonOf(err error) PackagePathReason {
	if err == nil {
		return ""
	}
	var pe *PackagePathError
	if !errors.As(err, &pe) {
		return ""
	}
	return pe.Reason
}

// CanonicalPackagePath canonicalises one raw package file name.
//
// It returns the canonical form: the NFC-normalised, POSIX-separated,
// relative path that IS the file's identity inside a package.
//
// A refusal is a typed *PackagePathError whose Reason names the rule
// that was broken. The function never returns a partially-canonicalised
// path: on error the returned string is empty.
func CanonicalPackagePath(raw string) (string, error) {
	// ── Phase 1: character-level rules, on the raw name ──────────────
	if raw == "" {
		return "", &PackagePathError{Reason: PackagePathReasonEmpty, Path: raw}
	}
	if strings.IndexByte(raw, 0) >= 0 {
		return "", &PackagePathError{Reason: PackagePathReasonNUL, Path: raw}
	}
	for _, r := range raw {
		if unicode.IsControl(r) {
			return "", &PackagePathError{Reason: PackagePathReasonControlCharacter, Path: raw}
		}
	}
	if strings.HasPrefix(raw, `\\`) {
		return "", &PackagePathError{Reason: PackagePathReasonUNC, Path: raw}
	}
	if hasDriveLetterPrefix(raw) {
		return "", &PackagePathError{Reason: PackagePathReasonDriveLetter, Path: raw}
	}
	if strings.HasPrefix(raw, "/") {
		return "", &PackagePathError{Reason: PackagePathReasonAbsolute, Path: raw}
	}
	if strings.ContainsRune(raw, '\\') {
		return "", &PackagePathError{Reason: PackagePathReasonBackslash, Path: raw}
	}

	// ── Phase 2: normalise to NFC ────────────────────────────────────
	// The normalised form is what is stored and exported (§5.4), so
	// every rule below is stated against it.
	canonical := norm.NFC.String(raw)

	// ── Phase 3: grammar rules, on the canonical form ────────────────
	if strings.HasSuffix(canonical, "/") {
		return "", &PackagePathError{Reason: PackagePathReasonTrailingSlash, Path: raw}
	}
	segments := strings.Split(canonical, "/")
	for _, segment := range segments {
		switch segment {
		case "":
			return "", &PackagePathError{Reason: PackagePathReasonEmptySegment, Path: raw}
		case ".":
			return "", &PackagePathError{Reason: PackagePathReasonDotSegment, Path: raw}
		case "..":
			return "", &PackagePathError{Reason: PackagePathReasonTraversal, Path: raw}
		}
	}
	if len(canonical) > MaxPackagePathBytes {
		return "", &PackagePathError{Reason: PackagePathReasonTooLong, Path: raw}
	}
	if len(segments) > MaxPackagePathDepth {
		return "", &PackagePathError{Reason: PackagePathReasonTooDeep, Path: raw}
	}
	return canonical, nil
}

// hasDriveLetterPrefix reports whether raw begins with a Windows
// drive-letter prefix: a single ASCII letter followed by ':'.
//
// The check is deliberately narrow. A colon elsewhere in a path is not a
// drive letter — `references/a:b.md` is an ordinary name — and the
// design does not forbid colons in package paths. Only the LEADING
// `X:` form is refused (§7 threat 4).
func hasDriveLetterPrefix(raw string) bool {
	if len(raw) < 2 {
		return false
	}
	if raw[1] != ':' {
		return false
	}
	c := raw[0]
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// PackagePathSet accumulates canonical paths and refuses the three
// collisions §5.4 and §7 forbid.
//
// It is the ingest-side guard: an import adds every entry name to a set,
// and the set refuses the second name that would collide with the first.
//
// The three refusals are distinct, and the precedence between them is
// fixed:
//
//	duplicate_path          two entries carried the SAME raw name
//	path_unicode_collision  two entries carried DIFFERENT raw names
//	                        that normalise to the same NFC form
//	path_case_collision     two canonical paths differ only by ASCII
//	                        case
//
// A duplicate is the most specific statement ("this exact name appeared
// twice"), so it wins over the unicode collision that also holds for it.
// The unicode collision is the next most specific ("these two different
// names are the same path after normalisation"), and the case collision
// is the most general ("these two paths would collide on a
// case-insensitive filesystem").
//
// The set is not safe for concurrent use; an import walks its entries
// sequentially.
type PackagePathSet struct {
	// canonical maps a canonical path to the raw name that produced it.
	// The raw name is kept ONLY to tell a duplicate apart from a unicode
	// collision — two names with the same canonical form are one of the
	// two, never both.
	canonical map[string]string

	// folded maps the ASCII-case-folded canonical path to the canonical
	// path that claimed it. ASCII folding touches A-Z only: a non-ASCII
	// letter has no ASCII case, so `café.md` and `CAFÉ.md` are two
	// different paths and are NOT a case collision.
	folded map[string]string
}

// NewPackagePathSet returns an empty set.
func NewPackagePathSet() *PackagePathSet {
	return &PackagePathSet{
		canonical: make(map[string]string),
		folded:    make(map[string]string),
	}
}

// Add canonicalises raw, registers it, and returns its canonical path.
//
// On a refusal the set is left unchanged: a rejected name never
// occupies a slot, so a caller that continues after a refusal cannot be
// misled by a half-registered path.
func (s *PackagePathSet) Add(raw string) (string, error) {
	canonical, err := CanonicalPackagePath(raw)
	if err != nil {
		return "", err
	}

	if existing, ok := s.canonical[canonical]; ok {
		reason := PackagePathReasonUnicodeCollision
		if existing == raw {
			reason = PackagePathReasonDuplicate
		}
		return "", &PackagePathError{
			Reason: reason,
			Path:   raw,
			Detail: canonical,
		}
	}

	folded := asciiFold(canonical)
	if owner, ok := s.folded[folded]; ok {
		return "", &PackagePathError{
			Reason: PackagePathReasonCaseCollision,
			Path:   raw,
			Detail: owner,
		}
	}

	s.canonical[canonical] = raw
	s.folded[folded] = canonical
	return canonical, nil
}

// Paths returns every canonical path in the set, sorted. The order does
// not depend on the order the names were added.
func (s *PackagePathSet) Paths() []string {
	paths := make([]string, 0, len(s.canonical))
	for path := range s.canonical {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths
}

// Len returns the number of canonical paths in the set.
func (s *PackagePathSet) Len() int {
	return len(s.canonical)
}

// RawPath returns the original name that produced the given canonical
// path.
//
// The returned name is DISPLAY-ONLY. It is what an import stage writes
// to `skill_package_files.raw_path`, so the original archive layout
// stays auditable (§5.3.1 rule 6). It never addresses a file, is never
// compared for identity, and is never part of the package hash — so the
// same package imported from an NFD-named archive and from an NFC-named
// archive produces the same rows and the same `package_hash`, and only
// this display value differs (§5.4).
func (s *PackagePathSet) RawPath(canonical string) (string, bool) {
	raw, ok := s.canonical[canonical]
	return raw, ok
}

// CanonicalPackagePaths canonicalises a list of raw names and returns
// their canonical paths, sorted.
//
// It refuses on the first collision it finds, and reports which name
// collided and against what. The returned slice is sorted, so the order
// of the input never reaches the caller.
func CanonicalPackagePaths(raws []string) ([]string, error) {
	set := NewPackagePathSet()
	for _, raw := range raws {
		if _, err := set.Add(raw); err != nil {
			return nil, err
		}
	}
	return set.Paths(), nil
}

// asciiFold lowercases the ASCII letters A-Z and leaves every other
// byte alone.
//
// It is byte-wise, which is safe for UTF-8: every byte of a multi-byte
// sequence has the high bit set, so it can never be mistaken for an
// ASCII letter.
func asciiFold(s string) string {
	b := []byte(s)
	for i := range b {
		if b[i] >= 'A' && b[i] <= 'Z' {
			b[i] += 'a' - 'A'
		}
	}
	return string(b)
}
