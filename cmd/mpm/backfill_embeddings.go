package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"time"

	mpminternal "github.com/flowbyte-com/mpm-core"
	"github.com/flowbyte-com/mpm-core/config"
	"github.com/flowbyte-com/mpm-core/usererror"
)

// handleBackfillEmbeddings runs the embedding backfill pipeline.
//
// 2026-09-28 extension: --include flag selects which surfaces to
// backfill. The default remains "memories" for backward
// compatibility — operators with scripts that depend on the
// previous behavior see no change. "references" extends the
// backfill to reference_chunks; "all" runs both passes in
// sequence.
//
// The two passes share the same embedding provider (the active
// one at the time of the call) and the same dry-run / refusal
// semantics. Per-chunk embedding failures are logged to
// mirror.jsonl via logEmbeddingFailure; the per-doc reference
// pass uses EmbedReferenceChunks which counts failed chunks
// internally.
func handleBackfillEmbeddings(args []string) int {
	fs := flag.NewFlagSet("backfill-embeddings", flag.ContinueOnError)
	batchSize := fs.Int("batch-size", 50, "Memories per batch")
	dryRun := fs.Bool("dry-run", false, "Count only, don't write embeddings")
	collection := fs.String("collection", "", "Filter by memory collection (empty = all)")
	include := fs.String("include", "memories", "Surfaces to backfill: memories, references, all")
	fs.Usage = func() {
		fmt.Println("Usage: mpm ops backfill-embeddings [flags]")
		fmt.Println("\nFlags:")
		fmt.Println("  --batch-size <n>    Memories per batch (default 50)")
		fmt.Println("  --collection <c>    Filter by memory collection (default: all)")
		fmt.Println("  --include <scope>   memories | references | all (default memories)")
		fmt.Println("  --dry-run           Count only, don't write embeddings")
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
			return 0
		}
		cfg := mpminternal.DefaultEmbeddingConfig()
		// Provider gate is inside runReferenceBackfill; the
		// function returns a refusal message rather than an
		// error code so partial successes in earlier passes
		// are still reported.
		runReferenceBackfill(dm, cfg)
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

// runReferenceBackfill is the reference-chunks pass. Iterates
// every reference doc that has at least one unembedded chunk and
// calls EmbedReferenceChunks, which fills NULL rows in place.
// Per-chunk failures inside EmbedReferenceChunks are counted but
// do not abort the doc — same semantics as the memory pass.
//
// The doc-level iteration is intentionally simple (no batching
// across docs) because the typical corpus is small (tens to
// hundreds of docs) and per-doc embedding already runs in
// sequence. A future enhancement could parallelize across
// docs with a worker pool, but the current shape mirrors the
// chunk-by-chunk loop inside EmbedReferenceChunks and keeps the
// code path easy to reason about.
func runReferenceBackfill(dm mpminternal.CoreDB, cfg *mpminternal.EmbeddingConfig) {
	if cfg.Source == mpminternal.EmbeddingSourceAbsent ||
		cfg.Source == mpminternal.EmbeddingSourceDisabled ||
		cfg.IntentionallyDisabled {
		fmt.Printf("   Refusing reference backfill: embedding provider is %s.\n", providerStateLabel(cfg))
		return
	}
	docIDs, err := listReferencesWithUnembeddedChunks(dm.SQLDB())
	if err != nil {
		fmt.Printf("   Error listing reference docs: %v\n", err)
		return
	}
	fmt.Printf("   Total reference docs with unembedded chunks: %d\n", len(docIDs))
	if len(docIDs) == 0 {
		return
	}
	processed, totalEmbedded, totalFailed := 0, 0, 0
	for _, docID := range docIDs {
		concrete, ok := dm.(*mpminternal.DatabaseManager)
		if !ok {
			fmt.Printf("   Skipping %s: CoreDB is not a *DatabaseManager (reference backfill needs concrete access)\n", docID)
			continue
		}
		embedded, failed, err := concrete.EmbedReferenceChunks(context.Background(), docID)
		if err != nil {
			fmt.Printf("   %s: error %v\n", docID, err)
			continue
		}
		totalEmbedded += embedded
		totalFailed += failed
		processed++
		fmt.Printf("   %s: %d embedded, %d failed\n", docID, embedded, failed)
	}
	fmt.Printf("\n⚡ Reference backfill complete: %d docs, %d chunks embedded, %d failed\n",
		processed, totalEmbedded, totalFailed)
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

func logEmbeddingFailure(id, content string, err error) {
	entry := map[string]interface{}{
		"ts":        time.Now().UTC().Format(time.RFC3339),
		"type":      "embedding_failure",
		"memory_id": id,
		"error":     err.Error(),
	}
	data, _ := json.Marshal(entry)
	var buf bytes.Buffer
	buf.Write(data)
	buf.WriteByte('\n')

	f, openErr := os.OpenFile(config.GetMirrorPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if openErr != nil {
		slog.Warn("embedding-backfill: failed to open mirror log", "error", openErr)
		return
	}
	defer f.Close()
	f.Write(buf.Bytes())
}
