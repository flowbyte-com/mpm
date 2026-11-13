// skill_package_export_loss.go — the export loss marker (R11).
//
// Design of record: PR #222 §6.3 ("The export contract: no silent loss")
// and §13 T44.
//
// ─────────────────────────────────────────────────────────────────────────────
// WHY THIS EXISTS
//
// P4 is the FIRST stage in which a stored skill can carry files that the
// existing skills export does not include. Before full package export
// exists (that is P7), `mpm export --collection skills` carries the
// PROJECTED SKILL ROW only — and a package's reference files, scripts and
// assets would be silently dropped.
//
// The bytes exist nowhere else. The `.skill` archive is the transport
// (§5.9), and the export writer is P7. So between P4 and P7 the skills
// export IS a loss path, and the contract is that the loss is NEVER
// SILENT:
//
//   - the export RESPONSE carries a structured loss marker, naming every
//     package that was not carried and how many there are;
//   - the CLI prints a loud notice naming the same packages.
//
// There is no configuration in which the loss is silent.
//
// ─────────────────────────────────────────────────────────────────────────────
// WHY THE MARKER IS NOT A WARNING THAT CAN BE MISSED
//
// The marker names the affected skills. A caller that reads the export
// response learns that reference files were omitted and which skills were
// affected; a caller that does not read the response still sees the
// notice on the terminal.

package internal

import (
	"fmt"
	"sort"
	"strings"
)

// SkillPackageExportOmitted names ONE package the skills export did not
// carry.
type SkillPackageExportOmitted struct {
	// SkillID is the canonical `skill:<name>-v<version>` id.
	SkillID string `json:"skill_id"`

	// Name is the package's skill name.
	Name string `json:"name"`

	// Version is the package's effective version.
	Version string `json:"version"`

	// FileCount is how many files the package carries. It is the
	// number of files that were NOT included in the export, so it
	// quantifies the loss.
	FileCount int `json:"file_count"`

	// PackageHash is the package's hash. It is the identity an
	// operator would need in order to re-import the package, so it
	// makes the notice actionable rather than merely informative.
	PackageHash string `json:"package_hash"`
}

// SkillPackageExportLoss is the structured loss marker carried by a
// skills export.
//
// The type is ALWAYS present in the export response, even when nothing
// was omitted. A count of zero is a positive statement — "this export
// carried every package it could" — and it keeps the response's shape
// stable, so a consumer never has to branch on whether the field
// exists.
type SkillPackageExportLoss struct {
	// Count is the number of packages that were omitted.
	Count int `json:"count"`

	// Packages names every omitted package, in a stable order.
	Packages []SkillPackageExportOmitted `json:"packages"`
}

// GetSkillPackageExportLoss reports the packages the skills export did
// NOT carry.
//
// The scope mirrors GetMemoriesForExport exactly — the same collection,
// since and until filters, and the same expiry clause — so the marker
// names precisely the packages whose skill rows WERE exported and whose
// content was not.
//
// The loss exists only when the export's scope includes the skills
// collection. An export of another collection carries no skill rows and
// therefore loses no package.
//
// The marker is advisory, never an error: an export that omits package
// content is still a successful export, and the caller is told what it
// does not carry.
func (dm *DatabaseManager) GetSkillPackageExportLoss(collection, since, until string) (*SkillPackageExportLoss, error) {
	loss := &SkillPackageExportLoss{Packages: []SkillPackageExportOmitted{}}

	// The skills collection is the only one whose export can lose a
	// package.
	if collection != "" && collection != "skills" {
		return loss, nil
	}

	query := `
		SELECT p.skill_id, p.name, p.version, p.package_hash,
		       (SELECT COUNT(*) FROM skill_package_files f WHERE f.skill_id = p.skill_id)
		FROM skill_packages p
		JOIN memories m ON m.id = p.skill_id
		WHERE m.deleted_at IS NULL
		  AND m.collection = 'skills'
	`
	args := []interface{}{}

	if collection != "" {
		query += " AND m.collection = ?"
		args = append(args, collection)
	}
	if since != "" {
		query += " AND m.created_at >= ?"
		args = append(args, since)
	}
	if until != "" {
		query += " AND m.created_at <= ?"
		args = append(args, until+" 23:59:59")
	}
	// The expiry clause is the ALIAS-QUALIFIED variant, because the
	// query joins `memories m`. The bare form would resolve against
	// whichever table happened to carry the column — and the bare form
	// becoming ambiguous is exactly the kind of silent divergence the
	// two variants exist to prevent.
	//
	// The scope is deliberately identical to GetMemoriesForExport's: the
	// same collection, since and until filters and the same expiry
	// clause. The marker names precisely the packages whose skill rows
	// WERE exported and whose content was not — so the two surfaces
	// cannot disagree.
	query += MemoryExpireClauseM
	query += " ORDER BY p.skill_id"

	rows, err := dm.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("query skill package export loss: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var omitted SkillPackageExportOmitted
		if err := rows.Scan(&omitted.SkillID, &omitted.Name, &omitted.Version,
			&omitted.PackageHash, &omitted.FileCount); err != nil {
			return nil, fmt.Errorf("scan skill package export loss: %w", err)
		}
		loss.Packages = append(loss.Packages, omitted)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate skill package export loss: %w", err)
	}

	loss.Count = len(loss.Packages)
	sort.SliceStable(loss.Packages, func(i, j int) bool {
		return loss.Packages[i].SkillID < loss.Packages[j].SkillID
	})
	return loss, nil
}

// Notice renders the loss marker as the operator-visible text.
//
// The text NAMES every affected package, because the marker is not a
// warning that can be missed (§6.3).
func (l *SkillPackageExportLoss) Notice() string {
	if l == nil || l.Count == 0 {
		return ""
	}
	lines := make([]string, 0, l.Count+1)
	lines = append(lines, fmt.Sprintf(
		"WARNING: this export omitted the CONTENT of %d skill package(s). "+
			"The projected skill rows were exported; the packages' reference files, scripts and assets were NOT. "+
			"Full package export is not available yet, so these bytes are not in this export.",
		l.Count))
	for _, p := range l.Packages {
		lines = append(lines, fmt.Sprintf(
			"  - %s (v%s, %d file(s), package_hash %s)",
			p.SkillID, p.Version, p.FileCount, p.PackageHash))
	}
	return strings.Join(lines, "\n")
}
