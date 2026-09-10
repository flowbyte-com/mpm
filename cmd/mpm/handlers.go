package main

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/flowbyte-com/mpm-core"
	"github.com/flowbyte-com/mpm-core/config"
	"github.com/flowbyte-com/mpm-core/usererror"
)

// Package-level singleton DatabaseManager — initialized once per process,
// shared across all handler calls to avoid connection proliferation.
var dbManager *internal.DatabaseManager
var dbManagerInitErr error
var dbManagerOnce sync.Once

// activeContext holds the mode/persona for the current CLI invocation.
// Set at the start of each handler via detectActiveContext(), cleared after use.
var activeMode string
var activePersona string

// detectActiveContext reads the current mode and persona from config files.
// These values are injected into memory metadata on every AddMemory call.
//
// v spec 2026-08-04: each requested name is run through
// internal.ResolveActivePersona / ResolveActiveMode so a stale
// legacy config/current_* file pointing at a deleted .md file falls
// back to system/standard instead of silently injecting a dangling
// name into memory metadata.
func detectActiveContext() (mode, persona string) {
	dm := getDBConcrete()
	modePath := filepath.Join(config.GetMPMDir(), "config", "current_mode")
	if data, err := os.ReadFile(modePath); err == nil {
		rawMode := strings.TrimSpace(string(data))
		mode = internal.ResolveActiveMode(dm, rawMode)
	}
	personaPath := filepath.Join(config.GetMPMDir(), "config", "current_persona")
	if data, err := os.ReadFile(personaPath); err == nil {
		rawPersona := strings.TrimSpace(string(data))
		persona = internal.ResolveActivePersona(dm, rawPersona)
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
	case "delete":
		// Alpha cleanup (2026-09-10): explicit soft-delete verb.
		// Reversible via `mpm memory restore <id>`. Distinct from
		// `mpm memory shred` (hard delete — irreversible).
		return handleMemoryDelete(args[1:])
	case "restore":
		// Alpha cleanup (2026-09-10): reverses a prior `mpm memory delete`
		// by clearing the deleted_at tombstone. Distinct from
		// `mpm challenge restore` (which only clears the challenged
		// status flag, not the deleted_at tombstone).
		return handleMemoryRestore(args[1:])
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
  mpm memory add [--fact <text>] [--tags <csv>] [--weight <0-100>]
                  [--expires-in <duration>] [-i|--interactive] [--json]
                  <content>
                  Add a new memory (returns ID)
  mpm memory search <query>      Search memories
  mpm memory search-term <term>  List memories matching term (500 char snippets)
  mpm memory show <id>           Show memory by ID
  mpm memory delete <id>         Soft-delete memory by ID (reversible via restore)
  mpm memory restore <id>        Restore a soft-deleted memory by ID
  mpm memory shred <id>          Permanent destruction of memory by ID (irreversible)
  mpm memory list                List recent memories
  mpm memory wipe                Wipe all memories (requires -f)

Flags for "mpm memory add":
  --fact <text>         Memory content (alternative to positional arg;
                        matches the mpm_memory action=save contract)
  --tags <csv>          Comma-separated tags, e.g. --tags alpha,beta
  --weight <0-100>      Weight (default 1)
  --expires-in <dur>    Time-to-live (e.g. 7d, 24h)
  -i, --interactive     Compose content via draft prompt
  --json, -j            Emit JSON output including tags + weight for
                        verification

Examples:
  mpm memory add "Remember to call mom"
  mpm memory add --fact "Project: alpha is shipped" --tags alpha,shipped --weight 50
  mpm memory add --fact "Expires in 7 days" --expires-in 7d
  mpm memory search "mom"
  mpm memory search-term "project"
  mpm memory show abc123
  mpm memory shred abc123
`
	return respond(output, "", 0)
}

// ============================================================================
// Utility Functions
// ============================================================================

// parseModeSelection parses a comma-separated list of 1-based mode indices.
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

// closeDB closes the DatabaseManager and logs any error instead of discarding it.
func closeDB(dm *internal.DatabaseManager) {
	if dm == nil {
		return
	}
	if err := dm.Close(); err != nil {
		usererror.Warn("database close error: %v", err)
	}
}

// closeSQLDB closes a raw *sql.DB and logs any error. Mirrors closeDB for
// DatabaseManager (LOW-03 fix). Used by handlers that open transient
// connections outside the singleton (backup/restore-db/wal-flush).
func closeSQLDB(db *sql.DB) {
	if db == nil {
		return
	}
	if err := db.Close(); err != nil {
		usererror.Warn("sql.DB close error: %v", err)
	}
}

// getDB returns the shared DatabaseManager, initializing it once on first call.
func getDB() internal.CoreDB {
	dbManagerOnce.Do(func() {
		dm, err := internal.NewDatabaseManager("")
		if err != nil {
			dbManagerInitErr = err
			return
		}
		dbManager = dm
	})
	if dbManagerInitErr != nil {
		usererror.Error("opening database: %v", dbManagerInitErr)
		return nil
	}
	return dbManager
}

// getDBConcrete returns the shared *DatabaseManager singleton, suitable
// for call sites that need the concrete type (e.g. legacy helpers like
// mpminternal.AddEvidence, mpminternal.HybridSearch, and printStatusDashboard
// that take *DatabaseManager rather than the CoreDB interface). The CoreDB
// interface is a per-component ownership abstraction; cmd/mpm's CLI handlers
// predate that layer and were written against the concrete type.
//
// The singleton is always a *DatabaseManager (set inside getDB()'s once
// initializer), so the type assertion is safe; the nil check guards it.
func getDBConcrete() *internal.DatabaseManager {
	if db := getDB(); db != nil {
		return db.(*internal.DatabaseManager)
	}
	return nil
}

// getMemoryStore returns a MemoryStore backed by the shared database manager.
// Lazily initializes dbManager if nil.
func getMemoryStore() *internal.MemoryStore {
	dm := getDB()
	if dm == nil {
		return nil
	}
	// Wrap the shared *sql.DB in a SQLiteConnection to satisfy MemoryStore.DB.
	// Set DM so that write-heavy paths (DecayWeights, DedupeMemories) route
	// through DatabaseManager.ExecTracked for WAL-backoff observability.
	return &internal.MemoryStore{
		DB:         &internal.SQLiteConnection{DB: dm.SQLDB()},
		DM:         dm,
		MirrorFile: filepath.Join(config.GetMPMDir(), "src", "db", "mirror.jsonl"),
	}
}

// datePrefix returns the first 10 chars (YYYY-MM-DD) of the input
// string if present, else the whole string. Used for legacy
// string-typed Created fields (sessions listing).
func datePrefix(s string) string {
	if len(s) >= 10 {
		return s[:10]
	}
	return s
}

// datePrefixSec returns the first 10 chars (YYYY-MM-DD) of the
// RFC3339 rendering of an int64 Unix-epoch seconds value. Used for
// the post-migration int64 CreatedAt fields (memories listing).
func datePrefixSec(sec int64) string {
	s := time.Unix(sec, 0).UTC().Format(time.RFC3339)
	if len(s) >= 10 {
		return s[:10]
	}
	return s
}

// min returns the smaller of two ints (Go 1.21+ has built-in; kept for clarity).
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

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
