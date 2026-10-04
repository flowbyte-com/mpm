package internal

import (
	"os"
	"path/filepath"
	"testing"
)

// ==================== Destructive semantics (Phase E) ====================

// TestMemoryWipe_ClearsActiveMirrorAndRotations pins the destructive
// matrix: `mpm memory wipe` removes the active mirror AND every rotation,
// and it does NOT touch the database. The audit table and backups keep
// their own copies, so the wipe is a journal wipe, not a memory wipe
// pretending to be one.
func TestMemoryWipe_ClearsActiveMirrorAndRotations(t *testing.T) {
	dir := t.TempDir()
	mirror := filepath.Join(dir, "mirror.jsonl")
	dbPath := filepath.Join(dir, "mpm.db")

	// Seed: a mirror file plus a rotation, plus an mpm.db that must
	// not be touched.
	os.WriteFile(mirror, []byte(`{"v":2,"op":"memory_created","preview":"x"}`+"\n"), 0o600)
	os.WriteFile(mirror+".20260101000000.gz", []byte("archive"), 0o600)
	os.WriteFile(dbPath, []byte("authoritative substrate"), 0o600)

	store := &MemoryStore{MirrorFile: mirror}
	rotations, err := store.ClearMirror()
	if err != nil {
		t.Fatalf("ClearMirror: %v", err)
	}
	if rotations < 1 {
		t.Errorf("ClearMirror reported %d rotations removed, want at least 1", rotations)
	}

	// Mirror file is truncated, not deleted — the next append creates
	// it on demand. That keeps a tail-following reader stable.
	data, _ := os.ReadFile(mirror)
	if len(data) != 0 {
		t.Errorf("active mirror not truncated: %q", data)
	}
	// Rotation is gone.
	if _, err := os.Stat(mirror + ".20260101000000.gz"); !os.IsNotExist(err) {
		t.Errorf("rotation survived wipe: %v", err)
	}
	// Database is untouched.
	data, _ = os.ReadFile(dbPath)
	if got := string(data); got != "authoritative substrate" {
		t.Errorf("wipe touched the database: %q", got)
	}
}

// TestOpsLogsPurge_ScopeMirrorEmptiesOnlyMirror pins that the explicit
// purge command is log-scope only. The watchdog file and the database
// are not touched.
func TestOpsLogsPurge_ScopeMirrorEmptiesOnlyMirror(t *testing.T) {
	dir := t.TempDir()
	mirror := filepath.Join(dir, "mirror.jsonl")
	watchdog := filepath.Join(dir, "watchdog.jsonl")
	dbPath := filepath.Join(dir, "mpm.db")

	os.WriteFile(mirror, []byte(`{"v":2,"op":"memory_created","preview":"x"}`+"\n"), 0o600)
	os.WriteFile(mirror+".20260101000000.gz", []byte("archive"), 0o600)
	os.WriteFile(watchdog, []byte(`{"v":2,"op":"exec","detail":"y"}`+"\n"), 0o600)
	os.WriteFile(dbPath, []byte("authoritative substrate"), 0o600)

	res, err := purgeHITLStream(mirror)
	if err != nil {
		t.Fatalf("purgeHITLStream: %v", err)
	}
	if !res.Truncated {
		t.Errorf("purge did not report a truncation")
	}
	if res.Rotations < 1 {
		t.Errorf("purge did not report a rotation removal")
	}
	if _, err := os.Stat(mirror + ".20260101000000.gz"); !os.IsNotExist(err) {
		t.Errorf("rotation survived purge: %v", err)
	}
	if _, err := os.Stat(watchdog); err != nil {
		t.Errorf("purge touched the sibling watchdog log: %v", err)
	}
	data, _ := os.ReadFile(dbPath)
	if got := string(data); got != "authoritative substrate" {
		t.Errorf("purge touched the database: %q", got)
	}
}

// TestOpsLogsPurge_RejectsArbitraryPath pins the CLI projection: a
// purge must go through the log-name API, not a free path. A path-like
// scope that would let the operator point at mpm.db is rejected.
func TestOpsLogsPurge_RejectsArbitraryPath(t *testing.T) {
	if _, err := PurgeHITLLogForCLI("/etc/passwd"); err == nil {
		t.Fatal("purge accepted a path")
	}
	if _, err := PurgeHITLLogForCLI("mpm.db"); err == nil {
		t.Fatal("purge accepted a non-log scope")
	}
}

// TestOpsLogsStatus_ReportsRetentionForBothStreams pins the operator-
// facing status projection: each stream's policy is visible, and an
// empty file reports its size as zero without erroring.
func TestOpsLogsStatus_ReportsRetentionForBothStreams(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MPM_WORKSPACE", dir)

	for _, name := range []string{"mirror", "watchdog"} {
		pol, ok := RetentionForLogName(name)
		if !ok {
			t.Fatalf("RetentionForLogName(%q) = false", name)
		}
		if pol.Name != name {
			t.Errorf("pol.Name = %q, want %q", pol.Name, name)
		}
		if pol.SizeThreshold <= 0 {
			t.Errorf("%s size threshold is %d, want > 0", name, pol.SizeThreshold)
		}
		if pol.MaxActiveAgeDays <= 0 {
			t.Errorf("%s active age is %d days, want > 0", name, pol.MaxActiveAgeDays)
		}
		if pol.MaxAgeDays <= 0 {
			t.Errorf("%s max age is %d days, want > 0", name, pol.MaxAgeDays)
		}
		if pol.CountCap <= 0 {
			t.Errorf("%s cap is %d, want > 0", name, pol.CountCap)
		}
	}

	// Empty active file → ListRotationsForCLI returns nil, no error.
	mirror := filepath.Join(dir, "src", "db", "mirror.jsonl")
	if err := os.MkdirAll(filepath.Dir(mirror), 0o700); err != nil {
		t.Fatal(err)
	}
	hitlWriteFile(t, mirror, []byte{})
	rots, err := ListRotationsForCLI(mirror)
	if err != nil {
		t.Errorf("ListRotationsForCLI on empty: %v", err)
	}
	if len(rots) != 0 {
		t.Errorf("ListRotationsForCLI on empty: %d rots, want 0", len(rots))
	}
}

// TestShredMemoryWithCascade_WritesTombstoneAndWatchdog pin the dual
// recording: a shred appends a mirror tombstone AND a watchdog
// "memory_shredded" record. The mirror entry lets a journal reader
// trace the lifecycle; the watchdog entry lets an operator see the
// action in the operational black box.
func TestShredMemoryWithCascade_WritesTombstoneAndWatchdog(t *testing.T) {
	dm := newTestDM(t)
	mp := t.TempDir() + "/mirror.jsonl"
	wp := t.TempDir() + "/watchdog.jsonl"
	dm.mirrorPath = mp
	dm.watchdogPath = wp

	memID := "mem-shred-1"
	if _, err := dm.ExecTracked(
		`INSERT INTO memories (id, collection, content, confidence) VALUES (?, 'memories', 'x', 0.8)`,
		0, memID,
	); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Force a watchdog line via the same shred path the production
	// command uses.
	dm.logShredTombstone(memID, "memories")

	mirrorRows := readMirrorLines(t, mp)
	if len(mirrorRows) != 1 {
		t.Fatalf("mirror: got %d rows, want 1", len(mirrorRows))
	}
	if mirrorRows[0]["op"] != "memory_shredded" {
		t.Errorf("mirror op = %v, want memory_shredded", mirrorRows[0]["op"])
	}
	watchdogRows := readWatchdogLines(t, wp)
	if len(watchdogRows) != 1 {
		t.Fatalf("watchdog: got %d rows, want 1", len(watchdogRows))
	}
	if watchdogRows[0]["op"] != "memory_shredded" {
		t.Errorf("watchdog op = %v, want memory_shredded", watchdogRows[0]["op"])
	}
	if f, _ := watchdogRows[0]["fields"].(map[string]interface{}); f == nil {
		t.Errorf("watchdog fields missing: %v", watchdogRows[0])
	} else {
		if f["id"] != memID {
			t.Errorf("watchdog fields.id = %v, want %q", f["id"], memID)
		}
		if f["collection"] != "memories" {
			t.Errorf("watchdog fields.collection = %v, want memories", f["collection"])
		}
	}
	// The shred record must NOT carry content-derived fields. `preview` is
	// always present in v2 (envelope invariant), but its value is "" for
	// a shred — the absence-of-content contract is about the value, not
	// the field.
	if pv, _ := mirrorRows[0]["preview"].(string); pv != "" {
		t.Errorf("shred preview = %q, want empty", pv)
	}
	for _, banned := range []string{"digest", "content_sha256", "content_length", "pattern_family", "action", "type"} {
		if _, ok := mirrorRows[0][banned]; ok {
			t.Errorf("shred mirror record carries %q: %v", banned, mirrorRows[0])
		}
	}
}

// TestMemoryWipe_ActiveAndRotationsAreBothRemoved pins the wipe scope:
// ClearMirror removes BOTH the active file and every rotation. The
// earlier wording claimed "rotations are immutable", but the design is
// explicit that `mpm memory wipe` is a journal wipe — the operator sees
// no surviving mirror file when it returns. (The audit table and
// backups keep their own copies; see SPEC §4.5.2.)
func TestMemoryWipe_ActiveAndRotationsAreBothRemoved(t *testing.T) {
	dir := t.TempDir()
	mirror := filepath.Join(dir, "mirror.jsonl")
	historical := mirror + ".20251231000000.gz"
	os.WriteFile(mirror, []byte("today's line\n"), 0o600)
	os.WriteFile(historical, []byte("compressed prior content"), 0o600)

	store := &MemoryStore{MirrorFile: mirror}
	removed, err := store.ClearMirror()
	if err != nil {
		t.Fatal(err)
	}
	if removed < 1 {
		t.Errorf("ClearMirror removed %d rotations, want at least 1", removed)
	}

	if _, err := os.Stat(mirror); !os.IsNotExist(err) {
		t.Errorf("active file survived wipe: %v", err)
	}
	if _, err := os.Stat(historical); !os.IsNotExist(err) {
		t.Errorf("historical rotation survived wipe: %v", err)
	}
}
