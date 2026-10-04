package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"

	mpminternal "github.com/flowbyte-com/mpm-core"
	"github.com/flowbyte-com/mpm-core/usererror"
)

// handleBackfillEmbeddings runs the embedding backfill pipeline.
//
// 2026-09-28 extensions:
//
//   - --include flag selects which surfaces to backfill. The
//     default remains "memories" for backward compatibility —
//     operators with scripts that depend on the previous
//     behavior see no change. "references" extends the backfill
//     to reference_chunks; "all" runs both passes in sequence.
//
//   - The reference pass is model-aware. It uses
//     RefreshStaleReferenceEmbeddings (canonical) rather than
//     the previous "fill NULL only" logic. Refreshed rows include
//     any with a NULL embedding, any with a "null" or "hash"
//     source, and any whose stored (model, dimension) fingerprint
//     does not match the active provider. The previous CLI
//     command would leave stale rows on disk when the operator
//     switched embedding models; the new command repairs them
//     automatically.
//
//   - --force-reembed re-embeds every reference chunk regardless
//     of fingerprint. Useful for benchmarking model quality
//     changes against the entire corpus. Not required for
//     normal model-mismatch recovery — that is automatic.
func handleBackfillEmbeddings(args []string) int {
	fs := flag.NewFlagSet("backfill-embeddings", flag.ContinueOnError)
	batchSize := fs.Int("batch-size", 50, "Memories per batch")
	dryRun := fs.Bool("dry-run", false, "Count only, don't write embeddings")
	collection := fs.String("collection", "", "Filter by memory collection (empty = all)")
	include := fs.String("include", "memories", "Surfaces to backfill: memories, references, all")
	forceReembed := fs.Bool("force-reembed", false, "Re-embed every reference chunk regardless of stored fingerprint (references pass only)")
	fs.Usage = func() {
		fmt.Println("Usage: mpm ops backfill-embeddings [flags]")
		fmt.Println("\nFlags:")
		fmt.Println("  --batch-size <n>      Memories per batch (default 50)")
		fmt.Println("  --collection <c>      Filter by memory collection (default: all)")
		fmt.Println("  --include <scope>     memories | references | all (default memories)")
		fmt.Println("  --dry-run             Count only, don't write embeddings")
		fmt.Println("  --force-reembed       Force every reference chunk to re-embed (references pass only)")
	}
	if err := fs.Parse(args); err != nil {
		return 1
	}

	// Validate --include. Anything other than the three canonical
	// values is a typo; surface it rather than silently running a
	// partial pass.
	switch *include {
	case "memories", "references", "all":
		// ok
	default:
		return usererror.Errorf(fmt.Sprintf("--include must be one of: memories, references, all (got %q)", *include))
	}

	// Use the shared DM singleton (getDB) instead of opening a fresh
	// connection via mpminternal.NewDatabaseManager(""). The previous
	// code created a separate connection per call, which (a) re-ran
	// the schema migration on every invocation, (b) re-ATTACHed the
	// shared DB unnecessarily, and (c) bypassed the per-component
	// connection ownership pattern that CoreDB Phase 2 introduced.
	// The shared singleton is the canonical entry point: see
	// getDB() in cmd/mpm/handlers.go.
	dm := getDB()
	if dm == nil {
		return 1
	}
	defer dm.Close()

	// Phase 1: memories (default) — always runs unless --include
	// is explicitly "references". Counts and dry-run both report
	// the memory count; the reference phase adds its own report
	// below.
	if *include != "references" {
		total, err := countMemoriesWithoutEmbedding(dm.SQLDB(), *collection)
		if err != nil {
			return usererror.Error("Error counting memories: %v", err)
		}
		fmt.Printf("   Total memories missing embeddings: %d\n", total)

		if *dryRun {
			// Skip the provider gate; dry-run is a count-only probe.
			if *include == "all" {
				refCount, _ := countReferencesWithUnembeddedChunks(dm.SQLDB())
				fmt.Printf("   Total references with unembedded chunks: %d\n", refCount)
			}
			return 0
		}

		if total > 0 {
			cfg := mpminternal.DefaultEmbeddingConfig()
			if code := gateProvider(cfg, "memory backfill"); code != 0 {
				return code
			}
			if err := runMemoryBackfill(dm, cfg, *collection, *batchSize); err != nil {
				fmt.Printf("   Memory backfill failed: %v\n", err)
			}
		} else {
			fmt.Println("   Nothing to do.")
		}
	}

	// Phase 2: references (when --include is references or all).
	if *include == "references" || *include == "all" {
		if *dryRun {
			// Dry-run already returned in the memories branch
			// above; this branch only runs when memories is
			// excluded (--include=references).
			refCount, _ := countReferencesWithUnembeddedChunks(dm.SQLDB())
			fmt.Printf("   Total references with unembedded chunks: %d\n", refCount)
			staleCount, _ := countReferencesWithStaleEmbeddings(dm.SQLDB())
			fmt.Printf("   Total references with stale (model-mismatched) embeddings: %d\n", staleCount)
			return 0
		}
		cfg := mpminternal.DefaultEmbeddingConfig()
		// Provider gate is inside runReferenceBackfill; the
		// function returns a refusal message rather than an
		// error code so partial successes in earlier passes
		// are still reported.
		runReferenceBackfill(dm, cfg, *forceReembed)
	}

	return 0
}

// gateProvider centralises the "is the embedding provider
// configured?" check that both backfill passes used to inline.
// Prints the refusal message and returns an int exit code (1
// when the provider is unusable, 0 when the gate passes). The
// caller can `return gateProvider(...)` from handleBackfill
// directly because the function returns the same int type the
// CLI convention expects.
func gateProvider(cfg *mpminternal.EmbeddingConfig, what string) int {
	if cfg.Source == mpminternal.EmbeddingSourceAbsent ||
		cfg.Source == mpminternal.EmbeddingSourceDisabled ||
		cfg.IntentionallyDisabled {
		return usererror.Errorf(fmt.Sprintf("refusing %s: embedding provider is %s.\nResolve one of:\n  - Run `mpm config profile add <name> --model <id> --base-url <url>` then `mpm config profile set <name> provider custom` to configure.\n  - Set `components[\"embedding\"]` in mpm_config.json to a real profile.\n  - Remove the `\"disabled\"` sentinel from `components[\"embedding\"]`.\n  - Set OLLAMA_ENDPOINT and OLLAMA_MODEL in the environment.",
			what, providerStateLabel(cfg)))
	}
	fmt.Printf("⚡ Embedding backfill using %s\n", cfg.Provider.Name())
	return 0
}

// runMemoryBackfill is the in-memory-pass loop extracted from
// handleBackfillEmbeddings. Split out so the reference pass can
// run after it without duplicating the batch scaffold.
func runMemoryBackfill(dm mpminternal.CoreDB, cfg *mpminternal.EmbeddingConfig, collection string, batchSize int) error {
	total, err := countMemoriesWithoutEmbedding(dm.SQLDB(), collection)
	if err != nil {
		return err
	}
	processed, failed := 0, 0
	offset := 0
	for {
		memories, err := fetchMemoriesWithoutEmbedding(dm.SQLDB(), collection, batchSize, offset)
		if err != nil {
			return err
		}
		if len(memories) == 0 {
			break
		}
		for _, mem := range memories {
			vec, err := cfg.Provider.Embed(mem.Content)
			if err != nil {
				logEmbeddingFailure(mem.ID, mem.Content, err)
				failed++
				continue
			}
			if err := updateMemoryEmbedding(dm.SQLDB(), mem.ID, vec); err != nil {
				logEmbeddingFailure(mem.ID, mem.Content, err)
				failed++
				continue
			}
			processed++
		}
		offset += batchSize
		fmt.Printf("   Memory progress: %d/%d done, %d failed\n", processed, total, failed)
		if len(memories) < batchSize {
			break
		}
	}
	fmt.Printf("\n⚡ Memory backfill complete: %d succeeded, %d failed (of %d total)\n", processed, failed, total)
	return nil
}

// runReferenceBackfill is the reference-chunks pass.
//
// 2026-09-28 update: model-aware. The previous implementation
// filled only NULL rows; the new one refreshes any row whose
// stored (source, model, dimension) fingerprint does not match
// the active embedding provider. The two implementation paths
// are:
//
//   - refresh-only-stale (default): calls
//     RefreshStaleReferenceEmbeddings, which scans every row
//     and re-embeds only the ones that
//     referenceEmbeddingNeedsRefresh flags. The fingerprint
//     columns are written on every successful re-embed so
//     the next pass with the same provider is a no-op.
//
//   - force-reembed (--force-reembed): sets every chunk's
//     embedding to NULL and then re-runs the per-doc embed
//     pass. This is the explicit operator override — useful
//     when the operator wants to compare two model variants
//     against the same corpus, or when a silent embedding
//     bug is suspected.
//
// The dry-run branch above reports the count of stale rows so
// the operator can see the model mismatch before triggering a
// write.
func runReferenceBackfill(dm mpminternal.CoreDB, cfg *mpminternal.EmbeddingConfig, forceReembed bool) {
	if cfg.Source == mpminternal.EmbeddingSourceAbsent ||
		cfg.Source == mpminternal.EmbeddingSourceDisabled ||
		cfg.IntentionallyDisabled {
		fmt.Printf("   Refusing reference backfill: embedding provider is %s.\n", providerStateLabel(cfg))
		return
	}
	concrete, ok := dm.(*mpminternal.DatabaseManager)
	if !ok {
		fmt.Printf("   Reference backfill needs *DatabaseManager; this CoreDB is a different type. Skipping.\n")
		return
	}
	ctx := context.Background()

	if forceReembed {
		// Clear every embedding column so EmbedReferenceChunks
		// (NULL-only path) re-embeds the full corpus. The
		// fingerprint columns are reset to defaults so the
		// needs-refresh logic also fires for any stragglers
		// the clear+re-embed pipeline might miss.
		fmt.Println("   --force-reembed set: clearing all reference embeddings...")
		if _, err := concrete.SQLDB().ExecContext(ctx, `
			UPDATE reference_chunks
			   SET embedding = NULL,
			       embedding_source = 'null',
			       embedding_dimension = NULL,
			       embedding_model = NULL
		`); err != nil {
			fmt.Printf("   Failed to clear embeddings: %v\n", err)
			return
		}
	}

	// Model-aware refresh. RefreshStaleReferenceEmbeddings
	// re-embeds any row whose fingerprint does not match the
	// active provider — that includes NULL embeddings (since
	// the clear in the force branch left them NULL), and any
	// row from a previous model version.
	refreshed, failed, err := concrete.RefreshStaleReferenceEmbeddings(ctx)
	if err != nil {
		fmt.Printf("   Refresh failed: %v\n", err)
		return
	}
	fmt.Printf("\n⚡ Reference backfill complete: %d chunks refreshed, %d failed\n",
		refreshed, failed)
}

// countReferencesWithUnembeddedChunks returns the number of
// reference docs that have at least one NULL-embedding chunk.
// Used by --dry-run and by the in-pass progress reporter.
// Matches the countMemoriesWithoutEmbedding style above (the
// row → Scan pattern rather than inline) so the mpm-lint
// no-check / ctx-plain-missing gate does not flag the
// QueryRow call.
func countReferencesWithUnembeddedChunks(db *sql.DB) (int, error) {
	var count int
	row := db.QueryRow(`
		SELECT COUNT(DISTINCT doc_id) FROM reference_chunks WHERE embedding IS NULL
	`)
	if err := row.Scan(&count); err != nil {
		return 0, err
	}
	return count, nil
}

// countReferencesWithStaleEmbeddings returns the number of
// reference chunks whose (source, model, dimension) fingerprint
// does not match the active embedding provider. Used by
// --dry-run so the operator can see how many rows would be
// refreshed in a real pass.
func countReferencesWithStaleEmbeddings(db *sql.DB) (int, error) {
	// Without an active provider, no row is "stale" by
	// fingerprint comparison — every row matches the
	// zero-valued fingerprint trivially. The dry-run
	// report is meaningful only when a provider is
	// configured; if not, the count is zero and the
	// operator sees "0 stale" alongside the refusal
	// message elsewhere in the dry-run output.
	active := mpminternal.CurrentEmbeddingFingerprint()
	if active.Model == "" {
		return 0, nil
	}
	// Count rows whose stored fingerprint is missing the
	// active model's name. This is a deliberately loose
	// check — it covers "model was never recorded" and
	// "model is different from active". A tighter check
	// would also verify dimension, but dimension is not
	// recoverable from the active provider without a
	// sample embed.
	var count int
	row := db.QueryRow(`
		SELECT COUNT(*) FROM reference_chunks
		 WHERE embedding IS NOT NULL
		   AND (embedding_model IS NULL OR embedding_model != ?)
	`, active.Model)
	if err := row.Scan(&count); err != nil {
		return 0, err
	}
	return count, nil
}

// listReferencesWithUnembeddedChunks returns the doc IDs that
// have at least one unembedded chunk. Order is by created_at
// ASC (oldest first) so the backfill is reproducible.
func listReferencesWithUnembeddedChunks(db *sql.DB) ([]string, error) {
	rows, err := db.Query(`
		SELECT DISTINCT rc.doc_id FROM reference_chunks rc
		JOIN reference_docs rd ON rd.id = rc.doc_id
		WHERE rc.embedding IS NULL
		ORDER BY rd.created_at ASC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ── SQL helpers ───────────────────────────────────────────────────────────────

type memRow struct {
	ID        string
	Content   string
	CreatedAt string
}

func countMemoriesWithoutEmbedding(db *sql.DB, collection string) (int, error) {
	var count int
	query := `SELECT COUNT(*) FROM memories WHERE (embedding IS NULL OR embedding = 'null') AND deleted_at IS NULL`
	args := []interface{}{}
	if collection != "" {
		query += " AND collection = ?"
		args = append(args, collection)
	}
	row := db.QueryRow(query, args...)
	if err := row.Scan(&count); err != nil {
		return 0, err
	}
	return count, nil
}

func fetchMemoriesWithoutEmbedding(db *sql.DB, collection string, limit, offset int) ([]memRow, error) {
	query := `SELECT id, content, created_at FROM memories WHERE (embedding IS NULL OR embedding = 'null') AND deleted_at IS NULL`
	args := []interface{}{}
	if collection != "" {
		query += " AND collection = ?"
		args = append(args, collection)
	}
	query += " ORDER BY created_at ASC LIMIT ? OFFSET ?"
	args = append(args, limit, offset)

	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []memRow
	for rows.Next() {
		var m memRow
		if err := rows.Scan(&m.ID, &m.Content, &m.CreatedAt); err != nil {
			return nil, fmt.Errorf("scanning memory without embedding row: %w", err)
		}
		results = append(results, m)
	}
	return results, rows.Err()
}

func updateMemoryEmbedding(db *sql.DB, id string, vec []float32) error {
	bytes, _ := json.Marshal(vec)
	_, err := db.Exec(`UPDATE memories SET embedding = ? WHERE id = ?`, string(bytes), id)
	return err
}

// ── Logging ───────────────────────────────────────────────────────────────────────

// providerStateLabel renders the EmbeddingConfig's source/state as a short
// human-readable label for the operator-facing refusal message. Helps
// distinguish "operator chose disabled" from "no profile configured."
func providerStateLabel(cfg *mpminternal.EmbeddingConfig) string {
	switch {
	case cfg.IntentionallyDisabled:
		return "intentionally disabled (components.embedding=\"disabled\")"
	case cfg.Source == mpminternal.EmbeddingSourceDisabled:
		return "disabled"
	case cfg.Source == mpminternal.EmbeddingSourceAbsent:
		return "absent (no provider configured)"
	default:
		return fmt.Sprintf("in state %s", cfg.Source)
	}
}

// logEmbeddingFailure records a backfill failure in the mirror journal.
//
// The record is built and written by the core package rather than here, so
// that this file is not a second place that knows how to open mirror.jsonl.
// The design's grep-gate (no writer outside the shared append path) exists
// precisely to stop a command-local OpenFile from becoming a copy of the
// mirror contract that drifts from it.
func logEmbeddingFailure(id, content string, err error) {
	mpminternal.LogEmbeddingFailureToMirror(id, err)
}
