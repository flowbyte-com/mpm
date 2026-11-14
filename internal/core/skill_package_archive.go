// skill_package_archive.go — the `.skill` ZIP archive adapter (Stage P5).
//
// Design of record: PR #222 (docs/dev/skill-package-format-design-2026-11-04.md)
// §5.3.1 (the root-normalisation rules), §7.1 (the entry-type check, R5),
// §4.4 Rule 2 (no package byte ever reaches the filesystem), §13 T04,
// T13–T22, T26, T35 and T48.
//
// ─────────────────────────────────────────────────────────────────────────────
// WHAT THIS FILE IS
// ─────────────────────────────────────────────────────────────────────────────
//
// This is an ADAPTER, not a second importer. It turns a ZIP archive into
// the SAME validated representation the directory walk produces —
// `[]packageImportFile` — and hands it to the SAME planner
// (`planPackageImportFiles`) that the directory path hands its files to.
//
// The rules that decide whether a package may be stored — the frontmatter
// contract, the semantic version, the secret and poison scans, the binary
// and executable rules, the manifest, the hashes, the identity, the
// idempotence and version-conflict checks, the transaction and the
// post-commit verification — are therefore applied ONCE, and they are
// applied to a directory import and to an archive import alike. That is
// what makes T04 and T48 true: the same logical package produces the same
// canonical paths, the same file rows, the same hashes and the same
// `package_hash` whichever way it arrived.
//
// ─────────────────────────────────────────────────────────────────────────────
// THE BOUNDARY IS RULE 2, NOT THE ENTRY-TYPE CHECK
// ─────────────────────────────────────────────────────────────────────────────
//
// §7.1 is explicit about this and it is the most important thing to get
// right here. A ZIP entry's TYPE is determined by an attacker-chosen
// header byte — the central directory's creator byte — so the type check
// is available only for a Unix-attribute archive:
//
//   - a Unix-attribute archive: a symlink, a FIFO or a device entry is
//     REFUSED with `unsupported_entry_type` (§13 T16);
//   - a FAT/MS-DOS-attribute archive: the same entries DEGRADE to regular
//     files, because the attributes are not interpreted, and the import
//     SUCCEEDS (§13 T35).
//
// That asymmetry is safe because of **Rule 2** (§4.4): an import writes
// the bytes into a database column and never materialises them as a file.
// There is no filesystem object to turn a symlink into an escape, and no
// code path connects package content to execution. The entry-type check
// is defence in depth; Rule 2 is the boundary.
//
// The DIRECTORY entry is the exception, and it is detected under BOTH
// creator bytes: a name ending in `/` is a directory by its own name, and
// the MS-DOS directory attribute is a second, independent signal. A
// directory entry is SKIPPED — it is not a file, it is not stored, and it
// does not participate in the root derivation (§5.3.1 rule 1).

package internal

import (
	"archive/zip"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// ─────────────────────────────────────────────────────────────────────────────
// THE ARCHIVE LIMITS
// ─────────────────────────────────────────────────────────────────────────────

// packageArchiveLimits are the limits that apply to the ARCHIVE itself,
// as opposed to the package's content.
//
// The package's content limits are P4's, unchanged: 512 files, 32 MiB in
// total, 4 MiB per file and 256 KiB for the entrypoint (O6). The two
// limits below are the only ones P5 adds, and both are stated here with
// the reason they exist.
type packageArchiveLimits struct {
	// MaxArchiveBytes is the maximum size of the archive FILE on disk.
	//
	// JUSTIFICATION. The package's UNCOMPRESSED content is capped at
	// 32 MiB by P4's total-byte limit, and that cap is enforced while
	// the bytes are produced — so a decompression bomb is refused
	// without ever being materialised. The on-disk size is a
	// different thing: it bounds the central directory, which
	// `archive/zip` parses eagerly into one header per entry, and it
	// bounds the compressed payload a single entry may carry.
	//
	// A legitimate archive holds at most 32 MiB of content. DEFLATE
	// never expands, and STORED entries are the same size as their
	// content, so 64 MiB — twice the content cap — accommodates every
	// legitimate archive with room for the central directory and the
	// local headers, and refuses an archive whose size is driven by
	// something other than package content.
	MaxArchiveBytes int64

	// MaxArchiveEntries is the maximum number of entries the archive's
	// central directory may declare.
	//
	// JUSTIFICATION. `archive/zip` builds one `zip.File` per central
	// directory record when the reader is created, so the entry count
	// is what bounds that allocation.
	//
	// The bound is enforced BEFORE the reader is built, from the
	// End-of-Central-Directory record — and it is enforced against TWO
	// quantities, because the declared count alone is not a bound.
	//
	// `archive/zip` parses directory records until it meets a bad
	// signature and compares the number it read with the DECLARED
	// count only afterwards, so an archive that declares two entries
	// and carries 150 000 real records has all 150 000 parsed — and a
	// header struct allocated for each — before the mismatch is
	// noticed. The SECOND bound is therefore the number of records the
	// central directory's BYTES could hold: the parse spans exactly
	// the central directory, and every record is at least 46 bytes
	// long, so a byte span holds at most span/46 records. That bound
	// survives a lying count, because it is derived from the bytes.
	//
	// A legitimate package carries at most 512 files (P4's cap) and at
	// most one directory entry per directory it contains, so at most
	// about 1024 entries. 4096 leaves four times that headroom for
	// archives that record directories generously, while keeping the
	// parsed central directory to a few megabytes.
	MaxArchiveEntries int
}

// defaultPackageArchiveLimits returns the archive limits.
func defaultPackageArchiveLimits() packageArchiveLimits {
	return packageArchiveLimits{
		MaxArchiveBytes:   64 << 20, // 64 MiB
		MaxArchiveEntries: 4096,
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// THE PUBLIC ENTRY POINT
// ─────────────────────────────────────────────────────────────────────────────

// ImportSkillPackageFromArchive imports a skill package from a `.skill`
// ZIP archive and stores it atomically.
//
// The archive is read into the SAME validated representation the
// directory walk produces, and the SAME planner, transaction and
// post-commit verification run over it. Nothing about a directory import
// is re-implemented here.
//
// The import owns ONE transaction, exactly as the directory import does,
// and validation and scanning happen BEFORE that transaction opens, so a
// rejected archive writes nothing at all.
//
// The accepted archive layouts are the two the operator's decision O9
// names: the FLAT layout (`SKILL.md` at the archive's top level) and the
// WRAPPED layout (one enclosing directory holding the whole package).
// The package root is derived from the ENTRYPOINT's parent directory
// (§5.3.1), so a flat package may carry any number of content
// directories; only COMPETING package roots — more than one `SKILL.md` —
// are refused.
func (dm *DatabaseManager) ImportSkillPackageFromArchive(opts PackageImportOptions) (*PackageImportResult, error) {
	if dm == nil {
		return nil, packageImportErrorf(PackageImportReasonImportRootInvalid, "", "no database manager")
	}
	if strings.TrimSpace(opts.Path) == "" {
		return nil, packageImportErrorf(PackageImportReasonImportRootInvalid, "", "no archive path was given")
	}

	limits := opts.Limits.withDefaults()
	author := opts.AuthorAgent
	if author == "" {
		author = "operator"
	}

	// ── Phase 1: read the archive. No transaction is open. ──────────────
	plan, err := dm.planPackageArchiveImport(opts, limits)
	if err != nil {
		return nil, err
	}

	// ── Phase 2: the single transaction. ────────────────────────────────
	result, err := dm.commitPackageImport(plan, author, opts.ActiveContext)
	if err != nil {
		return nil, err
	}

	// ── Phase 3: post-commit verification. ──────────────────────────────
	if err := dm.verifyPackageImport(result); err != nil {
		return nil, err
	}
	return result, nil
}

// planPackageArchiveImport performs every validation an archive import
// needs, and returns the plan the transaction will execute.
//
// NOTHING is written here. The archive is read, the entries are
// validated, the root is derived, the scans run and the manifest is
// built — all before a transaction exists, so a refusal at any point
// leaves the database untouched (§7 threat 17).
func (dm *DatabaseManager) planPackageArchiveImport(opts PackageImportOptions, limits PackageImportLimits) (*packageImportPlan, error) {
	limits = limits.withDefaults()

	// The poison scanner's configuration must be loadable BEFORE any
	// content is scanned. The scanner reads an operator-editable file
	// and can fail open; the import refuses instead (§7.2, R10, T43).
	//
	// The preflight comes FIRST here, before the archive is even
	// opened, so a workspace whose scanner cannot run refuses without
	// reading a byte of the archive.
	phrasesPath := dm.poisonPhrasesPath()
	if err := poisonScannerAvailable(phrasesPath); err != nil {
		return nil, packageImportErrorWrap(PackageImportReasonScannerUnavailable, "", err,
			"the toxic-phrase configuration could not be loaded: %v", err)
	}

	source, err := readPackageArchive(opts.Path, limits, defaultPackageArchiveLimits())
	if err != nil {
		return nil, err
	}

	return dm.planPackageImportFiles(opts, limits, phrasesPath, source)
}

// ─────────────────────────────────────────────────────────────────────────────
// READING THE ARCHIVE
// ─────────────────────────────────────────────────────────────────────────────

// packageArchiveEntry is one archive FILE entry, after the structural
// checks and before the root is derived.
type packageArchiveEntry struct {
	// rawPath is the entry's name exactly as the archive carried it.
	//
	// It is DISPLAY-ONLY: it becomes the row's `raw_path`, so the
	// original archive layout stays auditable (§5.4). It is never used
	// to address a file and never compared for identity.
	rawPath string

	// archivePath is the canonical form of rawPath relative to the
	// ARCHIVE root.
	//
	// It is the input to the root derivation (§5.3.1), and it is
	// relative to the archive — not to the package — so in the wrapped
	// layout it still carries the wrapper segment.
	archivePath string

	// content is the entry's bytes, exactly as they were decompressed.
	content []byte
}

// readPackageArchive reads an archive and returns the shared import
// representation.
//
// The whole archive is read into memory, and NO byte of it is ever
// written to the filesystem: Rule 2 (§4.4) says a package's bytes are a
// database column, and the archive path honours it by never
// materialising a temporary extraction tree. That is also why the
// entry-type check's limitation is harmless here — see the file's
// header.
func readPackageArchive(archivePath string, limits PackageImportLimits, archiveLimits packageArchiveLimits) (packageImportSource, error) {
	source := packageImportSource{origin: archivePath, entrypointIndex: -1}

	file, info, err := openPackageArchive(archivePath, archiveLimits)
	if err != nil {
		return source, err
	}
	defer file.Close()

	// ── The central directory is bounded BEFORE it is parsed. ───────
	//
	// `archive/zip` parses the whole central directory into memory when
	// the reader is created — one header per record — and it validates
	// the DECLARED count only AFTER the parse is over. So the declared
	// count alone does not bound that allocation: an archive that
	// declares two entries and carries 150 000 real records has all
	// 150 000 parsed before the mismatch is noticed.
	//
	// TWO bounds are applied, and together they hold:
	//
	//  1. the DECLARED count — the cheap check, which refuses an
	//     archive that plainly declares too many entries;
	//  2. the PARSE WINDOW — the number of records the central
	//     directory's BYTES could hold, derived from the record's own
	//     extent. This is the bound that survives a lying count,
	//     because it is derived from the bytes rather than from the
	//     count.
	dir, err := readPackageArchiveDirectory(file, info.Size())
	if err != nil {
		return source, packageImportErrorWrap(PackageImportReasonArchiveInvalid, "", err,
			"the archive %q has no usable central directory: %v", archivePath, err)
	}
	if dir.count > uint64(archiveLimits.MaxArchiveEntries) {
		return source, packageImportErrorf(PackageImportReasonFileCountLimit, "",
			"the archive declares %d entries, over the %d-entry cap",
			dir.count, archiveLimits.MaxArchiveEntries)
	}
	if maxRecords := dir.maxParseRecords(file, info.Size()); maxRecords > int64(archiveLimits.MaxArchiveEntries) {
		return source, packageImportErrorf(PackageImportReasonFileCountLimit, "",
			"the archive's central directory could hold %d records, over the %d-entry cap",
			maxRecords, archiveLimits.MaxArchiveEntries)
	}

	reader, err := zip.NewReader(file, info.Size())
	if err != nil {
		return source, packageImportErrorWrap(PackageImportReasonArchiveInvalid, "", err,
			"the archive %q could not be read: %v", archivePath, err)
	}

	entries, err := readPackageArchiveEntries(reader, limits)
	if err != nil {
		return source, err
	}
	if len(entries) == 0 {
		return source, packageImportErrorf(PackageImportReasonEntrypointNotFound, "",
			"the archive %q carries no files", archivePath)
	}

	// ── §5.3.1: derive the package root, then strip it. ───────────────
	files, entrypointIndex, err := derivePackageArchiveFiles(entries)
	if err != nil {
		return source, err
	}

	source.files = files
	source.entrypointIndex = entrypointIndex
	return source, nil
}

// openPackageArchive opens the archive file and returns it with the
// identity of the file that was opened.
//
// The identity is captured BEFORE the open and compared with the opened
// descriptor, exactly as the directory path does (§7 threat 19): if the
// path was re-pointed between the two, the bytes that would be imported
// are not the bytes the operator named, and the import refuses.
//
// A path that is not a regular file is refused BEFORE it is opened. That
// matters for FIFOs and devices: opening one would block, or would
// return a stream of bytes that are not a package at all.
func openPackageArchive(archivePath string, limits packageArchiveLimits) (*os.File, os.FileInfo, error) {
	// The classification is taken WITHOUT following a symlink, so a FIFO
	// is refused rather than opened.
	info, err := os.Lstat(archivePath)
	if err != nil {
		return nil, nil, packageImportErrorWrap(PackageImportReasonImportRootInvalid, "", err,
			"the archive %q could not be inspected: %v", archivePath, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		// The operator's own path may be a symlink — resolving it is
		// reasonable, and the RESOLVED path names the archive.
		resolved, err := filepath.EvalSymlinks(archivePath)
		if err != nil {
			return nil, nil, packageImportErrorWrap(PackageImportReasonImportRootInvalid, "", err,
				"the archive %q could not be resolved: %v", archivePath, err)
		}
		archivePath = resolved
		info, err = os.Stat(archivePath)
		if err != nil {
			return nil, nil, packageImportErrorWrap(PackageImportReasonImportRootInvalid, "", err,
				"the archive %q could not be inspected: %v", archivePath, err)
		}
	}
	if !info.Mode().IsRegular() {
		return nil, nil, packageImportErrorf(PackageImportReasonImportRootInvalid, "",
			"the archive %q is not a regular file", archivePath)
	}
	if info.Size() > limits.MaxArchiveBytes {
		return nil, nil, packageImportErrorf(PackageImportReasonSizeLimit, "",
			"the archive is %d bytes, over the %d-byte cap", info.Size(), limits.MaxArchiveBytes)
	}

	file, err := os.Open(archivePath)
	if err != nil {
		return nil, nil, packageImportErrorWrap(PackageImportReasonImportRootInvalid, "", err,
			"the archive %q could not be opened: %v", archivePath, err)
	}
	opened, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, nil, packageImportErrorWrap(PackageImportReasonImportRootInvalid, "", err,
			"the archive %q could not be inspected after opening: %v", archivePath, err)
	}
	if !os.SameFile(info, opened) {
		file.Close()
		return nil, nil, packageImportErrorf(PackageImportReasonChangedDuringRead, "",
			"the archive %q was replaced between its inspection and its open", archivePath)
	}
	return file, opened, nil
}

// The ZIP general purpose bit flags the importer honours.
const (
	// archiveFlagEncrypted is the traditional PKWARE encryption flag.
	archiveFlagEncrypted = 0x0001

	// archiveFlagStrongEncryption is the "strong encryption" flag,
	// which supersedes the traditional one.
	archiveFlagStrongEncryption = 0x0040
)

// readPackageArchiveEntries reads every FILE entry of an archive.
//
// The checks are ordered so that the cheapest refusal comes first, no
// check depends on a later one, and NO CHECK IS DONE AFTER THE BYTES
// HAVE BEEN PRODUCED unless it has to be:
//
//  1. a directory entry is SKIPPED (§5.3.1 rule 1) — before anything
//     else, because a directory entry's name is not a canonical path
//     and it is not a file;
//  2. the entry-type check (§7.1) — refused for a Unix-attribute
//     symlink, FIFO or device;
//  3. the encryption flags — an entry whose bytes cannot be read
//     cannot be scanned, so it is refused;
//  4. the file-count cap — checked before the entry's bytes are read;
//  5. the canonical-path rules, on the archive-relative path — the
//     safety rules AND the collision rules, both before the bytes are
//     produced, so an entry that breaks one is never decompressed;
//  6. the DECLARED size — refused before the bytes are produced, so a
//     decompression bomb never allocates;
//  7. the bytes themselves, bounded.
//
// The total-byte cap is checked as the bytes are read (§7 threat 11).
func readPackageArchiveEntries(reader *zip.Reader, limits PackageImportLimits) ([]packageArchiveEntry, error) {
	var entries []packageArchiveEntry
	var totalBytes int64

	// The path set accumulates the archive-relative names, so the
	// duplicate, unicode-collision and case-collision refusals are
	// reached BEFORE the entrypoint's uniqueness is considered
	// (§5.3.1 rule 2). "Two entries with the same canonical path" is a
	// different refusal from "two entrypoints", and the design says the
	// first one is reached first.
	paths := NewPackagePathSet()

	for _, zf := range reader.File {
		// ── 1. A directory entry is skipped, FIRST. ──────────────────
		//
		// `Mode()` reports a directory when the name ends in `/` and
		// when the creator byte's attributes say so — the MS-DOS
		// directory attribute is the second, independent signal. Both
		// are honoured under both creator bytes (§7.1).
		if zf.Mode().IsDir() {
			continue
		}

		// ── 2. The entry-type check (R5, §7.1). ────────────────────
		//
		// This check is available only for a Unix-attribute archive.
		// Under a FAT creator byte the same entry degrades to a
		// regular file and is accepted — and that is safe, because
		// Rule 2 says no package byte ever reaches the filesystem.
		if !zf.Mode().IsRegular() {
			return nil, packageImportErrorf(PackageImportReasonUnsupportedEntryType, zf.Name,
				"the archive entry %q is a %s, not a regular file", zf.Name, zf.Mode().Type())
		}

		// ── 3. An encrypted entry cannot be validated. ─────────────
		//
		// Two flags are honoured: the traditional PKWARE encryption
		// bit and the "strong encryption" bit that supersedes it.
		// Neither is supported by the reader, and an entry whose bytes
		// cannot be read cannot be scanned, so both are refused.
		if zf.Flags&(archiveFlagEncrypted|archiveFlagStrongEncryption) != 0 {
			return nil, packageImportErrorf(PackageImportReasonArchiveEncrypted, zf.Name,
				"the archive entry %q is encrypted, so its bytes cannot be validated", zf.Name)
		}

		// ── 4. The file-count cap, before the bytes are read. ───────
		if len(entries)+1 > limits.MaxFiles {
			return nil, packageImportErrorf(PackageImportReasonFileCountLimit, zf.Name,
				"the package carries more than %d files", limits.MaxFiles)
		}

		// ── 5. The canonical-path rules, on the archive-relative path.
		//
		// The name is registered in the set, so a duplicate, a unicode
		// collision and a case collision are all refused here — before
		// the root derivation, and therefore before the entrypoint's
		// uniqueness is considered (§5.3.1 rule 2 before rule 3).
		//
		// The check happens BEFORE the entry's bytes are produced, so
		// an entry whose name breaks a safety rule is refused without
		// its payload ever being decompressed.
		archivePath, err := paths.AddWithBudget(zf.Name, archivePathMaxDepth, archivePathMaxBytes)
		if err != nil {
			return nil, packageImportArchivePathError(zf.Name, err)
		}

		// ── 6. The DECLARED size, before the bytes are produced. ───
		//
		// This is the decompression-bomb defence: the declared size is
		// refused outright, so the entry is never decompressed at all.
		// The cap is ALSO enforced while reading (step 6), because a
		// header can lie.
		if zf.UncompressedSize64 > uint64(limits.MaxFileBytes) {
			return nil, packageImportErrorf(PackageImportReasonSizeLimit, zf.Name,
				"the archive entry %q declares %d bytes, over the %d-byte cap",
				zf.Name, zf.UncompressedSize64, limits.MaxFileBytes)
		}

		// ── 7. The bytes, bounded. ──────────────────────────────────
		content, err := readPackageArchiveEntryBytes(zf, limits.MaxFileBytes)
		if err != nil {
			return nil, err
		}
		totalBytes += int64(len(content))
		if totalBytes > limits.MaxTotalBytes {
			return nil, packageImportErrorf(PackageImportReasonSizeLimit, zf.Name,
				"the package's content exceeds the %d-byte total cap", limits.MaxTotalBytes)
		}

		entries = append(entries, packageArchiveEntry{
			rawPath:     zf.Name,
			archivePath: archivePath,
			content:     content,
		})
	}
	return entries, nil
}

// readPackageArchiveEntryBytes reads one entry's bytes, bounded.
//
// THE THREE CAPS OF §7 THREAT 9
//
// The design names three independent caps against ZIP bombs and extreme
// compression ratios, and all three are here:
//
//  1. the per-entry DECLARED-size cap — checked by the caller, before
//     the entry is opened, so a bomb is never decompressed at all;
//  2. the count of ACTUAL bytes read — the reader here is given one
//     byte more than the cap, so an entry that really carries more than
//     the cap produces more than the cap and is refused. The declared
//     size is an attacker-controlled header, so the only trustworthy
//     number is the one the importer COUNTS;
//  3. the total-uncompressed cap across the package — checked by the
//     caller as the bytes are read.
//
// The bound is enforced WHILE the bytes are read, not from the header
// alone: the reader is given one byte more than the cap, so an entry
// that actually carries more than the cap produces more than the cap and
// is refused.
//
// Reading to EOF is what verifies the CRC: `archive/zip` computes the
// checksum over the bytes it produced and reports a mismatch only when
// the entry has been read to its end. A reader that stopped early would
// never see the mismatch, so the read here always goes to EOF unless the
// cap is exceeded first.
func readPackageArchiveEntryBytes(zf *zip.File, maxBytes int64) ([]byte, error) {
	rc, err := zf.Open()
	if err != nil {
		return nil, classifyPackageArchiveError(zf.Name, err)
	}
	defer rc.Close()

	content, err := io.ReadAll(io.LimitReader(rc, maxBytes+1))
	if err != nil {
		return nil, classifyPackageArchiveError(zf.Name, err)
	}
	if int64(len(content)) > maxBytes {
		return nil, packageImportErrorf(PackageImportReasonSizeLimit, zf.Name,
			"the archive entry %q is over the %d-byte cap", zf.Name, maxBytes)
	}
	return content, nil
}

// classifyPackageArchiveError turns an archive-level read failure into a
// classified refusal.
//
// The distinction that matters is the CHECKSUM: a CRC mismatch means the
// bytes are not the bytes the archive says they are, which is its own
// code (§13 T22). Everything else — a malformed structure, a truncated
// entry, a size that does not match its payload, an unsupported
// compression method — is the archive being invalid (§13 T21, T26).
func classifyPackageArchiveError(name string, err error) error {
	if errors.Is(err, zip.ErrChecksum) {
		return packageImportErrorWrap(PackageImportReasonChecksumMismatch, name, err,
			"the archive entry %q does not match its checksum: %v", name, err)
	}
	return packageImportErrorWrap(PackageImportReasonArchiveInvalid, name, err,
		"the archive entry %q could not be read: %v", name, err)
}

// ─────────────────────────────────────────────────────────────────────────────
// THE ROOT DERIVATION — §5.3.1, EXACTLY
// ─────────────────────────────────────────────────────────────────────────────

// derivePackageArchiveFiles applies §5.3.1's rules, in order, and
// returns the package's files.
//
// The rules, in the order the design states them:
//
//  1. directory entries are skipped — already done, before the entries
//     reached this function;
//  2. the remaining file entries are canonicalised — already done, as
//     each entry was read;
//  3. the entrypoint is the file entry whose BASE NAME is exactly
//     `SKILL.md`; more than one is `ambiguous_package_root`; none is
//     `entrypoint_not_found`;
//  4. the package root is the entrypoint's PARENT DIRECTORY;
//  5. every file entry must lie under the root — otherwise
//     `path_outside_root`;
//  6. the root segment is stripped BY WHOLE SEGMENT, and the root itself
//     is never stored;
//  7. the derivation is order-independent — it uses the complete entry
//     set.
//
// The result is order-independent because the files are sorted by their
// canonical path before they are returned, exactly as the directory walk
// sorts them.
func derivePackageArchiveFiles(entries []packageArchiveEntry) ([]packageImportFile, int, error) {
	// ── Rule 3: find the entrypoint. ─────────────────────────────────
	entrypointIndex := -1
	var candidates []string
	for i := range entries {
		if path.Base(entries[i].archivePath) != packageEntrypointName {
			continue
		}
		if entrypointIndex >= 0 {
			candidates = append(candidates, entries[entrypointIndex].archivePath, entries[i].archivePath)
			return nil, -1, packageImportErrorf(PackageImportReasonAmbiguousEntrypoint, "",
				"the archive carries more than one %s, so it holds competing package roots%s",
				packageEntrypointName, formatPackageEntrypointCandidates(candidates))
		}
		entrypointIndex = i
	}
	if entrypointIndex < 0 {
		for i := range entries {
			if strings.EqualFold(path.Base(entries[i].archivePath), packageEntrypointName) {
				candidates = append(candidates, entries[i].archivePath)
			}
		}
		return nil, -1, packageImportErrorf(PackageImportReasonEntrypointNotFound, "",
			"the archive has no %s at its package root%s",
			packageEntrypointName, formatPackageEntrypointCandidates(candidates))
	}

	// ── Rule 4: the root is the entrypoint's parent directory. ──────
	//
	// An entrypoint at the archive's top level has no directory
	// component, so the root is the ARCHIVE root — the flat layout. An
	// entrypoint at `pkg/SKILL.md` has the parent `pkg`, so the root is
	// that wrapper — the wrapped layout.
	entrypointDir := path.Dir(entries[entrypointIndex].archivePath)
	if entrypointDir == "." || entrypointDir == "/" {
		entrypointDir = ""
	}
	rootSegments := 0
	if entrypointDir != "" {
		rootSegments = strings.Count(entrypointDir, "/") + 1
	}

	// The operator's decision O9 accepts exactly TWO layouts: the flat
	// layout and the WRAPPED layout, which is ONE enclosing directory.
	// A root deeper than one segment is a third shape — the package
	// wrapped in more than one directory — and it is refused rather
	// than silently reinterpreted.
	//
	// This is the one place where the implementation is NARROWER than
	// §5.3.1's derivation, which would accept a deeper root and treat
	// the entrypoint's whole parent path as the root. The narrower rule
	// is the operator's O9 instruction ("exactly one ... the flat layout
	// or the wrapped layout"), and it is reported as a deviation.
	if rootSegments > 1 {
		return nil, -1, packageImportErrorf(PackageImportReasonUnsupportedArchiveLayout, "",
			"the archive's package root %q is %d directories deep; the accepted layouts are the flat layout "+
				"and a single enclosing directory", entrypointDir, rootSegments)
	}

	// ── Rules 5 and 6: every entry must lie under the root, and the
	// root is stripped by whole segment. ──────────────────────────────
	files := make([]packageImportFile, 0, len(entries))
	newEntrypointIndex := -1
	for i := range entries {
		e := &entries[i]

		packagePath := e.archivePath
		if entrypointDir != "" {
			// The prefix is compared WHOLE-SEGMENT: a path whose
			// first segment merely shares a prefix with the root —
			// `pkg-extra/x.md` against a root of `pkg` — does not
			// begin with the root and is outside it (rule 5).
			if !strings.HasPrefix(e.archivePath, entrypointDir+"/") {
				return nil, -1, packageImportErrorf(PackageImportReasonPathOutsideRoot, e.rawPath,
					"the archive entry %q lies outside the package root %q", e.rawPath, entrypointDir)
			}
			packagePath = strings.TrimPrefix(e.archivePath, entrypointDir+"/")
		}

		// The canonical path is relative to the PACKAGE root, so §5.4's
		// depth and length limits are the PACKAGE's limits — the
		// archive path's own extra wrapper segment does not count
		// against them.
		canonical, err := CanonicalPackagePath(packagePath)
		if err != nil {
			return nil, -1, packageImportArchivePathError(e.rawPath, err)
		}

		files = append(files, packageImportFile{
			canonicalPath: canonical,
			rawPath:       e.rawPath,
			sizeBytes:     int64(len(e.content)),
			content:       e.content,
			sha256:        sha256Hex(e.content),
		})
		if i == entrypointIndex {
			newEntrypointIndex = len(files) - 1
		}
	}

	// The entrypoint's canonical path is exactly `SKILL.md`: the root
	// IS the entrypoint's parent directory, so stripping the root
	// leaves the entrypoint's own name (rule 6).
	if newEntrypointIndex < 0 || files[newEntrypointIndex].canonicalPath != packageEntrypointName {
		return nil, -1, packageImportErrorf(PackageImportReasonEntrypointNotFound, "",
			"the archive has no %s at its package root", packageEntrypointName)
	}

	// ── Rule 7: the derivation is order-independent. ─────────────────
	//
	// The files are sorted by their CANONICAL path, exactly as the
	// directory walk sorts them, so two archives carrying the same
	// entries in a different order produce the same rows, the same
	// manifest and the same `package_hash`.
	sort.SliceStable(files, func(i, j int) bool {
		return files[i].canonicalPath < files[j].canonicalPath
	})
	for i := range files {
		if files[i].canonicalPath == packageEntrypointName {
			newEntrypointIndex = i
			break
		}
	}
	return files, newEntrypointIndex, nil
}

// The archive path's budget.
//
// The depth budget is the package's depth plus ONE: the archive path may
// carry the single wrapper segment that the package's canonical path
// does not. The byte budget is twice the package's, plus the separator:
// the archive path is the package's canonical path plus the wrapper
// segment and a `/`, and the wrapper segment is itself bounded by the
// package's path limit.
const (
	archivePathMaxDepth = MaxPackagePathDepth + 1
	archivePathMaxBytes = MaxPackagePathBytes*2 + 1
)

// packageImportArchivePathError turns a canonical-path refusal into a
// classified IMPORT refusal, carrying the path rule's own code.
//
// The codes are the ones §5.4 and §7 name — `path_traversal` (§7
// threat 1, T13), `absolute_path` (threat 2, T14),
// `drive_letter_prefix` (threat 4, T15), `duplicate_path` (threat 6,
// T17), `path_case_collision` (threat 7, T18) and
// `path_unicode_collision` (threat 8, T19) — so a caller branches on
// one vocabulary rather than on a second encoding of the same rule.
func packageImportArchivePathError(raw string, err error) error {
	var pe *PackagePathError
	if !errors.As(err, &pe) {
		return packageImportErrorWrap(PackageImportReasonPathInvalid, raw, err,
			"the path %q was refused: %v", raw, err)
	}
	return packageImportErrorWrap(packageImportReasonForPathReason(pe.Reason), raw, err,
		"the path %q was refused: %v", raw, err)
}

// ─────────────────────────────────────────────────────────────────────────────
// THE END-OF-CENTRAL-DIRECTORY RECORD
// ─────────────────────────────────────────────────────────────────────────────

// The End-of-Central-Directory record's layout, and the two ZIP64
// records that extend it.
const (
	archiveEOCDSignature         = 0x06054b50
	archiveEOCDMinSize           = 22
	archiveEOCDMaxComment        = 0xffff
	archiveEOCDCountOffset       = 10
	archiveEOCDSizeOffset        = 12
	archiveEOCDOffsetOffset      = 16
	archiveZip64LocatorSignature = 0x07064b50
	archiveZip64LocatorSize      = 20
	archiveZip64EOCDSignature    = 0x06064b50
	archiveZip64EOCDCountOffset  = 32
	archiveZip64EOCDSizeOffset   = 40
	archiveZip64EOCDOffsetOffset = 48

	// archiveCentralDirectoryRecordMinBytes is the fixed size of a
	// central-directory file header, without its name, its extra field
	// or its comment.
	//
	// Every record is AT LEAST this long, so a byte span holds at most
	// span/this records. That is what turns a byte bound into an entry
	// bound.
	archiveCentralDirectoryRecordMinBytes = 46

	// archiveCentralDirectorySignature is the signature of a
	// central-directory file header.
	archiveCentralDirectorySignature = 0x02014b50
)

// packageArchiveDirectory is the archive's central directory, as the
// End-of-Central-Directory record declares it.
type packageArchiveDirectory struct {
	// count is the DECLARED number of entries.
	count uint64

	// size is the central directory's byte size, as declared.
	size int64

	// offset is the central directory's offset, as declared.
	offset int64

	// endOffset is the absolute offset of the End-of-Central-Directory
	// record — for a ZIP64 archive, the ZIP64 record, which is where
	// the central directory really ends.
	endOffset int64
}

// maxParseRecords returns the maximum number of central-directory
// records `archive/zip` can parse from this archive.
//
// # WHY THE DECLARED COUNT IS NOT THE BOUND
//
// `archive/zip` reads directory records until it meets a bad signature,
// and it compares the number it read with the DECLARED count only
// AFTERWARDS. So the declared count does not bound the parse at all: an
// archive that declares two entries and carries 150 000 real records
// has all 150 000 parsed, and a header struct allocated for each of
// them, before the mismatch is noticed.
//
// The bound that DOES hold is a bound on the BYTES. `archive/zip` parses
// from the central directory's first byte and stops at the
// End-of-Central-Directory record — whose signature is not a valid
// record signature — so the parse spans exactly the central directory.
// And every record is at least 46 bytes long, so a byte span holds at
// most span/46 records.
//
// # WHICH START `archive/zip` USES
//
// There are two, and which one applies is decided by the archive
// itself:
//
//   - the NORMAL start — the central directory's first byte, which
//     `archive/zip` derives as `endOffset - size`;
//   - the FALLBACK start — the declared offset. `archive/zip` uses it
//     only for an archive whose declared offsets are inconsistent: it
//     re-reads the record at the declared offset and, if a valid
//     central-directory header really is there, parses from THERE.
//
// The fallback is CHECKED, not assumed, and that matters: a PREFIXED
// (self-extracting) archive has a positive base offset, and its
// declared offset is relative to the archive's logical start — so the
// absolute offset points into the PREFIX. Taking the earlier of the
// two starts unconditionally would treat the whole prefix as parseable
// central directory, and would refuse a legitimate self-extracting
// archive that `archive/zip` reads perfectly.
//
// The result is therefore tight for every well-formed archive — the
// normal start — and sound for the malformed one, where the fallback
// really does apply.
func (d packageArchiveDirectory) maxParseRecords(r io.ReaderAt, size int64) int64 {
	start := d.endOffset - d.size

	// The fallback applies only when the base offset is positive AND
	// the bytes at the declared offset really are a central-directory
	// record. `archive/zip` reads exactly those four bytes to decide,
	// so this reads exactly the same four bytes.
	if d.endOffset-d.size-d.offset > 0 && d.offset >= 0 && d.offset+4 <= size {
		var sig [4]byte
		if _, err := r.ReadAt(sig[:], d.offset); err == nil {
			if binary.LittleEndian.Uint32(sig[:]) == archiveCentralDirectorySignature {
				start = d.offset
			}
		}
	}

	if start < 0 {
		start = 0
	}
	if start >= d.endOffset {
		return 0
	}
	return (d.endOffset - start) / archiveCentralDirectoryRecordMinBytes
}

// readPackageArchiveDirectory reads the End-of-Central-Directory record
// and returns the archive's central directory.
//
// # WHY IT IS READ BY HAND
//
// `archive/zip` parses the whole central directory into memory when the
// reader is created — one header per record — and it does NOT validate
// the declared count until the parse is over. So the only way to bound
// that allocation is to read the record that describes the directory's
// extent FIRST, and refuse before the reader is built.
//
// The record is fixed-size and sits at a known place near the end of the
// file, so it can be read without parsing anything.
//
// ZIP64 is handled: when the 16-bit count or either 32-bit offset is
// saturated, the real values live in the ZIP64 End-of-Central-Directory
// record, which the locator that precedes the EOCD points at.
//
// A ZIP64 locator's offset is resolved ABSOLUTELY, exactly as
// `archive/zip` resolves it. A prefixed (self-extracting) ZIP64 archive
// is therefore refused rather than mis-parsed — the same behaviour
// `archive/zip` itself has, and it fails closed.
func readPackageArchiveDirectory(r io.ReaderAt, size int64) (packageArchiveDirectory, error) {
	var dir packageArchiveDirectory

	if size < archiveEOCDMinSize {
		return dir, fmt.Errorf("the archive is too small to be a ZIP file")
	}

	// The EOCD is at the end of the file, but a comment of up to 65535
	// bytes may follow it, so the search window is the last 64 KiB plus
	// the record itself. The window is searched BACKWARDS, so the last
	// candidate wins — which is the real record rather than a signature
	// that happens to appear inside a comment.
	window := int64(archiveEOCDMinSize + archiveEOCDMaxComment)
	if window > size {
		window = size
	}
	buf := make([]byte, window)
	if _, err := r.ReadAt(buf, size-window); err != nil && !errors.Is(err, io.EOF) {
		return dir, err
	}

	index := archiveFindEOCD(buf)
	if index < 0 {
		return dir, fmt.Errorf("no end-of-central-directory record was found")
	}
	record := buf[index:]
	dir.endOffset = size - window + int64(index)

	dir.count = uint64(binary.LittleEndian.Uint16(record[archiveEOCDCountOffset:]))
	dir.size = int64(binary.LittleEndian.Uint32(record[archiveEOCDSizeOffset:]))
	dir.offset = int64(binary.LittleEndian.Uint32(record[archiveEOCDOffsetOffset:]))

	// ZIP64: any of the three saturated fields means the real values
	// live in the ZIP64 record.
	if dir.count == 0xffff || dir.size == 0xffffffff || dir.offset == 0xffffffff {
		size64, offset64, count64, end64, err := readPackageArchiveDirectory64(r, buf, index, size)
		if err != nil {
			return dir, err
		}
		dir.count = count64
		dir.size = size64
		dir.offset = offset64
		dir.endOffset = end64
	}
	return dir, nil
}

// readPackageArchiveDirectory64 reads the ZIP64 End-of-Central-Directory
// record and its locator.
//
// The locator sits immediately before the plain EOCD record and points
// at the ZIP64 record, whose own offset is the end of the central
// directory.
func readPackageArchiveDirectory64(r io.ReaderAt, buf []byte, index int, size int64) (dirSize int64, dirOffset int64, count uint64, endOffset int64, err error) {
	if index < archiveZip64LocatorSize {
		return 0, 0, 0, 0, fmt.Errorf("the archive declares ZIP64 fields but carries no locator")
	}
	locator := buf[index-archiveZip64LocatorSize:]
	if binary.LittleEndian.Uint32(locator) != archiveZip64LocatorSignature {
		return 0, 0, 0, 0, fmt.Errorf("the archive declares ZIP64 fields but carries no locator")
	}
	offset := int64(binary.LittleEndian.Uint64(locator[8:]))
	if offset < 0 || offset+archiveZip64EOCDOffsetOffset+8 > size {
		return 0, 0, 0, 0, fmt.Errorf("the archive's ZIP64 locator points outside the archive")
	}
	var record64 [archiveZip64EOCDOffsetOffset + 8]byte
	if _, err := r.ReadAt(record64[:], offset); err != nil {
		return 0, 0, 0, 0, err
	}
	if binary.LittleEndian.Uint32(record64[:]) != archiveZip64EOCDSignature {
		return 0, 0, 0, 0, fmt.Errorf("the archive's ZIP64 end-of-central-directory record is malformed")
	}
	return int64(binary.LittleEndian.Uint64(record64[archiveZip64EOCDSizeOffset:])),
		int64(binary.LittleEndian.Uint64(record64[archiveZip64EOCDOffsetOffset:])),
		binary.LittleEndian.Uint64(record64[archiveZip64EOCDCountOffset:]),
		offset,
		nil
}

// archiveFindEOCD returns the offset of the End-of-Central-Directory
// record within buf, or -1.
//
// The record's own comment length is checked, so a signature that
// happens to appear inside a comment is not mistaken for the record.
func archiveFindEOCD(buf []byte) int {
	for i := len(buf) - archiveEOCDMinSize; i >= 0; i-- {
		if binary.LittleEndian.Uint32(buf[i:]) != archiveEOCDSignature {
			continue
		}
		comment := int(binary.LittleEndian.Uint16(buf[i+archiveEOCDMinSize-2:]))
		if i+archiveEOCDMinSize+comment <= len(buf) {
			return i
		}
	}
	return -1
}
