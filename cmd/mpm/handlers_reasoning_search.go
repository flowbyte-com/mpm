// handlers_reasoning_search.go — F-A2/F-F1 reasoning discoverability.
//
// The hostile test surfaced that an unfamiliar agent could not answer
// "what did we decide?" / "what skills do we have?" / "what lessons
// were learned?" through documented, discoverable CLI surfaces.
// Lessons had `mpm lesson search`; decisions and skills did not.
//
// Both decisions (collection='decisions') and skills (collection='skills')
// live as memories with the appropriate collection tag. The F-A2/F-F1
// fix routes `mpm decision search <query>` and `mpm skill search <query>`
// through QueryMemory against that collection. The behaviour mirrors
// `mpm lesson search` so an agent learns one surface, gets three.
package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/flowbyte-com/mpm-core"
)

// handleCollectionSearch is the shared body for `mpm decision search`
// and `mpm skill search`. Both query the `memories` table filtered by
// collection via FTS5, returning up to 20 rows with --json support.
//
// The `mpm-core` package name is `internal` (the dir is
// internal/core and the package declaration is `package internal`).
// All cmd/mpm files reference it as `internal.X`.
func handleCollectionSearch(args []string, collection, label string) int {
	if len(args) == 0 {
		return respond("",
			fmt.Sprintf("Usage: mpm %s search <query> [--json]\n", label), 1)
	}

	jsonOutput, queryArgs := ExtractJSONFlag(args)
	if len(queryArgs) == 0 {
		return respond("",
			fmt.Sprintf("Usage: mpm %s search <query> [--json]\n", label), 1)
	}
	query := strings.Join(queryArgs, " ")

	store := internal.NewMemoryStore("")

	// F-A2/F-F1 hardening: any FTS panic (malformed query, nil DB
	// after partial init, etc.) is recovered and surfaced as a
	// controlled "search failed" message. The hostile test surfaced
	// this as an unrecoverable crash; the fix turns it into a normal
	// exit code so a follow-up call can diagnose.
	defer func() {
		if r := recover(); r != nil {
			respond("", fmt.Sprintf("Search failed: internal error: %v\n", r), 1)
		}
	}()

	// QueryMemory runs the same FTS5 search path that powers lesson
	// search. Per-collection scoping matches the storage layout
	// (decisions and skills are memories with a collection tag).
	results, err := store.QueryMemory(query, collection, 20, nil)
	if err != nil {
		return respond("", fmt.Sprintf("Search failed: %v\n", err), 1)
	}

	if len(results) == 0 {
		if jsonOutput {
			data, _ := json.Marshal(map[string]interface{}{
				"query":      query,
				"collection": collection,
				"results":    []interface{}{},
				"message":    fmt.Sprintf("No %s found", label),
			})
			fmt.Println(string(data))
			return 0
		}
		return respond(fmt.Sprintf("No %s found.\n", label), "", 0)
	}

	if jsonOutput {
		type result struct {
			ID        string   `json:"id"`
			Content   string   `json:"content"`
			Tags      []string `json:"tags"`
			CreatedAt string   `json:"created_at"`
		}
		out := make([]result, 0, len(results))
		for _, m := range results {
			if m == nil {
				continue
			}
			out = append(out, result{
				ID:        m.ID,
				Content:   m.Content,
				Tags:      m.Tags,
				CreatedAt: time.Unix(m.CreatedAt, 0).UTC().Format(time.RFC3339),
			})
		}
		data, _ := json.Marshal(map[string]interface{}{
			"query":      query,
			"collection": collection,
			"results":    out,
		})
		fmt.Println(string(data))
		return 0
	}

	var output strings.Builder
	output.WriteString(fmt.Sprintf("Found %d %s(s):\n\n", len(results), label))
	for _, m := range results {
		if m == nil {
			continue
		}
		snippet := m.Content
		if len(snippet) > 100 {
			snippet = snippet[:100] + "..."
		}
		output.WriteString(fmt.Sprintf("[%s]\n    %s\n\n", m.ID, snippet))
	}
	return respond(output.String(), "", 0)
}

// handleDecisionSearch is `mpm decision search <query>`. F-A2 fix:
// gives operators a discoverable way to answer "what did we decide
// about X?" without having to know the mpm_decisions call envelope.
func handleDecisionSearch(args []string) int {
	return handleCollectionSearch(args, "decisions", "decision")
}

// handleSkillSearch is `mpm skill search <query>`. F-F1 fix: gives
// operators a discoverable way to find a skill by name fragment or
// topic without enumerating the entire list.
func handleSkillSearch(args []string) int {
	return handleCollectionSearch(args, "skills", "skill")
}

// handleTheorySearch is `mpm theory search <query>`. Same surface
// parity as decisions/skills. Theories are also stored as memories
// with collection='theories'.
func handleTheorySearch(args []string) int {
	return handleCollectionSearch(args, "theories", "theory")
}