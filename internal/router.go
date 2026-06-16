package internal

import (
	"regexp"
	"strings"
)

// ComponentKind classifies whether a component is a mode (multi-select, threshold)
// or a persona (single-select, max-pool).
type ComponentKind int

const (
	KindMode ComponentKind = iota
	KindPersona
)

// Component holds the pre-compiled pattern set for one mode or persona.
// Patterns are OR-matched against the input (any single match scores +1).
// Anti-patterns are AND-matched (any single match scores -1 per match).
type Component struct {
	Name         string
	Kind         ComponentKind
	Patterns     []*regexp.Regexp
	AntiPatterns []*regexp.Regexp
	// explicitPatterns marks patterns that appear in frontmatter `patterns:` field
	// rather than extracted from body text — these get a weight boost.
	explicitPatterns int
}

// RoutingReport is the structured output of Evaluate().
type RoutingReport struct {
	SelectedModes   []string `json:"selected_modes"`
	SelectedPersona string   `json:"selected_persona"`
	Scores          map[string]ScoreEntry `json:"scores"`
}

// ScoreEntry describes why a component was or wasn't selected.
type ScoreEntry struct {
	Score      int      `json:"score"`
	Triggers   []string `json:"triggers,omitempty"`
	Penalties  []string `json:"penalties,omitempty"`
}

// Router evaluates prompts against pre-loaded mode and persona definitions.
type Router struct {
	modes     []*Component
	personas  []*Component
	modeThreshold int // minimum net score to activate a mode
}

// NewRouter loads all mode/*.md and persona/*.md files from basePath,
// compiles their patterns into regex, and returns a ready-to-evaluate Router.
func NewRouter(basePath string) (*Router, error) {
	modeDir := join(basePath, "mode")
	personaDir := join(basePath, "persona")

	modes, err := loadComponents(modeDir, KindMode)
	if err != nil {
		return nil, err
	}
	personas, err := loadComponents(personaDir, KindPersona)
	if err != nil {
		return nil, err
	}

	return &Router{
		modes:         modes,
		personas:      personas,
		modeThreshold: 1, // Any positive match activates the mode; caller decides whether to act.
	}, nil
}

// Evaluate scores an input string against all loaded modes and personas.
// Modes use threshold filtering (all components scoring >= threshold activate).
// Personas use max-pooling (only the highest-scoring persona wins, if score >= 1).
// Returns a RoutingReport with selected modes, selected persona, and full diagnostics.
func (r *Router) Evaluate(input string) RoutingReport {
	lower := strings.ToLower(input)
	scores := make(map[string]ScoreEntry)

	// --- Mode pipeline: threshold filter ---
	var selectedModes []string
	for _, comp := range r.modes {
		entry := scoreComponent(lower, comp)
		scores[comp.Name] = entry
		if entry.Score >= r.modeThreshold {
			selectedModes = append(selectedModes, comp.Name)
		}
	}

	// --- Persona pipeline: max-pool ---
	bestScore := 0
	bestPersona := ""
	for _, comp := range r.personas {
		entry := scoreComponent(lower, comp)
		scores[comp.Name] = entry
		if entry.Score > bestScore {
			bestScore = entry.Score
			bestPersona = comp.Name
		}
	}
	// Persona only activates if net score >= 1
	if bestScore < 1 {
		bestPersona = ""
	}

	return RoutingReport{
		SelectedModes:   selectedModes,
		SelectedPersona: bestPersona,
		Scores:          scores,
	}
}

// scoreComponent evaluates one component against the lowercased input.
func scoreComponent(lower string, comp *Component) ScoreEntry {
	entry := ScoreEntry{}
	seen := make(map[string]bool)

	// Score explicit patterns first (weight boost: +2 per explicit match)
	for i, re := range comp.Patterns {
		if re.MatchString(lower) {
			trigger := reString(re)
			if !seen[trigger] {
				entry.Triggers = append(entry.Triggers, trigger)
				seen[trigger] = true
			}
			weight := 1
			if i < comp.explicitPatterns {
				weight = 2
			}
			entry.Score += weight
		}
	}

	// Penalize anti-patterns (-1 per matched anti-pattern)
	for _, re := range comp.AntiPatterns {
		if re.MatchString(lower) {
			entry.Penalties = append(entry.Penalties, reString(re))
			entry.Score--
		}
	}

	return entry
}

// join is a convenience wrapper around filepath.Join for the router package.
func join(base, sub string) string {
	// Avoid importing filepath in internal — use simple concat with separator.
	// Caller ensures basePath is absolute and clean.
	if base == "" {
		return sub
	}
	if strings.HasSuffix(base, "/") {
		return base + sub
	}
	return base + "/" + sub
}

// reString returns the raw string representation of a compiled regex.
func reString(re *regexp.Regexp) string {
	return re.String()
}