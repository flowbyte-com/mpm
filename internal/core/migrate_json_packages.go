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
// The check reads the TWO numbers the document carries about its
// packages:
//
//   - `package_archives.count` — the archives the export PRODUCED;
//   - `packages_omitted.count` — the packages the export could NOT
//     carry.
//
// The second one is the one that matters most, and reading only the
// first is a fail-open: a document whose every package failed
// verification carries `package_archives.count = 0` and
// `packages_omitted.count = 1`, and a guard that reads the first number
// imports it normally — the projected rows land and the packages'
// content is left behind. The document's own statement of what it
// dropped is therefore read as well, and a document that dropped
// anything is package-bearing.
//
// The check is a NUDGE on the document's shape, not a second parser: it
// reads three keys and counts two arrays.

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// jsonPackageArchiveFacts reports what a JSON export document says
// about the skill packages it carries — and what it dropped.
//
// The two numbers are the document's own statements, and they are read
// independently: the archive count says what was PRODUCED, and the
// omission count says what was DROPPED. A document is package-bearing
// when either says there is a package in it.
//
// A malformed shape is reported as such rather than as zero, because
// an unknown number is not zero.
type jsonPackageArchiveFacts struct {
	// Archives is `package_archives.count`: the archives the document
	// says it produced. Zero when the document carries no index.
	Archives int

	// ArchivesListed is the number of archive entries the document
	// actually lists. A count that disagrees with the list is
	// malformed — the document contradicts itself.
	ArchivesListed int

	// Omitted is `packages_omitted.count`: the packages the document
	// says it could NOT carry. Zero when the document carries no
	// marker, or when it dropped nothing.
	Omitted int

	// OmittedListed is the number of omission entries the document
	// actually lists.
	OmittedListed int

	// Directory names the sibling directory the archives live in.
	Directory string

	// IndexPresent says the document carries a package-archive index
	// at all. A document without one is an older document, and the
	// omission marker alone decides it.
	IndexPresent bool

	// ScopePresent says the document carries the marker's `scope`
	// object. A pre-P7 document does not, and the scope's arithmetic
	// is therefore skipped rather than assumed.
	ScopePresent bool

	// ScopeSelected is the scope's `selected`: the number of packages
	// the export's filters chose.
	ScopeSelected int

	// ScopeCarried is the scope's `carried`: the number of packages
	// the export actually carried. It is the number of archives the
	// export produced.
	ScopeCarried int

	// Malformed says the document's package facts could not be read,
	// or contradict themselves. Reason names the problem.
	Malformed bool
	Reason    string
}

// jsonPackageArchiveFactsOf reads a document's package facts.
//
// The three keys are read from the RAW document, before any typed
// unmarshal of the whole thing: a typed unmarshal of the document
// fails for reasons that have nothing to do with the package facts (a
// malformed `memories` entry, say), and that failure must not be read
// as "there are no packages here".
func jsonPackageArchiveFactsOf(content []byte) jsonPackageArchiveFacts {
	facts := jsonPackageArchiveFacts{}

	trimmed := bytes.TrimSpace(content)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		// Not a JSON object. The parser refuses it on its own terms;
		// this helper says nothing about it.
		return facts
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &raw); err != nil {
		// The document cannot be parsed at all. Same reasoning.
		return facts
	}

	// ── The archive index. ────────────────────────────────────────────
	if indexJSON, ok := raw["package_archives"]; ok {
		facts.IndexPresent = true
		var index struct {
			Count     *int              `json:"count"`
			Directory string            `json:"directory"`
			Archives  []json.RawMessage `json:"archives"`
		}
		if err := json.Unmarshal(indexJSON, &index); err != nil {
			facts.Malformed = true
			facts.Reason = "the package-archive index is not readable"
			return facts
		}
		if index.Count == nil {
			facts.Malformed = true
			facts.Reason = "the package-archive index carries no count"
			return facts
		}
		if *index.Count < 0 {
			facts.Malformed = true
			facts.Reason = fmt.Sprintf("the package-archive index counts %d archives", *index.Count)
			return facts
		}
		if *index.Count != len(index.Archives) {
			facts.Malformed = true
			facts.Reason = fmt.Sprintf(
				"the package-archive index counts %d archive(s) but lists %d",
				*index.Count, len(index.Archives))
			return facts
		}
		facts.Archives = *index.Count
		facts.ArchivesListed = len(index.Archives)
		facts.Directory = index.Directory
	}

	// ── The omission marker. ──────────────────────────────────────────
	//
	// This is the number that says what the export DROPPED, and a
	// document that dropped anything is package-bearing — whether or
	// not it also carries an index.
	if omittedJSON, ok := raw["packages_omitted"]; ok {
		var omitted struct {
			Count    *int              `json:"count"`
			Packages []json.RawMessage `json:"packages"`
			Scope    json.RawMessage   `json:"scope"`
		}
		if err := json.Unmarshal(omittedJSON, &omitted); err != nil {
			facts.Malformed = true
			facts.Reason = "the package-omission marker is not readable"
			return facts
		}
		if omitted.Count == nil {
			facts.Malformed = true
			facts.Reason = "the package-omission marker carries no count"
			return facts
		}
		if *omitted.Count < 0 {
			facts.Malformed = true
			facts.Reason = fmt.Sprintf("the package-omission marker counts %d omitted package(s)", *omitted.Count)
			return facts
		}
		if *omitted.Count != len(omitted.Packages) {
			facts.Malformed = true
			facts.Reason = fmt.Sprintf(
				"the package-omission marker counts %d omitted package(s) but lists %d",
				*omitted.Count, len(omitted.Packages))
			return facts
		}
		facts.Omitted = *omitted.Count
		facts.OmittedListed = len(omitted.Packages)

		// ── The scope's arithmetic. ─────────────────────────────────
		//
		// The scope says how many packages the export was ABOUT
		// (`selected`), how many it CARRIED (`carried`), and the
		// marker says how many of the selected ones it could not
		// carry (`count`). Those three numbers are one identity:
		//
		//	selected == carried + omitted
		//
		// and the scope's `carried` is the same number as the
		// archive index's `count`. A document that breaks either
		// identity is contradicting ITSELF, and a contradiction about
		// how many packages it holds leaves that number unknown — so
		// it is malformed, and malformed is refused.
		//
		// Reading only the two counts was the fail-open this file
		// exists to close: a document that says "five packages were
		// selected, none were carried, none were omitted" names no
		// loss at all, and the migration would import it — leaving
		// five packages' content behind without a word.
		//
		// A document with NO scope is a pre-P7 document; the identity
		// is not checked, because there is nothing to check it
		// against.
		if scopeJSON := omitted.Scope; len(scopeJSON) > 0 && string(scopeJSON) != "null" {
			facts.ScopePresent = true
			var scope struct {
				Total    *int `json:"total"`
				Selected *int `json:"selected"`
				Carried  *int `json:"carried"`
			}
			if err := json.Unmarshal(scopeJSON, &scope); err != nil {
				facts.Malformed = true
				facts.Reason = "the package-omission marker's scope is not readable"
				return facts
			}
			if scope.Selected == nil || scope.Carried == nil {
				facts.Malformed = true
				facts.Reason = "the package-omission marker's scope carries no selected/carried counts"
				return facts
			}
			if scope.Total != nil && *scope.Total < 0 {
				facts.Malformed = true
				facts.Reason = fmt.Sprintf(
					"the package-omission marker's scope counts %d package(s) in the store",
					*scope.Total)
				return facts
			}
			if *scope.Selected < 0 || *scope.Carried < 0 {
				facts.Malformed = true
				facts.Reason = fmt.Sprintf(
					"the package-omission marker's scope counts %d selected and %d carried",
					*scope.Selected, *scope.Carried)
				return facts
			}
			facts.ScopeSelected = *scope.Selected
			facts.ScopeCarried = *scope.Carried
			if *scope.Carried != facts.Archives {
				facts.Malformed = true
				facts.Reason = fmt.Sprintf(
					"the scope says %d package(s) were carried but the archive index counts %d",
					*scope.Carried, facts.Archives)
				return facts
			}
			if *scope.Selected != *scope.Carried+facts.Omitted {
				facts.Malformed = true
				facts.Reason = fmt.Sprintf(
					"the scope says %d package(s) were selected but %d were carried and %d were omitted",
					*scope.Selected, *scope.Carried, facts.Omitted)
				return facts
			}
		}
	}

	return facts
}
