package internal

// skill_package_path_test.go — Skill Packages, stage P2: canonical
// relative paths, and the collision rules that make a canonical path an
// identity.
//
// Design of record:
//   docs/dev/skill-package-format-design-2026-11-04.md
//   §5.4 (canonical relative paths), §7 (threats 1, 2, 4, 6, 7, 8),
//   §10 P2 (this stage).
//   Approved: PR #222, comment 6658245185.
//
// These tests are DISPOSABLE and PURE: they touch no database, no
// archive and no filesystem path. The live ~/.mpm database is never
// opened.
//
// The shape of the suite is a CONTRACT, not an implementation detail.
// Each table pins the reason code a caller sees, because the reason
// code is what a later stage turns into a refusal message and an audit
// record. A change to a reason code is a change to the contract.

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// nfdCafe is "references/café.md" with the accent in NFD: "e" followed
// by U+0301 COMBINING ACUTE ACCENT. It is 20 bytes.
const nfdCafe = "references/cafe\u0301.md"

// nfcCafe is "references/café.md" in NFC: U+00E9 LATIN SMALL LETTER
// E WITH ACUTE. It is 19 bytes.
const nfcCafe = "references/caf\u00e9.md"

// ─────────────────────────────────────────────────────────────────────────────
// valid canonical paths
// ─────────────────────────────────────────────────────────────────────────────

// TestCanonicalPackagePathAcceptsValidPaths pins the accepting half of
// the grammar: a canonical path is unchanged by canonicalisation.
func TestCanonicalPackagePathAcceptsValidPaths(t *testing.T) {
	valid := []struct {
		name string
		in   string
		want string
	}{
		{"entrypoint", "SKILL.md", "SKILL.md"},
		{"reference", "references/principles.md", "references/principles.md"},
		{"script", "scripts/run.sh", "scripts/run.sh"},
		{"asset", "assets/logo.png", "assets/logo.png"},
		{"nested", "a/b/c/d.md", "a/b/c/d.md"},
		{"max depth", "a/b/c/d/e/f/g/h", "a/b/c/d/e/f/g/h"},
		{"spaces are not forbidden", "my file.md", "my file.md"},
		{"a colon is not a drive letter unless leading", "refs/a:b.md", "refs/a:b.md"},
		{"a dot in a name is not a dot segment", "..hidden.md", "..hidden.md"},
		{"a trailing dot is not a dot segment", "a.", "a."},
		{"unicode content", "文档/说明.md", "文档/说明.md"},
		{"nfd input normalises to nfc", nfdCafe, nfcCafe},
		{"nfc input is unchanged", nfcCafe, nfcCafe},
	}
	for _, tc := range valid {
		t.Run(tc.name, func(t *testing.T) {
			got, err := CanonicalPackagePath(tc.in)
			if err != nil {
				t.Fatalf("CanonicalPackagePath(%q) returned an error: %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("CanonicalPackagePath(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestCanonicalPackagePathIsIdempotent pins that a canonical path is a
// fixed point: canonicalising a canonical path changes nothing. This is
// what lets a stored path be re-validated without being rewritten.
func TestCanonicalPackagePathIsIdempotent(t *testing.T) {
	for _, in := range []string{"SKILL.md", "references/principles.md", nfcCafe, "a/b/c/d/e/f/g/h"} {
		once, err := CanonicalPackagePath(in)
		if err != nil {
			t.Fatalf("CanonicalPackagePath(%q): %v", in, err)
		}
		twice, err := CanonicalPackagePath(once)
		if err != nil {
			t.Fatalf("CanonicalPackagePath(%q) of a canonical path: %v", once, err)
		}
		if twice != once {
			t.Errorf("canonicalisation is not idempotent for %q: %q then %q", in, once, twice)
		}
	}
}

// TestCanonicalPackagePathAcceptsExactlyTheLengthLimit pins the byte
// limit at its boundary: 255 bytes is accepted, 256 is not.
//
// The limit is measured on the NFC form, because that is the form that
// is stored and compared.
func TestCanonicalPackagePathAcceptsExactlyTheLengthLimit(t *testing.T) {
	atLimit := strings.Repeat("a", MaxPackagePathBytes)
	got, err := CanonicalPackagePath(atLimit)
	if err != nil {
		t.Fatalf("a %d-byte path was refused: %v", MaxPackagePathBytes, err)
	}
	if got != atLimit {
		t.Errorf("the %d-byte path was not returned unchanged", MaxPackagePathBytes)
	}

	overLimit := strings.Repeat("a", MaxPackagePathBytes+1)
	_, err = CanonicalPackagePath(overLimit)
	assertPathReason(t, err, PackagePathReasonTooLong)
}

// TestCanonicalPackagePathAcceptsExactlyTheDepthLimit pins the depth
// limit at its boundary: 8 segments are accepted, 9 are not.
func TestCanonicalPackagePathAcceptsExactlyTheDepthLimit(t *testing.T) {
	atLimit := "a/b/c/d/e/f/g/h"
	got, err := CanonicalPackagePath(atLimit)
	if err != nil {
		t.Fatalf("an %d-segment path was refused: %v", MaxPackagePathDepth, err)
	}
	if got != atLimit {
		t.Errorf("the %d-segment path was not returned unchanged", MaxPackagePathDepth)
	}

	overLimit := "a/b/c/d/e/f/g/h/i"
	_, err = CanonicalPackagePath(overLimit)
	assertPathReason(t, err, PackagePathReasonTooDeep)
}

// ─────────────────────────────────────────────────────────────────────────────
// every documented rejection
// ─────────────────────────────────────────────────────────────────────────────

// TestCanonicalPackagePathRejectsUnsafePaths pins EVERY rejection rule
// in §7 threats 1, 2 and 4 and in §5.4, with the reason code each one
// carries.
//
// This table is the contract for the refusal vocabulary. A rule that is
// removed, renamed or silently allowed through breaks this test.
func TestCanonicalPackagePathRejectsUnsafePaths(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want PackagePathReason
	}{
		// §5.4: "free of a leading /"
		{"absolute posix", "/etc/passwd", PackagePathReasonAbsolute},
		{"absolute posix with a nested traversal", "/../etc/passwd", PackagePathReasonAbsolute},
		{"rooted at a double slash", "//etc/passwd", PackagePathReasonAbsolute},

		// §7 threat 4: drive letters and separators, rejected by an
		// EXPLICIT check and not by filepath.IsAbs — on Linux
		// `C:\Windows\...` is not an absolute path.
		{"drive letter with backslashes", `C:\Windows\System32\evil.dll`, PackagePathReasonDriveLetter},
		{"drive letter with slashes", "C:/Windows/evil.dll", PackagePathReasonDriveLetter},
		{"lowercase drive letter", "d:/data/evil.dll", PackagePathReasonDriveLetter},
		{"bare drive letter", "C:", PackagePathReasonDriveLetter},
		{"unc path", `\\server\share\evil.dll`, PackagePathReasonUNC},
		{"backslash anywhere", "refs\\a.md", PackagePathReasonBackslash},
		{"backslash at the end", "refs\\", PackagePathReasonBackslash},

		// §7 threat 1: ZIP slip / path traversal.
		{"parent segment", "../evil.md", PackagePathReasonTraversal},
		{"parent segment in the middle", "a/../../evil.md", PackagePathReasonTraversal},
		{"parent segment at the end", "a/..", PackagePathReasonTraversal},
		{"dot segment", "./evil.md", PackagePathReasonDotSegment},
		{"dot segment in the middle", "a/./b.md", PackagePathReasonDotSegment},
		{"dot segment at the end", "a/.", PackagePathReasonDotSegment},

		// §5.4: no empty segments, no trailing slash.
		{"empty segment", "a//b.md", PackagePathReasonEmptySegment},
		{"leading empty segment", "//b.md", PackagePathReasonAbsolute},
		{"trailing slash", "refs/", PackagePathReasonTrailingSlash},
		// The order of the checks is part of the contract: a trailing
		// slash is reported before the segment rules are reached, so a
		// path that is BOTH is reported as the trailing slash. It is
		// refused either way.
		{"trailing slash before a traversal", "../refs/", PackagePathReasonTrailingSlash},
		{"traversal without a trailing slash", "../refs", PackagePathReasonTraversal},

		// §7 threat 8: "Control characters and NUL are rejected."
		{"nul byte", "a\x00b.md", PackagePathReasonNUL},
		{"leading nul byte", "\x00a.md", PackagePathReasonNUL},
		{"bell", "a\x07b.md", PackagePathReasonControlCharacter},
		{"tab", "a\tb.md", PackagePathReasonControlCharacter},
		{"newline", "a\nb.md", PackagePathReasonControlCharacter},
		{"carriage return", "a\rb.md", PackagePathReasonControlCharacter},
		{"delete", "a\x7fb.md", PackagePathReasonControlCharacter},

		// §5.4: "at most 255 bytes long, at most 8 segments deep".
		{"too long", strings.Repeat("a", MaxPackagePathBytes+1), PackagePathReasonTooLong},
		{"too deep", "a/b/c/d/e/f/g/h/i", PackagePathReasonTooDeep},

		// The empty name.
		{"empty", "", PackagePathReasonEmpty},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := CanonicalPackagePath(tc.in)
			if err == nil {
				t.Fatalf("CanonicalPackagePath(%q) accepted the path and returned %q", tc.in, got)
			}
			if got != "" {
				t.Errorf("CanonicalPackagePath(%q) returned %q alongside an error; a refusal must return an empty string", tc.in, got)
			}
			assertPathReason(t, err, tc.want)
		})
	}
}

// TestCanonicalPackagePathDoesNotDependOnTheHostPlatform pins the
// property the design calls out in §7 threat 4: the absolute-path test
// is a PATH GRAMMAR rule, not a filepath.IsAbs call.
//
// `filepath.IsAbs` is false for `C:\Windows\...` on Linux, so an
// implementation that leaned on it would let a Windows path through on
// the CI runner while refusing it on a developer machine. The same
// applies to a UNC path.
func TestCanonicalPackagePathDoesNotDependOnTheHostPlatform(t *testing.T) {
	hostSpecific := []string{
		`C:\Windows\System32\evil.dll`,
		`\\server\share\evil.dll`,
		`C:evil.dll`,
		`\Windows\evil.dll`,
	}
	for _, in := range hostSpecific {
		if _, err := CanonicalPackagePath(in); err == nil {
			t.Errorf("CanonicalPackagePath(%q) accepted a Windows-shaped path", in)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Unicode normalisation
// ─────────────────────────────────────────────────────────────────────────────

// TestCanonicalPackagePathNormalisesToNFC pins the normalisation
// contract: the stored form is NFC, and the NFD and NFC spellings of
// the same name canonicalise to the SAME path.
//
// The byte counts are pinned because they are the whole point of §7
// threat 8: the two spellings are distinct Go strings, and no amount
// of lowercasing would unify them.
func TestCanonicalPackagePathNormalisesToNFC(t *testing.T) {
	if len(nfdCafe) != 20 || len(nfcCafe) != 19 {
		t.Fatalf("the fixtures changed: NFD is %d bytes, NFC is %d bytes; the test pins 20 and 19", len(nfdCafe), len(nfcCafe))
	}
	if nfdCafe == nfcCafe {
		t.Fatal("the NFD and NFC fixtures are the same string")
	}

	fromNFD, err := CanonicalPackagePath(nfdCafe)
	if err != nil {
		t.Fatalf("CanonicalPackagePath(NFD): %v", err)
	}
	fromNFC, err := CanonicalPackagePath(nfcCafe)
	if err != nil {
		t.Fatalf("CanonicalPackagePath(NFC): %v", err)
	}
	if fromNFD != nfcCafe {
		t.Errorf("the NFD spelling canonicalised to %q, want the NFC form %q", fromNFD, nfcCafe)
	}
	if fromNFD != fromNFC {
		t.Errorf("the NFD and NFC spellings canonicalised differently: %q and %q", fromNFD, fromNFC)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// the three collisions
// ─────────────────────────────────────────────────────────────────────────────

// TestPackagePathSetRejectsDuplicatePath pins §7 threat 6: two entries
// with the same canonical path are refused as `duplicate_path`.
func TestPackagePathSetRejectsDuplicatePath(t *testing.T) {
	set := NewPackagePathSet()
	if _, err := set.Add("references/principles.md"); err != nil {
		t.Fatalf("the first Add was refused: %v", err)
	}
	_, err := set.Add("references/principles.md")
	assertPathReason(t, err, PackagePathReasonDuplicate)
	if set.Len() != 1 {
		t.Errorf("the refused path was registered anyway: the set holds %d paths, want 1", set.Len())
	}
}

// TestPackagePathSetRejectsUnicodeCollision pins §7 threat 8: two
// DIFFERENT names that normalise to the same path are refused as
// `path_unicode_collision` — NOT as a duplicate.
//
// The distinction is load-bearing: a duplicate says "the same name
// twice"; a unicode collision says "two different names are the same
// path once normalised", which is the condition the normalisation
// itself creates.
func TestPackagePathSetRejectsUnicodeCollision(t *testing.T) {
	set := NewPackagePathSet()
	if _, err := set.Add(nfdCafe); err != nil {
		t.Fatalf("the first Add was refused: %v", err)
	}
	_, err := set.Add(nfcCafe)
	assertPathReason(t, err, PackagePathReasonUnicodeCollision)
	if set.Len() != 1 {
		t.Errorf("the refused path was registered anyway: the set holds %d paths, want 1", set.Len())
	}
}

// TestPackagePathSetRejectsCaseCollision pins §7 threat 7: two
// canonical paths differing only by ASCII case are refused as
// `path_case_collision`.
//
// This is the rule that makes an exported package safe on a
// case-insensitive filesystem.
func TestPackagePathSetRejectsCaseCollision(t *testing.T) {
	set := NewPackagePathSet()
	if _, err := set.Add("references/Principles.md"); err != nil {
		t.Fatalf("the first Add was refused: %v", err)
	}
	_, err := set.Add("references/principles.md")
	assertPathReason(t, err, PackagePathReasonCaseCollision)
	if set.Len() != 1 {
		t.Errorf("the refused path was registered anyway: the set holds %d paths, want 1", set.Len())
	}
}

// TestPackagePathSetFoldsOnlyASCII pins the scope of the case rule: the
// design says "differing only by ASCII case", and a non-ASCII letter
// has no ASCII case.
//
// `café.md` and `CAFÉ.md` are two DIFFERENT paths and must both be
// accepted. Lowercasing the whole string would have refused them.
func TestPackagePathSetFoldsOnlyASCII(t *testing.T) {
	set := NewPackagePathSet()
	if _, err := set.Add("references/caf\u00e9.md"); err != nil {
		t.Fatalf("the first Add was refused: %v", err)
	}
	if _, err := set.Add("references/CAF\u00c9.md"); err != nil {
		t.Fatalf("a non-ASCII case difference was refused: %v", err)
	}
	if set.Len() != 2 {
		t.Errorf("the set holds %d paths, want 2", set.Len())
	}
}

// TestPackagePathSetCollisionPrecedence pins the order the three
// refusals are decided in.
//
// The same pair of names can satisfy more than one rule at once. The
// precedence is: a duplicate (the same raw name twice) is the most
// specific statement, a unicode collision is next, and a case
// collision is the most general.
func TestPackagePathSetCollisionPrecedence(t *testing.T) {
	// The same raw name twice is a duplicate, even though the two
	// entries also share a canonical path.
	set := NewPackagePathSet()
	if _, err := set.Add("a.md"); err != nil {
		t.Fatalf("the first Add was refused: %v", err)
	}
	_, err := set.Add("a.md")
	assertPathReason(t, err, PackagePathReasonDuplicate)

	// Two different names with the same NFC form are a unicode
	// collision, not a duplicate and not a case collision.
	set = NewPackagePathSet()
	if _, err := set.Add(nfdCafe); err != nil {
		t.Fatalf("the first Add was refused: %v", err)
	}
	_, err = set.Add(nfcCafe)
	assertPathReason(t, err, PackagePathReasonUnicodeCollision)

	// Two names that differ only by ASCII case are a case collision.
	set = NewPackagePathSet()
	if _, err := set.Add("a.md"); err != nil {
		t.Fatalf("the first Add was refused: %v", err)
	}
	_, err = set.Add("A.md")
	assertPathReason(t, err, PackagePathReasonCaseCollision)
}

// TestPackagePathSetAcceptsDistinctPaths pins that a set of ordinary
// distinct paths is accepted and that the set's order does not depend
// on the order the names were added.
func TestPackagePathSetAcceptsDistinctPaths(t *testing.T) {
	names := []string{
		"SKILL.md",
		"references/principles.md",
		"references/templates.md",
		"scripts/run.sh",
		"assets/logo.png",
	}
	set := NewPackagePathSet()
	for _, name := range names {
		if _, err := set.Add(name); err != nil {
			t.Fatalf("Add(%q) was refused: %v", name, err)
		}
	}
	if set.Len() != len(names) {
		t.Fatalf("the set holds %d paths, want %d", set.Len(), len(names))
	}

	paths := set.Paths()
	want := []string{
		"SKILL.md",
		"assets/logo.png",
		"references/principles.md",
		"references/templates.md",
		"scripts/run.sh",
	}
	if len(paths) != len(want) {
		t.Fatalf("Paths() returned %d paths, want %d", len(paths), len(want))
	}
	for i := range want {
		if paths[i] != want[i] {
			t.Errorf("Paths()[%d] = %q, want %q", i, paths[i], want[i])
		}
	}
}

// TestPackagePathSetKeepsTheOriginalNameAsDisplayOnly pins that the
// original name survives — as display metadata — while the canonical
// path remains the identity.
//
// §5.4: the canonical path is the identity; `raw_path` is a display
// artefact. The original name is kept so an import can record it in
// `skill_package_files.raw_path`, and it is kept under the CANONICAL
// path, so the identity and the display value cannot be confused.
func TestPackagePathSetKeepsTheOriginalNameAsDisplayOnly(t *testing.T) {
	set := NewPackagePathSet()
	canonical, err := set.Add(nfdCafe)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if canonical != nfcCafe {
		t.Fatalf("the canonical path is %q, want %q", canonical, nfcCafe)
	}

	raw, ok := set.RawPath(canonical)
	if !ok {
		t.Fatal("the set does not know the canonical path it produced")
	}
	if raw != nfdCafe {
		t.Errorf("the display name is %q, want the original %q", raw, nfdCafe)
	}

	// The display name is looked up BY the canonical path, so it can
	// never be used as an address.
	if _, ok := set.RawPath(nfdCafe); ok {
		t.Error("the set resolved the ORIGINAL name; the display value must not be an address")
	}
}

// TestPackagePathSetIsOrderIndependent pins that adding the same names
// in a different order produces the same set.
func TestPackagePathSetIsOrderIndependent(t *testing.T) {
	names := []string{"SKILL.md", "references/principles.md", "scripts/run.sh"}
	forward := NewPackagePathSet()
	for _, name := range names {
		if _, err := forward.Add(name); err != nil {
			t.Fatalf("Add(%q): %v", name, err)
		}
	}
	backward := NewPackagePathSet()
	for i := len(names) - 1; i >= 0; i-- {
		if _, err := backward.Add(names[i]); err != nil {
			t.Fatalf("Add(%q): %v", names[i], err)
		}
	}
	if strings.Join(forward.Paths(), "|") != strings.Join(backward.Paths(), "|") {
		t.Errorf("the set depends on the insertion order: %v vs %v", forward.Paths(), backward.Paths())
	}
}

// TestCanonicalPackagePathsRejectsTheFirstCollision pins the
// convenience wrapper: it refuses on the first collision and returns
// nothing.
func TestCanonicalPackagePathsRejectsTheFirstCollision(t *testing.T) {
	paths, err := CanonicalPackagePaths([]string{"SKILL.md", "references/a.md"})
	if err != nil {
		t.Fatalf("distinct names were refused: %v", err)
	}
	if len(paths) != 2 {
		t.Fatalf("got %d paths, want 2", len(paths))
	}

	paths, err = CanonicalPackagePaths([]string{"SKILL.md", "SKILL.md"})
	if err == nil {
		t.Fatalf("a duplicate was accepted: %v", paths)
	}
	assertPathReason(t, err, PackagePathReasonDuplicate)
	if paths != nil {
		t.Errorf("a refusal returned %v alongside the error", paths)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// the typed refusal
// ─────────────────────────────────────────────────────────────────────────────

// TestPackagePathErrorSurvivesWrapping pins that the reason code is
// recoverable through fmt.Errorf("%w"), the same property
// PayloadShapeError has.
func TestPackagePathErrorSurvivesWrapping(t *testing.T) {
	_, err := CanonicalPackagePath("../evil.md")
	if err == nil {
		t.Fatal("the traversal path was accepted")
	}
	wrapped := fmt.Errorf("import: %w", err)
	if got := PackagePathReasonOf(wrapped); got != PackagePathReasonTraversal {
		t.Errorf("PackagePathReasonOf(wrapped) = %q, want %q", got, PackagePathReasonTraversal)
	}
	var typed *PackagePathError
	if !errors.As(wrapped, &typed) {
		t.Fatal("errors.As did not find the typed error through the wrapper")
	}
	if typed.Reason != PackagePathReasonTraversal {
		t.Errorf("the typed reason is %q, want %q", typed.Reason, PackagePathReasonTraversal)
	}
	if typed.OutcomeCode() != string(PackagePathReasonTraversal) {
		t.Errorf("OutcomeCode() = %q, want %q", typed.OutcomeCode(), PackagePathReasonTraversal)
	}
}

// TestPackagePathReasonOfIgnoresOtherErrors pins that the helper does
// not misclassify an unrelated error.
func TestPackagePathReasonOfIgnoresOtherErrors(t *testing.T) {
	if got := PackagePathReasonOf(errors.New("unrelated")); got != "" {
		t.Errorf("PackagePathReasonOf(unrelated) = %q, want the empty reason", got)
	}
	if got := PackagePathReasonOf(nil); got != "" {
		t.Errorf("PackagePathReasonOf(nil) = %q, want the empty reason", got)
	}
}

// TestPackagePathErrorCarriesNoFilesystemPath pins the boundary the
// design draws: a refused path is context, never an address.
//
// The error names the offending name so the operator can see what was
// rejected. It does not, and must not, resolve it against a root or
// hand it to a filesystem call.
func TestPackagePathErrorCarriesNoFilesystemPath(t *testing.T) {
	_, err := CanonicalPackagePath("/etc/passwd")
	if err == nil {
		t.Fatal("the absolute path was accepted")
	}
	typed := &PackagePathError{}
	if !errors.As(err, &typed) {
		t.Fatal("errors.As did not find the typed error")
	}
	if typed.Path != "/etc/passwd" {
		t.Errorf("the error's Path is %q, want the rejected name verbatim", typed.Path)
	}
	if strings.Contains(typed.Error(), "/etc/passwd") == false {
		t.Errorf("the error text does not name the rejected path: %q", typed.Error())
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// helper
// ─────────────────────────────────────────────────────────────────────────────

// assertPathReason fails the test unless err carries the expected
// reason code.
func assertPathReason(t *testing.T, err error, want PackagePathReason) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected the refusal %q, but the call succeeded", want)
	}
	got := PackagePathReasonOf(err)
	if got != want {
		t.Fatalf("the refusal reason is %q, want %q (error: %v)", got, want, err)
	}
}
