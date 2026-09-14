package main

import (
	"errors"
	"testing"

	mpminternal "github.com/flowbyte-com/mpm-core"
)

// TestDoctorService_checkEmbeddings tests the four-state embedding data check.
func TestDoctorService_checkEmbeddings(t *testing.T) {
	dm := newTestDMForCmd(t)
	svc := NewDoctorService(dm)

	// Install a test config override. We use Absent so the provider check
	// doesn't interfere with the embeddings-data check.
	prev := mpminternal.SetEmbedConfigForTest(&mpminternal.EmbeddingConfig{
		Source: mpminternal.EmbeddingSourceAbsent,
	})
	defer mpminternal.SetEmbedConfigForTest(prev)

	// Empty DB — no memories yet. Per spec §7.1 table, zero memories
	// with no hash/null counts hits the default (PASS) case.
	check := svc.checkEmbeddings()
	if check.Status != "PASS" {
		t.Fatalf("empty DB: got status %q, want PASS (msg: %q)", check.Status, check.Message)
	}
	// Final release-pass wording: label is "Stored embeddings" so the
	// historical-vs-current distinction is explicit in the rendered
	// output. Message stays in passive voice.
	if check.Name != "Stored embeddings" {
		t.Fatalf("got label %q, want 'Stored embeddings'", check.Name)
	}
	if check.Message != "0 memories, all provider-generated" {
		t.Fatalf("empty DB: got message %q, want '0 memories, all provider-generated'", check.Message)
	}
}

// TestDoctorService_checkEmbeddingProvider tests the four-state provider check.
func TestDoctorService_checkEmbeddingProvider(t *testing.T) {
	dm := newTestDMForCmd(t)
	svc := NewDoctorService(dm)

	tests := []struct {
		name       string
		cfg        *mpminternal.EmbeddingConfig
		wantStatus string
		wantMsg    string
	}{
		{
			name: "intentionally disabled",
			cfg: &mpminternal.EmbeddingConfig{
				Source:                mpminternal.EmbeddingSourceDisabled,
				ProviderName:          "null",
				IntentionallyDisabled: true,
			},
			wantStatus: "PASS",
			wantMsg:    "intentionally disabled",
		},
		{
			name: "absent — no provider configured",
			cfg: &mpminternal.EmbeddingConfig{
				Source:       mpminternal.EmbeddingSourceAbsent,
				ProviderName: "null",
				Status:       mpminternal.EmbeddingStatusNull,
			},
			// 2026-09-14 release-pass: embedding is OPTIONAL.
			// Absence alone is the neutral "INFO" status — neither
			// a successful check (PASS) nor a failure (WARN/FAIL).
			// Only configured-but-broken states remain WARN.
			wantStatus: "INFO",
			wantMsg:    "not configured · optional",
		},
		{
			name: "profile configured and reachable",
			cfg: &mpminternal.EmbeddingConfig{
				Source:       mpminternal.EmbeddingSourceProfile,
				ProfileName:  "local-ollama",
				ProviderName: "ollama:nomic-embed-text",
				Status:       mpminternal.EmbeddingStatusConfigured,
			},
			// 2026-09-14 release-pass: presentation reformats
			// the canonical `<host>:<model>` wire shape into the
			// human-facing `<model> · <Provider>` form. The
			// underlying wire shape is unchanged.
			wantStatus: "PASS",
			wantMsg:    `nomic-embed-text · Ollama`,
		},
		{
			name: "profile configured — unreachable",
			cfg: &mpminternal.EmbeddingConfig{
				Source:       mpminternal.EmbeddingSourceProfile,
				ProfileName:  "local-ollama",
				ProviderName: "ollama:nomic-embed-text",
				Status:       mpminternal.EmbeddingStatusUnreachable,
				LastError:    errors.New("connection refused"),
			},
			wantStatus: "WARN",
			wantMsg:    `nomic-embed-text · Ollama unreachable: connection refused`,
		},
		{
			name: "profile configured — misconfigured",
			cfg: &mpminternal.EmbeddingConfig{
				Source:       mpminternal.EmbeddingSourceProfile,
				ProfileName:  "broken",
				ProviderName: "null",
				Status:       mpminternal.EmbeddingStatusMisconfigured,
				LastError:    errors.New("profile \"broken\" referenced by components.embedding does not exist"),
			},
			// Misconfigured case keeps the original phrasing
			// (`misconfigured: <reason>`) — the model/provider
			// reformat only applies when both are known.
			wantStatus: "WARN",
			wantMsg:    `misconfigured: profile "broken" referenced by components.embedding does not exist`,
		},
		{
			name: "env fallback — legacy",
			cfg: &mpminternal.EmbeddingConfig{
				Source:       mpminternal.EmbeddingSourceEnvFallback,
				ProviderName: "ollama:nomic-embed-text",
				Status:       mpminternal.EmbeddingStatusConfigured,
			},
			wantStatus: "PASS",
			wantMsg:    `nomic-embed-text · Ollama (legacy env fallback)`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prev := mpminternal.SetEmbedConfigForTest(tt.cfg)
			defer mpminternal.SetEmbedConfigForTest(prev)

			check := svc.checkEmbeddingProvider()
			if check.Status != tt.wantStatus {
				t.Errorf("status = %q, want %q (msg: %q)", check.Status, tt.wantStatus, check.Message)
			}
			if check.Message != tt.wantMsg {
				t.Errorf("message = %q, want %q", check.Message, tt.wantMsg)
			}
		})
	}
}

// TestDoctorService_Check_aggregatesEmbeddingProvider verifies that
// checkEmbeddingProvider is registered in the Check() aggregator.
func TestDoctorService_Check_aggregatesEmbeddingProvider(t *testing.T) {
	dm := newTestDMForCmd(t)
	svc := NewDoctorService(dm)

	prev := mpminternal.SetEmbedConfigForTest(&mpminternal.EmbeddingConfig{
		Source:                mpminternal.EmbeddingSourceDisabled,
		IntentionallyDisabled: true,
	})
	defer mpminternal.SetEmbedConfigForTest(prev)

	report, err := svc.Check()
	if err != nil {
		t.Fatalf("Check() returned error: %v", err)
	}

	// Find the Embedding model check. 2026-09-14 release-pass
	// renamed the row from "Embedding provider" → "Embedding model"
	// to match the canonical terminology across all surfaces.
	var found bool
	for _, c := range report.Checks {
		if c.Name == "Embedding model" {
			found = true
			if c.Status != "PASS" {
				t.Errorf("Embedding model check status = %q, want PASS", c.Status)
			}
			if c.Message != "intentionally disabled" {
				t.Errorf("Embedding model message = %q, want 'intentionally disabled'", c.Message)
			}
		}
	}
	if !found {
		t.Error("Embedding model check not found in report.Checks")
	}
}
