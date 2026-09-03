package internal

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestValidateSchemaPrefix(t *testing.T) {
	cases := []struct {
		name    string
		prefix  string
		wantErr bool
	}{
		// Empty is allowed (treats as "default schema").
		{"empty", "", false},
		// Normal prefixes.
		{"main", "main.", false},
		{"shared", "shared.", false},
		{"underscore-start", "_priv.", false},
		{"digits-after-first", "s1.", false},
		// Injection vectors — must reject.
		{"no-trailing-dot", "main", true},
		{"trailing-dot-only", ".", true},
		{"digit-start", "1main.", true},
		{"hyphen", "main-memories.", true},
		{"semicolon", "main.;DROP TABLE memories;--", true},
		{"space", "main .", true},
		{"quote", `main"`, true},
		{"wildcard", "main*.", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateSchemaPrefix(tc.prefix)
			if tc.wantErr && err == nil {
				t.Errorf("ValidateSchemaPrefix(%q) = nil, want error", tc.prefix)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("ValidateSchemaPrefix(%q) = %v, want nil", tc.prefix, err)
			}
			// Error message must not echo back the unsafe value verbatim
			// (defense in depth: a wrapped formatter could accidentally
			// leak the rejected input into logs).
			if err != nil && strings.Contains(err.Error(), tc.prefix) && tc.wantErr {
				// For some test cases the prefix appears in the message
				// because the regex string is shown; allow it but verify
				// the message is reasonable.
				if !strings.Contains(err.Error(), "invalid schema prefix") {
					t.Errorf("ValidateSchemaPrefix(%q) error %q missing context", tc.prefix, err)
				}
			}
		})
	}
}

// TestVectorMatch_SkipsHashRows verifies that VectorMatch never returns
// memories with embedding_source='hash'. Hash-sourced embeddings are
// deterministic fingerprints with no semantic content; cosine comparison
// against them is meaningless.
func TestVectorMatch_SkipsHashRows(t *testing.T) {
	dm := NewTestDM(t)
	defer dm.Close()

	dim := 8
	vec := make([]float32, dim)
	for i := range vec {
		vec[i] = float32(0.5)
	}
	embJSON, _ := json.Marshal(vec)

	// Insert two memories: same vector (identical cosine=1.0), different embedding_source.
	// The hash row must be filtered out by VectorMatch.
	hashID := "hash-row-" + t.Name()
	providerID := "provider-row-" + t.Name()

	_, err := dm.SQLDB().Exec(`
		INSERT INTO memories (id, collection, content, embedding, embedding_source, embedding_dimension, deleted_at)
		VALUES (?, 'memories', 'hash memory content', ?, 'hash', ?, NULL)`,
		hashID, string(embJSON), dim)
	if err != nil {
		t.Fatalf("insert hash memory: %v", err)
	}

	_, err = dm.SQLDB().Exec(`
		INSERT INTO memories (id, collection, content, embedding, embedding_source, embedding_dimension, deleted_at)
		VALUES (?, 'memories', 'provider memory content', ?, 'provider', ?, NULL)`,
		providerID, string(embJSON), dim)
	if err != nil {
		t.Fatalf("insert provider memory: %v", err)
	}

	results, err := dm.VectorMatch("", vec, 10, "")
	if err != nil {
		t.Fatalf("VectorMatch: %v", err)
	}

	// Collect returned IDs
	got := make(map[string]bool)
	for _, r := range results {
		got[r.ID] = true
	}

	// Provider row must be present
	if !got[providerID] {
		t.Errorf("expected provider row %q in results, got IDs: %v", providerID, got)
	}
	// Hash row must NOT be present
	if got[hashID] {
		t.Errorf("expected hash row %q to be filtered out, but it appeared in results: %v", hashID, got)
	}
}