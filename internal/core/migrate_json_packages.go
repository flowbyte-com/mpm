package internal

// migrate_json_packages.go — the migration consumer's refusal on a
// document that carries package archives.
//
// Design of record: PR #222 §6.3 (the export contract: no silent loss).
//
// P7's skills export writes one `.skill` archive per packaged skill,
// BESIDE the export document, and names them in the document's
// `package_archives` index. The memory migration reads the document's
// `memories` array — the PROJECTED skill rows — and nothing else.
//
// So importing such a document would write the projected rows and
// SILENTLY DROP every package's reference files, scripts and assets.
// Those bytes exist nowhere else: the archive is the transport, and a
// dropped package is a lost package.
//
// The consumer therefore REFUSES, with a diagnostic that says what is
// missing and where it is. It is the fail-closed choice, and the
// alternative — implementing the complete import path — belongs with
// the package import surface (P8), not with the memory migration.
//
// The check is a NUDGE on the document's shape, not a second parser: it
// looks for the `package_archives` key the export writes and reads its
// count. A document that does not carry the key is not affected.

import (
	"bytes"
	"encoding/json"
)

// jsonPackageArchiveIndex reports the package-archive index a JSON
// document carries.
//
// The three return values are: how many archives the document names,
// the name of the directory that holds them, and whether the document
// carried the key at all. A document that carries the key with a count
// of zero is NOT a package-bearing document — it is a complete export
// of a workspace with no packages — and it is imported normally.
func jsonPackageArchiveIndex(content []byte) (int, string, bool) {
	trimmed := bytes.TrimSpace(content)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return 0, "", false
	}
	var probe struct {
		PackageArchives *struct {
			Count     *int   `json:"count"`
			Directory string `json:"directory"`
		} `json:"package_archives"`
	}
	if err := json.Unmarshal(trimmed, &probe); err != nil {
		// A document this consumer cannot parse is refused by the
		// parser, not here. The check is a nudge, never a second
		// verdict.
		return 0, "", false
	}
	if probe.PackageArchives == nil {
		return 0, "", false
	}
	count := 0
	if probe.PackageArchives.Count != nil {
		count = *probe.PackageArchives.Count
	}
	return count, probe.PackageArchives.Directory, true
}
