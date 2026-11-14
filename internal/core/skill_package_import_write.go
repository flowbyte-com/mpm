// skill_package_import_write.go — the scanning, the projection derivation
// and the single-transaction write of the package importer.
//
// Design of record: PR #222 §5.2 (version handling), §5.5 (the manifest),
// §5.7 (duplicate detection), §7.2 (the fail-closed scanner contract),
// §7.3 (scanner coverage by file class), §7 threat 17 (atomicity) and
// threat 18 (a poisoned reference rolls the whole import back).

package internal

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// ─────────────────────────────────────────────────────────────────────────────
// SCANNING
// ─────────────────────────────────────────────────────────────────────────────

// poisonScannerAvailable reports whether the poison scanner can load the
// operator's toxic-phrase list.
//
// The poison scanner reads an OPERATOR-EDITABLE file, so it is the one
// scanner that can fail open: a phrase file that cannot be read means
// the operator's phrases are not being applied, and a scan that did not
// run must never be reported as a scan that passed (§7.2, R10, T43).
//
// The check is deliberately made against the FILE, not against the
// scanner's cache: the scanner caches its phrase list per path, so a
// cached list could mask a phrase file that has since become unreadable.
//
// A phrase file that does not exist is NOT an unavailability: the scanner
// seeds it with the built-in defaults on first use, which is the normal
// first-run state.
func poisonScannerAvailable(path string) error {
	info, err := os.Stat(path)
	switch {
	case err == nil:
		if info.IsDir() {
			return fmt.Errorf("the toxic-phrase configuration %q is a directory", path)
		}
	case errors.Is(err, os.ErrNotExist):
		// The scanner seeds the defaults on first use. That is the
		// normal first-run state, not an unavailability.
		return nil
	default:
		return fmt.Errorf("the toxic-phrase configuration %q could not be inspected: %w", path, err)
	}

	// The file must be READABLE, and that is checked by OPENING it.
	//
	// The scanner's own loader fails OPEN here: loadPoisonPhrasesFromFile
	// substitutes the built-in defaults whenever the file cannot be
	// read, so an operator's phrase list that has been locked down (or
	// made unreadable by a permission change) would silently stop
	// applying — and the scan would report a pass it did not perform.
	// The open is the check that catches it (§7.2, R10, T43).
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("the toxic-phrase configuration %q could not be read: %w", path, err)
	}
	defer f.Close()

	// The list the scanner will actually use must be non-empty. An empty
	// list would match nothing and the scan would pass everything.
	phrases, err := loadPoisonPhrasesAt(path)
	if err != nil {
		return fmt.Errorf("the toxic-phrase configuration %q could not be loaded: %w", path, err)
	}
	if len(phrases) == 0 {
		return fmt.Errorf("the toxic-phrase configuration %q yielded no phrases", path)
	}
	return nil
}

// scanPackageFile scans one file and records what actually ran.
//
// The scan status is TRUTHFUL: a scanner that did not run is recorded as
// false, and a scanner that refused the content refuses the WHOLE import
// (§5.5, §7.2, §7.3).
//
// For a binary asset the poison scan does not apply — the poison scanner
// matches prose phrases, and running it over arbitrary binary bytes
// produces meaningless output — so the manifest records
// `{"secret": true, "poison": false}`: a DISCLOSED SKIP, not a claim of
// safety.
func (dm *DatabaseManager) scanPackageFile(f *packageImportFile, phrasesPath string, allowBinary bool) error {
	contentType, class, executable, err := packageContentTypeFor(f.canonicalPath)
	if err != nil {
		return err
	}
	f.contentType = contentType
	f.executable = executable
	f.role = PackageRoleForPath(packageEntrypointName, f.canonicalPath)

	// The executable check applies to EVERY file, whatever its extension
	// claims: a file named `notes.md` whose bytes are an ELF header is an
	// executable (§7 threat 16).
	if magic := packageExecutableMagic(f.content); magic != "" {
		if !allowBinary {
			return packageImportErrorf(PackageImportReasonBinaryNotAllowed, f.canonicalPath,
				"the content is a %s executable binary; executable binaries are refused", magic)
		}
		class = packageContentExecutable
	} else if err := packageSniffContentType(f.canonicalPath, contentType, class, f.content); err != nil {
		return err
	}

	if class != packageContentText && !allowBinary {
		return packageImportErrorf(PackageImportReasonBinaryNotAllowed, f.canonicalPath,
			"the file is a binary asset (%s); binary assets are refused unless the operator opts in",
			contentType)
	}

	// The secret scanner runs on EVERY file. The substrate exposes one
	// secret scanner, and it is in-process, so it cannot fail open.
	if sensitive, reason := isSensitiveContent(string(f.content)); sensitive {
		return packageImportErrorf(PackageImportReasonScannerSecret, f.canonicalPath,
			"the secret scanner rejected the content: %s", reason)
	}

	// The poison scanner runs on every TEXT file. A binary file records
	// a disclosed skip.
	if class == packageContentText {
		if poisoned, reason := isPoisonedAt(string(f.content), phrasesPath); poisoned {
			return packageImportErrorf(PackageImportReasonScannerPoison, f.canonicalPath,
				"the poison scanner rejected the content: %s", reason)
		}
		f.scanned = PackageScanStatus{Secret: true, Poison: true}
		return nil
	}

	f.scanned = PackageScanStatus{Secret: true, Poison: false}
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// THE DERIVED PROJECTION
// ─────────────────────────────────────────────────────────────────────────────

// parsePackageFrontmatter parses a package entrypoint's frontmatter
// WITHOUT requiring a version.
//
// It is ParseSkillFrontmatter with the version requirement removed,
// because a package that declares no version is a supported case (§5.2)
// and the substrate's parser refuses a document with no version.
func parsePackageFrontmatter(content string) (SkillFrontmatter, error) {
	const fence = "---"
	if !strings.HasPrefix(content, fence+"\n") {
		return SkillFrontmatter{}, fmt.Errorf("frontmatter missing: must start with %q", fence)
	}
	rest := strings.TrimPrefix(content, fence+"\n")
	idx := strings.Index(rest, "\n"+fence)
	if idx < 0 {
		return SkillFrontmatter{}, fmt.Errorf("frontmatter unterminated")
	}
	yamlBlock := rest[:idx]

	var fm SkillFrontmatter
	if err := yaml.Unmarshal([]byte(yamlBlock), &fm); err != nil {
		return SkillFrontmatter{}, fmt.Errorf("parse frontmatter: %w", err)
	}
	if fm.Name == "" {
		return SkillFrontmatter{}, fmt.Errorf("frontmatter missing required field: name")
	}
	return fm, nil
}

// derivePackageProjection derives the projected skill document from the
// ORIGINAL entrypoint bytes.
//
// The third-party file is NEVER rewritten (§5.2). What is derived is the
// `memories` projection — the row discovery reads — and it differs from
// the original by exactly ONE inserted frontmatter line, and only when
// the package declares no version.
//
// The warnings are non-blocking. A missing `when_to_use` is a WARNING,
// not a block (O10 — provisional: the design's recommended behaviour,
// pending the operator's decision).
func derivePackageProjection(entrypoint []byte, versionOverride string) (string, *string, string, []string, error) {
	content := string(entrypoint)

	fm, err := parsePackageFrontmatter(content)
	if err != nil {
		return "", nil, "", nil, packageImportErrorf(PackageImportReasonFrontmatterInvalid,
			packageEntrypointName, "%v", err)
	}

	var warnings []string
	if fm.WhenToUse == "" {
		warnings = append(warnings, "missing_when_to_use")
	} else if len(fm.WhenToUse) < 30 {
		warnings = append(warnings, "weak_when_to_use_short")
	}
	if fm.Description == "" {
		warnings = append(warnings, "missing_description")
	}

	var declaredVersion *string
	if fm.Version != "" {
		v := fm.Version
		declaredVersion = &v
		if versionOverride != "" {
			// The declared version IS the effective version (§5.2).
			// Silently ignoring the operator's override would be worse
			// than refusing it.
			return "", nil, "", nil, packageImportErrorf(PackageImportReasonVersionOverrideConflict,
				packageEntrypointName,
				"the package declares version %q, so the effective-version override %q does not apply",
				fm.Version, versionOverride)
		}
	}

	projection := content
	if declaredVersion == nil {
		projection, err = insertProjectionVersion(content, versionOverride)
		if err != nil {
			return "", nil, "", nil, err
		}
	}
	return fm.Name, declaredVersion, projection, warnings, nil
}

// insertProjectionVersion inserts the effective version into the
// projection's frontmatter.
//
// The insertion is ONE line, immediately after the opening fence, and
// everything else is preserved byte for byte: the third-party document is
// never rewritten, and the projection differs from it by exactly that one
// line (§5.2).
//
// The line goes at the TOP of the frontmatter block rather than after the
// `name:` key on purpose: a new top-level key at the head of a YAML
// mapping is always well-formed, whatever the original block's shape,
// whereas an insertion in the middle of the block could land inside a
// nested structure.
func insertProjectionVersion(content, version string) (string, error) {
	const fence = "---"
	if !strings.HasPrefix(content, fence+"\n") {
		return "", packageImportErrorf(PackageImportReasonFrontmatterInvalid, packageEntrypointName,
			"the entrypoint has no frontmatter to add a version to")
	}
	rest := strings.TrimPrefix(content, fence+"\n")
	if idx := strings.Index(rest, "\n"+fence); idx < 0 {
		return "", packageImportErrorf(PackageImportReasonFrontmatterInvalid, packageEntrypointName,
			"the entrypoint's frontmatter is unterminated")
	}
	effective := version
	if effective == "" {
		effective = packageDefaultUnversionedVersion
	}
	return fence + "\nversion: " + effective + "\n" + rest, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// THE SINGLE TRANSACTION
// ─────────────────────────────────────────────────────────────────────────────

// errPackageImportNoWrite is the sentinel that tells WithTx's callback
// wrapper "this import needs no write".
//
// It is NOT a failure: the callback returns it to leave the transaction
// with nothing to commit, and the caller recognises it and reports the
// idempotent outcome.
var errPackageImportNoWrite = errors.New("package import: no write required")

// commitPackageImport performs the import in ONE transaction.
//
// The transaction covers the projected skill row, the prior versions'
// is_latest changes, the content-addressed blob rows, the package
// manifest row, the per-file rows and the transactional provenance
// (§7 threat 17).
//
// The row order is the foreign-key order: the blobs first (the file rows
// reference them), then the projected skill row (the package row
// references it), then the package row (the file rows reference it), then
// the file rows.
func (dm *DatabaseManager) commitPackageImport(plan *packageImportPlan, author string, ac ActiveContext) (*PackageImportResult, error) {
	skillID, err := SkillIDForNameAndVersion(plan.name, plan.effectiveVersion)
	if err != nil {
		return nil, packageImportErrorf(PackageImportReasonNameInvalid, packageEntrypointName, "%v", err)
	}

	result := &PackageImportResult{
		SkillID:          skillID,
		Name:             plan.name,
		Version:          plan.effectiveVersion,
		DeclaredVersion:  plan.declaredVersion,
		VersionSource:    plan.versionSource,
		PackageHash:      plan.packageHash,
		Manifest:         plan.manifest,
		Entrypoint:       packageEntrypointName,
		EntrypointSHA256: plan.entrypointSHA256,
		ProjectionSHA256: plan.projectionSHA256,
		Warnings:         plan.warnings,
	}

	err = dm.WithTx(func(node DBNode) error {
		tx := node.Tx()
		if tx == nil {
			return fmt.Errorf("package import: a transaction is required")
		}

		// ── The idempotence and conflict checks run INSIDE the
		// transaction, against the same snapshot the writes land in.
		existingHash, err := lookupPackageHashTx(tx, skillID)
		if err != nil {
			return err
		}
		tombstoned, live, err := lookupSkillRowStateTx(tx, skillID)
		if err != nil {
			return err
		}

		switch {
		case existingHash != "":
			if existingHash != plan.packageHash {
				return packageImportErrorf(PackageImportReasonVersionConflict, packageEntrypointName,
					"skill %s is already published with a different package_hash (%s); a published package is immutable",
					skillID, existingHash)
			}
			if live {
				// The same package is already installed. Nothing is
				// written (§5.7).
				result.Status = PackageImportStatusAlreadyInstalled
				return errPackageImportNoWrite
			}
			// The package was soft-shredded. The re-import RESURRECTS
			// it (§5.7): the import path is not the legacy force path,
			// so R1's refusal does not apply to it.
			if err := dm.resurrectPackageProjection(tx, skillID, plan, author, ac); err != nil {
				return err
			}
			result.Status = PackageImportStatusResurrected
			return nil

		case live:
			// The id is taken by a skill that owns NO package. The
			// importer will not overwrite an ordinary skill: the
			// identity space is shared, and overwriting it in place
			// would be exactly the mutation §5.8 refuses.
			return packageImportErrorf(PackageImportReasonSkillIDConflict, packageEntrypointName,
				"skill %s already exists and is not a package; delete it first, or publish a new version", skillID)

		case tombstoned:
			// A soft-deleted skill owns the id, and it owns no package
			// (the package case is handled above). The import revives
			// it with the package's projection.
			//
			// This is the substrate's existing resurrection semantics,
			// not an invention: the legacy force path has always
			// revived a soft-deleted ordinary skill, and §5.7 says the
			// import path is not the legacy force path precisely so
			// that this revival is allowed.
			//
			// Nothing live is overwritten: the row was already
			// deleted, so there is no live document to lose. A LIVE
			// ordinary skill occupying the id is refused above.
			if err := dm.resurrectPackageProjection(tx, skillID, plan, author, ac); err != nil {
				return err
			}
			result.Status = PackageImportStatusResurrected
			if err := dm.writePackageRowsTx(tx, skillID, plan); err != nil {
				return err
			}
			return nil
		}

		// ── The fresh path. ────────────────────────────────────────────
		// The projection goes through the P3 tx-aware writer, so the
		// scanner guarantees, the content hash, the FTS indexing and the
		// artifact provenance all land inside this transaction.
		if _, err := dm.SaveSkillProjectionInTx(tx, plan.name, plan.effectiveVersion,
			plan.projection, author, ac); err != nil {
			return packageImportErrorf(PackageImportReasonFrontmatterInvalid, packageEntrypointName,
				"the projected skill document was refused: %v", err)
		}

		if err := dm.writePackageRowsTx(tx, skillID, plan); err != nil {
			return err
		}
		result.Status = PackageImportStatusImported
		return nil
	})

	if err != nil {
		if !errors.Is(err, errPackageImportNoWrite) {
			return nil, err
		}
		// The idempotent outcome: nothing was written, and the
		// transaction was rolled back cleanly. The package's rows are
		// read back anyway, so the response describes the package that
		// is actually installed (§5.7).
	}

	// The package's file rows are read back AFTER the commit. The
	// read-back is not optional: the P3 transaction writer defers its own
	// assertion until the commit, and a commit that landed fewer rows
	// than the import wrote is a silent loss.
	files, err := dm.packageManifestFiles(result.SkillID)
	if err != nil {
		return nil, fmt.Errorf("package import read-back: %w", err)
	}
	result.Files = files

	// The advisory is computed against the committed state, because the
	// other package it may share content with can be imported at any
	// time. It is advisory: a failure to compute it does not undo an
	// import that has already landed, so it is reported as a warning.
	advisories, advErr := dm.packageDuplicateContentAdvisories(result.SkillID, result.Files)
	if advErr != nil {
		result.Warnings = append(result.Warnings,
			fmt.Sprintf("advisory_unavailable: the duplicate-content advisory could not be computed: %v", advErr))
	} else {
		result.Advisories = advisories
	}
	return result, nil
}

// packageDuplicateContentAdvisories reports the ADVISORY the design
// names for content that is shared with another package (§5.7).
//
// "A different skill name with the same entrypoint hash | Advisory only:
// the response carries `possible_duplicate_content`. It does not block."
//
// It is an advisory and not a refusal because the same bytes under two
// names is not evidence of anything: a package may legitimately share a
// boilerplate reference file with another package, and the blob is
// deduplicated anyway.
func (dm *DatabaseManager) packageDuplicateContentAdvisories(skillID string, files []PackageManifestFile) ([]string, error) {
	if len(files) == 0 {
		return nil, nil
	}
	hashes := make([]interface{}, 0, len(files))
	for _, f := range files {
		hashes = append(hashes, f.SHA256)
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(hashes)), ",")

	// The other packages that reference one of this package's blobs.
	rows, err := dm.db.Query(`
		SELECT DISTINCT other.skill_id
		FROM skill_package_files other
		JOIN skill_packages p ON p.skill_id = other.skill_id
		WHERE other.skill_id != ?
		  AND other.sha256 IN (`+placeholders+`)
		ORDER BY other.skill_id
	`, append([]interface{}{skillID}, hashes...)...)
	if err != nil {
		return nil, fmt.Errorf("query the packages sharing this content: %w", err)
	}
	defer rows.Close()

	var others []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("read a package that shares this content: %w", err)
		}
		others = append(others, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate the packages that share this content: %w", err)
	}
	if len(others) == 0 {
		return nil, nil
	}
	return []string{fmt.Sprintf("possible_duplicate_content: %d other package(s) share this package's content (%s)",
		len(others), strings.Join(others, ", "))}, nil
}

// resurrectPackageProjection revives the skill row for a package import.
//
// It is the import path's revival: the row carries either no package at
// all, or a package whose hash is UNCHANGED, so the id never ends up
// carrying two documents.
func (dm *DatabaseManager) resurrectPackageProjection(tx *sql.Tx, skillID string, plan *packageImportPlan, author string, ac ActiveContext) error {
	if _, err := dm.ResurrectSkillProjectionInTx(tx, plan.name, plan.effectiveVersion,
		plan.projection, author, ac); err != nil {
		// A provenance failure is NOT a frontmatter failure, and the two
		// must not share a reason code: one says "the document was
		// refused", the other says "the attribution could not be
		// written, so the resurrection is rolled back".
		if errors.Is(err, errSkillProvenanceRefresh) {
			return packageImportErrorf(PackageImportReasonProvenanceFailed, packageEntrypointName,
				"the resurrected skill's provenance could not be refreshed: %v", err)
		}
		return packageImportErrorf(PackageImportReasonFrontmatterInvalid, packageEntrypointName,
			"the projected skill document was refused: %v", err)
	}
	return nil
}

// writePackageRowsTx writes the blob rows, the package row and the file
// rows, in foreign-key order.
func (dm *DatabaseManager) writePackageRowsTx(tx *sql.Tx, skillID string, plan *packageImportPlan) error {
	node := &txNode{tx: tx, dm: dm}

	// ── The blobs. A blob whose hash already exists is SHARED, not
	// duplicated (§5.7): the same bytes are stored once.
	for i := range plan.files {
		f := &plan.files[i]
		if _, err := node.ExecTracked(`
			INSERT INTO skill_package_blobs (sha256, size_bytes, content)
			VALUES (?, ?, ?)
			ON CONFLICT(sha256) DO NOTHING
		`, 5, f.sha256, f.sizeBytes, f.content); err != nil {
			return fmt.Errorf("insert package blob: %w", err)
		}
		// Read back: an INSERT that did not fail is not proof that the
		// row is there.
		var blobSize int64
		if err := node.QueryRowTracked(`SELECT size_bytes FROM skill_package_blobs WHERE sha256 = ?`,
			f.sha256).Scan(&blobSize); err != nil {
			return fmt.Errorf("read back package blob %s: %w", f.sha256, err)
		}
		if blobSize != f.sizeBytes {
			return fmt.Errorf("package blob %s read back with size %d, want %d", f.sha256, blobSize, f.sizeBytes)
		}
	}

	// ── The package row.
	declared := any(nil)
	if plan.declaredVersion != nil {
		declared = *plan.declaredVersion
	}
	if _, err := node.ExecTracked(`
		INSERT INTO skill_packages (
			skill_id, name, version, declared_version, version_source, format,
			entrypoint, entrypoint_sha256, manifest, package_hash, source_sha256, is_global
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, 0)
	`, 5,
		skillID, plan.name, plan.effectiveVersion, declared, plan.versionSource,
		PackageFormatV1, packageEntrypointName, plan.entrypointSHA256,
		plan.manifest, plan.packageHash,
	); err != nil {
		return fmt.Errorf("insert skill package: %w", err)
	}

	// ── The file rows.
	for i := range plan.files {
		f := &plan.files[i]
		if _, err := node.ExecTracked(`
			INSERT INTO skill_package_files (
				skill_id, path, sha256, size_bytes, content_type, role,
				executable, secret_scanned, poison_scanned, raw_path
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		`, 5,
			skillID, f.canonicalPath, f.sha256, f.sizeBytes, f.contentType,
			string(f.role), boolToInt(f.executable), boolToInt(f.scanned.Secret),
			boolToInt(f.scanned.Poison), f.rawPath,
		); err != nil {
			return fmt.Errorf("insert skill package file %q: %w", f.canonicalPath, err)
		}
	}
	return nil
}

// lookupPackageHashTx returns the package_hash of the package row for the
// skill id, or the empty string when there is none.
func lookupPackageHashTx(tx *sql.Tx, skillID string) (string, error) {
	var hash string
	err := tx.QueryRow(`SELECT package_hash FROM skill_packages WHERE skill_id = ?`, skillID).Scan(&hash)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("lookup skill package: %w", err)
	}
	return hash, nil
}

// lookupSkillRowStateTx reports whether the skill row is LIVE and whether
// it is TOMBSTONED.
//
// The tombstone check is deliberate: a soft-deleted skill still owns its
// id, so the row's presence decides whether the import writes a new row
// or revives the existing one.
func lookupSkillRowStateTx(tx *sql.Tx, skillID string) (tombstoned bool, live bool, err error) {
	var deletedAt sql.NullInt64
	err = tx.QueryRow(`SELECT deleted_at FROM memories WHERE id = ?`, skillID).Scan(&deletedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return false, false, nil
	}
	if err != nil {
		return false, false, fmt.Errorf("lookup skill row: %w", err)
	}
	if deletedAt.Valid {
		return true, false, nil
	}
	return false, true, nil
}

// packageManifestFiles reads back the package's file rows, in canonical
// path order.
//
// The read-back PROPAGATES its errors rather than truncating the list: a
// short list would make the post-commit verification compare a partial
// file set against the row count and report a mismatch that looks like a
// lost package.
func (dm *DatabaseManager) packageManifestFiles(skillID string) ([]PackageManifestFile, error) {
	rows, err := dm.db.Query(`
		SELECT path, sha256, size_bytes, content_type, role, executable,
		       secret_scanned, poison_scanned
		FROM skill_package_files
		WHERE skill_id = ?
		ORDER BY path
	`, skillID)
	if err != nil {
		return nil, fmt.Errorf("query the package's file rows: %w", err)
	}
	defer rows.Close()

	var out []PackageManifestFile
	for rows.Next() {
		var f PackageManifestFile
		var role string
		var executable, secret, poison int
		if err := rows.Scan(&f.Path, &f.SHA256, &f.SizeBytes, &f.ContentType,
			&role, &executable, &secret, &poison); err != nil {
			return nil, fmt.Errorf("read a package file row: %w", err)
		}
		f.Role = PackageRole(role)
		f.Executable = executable != 0
		f.Scanned = PackageScanStatus{Secret: secret != 0, Poison: poison != 0}
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate the package's file rows: %w", err)
	}
	return out, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// THE POST-COMMIT VERIFICATION
// ─────────────────────────────────────────────────────────────────────────────

// verifyPackageImport reads the committed rows back and checks them.
//
// The P3 transaction writer DEFERS its read-back assertion: an
// uncommitted row is invisible to a read outside the transaction, so the
// caller's commit is the point at which the write is proven to have
// landed. That makes the post-commit check the importer's job, and it is
// not optional — a commit that landed fewer rows than the import wrote
// is a silent loss, and the whole point of the package format is that
// the bytes are never silently lost.
func (dm *DatabaseManager) verifyPackageImport(result *PackageImportResult) error {
	if result.Status == PackageImportStatusAlreadyInstalled {
		// Nothing was written, so there is nothing to verify beyond the
		// rows that were already there.
		return nil
	}

	var packageHash, entrypointHash, version string
	err := dm.db.QueryRow(`
		SELECT package_hash, entrypoint_sha256, version
		FROM skill_packages WHERE skill_id = ?
	`, result.SkillID).Scan(&packageHash, &entrypointHash, &version)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("package import verification: the package row for %s is missing after the commit", result.SkillID)
	}
	if err != nil {
		return fmt.Errorf("package import verification: %w", err)
	}
	if packageHash != result.PackageHash {
		return fmt.Errorf("package import verification: the package row carries hash %s, want %s",
			packageHash, result.PackageHash)
	}
	if entrypointHash != result.EntrypointSHA256 {
		return fmt.Errorf("package import verification: the package row carries entrypoint hash %s, want %s",
			entrypointHash, result.EntrypointSHA256)
	}

	// The file rows.
	var fileCount int
	if err := dm.db.QueryRow(`SELECT COUNT(*) FROM skill_package_files WHERE skill_id = ?`,
		result.SkillID).Scan(&fileCount); err != nil {
		return fmt.Errorf("package import verification: %w", err)
	}
	if fileCount != len(result.Files) {
		return fmt.Errorf("package import verification: the package carries %d file rows, want %d",
			fileCount, len(result.Files))
	}

	// The blobs: every file's blob must be present, with the size the
	// file row records — AND with the content the recorded address names.
	//
	// The store is CONTENT-ADDRESSED: the sha256 column IS the address.
	// Checking presence and size alone does not establish that the
	// stored bytes hash to their recorded address, so the bytes are
	// re-hashed here. The blob is the package's single copy of those
	// bytes, and the file row's sha256 is what a later reader will use
	// to find them — a blob whose bytes do not hash to its address is a
	// corrupt package that no later read would detect.
	for _, f := range result.Files {
		var size int64
		var stored []byte
		err := dm.db.QueryRow(`SELECT size_bytes, content FROM skill_package_blobs WHERE sha256 = ?`,
			f.SHA256).Scan(&size, &stored)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("package import verification: the blob for %q is missing after the commit", f.Path)
		}
		if err != nil {
			return fmt.Errorf("package import verification: %w", err)
		}
		if size != f.SizeBytes {
			return fmt.Errorf("package import verification: the blob for %q is %d bytes, want %d",
				f.Path, size, f.SizeBytes)
		}
		if actual := sha256Hex(stored); actual != f.SHA256 {
			return fmt.Errorf("package import verification: the blob for %q hashes to %s, not the recorded %s",
				f.Path, actual, f.SHA256)
		}
	}

	// The projected skill row: it must exist, be live, and carry the
	// projection's content hash.
	var content string
	var metadata string
	var deletedAt sql.NullInt64
	err = dm.db.QueryRow(`SELECT content, COALESCE(metadata,'{}'), deleted_at FROM memories WHERE id = ?`,
		result.SkillID).Scan(&content, &metadata, &deletedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("package import verification: the projected skill row %s is missing after the commit", result.SkillID)
	}
	if err != nil {
		return fmt.Errorf("package import verification: %w", err)
	}
	if deletedAt.Valid {
		return fmt.Errorf("package import verification: the projected skill row %s is still soft-deleted", result.SkillID)
	}
	if contentHash(content) != result.ProjectionSHA256 {
		return fmt.Errorf("package import verification: the projected skill row's content hash does not match the projection")
	}
	return nil
}
