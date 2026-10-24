// embedding_body_sql_test.go — HAL-9001 Finding 5.
//
// The Doctor's two embedding rows make the same kind of decision about
// the same database:
//
//   - the coverage row (service_doctor.go) decides in SQL whether a
//     memory holds a vector, via hasVectorBodySQL;
//   - the integrity row (service_doctor_embedding_integrity.go)
//     decides in Go, via classifyEmbeddingPayload.
//
// Before the correction those two decisions disagreed by construction:
// the coverage row counted the `embedding_source` label and the
// integrity row counted the body. A clean install therefore reported
// "6 memories, all provider-generated" beside "6 live memories have no
// vector".
//
// The two definitions of "this row holds no vector" must now agree, or
// the contradiction returns in a different shape. This test pins that
// agreement.
//
// Note the deliberate asymmetry: the coverage row measures PRESENCE of
// a body and the integrity row measures VALIDITY of a body. A row
// holding "[]" or garbage has a body (so the coverage row counts it as
// covered) but is not a usable vector (so the integrity row FAILs it).
// That is the split the two rows are designed around, and it is not a
// disagreement.
package main

import (
	"fmt"
	"strings"
	"testing"
)

// TestHasVectorBodySQL_MatchesTheGoClassifier pins that the SQL
// predicate and the Go classifier agree on every body shape.
func TestHasVectorBodySQL_MatchesTheGoClassifier(t *testing.T) {
	dm := newTestDMForCmd(t)

	empty := ""
	spaces := "   "
	literalNull := "null"
	paddedNull := "  null  "
	tabNull := "\tnull\t"
	newlineNull := "\nnull\n"
	vector := "[0.1,0.2,0.3]"
	emptyArray := "[]"
	garbage := "not-a-vector"

	bodies := []struct {
		name string
		body *string
	}{
		{"sql null", nil},
		{"empty string", &empty},
		{"whitespace only", &spaces},
		{"literal null", &literalNull},
		{"padded literal null", &paddedNull},
		{"tab padded literal null", &tabNull},
		{"newline padded literal null", &newlineNull},
		{"a float array", &vector},
		{"an empty float array", &emptyArray},
		{"garbage", &garbage},
	}

	for i, tc := range bodies {
		id := fmt.Sprintf("body-%02d", i)
		var emb interface{}
		if tc.body != nil {
			emb = *tc.body
		}
		_, err := dm.SQLDB().Exec(
			`INSERT INTO memories
				(id, collection, content, embedding, embedding_source, weight, created_at, updated_at)
			 VALUES (?, 'memories', ?, ?, 'provider', 5,
			         CAST(strftime('%s','now') AS INTEGER), CAST(strftime('%s','now') AS INTEGER))`,
			id, tc.name, emb)
		if err != nil {
			t.Fatalf("insert %s: %v", id, err)
		}
	}

	rows, err := dm.SQLDB().Query(
		`SELECT id, ` + hasVectorBodySQL + ` FROM memories WHERE collection='memories' ORDER BY id`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()

	seen := 0
	for rows.Next() {
		var id string
		var sqlHasBody bool
		if err := rows.Scan(&id, &sqlHasBody); err != nil {
			t.Fatalf("scan: %v", err)
		}
		seen++

		var raw interface{}
		if err := dm.SQLDB().QueryRow(
			`SELECT embedding FROM memories WHERE id = ?`, id).Scan(&raw); err != nil {
			t.Fatalf("read %s: %v", id, err)
		}
		text := ""
		if raw != nil {
			switch v := raw.(type) {
			case string:
				text = v
			case []byte:
				text = string(v)
			}
		}

		// The Go side: classifyEmbeddingPayload reports an absent
		// vector as dim==0 with no defect flags.
		dim, malformed, emptyRow, nonFinite := classifyEmbeddingPayload(text)
		goAbsent := dim == 0 && !malformed && !emptyRow && !nonFinite

		if sqlHasBody == goAbsent {
			t.Errorf("%s: the SQL predicate says hasBody=%v but the Go classifier says "+
				"absent=%v; the two definitions of \"no vector\" must agree",
				id, sqlHasBody, goAbsent)
		}
	}
	if seen != len(bodies) {
		t.Errorf("read %d rows, want %d", seen, len(bodies))
	}
}

// TestHasVectorBodySQL_ExcludesTheLiteralNull pins the specific case
// that motivated the correction: the literal string "null" is how the
// substrate records "this memory has no embedding".
func TestHasVectorBodySQL_ExcludesTheLiteralNull(t *testing.T) {
	dm := newTestDMForCmd(t)

	_, err := dm.SQLDB().Exec(
		`INSERT INTO memories
			(id, collection, content, embedding, embedding_source, weight, created_at, updated_at)
		 VALUES ('literal-null', 'memories', 'no vector', 'null', 'provider', 5,
		         CAST(strftime('%s','now') AS INTEGER), CAST(strftime('%s','now') AS INTEGER))`)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	var hasBody bool
	if err := dm.SQLDB().QueryRow(
		`SELECT ` + hasVectorBodySQL + ` FROM memories WHERE id = 'literal-null'`).Scan(&hasBody); err != nil {
		t.Fatalf("query: %v", err)
	}
	if hasBody {
		t.Error("a memory whose embedding is the literal \"null\" must not count as holding a vector")
	}
	if !strings.Contains(hasVectorBodySQL, "'null'") {
		t.Errorf("the SQL predicate no longer excludes the literal null; got %q", hasVectorBodySQL)
	}
}
