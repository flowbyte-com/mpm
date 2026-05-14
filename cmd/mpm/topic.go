package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"

	mpminternal "mpm/internal"
)

// =============================================================================
// mpm topic — Manual topic management for memories
// =============================================================================

func topicCmd(args []string) int {
	if len(args) < 2 {
		return topicCmdHelp()
	}
	switch args[1] {
	case "create", "mk":
		return topicCreate(args[2:])
	case "add":
		return topicAdd(args[2:])
	case "remove":
		return topicRemove(args[2:])
	case "list", "ls":
		return topicList(args[2:])
	case "show", "cat":
		return topicShow(args[2:])
	case "rm", "delete":
		return topicRm(args[2:])
	case "help":
		return topicCmdHelp()
	default:
		fmt.Fprintf(os.Stderr, "❌ Unknown topic subcommand: %s\n", args[1])
		return topicCmdHelp()
	}
}

func topicCmdHelp() int {
	fmt.Print(`mpm topic — Manual topic management

Usage: mpm topic <subcommand> [args]

Subcommands:
  create [--today | --yesterday | --from YYYY-MM-DD --to YYYY-MM-DD] [--desc description]
    Create a new topic. Optionally scope to a date range.

  add <memory-id> <topic-name>
    Add a memory to a topic (creates topic if it doesn't exist).

  remove <memory-id> <topic-name>
    Remove a memory from a topic.

  list
    List all topics with memory counts.

  show <topic-name>
    Show memories in a topic (manual + auto from date range).

  rm <topic-name>
    Delete a topic by name (memories are NOT deleted).
  delete <topic-name>
    Alias for rm.

Examples:
  mpm topic create --today
  mpm topic create --yesterday
  mpm topic create --today --yesterday
  mpm topic create "My Range" --from 2026-04-01 --to 2026-04-02
  mpm topic add abc123 "Data Pipeline"
  mpm topic show "Data Pipeline"
  mpm topic delete "Old Sessions"
`)
	return 0
}

// --- create ---

func topicCreate(args []string) int {
	var name, fromDate, toDate, description string
	var useToday, useYesterday bool

	today := time.Now().UTC().Format("2006-01-02")
	yesterday := time.Now().UTC().AddDate(0, 0, -1).Format("2006-01-02")

	i := 0
	for i < len(args) {
		switch args[i] {
		case "--today":
			useToday = true
			i++
		case "--yesterday":
			useYesterday = true
			i++
		case "--from":
			if i+1 >= len(args) {
				fmt.Fprintf(os.Stderr, "❌ --from requires a date (YYYY-MM-DD)\n")
				return 1
			}
			fromDate = args[i+1]
			i += 2
		case "--to":
			if i+1 >= len(args) {
				fmt.Fprintf(os.Stderr, "❌ --to requires a date (YYYY-MM-DD)\n")
				return 1
			}
			toDate = args[i+1]
			i += 2
		case "--desc":
			if i+1 >= len(args) {
				fmt.Fprintf(os.Stderr, "❌ --desc requires a description\n")
				return 1
			}
			description = args[i+1]
			i += 2
		default:
			// First non-flag arg is the name (for custom range case)
			if name == "" && !strings.HasPrefix(args[i], "--") {
				name = args[i]
				i++
			} else {
				fmt.Fprintf(os.Stderr, "❌ Unknown flag: %s\n", args[i])
				return 1
			}
		}
	}

	// Handle --today / --yesterday shortcuts
	if useToday || useYesterday {
		if useToday && useYesterday {
			name = "today"
			fromDate = yesterday
			toDate = today
		} else if useToday {
			name = "today"
			fromDate = today
			toDate = today
		} else {
			name = "yesterday"
			fromDate = yesterday
			toDate = yesterday
		}
	} else if name == "" {
		fmt.Fprintf(os.Stderr, "❌ Usage: mpm topic create [--today | --yesterday | --from DATE --to DATE | name]\n")
		return 1
	}

	if name == "" {
		fmt.Fprintf(os.Stderr, "❌ Topic name required\n")
		return 1
	}

	// Check if topic already exists (idempotent for --today/--yesterday)
	dbMgr, err := mpminternal.NewDatabaseManager("")
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ DB: %v\n", err)
		return 1
	}
	defer dbMgr.Close()

	existingID, _ := dbMgr.GetTopicByName(name)
	if existingID != "" {
		fmt.Printf("⏭️  Topic '%s' already exists (id: %s)\n", name, existingID[:8])
		return 0
	}

	topicID, err := dbMgr.CreateTopic(name, description, fromDate, toDate)
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ Failed to create topic: %v\n", err)
		return 1
	}

	scope := ""
	if fromDate != "" || toDate != "" {
		scope = fmt.Sprintf(" [scope: %s → %s]", orEmpty(fromDate, "?"), orEmpty(toDate, "?"))
	}
	fmt.Printf("✅ Created topic: %s (id: %s)%s\n", name, topicID[:8], scope)
	return 0
}

// --- add ---

func topicAdd(args []string) int {
	if len(args) < 2 {
		fmt.Fprintf(os.Stderr, "Usage: mpm topic add <memory-id> <topic-name>\n")
		return 1
	}
	memoryID, topicName := args[0], strings.Join(args[1:], " ")

	dbMgr, err := mpminternal.NewDatabaseManager("")
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ DB: %v\n", err)
		return 1
	}
	defer dbMgr.Close()

	topicID, err := dbMgr.GetOrCreateTopic(topicName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ Failed to get/create topic: %v\n", err)
		return 1
	}

	err = dbMgr.AddMemoryToTopic(memoryID, topicID, "manual")
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ Failed to add memory to topic: %v\n", err)
		return 1
	}
	idShort := memoryID
	if len(idShort) > 8 {
		idShort = idShort[:8]
	}
	fmt.Printf("✅ Added %s to '%s'\n", idShort, topicName)
	return 0
}

// --- remove ---

func topicRemove(args []string) int {
	if len(args) < 2 {
		fmt.Fprintf(os.Stderr, "Usage: mpm topic remove <memory-id> <topic-name>\n")
		return 1
	}
	memoryID, topicName := args[0], strings.Join(args[1:], " ")

	dbMgr, err := mpminternal.NewDatabaseManager("")
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ DB: %v\n", err)
		return 1
	}
	defer dbMgr.Close()

	topicID, err := dbMgr.GetTopicByName(topicName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ Topic not found: %s\n", topicName)
		return 1
	}

	err = dbMgr.RemoveMemoryFromTopic(memoryID, topicID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ Failed to remove: %v\n", err)
		return 1
	}
	idShort := memoryID
	if len(idShort) > 8 {
		idShort = idShort[:8]
	}
	fmt.Printf("✅ Removed %s from '%s'\n", idShort, topicName)
	return 0
}

// --- list ---

func topicList(args []string) int {
	dbMgr, err := mpminternal.NewDatabaseManager("")
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ DB: %v\n", err)
		return 1
	}
	defer dbMgr.Close()

	topics, err := dbMgr.ListTopics()
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ Failed to list topics: %v\n", err)
		return 1
	}

	cyan := "\033[36m"
	magenta := "\033[35m"
	reset := "\033[0m"
	bold := "\033[1m"

	if len(topics) == 0 {
		fmt.Println("No topics yet. Run: mpm topic create <name>")
		return 0
	}

	fmt.Printf("%s%s%d topics%s\n\n", bold, cyan, len(topics), reset)
	for _, t := range topics {
		scope := ""
		if from := strField(t, "from_date"); from != "" {
			scope = fmt.Sprintf(" %s[%s → %s]%s", magenta, from, strField(t, "to_date"), reset)
		}
		age := strField(t, "created_at")
		if len(age) >= 10 {
			age = age[:10]
		}
		fmt.Printf("%s★ %s%s%s%s\n    %d memories | created %s\n",
			cyan, bold, t["name"], reset, scope,
			intField(t, "memory_count"), age)
	}
	return 0
}

// --- show ---

func topicShow(args []string) int {
	if len(args) < 1 {
		fmt.Fprintf(os.Stderr, "Usage: mpm topic show <topic-name>\n")
		return 1
	}
	topicName := strings.Join(args, " ")

	dbMgr, err := mpminternal.NewDatabaseManager("")
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ DB: %v\n", err)
		return 1
	}
	defer dbMgr.Close()

	topicID, err := dbMgr.GetTopicByName(topicName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ Topic not found: %s\n", topicName)
		return 1
	}

	topic, err := dbMgr.GetTopic(topicID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ Failed to get topic: %v\n", err)
		return 1
	}

	memories, err := dbMgr.GetTopicMemories(topicID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ Failed to get topic memories: %v\n", err)
		return 1
	}

	cyan := "\033[36m"
	magenta := "\033[35m"
	reset := "\033[0m"
	bold := "\033[1m"

	fmt.Printf("%s%s [%s]%s\n", bold, topic["name"], topicID[:min(8, len(topicID))], reset)
	if from := strField(topic, "from_date"); from != "" {
		fmt.Printf("  %sScope: %s → %s%s\n", magenta, from, strField(topic, "to_date"), reset)
	}
	fmt.Printf("  %d memories\n\n", len(memories))

	for i, m := range memories {
		role := strField(m, "role")
		roleTag := ""
		if role == "auto" {
			roleTag = fmt.Sprintf(" %s[auto]%s", magenta, reset)
		}
		content := strField(m, "content")
		if len(content) > 220 {
			content = content[:220] + "..."
		}
		content = stripMarkdownDisplay(content)
		age := strField(m, "created_at")
		if len(age) >= 10 {
			age = age[:10]
		}
		sid := strField(m, "session_id")
		sessionTag := ""
		if sid != "" {
			sessionTag = fmt.Sprintf(" %s[%s]%s", magenta, sid[:min(8, len(sid))], reset)
		}
		fmt.Printf("%s%d.%s%s%s  %s\n    %s\n\n",
			cyan, i+1, reset, roleTag, sessionTag, age, content)
	}
	return 0
}

// --- delete ---

func topicRm(args []string) int {
	if len(args) < 1 {
		fmt.Fprintf(os.Stderr, "Usage: mpm topic rm <topic-name>\n")
		return 1
	}
	topicName := strings.Join(args, " ")

	dbMgr, err := mpminternal.NewDatabaseManager("")
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ DB: %v\n", err)
		return 1
	}
	defer dbMgr.Close()

	topicID, err := dbMgr.GetTopicByName(topicName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ Topic not found: %s\n", topicName)
		return 1
	}

	err = dbMgr.DeleteTopic(topicID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ Failed to delete topic: %v\n", err)
		return 1
	}
	fmt.Printf("✅ Deleted topic '%s' (memories preserved)\n", topicName)
	return 0
}

// =============================================================================
// Helpers
// =============================================================================

func strField(m map[string]interface{}, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

func intField(m map[string]interface{}, key string) int {
	switch v := m[key].(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	}
	return 0
}

func orEmpty(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}


func stripMarkdownDisplay(s string) string {
	s = strings.TrimPrefix(s, "#")
	s = strings.TrimPrefix(s, " ")
	// Remove common markdown
	// Strip all leading # characters and whitespace
	for strings.HasPrefix(s, "#") {
		s = strings.TrimPrefix(s, "#")
	}
	s = strings.TrimSpace(s)
	// Strip all leading # characters and whitespace
	for strings.HasPrefix(s, "#") {
		s = strings.TrimPrefix(s, "#")
	}
	s = strings.TrimSpace(s)
	replacer := strings.NewReplacer(
		"**", "", "*", "", "`", "",
		"\n", " ", "  ", " ",
	)
	s = replacer.Replace(s)
	return strings.TrimSpace(s)
}

// =============================================================================
// Topic Suggestion Helpers
// =============================================================================

var stopWords = map[string]bool{
	"the":    true,
	"is":     true,
	"at":     true,
	"to":     true,
	"a":      true,
	"in":     true,
	"on":     true,
	"for":    true,
	"of":     true,
	"and":    true,
	"or":     true,
	"but":    true,
	"with":   true,
	"as":     true,
	"by":     true,
	"from":   true,
	"it":     true,
	"this":   true,
	"that":   true,
	"be":     true,
	"have":   true,
	"has":    true,
	"had":    true,
	"were":   true,
	"was":    true,
	"are":    true,
	"been":   true,
	"being":  true,
}

// sanitizeContentForFTS strips markdown, filters stop-words and short words,
// joins remaining keywords with " OR " for FTS5 query.
func sanitizeContentForFTS(content string) string {
	// Strip markdown (using same patterns as recall.go)
	content = stripMarkdownHeadings.ReplaceAllString(content, "")
	content = stripMarkdownBold.ReplaceAllString(content, "$1")
	content = stripMarkdownItalic.ReplaceAllString(content, "$1")
	content = stripMarkdownCode.ReplaceAllString(content, "$1")

	// Split on whitespace
	words := strings.Fields(content)

	// Filter stop-words and words < 4 chars
	var keywords []string
	for _, w := range words {
		lower := strings.ToLower(w)
		if stopWords[lower] {
			continue
		}
		if len(w) < 4 {
			continue
		}
		keywords = append(keywords, lower)
	}

	// Need at least 2 keywords
	if len(keywords) < 2 {
		return ""
	}

	return strings.Join(keywords, " OR ")
}

// computeTopicConfidence counts how many memoryKeywords appear in topicName.
// Returns matched / total_topic_words (0 if no topic words).
func computeTopicConfidence(memoryKeywords []string, topicName string) float64 {
	if len(memoryKeywords) == 0 || topicName == "" {
		return 0
	}

	topicWords := strings.Fields(strings.ToLower(topicName))
	if len(topicWords) == 0 {
		return 0
	}

	var matched int
	for _, kw := range memoryKeywords {
		kwLower := strings.ToLower(kw)
		for _, tw := range topicWords {
			if kwLower == tw {
				matched++
				break
			}
		}
	}

	return float64(matched) / float64(len(topicWords))
}

// suggestTopicsForMemory uses FTS5 to find topics matching the content,
// scores them by confidence, and returns suggestions meeting minConfidence.
func suggestTopicsForMemory(dm *mpminternal.DatabaseManager, memoryID, content string, maxTopics int, minConfidence float64) ([]map[string]interface{}, error) {
	// Sanitize content for FTS query
	ftsQuery := sanitizeContentForFTS(content)
	if ftsQuery == "" {
		return nil, nil
	}

	// Also get memory keywords from sanitize output for confidence scoring
	memoryKeywords := strings.Fields(ftsQuery)

	// FTS5 query to find active topics matching the content
	sql := `
		SELECT t.id, t.name, t.description
		FROM topics t
		JOIN topics_fts fts ON t.rowid = fts.rowid
		WHERE topics_fts MATCH ? AND t.is_active = 1
		LIMIT ?`

	rows, err := dm.SQLDB().Query(sql, ftsQuery, maxTopics)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var suggestions []map[string]interface{}
	for rows.Next() {
		var id, name, description string
		if err := rows.Scan(&id, &name, &description); err != nil {
			continue
		}

		// Compute confidence using topic name (and description, take max)
		confidenceName := computeTopicConfidence(memoryKeywords, name)
		confidenceDesc := computeTopicConfidence(memoryKeywords, description)
		confidence := confidenceName
		if confidenceDesc > confidence {
			confidence = confidenceDesc
		}

		if confidence < minConfidence {
			continue
		}

		suggestions = append(suggestions, map[string]interface{}{
			"id":          id,
			"name":        name,
			"description": description,
			"confidence":  confidence,
		})
	}

	return suggestions, nil
}
