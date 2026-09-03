package internal

import (
	"errors"
	"os"
	"strings"
	"testing"
)

// TestApplyEmbeddingFailureToMetadata pins FIX 2 of the final
// separation-review defects. The synthesis-time embedding-failure
// contract MUST:
//
//  1. Emit an AuditWarn row with component="synthesis" and the canonical
//     "synthesis succeeded but embedding failed" phrase carrying the
//     provider error verbatim.
//  2. Record the actual provider error text on metadata["embedding_error"].
//  3. Record metadata["embedding_status"] = "unavailable" — the
//     canonical sentinel matching the MCP save_to_memory
//     structured-response contract (spec §4.4).
//
// Without FIX 2 the failure was only visible in the audit log
// (which has retention rules) and the memory's embedding column was
// NULL without any on-row signal that the absence was a failure vs.
// a fresh write that simply hadn't been embedded yet.
//
// The helper is invoked from AutoSynthesize at line 537 of
// synthesis_auto.go (after the LLM succeeds) and is the load-bearing
// entry point for the failure contract. Exercising it directly proves
// the contract independent of FTS5 / LLM wiring — those integration
// paths are conditional on bm25 thresholds and corpus shape and are
// not part of this regression.
func TestApplyEmbeddingFailureToMetadata(t *testing.T) {
	dm, err := NewDatabaseManager(t.TempDir())
	if err != nil {
		t.Fatalf("NewDatabaseManager: %v", err)
	}
	defer dm.Close()

	const providerErr = "embed: Ollama returned 500: connection refused to upstream"
	embedErr := errors.New(providerErr)

	metadata := map[string]interface{}{
		"provenance": map[string]interface{}{"source": "synthetic"},
	}

	applyEmbeddingFailureToMetadata(dm, metadata, embedErr)

	// Invariant 1a: metadata.embedding_error carries the actual provider
	// error verbatim. Not a generic message, not a summary — the
	// operator-facing diagnostic must contain the same text the
	// transport layer returned.
	gotErr, ok := metadata["embedding_error"].(string)
	if !ok {
		t.Fatalf("metadata.embedding_error type = %T; want string", metadata["embedding_error"])
	}
	if gotErr != providerErr {
		t.Errorf("metadata.embedding_error = %q; want %q (verbatim provider text)", gotErr, providerErr)
	}

	// Invariant 1b: metadata.embedding_status = "unavailable" — the
	// canonical sentinel from spec §4.4.
	if got := metadata["embedding_status"]; got != "unavailable" {
		t.Errorf("metadata.embedding_status = %v; want \"unavailable\"", got)
	}

	// Invariant 1c: AuditWarn row written with the canonical phrase
	// and the provider error.
	var auditCount int
	if err := dm.db.QueryRow(`
		SELECT COUNT(*) FROM system_audit_log
		WHERE component = 'synthesis'
		  AND level = 'warn'
		  AND message LIKE '%synthesis succeeded but embedding failed%'
		  AND message LIKE ?
	`, "%"+providerErr+"%").Scan(&auditCount); err != nil {
		t.Fatalf("audit_log query: %v", err)
	}
	if auditCount != 1 {
		t.Errorf("AuditWarn row count = %d; want exactly 1 (phrase + provider err)", auditCount)
	}

	// Invariant 2: existing metadata keys are preserved. The helper
	// must ADD the embedding_error / embedding_status keys, not
	// overwrite the existing provenance / synthesized / source_ids
	// entries that AutoSynthesize set up before the embedding call.
	if _, hasProvenance := metadata["provenance"]; !hasProvenance {
		t.Errorf("helper overwrote provenance metadata; existing keys must survive")
	}
}

// TestApplyEmbeddingFailureToMetadata_AuditMessageContainsProviderError
// pins the audit-message contract independently of the metadata
// mutation. The audit log is the forensic trail — if the message
// doesn't carry the provider error, a future investigator can't tell
// WHY embedding failed without re-running the synthesis.
func TestApplyEmbeddingFailureToMetadata_AuditMessageContainsProviderError(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{"transport error", errors.New("embed: request failed: dial tcp 127.0.0.1:1: connect: connection refused")},
		{"provider 4xx", errors.New("embed: Ollama returned 404: model not found")},
		{"provider 5xx", errors.New("embed: Ollama returned 503: service unavailable")},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Each subtest gets a fresh DM — otherwise the
			// ORDER BY id DESC LIMIT 1 query below picks the previous
			// subtest's audit row.
			dm, err := NewDatabaseManager(t.TempDir())
			if err != nil {
				t.Fatalf("NewDatabaseManager: %v", err)
			}
			defer dm.Close()

			metadata := map[string]interface{}{}
			applyEmbeddingFailureToMetadata(dm, metadata, tc.err)

			// Pull the audit row's message and confirm it carries the
			// exact provider-error text.
			var msg string
			if err := dm.db.QueryRow(`
				SELECT message FROM system_audit_log
				WHERE component = 'synthesis' AND level = 'warn'
				ORDER BY id DESC LIMIT 1
			`).Scan(&msg); err != nil {
				t.Fatalf("audit_log read: %v", err)
			}
			if !strings.Contains(msg, tc.err.Error()) {
				t.Errorf("audit message missing provider error:\n  got:  %q\n  want substring: %q",
					msg, tc.err.Error())
			}
			if !strings.Contains(msg, "synthesis succeeded but embedding failed") {
				t.Errorf("audit message missing canonical phrase: %q", msg)
			}
		})
	}
}

// TestAutoSynthesize_EmbeddingFailureKeepsPersistenceContract is the
// structural assertion that FIX 2's helper is wired into the
// AutoSynthesize orchestration. Without this assertion, a future
// refactor could move the embedding-failure handling out of the
// synthesis path and the helper would still pass its own tests in
// isolation while breaking the load-bearing contract.
//
// The assertion is a source-level structural check: the AutoSynthesize
// function MUST call applyEmbeddingFailureToMetadata inside the
// `if embedErr != nil` branch. This pins the contract at the
// orchestration level without requiring an end-to-end FTS5/LLM drive.
func TestAutoSynthesize_EmbeddingFailureKeepsPersistenceContract(t *testing.T) {
	// Read synthesis_auto.go and confirm the helper is invoked from
	// AutoSynthesize's embedding-failure branch. This is a structural
	// assertion — it's not a runtime test, but it's load-bearing:
	// it pins the integration point so the helper can't be deleted
	// or bypassed by a future refactor.
	const sourceFile = "synthesis_auto.go"
	data, err := readFileForTest(sourceFile)
	if err != nil {
		t.Fatalf("read %s: %v", sourceFile, err)
	}
	src := string(data)

	// The AutoSynthesize function MUST contain the helper call inside
	// the `if embedErr != nil {` branch.
	autoIdx := strings.Index(src, "func AutoSynthesize(")
	if autoIdx < 0 {
		t.Fatalf("AutoSynthesize function not found in %s", sourceFile)
	}
	// Slice from AutoSynthesize onward to avoid matches in helper definitions.
	rest := src[autoIdx:]
	helperIdx := strings.Index(rest, "applyEmbeddingFailureToMetadata(")
	if helperIdx < 0 {
		t.Fatalf("AutoSynthesize does not invoke applyEmbeddingFailureToMetadata — FIX 2 wiring is broken")
	}
	// The helper invocation must be preceded by an `if embedErr != nil`
	// condition in the same function. Scan backward from helperIdx to
	// confirm.
	prefix := rest[:helperIdx]
	lastIfIdx := strings.LastIndex(prefix, "if embedErr != nil")
	if lastIfIdx < 0 {
		t.Errorf("applyEmbeddingFailureToMetadata called outside an `if embedErr != nil` branch")
	}
	// And the call must be inside AutoSynthesize, not in a helper that
	// happens to be defined after the function body.
	lastFuncEnd := strings.LastIndex(prefix, "func ")
	if lastFuncEnd > lastIfIdx {
		t.Errorf("`if embedErr != nil` branch is in a different function than the helper call")
	}
}

// readFileForTest reads a file relative to the current working
// directory. Tests must run with cwd = the package source directory
// (the default for `go test ./...`). Used only by the structural
// assertion TestAutoSynthesize_EmbeddingFailureKeepsPersistenceContract.
func readFileForTest(name string) ([]byte, error) {
	return os.ReadFile(name)
}
