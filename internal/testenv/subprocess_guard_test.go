package testenv

// Regression guard for the 2026-09-14 production-pollution incident.
//
// A test that launches a subprocess which can open the MPM database must
// not leave that subprocess free to resolve production state. The
// existing in-process guard (internal/core/test_db_safety_test.go) walks
// the AST for NewDatabaseManager("") — but it inspects a function CALL in
// the test process, so it is structurally blind to cmd.Env on an
// exec.Command. This guard closes that vector.
//
// # Scope
//
// This deliberately does NOT reject every exec.Command. Most subprocess
// usage in this repo is harmless: `go build`, `sqlite3`, `sh`. The guard
// targets the dangerous form specifically — a test that starts a
// DB-capable MPM binary without pinning its workspace.
//
// # Identifying an MPM subprocess
//
// The hard part is telling a DB-capable MPM child apart from a harmless
// one, because suites almost never write exec.Command("mpm", …). The
// shape is:
//
//	bin := mcpCmd(t)                  // helper builds "…/mpm-mcp-test"
//	cmd := exec.Command(bin)
//	cmd.Env = append(os.Environ(), …)  // ← the defect
//
// so resolution runs in three tiers, strongest first:
//
//  1. Literals reachable from the command argument. This covers
//     exec.Command("mpm", …) and exec.Command(filepath.Join(d, "mpm-test"))
//     directly, and — through simple `x := "literal"` assignment — an
//     identifier like bin := "/tmp/fake-mpm-mcp". A tier-1 answer is
//     DECISIVE: exec.Command("go", "build", …) resolves to "go" and is
//     dismissed, so ordinary build invocations never become candidates.
//  2. When the argument is opaque (a parameter, or a helper result),
//     fall back to a scan of the whole test package for an MPM binary
//     name. The name usually lives in the helper file while the Env
//     assignment lives in a consumer file, so a per-file scan misses it.
//  3. Anything else is not a candidate.
//
// # What counts as isolated
//
// A cmd.Env assignment is accepted only if it demonstrably pins the
// workspace: an explicit MPM_WORKSPACE, a HOME redirect to a temporary
// directory, or delegation to a sanctioned helper. A bare os.Environ() is
// always a violation, even if other entries are appended.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// violation is one finding from the repository scanner.
type violation struct {
	file   string
	line   int
	detail string
}

// mpmBinaryNames are binaries that can open the MPM database when run as
// a subprocess.
var mpmBinaryNames = []string{"mpm", "mpm-mcp", "mpm-critic", "mpm-lint"}

// sanctionedEnvHelpers are the ways a test may legitimately supply an
// isolated environment. Each is assumed to pin MPM_WORKSPACE by
// construction.
//
// Adding a name here is a deliberate act: a new helper joins the
// isolated set only once its body has been read and confirmed to pin
// the workspace. testenv.Env is the sanctioned default; the rest are
// pre-existing package-local helpers that were audited individually.
var sanctionedEnvHelpers = []string{
	"testenv.Env(",                  // the sanctioned default
	"testenv.WithExtra(",            // the sanctioned default
	"testenv.NoWorkspace(",          // sanctioned unset-workspace variant
	"NoWorkspace(",                  // in-package shorthand
	"WithExtra(",                    // in-package shorthand
	"isolatedMPMEnv(",               // in-package shorthand
	"clearEmbeddingEnv(",            // cmd/mpm/release_pass_20260914_multiprofile_test.go
	"clearEmbeddingEnvForHermetic(", // cmd/mpm/release_pass_baseline_defect_b_test.go
	"clearEnv(",                     //
	"hermeticEnv(",                  //
	"baseEnv(",                      //
	"testEnv(",                      //
	"isolatedEnv(",                  //
	"t.TempDir()",                   // an inline temp workspace
}

// cmdBinding records one exec.Cmd variable and the command it was bound
// to.
type cmdBinding struct {
	// binary is the resolved MPM binary name, or "" when the argument
	// was opaque.
	binary string
	// resolved is true when the argument yielded a definite name (tier
	// 1), false when it was opaque and needs the package-level fallback.
	resolved bool
	// undeterminable is true when the command is launched through a
	// generic runner whose binary is a caller-supplied parameter. The
	// guard cannot attribute such a call, and guessing would mean
	// accusing every helper in the repo — internal/core's runCmd, whose
	// only caller runs sqlite3, is the canonical example. Staying quiet
	// is the same principle applied to *exec.Cmd values the scan never
	// saw bound: the rule is narrow on purpose.
	undeterminable bool
}

// TestSubprocessEnvPinsWorkspace is the headline regression: a real MPM
// child, started with an ordinary-looking environment, lands in
// temporary state and provably cannot reach production.
//
// The child is a binary built from this source tree, not an installed
// one. The canary is a decoy in a hostile HOME plus AssertNoProductionAccess;
// the live database is never named as a target.
func TestSubprocessEnvPinsWorkspace(t *testing.T) {
	// A deliberately hostile HOME: MPM_WORKSPACE is absent from the
	// caller's environment, and HOME contains a .mpm tree shaped exactly
	// like the operator's. config.GetMPMDir() falls back to $HOME/.mpm,
	// so a child that ignored the pinned workspace would open this.
	decoy := t.TempDir()
	decoyDB := filepath.Join(decoy, ".mpm", "src", "db", "mpm.db")
	if err := os.MkdirAll(filepath.Dir(decoyDB), 0o755); err != nil {
		t.Fatalf("build decoy: %v", err)
	}
	const decoyContent = "DECOY — must never be opened"
	if err := os.WriteFile(decoyDB, []byte(decoyContent), 0o600); err != nil {
		t.Fatalf("seed decoy: %v", err)
	}

	// Prove the premise: from this process, with no MPM_WORKSPACE, the
	// $HOME/.mpm fallback is exactly the path the decoy occupies. This is
	// what makes the decoy a meaningful canary rather than decoration.
	if got := os.Getenv("MPM_WORKSPACE"); got != "" {
		t.Skipf("MPM_WORKSPACE is set in the ambient environment (%q); "+
			"this regression needs the unset case to be meaningful", got)
	}

	bin := buildMPMBinary(t)

	// WithExtra is used deliberately: it layers the hostile HOME on top
	// of the helper's own environment to show the pinned MPM_WORKSPACE
	// wins, while HOME alone is not what saves us.
	env := WithExtra(t, "HOME="+decoy)

	// The child has no PATH-relative way to find an installed mpm; it is
	// the exact artifact built above.
	cmd := exec.Command(bin, "memory", "add", "isolation canary", "the child must land in temp state")
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("child mpm memory add failed: %v\n%s", err, out)
	}

	// The write landed where MPM_WORKSPACE pointed.
	dbPath := DBPath(t)
	AssertNoProductionAccess(t, dbPath)
	if _, err := os.Stat(dbPath); err != nil {
		t.Fatalf("child did not create a database at the pinned workspace %q: %v\n%s", dbPath, err, out)
	}

	// And the decoy is byte-for-byte untouched.
	got, err := os.ReadFile(decoyDB)
	if err != nil {
		t.Fatalf("read decoy: %v", err)
	}
	if string(got) != decoyContent {
		t.Fatalf("decoy was modified by the child: %q", got)
	}

	// The decoy check above is the weaker of the two, because a child
	// that opens a non-database file there may fail before writing
	// anything. The real 2026-09-14 signature is subtler and is what
	// this asserts: an unisolated child does not corrupt what it finds,
	// it SILENTLY CREATES a second, fully-initialised database wherever
	// the $HOME/.mpm fallback points. That is how a test run leaves a
	// ~1 MB src/db/mpm.db inside a package directory, invisible to git
	// status because src/db is ignored.
	//
	// Verified directly: with `append(os.Environ(), "HOME=…")` and no
	// MPM_WORKSPACE, `mpm memory add` exits 0, prints "✅ Memory added",
	// and the row lands in $HOME/.mpm/src/db/mpm.db — a database that did
	// not exist a moment earlier.
	var created []string
	if err := filepath.Walk(decoy, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() && filepath.Ext(p) == ".db" && p != decoyDB {
			created = append(created, p)
		}
		return nil
	}); err != nil {
		t.Fatalf("walk decoy: %v", err)
	}
	if len(created) > 0 {
		t.Fatalf("the child created a database inside $HOME/.mpm at %v — "+
			"it resolved the fallback instead of the pinned MPM_WORKSPACE", created)
	}
}

// TestEnvClearsOperatorCredentials pins the hermetic-property contract:
// no operator credential survives into a child.
func TestEnvClearsOperatorCredentials(t *testing.T) {
	t.Setenv("SOME_AMBIENT_OPERATOR_VAR", "leaked")
	env := Env(t)

	have := map[string]string{}
	for _, kv := range env {
		if i := strings.IndexByte(kv, '='); i > 0 {
			have[kv[:i]] = kv[i+1:]
		}
	}
	for _, k := range providerKeys {
		if v, ok := have[k]; !ok || v != "" {
			t.Errorf("credential %s not cleared: present=%v value=%q", k, ok, v)
		}
	}
	// The two variables that must be PINNED, not cleared.
	if have["MPM_WORKSPACE"] == "" {
		t.Error("MPM_WORKSPACE must be pinned to a temp workspace, not blank")
	}
	if have["HOME"] == "" {
		t.Error("HOME must be redirected, not blank")
	}
	// Inherited ambient environment must not leak in.
	for _, kv := range env {
		if strings.HasPrefix(kv, "SOME_AMBIENT_OPERATOR_VAR=") {
			t.Error("Env inherited an ambient variable; it must be hermetic")
		}
	}
}

// TestNoUnisolatedMPMSubprocess is the repository-wide scanner. See the
// file header for scope and rationale.
func TestNoUnisolatedMPMSubprocess(t *testing.T) {
	root := findRepoRoot(t)

	// This file is excluded from its own scan, and must be: it contains
	// both the known-bad fixtures and a real MPM invocation of its own
	// (TestSubprocessEnvPinsWorkspace), so it necessarily matches the
	// pattern it is checking for. Everything it asserts about those forms
	// is pinned by TestScannerDetectsKnownBadForm above.
	const selfPath = "internal/testenv/subprocess_guard_test.go"

	var violations []violation

	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "vendor", "bin", "docs", "drills":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		if rel == selfPath {
			return nil
		}
		vs, err := scanForUnisolatedSubprocess(path, rel)
		if err != nil {
			return err
		}
		violations = append(violations, vs...)
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}

	for _, v := range violations {
		t.Errorf("unisolated MPM subprocess: %s:%d — %s\n"+
			"    set cmd.Env = testenv.WithExtra(t) (or pin MPM_WORKSPACE/HOME explicitly)",
			v.file, v.line, v.detail)
	}
}

// TestScannerDetectsKnownBadForm is the guard's own credibility check.
//
// A scanner that passes the repository on its first run proves nothing —
// it may simply be blind to the defect it was written for. These
// fixtures encode the exact shapes that caused the 2026-09-14 incident
// and the residual cmd/mpm-mcp hazard, and each MUST be flagged. The
// negative fixtures pin the rule's narrowness at the same time: an
// ordinary `go build` must not be flagged, or the guard would be
// rejected by every suite in the repo and switched off.
func TestScannerDetectsKnownBadForm(t *testing.T) {
	bad := []struct {
		name string
		src  string
	}{
		{
			// The literal form.
			name: "literal mpm binary with os.Environ",
			src: `package t
import ("os"; "os/exec"; "testing")
func TestX(t *testing.T) {
	cmd := exec.Command("mpm", "memory", "add", "x")
	cmd.Env = append(os.Environ(), "MPM_VERBOSE=1")
	_ = cmd.Run()
}`,
		},
		{
			// The shape in cmd/mpm-mcp: the binary is a variable holding
			// a helper result, and the name lives elsewhere entirely.
			name: "opaque binary with os.Environ",
			src: `package t
import ("os"; "os/exec"; "testing")
func TestX(t *testing.T) {
	bin := helper(t)
	cmd := exec.Command(bin)
	cmd.Env = os.Environ()
	_ = cmd.Run()
}
func helper(t *testing.T) string { return "/tmp/mpm-mcp-test" }`,
		},
		{
			// A suffixed temp-dir binary resolved through a local assign.
			name: "suffixed binary name via ident",
			src: `package t
import ("os"; "os/exec"; "testing")
func TestX(t *testing.T) {
	bin := "/tmp/fake-mpm-mcp"
	cmd := exec.Command(bin)
	cmd.Env = os.Environ()
	_ = cmd.Run()
}`,
		},
		{
			// No .Env at all: the child inherits the ambient environment
			// implicitly, which is the same defect in a quieter form.
			name: "no Env assignment at all",
			src: `package t
import ("os/exec"; "testing")
func TestX(t *testing.T) {
	cmd := exec.Command("/opt/bin/mpm-test", "status")
	_ = cmd.Run()
}`,
		},
		{
			// The converse of the scoping regression: a sibling `go build`
			// must not exonerate a real MPM violation in the same file.
			name: "sibling go build does not mask an MPM violation",
			src: `package t
import ("os"; "os/exec"; "testing")
func build(t *testing.T) {
	cmd := exec.Command("go", "build", ".")
	cmd.Env = WithExtra(t)
	_ = cmd.Run()
}
func TestX(t *testing.T) {
	cmd := exec.Command("mpm", "status")
	cmd.Env = os.Environ()
	_ = cmd.Run()
}`,
		},
		{
			// The same wrapper-struct shape, unisolated. Following the
			// field must not turn into blanket exoneration.
			name: "wrapper struct with an inherited env field",
			src: `package t
import ("os"; "os/exec"; "testing")
type execCmd struct { path string; env []string }
func mk(path string) *execCmd {
	return &execCmd{path: path, env: os.Environ()}
}
func (c *execCmd) run() {
	cmd := exec.Command(c.path)
	cmd.Env = c.env
	_ = cmd.Run()
}`,
		},
		{
			// A non-default local alias for os/exec. Found as a live
			// bypass: matching a fixed identifier list made the guard
			// blind to every alias the author did not anticipate.
			name: "aliased os/exec import",
			src: `package t
import subprocess "os/exec"
import "testing"
func TestX(t *testing.T) {
	cmd := subprocess.Command("mpm", "info")
	_ = cmd.Run()
}`,
		},
		{
			// CommandContext is the same hazard with a deadline attached.
			// Treating only Command as a candidate would let any suite opt
			// out of the guard by adding a timeout — a change suites want
			// anyway, which is precisely why it must not be an escape.
			name: "CommandContext is a candidate",
			src: `package t
import ("context"; "os/exec"; "testing")
func TestX(t *testing.T, ctx context.Context) {
	cmd := exec.CommandContext(ctx, "mpm", "info")
	_ = cmd.Run()
}`,
		},
		{
			// Aliased package AND the context form together.
			name: "aliased CommandContext",
			src: `package t
import x "os/exec"
import ("context"; "testing")
func TestX(t *testing.T, ctx context.Context) {
	cmd := x.CommandContext(ctx, "mpm", "info")
	_ = cmd.Run()
}`,
		},
	}

	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			got := scanSource(t, tc.src, "fixture_test.go")
			if len(got) == 0 {
				t.Fatalf("scanner missed the known-bad form:\n%s", tc.src)
			}
		})
	}

	good := []struct {
		name string
		src  string
	}{
		{
			// A build invocation is not an MPM child.
			name: "go build is not a candidate",
			src: `package t
import ("os"; "os/exec"; "testing")
func TestX(t *testing.T) {
	cmd := exec.Command("go", "build", "-o", "bin", ".")
	cmd.Env = os.Environ()
	_ = cmd.Run()
}`,
		},
		{
			// The context form must honour a pinned environment, or fixing
			// detection would just relocate the false positive.
			name: "CommandContext with pinned MPM_WORKSPACE",
			src: `package t
import ("context"; "os/exec"; "testing")
func TestX(t *testing.T, ctx context.Context) {
	cmd := exec.CommandContext(ctx, "mpm", "info")
	cmd.Env = []string{"MPM_WORKSPACE=" + t.TempDir(), "HOME=" + t.TempDir()}
	_ = cmd.Run()
}`,
		},
		{
			// A non-exec package's Command must not be swept up. Without
			// the import check, an aliased-import lookup that cannot see
			// the import table would flag every `x.Command(...)` in the
			// repository.
			name: "non-exec package selector is not a candidate",
			src: `package t
import "testing"
type fakePkg struct{}
func (fakePkg) Command(name string, args ...string) *struct{} { return nil }
func TestX(t *testing.T) {
	var p fakePkg
	_ = p.Command("mpm", "info")
}`,
		},
		{
			// The sanctioned form.
			name: "pinned MPM_WORKSPACE",
			src: `package t
import ("os"; "os/exec"; "testing")
func TestX(t *testing.T) {
	ws := t.TempDir()
	cmd := exec.Command("mpm", "memory", "add", "x")
	cmd.Env = append(os.Environ(), "MPM_WORKSPACE="+ws)
	_ = cmd.Run()
}`,
		},
		{
			// Delegation to the sanctioned helper.
			name: "testenv.WithExtra",
			src: `package t
import ("os/exec"; "testing")
func TestX(t *testing.T) {
	cmd := exec.Command("mpm", "status")
	cmd.Env = WithExtra(t, "MPM_VERBOSE=1")
	_ = cmd.Run()
}`,
		},
		{
			// A HOME redirect closes the ~/.mpm fallback.
			name: "HOME redirect to a temp dir",
			src: `package t
import ("os"; "os/exec"; "testing")
func TestX(t *testing.T) {
	cmd := exec.Command("mpm", "status")
	cmd.Env = append(os.Environ(), "HOME="+t.TempDir())
	_ = cmd.Run()
}`,
		},
		{
			// The dominant real-world isolated form in this repo: a
			// composite literal rather than an append. A scanner that
			// summarises the AST instead of reading the source renders
			// this as "…" and would flag every one of these.
			name: "composite literal pinning MPM_WORKSPACE",
			src: `package t
import ("os/exec"; "testing")
func TestX(t *testing.T) {
	ws := t.TempDir()
	cmd := exec.Command("mpm", "memory", "add", "x")
	cmd.Env = []string{
		"MPM_WORKSPACE=" + ws,
		"PATH=/usr/bin:/bin",
		"HOME=" + t.TempDir(),
	}
	_ = cmd.Run()
}`,
		},
		{
			// Same, but isolating via HOME alone.
			name: "composite literal pinning HOME only",
			src: `package t
import ("os/exec"; "testing")
func TestX(t *testing.T) {
	cmd := exec.Command("mpm", "status")
	cmd.Env = []string{
		"PATH=" + "/usr/bin",
		"HOME=" + t.TempDir(),
		"MPM_SCHEDULER_DISABLED=1",
	}
	_ = cmd.Run()
}`,
		},
		{
			// A wrapper struct carrying the child's environment, with the
			// value supplied by a constructor elsewhere in the file —
			// cmd/mpm/release_pass_20260914_dashboard_test.go's shape.
			name: "wrapper struct with an isolated env field",
			src: `package t
import ("os/exec"; "testing")
type execCmd struct { path string; env []string }
func mk(t *testing.T, path string) *execCmd {
	return &execCmd{path: path, env: WithExtra(t)}
}
func (c *execCmd) run() {
	cmd := exec.Command(c.path)
	cmd.Env = c.env
	_ = cmd.Run()
}`,
		},
		{
			// A generic runner whose binary is a caller-supplied
			// parameter. The guard cannot attribute it, so it must not
			// accuse — internal/core's runCmd, whose only caller runs
			// sqlite3, is the real instance of this.
			name: "generic runner with a parameterised binary",
			src: `package t
import ("os/exec"; "testing")
func runCmd(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}
func helper(t *testing.T) string { return "/tmp/mpm-test" }`,
		},
		{
			// The scoping regression: a `go build` in one function and an
			// MPM child in another, both using the variable name `cmd`.
			// The build's inherited environment is harmless; only the MPM
			// child's env is judged, and it is isolated here. A binding map
			// keyed by bare variable name would either flag the build or
			// exonerate the MPM child.
			name: "go build in a sibling function is not a candidate",
			src: `package t
import ("os"; "os/exec"; "testing")
func build(t *testing.T) {
	cmd := exec.Command("go", "build", ".")
	cmd.Env = os.Environ()
	_ = cmd.Run()
}
func TestX(t *testing.T) {
	cmd := exec.Command("mpm", "status")
	cmd.Env = WithExtra(t)
	_ = cmd.Run()
}`,
		},
	}

	for _, tc := range good {
		t.Run(tc.name, func(t *testing.T) {
			got := scanSource(t, tc.src, "fixture_test.go")
			if len(got) != 0 {
				t.Fatalf("scanner flagged a safe form: %v\n%s", got[0].detail, tc.src)
			}
		})
	}
}

// scanForUnisolatedSubprocess scans one test file, resolving opaque
// command arguments against the whole test package.
func scanForUnisolatedSubprocess(path, rel string) ([]violation, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, src, 0)
	if err != nil {
		return nil, err
	}
	pkgMentionsMPM, err := packageMentionsMPM(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	return scanFile(fset, f, src, rel, pkgMentionsMPM), nil
}

// scanSource is the fixture entry point: parse source from memory and
// scan it with the package-level fallback forced on, so a fixture that
// hides the binary name in a helper is still resolvable.
func scanSource(t *testing.T, src, name string) []violation {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, name, src, 0)
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	return scanFile(fset, f, []byte(src), name, true)
}

// funcSpan is the source range of one function, used to scope variable
// bindings.
type funcSpan struct {
	name       string
	start, end token.Pos
}

// enclosingFunc returns the name of the innermost function containing p.
// Scoping matters: cmd/mpm/call_io_test.go builds a binary with
// `cmd := exec.Command("go", "build", …)` in one function and invokes it
// with `cmd := exec.Command(mpmBin, …)` in another. Keying bindings by
// bare variable name across the file charges the first function's build
// invocation to the second function's MPM binary.
func enclosingFunc(spans []funcSpan, p token.Pos) string {
	best := ""
	var bestWidth token.Pos
	for _, s := range spans {
		if p < s.start || p >= s.end {
			continue
		}
		if w := s.end - s.start; best == "" || w < bestWidth {
			best, bestWidth = s.name, w
		}
	}
	return best
}

func funcSpans(f *ast.File) []funcSpan {
	var out []funcSpan
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok {
			continue
		}
		name := fd.Name.Name
		if fd.Recv != nil {
			name = "(method)." + name
		}
		out = append(out, funcSpan{name: name, start: fd.Pos(), end: fd.End()})
	}
	return out
}

// bindingKey identifies one command variable within one function.
type bindingKey struct {
	fn, varName string
}

func scanFile(fset *token.FileSet, f *ast.File, src []byte, rel string, pkgMentionsMPM bool) []violation {
	// Tier 1 prerequisite: map every local `x := <string expr>` so an
	// identifier passed to exec.Command can be resolved to its literal.
	idents := localStringBindings(f)
	// The same bindings, as source text, so `cmd.Env = env` can be judged
	// by what `env` was built from.
	exprSrcs := localExprSources(f, fset, src)
	fieldSrcs := structFieldSources(f, fset, src)
	spans := funcSpans(f)
	pins := pinningFuncs(f, spans)
	params := paramNames(f)

	type binding struct {
		cmd     cmdBinding
		pos     token.Pos
		envSeen bool
	}
	cmds := map[bindingKey]*binding{}

	// Pass 1: bind every exec.Cmd variable, and every .Env assignment, in
	// one walk so the two can be joined by position and scope.
	ast.Inspect(f, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok {
			return true
		}
		fn := enclosingFunc(spans, as.Pos())

		// cmd.Env = … — attach to the binding for this function.
		for _, lhs := range as.Lhs {
			sel, ok := lhs.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Env" {
				continue
			}
			id, ok := sel.X.(*ast.Ident)
			if !ok {
				continue
			}
			if b := cmds[bindingKey{fn, id.Name}]; b != nil {
				b.envSeen = true
			}
			continue
		}

		if len(as.Lhs) != 1 || len(as.Rhs) != 1 {
			return true
		}
		lhs, ok := as.Lhs[0].(*ast.Ident)
		if !ok {
			return true
		}
		call, ok := as.Rhs[0].(*ast.CallExpr)
		if !ok || !isExecCommand(call, f) || len(call.Args) == 0 {
			return true
		}
		// CommandContext's first argument is the context, not the binary,
		// so the binary lives one slot later. Reading the wrong index
		// would resolve a context.Context value as a path and silently
		// classify the call as unresolvable — i.e. as "not an MPM child",
		// which is exactly the bypass this is closing.
		binaryIdx := 0
		if sel, isSel := call.Fun.(*ast.SelectorExpr); isSel && sel.Sel.Name == "CommandContext" {
			binaryIdx = 1
		}
		if binaryIdx >= len(call.Args) {
			return true
		}
		cmds[bindingKey{fn, lhs.Name}] = &binding{
			cmd: resolveCommand(call.Args[binaryIdx], idents, params),
			pos: as.Pos(),
		}
		return true
	})

	// Pass 2: judge each MPM command's environment.
	var out []violation
	for key, b := range cmds {
		if !runsMPM(b.cmd, pkgMentionsMPM) {
			continue
		}
		if b.envSeen {
			continue // the .Env assignment was already judged at pass 2b
		}
		if pins[key.fn] {
			// The function pinned MPM_WORKSPACE (or HOME) into the
			// process environment with t.Setenv, so a child that inherits
			// that environment is already isolated.
			continue
		}
		out = append(out, violation{rel, fset.Position(b.pos).Line,
			"runs an MPM binary as " + key.varName + " but never assigns cmd.Env " +
				"(the child inherits the ambient environment)"})
	}

	ast.Inspect(f, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || len(as.Rhs) == 0 {
			return true
		}
		fn := enclosingFunc(spans, as.Pos())
		for _, lhs := range as.Lhs {
			sel, ok := lhs.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Env" {
				continue
			}
			id, ok := sel.X.(*ast.Ident)
			if !ok {
				continue
			}
			b := cmds[bindingKey{fn, id.Name}]
			if b == nil {
				// An *exec.Cmd we never saw bound in this function (a
				// parameter, or a helper's return value). Not enough
				// signal to accuse; staying quiet here is what keeps the
				// rule narrow.
				continue
			}
			if !runsMPM(b.cmd, pkgMentionsMPM) {
				continue
			}
			// The real source text, not a reconstructed approximation: a
			// composite literal like
			//   cmd.Env = []string{"MPM_WORKSPACE=" + ws, "HOME=" + t.TempDir()}
			// is the single most common isolated form in this repo, and an
			// AST summary that renders it as "…" would flag every one.
			//
			// An RHS that is a bare identifier is followed to its binding,
			// so `env := append(os.Environ(), "MPM_WORKSPACE="+ws)` is
			// recognised through `cmd.Env = env`. A wrapper struct's env
			// field (`cmd.Env = c.env`) is followed to whichever
			// composite literal supplied it.
			text := sourceText(fset, src, as.Rhs[0])
			if id, ok := as.Rhs[0].(*ast.Ident); ok {
				if bound := boundExprText(exprSrcs, id.Name); bound != "" {
					text = bound
				}
			}
			if sel, ok := as.Rhs[0].(*ast.SelectorExpr); ok {
				if bound := fieldSrcs[sel.Sel.Name]; bound != "" {
					text = bound
				}
			}
			if isIsolatedEnvExpr(text, pins[fn], exprSrcs) {
				continue
			}
			out = append(out, violation{rel, fset.Position(as.Pos()).Line,
				"cmd.Env = " + truncate(text, 60) + " does not pin MPM_WORKSPACE or HOME"})
		}
		return true
	})
	return out
}

// sourceText returns the verbatim source text of a node by slicing the
// original bytes with the file set's byte offsets.
func sourceText(fset *token.FileSet, src []byte, n ast.Node) string {
	start := fset.Position(n.Pos())
	end := fset.Position(n.End())
	if start.Filename != end.Filename ||
		start.Offset < 0 || end.Offset > len(src) || start.Offset > end.Offset {
		return "…"
	}
	return string(src[start.Offset:end.Offset])
}

// resolveCommand identifies the binary an exec.Command launches.
//
// A definite name from tier 1 makes `resolved` true — including a
// definite NON-MPM name, which is what lets `go build` be dismissed
// rather than escalated to the package fallback.
func resolveCommand(arg ast.Expr, idents map[string][]string, params map[string]bool) cmdBinding {
	// A plain string literal is a definitive answer in both directions.
	// Getting this wrong is how exec.Command("go", "build", …) ends up
	// escalated to the package-wide fallback and reported as an MPM child.
	if lit, ok := arg.(*ast.BasicLit); ok && lit.Kind == token.STRING {
		s, err := strconv.Unquote(lit.Value)
		if err != nil {
			return cmdBinding{resolved: false}
		}
		return cmdBinding{binary: matchMPMBinary(filepath.Base(s)), resolved: true}
	}
	// A compound expression (filepath.Join(dir, "mpm-test")) contributes
	// literals. A match is decisive; a miss is not, because the name may
	// be assembled at runtime.
	for _, s := range stringLiteralsIn(arg) {
		if m := matchMPMBinary(filepath.Base(s)); m != "" {
			return cmdBinding{binary: m, resolved: true}
		}
	}
	// A bare identifier, followed through its binding.
	if id, ok := arg.(*ast.Ident); ok {
		if lits, bound := idents[id.Name]; bound && len(lits) > 0 {
			for _, s := range lits {
				if m := matchMPMBinary(filepath.Base(s)); m != "" {
					return cmdBinding{binary: m, resolved: true}
				}
			}
			// Bound to literals, none of them an MPM binary.
			return cmdBinding{resolved: true}
		}
		// A parameter: the binary is decided by the caller, which this
		// file's scan cannot see.
		if params[id.Name] {
			return cmdBinding{undeterminable: true}
		}
	}
	return cmdBinding{resolved: false}
}

// runsMPM applies the tier-2 fallback: a resolved name is decisive in
// both directions, and an opaque argument defers to the package scan.
func runsMPM(b cmdBinding, pkgMentionsMPM bool) bool {
	if b.undeterminable {
		return false
	}
	if b.resolved {
		return b.binary != ""
	}
	return pkgMentionsMPM
}

// paramNames collects every function parameter name in the file. Used
// only to recognise a generic runner (see cmdBinding.undeterminable).
func paramNames(f *ast.File) map[string]bool {
	out := map[string]bool{}
	record := func(fields *ast.FieldList) {
		if fields == nil {
			return
		}
		for _, fld := range fields.List {
			for _, name := range fld.Names {
				out[name.Name] = true
			}
		}
	}
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok {
			record(fd.Type.Params)
		}
	}
	return out
}

// localExprSources maps `x := <expr>` to the expr's verbatim source text.
// Later bindings overwrite earlier ones, which over-approximates across
// functions in the same direction localStringBindings does.
func localExprSources(f *ast.File, fset *token.FileSet, src []byte) map[string]string {
	out := map[string]string{}
	ast.Inspect(f, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || len(as.Lhs) != 1 || len(as.Rhs) != 1 {
			return true
		}
		if id, ok := as.Lhs[0].(*ast.Ident); ok {
			if txt := sourceText(fset, src, as.Rhs[0]); txt != "" {
				out[id.Name] = txt
			}
		}
		return true
	})
	return out
}

// localStringBindings maps `x := <expr containing string literals>` to
// those literals, so exec.Command(bin) can be resolved to bin's value.
//
// The map is whole-file, not per-scope, so a name reused in two
// functions merges its bindings. That over-approximates, and it
// over-approximates in the safe direction for a binary-name question: the
// only way it produces a miss is if a name is bound to an MPM binary in
// one function and to something else in another.
func localStringBindings(f *ast.File) map[string][]string {
	out := map[string][]string{}
	add := func(name string, e ast.Expr) {
		if lits := stringLiteralsIn(e); len(lits) > 0 {
			out[name] = append(out[name], lits...)
		}
	}
	ast.Inspect(f, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.AssignStmt:
			if len(v.Lhs) == 1 && len(v.Rhs) == 1 {
				if id, ok := v.Lhs[0].(*ast.Ident); ok {
					add(id.Name, v.Rhs[0])
				}
			}
		case *ast.ValueSpec:
			for i, id := range v.Names {
				if i < len(v.Values) {
					add(id.Name, v.Values[i])
				}
			}
		}
		return true
	})
	return out
}

// stringLiteralsIn returns every string literal inside an expression.
// filepath.Join(dir, "mpm-test") yields "mpm-test", which is exactly the
// resolution this needs.
func stringLiteralsIn(e ast.Expr) []string {
	var out []string
	ast.Inspect(e, func(n ast.Node) bool {
		if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING {
			if s, err := strconv.Unquote(lit.Value); err == nil {
				out = append(out, s)
			}
		}
		return true
	})
	return out
}

// packageMentionsMPM reports whether any test file in dir names an MPM
// binary. The name usually lives in a shared helper file while the
// defect lives in a consumer file, so this scan must cross files.
func packageMentionsMPM(dir string) (bool, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false, err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, filepath.Join(dir, e.Name()), nil, 0)
		if err != nil {
			continue
		}
		found := false
		ast.Inspect(f, func(n ast.Node) bool {
			if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING {
				if s, err := strconv.Unquote(lit.Value); err == nil {
					if matchMPMBinary(filepath.Base(s)) != "" {
						found = true
					}
				}
			}
			return true
		})
		if found {
			return true, nil
		}
	}
	return false, nil
}

// structFieldSources maps a struct field name to the source text it was
// given in a composite literal anywhere in the file, for the fields
// that carry a child process's environment.
//
// This exists for the wrapper-struct pattern that
// cmd/mpm/release_pass_20260914_dashboard_test.go uses: an `execCmd`
// holding `env []string`, with `cmd.Env = c.env` in the run method and
// the actual value supplied by `&execCmd{env: testenv.Env(t)}` in a
// constructor three functions away. Following the field is a bounded hop
// — the alternative is either a false positive on every wrapper-struct
// test or rewriting working test infrastructure to suit a scanner.
//
// Keyed by field name alone, not by type, which over-approximates when a
// file has several structs with an `env` field. That errs toward
// reporting, and a report here costs a one-line fix.
func structFieldSources(f *ast.File, fset *token.FileSet, src []byte) map[string]string {
	out := map[string]string{}
	ast.Inspect(f, func(n ast.Node) bool {
		cl, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		for _, elt := range cl.Elts {
			kv, ok := elt.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			key, ok := kv.Key.(*ast.Ident)
			if !ok || (key.Name != "env" && key.Name != "Env") {
				continue
			}
			if txt := sourceText(fset, src, kv.Value); txt != "" {
				out[key.Name] = txt
			}
		}
		return true
	})
	return out
}

// pinningFuncs returns the set of functions that pin MPM_WORKSPACE or
// HOME into the process environment with t.Setenv / os.Setenv.
//
// Such a function's children are already isolated even with no cmd.Env
// of their own, and even with a bare os.Environ() — which is why
// cmd/mpm/help_regression_test.go's `t.Setenv("MPM_WORKSPACE", ws)` is
// a legitimate isolation mechanism rather than a gap. The set is
// function-scoped because t.Setenv restores on test teardown; a pin in
// one test says nothing about a sibling.
func pinningFuncs(f *ast.File, spans []funcSpan) map[string]bool {
	out := map[string]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		name := callTextOf(call)
		if name != "Setenv" && name != "t.Setenv" && name != "os.Setenv" {
			return true
		}
		key := stringLiteralsIn(call.Args[0])
		if len(key) == 0 {
			return true
		}
		switch key[0] {
		case "MPM_WORKSPACE", "HOME":
			out[enclosingFunc(spans, call.Pos())] = true
		}
		return true
	})
	return out
}

// callTextOf renders just the callee of a call, e.g. "t.Setenv".
func callTextOf(c *ast.CallExpr) string {
	switch v := c.Fun.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.SelectorExpr:
		if x, ok := v.X.(*ast.Ident); ok {
			return x.Name + "." + v.Sel.Name
		}
	}
	return ""
}

// boundExprText returns the source text of the expression a name was
// bound to, or "" if it was never bound in this file.
//
// This is what lets `env := append(os.Environ(), "MPM_WORKSPACE="+ws)`
// followed by `cmd.Env = env` be recognised: the assignment being judged
// is the identifier `env`, and only its binding says anything.
func boundExprText(srcs map[string]string, name string) string {
	return srcs[name]
}

// demonstrably pins the workspace.
//
// processPins is true when the enclosing function already exported a pin
// with t.Setenv, in which case inheriting the environment is isolated.
func isIsolatedEnvExpr(text string, processPins bool, srcs map[string]string) bool {
	t := strings.TrimSpace(text)
	// An explicit workspace pin is decisive. Go's os/exec dedups Env with
	// later entries winning, so append(os.Environ(), "MPM_WORKSPACE="+ws)
	// really does override an ambient MPM_WORKSPACE rather than racing it.
	if strings.Contains(t, "MPM_WORKSPACE") {
		return true
	}
	// A HOME redirect is the other accepted form: it closes the
	// $HOME/.mpm fallback. It only isolates if it points somewhere
	// temporary, so the value is followed — written inline
	// ("HOME=" + t.TempDir()) or through a variable
	// (home := t.TempDir(); … "HOME=" + home), which is how
	// cmd/mpm/scheduler_health_alpha_4_1_machine_mode_test.go writes it.
	if strings.Contains(t, "HOME=") {
		if strings.Contains(t, "TempDir") || strings.Contains(t, "os.TempDir") {
			return true
		}
		for name, src := range srcs {
			if strings.Contains(src, "TempDir") && mentionsName(t, name) {
				return true
			}
		}
	}
	// Delegation to a sanctioned helper.
	for _, name := range sanctionedEnvHelpers {
		if strings.Contains(t, name) {
			return true
		}
	}
	// Inheriting an environment that the enclosing function already
	// pinned is isolated.
	if processPins {
		return true
	}
	// Everything else is not isolation. A bare os.Environ() lands here:
	// it inherits whatever the developer's shell happened to export, which
	// is the defect this guard exists for.
	return false
}

// mentionsName reports whether text refers to name as a whole word, so
// `home` does not match inside `homedir`.
func mentionsName(text, name string) bool {
	for i := 0; i+len(name) <= len(text); i++ {
		if text[i:i+len(name)] != name {
			continue
		}
		if i > 0 && isIdentByte(text[i-1]) {
			continue
		}
		if i+len(name) < len(text) && isIdentByte(text[i+len(name)]) {
			continue
		}
		return true
	}
	return false
}

func isIdentByte(c byte) bool {
	return c == '_' || c == '.' ||
		(c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

// isExecCommand reports whether a call is an os/exec command
// constructor, in either its context-free or context-aware form.
//
// Three things are accepted, and the first two exist because they were
// found as real bypasses during the audit rather than hypothesized:
//
//   - Any local name for the package, not a hardcoded list. Go lets a
//     file write `x "os/exec"` or `osexec "os/exec"`, and a scanner that
//     matches a fixed set of identifiers is blind to every alias the
//     author did not think of. The import table is authoritative, so the
//     package's local name is read from it.
//   - CommandContext as well as Command. A deadline-wrapped MPM child is
//     still an MPM child; treating only Command as a candidate would let
//     a suite opt out of the guard by adding a timeout, which is a
//     change every suite wants anyway.
func isExecCommand(call *ast.CallExpr, file *ast.File) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	if sel.Sel.Name != "Command" && sel.Sel.Name != "CommandContext" {
		return false
	}
	return isExecPackageRef(sel.X, file)
}

// execPackageLocalNames returns the local identifiers under which a file
// imports os/exec. The empty string means "not imported", in which case a
// selector on that name is some other type and must not be treated as an
// exec call.
func execPackageLocalNames(f *ast.File) map[string]bool {
	out := map[string]bool{}
	for _, imp := range f.Imports {
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil || path != "os/exec" {
			continue
		}
		if imp.Name != nil {
			out[imp.Name.Name] = true
		} else {
			// Un-aliased import binds the package name itself.
			out["exec"] = true
		}
	}
	return out
}

// isExecPackageRef reports whether expr refers to the imported os/exec
// package, honouring any local alias.
//
// The fallbacks cover the case where the import table is unavailable
// (a fragment fed to the scanner without its imports). They are additive
// rather than a substitute: an unaliased `exec` selector is overwhelmingly
// the exec package, and erring toward detection is the correct bias for a
// guard whose failure mode is a missed production write rather than a
// noisy review comment.
func isExecPackageRef(expr ast.Expr, file *ast.File) bool {
	id, ok := expr.(*ast.Ident)
	if !ok {
		return false
	}
	if file != nil {
		return execPackageLocalNames(file)[id.Name]
	}
	return id.Name == "exec" || id.Name == "stdlibexec" || id.Name == "osexec"
}

// matchMPMBinary reports whether a binary basename is an MPM binary.
//
// Exact equality is not enough. Suites build their artifact under a
// suffixed name so the compiled binary never shadows an installed mpm on
// PATH — "mpm-test", "mpm-mcp-test", "mpm-isolation-canary" — and a
// fixture may prefix further. Matching therefore splits the basename on
// separators and accepts it if any component IS a known MPM binary.
//
// Component matching (not substring) is what keeps this from running
// away: "mpmetrics" and "mpmer" are single components that match
// nothing, while "mpm-mcp-test" contains "mpm" and "mpm-mcp" contains
// both "mpm" and "mcp".
func matchMPMBinary(base string) string {
	for _, part := range strings.FieldsFunc(base, func(r rune) bool {
		return r == '-' || r == '_' || r == '.' || r == '+'
	}) {
		for _, n := range mpmBinaryNames {
			if part == n {
				return n
			}
		}
	}
	return ""
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// buildMPMBinary compiles cmd/mpm from the current source tree into a
// per-test temporary directory and returns its path.
//
// The regression must exercise the source under test, not whatever `mpm`
// happens to be installed on the operator's PATH — that is the same
// mistake the pre-2026-08-14 mcp fixtures made, when they hardcoded one
// author's checkout path. -tags fts5 is required: MPM's schema does not
// come up without it.
func buildMPMBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "mpm-isolation-canary")
	cmd := exec.Command("go", "build", "-tags", "fts5", "-o", bin, "./cmd/mpm")
	cmd.Dir = findRepoRoot(t)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build cmd/mpm: %v\n%s", err, out)
	}
	return bin
}

func findRepoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			if _, err := os.Stat(filepath.Join(dir, "go.sum")); err == nil {
				return dir
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("repo root not found from cwd")
		}
		dir = parent
	}
}
