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
	output := `mpm shred - Remove objects from active MPM state

Scope of "shred":
  shred removes records from ACTIVE MPM state and runs the cascades
  defined for that object type. It does not erase bytes. The following
  are outside its guarantee and may still contain the content:
    - system_audit_log rows
    - mirror.jsonl / watchdog.jsonl, including rotated (.gz) copies
    - database backups and "mpm backup" dumps
    - SQLite WAL, free pages, and filesystem snapshots
  MPM has no secure-erasure capability. Only "uninstall.sh --shred"
  attempts a (still best-effort) overwrite of files on disk.

Usage:
  mpm shred <id>             Shred one memory by ID (permanent from active state)
  mpm shred topic <id>       Delete a topic and its memberships (no confirmation prompt)
  mpm shred session <id>     Soft-delete a session by ID (recoverable; no confirmation prompt)

Bulk targets (all sessions, all memories, all topics, the whole
database, all modes, all personas) require -f/--force to confirm.
That confirmation is currently unreachable: the router's global flag
parser consumes -f/--force before these handlers see it, so they
always abort with the warning below. Pre-existing defect; tracked
separately. Nothing is deleted in that state.

  mpm shred sessions         Delete all sessions (bulk)
  mpm shred memories         Delete all memories (bulk)
  mpm shred topics           Delete all topics (bulk)
  mpm shred database         Delete entire database file and recreate (bulk)
  mpm shred modes            Delete all modes (bulk)
  mpm shred personas         Delete all personas (bulk)

Examples:
  mpm shred abc123
  mpm shred topic t-abc123
  mpm shred session xyz789
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
		return respond("", "Refusing: 'mpm shred sessions' would delete ALL sessions.\nConfirmation is currently unavailable: the router's global flag parser consumes\n-f/--force before this handler sees it, so no invocation can confirm. Pre-existing\ndefect; tracked separately. Nothing was deleted.\n", 1)
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
		return respond("", "Refusing: 'mpm shred memories' would delete ALL memories.\nConfirmation is currently unavailable: the router's global flag parser consumes\n-f/--force before this handler sees it, so no invocation can confirm. Pre-existing\ndefect; tracked separately. Nothing was deleted.\n", 1)
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
		return respond("", "Refusing: 'mpm shred topics' would delete ALL topics.\nConfirmation is currently unavailable: the router's global flag parser consumes\n-f/--force before this handler sees it, so no invocation can confirm. Pre-existing\ndefect; tracked separately. Nothing was deleted.\n", 1)
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
		return respond("", "Refusing: 'mpm shred database' would delete the entire database file and recreate it.\nConfirmation is currently unavailable: the router's global flag parser consumes\n-f/--force before this handler sees it, so no invocation can confirm. Pre-existing\ndefect; tracked separately. Nothing was deleted.\n", 1)
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
		return respond("", "Refusing: 'mpm shred modes' would delete ALL modes.\nConfirmation is currently unavailable: the router's global flag parser consumes\n-f/--force before this handler sees it, so no invocation can confirm. Pre-existing\ndefect; tracked separately. Nothing was deleted.\n", 1)
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
		return respond("", "Refusing: 'mpm shred personas' would delete ALL personas.\nConfirmation is currently unavailable: the router's global flag parser consumes\n-f/--force before this handler sees it, so no invocation can confirm. Pre-existing\ndefect; tracked separately. Nothing was deleted.\n", 1)
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
