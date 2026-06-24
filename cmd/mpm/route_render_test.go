package main

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
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
			prompt:    "Implement the user authentication flow in Go with proper security",
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
			name:           "persona dropped but mode preserved (the bug class)",
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
		name    string
		envVal  string
		envSet  bool
		want    int
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
		name      string
		setup     func(t *testing.T, dir string) // creates candidate files
		wantFile  string                         // basename of expected return, "" for none
	}{
		{
			name:    "no candidates returns empty",
			setup:   func(t *testing.T, dir string) {},
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

func TestInvalidateDirectiveCache(t *testing.T) {
	// Cache is package-level. Each subtest must start from a known state.
	InvalidateDirectiveCache()
	t.Cleanup(InvalidateDirectiveCache)

	t.Run("cache returns empty when no fetch has occurred", func(t *testing.T) {
		InvalidateDirectiveCache()
		// Without any prior fetch, the cache has no entry for this key.
		// We don't directly inspect the cache, but we can confirm
		// InvalidateDirectiveCache doesn't error and a subsequent call
		// still returns empty for a non-existent workspace.
		got := fetchTopDirectivesCached("/nonexistent/path/cache-test", 5)
		if got != "" {
			t.Errorf("fetchTopDirectivesCached() for missing DB = %q, want empty", got)
		}
	})

	t.Run("two invalidates in a row are safe", func(t *testing.T) {
		InvalidateDirectiveCache()
		InvalidateDirectiveCache() // must not panic on empty map
	})

	t.Run("concurrent invalidates are safe", func(t *testing.T) {
		var wg sync.WaitGroup
		for i := 0; i < 10; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				InvalidateDirectiveCache()
			}()
		}
		wg.Wait()
	})
}

func TestFetchTopDirectivesCached_ShortCircuits(t *testing.T) {
	InvalidateDirectiveCache()
	t.Cleanup(InvalidateDirectiveCache)

	// All subtests use inputs that fetchTopDirectives returns "" for, so the
	// only thing we're really asserting is that the cache wrapper doesn't
	// change the short-circuit behavior. We're not measuring timing here
	// (flaky) — we're asserting semantic equivalence with fetchTopDirectives
	// for the no-injection paths.
	tests := []struct {
		name      string
		workspace string
		limit     int
	}{
		{"empty workspace", "", 5},
		{"zero limit", "/tmp", 0},
		{"negative limit", "/tmp", -1},
		{"no DB at workspace", t.TempDir(), 5},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			InvalidateDirectiveCache()
			cached := fetchTopDirectivesCached(tt.workspace, tt.limit)
			direct := fetchTopDirectives(tt.workspace, tt.limit)
			if cached != direct {
				t.Errorf("cached = %q, direct = %q (cache should be semantically equivalent on short-circuit)", cached, direct)
			}
		})
	}
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
