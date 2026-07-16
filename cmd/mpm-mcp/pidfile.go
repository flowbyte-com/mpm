// pidfile.go — mpm-mcp single-instance lock.
//
// On startup, attempts to atomically create a pidfile at
// $MPM_WORKSPACE/mpm-mcp.pid (or ~/.mpm/mpm-mcp.pid when the workspace
// is the user home). If the file already exists, reads the recorded pid
// and checks if it still refers to a live mpm-mcp. A live holder means
// we are an orphan from a previous gateway cycle — exit 0 silently. A
// dead pid means the pidfile is stale (the previous process crashed or
// was SIGKILL'd) — take over by replacing it.
//
// The pidfile is the actual mutual exclusion primitive; the O_EXCL
// create is the atomic test-and-set. JSON contents (pid, started_at,
// version) are for forensics, not for the lock itself.
//
// Why this exists: the openclaw gateway respawns mpm-mcp on every
// restart but does not reap the previous child. When the old mpm-mcp
// lingers, two processes hold RW handles on mpm.db simultaneously,
// producing SQLITE_BUSY on writes AND the OpenClaw 'reply session
// initialization conflicted' error on the next prompt. The pidfile
// turns the overlapping window from "two writers, sessions die" into
// "one writer, the other one is told to leave".

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/flowbyte-com/mpm-core/config"
)

// ErrOrphan is returned by AcquirePidfile when another live mpm-mcp
// already holds the lock. The caller should exit 0 — silently yielding
// the slot to the canonical instance.
var ErrOrphan = errors.New("mpm-mcp: another live instance holds the pidfile")

// pidfileVersion is stamped into the JSON payload for forensics.
// Bump if the pidfile schema changes; older readers should treat a
// mismatched version as stale and take over.
const pidfileVersion = "mpm-mcp-pidfile/1"

// PidfilePayload is the on-disk JSON shape. Keep it forward-compatible:
// new fields are added; old fields are not renamed or repurposed.
type PidfilePayload struct {
	PID       int    `json:"pid"`
	StartedAt string `json:"started_at"` // RFC3339Nano
	Version   string `json:"version"`
}

// PidfilePath returns the canonical path to the mpm-mcp pidfile.
// Rooted at config.GetMPMDir() so the pidfile always lives next to
// the database the process is locking.
func PidfilePath() string {
	return filepath.Join(config.GetMPMDir(), "mpm-mcp.pid")
}

// AcquirePidfile attempts to take the mpm-mcp single-instance lock.
//
// Returns nil on success — the caller is the canonical instance and
// must ReleasePidfile on shutdown. Returns ErrOrphan if another live
// mpm-mcp already holds the lock — the caller should exit 0. Returns
// a wrapped error for any other failure (I/O, parse, etc.).
func AcquirePidfile(path string) error {
	payload := PidfilePayload{
		PID:       os.Getpid(),
		StartedAt: time.Now().UTC().Format(time.RFC3339Nano),
		Version:   pidfileVersion,
	}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal pidfile: %w", err)
	}

	// Step 1: atomic test-and-set. O_EXCL fails if the file exists.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
	if err == nil {
		// We won the race. Write our payload, fsync to make the
		// acquire visible across processes, and close.
		if _, werr := f.Write(data); werr != nil {
			f.Close()
			os.Remove(path) // best-effort cleanup; next start handles re-acquire
			return fmt.Errorf("write pidfile: %w", werr)
		}
		if serr := f.Sync(); serr != nil {
			f.Close()
			os.Remove(path)
			return fmt.Errorf("fsync pidfile: %w", serr)
		}
		return f.Close()
	}
	if !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("create pidfile: %w", err)
	}

	// Step 2: file exists. Read it and decide orphan vs stale.
	existing, readErr := readPidfile(path)
	if readErr != nil {
		// Corrupt or unreadable pidfile. Treat as stale so the
		// operator is not permanently locked out by a bad file.
		// Log via stderr (mpm-mcp uses slog but at this stage the
		// logger may not be configured yet) and replace.
		fmt.Fprintf(os.Stderr, "mpm-mcp: pidfile unreadable (%v); taking over\n", readErr)
		return overwritePidfile(path, data)
	}

	// Step 3: is the recorded pid still alive AND still mpm-mcp?
	if isLiveMcpProcess(existing.PID) {
		return fmt.Errorf("%w: pid=%d started_at=%s", ErrOrphan, existing.PID, existing.StartedAt)
	}

	// Step 4: stale. The recorded process is gone (crashed, killed,
	// or PID-recycled onto something that is not mpm-mcp). Take over.
	return overwritePidfile(path, data)
}

// ReleasePidfile removes the pidfile if and only if it still refers
// to the current process. The PID check protects against a delayed
// release landing after another instance has legitimately taken over
// (which would otherwise wipe the new instance's pidfile).
func ReleasePidfile(path string) error {
	existing, err := readPidfile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil // already gone; nothing to do
		}
		return err
	}
	if existing.PID != os.Getpid() {
		// Not our pidfile. Leave it alone.
		return nil
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// readPidfile parses the JSON payload. Returns os.ErrNotExist if the
// file is missing, or a wrapped error for any other parse failure.
func readPidfile(path string) (*PidfilePayload, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var p PidfilePayload
	if jerr := json.Unmarshal(data, &p); jerr != nil {
		return nil, fmt.Errorf("parse pidfile json: %w", jerr)
	}
	if p.PID <= 0 {
		return nil, fmt.Errorf("pidfile has invalid pid=%d", p.PID)
	}
	return &p, nil
}

// overwritePidfile atomically replaces the pidfile via a temp + rename.
// Rename is atomic on POSIX within the same filesystem, so concurrent
// readers never see a half-written file.
func overwritePidfile(path string, data []byte) error {
	tmp := path + ".tmp." + strconv.Itoa(os.Getpid())
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return fmt.Errorf("write temp pidfile: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp) // best-effort cleanup
		return fmt.Errorf("rename pidfile: %w", err)
	}
	return nil
}

// isLiveMcpProcess reports whether pid is alive AND its executable
// still resolves to an mpm-mcp binary. The second check defends
// against PID reuse: if the kernel recycled the pid onto a different
// process, we must not consider the lock held.
//
// On Linux, /proc/<pid>/comm gives the short process name. On other
// platforms we shell out to `ps -p <pid> -o comm=` (a portable POSIX
// fallback). The platform-specific shell helpers live in
// pidfile_procname_linux.go and pidfile_procname_other.go.
func isLiveMcpProcess(pid int) bool {
	if pid <= 0 || pid == os.Getpid() {
		return false
	}
	// Signal 0 = existence check, no actual signal delivered.
	if err := syscall.Kill(pid, 0); err != nil {
		return false // ESRCH = no such process.
	}
	name, err := readProcComm(pid)
	if err != nil {
		// Cannot determine binary identity (no /proc on Linux, no ps,
		// or pid gone between kill and read). Err on the side of
		// "live" to avoid the exact race we are trying to prevent —
		// clobbering a live holder is worse than leaving a stale
		// pidfile for the operator to inspect.
		return true
	}
	return name == "mpm-mcp"
}
