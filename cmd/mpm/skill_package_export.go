package main

// skill_package_export.go — the CLI side of Stage P7's complete skills
// export.
//
// Design of record: PR #222 §6.3 (the export contract: no silent loss)
// and §10 (stage P7).
//
// ─────────────────────────────────────────────────────────────────────────────
// WHAT THE COMPLETE SKILLS EXPORT WRITES
//
// A complete skills export writes TWO things:
//
//   - the export document — the same JSON document the export has always
//     written, with the `memories` array, the `packages_omitted` marker
//     and, now, the `package_archives` index;
//   - a SIBLING DIRECTORY named for that export, holding one
//     `<name>-<version>.skill` archive per packaged skill.
//
// The index is machine-readable: it names the directory, and for each
// archive its file name, the skill id, the package hash and the
// archive's own SHA-256. An operator can therefore check every archive
// without re-deriving what it should contain.
//
// ─────────────────────────────────────────────────────────────────────────────
// THE PUBLICATION IS NOT ONE ATOMIC OPERATION
//
// The directory is published by a RENAME, and the document by a second
// rename. Two renames are not one atomic operation, and the design
// does not pretend otherwise: the ORDER makes the intermediate state
// harmless.
//
// The DIRECTORY is published FIRST and the DOCUMENT SECOND. A document
// is the index that points at the archives, so a document published
// without its archives would point at files that are not there — a lie
// the consumer cannot detect. The reverse — a directory with no
// document — points at nothing, and the recovery is to remove the
// directory and re-run the export.
//
// ─────────────────────────────────────────────────────────────────────────────
// THE SAFETY RULES
//
//   - Nothing is published until every archive has been written AND
//     its digest re-read from the staged file and checked. A document
//     is never truncated before its content is known to be complete.
//   - An existing sibling directory that holds files is REFUSED. An
//     export never silently overwrites an archive.
//   - A symlink is never followed: the output path and the sibling
//     directory are inspected WITHOUT following links, and a symlink
//     in either place is refused.
//   - The staging directory is created with a random name beside the
//     output, so the publication is a same-filesystem rename, and it
//     is removed on every failure path.
//   - Every published archive's digest is verified by reading it BACK
//     from the published location.

import (
	"bytes"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	mpminternal "github.com/flowbyte-com/mpm-core"
	"github.com/flowbyte-com/mpm-core/usererror"
)

// packageExportIndex is the machine-readable index of the produced
// archives.
//
// It is part of the export document, so a consumer of the document
// knows exactly which files were produced and what they should hash
// to.
type packageExportIndex struct {
	// Count is the number of archives the export produced.
	Count int `json:"count"`

	// Directory is the NAME of the sibling directory that holds them.
	// It is a name rather than a path because the directory is always
	// a sibling of the document: the pair travels together.
	Directory string `json:"directory"`

	// Archives names each produced archive.
	Archives []*mpminternal.PackageExportArchive `json:"archives"`
}

// packageExportDirectorySuffix is the suffix appended to the output
// path to name the sibling directory.
//
// The suffix is appended to the WHOLE file name, so `export.json`
// yields `export.json.packages`. The result is obviously derived from
// the document, and it cannot collide with the document itself.
const packageExportDirectorySuffix = ".packages"

// packageExportTarget is where a package-bearing export writes.
type packageExportTarget struct {
	// DocumentPath is the export document's path.
	DocumentPath string

	// DirectoryPath is the sibling directory's path.
	DirectoryPath string
}

// resolvePackageExportTarget derives the sibling directory's path from
// the document's path.
func resolvePackageExportTarget(documentPath string) packageExportTarget {
	return packageExportTarget{
		DocumentPath:  documentPath,
		DirectoryPath: documentPath + packageExportDirectorySuffix,
	}
}

// publishPackageExport writes the archives and the export document.
//
// The document is the LAST thing published, so the intermediate state —
// a directory with no document — never points at anything.
//
// The document bytes are rendered by the caller and handed over, so the
// document's content is complete before anything is published.
func publishPackageExport(target packageExportTarget, archives []*mpminternal.PackageExportArchive, document []byte) error {
	parent := filepath.Dir(target.DocumentPath)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return fmt.Errorf("create the export directory %q: %w", parent, err)
	}

	// ── The output path must not be a symlink. ─────────────────────────
	//
	// The export writes to a path the operator named. If that path is a
	// symlink, following it would write somewhere the operator did not
	// name — and the symlink could point at the live substrate.
	if info, err := os.Lstat(target.DocumentPath); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("the export path %q is a symlink; refusing to follow it", target.DocumentPath)
		}
		if info.IsDir() {
			return fmt.Errorf("the export path %q is a directory", target.DocumentPath)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect the export path %q: %w", target.DocumentPath, err)
	}

	// ── The sibling directory must not be a symlink, and must not
	// already hold files. ───────────────────────────────────────────────
	//
	// The emptiness check is repeated immediately before the rename,
	// so the window in which someone could put a file into the
	// directory between the check and the publication is as narrow as
	// it can be.
	if info, err := os.Lstat(target.DirectoryPath); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("the package directory %q is a symlink; refusing to follow it", target.DirectoryPath)
		}
		if !info.IsDir() {
			return fmt.Errorf("the package directory %q exists and is not a directory", target.DirectoryPath)
		}
		if err := refuseNonEmptyPackageDirectory(target.DirectoryPath); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect the package directory %q: %w", target.DirectoryPath, err)
	}

	// ── The archives must not collide. ─────────────────────────────────
	//
	// The export already rejects a collision — the file name is derived
	// from the package's identity, and two identities can derive one
	// name. This is the second line of defence: an archive set that
	// carries one name twice would publish one over the other, and the
	// export would silently lose a package.
	seen := make(map[string]bool, len(archives))
	for _, archive := range archives {
		if seen[archive.FileName] {
			return fmt.Errorf("the export carries two archives named %q; refusing to publish one over the other",
				archive.FileName)
		}
		seen[archive.FileName] = true
	}

	// ── Stage the archives. ─────────────────────────────────────────────
	//
	// The staging directory is a random name beside the output, so the
	// publication is a same-filesystem rename — and so a predictable
	// name cannot be pre-created as a symlink.
	staging, err := os.MkdirTemp(parent, ".mpm-export-staging-")
	if err != nil {
		return fmt.Errorf("create the staging directory: %w", err)
	}
	published := false
	defer func() {
		if !published {
			os.RemoveAll(staging)
		}
	}()

	for _, archive := range archives {
		if err := writeStagedArchive(staging, archive); err != nil {
			return err
		}
	}

	// ── Publish the directory. ──────────────────────────────────────────
	//
	// The directory is replaced wholesale by the rename, so it must be
	// EMPTY at this point — an EMPTY directory holds nothing to
	// overwrite. The check is repeated here, immediately before the
	// rename, so a file that appeared in the meantime is not silently
	// replaced.
	if info, err := os.Lstat(target.DirectoryPath); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("the package directory %q is a symlink; refusing to follow it", target.DirectoryPath)
		}
		if err := refuseNonEmptyPackageDirectory(target.DirectoryPath); err != nil {
			return err
		}
		if err := os.Remove(target.DirectoryPath); err != nil {
			return fmt.Errorf("remove the empty package directory %q: %w", target.DirectoryPath, err)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect the package directory %q: %w", target.DirectoryPath, err)
	}
	if err := os.Rename(staging, target.DirectoryPath); err != nil {
		return fmt.Errorf("publish the package directory %q: %w", target.DirectoryPath, err)
	}
	published = true

	// ── Publish the document. ───────────────────────────────────────────
	//
	// It is written to a temporary name and RENAMED over the output, so
	// the output is never truncated before its content is complete.
	documentTemp, err := os.CreateTemp(parent, ".mpm-export-document-")
	if err != nil {
		return fmt.Errorf("stage the export document: %w", err)
	}
	documentTempName := documentTemp.Name()
	documentPublished := false
	defer func() {
		if !documentPublished {
			os.Remove(documentTempName)
		}
	}()
	if _, err := documentTemp.Write(document); err != nil {
		documentTemp.Close()
		return fmt.Errorf("write the export document: %w", err)
	}
	if err := documentTemp.Close(); err != nil {
		return fmt.Errorf("close the export document: %w", err)
	}
	if err := os.Rename(documentTempName, target.DocumentPath); err != nil {
		return fmt.Errorf("publish the export document %q: %w", target.DocumentPath, err)
	}
	documentPublished = true

	// ── Verify what was published. ─────────────────────────────────────
	//
	// The archives are read BACK from their published location and
	// their digests are checked. A write that reported success but
	// produced different bytes is caught here rather than by whoever
	// imports the archive next.
	for _, archive := range archives {
		published_ := filepath.Join(target.DirectoryPath, archive.FileName)
		content, err := os.ReadFile(published_)
		if err != nil {
			return fmt.Errorf("read back the published archive %q: %w", published_, err)
		}
		sum := sha256.Sum256(content)
		if got := hex.EncodeToString(sum[:]); got != archive.SHA256 {
			return fmt.Errorf("the published archive %q hashes to %s, want %s",
				published_, got, archive.SHA256)
		}
	}
	return nil
}

// refuseNonEmptyPackageDirectory refuses a package directory that
// already holds files.
//
// An export never silently overwrites an existing export's archives:
// the bytes of a previous export are the operator's, and replacing them
// without a word would be a loss that nothing reported.
func refuseNonEmptyPackageDirectory(path string) error {
	entries, err := os.ReadDir(path)
	if err != nil {
		return fmt.Errorf("read the package directory %q: %w", path, err)
	}
	if len(entries) > 0 {
		return fmt.Errorf(
			"the package directory %q already holds %d file(s); refusing to overwrite an existing export. "+
				"Remove it and run the export again",
			path, len(entries))
	}
	return nil
}

// writeStagedArchive writes one archive into the staging directory and
// checks its digest.
func writeStagedArchive(staging string, archive *mpminternal.PackageExportArchive) error {
	path := filepath.Join(staging, archive.FileName)
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create the archive %q: %w", archive.FileName, err)
	}
	if _, err := file.Write(archive.Content); err != nil {
		file.Close()
		return fmt.Errorf("write the archive %q: %w", archive.FileName, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close the archive %q: %w", archive.FileName, err)
	}
	// Read it back and check the digest: a write that reported success
	// but produced different bytes is caught here.
	content, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read back the archive %q: %w", archive.FileName, err)
	}
	sum := sha256.Sum256(content)
	if got := hex.EncodeToString(sum[:]); got != archive.SHA256 {
		return fmt.Errorf("the staged archive %q hashes to %s, want %s",
			archive.FileName, got, archive.SHA256)
	}
	return nil
}

// encodePackageExportDocument renders the export document.
//
// The shape is stable: `memories`, `packages_omitted` and
// `package_archives` are ALWAYS present, so a consumer reads the same
// three keys whatever the export carried.
func encodePackageExportDocument(
	memories []map[string]interface{},
	loss *mpminternal.SkillPackageExportLoss,
	index *packageExportIndex,
) ([]byte, error) {
	if index == nil {
		index = &packageExportIndex{Archives: []*mpminternal.PackageExportArchive{}}
	}
	if index.Archives == nil {
		index.Archives = []*mpminternal.PackageExportArchive{}
	}
	document := map[string]interface{}{
		"memories":         memories,
		"packages_omitted": loss,
		"package_archives": index,
	}
	var buf writeBuffer
	encoder := json.NewEncoder(&buf)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(document); err != nil {
		return nil, fmt.Errorf("encode the export document: %w", err)
	}
	return buf.Bytes(), nil
}

// writeBuffer is a tiny in-memory writer, so the document can be
// rendered BEFORE anything is published.
type writeBuffer struct {
	data []byte
}

func (b *writeBuffer) Write(p []byte) (int, error) {
	b.data = append(b.data, p...)
	return len(p), nil
}

func (b *writeBuffer) Bytes() []byte {
	return b.data
}

// encodeExportJSON renders a value as the export's JSON encoding.
//
// The encoder is used rather than `json.Marshal` so the output is
// byte-for-byte what the export has always written, trailing newline
// included.
func encodeExportJSON(value interface{}) ([]byte, error) {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		return nil, fmt.Errorf("encode the export document: %w", err)
	}
	return buf.Bytes(), nil
}

// encodeExportCSV renders the export's CSV encoding.
//
// The CSV is rendered into memory before it is written, so the output
// file is never truncated before the rows are known to be complete.
func encodeExportCSV(memories []map[string]interface{}) ([]byte, error) {
	var buf bytes.Buffer
	writer := csv.NewWriter(&buf)

	// CSV header
	if err := writer.Write([]string{"id", "collection", "content", "tags", "created_at", "reinforcement_count", "weight", "is_long_term"}); err != nil {
		return nil, fmt.Errorf("write the CSV header: %w", err)
	}
	for _, m := range memories {
		id := stringFromMap(m, "id")
		coll := stringFromMap(m, "collection")
		content := stringFromMap(m, "content")
		created := stringFromMap(m, "created_at")
		tags := stringFromMap(m, "tags")
		// Use strict helpers: per the D-001 / W-008 contract from
		// GetMemoriesForExport, reinforcement_count and is_long_term
		// are int64 (INTEGER columns) and weight is float64 (REAL).
		// A type mismatch now warns instead of silently defaulting.
		rcVal, _ := int64FromMap(m, "reinforcement_count")
		weightVal, _ := float64FromMap(m, "weight")
		ltVal, _ := int64FromMap(m, "is_long_term")
		// weight is real-valued; present with 2 decimals in CSV.
		weightStr := strconv.FormatFloat(weightVal, 'f', 2, 64)
		if err := writer.Write([]string{id, coll, content, tags, created,
			fmt.Sprintf("%d", rcVal), weightStr, fmt.Sprintf("%d", ltVal)}); err != nil {
			return nil, fmt.Errorf("write a CSV row: %w", err)
		}
	}
	writer.Flush()
	if err := writer.Error(); err != nil {
		return nil, fmt.Errorf("flush the CSV: %w", err)
	}
	return buf.Bytes(), nil
}

// writeFileAtomically writes a file's bytes through a temporary name and
// a rename.
//
// The output is never truncated before its content is known to be
// complete: the bytes are written to a temporary file in the same
// directory, closed, and then RENAMED over the target. A reader sees
// either the old file or the new one.
func writeFileAtomically(path string, content []byte) error {
	parent := filepath.Dir(path)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return fmt.Errorf("create the output directory %q: %w", parent, err)
	}
	temp, err := os.CreateTemp(parent, ".mpm-export-document-")
	if err != nil {
		return fmt.Errorf("stage the output file: %w", err)
	}
	tempName := temp.Name()
	published := false
	defer func() {
		if !published {
			os.Remove(tempName)
		}
	}()
	if _, err := temp.Write(content); err != nil {
		temp.Close()
		return fmt.Errorf("write the output file: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close the output file: %w", err)
	}
	if err := os.Rename(tempName, path); err != nil {
		return fmt.Errorf("publish the output file %q: %w", path, err)
	}
	published = true
	return nil
}

// skillIDsAmong returns the ids of the SKILLS among the exported
// memories, as a set.
//
// It is the bridge between the memory selection and the package
// selection: a package belongs to a skill, so the packages an export
// carries are the packages of the skills the export selected.
//
// It is a SET rather than a list, because the question it answers is
// "is this skill among the exported rows?", which is a membership
// question.
func skillIDsAmong(memories []map[string]interface{}) map[string]bool {
	ids := make(map[string]bool, len(memories))
	for _, memory := range memories {
		collection := stringFromMap(memory, "collection")
		if collection != "skills" {
			continue
		}
		id := stringFromMap(memory, "id")
		if id == "" {
			continue
		}
		ids[id] = true
	}
	return ids
}

// usererrorf is a small helper so the export's refusals read as
// operator-facing errors.
func usererrorf(format string, args ...interface{}) int {
	usererror.Error(format, args...)
	return 1
}
