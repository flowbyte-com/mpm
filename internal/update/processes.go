package update

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
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

// HolderScan is the outcome of one writer scan.
//
// A scan has four outcomes, and collapsing them is the defect this
// type exists to prevent:
//
//   - occupied: at least one process holds the database;
//   - clear: every process was inspected and none holds it;
//   - unverifiable: no holder was found, but some processes could
//     neither be inspected nor positively ruled out. They are NAMED,
//     not hidden — the operator decides explicitly;
//   - inconclusive: observation itself failed (unreadable /proc, or
//     an unreadable lock table while uninspectable processes remain).
//     Nothing can be said; not even --force proceeds.
//
// An empty holder list is evidence of safety ONLY when Inconclusive is
// false AND Unverifiable is empty. Failing closed means: holders
// always refuse; inconclusive always refuses; unverifiable refuses
// unless the operator explicitly accepts the NAMED residual with
// --force (recorded, never silent).
type HolderScan struct {
	Holders []DBHolder
	// Unverifiable lists processes that could neither be inspected
	// nor ruled out, with the reason each.
	Unverifiable []UnverifiableProc
	// Inconclusive is true when quiescence could not even be
	// established.
	Inconclusive bool
	// Detail explains an inconclusive result for the operator.
	Detail string
	// Skipped counts processes that could not be fully inspected but
	// were ruled out (a foreign uid on a group/other-closed database).
	Skipped int
}

// ScanDBHolders lists the processes holding an open file description
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
//
// Fail-closed rule: a process whose file descriptors cannot be read is
// assumed to be a holder UNLESS it can be positively ruled out — its
// uid is neither ours nor root's, while the database file itself is
// closed to group and other. (The data root is created 0700 and the
// installer auto-corrects wider permissions, so the common case rules
// foreign processes out; anything else refuses.) A vanished process
// (the pid is gone mid-scan) is a benign race and is skipped.
func ScanDBHolders(dbFiles ...string) HolderScan {
	var scan HolderScan
	want := map[string]bool{}
	var strictPaths []string
	for _, f := range dbFiles {
		abs, err := filepath.Abs(f)
		if err != nil {
			continue
		}
		strictPaths = append(strictPaths, abs)
		want[abs] = true
		// SQLite may hold the main file while the sidecars come and
		// go; match any of the three spellings.
		want[strings.TrimSuffix(abs, "-wal")] = true
		want[strings.TrimSuffix(abs, "-shm")] = true
	}
	// Existing database files: only these can be held open. (A
	// missing sidecar cannot be held; judging by it would fail closed
	// on nothing.)
	var existing []string
	for _, p := range strictPaths {
		if _, err := os.Stat(p); err == nil {
			existing = append(existing, p)
		}
	}
	entries, err := os.ReadDir(procFSRoot)
	if err != nil {
		scan.Inconclusive = true
		scan.Detail = fmt.Sprintf("cannot read %s: %v", procFSRoot, err)
		return scan
	}
	ownUID := os.Geteuid()
	// Lock evidence is collected once, up front: /proc/locks is
	// world-readable, so it covers even the processes whose file
	// descriptors we cannot inspect. A lock on one of our inodes is
	// hard evidence of holding, regardless of privilege.
	lockedPIDs, locksOK := pidsLocking(wantDevs(strictPaths))
	var firstUninspectable string
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		fdDir := filepath.Join(procFSRoot, e.Name(), "fd")
		fds, err := os.ReadDir(fdDir)
		if err != nil {
			if os.IsNotExist(err) {
				continue // exited mid-scan: benign race
			}
			if isKernelThread(e.Name()) {
				scan.Skipped++
				continue
			}
			uid, groups, uidOK := procIDs(e.Name())
			if uidOK && uid != ownUID && uid != 0 && cannotOpenAny(existing, uid, groups) {
				// Positive exclusion: the process runs as a
				// foreign, non-root uid and the permission
				// check proves it cannot open any of the
				// existing database files — so it cannot hold
				// one open.
				scan.Skipped++
				continue
			}
			if lockedPIDs[pid] {
				// Hard evidence despite unreadable descriptors.
				scan.Holders = append(scan.Holders, DBHolder{PID: pid, Name: procComm(pid), File: "(lock held; descriptors unreadable)"})
				continue
			}
			if !locksOK {
				// The lock table itself is unreadable, so there is
				// no second source to consult: quiescence cannot
				// be established at all.
				if firstUninspectable == "" {
					firstUninspectable = fmt.Sprintf("cannot inspect pid %d (%v) and the lock table is unreadable", pid, err)
				}
				continue
			}
			// No lock on our files anywhere in the system.
			// That is not a clearance -- an idle, lock-free
			// holding cannot be disproven this way -- but it
			// is a NAMED residual the operator can judge,
			// instead of a silent skip or an unactionable
			// refusal. A same-uid process with unreadable
			// descriptors (a hardened agent, a keyring
			// daemon) lands here too: it is the operator's
			// own process, named, for an explicit decision.
			scan.Unverifiable = append(scan.Unverifiable, UnverifiableProc{
				PID:    pid,
				Name:   procComm(pid),
				Reason: fmt.Sprintf("descriptors unreadable (%v); no lock on the database was found", err),
			})
			continue
		}
		held := ""
		for _, fd := range fds {
			target, err := os.Readlink(filepath.Join(fdDir, fd.Name()))
			if err != nil {
				continue // fd closed mid-scan: benign race
			}
			// A deleted-but-held file reads as "path (deleted)".
			// It still pins the inode, so it still counts.
			target = strings.TrimSuffix(target, " (deleted)")
			if want[target] {
				held = target
				break
			}
		}
		if held == "" {
			continue
		}
		scan.Holders = append(scan.Holders, DBHolder{PID: pid, Name: procComm(pid), File: held})
	}
	sort.Slice(scan.Holders, func(i, j int) bool { return scan.Holders[i].PID < scan.Holders[j].PID })
	// A definite holder is actionable on its own; inconclusive
	// observation only decides the empty case. Either way the caller
	// refuses — but for different stated reasons.
	if len(scan.Holders) == 0 && firstUninspectable != "" {
		scan.Inconclusive = true
		scan.Detail = firstUninspectable
	}
	return scan
}

// procIDs reports the process's uid and supplementary groups from
// /proc/pid/status.
func procIDs(pid string) (uid int, groups []int, ok bool) {
	raw, err := os.ReadFile(filepath.Join(procFSRoot, pid, "status"))
	if err != nil {
		return 0, nil, false
	}
	var u int
	var g []int
	var haveU bool
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "Uid:") {
			f := strings.Fields(line)
			if len(f) < 2 {
				return 0, nil, false
			}
			v, err := strconv.Atoi(f[1])
			if err != nil {
				return 0, nil, false
			}
			u, haveU = v, true
		}
		if strings.HasPrefix(line, "Groups:") {
			for _, f := range strings.Fields(line)[1:] {
				v, err := strconv.Atoi(f)
				if err != nil {
					return 0, nil, false
				}
				g = append(g, v)
			}
		}
	}
	if !haveU {
		return 0, nil, false
	}
	return u, g, true
}

// cannotOpenAny reports whether the process credentials provably
// cannot open any of the given files for reading: neither the file
// itself (owner/group/other read bit for its class) nor directory
// traversal (execute bit on every ancestor) permits it. This is the
// same check the kernel performs on open(2), minus ACLs and LSM
// policy — and it fails closed: any unreadable stat or unparseable
// input means "cannot prove", i.e. false.
func cannotOpenAny(paths []string, uid int, groups []int) bool {
	if len(paths) == 0 {
		return true // nothing exists to hold open
	}
	inGroups := func(gid int) bool {
		for _, g := range groups {
			if g == gid {
				return true
			}
		}
		return false
	}
	classBits := func(fUid, fGid int, mode os.FileMode, need os.FileMode) bool {
		var bits os.FileMode
		switch {
		case uid == fUid:
			bits = (mode >> 6) & 7
		case inGroups(fGid):
			bits = (mode >> 3) & 7
		default:
			bits = mode & 7
		}
		return bits&need == need
	}
	statOwner := func(path string) (int, int, os.FileMode, bool) {
		var st unix.Stat_t
		if err := unix.Stat(path, &st); err != nil {
			return 0, 0, 0, false
		}
		return int(st.Uid), int(st.Gid), os.FileMode(st.Mode & 0777), true
	}
	for _, p := range paths {
		fUid, fGid, fMode, ok := statOwner(p)
		if !ok {
			return false
		}
		if !classBits(fUid, fGid, fMode, 4) {
			continue // this file provably unreadable; check the next
		}
		// Readable — but only reachable with execute on every
		// ancestor directory.
		dir := filepath.Dir(p)
		reachable := true
		for {
			dUid, dGid, dMode, ok := statOwner(dir)
			if !ok || !classBits(dUid, dGid, dMode, 1) {
				reachable = false
				break
			}
			if dir == "/" {
				break
			}
			dir = filepath.Dir(dir)
		}
		if reachable {
			return false // this file could be opened
		}
	}
	return true
}

// isKernelThread reports whether the process has an empty command
// line: only kernel threads (and zombies, which hold no files) look
// like that. A kernel thread performs no userspace file I/O and
// cannot be an SQLite client.
func isKernelThread(pid string) bool {
	raw, err := os.ReadFile(filepath.Join(procFSRoot, pid, "cmdline"))
	if err != nil {
		return false
	}
	return len(bytes.Trim(raw, "\x00 \t\n")) == 0
}

// wantDevs resolves each existing database path to its (device,
// inode) identity for lock matching. Missing files cannot be held
// open and are skipped.
func wantDevs(paths []string) map[[2]uint64]bool {
	want := map[[2]uint64]bool{}
	for _, p := range paths {
		var st unix.Stat_t
		if err := unix.Stat(p, &st); err != nil {
			continue
		}
		want[[2]uint64{st.Dev, st.Ino}] = true
	}
	return want
}

// lockDevIno parses a /proc/locks device:ino field ("MAJ:MIN:INO",
// major/minor hex, inode decimal) into a device number and inode.
func lockDevIno(field string) (uint64, uint64, bool) {
	parts := strings.Split(field, ":")
	if len(parts) != 3 {
		return 0, 0, false
	}
	maj, err := strconv.ParseUint(parts[0], 16, 32)
	if err != nil {
		return 0, 0, false
	}
	min, err := strconv.ParseUint(parts[1], 16, 32)
	if err != nil {
		return 0, 0, false
	}
	ino, err := strconv.ParseUint(parts[2], 10, 64)
	if err != nil {
		return 0, 0, false
	}
	return unix.Mkdev(uint32(maj), uint32(min)), ino, true
}

// pidsLocking reports the pids holding a lock on any of the wanted
// (device, inode) pairs, read from the world-readable /proc/locks.
// The second return is false when the locks table itself is
// unreadable — in which case lock evidence is unavailable, not absent.
func pidsLocking(want map[[2]uint64]bool) (map[int]bool, bool) {
	out := map[int]bool{}
	if len(want) == 0 {
		return out, true
	}
	raw, err := os.ReadFile(filepath.Join(procFSRoot, "locks"))
	if err != nil {
		return nil, false
	}
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 6 {
			continue
		}
		pid, err := strconv.Atoi(fields[4])
		if err != nil || pid <= 0 {
			continue
		}
		dev, ino, ok := lockDevIno(fields[5])
		if !ok {
			continue
		}
		if want[[2]uint64{dev, ino}] {
			out[pid] = true
		}
	}
	return out, true
}

// UnverifiableProc is a process the scan could neither inspect nor
// rule out, named so the operator can judge it.
type UnverifiableProc struct {
	PID    int
	Name   string
	Reason string
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
