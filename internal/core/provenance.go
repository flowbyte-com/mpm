// provenance.go — Env-var resolver + type definitions for artifact
// creation provenance. The resolver is the PROCESS-WIDE layer that
// reads env vars once per process. The PER-CALL effective provenance
// is computed by Resolve() with overrides.
//
// Spec: docs/superpowers/specs/2026-08-08-artifact-provenance-design.md
//
// Invariants this file preserves:
//
//   1. Declarative. MPM records what is declared; it does not infer
//      model reasoning characteristics from any other field.
//   2. NULL semantics. Absent env vars, or env vars set to empty
//      strings, are normalized to NULL/empty — never written as a
//      pseudo-value like "unknown" (except for actor_kind, which
//      defaults to "unknown" as a meaningful observation).
//   3. Capture broadly, interpret narrowly. The provider_metadata
//      field is opaque JSON; MPM validates it is a JSON object but
//      does not parse, re-marshal, or beautify its byte content.
//
// The DB-writing half of the feature lives in provenance_db.go.
package internal

import (
	"encoding/json"
	"os"
	"strconv"
	"strings"
)

// CreationProvenance is the wire format: the resolver reads env vars
// once at process start and stores the result on a *ProvenanceResolver.
// Pointer types are used for genuinely optional fields so a missing
// env var is nil, not the empty string.
//
// JSON tags use short names to match the wire format consumed by
// MPM_PROVENANCE: "framework", "model", "thinking_level", etc.
type CreationProvenance struct {
	ActorKind        string  `json:"actor_kind,omitempty"`
	ActorID          string  `json:"actor_id,omitempty"`
	FrameworkName    string  `json:"framework,omitempty"`
	FrameworkVersion string  `json:"framework_version,omitempty"`
	FrameworkAdapter string  `json:"framework_adapter,omitempty"`
	ProviderName     string  `json:"provider,omitempty"`
	ModelName        string  `json:"model,omitempty"`
	ModelRevision    string  `json:"model_revision,omitempty"`
	APIEndpoint      string  `json:"api_endpoint,omitempty"`
	Temperature      *float64 `json:"temperature,omitempty"`
	MaxTokens        *int    `json:"max_tokens,omitempty"`
	ReasoningMode    string  `json:"reasoning_mode,omitempty"`
	ReasoningEffort  *float64 `json:"reasoning_effort,omitempty"`
	ThinkingLevel    *string `json:"thinking_level,omitempty"`
	ThinkingTokens   *int64  `json:"thinking_tokens,omitempty"`
	ThinkingVisible  *bool   `json:"thinking_visible,omitempty"`
	SessionID        string  `json:"session_id,omitempty"`
	ProviderMetadata string  `json:"provider_metadata,omitempty"`
}

// EffectiveProvenance is the DB-ready form. It's the result of
// Resolve() — base fields plus per-call overrides. The DB writer
// reads from this struct, not from creation env.
type EffectiveProvenance struct {
	ActorKind        string
	ActorID          string
	FrameworkName    string
	FrameworkVersion string
	FrameworkAdapter string
	ProviderName     string
	ModelName        string
	ModelRevision    string
	APIEndpoint      string
	Temperature      *float64
	MaxTokens        *int
	ReasoningMode    string
	ReasoningEffort  *float64
	ThinkingLevel    *string
	ThinkingTokens   *int64
	ThinkingVisible  *bool
	SessionID        string
	InvocationID     string
	ParentArtifactID string
	// ParentInvocationID traces agent-of-agent invocation causality.
	// Distinct from ParentArtifactID: that column says "this artifact
	// was derived from that artifact"; this column says "this artifact
	// was produced inside an invocation spawned by that invocation".
	// Both nullable; together they power the future telemetry binary's
	// invocation-tree reconstruction (`WHERE parent_invocation_id = ?`).
	ParentInvocationID string
	ProviderMetadata   string
}

// ProvenanceResolver holds the process-wide base provenance. Construct
// once via NewFromEnv() and mount on DatabaseManager. Tests construct
// directly via &ProvenanceResolver{base: ...} and restore in t.Cleanup.
type ProvenanceResolver struct {
	base *CreationProvenance
}

// NewFromEnv reads the MPM_PROVENANCE* env vars and returns a resolver.
//
// Priority chain:
//  1. MPM_PROVENANCE (JSON blob). If absent or malformed, log and fall through.
//  2. MPM_PROVENANCE_* flat env vars.
//  3. MPM_SESSION_ID, MPM_AGENT_ID.
//  4. Defaults: actor_kind="unknown", everything else empty (NULL).
func NewFromEnv() *ProvenanceResolver {
	return &ProvenanceResolver{base: readFromEnv()}
}

// NewFromStatic returns a resolver with an explicit base. Tests use
// this to inject a deterministic provenance without env mutation.
func NewFromStatic(base *CreationProvenance) *ProvenanceResolver {
	return &ProvenanceResolver{base: base}
}

// Base returns the process-wide base provenance. Useful for CLI
// commands that want to show the user what they are sending.
func (r *ProvenanceResolver) Base() *CreationProvenance {
	return r.base
}

// Resolve produces the effective provenance for one artifact write.
// sessionID/invocationID/parentArtifactID are per-call overrides:
//   - sessionID: per-call ambient session (overrides MPM_SESSION_ID)
//   - invocationID: correlation identity — one model/agent turn
//   - parentArtifactID: causal chain (e.g., cascade-derived theory)
//
// For mpm call (process-scoped), the per-call args are usually empty
// and the base is the effective provenance. For long-lived MCP
// servers, the base is process-wide but the effective provenance is
// per-call.
//
// parentInvocationID is the per-call pointer to the spawning
// invocation. Agents that spawn sub-invocations (Hermes → Claude
// Code → memory) set this so the resulting artifact's provenance row
// can be walked back to its origin via the invocation tree. Nullable;
// defaults to empty string when the current invocation has no parent.
func (r *ProvenanceResolver) Resolve(
	sessionID, invocationID, parentArtifactID, parentInvocationID string,
) *EffectiveProvenance {
	if r == nil || r.base == nil {
		return &EffectiveProvenance{ActorKind: "unknown"}
	}
	return &EffectiveProvenance{
		ActorKind:          r.base.ActorKind,
		ActorID:            r.base.ActorID,
		FrameworkName:      r.base.FrameworkName,
		FrameworkVersion:   r.base.FrameworkVersion,
		FrameworkAdapter:   r.base.FrameworkAdapter,
		ProviderName:       r.base.ProviderName,
		ModelName:          r.base.ModelName,
		ModelRevision:      r.base.ModelRevision,
		APIEndpoint:        r.base.APIEndpoint,
		Temperature:        r.base.Temperature,
		MaxTokens:          r.base.MaxTokens,
		ReasoningMode:      r.base.ReasoningMode,
		ReasoningEffort:    r.base.ReasoningEffort,
		ThinkingLevel:      r.base.ThinkingLevel,
		ThinkingTokens:     r.base.ThinkingTokens,
		ThinkingVisible:    r.base.ThinkingVisible,
		SessionID:          pickFirst(sessionID, r.base.SessionID),
		InvocationID:       invocationID,
		ParentArtifactID:   parentArtifactID,
		ParentInvocationID: parentInvocationID,
		ProviderMetadata:   r.base.ProviderMetadata,
	}
}

func pickFirst(override, base string) string {
	if override != "" {
		return override
	}
	return base
}

func readFromEnv() *CreationProvenance {
	if jsonStr := os.Getenv("MPM_PROVENANCE"); jsonStr != "" {
		var c CreationProvenance
		if err := json.Unmarshal([]byte(jsonStr), &c); err == nil {
			// JSON wins. Apply defaults to fields the caller omitted.
			applyDefaults(&c)
			return &c
		}
		// Malformed JSON: fall through to flat vars.
	}

	c := &CreationProvenance{}
	c.FrameworkName = os.Getenv("MPM_PROVENANCE_FRAMEWORK")
	c.FrameworkVersion = os.Getenv("MPM_PROVENANCE_VERSION")
	c.FrameworkAdapter = os.Getenv("MPM_PROVENANCE_ADAPTER")
	c.ProviderName = os.Getenv("MPM_PROVENANCE_PROVIDER")
	c.ModelName = os.Getenv("MPM_PROVENANCE_MODEL")
	c.ModelRevision = os.Getenv("MPM_PROVENANCE_REVISION")
	c.APIEndpoint = os.Getenv("MPM_PROVENANCE_API")
	c.ReasoningMode = os.Getenv("MPM_PROVENANCE_REASONING_MODE")

	if v := os.Getenv("MPM_PROVENANCE_TEMPERATURE"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			c.Temperature = &f
		}
	}
	if v := os.Getenv("MPM_PROVENANCE_MAX_TOKENS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.MaxTokens = &n
		}
	}
	if v := os.Getenv("MPM_PROVENANCE_REASONING_EFFORT"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			c.ReasoningEffort = &f
		}
	}
	if v := os.Getenv("MPM_PROVENANCE_THINKING_LEVEL"); v != "" {
		s := v
		c.ThinkingLevel = &s
	}
	if v := os.Getenv("MPM_PROVENANCE_THINKING_TOKENS"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			c.ThinkingTokens = &n
		}
	}
	if v := os.Getenv("MPM_PROVENANCE_THINKING_VISIBLE"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			c.ThinkingVisible = &b
		}
	}
	c.ProviderMetadata = os.Getenv("MPM_PROVENANCE_METADATA")

	if v := os.Getenv("MPM_PROVENANCE_ACTOR"); v != "" {
		c.ActorKind = v
	}
	if v := os.Getenv("MPM_PROVENANCE_ACTOR_ID"); v != "" {
		c.ActorID = v
	}

	// MPM_SESSION_ID / MPM_AGENT_ID contribute only when not set by
	// MPM_PROVENANCE_*. They are not part of the new contract; they are
	// honored for backward compatibility with existing tooling.
	if c.SessionID == "" {
		c.SessionID = os.Getenv("MPM_SESSION_ID")
	}
	if c.ActorID == "" {
		c.ActorID = os.Getenv("MPM_AGENT_ID")
	}

	applyDefaults(c)
	return c
}

func applyDefaults(c *CreationProvenance) {
	if strings.TrimSpace(c.ActorKind) == "" {
		c.ActorKind = "unknown"
	}
	// All other fields default to empty/NULL. MPM does not infer
	// values from other fields.
}
