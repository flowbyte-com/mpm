package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	mpminternal "github.com/flowbyte-com/mpm-core"
)

func TestRenderRoute(t *testing.T) {
	// Use the live workspace — it has known mode/persona files and matches
	// the pattern in internal/router_test.go. This is integration-level.
	workspace := "/home/v/workspace/projects/mpm"

	tests := []struct {
		name         string
		prompt       string
		wantEmpty    bool
		wantContains []string
	}{
		{
			name:      "low-signal prompt produces no output",
			prompt:    "hi",
			wantEmpty: true,
		},
		{
			name:   "architect mode triggered by architecture keyword",
			prompt: "Design the system architecture for our new API gateway",
			wantContains: []string{
				"<system-reminder>",
				"mode=",
				"persona=",
				"</system-reminder>",
			},
		},
		{
			name:   "code-review-flavored prompt also routes",
			prompt: "Implement the user authentication flow in Go with proper security",
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

// TestRenderRoute_LongBodyTruncatesPersona exercises the persona-priority
// truncation path end-to-end. A regression caused renderRoute to pass an empty
// string as the persona to applyRouteLengthCap, so the persona branch was
// dead code and bodies in the 9,500-10,000 char range slipped past the cap.
// This test builds a synthetic workspace with a short mode and long persona,
// fires a prompt that routes to both, and asserts the persona is dropped with
// the short marker appended to mode.
func TestRenderRoute_LongBodyTruncatesPersona(t *testing.T) {
	workspace := t.TempDir()
	mustMkdir(t, filepath.Join(workspace, "mode"))
	mustMkdir(t, filepath.Join(workspace, "persona"))

	// Short mode — content well under 9,000 chars.
	shortModeBody := "## Bigmode\n\nShort mode body."
	mustWriteFile(t, filepath.Join(workspace, "mode", "bigmode.md"),
		"---\nname: bigmode\npatterns: bigmode\n---\n\n"+shortModeBody+"\n")

	// Long persona — pads with whitespace and a unique sentinel word to push
	// the total rendered body into the 9,500-10,000 range. The sentinel word
	// is the same one used in the prompt, so the persona will be selected.
	const sentinel = "persona-sentinel-truncatetest"
	// Persona label + 50 padding lines of ~150 chars each = ~7500 chars of
	// persona body, combined with the mode body this lands us in the
	// 9,500-10,000 range where the persona-priority branch should fire.
	padding := strings.Repeat(sentinel+" ", 20) + "\n"
	var personaBody strings.Builder
	personaBody.WriteString("## Bigpersona\n\n")
	for i := 0; i < 60; i++ {
		personaBody.WriteString(padding)
	}
	mustWriteFile(t, filepath.Join(workspace, "persona", "bigpersona.md"),
		"---\nname: bigpersona\npatterns: "+sentinel+"\n---\n\n"+personaBody.String())

	prompt := "design with bigmode and " + sentinel
	got, err := renderRoute(workspace, prompt)
	if err != nil {
		t.Fatalf("renderRoute: %v", err)
	}
	if got == "" {
		t.Fatalf("renderRoute returned empty — synthetic workspace didn't route the prompt as expected")
	}

	// The persona-priority truncation marker MUST appear in the rendered output.
	// The markers include a leading newline so they sit on their own line in
	// the rendered body — match the literals used in applyRouteLengthCap.
	shortMarker := "\n[...truncated, see mode/<name>.md for full content]"
	longMarker := "\n[...truncated]"
	if !strings.Contains(got, shortMarker) {
		t.Errorf("renderRoute() missing persona-priority truncation marker %q\nGot (%d chars):\n%s", shortMarker, len(got), got)
	}
	// The long (mode-truncation) marker must NOT appear on its own — the
	// persona-priority path should drop the persona and preserve the mode,
	// not truncate the mode. This assertion distinguishes the fixed behavior
	// from the buggy old one, which would have routed the whole concatenated
	// body to the long path (since the old call site passed the combined
	// body in the first slot and an empty string in the second, making the
	// persona-priority branch dead code).
	//
	// We can't just check strings.Contains for the bare long marker text —
	// `[...truncated]` is a substring of the short marker. So we look for the
	// long marker as it actually appears in the output: the short marker ends
	// with `]` and the long marker would need to appear after a non-`,` char.
	if strings.Contains(got, longMarker) && !strings.Contains(got, shortMarker) {
		t.Errorf("renderRoute() emitted the long (mode-truncation) marker without the short one — persona-priority path was bypassed\nGot: %s", got)
	}
	// The persona body content should NOT survive in the rendered output.
	// The persona label `persona=bigpersona` is also dropped, since the
	// persona was truncated entirely.
	if strings.Contains(got, "persona=bigpersona") {
		t.Errorf("renderRoute() should have dropped the persona section, but `persona=bigpersona` label is present\nGot: %s", got)
	}
	// The mode section must be preserved.
	if !strings.Contains(got, "mode=bigmode") {
		t.Errorf("renderRoute() should preserve the mode section, but `mode=bigmode` is missing\nGot: %s", got)
	}
	if !strings.Contains(got, shortModeBody) {
		t.Errorf("renderRoute() should preserve the mode body content\nGot: %s", got)
	}
	// Total output (including the system-reminder wrapper) must stay under
	// the 10,000-char Claude Code hook limit.
	if len(got) >= 10000 {
		t.Errorf("renderRoute() output = %d chars, must be < 10,000 to stay under hook limit", len(got))
	}
	t.Logf("renderRoute output length: %d chars (cap = 9,500 body, < 10,000 hook limit)", len(got))
}

func mustMkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
}

func mustWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestResolveRouteWorkspace(t *testing.T) {
	tests := []struct {
		name     string
		envValue string
		setEnv   bool
		// The unset-env case asserts a STRUCTURAL property (absolute, not ".")
		// rather than a fixed string, because the actual default depends on
		// the caller's MPM_WORKSPACE / $HOME state. Pre-fix this asserted
		// `"."`, which was the bug — see 2026-07-30 audit.
		wantLiteral string // when set, exact equality; otherwise check IsAbs + non-"."
	}{
		{name: "env var set", envValue: "/custom/path", setEnv: true, wantLiteral: "/custom/path"},
		{name: "env var unset falls back to absolute path (regression: not '.')", envValue: "", setEnv: false},
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
			if tt.wantLiteral != "" {
				if got != tt.wantLiteral {
					t.Errorf("resolveRouteWorkspace() = %q, want %q", got, tt.wantLiteral)
				}
				return
			}
			if got == "." {
				t.Errorf("resolveRouteWorkspace() = %q — relative path bug regression (2026-07-30 audit)", got)
			}
			if !filepath.IsAbs(got) {
				t.Errorf("resolveRouteWorkspace() = %q, want absolute path (resolved via config.GetMPMDir)", got)
			}
		})
	}
}

func TestExtractRoutePrompt(t *testing.T) {
	tests := []struct {
		name  string
		args  []string
		stdin string
		want  string
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
		{
			// Claude Code's UserPromptSubmit hook sends {user_prompt: ...}.
			// Pre-fix this fell through to literal-stdin and routed the whole
			// JSON blob. 2026-07-30 audit.
			name:  "stdin JSON with user_prompt field (Claude Code hook format)",
			args:  []string{},
			stdin: `{"user_prompt":"review this architecture","session_id":"abc"}`,
			want:  "review this architecture",
		},
		{
			// prompt wins when both fields are set (back-compat with the
			// original contract — positional scripts that build their own
			// JSON should still work).
			name:  "stdin JSON with both prompt and user_prompt prefers prompt",
			args:  []string{},
			stdin: `{"prompt":"from prompt","user_prompt":"from user_prompt"}`,
			want:  "from prompt",
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

func TestApplyRouteLengthCap(t *testing.T) {
	// Marker strings must match the literals in applyRouteLengthCap exactly —
	// they begin with a newline so they sit on their own line in the rendered
	// output. Length arithmetic in the test cases uses len(shortMarker) and
	// len(longMarker), so changing the marker text requires updating both.
	shortMarker := "\n[...truncated, see mode/<name>.md for full content]"
	longMarker := "\n[...truncated]"

	tests := []struct {
		name        string
		modeText    string
		personaText string
		// Expected return values. wantModeLen/wantPersonaLen of -1 means
		// "exact equality with the input" — used for short inputs where we
		// want to assert identity rather than a length.
		wantModeLen    int
		wantPersonaLen int
		wantMarker     string // "" = expect no marker; otherwise must appear in truncatedMode
	}{
		{
			name:           "under cap no truncation",
			modeText:       "MODE-CONTENT",
			personaText:    "PERSONA-CONTENT",
			wantModeLen:    -1, // exact match
			wantPersonaLen: -1, // exact match
		},
		{
			name:           "persona truncated when combined exceeds cap",
			modeText:       strings.Repeat("m", 5000),
			personaText:    strings.Repeat("p", 5000),
			wantModeLen:    5000 + len(shortMarker), // mode preserved + marker
			wantPersonaLen: 0,                       // persona dropped
			wantMarker:     shortMarker,
		},
		{
			name:           "mode truncated when even persona removal not enough",
			modeText:       strings.Repeat("M", 10000),
			personaText:    strings.Repeat("p", 100),
			wantModeLen:    routeModeHardCap + len(longMarker), // truncated to 9000 + marker
			wantPersonaLen: 0,                                  // persona dropped
			wantMarker:     longMarker,
		},
		{
			name:           "empty persona no truncation",
			modeText:       strings.Repeat("m", 1000),
			personaText:    "",
			wantModeLen:    -1, // exact match
			wantPersonaLen: -1, // exact match (empty)
		},
		{
			name: "persona dropped but mode preserved (the bug class)",
			// This is the regression case the renderer used to miss: a body
			// in the 9,500-10,000 range with a long persona and short mode.
			// The persona-priority branch should drop the persona and append
			// the short marker to mode.
			modeText:       strings.Repeat("m", 3000),
			personaText:    strings.Repeat("p", 7000),
			wantModeLen:    3000 + len(shortMarker),
			wantPersonaLen: 0,
			wantMarker:     shortMarker,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotMode, gotPersona := applyRouteLengthCap(tt.modeText, tt.personaText)
			if tt.wantModeLen == -1 {
				if gotMode != tt.modeText {
					t.Errorf("applyRouteLengthCap() mode = %q, want unchanged %q", gotMode, tt.modeText)
				}
			} else if len(gotMode) != tt.wantModeLen {
				t.Errorf("applyRouteLengthCap() mode length = %d, want %d\nGot: %s", len(gotMode), tt.wantModeLen, gotMode)
			}
			if tt.wantPersonaLen == -1 {
				if gotPersona != tt.personaText {
					t.Errorf("applyRouteLengthCap() persona = %q, want unchanged %q", gotPersona, tt.personaText)
				}
			} else if len(gotPersona) != tt.wantPersonaLen {
				t.Errorf("applyRouteLengthCap() persona length = %d, want %d\nGot: %s", len(gotPersona), tt.wantPersonaLen, gotPersona)
			}
			if tt.wantMarker != "" {
				if !strings.Contains(gotMode, tt.wantMarker) {
					t.Errorf("applyRouteLengthCap() mode missing expected marker %q\nGot mode: %s", tt.wantMarker, gotMode)
				}
			} else if strings.Contains(gotMode, "truncated") {
				t.Errorf("applyRouteLengthCap() unexpected truncation marker in mode: %s", gotMode)
			}
		})
	}
}

func TestDirectiveInjectionLimit(t *testing.T) {
	tests := []struct {
		name   string
		envVal string
		envSet bool
		want   int
	}{
		{name: "unset returns 0", envSet: false, want: 0},
		{name: "empty returns 0", envVal: "", envSet: true, want: 0},
		{name: "non-numeric returns 0", envVal: "five", envSet: true, want: 0},
		{name: "zero returns 0", envVal: "0", envSet: true, want: 0},
		{name: "negative returns 0", envVal: "-1", envSet: true, want: 0},
		{name: "positive returns N", envVal: "5", envSet: true, want: 5},
		{name: "large positive returns N", envVal: "100", envSet: true, want: 100},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.envSet {
				t.Setenv("MPM_ROUTE_DIRECTIVES", tt.envVal)
			} else {
				os.Unsetenv("MPM_ROUTE_DIRECTIVES")
			}
			got := directiveInjectionLimit()
			if got != tt.want {
				t.Errorf("directiveInjectionLimit() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestResolveMPMDatabase(t *testing.T) {
	tests := []struct {
		name     string
		setup    func(t *testing.T, dir string) // creates candidate files
		wantFile string                         // basename of expected return, "" for none
	}{
		{
			name:     "no candidates returns empty",
			setup:    func(t *testing.T, dir string) {},
			wantFile: "",
		},
		{
			name: "mpm.db at root is preferred",
			setup: func(t *testing.T, dir string) {
				mustWriteFile(t, filepath.Join(dir, "mpm.db"), "")
				mustWriteFile(t, filepath.Join(dir, "mpm.sqlite"), "")
			},
			wantFile: "mpm.db",
		},
		{
			name: "mpm.sqlite when no mpm.db",
			setup: func(t *testing.T, dir string) {
				mustWriteFile(t, filepath.Join(dir, "mpm.sqlite"), "")
			},
			wantFile: "mpm.sqlite",
		},
		{
			name: "src/db/mpm.db is fallback",
			setup: func(t *testing.T, dir string) {
				mustMkdir(t, filepath.Join(dir, "src", "db"))
				mustWriteFile(t, filepath.Join(dir, "src", "db", "mpm.db"), "")
			},
			wantFile: "mpm.db",
		},
		{
			name: "empty workspace returns empty",
			setup: func(t *testing.T, dir string) {
				mustWriteFile(t, filepath.Join(dir, "mpm.db"), "")
			},
			wantFile: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			tt.setup(t, dir)

			// Adjust for empty workspace case
			ws := dir
			if tt.name == "empty workspace returns empty" {
				ws = ""
			}

			got := resolveMPMDatabase(ws)
			if tt.wantFile == "" {
				if got != "" {
					t.Errorf("resolveMPMDatabase() = %q, want empty", got)
				}
				return
			}
			if filepath.Base(got) != tt.wantFile {
				t.Errorf("resolveMPMDatabase() = %q, want file %q", got, tt.wantFile)
			}
		})
	}
}

func TestFetchTopDirectives_GracefulDegradation(t *testing.T) {
	tests := []struct {
		name      string
		workspace string
		limit     int
		wantEmpty bool
	}{
		{name: "empty workspace", workspace: "", limit: 5, wantEmpty: true},
		{name: "zero limit", workspace: "/tmp", limit: 0, wantEmpty: true},
		{name: "negative limit", workspace: "/tmp", limit: -1, wantEmpty: true},
		{name: "non-existent workspace", workspace: "/nonexistent/path/abc123", limit: 5, wantEmpty: true},
		{name: "workspace without DB", workspace: t.TempDir(), limit: 5, wantEmpty: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := fetchTopDirectives(tt.workspace, tt.limit)
			if tt.wantEmpty && got != "" {
				t.Errorf("fetchTopDirectives() = %q, want empty (graceful degradation)", got)
			}
		})
	}
}

func TestRenderRoute_DirectiveInjectionOptIn(t *testing.T) {
	// Build a workspace that will route. Then verify that with MPM_ROUTE_DIRECTIVES
	// unset, the output does NOT include directive markers; with it set, the
	// output either includes directives (if a DB is present) or fails open
	// (if no DB is present). The point is to lock in the opt-in semantics.
	workspace := t.TempDir()
	mustMkdir(t, filepath.Join(workspace, "mode"))
	mustMkdir(t, filepath.Join(workspace, "persona"))
	mustWriteFile(t, filepath.Join(workspace, "mode", "testmode.md"),
		"---\nname: testmode\npatterns: testmode-directive-opt-in\n---\n\n# Test Mode\n\nTest mode body.\n")
	mustWriteFile(t, filepath.Join(workspace, "persona", "testpersona.md"),
		"---\nname: testpersona\npatterns: testpersona-directive-opt-in\n---\n\n# Test Persona\n\nTest persona body.\n")

	prompt := "testmode-directive-opt-in with testpersona-directive-opt-in"

	t.Run("default behavior excludes directive marker", func(t *testing.T) {
		os.Unsetenv("MPM_ROUTE_DIRECTIVES")
		got, err := renderRoute(workspace, prompt)
		if err != nil {
			t.Fatalf("renderRoute: %v", err)
		}
		if got == "" {
			t.Fatalf("renderRoute returned empty — synthetic workspace didn't route")
		}
		if strings.Contains(got, "Prime Directives") {
			t.Errorf("renderRoute() unexpectedly included directive marker\nGot: %s", got)
		}
	})

	t.Run("MPM_ROUTE_DIRECTIVES=0 is treated as disabled", func(t *testing.T) {
		t.Setenv("MPM_ROUTE_DIRECTIVES", "0")
		got, err := renderRoute(workspace, prompt)
		if err != nil {
			t.Fatalf("renderRoute: %v", err)
		}
		if strings.Contains(got, "Prime Directives") {
			t.Errorf("renderRoute() with MPM_ROUTE_DIRECTIVES=0 should not inject directives\nGot: %s", got)
		}
	})

	t.Run("MPM_ROUTE_DIRECTIVES=5 with no DB fails open (no injection)", func(t *testing.T) {
		// Synthetic workspace has no DB — directive injection must fail open
		// rather than failing the route.
		t.Setenv("MPM_ROUTE_DIRECTIVES", "5")
		got, err := renderRoute(workspace, prompt)
		if err != nil {
			t.Fatalf("renderRoute: %v", err)
		}
		if got == "" {
			t.Fatalf("renderRoute returned empty — synthetic workspace didn't route")
		}
		// Body must still be valid (mode + persona). The DB-less path means
		// no directive block, which is the documented fail-open behavior.
		if !strings.Contains(got, "mode=testmode") {
			t.Errorf("renderRoute() should preserve mode section\nGot: %s", got)
		}
	})
}

func TestRenderRoute_DirectiveCacheTTLBounds(t *testing.T) {
	// This test verifies the TTL constant is in a sensible range. If someone
	// bumps it to an hour thinking it helps performance, this test fails and
	// forces a conversation about staleness vs throughput.
	if directiveCacheTTL > 30*time.Second {
		t.Errorf("directiveCacheTTL = %v, want <= 30s to keep directive changes visible within a conversation", directiveCacheTTL)
	}
	if directiveCacheTTL < 100*time.Millisecond {
		t.Errorf("directiveCacheTTL = %v, want >= 100ms to provide any meaningful absorption of burst traffic", directiveCacheTTL)
	}
}

func TestShouldSkipRoute(t *testing.T) {
	tests := []struct {
		name       string
		prompt     string
		envValue   string
		envSet     bool
		wantSkip   bool
		wantReason string
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

func TestStripApplyFlag(t *testing.T) {
	tests := []struct {
		name      string
		args      []string
		wantApply bool
		wantArgs  []string
	}{
		{name: "no flag", args: []string{"hello", "world"}, wantApply: false, wantArgs: []string{"hello", "world"}},
		{name: "apply present", args: []string{"--apply", "hello"}, wantApply: true, wantArgs: []string{"hello"}},
		{name: "apply last", args: []string{"hello", "--apply"}, wantApply: true, wantArgs: []string{"hello"}},
		{name: "apply middle", args: []string{"a", "--apply", "b"}, wantApply: true, wantArgs: []string{"a", "b"}},
		{name: "apply twice idempotent", args: []string{"--apply", "--apply"}, wantApply: true, wantArgs: []string{}},
		{name: "empty args", args: []string{}, wantApply: false, wantArgs: []string{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			apply, got := stripApplyFlag(tt.args)
			if apply != tt.wantApply {
				t.Errorf("stripApplyFlag() apply = %v, want %v", apply, tt.wantApply)
			}
			if len(got) != len(tt.wantArgs) {
				t.Fatalf("stripApplyFlag() len = %d, want %d (got %v)", len(got), len(tt.wantArgs), got)
			}
			for i := range got {
				if got[i] != tt.wantArgs[i] {
					t.Errorf("stripApplyFlag() arg[%d] = %q, want %q", i, got[i], tt.wantArgs[i])
				}
			}
		})
	}
}

func TestEqualStringSlices(t *testing.T) {
	tests := []struct {
		name string
		a, b []string
		want bool
	}{
		{name: "both nil", a: nil, b: nil, want: true},
		{name: "both empty", a: []string{}, b: []string{}, want: true},
		{name: "identical", a: []string{"x", "y"}, b: []string{"x", "y"}, want: true},
		{name: "different length", a: []string{"x"}, b: []string{"x", "y"}, want: false},
		{name: "different order", a: []string{"x", "y"}, b: []string{"y", "x"}, want: false},
		{name: "different content", a: []string{"x", "y"}, b: []string{"x", "z"}, want: false},
		{name: "nil vs empty", a: nil, b: []string{}, want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := equalStringSlices(tt.a, tt.b); got != tt.want {
				t.Errorf("equalStringSlices(%v, %v) = %v, want %v", tt.a, tt.b, got, tt.want)
			}
		})
	}
}

// TestApplyRouteToActive exercises the active.json writeback path. Each
// subtest points MPM_WORKSPACE at a fresh temp dir so the SaveActiveJSON
// side effect stays hermetic. We seed an initial active.json, fire the
// helper, and assert the file's contents reflect the documented rules:
//   - SelectedPersona non-empty AND different → updates persona
//   - SelectedPersona empty → preserves current persona
//   - SelectedModes non-empty AND different → replaces modes
//   - SelectedModes empty → preserves current modes
//   - No-op (everything matches) → no write (file mtime untouched)
//
// "Write happened" is detected via file mtime (nanosecond resolution)
// rather than the RFC3339 string field — back-to-back subtests can land
// in the same wall-clock second and produce identical timestamp strings.
func TestApplyRouteToActive(t *testing.T) {
	workspace := t.TempDir()
	t.Setenv("MPM_WORKSPACE", workspace)

	seed := mpminternal.ActiveState{
		Persona: "default",
		Modes:   []string{"programming"},
		Updated: "2026-06-26T10:07:39+01:00",
	}
	if err := mpminternal.SaveActiveJSON(&seed); err != nil {
		t.Fatalf("seed SaveActiveJSON: %v", err)
	}

	tests := []struct {
		name        string
		report      mpminternal.RoutingReport
		wantPersona string
		wantModes   []string
		wantWrote   bool // true → file mtime must advance; false → mtime must NOT change
	}{
		{
			name:        "new persona and modes override baseline",
			report:      mpminternal.RoutingReport{SelectedPersona: "venkat", SelectedModes: []string{"architect", "moe"}},
			wantPersona: "venkat",
			wantModes:   []string{"architect", "moe"},
			wantWrote:   true,
		},
		{
			name:        "empty persona preserves current (don't clobber on low-signal)",
			report:      mpminternal.RoutingReport{SelectedPersona: "", SelectedModes: []string{"research"}},
			wantPersona: "venkat", // unchanged from previous test
			wantModes:   []string{"research"},
			wantWrote:   true,
		},
		{
			name:        "empty modes preserve current modes (don't wipe programming)",
			report:      mpminternal.RoutingReport{SelectedPersona: "marcus", SelectedModes: nil},
			wantPersona: "marcus",
			wantModes:   []string{"research"}, // unchanged
			wantWrote:   true,
		},
		{
			name:        "no-op when both fields already match — preserves file mtime",
			report:      mpminternal.RoutingReport{SelectedPersona: "marcus", SelectedModes: []string{"research"}},
			wantPersona: "marcus",
			wantModes:   []string{"research"},
			wantWrote:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			beforeInfo, statErr := os.Stat(mpminternal.ActiveJSONPath())
			if statErr != nil {
				t.Fatalf("stat before: %v", statErr)
			}
			callStart := time.Now()

			applyRouteToActive(tt.report)

			after, err := mpminternal.LoadActiveJSON()
			if err != nil {
				t.Fatalf("load after: %v", err)
			}
			if after.Persona != tt.wantPersona {
				t.Errorf("Persona = %q, want %q", after.Persona, tt.wantPersona)
			}
			if !equalStringSlices(after.Modes, tt.wantModes) {
				t.Errorf("Modes = %v, want %v", after.Modes, tt.wantModes)
			}

			afterInfo, statErr := os.Stat(mpminternal.ActiveJSONPath())
			if statErr != nil {
				t.Fatalf("stat after: %v", statErr)
			}

			if tt.wantWrote {
				if !afterInfo.ModTime().After(beforeInfo.ModTime()) {
					t.Errorf("active.json mtime did not advance (before=%v after=%v, call started %v)",
						beforeInfo.ModTime(), afterInfo.ModTime(), callStart)
				}
				if _, err := time.Parse(time.RFC3339, after.Updated); err != nil {
					t.Errorf("Updated = %q, want valid RFC3339: %v", after.Updated, err)
				}
			} else {
				if !afterInfo.ModTime().Equal(beforeInfo.ModTime()) {
					t.Errorf("active.json mtime changed on no-op route (before=%v after=%v)",
						beforeInfo.ModTime(), afterInfo.ModTime())
				}
			}
		})
	}
}

// TestHandleRoute_ApplyFlag verifies --apply is wired end-to-end through
// the command dispatcher: invoking `mpm route --apply "<prompt>"` against
// a hermetic workspace must mutate active.json and not block on errors.
func TestHandleRoute_ApplyFlag(t *testing.T) {
	workspace := t.TempDir()
	t.Setenv("MPM_WORKSPACE", workspace)
	mustMkdir(t, filepath.Join(workspace, "mode"))
	mustMkdir(t, filepath.Join(workspace, "persona"))
	mustWriteFile(t, filepath.Join(workspace, "mode", "architect.md"),
		"---\nname: architect\npatterns: architecture\n---\n\nArchitect mode body.\n")
	mustWriteFile(t, filepath.Join(workspace, "persona", "venkat.md"),
		"---\nname: venkat\npatterns: architecture\n---\n\nVenkat persona body.\n")
	t.Setenv("MPM_ROUTE_WORKSPACE", workspace)

	if err := mpminternal.SaveActiveJSON(&mpminternal.ActiveState{
		Persona: "default",
		Modes:   []string{"standard"},
		Updated: "2026-01-01T00:00:00Z",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	r := NewRouter()
	rc := r.handleRoute([]string{"--apply", "design the architecture"})
	if rc != 0 {
		t.Fatalf("handleRoute returned %d, want 0 (hook contract: never block)", rc)
	}

	got, err := mpminternal.LoadActiveJSON()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.Persona != "venkat" {
		t.Errorf("Persona = %q, want \"venkat\" (apply should have written route persona)", got.Persona)
	}
	if !equalStringSlices(got.Modes, []string{"architect"}) {
		t.Errorf("Modes = %v, want [architect]", got.Modes)
	}
	if got.Updated == "2026-01-01T00:00:00Z" {
		t.Errorf("Updated = %q, want new timestamp", got.Updated)
	}
}
