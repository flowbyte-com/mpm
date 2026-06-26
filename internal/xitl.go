package internal

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"mpm/internal/config"

	"gopkg.in/yaml.v3"
)

// EphemeralPersona represents a Just-In-Time generated persona definition
// persisted as a JSON blob in system_config under the key "ephemeral_persona".
//
// RENAME NOTE (2026-06-26): The AntiPatterns field was misnamed in the
// same way as the routing frontmatter field. It was always populated
// with voice-guard prose, never regex input filters. To match the
// routing rename (anti_patterns → voice_guards + domain_out), this
// field becomes VoiceGuards. The JSON tag stays "anti_patterns" for
// backward compatibility with existing on-disk ephemeral blobs in
// system_config — deserialization accepts the old tag, the Go struct
// field is renamed to match the new semantic.
//
// JIT personas do not support domain_out — by definition they're
// generated on the fly with voice-guard prose from the LLM, not hand-
// curated regex sets. If a domain_out is ever needed for an ephemeral
// persona, it's a separate field and a separate scope.
type EphemeralPersona struct {
	Name         string `json:"name"`
	Title        string `json:"title"`
	Creature     string `json:"creature"`
	Vibe         string `json:"vibe"`
	Voice        string `json:"voice"`
	AntiPatterns string `json:"anti_patterns"` // legacy tag — see note above
	// VoiceGuards is the new semantic-aligned field name. Kept separate
	// from AntiPatterns for one migration cycle to avoid breaking
	// existing on-disk blobs; populators should write BOTH for the
	// transition period (see SaveEphemeralPersona).
	VoiceGuards string `json:"voice_guards,omitempty"`
}

type activeState struct {
	Persona string   `json:"persona"`
	Modes   []string `json:"modes"`
	Updated string   `json:"updated"`
}

// fmPersona is the YAML shape shared by both frontmatter renderers.
// Hoisted to package scope so FormatEphemeralPersonaAsFrontmatter (which
// needs it as a *yaml.marshalable type) and FormatEphemeralPersonaAsMarkdown
// (which needs the same shape to guarantee key parity between the two
// outputs) can share one definition. Adding a field here is a deliberate
// API change — both formats will pick it up.
type fmPersona struct {
	Name        string `yaml:"name"`
	Title       string `yaml:"title"`
	Creature    string `yaml:"creature"`
	Vibe        string `yaml:"vibe"`
	Voice       string `yaml:"voice"`
	VoiceGuards string `yaml:"voice_guards"`
}

func activeJSONPath() string {
	return filepath.Join(config.GetMPMDir(), "active.json")
}

func loadActiveJSON() (*activeState, error) {
	data, err := os.ReadFile(activeJSONPath())
	if err != nil {
		return nil, err
	}
	var s activeState
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// AutoStatus holds both the boolean gate check and the reason for denial.
type AutoStatus struct {
	Active bool
	Reason string // empty when Active is true; describes which field blocks when false
}

// CheckAutoActive reads active.json and returns whether auto is engaged.
// When auto is not active, Reason describes the exact state (e.g. mode="standard", persona="default")
// so the caller can relay diagnostics to the user.
func CheckAutoActive() AutoStatus {
	s, err := loadActiveJSON()
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

// GetEphemeralPersona fetches the JIT persona blob from system_config.
func GetEphemeralPersona(dm *DatabaseManager) (*EphemeralPersona, error) {
	row, err := dm.GetSystemConfig("ephemeral_persona")
	if err != nil {
		return nil, err
	}
	raw, ok := row["raw_json"].(string)
	if !ok || raw == "" {
		return nil, fmt.Errorf("ephemeral_persona not found")
	}
	var ep EphemeralPersona
	if err := json.Unmarshal([]byte(raw), &ep); err != nil {
		return nil, fmt.Errorf("invalid ephemeral persona JSON: %w", err)
	}
	return &ep, nil
}

// SaveEphemeralPersona upserts the JIT persona blob into system_config.
//
// Snapshot column is empty — the JIT persona has no derived/indexed
// metadata worth pre-computing; readers always deserialize raw_json
// (GetEphemeralPersona only reads raw_json, never snapshot). Pass ""
// instead of "{}" so GetSystemConfig returns snapshot=nil rather than
// snapshot={} — same semantic ("no metadata") but consistent with the
// other SaveSystemConfig caller (handlers.go:1159 passes "").
func SaveEphemeralPersona(dm *DatabaseManager, ep *EphemeralPersona) error {
	data, err := json.Marshal(ep)
	if err != nil {
		return err
	}
	hash := sha256.Sum256(data)
	contentHash := hex.EncodeToString(hash[:])
	_, err = dm.SaveSystemConfig("ephemeral_persona", string(data), contentHash, "")
	return err
}

// DeleteEphemeralPersona removes the JIT persona blob from system_config.
func DeleteEphemeralPersona(dm *DatabaseManager) error {
	return dm.DeleteSystemConfig("ephemeral_persona")
}

// FormatEphemeralPersonaAsFrontmatter converts an EphemeralPersona into a
// YAML-frontmatter block suitable for injection into the system prompt.
//
// FRONTMATTER KEY MAPPING (2026-06-26): emits `voice_guards:` (the new
// field name) with content drawn from either VoiceGuards (preferred) or
// AntiPatterns (legacy fallback for backward compat). The old
// `anti_patterns:` key is no longer emitted because the router now
// ignores it (renamed to domain_out for input filters). VoiceGuards in
// frontmatter is for LLM context only, NOT compiled as regex — see
// router.go Component struct docs for the semantic split.
func FormatEphemeralPersonaAsFrontmatter(ep *EphemeralPersona) (string, error) {
	voiceGuards := ep.VoiceGuards
	if voiceGuards == "" {
		voiceGuards = ep.AntiPatterns // legacy fallback
	}
	fm := fmPersona{
		Name:        ep.Name,
		Title:       ep.Title,
		Creature:    ep.Creature,
		Vibe:        ep.Vibe,
		Voice:       ep.Voice,
		VoiceGuards: voiceGuards,
	}
	out, err := yaml.Marshal(&fm)
	if err != nil {
		return "", err
	}
	return "---\n" + string(out) + "---", nil
}

// FormatEphemeralPersonaAsMarkdown converts an EphemeralPersona struct into a
// full persona markdown file (YAML frontmatter + markdown body) for disk write.
//
// FRONTMATTER KEY (2026-06-26): emits `voice_guards:` in the frontmatter
// (renamed from anti_patterns). The body section header is also renamed
// from "## Anti-Patterns" to "## Voice Guards" to keep file shape and
// frontmatter key aligned.
//
// YAML SAFETY (2026-06-26): the frontmatter is emitted via yaml.v3
// marshalling, not fmt.Sprintf interpolation. The previous format
// injected raw %s values into the YAML block, which broke (and allowed
// injection of arbitrary keys/values) when any persona field contained a
// `:` or newline. yaml.Marshal escapes scalar values per the YAML 1.2
// spec so hostile input round-trips losslessly. The markdown body is
// still emitted via fmt.Sprintf because markdown is not a security
// boundary — injection there is a rendering oddity, not a parser
// exploit. See TestFormatEphemeralPersonaAsMarkdown_YAMLInjection for
// the regression guard.
func FormatEphemeralPersonaAsMarkdown(ep *EphemeralPersona) string {
	voiceGuards := ep.VoiceGuards
	if voiceGuards == "" {
		voiceGuards = ep.AntiPatterns
	}

	fm := fmPersona{
		Name:        ep.Name,
		Title:       ep.Title,
		Creature:    ep.Creature,
		Vibe:        ep.Vibe,
		Voice:       ep.Voice,
		VoiceGuards: voiceGuards,
	}
	// yaml.Marshal cannot fail for a struct of strings; the only failure
	// modes are cyclic values or unsupported kinds, neither of which apply.
	// Fall back to a minimal sentinel rather than panic, since this
	// function is on the persona-promotion write path.
	frontmatter, err := yaml.Marshal(&fm)
	if err != nil {
		frontmatter = []byte("name: error\n")
	}

	body := fmt.Sprintf(`# %s

## Creature
%s

## Vibe
%s

## Voice
%s

## Voice Guards
%s
`,
		ep.Title, ep.Creature, ep.Vibe, ep.Voice, voiceGuards,
	)
	return "---\n" + string(frontmatter) + "---\n\n" + body
}
