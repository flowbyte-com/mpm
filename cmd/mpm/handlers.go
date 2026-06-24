package main

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
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

	mpminternal "mpm/internal"
)

// Package-level singleton DatabaseManager — initialized once per process,
// shared across all handler calls to avoid connection proliferation.
var dbManager *internal.DatabaseManager
var dbManagerInitErr error

// watchPool is the shared WorkerPool used by file watcher and external DB pollers.
// It is initialized by main() in the unified process architecture.
var watchPool *WorkerPool

// Global synthesis worker — initialized in startWatchGoroutine, shared across
// all file event processors. Wired here to avoid creating per-event instances.
var watchSynthWorker *mpminternal.SynthesisWorker

// Global idle consolidation worker — runs pattern detection when the
// filesystem watcher has been quiet for 30+ minutes.
var watchIdleWorker *mpminternal.IdleConsolidationWorker

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

// activeContext holds the mode/persona for the current CLI invocation.
// Set at the start of each handler via detectActiveContext(), cleared after use.
var activeMode string
var activePersona string

// detectActiveContext reads the current mode and persona from config files.
// These values are injected into memory metadata on every AddMemory call.
func detectActiveContext() (mode, persona string) {
	modePath := filepath.Join(config.GetMPMDir(), "config", "current_mode")
	if data, err := os.ReadFile(modePath); err == nil {
		mode = strings.TrimSpace(string(data))
	}
	personaPath := filepath.Join(config.GetMPMDir(), "config", "current_persona")
	if data, err := os.ReadFile(personaPath); err == nil {
		persona = strings.TrimSpace(string(data))
	}
	return
}

// injectActiveContext sets activeMode and activePersona from config files.
// Call at the start of any handler that calls AddMemory.
func injectActiveContext() {
	activeMode, activePersona = detectActiveContext()
}

// clearActiveContext clears the active context after a handler completes.
// Call after AddMemory to prevent stale state leaking between invocations.
func clearActiveContext() {
	activeMode = ""
	activePersona = ""
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
	case "promote":
		return handlePromote(args)
	case "reinforce":
		return handleReinforce(args)
	case "weaken":
		return handleWeaken(args)
	case "snooze":
		return handleSnooze(args)
	case "set-weight":
		return handleSetWeight(args)
	case "patch-memory":
		return handlePatchMemory(args)
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
		for rows.Next() {
			var id, collection, content, metadata, created string
			if err := rows.Scan(&id, &collection, &content, &metadata, &created); err != nil {
				continue
			}
			directives = append(directives, directiveEntry{
				ID:         id,
				Collection: collection,
				Content:    content,
				CreatedAt:  created,
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
		return respond("", "Usage: mpm memory add [--expires-in <duration>] <content>", 1)
	}

	// Pre-scan --json, -i/--interactive, and --expires-in flags
	jsonOutput := false
	expiresIn := ""
	interactive := false
	contentArgs := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--json", "-j":
			jsonOutput = true
		case "-i", "--interactive":
			interactive = true
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
		if len(contentArgs) == 0 {
			return respond("", "Usage: mpm memory add [--expires-in <duration>] <content>", 1)
		}
		content = strings.Join(contentArgs, " ")
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

	store := getMemoryStore()
	mem, err := store.AddMemory(content, "memories", nil, memMetadata, "", "cli")
	if err != nil {
		return respond("", fmt.Sprintf("Failed to add memory: %v", err), 1)
	}

	// Set TTL if --expires-in was provided
	var suggestions []map[string]interface{}
	dm, err := mpminternal.NewDatabaseManager("")
	if err == nil {
		defer dm.Close()
		if expiresIn != "" {
			dur, parseErr := parseDuration(expiresIn)
			if parseErr == nil {
				dm.SetMemoryTTL(mem.ID, time.Now().Add(dur))
			}
		}

		// Get topic suggestions (non-blocking — failures are silently ignored)
		suggestions, _ = suggestTopicsForMemory(dm, mem.ID, content, 3, 0.3)

		// Fire-and-forget: check for near-miss candidates and auto-synthesize.
		// Opens its own DB connection since the enclosing dm will close.
		go func(id, c string) {
			synthDM, synthErr := mpminternal.NewDatabaseManager("")
			if synthErr != nil {
				slog.Warn("synthesis: failed to open db", "memory_id", id, "error", synthErr)
				return
			}
			defer synthDM.Close()
			client := mpminternal.NewSynthClient()
			mpminternal.AutoSynthesize(context.Background(), synthDM, client, id, c)
		}(mem.ID, content)
	}
	mem.SuggestedTopics = suggestions

	if jsonOutput {
		// JSON output mode
		type jsonResult struct {
			success         bool                     `json:"success"`
			id              string                   `json:"id"`
			content         string                   `json:"content"`
			suggestedTopics []map[string]interface{} `json:"suggested_topics,omitempty"`
		}
		result := jsonResult{
			success: true,
			id:      mem.ID,
			content: mem.Content,
		}
		if len(suggestions) > 0 {
			result.suggestedTopics = suggestions
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

// handleSuggestTags returns unique tags matching a prefix, one per line.
// Used by shell completion scripts. Hidden command: mpm _suggest_tags <prefix>
func handleSuggestTags(args []string) int {
	prefix := ""
	if len(args) >= 2 && args[0] == "_suggest_tags" {
		prefix = strings.ToLower(args[1])
	} else if len(args) >= 1 {
		prefix = strings.ToLower(args[len(args)-1])
	}

	dm, err := mpminternal.NewDatabaseManager("")
	if err != nil {
		return 1
	}
	defer dm.Close()

	rows, err := dm.SQLDB().Query(`
		SELECT DISTINCT value
		FROM memories,
		json_each(memories.tags)
		WHERE json_valid(memories.tags)
		AND memories.tags IS NOT NULL
		AND memories.tags != 'null'
		AND memories.tags != '[]'
		AND value IS NOT NULL
		AND value != ''
		AND lower(value) LIKE lower(?) || '%'
		ORDER BY lower(value)
	`, prefix)
	if err != nil {
		// Fallback: scan all tags in-memory if json_each fails
		if rows != nil {
			rows.Close()
		}
		allRows, err2 := dm.SQLDB().Query(`SELECT tags FROM memories WHERE tags IS NOT NULL`)
		if err2 != nil {
			return 1
		}
		defer allRows.Close()
		seen := map[string]bool{}
		var allTags []string
		for allRows.Next() {
			var tagsJSON string
			if allRows.Scan(&tagsJSON) != nil {
				continue
			}
			if tagsJSON == "" || tagsJSON == "null" || tagsJSON == "[]" {
				continue
			}
			var tags []string
			if json.Unmarshal([]byte(tagsJSON), &tags) == nil {
				for _, t := range tags {
					if t != "" && !seen[t] && strings.HasPrefix(strings.ToLower(t), prefix) {
						seen[t] = true
						allTags = append(allTags, t)
					}
				}
			}
		}
		for _, t := range allTags {
			fmt.Println(t)
		}
		return 0
	}
	defer rows.Close()

	found := false
	for rows.Next() {
		var value string
		if rows.Scan(&value) == nil && value != "" {
			fmt.Println(value)
			found = true
		}
	}
	_ = found // suppress unused variable warning
	return 0
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
	db := store.DB

	// Verify topic exists via direct ID lookup. SearchTopics uses FTS5 on
	// (name, description) only — hex IDs are not tokenized, so they never
	// match. Direct primary-key check is the right tool for ID-based lookups.
	var exists int
	err := db.QueryRow("SELECT 1 FROM topics WHERE id = ?", id).Scan(&exists)
	if err == sql.ErrNoRows {
		return respond("", fmt.Sprintf("Topic not found: %s\n", id), 1)
	}
	if err != nil {
		return respond("", fmt.Sprintf("Failed to check topic: %v\n", err), 1)
	}

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

	err := store.DeleteMemory(id, "session")
	if err != nil {
		return respond("", fmt.Sprintf("Failed to shred session: %v", err), 1)
	}

	return respond(fmt.Sprintf("Session shredded: %s\n", id), "", 0)
}

// ============================================================================
// Handler: gc (garbage collection / memory decay)
// ============================================================================

func handleGC(args []string) int {
	jsonOutput, _ := ExtractJSONFlag(args)
	dryRun := false
	aggressive := false
	review := false
	purge := false
	maxAgeHours := 24

	for _, arg := range args {
		if arg == "--dry-run" {
			dryRun = true
		}
		if arg == "--aggressive" {
			aggressive = true
		}
		if arg == "--review" {
			review = true
		}
		if arg == "--purge" {
			purge = true
		}
		if arg == "--shred-negative" {
			dryRun = false // explicit override to allow actual shredding
		}
		if strings.HasPrefix(arg, "--max-age=") {
			fmt.Sscanf(arg, "--max-age=%d", &maxAgeHours)
		}
	}

	dm, err := mpminternal.NewDatabaseManager("")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}
	defer dm.Close()

	// --shred-negative: hard-delete negative-weight memories that have a proven theory
	if shredNegative := func() bool {
		for _, arg := range args {
			if arg == "--shred-negative" {
				return true
			}
		}
		return false
	}(); shredNegative {
		negMemories, err := dm.GetNegativeWeightMemories()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			return 1
		}
		if len(negMemories) == 0 {
			fmt.Println("No negative-weight memories found.")
			return 0
		}
		fmt.Printf("Found %d negative-weight memories:\n\n", len(negMemories))
		var shredded int
		for _, m := range negMemories {
			id, _ := m["id"].(string)
			weight, _ := m["weight"].(int)
			theory, err := dm.GetProvenTheoryForMemory(id)
			if err != nil {
				fmt.Fprintf(os.Stderr, "  [%s] error checking theory: %v\n", id, err)
				continue
			}
			if theory != nil {
				if dryRun {
					fmt.Printf("  [%s] weight=%d → WOULD SHRED (proven theory %s)\n", id, weight, theory["id"])
				} else {
					if err := dm.ShredMemory(id); err != nil {
						fmt.Fprintf(os.Stderr, "  [%s] shred error: %v\n", id, err)
						continue
					}
					fmt.Printf("  [%s] weight=%d → SHREDDED (proven theory %s)\n", id, weight, theory["id"])
					shredded++
				}
			} else {
				fmt.Printf("  [%s] weight=%d → SKIP (no proven theory)\n", id, weight)
			}
		}
		if !dryRun && shredded > 0 {
			fmt.Printf("\nShredded %d memories with proven theories.\n", shredded)
		}
		return 0
	}

	now := time.Now()
	gcTimestampJSON, _ := json.Marshal(map[string]string{"timestamp": now.Format(time.RFC3339)})

	// Atomic frequency cap: UPDATE last_gc_at only if no recent GC has run.
	// This avoids the TOCTOU race between reading the config and writing it later.
	// If another GC process updated last_gc_at since our read, rowsAffected will be 0
	// and we'll skip this GC run.
	result, err := dm.SQLDB().Exec(`
		UPDATE system_config
		SET raw_json = ?
		WHERE key = 'last_gc_at'
		AND (
			raw_json IS NULL
			OR
			datetime(json_extract(raw_json, '$.updated_at')) < datetime('now', '-' || ? || ' hours')
		)
	`, string(gcTimestampJSON), strconv.Itoa(maxAgeHours))
	rowsAffected, _ := result.RowsAffected()
	if rowsAffected == 0 {
		// last_gc_at was updated by another process while we were working — skip this run.
		// Re-read and report the actual last GC time.
		lastGC, _ := dm.GetSystemConfig("last_gc_at")
		if updatedAt, ok := lastGC["updated_at"].(string); ok {
			if last, parseErr := time.Parse(time.RFC3339, updatedAt); parseErr == nil {
				fmt.Printf("Skipped: last gc was %s\n", last.Format("2006-01-02 15:04"))
				return 0
			}
		}
		fmt.Println("Skipped: recent GC detected")
		return 0
	}
	// Atomic update succeeded — we have the lock. Proceed with GC.
	// Note: all subsequent work happens AFTER this atomic check-and-set.

	// Purge mode: hard delete old reviewed memories and exit
	if purge {
		result, err := dm.SQLDB().Exec(`
			DELETE FROM memories
			WHERE deleted_at IS NOT NULL
			AND deleted_at < datetime('now', '-30 days')
		`)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			return 1
		}
		purged, _ := result.RowsAffected()
		fmt.Printf("Purged %d old deleted memories\n", purged)
		return 0
	}

	// Audit log retention sweep — drop entries older than 30 days. Wired
	// into the GC cycle rather than a separate cron because GC is the
	// canonical cleanup pass and we want one place to tune retention.
	if pruned, err := dm.PruneAuditLog(30); err != nil {
		fmt.Fprintf(os.Stderr, "audit prune error: %v\n", err)
	} else if pruned > 0 {
		fmt.Printf("Pruned %d audit log entries older than 30 days\n", pruned)
	}

	// Session handoffs retention sweep — 90 days. Handoffs are
	// higher-signal, lower-volume than audit log, so they get a longer
	// retention window. The next session may need to look back more than
	// 30 days to understand a long-running project.
	if pruned, err := dm.PruneHandoffs(90); err != nil {
		fmt.Fprintf(os.Stderr, "handoff prune error: %v\n", err)
	} else if pruned > 0 {
		fmt.Printf("Pruned %d session handoffs older than 90 days\n", pruned)
	}

	// Get all non-deleted memories
	rows, err := dm.SQLDB().Query(`
		SELECT id, weight, last_accessed_at, created_at, is_long_term
		FROM memories WHERE deleted_at IS NULL
	`)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}
	defer rows.Close()

	// Capture monotonic clock offset once at start of GC run to prevent clock-rollback exploits.
	// Using a captured "now" ensures all time calculations within this GC pass use the same
	// reference point, even if the system clock goes backward mid-run.
	monotonicNow := time.Now()
	var deadMemories []map[string]interface{}
	var updated, scanned int

	// Collect all computed weight changes for batch application (avoids N+1 SQL pattern).
	// Structure: []struct{ id string, oldWeight int, newWeight float64, isLTM bool }
	type weightDelta struct {
		id        string
		oldWeight int
		newWeight float64
		isLTM     bool
	}
	var deltas []weightDelta

	for rows.Next() {
		scanned++
		var id string
		var weight int
		var lastAccessed, createdAt *time.Time
		var isLongTerm bool

		rows.Scan(&id, &weight, &lastAccessed, &createdAt, &isLongTerm)

		// Compute days since access using captured monotonic time
		lastAccessTime := lastAccessed
		if lastAccessTime == nil {
			lastAccessTime = createdAt
		}
		if lastAccessTime == nil {
			// Both timestamps are NULL — skip this row
			continue
		}
		daysSinceAccess := monotonicNow.Sub(*lastAccessTime).Hours() / 24.0

		// Compute decay amount (float64 throughout)
		decay := computeDecay(float64(weight), daysSinceAccess, isLongTerm, createdAt, aggressive, monotonicNow)
		newWeight := float64(weight) - decay

		// Floor
		if newWeight < -10.0 {
			newWeight = -10.0
		}
		// LTM protection: preserve memories that are explicitly marked LTM OR have weight >= 10.
		// Applying the floor BEFORE dead detection ensures LTM memories are never flagged for deletion.
		if (isLongTerm || float64(weight) >= 10) && newWeight < 1.0 {
			newWeight = 1.0
		}

		// Record delta for batch update
		deltas = append(deltas, weightDelta{id: id, oldWeight: weight, newWeight: newWeight, isLTM: isLongTerm || float64(weight) >= 10})

		// Dead if <= 0 (post-clamp) and not LTM — LTM memories are never eligible for deletion.
		// Also catches already-dead memories (weight already <= 0 from a previous GC)
		// that were never soft-deleted — without the oldWeight > 0 guard they'd be missed.
		isLTM := deltas[len(deltas)-1].isLTM
		if newWeight <= 0.0 && !isLTM {
			var deadContent string
			dm.SQLDB().QueryRow(`SELECT SUBSTR(content, 1, 60) FROM memories WHERE id = ?`, id).Scan(&deadContent)
			deadMemories = append(deadMemories, map[string]interface{}{
				"id":      id,
				"content": deadContent,
				"weight":  weight,
				"decay":   decay,
			})
		}
	}

	// Batch-apply all weight updates to avoid N+1 SQL pattern.
	// Uses math.Floor for consistent rounding (not int() truncation which rounds toward zero).
	if !dryRun && len(deltas) > 0 {
		tx, txErr := dm.SQLDB().Begin()
		if txErr != nil {
			fmt.Fprintf(os.Stderr, "Error: failed to begin transaction: %v\n", txErr)
		} else {
			var batchErr error
			for _, d := range deltas {
				if d.newWeight == float64(d.oldWeight) {
					continue
				}
				rounded := int(math.Round(d.newWeight))
				if d.isLTM {
					if rounded < 1 {
						rounded = 1
					}
				} else {
					if rounded < -10 {
						rounded = -10
					}
				}
				if _, err := tx.Exec(`UPDATE memories SET weight = ? WHERE id = ?`, rounded, d.id); err != nil {
					batchErr = err
					break
				}
				updated++
			}
			if batchErr != nil {
				tx.Rollback()
				fmt.Fprintf(os.Stderr, "Error: batch update failed, rolled back: %v\n", batchErr)
			} else if err := tx.Commit(); err != nil {
				tx.Rollback()
				fmt.Fprintf(os.Stderr, "Error: batch commit failed, rolled back: %v\n", err)
			}
		}
	}

	// --review mode: soft-delete all dead and zombie memories (LTM already excluded above).
	// Also catches existing zombies (weight <= 0 from previous GC runs that were never cleaned).
	if review && !dryRun {
		for _, m := range deadMemories {
			if _, err := dm.SQLDB().Exec(`UPDATE memories SET deleted_at = CURRENT_TIMESTAMP WHERE id = ?`, m["id"]); err != nil {
				slog.Warn("gc soft-delete failed", "id", m["id"], "error", err)
			}
		}
		// Clean any lingering zombies not caught by this pass
		if _, err := dm.SQLDB().Exec(`UPDATE memories SET deleted_at = CURRENT_TIMESTAMP WHERE weight <= 0 AND is_long_term = 0 AND deleted_at IS NULL`); err != nil {
			slog.Warn("gc zombie cleanup failed", "error", err)
		}
		if len(deadMemories) > 0 {
			fmt.Printf("Soft-deleted %d dead memories\n", len(deadMemories))
		}
	}

	// Update last_gc_at timestamp
	if !dryRun {
		dm.SaveSystemConfig("last_gc_at", string(gcTimestampJSON), "", "")
	}

	// Output
	if jsonOutput {
		type gcResult struct {
			Scanned      int                      `json:"scanned"`
			Updated      int                      `json:"updated"`
			DeadCount    int                      `json:"dead_count"`
			DeadMemories []map[string]interface{} `json:"dead_memories,omitempty"`
			DryRun       bool                     `json:"dry_run"`
		}
		result := gcResult{
			Scanned:      scanned,
			Updated:      updated,
			DeadCount:    len(deadMemories),
			DeadMemories: deadMemories,
			DryRun:       dryRun,
		}
		data, _ := json.Marshal(result)
		fmt.Println(string(data))
	} else {
		fmt.Printf("Scanned: %d | Updated: %d | Dead: %d\n", scanned, updated, len(deadMemories))
		if len(deadMemories) > 0 {
			fmt.Println("\nDead memories (weight <= 0):")
			for _, m := range deadMemories {
				content := m["content"].(string)
				if len(content) > 60 {
					content = content[:60] + "…"
				}
				fmt.Printf("  [%s] %s\n", m["id"].(string)[:8], content)
			}
		}
	}
	return 0
}

// computeDecay returns the weight decay amount for a memory.
// All math is float64; only the final value is truncated on DB write.
// Uses a pre-captured "now" timestamp to prevent clock-rollback exploits.
func computeDecay(weight float64, daysSinceAccess float64, isLongTerm bool, createdAt *time.Time, aggressive bool, now time.Time) float64 {
	multiplier := 1.0
	if aggressive {
		multiplier = 2.0
	}

	if isLongTerm {
		return daysSinceAccess * 0.01 * multiplier
	}
	if weight >= 10 {
		return daysSinceAccess * 0.02 * multiplier
	}
	if weight >= 5 {
		return daysSinceAccess * 0.05 * multiplier
	}

	// Low-weight: decay scales with age (newer = faster decay)
	ageFactor := 1.0
	if createdAt != nil {
		// Use the captured monotonic reference: daysSinceCreated based on the pre-captured
		// timestamp, not wall-clock time. This prevents clock-rollback from slowing decay.
		daysSinceCreated := now.Sub(*createdAt).Hours() / 24.0
		if daysSinceCreated > 30.0 {
			ageFactor = 1.0
		} else {
			ageFactor = daysSinceCreated / 30.0
		}
	}
	baseDecay := 0.1 + 0.2*ageFactor
	return daysSinceAccess * baseDecay * multiplier
}

// handleRestore recovers a soft-deleted memory by clearing deleted_at and preserving
// its original LTM status and weight (minimum floor of 1).
func handleRestore(args []string) int {
	if len(args) < 2 {
		return respond("", "Usage: mpm restore <memory-id>\n", 1)
	}
	id := args[1]

	dm, err := mpminternal.NewDatabaseManager("")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}
	defer dm.Close()

	// Fetch current state to preserve is_long_term and avoid overwriting weight
	var currentIsLTM int
	var currentWeight int
	err = dm.SQLDB().QueryRow(
		`SELECT COALESCE(is_long_term, 0), COALESCE(weight, 1) FROM memories WHERE id = ?`,
		id,
	).Scan(&currentIsLTM, &currentWeight)
	if err != nil {
		if err == sql.ErrNoRows {
			return respond("", fmt.Sprintf("Memory not found: %s\n", id), 1)
		}
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}

	// Restore deleted_at, reset weight to max(original weight, 1) to prevent 0-weight limbo.
	// LTM status is preserved.
	result, err := dm.SQLDB().Exec(
		`UPDATE memories SET deleted_at = NULL, weight = MAX(?, 1) WHERE id = ?`,
		currentWeight, id,
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}
	affected, _ := result.RowsAffected()
	if affected == 0 {
		return respond("", fmt.Sprintf("Memory not found: %s\n", id), 1)
	}
	status := "restored"
	if currentIsLTM == 1 {
		status = "restored (LTM preserved)"
	}
	fmt.Printf("%s: %s\n", status, id)
	return 0
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
	jsonOutput, cleanedArgs := ExtractJSONFlag(args)

	// Extract description from non-flag args
	for _, arg := range cleanedArgs[1:] {
		if !strings.HasPrefix(arg, "-") {
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
			"id":          id,
			"name":        name,
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

	query := args[0]
	jsonOutput, queryArgs := ExtractJSONFlag(args)
	if len(queryArgs) > 0 {
		query = queryArgs[0]
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
	jsonOutput, _ := ExtractJSONFlag(args)

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
		memoryIDs := []string{}
		if rows, err := db.Query("SELECT memory_id FROM topic_memberships WHERE topic_id = ?", id); err == nil {
			for rows.Next() {
				var mid string
				rows.Scan(&mid)
				memoryIDs = append(memoryIDs, mid)
			}
			rows.Close()
		}
		chunkCount := len(memoryIDs)
		data, _ := json.Marshal(map[string]interface{}{
			"id":          id,
			"name":        name,
			"description": description,
			"created_at":  created,
			"memory_ids":  memoryIDs,
			"chunk_count": chunkCount,
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

	// Fetch top 3 memories for this topic
	dm, err := mpminternal.NewDatabaseManager("")
	if err == nil {
		memories, total, _ := dm.GetTopicTopMemories(id, 3)
		if len(memories) > 0 {
			bold := "\033[1m"
			reset := "\033[0m"
			dim := "\033[2m"

			output.WriteString(fmt.Sprintf("\n%s─ Top Memories ──────────────────────────%s\n", bold, reset))
			for i, mem := range memories {
				content := mem.Content
				if len(content) > 120 {
					content = content[:120] + "…"
				}
				output.WriteString(fmt.Sprintf("\n%d. %s\n", i+1, content))
			}
			if total > 3 {
				remaining := total - 3
				output.WriteString(fmt.Sprintf("\n%s[+ %d other linked memories]%s\n", dim, remaining, reset))
			}
			output.WriteString(fmt.Sprintf("%s─────────────────────────────────────────%s\n", bold, reset))
		}
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
	jsonOutput, _ := ExtractJSONFlag(args)

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

	// Inject active mode/persona from config files
	injectActiveContext()
	defer clearActiveContext()

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

	// Add to session collection
	mem, err := store.AddMemory(content, "session", nil, memMetadata, "", "cli")
	if err != nil {
		return respond("", fmt.Sprintf("Failed to add session: %v", err), 1)
	}

	return respond(fmt.Sprintf("Session added with ID: %s\n", mem.ID), "", 0)
}

// handleWake returns context from the last active session.
// Displays mode, persona, recent topics, and recent memories.
func handleWake(args []string) int {
	dm, err := mpminternal.NewDatabaseManager("")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}
	defer dm.Close()

	// Parse --strict flag
	strictMode := false
	cleanArgs := make([]string, 0, len(args))
	for _, a := range args {
		if a == "--strict" {
			strictMode = true
		} else {
			cleanArgs = append(cleanArgs, a)
		}
	}

	var sessionID string

	// Try to get the most recent explicit session record
	session, err := dm.GetLastSession()
	if err == nil && session != nil {
		sessionID, _ = session["session_id"].(string)
	}

	// Pull the latest handoff (read or unread) so the agent can see
	// what the previous session was doing, what it committed to, and
	// what was still unresolved. Distinct from the (currently empty)
	// sessions table — handoffs are bootstrap data, sessions are
	// content logs. The mpm call read_wake_context path marks the
	// handoff as read automatically; mpm wake leaves the read state
	// alone so the agent can browse the history.
	handoff, herr := dm.GetLatestHandoff()
	_ = herr // ignore: no handoffs is fine, just skip the block below

	// Strict mode: bypass fallback queries, rely solely on explicit session
	if strictMode && sessionID == "" {
		fmt.Println("No previous session found.")
		return 1
	}

	// Query all recent memories (any collection, not just 'session')
	var memories []map[string]interface{}
	rows, err := dm.SQLDB().Query(`
		SELECT id, collection, content, tags, metadata, created_at
		FROM memories
		WHERE deleted_at IS NULL
		ORDER BY created_at DESC
		LIMIT 10
	`)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var memID, collection, content, createdAt string
			var tagsJSON, metadataJSON sql.NullString
			if err := rows.Scan(&memID, &collection, &content, &tagsJSON, &metadataJSON, &createdAt); err != nil {
				slog.Warn("handleWake: rows.Scan failed", "error", err)
				break
			}
			var tags []string
			var memMeta map[string]interface{}
			if tagsJSON.Valid {
				json.Unmarshal([]byte(tagsJSON.String), &tags)
			}
			if metadataJSON.Valid {
				json.Unmarshal([]byte(metadataJSON.String), &memMeta)
			}
			memories = append(memories, map[string]interface{}{
				"id":         memID,
				"collection": collection,
				"content":    content,
				"tags":       tags,
				"metadata":   memMeta,
				"created_at": createdAt,
			})
		}
	}

	// Query recent lessons for additional cold-start context
	var lessons []map[string]interface{}
	lrows, err := dm.SQLDB().Query(`
		SELECT id, type, content, tags, created
		FROM lessons
		ORDER BY created DESC
		LIMIT 5
	`)
	if err == nil {
		defer lrows.Close()
		for lrows.Next() {
			var lessonID, lessonType, content, tagsJSON, created string
			if err := lrows.Scan(&lessonID, &lessonType, &content, &tagsJSON, &created); err != nil {
				break
			}
			var tagList []string
			if tagsJSON != "" {
				json.Unmarshal([]byte(tagsJSON), &tagList)
			}
			lessons = append(lessons, map[string]interface{}{
				"id":      lessonID,
				"type":    lessonType,
				"content": "[Lesson] " + content,
				"tags":    tagList,
				"created": created,
			})
		}
	}

	// Identity fallback: read active.json for persona and modes
	var activeMode, activePersona string
	mpmDir := config.GetMPMDir()
	activePath := filepath.Join(mpmDir, "active.json")
	if data, err := os.ReadFile(activePath); err == nil {
		type activeState struct {
			Persona string   `json:"persona"`
			Modes   []string `json:"modes"`
		}
		var active activeState
		if json.Unmarshal(data, &active) == nil {
			activePersona = active.Persona
			if len(active.Modes) > 0 {
				activeMode = strings.Join(active.Modes, ", ")
			}
		}
	}

	// Pre-scan for --json
	jsonOutput, _ := ExtractJSONFlag(cleanArgs)

	// Shared struct definitions for output
	type memoryRef struct {
		ID        string `json:"id"`
		Content   string `json:"content"`
		CreatedAt string `json:"created_at"`
	}
	type wakeResult struct {
		SessionID      string                 `json:"session_id"`
		ActiveMode     string                 `json:"active_mode"`
		ActivePersona  string                 `json:"active_persona"`
		RecentTopics   []string               `json:"recent_topics"`
		RecentMemories []memoryRef            `json:"recent_memories"`
		LastHandoff    *mpminternal.Handoff   `json:"last_handoff,omitempty"`
	}

	// Collect topics and build consolidated memory references
	topicSet := map[string]bool{}
	memRefs := make([]memoryRef, 0)

	for _, mem := range memories {
		memRefs = append(memRefs, memoryRef{
			ID:        mem["id"].(string),
			Content:   mem["content"].(string),
			CreatedAt: mem["created_at"].(string),
		})
		topics, _ := dm.GetMemoryTopics(mem["id"].(string))
		for _, t := range topics {
			topicSet[t.Name] = true
		}
	}

	for _, l := range lessons {
		memRefs = append(memRefs, memoryRef{
			ID:        l["id"].(string),
			Content:   l["content"].(string),
			CreatedAt: l["created"].(string),
		})
	}

	topics := make([]string, 0)
	for t := range topicSet {
		topics = append(topics, t)
	}

	result := wakeResult{
		SessionID:      sessionID,
		ActiveMode:     activeMode,
		ActivePersona:  activePersona,
		RecentTopics:   topics,
		RecentMemories: memRefs,
		LastHandoff:    handoff,
	}

	if jsonOutput {
		data, _ := json.Marshal(result)
		fmt.Println(string(data))
		return 0
	}

	// No context at all — human-readable empty state
	if len(memRefs) == 0 && activeMode == "" && activePersona == "" {
		fmt.Println("No previous session found.")
		return 0
	}

	// Human-readable output
	fmt.Println("╭─ Last Session ──────────────────────────────────────────────╮")
	if activeMode != "" {
		fmt.Printf("│ Mode:     %-42s │\n", activeMode)
	}
	if activePersona != "" {
		fmt.Printf("│ Persona:  %-42s │\n", activePersona)
	}
	if len(topics) > 0 {
		shown := topics
		if len(shown) > 3 {
			shown = shown[:3]
		}
		topicStr := strings.Join(shown, ", ")
		if len(topics) > 3 {
			topicStr += fmt.Sprintf(" (+%d more)", len(topics)-3)
		}
		fmt.Printf("│ Topics:   %-42s │\n", topicStr)
	}

	if len(memRefs) > 0 {
		fmt.Println("│                                                             │")
		fmt.Println("│ Recent context:                                            │")
		for _, ref := range memRefs {
			content := ref.Content
			if len(content) > 54 {
				content = content[:54] + "…"
			}
			fmt.Printf("│   • %-53s │\n", content)
		}
	}
	if handoff != nil {
		fmt.Println("│                                                             │")
		fmt.Println("│ Previous session handoff:                                  │")
		summary := handoff.Summary
		if len(summary) > 54 {
			summary = summary[:54] + "…"
		}
		fmt.Printf("│   [%s] %-49s │\n", handoff.EndedState, summary)
		if !handoff.EndedAt.IsZero() {
			ts := handoff.EndedAt.Format("2006-01-02 15:04 UTC")
			fmt.Printf("│   ended %s%-40s │\n", ts, "")
		}
		if len(handoff.Commitments) > 0 {
			fmt.Printf("│   %-54s │\n", "commitments:")
			for _, c := range handoff.Commitments {
				if len(c) > 52 {
					c = c[:52] + "…"
				}
				fmt.Printf("│     - %-50s │\n", c)
			}
		}
		if len(handoff.OpenQuestions) > 0 {
			fmt.Printf("│   %-54s │\n", "open:")
			for _, q := range handoff.OpenQuestions {
				if len(q) > 52 {
					q = q[:52] + "…"
				}
				fmt.Printf("│     ? %-50s │\n", q)
			}
		}
	}
	fmt.Println("╰─────────────────────────────────────────────────────────────╯")
	return 0
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
		metaJSON, _ := json.Marshal(mem.Metadata)
		if preamble := mpminternal.ProvenancePreamble(string(metaJSON)); preamble != "" {
			snippet = preamble + "\n" + snippet
		}
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
	store := getMemoryStore()
	if store == nil {
		return respond("", "Failed to initialize memory store", 1)
	}

	rows, err := store.DB.Query(`
		SELECT id, content, created_at
		FROM memories
		WHERE collection = 'session' AND deleted_at IS NULL
		ORDER BY created_at DESC
		LIMIT 20
	`)
	if err != nil {
		return respond("", fmt.Sprintf("Failed to list sessions: %v", err), 1)
	}
	defer rows.Close()

	type sessionItem struct {
		ID      string
		Content string
		Created string
	}
	var sessions []sessionItem
	for rows.Next() {
		var s sessionItem
		if err := rows.Scan(&s.ID, &s.Content, &s.Created); err != nil {
			continue
		}
		sessions = append(sessions, s)
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

	// Validate the mode exists on disk before adding to the active list.
	if !mm.Validate(name) {
		return respond("", fmt.Sprintf("Unknown mode: %s\n", name), 1)
	}

	// Read current active list, append, write back. SetActive() also
	// mirrors the first real mode to config/current_mode for memory injection.
	active, _ := mm.GetActive()
	for _, m := range active {
		if m == name {
			return respond(fmt.Sprintf("Mode already active: %s\n", name), "", 0)
		}
	}
	active = append(active, name)
	if err := mm.SetActive(active); err != nil {
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

	// Read current active list, drop the named one, write back. Does NOT
	// delete the .md file — manage those on the file system. SetActive()
	// also mirrors the next real mode to config/current_mode.
	active, _ := mm.GetActive()
	out := active[:0]
	found := false
	for _, m := range active {
		if m == name {
			found = true
			continue
		}
		out = append(out, m)
	}
	if !found {
		return respond("", fmt.Sprintf("Mode not active: %s\n", name), 1)
	}
	if err := mm.SetActive(out); err != nil {
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
	jsonOutput, filteredArgs := ExtractJSONFlag(args)
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
			"id":            lesson.ID,
			"type":          string(lesson.Type),
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
	jsonOutput, args := ExtractJSONFlag(args)
	for _, arg := range args {
		if strings.HasPrefix(arg, "--type=") {
			lessonType = strings.TrimPrefix(arg, "--type=")
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

	query := args[0]
	jsonOutput, queryArgs := ExtractJSONFlag(args)
	if len(queryArgs) > 0 {
		query = strings.Join(queryArgs, " ")
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
// Status Dashboard — mpm ops status
// ============================================================================

func handleStatus() int {
	dm, err := mpminternal.NewDatabaseManager("")
	if err != nil {
		return respond("", fmt.Sprintf("Error: %v\n", err), 1)
	}
	defer dm.Close()
	printStatusDashboard(dm, startTime)
	return 0
}

func printStatusDashboard(dm *mpminternal.DatabaseManager, startTime time.Time) {
	uptime := formatUptime(time.Since(startTime))
	totalMemories, _ := countMemories(dm, "")
	ltmCount, _ := countMemories(dm, "weight >= 10")
	theoriesCount, _ := countMemories(dm, "collection = 'theories'")
	decisionsCount, _ := countMemories(dm, "collection = 'decisions'")
	activeTheories, _ := countTheoriesByStatus(dm, "pending")
	resolvedTheories, _ := countTheoriesByStatus(dm, "resolved")

	daemonStatus := getDaemonStatus()

	synthCount, lastSynth := getSynthesisStats(dm)

	recentEvents := getRecentWatchdogEvents(dm, 3)

	// Auto-mode transparency
	modeLine := ""
	personaLine := ""
	active, err := loadActiveJSON()
	if err == nil {
		if len(active.Modes) > 0 && active.Modes[0] == "auto" {
			modeLine = "Mode: auto (loaded: auto)"
		} else if len(active.Modes) > 0 {
			modeLine = fmt.Sprintf("Mode: %s", strings.Join(active.Modes, ", "))
		}
		if active.Persona == "auto" {
			personaLine = "Persona: auto (loaded: auto)"
		} else if active.Persona == "ephemeral" {
			if ep, epErr := mpminternal.GetEphemeralPersona(dm); epErr == nil {
				displayName := ep.Name
				if ep.Title != "" {
					displayName = ep.Title
				}
				personaLine = fmt.Sprintf("Persona: auto (loaded: ephemeral - %q)", displayName)
			} else {
				personaLine = "Persona: auto (loaded: ephemeral)"
			}
		} else if active.Persona != "" {
			personaLine = fmt.Sprintf("Persona: %s", active.Persona)
		}
	}

	fmt.Println("⚡ MPM · System Status")
	fmt.Println("────────────────────────────────────")
	fmt.Printf("Uptime:    %s\n", uptime)
	if modeLine != "" {
		fmt.Printf("  %s\n", modeLine)
	}
	if personaLine != "" {
		fmt.Printf("  %s\n", personaLine)
	}
	fmt.Printf("Memories:  %d total | %d LTM\n", totalMemories, ltmCount)
	fmt.Printf("Theories:  %d total | %d pending | %d resolved\n", theoriesCount, activeTheories, resolvedTheories)
	fmt.Printf("Decisions: %d total\n", decisionsCount)
	fmt.Printf("Watcher:   %s\n", daemonStatus)
	fmt.Printf("Synthesis: %d merged | last: %s\n", synthCount, lastSynth)
	if len(recentEvents) > 0 {
		fmt.Println("────────────────────────────────────")
		fmt.Println("Recent events:")
		for _, e := range recentEvents {
			fmt.Printf("  %s %s\n", e.op, e.detail)
		}
	}
	fmt.Println("────────────────────────────────────")
	fmt.Println("Run `mpm help` for daily commands.")
	fmt.Println("Run `mpm ops help` for engine room.")
}

func countMemories(dm *mpminternal.DatabaseManager, where string) (int, error) {
	var query string
	var args []interface{}
	if where == "" {
		query = "SELECT COUNT(*) FROM memories WHERE deleted_at IS NULL"
	} else {
		query = "SELECT COUNT(*) FROM memories WHERE deleted_at IS NULL AND " + where
	}
	var count int
	err := dm.SQLDB().QueryRow(query, args...).Scan(&count)
	return count, err
}

func countTheoriesByStatus(dm *mpminternal.DatabaseManager, status string) (int, error) {
	var count int
	query := `SELECT COUNT(*) FROM memories
		WHERE collection = 'theories' AND deleted_at IS NULL
		AND json_extract(metadata, '$.status') = ?`
	err := dm.SQLDB().QueryRow(query, status).Scan(&count)
	return count, err
}

func getDaemonStatus() string {
	pid := readWatchPID()
	if pid == 0 {
		return "not running"
	}
	if !isWatchProcessAlive(pid) {
		return "not running"
	}
	return fmt.Sprintf("running (PID %d)", pid)
}

func getSynthesisStats(dm *mpminternal.DatabaseManager) (int, string) {
	var count int
	var lastTime string
	dm.SQLDB().QueryRow(`
		SELECT COUNT(*), MAX(json_extract(metadata, '$.synthesized_at'))
		FROM memories
		WHERE deleted_at IS NULL
		AND json_extract(metadata, '$.synthesized') = 'true'
	`).Scan(&count, &lastTime)
	if lastTime == "" {
		lastTime = "never"
	}
	return count, lastTime
}

type watchdogEvent struct {
	op     string
	detail string
}

func getRecentWatchdogEvents(dm *mpminternal.DatabaseManager, limit int) []watchdogEvent {
	path := dm.WatchdogPath()
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	lines := strings.Split(string(data), "\n")
	if len(lines) > limit {
		lines = lines[len(lines)-limit:]
	}
	var events []watchdogEvent
	for _, line := range lines {
		if line == "" {
			continue
		}
		var m map[string]interface{}
		if json.Unmarshal([]byte(line), &m) != nil {
			continue
		}
		op := ""
		if v, ok := m["op"].(string); ok {
			op = v
		}
		detail := ""
		if v, ok := m["reason"].(string); ok {
			detail = v
		} else if v, ok := m["error"].(string); ok {
			detail = v
		}
		if op != "" {
			events = append(events, watchdogEvent{op: op, detail: detail})
		}
	}
	return events
}

// ============================================================================
// Challenge — mpm challenge <id> "<evidence>"
// Implements v1.2 transactional challenge with bidirectional theory links.
func handleChallenge(args []string) int {
	if len(args) < 2 {
		return respond("", "Usage: mpm challenge <id> \"<evidence>\"\n", 1)
	}
	id := args[0]
	evidence := strings.Join(args[1:], " ")

	dm, err := mpminternal.NewDatabaseManager("")
	if err != nil {
		return respond("", fmt.Sprintf("Error: %v\n", err), 1)
	}
	defer dm.Close()

	// Verify memory exists
	mem, err := dm.GetMemory(id)
	if err != nil || mem == nil {
		return respond("", fmt.Sprintf("Memory not found: %s\n", id), 1)
	}

	// Generate theory ID BEFORE saving theory (forward link needed in memory metadata)
	theoryID := mpminternal.GenerateID()
	now := time.Now().Format(time.RFC3339)

	// Build theory content with back-link
	theoryContent := fmt.Sprintf(
		"HYPOTHESIS: Memory %s is obsolete.\nRATIONALE: %s\nSTATUS: pending\nVALIDATION_CRITERIA: Check weight trend over 30 days. If declining and evidence is strong, mark proven.",
		id, evidence)

	// Theory metadata with back-link to memory
	theoryMeta := map[string]interface{}{
		"status":    "pending",
		"type":      "challenge",
		"memory_id": id,
	}
	theoryMetaJSON, _ := json.Marshal(theoryMeta)

	// Memory metadata patch with forward link to theory
	patch := map[string]interface{}{
		"status":               "challenged",
		"challenged_theory_id": theoryID,
	}
	patchJSON, _ := json.Marshal(patch)

	// Transaction: patch memory + save theory atomically
	tx, err := dm.SQLDB().Begin()
	if err != nil {
		return respond("", fmt.Sprintf("Error: %v\n", err), 1)
	}
	defer tx.Rollback()

	// 1. Patch memory metadata with forward link
	result, err := tx.Exec(
		`UPDATE memories SET metadata = json_patch(COALESCE(metadata,'{}'), ?) WHERE id = ?`,
		string(patchJSON), id)
	if err != nil {
		return respond("", fmt.Sprintf("Error patching memory: %v\n", err), 1)
	}
	rowsAff, _ := result.RowsAffected()
	if rowsAff == 0 {
		return respond("", fmt.Sprintf("Memory not found: %s\n", id), 1)
	}

	// 2. Save theory with back-link
	_, err = tx.Exec(
		`INSERT INTO memories (id, collection, content, metadata, created_at, weight) VALUES (?, 'theories', ?, ?, ?, 1)`,
		theoryID, theoryContent, string(theoryMetaJSON), now)
	if err != nil {
		return respond("", fmt.Sprintf("Error saving theory: %v\n", err), 1)
	}

	if err := tx.Commit(); err != nil {
		return respond("", fmt.Sprintf("Error committing transaction: %v\n", err), 1)
	}

	fmt.Printf("⚡ Memory %s challenged.\n", id)
	fmt.Printf("   Memory: %s\n", id)
	fmt.Printf("   Theory: %s\n", theoryID)
	fmt.Printf("   Evidence: %s\n", evidence)
	return 0
}

// handleChallengeRestore — mpm challenge restore <id>
// Resolves the challenged theory and clears the memory's challenged status.
func handleChallengeRestore(args []string) int {
	if len(args) < 2 {
		return respond("", "Usage: mpm challenge restore <memory-id>\n", 1)
	}
	// args[0] is "restore", args[1] is the memory ID
	id := args[1]

	dm, err := mpminternal.NewDatabaseManager("")
	if err != nil {
		return respond("", fmt.Sprintf("Error: %v\n", err), 1)
	}
	defer dm.Close()

	mem, err := dm.GetMemory(id)
	if err != nil || mem == nil {
		return respond("", fmt.Sprintf("Memory not found: %s\n", id), 1)
	}

	metaStr, _ := mem["metadata"].(string)
	var meta map[string]interface{}
	if metaStr != "" {
		json.Unmarshal([]byte(metaStr), &meta)
	}
	theoryID, _ := meta["challenged_theory_id"].(string)

	if theoryID == "" {
		return respond("", fmt.Sprintf("Memory %s is not challenged.\n", id), 1)
	}

	tx, err := dm.SQLDB().Begin()
	if err != nil {
		return respond("", fmt.Sprintf("Error: %v\n", err), 1)
	}
	defer tx.Rollback()

	// 1. Resolve theory: status → disproven, memory_id → null (RFC 7396 null removal)
	resolvePatch := map[string]interface{}{"status": "disproven", "memory_id": nil}
	resolveJSON, _ := json.Marshal(resolvePatch)
	_, err = tx.Exec(
		`UPDATE memories SET metadata = json_patch(COALESCE(metadata,'{}'), ?) WHERE id = ?`,
		string(resolveJSON), theoryID)
	if err != nil {
		return respond("", fmt.Sprintf("Error resolving theory: %v\n", err), 1)
	}

	// 2. Clear challenged status from memory (null removes both keys per RFC 7396)
	clearPatch := map[string]interface{}{"status": nil, "challenged_theory_id": nil}
	clearJSON, _ := json.Marshal(clearPatch)
	_, err = tx.Exec(
		`UPDATE memories SET metadata = json_patch(COALESCE(metadata,'{}'), ?) WHERE id = ?`,
		string(clearJSON), id)
	if err != nil {
		return respond("", fmt.Sprintf("Error clearing memory status: %v\n", err), 1)
	}

	if err := tx.Commit(); err != nil {
		return respond("", fmt.Sprintf("Error committing transaction: %v\n", err), 1)
	}

	fmt.Printf("✓ Theory %s resolved as disproven. Memory %s cleared.\n", theoryID, id)
	return 0
}

// ============================================================================
// Context Switcher — mpm ops switch
// ============================================================================

func handleSwitch(args []string) int {
	stat, _ := os.Stdin.Stat()
	if (stat.Mode() & os.ModeCharDevice) == 0 {
		fmt.Println("[!] Error: Interactive switch requires a TTY. Cannot run in headless/MCP mode.")
		return 1
	}

	active, err := loadActiveJSON()
	if err != nil {
		fmt.Printf("[!] Error loading active.json: %v\n", err)
		return 1
	}

	fmt.Println("⚡ MPM Context Switcher")
	fmt.Println("─────────────────────────────────────────")
	fmt.Printf("Active Persona: %s\n", active.Persona)
	fmt.Printf("Active Modes:  %s\n", strings.Join(active.Modes, ", "))
	fmt.Println("─────────────────────────────────────────")

	fmt.Println("\nWhat do you want to change?")
	fmt.Println("  [1] Switch Persona")
	fmt.Println("  [2] Toggle Modes")
	fmt.Println("  [3] Exit")
	fmt.Print("\n> ")

	reader := bufio.NewReader(os.Stdin)
	line, err := reader.ReadString('\n')
	if err != nil {
		fmt.Println("[!] Read error")
		return 1
	}
	line = strings.TrimSpace(line)

	switch line {
	case "1":
		switchPersona(reader, active)
	case "2":
		toggleModes(reader, active)
	case "3":
		fmt.Println("No changes made.")
		return 0
	default:
		fmt.Println("[!] Invalid option")
		return 1
	}

	active.Updated = time.Now().UTC().Format(time.RFC3339)
	if err := saveActiveJSON(active); err != nil {
		fmt.Printf("[!] Error saving: %v\n", err)
		return 1
	}

	fmt.Printf("\n⚡ Context updated: [Persona: %s] | [Modes: %s]\n",
		active.Persona, strings.Join(active.Modes, ", "))
	return 0
}

func switchPersona(reader *bufio.Reader, active *ActiveState) {
	personas := getPersonaFiles()
	if len(personas) == 0 {
		fmt.Println("[!] No persona files found in persona/")
		return
	}

	fmt.Println("\nAvailable Personas:")
	for i, p := range personas {
		marker := ""
		if p == active.Persona {
			marker = " (current)"
		}
		fmt.Printf("  [%d] %s%s\n", i+1, p, marker)
	}
	fmt.Print("\nSelect persona (number): ")

	line, _ := reader.ReadString('\n')
	line = strings.TrimSpace(line)
	idx, err := strconv.Atoi(line)
	if err != nil || idx < 1 || idx > len(personas) {
		fmt.Println("[!] Invalid selection — no change made.")
		return
	}
	active.Persona = personas[idx-1]
}

func toggleModes(reader *bufio.Reader, active *ActiveState) {
	modes := getModeFiles()
	if len(modes) == 0 {
		fmt.Println("[!] No mode files found in mode/")
		return
	}

	fmt.Println("\nAvailable Modes (enter numbers separated by commas, e.g. 1,3):")
	activeMap := make(map[string]bool)
	for _, m := range active.Modes {
		activeMap[m] = true
	}

	for i, m := range modes {
		marker := ""
		if activeMap[m] {
			marker = " [*]"
		}
		fmt.Printf("  [%d] %s%s\n", i+1, m, marker)
	}
	fmt.Print("\nSelect modes: ")

	line, _ := reader.ReadString('\n')
	line = strings.TrimSpace(line)

	selected := parseModeSelection(line, modes)
	if selected == nil {
		fmt.Println("[!] Invalid selection — no change made.")
		return
	}
	active.Modes = selected
}

func parseModeSelection(line string, modes []string) []string {
	parts := strings.Split(line, ",")
	var result []string
	seen := make(map[string]bool)
	for _, p := range parts {
		p = strings.TrimSpace(p)
		idx, err := strconv.Atoi(p)
		if err != nil || idx < 1 || idx > len(modes) {
			return nil
		}
		name := modes[idx-1]
		if !seen[name] {
			result = append(result, name)
			seen[name] = true
		}
	}
	return result
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
	// Set DM so that write-heavy paths (DecayWeights, DedupeMemories) route
	// through DatabaseManager.ExecTracked for WAL-backoff observability.
	return &internal.MemoryStore{
		DB:         &internal.SQLiteConnection{DB: dbManager.SQLDB()},
		DM:         dbManager,
		MirrorFile: filepath.Join(config.GetMPMDir(), "src", "db", "mirror.jsonl"),
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
		// Pre-warm the synthesis dedup map from past merges so previously
		// resolved pairs are not re-synthesised after a restart.
		internal.InitSynthDedupFromDB(dm)
	}
	watcherCtx, watcherCancel = context.WithCancel(context.Background())
	watcherDone = make(chan struct{})
	watchPool.Start(watcherCtx)

	// Initialize and start the isolated synthesis worker pool.
	// This worker handles all LLM synthesis calls — never blocks the watcher.
	if watchSynthWorker == nil {
		synthClient := internal.NewSynthClient()
		watchSynthWorker = internal.NewSynthesisWorker(watchPool.DM(), synthClient, 3)
		watchSynthWorker.Start()
	}

	// Initialize and start the idle consolidation worker.
	// Fires when no filesystem events for 30 minutes, proposes cross-pattern theories.
	if watchIdleWorker == nil {
		watchIdleWorker = internal.NewIdleConsolidationWorker(watchPool.DM(), 30*time.Minute)
		watchIdleWorker.Start()
	}

	// Write PID file only in detached mode (--bg flag).
	// In in-process goroutine mode the PID would point to the main mpm process,
	// causing stale PID file confusion on abnormal exit.
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

		// Start periodic reconciliation sweep goroutine.
		go startReconciliationSweepGoroutine(watcherCtx, watchPool, false, false)

		// Submit startup events to the pool.
		watchPool.Submit(WatchEvent{Type: EventStartupSweep, DryRun: false, Verbose: false})
		// Immediate reconciliation sweep — catches orphan .jsonl files that the
		// startup sweep skips, closing the 10-minute blind spot before the
		// periodic sweeper fires.
		watchPool.Submit(WatchEvent{Type: EventReconciliationSweep, DryRun: false, Verbose: false})

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

	// Stop the isolated synthesis worker — waits for in-flight LLM calls
	// to complete (or hit their 30s deadline) before returning.
	if watchSynthWorker != nil {
		watchSynthWorker.Stop()
		watchSynthWorker = nil
	}

	// Stop the idle consolidation worker.
	if watchIdleWorker != nil {
		watchIdleWorker.Stop()
		watchIdleWorker = nil
	}
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

	// Retry loop: wait up to 10 seconds for the PID to exit, polling every 200ms.
	// The old 500ms sleep was insufficient when the watcher was in the middle of
	// a long synthesis API call (up to 300s timeout), causing a false "still alive"
	// report and stale PID file.
	for wait := 0; wait < 50; wait++ { // 50 × 200ms = 10s
		time.Sleep(200 * time.Millisecond)
		if !isWatchProcessAlive(pid) {
			deleteWatchPID()
			return respond("Watcher stopped.\n", "", 0)
		}
	}

	// Timed out — process still alive. Clean up PID file so the user can retry
	// without a stale-PID error. The orphan process will be handled on next start.
	deleteWatchPID()
	return respond(fmt.Sprintf("⚠️  Watcher (PID %d) did not stop within 10s — PID file cleaned. You may need to kill it manually.\n", pid), "", 0)
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

// draftInteractiveContent opens $EDITOR on a temp file, waits for the user
// to save+exit, reads the result, shows a preview, and asks for y/N
// confirmation. Returns the drafted content or an empty string if declined.
func draftInteractiveContent() (string, error) {
	tmpFile, err := os.CreateTemp("", "mpm_draft_*.md")
	if err != nil {
		return "", fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmpFile.Name()
	tmpFile.Close()
	defer os.Remove(tmpPath)

	editor := os.Getenv("EDITOR")
	if editor == "" {
		if _, err := exec.LookPath("nano"); err == nil {
			editor = "nano"
		} else if _, err := exec.LookPath("vim"); err == nil {
			editor = "vim"
		} else {
			return "", fmt.Errorf("no editor found — set $EDITOR or install nano/vim")
		}
	}

	cmd := exec.Command(editor, tmpPath)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("editor exited with error: %w", err)
	}

	data, err := os.ReadFile(tmpPath)
	if err != nil {
		return "", fmt.Errorf("read drafted content: %w", err)
	}
	content := strings.TrimSpace(string(data))
	if content == "" {
		return "", nil
	}

	// Preview
	fmt.Printf("\n%s─── Draft Preview ──────────────────────────────%s\n", "\033[1m\033[36m", "\033[0m")
	fmt.Println(content)
	fmt.Printf("%s──────────────────────────────────────────────────%s\n", "\033[1m\033[36m", "\033[0m")
	fmt.Printf("\nSave this memory? [y/N] ")

	var answer string
	fmt.Scanln(&answer)
	answer = strings.TrimSpace(strings.ToLower(answer))
	if answer != "y" && answer != "yes" {
		return "", nil
	}

	return content, nil
}

// ============================================================================
// Epistemology Engine — Theories & Decisions
// ============================================================================

// extractField extracts the value of a named field from structured input text.
// Fields are case-insensitive prefix matches on line starts.
// Multi-line values are supported; field boundaries are detected automatically.
func extractField(input, prefix string) string {
	lines := strings.Split(input, "\n")
	var result []string
	inField := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		upper := strings.ToUpper(trimmed)
		if strings.HasPrefix(upper, prefix) {
			inField = true
			rest := strings.TrimSpace(trimmed[len(prefix):])
			if rest != "" {
				result = append(result, rest)
			}
			continue
		}
		if inField {
			isNewField := false
			for _, p := range []string{"HYPOTHESIS:", "VALIDATION_CRITERIA:", "STATUS:", "TAGS:", "CONTEXT:", "CHOICE:", "RATIONALE:"} {
				if strings.HasPrefix(strings.ToUpper(trimmed), p) {
					isNewField = true
					break
				}
			}
			if isNewField {
				inField = false
			} else {
				result = append(result, trimmed)
			}
		}
	}
	return strings.TrimSpace(strings.Join(result, " "))
}

// handleProposeTheory parses structured text into a theory and saves to the theories collection.
func handleProposeTheory(args []string) int {
	if len(args) == 0 {
		return respond("", "Usage: mpm propose_theory <text>", 1)
	}

	input := strings.Join(args, " ")

	hypothesis := extractField(input, "HYPOTHESIS:")
	validationCriteria := extractField(input, "VALIDATION_CRITERIA:")
	status := extractField(input, "STATUS:")
	tagsStr := extractField(input, "TAGS:")

	if hypothesis == "" {
		hypothesis = strings.TrimSpace(input)
	}
	if status == "" {
		status = "pending"
	}

	var tags []string
	if tagsStr != "" {
		for _, t := range strings.Split(tagsStr, ",") {
			t = strings.TrimSpace(t)
			if t != "" {
				tags = append(tags, t)
			}
		}
	}

	content := hypothesis
	if validationCriteria != "" {
		content += "\n\nVALIDATION_CRITERIA: " + validationCriteria
	}

	meta := map[string]interface{}{
		"status": status,
	}
	if validationCriteria != "" {
		meta["validation_criteria"] = validationCriteria
	}

	store := getMemoryStore()
	if store == nil {
		return respond("", "Error: memory store not available\n", 1)
	}

	mem, err := store.AddMemory(content, "theories", tags, meta, "", "cli")
	if err != nil {
		return respond("", fmt.Sprintf("Failed to save theory: %v\n", err), 1)
	}

	// Auto-link to theories topic (idempotent via INSERT OR IGNORE)
	dm, dmErr := mpminternal.NewDatabaseManager("")
	if dmErr == nil {
		defer dm.Close()
		topicID, tErr := dm.GetOrCreateTopic("theories")
		if tErr == nil {
			dm.AddMemoryToTopic(mem.ID, topicID, "primary")
		}
	}

	return respond("", fmt.Sprintf("✅ Theory proposed: %s (status: %s)\n", mem.ID, status), 0)
}

// handleResolveTheory marks a theory as resolved, updating metadata and bumping weight.
func handleResolveTheory(args []string) int {
	if len(args) < 2 {
		return respond("", "Usage: mpm resolve_theory <id> <conclusion>", 1)
	}

	id := args[0]
	conclusion := strings.Join(args[1:], " ")

	dm, err := mpminternal.NewDatabaseManager("")
	if err != nil {
		return respond("", fmt.Sprintf("Error: %v\n", err), 1)
	}
	defer dm.Close()

	mem, err := dm.GetMemory(id)
	if err != nil {
		return respond("", fmt.Sprintf("Theory not found: %s\n", id), 1)
	}

	coll, _ := mem["collection"].(string)
	if coll != "theories" {
		return respond("", fmt.Sprintf("Memory %s is not a theory (collection: %s)\n", id, coll), 1)
	}

	// Build metadata patch (upserts into existing metadata via json_patch)
	now := time.Now().UTC().Format(time.RFC3339)
	patch := map[string]interface{}{
		"status":      "resolved",
		"conclusion":  conclusion,
		"resolved_at": now,
	}
	patchJSON, _ := json.Marshal(patch)

	if err := dm.UpdateMemoryMetadata(id, string(patchJSON)); err != nil {
		return respond("", fmt.Sprintf("Failed to resolve theory: %v\n", err), 1)
	}

	// Bump weight — reinforces the resolved theory
	dm.ReinforceMemory(id, 1)

	return respond("", fmt.Sprintf("✅ Theory resolved: %s — %s\n", id, conclusion), 0)
}

// handleRecordDecision parses structured decision text and saves to the decisions collection.
func handleRecordDecision(args []string) int {
	if len(args) == 0 {
		return respond("", "Usage: mpm record_decision <text>", 1)
	}

	input := strings.Join(args, " ")

	contextText := extractField(input, "CONTEXT:")
	choice := extractField(input, "CHOICE:")
	rationale := extractField(input, "RATIONALE:")
	tagsStr := extractField(input, "TAGS:")

	if choice == "" {
		choice = strings.TrimSpace(input)
	}

	var tags []string
	if tagsStr != "" {
		for _, t := range strings.Split(tagsStr, ",") {
			t = strings.TrimSpace(t)
			if t != "" {
				tags = append(tags, t)
			}
		}
	}

	content := "CHOICE: " + choice
	if contextText != "" {
		content += "\nCONTEXT: " + contextText
	}
	if rationale != "" {
		content += "\nRATIONALE: " + rationale
	}

	meta := map[string]interface{}{}
	if contextText != "" {
		meta["context"] = contextText
	}
	if rationale != "" {
		meta["rationale"] = rationale
	}

	store := getMemoryStore()
	if store == nil {
		return respond("", "Error: memory store not available\n", 1)
	}

	mem, err := store.AddMemory(content, "decisions", tags, meta, "", "cli")
	if err != nil {
		return respond("", fmt.Sprintf("Failed to record decision: %v\n", err), 1)
	}

	// Auto-link to decisions topic
	dm, dmErr := mpminternal.NewDatabaseManager("")
	if dmErr == nil {
		defer dm.Close()
		topicID, tErr := dm.GetOrCreateTopic("decisions")
		if tErr == nil {
			dm.AddMemoryToTopic(mem.ID, topicID, "primary")
		}
	}

	return respond("", fmt.Sprintf("✅ Decision recorded: %s\n", mem.ID), 0)
}

// handleTheories lists theories with status chips. Supports filter: all, pending, resolved.
func handleTheories(args []string) int {
	filter := "all"
	if len(args) > 0 {
		filter = args[0]
	}

	dm, err := mpminternal.NewDatabaseManager("")
	if err != nil {
		return respond("", fmt.Sprintf("Error: %v\n", err), 1)
	}
	defer dm.Close()

	memories, err := dm.GetMemoriesForExport("theories", "", "")
	if err != nil {
		return respond("", fmt.Sprintf("Error: %v\n", err), 1)
	}

	if len(memories) == 0 {
		return respond("", "No theories yet. Run `mpm propose_theory` to propose your first theory.\n", 0)
	}

	count := 0
	for _, m := range memories {
		content, _ := m["content"].(string)
		id, _ := m["id"].(string)

		var meta map[string]interface{}
		metaStr, _ := m["metadata"].(string)
		json.Unmarshal([]byte(metaStr), &meta)

		status, _ := meta["status"].(string)
		if status == "" {
			status = "pending"
		}

		if filter != "all" && status != filter {
			continue
		}

		display := strings.SplitN(content, "\n", 2)[0]
		if len(display) > 80 {
			display = display[:80] + "..."
		}

		fmt.Printf("[%s] %s  [status: %s]\n", id, display, status)
		count++

		if vc, ok := meta["validation_criteria"].(string); ok && vc != "" {
			vcDisplay := vc
			if len(vcDisplay) > 60 {
				vcDisplay = vcDisplay[:60] + "..."
			}
			fmt.Printf("      validation: %s\n", vcDisplay)
		}
	}

	if count == 0 {
		fmt.Printf("No %s theories found.\n", filter)
	}

	return 0
}

// backfillEpistemologyTopics links existing theories/decisions memories to their topics.
// Idempotent: AddMemoryToTopic uses INSERT OR IGNORE so it is safe to call repeatedly.
func backfillEpistemologyTopics() {
	dm, err := mpminternal.NewDatabaseManager("")
	if err != nil {
		return
	}
	defer dm.Close()

	rows, err := dm.SQLDB().Query(
		`SELECT id, collection FROM memories WHERE collection IN ('theories', 'decisions') AND deleted_at IS NULL`,
	)
	if err != nil {
		return
	}
	defer rows.Close()

	linked := 0
	now := time.Now().UTC().Format(time.RFC3339)
	for rows.Next() {
		var id, collection string
		if err := rows.Scan(&id, &collection); err != nil {
			continue
		}
		topicID, tErr := dm.GetOrCreateTopic(collection)
		if tErr != nil {
			continue
		}
		res, execErr := dm.SQLDB().Exec(
			`INSERT OR IGNORE INTO topic_memberships (memory_id, topic_id, created_at, role) VALUES (?, ?, ?, ?)`,
			id, topicID, now, "primary",
		)
		if execErr == nil {
			if ra, _ := res.RowsAffected(); ra > 0 {
				linked++
			}
		}
	}

	if linked > 0 {
		fmt.Printf("📚 Epistemology topics initialized: %d memories linked\n", linked)
	}
}

// handleDecisions displays the decision ledger with context, choice, and rationale for each entry.
func handleDecisions(args []string) int {
	dm, err := mpminternal.NewDatabaseManager("")
	if err != nil {
		return respond("", fmt.Sprintf("Error: %v\n", err), 1)
	}
	defer dm.Close()

	memories, err := dm.GetMemoriesForExport("decisions", "", "")
	if err != nil {
		return respond("", fmt.Sprintf("Error: %v\n", err), 1)
	}

	if len(memories) == 0 {
		return respond("", "No decisions recorded yet. Run `mpm record_decision` to log your first decision.\n", 0)
	}

	for _, m := range memories {
		content, _ := m["content"].(string)
		createdAt, _ := m["created_at"].(string)

		var meta map[string]interface{}
		metaStr, _ := m["metadata"].(string)
		json.Unmarshal([]byte(metaStr), &meta)

		contextText, _ := meta["context"].(string)
		rationale, _ := meta["rationale"].(string)

		choice := strings.SplitN(content, "\n", 2)[0]
		if strings.HasPrefix(strings.ToUpper(choice), "CHOICE: ") {
			choice = strings.TrimSpace(choice[7:])
		}

		dateStr := createdAt
		if len(dateStr) >= 10 {
			dateStr = dateStr[:10]
		}

		fmt.Println("─────────────────────")
		if contextText != "" {
			fmt.Printf("CONTEXT:  %s\n", contextText)
		} else {
			fmt.Println("CONTEXT:  —")
		}
		fmt.Printf("CHOICE:   %s\n", choice)
		if rationale != "" {
			fmt.Printf("RATIONALE: %s\n", rationale)
		} else {
			fmt.Println("RATIONALE: —")
		}
		fmt.Printf("[%s]\n", dateStr)
	}
	fmt.Println("─────────────────────")

	return 0
}

// handleHint checks recent conversation context for epistemologically relevant
// memories (theories and decisions). Supports --json and --max <n> flags.
func handleHint(args []string) int {
	// Parse --max and --json flags
	maxHints := 1
	jsonOutput := false
	cleanArgs := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--json":
			jsonOutput = true
		case "--max":
			if i+1 < len(args) {
				i++
				n, err := strconv.Atoi(args[i])
				if err == nil && n > 0 {
					maxHints = n
				}
			}
		default:
			cleanArgs = append(cleanArgs, args[i])
		}
	}

	if len(cleanArgs) == 0 {
		return respond("", "Usage: mpm hint [--json] [--max N] <conversation text>\n", 1)
	}

	conversationText := strings.Join(cleanArgs, " ")

	keywords := internal.ExtractConversationKeywords(conversationText, 50)

	dm, err := mpminternal.NewDatabaseManager("")
	if err != nil {
		return respond("", fmt.Sprintf("Error: %v\n", err), 1)
	}
	defer dm.Close()

	retrievalLimit := maxHints
	retrievalThreshold := -3.0
	if active, err := loadActiveJSON(); err == nil {
		mm := mpminternal.NewModeManager(config.GetMPMDir())
		for _, name := range active.Modes {
			if m, err := mm.Get(name); err == nil {
				retrievalLimit = m.RetrievalLimit
				retrievalThreshold = m.RetrievalThreshold
				break
			}
		}
	}
	if maxHints > 0 && maxHints < retrievalLimit {
		retrievalLimit = maxHints
	}

	overlaps, err := internal.FindEpistemologyOverlaps(dm, keywords, retrievalLimit, retrievalThreshold)
	if err != nil {
		return respond("", fmt.Sprintf("Error: %v\n", err), 1)
	}

	if len(overlaps) == 0 {
		if jsonOutput {
			return respond("", "[]\n", 0)
		}
		return respond("", "", 0)
	}

	if jsonOutput {
		out, _ := json.MarshalIndent(overlaps, "", "  ")
		return respond("", string(out)+"\n", 0)
	}

	// Regular mode: show first hint only, formatted
	hint := overlaps[0]
	collection, _ := hint["collection"].(string)
	content, _ := hint["content"].(string)
	createdAt, _ := hint["created_at"].(string)
	if len(createdAt) >= 10 {
		createdAt = createdAt[:10]
	}
	meta, _ := hint["metadata"].(map[string]interface{})

	var status string
	if meta != nil {
		if s, ok := meta["status"].(string); ok {
			status = s
		}
	}
	if status == "" {
		for _, line := range strings.Split(content, "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(strings.ToUpper(trimmed), "STATUS:") {
				status = strings.TrimSpace(trimmed[7:])
				break
			}
		}
	}

	switch collection {
	case "decisions":
		choice := strings.SplitN(content, "\n", 2)[0]
		if strings.HasPrefix(strings.ToUpper(choice), "CHOICE: ") {
			choice = strings.TrimSpace(choice[7:])
		}
		var rationale string
		for _, line := range strings.Split(content, "\n") {
			if strings.HasPrefix(strings.ToUpper(line), "RATIONALE:") {
				rationale = strings.TrimSpace(line[10:])
				break
			}
		}
		fmt.Printf("[Recall] You decided: %s\n", choice)
		if status != "" {
			fmt.Printf("  STATUS: %s | %s\n", status, createdAt)
		}
		if rationale != "" {
			fmt.Printf("  RATIONALE: %s\n", rationale)
		}
	case "theories":
		hypothesis := strings.SplitN(content, "\n", 2)[0]
		if strings.HasPrefix(strings.ToUpper(hypothesis), "HYPOTHESIS: ") {
			hypothesis = strings.TrimSpace(hypothesis[11:])
		}
		fmt.Printf("[Recall] Hypothesis: %s\n", hypothesis)
		if meta != nil {
			if conclusion, ok := meta["conclusion"].(string); ok && conclusion != "" {
				fmt.Printf("  STATUS: %s | %s\n", status, createdAt)
				fmt.Printf("  CONCLUSION: %s\n", conclusion)
			} else {
				fmt.Printf("  STATUS: %s | %s\n", status, createdAt)
			}
		} else {
			fmt.Printf("  STATUS: %s | %s\n", status, createdAt)
		}
	}

	return 0
}

// ============================================================================
// XITL Routing Engine — Context Sensing & JIT Personas
// ============================================================================

// handleStance routes stance subcommands (assume / synthesize).
func handleStance(args []string) int {
	if len(args) < 1 {
		return respond("", "Usage: mpm ops stance assume|synthesize ...\n", 1)
	}
	sub := args[0]
	switch sub {
	case "assume":
		return handleStanceAssume(args[1:])
	case "synthesize":
		return handleStanceSynthesize(args[1:])
	default:
		return respond("", "Usage: mpm ops stance assume|synthesize ...\n", 1)
	}
}

// handleStanceAssume hot-swaps an existing persona when auto is active.
// CLI: mpm ops stance assume <mode> <persona> <rationale>
func handleStanceAssume(args []string) int {
	if len(args) < 3 {
		return respond("", "Usage: mpm ops stance assume <mode> <persona> <rationale>\n", 1)
	}
	mode := args[0]
	persona := args[1]
	rationale := strings.Join(args[2:], " ")

	if status := mpminternal.CheckAutoActive(); !status.Active {
		return respond("", fmt.Sprintf("Error: cannot assume stance — %s\n", status.Reason), 1)
	}

	dm, err := mpminternal.NewDatabaseManager("")
	if err != nil {
		return respond("", fmt.Sprintf("Error: %v\n", err), 1)
	}
	defer dm.Close()

	// Collision rule: clear any existing ephemeral_persona
	mpminternal.DeleteEphemeralPersona(dm)

	// Read active.json
	active, err := loadActiveJSON()
	if err != nil {
		return respond("", fmt.Sprintf("Error reading active.json: %v\n", err), 1)
	}

	// Update mode/persona; leave the unspecified one as-is
	if mode != "-" {
		active.Modes = []string{mode}
	}
	if persona != "-" {
		active.Persona = persona
	}
	active.Updated = time.Now().UTC().Format(time.RFC3339)
	if err := saveActiveJSON(active); err != nil {
		return respond("", fmt.Sprintf("Error saving active.json: %v\n", err), 1)
	}

	// Audit trail: log to decisions table
	auditContent := fmt.Sprintf("CONTEXT: Stance hot-swap to %s/%s. RATIONALE: %s. CHOICE: assume_stance", mode, persona, rationale)
	dm.SaveMemory("decisions", auditContent, "", nil, nil, nil, false, 1)

	// Inject new directive into LLM context mid-session via stdout.
	// OpenClaw captures tool stdout and injects it into the session chat history,
	// so 808 reads this on the very next turn and hot-swaps without restart.
	directive := GetSystemPrompt()
	fmt.Print("\n[SYSTEM NOTIFICATION: STANCE HOT-SWAPPED]\n")
	fmt.Print("You must immediately adopt the following Mode and Persona directives for the remainder of this session:\n\n")
	fmt.Print(directive)
	fmt.Print("\n")

	return respond(fmt.Sprintf("Stance assumed: mode=%s persona=%s\n", mode, persona), "", 0)
}

// handleStanceSynthesize generates a JIT (ephemeral) persona when auto is active.
// CLI: mpm ops stance synthesize <name> --title <t> --creature <c> --vibe <v> --voice <v> --anti-patterns <a>
func handleStanceSynthesize(args []string) int {
	if len(args) < 1 {
		return respond("", "Usage: mpm ops stance synthesize <name> [flags]\n", 1)
	}

	name := args[0]
	title := ""
	creature := ""
	vibe := ""
	voice := ""
	antiPatterns := ""

	i := 1
	for i < len(args) {
		switch args[i] {
		case "--title":
			if i+1 < len(args) {
				i++
				title = args[i]
			}
		case "--creature":
			if i+1 < len(args) {
				i++
				creature = args[i]
			}
		case "--vibe":
			if i+1 < len(args) {
				i++
				vibe = args[i]
			}
		case "--voice":
			if i+1 < len(args) {
				i++
				voice = args[i]
			}
		case "--anti-patterns":
			if i+1 < len(args) {
				i++
				antiPatterns = args[i]
			}
		}
		i++
	}

	if status := mpminternal.CheckAutoActive(); !status.Active {
		return respond("", fmt.Sprintf("Error: cannot synthesize stance — %s\n", status.Reason), 1)
	}

	dm, err := mpminternal.NewDatabaseManager("")
	if err != nil {
		return respond("", fmt.Sprintf("Error: %v\n", err), 1)
	}
	defer dm.Close()

	ep := &mpminternal.EphemeralPersona{
		Name:         name,
		Title:        title,
		Creature:     creature,
		Vibe:         vibe,
		Voice:        voice,
		AntiPatterns: antiPatterns,
	}

	// Idempotent upsert into system_config
	if err := mpminternal.SaveEphemeralPersona(dm, ep); err != nil {
		return respond("", fmt.Sprintf("Error saving ephemeral persona: %v\n", err), 1)
	}

	// Update active.json to point to ephemeral
	active, err := loadActiveJSON()
	if err != nil {
		return respond("", fmt.Sprintf("Error reading active.json: %v\n", err), 1)
	}
	active.Persona = "ephemeral"
	active.Updated = time.Now().UTC().Format(time.RFC3339)
	if err := saveActiveJSON(active); err != nil {
		return respond("", fmt.Sprintf("Error saving active.json: %v\n", err), 1)
	}

	displayName := name
	if title != "" {
		displayName = title
	}

	// Inject new directive into LLM context mid-session via stdout.
	// OpenClaw captures tool stdout and injects it into the session chat history,
	// so 808 reads this on the very next turn and hot-swaps without restart.
	directive := GetSystemPrompt()
	fmt.Print("\n[SYSTEM NOTIFICATION: STANCE HOT-SWAPPED]\n")
	fmt.Print("You must immediately adopt the following Mode and Persona directives for the remainder of this session:\n\n")
	fmt.Print(directive)
	fmt.Print("\n")

	return respond(fmt.Sprintf("Synthesized ephemeral persona: %q (active: ephemeral)\n", displayName), "", 0)
}

// handleOpsPromote promotes an ephemeral persona to a permanent persona file.
// CLI: mpm ops promote
// Atomicity: 1) fetch ephemeral_persona 2) write file 3) clear sysconfig 4) update active.json
func handleOpsPromote() int {
	dm, err := mpminternal.NewDatabaseManager("")
	if err != nil {
		return respond("", fmt.Sprintf("Error: %v\n", err), 1)
	}
	defer dm.Close()

	ep, err := mpminternal.GetEphemeralPersona(dm)
	if err != nil {
		return respond("", "No ephemeral persona found. Nothing to promote.\n", 0)
	}

	// Format as markdown and write to personas/<name>.md
	md := mpminternal.FormatEphemeralPersonaAsMarkdown(ep)
	personaDir := filepath.Join(config.GetMPMDir(), "persona")
	if err := os.MkdirAll(personaDir, 0755); err != nil {
		return respond("", fmt.Sprintf("Error creating persona directory: %v\n", err), 1)
	}
	path := filepath.Join(personaDir, ep.Name+".md")
	if err := os.WriteFile(path, []byte(md), 0644); err != nil {
		return respond("", fmt.Sprintf("Error writing persona file: %v\n", err), 1)
	}

	// Only clear ephemeral_persona if file write succeeded
	if err := mpminternal.DeleteEphemeralPersona(dm); err != nil {
		// Log warning but don't fail — the file was written successfully
		fmt.Fprintf(os.Stderr, "Warning: failed to clear ephemeral_persona: %v\n", err)
	}

	// Update active.json to point to the newly permanent persona
	active, err := loadActiveJSON()
	if err != nil {
		return respond("", fmt.Sprintf("Error reading active.json: %v\n", err), 1)
	}
	active.Persona = ep.Name
	active.Updated = time.Now().UTC().Format(time.RFC3339)
	if err := saveActiveJSON(active); err != nil {
		return respond("", fmt.Sprintf("Error saving active.json: %v\n", err), 1)
	}

	return respond(fmt.Sprintf("Promoted ephemeral persona %q → %s\n", ep.Name, path), "", 0)
}
