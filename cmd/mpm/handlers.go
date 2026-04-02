package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"mpm/internal"
)

// ============================================================================
// Handler: memory
// ============================================================================

func handleMemory(conn net.Conn, args []string) {
	// Parse subcommand - args[0] is the subcommand (add, list, search, etc.)
	if len(args) < 1 {
		// No subcommand - show help
		handleMemoryHelp(conn)
		return
	}

	subCmd := args[0]
	switch subCmd {
	case "help":
		handleMemoryHelp(conn)
	case "add":
		handleMemoryAdd(conn, args[1:])
	case "search":
		handleMemorySearch(conn, args[1:])
	case "show":
		handleMemoryShow(conn, args[1:])
	case "shred":
		handleMemoryShred(conn, args[1:])
	case "list":
		handleMemoryList(conn, args[1:])
	case "search-term":
		handleMemorySearchTerm(conn, args[1:])
	case "wipe":
		handleMemoryWipe(conn, args[1:])
	default:
		handleMemoryHelp(conn)
	}
}

func handleMemoryHelp(conn net.Conn) {
	output := `mpm memory - Memory operations

Usage:
  mpm memory add <content>      Add a new memory (returns ID)
  mpm memory search <query>      Search memories
  mpm memory search-term <term>  List memories matching term (500 char snippets)
  mpm memory show <id>           Show memory by ID
  mpm memory shred <id>          Secure delete memory by ID
  mpm memory list                List recent memories
  mpm memory wipe                Wipe all memories (requires -f)

Examples:
  mpm memory add "Remember to call mom"
  mpm memory search "mom"
  mpm memory search-term "project"
  mpm memory show abc123
  mpm memory shred abc123
`
	sendResponse(conn, output, "", true, 0)
}

func handleMemoryAdd(conn net.Conn, args []string) {
	if len(args) == 0 {
		sendResponse(conn, "", "Usage: mpm memory add <content>", true, 1)
		return
	}

	content := strings.Join(args, " ")
	store := getMemoryStore()
	
	mem, err := store.AddMemory(content, "memories", nil, nil, "", "cli")
	if err != nil {
		sendResponse(conn, "", fmt.Sprintf("Failed to add memory: %v", err), true, 1)
		return
	}

	sendResponse(conn, fmt.Sprintf("Memory added with ID: %s\n", mem.ID), "", true, 0)
}

func handleMemorySearch(conn net.Conn, args []string) {
	if len(args) == 0 {
		sendResponse(conn, "", "Usage: mpm memory search <query>", true, 1)
		return
	}

	query := strings.Join(args, " ")
	store := getMemoryStore()

	memories, err := store.FullTextSearch(query, "memories", 20)
	if err != nil {
		sendResponse(conn, "", fmt.Sprintf("Search failed: %v", err), true, 1)
		return
	}

	if len(memories) == 0 {
		sendResponse(conn, "No memories found.\n", "", true, 0)
		return
	}

	var output strings.Builder
	output.WriteString(fmt.Sprintf("Found %d memories:\n\n", len(memories)))
	
	for _, mem := range memories {
		snippet := mem.Content
		if len(snippet) > 500 {
			snippet = snippet[:500] + "..."
		}
		snippet = strings.ReplaceAll(snippet, "\n", " ")
		
		created := mem.Created
		if len(created) > 10 {
			created = created[:10]
		}
		
		output.WriteString(fmt.Sprintf("[%s] %s\n", mem.ID, created))
		output.WriteString(fmt.Sprintf("    %s\n\n", snippet))
	}

	sendResponse(conn, output.String(), "", true, 0)
}

func handleMemoryShow(conn net.Conn, args []string) {
	if len(args) == 0 {
		sendResponse(conn, "", "Usage: mpm memory show <id>", true, 1)
		return
	}

	id := args[0]
	store := getMemoryStore()

	mem, err := store.GetByID(id, "memories")
	if err != nil || mem == nil {
		sendResponse(conn, "", fmt.Sprintf("Memory not found: %s\n", id), true, 1)
		return
	}

	var output strings.Builder
	output.WriteString(fmt.Sprintf("ID:      %s\n", mem.ID))
	output.WriteString(fmt.Sprintf("Created: %s\n", mem.Created))
	if mem.Source != "" {
		output.WriteString(fmt.Sprintf("Source:  %s\n", mem.Source))
	}
	if len(mem.Tags) > 0 {
		output.WriteString(fmt.Sprintf("Tags:    %s\n", strings.Join(mem.Tags, ", ")))
	}
	output.WriteString("\n")
	output.WriteString(mem.Content)
	output.WriteString("\n")

	sendResponse(conn, output.String(), "", true, 0)
}

func handleMemoryShred(conn net.Conn, args []string) {
	if len(args) == 0 {
		sendResponse(conn, "", "Usage: mpm memory shred <id>", true, 1)
		return
	}

	id := args[0]
	store := getMemoryStore()

	err := store.DeleteMemory(id, "memories")
	if err != nil {
		sendResponse(conn, "", fmt.Sprintf("Failed to shred memory: %v", err), true, 1)
		return
	}

	sendResponse(conn, fmt.Sprintf("Memory shredded: %s\n", id), "", true, 0)
}

func handleMemoryList(conn net.Conn, args []string) {
	store := getMemoryStore()

	memories, err := store.GetRecent(20)
	if err != nil {
		sendResponse(conn, "", fmt.Sprintf("Failed to list memories: %v", err), true, 1)
		return
	}

	if len(memories) == 0 {
		sendResponse(conn, "No memories stored.\n", "", true, 0)
		return
	}

	var output strings.Builder
	output.WriteString(fmt.Sprintf("Recent %d memories:\n\n", len(memories)))
	
	for _, mem := range memories {
		snippet := mem.Content
		if len(snippet) > 500 {
			snippet = snippet[:500] + "..."
		}
		snippet = strings.ReplaceAll(snippet, "\n", " ")
		
		created := mem.Created
		if len(created) > 10 {
			created = created[:10]
		}
		
		output.WriteString(fmt.Sprintf("[%s] %s\n", mem.ID, created))
		output.WriteString(fmt.Sprintf("    %s\n\n", snippet))
	}

	sendResponse(conn, output.String(), "", true, 0)
}

func handleMemorySearchTerm(conn net.Conn, args []string) {
	if len(args) == 0 {
		sendResponse(conn, "", "Usage: mpm memory search-term <term>", true, 1)
		return
	}

	term := strings.Join(args, " ")
	store := getMemoryStore()

	// Use the existing search functionality
	memories, err := store.FullTextSearch(term, "memories", 50)
	if err != nil {
		sendResponse(conn, "", fmt.Sprintf("Search failed: %v", err), true, 1)
		return
	}

	if len(memories) == 0 {
		sendResponse(conn, fmt.Sprintf("No memories matching '%s' found.\n", term), "", true, 0)
		return
	}

	var output strings.Builder
	output.WriteString(fmt.Sprintf("Found %d memories matching '%s':\n\n", len(memories), term))
	
	for _, mem := range memories {
		snippet := mem.Content
		if len(snippet) > 500 {
			snippet = snippet[:500] + "..."
		}
		snippet = strings.ReplaceAll(snippet, "\n", " ")
		
		created := mem.Created
		if len(created) > 10 {
			created = created[:10]
		}
		
		output.WriteString(fmt.Sprintf("[%s] %s\n", mem.ID, created))
		output.WriteString(fmt.Sprintf("    %s\n\n", snippet))
	}

	sendResponse(conn, output.String(), "", true, 0)
}

func handleMemoryWipe(conn net.Conn, args []string) {
	// Check for --force flag
	force := false
	for _, arg := range args {
		if arg == "--force" || arg == "-f" {
			force = true
		}
	}

	if !force {
		sendResponse(conn, "", "Wipe requires --force flag\n", true, 1)
		return
	}

	store := getMemoryStore()
	
	// Clear the mirror file
	err := store.ClearMirror()
	if err != nil {
		sendResponse(conn, "", fmt.Sprintf("Failed to wipe memories: %v", err), true, 1)
		return
	}

	sendResponse(conn, "All memories wiped.\n", "", true, 0)
}

// ============================================================================
// Handler: shred (topic, session, memory - same options)
// ============================================================================

func handleShred(conn net.Conn, args []string) {
	if len(args) < 1 {
		handleShredHelp(conn)
		return
	}

	targetType := args[0]
	
	switch targetType {
	case "sessions":
		handleShredSessions(conn, args[1:])
	case "memories":
		handleShredMemories(conn, args[1:])
	case "topics":
		handleShredTopics(conn, args[1:])
	case "database":
		handleShredDatabase(conn, args[1:])
	case "modes":
		handleShredModes(conn, args[1:])
	case "personas":
		handleShredPersonas(conn, args[1:])
	default:
		// Legacy: single item by ID (topic/session)
		if len(args) < 2 {
			handleShredHelp(conn)
			return
		}
		id := args[1]
		switch targetType {
		case "topic":
			handleShredTopic(conn, id)
		case "session":
			handleShredSession(conn, id)
		default:
			handleShredHelp(conn)
		}
	}
}

func handleShredHelp(conn net.Conn) {
	output := `mpm shred - Secure delete operations

Usage:
  mpm shred sessions         Delete all sessions (requires -f to confirm)
  mpm shred memories         Delete all memories (requires -f to confirm)
  mpm shred topics           Delete all topics (requires -f to confirm)
  mpm shred database         Delete entire database (requires -f to confirm)
  mpm shred modes            Delete all modes (requires -f to confirm)
  mpm shred personas         Delete all personas (requires -f to confirm)
  mpm shred topic <id>       Delete a topic by ID (requires -f to confirm)
  mpm shred session <id>     Delete a session by ID (requires -f to confirm)

Examples:
  mpm shred sessions -f
  mpm shred memories -f
  mpm shred topics -f
  mpm shred database -f
  mpm shred modes -f
  mpm shred personas -f
  mpm shred topic abc123 -f
  mpm shred session xyz789 -f
`
	sendResponse(conn, output, "", true, 0)
}

func handleShredSessions(conn net.Conn, args []string) {
	// Check for --force flag
	force := false
	for _, arg := range args {
		if arg == "--force" || arg == "-f" {
			force = true
		}
	}

	if !force {
		sendResponse(conn, "", "Warning: This will delete ALL sessions. Use 'mpm shred sessions -f' to confirm.\n", true, 1)
		return
	}

	store := getMemoryStore()
	count, err := store.DeleteAllByCollection("sessions")
	if err != nil {
		sendResponse(conn, "", fmt.Sprintf("Failed to delete sessions: %v", err), true, 1)
		return
	}

	// Run VACUUM to reclaim space
	db := store.DB
	if _, err := db.Exec("VACUUM"); err != nil {
		sendResponse(conn, "", fmt.Sprintf("Sessions deleted but vacuum failed: %v", err), true, 1)
		return
	}

	sendResponse(conn, fmt.Sprintf("All sessions deleted (%d records).\n", count), "", true, 0)
}

func handleShredMemories(conn net.Conn, args []string) {
	// Check for --force flag
	force := false
	for _, arg := range args {
		if arg == "--force" || arg == "-f" {
			force = true
		}
	}

	if !force {
		sendResponse(conn, "", "Warning: This will delete ALL memories. Use 'mpm shred memories -f' to confirm.\n", true, 1)
		return
	}

	store := getMemoryStore()
	count, err := store.DeleteAllMemories()
	if err != nil {
		sendResponse(conn, "", fmt.Sprintf("Failed to delete memories: %v", err), true, 1)
		return
	}

	// Run VACUUM to reclaim space
	db := store.DB
	if _, err := db.Exec("VACUUM"); err != nil {
		sendResponse(conn, "", fmt.Sprintf("Memories deleted but vacuum failed: %v", err), true, 1)
		return
	}

	sendResponse(conn, fmt.Sprintf("All memories deleted (%d records).\n", count), "", true, 0)
}

func handleShredTopics(conn net.Conn, args []string) {
	// Check for --force flag
	force := false
	for _, arg := range args {
		if arg == "--force" || arg == "-f" {
			force = true
		}
	}

	if !force {
		sendResponse(conn, "", "Warning: This will delete ALL topics. Use 'mpm shred topics -f' to confirm.\n", true, 1)
		return
	}

	store := getMemoryStore()
	db := store.DB

	// Delete topic memberships first
	if _, err := db.Exec("DELETE FROM topic_memberships"); err != nil {
		sendResponse(conn, "", fmt.Sprintf("Failed to delete topic memberships: %v", err), true, 1)
		return
	}

	// Delete topics
	result, err := db.Exec("DELETE FROM topics")
	if err != nil {
		sendResponse(conn, "", fmt.Sprintf("Failed to delete topics: %v", err), true, 1)
		return
	}

	count, _ := result.RowsAffected()

	// Run VACUUM to reclaim space
	if _, err := db.Exec("VACUUM"); err != nil {
		sendResponse(conn, "", fmt.Sprintf("Topics deleted but vacuum failed: %v", err), true, 1)
		return
	}

	sendResponse(conn, fmt.Sprintf("All topics deleted (%d records).\n", count), "", true, 0)
}

func handleShredDatabase(conn net.Conn, args []string) {
	// Check for --force flag
	force := false
	for _, arg := range args {
		if arg == "--force" || arg == "-f" {
			force = true
		}
	}

	if !force {
		sendResponse(conn, "", "Warning: This will delete the entire database and create a new one. Use 'mpm shred database -f' to confirm.\n", true, 1)
		return
	}

	paths := internal.DefaultMemoryPaths()
	dbPath := paths.SQLiteDBPath

	// Close any open connections
	store := getMemoryStore()
	db := store.DB
	if err := db.Close(); err != nil {
		sendResponse(conn, "", fmt.Sprintf("Failed to close database: %v", err), true, 1)
		return
	}

	// Remove the database file
	if err := os.Remove(dbPath); err != nil {
		sendResponse(conn, "", fmt.Sprintf("Failed to remove database file: %v", err), true, 1)
		return
	}

	// Recreate the database
	newStore := internal.NewMemoryStore(filepath.Dir(dbPath))
	if newStore.DB == nil {
		sendResponse(conn, "", "Failed to recreate database", true, 1)
		return
	}

	// Close the new store
	if err := newStore.DB.Close(); err != nil {
		sendResponse(conn, "", fmt.Sprintf("Failed to close new database: %v", err), true, 1)
		return
	}

	sendResponse(conn, fmt.Sprintf("Database recreated at: %s\n", dbPath), "", true, 0)
}

func handleShredModes(conn net.Conn, args []string) {
	// Check for --force flag
	force := false
	for _, arg := range args {
		if arg == "--force" || arg == "-f" {
			force = true
		}
	}

	if !force {
		sendResponse(conn, "", "Warning: This will delete ALL modes. Use 'mpm shred modes -f' to confirm.\n", true, 1)
		return
	}

	mm := internal.NewModeManager("")
	count, err := mm.RemoveAll()
	if err != nil {
		sendResponse(conn, "", fmt.Sprintf("Failed to delete modes: %v", err), true, 1)
		return
	}

	sendResponse(conn, fmt.Sprintf("All modes deleted (%d records).\n", count), "", true, 0)
}

func handleShredPersonas(conn net.Conn, args []string) {
	// Check for --force flag
	force := false
	for _, arg := range args {
		if arg == "--force" || arg == "-f" {
			force = true
		}
	}

	if !force {
		sendResponse(conn, "", "Warning: This will delete ALL personas. Use 'mpm shred personas -f' to confirm.\n", true, 1)
		return
	}

	pm := internal.NewPersonaManager("")
	count, err := pm.RemoveAll()
	if err != nil {
		sendResponse(conn, "", fmt.Sprintf("Failed to delete personas: %v", err), true, 1)
		return
	}

	sendResponse(conn, fmt.Sprintf("All personas deleted (%d records).\n", count), "", true, 0)
}

func handleShredTopic(conn net.Conn, id string) {
	store := getMemoryStore()

	// Get the topic first to verify it exists
	topic, err := store.SearchTopics(id, 1)
	if err != nil || len(topic) == 0 {
		sendResponse(conn, "", fmt.Sprintf("Topic not found: %s\n", id), true, 1)
		return
	}

	// Use the internal ShredTopic function via DeleteByID
	db := store.DB
	tx, err := db.Begin()
	if err != nil {
		sendResponse(conn, "", fmt.Sprintf("Failed to shred topic: %v", err), true, 1)
		return
	}
	defer tx.Rollback()

	// Delete memberships first
	if _, err = tx.Exec("DELETE FROM topic_memberships WHERE topic_id = ?", id); err != nil {
		sendResponse(conn, "", fmt.Sprintf("Failed to shred topic: %v", err), true, 1)
		return
	}

	// Delete the topic
	if _, err = tx.Exec("DELETE FROM topics WHERE id = ?", id); err != nil {
		sendResponse(conn, "", fmt.Sprintf("Failed to shred topic: %v", err), true, 1)
		return
	}

	if err := tx.Commit(); err != nil {
		sendResponse(conn, "", fmt.Sprintf("Failed to shred topic: %v", err), true, 1)
		return
	}

	_, err = db.Exec("VACUUM")
	sendResponse(conn, fmt.Sprintf("Topic shredded: %s\n", id), "", true, 0)
}

func handleShredSession(conn net.Conn, id string) {
	store := getMemoryStore()

	err := store.DeleteMemory(id, "sessions")
	if err != nil {
		sendResponse(conn, "", fmt.Sprintf("Failed to shred session: %v", err), true, 1)
		return
	}

	sendResponse(conn, fmt.Sprintf("Session shredded: %s\n", id), "", true, 0)
}

// ============================================================================
// Handler: topic
// ============================================================================

func handleTopic(conn net.Conn, args []string) {
	if len(args) < 1 {
		handleTopicHelp(conn)
		return
	}

	subCmd := args[0]
	switch subCmd {
	case "help":
		handleTopicHelp(conn)
	case "add":
		handleTopicAdd(conn, args[1:])
	case "search":
		handleTopicSearch(conn, args[1:])
	case "show":
		handleTopicShow(conn, args[1:])
	case "promote":
		handleTopicPromote(conn, args[1:])
	case "shred":
		if len(args) < 2 {
			handleTopicHelp(conn)
			return
		}
		handleShredTopic(conn, args[1])
	case "list":
		handleTopicList(conn, args[1:])
	default:
		handleTopicHelp(conn)
	}
}

func handleTopicHelp(conn net.Conn) {
	output := `mpm topic - Topic operations

Usage:
  mpm topic add <name> [description]    Add a new topic
  mpm topic search <query>            Search topics
  mpm topic show <id>                  Show topic by ID
  mpm topic promote <id>               Promote topic to memory
  mpm topic shred <id>                 Secure delete topic
  mpm topic list                       List topics

Examples:
  mpm topic add "golang patterns" "Things to remember about Go"
  mpm topic search "golang"
  mpm topic show abc123
  mpm topic promote abc123
`
	sendResponse(conn, output, "", true, 0)
}

func handleTopicAdd(conn net.Conn, args []string) {
	if len(args) == 0 {
		sendResponse(conn, "", "Usage: mpm topic add <name> [description]", true, 1)
		return
	}

	name := args[0]
	description := ""
	if len(args) > 1 {
		description = strings.Join(args[1:], " ")
	}

	store := getMemoryStore()
	db := store.DB

	// Add to topics table
	id := internal.GenerateID()
	_, err := db.Exec(`
		INSERT INTO topics (id, name, description)
		VALUES (?, ?, ?)
	`, id, name, description)

	if err != nil {
		sendResponse(conn, "", fmt.Sprintf("Failed to add topic: %v", err), true, 1)
		return
	}

	sendResponse(conn, fmt.Sprintf("Topic added: %s\n", name), "", true, 0)
}

func handleTopicSearch(conn net.Conn, args []string) {
	if len(args) == 0 {
		sendResponse(conn, "", "Usage: mpm topic search <query>", true, 1)
		return
	}

	query := strings.Join(args, " ")
	store := getMemoryStore()

	results, err := store.SearchTopics(query, 20)
	if err != nil {
		sendResponse(conn, "", fmt.Sprintf("Search failed: %v", err), true, 1)
		return
	}

	if len(results) == 0 {
		sendResponse(conn, "No topics found.\n", "", true, 0)
		return
	}

	var output strings.Builder
	output.WriteString(fmt.Sprintf("Found %d topics:\n\n", len(results)))

	for _, r := range results {
		snippet := r.Snippet
		if len(snippet) > 500 {
			snippet = snippet[:500] + "..."
		}
		output.WriteString(fmt.Sprintf("[%s]\n", r.ID))
		output.WriteString(fmt.Sprintf("    %s\n\n", snippet))
	}

	sendResponse(conn, output.String(), "", true, 0)
}

func handleTopicShow(conn net.Conn, args []string) {
	if len(args) == 0 {
		sendResponse(conn, "", "Usage: mpm topic show <id>", true, 1)
		return
	}

	id := args[0]
	store := getMemoryStore()
	db := store.DB

	var name, description, created string
	err := db.QueryRow(`
		SELECT name, description, created_at FROM topics WHERE id = ?
	`, id).Scan(&name, &description, &created)

	if err != nil {
		sendResponse(conn, "", fmt.Sprintf("Topic not found: %s\n", id), true, 1)
		return
	}

	var output strings.Builder
	output.WriteString(fmt.Sprintf("ID:          %s\n", id))
	output.WriteString(fmt.Sprintf("Name:        %s\n", name))
	output.WriteString(fmt.Sprintf("Created:     %s\n", created))
	if description != "" {
		output.WriteString(fmt.Sprintf("\nDescription:\n%s\n", description))
	}

	sendResponse(conn, output.String(), "", true, 0)
}

func handleTopicPromote(conn net.Conn, args []string) {
	if len(args) == 0 {
		sendResponse(conn, "", "Usage: mpm topic promote <id>", true, 1)
		return
	}

	id := args[0]
	store := getMemoryStore()

	err := store.PromoteTopicToMemory(id, "memories", nil)
	if err != nil {
		sendResponse(conn, "", fmt.Sprintf("Failed to promote topic: %v", err), true, 1)
		return
	}

	sendResponse(conn, fmt.Sprintf("Topic promoted to memory: %s\n", id), "", true, 0)
}

func handleTopicList(conn net.Conn, args []string) {
	store := getMemoryStore()
	db := store.DB

	rows, err := db.Query(`
		SELECT id, name, description, created_at FROM topics 
		ORDER BY created_at DESC LIMIT 20
	`)
	if err != nil {
		sendResponse(conn, "", fmt.Sprintf("Failed to list topics: %v", err), true, 1)
		return
	}
	defer rows.Close()

	var output strings.Builder
	output.WriteString("Topics:\n\n")

	for rows.Next() {
		var id, name, description, created string
		if err := rows.Scan(&id, &name, &description, &created); err != nil {
			continue
		}
		if len(description) > 100 {
			description = description[:100] + "..."
		}
		output.WriteString(fmt.Sprintf("[%s] %s\n", id, name))
		if description != "" {
			output.WriteString(fmt.Sprintf("    %s\n", description))
		}
	}

	sendResponse(conn, output.String(), "", true, 0)
}

// ============================================================================
// Handler: session
// ============================================================================

func handleSession(conn net.Conn, args []string) {
	if len(args) < 1 {
		handleSessionHelp(conn)
		return
	}

	subCmd := args[0]
	switch subCmd {
	case "help":
		handleSessionHelp(conn)
	case "add":
		handleSessionAdd(conn, args[1:])
	case "search":
		handleSessionSearch(conn, args[1:])
	case "show":
		handleSessionShow(conn, args[1:])
	case "shred":
		if len(args) < 2 {
			handleSessionHelp(conn)
			return
		}
		handleShredSession(conn, args[1])
	case "list":
		handleSessionList(conn, args[1:])
	default:
		handleSessionHelp(conn)
	}
}

func handleSessionHelp(conn net.Conn) {
	output := `mpm session - Session operations

Usage:
  mpm session add <content>      Add a new session entry (returns ID)
  mpm session search <query>     Search sessions
  mpm session show <id>          Show session by ID
  mpm session shred <id>         Secure delete session
  mpm session list               List recent sessions

Examples:
  mpm session add "Session notes for project discussion"
  mpm session search "golang"
  mpm session show abc123
  mpm session shred abc123
`
	sendResponse(conn, output, "", true, 0)
}

func handleSessionAdd(conn net.Conn, args []string) {
	if len(args) == 0 {
		sendResponse(conn, "", "Usage: mpm session add <content>", true, 1)
		return
	}

	content := strings.Join(args, " ")
	store := getMemoryStore()
	
	// Add to session collection
	mem, err := store.AddMemory(content, "session", nil, nil, "", "cli")
	if err != nil {
		sendResponse(conn, "", fmt.Sprintf("Failed to add session: %v", err), true, 1)
		return
	}

	sendResponse(conn, fmt.Sprintf("Session added with ID: %s\n", mem.ID), "", true, 0)
}

func handleSessionSearch(conn net.Conn, args []string) {
	if len(args) == 0 {
		sendResponse(conn, "", "Usage: mpm session search <query>", true, 1)
		return
	}

	query := strings.Join(args, " ")
	store := getMemoryStore()

	memories, err := store.SearchSessions(query, 20)
	if err != nil {
		sendResponse(conn, "", fmt.Sprintf("Search failed: %v", err), true, 1)
		return
	}

	if len(memories) == 0 {
		sendResponse(conn, "No sessions found.\n", "", true, 0)
		return
	}

	var output strings.Builder
	output.WriteString(fmt.Sprintf("Found %d sessions:\n\n", len(memories)))

	for _, mem := range memories {
		snippet := mem.Content
		if len(snippet) > 500 {
			snippet = snippet[:500] + "..."
		}
		snippet = strings.ReplaceAll(snippet, "\n", " ")

		output.WriteString(fmt.Sprintf("[%s] %s\n", mem.ID, mem.Created[:10]))
		output.WriteString(fmt.Sprintf("    %s\n\n", snippet))
	}

	sendResponse(conn, output.String(), "", true, 0)
}

func handleSessionShow(conn net.Conn, args []string) {
	if len(args) == 0 {
		sendResponse(conn, "", "Usage: mpm session show <id>", true, 1)
		return
	}

	id := args[0]
	store := getMemoryStore()

	mem, err := store.GetByID(id, "session")
	if err != nil || mem == nil {
		sendResponse(conn, "", fmt.Sprintf("Session not found: %s\n", id), true, 1)
		return
	}

	var output strings.Builder
	output.WriteString(fmt.Sprintf("ID:      %s\n", mem.ID))
	output.WriteString(fmt.Sprintf("Created: %s\n\n", mem.Created))
	output.WriteString(mem.Content)
	output.WriteString("\n")

	sendResponse(conn, output.String(), "", true, 0)
}

func handleSessionList(conn net.Conn, args []string) {
	store := getMemoryStore()

	memories, err := store.GetRecent(20)
	if err != nil {
		sendResponse(conn, "", fmt.Sprintf("Failed to list sessions: %v", err), true, 1)
		return
	}

	if len(memories) == 0 {
		sendResponse(conn, "No sessions stored.\n", "", true, 0)
		return
	}

	var output strings.Builder
	output.WriteString(fmt.Sprintf("Recent %d sessions:\n\n", len(memories)))

	for _, mem := range memories {
		snippet := mem.Content
		if len(snippet) > 500 {
			snippet = snippet[:500] + "..."
		}
		snippet = strings.ReplaceAll(snippet, "\n", " ")

		output.WriteString(fmt.Sprintf("[%s] %s\n", mem.ID, mem.Created[:10]))
		output.WriteString(fmt.Sprintf("    %s\n\n", snippet))
	}

	sendResponse(conn, output.String(), "", true, 0)
}

// ============================================================================
// Handler: mode
// ============================================================================

func handleMode(conn net.Conn, args []string) {
	if len(args) < 1 {
		// Interactive selection by default (replaces ~m hotkey behavior)
		handleModeSelect(conn)
		return
	}

	subCmd := args[0]
	switch subCmd {
	case "help":
		handleModeHelp(conn)
	case "list":
		handleModeList(conn)
	case "active":
		handleModeActive(conn)
	case "add":
		handleModeAdd(conn, args[1:])
	case "remove":
		handleModeRemove(conn, args[1:])
	case "clear":
		handleModeClear(conn)
	case "select":
		// Interactive mode selection via fzf
		handleModeSelect(conn)
	default:
		handleModeHelp(conn)
	}
}

func handleModeHelp(conn net.Conn) {
	output := `mpm mode - Mode operations

Usage:
  mpm mode                   Interactive multi-mode selection (TUI, auto-compiles)
  mpm mode list              List available modes
  mpm mode active            Show active modes
  mpm mode add <name>        Add a mode to active list
  mpm mode remove <name>     Remove a mode from active list
  mpm mode clear             Clear all active modes

Examples:
  mpm mode                   # Pick multiple modes, auto-compiles
  mpm mode add developer
  mpm mode remove developer
`
	sendResponse(conn, output, "", true, 0)
}

func handleModeList(conn net.Conn) {
	mm := internal.NewModeManager("")
	modes, err := mm.List()
	if err != nil {
		sendResponse(conn, "", fmt.Sprintf("Failed to list modes: %v", err), true, 1)
		return
	}

	var output strings.Builder
	output.WriteString(fmt.Sprintf("Available modes (%d):\n\n", len(modes)))

	for _, m := range modes {
		name := m.Name
		if name == "" {
			name = m.Title // fallback to title for modes that use title instead of name
		}
		output.WriteString(fmt.Sprintf("  %s\n", name))
		if m.Description != "" {
			output.WriteString(fmt.Sprintf("      %s\n", m.Description))
		}
	}

	sendResponse(conn, output.String(), "", true, 0)
}

func handleModeActive(conn net.Conn) {
	mm := internal.NewModeManager("")
	modes, err := mm.GetActive()
	if err != nil {
		sendResponse(conn, "", fmt.Sprintf("Failed to get active modes: %v", err), true, 1)
		return
	}

	if len(modes) == 0 {
		sendResponse(conn, "No active modes.\n", "", true, 0)
		return
	}

	var output strings.Builder
	output.WriteString("Active modes:\n\n")
	for _, m := range modes {
		output.WriteString(fmt.Sprintf("  %s\n", m))
	}

	sendResponse(conn, output.String(), "", true, 0)
}

func handleModeAdd(conn net.Conn, args []string) {
	if len(args) == 0 {
		sendResponse(conn, "", "Usage: mpm mode add <name>", true, 1)
		return
	}

	name := args[0]
	mm := internal.NewModeManager("")

	err := mm.AddMode(name)
	if err != nil {
		sendResponse(conn, "", fmt.Sprintf("Failed to add mode: %v", err), true, 1)
		return
	}

	// Compile the mode
	_, compileErr := mm.Compile()
	if compileErr != nil {
		sendResponse(conn, fmt.Sprintf("Mode added: %s\n\n", name), fmt.Sprintf("Warning: Auto-compile failed: %v\n", compileErr), true, 0)
		return
	}

	sendResponse(conn, fmt.Sprintf("Mode added: %s (compiled)\n", name), "", true, 0)
}

func handleModeRemove(conn net.Conn, args []string) {
	if len(args) == 0 {
		sendResponse(conn, "", "Usage: mpm mode remove <name>", true, 1)
		return
	}

	name := args[0]
	mm := internal.NewModeManager("")

	err := mm.RemoveMode(name)
	if err != nil {
		sendResponse(conn, "", fmt.Sprintf("Failed to remove mode: %v", err), true, 1)
		return
	}

	sendResponse(conn, fmt.Sprintf("Mode removed: %s\n", name), "", true, 0)
}

func handleModeClear(conn net.Conn) {
	mm := internal.NewModeManager("")

	err := mm.ClearModes()
	if err != nil {
		sendResponse(conn, "", fmt.Sprintf("Failed to clear modes: %v", err), true, 1)
		return
	}

	sendResponse(conn, "All modes cleared.\n", "", true, 0)
}

func handleModeSelect(conn net.Conn) {
	mm := internal.NewModeManager("")
	modes, err := mm.List()
	if err != nil {
		sendResponse(conn, "", fmt.Sprintf("Failed to list modes: %v", err), true, 1)
		return
	}

	if len(modes) == 0 {
		sendResponse(conn, "No modes available.\n", "", true, 0)
		return
	}

	// Get currently active modes
	activeModes, _ := mm.GetActive()
	activeSet := make(map[string]bool)
	for _, m := range activeModes {
		activeSet[m] = true
	}

	// Build selector items
	items := make([]selectorItem, 0, len(modes))
	for _, m := range modes {
		subtitle := ""
		if activeSet[m.Name] {
			subtitle = "[active]"
		}
		items = append(items, selectorItem{name: m.Name, subtitle: subtitle})
	}

	// Run PTY selector (multi-select)
	selected, err := runSelectorPTY(items, true, activeSet)
	if err != nil {
		sendResponse(conn, "", fmt.Sprintf("Selector error: %v", err), true, 1)
		return
	}

	if len(selected) == 0 {
		sendResponse(conn, "No modes selected.\n", "", true, 0)
		return
	}

	// Set new active modes
	err = mm.SetActive(selected)
	if err != nil {
		sendResponse(conn, "", fmt.Sprintf("Failed to set modes: %v", err), true, 1)
		return
	}

	// Auto-compile modes
	compiledCount, compileErr := mm.Compile()
	if compileErr != nil {
		sendResponse(conn, fmt.Sprintf("Active modes updated: %s\n\n", strings.Join(selected, ", ")), fmt.Sprintf("Warning: Auto-compile failed: %v\n", compileErr), true, 0)
		return
	}

	sendResponse(conn, fmt.Sprintf("Active modes updated: %s\nCompiled %d mode(s).\n", strings.Join(selected, ", "), compiledCount), "", true, 0)
}

// ============================================================================
// Handler: persona
// ============================================================================

func handlePersona(conn net.Conn, args []string) {
	if len(args) < 1 {
		// Interactive selection by default (replaces ~p hotkey behavior)
		handlePersonaSelect(conn)
		return
	}

	subCmd := args[0]
	switch subCmd {
	case "help":
		handlePersonaHelp(conn)
	case "list":
		handlePersonaList(conn)
	case "active":
		handlePersonaActive(conn)
	case "set":
		handlePersonaSet(conn, args[1:])
	case "clear":
		handlePersonaClear(conn)
	case "select":
		// Interactive persona selection via fzf
		handlePersonaSelect(conn)
	default:
		handlePersonaHelp(conn)
	}
}

func handlePersonaHelp(conn net.Conn) {
	output := `mpm persona - Persona operations

Usage:
  mpm persona                   Interactive persona selection (TUI, auto-compiles)
  mpm persona list              List available personas
  mpm persona active            Show active persona
  mpm persona set <name>        Set active persona
  mpm persona clear             Clear active persona

Examples:
  mpm persona                   # Pick one persona, auto-compiles
  mpm persona set 808
`
	sendResponse(conn, output, "", true, 0)
}

func handlePersonaList(conn net.Conn) {
	pm := internal.NewPersonaManager("")
	personas, err := pm.List()
	if err != nil {
		sendResponse(conn, "", fmt.Sprintf("Failed to list personas: %v", err), true, 1)
		return
	}

	var output strings.Builder
	output.WriteString(fmt.Sprintf("Available personas (%d):\n\n", len(personas)))

	for _, p := range personas {
		output.WriteString(fmt.Sprintf("  %s\n", p.Name))
		if p.Description != "" {
			output.WriteString(fmt.Sprintf("      %s\n", p.Description))
		}
	}

	sendResponse(conn, output.String(), "", true, 0)
}

func handlePersonaActive(conn net.Conn) {
	pm := internal.NewPersonaManager("")
	persona, err := pm.GetActive()
	if err != nil {
		sendResponse(conn, "", fmt.Sprintf("Failed to get active persona: %v", err), true, 1)
		return
	}

	if persona == "" {
		sendResponse(conn, "No active persona.\n", "", true, 0)
		return
	}

	sendResponse(conn, fmt.Sprintf("Active persona: %s\n", persona), "", true, 0)
}

func handlePersonaSet(conn net.Conn, args []string) {
	if len(args) == 0 {
		sendResponse(conn, "", "Usage: mpm persona set <name>", true, 1)
		return
	}

	name := args[0]
	pm := internal.NewPersonaManager("")

	// Verify persona exists
	_, err := pm.Get(name)
	if err != nil {
		sendResponse(conn, "", fmt.Sprintf("Persona not found: %s\n", name), true, 1)
		return
	}

	err = pm.SetActive(name)
	if err != nil {
		sendResponse(conn, "", fmt.Sprintf("Failed to set persona: %v", err), true, 1)
		return
	}

	// Auto-compile persona
	compiledCount, compileErr := pm.Compile()
	if compileErr != nil {
		sendResponse(conn, fmt.Sprintf("Persona set: %s\n\n", name), fmt.Sprintf("Warning: Auto-compile failed: %v\n", compileErr), true, 0)
		return
	}

	sendResponse(conn, fmt.Sprintf("Persona set: %s\nCompiled %d persona(s).\n", name, compiledCount), "", true, 0)
}

func handlePersonaClear(conn net.Conn) {
	pm := internal.NewPersonaManager("")

	err := pm.SetActive("")
	if err != nil {
		sendResponse(conn, "", fmt.Sprintf("Failed to clear persona: %v", err), true, 1)
		return
	}

	sendResponse(conn, "Persona cleared.\n", "", true, 0)
}

func handlePersonaSelect(conn net.Conn) {
	pm := internal.NewPersonaManager("")
	personas, err := pm.List()
	if err != nil {
		sendResponse(conn, "", fmt.Sprintf("Failed to list personas: %v", err), true, 1)
		return
	}

	if len(personas) == 0 {
		sendResponse(conn, "No personas available.\n", "", true, 0)
		return
	}

	// Get currently active persona
	activePersona, _ := pm.GetActive()
	activeSet := map[string]bool{activePersona: true}

	// Build selector items
	items := make([]selectorItem, 0, len(personas))
	for _, p := range personas {
		subtitle := ""
		if p.Name == activePersona {
			subtitle = "[active]"
		}
		items = append(items, selectorItem{name: p.Name, subtitle: subtitle})
	}

	// Run PTY selector (single-select for persona)
	selected, err := runSelectorPTY(items, false, activeSet)
	if err != nil {
		sendResponse(conn, "", fmt.Sprintf("Selector error: %v", err), true, 1)
		return
	}

	if len(selected) == 0 {
		sendResponse(conn, "No persona selected.\n", "", true, 0)
		return
	}

	// Set new active persona
	err = pm.SetActive(selected[0])
	if err != nil {
		sendResponse(conn, "", fmt.Sprintf("Failed to set persona: %v", err), true, 1)
		return
	}

	// Auto-compile persona
	compiledCount, compileErr := pm.Compile()
	if compileErr != nil {
		sendResponse(conn, fmt.Sprintf("Persona set: %s\n\n", selected[0]), fmt.Sprintf("Warning: Auto-compile failed: %v\n", compileErr), true, 0)
		return
	}

	sendResponse(conn, fmt.Sprintf("Persona set: %s\nCompiled %d persona(s).\n", selected[0], compiledCount), "", true, 0)
}

// ============================================================================
// Handler: llm
// ============================================================================

func handleLlm(conn net.Conn, args []string) {
	if len(args) < 1 {
		handleLlmHelp(conn)
		return
	}

	subCmd := args[0]
	switch subCmd {
	case "help":
		handleLlmHelp(conn)
	case "list":
		handleLlmList(conn)
	case "status":
		handleLlmStatus(conn)
	default:
		handleLlmHelp(conn)
	}
}

func handleLlmHelp(conn net.Conn) {
	output := `mpm llm - LLM operations

Usage:
  mpm llm list                  List available LLM providers
  mpm llm status                Show LLM configuration status

Examples:
  mpm llm list
  mpm llm status
`
	sendResponse(conn, output, "", true, 0)
}

func handleLlmList(conn net.Conn) {
	// List available LLM configurations
	output := `Available LLM providers:

  ollama          Local Ollama instance
  openai          OpenAI API
  anthropic       Anthropic Claude
  groq            Groq API

Configure via environment variables:
  OLLAMA_HOST     Ollama server address
  OPENAI_API_KEY  OpenAI API key
  ANTHROPIC_KEY   Anthropic API key
  GROQ_API_KEY    Groq API key
`
	sendResponse(conn, output, "", true, 0)
}

func handleLlmStatus(conn net.Conn) {
	var output strings.Builder
	output.WriteString("LLM Configuration:\n\n")

	// Check environment variables
	ollama := os.Getenv("OLLAMA_HOST")
	if ollama == "" {
		ollama = "localhost:11434 (default)"
	}
	output.WriteString(fmt.Sprintf("  OLLAMA_HOST:  %s\n", ollama))

	openai := os.Getenv("OPENAI_API_KEY")
	if openai != "" {
		output.WriteString(fmt.Sprintf("  OPENAI_API_KEY: [set] %s...\n", openai[:min(8, len(openai))]))
	} else {
		output.WriteString("  OPENAI_API_KEY: [not set]\n")
	}

	anthropic := os.Getenv("ANTHROPIC_KEY")
	if anthropic != "" {
		output.WriteString(fmt.Sprintf("  ANTHROPIC_KEY:  [set] %s...\n", anthropic[:min(8, len(anthropic))]))
	} else {
		output.WriteString("  ANTHROPIC_KEY:  [not set]\n")
	}

	sendResponse(conn, output.String(), "", true, 0)
}

// ============================================================================
// ============================================================================
// Handler: menu
// ============================================================================

func handleMenu(conn net.Conn) {
	enc := json.NewEncoder(conn)

	// Get watch status
	watching := watchPid != 0

	// Build menu output
	var output strings.Builder
	output.WriteString("\n")
	output.WriteString("╔══════════════════════════════════════════╗\n")
	output.WriteString("║         MPM Control Menu                 ║\n")
	output.WriteString("╠══════════════════════════════════════════╣\n")
	output.WriteString("║                                          ║\n")

	if watching {
		output.WriteString(fmt.Sprintf("║  🟢 Watch Daemon: Running (PID: %d)  ║\n", watchPid))
	} else {
		output.WriteString("║  🔴 Watch Daemon: Stopped                ║\n")
	}

	output.WriteString("║                                          ║\n")
	output.WriteString("║  Commands:                               ║\n")
	output.WriteString("║    mpm watch status   - Check status     ║\n")
	output.WriteString("║    mpm watch start   - Start watcher     ║\n")
	output.WriteString("║    mpm watch stop    - Stop watcher      ║\n")
	output.WriteString("║    mpm watch restart - Restart watcher   ║\n")
	output.WriteString("║                                          ║\n")
	output.WriteString("║    mpm session list   - Saved sessions    ║\n")
	output.WriteString("║    mpm dashboard     - Open TUI          ║\n")
	output.WriteString("║                                          ║\n")
	output.WriteString("╚══════════════════════════════════════════╝\n")
	output.WriteString("\n")

	enc.Encode(Message{
		Output:   output.String(),
		ExitCode: 0,
		Done:     true,
	})
}

// ============================================================================
// Handler: compile
// ============================================================================

func handleCompile(conn net.Conn, args []string) {
	if len(args) < 1 {
		handleCompileHelp(conn)
		return
	}

	target := args[0]
	switch target {
	case "mode":
		handleCompileMode(conn)
	case "persona":
		handleCompilePersona(conn)
	case "all":
		handleCompileAll(conn)
	default:
		handleCompileHelp(conn)
	}
}

func handleCompileHelp(conn net.Conn) {
	output := `mpm compile - Compilation operations

Usage:
  mpm compile mode              Compile all modes
  mpm compile persona           Compile all personas
  mpm compile all               Compile everything

Modes and personas are auto-compiled when changed.
This command forces a recompilation.

Examples:
  mpm compile mode
  mpm compile all
`
	sendResponse(conn, output, "", true, 0)
}

func handleCompileMode(conn net.Conn) {
	mm := internal.NewModeManager("")
	count, err := mm.Compile()
	if err != nil {
		sendResponse(conn, "", fmt.Sprintf("Failed to compile modes: %v", err), true, 1)
		return
	}

	sendResponse(conn, fmt.Sprintf("Compiled %d modes.\n", count), "", true, 0)
}

func handleCompilePersona(conn net.Conn) {
	pm := internal.NewPersonaManager("")
	count, err := pm.Compile()
	if err != nil {
		sendResponse(conn, "", fmt.Sprintf("Failed to compile personas: %v", err), true, 1)
		return
	}

	sendResponse(conn, fmt.Sprintf("Compiled %d personas.\n", count), "", true, 0)
}

func handleCompileAll(conn net.Conn) {
	mm := internal.NewModeManager("")
	pm := internal.NewPersonaManager("")

	modeCount, _ := mm.Compile()
	personaCount, _ := pm.Compile()

	sendResponse(conn, fmt.Sprintf("Compiled %d modes, %d personas.\n", modeCount, personaCount), "", true, 0)
}

// ============================================================================
// Utility Functions
// ============================================================================

func sendResponse(conn net.Conn, output, errMsg string, done bool, exitCode int) {
	resp := Message{
		Output:   output,
		Error:    errMsg,
		Done:     done,
		ExitCode: exitCode,
	}
	enc := json.NewEncoder(conn)
	if err := enc.Encode(resp); err != nil {
		fmt.Fprintf(os.Stderr, "[ERROR] sendResponse failed: %v\n", err)
		return
	}
	// Explicitly flush the encoder
	if flusher, ok := conn.(interface{ Flush() error }); ok {
		flusher.Flush()
	}
}

func getMemoryStore() *internal.MemoryStore {
	paths := internal.DefaultMemoryPaths()
	return internal.NewMemoryStore(filepath.Dir(paths.SQLiteDBPath))
}

func getSessionDir() string {
	paths := internal.DefaultMemoryPaths()
	return paths.SessionSavePath
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// GenerateID creates a unique ID for memories
func generateID() string {
	timestamp := time.Now().UnixNano()
	random := time.Now().UnixNano() % 1000000
	return fmt.Sprintf("%d-%d", timestamp, random)
}

// ============================================================================
// Handler: watch
// ============================================================================

func handleWatch(conn net.Conn, args []string) {
	enc := json.NewEncoder(conn)

	// No subcommand = show status
	if len(args) < 1 || args[0] == "status" {
		status := getWatchStatus()
		enc.Encode(Message{
			Output:   formatWatchStatus(status),
			ExitCode: 0,
			Done:     true,
		})
		return
	}

	subCmd := args[0]
	switch subCmd {
	case "start":
		if err := startWatchDaemon(); err != nil {
			enc.Encode(Message{
				Error:    err.Error(),
				ExitCode: 1,
				Done:     true,
			})
			return
		}
		enc.Encode(Message{
			Output:   fmt.Sprintf("✅ Watch daemon started (PID: %d)\n", watchPid),
			ExitCode: 0,
			Done:     true,
		})

	case "stop":
		if err := stopWatchDaemon(); err != nil {
			enc.Encode(Message{
				Error:    err.Error(),
				ExitCode: 1,
				Done:     true,
			})
			return
		}
		enc.Encode(Message{
			Output:   "✅ Watch daemon stopped\n",
			ExitCode: 0,
			Done:     true,
		})

	case "restart":
		if err := restartWatchDaemon(); err != nil {
			enc.Encode(Message{
				Error:    err.Error(),
				ExitCode: 1,
				Done:     true,
			})
			return
		}
		enc.Encode(Message{
			Output:   fmt.Sprintf("✅ Watch daemon restarted (PID: %d)\n", watchPid),
			ExitCode: 0,
			Done:     true,
		})

	default:
		enc.Encode(Message{
			Error:    fmt.Sprintf("Unknown watch subcommand: %s", subCmd),
			ExitCode: 1,
			Done:     true,
		})
	}
}

func formatWatchStatus(status map[string]interface{}) string {
	running := status["running"].(bool)
	pid := status["pid"].(int)

	if running {
		return fmt.Sprintf("🔍 Watch daemon: running (PID: %d)\n", pid)
	}
	return "🔍 Watch daemon: stopped\n"
}
