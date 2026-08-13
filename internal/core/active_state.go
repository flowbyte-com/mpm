// active_state.go — single canonical owner of the agent's runtime state.
//
// Before this file existed, the same struct (active.json shape) and
// the same read/write logic lived in three places:
//   - internal/xitl.go: activeState struct, loadActiveJSON, saveActiveJSON,
//     CheckAutoActive, IsAutoActive, AutoStatus
//   - cmd/mpm/simple_cmds.go: a *second* activeState struct + saveActiveJSON
//     in the main package
//   - cmd/mpm/handlers.go: yet more callers reading active.json
//
// The duplicate code drifted (different field tags, different JSON keys)
// and a bug surfaced where the internal reader expected `updated_at`
// but the main-package writer wrote `timestamp` (fixed in commit
// 06a0681 but it was the symptom, not the cause).
//
// This file is now the single source of truth. Both packages call
// Load/Check/Save here. The main-package copy of saveActiveJSON was
// deleted; the main-package active.json writer now calls SaveActiveJSON
// from this file.
package internal

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/flowbyte-com/mpm-core/config"
)

// ── Runtime active state (the on-disk active.json file) ────────────────────

// ActiveState mirrors the active.json schema on disk. Owned by
// cmd/mpm/mode and cmd/mpm/persona subcommands; read at session start
// (mpm wake) and on every route hook invocation.
type ActiveState struct {
	Persona string   `json:"persona"`
	Modes   []string `json:"modes"`
	Updated string   `json:"updated"`
}

// ActiveJSONPath returns the absolute path to the active.json file.
// Always rooted at config.GetMPMDir() — the same directory the rest of
// MPM uses for runtime state.
func ActiveJSONPath() string {
	return filepath.Join(config.GetMPMDir(), "active.json")
}

// LoadActiveJSON reads the active.json file and returns the parsed state.
// Missing file is NOT an error — a fresh install has no active.json, and
// callers should treat that as "no persona, no modes". Other read errors
// (corrupt JSON, permission denied) propagate so the operator can fix.
func LoadActiveJSON() (*ActiveState, error) {
	data, err := os.ReadFile(ActiveJSONPath())
	if err != nil {
		if os.IsNotExist(err) {
			return &ActiveState{}, nil
		}
		return nil, fmt.Errorf("read active.json: %w", err)
	}
	var s ActiveState
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("parse active.json: %w", err)
	}
	return &s, nil
}

// SaveActiveJSON writes the state to active.json with 0644 perms. JSON
// output is indented for human readability (operators debug this file
// by hand). The function is the single canonical writer; callers in
// cmd/mpm route through this method, not through any local copy.
func SaveActiveJSON(s *ActiveState) error {
	if s == nil {
		return fmt.Errorf("save active.json: nil state")
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal active.json: %w", err)
	}
	return os.WriteFile(ActiveJSONPath(), data, 0600)
}

// ── Auto-mode detection (the "is the agent in auto-mode?" gate) ──────────────

// AutoStatus holds both the boolean gate check and the reason for denial.
type AutoStatus struct {
	Active bool
	Reason string // empty when Active is true; describes which field blocks when false
}

// CheckAutoActive reads active.json and returns whether auto is engaged.
// When auto is not active, Reason describes the exact state (e.g.
// "mode=\"default\", persona=\"default\"") so the caller can relay
// diagnostics to the user.
func CheckAutoActive() AutoStatus {
	s, err := LoadActiveJSON()
	if err != nil {
		return AutoStatus{Active: false, Reason: fmt.Sprintf("cannot read active.json: %v", err)}
	}
	if s.Persona == "auto" {
		return AutoStatus{Active: true, Reason: ""}
	}
	for _, m := range s.Modes {
		if m == "auto" {
			return AutoStatus{Active: true, Reason: ""}
		}
	}
	return AutoStatus{Active: false, Reason: fmt.Sprintf("mode=%q, persona=%q — neither is set to \"auto\"", strings.Join(s.Modes, ","), s.Persona)}
}

// IsAutoActive is a convenience wrapper that returns only the boolean.
func IsAutoActive() bool {
	return CheckAutoActive().Active
}

// ── Active file validation + fallback ────────────────────────────────────────
//
// v spec 2026-08-04: an operator can rename / delete a mode or persona
// .md file while active.json (or the legacy config/current_* files)
// still references the old name. If we passed the dangling name
// through to the LLM, the agent would boot with a blank context
// window. The fallback layer below catches os.ErrNotExist at every
// load point and substitutes the safe default ("default" for both),
// logging to the audit table so operators see the substitution.
//
// "Safe default" = the alpha-baseline professional identity:
//   - persona → "default"  (utilitarian, no-fluff execution)
//   - mode    → "default"  (balanced retrieval for routine work)
// Both names MUST exist on disk; the resolver falls back to empty
// string if neither the requested nor the default file is present,
// in which case the caller treats empty as "no override" rather than
// crashing.

// ResolveActivePersona returns the persona name to use given a
// caller-supplied requested name. Validates that the persona file
// exists on disk; if not, logs a warning to the audit table (if dm
// is non-nil) and falls back to "default". Empty input also falls
// back to "default".
//
// The check is a single os.Stat — cheap; safe to call from every
// wake-context load and every MCP call resolution.
func ResolveActivePersona(dm *DatabaseManager, requested string) string {
	return resolveActiveComponent(dm, "persona", requested, "default")
}

// ResolveActiveMode is the mode-equivalent of ResolveActivePersona.
// Falls back to "default" when the requested mode file is missing.
func ResolveActiveMode(dm *DatabaseManager, requested string) string {
	return resolveActiveComponent(dm, "mode", requested, "default")
}

// resolveActiveComponent is the shared fallback for both kinds.
// kind is "persona" or "mode"; defaultName is the safe-substitute
// (always "default" in alpha). Returns the resolved name — never an
// error. If BOTH the requested and default files are missing,
// returns empty string (caller treats empty as "no override").
func resolveActiveComponent(dm *DatabaseManager, kind, requested, defaultName string) string {
	mpmDir := config.GetMPMDir()

	// Try the requested component first.
	if requested != "" {
		path := filepath.Join(mpmDir, kind, requested+".md")
		if _, err := os.Stat(path); err == nil {
			return requested
		}
		// Requested is missing — fall through to default. Successful fallback
		// is a deliberate, non-anomalous event (the system is designed to do
		// this), so it logs at AuditInfo — forensic trail only, not cluster
		// detection. The AuditError branch below covers the real anomaly:
		// the safe default itself is missing.
		if dm != nil {
			dm.LogAudit(AuditInfo, "router",
				fmt.Sprintf("%s %q not found on disk; falling back to %q", kind, requested, defaultName),
				"",
				AuditContext{
					"requested": requested,
					"fallback":  defaultName,
					"path":      filepath.Join(mpmDir, kind, requested+".md"),
				})
		}
	}

	// Try the safe default. If even that is missing, log + empty.
	defPath := filepath.Join(mpmDir, kind, defaultName+".md")
	if _, err := os.Stat(defPath); err == nil {
		return defaultName
	}
	if dm != nil {
		dm.LogAudit(AuditError, "router",
			fmt.Sprintf("%s fallback %q also missing on disk; agent will boot without %s context", kind, defaultName, kind),
			"",
			AuditContext{"fallback": defaultName, "path": defPath})
	}
	return ""
}

// ── ActiveContext (the agent-supplied provenance for write methods) ───────

// ActiveContext carries the agent's active mode/persona for provenance
// injection on memory writes. Mirrors the package-level globals in
// cmd/mpm/handlers.go (activeMode, activePersona). Callers set fields
// before invoking write methods; reads are safe with zero value.
//
// Empty values fall back to defaults at write time ("call" model,
// "mpm_call" agent) so a zero-value ActiveContext produces sane
// provenance without explicit configuration.
type ActiveContext struct {
	Mode    string
	Persona string
	// Model is the name of the model that produced this memory.
	Model string
	// Agent overrides the default "mpm_call" agent name in provenance.
	Agent string
	// SessionID is the runtime session UUID. Used by Arc 2 to pull
	// shared.event_wakes targeted at this session in the WakesPending
	// fold. Empty for unit tests that don't run a session.
	SessionID string
	// Hostname is reported in the shared.sessions heartbeat for
	// debugging ("who is awake?"). Empty is fine.
	Hostname string
	// FrameworkName identifies the calling framework (e.g. "mpm-cli",
	// "mcp", "hermes", "opencode"). The drill orchestrator uses this to
	// populate tool_invocations.framework_name and to group the
	// compatibility matrix. Empty is fine — defaults to "mpm-cli".
	FrameworkName string
}

// provenanceMeta is deprecated. The full provenance block is now
// captured at the artifact_provenance table by RecordArtifactProvenance.
// The legacy metadata.provenance.* JSON block is left in existing rows
// for forensic history but is no longer written to new rows.
//
// Spec: docs/superpowers/specs/2026-08-08-artifact-provenance-design.md
// (see "Migration plan > Legacy metadata.provenance.* JSON").
func (ac ActiveContext) provenanceMeta() map[string]interface{} {
	return map[string]interface{}{}
}

// withActiveContextMeta merges provenance + active-mode/persona into
// the caller's meta map. If meta is nil, a fresh map is allocated.
// Existing keys in meta are preserved; provenance overwrites if both
// define the same key.
func (ac ActiveContext) withActiveContextMeta(meta map[string]interface{}) map[string]interface{} {
	if meta == nil {
		meta = map[string]interface{}{}
	}
	for k, v := range ac.provenanceMeta() {
		meta[k] = v
	}
	if ac.Mode != "" {
		meta["active_mode"] = ac.Mode
	}
	if ac.Persona != "" {
		meta["active_persona"] = ac.Persona
	}
	return meta
}