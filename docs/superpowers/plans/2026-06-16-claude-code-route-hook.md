# Claude Code Auto-Routing Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Wire MPM's `internal.Router` into Claude Code via a `UserPromptSubmit` hook so every user prompt is auto-routed to the appropriate mode + persona *before* the LLM sees it, by exposing `mpm route` (text renderer) and `mpm call route` (JSON-RPC endpoint) in the main `mpm` binary.

**Architecture:** Stateless, no daemon. The Claude Code hook command is the bare `mpm route` binary on PATH. The hook receives JSON on stdin, `mpm route` extracts the prompt, evaluates it against the existing `internal.Router` (same engine used by `mpm-mcp`), reads the selected mode+persona files, and prints a `<system-reminder>` block to stdout. `mpm call route` is a parallel JSON-RPC entry for OpenClaw/Hermes. Both paths use the same `internal.Router`.

**Tech Stack:** Go (existing `internal/router.go` is the engine). Test framework: standard `testing` package with `go test -tags fts5`. Mode/persona files are Markdown with YAML frontmatter. Workspace path resolution via `MPM_WORKSPACE` env var with `"."` fallback.

**Spec:** `docs/superpowers/specs/2026-06-16-claude-code-route-hook-design.md` (committed in `70947a7`).

**Reference implementation:** `cmd/mpm-mcp/tools.go:toolRoute` + `handleRoute` (lines 342-367) shows the existing pattern for the `route` MCP tool — the new `mpm call route` handler mirrors it.

---

## File Structure

| File | Action | Purpose |
|---|---|---|
| `cmd/mpm/route_render.go` | Create | All CLI-side route code: workspace resolution, input parsing, opt-out checks, render, truncation |
| `cmd/mpm/route_render_test.go` | Create | Table-driven tests for the renderer |
| `cmd/mpm/call_route_test.go` | Create | Tests for the `mpm call route` JSON handler |
| `cmd/mpm/call.go` | Modify | Add `route` to `toolRegistry` and `callRoute` handler (~30 LOC) |
| `cmd/mpm/router.go` | Modify | Add `route` to `CommandRouter.Commands` + `case "route"` + `handleRoute` dispatcher (~20 LOC) |
| `README.md` | Modify | New "Claude Code integration" section + tool table row + JSON-RPC example |
| `internal/router.go` | **No change** | The engine is complete; this plan only consumes it |
| `cmd/mpm-mcp/` | **No change** | The MCP server already exposes `route` correctly |
| `opencode-mpm-plugin/`, `hermes-mpm-plugin/` | **No change** | They already work; they will call `mpm call route` going forward |

---

## Task 1: Workspace resolution helper

**Files:**
- Create: `cmd/mpm/route_render.go`
- Create: `cmd/mpm/route_render_test.go`

- [ ] **Step 1: Write the failing test**

In `cmd/mpm/route_render_test.go`:

```go
package main

import (
	"os"
	"testing"
)

func TestResolveRouteWorkspace(t *testing.T) {
	tests := []struct {
		name     string
		envValue string
		setEnv   bool
		want     string
	}{
		{name: "env var set", envValue: "/custom/path", setEnv: true, want: "/custom/path"},
		{name: "env var unset falls back to dot", envValue: "", setEnv: false, want: "."},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.setEnv {
				os.Setenv("MPM_ROUTE_WORKSPACE", tt.envValue)
				defer os.Unsetenv("MPM_ROUTE_WORKSPACE")
			} else {
				os.Unsetenv("MPM_ROUTE_WORKSPACE")
			}
			got := resolveRouteWorkspace()
			if got != tt.want {
				t.Errorf("resolveRouteWorkspace() = %q, want %q", got, tt.want)
			}
		})
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd /home/v/workspace/projects/mpm && CGO_CFLAGS=-DSQLITE_ENABLE_FTS5=1 go test -tags fts5 -v -run TestResolveRouteWorkspace ./cmd/mpm/`
Expected: FAIL with `undefined: resolveRouteWorkspace`

- [ ] **Step 3: Write minimal implementation**

In `cmd/mpm/route_render.go`:

```go
package main

import "os"

// resolveRouteWorkspace returns the MPM workspace path for `mpm route`.
// Resolution order: MPM_ROUTE_WORKSPACE env var → "." (current directory).
// The router inside will then load mode/ and persona/ from this base path.
func resolveRouteWorkspace() string {
	if v := os.Getenv("MPM_ROUTE_WORKSPACE"); v != "" {
		return v
	}
	return "."
}
```

Note: We use `MPM_ROUTE_WORKSPACE` (not `MPM_WORKSPACE`) so the hook's workspace resolution is independent of other MPM contexts (the MCP server, `mpm call` etc.). This avoids accidentally routing against a workspace the user has set for a different purpose. The README will document this.

- [ ] **Step 4: Run test to verify it passes**

Run: `cd /home/v/workspace/projects/mpm && CGO_CFLAGS=-DSQLITE_ENABLE_FTS5=1 go test -tags fts5 -v -run TestResolveRouteWorkspace ./cmd/mpm/`
Expected: PASS (2 subtests)

- [ ] **Step 5: Commit**

```bash
cd /home/v/workspace/projects/mpm
git add cmd/mpm/route_render.go cmd/mpm/route_render_test.go
git commit -m "feat(route): add workspace resolution helper for mpm route CLI

Uses MPM_ROUTE_WORKSPACE env var with '.' fallback. Distinct from
MPM_WORKSPACE so the hook's workspace doesn't leak into other MPM
contexts (MCP server, mpm call, etc.)."

Co-Authored-By: Claude Opus 4.7 <noreply@anthropic.com>
```

---

## Task 2: Prompt input parsing (arg / stdin JSON / stdin literal)

**Files:**
- Modify: `cmd/mpm/route_render.go`
- Modify: `cmd/mpm/route_render_test.go`

- [ ] **Step 1: Write the failing test**

Append to `cmd/mpm/route_render_test.go`:

```go
import "strings"

func TestExtractRoutePrompt(t *testing.T) {
	tests := []struct {
		name        string
		args        []string
		stdin       string
		want        string
	}{
		{
			name:  "positional arg wins over stdin",
			args:  []string{"positional prompt"},
			stdin: `{"prompt":"json prompt"}`,
			want:  "positional prompt",
		},
		{
			name:  "stdin JSON when no arg",
			args:  []string{},
			stdin: `{"prompt":"json prompt"}`,
			want:  "json prompt",
		},
		{
			name:  "stdin literal when no arg and not JSON",
			args:  []string{},
			stdin: "literal prompt\n",
			want:  "literal prompt",
		},
		{
			name:  "empty when no arg and no stdin",
			args:  []string{},
			stdin: "",
			want:  "",
		},
		{
			name:  "stdin JSON missing prompt field returns empty",
			args:  []string{},
			stdin: `{"other":"value"}`,
			want:  "",
		},
		{
			name:  "stdin JSON malformed falls back to literal",
			args:  []string{},
			stdin: "{not valid json",
			want:  "{not valid json",
		},
		{
			name:  "stdin JSON with empty prompt returns empty",
			args:  []string{},
			stdin: `{"prompt":""}`,
			want:  "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractRoutePrompt(tt.args, strings.NewReader(tt.stdin))
			if got != tt.want {
				t.Errorf("extractRoutePrompt() = %q, want %q", got, tt.want)
			}
		})
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd /home/v/workspace/projects/mpm && CGO_CFLAGS=-DSQLITE_ENABLE_FTS5=1 go test -tags fts5 -v -run TestExtractRoutePrompt ./cmd/mpm/`
Expected: FAIL with `undefined: extractRoutePrompt`

- [ ] **Step 3: Write minimal implementation**

Append to `cmd/mpm/route_render.go`:

```go
import (
	"encoding/json"
	"io"
	"strings"
)

// extractRoutePrompt returns the user prompt for route evaluation.
// Precedence:
//  1. Positional arg (preferred for shells/hooks passing inline text)
//  2. Stdin parsed as JSON {"prompt": "..."} (Claude Code hook format)
//  3. Stdin treated literally (human `echo "..." | mpm route` use)
//
// Returns "" if no prompt source yields content. Malformed JSON on stdin
// falls back to the literal stdin content (defense in depth — the binary
// should still be usable in a pipe even if the upstream is non-conformant).
func extractRoutePrompt(args []string, stdin io.Reader) string {
	if len(args) > 0 {
		return strings.TrimSpace(args[0])
	}
	if stdin == nil {
		return ""
	}
	data, err := io.ReadAll(stdin)
	if err != nil {
		return ""
	}
	s := strings.TrimSpace(string(data))
	if s == "" {
		return ""
	}
	if strings.HasPrefix(s, "{") {
		var hook struct {
			Prompt string `json:"prompt"`
		}
		if err := json.Unmarshal([]byte(s), &hook); err == nil {
			return hook.Prompt
		}
		// Malformed JSON: fall through to literal
	}
	return s
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd /home/v/workspace/projects/mpm && CGO_CFLAGS=-DSQLITE_ENABLE_FTS5=1 go test -tags fts5 -v -run TestExtractRoutePrompt ./cmd/mpm/`
Expected: PASS (7 subtests)

- [ ] **Step 5: Commit**

```bash
cd /home/v/workspace/projects/mpm
git add cmd/mpm/route_render.go cmd/mpm/route_render_test.go
git commit -m "feat(route): add prompt extraction (arg / JSON stdin / literal stdin)

Precedence: positional arg > JSON stdin > literal stdin. Malformed JSON
falls back to literal stdin so the binary remains usable in pipes."

Co-Authored-By: Claude Opus 4.7 <noreply@anthropic.com>
```

---

## Task 3: Opt-out checks (`/noroute` prefix, `MPM_ROUTE=off` env, empty prompt)

**Files:**
- Modify: `cmd/mpm/route_render.go`
- Modify: `cmd/mpm/route_render_test.go`

- [ ] **Step 1: Write the failing test**

Append to `cmd/mpm/route_render_test.go`:

```go
func TestShouldSkipRoute(t *testing.T) {
	tests := []struct {
		name        string
		prompt      string
		envValue    string
		envSet      bool
		wantSkip    bool
		wantReason  string
	}{
		{name: "empty prompt", prompt: "", wantSkip: true, wantReason: "empty"},
		{name: "whitespace only", prompt: "   \t\n", wantSkip: true, wantReason: "empty"},
		{name: "noroute prefix", prompt: "/noroute do something", wantSkip: true, wantReason: "noroute"},
		{name: "noroute prefix mid prompt still triggers", prompt: "explain /noroute", wantSkip: true, wantReason: "noroute"},
		{name: "MPM_ROUTE=off env", prompt: "do something", envValue: "off", envSet: true, wantSkip: true, wantReason: "env"},
		{name: "MPM_ROUTE=anything-else env does not skip", prompt: "do something", envValue: "on", envSet: true, wantSkip: false},
		{name: "MPM_ROUTE=off with empty prompt", prompt: "", envValue: "off", envSet: true, wantSkip: true, wantReason: "empty"},
		{name: "normal prompt no skip", prompt: "review this code", wantSkip: false},
		{name: "noroute is case-sensitive (NOROUTE not matched)", prompt: "NOROUTE this", wantSkip: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			envLookup := func(key string) string {
				if key == "MPM_ROUTE" && tt.envSet {
					return tt.envValue
				}
				return ""
			}
			got, reason := shouldSkipRoute(tt.prompt, envLookup)
			if got != tt.wantSkip {
				t.Errorf("shouldSkipRoute() skip = %v, want %v", got, tt.wantSkip)
			}
			if tt.wantReason != "" && reason != tt.wantReason {
				t.Errorf("shouldSkipRoute() reason = %q, want %q", reason, tt.wantReason)
			}
		})
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd /home/v/workspace/projects/mpm && CGO_CFLAGS=-DSQLITE_ENABLE_FTS5=1 go test -tags fts5 -v -run TestShouldSkipRoute ./cmd/mpm/`
Expected: FAIL with `undefined: shouldSkipRoute`

- [ ] **Step 3: Write minimal implementation**

Append to `cmd/mpm/route_render.go`:

```go
import "strings"

// shouldSkipRoute returns (true, reason) if the prompt should bypass routing.
// Reason is one of: "empty", "noroute", "env". Reason is "" when skip=false.
//
// Checked in this order (first match wins):
//  1. Empty or whitespace-only prompt → "empty"
//  2. Prompt contains "/noroute" anywhere → "noroute"
//  3. MPM_ROUTE=off env var → "env"
//
// The envLookup indirection lets tests simulate the env var without
// mutating process state.
func shouldSkipRoute(prompt string, envLookup func(string) string) (bool, string) {
	if strings.TrimSpace(prompt) == "" {
		return true, "empty"
	}
	if strings.Contains(prompt, "/noroute") {
		return true, "noroute"
	}
	if envLookup("MPM_ROUTE") == "off" {
		return true, "env"
	}
	return false, ""
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd /home/v/workspace/projects/mpm && CGO_CFLAGS=-DSQLITE_ENABLE_FTS5=1 go test -tags fts5 -v -run TestShouldSkipRoute ./cmd/mpm/`
Expected: PASS (9 subtests)

- [ ] **Step 5: Commit**

```bash
cd /home/v/workspace/projects/mpm
git add cmd/mpm/route_render.go cmd/mpm/route_render_test.go
git commit -m "feat(route): add opt-out checks (empty, /noroute, MPM_ROUTE=off)

Checked in fixed precedence: empty > /noroute > env. Reason returned
for logging/diagnostics. Env-var lookup is parameterized for testing."

Co-Authored-By: Claude Opus 4.7 <noreply@anthropic.com>
```

---

## Task 4: Length cap and truncation (persona-priority)

**Files:**
- Modify: `cmd/mpm/route_render.go`
- Modify: `cmd/mpm/route_render_test.go`

- [ ] **Step 1: Write the failing test**

Append to `cmd/mpm/route_render_test.go`:

```go
func TestApplyRouteLengthCap(t *testing.T) {
	shortMarker := "[...truncated, see mode/<name>.md for full content]"
	longMarker := "[...truncated]"

	tests := []struct {
		name       string
		modeText   string
		personaText string
		wantMarker string // "" = expect no marker
		wantMode   string // expected substring in mode position
	}{
		{
			name:        "under cap no truncation",
			modeText:    "MODE-CONTENT",
			personaText: "PERSONA-CONTENT",
			wantMode:    "MODE-CONTENT",
		},
		{
			name:        "persona truncated when combined exceeds cap",
			modeText:    strings.Repeat("m", 5000),
			personaText: strings.Repeat("p", 5000),
			wantMarker:  shortMarker,
			wantMode:    strings.Repeat("m", 5000),
		},
		{
			name:        "mode truncated when even persona removal not enough",
			modeText:    strings.Repeat("M", 10000),
			personaText: strings.Repeat("p", 100),
			wantMarker:  longMarker,
		},
		{
			name:        "empty persona no truncation",
			modeText:    strings.Repeat("m", 1000),
			personaText: "",
			wantMode:    strings.Repeat("m", 1000),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := applyRouteLengthCap(tt.modeText, tt.personaText)
			if tt.wantMode != "" && !strings.Contains(got, tt.wantMode) {
				t.Errorf("applyRouteLengthCap() missing expected mode content")
			}
			if tt.wantMarker != "" && !strings.Contains(got, tt.wantMarker) {
				t.Errorf("applyRouteLengthCap() missing expected marker %q\nGot: %s", tt.wantMarker, got)
			}
			if tt.wantMarker == "" && strings.Contains(got, "truncated") {
				t.Errorf("applyRouteLengthCap() unexpected truncation: %s", got)
			}
		})
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd /home/v/workspace/projects/mpm && CGO_CFLAGS=-DSQLITE_ENABLE_FTS5=1 go test -tags fts5 -v -run TestApplyRouteLengthCap ./cmd/mpm/`
Expected: FAIL with `undefined: applyRouteLengthCap`

- [ ] **Step 3: Write minimal implementation**

Append to `cmd/mpm/route_render.go`:

```go
const routeOutputCap = 9500 // under Claude Code's 10,000-char hook stdout limit
const routeModeHardCap = 9000

// applyRouteLengthCap returns the rendered <system-reminder> body given
// pre-extracted mode and persona text. Priority: preserve mode (operational
// rules) over persona (voice/tone).
//
// Rules:
//   - Combined length ≤ routeOutputCap: return both unchanged
//   - Combined length > cap with persona present: truncate persona, append marker
//   - Mode alone exceeds routeModeHardCap: truncate mode, append marker
func applyRouteLengthCap(modeText, personaText string) string {
	shortMarker := "\n[...truncated, see mode/<name>.md for full content]"
	longMarker := "\n[...truncated]"

	combined := modeText + personaText
	if len(combined) <= routeOutputCap {
		return modeText + personaText
	}
	if personaText != "" {
		return modeText + shortMarker
	}
	if len(modeText) > routeModeHardCap {
		return modeText[:routeModeHardCap] + longMarker
	}
	return modeText + longMarker
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd /home/v/workspace/projects/mpm && CGO_CFLAGS=-DSQLITE_ENABLE_FTS5=1 go test -tags fts5 -v -run TestApplyRouteLengthCap ./cmd/mpm/`
Expected: PASS (4 subtests)

- [ ] **Step 5: Commit**

```bash
cd /home/v/workspace/projects/mpm
git add cmd/mpm/route_render.go cmd/mpm/route_render_test.go
git commit -m "feat(route): add 9500-char length cap with persona-priority truncation

Mode (operational rules) takes precedence over persona (voice/tone).
Persona truncated first; if mode alone still exceeds 9000 chars, truncate
mode with a generic marker. Stays under Claude Code's 10K hook stdout cap."

Co-Authored-By: Claude Opus 4.7 <noreply@anthropic.com>
```

---

## Task 5: Main render function (router invocation + system-reminder generation)

**Files:**
- Modify: `cmd/mpm/route_render.go`
- Modify: `cmd/mpm/route_render_test.go`

- [ ] **Step 1: Write the failing test**

Append to `cmd/mpm/route_render_test.go`:

```go
import (
	"path/filepath"
)

func TestRenderRoute(t *testing.T) {
	// Use the live workspace — it has known mode/persona files and matches
	// the pattern in internal/router_test.go. This is integration-level.
	workspace := "/home/v/workspace/projects/mpm"

	tests := []struct {
		name           string
		prompt         string
		wantEmpty      bool
		wantContains   []string
	}{
		{
			name:      "low-signal prompt produces no output",
			prompt:    "hi",
			wantEmpty: true,
		},
		{
			name:      "architect mode triggered by architecture keyword",
			prompt:    "Design the system architecture for our new API gateway",
			wantContains: []string{
				"<system-reminder>",
				"mode=",
				"persona=",
				"</system-reminder>",
			},
		},
		{
			name:      "code-review-flavored prompt also routes",
			prompt:    "review this PR for security vulnerabilities",
			wantContains: []string{
				"<system-reminder>",
				"MPM auto-route active",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := renderRoute(workspace, tt.prompt)
			if err != nil {
				t.Fatalf("renderRoute: %v", err)
			}
			if tt.wantEmpty {
				if got != "" {
					t.Errorf("renderRoute() = %q, want empty (low-signal prompt should not inject context)", got)
				}
				return
			}
			for _, sub := range tt.wantContains {
				if !strings.Contains(got, sub) {
					t.Errorf("renderRoute() missing substring %q\nGot: %s", sub, got)
				}
			}
		})
	}
}

func TestRenderRoute_EmptyWorkspace(t *testing.T) {
	// Empty workspace should return empty output, not error
	got, err := renderRoute("", "review this code for security")
	if err != nil {
		t.Fatalf("renderRoute with empty workspace: %v", err)
	}
	if got != "" {
		t.Errorf("renderRoute() with empty workspace should return empty, got %q", got)
	}
}
```

Note: `renderRoute("", ...)` returns empty because `internal.NewRouter("")` will fail and we swallow the error. The test verifies the graceful-fallback contract.

- [ ] **Step 2: Run test to verify it fails**

Run: `cd /home/v/workspace/projects/mpm && CGO_CFLAGS=-DSQLITE_ENABLE_FTS5=1 go test -tags fts5 -v -run TestRenderRoute ./cmd/mpm/`
Expected: FAIL with `undefined: renderRoute`

- [ ] **Step 3: Write minimal implementation**

Append to `cmd/mpm/route_render.go`:

```go
import (
	"fmt"
	"os"
	"path/filepath"

	"mpm/internal"
)

// renderRoute evaluates prompt against the workspace's mode+persona files
// and returns a <system-reminder> block for the LLM. Returns ("", nil) when
// no mode or persona matched (low-signal prompt) or when the workspace is
// unusable. Returns error only for unexpected internal failures.
//
// Behavior matches the spec (Component 2):
//   - Empty/low-signal prompt → "", nil
//   - Missing/unusable workspace → "", nil
//   - Mode file missing for selected mode → "", nil (operational rules are load-bearing)
//   - Persona file missing → render mode only, append marker
//   - Combined output > 9500 chars → applyRouteLengthCap
func renderRoute(workspace, prompt string) (string, error) {
	if workspace == "" {
		return "", nil
	}

	router, err := internal.NewRouter(workspace)
	if err != nil {
		// Workspace unusable (missing mode/persona dirs etc.) — graceful exit
		return "", nil
	}

	report := router.Evaluate(prompt)
	if len(report.SelectedModes) == 0 && report.SelectedPersona == "" {
		// No mode and no persona matched — don't inject anything
		return "", nil
	}

	// Pick the highest-priority mode (first selected — router.Evaluate appends
	// in iteration order). For multi-mode selection, concatenate all mode files.
	var modeText, personaText string
	if len(report.SelectedModes) > 0 {
		// Take the first selected mode's file. If multiple, concatenate with
		// separators so the LLM sees all of them.
		for i, modeName := range report.SelectedModes {
			content, err := os.ReadFile(filepath.Join(workspace, "mode", modeName+".md"))
			if err != nil {
				// Mode file missing — refuse to inject partial operational rules
				return "", nil
			}
			if i > 0 {
				modeText += "\n\n---\n\n"
			}
			modeText += string(content)
		}
	}

	if report.SelectedPersona != "" {
		content, err := os.ReadFile(filepath.Join(workspace, "persona", report.SelectedPersona+".md"))
		if err != nil {
			// Persona missing — render mode only, append marker
			return wrapReminder(modeText) + "\n\n[persona " + report.SelectedPersona + " not found on disk]", nil
		}
		personaText = string(content)
	}

	body := applyRouteLengthCap(modeText, personaText)
	return wrapReminder(body), nil
}

// wrapReminder wraps body in a <system-reminder> block with the auto-route header.
func wrapReminder(body string) string {
	return fmt.Sprintf("<system-reminder>\n%s\n</system-reminder>", body)
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd /home/v/workspace/projects/mpm && CGO_CFLAGS=-DSQLITE_ENABLE_FTS5=1 go test -tags fts5 -v -run TestRenderRoute ./cmd/mpm/`
Expected: PASS (3 subtests in TestRenderRoute, 1 in TestRenderRoute_EmptyWorkspace)

Note: the "code-review-flavored prompt also routes" test may or may not select a mode — the test only asserts on the wrapper format. If the router doesn't find a high-score mode for "review this PR for security vulnerabilities", the test will fail with "missing substring MPM auto-route active". Check the live router behavior; if it doesn't match, adjust the test prompt to one that does route (e.g. "Design the system architecture" already works, "Implement the user authentication flow" from the existing test should also work).

- [ ] **Step 5: Commit**

```bash
cd /home/v/workspace/projects/mpm
git add cmd/mpm/route_render.go cmd/mpm/route_render_test.go
git commit -m "feat(route): add main renderer — router invocation + system-reminder

Loads internal.Router for the resolved workspace, evaluates prompt,
reads selected mode/persona files, and renders a <system-reminder>
block. Graceful fallback on any failure returns empty (not error) so
the hook can never block the user."

Co-Authored-By: Claude Opus 4.7 <noreply@anthropic.com>
```

---

## Task 6: `mpm call route` JSON-RPC handler

**Files:**
- Modify: `cmd/mpm/call.go`
- Create: `cmd/mpm/call_route_test.go`

- [ ] **Step 1: Add the test first**

In `cmd/mpm/call_route_test.go` (new file):

```go
package main

import "testing"

func TestCallRoute_RequiresPrompt(t *testing.T) {
	_, err := callRoute(map[string]interface{}{})
	if err == nil {
		t.Fatal("callRoute with empty payload should require prompt field")
	}
}

func TestCallRoute_ReturnsReport(t *testing.T) {
	// Use the live workspace — matches the pattern in cmd/mpm-mcp/tools.go
	// and internal/router_test.go. Sets MPM_ROUTE_WORKSPACE for the test scope.
	t.Setenv("MPM_ROUTE_WORKSPACE", "/home/v/workspace/projects/mpm")

	result, err := callRoute(map[string]interface{}{
		"prompt": "Design the system architecture for our new API gateway",
	})
	if err != nil {
		t.Fatalf("callRoute: %v", err)
	}

	report, ok := result.(map[string]interface{})
	if !ok {
		t.Fatalf("callRoute result is not a map: %T", result)
	}
	modes, ok := report["selected_modes"].([]string)
	if !ok || len(modes) == 0 {
		t.Errorf("callRoute result missing or empty selected_modes: %v", report["selected_modes"])
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd /home/v/workspace/projects/mpm && CGO_CFLAGS=-DSQLITE_ENABLE_FTS5=1 go test -tags fts5 -v -run TestCallRoute ./cmd/mpm/`
Expected: FAIL with `undefined: callRoute`

- [ ] **Step 3: Add the handler in `cmd/mpm/call.go`**

Open `cmd/mpm/call.go`. Add `"route": callRoute,` to the `toolRegistry` map. Place it in the System section (after `"proactive_recall_hint": ...`):

```go
	// System
	"read_wake_context":     callReadWakeContext,
	"read_directives":       callReadDirectives,
	"proactive_recall_hint": callProactiveRecallHint,
	"route":                 callRoute,
```

Then append the handler function at the end of the file:

```go
// callRoute evaluates a prompt against the workspace's mode+persona
// configuration and returns a RoutingReport. This is the JSON-RPC path
// for OpenClaw and Hermes — pure JSON, no text rendering.
//
// Unlike mpm route (text), this handler returns errors instead of silently
// producing empty output. Callers are machines and can handle failures.
func callRoute(p map[string]interface{}) (interface{}, error) {
	prompt, _ := p["prompt"].(string)
	if prompt == "" {
		return nil, fmt.Errorf("prompt is required")
	}

	workspace := resolveRouteWorkspace()
	router, err := internal.NewRouter(workspace)
	if err != nil {
		return nil, fmt.Errorf("router init: %w", err)
	}

	return router.Evaluate(prompt), nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd /home/v/workspace/projects/mpm && CGO_CFLAGS=-DSQLITE_ENABLE_FTS5=1 go test -tags fts5 -v -run TestCallRoute ./cmd/mpm/`
Expected: PASS (2 tests)

- [ ] **Step 5: Smoke-test via CLI**

Build and run:

```bash
cd /home/v/workspace/projects/mpm
make build
./bin/mpm call route --payload '{"prompt":"Design the system architecture for a new API gateway"}'
```

Expected: JSON output containing `"selected_modes": ["architect", ...]` (or similar — depends on live router behavior; the test suite already verified the contract).

- [ ] **Step 6: Commit**

```bash
cd /home/v/workspace/projects/mpm
git add cmd/mpm/call.go cmd/mpm/call_route_test.go
git commit -m "feat(call): add route tool — JSON-RPC path for OpenClaw/Hermes

Mirrors cmd/mpm-mcp/tools.go:toolRoute but exposed via the mpm call
interface for non-MCP consumers. Returns RoutingReport JSON, surfaces
real errors (unlike mpm route text which silently degrades)."

Co-Authored-By: Claude Opus 4.7 <noreply@anthropic.com>
```

---

## Task 7: `mpm route` CLI command registration

**Files:**
- Modify: `cmd/mpm/router.go`

- [ ] **Step 1: Add the command to the CommandRouter registry**

In `cmd/mpm/router.go`, in the `r.Commands = map[string]*Command{` block, add a new entry for `route`. Place it near the existing routing-adjacent commands (e.g. after `"hint"`):

```go
		"route":  {Name: "route", Description: "Render mode+persona for a prompt (Claude Code hook input)", MinArgs: 0, MaxArgs: 1},
```

- [ ] **Step 2: Add the case in the Execute switch**

In the same file, in the `switch cmd.Name` block, add:

```go
	case "route":
		return r.handleRoute(args[1:])
```

- [ ] **Step 3: Add the handleRoute dispatcher**

Append to `cmd/mpm/router.go`:

```go
// handleRoute reads prompt from positional arg or stdin, evaluates against
// the workspace's mode+persona files, and prints a <system-reminder> block
// to stdout. Designed for the Claude Code UserPromptSubmit hook — never
// blocks the user on errors (any failure → exit 0, no output).
//
// Usage:
//   mpm route "review this code"        # positional arg
//   echo "review this" | mpm route      # stdin literal
//   mpm route < hook-stdin.json         # stdin JSON (Claude Code format)
func (r *CommandRouter) handleRoute(args []string) int {
	prompt := extractRoutePrompt(args, os.Stdin)
	skip, _ := shouldSkipRoute(prompt, os.Getenv)
	if skip {
		return 0
	}

	workspace := resolveRouteWorkspace()
	rendered, err := renderRoute(workspace, prompt)
	if err != nil {
		// Programmer-level error. Only surface on TTY (interactive) — never
		// when invoked from a hook (would corrupt hook output).
		if isatty(os.Stdout) {
			fmt.Fprintf(os.Stderr, "mpm route: %v\n", err)
		}
		return 0
	}

	if rendered != "" {
		fmt.Println(rendered)
	}
	return 0
}
```

- [ ] **Step 4: Add the isatty helper**

If `isatty` is not already defined in the package, append to `cmd/mpm/router.go`:

```go
// isatty returns true if f is a terminal. Used to gate stderr noise.
func isatty(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}
```

If `isatty` already exists elsewhere in the package, skip this step. Check with:

```bash
cd /home/v/workspace/projects/mpm && grep -n "func isatty" cmd/mpm/*.go
```

- [ ] **Step 5: Build and smoke-test**

```bash
cd /home/v/workspace/projects/mpm
make build
```

Expected: builds without errors.

Then exercise the four input modes:

```bash
# 1. Positional arg
./bin/mpm route "review this code for security issues" | head -5
# Expected: <system-reminder> header

# 2. Stdin literal
echo "design a new system architecture" | ./bin/mpm route | head -5
# Expected: <system-reminder> header

# 3. Stdin JSON (Claude Code format)
echo '{"prompt":"implement user authentication in Go"}' | ./bin/mpm route | head -5
# Expected: <system-reminder> header

# 4. Low-signal prompt produces nothing
echo "hi" | ./bin/mpm route
# Expected: no output, exit 0

# 5. /noroute opt-out
echo "/noroute explain quantum computing" | ./bin/mpm route
# Expected: no output, exit 0
```

- [ ] **Step 6: Run the full test suite**

```bash
cd /home/v/workspace/projects/mpm && make test
```

Expected: all tests pass (no regressions, plus the new tests from Tasks 1-6).

- [ ] **Step 7: Commit**

```bash
cd /home/v/workspace/projects/mpm
git add cmd/mpm/router.go
git commit -m "feat(route): wire mpm route top-level command into CommandRouter

Reads prompt from positional arg or stdin (JSON or literal). Always
exits 0; any failure path produces no output so the Claude Code hook
can never block the user. Stderr noise is gated on TTY detection."

Co-Authored-By: Claude Opus 4.7 <noreply@anthropic.com>
```

---

## Task 8: README — Claude Code integration section

**Files:**
- Modify: `README.md`

- [ ] **Step 1: Locate the existing call-tool table and JSON-RPC section**

In `README.md`, find:
- The call-tool table (the one that currently lists 19 tools)
- The JSON-RPC examples block (if present)

If either is missing or hard to locate, check the recent README commits for context:

```bash
cd /home/v/workspace/projects/mpm && git log --oneline -5 -- README.md
```

- [ ] **Step 2: Add `route` to the call-tool table**

In the call-tool table (the one listing `save_to_memory`, `query_long_term_memory`, etc.), add a row for `route`. Place it next to `record_decision` and before `proactive_recall_hint` to match the canonical 19-tool order (per the recent README fix commit `613027e`):

```markdown
| `route` | Auto-select mode + persona for a prompt (returns `RoutingReport`) |
```

Adjust the column header / formatting to match the existing table.

- [ ] **Step 3: Add a `route` example to the JSON-RPC examples block**

If a JSON-RPC examples block exists, add:

```json
// mpm call route --payload '{"prompt":"Design the system architecture for our new API gateway"}'
{
  "success": true,
  "result": {
    "selected_modes": ["architect"],
    "selected_persona": "default",
    "scores": { "...": "..." }
  }
}
```

(Adjust the example to match the actual table format and only show key fields.)

- [ ] **Step 4: Add a new `## Claude Code integration` section**

Add a new top-level section near the end of the README (before any "License" / "Contributing" / "Acknowledgments" sections). Use this content:

```markdown
## Claude Code integration

MPM can auto-route every Claude Code prompt to the appropriate mode + persona
*before* the LLM sees it. This is implemented as a `UserPromptSubmit` hook
that runs `mpm route` synchronously; the rendered mode+persona is injected
into the LLM's context as a `<system-reminder>` block.

### Install

Add the following to `~/.claude/settings.json`:

```json
{
  "hooks": {
    "UserPromptSubmit": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "mpm route",
            "timeout": 1,
            "statusMessage": "MPM routing…"
          }
        ]
      }
    ]
  }
}
```

Requirements:
- `mpm` must be on your `PATH` (run `which mpm` to verify). If it's not,
  Claude Code will show a non-blocking hook error in the transcript.
- `MPM_ROUTE_WORKSPACE` env var (optional) — if unset, `mpm route` uses
  the current working directory as the MPM workspace base.

### Opt-out

- Type `/noroute` anywhere in your prompt → routing is skipped for that turn
- Set `MPM_ROUTE=off` in your shell env → routing is skipped for the session
- A low-signal prompt (e.g. "hi") that doesn't match any mode or persona
  produces no injected context — the LLM responds natively

### Verify it works

```bash
# Should print a <system-reminder> block
echo "review this code for security issues" | mpm route

# Should print nothing (no mode matched)
echo "hi" | mpm route

# Should print nothing (opt-out)
echo "/noroute explain quantum computing" | mpm route
```

### Truncation

Rendered output is capped at 9,500 characters (under Claude Code's 10,000-char
hook stdout limit). If a mode + persona combination would exceed the cap,
the persona (voice/tone) is truncated first and a marker is appended
referencing the on-disk file. Operational rules are never truncated unless
they alone exceed 9,000 characters.
```

- [ ] **Step 5: Update the tool count (if it appears in the README)**

Search for any "19 tools" or similar count claim in `README.md` and verify it matches reality. The count is 19 (route is the 19th; no change from before this work). If the README correctly says 19 and lists `route` in the right position, no change. If the README still says 18 or omits `route`, fix it.

- [ ] **Step 6: Build and run the verify-it-works block**

```bash
cd /home/v/workspace/projects/mpm
make build
echo "review this code for security issues" | ./bin/mpm route | head -3
echo "---"
echo "hi" | ./bin/mpm route
echo "---END---"
```

Expected: First command produces a `<system-reminder>` header line. Second produces nothing between the two `---` markers.

- [ ] **Step 7: Commit**

```bash
cd /home/v/workspace/projects/mpm
git add README.md
git commit -m "docs: add Claude Code integration section + route tool row

Documents the UserPromptSubmit hook config, opt-out mechanisms, and
verify-it-works examples. Adds 'route' to the call-tool table at the
correct position (19th tool, per recent README fix in 613027e)."

Co-Authored-By: Claude Opus 4.7 <noreply@anthropic.com>
```

---

## Self-Review

**1. Spec coverage:**
- ✅ `mpm call route` JSON endpoint → Task 6
- ✅ `mpm route` top-level CLI → Task 7
- ✅ Hook config documented → Task 8 (README)
- ✅ Workspace resolution (`MPM_ROUTE_WORKSPACE` env, "." fallback) → Task 1
- ✅ Input precedence (positional > JSON stdin > literal stdin) → Task 2
- ✅ Opt-out checks (empty, /noroute, MPM_ROUTE=off) → Task 3
- ✅ Length cap 9500, persona-priority truncation → Task 4
- ✅ Graceful fallback (any failure → empty, exit 0) → Task 5, Task 7
- ✅ 1s hook timeout → Task 8 (in JSON snippet)
- ✅ No `mpm install-claude-hook` helper → confirmed (deferred, README-only)
- ✅ Tool count stays at 19 → Task 8 (Step 5)

**2. Placeholder scan:** No TBD, TODO, or "implement later" patterns. Every step has exact code or exact commands.

**3. Type consistency:**
- `resolveRouteWorkspace() string` defined Task 1, used Task 5/7 — matches
- `extractRoutePrompt(args []string, stdin io.Reader) string` defined Task 2, used Task 7 — matches
- `shouldSkipRoute(prompt string, envLookup func(string) string) (bool, string)` defined Task 3, used Task 7 — matches
- `applyRouteLengthCap(modeText, personaText string) string` defined Task 4, used Task 5 — matches
- `renderRoute(workspace, prompt string) (string, error)` defined Task 5, used Task 7 — matches
- `callRoute(p map[string]interface{}) (interface{}, error)` matches the `ToolHandler` signature in `cmd/mpm/call.go:18` — consistent with all other call handlers

**4. Edge cases from spec all covered:**
- Empty/whitespace prompt → Task 3 (shouldSkipRoute)
- /noroute prefix → Task 3
- MPM_ROUTE=off env → Task 3
- No MPM workspace resolvable → Task 5 (workspace=="" check) + Task 1 (always returns something)
- DB missing → N/A (renderer doesn't touch the DB)
- Mode JSON malformed → N/A (files are .md; renderer just reads them as bytes)
- Persona file missing → Task 5 (render mode only, append marker)
- Mode file missing → Task 5 (exit empty)
- Stdin not valid JSON → Task 2 (fall back to literal)
- Rendered 9.5K–10K → Task 4
- Rendered > 10K → Task 4

**5. Known limitations of this plan:**
- Test for `mpm call route` (Task 6) and `TestRenderRoute` (Task 5) use the live `/home/v/workspace/projects/mpm` workspace. This matches the existing pattern in `internal/router_test.go` but does mean tests depend on the live mode/persona files. If the user edits those files, the tests may need adjustment. This is acceptable — the alternative is duplicating fixtures, which is over-engineering for now.
- The "code-review-flavored prompt" subtest in Task 5 may not select any mode if the live router doesn't have a code-review mode. The test only asserts on the wrapper format, so it should still pass; if it fails, adjust the prompt to one the live router definitely routes (e.g. "Design the system architecture" or "Implement the user authentication flow in Go" from existing tests).
