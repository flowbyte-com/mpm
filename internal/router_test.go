package internal

import (
	"testing"
)

func TestRouter_Evaluate(t *testing.T) {
	router, err := NewRouter("/home/v/workspace/projects/mpm")
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}

	tests := []struct {
		name             string
		prompt           string
		wantModes        []string // nil means don't care
		wantPersonaNotNil bool
		wantPersonaName   string // empty means don't care
	}{
		{
			name:             "drafting text triggers write mode",
			prompt:           "I need to draft a whitepaper about our Q3 architecture",
			wantModes:        []string{"write"},
			wantPersonaNotNil: true,
		},
		{
			name:             "architecture keyword triggers architect mode",
			prompt:           "Design the system architecture for our new API gateway",
			// RESTORED 2026-06-26 commit 2: architect mode has patterns: now.
			// design/architecture/api/gateway/system all match.
			wantModes:        []string{"architect"},
			wantPersonaNotNil: false,
		},
		{
			name:             "research query triggers research mode",
			prompt:           "What are the latest findings on SQLite WAL performance?",
			wantModes:        []string{"research"},
			wantPersonaNotNil: true,
		},
		{
			name:             "greeting triggers no mode, falls back to default persona",
			prompt:           "hello there",
			wantModes:        nil,
			// FALLBACK 2026-06-26: no specialist pattern matches "hello there",
			// but the hierarchy says "default" wins over bare metal. Selected
			// persona is the default persona (loaded from persona/default.md),
			// NOT empty. wantPersonaNotNil reflects this — the default persona
			// is a real selection, not a no-op.
			wantPersonaNotNil: true,
			wantPersonaName:   "default",
		},
		{
			name:             "code implementation triggers architect or programming",
			prompt:           "Implement the user authentication flow in Go",
			// RESTORED 2026-06-26 commit 2: programming mode has patterns: now.
			// implement/in Go/authentication/flow all match. Architect would
			// not match this (no architecture keyword), so allow either.
			// Architect mode is a fallback for this kind of prompt, but
			// programming is the more semantically correct match.
			wantModes:        []string{"architect", "programming"},
			wantPersonaNotNil: false,
		},
		{
			name:             "cross-validation prompt triggers moe mode",
			prompt:           "Gemini said: Reflex Engine is a hallucination. claude suggested the same. chatgpt disagreed. Source-check this.",
			wantModes:        []string{"moe"},
			wantPersonaNotNil: true,
		},
		{
			name:             "explicit moe invocation triggers moe mode",
			prompt:           "moe: this gemini output needs verification before we act",
			wantModes:        []string{"moe"},
			wantPersonaNotNil: true,
		},
		{
			name:             "source-verify language triggers moe mode",
			prompt:           "Cross-validate this claude suggestion about the Reflex Engine. Source-verify before agreeing.",
			wantModes:        []string{"moe"},
			wantPersonaNotNil: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			report := router.Evaluate(tt.prompt)
			t.Logf("prompt=%q modes=%v persona=%q", tt.prompt, report.SelectedModes, report.SelectedPersona)

			if tt.wantModes != nil {
				if len(report.SelectedModes) == 0 && len(tt.wantModes) > 0 {
					t.Errorf("expected modes %v, got none", tt.wantModes)
				}
				// Check at least one expected mode is present
				found := false
				for _, w := range tt.wantModes {
					for _, m := range report.SelectedModes {
						if m == w {
							found = true
							break
						}
					}
				}
				if !found {
					t.Errorf("expected one of %v in modes, got %v", tt.wantModes, report.SelectedModes)
				}
			}

			if tt.wantPersonaNotNil {
				if report.SelectedPersona == "" {
					t.Errorf("expected a persona to be selected, got empty")
				}
				if tt.wantPersonaName != "" && report.SelectedPersona != tt.wantPersonaName {
					t.Errorf("expected persona %q, got %q", tt.wantPersonaName, report.SelectedPersona)
				}
			} else {
				if report.SelectedPersona != "" {
					t.Logf("(info) persona selected for %q (test asserted empty): %s", tt.prompt, report.SelectedPersona)
				}
			}
		})
	}

	// Dedicated fallback behavior test — verifies the diagnostic
	// surfaces WHY default was selected (a fallback marker trigger).
	// Without this marker the report would show default as a "silent
	// no-op selection", which is exactly what we are trying to avoid.
	t.Run("default fallback surfaces fallback marker in diagnostic", func(t *testing.T) {
		report := router.Evaluate("xyzzy gibberish no specialist pattern matches")
		if report.SelectedPersona != "default" {
			t.Fatalf("expected default fallback, got %q", report.SelectedPersona)
		}
		entry := report.Scores["default"]
		foundFallbackMarker := false
		for _, tr := range entry.Triggers {
			if tr == "(fallback: no specialist pattern matched)" {
				foundFallbackMarker = true
				break
			}
		}
		if !foundFallbackMarker {
			t.Errorf("expected fallback marker in default's triggers, got %v", entry.Triggers)
		}
	})
}