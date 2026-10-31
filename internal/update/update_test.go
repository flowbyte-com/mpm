package update

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeBuilder assembles bundles from shell-script binaries. No Go
// toolchain, no network: hermetic and fast.
type fakeBuilder struct {
	commit  string
	version string
	// noPayload omits the plugin payload (validation must fail).
	noPayload bool
	// buildErr, when set, fails the build.
	buildErr error
	// extraFiles are written into the bundle root (e.g. ".git", "mode").
	extraFiles map[string]string
}

func (f fakeBuilder) BuildFromCheckout(srcDir, dstDir string) (string, error) {
	if f.buildErr != nil {
		return "", f.buildErr
	}
	// An honest builder reports the checkout it built from. When the
	// source is a real git checkout, use its HEAD; otherwise use the
	// canned commit.
	commit := f.commit
	if sha, err := gitHeadCommit(srcDir); err == nil {
		commit = sha
	}
	if commit == "" {
		commit = "0123456789abcdef0123456789abcdef01234567"
	}
	if err := os.MkdirAll(filepath.Join(dstDir, "bin"), 0755); err != nil {
		return "", err
	}
	for _, bin := range append(append([]string{}, runtimeBinaries...), UpdaterBinaryName) {
		script := fmt.Sprintf("#!/bin/sh\nif [ \"$1\" = \"--version\" ]; then echo \"commit: %s\"; echo \"version: %s\"; exit 0; fi\ncase \"$*\" in *health_check*) echo '{\"ok\":true}';; *) echo '{}';; esac\n", commit, f.version)
		if bin != "mpm" && bin != "mpm-update" {
			script = "#!/bin/sh\nexit 0\n"
		}
		if err := os.WriteFile(filepath.Join(dstDir, "bin", bin), []byte(script), 0755); err != nil {
			return "", err
		}
	}
	version := f.version
	if version == "" {
		version = "v0.0.0-test"
	}
	if err := WriteRelease(dstDir, commit, commit, 1700000000, version); err != nil {
		return "", err
	}
	if !f.noPayload {
		for _, p := range []string{
			"agent_installation/mpm-opencode/server.js",
			"agent_installation/mpm-opencode/dist/index.v2.js",
		} {
			full := filepath.Join(dstDir, p)
			if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
				return "", err
			}
			if err := os.WriteFile(full, []byte("// payload"), 0644); err != nil {
				return "", err
			}
		}
	}
	for name, content := range f.extraFiles {
		full := filepath.Join(dstDir, name)
		os.MkdirAll(filepath.Dir(full), 0755)
		os.WriteFile(full, []byte(content), 0644)
	}
	return commit, nil
}

// testLayout creates an isolated data root with a live database, a
// config file, and (optionally) an installed active release.
func testLayout(t *testing.T, activeRev string) string {
	t.Helper()
	if activeRev == "" {
		return testLayoutWithOldBuilder(t, "", nil)
	}
	return testLayoutWithOldBuilder(t, activeRev, fakeBuilder{commit: activeRev})
}

// testLayoutWithOldBuilder is testLayout with an explicit builder for
// the pre-installed release (lets a test install an old runtime whose
// health check behaves differently from the candidate's).
func testLayoutWithOldBuilder(t *testing.T, activeRev string, b Builder) string {
	t.Helper()
	root := t.TempDir()
	l := NewLayout(root)
	if err := os.MkdirAll(filepath.Join(root, "src", "db"), 0700); err != nil {
		t.Fatal(err)
	}
	mkDB(t, l.LiveDB(), map[string]string{
		"memories": "CREATE TABLE memories (id TEXT PRIMARY KEY, content TEXT)",
	})
	os.WriteFile(filepath.Join(root, "mpm_config.json"), []byte(`{"secret":"s3cret"}`), 0600)
	if activeRev != "" {
		dir := t.TempDir()
		if _, err := b.BuildFromCheckout(t.TempDir(), dir); err != nil {
			t.Fatal(err)
		}
		os.WriteFile(filepath.Join(dir, "RELEASE.json"), []byte(fmt.Sprintf(`{"revision":%q,"commit":%q,"short_commit":"12345678","build_epoch":1,"version":"v0","built_at":"x","bundle_layout":"versioned-bundle/1"}`, activeRev, activeRev)), 0644)
		if err := os.MkdirAll(l.Releases(), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(dir, l.ReleaseDir(activeRev)); err != nil {
			t.Fatal(err)
		}
		os.Symlink("releases/"+activeRev, l.Current())
	}
	return root
}

func mkDB(t *testing.T, path string, tables map[string]string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(path), 0700)
	os.Remove(path)
	db := openTestDB(t, path)
	for _, ddl := range tables {
		if _, err := db.Exec(ddl); err != nil {
			db.Close()
			t.Fatal(err)
		}
	}
	if _, err := db.Exec("INSERT INTO memories (id, content) VALUES ('m1','hello')"); err != nil {
		db.Close()
		t.Fatal(err)
	}
	// Owner-only, like production (data root 0700, restored copies
	// 0600): the writer scan's foreign-uid rule-outs depend on it.
	if err := os.Chmod(path, 0600); err != nil {
		db.Close()
		t.Fatal(err)
	}
	// Closed immediately, not at test cleanup: database/sql pools keep
	// the file descriptor open, and an idle pooled handle is still an
	// open file description — which is exactly what the writer scan
	// detects. (The deferred Cleanup close stays as a safety net.)
	db.Close()
}

// --- already up to date -------------------------------------------------

func TestUpdateAlreadyUpToDate(t *testing.T) {
	rev := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	root := testLayout(t, rev)
	o := Options{DataRoot: root, Fetcher: stubFetcher{rev: rev}}
	out, err := Update(context.Background(), o)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if !out.AlreadyCurrent {
		t.Fatalf("expected AlreadyCurrent, got %+v", out)
	}
}

type stubFetcher struct{ rev string }

func (s stubFetcher) LatestStable(tag string) (ReleaseInfo, error) {
	return ReleaseInfo{Tag: s.rev, Commit: s.rev, Assets: map[string]string{}}, nil
}
func (s stubFetcher) DownloadBundle(info ReleaseInfo, dir string) (string, error) {
	return "", fmt.Errorf("not needed")
}

// --- valid upgrade via local source -------------------------------------

func TestUpdateValidUpgradeLocal(t *testing.T) {
	oldRev := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	root := testLayout(t, oldRev)
	beforeCfg, _ := os.ReadFile(filepath.Join(root, "mpm_config.json"))
	srcDir := t.TempDir()
	mustGit(t, srcDir)
	newCommit, err := gitHeadCommit(srcDir)
	if err != nil {
		t.Fatal(err)
	}
	o := Options{DataRoot: root, WantLocal: srcDir, Builder: fakeBuilder{}, Now: func() time.Time { return time.Unix(1700000000, 0).UTC() }}
	_ = beforeCfg
	out, err := Update(context.Background(), o)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if out.ToRev != newCommit {
		t.Fatalf("ToRev = %q, want %q", out.ToRev, newCommit)
	}
	if _, rev, _ := CurrentReleaseTarget(root); rev != newCommit {
		t.Fatalf("pointer = %q, want %q", rev, newCommit)
	}
	// Previous release recorded.
	if got := previousRevOf(ReadPreviousRelease(root)); got != oldRev {
		t.Fatalf("previous = %q, want %q", got, oldRev)
	}
	// Stable updater installed outside releases/.
	if _, err := os.Stat(filepath.Join(root, StableUpdaterName)); err != nil {
		t.Fatalf("stable updater missing: %v", err)
	}
	// Config preserved byte-for-byte.
	afterCfg, _ := os.ReadFile(filepath.Join(root, "mpm_config.json"))
	if string(afterCfg) != `{"secret":"s3cret"}` {
		t.Fatalf("config changed: %q", afterCfg)
	}
	// History recorded.
	hist, _ := ReadHistory(root)
	if len(hist) != 1 || hist[0].Outcome != "success" {
		t.Fatalf("history = %+v", hist)
	}
	// Idempotent re-run.
	o2 := Options{DataRoot: root, WantLocal: srcDir, Builder: fakeBuilder{}}
	out2, err := Update(context.Background(), o2)
	if err != nil {
		t.Fatalf("re-run: %v", err)
	}
	if !out2.AlreadyCurrent {
		t.Fatalf("re-run should be AlreadyCurrent, got %+v", out2)
	}
}

func mustGit(t *testing.T, dir string) {
	t.Helper()
	run := func(args ...string) {
		t.Helper()
		cmd := gitCmd(dir, args...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init")
	run("config", "user.email", "t@t")
	run("config", "user.name", "t")
	os.WriteFile(filepath.Join(dir, "f"), []byte("x"), 0644)
	run("add", ".")
	run("commit", "-m", "x")
}

// --- invalid manifest ----------------------------------------------------

func TestUpdateInvalidManifestRefused(t *testing.T) {
	root := testLayout(t, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	srcDir := t.TempDir()
	mustGit(t, srcDir)
	bad := fakeBuilder{commit: "cccccccccccccccccccccccccccccccccccccccc"}
	// Corrupt the bundle after build via a wrapper builder.
	wrap := corruptingBuilder{inner: bad}
	o := Options{DataRoot: root, WantLocal: srcDir, Builder: wrap}
	if _, err := Update(context.Background(), o); err == nil {
		t.Fatal("expected validation failure")
	}
	if _, _, err := CurrentReleaseTarget(root); err != nil {
		t.Fatal("pointer must be untouched")
	}
}

type corruptingBuilder struct{ inner fakeBuilder }

func (c corruptingBuilder) BuildFromCheckout(src, dst string) (string, error) {
	commit, err := c.inner.BuildFromCheckout(src, dst)
	if err != nil {
		return "", err
	}
	os.WriteFile(filepath.Join(dst, "RELEASE.json"), []byte("{not json"), 0644)
	return commit, nil
}

// --- missing payload -----------------------------------------------------

func TestUpdateMissingPayloadRefused(t *testing.T) {
	root := testLayout(t, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	srcDir := t.TempDir()
	mustGit(t, srcDir)
	o := Options{DataRoot: root, WantLocal: srcDir, Builder: fakeBuilder{commit: "dddddddddddddddddddddddddddddddddddddddd", noPayload: true}}
	if _, err := Update(context.Background(), o); err == nil {
		t.Fatal("expected payload refusal")
	}
}

// --- failed build ----------------------------------------------------------

func TestUpdateFailedBuildChangesNothing(t *testing.T) {
	rev := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	root := testLayout(t, rev)
	srcDir := t.TempDir()
	mustGit(t, srcDir)
	o := Options{DataRoot: root, WantLocal: srcDir, Builder: fakeBuilder{buildErr: fmt.Errorf("boom")}}
	if _, err := Update(context.Background(), o); err == nil {
		t.Fatal("expected build failure")
	}
	if _, got, _ := CurrentReleaseTarget(root); got != rev {
		t.Fatalf("pointer moved to %q", got)
	}
}

// --- failed activation restores the pointer ---------------------------------

func TestUpdateFailedActivationRestoresPointer(t *testing.T) {
	rev := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	root := testLayout(t, rev)
	srcDir := t.TempDir()
	mustGit(t, srcDir)
	o := Options{DataRoot: root, WantLocal: srcDir, Builder: fakeBuilder{commit: "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"}, FailAt: "activate"}
	if _, err := Update(context.Background(), o); err == nil {
		t.Fatal("expected injected failure")
	}
	if _, got, _ := CurrentReleaseTarget(root); got != rev {
		t.Fatalf("pointer = %q, want restored %q", got, rev)
	}
}

// --- concurrent updates ------------------------------------------------------

func TestUpdateConcurrentRejected(t *testing.T) {
	root := t.TempDir()
	l, err := AcquireUpdateLock(root)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Release()
	if _, err := AcquireUpdateLock(root); err == nil {
		t.Fatal("second lock should be refused")
	} else if _, ok := err.(*UpdateLockedError); !ok {
		t.Fatalf("want UpdateLockedError, got %T", err)
	}
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			o := Options{DataRoot: root, WantLocal: "/nonexistent", Builder: fakeBuilder{}}
			_, errs[i] = Update(context.Background(), o)
		}(i)
	}
	wg.Wait()
	// Both fail (bad source), but neither may deadlock; at least the
	// lock path is exercised under contention.
}

// --- invalid source revision ---------------------------------------------------

func TestUpdateInvalidSourceRevision(t *testing.T) {
	root := testLayout(t, "")
	o := Options{DataRoot: root, WantLocal: "/nonexistent-dir"}
	if _, err := Update(context.Background(), o); err == nil {
		t.Fatal("expected error for missing source")
	}
	o = Options{DataRoot: root, WantLocal: t.TempDir(), Builder: fakeBuilder{}}
	if _, err := Update(context.Background(), o); err == nil {
		t.Fatal("expected error for non-git source without builder commit")
	}
}

// --- rollback: success and refusal ---------------------------------------------

func TestRollbackSuccess(t *testing.T) {
	oldRev := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	root := testLayout(t, oldRev)
	srcDir := t.TempDir()
	mustGit(t, srcDir)
	o := Options{DataRoot: root, WantLocal: srcDir, Builder: fakeBuilder{}}
	if _, err := Update(context.Background(), o); err != nil {
		t.Fatalf("update: %v", err)
	}
	rout, err := Rollback(Options{DataRoot: root})
	if err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if rout.ToRev != oldRev {
		t.Fatalf("ToRev = %q", rout.ToRev)
	}
	if _, got, _ := CurrentReleaseTarget(root); got != oldRev {
		t.Fatalf("pointer = %q", got)
	}
}

func TestRollbackRefusedAfterMigration(t *testing.T) {
	oldRev := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	root := testLayout(t, oldRev)
	srcDir := t.TempDir()
	mustGit(t, srcDir)
	o := Options{DataRoot: root, WantLocal: srcDir, Builder: fakeBuilder{}}
	if _, err := Update(context.Background(), o); err != nil {
		t.Fatalf("update: %v", err)
	}
	_, newCommit, _ := CurrentReleaseTarget(root)
	// Simulate a migration: add a column to the live database.
	db := openTestDB(t, NewLayout(root).LiveDB())
	if _, err := db.Exec("ALTER TABLE memories ADD COLUMN migrated INTEGER DEFAULT 0"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	_, rerr := Rollback(Options{DataRoot: root})
	if rerr == nil {
		t.Fatal("expected rollback refusal after schema change")
	}
	if _, ok := rerr.(*RollbackRefused); !ok {
		t.Fatalf("want RollbackRefused, got %T: %v", rerr, rerr)
	}
	// Pointer untouched.
	if _, got, _ := CurrentReleaseTarget(root); got != newCommit {
		t.Fatalf("pointer moved to %q", got)
	}
	// --with-db-restore --force performs both halves.
	rout, err := Rollback(Options{DataRoot: root, WithDBRestore: true, Force: true, Services: QuietServices{}})
	if err != nil {
		t.Fatalf("forced rollback with restore: %v", err)
	}
	if rout.ToRev != oldRev {
		t.Fatalf("ToRev = %q", rout.ToRev)
	}
}

// --- stale report --------------------------------------------------------------

func TestFormatStaleReport(t *testing.T) {
	rep := StaleReport{ActiveRev: "aaa", Processes: []LiveProcess{
		{PID: 1, Name: "mpm", Release: "aaa"},
		{PID: 2, Name: "mpm-scheduler", Release: "bbb"},
		{PID: 3, Name: "opencode", OpenCode: true},
	}}
	rep.Stale = []LiveProcess{rep.Processes[1]}
	rep.OpenCodeSessions = []LiveProcess{rep.Processes[2]}
	s := FormatStaleReport(rep)
	if !strings.Contains(s, "STALE") || !strings.Contains(s, "restart by hand") {
		t.Fatalf("report missing states:\n%s", s)
	}
}

// --- GitHub fetcher against a local server ---------------------------------------

func TestGitHubFetcherEndToEnd(t *testing.T) {
	bundle := buildTestTarball(t)
	sum := sha256.Sum256(bundle)
	sums := hex.EncodeToString(sum[:]) + "  mpm_v9.9.9_linux_amd64.tar.gz\n"
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/releases"):
			fmt.Fprintf(w, `[{"tag_name":"v9.9.9","target_commitish":"ffffffffffffffffffffffffffffffffffffffff","draft":false,"prerelease":false,"assets":[{"name":"mpm_v9.9.9_linux_amd64.tar.gz","browser_download_url":%q},{"name":"SHA256SUMS","browser_download_url":%q}]}]`, srv.URL+"/bundle.tgz", srv.URL+"/sums")
		case r.URL.Path == "/bundle.tgz":
			w.Write(bundle)
		case r.URL.Path == "/sums":
			w.Write([]byte(sums))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	f := GitHubFetcher{APIBase: srv.URL, Repository: "o/r"}
	info, err := f.LatestStable("")
	if err != nil {
		t.Fatal(err)
	}
	if info.Tag != "v9.9.9" {
		t.Fatalf("tag = %q", info.Tag)
	}
	dir := t.TempDir()
	extracted, err := f.DownloadBundle(info, dir)
	if err != nil {
		t.Fatal(err)
	}
	rep := ValidateBundle(extracted, "")
	if !rep.Valid {
		t.Fatalf("downloaded bundle invalid: %+v", rep.Checks)
	}
}

func TestGitHubFetcherChecksumMismatch(t *testing.T) {
	bundle := buildTestTarball(t)
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/releases"):
			fmt.Fprintf(w, `[{"tag_name":"v9.9.9","target_commitish":"ff","draft":false,"prerelease":false,"assets":[{"name":"mpm_v9.9.9_linux_amd64.tar.gz","browser_download_url":%q},{"name":"SHA256SUMS","browser_download_url":%q}]}]`, srv.URL+"/bundle.tgz", srv.URL+"/sums")
		case r.URL.Path == "/bundle.tgz":
			w.Write(bundle)
		case r.URL.Path == "/sums":
			w.Write([]byte(strings.Repeat("0", 64) + "  mpm_v9.9.9_linux_amd64.tar.gz\n"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	f := GitHubFetcher{APIBase: srv.URL, Repository: "o/r"}
	info, _ := f.LatestStable("")
	if _, err := f.DownloadBundle(info, t.TempDir()); err == nil {
		t.Fatal("expected checksum refusal")
	}
}

func TestGitHubFetcherInterruptedDownload(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/releases"):
			fmt.Fprintf(w, `[{"tag_name":"v9.9.9","target_commitish":"ff","draft":false,"prerelease":false,"assets":[{"name":"mpm_v9.9.9_linux_amd64.tar.gz","browser_download_url":%q},{"name":"SHA256SUMS","browser_download_url":%q}]}]`, srv.URL+"/bundle.tgz", srv.URL+"/sums")
		case r.URL.Path == "/bundle.tgz":
			w.Header().Set("Content-Length", "1000000")
			w.Write([]byte("truncated"))
		case r.URL.Path == "/sums":
			w.Write([]byte("x"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	f := GitHubFetcher{APIBase: srv.URL, Repository: "o/r"}
	info, _ := f.LatestStable("")
	if _, err := f.DownloadBundle(info, t.TempDir()); err == nil {
		t.Fatal("expected interrupted-download failure")
	}
}

// buildTestTarball assembles a minimal valid bundle archive.
func buildTestTarball(t *testing.T) []byte {
	t.Helper()
	dir := t.TempDir()
	b := fakeBuilder{commit: "ffffffffffffffffffffffffffffffffffffffff", version: "v9.9.9"}
	bundle, err := func() (string, error) {
		d := filepath.Join(dir, "b")
		c, err := b.BuildFromCheckout(dir, d)
		if err != nil {
			return "", err
		}
		os.WriteFile(filepath.Join(d, "RELEASE.json"), []byte(fmt.Sprintf(`{"revision":"v9.9.9","commit":%q,"short_commit":"ffffffff","build_epoch":1,"version":"v9.9.9","built_at":"x","bundle_layout":"versioned-bundle/1"}`, c)), 0644)
		return d, nil
	}()
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	filepath.Walk(bundle, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(bundle, p)
		if rel == "." {
			return nil
		}
		hdr := &tar.Header{Name: rel, Mode: int64(fi.Mode().Perm()), Size: 0}
		if fi.IsDir() {
			hdr.Typeflag = tar.TypeReg
			hdr.Name += "/.keep"
			raw := []byte{}
			hdr.Size = int64(len(raw))
			tw.WriteHeader(hdr)
			tw.Write(raw)
			return nil
		}
		raw, _ := os.ReadFile(p)
		hdr.Typeflag = tar.TypeReg
		hdr.Size = int64(len(raw))
		tw.WriteHeader(hdr)
		tw.Write(raw)
		return nil
	})
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

// --- parity: Go validator vs shell library ------------------------------------------

func TestBundleValidationMatchesShellLibrary(t *testing.T) {
	shell := readShellForbidden(t)
	if len(shell) == 0 {
		t.Skip("shell library not available")
	}
	got := map[string]bool{}
	for _, p := range forbiddenInBundle {
		got[p] = true
	}
	for _, p := range shell {
		if !got[p] {
			t.Errorf("shell forbids %q but the Go validator does not", p)
		}
	}
	for _, p := range forbiddenInBundle {
		found := false
		for _, s := range shell {
			if s == p {
				found = true
			}
		}
		if !found {
			t.Errorf("Go validator forbids %q but the shell library does not", p)
		}
	}
}

func readShellForbidden(t *testing.T) []string {
	t.Helper()
	for _, root := range []string{".", "..", "../.."} {
		raw, err := os.ReadFile(filepath.Join(root, "scripts", "lib", "runtime_bundle.sh"))
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(raw), "\n") {
			if strings.HasPrefix(line, "RB_FORBIDDEN_IN_BUNDLE=") {
				v := strings.Trim(strings.TrimPrefix(line, "RB_FORBIDDEN_IN_BUNDLE="), `"`)
				return strings.Fields(v)
			}
		}
	}
	return nil
}

// --- helpers --------------------------------------------------------------------------

func gitCmd(dir string, args ...string) *testCmd {
	return &testCmd{dir: dir, args: args}
}
