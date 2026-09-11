// active_state.go — single canonical owner of the agent's runtime state.
//
// Before this file existed, the same struct (active.json shape) and the
// same read/write logic lived in three places:
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
//
// Selector intent model (2026-09-11):
//
// Persona is 0..1, modes are 0..N. Both dimensions must distinguish:
//
//	1. explicit selection    (user picked a value via the CLI)
//	2. explicit clear / none  (user cleared via the CLI; "no override")
//	3. absent / uninitialised (no active.json on disk yet)
//	4. stale / invalid       (selected name no longer resolves to a file)
//	5. default fallback      (system default, only when (3) or (4))
//
// The old ActiveState used `string` / `[]string` for persona/modes, which
// cannot distinguish (2) from (3) — both produce zero values when
// unmarshalled. We now use `*string` / `*[]string` so:
//
//	nil   → absent / uninitialised       → default fallback
//	&""   → explicit clear                → empty
//	&"x"  → explicit selection            → "x" if file exists, else fallback
//
// The resolver API is plural for modes (0..N) and singular for persona
// (0..1). See Resolution and ModeResolution below.
//
// Legacy callers that still use string→string APIs continue to work
// because the singular resolver preserves the pre-pointer semantics:
// empty input → default fallback.
package internal

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/flowbyte-com/mpm-core/config"
)

// ── Source vocabulary ────────────────────────────────────────────────────────
//
// The resolver tags every resolved entry with one of three sources so
// callers can distinguish explicit user intent from system fallback
// from "no override". The vocabulary is small and stable; do not
// introduce a fourth value without updating downstream consumers
// (wake_context.go, handlers.go, route_render.go).

const (
	// SourceExplicit — user selected the value AND the file exists on disk.
	SourceExplicit = "explicit"
	// SourceFallback — user selected the value OR active.json was absent,
	// and the system substituted the default file because the requested
	// value (or absent state) did not resolve to a real file.
	SourceFallback = "fallback"
	// SourceEmpty — explicit clear by the user, OR both the requested and
	// default files are missing. Caller treats empty as "no override".
	SourceEmpty = "empty"
)

// ── Resolution types ────────────────────────────────────────────────────────

// Resolution is the resolver's canonical output for a single-value
// selector (persona). Name is the resolved name (possibly empty);
// Source describes how the value was obtained.
//
//	{Name: "critic", Source: SourceExplicit}  user selected, file exists
//	{Name: "default", Source: SourceFallback} user selected X, X missing, fell back
//	{Name: "",       Source: SourceEmpty}     user cleared OR all-missing
type Resolution struct {
	Name   string `json:"name"`
	Source string `json:"source"`
}

// IsZero reports whether the resolution represents "no override" —
// useful for short-circuit checks at call sites.
func (r Resolution) IsZero() bool {
	return r.Name == "" && r.Source == SourceEmpty
}

// ResolvedMode is the per-entry resolution inside a multi-mode selection.
// A multi-mode list may have any number of resolved entries; see
// ModeResolution for the aggregate.
type ResolvedMode struct {
	Name   string `json:"name"`
	Source string `json:"source"`
}

// ModeResolution is the resolver's canonical output for a multi-value
// selector (modes). Modes is the resolved list (possibly empty). The
// aggregate Source describes how the SELECTION AS A WHOLE was obtained:
//
//	{[],       SourceEmpty}     explicit clear, OR absent + no default
//	{[d],      SourceFallback}  absent state + bootstrap to default file
//	{[a, b],   SourceExplicit}  user selected, both files exist
//
// Stale / invalid entries (selected name with missing file) are NOT
// replaced by `default`; they are dropped from Modes. The overall Source
// is "explicit" when at least one valid selection survives. If ALL
// selections are stale, the resolver falls back to [default] with
// SourceFallback — the established stale-state recovery policy.
type ModeResolution struct {
	Modes   []ResolvedMode `json:"modes"`
	Source  string         `json:"source"`
	Missing []string       `json:"missing,omitempty"`
}

// IsZero reports whether the resolution represents "no override" (no
// modes, source empty).
func (mr ModeResolution) IsZero() bool {
	return len(mr.Modes) == 0 && mr.Source == SourceEmpty
}

// Names returns just the resolved names — useful when callers want the
// collection without per-entry source metadata.
func (mr ModeResolution) Names() []string {
	out := make([]string, 0, len(mr.Modes))
	for _, m := range mr.Modes {
		if m.Name != "" {
			out = append(out, m.Name)
		}
	}
	return out
}

// First returns the first resolved mode name (or "") — convenience for
// callers that need a singular string (e.g. memory metadata injection).
// Equivalent to Names()[0] but with bounds safety.
func (mr ModeResolution) First() string {
	if len(mr.Modes) == 0 {
		return ""
	}
	return mr.Modes[0].Name
}

// ── Runtime active state (the on-disk active.json file) ────────────────────

// ActiveState mirrors the active.json schema on disk. Persona and Modes
// are pointers so callers can distinguish "user explicitly cleared"
// (`&""` for persona, `&[]string{}` for modes) from "field absent /
// uninitialised" (`nil`). This distinction drives the resolver's
// fallback semantics — see file-level doc above.
//
// Owned by cmd/mpm/mode and cmd/mpm/persona subcommands; read at session
// start (mpm wake) and on every route hook invocation.
type ActiveState struct {
	// Persona: nil = absent; &"" = explicit clear; &"name" = explicit.
	Persona *string `json:"persona,omitempty"`
	// Modes: nil = absent; &[] = explicit clear; &[…] = explicit.
	Modes *[]string `json:"modes,omitempty"`
	// Updated: RFC3339 timestamp of the last write.
	Updated string `json:"updated"`
}

// PersonaString returns the persona as a plain string (empty for both
// nil and &""). Callers that don't care about intent distinction can
// use this; callers that do should consult Persona directly.
func (s *ActiveState) PersonaString() string {
	if s.Persona == nil {
		return ""
	}
	return *s.Persona
}

// ModesSlice returns the modes as a plain slice (empty for both nil
// and &[]). Callers that don't care about intent distinction can use
// this; callers that do should consult Modes directly.
func (s *ActiveState) ModesSlice() []string {
	if s.Modes == nil {
		return nil
	}
	return *s.Modes
}

// IsPersonaExplicitClear reports whether the user explicitly cleared
// the persona (`mpm persona clear`).
func (s *ActiveState) IsPersonaExplicitClear() bool {
	return s.Persona != nil && *s.Persona == ""
}

// IsModesExplicitClear reports whether the user explicitly cleared
// the modes (`mpm mode clear`).
func (s *ActiveState) IsModesExplicitClear() bool {
	return s.Modes != nil && len(*s.Modes) == 0
}

// IsPersonaAbsent reports whether the persona field is absent from
// active.json (no user action yet).
func (s *ActiveState) IsPersonaAbsent() bool {
	return s.Persona == nil
}

// IsModesAbsent reports whether the modes field is absent from
// active.json (no user action yet).
func (s *ActiveState) IsModesAbsent() bool {
	return s.Modes == nil
}

// ActiveJSONPath returns the absolute path to the active.json file.
// Always rooted at config.GetMPMDir() — the same directory the rest of
// MPM uses for runtime state.
func ActiveJSONPath() string {
	return filepath.Join(config.GetMPMDir(), "active.json")
}

// LoadActiveJSON reads the active.json file and returns the parsed state.
// Missing file is NOT an error — a fresh install has no active.json, and
// callers should treat that as "absent" (both Persona and Modes nil).
// Other read errors (corrupt JSON, permission denied) propagate so the
// operator can fix.
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

// SaveActiveJSON writes the state to active.json with 0600 perms. JSON
// output is indented for human readability (operators debug this file
// by hand). The function is the single canonical writer; callers in
// cmd/mpm route through this method, not through any local copy.
//
// Persona and Modes are written verbatim — pointer types mean nil
// omits the key (absent state), &"" writes "" (explicit clear),
// &"x" writes "x" (explicit selection).
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
	if s.Persona != nil && *s.Persona == "auto" {
		return AutoStatus{Active: true, Reason: ""}
	}
	if s.Modes != nil {
		for _, m := range *s.Modes {
			if m == "auto" {
				return AutoStatus{Active: true, Reason: ""}
			}
		}
	}
	modeStr := "<absent>"
	if s.Modes != nil {
		modeStr = strings.Join(*s.Modes, ",")
	}
	personaStr := "<absent>"
	if s.Persona != nil {
		personaStr = *s.Persona
	}
	return AutoStatus{Active: false, Reason: fmt.Sprintf("mode=%q, persona=%q — neither is set to \"auto\"", modeStr, personaStr)}
}

// IsAutoActive is a convenience wrapper that returns only the boolean.
func IsAutoActive() bool {
	return CheckAutoActive().Active
}

// ── Active file validation + fallback ────────────────────────────────────────
//
// v spec 2026-09-11 (selector hardening):
//
//	explicit valid → return that value (SourceExplicit)
//	explicit invalid (file gone) → drop, fall back to default (SourceFallback)
//	explicit clear → return empty (SourceEmpty)
//	absent / uninitialised → try default (SourceFallback if default exists,
//	                           SourceEmpty otherwise)
//	both requested + default missing → return empty (SourceEmpty)
//
// "Safe default" = the alpha-baseline professional identity:
//   - persona → "default"  (utilitarian, no-fluff execution)
//   - mode    → "default"  (balanced retrieval for routine work)
//
// Both names MUST exist on disk; the resolver falls back to empty
// if neither the requested nor the default file is present.

// resolveActivePersonaComponent is the shared implementation for the
// pointer-aware persona resolver. The four input cases map onto the
// three Resolution cases as follows:
//
//	requested == nil        → absent / uninitialised → bootstrap
//	*requested == ""        → explicit clear         → SourceEmpty
//	*requested == "x" valid → SourceExplicit
//	*requested == "x" stale → SourceFallback (after trying default)
func resolveActivePersonaComponent(dm *DatabaseManager, requested *string, defaultName string) Resolution {
	mpmDir := config.GetMPMDir()

	// Case 1: explicit clear → empty, no further resolution.
	if requested != nil && *requested == "" {
		return Resolution{Name: "", Source: SourceEmpty}
	}

	// Case 2: explicit selection → check the file exists.
	if requested != nil && *requested != "" {
		path := filepath.Join(mpmDir, "persona", *requested+".md")
		if _, err := os.Stat(path); err == nil {
			return Resolution{Name: *requested, Source: SourceExplicit}
		}
		// Selected file is missing — fall through to default. The fallback
		// is a deliberate, non-anomalous event (the system is designed to
		// do this), so it logs at AuditInfo — forensic trail only.
		if dm != nil {
			dm.LogAudit(AuditInfo, "router",
				fmt.Sprintf("persona %q not found on disk; falling back to %q", *requested, defaultName),
				"",
				AuditContext{
					"requested": *requested,
					"fallback":  defaultName,
					"path":      path,
				})
		}
		// Try the safe default below.
	}

	// Case 3 (absent OR explicit-but-missing): try the safe default.
	defPath := filepath.Join(mpmDir, "persona", defaultName+".md")
	if _, err := os.Stat(defPath); err == nil {
		return Resolution{Name: defaultName, Source: SourceFallback}
	}
	if dm != nil {
		dm.LogAudit(AuditInfo, "router",
			fmt.Sprintf("persona fallback %q also missing on disk; agent will boot without persona context", defaultName),
			"",
			AuditContext{"fallback": defaultName, "path": defPath})
	}
	return Resolution{Name: "", Source: SourceEmpty}
}

// ResolveActivePersona is the canonical pointer-aware persona resolver.
// The pointer parameter carries intent:
//
//	requested == nil        → absent / uninitialised
//	*requested == ""        → explicit clear
//	*requested == "name"    → explicit selection
//
// Returns the Resolution (name + source). New code should call this
// directly. Legacy callers can use the ResolveActivePersona string
// wrapper for back-compat.
func ResolveActivePersonaIntent(dm *DatabaseManager, requested *string) Resolution {
	return resolveActivePersonaComponent(dm, requested, "default")
}

// ResolveActivePersona preserves the original singular string API
// used by pre-2026-09-11 callers and tests:
//
//	""     → absent / bootstrap → default fallback
//	"x"    → explicit selection → x if exists, else default
//
// This signature cannot distinguish explicit-clear from absent — if you
// need that distinction, use ResolveActivePersonaIntent with the
// underlying *string from ActiveState.Persona.
//
// New code should prefer ResolveActivePersonaIntent for the full
// source-aware Resolution.
func ResolveActivePersona(dm *DatabaseManager, requested string) string {
	if requested == "" {
		return resolveActivePersonaComponent(dm, nil, "default").Name
	}
	r := requested
	return resolveActivePersonaComponent(dm, &r, "default").Name
}

// resolveActiveModesComponent is the shared implementation for the
// pointer-aware multi-mode resolver. See ModeResolution for the
// return-shape contract.
func resolveActiveModesComponent(dm *DatabaseManager, requested *[]string, defaultName string) ModeResolution {
	mpmDir := config.GetMPMDir()
	var missing []string

	// Case 1: explicit clear → empty result.
	if requested != nil && len(*requested) == 0 {
		return ModeResolution{Modes: []ResolvedMode{}, Source: SourceEmpty}
	}

	// Case 2: explicit selection → validate each entry. Stale entries are
	// dropped (NOT replaced with `default`).
	if requested != nil && len(*requested) > 0 {
		var resolved []ResolvedMode
		anyExplicit := false
		for _, name := range *requested {
			if name == "" {
				continue
			}
			path := filepath.Join(mpmDir, "mode", name+".md")
			if _, err := os.Stat(path); err == nil {
				resolved = append(resolved, ResolvedMode{Name: name, Source: SourceExplicit})
				anyExplicit = true
			} else {
				missing = append(missing, name)
				if dm != nil {
					dm.LogAudit(AuditInfo, "router",
						fmt.Sprintf("mode %q not found on disk; dropping (not replacing with default)", name),
						"",
						AuditContext{
							"requested": name,
							"path":      path,
						})
				}
			}
		}
		if anyExplicit {
			return ModeResolution{Modes: resolved, Source: SourceExplicit, Missing: missing}
		}
		// All selected modes are stale — try the default.
		// Fall through with `missing` populated.
	}

	// Case 3 (absent OR all-selected-stale): try the safe default.
	// Preserve the `missing` slice so callers can see what was dropped.
	defPath := filepath.Join(mpmDir, "mode", defaultName+".md")
	if _, err := os.Stat(defPath); err == nil {
		return ModeResolution{
			Modes:   []ResolvedMode{{Name: defaultName, Source: SourceFallback}},
			Source:  SourceFallback,
			Missing: missing,
		}
	}
	if dm != nil {
		dm.LogAudit(AuditInfo, "router",
			fmt.Sprintf("mode fallback %q also missing on disk; agent will boot without mode context", defaultName),
			"",
			AuditContext{"fallback": defaultName, "path": defPath})
	}
	return ModeResolution{Modes: []ResolvedMode{}, Source: SourceEmpty}
}

// ResolveActiveModes is the canonical pointer-aware multi-mode resolver.
// The pointer parameter carries intent:
//
//	requested == nil              → absent / uninitialised (bootstrap)
//	*requested == nil or len==0   → explicit clear (empty list)
//	*requested == […]             → explicit selection (validate each)
//
// Stale entries (selected name with missing file) are dropped — they
// do NOT inject "default" alongside surviving modes. If all selected
// modes are stale, the resolver falls back to [default] with
// SourceFallback (the established stale-state recovery policy).
//
// Returns the ModeResolution (modes + source + missing). New code
// should call this directly. Legacy callers can use the
// ResolveActiveModeLegacy string wrapper for back-compat.
func ResolveActiveModes(dm *DatabaseManager, requested *[]string) ModeResolution {
	return resolveActiveModesComponent(dm, requested, "default")
}

// ResolveActiveModesLegacy preserves the original singular string API
// (with comma-joined input) for pre-2026-09-11 callers:
//
//	""                → absent / bootstrap → default fallback
//	"x"               → explicit selection → x if exists, else default
//	"x,y,z"           → first valid token (stale tokens dropped)
//
// Returns the first valid resolved name. Use ResolveActiveModes for
// the full collection.
func ResolveActiveModesLegacy(dm *DatabaseManager, requested string) string {
	if requested == "" {
		return resolveActiveModesComponent(dm, nil, "default").First()
	}
	mr := resolveActiveModesComponent(dm, splitCSV(requested), "default")
	names := mr.Names()
	if len(names) > 0 {
		return names[0]
	}
	return ""
}

// ── Original-signature back-compat aliases ───────────────────────────────────
//
// Pre-2026-09-11 callers and tests (e.g. wake_active_state_symmetry_test.go,
// active_state_test.go, cmd/mpm/handlers.go) use the names ResolveActivePersona
// and ResolveActiveMode with the (string, string) signature. These
// aliases preserve those signatures and route through the *Legacy
// resolvers, so existing code continues to work without modification.
// New code should call ResolveActivePersona / ResolveActiveModes with
// the pointer-aware API.

// ResolveActivePersonaCompat is the explicit back-compat alias for the
// original singular string-in/string-out signature. Calls the canonical
// ResolveActivePersona (string) which is defined above. Use
// ResolveActivePersona directly.
func ResolveActivePersonaCompat(dm *DatabaseManager, requested string) string {
	return ResolveActivePersona(dm, requested)
}

// ResolveActiveMode is the original singular string-in/string-out signature
// for modes preserved for back-compat. Internally routes through
// ResolveActiveModesLegacy.
func ResolveActiveMode(dm *DatabaseManager, requested string) string {
	return ResolveActiveModesLegacy(dm, requested)
}

// splitCSV splits a comma-separated string and trims whitespace from
// each token. Empty tokens are dropped. Used by the legacy singular
// mode resolver.
func splitCSV(s string) *[]string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return &out
}

// ── ActiveContext (the agent-supplied provenance for write methods) ──────

// ActiveContext carries the agent's active mode/persona for provenance
// injection on memory writes. Mirrors the package-level globals in
// cmd/mpm/handlers.go (activeMode, activePersona). Callers set fields
// before invoking write methods; reads are safe with zero value.
//
// Empty values fall back to defaults at write time ("call" model,
// "mpm_call" agent) so a zero-value ActiveContext produces sane
// provenance without explicit configuration.
//
// NOTE: As of 2026-08-24, ActiveContext is the MODE/PERSONA lens only.
// Provenance fields (provider, model, temperature, reasoning, invocation)
// belong to EffectiveProvenance via ProvenanceResolver. ActiveContext
// carries only the per-call invocation correlation IDs that must be
// threaded from the dispatcher. All other provenance is resolved from
// env via ProvenanceResolver — see forensic audit §9.
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
	// InvocationID is the per-call correlation ID for this tool invocation.
	// When set, it is threaded into EffectiveProvenance and work_events.
	// Generate via GenerateID() at the dispatcher. Empty for unit tests.
	InvocationID string
	// ParentInvocationID traces agent-of-agent causality (Hermes → Claude Code).
	ParentInvocationID string
}

// provenanceMeta is deprecated. The full provenance block is now
// captured at the artifact_provenance table by RecordArtifactProvenance.
// The legacy metadata.provenance.* JSON block is left in existing rows
// for forensic history but is no longer written to new rows.
//
// Spec: docs/archive/2026-08-08-artifact-provenance-design.md
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