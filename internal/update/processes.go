package update

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
)

// LiveProcess is one process whose executable resolves into a versioned
// release — or which looks like an agent session the operator must
// restart by hand.
type LiveProcess struct {
	PID int
	// Name is the executable's basename or command.
	Name string
	// Release is the release revision the process runs from ("" when
	// the process is not a versioned MPM binary).
	Release string
	// OpenCode is true when the process looks like an OpenCode/agent
	// session rather than an MPM binary.
	OpenCode bool
}

// StaleReport compares the running processes against the active
// release.
type StaleReport struct {
	// ActiveRev is the active release revision ("" when none).
	ActiveRev string
	// Processes are the MPM processes found, newest understanding
	// first (sorted by PID for stability).
	Processes []LiveProcess
	// Stale are the MPM processes NOT running the active release.
	Stale []LiveProcess
	// OpenCodeSessions are agent sessions the updater will never touch.
	OpenCodeSessions []LiveProcess
}

// ScanLiveProcesses inventories the processes relevant to an update.
// It is Linux-only (the /proc scan); elsewhere it returns a report with
// an explanatory note rather than a fabricated inventory.
func ScanLiveProcesses(dataRoot string) (StaleReport, error) {
	var rep StaleReport
	_, rev, err := CurrentReleaseTarget(dataRoot)
	if err == nil {
		rep.ActiveRev = rev
	}
	if runtime.GOOS != "linux" {
		return rep, fmt.Errorf("process inventory is supported on Linux; on %s the operator must check running processes by hand", runtime.GOOS)
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return rep, fmt.Errorf("could not inventory processes: %w", err)
	}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		proc, ok := classifyPID(dataRoot, pid)
		if !ok {
			continue
		}
		rep.Processes = append(rep.Processes, proc)
	}
	sort.Slice(rep.Processes, func(i, j int) bool { return rep.Processes[i].PID < rep.Processes[j].PID })
	for _, p := range rep.Processes {
		switch {
		case p.OpenCode:
			rep.OpenCodeSessions = append(rep.OpenCodeSessions, p)
		case rep.ActiveRev != "" && p.Release != "" && p.Release != rep.ActiveRev:
			rep.Stale = append(rep.Stale, p)
		}
	}
	return rep, nil
}

func classifyPID(dataRoot string, pid int) (LiveProcess, bool) {
	base := fmt.Sprintf("/proc/%d", pid)
	exe, err := os.Readlink(base + "/exe")
	if err != nil {
		// Kernel threads and processes owned by other users have no
		// readable exe. They are not ours.
		return LiveProcess{}, false
	}
	raw, err := os.ReadFile(base + "/cmdline")
	if err != nil {
		return LiveProcess{}, false
	}
	parts := strings.Split(string(raw), "\x00")
	cmd := ""
	for _, p := range parts {
		if p != "" {
			cmd = p
			break
		}
	}
	name := exe
	if i := strings.LastIndex(exe, "/"); i >= 0 {
		name = exe[i+1:]
	}
	// Agent sessions are matched on the BINARY name and the command's own
	// tokens — never on the full executable path. Matching the path
	// would flag every binary that happens to live under a directory
	// with "opencode" in its name (including this updater in its own
	// worktree).
	if isAgentSession(name, parts) {
		return LiveProcess{PID: pid, Name: name, OpenCode: true}, true
	}
	// A versioned MPM binary resolves under <data root>/releases/<rev>.
	prefix := dataRoot + "/" + ReleasesDirName + "/"
	if strings.HasPrefix(exe, prefix) {
		rest := strings.TrimPrefix(exe, prefix)
		rev := rest
		if i := strings.Index(rest, "/"); i >= 0 {
			rev = rest[:i]
		}
		if isMPMBinary(name) {
			return LiveProcess{PID: pid, Name: name, Release: rev}, true
		}
	}
	if isMPMBinary(name) || isMPMBinary(cmdBase(cmd)) {
		// An MPM binary running from somewhere else (a dev checkout,
		// /usr/local/bin, a stale copy). It cannot be mapped to a
		// release, but the operator should know it exists.
		return LiveProcess{PID: pid, Name: name}, true
	}
	return LiveProcess{}, false
}

// isAgentSession reports whether a process looks like an agent
// session the updater must never touch.
func isAgentSession(exeBase string, argv []string) bool {
	if strings.Contains(strings.ToLower(exeBase), "opencode") {
		return true
	}
	for _, tok := range argv {
		base := tok
		if i := strings.LastIndex(tok, "/"); i >= 0 {
			base = tok[i+1:]
		}
		if strings.Contains(strings.ToLower(base), "opencode") {
			return true
		}
	}
	return false
}

func cmdBase(cmd string) string {
	if i := strings.LastIndex(cmd, "/"); i >= 0 {
		return cmd[i+1:]
	}
	return cmd
}

func isMPMBinary(name string) bool {
	switch name {
	case "mpm", "mpm-mcp", "mpm-scheduler", "mpm-critic", "mpm-telemetry", "mpm-update":
		return true
	}
	return false
}

// procFSRoot is /proc on Linux. Tests override it with a fake tree.
var procFSRoot = "/proc"

// DBHolder is one process holding an open file description on a
// database file.
type DBHolder struct {
	PID  int
	Name string
	File string
}

// OpenDBHolders lists the processes holding an open file description
// on any of the given database files (the live database and its WAL
// and SHM sidecars).
//
// This is hard evidence, not heuristics: an SQLite connection is an
// open file description. A process on this list is a WRITER for
// restore purposes whether it looks like a daemon, an agent session,
// a CLI or anything else.
//
// The updater itself is never excluded: by construction it holds no
// database handle at restore time (every helper opens and closes), so
// any holder — including, hypothetically, this process — is a real
// writer and must block the restore.
func OpenDBHolders(dbFiles ...string) []DBHolder {
	want := map[string]string{}
	for _, f := range dbFiles {
		abs, err := filepath.Abs(f)
		if err != nil {
			continue
		}
		want[abs] = f
		// SQLite may hold the main file while the sidecars come and
		// go; match any of the three spellings.
		want[strings.TrimSuffix(abs, "-wal")] = f
		want[strings.TrimSuffix(abs, "-shm")] = f
	}
	var out []DBHolder
	entries, err := os.ReadDir(procFSRoot)
	if err != nil {
		return nil
	}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		fdDir := filepath.Join(procFSRoot, e.Name(), "fd")
		fds, err := os.ReadDir(fdDir)
		if err != nil {
			continue
		}
		held := ""
		for _, fd := range fds {
			target, err := os.Readlink(filepath.Join(fdDir, fd.Name()))
			if err != nil {
				continue
			}
			// A deleted-but-held file reads as "path (deleted)".
			// It still pins the inode, so it still counts.
			target = strings.TrimSuffix(target, " (deleted)")
			if _, ok := want[target]; ok {
				held = target
				break
			}
		}
		if held == "" {
			continue
		}
		out = append(out, DBHolder{PID: pid, Name: procComm(pid), File: held})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PID < out[j].PID })
	return out
}

// procComm names a process for operator output: the exe basename, or
// the comm value when the exe is unreadable.
func procComm(pid int) string {
	if exe, err := os.Readlink(filepath.Join(procFSRoot, strconv.Itoa(pid), "exe")); err == nil {
		if i := strings.LastIndex(exe, "/"); i >= 0 {
			return exe[i+1:]
		}
		return exe
	}
	if raw, err := os.ReadFile(filepath.Join(procFSRoot, strconv.Itoa(pid), "comm")); err == nil {
		return strings.TrimSpace(string(raw))
	}
	return fmt.Sprintf("pid %d", pid)
}

// FormatStaleReport renders the inventory for the operator. It never
// claims every process runs the new release: the pointer swap changes
// the filesystem, not running processes.
func FormatStaleReport(rep StaleReport) string {
	var b strings.Builder
	if len(rep.Processes) == 0 {
		b.WriteString("no running MPM processes found\n")
	} else {
		fmt.Fprintf(&b, "active release: %s\n", displayRev(rep.ActiveRev))
		for _, p := range rep.Processes {
			state := "current"
			if p.OpenCode {
				state = "agent session — restart by hand; the updater will never touch it"
			} else if rep.ActiveRev != "" && p.Release != "" && p.Release != rep.ActiveRev {
				state = "STALE — predates the active release; restart it to pick up the new runtime"
			} else if p.Release == "" {
				state = "unmapped — not running from a versioned release (dev checkout or stray copy)"
			}
			fmt.Fprintf(&b, "  pid %d %s: %s\n", p.PID, p.Name, state)
		}
	}
	if len(rep.OpenCodeSessions) > 0 {
		fmt.Fprintf(&b, "%d OpenCode session(s) detected — restart OpenCode by hand; the updater does not terminate agent sessions\n", len(rep.OpenCodeSessions))
	}
	return b.String()
}

func displayRev(rev string) string {
	if rev == "" {
		return "(none)"
	}
	return rev
}
