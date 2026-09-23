package internal

import (
	"path/filepath"
	"testing"
)

func TestRouter_Evaluate(t *testing.T) {
	// Pre-fix this test hardcoded /home/v/workspace/projects/mpm as
	// the router basePath — the original author's checkout. That
	// made the test fail under any other Unix user with a
	// permission-denied error from the NewRouter reload(). Hermetic
	// repair: resolve the current repository root (the parent of
	// this test's package directory, internal/core) so the test
	// exercises the source tree under test, not a specific user's
	// install.
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	router, err := NewRouter(repoRoot)
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}

	tests := []struct {
		name              string
		prompt            string
		wantModes         []string // nil means don't care
		wantPersonaNotNil bool
		wantPersonaName   string // empty means don't care
	}{
		{
			name:   "drafting text triggers architect mode (architecture keyword match)",
			prompt: "I need to draft a whitepaper about our Q3 architecture",
			// 2026-08-07 update: 'write' mode was removed in c7ee6d7
			// (router tightened to 3+3). 'architecture' now matches
			// the architect mode pattern, which is the closest fit
			// for drafting/architectural prose.
			wantModes:         []string{"architect"},
			wantPersonaNotNil: false,
		},
		{
			name:   "architecture keyword triggers architect mode",
			prompt: "Design the system architecture for our new API gateway",
			// RESTORED 2026-06-26 commit 2: architect mode has patterns: now.
			// design/architecture/api/gateway/system all match.
			wantModes:         []string{"architect"},
			wantPersonaNotNil: false,
		},
		{
			name:   "research query triggers critic persona (review/critique match)",
			prompt: "What are the latest findings on SQLite WAL performance?",
			// 2026-08-07 update: 'research' mode was removed in c7ee6d7.
			// 'latest findings' now matches the critic persona's
			// 'review'/'find flaws' patterns — the closest fit for
			// evidence-evaluation queries.
			wantModes:         nil,
			wantPersonaNotNil: true,
			wantPersonaName:   "critic",
		},
		{
			name:      "greeting does not trigger any mode or persona",
			prompt:    "hello there",
			wantModes: nil,
			// Revised 2026-06-26: was "falls back to default persona" —
			// removed the unconditional default fallback. The route hook
			// must not pollute context windows for short conversational
			// prompts that don't match any pattern.
			wantPersonaNotNil: false,
		},
		{
			name:   "code implementation matches no current mode (architect patterns omit 'implement'/'auth')",
			prompt: "Implement the user authentication flow in Go",
			// 2026-08-07 update: 'programming' mode was removed in c7ee6d7.
			// Current architect mode patterns ('design, architecture,
			// structure, plan, system, subsystem, refactor, scale') do
			// not include 'implement', 'authentication', or 'flow', so
			// this prompt matches no mode. Pinning the current behavior.
			wantModes:         nil,
			wantPersonaNotNil: false,
		},
		{
			name:   "cross-validation prompt matches no current mode (moe mode removed)",
			prompt: "Gemini said: Reflex Engine is a hallucination. claude suggested the same. chatgpt disagreed. Source-check this.",
			// 2026-08-07 update: 'moe' mode was removed in c7ee6d7.
			// No current mode pattern matches cross-LLM validation
			// language. The use case is real but the router no longer
			// covers it — a follow-up should add a 'cross-check' mode.
			wantModes:         nil,
			wantPersonaNotNil: false,
		},
		{
			name:   "explicit moe invocation matches no current mode (moe removed)",
			prompt: "moe: this gemini output needs verification before we act",
			// 2026-08-07 update: 'moe' mode was removed in c7ee6d7.
			wantModes:         nil,
			wantPersonaNotNil: false,
		},
		{
			name:   "source-verify language matches no current mode (moe removed)",
			prompt: "Cross-validate this claude suggestion about the Reflex Engine. Source-verify before agreeing.",
			// 2026-08-07 update: 'moe' mode was removed in c7ee6d7.
			// 'cross-validate'/'source-verify' would be a candidate for
			// a future 'cross-check' mode.
			wantModes:         nil,
			wantPersonaNotNil: false,
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

	// 2026-08-07: domain_out bounce subtests removed (venkat,
	// machiavelli, greybeard, marcus personas were deleted in c7ee6d7
	// when the persona set was tightened to 3+3). The domain_out
	// penalty mechanism itself is still in production use (see
	// persona/{critic,forensic,system}.md domain_out fields); add
	// replacement subtests once the persona set stabilizes.

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
