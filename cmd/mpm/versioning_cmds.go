package main

import (
	"fmt"
	"strconv"

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
	}
	if !ok2 {
		usererror.Error("version %d not found for memory %s", v2, id)
	}

	dmp := diffmatchpatch.New()
	diffs := dmp.DiffMain(v1Info.content, v2Info.content, true)
	diffs = dmp.DiffCleanupSemantic(diffs)

	fmt.Printf("--- v%d (%s)\n", v1, mpminternal.FormatUnixSeconds(v1Info.revision.CreatedAt))
	fmt.Printf("+++ v%d (%s)\n", v2, mpminternal.FormatUnixSeconds(v2Info.revision.CreatedAt))
	fmt.Print(dmp.DiffPrettyText(diffs))
	return 0
}

// mpm diff-lines <v1-content> <v2-content> — Compute unified diff of two text blocks (used for testing)
func handleDiffLines(args []string) int {
	if len(args) < 3 {
		usererror.Usage("mpm diff-lines <v1-content> <v2-content>")
		return 1
	}

	dmp := diffmatchpatch.New()
	diffs := dmp.DiffMain(args[1], args[2], true)
	diffs = dmp.DiffCleanupSemantic(diffs)
	fmt.Print(dmp.DiffPrettyText(diffs))
	return 0
}
