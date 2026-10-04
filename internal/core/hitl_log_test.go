package internal

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The v2 HITL redesign has one load-bearing structural invariant: every
// writer for a stream goes through one common append/rotation path, and
// that path is the only code that opens the file. A second opener would
// reintroduce exactly the bespoke rotation logic the redesign deleted, so
// the invariant is enforced mechanically rather than by convention.

// hitlLogFileLiterals are the two managed file names.
var hitlLogFileLiterals = []string{"mirror.jsonl", "watchdog.jsonl"}

// hitlFileOpenFuncs are the calls that would put a writer in a position to
// write a HITL file. Directory creation is deliberately absent: making the
// directory a writer needs is not the same as writing the log.
//
// (name, package-qualified selector suffix)
var hitlFileOpenFuncs = map[string]bool{
	"OpenFile":    true,
	"Create":      true,
	"WriteFile":   true,
	"Open":        true,
	"Truncate":    true,
	"Remove":      true,
	"RemoveAll":   true,
	"Chmod":       false, // metadata only; the shared path uses it
	"WriteString": false,
}

// hitlPathOpenAllowlist are the functions permitted to touch a HITL file by
// name. They ARE the shared path: the stream writer, the rotation writer
// and the purge writer. Anything else naming one of these files while
// holding a file operation is a second writer.
var hitlPathOpenAllowlist = map[string]bool{
	"(*DatabaseManager).logWatchdogEvent": true,
	"appendMirrorLine":                    true,
	"hitlStreamFor":                       false, // path helper, no file op
	"rotateLogAt":                         true,
	"purgeHITLStream":                     true,
}

// TestHITLStream_AllWritersGoThroughTheSharedPath is the grep gate from the
// design, implemented as an AST walk rather than a text search.
//
// A text search would either miss a writer that builds the path from a
// variable (`hitlStreamFor(dm.mirrorPath, ...)`) or fire on every legitimate
// path construction. The invariant is narrower and more useful than either:
// a function that performs a file operation AND mentions a managed log name
// must be one of the shared-path functions.
func TestHITLStream_AllWritersGoThroughTheSharedPath(t *testing.T) {
	paths, err := trackedGoSourceFiles()
	if err != nil {
		t.Fatalf("enumerate source files: %v", err)
	}

	var checked int
	for _, path := range paths {
		fset := token.NewFileSet()
		file, parseErr := parser.ParseFile(fset, path, nil, 0)
		if parseErr != nil {
			continue // not our problem to report; the build gate owns it
		}
		checked++

		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			qualified := qualifyFunc(fn)
			if hitlPathOpenAllowlist[qualified] {
				continue
			}

			opensFile := false
			mentionsLog := false
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				switch node := n.(type) {
				case *ast.CallExpr:
					sel, isSel := node.Fun.(*ast.SelectorExpr)
					if !isSel {
						return true
					}
					if pkg, isIdent := sel.X.(*ast.Ident); isIdent && pkg.Name == "os" {
						if _, isOpen := hitlFileOpenFuncs[sel.Sel.Name]; isOpen {
							opensFile = opensFile || hitlFileOpenFuncs[sel.Sel.Name]
						}
					}
				case *ast.BasicLit:
					if node.Kind != token.STRING {
						return true
					}
					if s, err := strconv.Unquote(node.Value); err == nil {
						for _, lit := range hitlLogFileLiterals {
							if strings.Contains(s, lit) {
								mentionsLog = true
							}
						}
					}
				}
				return true
			})

			if opensFile && mentionsLog {
				t.Errorf("%s: %s performs a file operation and names a managed HITL log; "+
					"all writers must go through appendMirrorLine / logWatchdogEvent "+
					"(or the shared rotation and purge helpers)",
					fset.Position(fn.Pos()), qualified)
			}
		}
	}

	if checked == 0 {
		t.Fatal("no Go files were scanned — the gate passed because it inspected nothing")
	}
}

// trackedGoSourceFiles returns every Go source file tracked by the
// repository that this package and its in-tree callers could contain a
// writer in. The set is bounded by the actual source tree (not by a
// recursive filesystem walk) because `~/.mpm` is now both the canonical
// repository root and the runtime/install root: a raw walk would also
// visit `src/db`, `backups/`, `blobs/`, `run/`, `migrations/`, and any
// future runtime dir, none of which are source.
//
// The filter drops `_test.go` (the gate scans producer code, not tests
// that exist to validate the gate) and skips vendor / node_modules by
// construction: those directories are not tracked.
//
// Falling back to `git ls-files` keeps the test independent of the test
// runner's CWD: the source root is the parent of the directory
// containing go.mod that the test happens to be invoked from, and
// `git` resolves paths from the repo root.
func trackedGoSourceFiles() ([]string, error) {
	repoRoot, err := hitlRepoRoot()
	if err != nil {
		return nil, err
	}
	cmd := exec.Command("git", "-C", repoRoot, "ls-files", "-z", "--", "*.go")
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("git ls-files: %w", err)
	}
	var paths []string
	for _, p := range bytes.Split(bytes.TrimRight(stdout.Bytes(), "\x00"), []byte{0}) {
		if len(p) == 0 {
			continue
		}
		name := string(p)
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		full := filepath.Join(repoRoot, name)
		paths = append(paths, full)
	}
	return paths, nil
}

// hitlRepoRoot walks up from this test file's directory to find the
// enclosing repository. The directory containing go.mod is the repo
// root when the package is inside the main module; git is then used
// (above) to enumerate the source files actually tracked there.
func hitlRepoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for i := 0; i < 8; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", fmt.Errorf("could not locate repo root from %s", dir)
}

// qualifyFunc renders *ast.FuncDecl as "pkg.Func" or "(*Type).Method".
func qualifyFunc(fn *ast.FuncDecl) string {
	if fn.Recv != nil && len(fn.Recv.List) > 0 {
		return "(" + recvTypeName(fn.Recv.List[0].Type) + ")." + fn.Name.Name
	}
	return fn.Name.Name
}

func recvTypeName(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.StarExpr:
		return "*" + recvTypeName(t.X)
	case *ast.Ident:
		return t.Name
	case *ast.IndexExpr:
		return recvTypeName(t.X)
	case *ast.SelectorExpr:
		return t.Sel.Name
	default:
		return "?"
	}
}

// ==================== Rotation and retention ====================

// hitlTestPolicy is a policy small enough to exercise with real bytes and
// real files. Production numbers (5 MiB / 30 days) are the ones under test
// in TestHITLPolicy_ProductionValues below; here the shape of the rule
// matters, not the magnitude.
func hitlTestPolicy(name string, size int64, activeAge, maxAge time.Duration, cap int) streamPolicy {
	return streamPolicy{
		Name:          name,
		SizeThreshold: size,
		MaxActiveAge:  activeAge,
		MaxAge:        maxAge,
		CountCap:      cap,
	}
}

// hitlTestStream returns a stream bound to a temp path with an injectable
// clock, bypassing the process-wide registry so a test can move time
// without moving it for anything else.
func hitlTestStream(t *testing.T, pol streamPolicy, now *time.Time) *hitlStream {
	t.Helper()
	return &hitlStream{
		path:  filepath.Join(t.TempDir(), pol.Name+".jsonl"),
		pol:   pol,
		clock: func() time.Time { return *now },
	}
}

// TestHITLPolicy_ProductionValues pins the canonical retention table. These
// are the numbers an operator reads in `mpm ops logs status`; changing one
// is a product decision, not a refactor, and this test is where that shows
// up.
func TestHITLPolicy_ProductionValues(t *testing.T) {
	cases := []struct {
		pol       streamPolicy
		size      int64
		activeAge time.Duration
		maxAge    time.Duration
		cap       int
	}{
		{mirrorPolicy, 5 * 1024 * 1024, 30 * 24 * time.Hour, 365 * 24 * time.Hour, 12},
		{watchdogPolicy, 2 * 1024 * 1024, 30 * 24 * time.Hour, 90 * 24 * time.Hour, 5},
	}
	for _, tc := range cases {
		if tc.pol.SizeThreshold != tc.size {
			t.Errorf("%s size threshold = %d, want %d", tc.pol.Name, tc.pol.SizeThreshold, tc.size)
		}
		if tc.pol.MaxActiveAge != tc.activeAge {
			t.Errorf("%s max active age = %v, want %v", tc.pol.Name, tc.pol.MaxActiveAge, tc.activeAge)
		}
		if tc.pol.MaxAge != tc.maxAge {
			t.Errorf("%s max age = %v, want %v", tc.pol.Name, tc.pol.MaxAge, tc.maxAge)
		}
		if tc.pol.CountCap != tc.cap {
			t.Errorf("%s rotation cap = %d, want %d", tc.pol.Name, tc.pol.CountCap, tc.cap)
		}
	}
}

// TestHITLRotation_SizeTrigger pins the size half of "size OR age".
func TestHITLRotation_SizeTrigger(t *testing.T) {
	pol := hitlTestPolicy("watchdog", 256, 0, 0, 0)
	now := time.Now()
	s := hitlTestStream(t, pol, &now)

	for i := 0; i < 3; i++ {
		line := []byte(`{"v":2,"op":"exec","detail":"` + strings.Repeat("x", 120) + `"}`)
		if err := s.Append(line); err != nil {
			t.Fatalf("append: %v", err)
		}
	}

	rots, err := listRotations(s.path)
	if err != nil {
		t.Fatalf("listRotations: %v", err)
	}
	if len(rots) == 0 {
		t.Fatal("expected the size threshold to have produced at least one rotation")
	}

	// The active file must still be appendable and must hold only the
	// most recent line, not the pre-rotation history.
	data, err := os.ReadFile(s.path)
	if err != nil {
		t.Fatalf("read active: %v", err)
	}
	if n := strings.Count(strings.TrimRight(string(data), "\n"), "\n") + 1; n != 1 {
		t.Errorf("active file holds %d lines after rotation, want 1", n)
	}
}

// TestHITLRotation_AgeTrigger pins the age half, using an injected clock
// rather than a 30-day sleep. A test that waits for the real interval would
// pin the suite's runtime to the product's retention window, which is
// exactly the coupling the design forbids.
func TestHITLRotation_AgeTrigger(t *testing.T) {
	pol := hitlTestPolicy("mirror", 1<<30, 30*24*time.Hour, 0, 0) // size trigger deliberately unreachable
	now := time.Now()
	s := hitlTestStream(t, pol, &now)

	if err := s.Append([]byte(`{"v":2,"op":"memory_created","preview":"first"}`)); err != nil {
		t.Fatalf("append: %v", err)
	}
	if rots, _ := listRotations(s.path); len(rots) != 0 {
		t.Fatalf("rotated before the age trigger was due: %v", rots)
	}

	// One second short of the window: still no rotation.
	now = now.Add(30*24*time.Hour - time.Second)
	if rotated, err := rotateHITLIfNeeded(s.path, s.pol, now); err != nil || rotated {
		t.Fatalf("rotated early: rotated=%v err=%v", rotated, err)
	}

	// At the window: rotation happens and the content is archived.
	now = now.Add(time.Second)
	rotated, err := rotateHITLIfNeeded(s.path, s.pol, now)
	if err != nil {
		t.Fatalf("rotateHITLIfNeeded: %v", err)
	}
	if !rotated {
		t.Fatal("age trigger did not fire at MaxActiveAge")
	}
	rots, _ := listRotations(s.path)
	if len(rots) != 1 {
		t.Fatalf("expected 1 rotation, got %d", len(rots))
	}
	if err := s.Append([]byte(`{"v":2,"op":"memory_created","preview":"second"}`)); err != nil {
		t.Fatalf("append after age rotation: %v", err)
	}
	data, _ := os.ReadFile(s.path)
	if !strings.Contains(string(data), "second") {
		t.Error("active file does not hold the post-rotation append")
	}
}

// TestHITLRotation_EmptyFileNeverRotates pins that an empty active file is
// not archived. With an age trigger, a stream that is opened once and
// written to rarely would otherwise emit an empty .gz per append.
func TestHITLRotation_EmptyFileNeverRotates(t *testing.T) {
	pol := hitlTestPolicy("mirror", 1, time.Nanosecond, 0, 0)
	s := hitlTestStream(t, pol, ptrTime(time.Now().Add(365*24*time.Hour)))

	// Create an empty active file, then let the age trigger be far overdue.
	if err := s.Append([]byte(`{"v":2,"op":"memory_created","preview":"x"}`)); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := os.Truncate(s.path, 0); err != nil {
		t.Fatal(err)
	}
	if rotated, err := rotateHITLIfNeeded(s.path, s.pol, s.now()); err != nil || rotated {
		t.Fatalf("empty file rotated: rotated=%v err=%v", rotated, err)
	}
	if rots, _ := listRotations(s.path); len(rots) != 0 {
		t.Errorf("empty file produced rotations: %v", rots)
	}
}

func ptrTime(t time.Time) *time.Time { return &t }

// TestHITLRetention_ExpiresByAge pins that a rotation older than MaxAge is
// deleted regardless of the count cap.
func TestHITLRetention_ExpiresByAge(t *testing.T) {
	pol := hitlTestPolicy("watchdog", 1<<30, 0, 90*24*time.Hour, 5)
	s := hitlTestStream(t, pol, ptrTime(time.Now()))

	base := time.Now()
	for i, daysOld := range []int{10, 40, 91, 200, 365, 400} {
		name := fmt.Sprintf("%s.2026010%d000000.gz", s.path, i)
		hitlWriteFile(t, name, []byte("archive"))
		ts := base.Add(-time.Duration(daysOld) * 24 * time.Hour)
		if err := os.Chtimes(name, ts, ts); err != nil {
			t.Fatal(err)
		}
	}

	if err := enforceHITLRetention(s.path, pol, base); err != nil {
		t.Fatalf("enforceHITLRetention: %v", err)
	}

	rots, _ := listRotations(s.path)
	if len(rots) != 2 {
		var ages []string
		for _, r := range rots {
			ages = append(ages, r.path)
		}
		t.Fatalf("expected the 2 in-window rotations to survive, got %d: %v", len(rots), ages)
	}
}

// TestHITLRetention_CapsCountNewestFirst pins that a burst larger than the
// cap keeps the NEWEST rotations. Deleting the wrong end here is silent data
// loss: the recent history is the part an incident review needs.
func TestHITLRetention_CapsCountNewestFirst(t *testing.T) {
	pol := hitlTestPolicy("mirror", 1<<30, 0, 365*24*time.Hour, 3)
	s := hitlTestStream(t, pol, ptrTime(time.Now()))

	base := time.Now().Truncate(time.Second)
	var names []string
	for i := 0; i < 8; i++ {
		name := fmt.Sprintf("%s.202601010000%02d.gz", s.path, i)
		hitlWriteFile(t, name, []byte("archive"))
		ts := base.Add(-time.Duration(8-i) * time.Hour)
		if err := os.Chtimes(name, ts, ts); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}

	if err := enforceHITLRetention(s.path, pol, base); err != nil {
		t.Fatalf("enforceHITLRetention: %v", err)
	}
	rots, _ := listRotations(s.path)
	if len(rots) != 3 {
		t.Fatalf("cap not applied: %d rotations remain", len(rots))
	}
	// Newest first: indices 7, 6, 5.
	want := map[string]bool{names[7]: true, names[6]: true, names[5]: true}
	for _, r := range rots {
		if !want[r.path] {
			t.Errorf("surviving rotation %s is not among the 3 newest", r.path)
		}
	}
}

// TestHITLRetention_ExpiryRunsBeforeCap pins the ordering. Applying the cap
// first would let a cap-sized backlog of ancient rotations survive on count
// alone, which makes the disk usage a function of burst history rather than
// of time.
func TestHITLRetention_ExpiryRunsBeforeCap(t *testing.T) {
	pol := hitlTestPolicy("watchdog", 1<<30, 0, 90*24*time.Hour, 5)
	s := hitlTestStream(t, pol, ptrTime(time.Now()))

	base := time.Now()
	for i := 0; i < 5; i++ { // exactly at the cap
		name := fmt.Sprintf("%s.2020010100000%d.gz", s.path, i)
		hitlWriteFile(t, name, []byte("archive"))
		ts := base.Add(-time.Duration(400-i) * 24 * time.Hour)
		if err := os.Chtimes(name, ts, ts); err != nil {
			t.Fatal(err)
		}
	}

	if err := enforceHITLRetention(s.path, pol, base); err != nil {
		t.Fatalf("enforceHITLRetention: %v", err)
	}
	if rots, _ := listRotations(s.path); len(rots) != 0 {
		t.Errorf("an all-ancient backlog survived the cap: %d remain", len(rots))
	}
}

// TestHITLStream_FilePermissions pins 0600 on the active file. The journal
// and the black box both carry content excerpts, and config/fileperms.go
// already lists mirror.jsonl* as private material; os.Create would have
// produced 0644 under a default umask.
func TestHITLStream_FilePermissions(t *testing.T) {
	pol := hitlTestPolicy("mirror", 1<<30, 0, 0, 0)
	s := hitlTestStream(t, pol, ptrTime(time.Now()))

	if err := s.Append([]byte(`{"v":2,"op":"memory_created","preview":"x"}`)); err != nil {
		t.Fatalf("append: %v", err)
	}
	info, err := os.Stat(s.path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("active log mode = %o, want 600", got)
	}

	// Rotations are equally private.
	if err := rotateLogAt(s.path, time.Now()); err != nil {
		t.Fatalf("rotateLogAt: %v", err)
	}
	rots, _ := listRotations(s.path)
	if len(rots) != 1 {
		t.Fatalf("expected 1 rotation, got %d", len(rots))
	}
	rinfo, err := os.Stat(rots[0].path)
	if err != nil {
		t.Fatal(err)
	}
	if got := rinfo.Mode().Perm(); got != 0o600 {
		t.Errorf("rotation mode = %o, want 600", got)
	}
}

// TestRotateLogAt_SameSecondDoesNotOverwrite pins the de-duplicated
// rotation name. With an age trigger, a process that writes once a second
// across a 30-day boundary rotates repeatedly, and the pre-v2 os.Create
// silently truncated the archive it had just written.
func TestRotateLogAt_SameSecondDoesNotOverwrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mirror.jsonl")

	hitlWriteFile(t, path, []byte("first generation\n"))
	now := time.Now()
	if err := rotateLogAt(path, now); err != nil {
		t.Fatal(err)
	}
	hitlWriteFile(t, path, []byte("second generation\n"))
	if err := rotateLogAt(path, now); err != nil { // identical timestamp
		t.Fatal(err)
	}

	rots, _ := listRotations(path)
	if len(rots) != 2 {
		t.Fatalf("same-second rotation overwrote its predecessor: %d rotations", len(rots))
	}
	bodies := map[string]bool{}
	for _, r := range rots {
		f, err := os.Open(r.path)
		if err != nil {
			t.Fatal(err)
		}
		gz, err := gzip.NewReader(f)
		if err != nil {
			f.Close()
			t.Fatalf("gunzip %s: %v", r.path, err)
		}
		body, err := io.ReadAll(gz)
		gz.Close()
		f.Close()
		if err != nil {
			t.Fatal(err)
		}
		bodies[string(body)] = true
	}
	if !bodies["first generation\n"] || !bodies["second generation\n"] {
		t.Errorf("an archived generation was lost: %v", bodies)
	}
}

// TestHITLStream_CorruptRotationIsTolerated pins that a rotation which is
// not valid gzip — truncated by a crash, or written by something else
// entirely — is still counted and removed by filename, and never parsed.
// Retention and purge are filename-scoped by design; making them parse
// gzip would let one bad archive block all cleanup.
func TestHITLStream_CorruptRotationIsTolerated(t *testing.T) {
	pol := hitlTestPolicy("watchdog", 1<<30, 0, 90*24*time.Hour, 5)
	s := hitlTestStream(t, pol, ptrTime(time.Now()))

	hitlWriteFile(t, s.path, []byte(`{"v":2,"op":"exec"}`+"\n"))
	corrupt := s.path + ".20260101000000.gz"
	hitlWriteFile(t, corrupt, []byte("\x1f\x8b truncated garbage not gzip at all"))

	rots, err := listRotations(s.path)
	if err != nil {
		t.Fatalf("listRotations on a corrupt archive: %v", err)
	}
	if len(rots) != 1 {
		t.Fatalf("corrupt rotation not listed: %d", len(rots))
	}

	res, err := purgeHITLStream(s.path)
	if err != nil {
		t.Fatalf("purgeHITLStream on a corrupt archive: %v", err)
	}
	if res.Rotations != 1 {
		t.Errorf("purge removed %d rotations, want 1", res.Rotations)
	}
	if _, err := os.Stat(corrupt); !os.IsNotExist(err) {
		t.Error("corrupt rotation survived purge")
	}
}

// TestPurgeHITLStream_IsLogScopeOnly pins the design's destructive matrix:
// purging a log removes the log's copies and nothing else. The cognitive
// substrate is a different store, reached through `mpm memory shred`; a
// purge that reached into mpm.db would be a general content-erasure command
// wearing a log's name.
func TestPurgeHITLStream_IsLogScopeOnly(t *testing.T) {
	dir := t.TempDir()
	mirror := filepath.Join(dir, "mirror.jsonl")
	dbPath := filepath.Join(dir, "mpm.db")
	backup := filepath.Join(dir, "mpm.db.bak")

	hitlWriteFile(t, mirror, []byte(`{"v":2,"op":"memory_created","preview":"x"}`+"\n"))
	hitlWriteFile(t, dbPath, []byte("authoritative substrate"))
	hitlWriteFile(t, backup, []byte("a backup"))
	hitlWriteFile(t, mirror+".20260101000000.gz", []byte("archive"))

	if _, err := purgeHITLStream(mirror); err != nil {
		t.Fatalf("purge: %v", err)
	}

	if info, err := os.Stat(mirror); err != nil || info.Size() != 0 {
		t.Errorf("active log not truncated: err=%v", err)
	}
	if got := string(hitlReadFile(t, dbPath)); got != "authoritative substrate" {
		t.Errorf("purge touched the database: %q", got)
	}
	if got := string(hitlReadFile(t, backup)); got != "a backup" {
		t.Errorf("purge touched the backup: %q", got)
	}
}

// TestPurgeHITLLogForCLI_RejectsUnknownStream pins that the CLI purge
// cannot be pointed at an arbitrary path: it resolves by managed name.
func TestPurgeHITLLogForCLI_RejectsUnknownStream(t *testing.T) {
	if _, err := PurgeHITLLogForCLI("mpm.db"); err == nil {
		t.Fatal("purge accepted a non-log scope")
	}
	if _, err := PurgeHITLLogForCLI("../escape"); err == nil {
		t.Fatal("purge accepted a path-like scope")
	}
}

// TestHITLStream_AppendNeverFailsTheCaller pins the non-fatal contract at
// the lowest level: an unwritable path returns an error the caller may
// ignore, rather than panicking or blocking a memory write.
func TestHITLStream_AppendNeverFailsTheCaller(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "mirror.jsonl")
	hitlWriteFile(t, blocker, nil)
	if err := os.Chmod(dir, 0o500); err != nil { // read+execute: append must fail
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	s := &hitlStream{path: blocker, pol: mirrorPolicy, clock: time.Now}
	err := s.Append([]byte(`{"v":2,"op":"memory_created","preview":"x"}`))
	if err == nil {
		t.Skip("running as a user that ignores directory permissions")
	}
}

// ==================== SQL shape normalisation ====================

// hitlSQLSentinels are the credential shapes the design requires to be
// absent from every watchdog record. Each is a real-world token family,
// not a synthetic string, because the point is to prove the SHAPE is
// stripped rather than to prove one literal is filtered.
var hitlSQLSentinels = []struct {
	name  string
	value string
}{
	{"anthropic-key", "sk-ant-api03-" + strings.Repeat("AbCdEf0123", 3)},
	{"github-pat", "ghp_" + strings.Repeat("a1B2c3D4", 4) + "extra"},
	{"bearer-token", "Bearer abcdef0123456789"},
	{"private-key", "-----BEGIN RSA PRIVATE KEY-----\nMIIEow==\n-----END RSA PRIVATE KEY-----"},
}

// TestNormalizeSQLShape_ReducesToStructure pins the transformation: values
// and literals become `?`, identifiers survive, comments and whitespace
// collapse. Two statements that differ only in their literals produce the
// SAME shape — which is the entire reason the shape is safe to log.
func TestNormalizeSQLShape_ReducesToStructure(t *testing.T) {
	a := normalizeSQLShape(`SELECT id FROM memories WHERE collection = 'decisions' AND weight > 0.5 -- trailing note`)
	b := normalizeSQLShape(`SELECT id FROM memories WHERE collection = 'lessons' AND weight > 0.9 /* other note */`)
	if a != b {
		t.Errorf("statements differing only in literals produced different shapes:\n  %q\n  %q", a, b)
	}
	if strings.Contains(a, "decisions") || strings.Contains(a, "0.5") || strings.Contains(a, "trailing note") {
		t.Errorf("literal or comment survived normalisation: %q", a)
	}
	if !strings.Contains(a, "memories") {
		t.Errorf("identifier column lost: %q", a)
	}
}

// TestNormalizeSQLShape_KeepsQuotedIdentifiers pins that a double-quoted or
// bracketed name that is a real identifier stays readable — otherwise every
// `"weight"` in the schema turns into a `?` and the log stops describing
// anything.
func TestNormalizeSQLShape_KeepsQuotedIdentifiers(t *testing.T) {
	got := normalizeSQLShape(`SELECT "weight" FROM memories WHERE [collection] = 'x'`)
	if !strings.Contains(got, `"weight"`) {
		t.Errorf("double-quoted identifier was masked: %q", got)
	}
	if !strings.Contains(got, "[collection]") {
		t.Errorf("bracketed identifier was masked: %q", got)
	}
}

// TestNormalizeSQLShape_BoundsLength pins the 120-character ceiling. A
// shape longer than that is either generated or pathological, and either way
// it does not belong on a terminal line.
func TestNormalizeSQLShape_BoundsLength(t *testing.T) {
	got := normalizeSQLShape("SELECT " + strings.Repeat("col_a, col_b, ", 60) + "col_c FROM t")
	if len([]rune(got)) > sqlShapeMax {
		t.Errorf("shape is %d chars, over the %d bound: %q", len([]rune(got)), sqlShapeMax, got)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("truncated shape does not mark the cut: %q", got)
	}
}

// TestSQLShapeNeverCarriesBoundValues is the design's SQL secrecy contract,
// pinned per sentinel family. Each value is interpolated into a real
// statement, the statement is normalised, and the secret must be absent
// from the result.
//
// This is the "never log interpolated sensitive literals" half. The other
// half — "never log bound values" — is structural: a bound value is a Go
// argument and never appears in the statement text at all, which
// TestWatchdog_NeverLogsBoundValues demonstrates end to end.
func TestSQLShapeNeverCarriesBoundValues(t *testing.T) {
	for _, s := range hitlSQLSentinels {
		stmt := "INSERT INTO memories (id, content) VALUES ('" + s.value + "', 'body')"
		shape := normalizeSQLShape(stmt)
		if strings.Contains(shape, s.value) {
			t.Errorf("%s: secret value survived into the SQL shape: %q", s.name, shape)
		}
		if secretFragment := strings.SplitN(s.value, "\n", 2)[0]; len(secretFragment) > 12 &&
			strings.Contains(shape, secretFragment) {
			t.Errorf("%s: secret fragment survived into the SQL shape: %q", s.name, shape)
		}
	}
}

// TestRedactSensitiveText covers the defence-in-depth layer that runs on
// driver error text and free-form detail.
func TestRedactSensitiveText(t *testing.T) {
	for _, s := range hitlSQLSentinels {
		got := redactSensitiveText("driver said: " + s.value)
		if strings.Contains(got, strings.SplitN(s.value, "\n", 2)[0]) {
			t.Errorf("%s: redaction left the secret in place: %q", s.name, got)
		}
		if !strings.Contains(got, "[redacted:") {
			t.Errorf("%s: redaction did not leave a family marker: %q", s.name, got)
		}
	}
}

// ==================== helpers ====================

func hitlWriteFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func hitlReadFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return data
}
