// Package internal — definition.go
//
// Centralized eligibility for persona/mode definition files.
//
// Two parallel loaders historically handled the same .md files with
// subtly different rules (PersonaManager / ModeManager in persona.go and
// mode.go; the router loader in router_loader.go). The most consequential
// drift was documentation-file filtering: a README.md with valid-looking
// frontmatter (e.g. `name: README`) was a latent selectable persona.
//
// This file is the single source of truth for "is this entry a selectable
// definition?". All three loaders MUST consult IsDefinitionFile before
// attempting to parse the file. The contract is:
//
//	.selectable  =  .md extension AND NOT documentation AND parseable schema
//
// Documentation entries are unconditional: they are NEVER selectable,
// regardless of frontmatter content. This pins the fragile case where a
// README carries valid-looking YAML metadata.
package internal

import (
	"path/filepath"
	"strings"
)

// Documentation stems — case-insensitive. Any .md file whose filename
// (without extension) matches one of these is treated as documentation
// and excluded from selectable discovery.
//
// README is the canonical case; supporting variants (readme.md,
// README.txt, etc.) are pre-emptively excluded so a future filename
// change cannot accidentally re-introduce them as selectable
// definitions.
var documentationStems = map[string]bool{
	"readme": true,
}

// IsDocumentationFile reports whether the given entry name refers to a
// documentation file that must NEVER be surfaced as a selectable persona
// or mode definition. The check is case-insensitive on the filename
// stem (name without extension) and is unconditional: even if the file
// carries valid-looking frontmatter (`name: README`), it remains
// documentation.
//
// The function accepts bare filenames ("README.md") and full paths
// ("/path/to/README.md") interchangeably.
func IsDocumentationFile(name string) bool {
	if name == "" {
		return false
	}
	stem := strings.ToLower(strings.TrimSuffix(filepath.Base(name), filepath.Ext(name)))
	return documentationStems[stem]
}

// IsDefinitionFile reports whether the entry is a candidate selectable
// definition. A definition is:
//
//   - a .md file (Markdown schema is the only supported format),
//   - not a documentation file (see IsDocumentationFile),
//   - in any other case excluded.
//
// The function does not validate the file's frontmatter or body — that
// is the parser's job. IsDefinitionFile is the cheap pre-filter that
// loaders use to decide whether to attempt parsing at all.
func IsDefinitionFile(name string) bool {
	if name == "" {
		return false
	}
	if filepath.Ext(name) != ".md" {
		return false
	}
	return !IsDocumentationFile(name)
}