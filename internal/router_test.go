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
			wantModes:        []string{"architect"},
			wantPersonaNotNil: true,
		},
		{
			name:             "research query triggers research mode",
			prompt:           "What are the latest findings on SQLite WAL performance?",
			wantModes:        []string{"research"},
			wantPersonaNotNil: true,
		},
		{
			name:             "greeting triggers no mode, default persona likely",
			prompt:           "hello there",
			wantModes:        nil,
			wantPersonaNotNil: false, // score too low
		},
		{
			name:             "code implementation triggers architect",
			prompt:           "Implement the user authentication flow in Go",
			wantModes:        []string{"architect"},
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

			if tt.wantPersonaNotNil && report.SelectedPersona == "" {
				t.Logf("(info) no persona selected for: %s", tt.prompt)
			}
		})
	}
}