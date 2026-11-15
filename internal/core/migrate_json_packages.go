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
// The check reads the `package_archives` key the export writes and its
// count. A document that does not carry the key is not affected.

import (
	"bytes"
	"encoding/json"
)

// jsonPackageArchiveIndex reports the package-archive index a JSON
// document carries.
//
// The four return values are: how many archives the document names, the
// name of the directory that holds them, whether the document carried
// the key at all, and whether the index could be READ.
//
// The distinction between the last two is the whole point of the
// function, and it is the fail-closed direction:
//
//   - the key is **absent** — the document carries no archive index, so
//     there is nothing to drop, and the document is imported normally;
//   - the key is **present and readable** with a count of zero — a
//     complete export of a workspace with no packages, imported
//     normally;
//   - the key is **present and its count cannot be read** — the
//     document is MALFORMED, and the consumer cannot tell how many
//     packages it is carrying. The answer is "package-bearing", because
//     the safe reading of an unreadable index is the one that refuses.
//
// The malformed case is the reason this function does not simply
// `json.Unmarshal` into a struct and move on: an unmarshal failure
// would leave the count at zero, the guard would not fire, and the
// projected rows would be imported with the packages' content left
// behind. That is a fail-open in a fail-closed design.
func jsonPackageArchiveIndex(content []byte) (count int, directory string, present bool, readable bool) {
	trimmed := bytes.TrimSpace(content)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return 0, "", false, true
	}

	// The key's presence is decided on the RAW document, before any
	// typed unmarshal: a typed unmarshal of the whole document fails
	// for reasons that have nothing to do with the archive index (a
	// malformed `memories` entry, say), and that failure must not be
	// read as "there is no index here".
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &raw); err != nil {
		// The document cannot be parsed at all. The parser refuses it
		// on its own terms; this helper says nothing about it.
		return 0, "", false, true
	}
	indexJSON, ok := raw["package_archives"]
	if !ok {
		return 0, "", false, true
	}

	// The key is present. Its count must now be readable, or the
	// document is treated as package-bearing.
	var index struct {
		Count     *int   `json:"count"`
		Directory string `json:"directory"`
	}
	if err := json.Unmarshal(indexJSON, &index); err != nil {
		// The index is present but malformed. The count cannot be
		// read, so the number of packages the document carries is
		// unknown — and an unknown number is not zero.
		return 0, "", true, false
	}
	if index.Count == nil {
		// The index is present but carries no count. Same reasoning:
		// the count is unknown, so the document is package-bearing.
		return 0, index.Directory, true, false
	}
	return *index.Count, index.Directory, true, true
}
