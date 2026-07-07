package internal

// ArtifactTable maps an artifact type to its underlying SQLite table name.
// Used by ShowConfidence/RecomputeConfidence to read the live confidence
// column after an evidence write. Memories and lessons are the only v1
// artifact types — everything else (including empty) maps to memories.
func ArtifactTable(artifactType string) string {
	if artifactType == "lesson" {
		return "lessons"
	}
	return "memories"
}