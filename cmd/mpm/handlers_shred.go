package main

import (
	"database/sql"
	"fmt"

	"github.com/flowbyte-com/mpm-core"
)

// Messages for the bulk shred forms that are deliberately unavailable.
//
// Each states what the form actually does, why it is not offered, and
// what to use instead. The point is that an operator who types one of
// these learns the truth about MPM's behaviour rather than being told
// to re-run a command that will never work, or that their memories were
// deleted when they were not.
const (
	shredSessionsDisabledMsg = `Refusing: 'mpm shred sessions' is not available.

It would not delete anything. The underlying operation renames the
'sessions' collection to 'sessions_inactive' and sets a metadata flag;
every row, FTS entry and topic membership stays in the database and
remains searchable. Reporting that as "all sessions deleted" would be
false, so the form is disabled. -f and --force do not change this.

To remove a session for real, shred it by ID:
  mpm memory shred <id>

Or uninstall MPM state entirely:
  ./uninstall.sh --purge      (remove state)
  ./uninstall.sh --shred      (overwrite files first, then remove)
`

	shredMemoriesDisabledMsg = `Refusing: 'mpm shred memories' is not available.

It would not delete anything. The underlying operation renames every
collection to '<name>_inactive' and sets a metadata flag; every memory
row, FTS entry and topic membership stays in the database and remains
searchable. Reporting that as "all memories deleted" would be false, so
the form is disabled. -f and --force do not change this.

To remove a memory for real, shred it by ID — this is the supported
per-object path, and it runs the full cascade sweep:
  mpm shred <id>
  mpm memory shred <id>

Or uninstall MPM state entirely:
  ./uninstall.sh --purge      (remove state)
  ./uninstall.sh --shred      (overwrite files first, then remove)
`

	shredDatabaseDisabledMsg = `Refusing: 'mpm shred database' is not available.

It would destroy the database and fail to rebuild it. The recreation
step calls internal.NewMemoryStore, which discards its path argument and
never opens a database, so the command would delete mpm.db, find that
the "new" store has no DB, and exit with the workspace holding no
database at all. It would also have ignored the -wal/-shm sidecars,
telemetry.db, the mirror and watchdog logs, and every backup — deleting
the database while leaving the content in all of those.

-f and --force do not change this.

To remove MPM state, use the uninstaller, which is complete at that
scope and is the only path that also covers backups and logs:
  ./uninstall.sh --purge      (remove all persistent state)
  ./uninstall.sh --shred      (best-effort overwrite, then remove)
`
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

Available (no confirmation prompt — one object per invocation):
  mpm shred <id>             Shred one memory by ID (permanent from active state)
  mpm shred topic <id>       Delete a topic and its memberships
  mpm shred session <id>     Soft-delete a session by ID (recoverable, NOT a shred)

Available (bulk; -f required, otherwise nothing is deleted):
  mpm shred topics -f        Delete all topics and all topic memberships
  mpm shred modes -f         Delete all mode files
  mpm shred personas -f      Delete all persona files
  mpm shred database -f      Reset the active substrate; retain previous
                              as mpm.db.pre-shred-<nanos>; preserve logs,
                              backups, telemetry, mode/, persona/

  Note: mode/ and persona/ are git-tracked source in the canonical
  ~/.mpm checkout, so the last two refuse there. Use ` + "`git rm`" + ` for tracked files.

Not available (-f does not enable them):
  mpm shred sessions         Would only rename the collection, not delete it
  mpm shred memories         Would only rename the collection, not delete it

  Each prints why when invoked. To remove MPM state entirely use:
    ./uninstall.sh --purge    (remove all persistent state)
    ./uninstall.sh --shred    (best-effort overwrite, then remove)

Examples:
  mpm shred abc123
  mpm shred topic t-abc123
  mpm shred topics -f
`
	return respond(output, "", 0)
}

// handleShredSessions is DISABLED and refuses unconditionally.
//
// It is not waiting for a confirmation flag — the operation it would
// perform is not a deletion. MemoryStore.DeleteAllByCollection
// (internal/core/memory.go:2593) issues an UPDATE that renames
// `sessions` to `sessions_inactive` and sets a metadata flag. Every
// row, every FTS entry and every topic membership stays in the
// database, and the content remains fully searchable (verified: a
// single-token FTS query matches every renamed row). The only thing
// removed is the collection from the default listing path.
//
// Activating it would print "All sessions deleted (N records)" while
// deleting nothing, which is worse than the command being unavailable.
// Turning it into a real delete is a redesign, not a plumbing fix: it
// would need the broad sweep and cascade handling that the per-ID
// shred path already implements.
func handleShredSessions(args []string) int {
	return respond("", shredSessionsDisabledMsg, 1)
}

func handleShredMemories(args []string) int {
	return respond("", shredMemoriesDisabledMsg, 1)
}

// handleShredDatabase is the CLI entry point for the active-substrate
// reset. The real implementation lives in handlers_shred_database_reset.go.
// This file owns the disabled-form message constant only — the
// historical refusal stub was replaced by the safe reset on 2026-10-04.
//
// The disabled-form message constant `shredDatabaseDisabledMsg` is
// kept verbatim for any reader that still links to it from
// documentation or older tests. New tests assert the live contract
// defined in handlers_shred_database_reset.go.

func handleShredTopics(args []string) int {
	if !forceRequested() {
		return respond("", "Refusing: 'mpm shred topics' would delete ALL topics and every\n"+
			"topic membership (including session-linked ones). Nothing was deleted.\n"+
			"Re-run with -f to confirm.\n", 1)
	}

	store := getMemoryStore()
	db := store.DB

	// One transaction. Previously these were two independent Exec calls,
	// so a failure between them left every membership deleted with the
	// topics still present — a half-applied bulk operation with no way
	// to tell from the outside.
	tx, err := db.Begin()
	if err != nil {
		return respond("", fmt.Sprintf("Failed to shred topics: %v", err), 1)
	}
	defer tx.Rollback()

	memberships, err := tx.Exec("DELETE FROM topic_memberships")
	if err != nil {
		return respond("", fmt.Sprintf("Failed to delete topic memberships: %v", err), 1)
	}
	// topics_ad (fts_recovery.go:166) removes the FTS entry per row.
	result, err := tx.Exec("DELETE FROM topics")
	if err != nil {
		return respond("", fmt.Sprintf("Failed to delete topics: %v", err), 1)
	}
	if err := tx.Commit(); err != nil {
		return respond("", fmt.Sprintf("Failed to commit topic shred: %v", err), 1)
	}

	topicCount, _ := result.RowsAffected()
	memberCount, _ := memberships.RowsAffected()

	// Space reclamation happens during maintenance cycle (deferred VACUUM)
	return respond(fmt.Sprintf("All topics deleted (%d topics, %d memberships).\n"+
		"  Structural anchors (decisions/theories) are recreated by the next\n"+
		"  backfill; existing memories keep their content but lose that grouping.\n"+
		"  NOT erased: audit log, mirror history and rotations, backups, WAL.\n",
		topicCount, memberCount), "", 0)
}

func handleShredModes(args []string) int {
	if !forceRequested() {
		return respond("", "Refusing: 'mpm shred modes' would delete every mode file.\n"+
			"Nothing was deleted. Re-run with -f to confirm.\n", 1)
	}

	// RemoveAll refuses inside a Git worktree. Under the canonical
	// ~/.mpm layout, mode/ is git-tracked source at the runtime root, so
	// this is the guard that stands between a working flag handler and
	// deleting repository source. It lives in RemoveAll, downstream of
	// this force check, so propagating force cannot bypass it.
	mm := internal.NewModeManager("")
	count, err := mm.RemoveAll()
	if err != nil {
		return respond("", fmt.Sprintf("Refusing: %v\n", err), 1)
	}

	return respond(fmt.Sprintf("All modes deleted (%d files).\n", count), "", 0)
}

func handleShredPersonas(args []string) int {
	if !forceRequested() {
		return respond("", "Refusing: 'mpm shred personas' would delete every persona file.\n"+
			"Nothing was deleted. Re-run with -f to confirm.\n", 1)
	}

	// Same Git-worktree guard as modes — persona/ is tracked source at
	// the runtime root under the canonical layout.
	pm := internal.NewPersonaManager("")
	count, err := pm.RemoveAll()
	if err != nil {
		return respond("", fmt.Sprintf("Refusing: %v\n", err), 1)
	}

	return respond(fmt.Sprintf("All personas deleted (%d files).\n", count), "", 0)
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
	// `mpm shred session <id>` and the alias `mpm session shred <id>` are
	// SOFT deletes (store.DeleteMemory sets `deleted_at`; the row stays in
	// the substrate with full content + history preserved, and is
	// recoverable via `mpm memory restore <id>`). The help text and the
	// SPEC lifecycle table both say so; this success message is the only
	// place that called it a "shred" and the truthfulness test pins
	// against that wording. Keep the verb "soft-deleted" in the response
	// so an operator who runs it sees the same word the help promised.
	store := getMemoryStore()

	err := store.DeleteMemory(id, "session")
	if err != nil {
		return respond("", fmt.Sprintf("Failed to soft-delete session: %v", err), 1)
	}

	return respond(fmt.Sprintf(
		"Session soft-deleted: %s (recoverable via 'mpm memory restore %s')\n",
		id, id,
	), "", 0)
}
