package main

import (
	"fmt"
	"strconv"
	"strings"

	mpminternal "github.com/flowbyte-com/mpm-core"
	"github.com/flowbyte-com/mpm-core/usererror"

	"github.com/sergi/go-diff/diffmatchpatch"
)

// mpm history <id> — Show version history for a memory
func handleHistory(args []string) int {
	if len(args) < 2 {
		usererror.Usage("mpm history <id>")
		return 1
	}

	id := args[1]

	dm := getDB()
	if dm == nil {
		return 1
	}

	revisions, err := dm.GetMemoryRevisions(id)
	if err != nil {
		usererror.Error("%v", err)
	}
	if len(revisions) == 0 {
		fmt.Printf("No revisions found for memory %s\n", id)
		return 0
	}

	for _, r := range revisions {
		label := fmt.Sprintf("v%d", r.Version)
		if r.Version == revisions[0].Version {
			label += "  (current)"
		}
		fmt.Printf("%s  %s\n", label, mpminternal.FormatUnixSeconds(r.CreatedAt))
	}
	return 0
}

// mpm diff <id> <v1> <v2> — Show unified diff between two versions
func handleDiff(args []string) int {
	if len(args) < 4 {
		usererror.Usage("mpm diff <id> <v1> <v2>")
		return 1
	}

	id := args[1]
	v1, err := strconv.Atoi(args[2])
	if err != nil {
		usererror.Error("v1 must be an integer, got %q", args[2])
	}
	v2, err := strconv.Atoi(args[3])
	if err != nil {
		usererror.Error("v2 must be an integer, got %q", args[3])
	}

	dm := getDB()
	if dm == nil {
		return 1
	}

	// Fetch both versions directly from memory_revisions
	type versionInfo struct {
		content  string
		revision mpminternal.MemoryRevision
	}
	versions := map[int]versionInfo{}

	revisions, err := dm.GetMemoryRevisions(id)
	if err != nil {
		usererror.Error("%v", err)
	}
	for _, r := range revisions {
		if r.Version == v1 || r.Version == v2 {
			versions[r.Version] = versionInfo{content: r.Content, revision: r}
		}
	}

	v1Info, ok1 := versions[v1]
	v2Info, ok2 := versions[v2]
	if !ok1 {
		usererror.Error("version %d not found for memory %s", v1, id)
		return 1
	}
	if !ok2 {
		usererror.Error("version %d not found for memory %s", v2, id)
		return 1
	}

	dmp := diffmatchpatch.New()
	diffs := dmp.DiffMain(v1Info.content, v2Info.content, true)
	diffs = dmp.DiffCleanupSemantic(diffs)

	fmt.Printf("--- v%d (%s)\n", v1, mpminternal.FormatUnixSeconds(v1Info.revision.CreatedAt))
	fmt.Printf("+++ v%d (%s)\n", v2, mpminternal.FormatUnixSeconds(v2Info.revision.CreatedAt))
	fmt.Print(dmp.DiffPrettyText(diffs))
	return 0
}

// mpm diff-lines <v1-content> <v2-content> — Compute line-oriented diff of two text blocks (used for testing)
//
// 2026-09-10 fix: the previous shape used DiffPrettyText on a
// character-level diff, which collapsed short inputs ("alpha" vs
// "beta") into a single concatenated line with ANSI color codes
// rather than a readable line diff. The fix uses the library's
// line-mode encoding (DiffLinesToChars → DiffMain → DiffCharsToLines)
// and renders the result with explicit `+`/`-`/` ` per-line prefixes,
// so each input line is identified as unchanged, removed, or
// inserted.
//
// 2026-09-11 follow-up (T82): the 2026-09-10 fix was structurally
// correct (line-mode encoding) but the render loop still iterated
// over individual bytes inside d.Text, prefixing every byte. The
// output for `diff-lines alpha beta` was therefore `-a-l-p-h-a+b+e+t+a`
// — a per-character concatenation rather than a per-line prefix.
// This second pass writes one prefix per decoded line and appends a
// newline after each so the rendered shape is:
//
//   -alpha
//   +beta
//
// which is what `mpm debug diff-lines` documented as producing.
//
// The character-level DiffPrettyText path is preserved in handleDiff
// (above), which is for memory version diffs where the content is
// typically a full document and semantic cleanup is meaningful.
func handleDiffLines(args []string) int {
	if len(args) < 3 {
		usererror.Usage("mpm diff-lines <v1-content> <v2-content>")
		return 1
	}

	dmp := diffmatchpatch.New()
	// Encode each input line as a single rune so DiffMain operates
	// at line granularity; the returned lineArray maps runes back to
	// the original line strings (one entry per unique line across
	// both inputs).
	chars1, chars2, lineArray := dmp.DiffLinesToChars(args[1], args[2])
	diffs := dmp.DiffMain(chars1, chars2, false)
	diffs = dmp.DiffCharsToLines(diffs, lineArray)

	// Render the line-mode diff with explicit per-line prefixes.
	// Unchanged lines get a leading space; inserted lines get `+`;
	// deleted lines get `-`. DiffCharsToLines emits d.Text that
	// already contains the original line strings (possibly several
	// lines joined by '\n'), so we split on '\n' and emit one prefix
	// per line — never per byte. A trailing newline after every line
	// (including the last) keeps the output consumable by downstream
	// tools (line counts, `wc -l`, diff post-processors).
	var b []byte
	for _, d := range diffs {
		prefix := byte(' ')
		switch d.Type {
		case diffmatchpatch.DiffInsert:
			prefix = '+'
		case diffmatchpatch.DiffDelete:
			prefix = '-'
		}
		// Empty diff entries (can appear when one side is empty)
		// still emit a newline so the line accounting stays even.
		if d.Text == "" {
			b = append(b, prefix)
			b = append(b, '\n')
			continue
		}
		for _, line := range strings.Split(d.Text, "\n") {
			// The library's encoding collapses runs of identical
			// lines; an emitted line is never the empty trailing
			// fragment after a final '\n', so we don't need to
			// special-case it. A line of literal "" can arise from
			// the library's split semantics on a trailing newline;
			// skip those so we don't emit blank "+\n" / "-\n" /
			// " \n" lines that don't correspond to a real input line.
			if line == "" {
				continue
			}
			b = append(b, prefix)
			b = append(b, line...)
			b = append(b, '\n')
		}
	}
	fmt.Print(string(b))
	return 0
}
