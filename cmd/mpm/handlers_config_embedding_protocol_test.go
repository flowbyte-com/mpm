// handlers_config_embedding_protocol_test.go — HAL-9001 Finding 2.
//
// The embedding wizard offers the operator a PROTOCOL, not a vendor.
// Every protocol it offers must map onto a provider the runtime's
// embedding resolver implements, and the resulting configuration must
// be executable by that backend.
//
// This file pins the mapping itself, for every protocol the picker
// offers, through the runtime's real resolver.
//
// The public interface stays vendor-neutral: these assertions are
// about the translation from the operator's choice to the provider
// the resolver understands, not about adding vendor names to the
// menu.
package main

import (
	"testing"

	mpminternal "github.com/flowbyte-com/mpm-core"
	"github.com/flowbyte-com/mpm-core/config"
)

// TestEmbeddingProtocolChoices_EveryProtocolResolvesThroughTheRuntime
// pins that each protocol the picker offers produces a profile the
// runtime can execute.
//
// A protocol with no implemented provider behind it is a trap: the
// wizard accepts the operator's answers, writes a configuration, and
// the failure surfaces later as "provider … is not implemented".
func TestEmbeddingProtocolChoices_EveryProtocolResolvesThroughTheRuntime(t *testing.T) {
	if len(embeddingProtocolChoices) == 0 {
		t.Fatal("the embedding wizard offers no protocols")
	}

	for _, protocol := range embeddingProtocolChoices {
		t.Run(protocol.id, func(t *testing.T) {
			prof := config.Profile{Name: "embedding"}
			mergeProfileDefaults(&prof, protocol.defaults)
			prof.Model = "some-embedding-model"

			if prof.Provider == "" {
				t.Fatalf("protocol %q maps to no runtime provider", protocol.id)
			}
			if prof.Provider == "custom" {
				t.Fatalf("protocol %q maps to provider %q, which the embedding resolver does not implement",
					protocol.id, prof.Provider)
			}

			// The real resolver, on a configuration that binds only
			// this profile.
			res := mpminternal.ResolveEmbeddingConfig(&config.Config{
				Profiles:   map[string]config.Profile{prof.Name: prof},
				Components: map[string]string{"embedding": prof.Name},
			})
			if res.Status != mpminternal.EmbeddingStatusConfigured {
				t.Errorf("protocol %q produced a profile the runtime cannot execute: status %q (error: %v)",
					protocol.id, res.Status, res.LastError)
			}
		})
	}
}

// TestEmbeddingProtocolChoices_CoverTheSupportedBackends pins that the
// picker still offers both runtime backends.
//
// The two supported embedding transports are the OpenAI-compatible
// wire and the Ollama wire. Anthropic-compatible is deliberately
// absent: Anthropic exposes no native /embeddings endpoint, and
// offering it would produce a configuration that cannot run.
func TestEmbeddingProtocolChoices_CoverTheSupportedBackends(t *testing.T) {
	want := map[string]bool{"openai-compatible": false, "ollama": false}
	for _, protocol := range embeddingProtocolChoices {
		if _, ok := want[protocol.id]; ok {
			want[protocol.id] = true
		}
	}
	for id, offered := range want {
		if !offered {
			t.Errorf("the embedding wizard no longer offers the %q protocol", id)
		}
	}
	for _, protocol := range embeddingProtocolChoices {
		if protocol.id == "anthropic-compatible" {
			t.Error("the embedding wizard must not offer the Anthropic-compatible protocol: it has no /embeddings endpoint")
		}
	}
}
