package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"mpm/internal"
	"mpm/internal/config"
	"encoding/json"
)


// Package-level singleton DatabaseManager — initialized once per process,
// shared across all handler calls to avoid connection proliferation.
var dbManager *internal.DatabaseManager
var dbManagerInitErr error

// watchPool is the shared WorkerPool used by file watcher and external DB pollers.
// It is initialized by main() in the unified process architecture.
var watchPool *WorkerPool

// Default worker pool size for the watcher background goroutines.
const defaultWorkerPoolSize = 3

// watchPIDFile is the path to the watcher's PID file.
// Written by the detached child process, used by parent stop/status to locate the watcher.
const watchPIDFile = "watch.pid"

// watchPIDPath returns the absolute path to the watch.pid file.
// Uses GetMPMDir() so the PID file lives alongside mpm.db and other runtime data.
func watchPIDPath() string {
	return filepath.Join(config.GetMPMDir(), watchPIDFile)
}

// readWatchPID reads the PID from watch.pid and returns it.
// Returns 0 if the file does not exist or cannot be read.
func readWatchPID() int {
	data, err := os.ReadFile(watchPIDPath())
	if err != nil {
		return 0
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(data)))
	return pid
}

// writeWatchPID writes the current process PID to watch.pid.
func writeWatchPID() error {
	return os.WriteFile(watchPIDPath(), []byte(fmt.Sprintf("%d", os.Getpid())), 0600)
}

// deleteWatchPID removes the watch.pid file.
// Safe to call even if the file does not exist.
func deleteWatchPID() {
	os.Remove(watchPIDPath())
}

// isWatchProcessAlive checks if the process identified by pid is running.
// Uses signal 0 (no actual signal sent) to test process existence.
func isWatchProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// Signal 0 checks if process exists without sending a real signal
	err = proc.Signal(syscall.Signal(0))
	return err == nil
}

// watcherLifecycle tracks the fsnotify watcher goroutine lifecycle.
var (
	watcherCtx    context.Context
	watcherCancel context.CancelFunc
	watcherDone   chan struct{}
)

// respond prints output/error and returns an exit code.
// This replaces the old sendResponse() that wrote JSON over a socket.
func respond(output, errMsg string, exitCode int) int {
	if output != "" {
		fmt.Print(output)
	}
	if errMsg != "" {
		fmt.Fprint(os.Stderr, errMsg)
	}
	return exitCode
}

func handleMemory(args []string) int {
	// Parse subcommand - args[0] is the subcommand (add, list, search, etc.)
	if len(args) < 1 {
		// No subcommand - show help
		return handleMemoryHelp()
	}

	subCmd := args[0]
	switch subCmd {
	case "help":
		return handleMemoryHelp()
	case "add":
		return handleMemoryAdd(args[1:])
	case "search":
		return handleMemorySearch(args[1:])
	case "show":
		return handleMemoryShow(args[1:])
	case "shred":
		return handleMemoryShred(args[1:])
	case "list":
		return handleMemoryList(args[1:])
	case "search-term":
		return handleMemorySearchTerm(args[1:])
	case "wipe":
		return handleMemoryWipe(args[1:])
	default:
		return handleMemoryHelp()
	}
}

func handleMemoryHelp() int {
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
	return respond(output, "", 0)
}

// ============================================================================
// Handler: prime-directives
// ============================================================================

func handlePrimeDirectives() int {
	// Pre-scan for --json since callers may place it after the command
	jsonOutput := false
	for _, arg := range os.Args[1:] {
		if arg == "--json" || arg == "-j" {
			jsonOutput = true
			break
		}
	}

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
		WHERE is_prime_directive = 1
		ORDER BY created_at ASC
	`)
	if err != nil {
		return respond("", fmt.Sprintf("Error querying prime directives: %v\n", err), 1)
	}
	defer rows.Close()

	if jsonOutput {
		type directiveEntry struct {
			ID        string `json:"id"`
			Collection string `json:"collection"`
			Content   string `json:"content"`
			CreatedAt string `json:"created_at"`
		}
		directives := make([]directiveEntry, 0)
		for rows.Next() {
			var id, collection, content, metadata, created string
			if err := rows.Scan(&id, &collection, &content, &metadata, &created); err != nil {
				continue
			}
			directives = append(directives, directiveEntry{
				ID:         id,
				Collection: collection,
				Content:    content,
				CreatedAt:   created,
			})
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
	for rows.Next() {
		var id, collection, content, metadata, created string
		if err := rows.Scan(&id, &collection, &content, &metadata, &created); err != nil {
			continue
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

	if count == 0 {
		output.WriteString("No prime directives found. Run the session that defines them.\n")
	}

	output.WriteString("\xe2\x94\x81\xe2\x95\x90\xe2\x94\x81\xe2\x95\x90\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\n")
	return respond(output.String(), "", 0)
}


func handleMemoryAdd(args []string) int {
	if len(args) == 0 {
		return respond("", "Usage: mpm memory add <content>", 1)
	}

	content := strings.Join(args, " ")
	store := getMemoryStore()
	
	mem, err := store.AddMemory(content, "memories", nil, nil, "", "cli")
	if err != nil {
		return respond("", fmt.Sprintf("Failed to add memory: %v", err), 1)
	}

	return respond(fmt.Sprintf("Memory added with ID: %s\n", mem.ID), "", 0)
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
		
		created := mem.Created
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
		
		created := mem.Created
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

// ============================================================================
// Handler: shred (topic, session, memory - same options)
// ============================================================================

func handleShred(args []string) int {
	if len(args) < 1 {
		return handleShredHelp()
	}

	targetType := args[0]
	
	switch targetType {
	case "sessions":
		return handleShredSessions(args[1:])
	case "memories":
		return handleShredMemories(args[1:])
	case "topics":
		return handleShredTopics(args[1:])
	case "database":
		return handleShredDatabase(args[1:])
	case "modes":
		return handleShredModes(args[1:])
	case "personas":
		return handleShredPersonas(args[1:])
	default:
		// Legacy: single item by ID (topic/session)
		if len(args) < 2 {
			return handleShredHelp()
		}
		id := args[1]
		switch targetType {
		case "topic":
			return handleShredTopic(id)
		case "session":
			return handleShredSession(id)
		default:
			return handleShredHelp()
		}
	}
}

func handleShredHelp() int {
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
	return respond(output, "", 0)
}

func handleShredSessions(args []string) int {
	// Check for --force flag
	force := false
	for _, arg := range args {
		if arg == "--force" || arg == "-f" {
			force = true
		}
	}

	if !force {
		return respond("", "Warning: This will delete ALL sessions. Use 'mpm shred sessions -f' to confirm.\n", 1)
	}

	store := getMemoryStore()
	count, err := store.DeleteAllByCollection("sessions")
	if err != nil {
		return respond("", fmt.Sprintf("Failed to delete sessions: %v", err), 1)
	}

	// Space reclamation happens during maintenance cycle (deferred VACUUM)
	return respond(fmt.Sprintf("All sessions deleted (%d records).\n", count), "", 0)
}

func handleShredMemories(args []string) int {
	// Check for --force flag
	force := false
	for _, arg := range args {
		if arg == "--force" || arg == "-f" {
			force = true
		}
	}

	if !force {
		return respond("", "Warning: This will delete ALL memories. Use 'mpm shred memories -f' to confirm.\n", 1)
	}

	store := getMemoryStore()
	count, err := store.DeleteAllMemories()
	if err != nil {
		return respond("", fmt.Sprintf("Failed to delete memories: %v", err), 1)
	}

	// Space reclamation happens during maintenance cycle (deferred VACUUM)
	return respond(fmt.Sprintf("All memories deleted (%d records).\n", count), "", 0)
}

func handleShredTopics(args []string) int {
	// Check for --force flag
	force := false
	for _, arg := range args {
		if arg == "--force" || arg == "-f" {
			force = true
		}
	}

	if !force {
		return respond("", "Warning: This will delete ALL topics. Use 'mpm shred topics -f' to confirm.\n", 1)
	}

	store := getMemoryStore()
	db := store.DB

	// Delete topic memberships first
	if _, err := db.Exec("DELETE FROM topic_memberships"); err != nil {
		return respond("", fmt.Sprintf("Failed to delete topic memberships: %v", err), 1)
	}

	// Delete topics
	result, err := db.Exec("DELETE FROM topics")
	if err != nil {
		return respond("", fmt.Sprintf("Failed to delete topics: %v", err), 1)
	}

	count, _ := result.RowsAffected()

	// Space reclamation happens during maintenance cycle (deferred VACUUM)
	return respond(fmt.Sprintf("All topics deleted (%d records).\n", count), "", 0)
}

func handleShredDatabase(args []string) int {
	// Check for --force flag
	force := false
	for _, arg := range args {
		if arg == "--force" || arg == "-f" {
			force = true
		}
	}

	if !force {
		return respond("", "Warning: This will delete the entire database and create a new one. Use 'mpm shred database -f' to confirm.\n", 1)
	}

	paths := internal.DefaultMemoryPaths()
	dbPath := paths.SQLiteDBPath

	// Close any open connections
	store := getMemoryStore()
	if store == nil || store.DB == nil {
		return respond("", "Database not initialized\n", 1)
	}
	if err := store.DB.Close(); err != nil {
		return respond("", fmt.Sprintf("Failed to close database: %v", err), 1)
	}

	// Remove the database file
	if err := os.Remove(dbPath); err != nil {
		return respond("", fmt.Sprintf("Failed to remove database file: %v", err), 1)
	}

	// Recreate the database
	newStore := internal.NewMemoryStore(filepath.Dir(dbPath))
	if newStore.DB == nil {
		return respond("", "Failed to recreate database", 1)
	}

	// Close the new store
	if err := newStore.DB.Close(); err != nil {
		return respond("", fmt.Sprintf("Failed to close new database: %v", err), 1)
	}

	return respond(fmt.Sprintf("Database recreated at: %s\n", dbPath), "", 0)
}

func handleShredModes(args []string) int {
	// Check for --force flag
	force := false
	for _, arg := range args {
		if arg == "--force" || arg == "-f" {
			force = true
		}
	}

	if !force {
		return respond("", "Warning: This will delete ALL modes. Use 'mpm shred modes -f' to confirm.\n", 1)
	}

	mm := internal.NewModeManager("")
	count, err := mm.RemoveAll()
	if err != nil {
		return respond("", fmt.Sprintf("Failed to delete modes: %v", err), 1)
	}

	return respond(fmt.Sprintf("All modes deleted (%d records).\n", count), "", 0)
}

func handleShredPersonas(args []string) int {
	// Check for --force flag
	force := false
	for _, arg := range args {
		if arg == "--force" || arg == "-f" {
			force = true
		}
	}

	if !force {
		return respond("", "Warning: This will delete ALL personas. Use 'mpm shred personas -f' to confirm.\n", 1)
	}

	pm := internal.NewPersonaManager("")
	count, err := pm.RemoveAll()
	if err != nil {
		return respond("", fmt.Sprintf("Failed to delete personas: %v", err), 1)
	}

	return respond(fmt.Sprintf("All personas deleted (%d records).\n", count), "", 0)
}

func handleShredTopic(id string) int {
	store := getMemoryStore()

	// Get the topic first to verify it exists
	topic, err := store.SearchTopics(id, 1)
	if err != nil || len(topic) == 0 {
		return respond("", fmt.Sprintf("Topic not found: %s\n", id), 1)
	}

	// Use the internal ShredTopic function via DeleteByID
	db := store.DB
	tx, err := db.Begin()
	if err != nil {
		return respond("", fmt.Sprintf("Failed to shred topic: %v", err), 1)
	}
	defer tx.Rollback()

	// Delete memberships first
	if _, err = tx.Exec("DELETE FROM topic_memberships WHERE topic_id = ?", id); err != nil {
		return respond("", fmt.Sprintf("Failed to shred topic: %v", err), 1)
	}

	// Delete the topic
	if _, err = tx.Exec("DELETE FROM topics WHERE id = ?", id); err != nil {
		return respond("", fmt.Sprintf("Failed to shred topic: %v", err), 1)
	}

	if err := tx.Commit(); err != nil {
		return respond("", fmt.Sprintf("Failed to shred topic: %v", err), 1)
	}

	// Space reclamation happens during maintenance cycle (deferred VACUUM)
	return respond(fmt.Sprintf("Topic shredded: %s\n", id), "", 0)
}

func handleShredSession(id string) int {
	store := getMemoryStore()

	err := store.DeleteMemory(id, "sessions")
	if err != nil {
		return respond("", fmt.Sprintf("Failed to shred session: %v", err), 1)
	}

	return respond(fmt.Sprintf("Session shredded: %s\n", id), "", 0)
}

// ============================================================================
// Handler: topic
// ============================================================================

func handleTopic(args []string) int {
	if len(args) < 1 {
		return handleTopicHelp()
	}

	subCmd := args[0]
	switch subCmd {
	case "help":
		return handleTopicHelp()
	case "add":
		return handleTopicAdd(args[1:])
	case "search":
		return handleTopicSearch(args[1:])
	case "show":
		return handleTopicShow(args[1:])
	case "promote":
		return handleTopicPromote(args[1:])
	case "shred":
		if len(args) < 2 {
			return handleTopicHelp()
		}
		return handleShredTopic(args[1])
	case "list":
		return handleTopicList(args[1:])
	default:
		return handleTopicHelp()
	}
}

func handleTopicHelp() int {
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
	return respond(output, "", 0)
}

func handleTopicAdd(args []string) int {
	if len(args) == 0 {
		return respond("", "Usage: mpm topic add <name> [description] [--json]", 1)
	}

	name := args[0]
	description := ""
	jsonOutput := false

	// Pre-scan for --json
	for _, arg := range args[1:] {
		if arg == "--json" || arg == "-j" {
			jsonOutput = true
		} else if !strings.HasPrefix(arg, "-") {
			description = arg
		}
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
		if jsonOutput {
			data, _ := json.Marshal(map[string]interface{}{"success": false, "error": fmt.Sprintf("Failed to add topic: %v", err)})
			fmt.Println(string(data))
		} else {
			respond("", fmt.Sprintf("Failed to add topic: %v", err), 1)
		}
		return 1
	}

	if jsonOutput {
		data, _ := json.Marshal(map[string]interface{}{
			"success":     true,
			"id":         id,
			"name":       name,
			"description": description,
		})
		fmt.Println(string(data))
	} else {
		respond(fmt.Sprintf("Topic added: %s\n", name), "", 0)
	}
	return 0
}

func handleTopicSearch(args []string) int {
	if len(args) == 0 {
		return respond("", "Usage: mpm topic search <query> [--json]", 1)
	}

	query := strings.Join(args, " ")
	jsonOutput := false
	if args[len(args)-1] == "--json" || args[len(args)-1] == "-j" {
		jsonOutput = true
		args = args[:len(args)-1]
		query = strings.Join(args, " ")
	}

	store := getMemoryStore()

	results, err := store.SearchTopics(query, 20)
	if err != nil {
		if jsonOutput {
			data, _ := json.Marshal(map[string]interface{}{"query": query, "results": []interface{}{}, "error": fmt.Sprintf("Search failed: %v", err)})
			fmt.Println(string(data))
		} else {
			respond("", fmt.Sprintf("Search failed: %v", err), 1)
		}
		return 1
	}

	if len(results) == 0 {
		if jsonOutput {
			data, _ := json.Marshal(map[string]interface{}{"query": query, "results": []interface{}{}, "message": "No topics found"})
			fmt.Println(string(data))
		} else {
			respond("No topics found.\n", "", 0)
		}
		return 0
	}

	if jsonOutput {
		type topicResult struct {
			ID          string `json:"id"`
			Name        string `json:"name"`
			Description string `json:"description"`
			CreatedAt   string `json:"created_at"`
		}
		result := make([]topicResult, 0, len(results))
		for _, r := range results {
			result = append(result, topicResult{
				ID:          r.ID,
				Name:        r.Snippet,
				Description: r.Snippet,
				CreatedAt:   r.Created,
			})
		}
		data, _ := json.Marshal(map[string]interface{}{"query": query, "results": result})
		fmt.Println(string(data))
		return 0
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

	return respond(output.String(), "", 0)
}

func handleTopicShow(args []string) int {
	if len(args) == 0 {
		return respond("", "Usage: mpm topic show <id> [--json]", 1)
	}

	id := args[0]
	jsonOutput := false
	if args[len(args)-1] == "--json" || args[len(args)-1] == "-j" {
		jsonOutput = true
	}

	store := getMemoryStore()
	db := store.DB

	var name, description, created string
	err := db.QueryRow(`
		SELECT name, description, created_at FROM topics WHERE id = ?
	`, id).Scan(&name, &description, &created)

	if err != nil {
		if jsonOutput {
			data, _ := json.Marshal(map[string]interface{}{"success": false, "error": "Topic not found: " + id})
			fmt.Println(string(data))
		} else {
			respond("", fmt.Sprintf("Topic not found: %s\n", id), 1)
		}
		return 1
	}

	if jsonOutput {
		// Get memory IDs for this topic
		rows, _ := db.Query("SELECT memory_id FROM topic_memberships WHERE topic_id = ?", id)
		memoryIDs := []string{}
		for rows.Next() {
			var mid string
			rows.Scan(&mid)
			memoryIDs = append(memoryIDs, mid)
		}
		rows.Close()
		chunkCount := len(memoryIDs)
		data, _ := json.Marshal(map[string]interface{}{
			"id":           id,
			"name":         name,
			"description":  description,
			"created_at":   created,
			"memory_ids":   memoryIDs,
			"chunk_count":  chunkCount,
		})
		fmt.Println(string(data))
		return 0
	}

	var output strings.Builder
	output.WriteString(fmt.Sprintf("ID:          %s\n", id))
	output.WriteString(fmt.Sprintf("Name:        %s\n", name))
	output.WriteString(fmt.Sprintf("Created:     %s\n", created))
	if description != "" {
		output.WriteString(fmt.Sprintf("\nDescription:\n%s\n", description))
	}

	return respond(output.String(), "", 0)
}

func handleTopicPromote(args []string) int {
	if len(args) == 0 {
		return respond("", "Usage: mpm topic promote <id>", 1)
	}

	id := args[0]
	store := getMemoryStore()

	err := store.PromoteTopicToMemory(id, "memories", nil)
	if err != nil {
		return respond("", fmt.Sprintf("Failed to promote topic: %v", err), 1)
	}

	return respond(fmt.Sprintf("Topic promoted to memory: %s\n", id), "", 0)
}

func handleTopicList(args []string) int {
	jsonOutput := false
	for _, arg := range args {
		if arg == "--json" || arg == "-j" {
			jsonOutput = true
		}
	}

	store := getMemoryStore()
	db := store.DB

	rows, err := db.Query(`
		SELECT id, name, description, created_at FROM topics
		ORDER BY created_at DESC LIMIT 20
	`)
	if err != nil {
		return respond("", fmt.Sprintf("Failed to list topics: %v", err), 1)
	}
	defer rows.Close()

	if jsonOutput {
		type topicEntry struct {
			ID          string `json:"id"`
			Name        string `json:"name"`
			Description string `json:"description"`
			CreatedAt   string `json:"created_at"`
		}
		result := make([]topicEntry, 0)
		for rows.Next() {
			var id, name, description, created string
			if err := rows.Scan(&id, &name, &description, &created); err != nil {
				continue
			}
			result = append(result, topicEntry{
				ID:          id,
				Name:        name,
				Description: description,
				CreatedAt:   created,
			})
		}
		data, _ := json.Marshal(map[string]interface{}{"topics": result})
		fmt.Println(string(data))
		return 0
	}

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

	return respond(output.String(), "", 0)
}

func handleSession(args []string) int {
	if len(args) < 1 {
		return handleSessionHelp()
	}

	subCmd := args[0]
	switch subCmd {
	case "help":
		return handleSessionHelp()
	case "add":
		return handleSessionAdd(args[1:])
	case "search":
		return handleSessionSearch(args[1:])
	case "show":
		return handleSessionShow(args[1:])
	case "shred":
		if len(args) < 2 {
			return handleSessionHelp()
		}
		return handleShredSession(args[1])
	case "list":
		return handleSessionList(args[1:])
	default:
		return handleSessionHelp()
	}
}

func handleSessionHelp() int {
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
	return respond(output, "", 0)
}

func handleSessionAdd(args []string) int {
	if len(args) == 0 {
		return respond("", "Usage: mpm session add <content>", 1)
	}

	content := strings.Join(args, " ")
	store := getMemoryStore()
	
	// Add to session collection
	mem, err := store.AddMemory(content, "session", nil, nil, "", "cli")
	if err != nil {
		return respond("", fmt.Sprintf("Failed to add session: %v", err), 1)
	}

	return respond(fmt.Sprintf("Session added with ID: %s\n", mem.ID), "", 0)
}

func handleSessionSearch(args []string) int {
	if len(args) == 0 {
		return respond("", "Usage: mpm session search <query>", 1)
	}

	query := strings.Join(args, " ")
	store := getMemoryStore()

	memories, err := store.SearchSessions(query, 20)
	if err != nil {
		return respond("", fmt.Sprintf("Search failed: %v", err), 1)
	}

	if len(memories) == 0 {
		return respond("No sessions found.\n", "", 0)
	}

	var output strings.Builder
	output.WriteString(fmt.Sprintf("Found %d sessions:\n\n", len(memories)))

	for _, mem := range memories {
		snippet := mem.Content
		if len(snippet) > 500 {
			snippet = snippet[:500] + "..."
		}
		snippet = strings.ReplaceAll(snippet, "\n", " ")

		output.WriteString(fmt.Sprintf("[%s] %s\n", mem.ID, datePrefix(mem.Created)))
		output.WriteString(fmt.Sprintf("    %s\n\n", snippet))
	}

	return respond(output.String(), "", 0)
}

func handleSessionShow(args []string) int {
	if len(args) == 0 {
		return respond("", "Usage: mpm session show <id>", 1)
	}

	id := args[0]
	store := getMemoryStore()

	mem, err := store.GetByID(id, "session")
	if err != nil || mem == nil {
		return respond("", fmt.Sprintf("Session not found: %s\n", id), 1)
	}

	var output strings.Builder
	output.WriteString(fmt.Sprintf("ID:      %s\n", mem.ID))
	output.WriteString(fmt.Sprintf("Created: %s\n\n", mem.Created))
	output.WriteString(mem.Content)
	output.WriteString("\n")

	return respond(output.String(), "", 0)
}

func handleSessionList(args []string) int {
	store := getSessionStore()

	sessions, err := store.GetRecentSessions(20)
	if err != nil {
		return respond("", fmt.Sprintf("Failed to list sessions: %v", err), 1)
	}

	if len(sessions) == 0 {
		return respond("No sessions stored.\n", "", 0)
	}

	var output strings.Builder
	output.WriteString(fmt.Sprintf("Recent %d sessions:\n\n", len(sessions)))

	for _, sess := range sessions {
		snippet := sess.Content
		if len(snippet) > 500 {
			snippet = snippet[:500] + "..."
		}
		snippet = strings.ReplaceAll(snippet, "\n", " ")

		output.WriteString(fmt.Sprintf("[%s] %s\n", sess.ID, datePrefix(sess.Created)))
		output.WriteString(fmt.Sprintf("    %s\n\n", snippet))
	}

	return respond(output.String(), "", 0)
}

// ============================================================================
// Handler: reference
// ============================================================================

func handleReference(args []string) int {
	if len(args) < 1 {
		return handleReferenceHelp()
	}

	subCmd := args[0]
	switch subCmd {
	case "help":
		return handleReferenceHelp()
	case "list":
		return handleReferenceList()
	case "add":
		return handleReferenceAdd(args[1:])
	case "search":
		return handleReferenceSearch(args[1:])
	case "get":
		return handleReferenceGet(args[1:])
	case "shred":
		return handleReferenceShred(args[1:])
	case "scan":
		return handleReferenceScan()
	default:
		return handleReferenceHelp()
	}
}

func handleReferenceHelp() int {
	output := `mpm reference - Reference library

Usage:
  mpm reference add <file>    Ingest a document (PDF/EPUB/md/txt)
  mpm reference list           List all documents
  mpm reference search <query> Search document content
  mpm reference get <id>      Show full document
  mpm reference shred <id>     Remove a document
  mpm reference scan           Scan reference dir and ingest all

Examples:
  mpm reference add book.pdf
  mpm reference list
  mpm reference search "machiavelli"
  mpm reference shred abc123
`
	return respond(output, "", 0)
}

func handleReferenceList() int {
	store := getReferenceStore()
	refs := store.List()

	if len(refs) == 0 {
		return respond("No references stored. Add some with: mpm reference add <file>\n", "", 0)
	}

	var output strings.Builder
	output.WriteString(fmt.Sprintf("References (%d):\n\n", len(refs)))

	for _, ref := range refs {
		output.WriteString(fmt.Sprintf("[%s] %s\n", ref.ID, ref.Title))
		created := ref.Created
		if len(created) >= 10 {
			created = created[:10]
		}
		output.WriteString(fmt.Sprintf("    %s\n", created))
		if len(ref.Tags) > 0 {
			output.WriteString(fmt.Sprintf("    Tags: %s\n", strings.Join(ref.Tags, ", ")))
		}
		output.WriteString("\n")
	}

	return respond(output.String(), "", 0)
}

func handleReferenceAdd(args []string) int {
	if len(args) == 0 {
		return respond("", "Usage: mpm reference add <file>\n", 1)
	}

	filePath := args[0]
	if _, err := os.Stat(filePath); os.IsNotExist(err) {
		return respond("", fmt.Sprintf("File not found: %s\n", filePath), 1)
	}

	// Parse file based on extension
	ext := strings.ToLower(filepath.Ext(filePath))
	var content string
	var parseErr error

	switch ext {
	case ".pdf":
		content, parseErr = internal.ParsePDF(filePath)
	case ".epub":
		content, parseErr = internal.ParseEPUB(filePath)
	case ".txt", ".md":
		data, err := os.ReadFile(filePath)
		if err != nil {
			return respond("", fmt.Sprintf("Failed to read file: %v\n", err), 1)
		}
		content = string(data)
	default:
		// Try as plain text
		data, err := os.ReadFile(filePath)
		if err != nil {
			return respond("", fmt.Sprintf("Unsupported file type: %s\n", ext), 1)
		}
		content = string(data)
	}

	if parseErr != nil {
		return respond("", fmt.Sprintf("Failed to parse file: %v\n", parseErr), 1)
	}

	title := filepath.Base(filePath)
	store := getReferenceStore()

	// Use the ReferenceStore.Add which writes to JSON
	_, err := store.Add(title, filePath, nil, content)
	if err != nil {
		return respond("", fmt.Sprintf("Failed to add reference: %v\n", err), 1)
	}

	return respond(fmt.Sprintf("Reference added: %s\n", title), "", 0)
}

func handleReferenceSearch(args []string) int {
	if len(args) == 0 {
		return respond("", "Usage: mpm reference search <query>\n", 1)
	}

	query := strings.Join(args, " ")
	store := getReferenceStore()
	results := store.Search(query)

	if len(results) == 0 {
		return respond(fmt.Sprintf("No references found matching: %s\n", query), "", 0)
	}

	var output strings.Builder
	output.WriteString(fmt.Sprintf("Found %d references matching \"%s\":\n\n", len(results), query))

	for _, ref := range results {
		output.WriteString(fmt.Sprintf("[%s] %s\n", ref.ID, ref.Title))
		snippet := ref.Content
		if len(snippet) > 200 {
			snippet = snippet[:200] + "..."
		}
		output.WriteString(fmt.Sprintf("    %s\n\n", strings.ReplaceAll(snippet, "\n", " ")))
	}

	return respond(output.String(), "", 0)
}

func handleReferenceGet(args []string) int {
	if len(args) == 0 {
		return respond("", "Usage: mpm reference get <id>\n", 1)
	}

	id := args[0]
	store := getReferenceStore()

	ref, err := store.GetByID(id)
	if err != nil {
		return respond("", fmt.Sprintf("Reference not found: %s\n", id), 1)
	}

	var output strings.Builder
	output.WriteString(fmt.Sprintf("[%s] %s\n", ref.ID, ref.Title))
	output.WriteString(fmt.Sprintf("Created: %s\n", ref.Created))
	if len(ref.Tags) > 0 {
		output.WriteString(fmt.Sprintf("Tags: %s\n", strings.Join(ref.Tags, ", ")))
	}
	output.WriteString(fmt.Sprintf("\n%s\n", ref.Content))

	return respond(output.String(), "", 0)
}

func handleReferenceShred(args []string) int {
	if len(args) == 0 {
		return respond("", "Usage: mpm reference shred <id>\n", 1)
	}

	id := args[0]
	store := getReferenceStore()

	if err := store.Remove(id); err != nil {
		return respond("", fmt.Sprintf("Failed to remove reference: %v\n", err), 1)
	}

	return respond(fmt.Sprintf("Reference removed: %s\n", id), "", 0)
}

func handleReferenceScan() int {
	store := getReferenceStore()
	paths := internal.DefaultMemoryPaths()
	refDir := filepath.Join(paths.SessionSavePath, "reference")

	if _, err := os.Stat(refDir); os.IsNotExist(err) {
		return respond("No reference directory found. Create it and add files, then run scan.\n", "", 0)
	}

	entries, err := os.ReadDir(refDir)
	if err != nil {
		return respond("", fmt.Sprintf("Failed to read reference directory: %v\n", err), 1)
	}

	var output strings.Builder
	count := 0

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		filePath := filepath.Join(refDir, entry.Name())
		ext := strings.ToLower(filepath.Ext(filePath))

		if ext != ".pdf" && ext != ".epub" && ext != ".txt" && ext != ".md" {
			continue
		}

		// Parse file
		var content string
		var parseErr error

		switch ext {
		case ".pdf":
			content, parseErr = internal.ParsePDF(filePath)
		case ".epub":
			content, parseErr = internal.ParseEPUB(filePath)
		case ".txt", ".md":
			data, err := os.ReadFile(filePath)
			if err != nil {
				continue
			}
			content = string(data)
		default:
			continue
		}

		if parseErr != nil {
			output.WriteString(fmt.Sprintf("Skipped (parse error): %s\n", entry.Name()))
			continue
		}

		title := entry.Name()
		_, err = store.Add(title, filePath, nil, content)
		if err != nil {
			output.WriteString(fmt.Sprintf("Failed: %s - %v\n", entry.Name(), err))
			continue
		}

		count++
	}

	output.WriteString(fmt.Sprintf("\nScanned %d references from %s\n", count, refDir))
	return respond(output.String(), "", 0)
}

// ============================================================================
// Handler: mode
// ============================================================================

func handleMode(args []string) int {
	if len(args) < 1 {
		return handleModeSelect()
	}

	subCmd := args[0]
	switch subCmd {
	case "help":
		return handleModeHelp()
	case "list":
		return handleModeList()
	case "active":
		return handleModeActive()
	case "add":
		return handleModeAdd(args[1:])
	case "remove":
		return handleModeRemove(args[1:])
	case "clear":
		return handleModeClear()
	case "select":
		// Interactive mode selection via fzf
		return handleModeSelect()
	default:
		return handleModeHelp()
	}
}

func handleModeHelp() int {
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
	return respond(output, "", 0)
}

func handleModeList() int {
	mm := internal.NewModeManager("")
	modes, err := mm.List()
	if err != nil {
		return respond("", fmt.Sprintf("Failed to list modes: %v", err), 1)
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

	return respond(output.String(), "", 0)
}

func handleModeActive() int {
	mm := internal.NewModeManager("")
	modes, err := mm.GetActive()
	if err != nil {
		return respond("", fmt.Sprintf("Failed to get active modes: %v", err), 1)
	}

	if len(modes) == 0 {
		return respond("No active modes.\n", "", 0)
	}

	var output strings.Builder
	output.WriteString("Active modes:\n\n")
	for _, m := range modes {
		output.WriteString(fmt.Sprintf("  %s\n", m))
	}

	return respond(output.String(), "", 0)
}

func handleModeAdd(args []string) int {
	if len(args) == 0 {
		return respond("", "Usage: mpm mode add <name>", 1)
	}

	name := args[0]
	mm := internal.NewModeManager("")

	err := mm.AddMode(name)
	if err != nil {
		return respond("", fmt.Sprintf("Failed to add mode: %v", err), 1)
	}
	return respond(fmt.Sprintf("Mode added: %s\n", name), "", 0)
}

func handleModeRemove(args []string) int {
	if len(args) == 0 {
		return respond("", "Usage: mpm mode remove <name>", 1)
	}

	name := args[0]
	mm := internal.NewModeManager("")

	err := mm.RemoveMode(name)
	if err != nil {
		return respond("", fmt.Sprintf("Failed to remove mode: %v", err), 1)
	}

	return respond(fmt.Sprintf("Mode removed: %s\n", name), "", 0)
}

func handleModeClear() int {
	mm := internal.NewModeManager("")

	err := mm.ClearModes()
	if err != nil {
		return respond("", fmt.Sprintf("Failed to clear modes: %v", err), 1)
	}

	return respond("All modes cleared.\n", "", 0)
}

func handleModeSelect() int {
	mm := internal.NewModeManager("")
	modes, err := mm.List()
	if err != nil {
		return respond("", fmt.Sprintf("Failed to list modes: %v", err), 1)
	}

	if len(modes) == 0 {
		return respond("No modes available.\n", "", 0)
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
		return respond("", fmt.Sprintf("Selector error: %v", err), 1)
	}

	if len(selected) == 0 {
		return respond("No modes selected.\n", "", 0)
	}

	// Set new active modes
	err = mm.SetActive(selected)
	if err != nil {
		return respond("", fmt.Sprintf("Failed to set modes: %v", err), 1)
	}
	return respond(fmt.Sprintf("Active modes updated: %s\n", strings.Join(selected, ", ")), "", 0)

}

// ============================================================================
// Handler: persona
// ============================================================================

func handlePersona(args []string) int {
	if len(args) < 1 {
		return handlePersonaSelect()
	}

	subCmd := args[0]
	switch subCmd {
	case "help":
		return handlePersonaHelp()
	case "list":
		return handlePersonaList()
	case "active":
		return handlePersonaActive()
	case "set":
		return handlePersonaSet(args[1:])
	case "clear":
		return handlePersonaClear()
	case "select":
		// Interactive persona selection via fzf
		return handlePersonaSelect()
	default:
		return handlePersonaHelp()
	}
}

func handlePersonaHelp() int {
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
	return respond(output, "", 0)
}

func handlePersonaList() int {
	pm := internal.NewPersonaManager("")
	personas, err := pm.List()
	if err != nil {
		return respond("", fmt.Sprintf("Failed to list personas: %v", err), 1)
	}

	var output strings.Builder
	output.WriteString(fmt.Sprintf("Available personas (%d):\n\n", len(personas)))

	for _, p := range personas {
		output.WriteString(fmt.Sprintf("  %s\n", p.Name))
		if p.Description != "" {
			output.WriteString(fmt.Sprintf("      %s\n", p.Description))
		}
	}

	return respond(output.String(), "", 0)
}

func handlePersonaActive() int {
	pm := internal.NewPersonaManager("")
	persona, err := pm.GetActive()
	if err != nil {
		return respond("", fmt.Sprintf("Failed to get active persona: %v", err), 1)
	}

	if persona == "" {
		return respond("No active persona.\n", "", 0)
	}

	return respond(fmt.Sprintf("Active persona: %s\n", persona), "", 0)
}

func handlePersonaSet(args []string) int {
	if len(args) == 0 {
		return respond("", "Usage: mpm persona set <name>", 1)
	}

	name := args[0]
	pm := internal.NewPersonaManager("")

	// Verify persona exists
	_, err := pm.Get(name)
	if err != nil {
		return respond("", fmt.Sprintf("Persona not found: %s\n", name), 1)
	}

	err = pm.SetActive(name)
	if err != nil {
		return respond("", fmt.Sprintf("Failed to set persona: %v", err), 1)
	}
	return respond(fmt.Sprintf("Persona set: %s\n", name), "", 0)
}

func handlePersonaClear() int {
	pm := internal.NewPersonaManager("")

	err := pm.SetActive("")
	if err != nil {
		return respond("", fmt.Sprintf("Failed to clear persona: %v", err), 1)
	}

	return respond("Persona cleared.\n", "", 0)
}

func handlePersonaSelect() int {
	pm := internal.NewPersonaManager("")
	personas, err := pm.List()
	if err != nil {
		return respond("", fmt.Sprintf("Failed to list personas: %v", err), 1)
	}

	if len(personas) == 0 {
		return respond("No personas available.\n", "", 0)
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
		return respond("", fmt.Sprintf("Selector error: %v", err), 1)
	}

	if len(selected) == 0 {
		return respond("No persona selected.\n", "", 0)
	}

	// Set new active persona
	err = pm.SetActive(selected[0])
	if err != nil {
		return respond("", fmt.Sprintf("Failed to set persona: %v", err), 1)
	}
	return respond(fmt.Sprintf("Persona set: %s\n", selected[0]), "", 0)

}

// ============================================================================
// Handler: llm
// ============================================================================

// ============================================================================
// Handler: lesson
// ============================================================================

func handleLesson(args []string) int {
	if len(args) < 1 {
		return handleLessonHelp()
	}

	subCmd := args[0]
	switch subCmd {
	case "help":
		return handleLessonHelp()
	case "add":
		return handleLessonAdd(args[1:])
	case "list":
		return handleLessonList(args[1:])
	case "search":
		return handleLessonSearch(args[1:])
	case "get":
		return handleLessonGet(args[1:])
	case "shred":
		return handleLessonShred(args[1:])
	case "stats":
		return handleLessonStats()
	default:
		return handleLessonHelp()
	}
}

func handleLessonHelp() int {
	output := `mpm lesson - Lesson operations

Usage:
  mpm lesson add <content> [--type warning|practice|insight] [--tags tags]
  mpm lesson list [--type warning|practice|insight]
  mpm lesson search <query>
  mpm lesson get <id>
  mpm lesson shred <id>
  mpm lesson stats

Examples:
  mpm lesson add "Check file extensions before executing" --type warning --tags safety,files
  mpm lesson add "Use gofmt for Go code formatting" --type practice --tags go,style
  mpm lesson list
  mpm lesson list --type warning
  mpm lesson search "safety"
  mpm lesson get abc123
  mpm lesson shred abc123
  mpm lesson stats

Lesson types:
  warning  - "don't do X" (negative lessons)
  practice - "do Y" (positive lessons, best practices)
  insight  - "X leads to Y" (causal knowledge, default)
`
	return respond(output, "", 0)
}

func handleLessonAdd(args []string) int {
	if len(args) == 0 {
		return respond("", "Usage: mpm lesson add <content> [--type warning|practice|insight] [--tags tags] [--json]", 1)
	}

	content := ""
	lessonType := "insight"
	var tags []string
	jsonOutput := false

	// Pre-scan for --json since callers may place it after the content
	filteredArgs := []string{}
	for _, arg := range args {
		if arg == "--json" || arg == "-j" {
			jsonOutput = true
		} else {
			filteredArgs = append(filteredArgs, arg)
		}
	}
	args = filteredArgs

	i := 0
	for i < len(args) {
		switch args[i] {
		case "--type":
			if i+1 < len(args) {
				lessonType = args[i+1]
				i += 2
			} else {
				i++
			}
		case "--tags":
			if i+1 < len(args) {
				tags = strings.Split(args[i+1], ",")
				i += 2
			} else {
				i++
			}
		default:
			// First non-flag arg is the content; consume it and stop
			content = args[i]
			// Check if there are more args that aren't flags
			i++
			for i < len(args) && !strings.HasPrefix(args[i], "--") {
				content += " " + args[i]
				i++
			}
			// Skip any remaining flags
			for i < len(args) {
				i++
			}
			break
		}
	}

	if content == "" {
		return respond("", "Usage: mpm lesson add <content>", 1)
	}

	lessonStore, err := internal.NewLessonStore("")
	if err != nil {
		if jsonOutput {
			data, _ := json.Marshal(map[string]interface{}{"success": false, "error": fmt.Sprintf("Failed to initialize lesson store: %v", err)})
			fmt.Println(string(data))
		} else {
			respond("", fmt.Sprintf("Failed to initialize lesson store: %v", err), 1)
		}
		return 1
	}
	lesson, err := lessonStore.AddLesson(content, internal.LessonType(lessonType), tags, "")
	if err != nil {
		if jsonOutput {
			data, _ := json.Marshal(map[string]interface{}{"success": false, "error": fmt.Sprintf("Failed to add lesson: %v", err)})
			fmt.Println(string(data))
		} else {
			respond("", fmt.Sprintf("Failed to add lesson: %v", err), 1)
		}
		return 1
	}

	if jsonOutput {
		data, _ := json.Marshal(map[string]interface{}{
			"success":       true,
			"id":           lesson.ID,
			"type":         string(lesson.Type),
			"reinforcement": lesson.ReinforcementCount,
		})
		fmt.Println(string(data))
	} else {
		respond(fmt.Sprintf("Lesson added with ID: %s (reinforcement: %d)\n", lesson.ID, lesson.ReinforcementCount), "", 0)
	}
	return 0
}

func handleLessonList(args []string) int {
	lessonType := ""
	jsonOutput := false
	for _, arg := range args {
		if strings.HasPrefix(arg, "--type=") {
			lessonType = strings.TrimPrefix(arg, "--type=")
		}
		if arg == "--json" || arg == "-j" {
			jsonOutput = true
		}
	}

	lessonStore, storeErr := internal.NewLessonStore("")
	if storeErr != nil {
		return respond("", fmt.Sprintf("Failed to initialize lesson store: %v", storeErr), 1)
	}
	lessons, listErr := lessonStore.ListLessons(internal.LessonType(lessonType))
	if listErr != nil {
		return respond("", fmt.Sprintf("Failed to list lessons: %v", listErr), 1)
	}

	if len(lessons) == 0 {
		if jsonOutput {
			fmt.Println(`{"lessons": [], "message": "No lessons stored"}`)
		} else {
			respond("No lessons stored.\n", "", 0)
		}
		return 0
	}

	if jsonOutput {
		type lessonEntry struct {
			ID        string   `json:"id"`
			Type      string   `json:"type"`
			Content   string   `json:"content"`
			Tags      []string `json:"tags"`
			CreatedAt string   `json:"created_at"`
		}
		result := make([]lessonEntry, 0, len(lessons))
		for _, l := range lessons {
			result = append(result, lessonEntry{
				ID:        l.ID,
				Type:      string(l.Type),
				Content:   l.Content,
				Tags:      l.Tags,
				CreatedAt: l.Created,
			})
		}
		data, _ := json.Marshal(map[string]interface{}{"lessons": result})
		fmt.Println(string(data))
		return 0
	}

	var output strings.Builder
	output.WriteString(fmt.Sprintf("Lessons (%d):\n\n", len(lessons)))
	for _, l := range lessons {
		typeLabel := string(l.Type)
		output.WriteString(fmt.Sprintf("[%s] [%s] %s\n", l.ID, typeLabel, l.Created[:10]))
		snippet := l.Content
		if len(snippet) > 100 {
			snippet = snippet[:100] + "..."
		}
		output.WriteString(fmt.Sprintf("    %s\n\n", snippet))
	}

	return respond(output.String(), "", 0)
}

func handleLessonSearch(args []string) int {
	if len(args) == 0 {
		return respond("", "Usage: mpm lesson search <query> [--json]", 1)
	}

	query := strings.Join(args, " ")
	jsonOutput := false
	if args[len(args)-1] == "--json" || args[len(args)-1] == "-j" {
		jsonOutput = true
		// Remove --json from query
		args = args[:len(args)-1]
		query = strings.Join(args, " ")
	}

	lessonStore, storeErr := internal.NewLessonStore("")
	if storeErr != nil {
		return respond("", fmt.Sprintf("Failed to initialize lesson store: %v", storeErr), 1)
	}
	results, searchErr := lessonStore.SearchLessons(query, 20)
	if searchErr != nil {
		return respond("", fmt.Sprintf("Search failed: %v", searchErr), 1)
	}

	if len(results) == 0 {
		if jsonOutput {
			data, _ := json.Marshal(map[string]interface{}{"query": query, "results": []interface{}{}, "message": "No lessons found"})
			fmt.Println(string(data))
		} else {
			respond("No lessons found.\n", "", 0)
		}
		return 0
	}

	if jsonOutput {
		type lessonResult struct {
			ID        string   `json:"id"`
			Type      string   `json:"type"`
			Content   string   `json:"content"`
			Tags      []string `json:"tags"`
			CreatedAt string   `json:"created_at"`
		}
		result := make([]lessonResult, 0, len(results))
		for _, l := range results {
			result = append(result, lessonResult{
				ID:        l.ID,
				Type:      string(l.Type),
				Content:   l.Content,
				Tags:      l.Tags,
				CreatedAt: l.Created,
			})
		}
		data, _ := json.Marshal(map[string]interface{}{"query": query, "results": result})
		fmt.Println(string(data))
		return 0
	}

	var output strings.Builder
	output.WriteString(fmt.Sprintf("Found %d lessons:\n\n", len(results)))
	for _, l := range results {
		snippet := l.Content
		if len(snippet) > 100 {
			snippet = snippet[:100] + "..."
		}
		output.WriteString(fmt.Sprintf("[%s] [%s]\n    %s\n\n", l.ID, l.Type, snippet))
	}

	return respond(output.String(), "", 0)
}

func handleLessonGet(args []string) int {
	if len(args) == 0 {
		return respond("", "Usage: mpm lesson get <id>", 1)
	}

	id := args[0]
	lessonStore, storeErr := internal.NewLessonStore("")
	if storeErr != nil {
		return respond("", fmt.Sprintf("Failed to initialize lesson store: %v", storeErr), 1)
	}
	lesson, getErr := lessonStore.GetLesson(id)
	if getErr != nil {
		return respond("", fmt.Sprintf("Lesson not found: %s\n", id), 1)
	}

	var output strings.Builder
	output.WriteString(fmt.Sprintf("ID:      %s\n", lesson.ID))
	output.WriteString(fmt.Sprintf("Type:    %s\n", lesson.Type))
	output.WriteString(fmt.Sprintf("Created: %s\n", lesson.Created))
	output.WriteString(fmt.Sprintf("Reinforcement: %d\n", lesson.ReinforcementCount))
	if len(lesson.Tags) > 0 {
		output.WriteString(fmt.Sprintf("Tags:    %s\n", strings.Join(lesson.Tags, ", ")))
	}
	output.WriteString("\n")
	output.WriteString(lesson.Content)
	output.WriteString("\n")

	return respond(output.String(), "", 0)
}

func handleLessonShred(args []string) int {
	if len(args) == 0 {
		return respond("", "Usage: mpm lesson shred <id>", 1)
	}

	id := args[0]
	lessonStore, storeErr := internal.NewLessonStore("")
	if storeErr != nil {
		return respond("", fmt.Sprintf("Failed to initialize lesson store: %v", storeErr), 1)
	}
	err := lessonStore.DeleteLesson(id)
	if err != nil {
		return respond("", fmt.Sprintf("Failed to shred lesson: %v", err), 1)
	}

	return respond(fmt.Sprintf("Lesson shredded: %s\n", id), "", 0)
}

func handleLessonStats() int {
	lessonStore, storeErr := internal.NewLessonStore("")
	if storeErr != nil {
		return respond("", fmt.Sprintf("Failed to initialize lesson store: %v", storeErr), 1)
	}
	stats, statsErr := lessonStore.GetLessonStats()
	if statsErr != nil {
		return respond("", fmt.Sprintf("Failed to get stats: %v", statsErr), 1)
	}

	var output strings.Builder
	output.WriteString("Lesson Statistics:\n\n")
	output.WriteString(fmt.Sprintf("Total: %d\n", stats["total_lessons"]))
	output.WriteString("\nBy type:\n")
	if byType, ok := stats["by_type"].(map[string]int); ok {
		for t, count := range byType {
			output.WriteString(fmt.Sprintf("  %s: %d\n", t, count))
		}
	}
	return respond(output.String(), "", 0)
}

// ============================================================================
// Handler: menu
// ============================================================================

func handleMenu() int {
	// Get watch status
	watching := false
	if watchPool != nil {
		watching = watchPool.ActiveWorkers() > 0
	}

	// Build menu output
	var output strings.Builder
	output.WriteString("\n")
	output.WriteString("╔══════════════════════════════════════════╗\n")
	output.WriteString("║         MPM Control Menu                 ║\n")
	output.WriteString("╠══════════════════════════════════════════╣\n")
	output.WriteString("║                                          ║\n")

	if watching {
		output.WriteString("║  🟢 Watcher Active                        ║\n")
	} else {
		output.WriteString("║  🔴 Watcher: Inactive                     ║\n")
	}

	output.WriteString("║                                          ║\n")
	output.WriteString("║  Commands:                               ║\n")
	output.WriteString("║    mpm session list   - Saved sessions    ║\n")
	output.WriteString("║    mpm dashboard     - Open TUI          ║\n")
	output.WriteString("║                                          ║\n")
	output.WriteString("╚══════════════════════════════════════════╝\n")
	output.WriteString("\n")

	return respond(output.String(), "", 0)
}

// ============================================================================
// Utility Functions
// ============================================================================


func getMemoryStore() *internal.MemoryStore {
	if dbManager == nil {
		dm, err := internal.NewDatabaseManager("")
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error opening database: %v\n", err)
			return nil
		}
		dbManager = dm
	}
	// Wrap the shared *sql.DB in a SQLiteConnection to satisfy MemoryStore.DB.
	return &internal.MemoryStore{
		DB: &internal.SQLiteConnection{DB: dbManager.SQLDB()},
	}
}

// initDB is called once during main() startup to prime the singleton.
func initDB() error {
	if dbManager != nil {
		return nil
	}
	dm, err := internal.NewDatabaseManager("")
	if err != nil {
		return fmt.Errorf("failed to open database: %w", err)
	}
	dbManager = dm
	return nil
}

// closeDB closes the singleton connection. Call from main() on exit.
func closeDB() {
	if dbManager != nil {
		dbManager.Close()
		dbManager = nil
	}
}

func getSessionDir() string {
	paths := internal.DefaultMemoryPaths()
	return paths.SessionSavePath
}

func getSessionStore() *internal.SessionStore {
	paths := internal.DefaultMemoryPaths()
	return internal.NewSessionStore(paths.SessionSavePath)
}

func getReferenceStore() *internal.ReferenceStore {
	paths := internal.DefaultMemoryPaths()
	return internal.NewReferenceStore(paths.MemoryPath)
}

// getStatusCounts returns memory, session, topic, and reference counts
func getStatusCounts() (memories int, sessions int, topics int, references int) {
	memStore := getMemoryStore()
	sessStore := getSessionStore()
	refStore := getReferenceStore()

	if m, err := memStore.GetMemoryCount(); err == nil {
		memories = m
	}
	if s, err := sessStore.GetSessionCount(); err == nil {
		sessions = s
	}
	if t, err := memStore.GetTopicCount(); err == nil {
		topics = t
	}
	if r, err := refStore.GetReferenceCount(); err == nil {
		references = r
	}
	return
}

func datePrefix(s string) string {
	if len(s) >= 10 {
		return s[:10]
	}
	return s
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// GenerateID creates a unique ID for memories
func generateID() string {
	return internal.GenerateID()
}

// ============================================================================
// Handler: watch
// ============================================================================

// handleWatch manages the internal fsnotify watcher goroutine lifecycle.
// In the unified process architecture, the watcher runs within the same process.
// It supports both in-process goroutine management (direct start/stop) and detached
// mode where the watcher runs as a separate child process.
func handleWatch(args []string) int {
	if len(args) < 1 || args[0] == "status" {
		return handleWatchStatus()
	}

	subCmd := args[0]
	switch subCmd {
	case "start":
		// Check if --bg flag is present (indicates this is the child process)
		bgFlag := false
		for _, arg := range args[1:] {
			if arg == "--bg" {
				bgFlag = true
				break
			}
		}

		if !bgFlag {
			// PARENT: Check if watcher is already running via PID file
			existingPID := readWatchPID()
			if existingPID > 0 && isWatchProcessAlive(existingPID) {
				return respond("", fmt.Sprintf("Watcher is already running (PID %d)\n", existingPID), 1)
			}

			// SPAWN CHILD: Re-invoke self with --bg flag
			exe, err := os.Executable()
			if err != nil {
				return respond("", fmt.Sprintf("Error: cannot find executable: %v\n", err), 1)
			}
			cmd := exec.Command(exe, append([]string{"watch", "start", "--bg"}, args[1:]...)...)
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
			if err := cmd.Start(); err != nil {
				return respond("", fmt.Sprintf("Error: failed to start watcher: %v\n", err), 1)
			}
			fmt.Printf("🚀 Watcher started in background (PID %d)\n", cmd.Process.Pid)
			os.Exit(0)
		}

		// CHILD: --bg flag present — proceed with normal startup
		if err := startWatchGoroutine(); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		// Block indefinitely — this process IS the watcher, don't return to main()
		select {}
		// Unreachable — select{} blocks forever until signal fires

	case "stop":
		return handleWatchStop()

	case "restart":
		// Check if detached watcher is running via PID file
		pid := readWatchPID()
		if pid > 0 && isWatchProcessAlive(pid) {
			return respond("", "Restart is not supported while the detached watcher is running. Use 'mpm watch stop' then 'mpm watch start' to restart.\n", 1)
		}
		// For in-process restart (non-detached), stop and restart the goroutine
		stopWatchGoroutine()
		if err := startWatchGoroutine(); err != nil {
			return respond("", fmt.Sprintf("Error restarting: %v\n", err), 1)
		}
		return respond("File watcher restarted\n", "", 0)

	case "add-path":
		return handleWatchAddPath(args[1:])
	case "remove-path":
		return handleWatchRemovePath(args[1:])
	case "list-paths", "paths":
		return handleWatchListPaths(args[1:])
	case "help":
		return handleWatchHelp()

	default:
		return handleWatchHelp()
	}
}

// startWatchGoroutine launches the fsnotify watcher and external DB pollers
// as background goroutines within the current process.
// When run as a detached child (--bg flag), it also:
//   - Writes its PID to watch.pid
//   - Registers a SIGTERM/Interrupt handler for graceful shutdown
//   - Blocks forever (select{}) until signalled
func startWatchGoroutine() error {
	if watcherCancel != nil {
		return fmt.Errorf("watcher is already running")
	}

	// Lazy-init the worker pool with its own DatabaseManager.
	// The DM is opened once and shared across all pool workers.
	if watchPool == nil {
		dm, err := internal.NewDatabaseManager("")
		if err != nil {
			return fmt.Errorf("failed to open database for watcher: %w", err)
		}
		watchPool = NewWorkerPool(dm, defaultWorkerPoolSize)
	}
	watcherCtx, watcherCancel = context.WithCancel(context.Background())
	watcherDone = make(chan struct{})
	watchPool.Start(watcherCtx)

	// Write PID file after worker pool is confirmed alive
	// (not before — avoids writing a PID for a pool that might fail to start)
	_ = writeWatchPID()

	// Set up graceful shutdown handler
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)

	go func() {
		defer close(watcherDone)
		defer deleteWatchPID() // clean up PID file on exit

		// Launch the fsnotify watcher event loop in its own goroutine
		// (it blocks internally on fsnotify events).
		go startWatcherGoroutine(watcherCtx, watchPool, false, false)

		// Start external DB polling goroutines (each spawns its own goroutine).
		startExternalDBPollGoroutines(watcherCtx, watchPool, false, false)

		// Submit a startup sweep event to the pool.
		watchPool.Submit(WatchEvent{Type: EventStartupSweep, DryRun: false, Verbose: false})

		// Block until cancelled or signal received
		select {
		case <-watcherCtx.Done():
			// Cancelled by stopWatchGoroutine
		case sig := <-sigCh:
			// SIGTERM or Interrupt received — graceful shutdown
			fmt.Fprintf(os.Stderr, "\n⚠️  Received %v — shutting down watcher...\n", sig)
			stopWatchGoroutine()
		}
	}()

	return nil
}

// stopWatchGoroutine cancels the watcher goroutine and waits for it to finish.
func stopWatchGoroutine() {
	if watcherCancel == nil {
		return
	}
	watcherCancel()
	<-watcherDone
	watcherCancel = nil
	watcherCtx = nil
	watcherDone = nil
}

// handleWatchStop reads the PID from watch.pid and signals the watcher to stop.
func handleWatchStop() int {
	pid := readWatchPID()
	if pid == 0 {
		// Already stopped — desired state achieved, return 0
		return respond("Watcher is not running (already stopped).\n", "", 0)
	}

	proc, _ := os.FindProcess(pid)
	if !isWatchProcessAlive(pid) {
		// Process gone — clean up stale PID file, return 0
		deleteWatchPID()
		return respond("Watcher is not running (already stopped).\n", "", 0)
	}

	// Process is alive — send Interrupt signal and wait for graceful shutdown
	proc.Signal(os.Interrupt)

	// Give it a moment to shut down gracefully
	// PIDs are reused slowly on Linux, so this avoids stale PID confusion
	time.Sleep(500 * time.Millisecond)

	// Verify it's gone
	if isWatchProcessAlive(pid) {
		deleteWatchPID()
		return respond(fmt.Sprintf("Watcher stop signal sent (PID %d) — it may take a moment to shut down.\n", pid), "", 0)
	}

	deleteWatchPID()
	return respond("Watcher stopped.\n", "", 0)
}

// handleWatchStatus checks if the watcher is running via the PID file.
func handleWatchStatus() int {
	pid := readWatchPID()
	if pid == 0 {
		return respond("Watcher is not running.\n", "", 0)
	}

	if !isWatchProcessAlive(pid) {
		// Stale PID file — clean it up
		deleteWatchPID()
		return respond("Watcher is not running.\n", "", 0)
	}

	// Process is alive — report status
	// Note: In detached mode, the parent process cannot query the child's pool state.
	// Pool statistics (active workers, events processed) are only visible to the
	// child process itself. We report running state based on the PID file alone.
	if watchPool == nil {
		return respond(fmt.Sprintf("Watcher is running (PID %d)\n", pid), "", 0)
	}
	active := watchPool.ActiveWorkers()
	processed := watchPool.ProcessedCount()
	return respond(fmt.Sprintf("Watcher is running (PID %d, %d active workers, %d events processed)\n", pid, active, processed), "", 0)
}

// formatWatchStatus returns a human-readable status string for the file watcher.
func formatWatchStatus() string {
	if watchPool == nil {
		return "Watch system: not initialized\n"
	}
	active := watchPool.ActiveWorkers()
	processed := watchPool.ProcessedCount()
	if watcherCancel != nil && watcherDone != nil {
		return fmt.Sprintf("Watch system: running (%d active workers, %d events processed)\n", active, processed)
	}
	return fmt.Sprintf("Watch system: stopped (%d events processed)\n", processed)
}

func handleWatchHelp() int {
	output := `mpm watch - File watcher for automatic memory ingestion

Usage:
  mpm watch start              Start the file watcher
  mpm watch stop               Stop the file watcher
  mpm watch status             Check watcher status
  mpm watch add-path <path>    Add a directory to watch
  mpm watch remove-path <path> Remove a directory from watch list
  mpm watch list-paths         List watched directories

Examples:
  mpm watch start
  mpm watch stop
  mpm watch status
  mpm watch add-path /home/user/notes --type memory
  mpm watch list-paths
`
	return respond(output, "", 0)
}
