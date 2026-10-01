// Package testenv provides the single sanctioned way for MPM's test
// suites to launch a subprocess that can open the MPM database.
//
// # Why this exists
//
// MPM binaries resolve their database through two independent paths, and
// BOTH can reach production state:
//
//  1. config.GetMPMDir() (internal/core/config/config.go) — reads
//     MPM_WORKSPACE, then falls back to $HOME/.mpm.
//  2. mpmcli.ResolveWorkspace() (internal/core/mpmcli) — reads
//     MPM_WORKSPACE, then falls back to "." (the process CWD).
//
// On a developer machine $HOME/.mpm is frequently a symlink to the
// repository checkout, so the $HOME/.mpm fallback and the repository's
// own src/db/mpm.db are frequently the SAME file. A child process that
// does not set MPM_WORKSPACE therefore has no safe landing: it either
// opens production outright (via $HOME/.mpm) or silently creates a
// second database inside whatever directory the test happened to run in.
// Both have happened — internal/testenv's own doc comment records both
// incidents, and the second one left a 995 KB cmd/mpm-mcp/src/db/mpm.db
// behind that no git status would ever show.
//
// # The contract
//
// Use Env(t) for every subprocess that can open the MPM database:
//
//	cmd := exec.Command(bin, "memory", "add", "…")
//	cmd.Env = testenv.Env(t)
//	out, err := cmd.CombinedOutput()
//
// Env pins MPM_WORKSPACE to a per-test temporary directory, so neither
// the $HOME/.mpm fallback nor the CWD fallback can be reached. It also
// points HOME at a temporary directory as defence in depth, so a child
// that ignores MPM_WORKSPACE entirely still cannot resolve the
// operator's real ~/.mpm.
//
// NoWorkspace(t) is the one sanctioned exception, for tests whose
// subject IS the unset-workspace path.
//
// # Why not just os.Environ()?
//
// Inheriting the ambient environment is the single most common way a
// test reaches production: the developer's own MPM_WORKSPACE, or the
// absence of one, silently determines which database the child opens.
// A test must not depend on the shell it happens to run under. Env
// returns a fixed, explicit environment instead.
//
// # Provider credentials
//
// Env clears every provider credential MPM knows about, so a test can
// never make a paid network call by inheriting the operator's keys.
// A test that genuinely needs a fake provider should pass
// WithExtra("SOME_KEY=…") explicitly and point it at an httptest
// server; nothing here reads a credential from the ambient
// environment.
package testenv

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// providerKeys are every provider credential/env var MPM consults
// (mirrors the surface_exhausted list in internal/core/synth). They are
// emitted with an empty value so a child cannot authenticate even if
// some code path prefers the env var over the config file.
var providerKeys = []string{
	"OPENAI_API_KEY", "OPENAI_BASE_URL", "OPENAI_MODEL",
	"ANTHROPIC_API_KEY",
	"MINIMAX_API_KEY",
	"GOOGLE_API_KEY", "GEMINI_API_KEY",
	"XAI_API_KEY",
	"MISTRAL_API_KEY",
	"COHERE_API_KEY",
	"OPENROUTER_API_KEY", "OPENROUTER_BASE_URL",
	"OAI_COMPAT_API_KEY", "OAI_COMPAT_BASE_URL", "OAI_COMPAT_MODEL",
	"OLLAMA_ENDPOINT", "OLLAMA_MODEL",
}

// isolatedKeys are MPM's own knobs that would let a child bind to
// operator state or escape the temp workspace.
var isolatedKeys = []string{
	"MPM_DB_PATH",
	"MPM_SHARED_DB",
	"MPM_ACTIVE_MODE",
	"MPM_ACTIVE_PERSONA",
	"MPM_SESSION_ID",
}

// testPath is the PATH handed to children. It must contain the Go
// toolchain because several suites shell out to `go build` from inside
// a test, and must contain /usr/bin:/bin for ordinary binaries.
const testPath = "/usr/bin:/bin:/usr/local/go/bin"

// cachedEnv is one test's isolated environment, computed once.
type cachedEnv struct {
	workspace string
	home      string
	env       []string
}

// envCache memoizes the per-test environment. t.TempDir() returns a NEW
// directory on every call, so without this the workspace a child was
// handed and the one Workspace(t) reports would be different
// directories — and every assertion about "where did the child write?"
// would silently be about the wrong path. Keying on the *testing.T
// keeps parallel tests independent.
var envCache = struct {
	mu sync.Mutex
	m  map[*testing.T]*cachedEnv
}{m: map[*testing.T]*cachedEnv{}}

// load returns the test's environment, building it on first use.
func load(t *testing.T) *cachedEnv {
	t.Helper()

	envCache.mu.Lock()
	defer envCache.mu.Unlock()

	if c, ok := envCache.m[t]; ok {
		return c
	}

	// Two independent temporary roots. MPM_WORKSPACE is what the binaries
	// actually consult; HOME is the fallback. Setting both means neither
	// resolution path can reach the operator's state.
	c := &cachedEnv{
		workspace: t.TempDir(),
		home:      t.TempDir(),
	}
	c.env = append([]string{
		"MPM_WORKSPACE=" + c.workspace,
		"HOME=" + c.home,
		"PATH=" + testPath,
		// Keep the scheduler out of the picture entirely; several
		// suites assert on machine-mode output that this would
		// otherwise suppress.
		"MPM_SCHEDULER_DISABLED=1",
	}, blank(providerKeys)...)
	c.env = append(c.env, blank(isolatedKeys)...)

	envCache.m[t] = c
	t.Cleanup(func() {
		envCache.mu.Lock()
		delete(envCache.m, t)
		envCache.mu.Unlock()
	})
	return c
}

// Env returns a complete environment for a subprocess that can open the
// MPM database. The result is safe to assign directly to cmd.Env.
//
// The environment is hermetic: it does not inherit the ambient
// environment at all, so the test's result cannot vary with the shell
// it runs under.
//
// Env is stable within a test — repeated calls return the same
// workspace — so Env, WithExtra, Workspace and DBPath always agree.
func Env(t *testing.T) []string {
	t.Helper()
	return load(t).env
}

// WithExtra returns Env(t) plus the supplied "K=V" entries, appended
// last so a caller can deliberately override a value Env cleared (for
// example to point a fake provider at an httptest server).
func WithExtra(t *testing.T, extra ...string) []string {
	t.Helper()
	// Copied so a caller's append cannot write into the cached slice.
	return concat(Env(t), extra)
}

// NoWorkspace returns an environment that deliberately OMITS
// MPM_WORKSPACE, plus a temporary working directory to assign to
// cmd.Dir.
//
// Some regressions are specifically about the unset-workspace path —
// cmd/mpm-telemetry/help_inert_test.go exists to prove that a help
// surface does not require MPM_WORKSPACE or resolve a workspace before
// short-circuiting. For those, adding MPM_WORKSPACE would delete the
// thing being tested.
//
// Omitting it does not mean accepting the hazard, because MPM has two
// independent fallbacks. NoWorkspace closes the other one: HOME stays
// pinned to a temp dir (so config.GetMPMDir's $HOME/.mpm cannot resolve)
// and a temp work directory is returned (so mpmcli.ResolveWorkspace's "."
// cannot resolve to the package directory). Both are returned because
// cmd.Dir is a separate field from cmd.Env and is easy to forget:
//
//	cmd := exec.Command(bin, "telemetry", "--help")
//	cmd.Env, cmd.Dir = testenv.NoWorkspace(t)
//
// This is the only sanctioned way to run a subprocess without a pinned
// MPM_WORKSPACE. Prefer Env unless the unset case is the subject.
func NoWorkspace(t *testing.T, extra ...string) (env []string, workDir string) {
	t.Helper()
	c := load(t)

	out := make([]string, 0, len(c.env)+len(extra))
	for _, kv := range c.env {
		// MPM_WORKSPACE is the whole point of this variant.
		if strings.HasPrefix(kv, "MPM_WORKSPACE=") {
			continue
		}
		out = append(out, kv)
	}
	return concat(out, extra), t.TempDir()
}

// Workspace returns the temporary directory Env(t) pinned as
// MPM_WORKSPACE. Callers that need to inspect or seed a child's
// database use this instead of inventing a second temp dir, which would
// silently point the child somewhere else.
func Workspace(t *testing.T) string {
	t.Helper()
	return load(t).workspace
}

// Home returns the temporary directory Env(t) pinned as HOME.
func Home(t *testing.T) string {
	t.Helper()
	return load(t).home
}

// DBPath returns the database path inside the workspace Env(t) pins.
// Mirrors the "The database is ALWAYS at mpm/src/db/mpm.db" contract
// used throughout cmd/mpm.
func DBPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(Workspace(t), "src", "db", "mpm.db")
}

func concat(base, extra []string) []string {
	out := make([]string, 0, len(base)+len(extra))
	out = append(out, base...)
	return append(out, extra...)
}

func blank(keys []string) []string {
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"=")
	}
	return out
}

// AssertNoProductionAccess fails the test if the given path resolves
// inside the operator's real MPM state. Intended for the canary in the
// subprocess-isolation regression: it proves a child opened temporary
// state without ever naming the production database as a target.
func AssertNoProductionAccess(t *testing.T, path string) {
	t.Helper()
	real, err := os.Readlink(os.Getenv("HOME") + "/.mpm")
	if err != nil {
		real = os.Getenv("HOME") + "/.mpm"
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		t.Fatalf("resolve %q: %v", path, err)
	}
	if abs == real || filepath.Dir(filepath.Dir(filepath.Dir(abs))) == real {
		t.Fatalf("path %q resolves inside production MPM state %q", abs, real)
	}
}
