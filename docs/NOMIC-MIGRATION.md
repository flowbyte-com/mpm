# Nomic migration — plan and runbook

Migrates the live store from `all-minilm` (384-dim) to `nomic-embed-text`
(768-dim). Supersedes the "do not migrate yet" recommendation in
[`EMBEDDING-MODEL-COMPARISON.md`](./EMBEDDING-MODEL-COMPARISON.md) §7.

**Decision:** adopt `nomic-embed-text`. Precondition (a) — separating
retrieval from persistence — is satisfied by
[`RECALL-MUTATION.md`](./RECALL-MUTATION.md). Precondition (b), the
query-instruction split, is **deliberately deferred**; see
[§5](#5-what-this-migration-does-not-do).

## 1. What changes

| | before | after |
|---|---|---|
| `memories.embedding` | 146 × 384-dim | 146 × 768-dim |
| `memories.embedding_model` | `ollama:all-minilm` | `ollama:nomic-embed-text` |
| `memories.embedding_dimension` | 384 | 768 |
| `reference_chunks.embedding` (×232) | 384-dim | 768-dim |
| `~/.mpm/mpm_config.json` `profiles.embedding.model` | `all-minilm` | `nomic-embed-text` |

378 rows re-embedded in total. The statement is `UPDATE`-only, over three
columns, keyed by `id`.

## 2. What must not change

`content`, `weight`, `metadata`, `created_at`, `deleted_at`, `collection`,
`tags`, `session_id`, `is_long_term`, `last_accessed_at`,
`reinforcement_count`, `reference_id` — and every row count.

Verified by digesting each row's invariant columns before and after and
comparing the two maps. A row that changes any invariant column, appears,
or disappears fails the migration and triggers an automatic restore.

**The 3 soft-deleted memories are excluded from re-embedding by design.**
They carry no vector and must keep carrying none; leaving them alone is the
correct end state, not an omission. The migration asserts afterwards that
zero soft-deleted memories hold a vector.

**Challenge state is untouched.** 41 memories are challenged. The
migration changes nothing about them, and the digest covers `metadata`,
which is where challenge state lives.

## 3. Ordering, and why

Vectors first, config last.

`VectorMatch` filters on `embedding_model = CurrentEmbeddingFingerprint().Model`
(`internal/core/embeddings.go:235`). Flipping the config before rewriting
the vectors would make every stored row fail that filter: retrieval would
degrade to lexical-only, print `Hybrid Recall` anyway, and look healthy.
Doing it in this order means there is no window in which the store is
internally inconsistent with the config.

The same filter is why §4's preflight is not optional.

## 4. Backup and reversal

**Backup** (SQLite-consistent, taken via the backup API — the live DB is
in WAL mode, so a file copy is not consistent):

```
/home/v/.mpm/backups/mpm.db.pre-nomic-<timestamp>
```

Verified at creation by `PRAGMA integrity_check` and by comparing every
invariant column and every count against the live store. A backup that
does not match the live store is not a backup.

The config file is copied to `mpm_config.json.pre-nomic-<timestamp>` in
the same step that changes it.

**Reversal**, in order:

```bash
cp ~/.mpm/backups/mpm.db.pre-nomic-<ts> ~/.mpm/src/db/mpm.db
rm -f ~/.mpm/src/db/mpm.db-wal ~/.mpm/src/db/mpm.db-shm
cp ~/.mpm/mpm_config.json.pre-nomic-<ts> ~/.mpm/mpm_config.json
# restart MCP servers and the scheduler
```

Reversal needs no re-embedding: the backup holds the 384-dim vectors, and
the restored config makes them match again. This is why the backup is a
full database and not a list of changes.

## 5. What this migration does not do

**No query prefixing.** Nomic's documented convention is
`search_document: ` for indexed text and `search_query: ` for queries.
Measured gain: **+0.022 MRR** (0.783 → 0.804, raw cosine, corpus fixed).

It is deferred, not forgotten. A retrieval gain and a model change that
land together cannot be attributed separately: if the post-migration
benchmark improves, there is no way to tell whether the model earned it or
the prefix did, and the next person to tune the prefix has no baseline to
subtract. Separating them costs one extra benchmark run and keeps the
attribution honest.

**No threshold recalibration.** The contradiction threshold
(`ContradictionCandidateThreshold = 0.85`) is not a ranking constant; it
governs candidate *proposal* only, and after
[`RECALL-MUTATION.md`](./RECALL-MUTATION.md) no ordinary recall can write
a row on its basis. It is recalibration-adjacent work, not migration
work.

**No change to the 0/7 negative-query result.** See §6.

## 6. Known limitation, carried forward unchanged

All four benchmarked models return 10 results for all 7 negative queries.
Nomic does not fix this and is not claimed to.

The cause is structural, in the ranker rather than the model:
`DefaultHybridConfig.RetrievalThreshold` is `-3.0`, a value in log-space
that vector-only rows (cosine ∈ [0,1]) can never reach. There is
effectively no vector similarity floor, so *"no relevant results"* is not
a reachable state on the vector leg for any model.

Migrating does not regress this — it was already 0/7 — and it does not
fix it. Fixing it means adding a calibrated floor to the vector leg and
measuring it, which is a ranker change with its own benchmark, not a
migration side effect. It remains open.

## 7. Runbook

```bash
python3 migrate_nomic.py plan     # read-only: what would change, nothing written
python3 migrate_nomic.py backup   # SQLite-consistent backup + content verification
python3 migrate_nomic.py apply    # re-embed, verify, flip config
```

`apply` aborts before touching anything if `nomic-embed-text` does not
probe at 768 dimensions on this machine. If the invariant comparison fails
after re-embedding, it restores from the backup it took at the start of the
same run and exits non-zero.

**After `apply`, and before any live recall:**

1. Restart long-running processes that cache the embedding configuration.
   This is not advisory. `DefaultEmbeddingConfig()` loads the config behind
   a `sync.Once` (`internal/core/embeddings.go:622`) and caches the resolved
   provider for the life of the process; `ActiveEmbeddingDimension()` caches
   a sampled dimension the same way. An MCP server or scheduler started
   before the config flip therefore keeps a provider named
   `ollama:all-minilm`, keeps computing that fingerprint, and keeps
   filtering new 768-dim vectors out of every query — silently, and while
   still reporting a hybrid search. Short-lived CLI invocations pick up the
   new config on their next run and need nothing.
2. Validate semantic retrieval on an **isolated copy** of the migrated
   store: confirm the fingerprint on the retrieval path, and confirm rows
   are actually vector-sourced rather than merely labelled as such.
3. Only then exercise live recall.