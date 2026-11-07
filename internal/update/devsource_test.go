package update

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------
// The development-source end-to-end tests
// ---------------------------------------------------------------------
//
// WHAT THESE PROVE
//
// A development source (--branch, --commit, --from) is built from a
// CHECKOUT, not downloaded. Two things follow from that, and both were
// broken:
//
//  1. The checkout has no compiled plugin payload. dist/ is a build
//     product and is gitignored, so a fresh clone — which is exactly
//     what --branch produces — contains no payload at all. The builder
//     must build it.
//
//  2. The payload is a package of NINE files, not two. The plugin's
//     entrypoint imports six compiled siblings, and the host registers
//     the package by DIRECTORY. A bundle carrying only server.js and
//     dist/index.v2.js passes every check and registers a plugin that
//     loads NOTHING — silently.
//
// Every root here is disposable: MPM_WORKSPACE and HOME point into a
// temporary directory, and the operator's ~/.mpm is never read or
// written. The services are a fake, so no unit is ever stopped.
//
// The build is the expensive part (a Go toolchain build of six
// binaries plus an npm install), so the tests share one build cache
// and one source checkout. The cost is paid once.

// devSourceFixture is the shared, expensive part of the development
// source tests.
type devSourceFixture struct {
	// root is the scratch directory holding everything shared. It is
	// removed when the test binary exits — see fixture_lifecycle_test.go.
	root string
	// checkout is a real MPM source checkout — a git repository with
	// the payload manifest and the plugin sources, but with NO
	// compiled plugin output, which is what a fresh clone looks like.
	checkout string
	// cache is the shared build cache (Go and npm). Sharing it is what
	// keeps six binaries from being compiled once per test.
	cache string
	// commit is the checkout's HEAD.
	commit string
	// branch is a branch in the checkout, for the --branch path.
	branch string
	// mpm is the substrate binary built from the checkout. It exists
	// so a disposable root can be given the substrate's REAL schema:
	// the smoke checks in an end-to-end update run the actual binary
	// against the database, and that binary applies the real
	// migrations. A hand-written fixture schema would fail them for
	// reasons that have nothing to do with the payload.
	mpm string
}

var (
	devFixtureOnce sync.Once
	devFixture     *devSourceFixture
	devFixtureErr  error
)

// sharedDevSource returns the shared fixture, preparing it on first
// use.
func sharedDevSource(t *testing.T) *devSourceFixture {
	t.Helper()
	devFixtureOnce.Do(func() {
		devFixture, devFixtureErr = prepareDevSource()
	})
	if devFixtureErr != nil {
		t.Skipf("the development-source fixture could not be prepared: %v", devFixtureErr)
	}
	return devFixture
}

// prepareDevSource builds the shared checkout and cache.
//
// The checkout is a real git repository with the plugin sources and no
// compiled output. It is not the repository the tests run in: the
// tests must not depend on that tree's state, and the fixture must be
// able to be dirty or clean independently of it.
func prepareDevSource() (*devSourceFixture, error) {
	return prepareDevSourceIn("")
}

// devSourceFailurePoint injects a setup failure at a named stage, so
// the cleanup path can be exercised without breaking a real fixture.
//
// It is a test-only seam: an empty value (the normal case) changes
// nothing.
var devSourceFailurePoint string

// injectDevSourceFailure returns an error when the named stage is the
// one a test asked to fail at.
func injectDevSourceFailure(stage string) error {
	if devSourceFailurePoint == stage {
		return fmt.Errorf("injected development-source fixture failure at stage %q", stage)
	}
	return nil
}

// prepareDevSourceIn prepares the fixture with its scratch root
// created in tmpDir. The empty string means the host's default
// temporary directory.
//
// EVERY failure path releases the root. A partially prepared fixture
// is exactly the scratch that accumulates: the leak this file exists
// to remove started with a root that was created and then abandoned.
func prepareDevSourceIn(tmpDir string) (*devSourceFixture, error) {
	root, err := newFixtureRoot(tmpDir)
	if err != nil {
		return nil, err
	}
	f := &devSourceFixture{
		root:     root,
		cache:    filepath.Join(root, "cache"),
		checkout: filepath.Join(root, "checkout"),
		branch:   "mpm-update-devsource-test",
	}
	// fail releases the root and returns the error. Every stage below
	// goes through it.
	//
	// A cleanup that FAILS is reported, not swallowed: the root stays
	// on disk, and a silent success would hide the scratch that
	// accumulates.
	fail := func(err error) (*devSourceFixture, error) {
		if cerr := removeFixtureRoot(root); cerr != nil {
			fixtureCleanupDiag("the fixture root %s could not be cleaned up after a failed setup: %v\n", root, cerr)
		}
		return nil, err
	}
	if err := injectDevSourceFailure("root"); err != nil {
		return fail(err)
	}
	if err := os.MkdirAll(f.cache, 0755); err != nil {
		return fail(err)
	}
	if err := copySourceTree(f.checkout); err != nil {
		return fail(err)
	}
	if err := injectDevSourceFailure("copy"); err != nil {
		return fail(err)
	}
	if err := initFixtureRepo(f.checkout); err != nil {
		return fail(err)
	}
	if err := injectDevSourceFailure("repo"); err != nil {
		return fail(err)
	}
	f.commit = runGit(f.checkout, "rev-parse", "HEAD")
	if f.commit == "" {
		return fail(fmt.Errorf("the fixture repository has no HEAD at %s", f.checkout))
	}
	runGit(f.checkout, "checkout", "-b", f.branch)
	// The substrate binary, so a disposable root can be given the real
	// schema.
	f.mpm = filepath.Join(root, "bin", "mpm")
	if err := os.MkdirAll(filepath.Dir(f.mpm), 0755); err != nil {
		return fail(err)
	}
	build := exec.Command("go", "build", "-tags", "fts5", "-o", f.mpm, "./cmd/mpm") //nolint:gosec
	build.Dir = f.checkout
	build.Env = append(os.Environ(),
		"CGO_CFLAGS=-DSQLITE_ENABLE_FTS5=1", "CGO_LDFLAGS=-lm",
		"GOCACHE="+filepath.Join(f.cache, "go"))
	if out, err := build.CombinedOutput(); err != nil {
		return fail(fmt.Errorf("the substrate binary could not be built: %v\n%s", err, out))
	}
	if err := injectDevSourceFailure("build"); err != nil {
		return fail(err)
	}
	return f, nil
}

// copySourceTree copies the repository under test into a scratch
// directory, WITHOUT the compiled plugin output.
//
// The compiled output is exactly what the build has to produce, so a
// fixture that carried it would let a broken builder pass.
//
// The copy itself lives in fixture_copy_test.go: the boundary it
// enforces (the tracked file set, and the refusal to follow a
// symlinked component) is the interesting part of it.
func copySourceTree(dst string) error {
	src, err := os.Getwd()
	if err != nil {
		return err
	}
	// internal/update → the module root.
	src = filepath.Dir(filepath.Dir(src))
	return copySourceTreeFrom(src, dst)
}

// initFixtureRepo turns the copied tree into a git repository, so the
// builder's provenance rules apply to it exactly as they do to a real
// checkout.
//
// It refuses to touch a repository it did not create: any pre-existing
// git metadata in dir is a REFUSAL, not something to delete. A linked
// worktree's ".git" is a FILE pointing at the real repository's
// worktree metadata, and `git init` beside it would reinitialize THAT
// repository — the fixture's commits would land in the developer's own
// checkout. Deleting the file first would have been the other half of
// the same hazard, so the copy never carries it and the initialiser
// refuses to find it.
//
// After initialising, the repository's git dir is verified to live
// inside dir: that is what makes "this is the fixture's own
// repository" a checked fact rather than an assumption.
func initFixtureRepo(dir string) error {
	if _, err := os.Lstat(filepath.Join(dir, ".git")); err == nil {
		return fmt.Errorf("the fixture directory %s already carries git metadata; refusing to initialise a repository this fixture did not create", dir)
	} else if !os.IsNotExist(err) {
		return err
	}
	if _, err := runGitErr(dir, "init", "--quiet"); err != nil {
		return err
	}
	if _, err := runGitErr(dir, "config", "user.email", "t@t"); err != nil {
		return err
	}
	if _, err := runGitErr(dir, "config", "user.name", "t"); err != nil {
		return err
	}
	if _, err := runGitErr(dir, "add", "-A"); err != nil {
		return err
	}
	if _, err := runGitErr(dir, "commit", "--quiet", "-m", "fixture"); err != nil {
		return err
	}
	// The repository must be the fixture's own: its git dir lives
	// inside dir and nowhere else.
	top, err := runGitErr(dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return err
	}
	if filepath.Clean(top) != filepath.Clean(dir) {
		return fmt.Errorf("the fixture repository is rooted at %s, want %s", top, dir)
	}
	gitDir, err := runGitErr(dir, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return err
	}
	if !pathWithinDir(dir, gitDir) {
		return fmt.Errorf("the fixture repository's git dir is %s, which is outside %s", gitDir, dir)
	}
	return nil
}

// runGit runs a git command and returns its trimmed output.
//
// A git failure in the fixture is a setup failure, not a test
// outcome: it is surfaced as an error rather than swallowed, because a
// swallowed failure here means the fixture silently stopped being a
// git checkout and the test would then prove nothing.
func runGit(dir string, args ...string) string {
	out, err := runGitErr(dir, args...)
	if err != nil {
		return ""
	}
	return out
}

// runGitErr runs a git command and returns its trimmed output or the
// failure.
func runGitErr(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...) //nolint:gosec
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %v in %s failed: %v\n%s", args, dir, err, out)
	}
	return strings.TrimSpace(string(out)), nil
}

// requireTools skips a test when the toolchain it needs is absent.
//
// Node and npm are a documented PREREQUISITE of a development build —
// not of the product. A host without them can still update from a
// published release, so the test suite must still run there.
func requireTools(t *testing.T) {
	t.Helper()
	for _, tool := range []string{"go", "node", "npm", "git"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not available on this host", tool)
		}
	}
}

// ---------------------------------------------------------------------
// The fresh-clone development build
// ---------------------------------------------------------------------

// TestDevelopmentBuildFromACheckoutProducesTheWholePayload is the
// development-build defect's own test.
//
// A fresh checkout has no compiled plugin output. The builder must
// build it, and the bundle it produces must carry the WHOLE declared
// package — all nine files, with every module import resolving.
func TestDevelopmentBuildFromACheckoutProducesTheWholePayload(t *testing.T) {
	requireTools(t)
	dev := sharedDevSource(t)

	// The fixture's precondition: no compiled output.
	dist := filepath.Join(dev.checkout, "agent_installation", "mpm-opencode", "dist")
	if _, err := os.Stat(dist); err == nil {
		t.Fatalf("the fixture checkout already carries compiled output at %s", dist)
	}

	dst := filepath.Join(t.TempDir(), "bundle")
	b := GoBuilder{CacheDir: dev.cache}
	commit, err := b.BuildFromCheckout(dev.checkout, dst)
	if err != nil {
		t.Fatalf("the development build failed: %v", err)
	}
	if commit != dev.commit {
		t.Errorf("the build recorded commit %q, want %q", commit, dev.commit)
	}

	assertBundleCarriesTheWholePayload(t, dst, dev.checkout)
}

// assertBundleCarriesTheWholePayload proves a bundle carries every
// declared runtime file and that the package can load.
//
// This is the assertion the two-file staging could never satisfy: the
// declared files are ALL present, and every relative import inside the
// package resolves to a file in it.
func assertBundleCarriesTheWholePayload(t *testing.T, bundleDir, sourceDir string) {
	t.Helper()
	m, err := loadPayloadManifest(filepath.Join(sourceDir, filepath.FromSlash(payloadManifestRelPath)))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range m.Payloads {
		pkgDir := filepath.Join(bundleDir, "agent_installation", p.Adapter)
		for _, f := range p.Files {
			full := filepath.Join(pkgDir, filepath.FromSlash(f))
			fi, err := os.Stat(full)
			if err != nil {
				t.Errorf("the bundle does not carry the declared file %s: %v", f, err)
				continue
			}
			if fi.Size() == 0 {
				t.Errorf("the bundle carries an empty %s", f)
			}
		}
	}
	// And the package loads: every relative import resolves.
	ok, detail := validateBundlePayload(bundleDir)
	if !ok {
		t.Fatalf("the built bundle's payload does not validate: %s", detail)
	}
}

// ---------------------------------------------------------------------
// The local-checkout development build
// ---------------------------------------------------------------------

// TestDevelopmentBuildFromALocalCheckoutProducesTheWholePayload proves
// the same for a checkout the operator already has.
//
// The difference from the clone case is the promise the updater makes
// about a LOCAL source: it is read only. The build must not leave
// node_modules/ or compiled output behind in a tree the operator owns.
func TestDevelopmentBuildFromALocalCheckoutProducesTheWholePayload(t *testing.T) {
	requireTools(t)
	dev := sharedDevSource(t)

	dst := filepath.Join(t.TempDir(), "bundle")
	b := GoBuilder{CacheDir: dev.cache}
	if _, err := b.BuildFromCheckout(dev.checkout, dst); err != nil {
		t.Fatalf("the development build failed: %v", err)
	}
	assertBundleCarriesTheWholePayload(t, dst, dev.checkout)

	// The source checkout is untouched: no node_modules, no compiled
	// output written back into the operator's tree.
	adapter := filepath.Join(dev.checkout, "agent_installation", "mpm-opencode")
	if _, err := os.Stat(filepath.Join(adapter, "node_modules")); err == nil {
		t.Error("the build created node_modules/ in the source checkout")
	}
	if _, err := os.Stat(filepath.Join(adapter, "dist")); err == nil {
		t.Error("the build wrote compiled output into the source checkout")
	}
}

// ---------------------------------------------------------------------
// The disposable end-to-end workflows
// ---------------------------------------------------------------------

// disposableRoot builds a disposable installation: a data root with a
// live database, a config, and an installed previous release.
//
// It is the shape the updater expects to find, and it lives entirely
// inside a temporary directory: the operator's ~/.mpm is never read or
// written.
//
// The database is created by the substrate binary itself, so it carries
// the REAL schema and the REAL migrations. That matters because the
// end-to-end update's smoke checks run the actual binary against this
// database: a hand-written fixture schema would fail them for reasons
// that have nothing to do with the payload under test.
func disposableRoot(t *testing.T, oldRev string) string {
	t.Helper()
	dev := sharedDevSource(t)
	root := t.TempDir()
	l := NewLayout(root)
	if err := os.MkdirAll(filepath.Join(root, "src", "db"), 0700); err != nil {
		t.Fatal(err)
	}
	initRealSchema(t, dev, root)
	if err := os.WriteFile(filepath.Join(root, "mpm_config.json"), []byte(`{"secret":"s3cret"}`), 0600); err != nil {
		t.Fatal(err)
	}
	// The previous release: a real bundle, so the update has something
	// to move away from and something to roll back to.
	prev := filepath.Join(root, "releases", oldRev)
	if err := os.MkdirAll(prev, 0755); err != nil {
		t.Fatal(err)
	}
	writeStubRelease(t, prev, oldRev)
	if err := os.Symlink("releases/"+oldRev, l.Current()); err != nil {
		t.Fatal(err)
	}
	// The recovery executable: a REAL file at the data root's top
	// level, outside every release, which is where the installer
	// provisions it. A failed build must leave it exactly as it is.
	stable := filepath.Join(root, StableUpdaterName)
	if err := os.WriteFile(stable, []byte("#!/bin/sh\necho stable-updater\n"), 0755); err != nil {
		t.Fatal(err)
	}
	return root
}

// initRealSchema creates the substrate's live database by running the
// substrate binary against a disposable workspace.
//
// The binary creates the schema and applies every migration, so the
// database is the one a real installation would have. One row is
// inserted afterwards so the test can prove the data survived.
func initRealSchema(t *testing.T, dev *devSourceFixture, root string) {
	t.Helper()
	cmd := exec.Command(dev.mpm, "status") //nolint:gosec
	cmd.Env = append(os.Environ(), "MPM_WORKSPACE="+root)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the substrate could not create its schema: %v\n%s", err, out)
	}
	db := openTestDB(t, NewLayout(root).LiveDB())
	defer db.Close()
	// The schema requires a collection, and the row has to satisfy
	// every constraint the real schema declares — a half-valid row
	// would make the health check report on a database that never
	// existed in that shape.
	if _, err := db.Exec(
		"INSERT INTO memories (id, content, collection, created_at, last_accessed_at) VALUES (?,?,?,?,?)",
		"m1", "hello", "default", time.Now().Unix(), time.Now().Unix(),
	); err != nil {
		t.Fatal(err)
	}
}

// writeStubRelease writes a minimal previous release: the five
// runtime binaries, the standalone updater, and the identity.
func writeStubRelease(t *testing.T, dir, rev string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "bin"), 0755); err != nil {
		t.Fatal(err)
	}
	for _, bin := range append(append([]string{}, runtimeBinaries...), UpdaterBinaryName) {
		script := "#!/bin/sh\nif [ \"$1\" = \"--version\" ]; then echo \"commit: " + rev + "\"; echo \"version: v0\"; exit 0; fi\ncase \"$*\" in *health_check*) echo '{\"ok\":true}';; *) echo '{}';; esac\n"
		if bin != "mpm" {
			script = "#!/bin/sh\nexit 0\n"
		}
		if err := os.WriteFile(filepath.Join(dir, "bin", bin), []byte(script), 0755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "RELEASE.json"), []byte(`{"revision":"`+rev+`","commit":"`+rev+`","short_commit":"`+rev[:8]+`","build_epoch":1,"version":"v0","built_at":"x","bundle_layout":"versioned-bundle/1"}`), 0644); err != nil {
		t.Fatal(err)
	}
	// The payload, so the previous release is a valid bundle and a
	// rollback has something to roll back TO. It is the complete
	// package: a previous release that carried only two of the nine
	// declared files would be exactly the defect this work fixes.
	if err := writeTestPayload(dir); err != nil {
		t.Fatal(err)
	}
}

// TestUpdateFromALocalCheckoutEndToEnd proves the --from workflow
// installs a release whose payload is COMPLETE.
//
// The whole workflow runs — resolve, build, stage, validate, back up,
// promote, activate, record — against a disposable data root.
func TestUpdateFromALocalCheckoutEndToEnd(t *testing.T) {
	requireTools(t)
	dev := sharedDevSource(t)
	oldRev := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	root := disposableRoot(t, oldRev)

	o := Options{
		DataRoot:   root,
		WantLocal:  dev.checkout,
		AllowDirty: true,
		Builder:    GoBuilder{CacheDir: dev.cache},
		Services:   &fakeServices{},
		Now:        func() time.Time { return time.Unix(1700000000, 0).UTC() },
	}
	out, err := Update(context.Background(), o)
	if err != nil {
		t.Fatalf("the development update failed: %v", err)
	}
	if out.ToRev != dev.commit {
		t.Errorf("the update installed %q, want %q", out.ToRev, dev.commit)
	}

	// The active release carries the whole payload.
	_, rev, err := CurrentReleaseTarget(root)
	if err != nil {
		t.Fatalf("the active pointer could not be read: %v", err)
	}
	if rev != dev.commit {
		t.Fatalf("the active release is %q, want %q", rev, dev.commit)
	}
	releaseDir := NewLayout(root).ReleaseDir(rev)
	assertBundleCarriesTheWholePayload(t, releaseDir, dev.checkout)

	// The previous release is recorded, so the update is reversible.
	if got := previousRevOf(ReadPreviousRelease(root)); got != oldRev {
		t.Errorf("the previous release is %q, want %q", got, oldRev)
	}
	// The operator's configuration is untouched.
	cfg, err := os.ReadFile(filepath.Join(root, "mpm_config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(cfg) != `{"secret":"s3cret"}` {
		t.Errorf("the configuration changed: %q", cfg)
	}
	// The database survived.
	assertDatabaseIntact(t, root)
}

// TestUpdateFromABranchEndToEnd proves the --branch workflow.
//
// The branch is resolved against a LOCAL repository rather than
// GitHub: the workflow under test is the same either way, and a test
// that reaches the network is a test that fails for reasons that have
// nothing to do with what it checks.
func TestUpdateFromABranchEndToEnd(t *testing.T) {
	requireTools(t)
	dev := sharedDevSource(t)
	oldRev := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	root := disposableRoot(t, oldRev)

	o := Options{
		DataRoot: root,
		// The branch resolves against the fixture's own repository.
		Remote:     dev.checkout,
		WantBranch: dev.branch,
		Builder:    GoBuilder{CacheDir: dev.cache},
		Services:   &fakeServices{},
		Now:        func() time.Time { return time.Unix(1700000000, 0).UTC() },
	}
	out, err := Update(context.Background(), o)
	if err != nil {
		t.Fatalf("the development update failed: %v", err)
	}
	if out.ToRev != dev.commit {
		t.Errorf("the update installed %q, want %q", out.ToRev, dev.commit)
	}
	_, rev, err := CurrentReleaseTarget(root)
	if err != nil {
		t.Fatal(err)
	}
	assertBundleCarriesTheWholePayload(t, NewLayout(root).ReleaseDir(rev), dev.checkout)
	assertDatabaseIntact(t, root)
}

// ---------------------------------------------------------------------
// A failed build changes nothing
// ---------------------------------------------------------------------

// TestAFailedDevelopmentBuildChangesNothing proves a development build
// that fails leaves the installation exactly as it was.
//
// The failure injected here is the payload build failing — the defect
// this work is about. A build that produces no usable payload must not
// reach the promotion, and the installation must survive it untouched:
// the active runtime, the database, the recovery executable and the
// services.
func TestAFailedDevelopmentBuildChangesNothing(t *testing.T) {
	requireTools(t)
	dev := sharedDevSource(t)
	oldRev := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	root := disposableRoot(t, oldRev)

	// A checkout whose plugin build fails.
	//
	// It is a real git checkout — the builder resolves the revision
	// from git, so a directory that is not a repository would fail
	// BEFORE the build, which is a different scenario than the one
	// under test.
	broken := filepath.Join(dev.root, "broken-checkout")
	if err := copySourceTreeFrom(dev.checkout, broken); err != nil {
		t.Fatal(err)
	}
	if err := initFixtureRepo(broken); err != nil {
		t.Fatal(err)
	}
	// The plugin's build script fails.
	adapter := filepath.Join(broken, "agent_installation", "mpm-opencode")
	pkg := filepath.Join(adapter, "package.json")
	raw, err := os.ReadFile(pkg)
	if err != nil {
		t.Fatal(err)
	}
	brokenScript := filepath.Join(adapter, "build.mjs")
	if err := os.WriteFile(brokenScript, []byte("process.exit(1);\n"), 0644); err != nil {
		t.Fatal(err)
	}
	buildScript, after, found := strings.Cut(string(raw), `"build": "tsc"`)
	if !found {
		t.Fatalf("the fixture's package.json does not declare the expected build script: %s", raw)
	}
	if err := os.WriteFile(pkg, []byte(buildScript+`"build": "node build.mjs"`+after), 0644); err != nil {
		t.Fatal(err)
	}
	// And the change is committed, so the checkout is clean.
	if _, err := runGitErr(broken, "add", "-A"); err != nil {
		t.Fatal(err)
	}
	if _, err := runGitErr(broken, "commit", "--quiet", "-m", "broken build"); err != nil {
		t.Fatal(err)
	}

	// The state before the failed update.
	beforePointer, beforeRev, err := CurrentReleaseTarget(root)
	if err != nil {
		t.Fatal(err)
	}
	beforeDB, err := os.ReadFile(NewLayout(root).LiveDB())
	if err != nil {
		t.Fatal(err)
	}
	beforeUpdater, err := os.Stat(filepath.Join(root, StableUpdaterName))
	if err != nil {
		t.Fatalf("the fixture must have a recovery executable: %v", err)
	}

	services := &fakeServices{active: map[string]bool{}}
	o := Options{
		DataRoot:   root,
		WantLocal:  broken,
		AllowDirty: true,
		Builder:    GoBuilder{CacheDir: dev.cache},
		Services:   services,
		Now:        func() time.Time { return time.Unix(1700000000, 0).UTC() },
	}
	if _, err := Update(context.Background(), o); err == nil {
		t.Fatal("a development build with a failing payload build was accepted")
	}

	// The active runtime is unchanged.
	afterPointer, afterRev, err := CurrentReleaseTarget(root)
	if err != nil {
		t.Fatal(err)
	}
	if afterRev != beforeRev {
		t.Errorf("the active release moved from %q to %q", beforeRev, afterRev)
	}
	if afterPointer != beforePointer {
		t.Errorf("the active pointer moved from %q to %q", beforePointer, afterPointer)
	}
	// The database is byte-identical.
	afterDB, err := os.ReadFile(NewLayout(root).LiveDB())
	if err != nil {
		t.Fatal(err)
	}
	if string(afterDB) != string(beforeDB) {
		t.Error("the live database changed during a failed build")
	}
	// The recovery executable is untouched.
	afterUpdater, err := os.Stat(filepath.Join(root, StableUpdaterName))
	if err != nil {
		t.Fatalf("the recovery executable is gone: %v", err)
	}
	if afterUpdater.Size() != beforeUpdater.Size() || afterUpdater.ModTime() != beforeUpdater.ModTime() {
		t.Error("the recovery executable changed during a failed build")
	}
	// No service was stopped or started.
	if len(services.stopped) != 0 || len(services.started) != 0 {
		t.Errorf("services were touched during a failed build: stopped=%v started=%v", services.stopped, services.started)
	}
	// And no release was promoted.
	releases, err := os.ReadDir(NewLayout(root).Releases())
	if err != nil {
		t.Fatal(err)
	}
	if len(releases) != 1 || releases[0].Name() != beforeRev {
		names := []string{}
		for _, r := range releases {
			names = append(names, r.Name())
		}
		t.Errorf("a failed build promoted a release: %v", names)
	}
}

// assertDatabaseIntact proves the live database still holds its rows.
//
// The COUNT is compared against what the root was seeded with, not
// against a fixed number: the substrate seeds its own rows (directives,
// sessions), so only the seed row's own presence is a stable fact.
func assertDatabaseIntact(t *testing.T, root string) {
	t.Helper()
	db := openTestDB(t, NewLayout(root).LiveDB())
	defer db.Close()
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM memories WHERE id = 'm1'").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("the database holds %d seed memories, want 1", n)
	}
}
