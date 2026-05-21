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
type EphemeralPersona struct {
	Name         string `json:"name"`
	Title        string `json:"title"`
	Creature     string `json:"creature"`
	Vibe         string `json:"vibe"`
	Voice        string `json:"voice"`
	AntiPatterns string `json:"anti_patterns"`
}

type activeState struct {
	Persona string   `json:"persona"`
	Modes   []string `json:"modes"`
	Updated string   `json:"updated"`
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

func saveActiveJSON(s *activeState) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(activeJSONPath(), data, 0644)
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

// IsAutoActive is a convenience wrapper that returns only the boolean.
func IsAutoActive() bool {
	return CheckAutoActive().Active
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
func SaveEphemeralPersona(dm *DatabaseManager, ep *EphemeralPersona) error {
	data, err := json.Marshal(ep)
	if err != nil {
		return err
	}
	hash := sha256.Sum256(data)
	contentHash := hex.EncodeToString(hash[:])
	_, err = dm.SaveSystemConfig("ephemeral_persona", string(data), contentHash, "{}")
	return err
}

// DeleteEphemeralPersona removes the JIT persona blob from system_config.
func DeleteEphemeralPersona(dm *DatabaseManager) error {
	return dm.DeleteSystemConfig("ephemeral_persona")
}

// FormatEphemeralPersonaAsFrontmatter converts an EphemeralPersona into a
// YAML-frontmatter block suitable for injection into the system prompt.
func FormatEphemeralPersonaAsFrontmatter(ep *EphemeralPersona) (string, error) {
	type fmPersona struct {
		Name         string `yaml:"name"`
		Title        string `yaml:"title"`
		Creature     string `yaml:"creature"`
		Vibe         string `yaml:"vibe"`
		Voice        string `yaml:"voice"`
		AntiPatterns string `yaml:"anti_patterns"`
	}
	fm := fmPersona{
		Name:         ep.Name,
		Title:        ep.Title,
		Creature:     ep.Creature,
		Vibe:         ep.Vibe,
		Voice:        ep.Voice,
		AntiPatterns: ep.AntiPatterns,
	}
	out, err := yaml.Marshal(&fm)
	if err != nil {
		return "", err
	}
	return "---\n" + string(out) + "---", nil
}

// FormatEphemeralPersonaAsMarkdown converts an EphemeralPersona struct into a
// full persona markdown file (YAML frontmatter + markdown body) for disk write.
func FormatEphemeralPersonaAsMarkdown(ep *EphemeralPersona) string {
	return fmt.Sprintf(`---
name: %s
title: %s
creature: %s
vibe: %s
voice: %s
anti_patterns: %s
---

# %s

## Creature
%s

## Vibe
%s

## Voice
%s

## Anti-Patterns
%s
`,
		ep.Name, ep.Title, ep.Creature, ep.Vibe, ep.Voice, ep.AntiPatterns,
		ep.Title, ep.Creature, ep.Vibe, ep.Voice, ep.AntiPatterns,
	)
}
