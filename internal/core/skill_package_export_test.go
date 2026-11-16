// skill_package_export_test.go — Stage P7: the deterministic `.skill`
// export writer.
//
// Design of record: PR #222 §5.6, §6.3, §10 and §13 (T07, T11, T44).
//
// EVERY test here owns its workspace: the fixtures pin MPM_WORKSPACE to
// a disposable directory before the manager is constructed, so no test
// can reach the live database, the live workspace or the operator's
// checkout.
//
// The acceptance rows this file pins:
//
//	T07 — export → import preserves `package_hash` and every per-file
//	       hash, and two exports are byte-identical;
//	T11 — export from one workspace and import into another preserves
//	       the package identity.
//
// plus the stage's own coverage: the flat layout, the original bytes
// rather than the derived projection, the versionless package, scripts,
// binary assets, empty files, shared blobs, differently-ordered source
// archives, a corrupted blob, a missing file row, a corrupted manifest,
// an unsafe file name, and the refusal to mutate anything.

package internal

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"
)

// ─── Fixtures ──────────────────────────────────────────────────────────────

// p7DM opens a database in a FRESH disposable workspace.
//
// The workspace root is passed EXPLICITLY: `NewDatabaseManager("")`
// falls through to the ambient workspace, and the substrate's DB-safety
// guard refuses a test that opens a database that way.
func p7DM(t *testing.T) *DatabaseManager {
	t.Helper()
	ws := t.TempDir()
	t.Setenv("MPM_WORKSPACE", ws)
	t.Setenv("MPM_SHARED_DB", "")
	dm, err := NewDatabaseManager(ws)
	if err != nil {
		t.Fatalf("NewDatabaseManager: %v", err)
	}
	t.Cleanup(func() { dm.Close() })
	return dm
}

// p7Tree writes a package tree into a fresh temporary directory.
func p7Tree(t *testing.T, files map[string][]byte) string {
	t.Helper()
	root := t.TempDir()
	for rel, content := range files {
		full := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", full, err)
		}
		if err := os.WriteFile(full, content, 0o600); err != nil {
			t.Fatalf("write %s: %v", full, err)
		}
	}
	return root
}

// p7SkillDoc builds an entrypoint document.
func p7SkillDoc(name, version, body string) string {
	fm := "---\nname: " + name + "\ndescription: a test skill\nwhen_to_use: when a test needs this exact skill\n"
	if version != "" {
		fm += "version: " + version + "\n"
	}
	return fm + "---\n" + body
}

// p7Import imports a package from a directory tree.
func p7Import(t *testing.T, dm *DatabaseManager, name, version string, files map[string][]byte) *PackageImportResult {
	t.Helper()
	return p7ImportOpts(t, dm, PackageImportOptions{}, name, version, files)
}

// p7TreeOrdered writes a package tree, creating the files in the given
// ORDER.
//
// `p7Tree` iterates a Go map, so the order the files are created in is
// randomised — which is fine for a test about CONTENT and wrong for a
// test about ORDER. A test that says "the source arrived in this
// order" has to mean it, so this helper takes the order explicitly.
func p7TreeOrdered(t *testing.T, files map[string][]byte, order []string) string {
	t.Helper()
	root := t.TempDir()
	for _, rel := range order {
		full := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", full, err)
		}
		content, ok := files[rel]
		if !ok {
			t.Fatalf("the order names %q, which the fixture does not hold", rel)
		}
		if err := os.WriteFile(full, content, 0o600); err != nil {
			t.Fatalf("write %s: %v", full, err)
		}
	}
	return root
}

// p7ImportOpts is p7Import with operator options.
func p7ImportOpts(t *testing.T, dm *DatabaseManager, opts PackageImportOptions, name, version string, files map[string][]byte) *PackageImportResult {
	t.Helper()
	tree := map[string][]byte{}
	for k, v := range files {
		tree[k] = v
	}
	tree["SKILL.md"] = []byte(p7SkillDoc(name, version, "the original entrypoint body\n"))
	opts.Path = p7Tree(t, tree)
	result, err := dm.ImportSkillPackageFromDir(opts)
	if err != nil {
		t.Fatalf("import the package: %v", err)
	}
	return result
}

// p7ImportOrdered is p7ImportOpts with the SOURCE files created in a
// named order.
//
// The entrypoint is created FIRST, so the two trees differ only in the
// order of the supporting files — which is the difference the test is
// about.
func p7ImportOrdered(t *testing.T, dm *DatabaseManager, opts PackageImportOptions, name, version string, files map[string][]byte, order []string) *PackageImportResult {
	t.Helper()
	tree := map[string][]byte{}
	for k, v := range files {
		tree[k] = v
	}
	tree["SKILL.md"] = []byte(p7SkillDoc(name, version, "the original entrypoint body\n"))
	full := append([]string{"SKILL.md"}, order...)
	opts.Path = p7TreeOrdered(t, tree, full)
	result, err := dm.ImportSkillPackageFromDir(opts)
	if err != nil {
		t.Fatalf("import the package: %v", err)
	}
	return result
}

// p7Export exports a package and returns the archive.
func p7Export(t *testing.T, dm *DatabaseManager, skillID string) *PackageExportArchive {
	t.Helper()
	archive, err := dm.ExportSkillPackage(context.Background(), PackageSelector{SkillID: skillID})
	if err != nil {
		t.Fatalf("export the package %s: %v", skillID, err)
	}
	return archive
}

// p7ArchiveEntries lists an archive's entry names, in the order the
// archive carries them.
func p7ArchiveEntries(t *testing.T, content []byte) []string {
	t.Helper()
	reader, err := zip.NewReader(bytes.NewReader(content), int64(len(content)))
	if err != nil {
		t.Fatalf("read the archive: %v", err)
	}
	names := make([]string, 0, len(reader.File))
	for _, file := range reader.File {
		names = append(names, file.Name)
	}
	return names
}

// p7ArchiveEntry reads one entry's bytes.
func p7ArchiveEntry(t *testing.T, content []byte, name string) []byte {
	t.Helper()
	reader, err := zip.NewReader(bytes.NewReader(content), int64(len(content)))
	if err != nil {
		t.Fatalf("read the archive: %v", err)
	}
	for _, file := range reader.File {
		if file.Name != name {
			continue
		}
		rc, err := file.Open()
		if err != nil {
			t.Fatalf("open the archive entry %q: %v", name, err)
		}
		defer rc.Close()
		var buf bytes.Buffer
		if _, err := buf.ReadFrom(rc); err != nil {
			t.Fatalf("read the archive entry %q: %v", name, err)
		}
		return buf.Bytes()
	}
	t.Fatalf("the archive has no entry %q; it carries %v", name, p7ArchiveEntries(t, content))
	return nil
}

// p7WriteArchive writes an archive to a temporary file and returns its
// path.
func p7WriteArchive(t *testing.T, archive *PackageExportArchive) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), archive.FileName)
	if err := os.WriteFile(path, archive.Content, 0o600); err != nil {
		t.Fatalf("write the archive: %v", err)
	}
	return path
}

// p7FileHashes reads a package's per-file hashes, by canonical path.
func p7FileHashes(t *testing.T, dm *DatabaseManager, skillID string) map[string]string {
	t.Helper()
	rows, err := dm.db.Query(`SELECT path, sha256 FROM skill_package_files WHERE skill_id = ? ORDER BY path`, skillID)
	if err != nil {
		t.Fatalf("query the file rows: %v", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var path, hash string
		if err := rows.Scan(&path, &hash); err != nil {
			t.Fatalf("scan a file row: %v", err)
		}
		out[path] = hash
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate the file rows: %v", err)
	}
	return out
}

// ─── T07: determinism and the round trip ───────────────────────────────────

// TestP7ExportIsByteIdenticalOnRepeatedExports pins the determinism
// claim (T07).
//
// Two exports of the same package must produce the SAME BYTES — not
// merely the same content. The archive's own SHA-256 is the thing an
// operator checks, so it must be a function of the package alone.
func TestP7ExportIsByteIdenticalOnRepeatedExports(t *testing.T) {
	dm := p7DM(t)
	imported := p7Import(t, dm, "repeatable", "1.0.0", map[string][]byte{
		"references/templates.md": []byte("the reference body\n"),
		"scripts/run.sh":          []byte("#!/bin/sh\necho hello\n"),
	})

	first := p7Export(t, dm, imported.SkillID)
	second := p7Export(t, dm, imported.SkillID)

	if !bytes.Equal(first.Content, second.Content) {
		t.Fatalf("two exports of the same package differ: %d and %d bytes, digests %s and %s",
			len(first.Content), len(second.Content), first.SHA256, second.SHA256)
	}
	if first.SHA256 != second.SHA256 {
		t.Errorf("two exports of the same package have different digests: %s and %s",
			first.SHA256, second.SHA256)
	}
	if first.SHA256 != p7DigestOf(first.Content) {
		t.Errorf("the archive's digest is %s, want the digest of its bytes %s",
			first.SHA256, p7DigestOf(first.Content))
	}
}

// TestP7ExportRoundTripPreservesThePackageHash pins T07's other half:
// export → import reproduces the package hash and every per-file hash.
//
// The round trip is the property that makes the archive a TRANSPORT.
// The package hash covers the manifest, and the manifest covers the
// paths, the hashes, the sizes, the content types, the roles, the
// executable flags and the scan statuses — so preserving it means the
// re-imported package is the same package.
func TestP7ExportRoundTripPreservesThePackageHash(t *testing.T) {
	dm := p7DM(t)
	imported := p7Import(t, dm, "roundtrip", "1.0.0", map[string][]byte{
		"references/templates.md": []byte("the reference body\n"),
		"scripts/run.sh":          []byte("#!/bin/sh\necho hello\n"),
		"assets/empty.md":         []byte(""),
	})

	archive := p7Export(t, dm, imported.SkillID)

	// A DIFFERENT workspace: the archive is the transport, and the
	// transport carries the package across it.
	target := p7DM(t)
	result, err := target.ImportSkillPackageFromArchive(PackageImportOptions{
		Path: p7WriteArchive(t, archive),
	})
	if err != nil {
		t.Fatalf("import the exported archive: %v", err)
	}

	if result.PackageHash != imported.PackageHash {
		t.Errorf("the re-imported package hashes to %s, want %s",
			result.PackageHash, imported.PackageHash)
	}
	if result.SkillID != imported.SkillID {
		t.Errorf("the re-imported package has the id %q, want %q",
			result.SkillID, imported.SkillID)
	}

	before := p7FileHashes(t, dm, imported.SkillID)
	after := p7FileHashes(t, target, result.SkillID)
	if len(before) != len(after) {
		t.Fatalf("the re-imported package carries %d files, want %d", len(after), len(before))
	}
	for path, hash := range before {
		if after[path] != hash {
			t.Errorf("the re-imported file %q hashes to %q, want %q", path, after[path], hash)
		}
	}
}

// TestP7ExportRoundTripAcrossWorkspaces pins T11.
//
// The archive is the archive-as-transport of O4: export from one
// workspace, import into another, and the package identity survives.
func TestP7ExportRoundTripAcrossWorkspaces(t *testing.T) {
	source := p7DM(t)
	imported := p7Import(t, source, "crossing", "1.0.0", map[string][]byte{
		"references/templates.md": []byte("the reference body\n"),
	})
	archive := p7Export(t, source, imported.SkillID)

	// The archive is written to a path OUTSIDE both workspaces, so
	// nothing about the source workspace travels with it.
	carried := filepath.Join(t.TempDir(), archive.FileName)
	if err := os.WriteFile(carried, archive.Content, 0o600); err != nil {
		t.Fatalf("write the archive: %v", err)
	}

	target := p7DM(t)
	result, err := target.ImportSkillPackageFromArchive(PackageImportOptions{Path: carried})
	if err != nil {
		t.Fatalf("import the carried archive: %v", err)
	}
	if result.PackageHash != imported.PackageHash {
		t.Errorf("the package hashes to %s after crossing, want %s",
			result.PackageHash, imported.PackageHash)
	}
}

// TestP7ExportCarriesTheOriginalBytesNotTheProjection pins that the
// archive carries the ORIGINAL entrypoint, not the derived projection.
//
// The two differ for a versionless package: the projection carries one
// inserted `version:` line. An archive that carried the projection would
// make the re-imported package's ORIGINAL differ from the source's —
// and the export would be rewriting the third-party document.
func TestP7ExportCarriesTheOriginalBytesNotTheProjection(t *testing.T) {
	dm := p7DM(t)
	imported := p7Import(t, dm, "versionless", "", map[string][]byte{
		"references/templates.md": []byte("the reference body\n"),
	})

	archive := p7Export(t, dm, imported.SkillID)
	entry := p7ArchiveEntry(t, archive.Content, "SKILL.md")

	if strings.Contains(string(entry), "version:") {
		t.Errorf("the exported entrypoint carries a version line:\n%s", entry)
	}
	if p7DigestOf(entry) != imported.EntrypointSHA256 {
		t.Errorf("the exported entrypoint hashes to %s, want the original's %s",
			p7DigestOf(entry), imported.EntrypointSHA256)
	}

	// The re-imported package is versionless too, and its original is
	// the same original.
	target := p7DM(t)
	result, err := target.ImportSkillPackageFromArchive(PackageImportOptions{
		Path: p7WriteArchive(t, archive),
	})
	if err != nil {
		t.Fatalf("import the exported archive: %v", err)
	}
	if result.Version != "1.0.0-unversioned" {
		t.Errorf("the re-imported package's version is %q, want 1.0.0-unversioned", result.Version)
	}
	if result.PackageHash != imported.PackageHash {
		t.Errorf("the re-imported package hashes to %s, want %s",
			result.PackageHash, imported.PackageHash)
	}
}

// TestP7ExportUsesTheFlatLayout pins the layout (§5.6).
//
// Every canonical path sits at the archive's TOP LEVEL, so the
// entrypoint `SKILL.md` is at the archive's top level and §5.3.1's rule
// 4 makes the archive root the package root.
func TestP7ExportUsesTheFlatLayout(t *testing.T) {
	dm := p7DM(t)
	imported := p7Import(t, dm, "flat", "1.0.0", map[string][]byte{
		"references/templates.md": []byte("the reference body\n"),
		"scripts/run.sh":          []byte("#!/bin/sh\necho hello\n"),
	})

	archive := p7Export(t, dm, imported.SkillID)
	names := p7ArchiveEntries(t, archive.Content)

	if len(names) != 3 {
		t.Fatalf("the archive carries %d entries, want 3: %v", len(names), names)
	}
	if names[0] != "SKILL.md" {
		t.Errorf("the archive's first entry is %q, want SKILL.md", names[0])
	}
	want := []string{"SKILL.md", "references/templates.md", "scripts/run.sh"}
	for i := range want {
		if names[i] != want[i] {
			t.Errorf("the archive's entry %d is %q, want %q", i, names[i], want[i])
		}
	}
	for _, name := range names {
		if strings.HasPrefix(name, "./") || strings.Contains(name, "..") {
			t.Errorf("the archive's entry %q is not a flat canonical path", name)
		}
	}
}

// TestP7ExportCarriesEveryKindOfFile pins that the archive carries
// markdown references, scripts, binary assets and empty files.
//
// The binary asset needs the import's opt-in, which is the design's
// recommended default (O3, §7.3).
func TestP7ExportCarriesEveryKindOfFile(t *testing.T) {
	dm := p7DM(t)
	imported := p7ImportOpts(t, dm, PackageImportOptions{AllowBinary: true},
		"mixed", "1.0.0", map[string][]byte{
			"references/templates.md": []byte("the reference body\n"),
			"scripts/run.sh":          []byte("#!/bin/sh\necho hello\n"),
			"assets/logo.png":         {0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0x01},
			"references/empty.md":     []byte(""),
		})

	archive := p7Export(t, dm, imported.SkillID)
	names := p7ArchiveEntries(t, archive.Content)
	want := []string{
		"SKILL.md",
		"assets/logo.png",
		"references/empty.md",
		"references/templates.md",
		"scripts/run.sh",
	}
	if len(names) != len(want) {
		t.Fatalf("the archive carries %d entries, want %d: %v", len(names), len(want), names)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Errorf("the archive's entry %d is %q, want %q", i, names[i], want[i])
		}
	}

	// The bytes are the original bytes.
	if got := p7ArchiveEntry(t, archive.Content, "assets/logo.png"); !bytes.Equal(got, []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0x01}) {
		t.Errorf("the exported binary asset is %v, want the original bytes", got)
	}
	if got := p7ArchiveEntry(t, archive.Content, "references/empty.md"); len(got) != 0 {
		t.Errorf("the exported empty file carries %d bytes, want 0", len(got))
	}

	// And the round trip preserves the identity.
	target := p7DM(t)
	result, err := target.ImportSkillPackageFromArchive(PackageImportOptions{
		Path: p7WriteArchive(t, archive), AllowBinary: true,
	})
	if err != nil {
		t.Fatalf("import the exported archive: %v", err)
	}
	if result.PackageHash != imported.PackageHash {
		t.Errorf("the re-imported package hashes to %s, want %s",
			result.PackageHash, imported.PackageHash)
	}
}

// TestP7ExportOfPackagesBuiltFromDifferentlyOrderedSourcesProducesTheSame
// Bytes pins that the archive's identity does not depend on the order
// the SOURCE happened to arrive in.
//
// Two packages with the same content, imported from directories whose
// files were created in different orders, are the same package — so
// they must export to the same bytes.
//
// The order is CONTROLLED, not merely requested: the fixture writes
// the files in the order the test names, because a fixture that
// iterated a Go map would create them in a random order and the test
// would pass for the wrong reason.
func TestP7ExportOfPackagesBuiltFromDifferentlyOrderedSourcesProducesTheSameBytes(t *testing.T) {
	files := map[string][]byte{
		"references/templates.md": []byte("the reference body\n"),
		"scripts/run.sh":          []byte("#!/bin/sh\necho hello\n"),
		"assets/logo.png":         {0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a},
	}
	build := func(t *testing.T, order []string) *PackageExportArchive {
		t.Helper()
		dm := p7DM(t)
		imported := p7ImportOrdered(t, dm, PackageImportOptions{AllowBinary: true},
			"ordered", "1.0.0", files, order)
		return p7Export(t, dm, imported.SkillID)
	}

	first := build(t, []string{"references/templates.md", "scripts/run.sh", "assets/logo.png"})
	second := build(t, []string{"assets/logo.png", "scripts/run.sh", "references/templates.md"})

	if !bytes.Equal(first.Content, second.Content) {
		t.Errorf("two exports of the same package built from differently-ordered sources differ: %s and %s",
			first.SHA256, second.SHA256)
	}
}

// ─── The refusals ──────────────────────────────────────────────────────────

// TestP7ExportRefusesACorruptPackage pins that a package that does not
// verify is NOT exported.
//
// A corrupt package is never exported: an archive is a claim that these
// are the package's bytes, and a damaged package cannot make that
// claim.
func TestP7ExportRefusesACorruptPackage(t *testing.T) {
	t.Run("a corrupted blob", func(t *testing.T) {
		dm := p7DM(t)
		imported := p7Import(t, dm, "corrupt", "1.0.0", map[string][]byte{
			"references/templates.md": []byte("the reference body\n"),
		})
		if _, err := dm.db.Exec(`UPDATE skill_package_blobs SET content = ? WHERE sha256 = (
			SELECT f.sha256 FROM skill_package_files f WHERE f.skill_id = ? AND f.path = 'references/templates.md')`,
			[]byte("a tampered body\n"), imported.SkillID); err != nil {
			t.Fatalf("corrupt the blob: %v", err)
		}

		_, err := dm.ExportSkillPackage(context.Background(), PackageSelector{SkillID: imported.SkillID})
		if err == nil {
			t.Fatalf("the export of a corrupted package succeeded")
		}
		if reason := PackageReadReasonOf(err); reason != PackageReadReasonIntegrityFailure {
			t.Errorf("the refusal's reason is %q, want %q", reason, PackageReadReasonIntegrityFailure)
		}
	})

	t.Run("a missing file row", func(t *testing.T) {
		dm := p7DM(t)
		imported := p7Import(t, dm, "missingrow", "1.0.0", map[string][]byte{
			"references/templates.md": []byte("the reference body\n"),
		})
		if _, err := dm.db.Exec(`DELETE FROM skill_package_files WHERE skill_id = ? AND path = 'references/templates.md'`,
			imported.SkillID); err != nil {
			t.Fatalf("delete the file row: %v", err)
		}

		_, err := dm.ExportSkillPackage(context.Background(), PackageSelector{SkillID: imported.SkillID})
		if err == nil {
			t.Fatalf("the export of a package with a missing file row succeeded")
		}
		if reason := PackageReadReasonOf(err); reason != PackageReadReasonIntegrityFailure {
			t.Errorf("the refusal's reason is %q, want %q", reason, PackageReadReasonIntegrityFailure)
		}
	})

	t.Run("a corrupted manifest", func(t *testing.T) {
		dm := p7DM(t)
		imported := p7Import(t, dm, "badmanifest", "1.0.0", map[string][]byte{
			"references/templates.md": []byte("the reference body\n"),
		})
		if _, err := dm.db.Exec(`UPDATE skill_packages SET manifest = ? WHERE skill_id = ?`,
			"{ not a manifest", imported.SkillID); err != nil {
			t.Fatalf("corrupt the manifest: %v", err)
		}

		_, err := dm.ExportSkillPackage(context.Background(), PackageSelector{SkillID: imported.SkillID})
		if err == nil {
			t.Fatalf("the export of a package with a corrupted manifest succeeded")
		}
	})

	t.Run("a projection that contradicts the manifest", func(t *testing.T) {
		dm := p7DM(t)
		imported := p7Import(t, dm, "contradicted", "1.0.0", map[string][]byte{
			"references/templates.md": []byte("the reference body\n"),
		})
		if _, err := dm.db.Exec(`UPDATE memories SET content = ? WHERE id = ?`,
			p7SkillDoc("contradicted", "1.0.0", "a different body\n"), imported.SkillID); err != nil {
			t.Fatalf("rewrite the projection: %v", err)
		}

		_, err := dm.ExportSkillPackage(context.Background(), PackageSelector{SkillID: imported.SkillID})
		if err == nil {
			t.Fatalf("the export of a contradicted package succeeded")
		}
	})
}

// TestP7ExportRefusesAnUnsafeFileName pins the path-traversal refusal.
//
// A skill name is an IDENTIFIER, and an identifier may contain a path
// separator: `a/b` is a legal skill name. The derived file name must
// therefore be checked as a FILE NAME, and a name that is not a single
// safe path component is refused.
func TestP7ExportRefusesAnUnsafeFileName(t *testing.T) {
	for _, name := range []string{"../escape", "a/b", "..", ".hidden"} {
		t.Run(name, func(t *testing.T) {
			if _, err := packageExportBaseName(name, "1.0.0"); err == nil {
				t.Errorf("the file name derived from %q was accepted", name)
			}
		})
	}
	for _, name := range []string{"ordinary", "with.dots", "with_underscores"} {
		t.Run(name, func(t *testing.T) {
			base, err := packageExportBaseName(name, "1.0.0")
			if err != nil {
				t.Errorf("the file name derived from %q was refused: %v", name, err)
			}
			if base != name+"-1.0.0" {
				t.Errorf("the file name derived from %q is %q", name, base)
			}
		})
	}
}

// TestP7ExportNamesAPackageItCannotPublish pins that a package whose
// identity cannot be a file name is NAMED, not silently dropped.
//
// The batch export reports it in the omissions, with its reason.
//
// The reason is `export_failed` and NOT `integrity_failure`: the
// package VERIFIED — its bytes are consistent — and it is the
// PUBLICATION that failed. Reporting it as an integrity failure would
// be a false statement about the operator's stored bytes, and it would
// send the operator looking for corruption that is not there.
func TestP7ExportNamesAPackageItCannotPublish(t *testing.T) {
	dm := p7DM(t)
	// A skill NAME may contain a path separator: it is an identifier,
	// not a file name. The importer accepts it, and the exporter must
	// refuse to publish it.
	imported := p7Import(t, dm, "a/b", "1.0.0", nil)

	archives, omitted, err := dm.ExportSkillPackages(context.Background(), []string{imported.SkillID})
	if err != nil {
		t.Fatalf("export the packages: %v", err)
	}
	if len(archives) != 0 {
		t.Errorf("the export produced %d archives, want none", len(archives))
	}
	if len(omitted) != 1 {
		t.Fatalf("the export omitted %d packages, want 1", len(omitted))
	}
	if omitted[0].SkillID != imported.SkillID {
		t.Errorf("the omitted package is %q, want %q", omitted[0].SkillID, imported.SkillID)
	}
	if omitted[0].Reason == "" {
		t.Errorf("the omitted package carries no reason")
	}
	if omitted[0].Reason != "export_failed" {
		t.Errorf("the omitted package's reason is %q, want %q: the package "+
			"VERIFIED, so calling it an integrity failure is a false "+
			"statement about bytes that are consistent",
			omitted[0].Reason, "export_failed")
	}
	if omitted[0].Detail == "" {
		t.Errorf("the omitted package carries no detail naming the cause")
	}
}

// TestP7ExportNamesTheIdentityOfAPackageItCouldNotCarry pins that the
// exporter's OWN failure record carries the package's identity.
//
// The omission record is what the operator reads. A record that says
// "0 file(s), package_hash " is a record about a package the operator
// has never heard of: the file count is false, and the hash is the
// identifier that names the lost bytes.
//
// The identity is read on the SAME transaction the archives are read
// from, so the record and the archives describe one snapshot.
func TestP7ExportNamesTheIdentityOfAPackageItCouldNotCarry(t *testing.T) {
	dm := p7DM(t)
	imported := p7Import(t, dm, "damaged", "1.0.0", map[string][]byte{
		"references/templates.md": []byte("the reference body\n"),
		"scripts/run.sh":          []byte("#!/bin/sh\necho hi\n"),
	})

	// The package's identity, read straight from the store, is what the
	// omission record must state.
	var wantHash string
	if err := dm.db.QueryRow(`SELECT package_hash FROM skill_packages WHERE skill_id = ?`,
		imported.SkillID).Scan(&wantHash); err != nil {
		t.Fatalf("read the package hash: %v", err)
	}
	if wantHash == "" {
		t.Fatalf("the imported package carries no hash")
	}

	// Damage the package, so the export cannot carry it.
	if _, err := dm.db.Exec(`UPDATE skill_package_blobs SET content = ? WHERE sha256 = (
		SELECT f.sha256 FROM skill_package_files f WHERE f.skill_id = ? AND f.path = 'references/templates.md')`,
		[]byte("a tampered body\n"), imported.SkillID); err != nil {
		t.Fatalf("corrupt the blob: %v", err)
	}

	archives, omitted, err := dm.ExportSkillPackages(context.Background(), []string{imported.SkillID})
	if err != nil {
		t.Fatalf("export the packages: %v", err)
	}
	if len(archives) != 0 {
		t.Errorf("the export produced %d archives, want none", len(archives))
	}
	if len(omitted) != 1 {
		t.Fatalf("the export omitted %d packages, want 1", len(omitted))
	}
	if omitted[0].PackageHash != wantHash {
		t.Errorf("the omitted package's hash is %q, want %q: the hash is the "+
			"identifier that names the lost bytes",
			omitted[0].PackageHash, wantHash)
	}
	// The package carries THREE files: the entrypoint the import
	// writes, plus the two the fixture supplied. The count is the
	// STORE's count, and it is the same count the scope reports.
	if omitted[0].FileCount != 3 {
		t.Errorf("the omitted package carries %d file(s), want 3", omitted[0].FileCount)
	}
}

// TestP7TheDocumentPathRefusesAnUnpublishableNameWithTheSameReason pins
// that the ORDINARY-skill path and the PACKAGE path agree about why a
// name cannot be published.
//
// A skill's name is an identifier, and `a/b` is a legal one, so the
// derived file name can be refused. The package path reports that as
// `unpublishable`; if the ordinary-skill path reported it as an
// integrity failure, the two paths would disagree about the same
// fact, and the reason table would tell an operator only half of it.
func TestP7TheDocumentPathRefusesAnUnpublishableNameWithTheSameReason(t *testing.T) {
	dm := p7DM(t)
	// An ORDINARY skill — a row with no package — whose name is an
	// identifier that is not a safe file name.
	skillID := "skill:a/b-v1.0.0"
	if _, err := dm.db.Exec(`INSERT INTO memories (id, collection, content, tags, metadata, weight, is_global, deleted_at)
		VALUES (?, 'skills', ?, '[]', '{}', 5, 0, NULL)`,
		skillID, p7SkillDoc("a/b", "1.0.0", "a body\n")); err != nil {
		t.Fatalf("seed the ordinary skill: %v", err)
	}

	_, err := dm.ExportSkillDocument(context.Background(), skillID)
	if err == nil {
		t.Fatalf("the export of a skill named %q was accepted", "a/b")
	}
	if reason := PackageReadReasonOf(err); reason != PackageReadReasonUnpublishable {
		t.Errorf("the refusal's reason is %q, want %q: the skill is INTACT and "+
			"cannot be PUBLISHED, and the package path says the same thing",
			reason, PackageReadReasonUnpublishable)
	}
}

// TestP7TheArchiveOrderIsAStrictWeakOrdering pins the writer's
// comparator.
//
// Go does NOT validate the ordering it is handed to `sort`, so the
// contract — irreflexive, asymmetric, transitive — is the author's
// responsibility. The earlier comparator returned true for
// `less(SKILL.md, SKILL.md)`, which is a trap rather than an
// observable defect (a manifest's paths are unique), and a trap in a
// deterministic writer is worth closing.
func TestP7TheArchiveOrderIsAStrictWeakOrdering(t *testing.T) {
	entries := []packageExportEntry{
		{Path: "SKILL.md", Content: []byte("entry\n")},
		{Path: "SKILL.md", Content: []byte("entry\n")},
		{Path: "references/a.md", Content: []byte("a\n")},
		{Path: "references/b.md", Content: []byte("b\n")},
	}

	// The comparator the WRITER uses — the production function, not a
	// copy of it. A test that checks its own copy of the logic proves
	// nothing about the archive.
	less := packageExportEntryLess

	// And the writer really calls THAT function. A named comparator
	// that the writer does not use is a dead function, and a test
	// pointed at it proves nothing at all: the writer's own inline
	// copy could be broken while the test stayed green. The check is
	// a string check on the source, which is the same shape as the
	// repository's other architecture guards.
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatalf("could not resolve the test file's path")
	}
	source, err := os.ReadFile(filepath.Join(filepath.Dir(thisFile), "skill_package_export.go"))
	if err != nil {
		t.Fatalf("read the exporter's source: %v", err)
	}
	if !strings.Contains(string(source), "packageExportEntryLess(ordered[i], ordered[j])") {
		t.Errorf("the writer does not call packageExportEntryLess; it keeps its " +
			"own copy of the ordering, so the named comparator is dead code " +
			"and the contract above is not the one the archive is written with")
	}

	for _, e := range entries {
		if less(e, e) {
			t.Errorf("the comparator says %q precedes itself; it is not "+
				"irreflexive, and Go does not validate the ordering it is given",
				e.Path)
		}
	}
	for _, a := range entries {
		for _, b := range entries {
			if less(a, b) && less(b, a) {
				t.Errorf("the comparator says %q precedes %q AND the other "+
					"way round; it is not asymmetric", a.Path, b.Path)
			}
		}
	}

	// And the archive it writes is still deterministic: the entrypoint
	// first, then the other paths in order.
	content, err := writePackageArchive([]packageExportEntry{
		{Path: "references/b.md", Content: []byte("b\n")},
		{Path: "SKILL.md", Content: []byte("entry\n")},
		{Path: "references/a.md", Content: []byte("a\n")},
	})
	if err != nil {
		t.Fatalf("write the archive: %v", err)
	}
	var paths []string
	reader, err := zip.NewReader(bytes.NewReader(content), int64(len(content)))
	if err != nil {
		t.Fatalf("read the archive: %v", err)
	}
	for _, f := range reader.File {
		paths = append(paths, f.Name)
	}
	want := []string{"SKILL.md", "references/a.md", "references/b.md"}
	if len(paths) != len(want) {
		t.Fatalf("the archive holds %d entries (%v), want %d", len(paths), paths, len(want))
	}
	for i := range want {
		if paths[i] != want[i] {
			t.Errorf("the archive's entry %d is %q, want %q", i, paths[i], want[i])
		}
	}
}

// ─── The read-only guarantee ───────────────────────────────────────────────

// TestP7ExportChangesNothing pins that the export is READ-ONLY.
//
// The export reads the package, verifies it and serialises it. It must
// leave the whole substrate exactly as it found it — no repaired
// manifest, no rescanned file, no rewritten row.
func TestP7ExportChangesNothing(t *testing.T) {
	dm := p7DM(t)
	imported := p7Import(t, dm, "readonly", "1.0.0", map[string][]byte{
		"references/templates.md": []byte("the reference body\n"),
	})

	before := p6Census(t, dm)
	archive := p7Export(t, dm, imported.SkillID)
	after := p6Census(t, dm)

	if before != after {
		t.Errorf("the export changed the substrate:\nbefore %s\nafter  %s", before, after)
	}
	if len(archive.Content) == 0 {
		t.Errorf("the export produced no bytes")
	}
}

// p7DigestOf is the lowercase hex SHA-256 of some bytes.
func p7DigestOf(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

// ─── The archive's shape ───────────────────────────────────────────────────

// TestP7ExportPinsTheArchiveShape pins the three properties that make
// the archive deterministic: the ENTRY ORDER, the TIMESTAMP and the
// PERMISSIONS.
//
// They are read back from the archive with the standard ZIP reader, so
// the assertions are about the bytes on the wire rather than about the
// writer's internal state.
func TestP7ExportPinsTheArchiveShape(t *testing.T) {
	dm := p7DM(t)
	imported := p7ImportOpts(t, dm, PackageImportOptions{AllowBinary: true}, "shaped", "1.0.0", map[string][]byte{
		"references/templates.md": []byte("the reference body\n"),
		"scripts/run.sh":          []byte("#!/bin/sh\necho hello\n"),
		"assets/logo.png":         {0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a},
	})

	archive := p7Export(t, dm, imported.SkillID)
	reader, err := zip.NewReader(bytes.NewReader(archive.Content), int64(len(archive.Content)))
	if err != nil {
		t.Fatalf("read the archive: %v", err)
	}

	// The ORDER: the entrypoint first, then the rest sorted.
	wantOrder := []string{"SKILL.md", "assets/logo.png", "references/templates.md", "scripts/run.sh"}
	if len(reader.File) != len(wantOrder) {
		t.Fatalf("the archive carries %d entries, want %d", len(reader.File), len(wantOrder))
	}
	for i := range wantOrder {
		if reader.File[i].Name != wantOrder[i] {
			t.Errorf("the archive's entry %d is %q, want %q", i, reader.File[i].Name, wantOrder[i])
		}
	}

	// The TIMESTAMP: every entry carries the ZIP epoch, so an export
	// does not record when it happened.
	for _, file := range reader.File {
		want := time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC)
		if !file.Modified.UTC().Equal(want) {
			t.Errorf("the entry %q carries the timestamp %s, want %s",
				file.Name, file.Modified.UTC(), want)
		}
	}

	// The PERMISSIONS: 0644, and 0755 for a script.
	for _, file := range reader.File {
		want := os.FileMode(0o644)
		if file.Name == "scripts/run.sh" {
			want = 0o755
		}
		if got := file.Mode().Perm(); got != want {
			t.Errorf("the entry %q carries the mode %o, want %o", file.Name, got, want)
		}
	}

	// The COMPRESSION: no compression at all, so the archive's bytes
	// are a function of the package's bytes and the pinned header
	// fields — and not of the Go release that produced them.
	for _, file := range reader.File {
		if file.Method != zip.Store {
			t.Errorf("the entry %q uses the compression method %d, want %d (Store)",
				file.Name, file.Method, zip.Store)
		}
	}
}

// TestP7ExportArchiveBytesAreIdenticalAcrossGoReleases pins the
// property the STORE decision buys.
//
// Go's DEFLATE output is NOT stable across Go releases: the same bytes
// compressed by Go 1.26.6 and by Go 1.27.3 produce archives of
// different lengths and different digests. A deflate archive's identity
// would therefore depend on the TOOLCHAIN that produced it.
//
// The archive is STORED, not deflated, so its bytes are a function of
// the package's bytes and the pinned header fields alone. This test
// pins the archive's digest as a constant: a change to the writer, or a
// change to the Go release's ZIP output, would move it.
//
// The digest is the point. A test that compared two exports of the same
// package would pass on BOTH Go releases — and would not have caught
// the toolchain dependence that the STORE decision avoids. A constant
// catches both.
//
// WHAT THIS PIN DOES NOT PROMISE. Go's `archive/zip` writer does not
// promise byte stability: "the exact bytes written to w are not
// covered by the Go 1 compatibility promise. Callers, including tests,
// should not depend on the exact written bytes." Go 1.27.3 already
// changed the writer (the Zip64 threshold semantics and the EOCD64
// emission rule), so the class of change is live.
//
// So this constant is a GUARD, not a promise. If a future Go release
// changes the writer, this test fails - and that failure is the
// intended behaviour: the archive's bytes changed, so every digest an
// operator has recorded for an archive is now wrong, and that is worth
// a loud failure rather than a silent drift. The archive's identity is
// the operator's guarantee; it is not the Go release's.
func TestP7ExportArchiveBytesAreIdenticalAcrossGoReleases(t *testing.T) {
	dm := p7DM(t)
	imported := p7Import(t, dm, "pinned", "1.0.0", map[string][]byte{
		"references/templates.md": []byte("the reference body\n"),
	})

	archive := p7Export(t, dm, imported.SkillID)
	// The pinned digest. It was measured with Go 1.27.3, and CI runs
	// Go 1.26.6 — so this constant is a CROSS-VERSION pin: the two
	// toolchains must agree on the archive's bytes, or this test fails
	// on one of them.
	//
	// A DEFLATE archive would not survive that: Go's deflate output
	// differs between the two releases - the same input produces
	// archives of different sizes and different digests under Go
	// 1.26.6 and Go 1.27.3. (The exact sizes depend on the fixture,
	// so none is quoted: the DIGESTS differing is the claim.) The
	// STORE decision is what makes this constant possible.
	const wantDigest = "61bcd485ef6dde79d442f63168652437d0ec453e00de8bb24336ae9970131afd"

	if archive.SHA256 != wantDigest {
		t.Errorf("the exported archive hashes to %s, want the pinned %s.\n"+
			"The archive's bytes are pinned so that a change to the writer, or to the Go "+
			"release's ZIP output, is visible here rather than at an operator's import",
			archive.SHA256, wantDigest)
	}
}

// TestP7ExportRejectsTwoPackagesThatDeriveTheSameFileName pins the
// collision refusal.
//
// The file name is `<name>-<version>.skill`, and the derivation is not
// injective: `a-b` at `1.0.0` and `a` at `b-1.0.0` both derive
// `a-b-1.0.0.skill`. Two archives with one name cannot both be
// published — the second would overwrite the first — so the second is
// NAMED as an omission with its reason.
func TestP7ExportRejectsTwoPackagesThatDeriveTheSameFileName(t *testing.T) {
	dm := p7DM(t)
	// Two identities that derive ONE file name.
	//
	//   "a" at "1.0.0-b-2.0.0"  ->  "a-1.0.0-b-2.0.0"
	//   "a-1.0.0-b" at "2.0.0"  ->  "a-1.0.0-b-2.0.0"
	//
	// Both are legal: a skill name may contain hyphens (but not "-v",
	// which would collide with the id format), and a semver prerelease
	// may contain hyphens.
	first := p7Import(t, dm, "a", "1.0.0-b-2.0.0", nil)
	second := p7Import(t, dm, "a-1.0.0-b", "2.0.0", nil)

	archives, omitted, err := dm.ExportSkillPackages(context.Background(),
		[]string{first.SkillID, second.SkillID})
	if err != nil {
		t.Fatalf("export the packages: %v", err)
	}

	if first.SkillID == second.SkillID {
		t.Skipf("the fixture's two packages share an id")
	}
	if len(archives) != 1 {
		t.Fatalf("the export produced %d archives, want 1", len(archives))
	}
	if len(omitted) != 1 {
		t.Fatalf("the export omitted %d packages, want 1", len(omitted))
	}
	if omitted[0].Reason != "file_name_collision" {
		t.Errorf("the omission's reason is %q, want file_name_collision", omitted[0].Reason)
	}
	if omitted[0].Detail == "" {
		t.Errorf("the omission carries no detail")
	}
}

// TestP7ExportFlattensAWrappedSource pins that a package imported from
// a WRAPPED archive exports as a FLAT archive.
//
// The P5 importer accepts an archive whose entries sit under a wrapper
// directory: the package root is the entrypoint's parent (§5.3.1's rule
// 4). The exporter always writes the FLAT layout, so the archive it
// produces is accepted by the importer by construction — whatever the
// source's layout was.
//
// The package's IDENTITY is unaffected: the wrapper is a property of the
// transport, not of the package.
func TestP7ExportFlattensAWrappedSource(t *testing.T) {
	dm := p7DM(t)

	// A WRAPPED archive: everything sits under `wrapper/`.
	wrapper := "wrapper"
	content := map[string][]byte{
		"SKILL.md":                []byte(p7SkillDoc("wrapped", "1.0.0", "the original entrypoint body\n")),
		"references/templates.md": []byte("the reference body\n"),
		"scripts/run.sh":          []byte("#!/bin/sh\necho hello\n"),
	}
	path := p7WriteArchiveEntries(t, wrapper, content)

	imported, err := dm.ImportSkillPackageFromArchive(PackageImportOptions{Path: path})
	if err != nil {
		t.Fatalf("import the wrapped archive: %v", err)
	}

	archive := p7Export(t, dm, imported.SkillID)

	// The exported archive is FLAT.
	names := p7ArchiveEntries(t, archive.Content)
	want := []string{"SKILL.md", "references/templates.md", "scripts/run.sh"}
	if len(names) != len(want) {
		t.Fatalf("the archive carries %d entries, want %d: %v", len(names), len(want), names)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Errorf("the archive's entry %d is %q, want %q", i, names[i], want[i])
		}
	}

	// And the round trip preserves the identity.
	target := p7DM(t)
	result, err := target.ImportSkillPackageFromArchive(PackageImportOptions{
		Path: p7WriteArchive(t, archive),
	})
	if err != nil {
		t.Fatalf("import the exported archive: %v", err)
	}
	if result.PackageHash != imported.PackageHash {
		t.Errorf("the re-imported package hashes to %s, want %s",
			result.PackageHash, imported.PackageHash)
	}
}

// TestP7ExportCarriesAPackageWithSharedBlobs pins that a package whose
// files SHARE a content-addressed blob round-trips exactly.
//
// Blobs are content-addressed and shared: two files with identical bytes
// point at ONE blob row. The export writes each file's bytes into the
// archive, so the shared blob is carried twice — and the re-import
// shares it again. The identity must be identical either way.
func TestP7ExportCarriesAPackageWithSharedBlobs(t *testing.T) {
	dm := p7DM(t)
	shared := []byte("the same bytes\n")
	imported := p7Import(t, dm, "shared", "1.0.0", map[string][]byte{
		"references/a.md": shared,
		"references/b.md": shared,
		"references/c.md": []byte("different bytes\n"),
	})

	// The two identical files really do share one blob: their recorded
	// hashes are the same, and the blob table holds one row for them.
	var hashA, hashB string
	if err := dm.db.QueryRow(`SELECT sha256 FROM skill_package_files WHERE skill_id = ? AND path = 'references/a.md'`,
		imported.SkillID).Scan(&hashA); err != nil {
		t.Fatalf("read the file row: %v", err)
	}
	if err := dm.db.QueryRow(`SELECT sha256 FROM skill_package_files WHERE skill_id = ? AND path = 'references/b.md'`,
		imported.SkillID).Scan(&hashB); err != nil {
		t.Fatalf("read the file row: %v", err)
	}
	if hashA != hashB {
		t.Fatalf("the two identical files record different hashes (%s and %s); "+
			"they should share one content-addressed blob", hashA, hashB)
	}
	var blobRows int
	if err := dm.db.QueryRow(`SELECT COUNT(*) FROM skill_package_blobs WHERE sha256 = ?`, hashA).Scan(&blobRows); err != nil {
		t.Fatalf("count the blob rows: %v", err)
	}
	if blobRows != 1 {
		t.Fatalf("the shared content occupies %d blob rows, want 1", blobRows)
	}

	archive := p7Export(t, dm, imported.SkillID)
	names := p7ArchiveEntries(t, archive.Content)
	if len(names) != 4 {
		t.Fatalf("the archive carries %d entries, want 4: %v", len(names), names)
	}

	target := p7DM(t)
	result, err := target.ImportSkillPackageFromArchive(PackageImportOptions{
		Path: p7WriteArchive(t, archive),
	})
	if err != nil {
		t.Fatalf("import the exported archive: %v", err)
	}
	if result.PackageHash != imported.PackageHash {
		t.Errorf("the re-imported package hashes to %s, want %s",
			result.PackageHash, imported.PackageHash)
	}
}

// p7WriteArchiveEntries writes a ZIP whose entries sit under a wrapper
// directory, and returns its path.
//
// It is the WRAPPED source layout the P5 importer accepts.
func p7WriteArchiveEntries(t *testing.T, wrapper string, files map[string][]byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "wrapped.skill")
	file, err := os.Create(path)
	if err != nil {
		t.Fatalf("create the archive: %v", err)
	}
	writer := zip.NewWriter(file)
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		header := &zip.FileHeader{Name: wrapper + "/" + name, Method: zip.Store}
		entry, err := writer.CreateHeader(header)
		if err != nil {
			file.Close()
			t.Fatalf("create the archive entry %q: %v", name, err)
		}
		if _, err := entry.Write(files[name]); err != nil {
			file.Close()
			t.Fatalf("write the archive entry %q: %v", name, err)
		}
	}
	if err := writer.Close(); err != nil {
		file.Close()
		t.Fatalf("close the archive: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close the archive file: %v", err)
	}
	return path
}

// ─── The legacy single-document skill ──────────────────────────────────────

// TestP7ExportDocumentRefusesAPackagedSkill pins the refusal.
//
// The skill's row holds the PROJECTION of its package, and the
// package's other files live in their own rows and blobs. A one-file
// archive built from the row would therefore carry the projection
// rather than the original bytes, and would omit every other file —
// so the package would come back a DIFFERENT package, or no package at
// all. That is the silent loss §6.3 exists to prevent, so the exporter
// refuses instead.
func TestP7ExportDocumentRefusesAPackagedSkill(t *testing.T) {
	dm := p7DM(t)
	imported := p7Import(t, dm, "packaged", "1.0.0", map[string][]byte{
		"references/templates.md": []byte("the reference body\n"),
	})

	_, err := dm.ExportSkillDocument(context.Background(), imported.SkillID)
	if err == nil {
		t.Fatalf("the one-file export accepted a PACKAGED skill")
	}
	if !strings.Contains(err.Error(), "PACKAGE") {
		t.Errorf("the refusal does not say the skill is a package: %v", err)
	}
	if !strings.Contains(err.Error(), "Export the package instead") {
		t.Errorf("the refusal does not say the way out: %v", err)
	}
}

// TestP7ExportDocumentProducesAnArchiveTheImporterAccepts pins that the
// one-file archive is a valid package for the P5 importer.
//
// The check is against the substrate's OWN reader
// (`ImportSkillPackageFromArchive`), not against a second
// interpretation of `archive/zip`.
func TestP7ExportDocumentProducesAnArchiveTheImporterAccepts(t *testing.T) {
	dm := p7DM(t)

	document := p7SkillDoc("legacy", "2.1.0", "the legacy body\n")
	skillID, err := dm.SaveSkill("legacy", "2.1.0", document, "test", false)
	if err != nil {
		t.Fatalf("save the skill: %v", err)
	}

	archive, err := dm.ExportSkillDocument(context.Background(), skillID)
	if err != nil {
		t.Fatalf("export the document: %v", err)
	}

	// The archive carries ONE entry: the skill's own document.
	names := p7ArchiveEntries(t, archive.Content)
	if len(names) != 1 || names[0] != "SKILL.md" {
		t.Fatalf("the one-file archive carries %v, want exactly [SKILL.md]", names)
	}

	// And the importer accepts it.
	target := p7DM(t)
	result, err := target.ImportSkillPackageFromArchive(PackageImportOptions{
		Path: p7WriteArchive(t, archive),
	})
	if err != nil {
		t.Fatalf("the one-file archive is NOT accepted by the importer: %v", err)
	}
	// The skill's SEMANTICS survive: the same name and the same
	// version. The document declared a version, so the projection is
	// byte-identical to the original and the round trip is exact.
	if result.Name != "legacy" {
		t.Errorf("the re-imported skill is named %q, want legacy", result.Name)
	}
	if result.Version != "2.1.0" {
		t.Errorf("the re-imported skill is at version %q, want 2.1.0", result.Version)
	}
}

// TestP7TheSubstrateHoldsNoVersionlessSkill pins WHY the one-file
// export never has to invent a version.
//
// The package format requires a version in the frontmatter (§5.2), and
// the substrate enforces that at the door: a skill whose document
// declares no version is REFUSED when it is saved. So every skill in
// the store declares a version, the one-file archive always carries
// one, and the transport adaptation never has to insert a line.
//
// The claim this replaces said the exporter gave a versionless
// document the effective version `1.0.0-unversioned`. That is the
// IMPORTER's rule for a PACKAGE (the importer inserts the line into
// the projection it stores) — and it cannot happen here: the substrate
// holds no versionless skill to export.
func TestP7TheSubstrateHoldsNoVersionlessSkill(t *testing.T) {
	dm := p7DM(t)

	document := "---\nname: versionless\ndescription: a test skill\n" +
		"when_to_use: when a test needs this exact skill\n---\nthe body\n"
	_, err := dm.SaveSkill("versionless", "1.0.0", document, "test", false)
	if err == nil {
		t.Fatalf("the substrate stored a skill whose document declares no version")
	}
	if !strings.Contains(err.Error(), "version") {
		t.Errorf("the refusal does not name the version: %v", err)
	}
}

// TestP7ExportDocumentWritesNothing pins that the one-file export
// changes nothing in the substrate.
func TestP7ExportDocumentWritesNothing(t *testing.T) {
	dm := p7DM(t)
	document := p7SkillDoc("legacy-quiet", "1.0.0", "the legacy body\n")
	skillID, err := dm.SaveSkill("legacy-quiet", "1.0.0", document, "test", false)
	if err != nil {
		t.Fatalf("save the skill: %v", err)
	}
	before := p6Census(t, dm)
	if _, err := dm.ExportSkillDocument(context.Background(), skillID); err != nil {
		t.Fatalf("export the document: %v", err)
	}
	after := p6Census(t, dm)
	if before != after {
		t.Errorf("the one-file export changed the substrate: before %s, after %s", before, after)
	}
}
