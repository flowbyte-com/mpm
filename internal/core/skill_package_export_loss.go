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

	// Reason names WHY the package was not carried.
	//
	// It is the difference between "this package is missing" and "this
	// package is missing BECAUSE its blob is gone", and only the
	// second is actionable. The vocabulary is closed:
	//
	//   - `integrity_failure` — the package did not verify, so its
	//     bytes cannot be exported;
	//   - `package_not_found` — the package row is gone;
	//   - `file_name_collision` — another package in the same export
	//     derives the same published file name;
	//   - `skill_row_not_exported` — the package is in the export's
	//     scope, but its skill row is not among the exported rows;
	//   - `database_failure` — the substrate failed;
	//   - `export_failed` — anything else.
	Reason string `json:"reason,omitempty"`

	// Detail is the human-readable explanation.
	Detail string `json:"detail,omitempty"`
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

	// Scope reports the export's coverage, so a consumer can tell
	// "nothing was omitted out of nothing" from "nothing was omitted
	// out of twelve".
	//
	// A zero Count is a positive statement only in the light of the
	// scope: zero omitted out of twelve selected packages is a
	// COMPLETE export; zero omitted out of zero selected packages says
	// nothing about the packages that were never selected.
	Scope SkillPackageExportScope `json:"scope"`
}

// SkillPackageExportScope reports what an export covered.
//
// The three numbers are the ones the loss marker's meaning depends on.
type SkillPackageExportScope struct {
	// Total is the number of packages in the store — every package,
	// whatever the export's filters selected.
	Total int `json:"total"`

	// Selected is the number of packages the export's filters chose.
	// It is the denominator of the omission count.
	Selected int `json:"selected"`

	// Carried is the number of packages the export actually carried.
	// It is the number of `.skill` archives the export produced.
	Carried int `json:"carried"`
}

// SkillPackageExportScopeResult reports the packages an export's SCOPE
// covers.
//
// It is the input to the export's decision: a package in the scope is a
// package the export is about, and therefore a package whose archive it
// must carry — or must name as omitted.
type SkillPackageExportScopeResult struct {
	// Packages are the packages in the scope, in a stable order.
	Packages []SkillPackageExportOmitted

	// Total is the number of packages in the store, whatever the
	// export's filters selected. It is the denominator that makes an
	// omission count readable: a scope of three out of twelve says the
	// date filter chose three, not that there are three.
	Total int
}

// SkillPackageExportScope reports the packages an export's scope
// covers.
//
// The scope mirrors GetMemoriesForExport exactly — the same collection,
// since and until filters and the same expiry clause — so the two
// surfaces cannot disagree about which packages this export is about.
//
// Before P7 this method reported the packages the export did NOT carry,
// which was every package in scope, because the export carried none.
// P7 makes the export COMPLETE, so the scope and the omissions are
// different things: the scope is what the export is ABOUT, and the
// omissions are what it could not carry. The caller exports the scope
// and then reports the difference.
func (dm *DatabaseManager) SkillPackageExportScope(collection, since, until string) (*SkillPackageExportScopeResult, error) {
	scope := &SkillPackageExportScopeResult{Packages: []SkillPackageExportOmitted{}}

	// The skills collection is the only one whose export can lose a
	// package.
	if collection != "" && collection != "skills" {
		return scope, nil
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
		return nil, fmt.Errorf("query skill package export scope: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var omitted SkillPackageExportOmitted
		if err := rows.Scan(&omitted.SkillID, &omitted.Name, &omitted.Version,
			&omitted.PackageHash, &omitted.FileCount); err != nil {
			return nil, fmt.Errorf("scan skill package export scope: %w", err)
		}
		scope.Packages = append(scope.Packages, omitted)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate skill package export scope: %w", err)
	}

	sort.SliceStable(scope.Packages, func(i, j int) bool {
		return scope.Packages[i].SkillID < scope.Packages[j].SkillID
	})

	// The total: every package in the store, whatever the filters
	// chose. It is a second, unfiltered count.
	total, err := dm.countSkillPackages()
	if err != nil {
		return nil, err
	}
	scope.Total = total
	return scope, nil
}

// countSkillPackages counts every package in the store.
func (dm *DatabaseManager) countSkillPackages() (int, error) {
	var total int
	if err := dm.db.QueryRow(`SELECT COUNT(*) FROM skill_packages`).Scan(&total); err != nil {
		return 0, fmt.Errorf("count skill packages: %w", err)
	}
	return total, nil
}

// Notice renders the loss marker as the operator-visible text.
//
// The text NAMES every affected package, because the marker is not a
// warning that can be missed (§6.3).
//
// It names every package the export did NOT carry, and — since P7 —
// the reason it could not be carried, because "this package is missing"
// and "this package is missing BECAUSE its blob is gone" are different
// facts and only the second is actionable.
//
// The opening sentence depends on WHY the package was not carried. For
// most reasons the projected row WAS exported and the package's files
// were not; for `skill_row_not_exported` the row was not exported
// either, and saying otherwise would misdescribe what the operator has.
func (l *SkillPackageExportLoss) Notice() string {
	if l == nil {
		return ""
	}
	if l.Count == 0 {
		// Nothing was lost, so there is no WARNING. The scope is
		// still stated: "carried 12 of 12" is a positive claim about
		// a known denominator, and it is what distinguishes a
		// COMPLETE export from an empty one.
		if l.Scope.Selected == 0 {
			return ""
		}
		return fmt.Sprintf(
			"this export carried %d of %d selected skill package(s); %d package(s) are in the store",
			l.Scope.Carried, l.Scope.Selected, l.Scope.Total)
	}
	lines := make([]string, 0, l.Count+2)
	lines = append(lines, fmt.Sprintf(
		"WARNING: this export could NOT carry %d skill package(s). %s",
		l.Count, l.omissionSummary()))
	if l.Scope.Total > 0 || l.Scope.Selected > 0 {
		lines = append(lines, fmt.Sprintf(
			"  scope: %d package(s) in the store, %d selected by this export, %d carried",
			l.Scope.Total, l.Scope.Selected, l.Scope.Carried))
	}
	for _, p := range l.Packages {
		reason := p.Reason
		if reason == "" {
			reason = "export_failed"
		}
		lines = append(lines, fmt.Sprintf(
			"  - %s (v%s, %d file(s), package_hash %s) — %s",
			p.SkillID, p.Version, p.FileCount, p.PackageHash, reason))
	}
	return strings.Join(lines, "\n")
}

// omissionSummary renders the one sentence that says WHAT was not
// carried.
//
// The sentence is per-reason, because the reasons differ in what the
// operator is missing: `skill_row_not_exported` means the projected
// row is missing too, and every other reason means the row is there
// and the package's files are not.
func (l *SkillPackageExportLoss) omissionSummary() string {
	for _, p := range l.Packages {
		if p.Reason == "skill_row_not_exported" {
			return "The projected skill rows were NOT exported for them either. " +
				"These bytes are not in this export."
		}
	}
	return "The projected skill rows were exported; the packages' reference files, " +
		"scripts and assets were NOT. These bytes are not in this export."
}
