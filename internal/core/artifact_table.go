package internal

// ArtifactTable maps an artifact type to its underlying SQLite table name.
//
// INVARIANT: this function only ever returns one of two hardcoded strings:
// "memories" or "lessons". It is NOT user-controllable. Callers pass the
// return value into fmt.Sprintf("UPDATE %s SET ...", table) at
// evidence_store.go (190, 260, 318, 409, 633, 827), so any change to add
// a new artifact type MUST be reflected here AND validated against the
// canonical schema allow-list in canonical_dump.go.
//
// Used by ShowConfidence/RecomputeConfidence to read the live confidence
// column after an evidence write. Memories and lessons are the only v1
// artifact types — everything else (including empty) maps to memories.
func ArtifactTable(artifactType string) string {
	if artifactType == "lesson" {
		return "lessons"
	}
	return "memories"
}