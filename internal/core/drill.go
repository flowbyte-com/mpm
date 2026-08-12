// drill.go — drill YAML spec types.
//
// A DrillSpec is the behavioural test contract: the orchestrator loads
// one from ~/.mpm/drills/<id>.yaml, runs the synthetic or real
// harness, and scores the resulting tool_invocations sequence against
// Expect. The struct shape is the YAML contract — do not rename
// fields without bumping a migration warning.

package internal

// DrillSpec describes one behavioural drill. Fields mirror the YAML
// top-level keys; yaml.v3 unmarshals them via the `yaml:` tags.
type DrillSpec struct {
	ID          string      `yaml:"id"`
	Description string      `yaml:"description"`
	Framework   string      `yaml:"framework"`
	Prompt      string      `yaml:"prompt"`
	Expect      DrillExpect `yaml:"expect"`
	TimeoutSecs int         `yaml:"timeout_secs"`
}

// DrillExpect captures the four compliance checks the scorer applies.
// ToolsRequired and Sequence are required for a meaningful spec; the
// loader rejects empty ToolsRequired.
type DrillExpect struct {
	ToolsRequired     []string        `yaml:"tools_required"`
	Sequence          []DrillStep     `yaml:"sequence"`
	ArtifactsRequired []DrillArtifact `yaml:"artifacts_required"`
	Forbidden         []string        `yaml:"forbidden"`
}

// DrillStep is one ordered tool/action pair in Expect.Sequence. The
// scorer walks invocations in started_at order and matches (tool, action)
// against this list.
type DrillStep struct {
	Tool   string `yaml:"tool"`
	Action string `yaml:"action"`
}

// DrillArtifact declares a side-effect that the drill must produce —
// e.g. a row in `lessons` carrying tag X. The scorer queries the
// appropriate table based on Type; v1 supports "lesson" only.
type DrillArtifact struct {
	Type            string   `yaml:"type"`
	Collection      string   `yaml:"collection"`
	Tags            []string `yaml:"tags"`
	ContentContains string   `yaml:"content_contains"`
}
