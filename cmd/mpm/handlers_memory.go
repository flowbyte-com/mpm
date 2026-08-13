package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	mpminternal "github.com/flowbyte-com/mpm-core"
	"github.com/flowbyte-com/mpm-core/synth"
	"github.com/flowbyte-com/mpm-core/usererror"
)

// ============================================================================
// Handler: prime-directives
// ============================================================================

func handlePrimeDirectives() int {
	jsonOutput, _ := ExtractJSONFlag(os.Args[1:])

	store := getMemoryStore()
	if store == nil {
		return respond("", "Error: memory store not available\n", 1)
	}
	if store.DB == nil {
		if err := store.InitSQLite(); err != nil {
			return respond("", fmt.Sprintf("Error initializing memory store: %v\n", err), 1)
		}
	}

	rows, err := store.DB.Query(`
		SELECT id, collection, content, metadata, created_at
		FROM memories
		WHERE (collection = 'directives' OR is_prime_directive = 1)
		  AND deleted_at IS NULL
		ORDER BY created_at ASC
	`)
	if err != nil {
		return respond("", fmt.Sprintf("Error querying prime directives: %v\n", err), 1)
	}
	defer rows.Close()

	if jsonOutput {
		type directiveEntry struct {
			ID         string `json:"id"`
			Collection string `json:"collection"`
			Content    string `json:"content"`
			CreatedAt  string `json:"created_at"`
		}
		directives := make([]directiveEntry, 0)
		scanErr := func() error {
			for rows.Next() {
				var id, collection, content, metadata, created string
				if err := rows.Scan(&id, &collection, &content, &metadata, &created); err != nil {
					return fmt.Errorf("scanning prime directive row: %w", err)
				}
				directives = append(directives, directiveEntry{
					ID:         id,
					Collection: collection,
					Content:    content,
					CreatedAt:  created,
				})
			}
			return nil
		}()
		if scanErr != nil {
			usererror.Warn("handlePrimeDirectives: %v", scanErr)
			return 1
		}
		if len(directives) == 0 {
			fmt.Println(`{"directives": [], "message": "No prime directives found"}`)
		} else {
			data, _ := json.Marshal(map[string]interface{}{"directives": directives})
			fmt.Println(string(data))
		}
		return 0
	}

	var output strings.Builder
	output.WriteString("\n\xf0\x9f\x9b\xb8 808 PRIME DIRECTIVES \xf0\x9f\x9b\xb8\n")
	output.WriteString("\xe2\x94\x81\xe2\x95\x90\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\n\n")

	count := 0
	scanErr := func() error {
		for rows.Next() {
			var id, collection, content, metadata, created string
			if err := rows.Scan(&id, &collection, &content, &metadata, &created); err != nil {
				return fmt.Errorf("scanning prime directive output row: %w", err)
			}
			output.WriteString(fmt.Sprintf("[%s] %s\n\n", id, collection))
			content = strings.TrimSpace(content)
			for i := 0; i < len(content); i += 70 {
				end := i + 70
				if end > len(content) {
					end = len(content)
				}
				output.WriteString(content[i:end] + "\n")
			}
			output.WriteString("\n")
			count++
		}
		return nil
	}()
	if scanErr != nil {
		usererror.Warn("handlePrimeDirectives: %v", scanErr)
		return 1
	}

	if count == 0 {
		output.WriteString("No prime directives found. Run the session that defines them.\n")
	}

	output.WriteString("\xe2\x94\x81\xe2\x95\x90\xe2\x94\x81\xe2\x95\x90\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\n")
	return respond(output.String(), "", 0)
}

func handleMemoryAdd(args []string) int {
	if len(args) == 0 {
		return respond("", "Usage: mpm memory add [--fact <text>] [--tags <csv>] [--weight <0-100>] [--expires-in <duration>] [-i|--interactive] [--json] <content>", 1)
	}

	// Pre-scan --json, -i/--interactive, --fact, --tags, --weight, --expires-in flags.
	// Unrecognized flags fall through to contentArgs (positional content),
	// but --fact/--tags/--weight are EXPLICITLY recognized so a typo in flag
	// shape doesn't silently land as content. Production hit 2026-08-13:
	// an agent invoked `mpm memory add --fact X --weight 85 --tags a,b,c`
	// expecting flags to work; the handler passed them through as content
	// args, the row landed with content "--fact X --weight 85 --tags a,b,c"
	// and tags=null, weight=1. Silent failure with success return — exactly
	// the shape the always-cross-check-with-sqlite3 rule exists to catch.
	jsonOutput := false
	expiresIn := ""
	interactive := false
	factArg := ""    // --fact <text>: alternative to positional content (matches mpm_memory action=save payload field name)
	tagsArg := ""    // --tags <csv>: comma-separated tags
	weightArg := 1.0 // --weight <0-100>: weight; default 1 (matches the hardcoded value AddMemoryWithWeight substitutes)
	weightSet := false // tracks whether --weight was actually supplied (so we can tell "user passed 0" from "user didn't pass anything")
	contentArgs := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--json", "-j":
			jsonOutput = true
		case "-i", "--interactive":
			interactive = true
		case "--fact":
			if i+1 >= len(args) {
				return respond("", "--fact requires a value\n", 1)
			}
			i++
			factArg = args[i]
		case "--tags":
			if i+1 >= len(args) {
				return respond("", "--tags requires a value\n", 1)
			}
			i++
			tagsArg = args[i]
		case "--weight":
			if i+1 >= len(args) {
				return respond("", "--weight requires a value\n", 1)
			}
			i++
			w, parseErr := strconv.ParseFloat(args[i], 64)
			if parseErr != nil {
				return respond("", fmt.Sprintf("--weight: invalid float %q\n", args[i]), 1)
			}
			weightArg = w
			weightSet = true
		case "--expires-in":
			if i+1 < len(args) {
				i++
				expiresIn = args[i]
			}
		default:
			contentArgs = append(contentArgs, args[i])
		}
	}

	var content string
	if interactive {
		drafted, err := draftInteractiveContent()
		if err != nil {
			return respond("", fmt.Sprintf("Interactive input failed: %v\n", err), 1)
		}
		if drafted == "" {
			return respond("", "No content provided.\n", 0)
		}
		content = drafted
	} else {
		// Content precedence: --fact > positional args. This mirrors the
		// mpm_memory action=save contract (`params.fact`) so the CLI and
		// the tool path agree on what "the fact" means. If both are given,
		// --fact wins and the positional args are silently dropped — log
		// this via warn so the operator notices, since the alternative is
		// the same silent-failure shape this fix exists to prevent.
		switch {
		case factArg != "":
			content = factArg
			if len(contentArgs) > 0 {
				usererror.Warn("--fact provided alongside %d positional arg(s); positional dropped (use one or the other)", len(contentArgs))
			}
		case len(contentArgs) == 0:
			return respond("", "Usage: mpm memory add [--fact <text>] [--tags <csv>] [--weight <0-100>] [--expires-in <duration>] [-i|--interactive] [--json] <content>\n", 1)
		default:
			content = strings.Join(contentArgs, " ")
		}
	}

	// Parse tags: comma-separated, trim spaces, drop empties.
	var tagsList []string
	if tagsArg != "" {
		for _, t := range strings.Split(tagsArg, ",") {
			if trimmed := strings.TrimSpace(t); trimmed != "" {
				tagsList = append(tagsList, trimmed)
			}
		}
	}

	// Inject active mode/persona from config files
	injectActiveContext()
	defer clearActiveContext()

	// Build metadata with provenance + active context
	memMetadata := map[string]interface{}{
		"provenance": map[string]interface{}{
			"source":  "human",
			"model":   "direct",
			"compute": "absolute",
			"agent":   "mpm_cli",
			"persona": "operator",
		},
	}
	if activeMode != "" {
		memMetadata["active_mode"] = activeMode
	}
	if activePersona != "" {
		memMetadata["active_persona"] = activePersona
	}
	// Echo the parsed flags back into metadata for forensic clarity (and so
	// a subsequent `mpm memory show <id>` shows the user what was applied
	// vs. what was silently defaulted). Production incident: a row saved
	// with weight=1 and tags=null looked indistinguishable from a row that
	// was saved with explicit weight=1 tags=null until the operator queried
	// sqlite3 directly.
	if weightSet {
		memMetadata["cli_weight"] = weightArg
	}
	if len(tagsList) > 0 {
		memMetadata["cli_tags"] = tagsList
	}

	store := getMemoryStore()
	mem, err := store.AddMemoryWithWeight(content, "memories", tagsList, memMetadata, "", "cli", weightArg)
	if err != nil {
		return respond("", fmt.Sprintf("Failed to add memory: %v", err), 1)
	}

	// Set TTL if --expires-in was provided
	var suggestions []map[string]interface{}
	if dm := getDBConcrete(); dm != nil {
		if expiresIn != "" {
			dur, parseErr := parseDuration(expiresIn)
			if parseErr == nil {
				dm.SetMemoryTTL(mem.ID, time.Now().Add(dur))
			}
		}

		// Get topic suggestions (non-blocking — failures are silently ignored)
		suggestions, _ = suggestTopicsForMemory(dm, mem.ID, content, 3, 0.3)

		// Fire-and-forget: check for near-miss candidates and auto-synthesize.
		// Uses synthesis worker pool (bounded, with context) so it doesn't block the caller's connection.
		if dm != nil {
			if synthDM, err := dm.NewSession(); err == nil {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
				pool := mpminternal.GetSynthesisPool(3)
				pool.Submit(ctx, synthDM, synth.NewSynthClient(), mem.ID, content)
				cancel()
			} else {
				slog.Warn("synthesis: failed to open db session", "memory_id", mem.ID, "error", err)
			}
		}
	}
	mem.SuggestedTopics = suggestions

	if jsonOutput {
		// JSON output mode
		type jsonResult struct {
			Success         bool                     `json:"success"`
			ID              string                   `json:"id"`
			Content         string                   `json:"content"`
			// Echo tags/weight so the operator can verify at the CLI that
			// the flags were applied — not just that the call returned
			// success. Production incident: silent-failure shape hid the
			// fact that --tags/--weight weren't recognized; the user
			// only saw the bug when they queried sqlite3 directly.
			Tags            []string                 `json:"tags,omitempty"`
			Weight          float64                  `json:"weight,omitempty"`
			SuggestedTopics []map[string]interface{} `json:"suggested_topics,omitempty"`
		}
		result := jsonResult{
			Success: true,
			ID:      mem.ID,
			Content: mem.Content,
			Tags:    tagsList,
			Weight:  weightArg,
		}
		if len(suggestions) > 0 {
			result.SuggestedTopics = suggestions
		}
		out, _ := json.Marshal(result)
		fmt.Println(string(out))
		return 0
	}

	// Human output mode
	output := fmt.Sprintf("✅ Memory added: %s\n", mem.ID)
	if len(suggestions) > 0 {
		var parts []string
		for _, s := range suggestions {
			if name, ok := s["name"].(string); ok {
				if conf, ok := s["confidence"].(float64); ok {
					parts = append(parts, fmt.Sprintf("%s (%.2f)", name, conf))
				}
			}
		}
		if len(parts) > 0 {
			output += fmt.Sprintf("💡 Consider linking to: %s\n", strings.Join(parts, ", "))
		}
	}
	return respond(output, "", 0)
}

func handleMemorySearch(args []string) int {
	if len(args) == 0 {
		return respond("", "Usage: mpm memory search <query>", 1)
	}

	query := strings.Join(args, " ")
	store := getMemoryStore()

	memories, err := store.FullTextSearch(query, "memories", 20)
	if err != nil {
		return respond("", fmt.Sprintf("Search failed: %v", err), 1)
	}

	if len(memories) == 0 {
		return respond("No memories found.\n", "", 0)
	}

	var output strings.Builder
	output.WriteString(fmt.Sprintf("Found %d memories:\n\n", len(memories)))

	for _, mem := range memories {
		snippet := mem.Content
		metaJSON, _ := json.Marshal(mem.Metadata)
		if preamble := mpminternal.ProvenancePreamble(string(metaJSON)); preamble != "" {
			snippet = preamble + "\n" + snippet
		}
		if len(snippet) > 500 {
			snippet = snippet[:500] + "..."
		}
		snippet = strings.ReplaceAll(snippet, "\n", " ")

		created := mpminternal.FormatUnixSeconds(mem.CreatedAt)
		if len(created) > 10 {
			created = created[:10]
		}

		output.WriteString(fmt.Sprintf("[%s] %s\n", mem.ID, created))
		output.WriteString(fmt.Sprintf("    %s\n\n", snippet))
	}

	return respond(output.String(), "", 0)
}

func handleMemoryShow(args []string) int {
	if len(args) == 0 {
		return respond("", "Usage: mpm memory show <id>", 1)
	}

	id := args[0]
	store := getMemoryStore()

	mem, err := store.GetByID(id, "memories")
	if err != nil || mem == nil {
		return respond("", fmt.Sprintf("Memory not found: %s\n", id), 1)
	}

	var output strings.Builder
	output.WriteString(fmt.Sprintf("ID:      %s\n", mem.ID))
	output.WriteString(fmt.Sprintf("Created: %s\n", mpminternal.FormatUnixSeconds(mem.CreatedAt)))
	if mem.Source != "" {
		output.WriteString(fmt.Sprintf("Source:  %s\n", mem.Source))
	}
	if len(mem.Tags) > 0 {
		output.WriteString(fmt.Sprintf("Tags:    %s\n", strings.Join(mem.Tags, ", ")))
	}
	output.WriteString("\n")
	output.WriteString(mem.Content)
	output.WriteString("\n")

	return respond(output.String(), "", 0)
}

func handleMemoryShred(args []string) int {
	if len(args) == 0 {
		return respond("", "Usage: mpm memory shred <id>", 1)
	}

	id := args[0]
	store := getMemoryStore()

	err := store.DeleteMemory(id, "memories")
	if err != nil {
		return respond("", fmt.Sprintf("Failed to shred memory: %v", err), 1)
	}

	return respond(fmt.Sprintf("Memory shredded: %s\n", id), "", 0)
}

func handleMemoryList(args []string) int {
	store := getMemoryStore()

	memories, err := store.GetRecent(20)
	if err != nil {
		return respond("", fmt.Sprintf("Failed to list memories: %v", err), 1)
	}

	if len(memories) == 0 {
		return respond("No memories stored.\n", "", 0)
	}

	var output strings.Builder
	output.WriteString(fmt.Sprintf("Recent %d memories:\n\n", len(memories)))

	for _, mem := range memories {
		snippet := mem.Content
		if len(snippet) > 500 {
			snippet = snippet[:500] + "..."
		}
		snippet = strings.ReplaceAll(snippet, "\n", " ")

		created := mpminternal.FormatUnixSeconds(mem.CreatedAt)
		if len(created) > 10 {
			created = created[:10]
		}

		output.WriteString(fmt.Sprintf("[%s] %s\n", mem.ID, created))
		output.WriteString(fmt.Sprintf("    %s\n\n", snippet))
	}

	return respond(output.String(), "", 0)
}

func handleMemorySearchTerm(args []string) int {
	if len(args) == 0 {
		return respond("", "Usage: mpm memory search-term <term>", 1)
	}

	term := strings.Join(args, " ")
	store := getMemoryStore()

	// Use the existing search functionality
	memories, err := store.FullTextSearch(term, "memories", 50)
	if err != nil {
		return respond("", fmt.Sprintf("Search failed: %v", err), 1)
	}

	if len(memories) == 0 {
		return respond(fmt.Sprintf("No memories matching '%s' found.\n", term), "", 0)
	}

	var output strings.Builder
	output.WriteString(fmt.Sprintf("Found %d memories matching '%s':\n\n", len(memories), term))

	for _, mem := range memories {
		snippet := mem.Content
		if len(snippet) > 500 {
			snippet = snippet[:500] + "..."
		}
		snippet = strings.ReplaceAll(snippet, "\n", " ")

		created := mpminternal.FormatUnixSeconds(mem.CreatedAt)
		if len(created) > 10 {
			created = created[:10]
		}

		output.WriteString(fmt.Sprintf("[%s] %s\n", mem.ID, created))
		output.WriteString(fmt.Sprintf("    %s\n\n", snippet))
	}

	return respond(output.String(), "", 0)
}

func handleMemoryWipe(args []string) int {
	// Check for --force flag
	force := false
	for _, arg := range args {
		if arg == "--force" || arg == "-f" {
			force = true
		}
	}

	if !force {
		return respond("", "Wipe requires --force flag\n", 1)
	}

	store := getMemoryStore()

	// Clear the mirror file
	err := store.ClearMirror()
	if err != nil {
		return respond("", fmt.Sprintf("Failed to wipe memories: %v", err), 1)
	}

	return respond("All memories wiped.\n", "", 0)
}
