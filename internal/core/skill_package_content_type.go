// skill_package_content_type.go — the explicit content-type allow-list and
// the content sniff.
//
// Design of record: PR #222 §5.5 ("`content_type` comes from a small,
// explicit allow-list keyed on extension plus a content sniff — never a
// general MIME guesser") and §7.3 (the scanner coverage table).
//
// The allow-list is CLOSED. A file whose extension is not on it is
// refused, not guessed at: an unknown extension is an unknown content
// type, and an unknown content type has no defined scanner coverage, so
// storing it would mean storing a file whose scan coverage nobody
// specified.

package internal

import (
	"bytes"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// packageContentClass is the class of a package file, and it decides
// which scanners apply (§7.3).
type packageContentClass int

const (
	// packageContentText — prose or structured text. BOTH scanners run.
	packageContentText packageContentClass = iota

	// packageContentBinary — a binary asset. Only the secret scanner
	// runs; the poison scan does NOT apply, and the manifest records
	// that as a DISCLOSED SKIP (`poison: false`), never as a pass.
	packageContentBinary

	// packageContentExecutable — an executable binary (ELF, PE, Mach-O).
	// Rejected by default; the opt-in still only stores bytes (§7
	// threat 16).
	packageContentExecutable
)

// packageContentType is one entry of the allow-list.
type packageContentType struct {
	// contentType is the stored value.
	contentType string

	// class decides the scanner coverage.
	class packageContentClass

	// executable marks a script: the file is STORED and FLAGGED, never
	// executed (§7 threat 16).
	executable bool
}

// packageContentTypes is the explicit allow-list, keyed on the lower-cased
// extension including the dot.
//
// The list is the §7.3 table and nothing more:
//
//   - Markdown, text, JSON and YAML — full secret AND poison scan;
//   - scripts (sh, py, js, rb, pl) — full scan, flagged executable,
//     NEVER executed;
//   - binary assets (png, jpg, jpeg, svg, pdf, woff2) — secret scan
//     only, disclosed poison skip, and OPT-IN only.
//
// An extension that is not here has no defined scanner coverage, so it is
// refused.
var packageContentTypes = map[string]packageContentType{
	".md":    {contentType: "text/markdown; charset=utf-8", class: packageContentText},
	".txt":   {contentType: "text/plain; charset=utf-8", class: packageContentText},
	".json":  {contentType: "application/json", class: packageContentText},
	".yaml":  {contentType: "application/yaml", class: packageContentText},
	".yml":   {contentType: "application/yaml", class: packageContentText},
	".sh":    {contentType: "text/x-shellscript", class: packageContentText, executable: true},
	".py":    {contentType: "text/x-python", class: packageContentText, executable: true},
	".js":    {contentType: "text/javascript", class: packageContentText, executable: true},
	".rb":    {contentType: "text/x-ruby", class: packageContentText, executable: true},
	".pl":    {contentType: "text/x-perl", class: packageContentText, executable: true},
	".png":   {contentType: "image/png", class: packageContentBinary},
	".jpg":   {contentType: "image/jpeg", class: packageContentBinary},
	".jpeg":  {contentType: "image/jpeg", class: packageContentBinary},
	".svg":   {contentType: "image/svg+xml", class: packageContentBinary},
	".pdf":   {contentType: "application/pdf", class: packageContentBinary},
	".woff2": {contentType: "font/woff2", class: packageContentBinary},
}

// packageExecutableMagics are the executable-binary magics the importer
// refuses by default (§7 threat 16).
//
// The list is checked against the file's FIRST bytes, whatever the
// extension claims. A file named `notes.md` whose bytes are an ELF
// header is an executable, and the extension does not make it prose.
var packageExecutableMagics = []struct {
	name  string
	magic []byte
}{
	{name: "ELF", magic: []byte{0x7f, 'E', 'L', 'F'}},
	{name: "PE/DOS", magic: []byte{'M', 'Z'}},
	{name: "Mach-O", magic: []byte{0xcf, 0xfa, 0xed, 0xfe}},
	{name: "Mach-O (32-bit)", magic: []byte{0xce, 0xfa, 0xed, 0xfe}},
	{name: "Mach-O (universal)", magic: []byte{0xca, 0xfe, 0xba, 0xbe}},
}

// packageBinaryMagics are the content magics used to CONFIRM that a file
// whose extension claims a binary type really is that type (§5.5: the
// allow-list is keyed on extension PLUS a content sniff).
//
// A mismatch is a refusal, not a repair: the operator's file is not the
// file its name claims.
var packageBinaryMagics = map[string][]byte{
	".png":   {0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a},
	".jpg":   {0xff, 0xd8, 0xff},
	".jpeg":  {0xff, 0xd8, 0xff},
	".pdf":   {'%', 'P', 'D', 'F'},
	".woff2": {'w', 'O', 'F', '2'},
}

// packageContentTypeFor derives the content type of a file from its
// extension, and classifies it.
//
// It returns the content type, the class and whether the file is a
// script. A refusal is a *PackageImportError with the reason
// `content_type_not_allowed`.
func packageContentTypeFor(canonicalPath string) (string, packageContentClass, bool, error) {
	ext := strings.ToLower(filepath.Ext(canonicalPath))
	entry, ok := packageContentTypes[ext]
	if !ok {
		return "", packageContentText, false, packageImportErrorf(
			PackageImportReasonContentTypeNotAllowed, canonicalPath,
			"the extension %q is not on the package content-type allow-list", ext)
	}
	return entry.contentType, entry.class, entry.executable, nil
}

// packageExecutableMagic returns the name of the executable format the
// bytes begin with, or the empty string when they do not begin with one.
func packageExecutableMagic(content []byte) string {
	for _, m := range packageExecutableMagics {
		if len(content) >= len(m.magic) && bytes.Equal(content[:len(m.magic)], m.magic) {
			return m.name
		}
	}
	return ""
}

// packageSniffContentType checks the file's bytes against the content
// type its extension claims.
//
// The check is deliberately narrow:
//
//   - a binary type is confirmed against its magic bytes, so a `.png`
//     that is not a PNG is refused;
//   - a text type is confirmed to be UTF-8 with no NUL byte, so a file
//     that claims to be prose but carries binary bytes is refused;
//   - an executable magic is detected for EVERY file, whatever its
//     extension, and is refused by default.
//
// A refusal is a *PackageImportError with the reason
// `content_type_mismatch`.
func packageSniffContentType(canonicalPath, contentType string, class packageContentClass, content []byte) error {
	if magic := packageExecutableMagic(content); magic != "" {
		return packageImportErrorf(PackageImportReasonContentTypeMismatch, canonicalPath,
			"the content is a %s executable binary, not %q", magic, contentType)
	}
	switch class {
	case packageContentBinary:
		ext := strings.ToLower(filepath.Ext(canonicalPath))
		magic, ok := packageBinaryMagics[ext]
		if !ok {
			// The binary type has no magic to check against (SVG is
			// XML, not a magic-number format). The extension is the
			// whole of the claim, which is recorded as such.
			return nil
		}
		if len(content) < len(magic) || !bytes.Equal(content[:len(magic)], magic) {
			return packageImportErrorf(PackageImportReasonContentTypeMismatch, canonicalPath,
				"the content does not carry the %s magic its extension claims", ext)
		}
	case packageContentText:
		if !isValidUTF8Content(content) {
			return packageImportErrorf(PackageImportReasonInvalidUTF8, canonicalPath,
				"the content is not valid UTF-8 text")
		}
	}
	return nil
}

// isValidUTF8Content reports whether the bytes are valid UTF-8 with no
// NUL byte.
//
// The NUL check is what separates "text" from "binary that happens to
// decode": a UTF-8 string may contain a NUL, but a file that carries one
// is not prose, and the poison scanner's phrase matching over it would
// be meaningless.
func isValidUTF8Content(content []byte) bool {
	if bytes.IndexByte(content, 0) >= 0 {
		return false
	}
	return utf8.Valid(content)
}
