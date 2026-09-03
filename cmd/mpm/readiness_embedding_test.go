package main

import (
	"errors"
	"testing"

	mpminternal "github.com/flowbyte-com/mpm-core"
)

// TestReadinessChecker_checkEmbeddings tests the four-state embedding readiness check.
func TestReadinessChecker_checkEmbeddings(t *testing.T) {
	tests := []struct {
		name      string
		cfg       *mpminternal.EmbeddingConfig
		wantOK    bool
		wantNote  string
	}{
		{
			name: "intentionally disabled",
			cfg: &mpminternal.EmbeddingConfig{
				Source:                mpminternal.EmbeddingSourceDisabled,
				ProviderName:          "null",
				IntentionallyDisabled: true,
			},
			wantOK:   true,
			wantNote: "embedding intentionally disabled",
		},
		{
			name: "configured and reachable",
			cfg: &mpminternal.EmbeddingConfig{
				Source:       mpminternal.EmbeddingSourceProfile,
				ProfileName:  "local-ollama",
				ProviderName: "ollama:nomic-embed-text",
				Status:       mpminternal.EmbeddingStatusConfigured,
			},
			wantOK:   true,
			wantNote: "embedding provider reachable",
		},
		{
			name: "configured but unreachable",
			cfg: &mpminternal.EmbeddingConfig{
				Source:       mpminternal.EmbeddingSourceProfile,
				ProfileName:  "local-ollama",
				ProviderName: "ollama:nomic-embed-text",
				Status:       mpminternal.EmbeddingStatusUnreachable,
				LastError:    errors.New("connection refused"),
			},
			wantOK:   false,
			wantNote: "embedding provider configured but unreachable",
		},
		{
			name: "misconfigured",
			cfg: &mpminternal.EmbeddingConfig{
				Source:       mpminternal.EmbeddingSourceProfile,
				ProfileName:  "broken",
				ProviderName: "null",
				Status:       mpminternal.EmbeddingStatusMisconfigured,
				LastError:    errors.New("profile \"broken\" does not exist"),
			},
			wantOK:   false,
			wantNote: "embedding provider misconfigured: profile \"broken\" does not exist",
		},
		{
			name: "absent — no provider configured",
			cfg: &mpminternal.EmbeddingConfig{
				Source:       mpminternal.EmbeddingSourceAbsent,
				ProviderName: "null",
				Status:       mpminternal.EmbeddingStatusNull,
			},
			wantOK:   false, // spec §7.2: absent is WARN; ReadinessItem has no tri-state, so WARN maps to OK=false
			wantNote: "no embedding provider configured",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prev := mpminternal.SetEmbedConfigForTest(tt.cfg)
			defer mpminternal.SetEmbedConfigForTest(prev)

			item := checkEmbeddings()
			if item.OK != tt.wantOK {
				t.Errorf("OK = %v, want %v (note: %q)", item.OK, tt.wantOK, item.Detail)
			}
			if item.Detail != tt.wantNote {
				t.Errorf("Detail = %q, want %q", item.Detail, tt.wantNote)
			}
		})
	}
}
