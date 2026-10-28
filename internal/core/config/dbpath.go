// dbpath.go — THE single resolver for "which database does this process
// open?".
//
// WHY THIS FILE EXISTS
//
// MPM had THREE different answers to that question, and one of them was
// silently wrong.
//
//	1. The CLI and the MCP server ignored MPM_DB_PATH entirely and used
//	   the workspace: <workspace>/src/db/mpm.db.
//
//	2. mpm-scheduler and mpm-critic read MPM_DB_PATH (as the default of
//	   their --db flag) and then derived the workspace with
//	   Dir(Dir(Dir(path))) — the assumption being that the path always
//	   has the canonical <root>/src/db/mpm.db shape.
//
//	3. Because the shape was ASSUMED rather than CHECKED, a path that did
//	   not have it was silently rewritten into a different database:
//
//	       MPM_DB_PATH=/tmp/other.db   →  workspace "/"  →  DB "/src/db/mpm.db"
//	       MPM_DB_PATH=/srv/data/mpm.db →  workspace "/"  →  DB "/src/db/mpm.db"
//
//	   The operator asked for one database and got another. Nothing said
//	   so, at startup or ever afterwards. Every write landed in a
//   database the operator did not name.
//
// THE RULE
//
// MPM_WORKSPACE is the canonical workspace selector (decision D4).
// MPM_DB_PATH may only ever RESTATE the workspace's own database — it
// is a legacy override, and a legacy override that points somewhere
// else is a conflict, not a selection.
//
// Therefore:
//
//   - MPM_DB_PATH unset                       → the workspace's database.
//   - MPM_DB_PATH names the workspace's DB    → accepted; it agrees.
//   - MPM_DB_PATH names something else         → EXPLICIT ERROR, naming
//                                                both paths. MPM refuses
//                                                to choose between them.
//
// The one supported way to point a process at a different workspace is
// MPM_WORKSPACE, or an explicit --db that has the canonical shape (the
// scheduler's and the critic's own flag).
//
// WHY NOT SIMPLY "SUPPORT ANY PATH"
//
// Because the substrate has exactly ONE database per workspace, and a
// dozen workspace-scoped sidecars that hang off it: the maintenance
// lock, the mirror log, the watchdog log, the scheduler lock, the
// configuration file, the backups directory. A database at a
// non-canonical path has none of them, and the process would silently
// run against an unmaintained database with no lock coordination
// against the scheduler that owns it. The canonical layout is a
// correctness property, not a preference.
//
// THE ESCAPE HATCH
//
// An operator who genuinely needs a database at a non-canonical path
// (a recovered database, say) gives it a canonical home:
//
//	    mkdir -p /srv/recovered/src/db
//	    ln -s /srv/data/mpm.db /srv/recovered/src/db/mpm.db
//	    MPM_WORKSPACE=/srv/recovered mpm-scheduler
//
// The symlink is resolved by inode (see sameFile), so the override is
// accepted as naming the workspace's own database.

package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// dbFileName is the canonical file name of the substrate database.
const dbFileName = "mpm.db"

// CanonicalDBPath returns the canonical database path for a workspace
// root: <workspace>/src/db/mpm.db.
//
// This is the ONLY shape the substrate supports. Every runtime path
// that belongs to the database — the maintenance lock, the mirror log,
// the watchdog log, the WAL sidecars — is derived from this path, which
// is why the shape is a correctness property rather than a preference.
func CanonicalDBPath(workspace string) string {
	return filepath.Join(workspace, "src", "db", dbFileName)
}

// DBPathConflictError is returned when the environment's database
// selectors disagree and MPM cannot honour both.
//
// It names BOTH paths, because the operator's next action depends on
// which one they meant: unset MPM_DB_PATH, or point MPM_WORKSPACE at
// the workspace that owns the MPM_DB_PATH database.
type DBPathConflictError struct {
	// Override is the MPM_DB_PATH value, or the explicit --db value.
	Override string
	// Canonical is the database the effective workspace resolves to.
	Canonical string
	// Workspace is the effective workspace root.
	Workspace string
	// Reason is a short explanation of what disagrees.
	Reason string
}

// Error renders the conflict so that an operator reading only this
// message knows both paths and what to do about them.
func (e *DBPathConflictError) Error() string {
	var b strings.Builder
	b.WriteString("database path conflict: ")
	b.WriteString(e.Reason)
	b.WriteString("\n")
	b.WriteString("  MPM_DB_PATH / --db names : ")
	b.WriteString(e.Override)
	b.WriteString("\n")
	if e.Canonical != "" {
		b.WriteString("  workspace ")
		b.WriteString(e.Workspace)
		b.WriteString(" resolves to : ")
		b.WriteString(e.Canonical)
		b.WriteString("\n")
	}
	b.WriteString("  MPM will not open one database while another is named. ")
	b.WriteString("Unset MPM_DB_PATH, or set MPM_WORKSPACE to the workspace that owns the named database.")
	return b.String()
}

// IsDBPathConflict reports whether err is a database-path conflict.
// Callers that want to distinguish "the configuration disagrees" from
// "the database could not be opened" use it.
func IsDBPathConflict(err error) bool {
	var c *DBPathConflictError
	return errors.As(err, &c)
}

// sameFile reports whether two paths name the same file.
//
// The comparison is by INODE when both paths exist, so a symlinked
// workspace (`~/.mpm` → `/srv/mpm`) is recognised as the same
// database however it is spelled. When the file does not exist yet
// (a cold install), the comparison falls back to the cleaned absolute
// paths.
func sameFile(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	absA, errA := filepath.Abs(a)
	absB, errB := filepath.Abs(b)
	if errA == nil && errB == nil && filepath.Clean(absA) == filepath.Clean(absB) {
		return true
	}
	fa, err := os.Stat(a)
	if err != nil {
		return false
	}
	fb, err := os.Stat(b)
	if err != nil {
		return false
	}
	return os.SameFile(fa, fb)
}

// HasCanonicalDBShape reports whether p names <root>/src/db/mpm.db for
// some non-degenerate root.
//
// It is the check the old Dir(Dir(Dir(path))) derivation ASSUMED and
// never made. A path that does not have this shape cannot be turned
// into a workspace root, and the old code turned it into one anyway —
// which is how `--db /tmp/other.db` came to mean "open /src/db/mpm.db".
func HasCanonicalDBShape(p string) bool {
	_, ok := WorkspaceRootForDBPath(p)
	return ok
}

// WorkspaceRootForDBPath returns the workspace root that owns p, when p
// has the canonical <root>/src/db/mpm.db shape.
//
// The second return value is false when p does not have that shape, or
// when the implied root is degenerate ("/" or "."), because a
// workspace at the filesystem root is never a legitimate MPM workspace.
func WorkspaceRootForDBPath(p string) (string, bool) {
	if p == "" {
		return "", false
	}
	clean := filepath.Clean(p)
	dir, file := filepath.Split(clean)
	if file != dbFileName {
		return "", false
	}
	dbDir := filepath.Clean(dir)
	if filepath.Base(dbDir) != "db" {
		return "", false
	}
	srcDir := filepath.Dir(dbDir)
	if filepath.Base(srcDir) != "src" {
		return "", false
	}
	root := filepath.Dir(srcDir)
	if root == "" || root == "." || root == string(filepath.Separator) {
		return "", false
	}
	return root, true
}

// ValidateDBPathOverride checks the MPM_DB_PATH environment variable
// against the canonical database of the given workspace.
//
// It returns the canonical database path — which is what the caller
// should open — and an error when the override names a DIFFERENT
// database.
//
// The returned path is always the workspace's own database. MPM_DB_PATH
// is an override that may restate it, never one that redirects it:
// the workspace owns the maintenance lock, the mirror log, the watchdog
// log and the configuration, and a database without those is not part of
// an installation.
func ValidateDBPathOverride(workspace string) (string, error) {
	canonical := CanonicalDBPath(workspace)
	override := strings.TrimSpace(os.Getenv("MPM_DB_PATH"))
	if override == "" {
		return canonical, nil
	}
	if sameFile(override, canonical) {
		return canonical, nil
	}
	reason := "MPM_DB_PATH names a different database than the workspace's own"
	if root, ok := WorkspaceRootForDBPath(override); ok {
		reason = fmt.Sprintf(
			"MPM_DB_PATH names the database of workspace %s, but the effective workspace is %s",
			root, workspace)
	}
	return "", &DBPathConflictError{
		Override:  override,
		Canonical: canonical,
		Workspace: workspace,
		Reason:    reason,
	}
}

// ResolveTarget resolves the effective workspace root and the database
// path for a process, from an explicit --db value (empty for the
// binaries that have no such flag) and the environment.
//
// This is THE resolver. Every MPM binary that opens the database goes
// through it, so there is exactly one place where the answer is
// computed and exactly one set of rules:
//
//  1. An explicit --db WINS, but only when it has the canonical
//     <root>/src/db/mpm.db shape. The implied <root> becomes the
//     effective workspace. A path without that shape is REJECTED
//     explicitly — it is never rewritten into some other database.
//
//  2. Otherwise the effective workspace is the environment's:
//     MPM_WORKSPACE, or $HOME/.mpm.
//
//  3. MPM_DB_PATH must, when it is set, name the effective workspace's
//     own database. When it does not, the two selectors disagree and
//     the disagreement is an explicit error.
//
// The returned workspace is the root that owns the database, so the
// caller can pass it to NewDatabaseManager and get exactly the
// database that was resolved here.
func ResolveTarget(explicitDB string) (workspace string, dbPath string, err error) {
	explicitDB = strings.TrimSpace(explicitDB)

	if explicitDB != "" {
		root, ok := WorkspaceRootForDBPath(explicitDB)
		if !ok {
			return "", "", &DBPathConflictError{
				Override:  explicitDB,
				Canonical: "",
				Workspace: "",
				Reason: fmt.Sprintf(
					"the database path does not have the canonical <workspace>/src/db/%s shape, "+
						"so no workspace owns it", dbFileName),
			}
		}
		// An explicit --db and an explicit MPM_WORKSPACE must agree.
		// Two selectors naming two workspaces is a configuration
		// conflict, not a precedence rule to be resolved silently.
		if envWS := strings.TrimSpace(os.Getenv("MPM_WORKSPACE")); envWS != "" {
			if !sameFile(envWS, root) {
				return "", "", &DBPathConflictError{
					Override:  explicitDB,
					Canonical: CanonicalDBPath(root),
					Workspace: root,
					Reason: fmt.Sprintf(
						"the explicit database path names workspace %s, but MPM_WORKSPACE names %s",
						root, envWS),
				}
			}
		}
		workspace = root
		dbPath = CanonicalDBPath(root)
		// The override's own path IS the canonical path of the root
		// it names; keep the operator's spelling for the log line
		// but open the canonical one.
		if sameFile(explicitDB, dbPath) {
			dbPath = explicitDB
		}
		// An explicit --db and MPM_DB_PATH are two selectors too.
		// When both are set they must name the same database.
		if env := strings.TrimSpace(os.Getenv("MPM_DB_PATH")); env != "" && !sameFile(env, dbPath) {
			return "", "", &DBPathConflictError{
				Override:  env,
				Canonical: dbPath,
				Workspace: workspace,
				Reason: fmt.Sprintf(
					"MPM_DB_PATH names %s, but the explicit database path names %s",
					env, explicitDB),
			}
		}
	} else {
		workspace = GetWorkspace()
		var verr error
		dbPath, verr = ValidateDBPathOverride(workspace)
		if verr != nil {
			return "", "", verr
		}
	}
	return workspace, dbPath, nil
}
