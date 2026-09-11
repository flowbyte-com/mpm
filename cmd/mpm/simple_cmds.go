package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/flowbyte-com/mpm-core/config"

	mpminternal "github.com/flowbyte-com/mpm-core"
	"github.com/flowbyte-com/mpm-core/synth"
	"github.com/flowbyte-com/mpm-core/usererror"
)

// =============================================================================
// Simplified Memory Commands
// All commands work on memories - collections are just metadata flags
// =============================================================================

// mpm add <content> — Add a new memory
func handleAdd(args []string) int {
	fs := flag.NewFlagSet("add", flag.ContinueOnError)
	collection := fs.String("collection", "memories", "Collection name")
	tag := fs.String("tag", "", "Tag to add (can specify multiple)")
	session := fs.String("session", "", "Session ID to associate")
	weight := fs.Float64("weight", float64(mpminternal.DefaultMemoryWeight), "Initial weight (canonical default matches mpm_memory save)")
	ttl := fs.String("ttl", "", "Time to live (e.g., 7d, 24h)")
	jsonOutput := fs.Bool("json", false, "Output JSON for tool integration")
	fs.Usage = func() {
		fmt.Println("Usage: mpm add [flags] <content>")
		fmt.Println("Flags:")
		fmt.Println("  --tag <a,b,c>     Comma-separated tags")
		fmt.Println("  --weight <0-100> Initial weight (canonical default matches mpm_memory save)")
		fmt.Println("  --collection <c> Collection name (default memories)")
		fmt.Println("  --ttl <duration> Time to live (e.g. 7d, 24h)")
		fmt.Println("Notes:")
		fmt.Println("  To add content starting with '-' or '--', prefix with '--' to")
		fmt.Println("  terminate flag parsing: mpm add -- '---yaml-front-matter'")
		fmt.Println("Example: mpm add --tag personal,important 'Remember to call mom'")
	}

	// Pre-scan: Go's flag.Parse switches to positional-only mode once a
	// positional argument is seen. This means `mpm add "hello" --weight 5`
	// would silently absorb `--weight 5` as content. The same pattern is
	// used in handleMemoryAdd (handlers_memory.go) — pull all known
	// flags out before the positional content so flag.Parse sees them.
	cleaned := reorderFlagsBeforePositionals(args[1:],
		"--collection", "--tag", "--session", "--weight", "--ttl", "--json")

	// Parse with standard flag parser, which handles arbitrary argument ordering.
	// After parsing, fs.Args() contains the positional arguments (the content).
	if err := fs.Parse(cleaned); err != nil {
		// On error (e.g., unknown flag), fs.HasError() is true; error already printed
		return 1
	}

	// Extract content: use first positional arg, or join all for multi-word content
	if fs.NArg() == 0 {
		usererror.Error("content required")
		return 1
	}
	content := strings.Join(fs.Args(), " ")

	if *weight < 0 || *weight > 100 {
		usererror.Error("--weight must be 0-100 (got %v) — 0 means use the canonical default; fractional values <1 are the legacy 0-1.0 float scale", *weight)
		return 1
	}

	dm := getDB()
	if dm == nil {
		return 1
	}

	tags := []string{}
	if *tag != "" {
		for _, t := range strings.Split(*tag, ",") {
			t = strings.TrimSpace(t)
			if t != "" {
				tags = append(tags, t)
			}
		}
	}

	metadata := map[string]interface{}{
		"provenance": map[string]interface{}{
			"source":  "human",
			"model":   "direct",
			"compute": "absolute",
			"agent":   "mpm_cli",
			"persona": "operator",
		},
	}

	// Size guard: cap memory content at 1 MiB so a stray huge --content
	// (or paste of a binary blob) can't OOM the CLI. The reference
	// ingest path has its own 100 MiB ceiling tuned for parsed text.
	const maxAddBytes = 1 << 20 // 1 MiB
	if len(content) > maxAddBytes {
		return usererror.Error("memory content too large: %d bytes (max %d = 1 MiB); split into chunks or use `mpm reference add`", len(content), maxAddBytes)
	}

	// Auto-embed: try real embeddings; on provider failure we still
	// save the memory with NULL embedding so the operator never loses
	// data, then surface the error so they can run
	// `mpm ops backfill-embeddings` later.
	_, embedErr := mpminternal.EmbedText(content)
	// 2026-09-10 cleanup: route through AddMemoryWithWeight (not
	// SaveMemory directly) so the legacy-float → 0-100 column
	// normalization is applied uniformly. Pre-fix, `mpm add` skipped
	// normalization — a default weight of 0.5 (legacy float) landed
	// in the column as 0.5 instead of 5, while `mpm memory add`
	// (which goes through AddMemoryWithWeight) stored 5. The two CLI
	// surfaces now agree: default weight 0.5 → column 5.
	//
	// AddMemoryWithWeight runs its own embedding call internally; the
	// local embedErr is kept only so we can surface the same warning
	// if embedding failed at the pre-store step.

	// Wrap in a MemoryStore backed by dm to call AddMemoryWithWeight.
	store := &mpminternal.MemoryStore{
		DB:         &mpminternal.SQLiteConnection{DB: dm.SQLDB()},
		DM:         dm,
		MirrorFile: filepath.Join(config.GetMPMDir(), "src", "db", "mirror.jsonl"),
	}
	mem, err, _ := store.AddMemoryWithWeight(content, *collection, tags, metadata, *session, "human", *weight)
	if err != nil {
		return usererror.Errorf(fmt.Sprintf("save memory: %v", err))
	}
	id := mem.ID
	if embedErr != nil {
		cfg := mpminternal.DefaultEmbeddingConfig()
		usererror.Warn("Memory saved (id %s) with NULL embedding.\n"+
			"  embedding provider %q is unreachable: %v\n"+
			"Run `mpm ops backfill-embeddings` after the provider is available.",
			id, cfg.ProviderName, embedErr)
		return 1
	}

	// Set TTL if specified
	if *ttl != "" {
		dur, parseErr := parseDuration(*ttl)
		if parseErr != nil {
			usererror.Error("invalid --ttl %q: %v", *ttl, parseErr)
			return 1
		}
		t := time.Now().Add(dur)
		if err := dm.SetMemoryTTL(id, t); err != nil {
			usererror.Error("%v", err)
			return 1
		}
	}

	// D-017: distinguish "added" from "already exists". SaveMemory is
	// content-hash idempotent — a duplicate identical save returns the
	// existing row's id without inserting. Querying the row's
	// created_at lets us tell the user whether this was a fresh add
	// or a dedup hit, so a workflow that re-saves a memory by mistake
	// doesn't silently lose visibility.
	isNew := true
	if row := dm.SQLDB().QueryRow(
		`SELECT created_at FROM memories WHERE id = ?`, id,
	); row != nil {
		var createdAt int64
		if err := row.Scan(&createdAt); err == nil {
			// M3 audit D-017: a row whose created_at is meaningfully
			// older than the current second cannot have been inserted
			// by this call — it must pre-exist. Use a 2-second window
			// to absorb SQLite timestamp rounding on fast machines.
			if time.Now().Unix()-createdAt > 2 {
				isNew = false
			}
		}
	}

	if *jsonOutput {
		data, _ := json.Marshal(map[string]interface{}{
			"id":           id,
			"success":      true,
			"already_existed": !isNew,
		})
		fmt.Println(string(data))
	} else if isNew {
		fmt.Printf("Added memory %s to %s (weight=%v)\n", id, *collection, *weight)
	} else {
		fmt.Printf("Memory already exists: %s (idempotent save — no new row written)\n", id)
	}
	return 0
}

// mpm ls — List memories
// reorderFlagsBeforePositionals rewrites argv so all named flags (and
// their values) appear before the first positional argument. Go's
// standard library flag.Parse switches to positional-only mode once
// any positional argument is seen, so a flag written AFTER the
// positional content (e.g. `mpm add "hello" --weight 5`) is silently
// absorbed as content. This helper extracts the named flags and
// reorders argv so flag.Parse sees them in the expected order.
//
// Two flags that take values are recognized: those in flagNames take
// the immediately following argv entry as their value. Single-token
// boolean flags (currently only --json) have no value and are
// extracted whole. Unknown flags are passed through unchanged so
// flag.Parse can emit its own error.
//
// This is intentionally narrow: callers must list every flag their
// flag.NewFlagSet knows about. The function does not attempt to
// be a general-purpose flag rewriter.
func reorderFlagsBeforePositionals(argv []string, flagNames ...string) []string {
	boolFlags := map[string]bool{}
	valueFlags := map[string]bool{}
	for _, f := range flagNames {
		if f == "--json" {
			boolFlags[f] = true
		} else {
			valueFlags[f] = true
		}
	}

	var flags, positionals []string
	i := 0
	for i < len(argv) {
		a := argv[i]
		if boolFlags[a] {
			flags = append(flags, a)
			i++
			continue
		}
		if valueFlags[a] {
			// Take this flag and its value (if present).
			flags = append(flags, a)
			if i+1 < len(argv) {
				flags = append(flags, argv[i+1])
				i += 2
				continue
			}
			i++
			continue
		}
		positionals = append(positionals, a)
		i++
	}
	out := make([]string, 0, len(argv))
	out = append(out, flags...)
	out = append(out, positionals...)
	return out
}

func handleLs(args []string) int {
	fs := flag.NewFlagSet("ls", flag.ContinueOnError)
	collection := fs.String("collection", "", "Filter by collection")
	tag := fs.String("tag", "", "Filter by tag")
	limit := fs.Int("limit", 20, "Maximum results")
	since := fs.String("since", "", "Since date (YYYY-MM-DD)")
	until := fs.String("until", "", "Until date (YYYY-MM-DD)")
	jsonOutput := fs.Bool("json", false, "Emit JSON array of memory rows on stdout (LF1 reorder still applies for positional content)")
	fs.Usage = func() {
		fmt.Println("Usage: mpm ls [options] [--json]")
		fmt.Println("\nList options:")
		fs.PrintDefaults()
		fmt.Println("\nWhen --json is set, output is a single JSON array of memory")
		fmt.Println("rows on stdout. Diagnostics stay on stderr.")
	}
	if err := fs.Parse(args[1:]); err != nil {
		return 1
	}

	dm := getDB()
	if dm == nil {
		return 1
	}

	memories, err := dm.GetMemoriesForExport(*collection, *since, *until)
	if err != nil {
		usererror.Error("%v", err)
		return 1
	}

	filtered := memories
	if *tag != "" {
		filtered = filterByTag(filtered, *tag)
	}

	if len(filtered) > *limit {
		filtered = filtered[:*limit]
	}

	if *jsonOutput {
		// JSON path: emit a single array on stdout. Diagnostics must
		// NOT land here (R2 — agent integrations would otherwise see
		// unparseable mixed output). Use os.Stdout directly via fmt so
		// usererror warnings on stderr stay separate.
		rows := make([]map[string]interface{}, 0, len(filtered))
		for _, m := range filtered {
			rows = append(rows, lsMemoryRow(m))
		}
		data, mErr := json.Marshal(rows)
		if mErr != nil {
			usererror.Error("ls --json: marshal failed: %v", mErr)
			return 1
		}
		fmt.Println(string(data))
		return 0
	}

	if len(filtered) == 0 {
		fmt.Println("No memories found")
		return 0
	}

	fmt.Printf("\n%-18s %-20s %-10s %-6s %s\n", "ID", "CREATED", "COLLECTION", "WEIGHT", "CONTENT")
	fmt.Println(strings.Repeat("-", 80))

	for _, m := range filtered {
		id := stringFromMap(m, "id")
		created := stringFromMap(m, "created_at")
		coll := stringFromMap(m, "collection")
		// weight is REAL in the schema → float64 from GetMemoriesForExport.
		// Previously this asserted int64/float64 with a default of 1 that
		// masked every actual weight (D-001). float64FromMap surfaces
		// any future type mismatch via a usererror warning.
		wf, _ := float64FromMap(m, "weight")
		w := int(wf)
		content := stringFromMap(m, "content")
		meta := stringFromMap(m, "metadata")
		chip := ""
		if strings.Contains(meta, `"status":"challenged"`) {
			chip = " [CHALLENGED]"
		}
		if len(content) > 50 {
			content = content[:50] + "..."
		}

		// F-I1: show the actual memory ID (truncated to 16 chars to
		// fit the column) instead of the row index. The row index is
		// useless for follow-up commands (mpm show <index> would
		// never find anything because IDs are hex strings, not
		// integers) and confused operators about how to address a
		// specific memory.
		idDisplay := id
		if len(idDisplay) > 16 {
			idDisplay = idDisplay[:16]
		}
		fmt.Printf("%-18s %.20s %-10s %-6d%s %s\n", idDisplay, created, coll, w, chip, content)
	}

	fmt.Printf("\n%d memories shown\n", len(filtered))
	return 0
}

// lsMemoryRow normalizes a memory map into the JSON-friendly shape used
// by `mpm ls --json`. Kept consistent with the field names emitted by
// `mpm show --json` so an agent can rely on the same keys across the
// list and show surfaces.
func lsMemoryRow(m map[string]interface{}) map[string]interface{} {
	row := map[string]interface{}{}
	for k, v := range m {
		row[k] = v
	}
	return row
}

func filterByTag(memories []map[string]interface{}, tag string) []map[string]interface{} {
	var result []map[string]interface{}
	for _, m := range memories {
		tags, ok := m["tags"].(string)
		if !ok {
			continue
		}
		if strings.Contains(tags, tag) {
			result = append(result, m)
		}
	}
	return result
}

// mpm show <id> — Show memory details
func handleShow(args []string) int {
	fs := flag.NewFlagSet("show", flag.ContinueOnError)
	jsonOutput := fs.Bool("json", false, "Emit single JSON object to stdout; diagnostics stay on stderr")
	fs.Usage = func() {
		fmt.Println("Usage: mpm show [--json] <id>")
		fmt.Println("\nShow options:")
		fs.PrintDefaults()
		fmt.Println("\nWhen --json is set, the memory row is emitted as a single")
		fmt.Println("JSON object. Without --json the human-readable card is printed.")
	}
	// LF1 parity: Go's flag.Parse switches to positional-only mode once
	// a positional argument is seen. `mpm show <id> --json` would silently
	// absorb "--json" as part of the ID and fail to look up the memory.
	// reorderFlagsBeforePositionals pulls --json out before the positional
	// ID so flag.Parse sees them in the expected order.
	cleaned := reorderFlagsBeforePositionals(args[1:], "--json")
	if err := fs.Parse(cleaned); err != nil {
		return 1
	}
	id := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if id == "" {
		usererror.Usage("mpm show [--json] <id>")
		return 1
	}

	dm := getDB()
	if dm == nil {
		return 1
	}
	return runShow(dm, id, *jsonOutput)
}

// runShow is the injectable core of `mpm show` so tests can drive it
// against a hermetic database. The jsonOutput parameter routes through
// the JSON envelope when true; otherwise the human-readable card is
// emitted as before.
func runShow(dm mpminternal.CoreDB, id string, jsonOutput bool) int {
	mem, err := dm.GetMemory(id)
	if err != nil || mem == nil {
		usererror.Error("Memory not found: %s", id)
		return 1
	}

	if jsonOutput {
		// JSON envelope shape: id, collection, created_at, session_id,
		// weight, tags, content, metadata. Kept consistent with the
		// row shape `mpm ls --json` emits (each row in `ls --json` has
		// the same keys), so an agent iterating `ls --json` then
		// `show --json <id>` sees a stable schema.
		row := map[string]interface{}{}
		for k, v := range mem {
			row[k] = v
		}
		row["id"] = id
		data, mErr := json.Marshal(row)
		if mErr != nil {
			usererror.Error("show --json: marshal failed: %v", mErr)
			return 1
		}
		fmt.Println(string(data))
		return 0
	}

	fmt.Println("\n══════════════════════════════════════════")
	fmt.Printf("ID:           %s\n", id)
	fmt.Printf("Collection:   %s\n", mem["collection"])
	fmt.Printf("Created:      %s\n", mem["created_at"])

	if sess, ok := mem["session_id"].(string); ok {
		fmt.Printf("Session:      %s\n", sess)
	}

	// GetMemory builds its map with Go `int` for weight (web_db.go), so an
	// int64 assertion here silently fell back to the hardcoded 1 and
	// `mpm debug show` printed Weight: 1 regardless of stored value.
	// Accept every numeric shape the driver may hand us instead of defaulting.
	w := 1
	switch we := mem["weight"].(type) {
	case int:
		w = we
	case int64:
		w = int(we)
	case float64:
		w = int(we)
	}
	fmt.Printf("Weight:       %d\n", w)

	meta, _ := mem["metadata"].(string)
	if strings.Contains(meta, `"status":"challenged"`) {
		fmt.Println("[CHALLENGED]")
	}

	fmt.Printf("Tags:         %s\n", mem["tags"])

	fmt.Println("\n──────────────────────────────────────────")
	fmt.Printf("Content:\n%s\n", mem["content"])
	fmt.Print("══════════════════════════════════════════\n")

	return 0
}

// mpm rm <id> [--force] — Soft delete memory
//
// W-011: prime-directive armor. Seed memories are flagged with
// `is_prime_directive=1` in the DB and labeled "non-negotiable" in
// their content. The substrate previously honored neither — a typo
// in `mpm rm <id>` could shred a load-bearing bootstrap memory. Now
// the rm handler reads the flag up-front and refuses the deletion
// unless `--force` is supplied.
//
// The `--force` token is consumed by router.parseFlags BEFORE we see
// the args: it sets `MPM_FORCE=1` and strips the token from argv
// (router.go:467). So the rm handler reads the env var rather than
// scanning for the literal token — otherwise the flag would be eaten
// upstream and we'd silently ignore it.
func handleRm(args []string) int {
	force := os.Getenv("MPM_FORCE") == "1"
	if len(args) < 2 {
		usererror.Usage("mpm rm <id> [--force]")
		return 1
	}
	id := args[1]

	dm := getDB()
	if dm == nil {
		return 1
	}

	// W-011: prime-directive check. Read the flag BEFORE issuing the
	// soft-delete UPDATE so we can refuse with a clear error rather
	// than silently stripping the protection. The query is bounded
	// by the id PK so it's a single row read, not a scan.
	if !force {
		var isPrime int
		err := dm.SQLDB().QueryRow(
			`SELECT is_prime_directive FROM memories WHERE id = ? AND deleted_at IS NULL`,
			id,
		).Scan(&isPrime)
		if err == nil && isPrime == 1 {
			usererror.Error(
				"refusing to delete %s: memory is flagged is_prime_directive=1 (non-negotiable). "+
					"Pass --force to override (e.g. `mpm rm %s --force`)",
				id, id,
			)
			return 1
		}
		// err != nil is fine here — the row may not exist, and the
		// soft-delete UPDATE below will surface "no live memory" via
		// the rows-affected check.
	}

	// `err` is reused below; it was previously declared by
	// `dm, err := mpminternal.NewDatabaseManager("")`; declare explicitly
	// because the singleton lookup doesn't introduce one.
	var err error

	// Soft delete by setting deleted_at (INTEGER Unix epoch, matches expires_at).
	// Guard on deleted_at IS NULL so re-deleting a deleted row is a
	// no-op (idempotent) AND so we can detect a missing id via the
	// rows-affected check.
	res, err := dm.SQLDB().Exec(`UPDATE memories SET deleted_at = CAST(strftime('%s','now') AS INTEGER) WHERE id = ? AND deleted_at IS NULL`, id)
	if err != nil {
		usererror.Error("%v", err)
		return 1
	}
	if affected, aerr := res.RowsAffected(); aerr != nil {
		usererror.Error("%v", aerr)
		return 1
	} else if affected == 0 {
		usererror.Error("rm failed: no live memory with id %s (not found or already deleted)", id)
		return 1
	}

	fmt.Printf("Deleted memory %s\n", id)
	return 0
}

// mpm patch-memory <id> <json-patch> — Patch metadata JSON in-place (no content change, no FTS re-index)
// mpm patch-memory <id> <json-patch> — Patch metadata JSON in-place (no content change, no FTS re-index).
//
// Two forms are supported:
//   1. mpm patch-memory <id> '{...}'   — full JSON object patch (the canonical form)
//   2. mpm patch-memory <id> -- <key>=<value> [...]  — ergonomic shortcut; the CLI
//      builds a JSON patch object from one or more key=value pairs and applies
//      them via json_patch (upsert semantics). Multiple pairs are merged.
//
// T28 (2026-09-11): added the ergonomic -- <key>=<value> form so operators
// don't have to escape JSON braces on the shell. The legacy positional
// JSON-object form is unchanged — back-compat verified by the existing
// patch-memory usage in handlers_epistemology.go (resolve_theory
// constructs a JSON patch and calls UpdateMemoryMetadata directly,
// bypassing this handler entirely).
//
// Examples:
//   mpm patch-memory abc123 '{ "status": "proven" }'
//   mpm patch-memory abc123 -- status=proven resolved_at=2026-09-11T13:00:00Z
//   mpm patch-memory abc123 -- confidence=0.9
//
// JSON values: bare values are parsed as JSON literals (numbers, booleans,
// nulls). For string values with spaces or special characters, wrap the
// value in quotes: `-- note="needs follow-up"` (the value parses as the
// JSON string "needs follow-up").
func handlePatchMemory(args []string) int {
	if len(args) < 3 {
		usererror.Usage("mpm patch-memory <id> {json-patch} | -- <key>=<value> [...]")
		return 1
	}

	id := args[1]
	var patchJSON string

	if args[2] == "--" {
		// Ergonomic shortcut form: -- <key>=<value> [<key>=<value> ...]
		if len(args) < 4 {
			usererror.Error("patch-memory -- requires at least one key=value pair")
			return 1
		}
		pairs := args[3:]
		patch := make(map[string]interface{}, len(pairs))
		for _, pair := range pairs {
			eq := strings.IndexByte(pair, '=')
			if eq < 0 {
				usererror.Error("patch-memory: invalid pair %q (expected key=value)", pair)
				return 1
			}
			key := pair[:eq]
			raw := pair[eq+1:]
			val, err := parseJSONPatchValue(raw)
			if err != nil {
				usererror.Error("patch-memory: invalid value for key %q: %v", key, err)
				return 1
			}
			patch[key] = val
		}
		encoded, err := json.Marshal(patch)
		if err != nil {
			usererror.Error("patch-memory: encode patch: %v", err)
			return 1
		}
		patchJSON = string(encoded)
	} else {
		patchJSON = args[2]
		// Basic JSON validity check
		if !strings.HasPrefix(strings.TrimSpace(patchJSON), "{") {
			usererror.Error("patch must be a JSON object string (or use the -- <key>=<value> form)")
			return 1
		}
	}

	dm := getDB()
	if dm == nil {
		return 1
	}

	if err := dm.UpdateMemoryMetadata(id, patchJSON); err != nil {
		usererror.Error("%v", err)
		return 1
	}

	fmt.Printf("Patched metadata for memory %s\n", id)
	return 0
}

// parseJSONPatchValue accepts a string from the CLI and parses it into a
// JSON value. Bare values try JSON literals first (number, bool, null);
// if that fails, the value is treated as a JSON string. Quoted values
// (`"..."`) are parsed as a JSON string literal so spaces and special
// characters round-trip cleanly.
func parseJSONPatchValue(raw string) (interface{}, error) {
	if raw == "" {
		return nil, fmt.Errorf("empty value")
	}
	// Try a JSON literal first (handles true/false/null/numbers).
	var anyVal interface{}
	if err := json.Unmarshal([]byte(raw), &anyVal); err == nil {
		// Only accept literal scalars — strings must be quoted.
		if _, isString := anyVal.(string); isString {
			return raw, nil // treat as raw string
		}
		return anyVal, nil
	}
	// Otherwise the value is a raw string. Use it verbatim — the caller
	// can quote it if they need to embed spaces or special characters.
	return raw, nil
}

// mpm promote <id> — Make memory LTM
func handlePromote(args []string) int {
	if len(args) < 2 {
		usererror.Usage("mpm promote <id>")
		return 1
	}

	id := args[1]

	dm := getDB()
	if dm == nil {
		return 1
	}

	// Clear TTL (make permanent) and reinforce heavily
	if err := dm.SetMemoryTTL(id, time.Time{}); err != nil {
		usererror.Error("%v", err)
		return 1
	}
	if err := dm.ReinforceMemory(id, 9); err != nil {
		usererror.Error("%v", err)
		return 1
	}

	// Update weight to 10 and is_long_term = 1. Guard on deleted_at so a
	// soft-deleted row can't be silently resurrected by promote, AND on
	// collection != 'lessons' so a lessons row (which has its own
	// lifecycle) can't be silently locked into LTM by promote.
	res, err := dm.SQLDB().Exec(`
		UPDATE memories
		SET weight = 10, is_long_term = 1
		WHERE id = ? AND deleted_at IS NULL AND collection != 'lessons'
	`, id)
	if err != nil {
		usererror.Error("%v", err)
		return 1
	}
	affected, aerr := res.RowsAffected()
	if aerr != nil {
		usererror.Error("%v", aerr)
		return 1
	}
	if affected == 0 {
		// Distinguish "missing/deleted" from "rejected as lesson".
		var exists int
		if err := dm.SQLDB().QueryRow(`SELECT COUNT(*) FROM memories WHERE id = ? AND deleted_at IS NULL`, id).Scan(&exists); err != nil {
			usererror.Error("%v", err)
			return 1
		}
		if exists == 0 {
			usererror.Error("promote failed: no live row with id %s (not found or deleted)", id)
		} else {
			usererror.Error("promote refused: lessons rows have their own lifecycle; use `mpm lessons` commands instead")
		}
		return 1
	}

	fmt.Printf("Promoted memory %s to LTM (weight=10)\n", id)
	return 0
}

// mpm reinforce <id> [delta] — Increment reinforcement
// handleFeedback dispatches +<id> and -<id> shortcuts.
// Delta sign is determined by the caller: +1 for reinforce, -1 for weaken.
func handleFeedback(args []string) int {
	if len(args) < 3 {
		usererror.Error("internal: handleFeedback requires id and delta")
		return 1
	}
	id := args[1]
	delta, err := strconv.Atoi(args[2])
	if err != nil || delta == 0 {
		usererror.Error("invalid delta %q", args[2])
		return 1
	}

	dm := getDB()
	if dm == nil {
		return 1
	}

	// Check memory exists and challenged status
	mem, err := dm.GetMemory(id)
	if err != nil || mem == nil {
		usererror.Error("memory not found: %s", id)
		return 1
	}
	isChallenged := false
	if metaStr, ok := mem["metadata"].(string); ok && metaStr != "" {
		var meta map[string]interface{}
		if json.Unmarshal([]byte(metaStr), &meta) == nil {
			if s, ok := meta["status"].(string); ok && s == "challenged" {
				isChallenged = true
			}
		}
	}

	if delta > 0 {
		// Positive feedback: implicit challenge restore if challenged, then reinforce
		if isChallenged {
			if err := dm.ChallengeAndReinforce(id, delta); err != nil {
				usererror.Error("%v", err)
				return 1
			}
			fmt.Printf("⚡ Reinforced memory %s (+%d) — challenge cleared\n", id, delta)
		} else {
			if err := dm.ReinforceMemory(id, delta); err != nil {
				usererror.Error("%v", err)
				return 1
			}
			fmt.Printf("⚡ Reinforced memory %s (+%d)\n", id, delta)
		}
	} else {
		// Negative feedback: weaken with hard floor at 1
		if err := dm.AdjustMemoryWeight(id, delta); err != nil {
			usererror.Error("%v", err)
			return 1
		}
		// Check if already at minimum
		updated, _ := dm.GetMemory(id)
		if w, ok := updated["weight"].(int64); ok && w <= 1 {
			fmt.Printf("Weakened memory %s (%d) — at minimum weight (1)\n", id, delta)
		} else {
			fmt.Printf("⚡ Weakened memory %s (%d)\n", id, delta)
		}
	}
	return 0
}

// handleReinforce is the public command handler for `mpm reinforce`.
func handleReinforce(args []string) int {
	if len(args) < 2 {
		usererror.Usage("mpm reinforce <id> [delta]")
		return 1
	}

	id := args[1]
	delta := 1
	if len(args) >= 3 {
		d, err := strconv.Atoi(args[2])
		if err != nil {
			usererror.Error("reinforce: invalid delta %q (must be an integer)", args[2])
			return 1
		}
		if d <= 0 {
			usererror.Error("reinforce: delta must be >= 1 (got %d) — use `mpm weaken` to reduce weight", d)
			return 1
		}
		delta = d
	}

	dm := getDB()
	if dm == nil {
		return 1
	}

	// `err` is reused below; it was previously declared by
	// `dm, err := mpminternal.NewDatabaseManager("")`; declare explicitly
	// because the singleton lookup doesn't introduce one.
	var err error

	err = dm.ReinforceMemory(id, delta)
	if err != nil {
		usererror.Error("%v", err)
		return 1
	}

	fmt.Printf("Reinforced memory %s (+%d)\n", id, delta)
	return 0
}

// mpm weaken <id> [delta] — Decrement reinforcement
func handleWeaken(args []string) int {
	if len(args) < 2 {
		usererror.Usage("mpm weaken <id> [delta]")
		return 1
	}

	id := args[1]
	delta := 1
	if len(args) >= 3 {
		d, err := strconv.Atoi(args[2])
		if err != nil {
			usererror.Error("weaken: invalid delta %q (must be an integer)", args[2])
			return 1
		}
		if d <= 0 {
			usererror.Error("weaken: delta must be >= 1 (got %d) — use `mpm reinforce` to increase weight", d)
			return 1
		}
		delta = d
	}

	dm := getDB()
	if dm == nil {
		return 1
	}

	// `err` is reused below; declare explicitly because the singleton
	// lookup (getDB()) doesn't introduce one.
	var err error
	err = dm.WeakenMemory(id, delta)
	if err != nil {
		usererror.Error("%v", err)
		return 1
	}

	fmt.Printf("Weakened memory %s (-%d)\n", id, delta)
	return 0
}
// mpm set-weight <id> <weight> — Set weight directly
func handleSetWeight(args []string) int {
	if len(args) < 3 {
		usererror.Usage("mpm set-weight <id> <weight>")
		return 1
	}

	id := args[1]
	// W-004 (2026-08-31): parse as float64 so fractional weights like 7.5
	// reach the REAL column intact. The previous Atoi parse truncated to
	// int before the SQLite UPDATE.
	w, err := strconv.ParseFloat(args[2], 64)
	if err != nil {
		usererror.Error("invalid weight '%s'", args[2])
		return 1
	}
	if w < 0 || w > 100 {
		usererror.Error("weight must be 0-100 (got %v)", w)
		return 1
	}

	dm := getDB()
	if dm == nil {
		return 1
	}

	// Defense Triad rule 3: assert the row exists and is live. A bare
	// `WHERE id = ?` would resurrect soft-deleted rows and silently
	// succeed on a typo'd id.
	res, err := dm.SQLDB().Exec(`UPDATE memories SET weight = ? WHERE id = ? AND deleted_at IS NULL`, w, id)
	if err != nil {
		usererror.Error("%v", err)
		return 1
	}
	if affected, aerr := res.RowsAffected(); aerr != nil {
		usererror.Error("%v", aerr)
		return 1
	} else if affected == 0 {
		usererror.Error("set-weight failed: no live memory with id %s (not found or deleted)", id)
		return 1
	}

	fmt.Printf("Set weight of %s to %v\n", id, w)
	return 0
}

// mpm snooze <id> [--days N | --duration <n><unit> | --until <RFC3339>] —
// Bump memory relevance without promoting to LTM. Increments weight by 1
// (capped at 9 to avoid LTM promotion) and refreshes last_accessed_at.
// Never sets is_long_term or inflates weight to >= 10.
//
// F-E1: the CLI used to silently ignore --duration 1h and apply the default
// 1-day bump. The fix accepts an explicit duration with units (m, h, d, w)
// and rejects unknown units. --days remains supported for backwards
// compatibility (it is identical to --duration Nd).
//
// T26 (2026-09-11): --until <RFC3339-time> lets operators express
// "snooze until tomorrow morning" or "snooze until next Monday 09:00".
// The flag accepts RFC3339 / RFC3339Nano with timezone (e.g.
// "2026-09-12T09:00:00Z" or "2026-09-12T11:00:00+02:00") and converts
// to seconds-from-now. The 1-year cap and all other guardrails from
// --duration apply identically — a typo'd far-future timestamp surfaces
// the same clear "use `mpm promote` for permanent durability" error.
// --days and --duration remain supported for muscle memory and
// scripting convenience.
func handleSnooze(args []string) int {
	if len(args) < 2 {
		usererror.Usage("mpm snooze <id> [--days N | --duration <n><unit> | --until <RFC3339>]")
		return 1
	}
	id := args[1]

	// parseDurationSeconds accepts a string like "1h", "30m", "2d", "1w" and
	// returns the equivalent seconds. Unknown units return an error so the
	// caller can reject the input explicitly rather than silently falling
	// back to the default.
	parseDurationSeconds := func(raw string) (int, error) {
		if raw == "" {
			return 0, fmt.Errorf("empty duration")
		}
		// Last char is unit; everything before is the magnitude.
		unit := raw[len(raw)-1]
		numPart := raw[:len(raw)-1]
		n, err := strconv.Atoi(numPart)
		if err != nil || n <= 0 {
			return 0, fmt.Errorf("invalid duration %q (expected <n><unit>, e.g. 1h, 30m, 2d, 1w)", raw)
		}
		switch unit {
		case 'm':
			return n * 60, nil
		case 'h':
			return n * 3600, nil
		case 'd':
			return n * 86400, nil
		case 'w':
			return n * 7 * 86400, nil
		default:
			return 0, fmt.Errorf("unknown duration unit %q (use m, h, d, or w)", string(unit))
		}
	}

	seconds := 86400 // default: 1 day
	for i := 2; i < len(args); i++ {
		switch args[i] {
		case "--days":
			if i+1 >= len(args) {
				usererror.Error("snooze: --days requires a value")
				return 1
			}
			i++
			d, err := strconv.Atoi(args[i])
			if err != nil {
				usererror.Error("snooze: invalid --days %q (must be a positive integer)", args[i])
				return 1
			}
			if d <= 0 {
				usererror.Error("snooze: --days must be >= 1 (got %d)", d)
				return 1
			}
			// Cap snooze at 1 year so a typo can't quietly turn a
			// memory into a "never accessed / never decays" row
			// without going through `mpm promote`. Use promote for
			// permanent durability.
			if d > 365 {
				usererror.Error("snooze: --days capped at 365 (got %d) — use `mpm promote` for permanent durability", d)
				return 1
			}
			seconds = d * 86400
		case "--duration":
			if i+1 >= len(args) {
				usererror.Error("snooze: --duration requires a value (e.g. 1h, 30m, 2d, 1w)")
				return 1
			}
			i++
			sec, err := parseDurationSeconds(args[i])
			if err != nil {
				usererror.Error("snooze: %v", err)
				return 1
			}
			// Cap at 1 year (31,536,000 seconds) so an accidental
			// "1y" or "52w" can't quietly turn a memory into a
			// "never decays" row without going through `mpm promote`.
			if sec > 31536000 {
				usererror.Error("snooze: --duration capped at 1 year (got %s) — use `mpm promote` for permanent durability", args[i])
				return 1
			}
			seconds = sec
		case "--until":
			if i+1 >= len(args) {
				usererror.Error("snooze: --until requires a value (RFC3339 timestamp, e.g. 2026-09-12T09:00:00Z)")
				return 1
			}
			i++
			t, err := time.Parse(time.RFC3339, args[i])
			if err != nil {
				usererror.Error("snooze: invalid --until %q (must be RFC3339, e.g. 2026-09-12T09:00:00Z or 2026-09-12T11:00:00+02:00): %v", args[i], err)
				return 1
			}
			now := time.Now()
			delta := t.Sub(now)
			// Past or present timestamps are a user error: snoozing
			// for <= 0 seconds is a no-op that could quietly look
			// like "it ran successfully" while doing nothing. Reject
			// explicitly so the operator notices and uses the
			// right form for their intent.
			if delta <= 0 {
				usererror.Error("snooze: --until %s is in the past (now=%s); pick a future timestamp", args[i], now.UTC().Format(time.RFC3339))
				return 1
			}
			sec := int(delta.Seconds())
			// Round up to the next whole second so a `--until
			// 2026-09-12T09:00:00.5Z` (mid-second) rounds to 1s
			// rather than truncating to 0s and falling into the
			// past-timestamp branch above.
			if delta > time.Duration(sec)*time.Second {
				sec++
			}
			if sec > 31536000 {
				usererror.Error("snooze: --until %s is more than 1 year out (delta=%s) — use `mpm promote` for permanent durability", args[i], delta)
				return 1
			}
			seconds = sec
		default:
			usererror.Error("snooze: unknown flag %q (use --days N, --duration <n><unit>, or --until <RFC3339>)", args[i])
			return 1
		}
	}

	dm := getDB()
	if dm == nil {
		return 1
	}

	// Bump weight by 1 (cap at 9 to prevent LTM promotion), refresh timestamp.
	// last_accessed_at is advanced by the requested duration (in seconds);
	// using seconds rather than days lets --duration 1h add 3600 seconds
	// (the F-E1 fix).
	res, err := dm.SQLDB().Exec(`
		UPDATE memories
		SET weight = MIN(weight + 1, 9),
		    last_accessed_at = CAST(strftime('%s','now', '+' || ? || ' seconds') AS INTEGER), runtime_seconds_since_access = 0, runtime_last_accrued_at = CAST(strftime('%s','now') AS INTEGER)
		WHERE id = ? AND deleted_at IS NULL
	`, seconds, id)
	if err != nil {
		usererror.Error("%v", err)
		return 1
	}
	if affected, aerr := res.RowsAffected(); aerr != nil {
		usererror.Error("%v", aerr)
		return 1
	} else if affected == 0 {
		usererror.Error("snooze failed: no live memory with id %s (not found or deleted)", id)
		return 1
	}

	fmt.Printf("Snoozed memory %s (+1 weight, +%d second(s) last_accessed)\n", id, seconds)
	return 0
}

// mpm shred <id> — Secure delete memory
func handleShredMem(args []string) int {
	if len(args) < 2 {
		usererror.Usage("mpm shred <id>")
		return 1
	}

	id := args[1]

	dm := getDB()
	if dm == nil {
		return 1
	}

	// Route through the substrate's ShredMemoryWithCascade so the CLI
	// and the MCP path share the same broad-sweep + cascade behaviour
	// (id removed from session_handoffs, lessons, topics,
	// capabilities, evidence, retrieval_metadata, confidence_history,
	// artifact_provenance, synth_runs, memory_revisions, plus
	// topic_memberships and the challenged theory cascade).
	result, err := dm.ShredMemoryWithCascade(id)
	if err != nil {
		usererror.Error("shred %s: %v", id, err)
		return 1
	}

	theoryID, _ := result["theory_purged"].(string)
	sweep, _ := result["sweep"].(map[string]int64)
	if theoryID != "" {
		fmt.Printf("⚡ Memory %s shredded. Theory %s purged.\n", id, theoryID)
	} else {
		fmt.Printf("⚡ Memory %s shredded.\n", id)
	}
	if len(sweep) > 0 {
		fmt.Printf("  sweep:\n")
		// Stable ordering so the output is reproducible across runs.
		tables := make([]string, 0, len(sweep))
		for t := range sweep {
			tables = append(tables, t)
		}
		sort.Strings(tables)
		for _, t := range tables {
			n := sweep[t]
			fmt.Printf("    - %s: %v rows\n", t, n)
		}
	}
	return 0
}

// =============================================================================
// Reference Commands
// =============================================================================

// handleRefAdd ingests a file as a reference document
func handleRefAdd(args []string) int {
	// R3: --help / -h / "help" short-circuit. See handleMemoryAdd for
	// the rationale; same defect, same fix. Without this, `mpm reference
	// add --help` would attempt to open a file named "help" and surface
	// "File not found" instead of printing reference help.
	for _, a := range args {
		if a == "-h" || a == "--help" || a == "help" {
			return printRefHelp()
		}
	}
	fs := flag.NewFlagSet("reference add", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	tag := fs.String("tag", "", "Tags for the reference")
	reason := fs.String("reason", "", "Import reason (why this is being added)")
	jsonOutput := fs.Bool("json", false, "Output JSON for tool integration")
	chunkSize := fs.Int("chunk-size", 512, "Target chunk size in tokens (default: 512, range: 64-2048)")
	// --url is recognised but not yet implemented. The alpha-4.1.2
	// auditor (D-005/W-002) observed that `mpm reference add --url`
	// silently does nothing. The surface has always been filesystem-
	// only — there's no http fetch code in internal/core/reference_*.
	// Per the alpha-4.1.2 spec, this is classified FUTURE FEATURE
	// (preserving the current architecture) and we surface an explicit
	// error so operators are not silently misled.
	urlFlag := fs.String("url", "", "DEPRECATED stub: URL ingestion is not yet supported (alpha-4.1.2 D-005). Downloads the URL into a local file first, then re-run.")
	if err := fs.Parse(args[1:]); err != nil {
		return 1
	}

	if *urlFlag != "" {
		return usererror.Error("--url is reserved for a future release. " +
			"In the meantime, download the document (curl/wget) and pass the local path to `mpm reference add <file>`. " +
			"Tracked as alpha-4.1.2 D-005/W-002 FUTURE FEATURE.")
	}

	if fs.NArg() < 1 {
		usererror.Usage("mpm reference add <file> [--tag tag1,tag2] [--reason <text>] [--chunk-size <tokens>] [--json]\n" +
			"  --reason: import reason (why this is being added; seed of the admission justification chain)\n" +
			"  --chunk-size: target chunk size in tokens (default: 512, range: 64-2048)")
		return 1
	}

	filePath := fs.Arg(0)
	if _, err := os.Stat(filePath); os.IsNotExist(err) {
		return usererror.Error("File not found: %s", filePath)
	}

	// Size guard: cap reference ingest at 100 MiB so a stray huge file
	// can't OOM the CLI. The parsed content (text extracted from PDF/
	// EPUB) is usually a fraction of the raw file size; this is a
	// defence-in-depth ceiling on the input, not on memory use.
	const maxRefBytes = 100 << 20 // 100 MiB
	if info, err := os.Stat(filePath); err == nil && info.Size() > maxRefBytes {
		return usererror.Error("reference file too large: %d bytes (max %d = 100 MiB)", info.Size(), maxRefBytes)
	}

	// Validate chunk size range (flag.Parse already applied the value)
	if *chunkSize < 64 || *chunkSize > 2048 {
		return usererror.Error("--chunk-size must be between 64 and 2048 (got %d)", *chunkSize)
	}

	dm := getDB()
	if dm == nil {
		return 1
	}

	ext := strings.ToLower(filepath.Ext(filePath))
	var content string
	var parseErr error

	switch ext {
	case ".pdf":
		content, parseErr = mpminternal.ParsePDF(filePath)
	case ".epub":
		content, parseErr = mpminternal.ParseEPUB(filePath)
	case ".html", ".xhtml":
		data, err := os.ReadFile(filePath)
		if err != nil {
			usererror.Error("Error reading file: %v", err)
			return 1
		}
		content = mpminternal.StripHTML(string(data))
	case ".txt", ".md":
		data, err := os.ReadFile(filePath)
		if err != nil {
			usererror.Error("Error reading file: %v", err)
			return 1
		}
		content = string(data)
	default:
		data, err := os.ReadFile(filePath)
		if err != nil {
			usererror.Error("Error reading file: %v", err)
			return 1
		}
		content = string(data)
	}

	if parseErr != nil {
		usererror.Error("Error parsing file: %v", parseErr)
		return 1
	}

	title := filepath.Base(filePath)
	sourceType := mpminternal.DetectSourceType(filePath)
	contentHash := mpminternal.HashContent(content)

	tags := []string{}
	if *tag != "" {
		for _, t := range strings.Split(*tag, ",") {
			t = strings.TrimSpace(t)
			if t != "" {
				tags = append(tags, t)
			}
		}
	}

	chunks, err := mpminternal.ChunkByTokens(content, *chunkSize)
	if err != nil {
		usererror.Error("failed to chunk content: %v", err)
	}

	// Look up an existing doc by source path so re-ingest reuses the
	// doc id; the chunk_hash diff in AddReference then runs against
	// the existing chunk rows. Without this every CLI ingest would
	// create a fresh doc (different id) and the diff would never
	// fire. Short-circuit when the file is byte-identical to last time.
	existing, err := mpminternal.FindReferenceBySourcePath(dm.SQLDB(), filePath)
	if err != nil {
		usererror.Error("Error looking up existing reference: %v", err)
		return 1
	}
	if existing != nil && existing.ContentHash == contentHash {
		if *jsonOutput {
			data, _ := json.Marshal(map[string]interface{}{
				"success":      true,
				"id":           existing.ID,
				"title":        existing.Title,
				"total_chunks": existing.TotalChunks,
				"unchanged":    true,
			})
			fmt.Println(string(data))
		} else {
			fmt.Printf("Reference unchanged: %s (id=%s)\n", existing.Title, existing.ID)
		}
		return 0
	}

	now := strconv.FormatInt(time.Now().Unix(), 10)
	docID := mpminternal.GenerateID()
	if existing != nil {
		docID = existing.ID
	}

	refChunks := make([]mpminternal.ReferenceChunk, len(chunks))
	for i, c := range chunks {
		refChunks[i] = mpminternal.ReferenceChunk{
			ID:         mpminternal.ComputeChunkID(docID, c.Index, contentHash), // stable across re-ingest
			DocID:      docID,
			ChunkIndex: c.Index,
			Section:    c.Section,
			Content:    c.Content,
			SourcePath: filePath,
		}
	}

	doc := &mpminternal.ReferenceDoc{
		ID:           docID,
		Title:        title,
		SourcePath:   filePath,
		SourceType:   sourceType,
		Tags:         tags,
		ImportReason: *reason,
		Content:      content,
		ContentHash:  contentHash,
		TotalChunks:  len(chunks),
		LastIndexed:  now,
		Created:      now,
	}

	err = dm.AddReference(doc, refChunks)
	if err != nil {
		usererror.Error("Error saving reference: %v", err)
	}

	// Embed chunks in a separate phase after the chunk-insert tx
	// commits. Embedding failures are best-effort: chunk rows are
	// already durable, and embedding can be retried via a separate
	// pass. We surface the count so the operator sees what was
	// embedded without failing the ingest.
	embedded, _, _ := dm.EmbedReferenceChunks(context.Background(), doc.ID)

	if *jsonOutput {
		tagsStr := strings.Join(tags, ",")
		data, _ := json.Marshal(map[string]interface{}{
			"success":       true,
			"id":            doc.ID,
			"title":         doc.Title,
			"total_chunks":  doc.TotalChunks,
			"embedded":      embedded,
			"tags":          tagsStr,
			"import_reason": doc.ImportReason,
		})
		fmt.Println(string(data))
	} else {
		if doc.ImportReason != "" {
			fmt.Printf("Added reference: %s (%d chunks, %d embedded)\n  reason: %s\n", title, len(chunks), embedded, doc.ImportReason)
		} else {
			fmt.Printf("Added reference: %s (%d chunks, %d embedded)\n", title, len(chunks), embedded)
		}
	}
	return 0
}

// handleRefList lists all reference documents
func handleRefList(args []string) int {
	fs := flag.NewFlagSet("reference ls", flag.ContinueOnError)
	jsonOutput := fs.Bool("json", false, "Output JSON for tool integration")
	if err := fs.Parse(args[1:]); err != nil {
		return 1
	}
	*jsonOutput, _ = ExtractJSONFlag(args[1:])

	dm := getDB()
	if dm == nil {
		return 1
	}

	refs, err := dm.ListReferences(50, 0)
	if err != nil {
		usererror.Error("%v", err)
		return 1
	}

	if len(refs) == 0 {
		if *jsonOutput {
			fmt.Println(`{"references": [], "message": "No references stored"}`)
		} else {
			fmt.Println("No references stored")
		}
		return 0
	}

	if *jsonOutput {
		type refEntry struct {
			ID           string `json:"id"`
			Title        string `json:"title"`
			TotalChunks  int    `json:"total_chunks"`
			Tags         string `json:"tags"`
			ImportReason string `json:"import_reason"`
			CreatedAt    string `json:"created_at"`
			Freshness    string `json:"freshness"`
		}
		result := make([]refEntry, 0, len(refs))
		for _, r := range refs {
			chunks := 0
			switch c := r["total_chunks"].(type) {
			case int64:
				chunks = int(c)
			case int:
				chunks = c
			case int32:
				chunks = int(c)
			}
			created := ""
			if c, ok := r["created_at"].(string); ok {
				created = c
			}
			tags := ""
			if t, ok := r["tags"].(string); ok {
				tags = t
			}
			reason := ""
			if rr, ok := r["import_reason"].(string); ok {
				reason = rr
			}
			freshness, _ := r["freshness"].(string)
			refID, _ := r["id"].(string)
			refTitle, _ := r["title"].(string)
			result = append(result, refEntry{
				ID:           refID,
				Title:        refTitle,
				TotalChunks:  chunks,
				Tags:         tags,
				ImportReason: reason,
				CreatedAt:    created,
				Freshness:    freshness,
			})
		}
		data, _ := json.Marshal(map[string]interface{}{"references": result})
		fmt.Println(string(data))
		return 0
	}

	fmt.Println("\nReferences:")
	for _, r := range refs {
		tags := ""
		if t, ok := r["tags"].(string); ok && t != "" {
			tags = fmt.Sprintf(" [%s]", t)
		}
		chunks := 0
		switch c := r["total_chunks"].(type) {
		case int64:
			chunks = int(c)
		case int:
			chunks = c
		case int32:
			chunks = int(c)
		}
		created := ""
		if c, ok := r["created_at"].(string); ok {
			created = c
		}
		refID, _ := r["id"].(string)
		refTitle, _ := r["title"].(string)
		freshness, _ := r["freshness"].(string)
		// F-G1/F-G2: surface the freshness classifier so operators
		// can spot stale/historical refs at a glance. Without this,
		// a backdated 5-year-old ref looks identical to a fresh one
		// in the listing.
		freshnessBadge := ""
		if freshness != "" && freshness != "current" {
			freshnessBadge = fmt.Sprintf(" {%s}", freshness)
		}
		reason := ""
		if rr, ok := r["import_reason"].(string); ok && rr != "" {
			r := rr
			if len(r) > 100 {
				r = r[:97] + "..."
			}
			reason = fmt.Sprintf("\n       reason: %s", r)
		}
		fmt.Printf("  %s | %s | %d chunks |%s%s\n",
			refID[:min(len(refID), 16)],
			refTitle,
			chunks,
			tags,
			freshnessBadge)
		if created != "" {
			fmt.Printf("       created: %s%s\n", created, reason)
		} else if reason != "" {
			fmt.Printf("       %s\n", reason[1:]) // strip leading newline
		}
	}
	return 0
}

// handleRefShow shows a reference document with its chunks
func handleRefShow(args []string) int {
	if len(args) < 2 {
		usererror.Usage("mpm reference show <id> [--json]")
		return 1
	}

	id := args[1]
	dm := getDB()
	if dm == nil {
		return 1
	}

	// Pre-scan for --json
	jsonOutput, _ := ExtractJSONFlag(args[2:])

	ref, err := dm.GetReference(id)
	if err != nil {
		if jsonOutput {
			data, _ := json.Marshal(map[string]interface{}{"success": false, "error": "Reference not found: " + id})
			fmt.Println(string(data))
		} else {
			usererror.Error("Reference not found: %s", id)
		}
		return 1
	}

	refShowID, _ := ref["id"].(string)
	refShowTitle, _ := ref["title"].(string)
	tags := ""
	if t, ok := ref["tags"].(string); ok {
		tags = t
	}
	created := ""
	if c, ok := ref["created_at"].(string); ok {
		created = c
	}

	chunks, ok := ref["chunks"].([]map[string]interface{})
	if jsonOutput {
		type chunkEntry struct {
			Index   int    `json:"index"`
			Content string `json:"content"`
		}
		chunkResult := make([]chunkEntry, 0)
		if ok {
			for _, c := range chunks {
				idxVal := c["chunk_index"]
				idx := 0
				switch v := idxVal.(type) {
				case int64:
					idx = int(v)
				case int:
					idx = v
				case int32:
					idx = int(v)
				case float64:
					idx = int(v)
				}
				content, _ := c["content"].(string)
				chunkResult = append(chunkResult, chunkEntry{Index: idx, Content: content})
			}
		}
		// F-G1/F-G2: include freshness in the JSON envelope so
		// `mpm reference show <id> --json` exposes the staleness
		// signal for downstream consumers (and is the canonical
		// audit record).
		freshness, _ := ref["freshness"].(string)
		data, _ := json.Marshal(map[string]interface{}{
			"id":         refShowID,
			"title":      refShowTitle,
			"tags":       tags,
			"created_at": created,
			"freshness":  freshness,
			"chunks":     chunkResult,
		})
		fmt.Println(string(data))
		return 0
	}

	fmt.Printf("\n[%s] %s\n", refShowID, refShowTitle)
	// F-G1/F-G2: surface freshness as a header line. Stale/historical
	// refs are the dangerous case (operator reads them and trusts the
	// content as current authority); make the signal loud.
	if f, ok := ref["freshness"].(string); ok && f != "" && f != "current" {
		fmt.Printf("⚠️  Freshness: %s\n", f)
	}
	if st, ok := ref["source_type"].(string); ok && st != "" {
		fmt.Printf("Type: %s\n", st)
	}
	if fp, ok := ref["file_path"].(string); ok && fp != "" {
		fmt.Printf("Path: %s\n", fp)
	}
	if tc, ok := ref["total_chunks"].(int64); ok {
		fmt.Printf("Chunks: %d\n", int(tc))
	} else if tc, ok := ref["total_chunks"].(int); ok {
		fmt.Printf("Chunks: %d\n", tc)
	}

	if ok && len(chunks) > 0 {
		fmt.Printf("\n--- Content (%d chunks) ---\n", len(chunks))
		for _, c := range chunks {
			if sec, ok := c["section"].(string); ok && sec != "" {
				fmt.Printf("\n## %s\n\n", sec)
			}
			content := c["content"].(string)
			if len(content) > 500 {
				content = content[:500] + "..."
			}
			fmt.Printf("%s\n\n", content)
		}
	} else {
		content, _ := ref["content"].(string)
		if content == "" {
			content, _ = ref["title"].(string)
		}
		fmt.Printf("\n%s\n", content)
	}
	return 0
}

// handleRefSearch searches reference chunks
func handleRefSearch(args []string) int {
	if len(args) < 2 {
		usererror.Usage("mpm reference search <query> [--json]")
		return 1
	}

	jsonOutput, cleanArgs := ExtractJSONFlag(args[1:])
	query := strings.Join(cleanArgs, " ")
	dm := getDB()
	if dm == nil {
		return 1
	}

	chunks, err := dm.SearchReferenceChunks(query, 20)
	if err != nil {
		usererror.Error("%v", err)
		return 1
	}

	if len(chunks) == 0 {
		if jsonOutput {
			data, _ := json.Marshal(map[string]interface{}{"query": query, "results": []interface{}{}, "message": "No results found"})
			fmt.Println(string(data))
		} else {
			fmt.Printf("No results found for: %s\n", query)
		}
		return 0
	}

	if jsonOutput {
		type chunkResult struct {
			DocID      string  `json:"doc_id"`
			DocTitle   string  `json:"doc_title"`
			ChunkIndex int     `json:"chunk_index"`
			Content    string  `json:"content"`
			Score      float64 `json:"score"`
		}
		results := make([]chunkResult, 0, len(chunks))
		for _, c := range chunks {
			docTitle := ""
			if dt, ok := c["doc_title"].(string); ok {
				docTitle = dt
			}
			docID := ""
			if did, ok := c["id"].(string); ok {
				docID = did
			}
			idxVal := c["chunk_index"]
			idx := 0
			switch v := idxVal.(type) {
			case int64:
				idx = int(v)
			case int:
				idx = v
			case int32:
				idx = int(v)
			case float64:
				idx = int(v)
			}
			content, _ := c["content"].(string)
			score := 0.0
			if s, ok := c["score"].(float64); ok {
				score = s
			}
			results = append(results, chunkResult{
				DocID:      docID,
				DocTitle:   docTitle,
				ChunkIndex: idx,
				Content:    content,
				Score:      score,
			})
		}
		data, _ := json.Marshal(map[string]interface{}{"query": query, "results": results})
		fmt.Println(string(data))
		return 0
	}

	fmt.Printf("\nFound %d matching chunks:\n\n", len(chunks))
	for _, c := range chunks {
		docTitle := ""
		if dt, ok := c["doc_title"].(string); ok {
			docTitle = dt
		}
		content, _ := c["content"].(string)
		if len(content) > 200 {
			content = content[:200] + "..."
		}
		idxVal := c["chunk_index"]
		idx := 0
		switch v := idxVal.(type) {
		case int64:
			idx = int(v)
		case int:
			idx = v
		case int32:
			idx = int(v)
		case float64:
			idx = int(v)
		}
		idStr, _ := c["id"].(string)
		fmt.Printf("[%s chunk %d] %s\n%s\n\n", docTitle, idx, idStr[:min(len(idStr), 8)], content)
	}
	return 0
}

// handleRefShred deletes a reference document
func handleRefShred(args []string) int {
	if len(args) < 2 {
		usererror.Usage("mpm reference shred <id>")
		return 1
	}

	id := args[1]
	dm := getDB()
	if dm == nil {
		return 1
	}

	// `err` is reused below; declare explicitly because the singleton
	// lookup (getDB()) doesn't introduce one.
	var err error
	err = dm.DeleteReference(id)
	if err != nil {
		usererror.Error("Error deleting reference: %v", err)
	}

	fmt.Printf("Deleted reference: %s\n", id)
	return 0
}

// handleRefInteractions prints recent reference retrieval events. The
// audit trail for the admission function (Phase 3) — without it, the
// reference-to-memory path is not observable.
func handleRefInteractions(args []string) int {
	fs := flag.NewFlagSet("reference interactions", flag.ContinueOnError)
	jsonOutput := fs.Bool("json", false, "Output JSON for tool integration")
	limit := fs.Int("limit", 30, "Max interactions to show")
	docID := fs.String("doc", "", "Filter to a single reference doc id")
	if err := fs.Parse(args[1:]); err != nil {
		return 1
	}
	*jsonOutput, _ = ExtractJSONFlag(args[1:])

	dm := getDB()
	if dm == nil {
		return 1
	}

	// `err` is reused below; declare explicitly because the singleton
	// lookup (getDB()) doesn't introduce one.
	var err error

	var rows []map[string]interface{}
	if *docID != "" {
		rows, err = dm.GetInteractionsForDoc(*docID, *limit)
	} else {
		rows, err = dm.GetRecentInteractions(*limit)
	}
	if err != nil {
		usererror.Error("%v", err)
	}

	if *jsonOutput {
		data, _ := json.Marshal(map[string]interface{}{"interactions": rows})
		fmt.Println(string(data))
		return 0
	}
	fmt.Printf("\nRecent reference interactions (%d):\n", len(rows))
	for _, r := range rows {
		rank := 0
		if v, ok := r["rank"].(int); ok {
			rank = v
		}
		score := 0.0
		if v, ok := r["score"].(float64); ok {
			score = v
		}
		created := ""
		if v, ok := r["created_at"].(string); ok {
			created = v
			if len(created) > 19 {
				created = created[:19]
			}
		}
		title := ""
		if v, ok := r["doc_title"].(string); ok {
			title = v
			if len(title) > 40 {
				title = title[:37] + "..."
			}
		}
		q, _ := r["query"].(string)
		kind, _ := r["search_kind"].(string)
		docIDStr, _ := r["doc_id"].(string)
		if docIDStr == "" {
			if v, ok := r["id"].(string); ok {
				docIDStr = v
			}
		}
		fmt.Printf("  %s | %-12s | rank=%-2d score=%6.2f | %s\n", created, kind, rank, score, title)
		fmt.Printf("    query: %q\n", q)
		if docIDStr != "" {
			fmt.Printf("    doc:   %s\n", docIDStr)
		}
	}
	return 0
}

// handleRefUsed prints the most-retrieved references, ranked by interaction
// count. Helps identify which references the system is leaning on, and
// which are dormant.
func handleRefUsed(args []string) int {
	fs := flag.NewFlagSet("reference used", flag.ContinueOnError)
	jsonOutput := fs.Bool("json", false, "Output JSON for tool integration")
	limit := fs.Int("limit", 20, "Max references to show")
	if err := fs.Parse(args[1:]); err != nil {
		return 1
	}
	*jsonOutput, _ = ExtractJSONFlag(args[1:])

	dm := getDB()
	if dm == nil {
		return 1
	}

	rows, err := dm.GetMostUsedReferences(*limit)
	if err != nil {
		usererror.Error("%v", err)
	}

	if *jsonOutput {
		data, _ := json.Marshal(map[string]interface{}{"used": rows})
		fmt.Println(string(data))
		return 0
	}
	fmt.Printf("\nMost-used references (by interaction count):\n")
	for _, r := range rows {
		hits := 0
		if v, ok := r["hits"].(int64); ok {
			hits = int(v)
		} else if v, ok := r["hits"].(int); ok {
			hits = v
		}
		dq := 0
		if v, ok := r["distinct_queries"].(int64); ok {
			dq = int(v)
		} else if v, ok := r["distinct_queries"].(int); ok {
			dq = v
		}
		title := ""
		if v, ok := r["title"].(string); ok {
			title = v
			if len(title) > 50 {
				title = title[:47] + "..."
			}
		}
		fmt.Printf("  %4d hits (%2d queries) | %s\n", hits, dq, title)
	}
	return 0
}

// handleRefAdmit runs the admission function: finds chunks that have been
// retrieved 3+ times across 2+ distinct queries, sends each to the LLM
// for evaluation, and writes admitted memories (or rejected audit rows).
//
// Per decision 9aa0ee2c6de8492a: LLM-based, autonomous, single-model per
// call. The admitting model produces both the admit/reject and the chain.
// v is not in the loop; the audit trail (admission_log + memory metadata)
// is the surface v reviews when v chooses to.
func handleRefAdmit(args []string) int {
	fs := flag.NewFlagSet("reference admit", flag.ContinueOnError)
	jsonOutput := fs.Bool("json", false, "Output JSON for tool integration")
	limit := fs.Int("limit", 5, "Max candidates to evaluate per run")
	dryRun := fs.Bool("dry-run", false, "Find candidates but do not call the LLM or write anything")
	if err := fs.Parse(args[1:]); err != nil {
		return 1
	}
	*jsonOutput, _ = ExtractJSONFlag(args[1:])

	// enrichCandidate takes *DatabaseManager (concrete), not the CoreDB
	// interface. The singleton is always a *DatabaseManager.
	dm := getDBConcrete()
	if dm == nil {
		return 1
	}

	candidates, err := dm.FindAdmissionCandidates(*limit)
	if err != nil {
		usererror.Error("Error finding candidates: %v", err)
		return 1
	}
	if len(candidates) == 0 {
		if *jsonOutput {
			fmt.Println(`{"admitted":0,"rejected":0,"candidates":0}`)
		} else {
			fmt.Println("No admission candidates (need 3+ hits across 2+ distinct queries per chunk).")
		}
		return 0
	}

	if *dryRun {
		if *jsonOutput {
			data, _ := json.Marshal(map[string]interface{}{
				"dry_run":    true,
				"candidates": len(candidates),
				"items":      candidates,
			})
			fmt.Println(string(data))
		} else {
			fmt.Printf("Found %d admission candidates (dry-run, not evaluated):\n", len(candidates))
			for i, c := range candidates {
				title := c.DocTitle
				if len(title) > 50 {
					title = title[:47] + "..."
				}
				fmt.Printf("  %d. %s (chunk %s) — %d hits, %d distinct queries\n",
					i+1, title, c.ChunkID[:min(len(c.ChunkID), 12)], c.HitCount, c.DistinctQueries)
			}
		}
		return 0
	}

	client := synth.NewSynthClient()
	if client.APIKey == "" {
		usererror.Error("no API key configured (set api_key in mpm_config.json synth block or MINIMAX_API_KEY env var)")
	}

	// Read active context for memory provenance.
	active := loadActiveForAdmission()
	results := []map[string]interface{}{}
	admitted := 0
	rejected := 0
	errs := 0
	for _, candidate := range candidates {
		// Enrich candidate with active context.
		activeProject, nearestTheories := enrichCandidate(dm, active)
		candidate.ActiveProject = activeProject
		candidate.NearestTheories = nearestTheories

		ctx, cancel := context.WithTimeout(context.Background(), client.Timeout)
		result, err := mpminternal.EvaluateCandidate(ctx, client, candidate)
		cancel()
		if err != nil {
			errs++
			if *jsonOutput {
				results = append(results, map[string]interface{}{
					"chunk_id": candidate.ChunkID,
					"doc_id":   candidate.DocID,
					"error":    err.Error(),
				})
			} else {
				usererror.Warn("err %s: %v", candidate.ChunkID[:min(len(candidate.ChunkID), 12)], err)
			}
			continue
		}

		// Record the outcome to admission_log regardless of admit/reject.
		if err := dm.RecordAdmissionOutcome(candidate, result, client.Model); err != nil {
			slog.Warn("failed to record admission outcome", "chunk_id_prefix", candidate.ChunkID[:min(len(candidate.ChunkID), 12)], "error", err.Error())
		}

		if !result.Admit {
			rejected++
			if *jsonOutput {
				results = append(results, map[string]interface{}{
					"chunk_id": candidate.ChunkID,
					"doc_id":   candidate.DocID,
					"admit":    false,
					"reason":   result.Reason,
				})
			} else {
				title := candidate.DocTitle
				if len(title) > 50 {
					title = title[:47] + "..."
				}
				fmt.Printf("  reject: %s — %s\n", title, result.Reason)
			}
			continue
		}

		// Admit: write memory via SaveMemoryWithContext. The ActiveContext
		// carries the admission model name so it lands in metadata.provenance.model
		// and the memory_source_evidence_ai trigger attributes the memory to
		// the admission model.
		admissionAC := mpminternal.ActiveContext{
			Mode:    active.Mode,
			Persona: active.Persona,
			Model:   client.Model,
			Agent:   "mpm_admission",
		}
		// Merge LLM tags with provenance tags.
		tags := append([]string{"admission", "auto"}, result.Tags...)
		resp, _, err := dm.SaveMemoryWithContext(
			result.Content,
			"memories",
			tags,
			result.Confidence,
			"",
			admissionAC,
		)
		if err != nil {
			errs++
			usererror.Warn("err memory for %s: %v", candidate.ChunkID[:min(len(candidate.ChunkID), 12)], err)
			continue
		}
		admitted++
		if *jsonOutput {
			results = append(results, map[string]interface{}{
				"chunk_id":      candidate.ChunkID,
				"doc_id":        candidate.DocID,
				"admit":         true,
				"memory_id":     resp["id"],
				"content":       result.Content,
				"confidence":    result.Confidence,
				"tags":          result.Tags,
				"justification": result.Justification,
			})
		} else {
			title := candidate.DocTitle
			if len(title) > 50 {
				title = title[:47] + "..."
			}
			fmt.Printf("  admit: [%s] %.2f | %s\n", resp["id"], result.Confidence, title)
			fmt.Printf("    content: %s\n", truncate(result.Content, 200))
		}
	}

	if *jsonOutput {
		data, _ := json.Marshal(map[string]interface{}{
			"admitted":   admitted,
			"rejected":   rejected,
			"errors":     errs,
			"candidates": len(candidates),
			"results":    results,
		})
		fmt.Println(string(data))
	} else {
		fmt.Printf("\nAdmission run complete: %d admitted, %d rejected, %d errors (of %d candidates)\n",
			admitted, rejected, errs, len(candidates))
	}
	return 0
}

// activeAdmission holds the active mode/persona for memory provenance.
type activeAdmission struct {
	Mode    string
	Persona string
}

func loadActiveForAdmission() activeAdmission {
	out := activeAdmission{}
	st, err := mpminternal.LoadActiveJSON()
	if err != nil {
		return out
	}
	if len(st.Modes) > 0 {
		out.Mode = st.Modes[0]
	}
	out.Persona = st.Persona
	return out
}

// enrichCandidate pulls the active project and nearest existing theories
// for the LLM to evaluate connection. Cheap queries: active project is
// read from active.json, nearest theories is the top-3 highest-weight
// memories with at least one tag overlap with the candidate.
func enrichCandidate(dm *mpminternal.DatabaseManager, active activeAdmission) (string, []string) {
	// Active project: read from active.json's first mode. If the mode is
	// "programming" or "debugging", we don't have a project name in MPM
	// today; leave it blank and let the LLM evaluate without it.
	activeProject := ""
	switch active.Mode {
	case "programming", "debugging", "architect":
		activeProject = "MPM development (mode: " + active.Mode + ")"
	case "research", "write":
		activeProject = "research/writing session"
	}

	// Nearest theories: top 3 memories with weight > 5.0. Cheap proxy
	// for "things v cares about" without running another LLM call.
	theories, _ := dm.SQLDB().Query(`
		SELECT id, substr(content, 1, 80) FROM memories
		WHERE deleted_at IS NULL AND weight > 5.0
		ORDER BY weight DESC LIMIT 3
	`)
	defer theories.Close()
	out := []string{}
	for theories.Next() {
		var id, content string
		if err := theories.Scan(&id, &content); err == nil {
			out = append(out, content)
		}
	}
	return activeProject, out
}

// handleRef routes reference subcommands
func handleRef(args []string) int {
	if len(args) < 1 {
		_ = printRefHelp()
		return 1
	}

	sub := args[0]
	switch sub {
	case "add":
		return handleRefAdd(args)
	case "ls", "list":
		return handleRefList(args)
	case "show", "get":
		return handleRefShow(args)
	case "search":
		return handleRefSearch(args)
	case "shred", "rm":
		return handleRefShred(args)
	case "interactions":
		return handleRefInteractions(args)
	case "used":
		return handleRefUsed(args)
	case "admit":
		return handleRefAdmit(args)
	default:
		_ = printRefHelp()
		return 1
	}
}

func printRefHelp() int {
	fmt.Println(`mpm reference - Reference library
Usage:
  mpm reference add <file> [--tag tags]    Ingest a document
  mpm reference ls                          List all references
  mpm reference show <id>                   Show reference with chunks
  mpm reference search <query>              Search reference content
  mpm reference used                        Show most-retrieved references
  mpm reference interactions                Show recent retrieval events
  mpm reference admit [--limit N] [--dry-run]   Run admission function on candidates
  mpm reference shred <id>                   Delete a reference

Supported formats: .txt, .md, .html, .epub, .pdf`)
	return 0
}

// Context Switcher — Active State
//
// active.json read/write was duplicated here in main package AND in
// internal/xitl.go for years, with subtly different shapes (this one
// defaulted to Persona="default" / Modes=["default"] when missing; the
// internal one defaulted to zero values). 2026-06-26 consolidation
// moved the canonical owner to internal/active_state.go (internal
// package) so both surfaces route through one path. The main-package
// loadActiveJSON/saveActiveJSON/activeJSONPath were deleted; callers
// below now use mpminternal.LoadActiveJSON / mpminternal.SaveActiveJSON.

func getModeFiles() []string {
	dir := filepath.Join(config.GetMPMDir(), "mode")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasSuffix(name, ".md") {
			names = append(names, strings.TrimSuffix(name, ".md"))
		}
	}
	return names
}

func getPersonaFiles() []string {
	dir := filepath.Join(config.GetMPMDir(), "persona")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasSuffix(name, ".md") {
			names = append(names, strings.TrimSuffix(name, ".md"))
		}
	}
	return names
}

// GetSystemPrompt reads the active persona and mode .md files and concatenates
// their frontmatter directives into a single system prompt string.
// If the active persona is "ephemeral", it fetches the JIT persona blob from
// system_config and formats it as YAML frontmatter in place of a file read.
func GetSystemPrompt() string {
	active, err := mpminternal.LoadActiveJSON()
	if err != nil {
		return ""
	}

	var parts []string

	// Ephemeral persona intercept: fetch from system_config
	if active.Persona == "ephemeral" {
		if dm := getDBConcrete(); dm != nil {
			if ep, epErr := mpminternal.GetEphemeralPersona(dm); epErr == nil {
				if fm, fmErr := mpminternal.FormatEphemeralPersonaAsFrontmatter(ep); fmErr == nil {
					parts = append(parts, "## Active Persona (JIT)\n\n"+fm)
				}
			}
		}
	} else {
		personaPath := filepath.Join(config.GetMPMDir(), "persona", active.Persona+".md")
		if data, err := os.ReadFile(personaPath); err == nil {
			if content := extractFrontmatterDirective(string(data)); content != "" {
				parts = append(parts, content)
			}
		}
	}

	for _, mode := range active.Modes {
		modePath := filepath.Join(config.GetMPMDir(), "mode", mode+".md")
		if data, err := os.ReadFile(modePath); err == nil {
			if content := extractFrontmatterDirective(string(data)); content != "" {
				parts = append(parts, content)
			}
			if limit, threshold := extractRetrievalParams(string(data)); limit > 0 {
				parts = append(parts, fmt.Sprintf("retrieval_limit=%d, retrieval_threshold=%.1f", limit, threshold))
			}
		}
	}

	return strings.TrimSpace(strings.Join(parts, "\n\n"))
}

// extractFrontmatterDirective reads YAML frontmatter from a .md file and returns
// the "directive" or "purpose" or "description" field, whichever is found first.
func extractFrontmatterDirective(content string) string {
	idx := strings.Index(content, "---")
	if idx == -1 {
		return ""
	}
	body := content[idx+3:]
	endIdx := strings.Index(body, "---")
	if endIdx == -1 {
		return ""
	}
	fm := body[:endIdx]

	for _, key := range []string{"directive", "purpose", "description"} {
		prefix := key + ":"
		for _, line := range strings.Split(fm, "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), prefix) {
				return strings.TrimSpace(strings.TrimPrefix(line, prefix))
			}
		}
	}
	return ""
}

// extractRetrievalParams reads retrieval_limit and retrieval_threshold from
// a mode .md file's YAML frontmatter. Returns (0, 0) if not found.
func extractRetrievalParams(content string) (int, float64) {
	idx := strings.Index(content, "---")
	if idx == -1 {
		return 0, 0
	}
	body := content[idx+3:]
	endIdx := strings.Index(body, "---")
	if endIdx == -1 {
		return 0, 0
	}
	fm := body[:endIdx]

	limit := 0
	threshold := 0.0
	for _, line := range strings.Split(fm, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "retrieval_limit:") {
			val := strings.TrimSpace(trimmed[16:])
			if n, err := strconv.Atoi(val); err == nil && n > 0 {
				limit = n
			}
		}
		if strings.HasPrefix(trimmed, "retrieval_threshold:") {
			val := strings.TrimSpace(trimmed[20:])
			if f, err := strconv.ParseFloat(val, 64); err == nil {
				threshold = f
			}
		}
	}
	return limit, threshold
}
