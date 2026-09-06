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

	// D3 fix: POSIX-style `--` separator. The router-level parseFlags
	// (router.go:474) rewrites `-h/--help` (and a handful of other
	// names) but does NOT know about per-handler flags like --fact,
	// --tags, --weight. If the operator's content begins with `-`
	// (think YAML front-matter `---\nfoo: bar` or a literal flag-like
	// token they want preserved as data), the documented escape hatch
	// is to prefix with `--`: `mpm memory add -- ---yaml-front-matter`.
	//
	// Pre-fix this worked for `mpm add` (the older simple_cmds.go
	// path uses stdlib flag, which respects `--`), but for the
	// `mpm memory add` path the pre-scan switch below does NOT
	// consume a standalone `--` token — it fell through into
	// contentArgs and got joined with the rest of the content,
	// producing rows like content=`-- ---yaml-front-matter`. The
	// audit reported this as "mpm add rejects content beginning
	// with `-`"; the actual defect is that the documented escape
	// hatch printed in the help text did not work for the
	// `mpm memory add` path.
	//
	// D3 fix: POSIX-style `--` separator. The router-level parseFlags
	// (router.go:474) rewrites `-h/--help` (and a handful of other
	// names) but does NOT know about per-handler flags like --fact,
	// --tags, --weight. If the operator's content begins with `-`
	// (think YAML front-matter `---\nfoo: bar` or a literal flag-like
	// token they want preserved as data), the documented escape hatch
	// is to prefix with `--`: `mpm memory add -- ---yaml-front-matter`.
	//
	// Pre-fix this worked for `mpm add` (the older simple_cmds.go
	// path uses stdlib flag, which respects `--`), but for the
	// `mpm memory add` path the pre-scan switch below did NOT consume
	// a standalone `--` token — it fell through into contentArgs and
	// got joined with the rest of the content, producing rows like
	// content=`-- ---yaml-front-matter`. The audit reported this as
	// "mpm add rejects content beginning with `-`"; the actual defect
	// was that the documented escape hatch printed in the help text
	// did not work for the `mpm memory add` path.
	//
	// Stage S1 of the CLI refactor (2026-09-06): use the shared
	// `splitOnDashDash` helper. Tokens before the first `--` go to
	// the pre-scan switch (flag parsing). Tokens after the first `--`
	// are appended verbatim to contentArgs and must not be
	// interpreted as flags. Single-dash leading tokens (`-foo`) and
	// non-flag-like content BEFORE the separator are still rejected
	// at parse time as ambiguous, preserving the 2026-08-13
	// silent-failure invariant (a typo in --fact/--tags shape must
	// NEVER silently land as content).
	flagsArgs, rawArgs := splitOnDashDash(args)
	// Stage S2 of the CLI refactor (2026-09-06): --json / -j is
	// extracted by the canonical ExtractJSONFlag helper before the
	// pre-scan switch runs. Same contract as the prior inline case
	// (exact match, removed from the working slice, sets the bool).
	jsonOutput, flagsArgs := ExtractJSONFlag(flagsArgs)

	// Pre-scan -i/--interactive, --fact, --tags, --weight, --expires-in flags.
	// Unrecognized flags fall through to contentArgs (positional content),
	// but --fact/--tags/--weight are EXPLICITLY recognized so a typo in flag
	// shape doesn't silently land as content. Production hit 2026-08-13:
	// an agent invoked `mpm memory add --fact X --weight 85 --tags a,b,c`
	// expecting flags to work; the handler passed them through as content
	// args, the row landed with content "--fact X --weight 85 --tags a,b,c"
	// and tags=null, weight=1. Silent failure with success return — exactly
	// the shape the always-cross-check-with-sqlite3 rule exists to catch.
	// jsonOutput is set by ExtractJSONFlag above (S2 migration); the
	// pre-scan switch no longer carries a `case "--json"` arm.
	expiresIn := ""
	interactive := false
	factArg := ""    // --fact <text>: alternative to positional content (matches mpm_memory action=save payload field name)
	fileArg := ""    // --file <path>: read content from file (F-A3 fix — no silent 64KB truncation)
	tagsArg := ""    // --tags <csv>: comma-separated tags
	weightArg := 1.0 // --weight <0-100>: weight; default 1 (matches the hardcoded value AddMemoryWithWeight substitutes)
	weightSet := false // tracks whether --weight was actually supplied (so we can tell "user passed 0" from "user didn't pass anything")
	contentArgs := make([]string, 0, len(flagsArgs)+len(rawArgs))
	for i := 0; i < len(flagsArgs); i++ {
		switch flagsArgs[i] {
		case "-i", "--interactive":
			interactive = true
		case "--file", "-f":
			// F-A3: --file reads content from a file in full, capped at
			// memoryMaxFileBytes (100 MiB). The previous behaviour was
			// either to fall through as positional content (length 18
			// stored for `mpm memory add --file /tmp/p1g.txt`) or to
			// silently truncate to ~64KB. We now reject oversized files
			// explicitly and never silently truncate.
			if i+1 >= len(flagsArgs) {
				return respond("", "--file requires a path\n", 1)
			}
			i++
			fileArg = flagsArgs[i]
		case "--fact":
			if i+1 >= len(flagsArgs) {
				return respond("", "--fact requires a value\n", 1)
			}
			i++
			factArg = flagsArgs[i]
		case "--tags":
			if i+1 >= len(flagsArgs) {
				return respond("", "--tags requires a value\n", 1)
			}
			i++
			tagsArg = flagsArgs[i]
		case "--weight":
			if i+1 >= len(flagsArgs) {
				return respond("", "--weight requires a value\n", 1)
			}
			i++
			w, parseErr := strconv.ParseFloat(flagsArgs[i], 64)
			if parseErr != nil {
				return respond("", fmt.Sprintf("--weight: invalid float %q\n", flagsArgs[i]), 1)
			}
			weightArg = w
			weightSet = true
		case "--expires-in":
			if i+1 >= len(flagsArgs) {
				return respond("", "--expires-in requires a value (e.g. 7d, 24h, 30m)\n", 1)
			}
			i++
			expiresIn = flagsArgs[i]
			// F-H2: validate the duration at parse time so we don't
			// create the memory before discovering the value is
			// garbage (which would force a rollback row, leaving
			// a memory with no TTL that the operator didn't expect).
			if _, parseErr := parseDuration(expiresIn); parseErr != nil {
				return respond("", fmt.Sprintf("--expires-in %q: invalid duration (use Nd, Nh, Nm, or Go duration like 24h)\n", expiresIn), 1)
			}
		default:
			contentArgs = append(contentArgs, flagsArgs[i])
		}
	}

	// S1: append rawArgs (everything after the first `--`) verbatim
	// to contentArgs. These tokens must not be re-parsed as flags —
	// they are positional content per POSIX `--` semantics.
	contentArgs = append(contentArgs, rawArgs...)

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

	// F-2 (2026-09-04 residual inventory, P1): reject whitespace-only
	// content at the CLI boundary. Pre-fix, a literal "   " passed both
	// the `factArg != ""` truthiness check (whitespace is truthy in Go)
	// and the positional empty check, then fell through to
	// store.AddMemoryWithWeight which produced a row with content="   "
	// — a low-quality artifact future retrieval would surface verbatim.
	// Worse, the topic-suggestion path that runs after the write
	// dereferences the new memory's ID and panics on whitespace content
	// (FTS sanitizer returns "" → NULL path was not nil-safe).
	// Match the pattern already in use at handlers_epistemology.go:81
	// for theory hypothesis.
	if strings.TrimSpace(content) == "" {
		return respond("", "Error: memory content is required (non-empty)\n", 1)
	}
	// Persist the trimmed form so retrieval doesn't surface leading/trailing
	// whitespace as part of the content.
	content = strings.TrimSpace(content)

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
	mem, err, _ := store.AddMemoryWithWeight(content, "memories", tagsList, memMetadata, "", "cli", weightArg)
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
		return respond("", "Usage: mpm memory search <query> [--limit N] [--json]", 1)
	}

	// Alpha-4.1 F-005 / W-006: strip `--json` / `-j` from the argument
	// list before constructing the query. Pre-fix this joined the
	// literal flag string into the FTS5 query and returned zero hits
	// because no document contained the token "--json".
	//
	// Stage S2 of the CLI refactor (2026-09-06): use the canonical
	// ExtractJSONFlag helper instead of the per-handler
	// stripMemoryFlagToken scanner. Same contract for the
	// `--json` / `-j` exact-match forms; the stripMemoryFlagToken
	// helper's extra `--flag=value` prefix match is dropped here
	// because no test or document exercises that form for memory
	// search (the F-005/W-006 regression only covered `--json`
	// exact match). See cli_args_json.go for the canonical contract.
	wantJSON, cleaned := ExtractJSONFlag(args)
	// D-4.2: also strip `--limit N` / `-l N` so the limit flag doesn't
	// pollute the FTS5 query. Default to 20 (matches prior hardcoded
	// behaviour); explicit --limit N caps the result set.
	limit := 20
	cleaned, limit = extractLimitFlag(cleaned, limit)
	query := strings.Join(cleaned, " ")
	if strings.TrimSpace(query) == "" {
		return respond("", "Usage: mpm memory search <query> [--limit N] [--json]", 1)
	}

	store := getMemoryStore()

	memories, err := store.FullTextSearch(query, "memories", limit)
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

// stripMemoryFlagToken (Stage S2 of the CLI refactor, 2026-09-06):
// removed. The helper had a single --json / -j call site
// (handleMemorySearch), which now uses the canonical ExtractJSONFlag
// helper in cli_args_json.go. The `--flag=value` form that this
// helper also handled is not exercised by any test or documented
// caller for --json in MPM today, so the contract narrowing is safe.
// If a future stage needs a general flag-stripper (e.g., for
// parseBoundedInt in S3), reintroduce one with the same variadic
// shape under cli_args.go or similar.

// extractLimitFlag parses `--limit N` / `-l N` from args, returning the
// remaining args and the parsed limit. D-4.2: supports both space-separated
// (`--limit 5`) and equals-form (`--limit=5`); an unparseable value leaves
// the default untouched. Used by `mpm memory search --limit`.
func extractLimitFlag(args []string, defaultLimit int) (cleaned []string, limit int) {
	limit = defaultLimit
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--limit" || a == "-l":
			// Value is the next arg, if present and parseable.
			if i+1 < len(args) {
				if n, err := strconv.Atoi(args[i+1]); err == nil && n > 0 {
					limit = n
				}
				i++ // consume the value regardless
			}
		case strings.HasPrefix(a, "--limit="):
			if n, err := strconv.Atoi(strings.TrimPrefix(a, "--limit=")); err == nil && n > 0 {
				limit = n
			}
		case strings.HasPrefix(a, "-l="):
			if n, err := strconv.Atoi(strings.TrimPrefix(a, "-l=")); err == nil && n > 0 {
				limit = n
			}
		default:
			out = append(out, a)
		}
	}
	return out, limit
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
