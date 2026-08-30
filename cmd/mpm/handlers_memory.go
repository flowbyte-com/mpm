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

// memoryMaxFileBytes caps the --file payload size accepted by
// `mpm memory add`. The pre-audit path either fell through as positional
// content (length 18 stored) or silently truncated large files; F-A3
// demands an explicit ceiling rather than silent truncation. 100 MiB
// matches the reference-add ceiling so the two ingest paths agree.
const memoryMaxFileBytes = 100 << 20

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
	fileArg := ""    // --file <path>: read content from file (F-A3 fix — no silent 64KB truncation)
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
		case "--file", "-f":
			// F-A3: --file reads content from a file in full, capped at
			// memoryMaxFileBytes (100 MiB). The previous behaviour was
			// either to fall through as positional content (length 18
			// stored for `mpm memory add --file /tmp/p1g.txt`) or to
			// silently truncate to ~64KB. We now reject oversized files
			// explicitly and never silently truncate.
			if i+1 >= len(args) {
				return respond("", "--file requires a path\n", 1)
			}
			i++
			fileArg = args[i]
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
			if i+1 >= len(args) {
				return respond("", "--expires-in requires a value (e.g. 7d, 24h, 30m)\n", 1)
			}
			i++
			expiresIn = args[i]
			// F-H2: validate the duration at parse time so we don't
			// create the memory before discovering the value is
			// garbage (which would force a rollback row, leaving
			// a memory with no TTL that the operator didn't expect).
			if _, parseErr := parseDuration(expiresIn); parseErr != nil {
				return respond("", fmt.Sprintf("--expires-in %q: invalid duration (use Nd, Nh, Nm, or Go duration like 24h)\n", expiresIn), 1)
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
		// Content precedence: --file > --fact > positional args. The --file
		// branch is F-A3's headline fix: read the file in full, never
		// silently truncate to ~64KB. A file larger than memoryMaxFileBytes
		// (100 MiB) is rejected with an explicit error so the operator
		// knows the memory was NOT stored.
		if fileArg != "" {
			// Mutual exclusion: --file alongside --fact is an operator
			// mistake (two ways to specify content). Reject explicitly.
			if factArg != "" {
				return respond("", "--file and --fact are mutually exclusive (use one)\n", 1)
			}
			if len(contentArgs) > 0 {
				return respond("", "--file and positional content are mutually exclusive (use one)\n", 1)
			}
			info, statErr := os.Stat(fileArg)
			if statErr != nil {
				return respond("", fmt.Sprintf("--file: %v\n", statErr), 1)
			}
			if info.Size() > memoryMaxFileBytes {
				return respond("", fmt.Sprintf("--file: %s is %d bytes (max %d = 100 MiB) — refusing to store a memory this large; chunk it or use `mpm reference add` instead\n", fileArg, info.Size(), memoryMaxFileBytes), 1)
			}
			data, err := os.ReadFile(fileArg)
			if err != nil {
				return respond("", fmt.Sprintf("--file: read %s: %v\n", fileArg, err), 1)
			}
			content = string(data)
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
	}

	// Parse tags: comma-separated, trim spaces, drop empties.
	// F-H3: --tags accepts CSV only. A value that LOOKS LIKE JSON
	// (starts with `{` or `[`) is rejected explicitly — the pre-audit
	// behaviour stored the literal string as a single tag, producing
	// a row like ["{\"a\":1}"] in the database (a nested-JSON array
	// wrapper) which downstream consumers couldn't parse.
	var tagsList []string
	if tagsArg != "" {
		trimmed := strings.TrimSpace(tagsArg)
		if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
			return respond("", fmt.Sprintf("--tags: JSON not supported (use CSV, e.g. --tags a,b,c); got %q\n", tagsArg), 1)
		}
		for _, t := range strings.Split(tagsArg, ",") {
			if tt := strings.TrimSpace(t); tt != "" {
				tagsList = append(tagsList, tt)
			}
		}
	}

	// Inject active mode/persona from config files
	injectActiveContext()
	defer clearActiveContext()

	// Build metadata with active context only.
	// Provenance is recorded in the artifact_provenance table (via RecordArtifactProvenance
	// inside SaveMemoryNode), which is the canonical provenance record. The
	// metadata.provenance block is no longer written — it previously contained
	// hardcoded fake values (source=human, model=direct) that misled GetMemoryStats.
	memMetadata := map[string]interface{}{}
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

	// Set TTL if --expires-in was provided. Validation happens at
	// parse time (see the --expires-in case above), so by the time we
	// get here parseDuration cannot fail.
	var suggestions []map[string]interface{}
	if dm := getDBConcrete(); dm != nil {
		if expiresIn != "" {
			dur, _ := parseDuration(expiresIn)
			dm.SetMemoryTTL(mem.ID, time.Now().Add(dur))
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
		// JSON output mode. The envelope MUST stay parity-compatible with
		// `mpm call mpm_memory save` (internal/core/memory_tools.go), so an
		// agent that automates against either surface sees the same shape:
		//   - success, id, content, tags, weight, pointer (always)
		//   - content_truncated / content_bytes / note (only when content
		//     exceeded the wire bound, per audit finding F4)
		// `suggested_topics` is a CLI-only convenience hint — agents that
		// want it should call `mpm memory suggest` explicitly.
		echoContent, truncated := mpminternal.BoundInlineContent(mem.Content)
		type jsonResult struct {
			Success           bool                     `json:"success"`
			ID                string                   `json:"id"`
			Content           string                   `json:"content"`
			// Echo tags/weight so the operator can verify at the CLI that
			// the flags were applied — not just that the call returned
			// success. Production incident: silent-failure shape hid the
			// fact that --tags/--weight weren't recognized; the user
			// only saw the bug when they queried sqlite3 directly.
			Tags              []string                 `json:"tags,omitempty"`
			Weight            float64                  `json:"weight,omitempty"`
			Pointer           string                   `json:"pointer"`
			ContentTruncated  bool                     `json:"content_truncated,omitempty"`
			ContentBytes      int                      `json:"content_bytes,omitempty"`
			Note              string                   `json:"note,omitempty"`
			SuggestedTopics   []map[string]interface{} `json:"suggested_topics,omitempty"`
		}
		result := jsonResult{
			Success: true,
			ID:      mem.ID,
			Content: echoContent,
			Tags:    tagsList,
			Weight:  weightArg,
			Pointer: "mpm://memory/" + mem.ID,
		}
		if truncated {
			result.ContentTruncated = true
			result.ContentBytes = len(mem.Content)
			result.Note = "content stored in full; inline echo bounded — retrieve via mpm memory show"
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

	// Alpha-4.1 F-005 / W-006: strip `--json` / `-j` from the argument
	// list before constructing the query. Pre-fix this joined the
	// literal flag string into the FTS5 query and returned zero hits
	// because no document contained the token "--json".
	cleaned, wantJSON := stripMemoryFlagToken(args, "--json", "-j")
	query := strings.Join(cleaned, " ")
	if strings.TrimSpace(query) == "" {
		return respond("", "Usage: mpm memory search <query>", 1)
	}

	store := getMemoryStore()

	memories, err := store.FullTextSearch(query, "memories", 20)
	if err != nil {
		return respond("", fmt.Sprintf("Search failed: %v", err), 1)
	}

	if wantJSON {
		// JSON envelope — the canonical machine contract. Always
		// emit the same shape regardless of hit count so callers can
		// parse without branching.
		items := make([]map[string]interface{}, 0, len(memories))
		for _, mem := range memories {
			items = append(items, map[string]interface{}{
				"id":         mem.ID,
				"created_at": mem.CreatedAt,
				"tags":       mem.Tags,
				"metadata":   mem.Metadata,
				"snippet":    truncateSnippet(mem.Content, 500),
			})
		}
		body, _ := json.Marshal(map[string]interface{}{
			"success": true,
			"query":   query,
			"count":   len(memories),
			"memories": items,
		})
		return respond(string(body)+"\n", "", 0)
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

// stripMemoryFlagToken removes one or more flag tokens (with their
// optional `=value` form) from an argument list. Returns the cleaned
// list plus a bool indicating whether any matching flag was found.
// Shared by handleMemorySearch / List / Show so W-006 has a single
// canonical flag-stripper.
func stripMemoryFlagToken(args []string, flags ...string) (cleaned []string, found bool) {
	out := make([]string, 0, len(args))
	for _, a := range args {
		stripped := false
		for _, f := range flags {
			if a == f {
				found = true
				stripped = true
				break
			}
			// `--flag=value` form: drop entirely (we don't carry the value).
			if strings.HasPrefix(a, f+"=") {
				found = true
				stripped = true
				break
			}
		}
		if !stripped {
			out = append(out, a)
		}
	}
	return out, found
}

// truncateSnippet is a small helper for the JSON output path so the
// snippet field stays bounded regardless of source content size.
func truncateSnippet(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
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
