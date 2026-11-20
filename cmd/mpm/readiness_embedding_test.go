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
			wantNote: "intentionally disabled",
		},
		{
			name: "configured and reachable",
			cfg: &mpminternal.EmbeddingConfig{
				Source:       mpminternal.EmbeddingSourceProfile,
				ProfileName:  "local-ollama",
				ProviderName: "ollama:nomic-embed-text",
				Status:       mpminternal.EmbeddingStatusConfigured,
			},
			// 2026-09-14 release-pass: presentation reformats
			// `<host>:<model>` wire into `<model> · <Provider>`.
			wantOK:   true,
			wantNote: "nomic-embed-text · Ollama",
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
			// 2026-09-14 release-pass: unreachable detail uses the
			// same `<model> · <Provider>` reformat.
			wantOK:   false,
			wantNote: "nomic-embed-text · Ollama · unreachable",
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
			wantNote: "misconfigured: profile \"broken\" does not exist",
		},
		{
			name: "absent — no provider configured",
			cfg: &mpminternal.EmbeddingConfig{
				Source:       mpminternal.EmbeddingSourceAbsent,
				ProviderName: "null",
				Status:       mpminternal.EmbeddingStatusNull,
			},
			// 2026-09-14 release-pass: embedding is optional;
			// absence is informational (OK=true) and the row reads
			// "not configured · optional".
			wantOK:   true,
			wantNote: "not configured · optional",
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
