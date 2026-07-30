package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
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
	weight := fs.Int("weight", 1, "Initial weight (1-100)")
	ttl := fs.String("ttl", "", "Time to live (e.g., 7d, 24h)")
	jsonOutput := fs.Bool("json", false, "Output JSON for tool integration")
	fs.Usage = func() {
		fmt.Println("Usage: mpm add [flags] <content>")
		fmt.Println("Flags:")
		fmt.Println("  --tag <a,b,c>     Comma-separated tags")
		fmt.Println("  --weight <1-100> Initial weight (default 1)")
		fmt.Println("  --collection <c> Collection name (default memories)")
		fmt.Println("  --ttl <duration> Time to live (e.g. 7d, 24h)")
		fmt.Println("Example: mpm add --tag personal,important 'Remember to call mom'")
	}

	// Parse with standard flag parser, which handles arbitrary argument ordering.
	// After parsing, fs.Args() contains the positional arguments (the content).
	if err := fs.Parse(args[1:]); err != nil {
		// On error (e.g., unknown flag), fs.HasError() is true; error already printed
		return 1
	}

	// Extract content: use first positional arg, or join all for multi-word content
	if fs.NArg() == 0 {
		usererror.Error("content required")
	}
	content := strings.Join(fs.Args(), " ")

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

	// Auto-embed: try real embeddings, fall back to hash if provider unavailable
	var embedding []float32
	cfg := mpminternal.DefaultEmbeddingConfig()
	if cfg.Provider.Name() != "null" {
		if vec, err := cfg.Provider.Embed(content); err == nil && len(vec) > 0 {
			embedding = vec
		} else {
			embedding = mpminternal.HashEmbed(content)
		}
	} else {
		embedding = mpminternal.HashEmbed(content)
	}
	isLongTerm := *weight >= 10

	id, err := dm.SaveMemory(*collection, content, *session, tags, metadata, embedding, isLongTerm, *weight)
	if err != nil {
		usererror.Error("%v", err)
	}

	// Set TTL if specified
	if *ttl != "" {
		dur, err := parseDuration(*ttl)
		if err == nil {
			t := time.Now().Add(dur)
			dm.SetMemoryTTL(id, t)
		}
	}

	if *jsonOutput {
		data, _ := json.Marshal(map[string]interface{}{
			"id":      id,
			"success": true,
		})
		fmt.Println(string(data))
	} else {
		fmt.Printf("Added memory %s to %s (weight=%d)\n", id, *collection, *weight)
	}
	return 0
}

// mpm ls — List memories
func handleLs(args []string) int {
	fs := flag.NewFlagSet("ls", flag.ContinueOnError)
	collection := fs.String("collection", "", "Filter by collection")
	tag := fs.String("tag", "", "Filter by tag")
	limit := fs.Int("limit", 20, "Maximum results")
	since := fs.String("since", "", "Since date (YYYY-MM-DD)")
	until := fs.String("until", "", "Until date (YYYY-MM-DD)")
	fs.Usage = func() {
		fmt.Println("Usage: mpm ls [options]")
		fmt.Println("\nList options:")
		fs.PrintDefaults()
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
	}

	filtered := memories
	if *tag != "" {
		filtered = filterByTag(filtered, *tag)
	}

	if len(filtered) > *limit {
		filtered = filtered[:*limit]
	}

	if len(filtered) == 0 {
		fmt.Println("No memories found")
		return 0
	}

	fmt.Printf("\n%4s  %-20s %-10s %-6s %s\n", "ID", "CREATED", "COLLECTION", "WEIGHT", "CONTENT")
	fmt.Println(strings.Repeat("-", 80))

	for i, m := range filtered {
		id, _ := m["id"].(string)
		created, _ := m["created_at"].(string)
		coll, _ := m["collection"].(string)
		w := 1
		if we, ok := m["weight"].(int64); ok {
			w = int(we)
		}
		if we, ok := m["weight"].(float64); ok {
			w = int(we)
		}
		content, _ := m["content"].(string)
		meta, _ := m["metadata"].(string)
		chip := ""
		if strings.Contains(meta, `"status":"challenged"`) {
			chip = " [CHALLENGED]"
		}
		if len(content) > 50 {
			content = content[:50] + "..."
		}

		fmt.Printf("%4d  %.20s %-10s %-6d%s %s\n", i+1, created, coll, w, chip, content)
		_ = id
	}

	fmt.Printf("\n%d memories shown\n", len(filtered))
	return 0
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
	if len(args) < 2 {
		usererror.Usage("mpm show <id>")
		return 1
	}

	id := args[1]

	dm := getDB()
	if dm == nil {
		return 1
	}

	mem, err := dm.GetMemory(id)
	if err != nil || mem == nil {
		usererror.Error("Memory not found: %s", id)
		return 1
	}

	fmt.Println("\n══════════════════════════════════════════")
	fmt.Printf("ID:           %s\n", id)
	fmt.Printf("Collection:   %s\n", mem["collection"])
	fmt.Printf("Created:      %s\n", mem["created_at"])

	if sess, ok := mem["session_id"].(string); ok {
		fmt.Printf("Session:      %s\n", sess)
	}

	w := 1
	if we, ok := mem["weight"].(int64); ok {
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

// mpm rm <id> — Soft delete memory
func handleRm(args []string) int {
	if len(args) < 2 {
		usererror.Usage("mpm rm <id>")
		return 1
	}

	id := args[1]

	dm := getDB()
	if dm == nil {
		return 1
	}

	// `err` is reused below; it was previously declared by
	// `dm, err := mpminternal.NewDatabaseManager("")`; declare explicitly
	// because the singleton lookup doesn't introduce one.
	var err error

	// Soft delete by setting deleted_at (INTEGER Unix epoch, matches expires_at)
	_, err = dm.SQLDB().Exec(`UPDATE memories SET deleted_at = strftime('%s','now') WHERE id = ?`, id)
	if err != nil {
		usererror.Error("%v", err)
	}

	fmt.Printf("Deleted memory %s\n", id)
	return 0
}

// mpm patch-memory <id> <json-patch> — Patch metadata JSON in-place (no content change, no FTS re-index)
func handlePatchMemory(args []string) int {
	if len(args) < 3 {
		usererror.Usage("mpm patch-memory <id> <json-patch>")
		return 1
	}

	id := args[1]
	patchJSON := args[2]

	// Basic JSON validity check
	if !strings.HasPrefix(strings.TrimSpace(patchJSON), "{") {
		usererror.Error("patch must be a JSON object string")
	}

	dm := getDB()
	if dm == nil {
		return 1
	}

	// `err` is reused below; it was previously declared by
	// `dm, err := mpminternal.NewDatabaseManager("")`; declare explicitly
	// because the singleton lookup doesn't introduce one.
	var err error

	err = dm.UpdateMemoryMetadata(id, patchJSON)
	if err != nil {
		usererror.Error("%v", err)
	}

	fmt.Printf("Patched metadata for memory %s\n", id)
	return 0
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
	dm.SetMemoryTTL(id, time.Time{})
	dm.ReinforceMemory(id, 9)

	// Update weight to 10 and is_long_term = 1
	dm.SQLDB().Exec(`UPDATE memories SET weight = 10, is_long_term = 1 WHERE id = ?`, id)

	fmt.Printf("Promoted memory %s to LTM (weight=10)\n", id)
	return 0
}

// mpm reinforce <id> [delta] — Increment reinforcement
// handleFeedback dispatches +<id> and -<id> shortcuts.
// Delta sign is determined by the caller: +1 for reinforce, -1 for weaken.
func handleFeedback(args []string) int {
	if len(args) < 2 {
		usererror.Error("internal: handleFeedback requires id and delta")
	}
	id := args[1]
	delta, err := strconv.Atoi(args[2])
	if err != nil || delta == 0 {
		usererror.Error("invalid delta %q", args[2])
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
			}
			fmt.Printf("⚡ Reinforced memory %s (+%d) — challenge cleared\n", id, delta)
		} else {
			if err := dm.ReinforceMemory(id, delta); err != nil {
				usererror.Error("%v", err)
			}
			fmt.Printf("⚡ Reinforced memory %s (+%d)\n", id, delta)
		}
	} else {
		// Negative feedback: weaken with hard floor at 1
		if err := dm.AdjustMemoryWeight(id, delta); err != nil {
			usererror.Error("%v", err)
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
		if d, err := strconv.Atoi(args[2]); err == nil {
			delta = d
		}
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
		if d, err := strconv.Atoi(args[2]); err == nil {
			delta = d
		}
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
	w, err := strconv.Atoi(args[2])
	if err != nil {
		usererror.Error("invalid weight '%s'", args[2])
	}
	if w < 0 || w > 100 {
		usererror.Error("weight must be 0-100")
	}

	dm := getDB()
	if dm == nil {
		return 1
	}

	_, err = dm.SQLDB().Exec(`UPDATE memories SET weight = ? WHERE id = ?`, w, id)
	if err != nil {
		usererror.Error("%v", err)
	}

	fmt.Printf("Set weight of %s to %d\n", id, w)
	return 0
}

// mpm snooze <id> [--days N] — Bump memory relevance without promoting to LTM.
// Increments weight by 1 (capped at 9 to avoid LTM promotion) and refreshes
// last_accessed_at. Never sets is_long_term or inflates weight to >= 10.
func handleSnooze(args []string) int {
	if len(args) < 2 {
		usererror.Usage("mpm snooze <id> [--days N]")
		return 1
	}
	id := args[1]
	days := 1
	for i := 2; i < len(args); i++ {
		if args[i] == "--days" && i+1 < len(args) {
			i++
			if d, err := strconv.Atoi(args[i]); err == nil && d > 0 {
				days = d
			}
		}
	}

	dm := getDB()
	if dm == nil {
		return 1
	}

	// `err` is reused below; declare explicitly because the singleton
	// lookup (getDB()) doesn't introduce one.
	var err error

	// Bump weight by 1 (cap at 9 to prevent LTM promotion), refresh timestamp
	_, err = dm.SQLDB().Exec(`
		UPDATE memories
		SET weight = MIN(weight + 1, 9),
		    last_accessed_at = datetime('now', '+' || ? || ' days')
		WHERE id = ? AND deleted_at IS NULL
	`, days, id)
	if err != nil {
		usererror.Error("%v", err)
	}

	fmt.Printf("Snoozed memory %s (+1 weight, +%d day(s) last_accessed)\n", id, days)
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

	// Fetch memory to extract challenged_theory_id before deletion
	mem, err := dm.GetMemory(id)
	var theoryID string
	if err == nil && mem != nil {
		metaStr, _ := mem["metadata"].(string)
		var meta map[string]interface{}
		if metaStr != "" {
			json.Unmarshal([]byte(metaStr), &meta)
		}
		theoryID, _ = meta["challenged_theory_id"].(string)
	}

	// Transaction: DELETE topic_memberships → DELETE theory (if exists) → DELETE memory
	tx, err := dm.SQLDB().Begin()
	if err != nil {
		usererror.Error("%v", err)
	}
	defer tx.Rollback()

	if _, err = tx.Exec(`DELETE FROM topic_memberships WHERE memory_id = ?`, id); err != nil {
		usererror.Error("%v", err)
	}

	if theoryID != "" {
		if _, err = tx.Exec(`DELETE FROM memories WHERE id = ?`, theoryID); err != nil {
			usererror.Error("%v", err)
		}
	}

	if _, err = tx.Exec(`DELETE FROM memories WHERE id = ?`, id); err != nil {
		usererror.Error("%v", err)
	}

	if err := tx.Commit(); err != nil {
		usererror.Error("%v", err)
	}

	if theoryID != "" {
		fmt.Printf("⚡ Memory %s shredded. Theory %s purged.\n", id, theoryID)
	} else {
		fmt.Printf("⚡ Memory %s shredded.\n", id)
	}
	return 0
}

// =============================================================================
// Reference Commands
// =============================================================================

// handleRefAdd ingests a file as a reference document
func handleRefAdd(args []string) int {
	if len(args) < 2 {
		usererror.Usage("mpm reference add <file> [--tag tag1,tag2] [--reason <text>] [--chunk-size <tokens>] [--json]")
// Multi-line usage help text — structured output, not a single error message
		fmt.Fprintf(os.Stderr, "  --reason: import reason (why this is being added; seed of the admission justification chain)\n")
// Multi-line usage help text — structured output, not a single error message
		fmt.Fprintf(os.Stderr, "  --chunk-size: target chunk size in tokens (default: 512, range: 64-2048)\n")
		return 1
	}

	filePath := args[1]
	if _, err := os.Stat(filePath); os.IsNotExist(err) {
		usererror.Error("File not found: %s", filePath)
	}

	fs := flag.NewFlagSet("reference add", flag.ContinueOnError)
	tag := fs.String("tag", "", "Tags for the reference")
	reason := fs.String("reason", "", "Import reason (why this is being added)")
	jsonOutput := fs.Bool("json", false, "Output JSON for tool integration")
	chunkSize := fs.Int("chunk-size", 512, "Target chunk size in tokens (default: 512, range: 64-2048)")
	if err := fs.Parse(args[2:]); err != nil {
		return 1
	}

	// Pre-scan for --json and --chunk-size since callers may place them after the path
	var parsedChunkSize int
	jsonOutputFromArgs, argsWithoutJSON := ExtractJSONFlag(args[2:])
	*jsonOutput = jsonOutputFromArgs

	for i, arg := range argsWithoutJSON {
		if arg == "--chunk-size" && i+1 < len(argsWithoutJSON) {
			fmt.Sscanf(argsWithoutJSON[i+1], "%d", &parsedChunkSize)
		}
	}

	// Validate and apply chunk size
	if parsedChunkSize != 0 {
		*chunkSize = parsedChunkSize
	}
	if *chunkSize < 64 || *chunkSize > 2048 {
		usererror.Error("--chunk-size must be between 64 and 2048 (got %d)", *chunkSize)
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
		}
		content = mpminternal.StripHTML(string(data))
	case ".txt", ".md":
		data, err := os.ReadFile(filePath)
		if err != nil {
			usererror.Error("Error reading file: %v", err)
		}
		content = string(data)
	default:
		data, err := os.ReadFile(filePath)
		if err != nil {
			usererror.Error("Unsupported file type: %s", ext)
		}
		content = string(data)
	}

	if parseErr != nil {
		usererror.Error("Error parsing file: %v", parseErr)
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

	now := time.Now().UTC().Format(time.RFC3339)
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
			refID, _ := r["id"].(string)
			refTitle, _ := r["title"].(string)
			result = append(result, refEntry{
				ID:           refID,
				Title:        refTitle,
				TotalChunks:  chunks,
				Tags:         tags,
				ImportReason: reason,
				CreatedAt:    created,
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
		reason := ""
		if rr, ok := r["import_reason"].(string); ok && rr != "" {
			r := rr
			if len(r) > 100 {
				r = r[:97] + "..."
			}
			reason = fmt.Sprintf("\n       reason: %s", r)
		}
		fmt.Printf("  %s | %s | %d chunks |%s\n",
			refID[:min(len(refID), 16)],
			refTitle,
			chunks,
			tags)
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
		data, _ := json.Marshal(map[string]interface{}{
			"id":         refShowID,
			"title":      refShowTitle,
			"tags":       tags,
			"created_at": created,
			"chunks":     chunkResult,
		})
		fmt.Println(string(data))
		return 0
	}

	fmt.Printf("\n[%s] %s\n", refShowID, refShowTitle)
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
// defaulted to Persona="default" / Modes=["standard"] when missing; the
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
