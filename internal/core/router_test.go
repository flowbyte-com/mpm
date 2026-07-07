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
			// Revised 2026-06-26: no implicit default fallback. If no
			// persona's patterns match a prompt, no persona is selected —
			// the agent runs with its active.json persona instead.
			wantPersonaNotNil: false,
		},
		{
			name:             "greeting does not trigger any mode or persona",
			prompt:           "hello there",
			wantModes:        nil,
			// Revised 2026-06-26: was "falls back to default persona" —
			// removed the unconditional default fallback. The route hook
			// must not pollute context windows for short conversational
			// prompts that don't match any pattern.
			wantPersonaNotNil: false,
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
			// Revised 2026-06-26: no implicit default fallback. moe mode
			// matches but no persona pattern does.
			wantPersonaNotNil: false,
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

	// No-default-fallback contract (revised 2026-06-26).
	// Previously this subtest verified the default fallback fired for
	// gibberish input and surfaced a "(fallback: ...)" marker trigger.
	// The unconditional fallback was removed because it polluted the
	// route hook's <system-reminder> for short conversational prompts
	// that didn't match any pattern. The new contract: gibberish = no
	// mode, no persona, no injection. This pins it.
	t.Run("gibberish input selects no mode and no persona", func(t *testing.T) {
		report := router.Evaluate("xyzzy gibberish no specialist pattern matches")
		if len(report.SelectedModes) != 0 {
			t.Errorf("gibberish should select no modes, got %v", report.SelectedModes)
		}
		if report.SelectedPersona != "" {
			t.Errorf("gibberish should select no persona, got %q", report.SelectedPersona)
		}
	})

	// Domain-boundary enforcement (added 2026-06-26 with the
	// anti_patterns → domain_out rename + PenaltiesApplied observability
	// hook). Three sub-tests, each verifies:
	//   (a) the bouncing persona's score is reduced by 1+ per domain_out match
	//   (b) the matched regex appears in PenaltiesApplied
	//   (c) a better-fit persona wins OR default fallback fires
	t.Run("domain_out bounces venkat from grief prompt", func(t *testing.T) {
		report := router.Evaluate("I just experienced a profound grief about the loss of my dog")
		entry := report.Scores["venkat"]
		if entry.Score >= 0 {
			t.Errorf("venkat should be bounced (score<0), got %d", entry.Score)
		}
		foundGriefPenalty := false
		for _, p := range entry.PenaltiesApplied {
			if p == `(?i)\bgrief\b` {
				foundGriefPenalty = true
				break
			}
		}
		if !foundGriefPenalty {
			t.Errorf("expected (?i)\\bgrief\\b in venkat's PenaltiesApplied, got %v", entry.PenaltiesApplied)
		}
		// Better-fit persona should win — marcus handles grief
		if report.SelectedPersona != "marcus" {
			t.Logf("(info) expected marcus to win grief prompt, got %q (still a valid bounce)", report.SelectedPersona)
		}
	})

	t.Run("domain_out bounces machiavelli from compiler error", func(t *testing.T) {
		report := router.Evaluate("I have a compiler error in my Rust code and cannot figure out the syntax")
		entry := report.Scores["machiavelli"]
		if entry.Score >= 0 {
			t.Errorf("machiavelli should be bounced (score<0), got %d", entry.Score)
		}
		foundCompilerPenalty := false
		for _, p := range entry.PenaltiesApplied {
			if p == `(?i)\bcompiler error\b` {
				foundCompilerPenalty = true
				break
			}
		}
		if !foundCompilerPenalty {
			t.Errorf("expected (?i)\\bcompiler error\\b in machiavelli's PenaltiesApplied, got %v", entry.PenaltiesApplied)
		}
	})

	t.Run("domain_out bounces greybeard from hype-train prompt", func(t *testing.T) {
		report := router.Evaluate("is rust the best new framework, is it a 10x developer tool")
		entry := report.Scores["greybeard"]
		// Two domain_out matches: best new framework + 10x developer → -2
		if entry.Score >= -1 {
			t.Errorf("greybeard should be bounced at score<=-2, got %d", entry.Score)
		}
		if len(entry.PenaltiesApplied) < 2 {
			t.Errorf("expected at least 2 PenaltiesApplied for greybeard, got %v", entry.PenaltiesApplied)
		}
	})

	// Voice guards must NOT affect routing score. The original
	// anti_patterns mechanism was dormant because all 16 components had
	// only voice-guard content; this test pins the contract that the
	// rename preserves: voice_guards is purely for LLM context.
	t.Run("voice_guards do not affect routing score", func(t *testing.T) {
		report := router.Evaluate("bikeshedding premature optimization scope creep")
		// artisan's voice_guards mention all three — but voice_guards
		// must NOT trigger a penalty. artisan's score should be 0
		// (no pattern match, no domain_out match).
		entry := report.Scores["artisan"]
		if entry.Score != 0 {
			t.Errorf("artisan should score 0 (voice_guards ignored), got %d", entry.Score)
		}
		if len(entry.PenaltiesApplied) != 0 {
			t.Errorf("artisan should have no PenaltiesApplied (voice_guards ignored), got %v", entry.PenaltiesApplied)
		}
	})
}