package main

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	"github.com/flowbyte-com/mpm-core"
)

// ============================================================================
// Handler: shred (topic, session, memory - same options)
// ============================================================================

func handleShred(args []string) int {
	// Defect class (2026-09-13 acceptance): `mpm shred --help` used
	// to reach this handler with args[0]="help" treated as the
	// target type, then fall through to a non-existent case and
	// surface an opaque error. requireHelpShortCircuit routes --help
	// to the shred help page and exits 0.
	if requireHelpShortCircuit(args, func() {
		handleShredHelp()
	}) {
		return 0
	}
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
			// Bare ID with no subcommand — the primary dispatcher routes
			// `mpm shred <id>` here for memory shred (router.go:298).
			// Routing to handleShredHelp returns exit 0, which silently
			// tells the user a shred happened when it didn't — a trap the
			// hygiene pass hit live. Route unknown bare targets to the
			// memory-shred path so a typo'd/bogus id fails loudly instead.
			return handleShredMem(append([]string{"shred"}, args...))
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
