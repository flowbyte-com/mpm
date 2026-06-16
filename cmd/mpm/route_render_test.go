package main

import (
	"os"
	"strings"
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
	shortMarker := "[...truncated, see mode/<name>.md for full content]"
	longMarker := "[...truncated]"

	tests := []struct {
		name        string
		modeText    string
		personaText string
		wantMarker  string // "" = expect no marker
		wantMode    string // expected substring in mode position
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
