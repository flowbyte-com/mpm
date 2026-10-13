// test_environment_invariants_test.go — pins the build settings that Go
// tests in this repository REQUIRE, at every place a test command is
// written down.
//
// Why this file exists
//
// On 2026-10-12 the credential-resolver work appeared to produce 145
// test failures. It produced none. The failures were an artifact of how
// the tests were invoked: `go test -tags fts5` carries the Go build tag
// but NOT the CGO environment, so the mattn/go-sqlite3 driver was
// compiled without FTS5 and every search, pointer-resolution and
// scheduler-trigger assertion failed at once. The cost was not the
// spurious failures — it was that the real signal had to be
// re-established by hand before any result could be trusted.
//
// Three settings are required together. Any one missing breaks the
// suite, in three different ways:
//
//	CGO_CFLAGS=-DSQLITE_ENABLE_FTS5=1   C-level: compile FTS5 into SQLite
//	-tags fts5                         Go-level: select the FTS5 driver path
//	CGO_LDFLAGS=-lm                    link: FTS5's bm25() calls log(),
//	                                   which lives in libm. Hosts that link
//	                                   libm implicitly (most dev machines)
//	                                   never notice; CI runners and fresh
//	                                   containers fail with
//	                                   "undefined reference to `log`".
//
// The third is the one that keeps getting left out: it is invisible on
// the machine you develop on and fatal on the machine you ship from.
//
// These tests do not run the suite — they pin the flag sets written
// into the Makefile, the CI workflow and the agent-facing docs, so that
// a future edit which drops a flag fails loudly in review instead of
// quietly producing a wall of fake failures.

package internal

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// requiredTestSettings are the build settings every `go test` invocation
// in this repository must be able to satisfy. The first and third are
// environment; the second is a flag.
type requiredSetting struct {
	name string
	// token is the literal that must appear alongside the invocation.
	token string
	// why explains the failure mode in the assertion message, so whoever
	// trips this test learns what breaks rather than just what changed.
	why string
}

var requiredTestSettings = []requiredSetting{
	{
		name:  "CGO_CFLAGS",
		token: "CGO_CFLAGS",
		why: "without it SQLite is compiled without FTS5 and every search, " +
			"pointer-resolution and scheduler-trigger assertion fails",
	},
	{
		name:  "fts5 build tag",
		token: "-tags fts5",
		why: "without it the Go side selects the non-FTS5 driver path even " +
			"though the C side has FTS5 compiled in",
	},
	{
		name:  "CGO_LDFLAGS",
		token: "CGO_LDFLAGS",
		why: "without -lm linking fails with \"undefined reference to `log`\" " +
			"from FTS5's bm25(); this is invisible on hosts that link libm " +
			"implicitly and fatal on CI runners and fresh containers",
	},
}

// goTestRE matches a real test invocation in build files: either the
// literal `go test` or the Makefile's `$(GO) test`.
var goTestRE = regexp.MustCompile(`(?:\$\(GO\)|\bgo)\s+test\b`)

// commentRE matches a Makefile comment line, including recipe-scoped
// comments (tab or spaces, then `#`).
var commentRE = regexp.MustCompile(`^\s*#`)

// quotedSpanRE matches a quoted or backticked span within a line. The
// Makefile quotes prose in its guard recipes — for example
// `echo "\`go test -run\` exits 0 on an empty match"` — and that prose
// names the test runner without invoking it. Blanking these spans before
// matching keeps the guard from firing on its own explanatory text.
var quotedSpanRE = regexp.MustCompile("`[^`]*`|\"[^\"]*\"|'[^']*'")

// assertionLine is one logical line plus the source line number it began
// on, for use in failure messages.
type assertionLine struct {
	lineNo int
	text   string
}

// isToolchainInvocation reports whether line actually runs the test runner,
// rather than merely naming it inside a comment or an echo.
func isToolchainInvocation(line string) bool {
	if commentRE.MatchString(line) {
		return false
	}
	return goTestRE.MatchString(quotedSpanRE.ReplaceAllString(line, ""))
}

// logicalLines splits text into logical lines, joining backslash
// continuations.
//
// Build settings are conventionally written one per line for
// readability:
//
//	cd internal/core && \
//	  CGO_CFLAGS=-DSQLITE_ENABLE_FTS5=1 CGO_LDFLAGS=-lm \
//	  go test -tags fts5 -v ./... -run TestName
//
// Checking those lines individually would flag the `go test` line as
// missing its environment even though the continuation carries it. The
// settings belong to the command, not to the physical row it happens to
// land on, so the guard has to see the whole command.
//
// Each returned entry carries the 1-based number of the line the command
// *started* on, for use in failure messages.
func logicalLines(text string) []assertionLine {
	var out []assertionLine
	lines := strings.Split(text, "\n")
	for i := 0; i < len(lines); {
		start := i + 1
		joined := strings.TrimRight(lines[i], " \t")
		for strings.HasSuffix(joined, "\\") && i+1 < len(lines) {
			joined = strings.TrimSuffix(strings.TrimRight(joined, "\\"), "")
			i++
			joined += " " + strings.TrimSpace(lines[i])
		}
		out = append(out, assertionLine{lineNo: start, text: joined})
		i++
	}
	return out
}

// missingSettings returns the required settings absent from line.
//
// A per-line check is sufficient: the Makefile uses one command per line
// with the environment inline, so this catches the real failure mode —
// a recipe line added without the flags — without needing to parse
// recipes, continuations, or nested shells.
func missingSettings(line string) []string {
	var missing []string
	for _, req := range requiredTestSettings {
		if !strings.Contains(line, req.token) {
			missing = append(missing, req.name)
		}
	}
	return missing
}

// TestBuildConfig_MakefileGoTestLinesCarryAllRequiredSettings pins that
// every test invocation in the Makefile carries all three required
// settings. This is the single source of truth for local test execution
// (AGENTS.md §1 names `make test-race` the canonical gate), so an
// invocation added here without the flags is a defect that reaches every
// developer and every pre-commit hook.
func TestBuildConfig_MakefileGoTestLinesCarryAllRequiredSettings(t *testing.T) {
	root := findRepoRoot(t)
	data, err := os.ReadFile(filepath.Join(root, "Makefile"))
	if err != nil {
		t.Fatalf("read Makefile: %v", err)
	}
	checked := 0
	for _, logical := range logicalLines(string(data)) {
		line := logical.text
		i := logical.lineNo - 1
		if !isToolchainInvocation(line) {
			continue
		}
		checked++
		missing := missingSettings(line)
		if len(missing) == 0 {
			continue
		}
		var why []string
		for _, name := range missing {
			for _, req := range requiredTestSettings {
				if req.name == name {
					why = append(why, name+": "+req.why)
				}
			}
		}
		t.Errorf("Makefile:%d invokes the test runner without %s\n  line: %s\n  why:  %s\n"+
			"  fix: add the missing setting(s) inline, as every other test recipe in this file does",
			i+1, strings.Join(missing, ", "), strings.TrimSpace(line), strings.Join(why, " / "))
	}

	if checked == 0 {
		t.Fatal("no test invocations found in the Makefile; this guard would pass " +
			"vacuously if the Makefile were restructured or renamed")
	}
	t.Logf("verified %d Makefile test invocation(s) carry all required build settings", checked)
}

// TestBuildConfig_CIWorkflowGoTestLinesCarryFTS5Tag pins that every test
// invocation in the GitHub Actions workflow carries the fts5 build tag.
//
// CI pins CGO_CFLAGS/CGO_LDFLAGS/CGO_ENABLED once at workflow level
// (env:) so each step inherits them — that part is correct and this test
// does not second-guess it. But the build tag is a per-command flag with
// no workflow-level equivalent, so each invocation has to carry it
// individually, and exactly one did not: the gating "Core tests" step
// ran `go test -short -count=1 ./... -run ...` with no `-tags fts5`. It
// passed only because that `-run` filter happened to select tests that
// tolerate a non-FTS5 build; the step was one edit away from silently
// skipping the FTS5 path entirely.
func TestBuildConfig_CIWorkflowGoTestLinesCarryFTS5Tag(t *testing.T) {
	root := findRepoRoot(t)
	rel := filepath.Join(".github", "workflows", "build-test.yml")
	data, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	text := string(data)

	checked := 0
	for _, logical := range logicalLines(text) {
		line := logical.text
		i := logical.lineNo - 1
		if !isToolchainInvocation(line) {
			continue
		}
		// `go tool cover` is not an invocation.
		if strings.Contains(line, "go tool") {
			continue
		}
		checked++
		if !strings.Contains(line, "-tags fts5") {
			t.Errorf("%s:%d runs the test runner without `-tags fts5`\n  line: %s\n"+
				"  why: CGO_CFLAGS is pinned at workflow level but the build tag is a\n"+
				"       per-command flag with no workflow-level equivalent, so it must\n"+
				"       appear on every invocation. Without it this step silently compiles\n"+
				"       the non-FTS5 driver path.",
				rel, i+1, strings.TrimSpace(line))
		}
	}
	if checked == 0 {
		t.Fatalf("no test invocations found in %s; this guard would pass vacuously", rel)
	}
	t.Logf("verified %d CI test invocation(s) carry -tags fts5", checked)
}

// TestBuildConfig_CIWorkflowPinsCGOEnvironment pins the workflow-level
// env: block. This is the setting that cannot be pushed down onto
// individual commands, and it is the one whose absence produces a link
// failure on the runner rather than an assertion failure in a test.
func TestBuildConfig_CIWorkflowPinsCGOEnvironment(t *testing.T) {
	root := findRepoRoot(t)
	rel := filepath.Join(".github", "workflows", "build-test.yml")
	data, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	text := string(data)

	for _, want := range []string{
		`CGO_ENABLED: "1"`,
		`CGO_CFLAGS: "-DSQLITE_ENABLE_FTS5=1"`,
		`CGO_LDFLAGS: "-lm"`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("%s must pin %s at workflow level\n  why: without it every CI test step "+
				"either compiles without FTS5 or fails to link on `log`", rel, want)
		}
	}
}

// TestBuildConfig_AgentDocsNameEveryRequiredBuildSetting pins the
// agent-facing and contributor docs.
//
// These are the files an agent reads before running tests. They already
// warned correctly against bare `go test -race ./...`, but their
// "required settings" lists named only two of the three — CGO_LDFLAGS
// was absent, which is the setting a developer cannot discover from a
// local failure (their machine links libm implicitly) and only meets on
// CI. A doc that lists two of three required settings is worse than one
// that lists none: it reads as complete.
func TestBuildConfig_AgentDocsNameEveryRequiredBuildSetting(t *testing.T) {
	root := findRepoRoot(t)

	docs := []string{"AGENTS.md", "CLAUDE.md"}
	for _, doc := range docs {
		t.Run(doc, func(t *testing.T) {
			path := filepath.Join(root, doc)
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read %s: %v", doc, err)
			}
			text := string(data)

			for _, req := range requiredTestSettings {
				if !strings.Contains(text, req.token) {
					t.Errorf("%s does not name the required setting %q\n  why: %s\n"+
						"  This doc is where an agent looks before running tests; a partial\n"+
						"  list reads as complete and sends them down a path that fails on CI\n"+
						"  rather than on their own machine.",
						doc, req.token, req.why)
				}
			}
		})
	}
}

// TestBuildConfig_MakefileBuildLinesCarryAllRequiredSettings pins build/test
// symmetry.
//
// `make build` and `make test` were not symmetric: every test recipe
// carried CGO_LDFLAGS=-lm, but none of the five build recipes did. That
// asymmetry was invisible locally (dev hosts link libm implicitly) and
// only bites where it does not — CI runners and fresh containers — which
// is exactly the profile that keeps a latent build defect latent. A
// binary that builds locally and fails to link in CI is a miserable thing
// to debug, and the fix is cheaper as a pinned invariant than as an
// outage.
//
// Scoped to `go build` rather than every toolchain command: `go run
// ./cmd/gen-cli` (Makefile) is a code generator that does not touch
// go-sqlite3, and requiring FTS5 flags there would be cargo-culting.
func TestBuildConfig_MakefileBuildLinesCarryAllRequiredSettings(t *testing.T) {
	root := findRepoRoot(t)
	data, err := os.ReadFile(filepath.Join(root, "Makefile"))
	if err != nil {
		t.Fatalf("read Makefile: %v", err)
	}

	goBuildRE := regexp.MustCompile(`(?:\$\(GO\)|\bgo)\s+build\b`)

	checked := 0
	for _, logical := range logicalLines(string(data)) {
		line := logical.text
		if commentRE.MatchString(line) ||
			!goBuildRE.MatchString(quotedSpanRE.ReplaceAllString(line, "")) {
			continue
		}
		checked++
		if missing := missingSettings(line); len(missing) > 0 {
			t.Errorf("Makefile:%d builds without %s\n  line: %s\n"+
				"  why: `make build` must carry the same settings as `make test`, or the\n"+
				"       binaries link only where libm is implicit",
				logical.lineNo, strings.Join(missing, ", "), strings.TrimSpace(line))
		}
	}
	if checked == 0 {
		t.Fatal("no `go build` invocations found in the Makefile; this guard would pass vacuously")
	}
	t.Logf("verified %d Makefile build invocation(s) carry all required build settings", checked)
}

// TestBuildConfig_ContributingPointsAtMakeTarget pins that the
// contributor guide does not hand out a partial hand-rolled test command.
//
// CONTRIBUTING.md previously offered `go test -tags fts5 -v ./... -run
// TestName` as the way to run a single test. That carries the build tag
// but neither CGO setting, so following it verbatim fails to link with
// "undefined reference to `log`". Pointing at `make test` instead keeps
// the flag set in one place.
func TestBuildConfig_ContributingPointsAtMakeTarget(t *testing.T) {
	root := findRepoRoot(t)
	path := filepath.Join(root, "docs", "CONTRIBUTING.md")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("no docs/CONTRIBUTING.md: %v", err)
	}
	text := string(data)

	// Any hand-rolled `go test` line that does not go through the
	// Makefile must still carry every required setting.
	for _, logical := range logicalLines(text) {
		line := logical.text
		i := logical.lineNo - 1
		if !isToolchainInvocation(line) {
			continue
		}
		missing := missingSettings(line)
		if len(missing) > 0 {
			t.Errorf("docs/CONTRIBUTING.md:%d gives a hand-rolled test command missing %s\n  line: %s\n"+
				"  fix: use `make test` (flag set lives in the Makefile) or include every\n"+
				"       required setting inline",
				i+1, strings.Join(missing, ", "), strings.TrimSpace(line))
		}
	}

	if !strings.Contains(text, "make test") {
		t.Error("docs/CONTRIBUTING.md never points at `make test`; the contributor guide " +
			"should route test execution through the Makefile so the flag set has one owner")
	}
}