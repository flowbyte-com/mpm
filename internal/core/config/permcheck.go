// permcheck.go — verify and auto-heal the runtime directory permissions.
//
// v spec 2026-08-04: mpm runs in a single user's context, so the
// runtime data directory must not be readable by other users on the
// host. All auto-creation paths use 0700 now, but a user might
// accidentally chmod their ~/.mpm wider (e.g., `chmod -R 755` to fix
// some other issue, or a tarball restore that preserved 0755). The
// startup perms check catches this and auto-heals if the current
// user owns the directory. Manual fix is required only when
// ownership doesn't allow chmod.
//
// UX: auto-heal when possible (we own the dir → log warning + fix +
// continue booting). Fatal only when the chmod itself fails (wrong
// ownership / not the user, immutable parent, etc.). This avoids
// the operator-digs-through-logs-to-fix-perms friction while keeping
// the hard security line — we never silently allow wide perms.
//
// Lives in package config next to GetMPMDir (which provides the
// directory the check operates on). Keeping the two together makes
// the security surface discoverable from one file.

package config

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
)

// ErrDirPermsTooWide is returned when the directory permissions are
// wider than 0700 AND the current user lacks ownership to chmod.
// Caller decides whether to make this fatal (mpm, mpm-scheduler,
// mpm-mcp all wrap the call in a fatal-exit on error).
var ErrDirPermsTooWide = errors.New("mpm runtime directory has wider permissions than 0700 and cannot be auto-healed")

// AssertUserDirPerms0700 verifies (and auto-heals if possible) that
// dir has permissions no wider than 0700.
//
// Return semantics:
//   - nil: dir is compliant (was already <=0700, OR was wider but
//     auto-healed succeeded).
//   - error wrapping ErrDirPermsTooWide: dir was wider than 0700 and
//     the chmod attempt failed (most commonly: current user doesn't
//     own the directory). Manual fix required.
//   - other error: stat failed (path missing, permission denied on
//     the parent, etc.).
//
// Logging: the function emits exactly one slog line per call. WARN if
// auto-heal was attempted (regardless of outcome), INFO if it
// succeeded, ERROR wrapped in the returned error otherwise. The
// caller decides whether to log a final fatal message.
//
// Intended to be called once at process startup, BEFORE opening any
// DB connection. Cheap — one stat, maybe one chmod.
func AssertUserDirPerms0700(dir string) error {
	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("stat mpm dir %q: %w", dir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("mpm path %q is not a directory (mode %s)", dir, info.Mode())
	}

	mode := info.Mode().Perm()
	if mode <= 0o700 {
		// Already compliant (or stricter; 0o600 / 0o500 / 0o400 are
		// all fine — anything tighter than 0700 is allowed).
		return nil
	}

	// Perms too wide. Attempt auto-heal. The chmod itself is the
	// ownership check — if we have permission to chmod, we're the
	// owner (or root); if we don't, EPERM tells us to bail.
	slog.Warn("mpm dir perms too wide; auto-correcting to 0700",
		"dir", dir,
		"current_mode", fmt.Sprintf("%04o", mode),
		"euid", os.Geteuid())

	if chmodErr := os.Chmod(dir, 0o700); chmodErr != nil {
		return fmt.Errorf("%w: dir=%s mode=%04o euid=%d chmod_err=%v; manual fix required: chmod 700 %s",
			ErrDirPermsTooWide, dir, mode, os.Geteuid(), chmodErr, dir)
	}

	slog.Info("mpm dir perms auto-corrected to 0700",
		"dir", dir,
		"previous_mode", fmt.Sprintf("%04o", mode))
	return nil
}