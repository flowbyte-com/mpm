package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"mpm/internal/config"

	mpminternal "mpm/internal"
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
		fmt.Fprintf(os.Stderr, "Error: content required\n")
		return 1
	}
	content := strings.Join(fs.Args(), " ")

	dm, err := mpminternal.NewDatabaseManager("")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}
	defer dm.Close()

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
		"source": "cli",
	}

	embedding := mpminternal.HashEmbed(content)
	isLongTerm := *weight >= 10

	id, err := dm.SaveMemory(*collection, content, *session, tags, metadata, embedding, isLongTerm, *weight)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
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

	dm, err := mpminternal.NewDatabaseManager("")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}
	defer dm.Close()

	memories, err := dm.GetMemoriesForExport(*collection, *since, *until)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
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
		if len(content) > 50 {
			content = content[:50] + "..."
		}

		fmt.Printf("%4d  %.20s %-10s %-6d %s\n", i+1, created, coll, w, content)
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
		fmt.Fprintf(os.Stderr, "Usage: mpm show <id>\n")
		return 1
	}

	id := args[1]

	dm, err := mpminternal.NewDatabaseManager("")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}
	defer dm.Close()

	mem, err := dm.GetMemory(id)
	if err != nil || mem == nil {
		fmt.Fprintf(os.Stderr, "Memory not found: %s\n", id)
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

	fmt.Printf("Tags:         %s\n", mem["tags"])

	fmt.Println("\n──────────────────────────────────────────")
	fmt.Printf("Content:\n%s\n", mem["content"])
	fmt.Print("══════════════════════════════════════════\n")

	return 0
}

// mpm rm <id> — Soft delete memory
func handleRm(args []string) int {
	if len(args) < 2 {
		fmt.Fprintf(os.Stderr, "Usage: mpm rm <id>\n")
		return 1
	}

	id := args[1]

	dm, err := mpminternal.NewDatabaseManager("")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}
	defer dm.Close()

	// Soft delete by setting deleted_at
	_, err = dm.SQLDB().Exec(`UPDATE memories SET deleted_at = CURRENT_TIMESTAMP WHERE id = ?`, id)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}

	fmt.Printf("Deleted memory %s\n", id)
	return 0
}

// mpm patch-memory <id> <json-patch> — Patch metadata JSON in-place (no content change, no FTS re-index)
func handlePatchMemory(args []string) int {
	if len(args) < 3 {
		fmt.Fprintf(os.Stderr, "Usage: mpm patch-memory <id> <json-patch>\n")
		return 1
	}

	id := args[1]
	patchJSON := args[2]

	// Basic JSON validity check
	if !strings.HasPrefix(strings.TrimSpace(patchJSON), "{") {
		fmt.Fprintf(os.Stderr, "Error: patch must be a JSON object string\n")
		return 1
	}

	dm, err := mpminternal.NewDatabaseManager("")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}
	defer dm.Close()

	err = dm.UpdateMemoryMetadata(id, patchJSON)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}

	fmt.Printf("Patched metadata for memory %s\n", id)
	return 0
}

// mpm promote <id> — Make memory LTM
func handlePromote(args []string) int {
	if len(args) < 2 {
		fmt.Fprintf(os.Stderr, "Usage: mpm promote <id>\n")
		return 1
	}

	id := args[1]

	dm, err := mpminternal.NewDatabaseManager("")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}
	defer dm.Close()

	// Clear TTL (make permanent) and reinforce heavily
	dm.SetMemoryTTL(id, time.Time{})
	dm.ReinforceMemory(id, 9)

	// Update weight to 10 and is_long_term = 1
	dm.SQLDB().Exec(`UPDATE memories SET weight = 10, is_long_term = 1 WHERE id = ?`, id)

	fmt.Printf("Promoted memory %s to LTM (weight=10)\n", id)
	return 0
}

// mpm reinforce <id> [delta] — Increment reinforcement
func handleReinforce(args []string) int {
	if len(args) < 2 {
		fmt.Fprintf(os.Stderr, "Usage: mpm reinforce <id> [delta]\n")
		return 1
	}

	id := args[1]
	delta := 1
	if len(args) >= 3 {
		if d, err := strconv.Atoi(args[2]); err == nil {
			delta = d
		}
	}

	dm, err := mpminternal.NewDatabaseManager("")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}
	defer dm.Close()

	err = dm.ReinforceMemory(id, delta)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}

	fmt.Printf("Reinforced memory %s (+%d)\n", id, delta)
	return 0
}

// mpm weaken <id> [delta] — Decrement reinforcement
func handleWeaken(args []string) int {
	if len(args) < 2 {
		fmt.Fprintf(os.Stderr, "Usage: mpm weaken <id> [delta]\n")
		return 1
	}

	id := args[1]
	delta := 1
	if len(args) >= 3 {
		if d, err := strconv.Atoi(args[2]); err == nil {
			delta = d
		}
	}

	dm, err := mpminternal.NewDatabaseManager("")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}
	defer dm.Close()

	err = dm.WeakenMemory(id, delta)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}

	fmt.Printf("Weakened memory %s (-%d)\n", id, delta)
	return 0
}

// mpm set-weight <id> <weight> — Set weight directly
func handleSetWeight(args []string) int {
	if len(args) < 3 {
		fmt.Fprintf(os.Stderr, "Usage: mpm set-weight <id> <weight>\n")
		return 1
	}

	id := args[1]
	w, err := strconv.Atoi(args[2])
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: invalid weight '%s'\n", args[2])
		return 1
	}
	if w < 0 || w > 100 {
		fmt.Fprintf(os.Stderr, "Error: weight must be 0-100\n")
		return 1
	}

	dm, err := mpminternal.NewDatabaseManager("")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}
	defer dm.Close()

	_, err = dm.SQLDB().Exec(`UPDATE memories SET weight = ? WHERE id = ?`, w, id)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}

	fmt.Printf("Set weight of %s to %d\n", id, w)
	return 0
}

// mpm snooze <id> [--days N] — Bump memory relevance without promoting to LTM.
// Increments weight by 1 (capped at 9 to avoid LTM promotion) and refreshes
// last_accessed_at. Never sets is_long_term or inflates weight to >= 10.
func handleSnooze(args []string) int {
	if len(args) < 2 {
		fmt.Fprintf(os.Stderr, "Usage: mpm snooze <id> [--days N]\n")
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

	dm, err := mpminternal.NewDatabaseManager("")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}
	defer dm.Close()

	// Bump weight by 1 (cap at 9 to prevent LTM promotion), refresh timestamp
	_, err = dm.SQLDB().Exec(`
		UPDATE memories
		SET weight = MIN(weight + 1, 9),
		    last_accessed_at = datetime('now', '+' || ? || ' days')
		WHERE id = ? AND deleted_at IS NULL
	`, days, id)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}

	fmt.Printf("Snoozed memory %s (+1 weight, +%d day(s) last_accessed)\n", id, days)
	return 0
}

// mpm shred <id> — Secure delete memory
func handleShredMem(args []string) int {
	if len(args) < 2 {
		fmt.Fprintf(os.Stderr, "Usage: mpm shred <id>\n")
		return 1
	}

	id := args[1]

	dm, err := mpminternal.NewDatabaseManager("")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}
	defer dm.Close()

	err = mpminternal.ShredMemory(dm.SQLDB(), id)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}

	fmt.Printf("Securely deleted memory %s\n", id)
	return 0
}

// =============================================================================
// Reference Commands
// =============================================================================

// handleRefAdd ingests a file as a reference document
func handleRefAdd(args []string) int {
	if len(args) < 2 {
		fmt.Fprintf(os.Stderr, "Usage: mpm reference add <file> [--tag tag1,tag2] [--chunk-size <tokens>] [--json]\n")
		fmt.Fprintf(os.Stderr, "  --chunk-size: target chunk size in tokens (default: 512, range: 64-2048)\n")
		return 1
	}

	filePath := args[1]
	if _, err := os.Stat(filePath); os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "File not found: %s\n", filePath)
		return 1
	}

	fs := flag.NewFlagSet("reference add", flag.ContinueOnError)
	tag := fs.String("tag", "", "Tags for the reference")
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
		fmt.Fprintf(os.Stderr, "Error: --chunk-size must be between 64 and 2048 (got %d)\n", *chunkSize)
		return 1
	}

	dm, err := mpminternal.NewDatabaseManager("")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}
	defer dm.Close()

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
			fmt.Fprintf(os.Stderr, "Error reading file: %v\n", err)
			return 1
		}
		content = mpminternal.StripHTML(string(data))
	case ".txt", ".md":
		data, err := os.ReadFile(filePath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error reading file: %v\n", err)
			return 1
		}
		content = string(data)
	default:
		data, err := os.ReadFile(filePath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Unsupported file type: %s\n", ext)
			return 1
		}
		content = string(data)
	}

	if parseErr != nil {
		fmt.Fprintf(os.Stderr, "Error parsing file: %v\n", parseErr)
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
		fmt.Fprintf(os.Stderr, "Error: failed to chunk content: %v\n", err)
		return 1
	}
	refChunks := make([]mpminternal.ReferenceChunk, len(chunks))
	for i, c := range chunks {
		refChunks[i] = mpminternal.ReferenceChunk{
			ID:         mpminternal.GenerateID(),
			DocID:      "", // will be set after doc creation
			ChunkIndex: c.Index,
			Section:    c.Section,
			Content:    c.Content,
			SourcePath: filePath,
		}
	}

	doc := &mpminternal.ReferenceDoc{
		ID:          mpminternal.GenerateID(),
		Title:       title,
		SourcePath:  filePath,
		SourceType:  sourceType,
		Tags:        tags,
		Content:     content,
		ContentHash: contentHash,
		TotalChunks: len(chunks),
		LastIndexed: time.Now().UTC().Format(time.RFC3339),
		Created:     time.Now().UTC().Format(time.RFC3339),
	}

	for i := range refChunks {
		refChunks[i].DocID = doc.ID
	}

	err = dm.AddReference(doc, refChunks)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error saving reference: %v\n", err)
		return 1
	}

	if *jsonOutput {
		tagsStr := strings.Join(tags, ",")
		data, _ := json.Marshal(map[string]interface{}{
			"success":       true,
			"id":            doc.ID,
			"title":         doc.Title,
			"total_chunks":  doc.TotalChunks,
			"tags":          tagsStr,
		})
		fmt.Println(string(data))
	} else {
		fmt.Printf("Added reference: %s (%d chunks)\n", title, len(chunks))
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

	dm, err := mpminternal.NewDatabaseManager("")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}
	defer dm.Close()

	refs, err := dm.ListReferences(50, 0)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
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
			ID          string `json:"id"`
			Title       string `json:"title"`
			TotalChunks int    `json:"total_chunks"`
			Tags        string `json:"tags"`
			CreatedAt   string `json:"created_at"`
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
			refID, _ := r["id"].(string)
			refTitle, _ := r["title"].(string)
			result = append(result, refEntry{
				ID:          refID,
				Title:       refTitle,
				TotalChunks: chunks,
				Tags:        tags,
				CreatedAt:   created,
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
		fmt.Printf("  %s | %s | %d chunks |%s\n",
			refID[:min(len(refID), 16)],
			refTitle,
			chunks,
			tags)
		if created != "" {
			fmt.Printf("       created: %s\n", created)
		}
	}
	return 0
}

// handleRefShow shows a reference document with its chunks
func handleRefShow(args []string) int {
	if len(args) < 2 {
		fmt.Fprintf(os.Stderr, "Usage: mpm reference show <id> [--json]\n")
		return 1
	}

	id := args[1]
	dm, err := mpminternal.NewDatabaseManager("")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}
	defer dm.Close()

	// Pre-scan for --json
	jsonOutput, _ := ExtractJSONFlag(args[2:])

	ref, err := dm.GetReference(id)
	if err != nil {
		if jsonOutput {
			data, _ := json.Marshal(map[string]interface{}{"success": false, "error": "Reference not found: " + id})
			fmt.Println(string(data))
		} else {
			fmt.Fprintf(os.Stderr, "Reference not found: %s\n", id)
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
		fmt.Fprintf(os.Stderr, "Usage: mpm reference search <query> [--json]\n")
		return 1
	}

	jsonOutput, cleanArgs := ExtractJSONFlag(args[1:])
	query := strings.Join(cleanArgs, " ")
	dm, err := mpminternal.NewDatabaseManager("")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}
	defer dm.Close()

	chunks, err := dm.SearchReferenceChunks(query, 20)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
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
			DocID       string  `json:"doc_id"`
			DocTitle    string  `json:"doc_title"`
			ChunkIndex  int     `json:"chunk_index"`
			Content     string  `json:"content"`
			Score       float64 `json:"score"`
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
		fmt.Fprintf(os.Stderr, "Usage: mpm reference shred <id>\n")
		return 1
	}

	id := args[1]
	dm, err := mpminternal.NewDatabaseManager("")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}
	defer dm.Close()

	err = dm.DeleteReference(id)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error deleting reference: %v\n", err)
		return 1
	}

	fmt.Printf("Deleted reference: %s\n", id)
	return 0
}

// handleRef routes reference subcommands
func handleRef(args []string) int {
	if len(args) < 2 {
		printRefHelp()
		return 1
	}

	sub := args[1]
	switch sub {
	case "add":
		return handleRefAdd(args[1:])
	case "ls", "list":
		return handleRefList(args[1:])
	case "show", "get":
		return handleRefShow(args[1:])
	case "search":
		return handleRefSearch(args[1:])
	case "shred", "rm":
		return handleRefShred(args[1:])
	default:
		printRefHelp()
		return 1
	}
}

func printRefHelp() {
	fmt.Println(`mpm reference - Reference library
Usage:
  mpm reference add <file> [--tag tags]    Ingest a document
  mpm reference ls                          List all references
  mpm reference show <id>                   Show reference with chunks
  mpm reference search <query>              Search reference content
  mpm reference shred <id>                   Delete a reference

Supported formats: .txt, .md, .html, .epub, .pdf`)
}

// =============================================================================
// Context Switcher — Active State
// =============================================================================

// ActiveState represents the current persona and mode configuration.
// Persisted as active.json in the MPM directory.
type ActiveState struct {
	Persona string   `json:"persona"`
	Modes   []string `json:"modes"`
	Updated string   `json:"updated"`
}

func activeJSONPath() string {
	return filepath.Join(config.GetMPMDir(), "active.json")
}

func loadActiveJSON() (*ActiveState, error) {
	path := activeJSONPath()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return &ActiveState{
			Persona: "default",
			Modes:   []string{"standard"},
			Updated: time.Now().UTC().Format(time.RFC3339),
		}, nil
	}
	if err != nil {
		return nil, err
	}
	var state ActiveState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, err
	}
	return &state, nil
}

func saveActiveJSON(s *ActiveState) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(activeJSONPath(), data, 0644)
}

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
func GetSystemPrompt() string {
	active, err := loadActiveJSON()
	if err != nil {
		return ""
	}

	var parts []string

	personaPath := filepath.Join(config.GetMPMDir(), "persona", active.Persona+".md")
	if data, err := os.ReadFile(personaPath); err == nil {
		if content := extractFrontmatterDirective(string(data)); content != "" {
			parts = append(parts, content)
		}
	}

	for _, mode := range active.Modes {
		modePath := filepath.Join(config.GetMPMDir(), "mode", mode+".md")
		if data, err := os.ReadFile(modePath); err == nil {
			if content := extractFrontmatterDirective(string(data)); content != "" {
				parts = append(parts, content)
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
