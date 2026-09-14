// cmd/mpm/work_pointer.go — canonical work-ID / pointer
// normalisation shared by every `mpm work item <sub>` handler.
//
// 2026-09-14 release-pass: MPM emits work pointers in the
// canonical `mpm://work/<id>` form on the wire (see
// internal/core/work.go:65 — `Pointer` field). Operators paste
// these pointers into terminal commands; pre-fix every work
// subcommand rejected `mpm://work/<id>` with a "work not
// found" error even though `mpm work item show <id>` (bare
// id) resolved correctly. The fix centralises ID normalisation
// here so every work subcommand that accepts an ID strips the
// canonical pointer prefix in one place.
//
// Accepted forms (after stripping leading/trailing whitespace):
//
//   "d72058d7e8e71b44"               → "d72058d7e8e71b44"
//   "mpm://work/d72058d7e8e71b44"    → "d72058d7e8e71b44"
//   "mpm://work/d72058d7e8e71b44/"   → "d72058d7e8e71b44"  (trailing slash)
//   ""                                → ""
//
// Rejected forms (return the input unchanged so the handler can
// surface a clean "work not found" error rather than a stripped
// empty string):
//
//   "mpm://memory/d72058d7e8e71b44"   — wrong artifact type; NOT
//                                        silently coerced. Callers
//                                        that want pointer-aware
//                                        routing must use
//                                        `mpm_resolve` instead.
//   "mpm://work/"                      — empty id component
//   "mpm://blob/d72058d7e8e71b44"      — wrong artifact type
//
// Bare IDs that don't include the canonical pointer prefix are
// returned as-is; the resolution boundary stays a single chokepoint.
package main

import "strings"

// workPointerPrefix is the canonical URI scheme + host + path
// prefix for work artifacts. Mirrored from internal/core/work.go
// where the Pointer field is constructed.
const workPointerPrefix = "mpm://work/"

// NormalizeWorkID strips the canonical work-pointer prefix
// from id and returns the bare work ID. Returns id unchanged if
// id does not start with the prefix (it might be a bare id) or
// if id carries a different pointer scheme (the caller should
// not silently coerce a `mpm://memory/<id>` into a work lookup).
//
// This is the single chokepoint for work-ID normalisation. Every
// `mpm work item <sub>` handler that accepts an id must call
// this before passing it to the substrate. The substrate itself
// does not parse URIs; that policy lives here so the CLI does
// not re-implement prefix-stripping in every handler.
func NormalizeWorkID(id string) string {
	id = strings.TrimSpace(id)
	if !strings.HasPrefix(id, workPointerPrefix) {
		return id
	}
	stripped := strings.TrimPrefix(id, workPointerPrefix)
	stripped = strings.TrimSuffix(stripped, "/")
	stripped = strings.TrimSpace(stripped)
	// An empty id after stripping (e.g. "mpm://work/" or
	// "mpm://work/  ") is a malformed pointer. Return the
	// original input so the handler can surface "work not
	// found" against the original form rather than an empty
	// string lookup.
	if stripped == "" {
		return id
	}
	return stripped
}

// IsWorkPointer reports whether id carries the canonical
// `mpm://work/` prefix. Useful for callers that want to render
// the input form verbatim without re-stripping.
func IsWorkPointer(id string) bool {
	return strings.HasPrefix(strings.TrimSpace(id), workPointerPrefix)
}
