package main

import (
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/sergi/go-diff/diffmatchpatch"
	mpminternal "mpm/internal"
)

// mpm history <id> — Show version history for a memory
func handleHistory(args []string) int {
	if len(args) < 2 {
		fmt.Fprintf(os.Stderr, "Usage: mpm history <id>\n")
		return 1
	}

	id := args[1]

	dm, err := mpminternal.NewDatabaseManager("")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}
	defer dm.Close()

	revisions, err := dm.GetMemoryRevisions(id)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
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
		fmt.Printf("%s  %s\n", label, r.CreatedAt.UTC().Format(time.RFC3339))
	}
	return 0
}

// mpm diff <id> <v1> <v2> — Show unified diff between two versions
func handleDiff(args []string) int {
	if len(args) < 4 {
		fmt.Fprintf(os.Stderr, "Usage: mpm diff <id> <v1> <v2>\n")
		return 1
	}

	id := args[1]
	v1, err := strconv.Atoi(args[2])
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: v1 must be an integer, got %q\n", args[2])
		return 1
	}
	v2, err := strconv.Atoi(args[3])
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: v2 must be an integer, got %q\n", args[3])
		return 1
	}

	dm, err := mpminternal.NewDatabaseManager("")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}
	defer dm.Close()

	// Fetch both versions directly from memory_revisions
	type versionInfo struct {
		content  string
		revision mpminternal.MemoryRevision
	}
	versions := map[int]versionInfo{}

	revisions, err := dm.GetMemoryRevisions(id)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}
	for _, r := range revisions {
		if r.Version == v1 || r.Version == v2 {
			versions[r.Version] = versionInfo{content: r.Content, revision: r}
		}
	}

	v1Info, ok1 := versions[v1]
	v2Info, ok2 := versions[v2]
	if !ok1 {
		fmt.Fprintf(os.Stderr, "Error: version %d not found for memory %s\n", v1, id)
		return 1
	}
	if !ok2 {
		fmt.Fprintf(os.Stderr, "Error: version %d not found for memory %s\n", v2, id)
		return 1
	}

	dmp := diffmatchpatch.New()
	diffs := dmp.DiffMain(v1Info.content, v2Info.content, true)
	diffs = dmp.DiffCleanupSemantic(diffs)

	fmt.Printf("--- v%d (%s)\n", v1, v1Info.revision.CreatedAt.UTC().Format(time.RFC3339))
	fmt.Printf("+++ v%d (%s)\n", v2, v2Info.revision.CreatedAt.UTC().Format(time.RFC3339))
	fmt.Print(dmp.DiffPrettyText(diffs))
	return 0
}

// mpm diff-lines <v1-content> <v2-content> — Compute unified diff of two text blocks (used for testing)
func handleDiffLines(args []string) int {
	if len(args) < 3 {
		fmt.Fprintf(os.Stderr, "Usage: mpm diff-lines <v1-content> <v2-content>\n")
		return 1
	}

	dmp := diffmatchpatch.New()
	diffs := dmp.DiffMain(args[1], args[2], true)
	diffs = dmp.DiffCleanupSemantic(diffs)
	fmt.Print(dmp.DiffPrettyText(diffs))
	return 0
}
