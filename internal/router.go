package internal

import (
	"os"
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
// It tracks the mtime of the mode and persona directories and re-loads
// from disk automatically if any file has changed since the last load.
type Router struct {
	basePath       string
	modes          []*Component
	personas       []*Component
	modeThreshold  int // minimum net score to activate a mode
	modeDirMtime   int64
	personaDirMtime int64
}

// NewRouter loads all mode/*.md and persona/*.md files from basePath,
// compiles their patterns into regex, and returns a ready-to-evaluate Router.
func NewRouter(basePath string) (*Router, error) {
	r := &Router{
		basePath:      basePath,
		modeThreshold: 1, // Any positive match activates the mode; caller decides whether to act.
	}
	if err := r.reload(); err != nil {
		return nil, err
	}
	return r, nil
}

// reload re-reads mode and persona directories from disk, recompiles all
// regex, and updates tracked mtimes. Called automatically by Evaluate() if
// it detects a file changed since last load. Can also be called explicitly.
func (r *Router) reload() error {
	modeDir := join(r.basePath, "mode")
	personaDir := join(r.basePath, "persona")

	modes, err := loadComponents(modeDir, KindMode)
	if err != nil {
		return err
	}
	personas, err := loadComponents(personaDir, KindPersona)
	if err != nil {
		return err
	}

	modeMtime, _ := dirMtime(modeDir)
	personaMtime, _ := dirMtime(personaDir)

	r.modes = modes
	r.personas = personas
	r.modeDirMtime = modeMtime
	r.personaDirMtime = personaMtime
	return nil
}

// dirMtime returns the max mtime among all files in dir, or 0 if dir doesn't exist.
func dirMtime(dir string) (int64, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, err
	}
	var max int64
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if mt := info.ModTime().UnixNano(); mt > max {
			max = mt
		}
	}
	return max, nil
}

// Modes returns the current loaded mode components (exposed for testing).
func (r *Router) Modes() []*Component { return r.modes }

// Personas returns the current loaded persona components (exposed for testing).
func (r *Router) Personas() []*Component { return r.personas }

// maybeReload checks whether any mode or persona file has changed on disk
// and reloads if so. Safe to call on every Evaluate() — the stat cost is
// microseconds and reload only happens when a file actually changed.
func (r *Router) maybeReload() {
	modeDir := join(r.basePath, "mode")
	personaDir := join(r.basePath, "persona")

	currentModeMtime, err := dirMtime(modeDir)
	if err == nil && currentModeMtime != r.modeDirMtime {
		r.reload()
		return
	}
	currentPersonaMtime, err := dirMtime(personaDir)
	if err == nil && currentPersonaMtime != r.personaDirMtime {
		r.reload()
	}
}

// Evaluate scores an input string against all loaded modes and personas.
// Modes use threshold filtering (all components scoring >= threshold activate).
// Personas use max-pooling (only the highest-scoring persona wins, if score >= 1).
// Returns a RoutingReport with selected modes, selected persona, and full diagnostics.
//
// If any mode or persona file has changed on disk since the last load (detected
// via directory mtime), Evaluate() reloads automatically before scoring. This
// means new modes and personas are picked up without restarting mpm-mcp.
func (r *Router) Evaluate(input string) RoutingReport {
	// Hot reload: check if any file changed since last load.
	r.maybeReload()

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