# MPM Changelog

All notable changes to this project are documented here. The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [1.1.0] - 2026-06-19

### Added

- **release:** synthesis engine joins git log with #changelog memories ([687f1b7](https://github.com/example/mpm/commit/687f1b7f2f402c1491303770fd1984a6512132c4))
  > Closes the changelog arc end-to-end. The mpm ops changelog build
  > subcommand gains a --with-synthesis flag that joins git-sourced
  > ChangelogEntry rows with agent-written #changelog memories and
  > emits a unified CHANGELOG.md that has both the commit facts and
  > the agent's prose narrative.
  > 
  > Architecture:
  > 
  >   - internal/changelog_synthesis.go: SynthesizeChangelog pure
  >     function. Input: ChangelogDocument + *sql.DB. Output:
  >     ChangelogSynthesisResult with synthesized document, orphan
  >     list, and matched/unmatched counts. Does NOT mutate the
  >     input document (returns a working copy).
  > 
  >   - cmd/mpm/changelog.go: --with-synthesis flag routes through
  >     runSynthesis helper. When the flag is set, the document is
  >     replaced with the synthesized version, the renderer runs
  >     again on the synthesized data, and a summary line + WARN
  >     are emitted to stderr.
  > 
  >   - internal/changelog.go renderEntry: now reads Body and emits
  >     it as a blockquote per line directly beneath the bullet.
  >     This is the synthesis engine's prose channel. Per-line
  >     prefix handles multi-line prose correctly even when the
  >     body contains nested code blocks or list items.
  > 
  > Orphan handling (3 signals):
  > 
  >   1. WARN to stderr during the build, naming the count and
  >      the most common causes (hallucinated hash, squashed
  >      commit, abandoned branch).
  > 
  >   2. Synthetic ## [orphans] - 2026-06-19 release appended at
  >      the bottom of the document, with each orphan's claimed
  >      commit hash preserved for forensics.
  > 
  >   3. The orphan memory's full body is rendered under the
  >      orphan bullet, so a human reading the file later sees
  >      both the join failure and the prose the agent thought
  >      they were contributing.
  > 
  > Renderer discipline:
  > 
  >   - Each line of Body gets its own '> ' prefix. A single
  >     multi-line prefix would break on nested code blocks or
  >     list items in the prose.
  > 
  >   - Blockquote preserves the visual hierarchy between
  >     immutable commit facts (the bullet) and mutable agent
  >     prose (the blockquote). Reader can scan just the
  >     bullets or expand to read the prose.
  > 
  >   - No truncation, no max-prose-lines flag (YAGNI for v1).
  > 
  > Strict retrospective contract:
  > 
  >   - The join is on commit_hash, never on commit subject,
  >     scope, or any other soft field. The synthesis query
  >     can never invent data.
  > 
  >   - The synthesis path does NOT modify git-sourced entries
  >     that have no matching memory. Their body stays empty,
  >     the renderer emits just the bullet.
  > 
  >   - Orphan memories cannot silently disappear: WARN + section
  >     + body content. Three signals, all from one return value.
  > 
  > Tests (7):
  > 
  >   - TestSynthesize_PlainBullet: no memory matches, body stays
  >     empty, no blockquote emitted.
  >   - TestSynthesize_MergedProse: matching memory fills body,
  >     MPMMemoryIDs populated, blockquote rendered.
  >   - TestSynthesize_OrphanMemory: orphan with non-matching
  >     hash is returned in res.Orphans, no error.
  >   - TestSynthesize_OrphanAttachedAsRelease: the cmd/mpm
  >     helper attaches orphans as a synthetic release; the
  >     renderer emits ## [orphans] with the hash and content.
  >   - TestSynthesize_DoesNotMutateInput: input document
  >     unchanged after synthesis.
  >   - TestFetchChangelogMemories_Unkeyed: a memory with no
  >     #commit:<hash> tag returns in the unkeyed slice
  >     (defensive — should not happen via the MCP tool, but
  >     possible via direct writes).
  >   - TestExtractCommitHashFromTags: 5 table cases for the
  >     helper, including empty input and prefix-only edge.
  > 
  > End-to-end smoke (real MPM DB):
  > 
  >   - Logged a #changelog memory for the current commit
  >     (7ae720b). Ran 'mpm ops changelog build --with-synthesis
  >     --dry-run'. Confirmed: matched count = 1, the matched
  >     entry renders the agent prose as blockquote.
  >   - Logged an orphan with a fake hash (deadbeef). Re-ran.
  >     Confirmed: WARN fires, orphan count = 1, ## [orphans]
  >     section rendered with the deadbeef hash preserved.
  > 
  > The Twist rule held: no hardcoded tool counts in the new
  > code. Help text now uses fs.PrintDefaults() so adding a
  > flag in the future auto-appears in --help without a
  > separate manual update.
  > 
  > This commit closes the changelog arc that started in
  > commit aa46e5c (scaffolding), continued in 7ae720b (MCP
  > tool), and now lands the synthesis engine itself.
- **mcp:** log_to_changelog tool — agents write changelog memories tied to commits ([7ae720b](https://github.com/example/mpm/commit/7ae720b865afeb6d0b2eafa0ff8e30b393f6e8bf))
  > Closes the loop on the changelog arc. Agents can now self-report
  > their work as a structured memory tagged #changelog and
  > #commit:<full-sha-1>. The future synthesis engine joins these
  > memories with the git log to produce a unified CHANGELOG.md that
  > includes both the commit-level facts and the agent's prose
  > narrative.
  > 
  > Strict retrospective contract: every call REQUIRES a full 40-char
  > SHA-1 commit_hash. Empty / short / non-hex / ref-name hashes are
  > all rejected at write time. The strictness is load-bearing — the
  > synthesis engine's join is (commit_hash, mpm_memory_id) and is only
  > unambiguous if both sides agree on the exact hash.  returns the canonical form, so agents have an obvious way to
  > get it right.
  > 
  > Auto-injected tags:
  >   - #changelog             (synthesis engine's filter)
  >   - #commit:<lowercase-hash> (per-commit join key, normalized)
  > 
  > Caller-supplied tags are accepted as-is; the #changelog dedup is
  > case-insensitive so callers cannot accidentally double-tag.
  > 
  > Memory storage:
  >   - collection: 'changelog' (dedicated, not 'memories' — synthesis
  >     can scope cheaply with WHERE collection = changelog)
  >   - weight: 1.0 (permanent, high-importance)
  >   - TTL: '0' (no expiry)
  > 
  > Body format: '[commit: <hash>]\n\n<fact>' — the header line makes
  > the join visible at a glance in raw memory dumps; the synthesis
  > engine does not need to parse it (it uses the tag index) but
  > having it in the body is cheap and aids debugging.
  > 
  > Implementation:
  >   - internal/changelog_mcp.go: LogChangelogEntry method on
  >     DatabaseManager + ChangelogTag / CommitTagPrefix constants.
  >     Testable against a per-test in-memory DM without standing up
  >     an MCP server.
  >   - cmd/mpm-mcp/tools.go: toolLogToChangelog spec + handler
  >     (20th MCP tool). Description mandates aa46e5c0103d232d0eebab13304f79595d4c7320
  >     so agents have the right command in front of them.
  > 
  > Tests (8):
  >   - TestLogChangelogEntry_HappyPath: write succeeds, memory is
  >     queryable with all expected tags and collection.
  >   - TestLogChangelogEntry_RejectsEmptyCommit: strict contract.
  >   - TestLogChangelogEntry_RejectsMalformedCommit: short / non-hex
  >     / too-long / ref names all rejected with explanatory error.
  >   - TestLogChangelogEntry_RejectsEmptyFact: no empty memories.
  >   - TestLogChangelogEntry_DedupesChangelogTag: case-insensitive
  >     dedup, no duplicate tag index entries.
  >   - TestLogChangelogEntry_NilDM: defensive error on nil receiver.
  >   - TestLogChangelogEntry_JoinKeyShape: load-bearing assertion
  >     that the synthesis engine's lookup keys are present.
  >   - TestLogChangelogEntry_SynthesisEngineLookup: end-to-end smoke
  >     — write 3 changelog memories + 1 unrelated, run the synthesis
  >     engine's query, confirm exactly 3 rows.
  > 
  > Next arc: synthesis engine. Join ChangelogEntry.CommitHash with
  > memories WHERE commit:<hash> IN tags, merge prose into the entry's
  > body, emit a unified CHANGELOG.md. The synthesis query is the
  > same one the smoke test already runs; the engine is just the
  > plumbing that ties the rows to the rendered entries.
  > 
  > [commit: 7ae720b865afeb6d0b2eafa0ff8e30b393f6e8bf]
  > 
  > Synthesis engine ships. The ChangelogEntry Body is now populated from agent-written #changelog memories on a per-commit basis; unmatched entries render as plain bullets; orphan memories surface in a dedicated ## [orphans] section at the bottom of the file with a terminal WARN. Renderer wraps Body in blockquote per line so multi-line prose stacks cleanly. --with-synthesis is opt-in so the un-synthesized git-only form is still the default. 6 synthesis tests in internal/changelog_synthesis_test.go. The renderer in internal/changelog.go now reads Body per entry; the synthesis engine is the only producer of non-empty Body in v1, but the path is open for future content types.
  > 
  > [commit: 7ae720b865afeb6d0b2eafa0ff8e30b393f6e8bf]
  > 
  > synthesis engine: orphan path verified, memory logged, WARN fires, ## [orphans] section rendered with the commit hash preserved for forensics. Now do the same for the matched path on the current commit.
- **release:** changelog scaffolding via mpm ops changelog build ([aa46e5c](https://github.com/example/mpm/commit/aa46e5c0103d232d0eebab13304f79595d4c7320))
  > Adds a structured release-notes system. The new subcommand reads
  > git log since the previous tag, parses conventional commits, and
  > emits both CHANGELOG.md (Keep-a-Changelog format for humans) and
  > changelog.json (structured for agents and the future synthesis
  > engine).
  > 
  > Schema is shaped for the synthesis engine that will land in the next
  > arc:
  > 
  >   ChangelogEntry {
  >     CommitHash   string   // git-sourced: authoritative identity
  >     MPMMemoryIDs []string // agent-sourced: join key for synthesis
  >     ...
  >   }
  > 
  > Every entry carries both fields. Git-sourced entries leave
  > MPMMemoryIDs empty (but never nil — []string{} for shape stability);
  > agent-sourced entries populated via the future log_to_changelog MCP
  > tool will fill MPMMemoryIDs. The synthesis engine reconciles both
  > kinds via a (commit_hash, mpm_memory_id) join.
  > 
  > Hand-written release highlights (--release-notes <file>) are rendered
  > as a blockquote at the top of the release. This is where the
  > operator adds the 'why this matters' prose that the git log cannot
  > provide.
  > 
  > Legacy backfill mode (--legacy) dumps all pre-since commits as raw
  > bullet points under ## [1.1.0-legacy] - Legacy Backfill, sorted by
  > commit hash, no prose interpretation. Avoids losing the 113 commits
  > between v1.0.0-hardened and the new tag while keeping the operator
  > effort to a single flag.
  > 
  > Initial release: ## [1.1.0] - 2026-06-19 covers the reference arc
  > we just finished (unify ReferenceDB, chunk-hash diff, embedding
  > split) plus 113 other commits since v1.0.0-hardened. 116 entries
  > total, plus 295 unedited legacy entries.
  > 
  > This is the foundation for the synthesis engine. The next arc
  > builds the log_to_changelog MCP tool that lets agents write their
  > own changelog memories tagged #changelog, and the synthesis engine
  > that merges those memories with the git log into a single
  > authoritative changelog.
  > 
  > Flags:
  >   --since <ref>           Lower bound (default: latest tag)
  >   --until <ref>           Upper bound (default: HEAD)
  >   --release-version <v>   Version string (default: minor bump)
  >   --date <YYYY-MM-DD>     Release date (default: today UTC)
  >   --output <path>         Markdown output (default: CHANGELOG.md)
  >   --json <path>           JSON output (default: changelog.json)
  >   --project <name>        Project name in header (default: MPM)
  >   --legacy                Emit pre-since commits as Legacy Backfill
  >   --repo <path>           Git repo directory (default: cwd)
  >   --dry-run               Print Markdown to stdout; do not write
  >   --release-notes <path>  File of hand-written release highlights
  > 
  > Discovered a global --version flag collision with router.parseFlags
  > (rewrites --version to the bare 'version' token for the global
  > version command); renamed to --release-version.
  > 
  > 9 tests: parse conventional commit, parse non-conventional, empty
  > body, group by section, render markdown structure, render highlight
  > as blockquote, render legacy backfill, git log integration against
  > the real repo, schema shape for synthesis.
  > 
  > Tagged v1.1.0.
- **reference:** embed chunks in separate phase via EmbedReferenceChunks ([914ebf9](https://github.com/example/mpm/commit/914ebf90665964d6f75a4dbc1847f13a83977820))
  > AddReference now stops at chunk inserts — embeddings are filled in
  > a SEPARATE phase via DatabaseManager.EmbedReferenceChunks(ctx,
  > docID). The split is load-bearing:
  > 
  > - AddReference tx stays small/fast. Embedding is a slow provider call
  >   (Ollama local is ~50ms per chunk; cloud APIs are network-bound).
  >   Putting it inside the tx would bloat the WAL, block ingest, and
  >   couple chunk durability to provider availability.
  > 
  > - Embedding is independently retryable. A transient provider outage
  >   leaves embedding NULL on the affected rows and the next pass
  >   fills them in. No chunk inserts roll back.
  > 
  > - Embedding is parallelizable at the caller level. Wrap a loop over
  >   docIDs in a worker pool without rewriting AddReference.
  > 
  > Diff integration (chunks_have content_hash from the previous commit):
  > 
  > - AddReference Updated branch clears embedding=NULL on content change
  >   via ON CONFLICT(id) DO UPDATE. Stale vectors would misroute
  >   semantic search, so re-embedding is forced exactly when content
  >   changed.
  > 
  > - Unchanged branch leaves the existing embedding alone — the load-
  >   bearing win of the diff + embedding split. Re-ingesting a 1000-
  >   chunk manual with 5 changed chunks costs 5 embeds, not 1000.
  > 
  > - Deleted (orphan) chunks are DELETEd, so the embedding column goes
  >   with the row via the same statement.
  > 
  > Schema:
  > 
  > - reference_chunks gains embedding BLOB via SafeMigrations (JSON-
  >   marshalled []float32, matches memories.embedding convention so
  >   vector-search code does not need to special-case the two columns).
  >   CREATE TABLE updated so new DBs include the column from the start.
  > 
  > Caller updates:
  > 
  > - internal/call_helpers.go AddReferenceFromFileWith: calls
  >   EmbedReferenceChunks after AddReference succeeds. Embedding failures
  >   are best-effort (chunk rows are already durable), surfaced as a
  >   count in the response map.
  > 
  > - cmd/mpm simple_cmds.go add-reference CLI: same call after
  >   AddReference. JSON output gains an embedded field; human output
  >   prints 'N chunks, M embedded'.
  > 
  > Tests (6 new):
  > 
  > - TestEmbedReferenceChunksFreshIngest: chunks start NULL, embed pass
  >   fills them, second pass is a no-op (idempotent).
  > - TestEmbedReferenceChunksReIngestSameContent: re-ingest of unchanged
  >   content leaves the embedding byte-for-byte unchanged. The
  >   load-bearing assertion — proves the diff-based bypass actually
  >   saves work.
  > - TestEmbedReferenceChunksReIngestUpdatedContent: AddReference
  >   Updated branch clears embedding; embed pass picks it up; new vector
  >   differs from the original (proves it was regenerated, not cached).
  > - TestEmbedReferenceChunksOrphanDropsEmbedding: orphan chunk row is
  >   deleted entirely, embedding goes with it via the DELETE (not via a
  >   column clear). Kept chunks keep their embeddings.
  > - TestEmbedReferenceChunksEmptyDocID: defensive error on empty input.
  > - TestEmbedReferenceChunksNilDM: defensive error on nil receiver.
  > 
  > Architecture note: this commit confirms the Shelf-vs-Mind decision.
  > References are a shelf — exist, indexed, embedded for search. They
  > are NOT memories, and they do NOT pass through the admission function
  > at ingest time. Admission is a per-consult decision triggered later
  > when an agent extracts a pattern from a reference it has consulted.
  > This was the architectural simplification that cut the second-half
  > scope from 'admission tuning' to nothing — the whole tuning axis
  > goes away once admission is no longer on the ingest hot path.
  > 
  > Build clean, vet zero new warnings, full test suite green.
- **reference:** chunk-hash diff ingest ([95775f2](https://github.com/example/mpm/commit/95775f220c53625abac7a661030583a36404fadb))
  > Re-ingesting a reference that has not changed used to rewrite every
  > chunk row — wasted IO, audit-log noise, and pointless downstream work
  > once per-chunk embedding lands. AddReference now upserts the doc and
  > applies a content-hash-based chunk diff inside a single WithTx tx:
  > 
  > - chunks whose content_hash already exists for the doc are Unchanged
  >   (skipped — their existing row is the truth)
  > - chunks with new content_hash are Inserted
  > - existing chunks whose content_hash is not in the new set are Deleted
  >   (orphan cleanup so a manual that lost a chapter does not keep stale
  >   rows around)
  > 
  > Diff is keyed by content_hash, not chunk id. Old chunks had random
  > GenerateID() ids, so id-based diffs would always classify everything
  > as new regardless of content. content_hash is the actual identity of
  > a chunk.
  > 
  > Substrate:
  > 
  > - Schema: reference_chunks gains content_hash TEXT (SafeMigrations,
  >   NULL for legacy rows; operator can UPDATE to backfill). CREATE
  >   TABLE updated so new DBs include the column from the start.
  > - ComputeChunkID(docID, chunkIndex, contentHash) returns sha256 hex
  >   truncated to 12 chars — deterministic ids for future code that
  >   wants them (current ingest paths generate ComputeChunkID for new
  >   chunks via the updated callers).
  > - ComputeChunkHash(content) = sha256 hex. Aliased to HashContent
  >   with a chunk-level name for call-site clarity.
  > - FindReferenceBySourcePath(db, path) returns the existing doc for
  >   a given source path or (nil, nil) for first ingest.
  > 
  > Behavior change: AddReference no longer returns ErrAlreadyExists on
  > duplicate doc id. It upserts via ON CONFLICT(id) DO UPDATE so the
  > diff can run against existing rows. Callers wanting strict one-shot
  > mode must check existence first (FindReferenceBySourcePath). The
  > ErrAlreadyExists sentinel was removed since it had no callers.
  > 
  > Caller updates:
  > 
  > - internal/call_helpers.go AddReferenceFromFileWith: look up doc by
  >   source path first; short-circuit when content_hash matches (return
  >   unchanged=true); otherwise reuse the existing doc id so the chunk
  >   diff runs in place.
  > - cmd/mpm/simple_cmds.go add-reference CLI: same lookup + short-
  >   circuit + id reuse. JSON output gains an "unchanged" flag for the
  >   no-op case.
  > 
  > Tests (6 new):
  > 
  > - TestComputeChunkIDStable: same inputs → same id; different
  >   chunkIndex / docID / content → different id.
  > - TestDiffChunksFreshIngest: new content, old rows all become
  >   orphans (full delete + insert cycle).
  > - TestDiffChunksReIngestSameContent: same content → all Unchanged,
  >   zero inserts, zero deletes.
  > - TestDiffChunksReIngestWithChanges: mix of Unchanged + Updated +
  >   Inserted + Deleted — all four diff branches fire correctly.
  > - TestAddReferenceReIngestAppliesDiff: end-to-end through
  >   AddReference. Verifies orphan deletion, content_hash refresh on
  >   updated rows, doc.total_chunks reflects new chunk count.
  > - TestFindReferenceBySourcePath: lookup, missing path returns
  >   (nil, nil), empty path rejected.
  > 
  > Build clean, vet zero new warnings, full test suite green.
  > 
  > Migration cost: existing libraries with chunks lacking content_hash
  > cannot be matched by content, so the first re-ingest after upgrade
  > produces a delete-everything + insert-everything cycle on each doc
  > (operator can UPDATE reference_chunks SET content_hash = <sha256 of
  > content> to backfill in place and skip the churn). Subsequent
  > re-ingests are idempotent and cheap.
- **admission:** LLM-based admission function with single-model chain ([3b5bf09](https://github.com/example/mpm/commit/3b5bf0947f917417578ed678fe724096887c8e25))
  > Implements the admission function described in decision 9aa0ee2c6de8492a:
  > LLM-based, autonomous, single-model per call, decay-bounded. The admitting
  > model produces both the admit/reject decision and the justification chain
  > — no two-model pipelines (rationale in the decision: same model, one call,
  > one auditable chain).
  > 
  > Architecture (per decisions 355fb7f381e63199 / 4f1c1fbc41a7a765 /
  > b90e4fe54507c3b9 / bab54dba5a768490 / 9aa0ee2c6de8492a):
  > - Trigger: chunk with 3+ retrieval hits across 2+ distinct queries
  > - Single LLM call per candidate with admissionSystemPrompt
  > - JSON response: {admit, content, justification, confidence, tags, reason}
  > - On admit: write memory via SaveMemoryWithContext with admission model
  >   stamped in metadata.provenance.model
  > - On reject: write a row to admission_log with the reason
  > - v is NOT in the loop (decision: 'ability to review, not need'); the
  >   admission_log + memory metadata are the audit surfaces
  > 
  > New code:
  > - internal/admission.go (NEW): admissionSystemPrompt, admitResult,
  >   admitChainEntry, AdmissionCandidate, SynthClient.EvaluateCandidate.
  > - internal/web_db.go: FindAdmissionCandidates, RecordAdmissionOutcome.
  > - internal/schema.go: admission_log table + 3 indices (doc_id, admit, created_at).
  > - internal/call_helpers.go: ActiveContext now takes Model and Agent
  >   parameters; default Agent is mpm_call but admission passes mpm_admission.
  > - cmd/mpm/simple_cmds.go: handleRefAdmit, helper loadActiveForAdmission /
  >   enrichCandidate, new 'mpm kb reference admit' subcommand.
  > - internal/synthesize.go: parseResponseBody now scans for type=text
  >   blocks; previously returned empty when Anthropic-style responses led
  >   with a thinking block. Same fix benefits synthesis path.
  > 
  > Smoke test (3 candidates from prior retrieval activity):
  >   admitted: 1, rejected: 2, errors: 0
  >   admitted memory: weight 7 (=int(0.75*10), confidence 0.75)
  >   provenance.model: MiniMax-M2.7, agent: mpm_admission
  >   justification: 4 entries (active_project, existing_theory,
  >     capability, novel_pattern) with strength 0.6-0.9
  > 
  > Re-admission guard: FindAdmissionCandidates excludes chunks whose first
  > 80 chars match a memory_revisions row from the last 7 days. Prevents
  > re-evaluation of the same content; the existing memory's reinforcement
  > is the right signal that the admission is still in the system.
  > 
  > Build clean, all internal + cmd tests pass.
  > 
  > The admission function is now an autonomous part of the system. The
  > decay path (last_accessed_at, reinforcement_count, weight) is the
  > safety net; bad admissions decay, good ones reinforce.
- **reference:** add reference_interactions audit table ([222d367](https://github.com/example/mpm/commit/222d367ccfd19a02d8473829bca93710fe8147d7))
  > Wires the third primitive from decision b90e4fe54507c3b9 into the
  > live system. The reference_interactions table is the substrate the
  > admission function reads to know which references have been used, in
  > what context, and how often. Without it, the reference-to-memory path
  > is not observable.
  > 
  > Schema (internal/schema.go):
  > - reference_interactions table: id, doc_id, chunk_id, query, search_kind,
  >   rank, score, created_at. Foreign key to reference_docs with cascade.
  > - Three indices: doc_id, chunk_id, query.
  > 
  > Recording (internal/web_db.go):
  > - SearchReferenceChunks now records one row per chunk surfaced.
  > - BM25 score is captured when FTS5 is the search path.
  > - Rank is 1-based position in result set.
  > - Queries shorter than 3 chars are filtered (likely exploratory clicks).
  > - Recording errors are swallowed: failure to record must not break
  >   search results; the audit table is not on the critical path.
  > 
  > Query API:
  > - GetRecentInteractions(limit): N most recent, joined with doc title
  >   and import_reason. Used by admission function for prioritization.
  > - GetInteractionsForDoc(docID, limit): all interactions for one doc,
  >   reverse chronological. Used to evaluate reference's active use.
  > - GetMostUsedReferences(limit): aggregated by hit count + distinct
  >   query count. Used to prioritize high-value references.
  > 
  > CLI (cmd/mpm/simple_cmds.go):
  > - mpm kb reference interactions [--doc <id>] [--limit N]: recent events
  > - mpm kb reference used [--limit N]: top references by hit count
  > 
  > Tests (internal/web_db_interactions_test.go):
  > - TestReferenceInteractionsTableExists: table created by InitSchema
  > - TestReferenceInteractionRecordAndRead: round-trip record+read
  > - TestReferenceInteractionFilterShortQuery: 2-char query not recorded
  > - TestReferenceMostUsedAggregation: doc with more hits ranks first
  > - All four pass; uses NewDatabaseManagerForDB with temp DB to avoid
  >   polluting the live MPM database.
  > 
  > Smoke test:
  > - 94 interactions recorded across 9 distinct queries
  > - 14 distinct docs have been used
  > - Top reference: claudecode_ses_80019238 (81 hits, 6 distinct queries)
  >   — the same load-bearing transcript flagged in Phase 1.5 review
  > 
  > This is the audit trail. The admission function (Phase 3) will read
  > from this table to know which references are candidates for memory
  > extraction.
- **reference:** add import_reason field for admission justification ([7ab8971](https://github.com/example/mpm/commit/7ab89718d7069254c4aaf38f2dae706e8e522924))
  > Wires the import_reason primitive from decision b90e4fe54507c3b9 into
  > the reference storage layer. The import_reason is the seed of the
  > admission justification chain — written at ingest time, before the
  > chain exists, so the reference layer is organized by anticipated
  > relevance rather than being a uniform shelf.
  > 
  > Schema changes:
  > - reference_docs.import_reason TEXT column added (live schema in
  >   internal/schema.go + dead-but-kept-in-sync path in reference_new.go)
  > - ALTER TABLE migration added to SafeMigrations so existing DBs pick
  >   up the column on next init
  > - dm.AddReference (web_db.go) writes the column on insert
  > - dm.ListReferences and dm.GetReference (web_db.go) read it back
  > - handleRefAdd parses new --reason flag, passes it through to doc
  > - handleRefList displays import_reason in both human and JSON output
  > 
  > Verified end-to-end:
  > - mpm reference add <file> --reason '...' → import_reason stored
  > - mpm kb reference ls --json → import_reason in output
  > - mpm kb reference ls (human) → reason shown under each ref
  > - Existing 103 phase-1-5 references backfilled with import_reason
  >   based on their source tag (claudecode-curated, claudecode, opencode)
  > - All internal/ and cmd/mpm/ tests pass
  > 
  > This makes the reference layer observable at admission time, which
  > is what the connection-driven admission criterion (355fb7f381e63199)
  > needs to evaluate proposed memories against. Without import_reason,
  > the system cannot tell whether a reference was anticipated to be
  > relevant, only that it exists.
- **directives:** wire Epistemology Engine behavioral rules ([b41fba9](https://github.com/example/mpm/commit/b41fba97dbccc7841759733ca777f4424e06325b))
  > Two Prime Directives (loaded into system prompt via read_directives):
  > - Challenge Rule: when terminal output, test failures, or user feedback
  >   contradicts an existing memory/theory/decision/lesson, log it as
  >   evidence with type=challenge rather than overwriting the artifact.
  > - Theory Validation Rule: resolving a pending theory or fixing a bug
  >   requires evidence (type=test/reproduction/decision_outcome), not
  >   just a conclusion.
  > 
  > Two Mode sections (loaded contextually based on agent's current mode):
  > - debugging.md: query_confidence_history on suspicious memories; log
  >   the root cause as evidence before writing the patch.
  > - research.md: list_evidence before overturning a prior architectural
  >   decision; log a challenge against the old decision if proceeding.
  > 
  > Prime/Mode split follows the existing architecture: universal rules in
  > the directives collection (always-loaded), contextual rules in mode
  > files (loaded only when the agent enters that mode).
  > 
  > Verified via mpm call read_directives: both directives are present
  > in the collection and will be loaded into the agent's system prompt.
  > 
  > Co-Authored-By: Claude Opus 4.7 <noreply@anthropic.com>
- **openclaw:** wire add_evidence, list_evidence, query_confidence_history ([29b6dca](https://github.com/example/mpm/commit/29b6dca0edd87388bc9022613da1d934ad8b614e))
  > The Epistemology Engine backend (Go) has been complete since the
  > foundation plan shipped, but the LLM-facing surface in the OpenClaw
  > plugin was the missing layer — without these tools registered, an
  > agent can only observe confidence scores it can't influence.
  > 
  > This wires the three v1 evidence/confidence tools into the OpenClaw
  > plugin alongside the existing 18 tools:
  > 
  >   - add_evidence: log a single observation/test/reproduction/challenge/
  >     decision_outcome/external_reference against an artifact. Triggers
  >     an atomic INSERT + RecomputeConfidence transaction on the backend.
  >   - list_evidence: full chronological ledger of evidence on an artifact.
  >   - query_confidence_history: confidence timeline with optional limit
  >     (backend defaults to 10; schema description reflects this).
  > 
  > Two schema fixes applied vs. the original drafts:
  > - query_confidence_history.limit: schema says "Defaults to 10" to match
  >   GetConfidenceForArtifact's backend default (was 50 in draft).
  > - additionalProperties: false on all three schemas, matching the
  >   defensive convention of every other schema in this file.
  > 
  > End-to-end verified: built plugin → rebuilt MPM binary → ran all three
  > tools against a fresh test memory and got back valid confidence /
  > evidence / history JSON.
  > 
  > Co-Authored-By: Claude Opus 4.7 <noreply@anthropic.com>
- **evidence:** atomic INSERT + recompute via DBNode + WithTx ([c90c0c0](https://github.com/example/mpm/commit/c90c0c0071ff1b7913982f065f2d45d9a706d71c))
  > Previously AddEvidence did INSERT then RecomputeConfidence as separate
  > auto-commit statements. If the process died between them, or if the
  > recompute failed (e.g., CHECK constraint violation), the evidence row
  > remained as an orphan with stale confidence.
  > 
  > Refactor:
  > - DBNode interface (ExecTracked, QueryTracked, QueryRowTracked) lets a
  >   function run against either *DatabaseManager or a transaction.
  > - txNode wraps *sql.Tx with identical telemetry to DatabaseManager.
  > - WithTx(fn) runs fn inside a transaction; commits on nil, rolls back
  >   on error or panic (panic is re-raised).
  > - RecomputeConfidence and helpers now take DBNode.
  > - AddEvidence wraps INSERT + recompute in dm.WithTx.
  > 
  > Regression test:
  > - TestEvidenceStore_WithTxRollsBackOnRecomputeFailure forces a
  >   confidence_history.trigger CHECK violation inside a WithTx and
  >   verifies the evidence row, confidence column, and history table
  >   all roll back.
  > 
  > Co-Authored-By: Claude Opus 4.7 <noreply@anthropic.com>
- **migration:** backfill initial confidence for existing rows ([e762902](https://github.com/example/mpm/commit/e762902cb3c4f01a4abe5531023716ae28d5b9ab))
- **idle_dream:** add confidence decay cycle for time-dependent recompute ([dd5e660](https://github.com/example/mpm/commit/dd5e660dca42d396430adbed794f94426b418a5b))
- **call:** add_evidence, list_evidence, query_confidence_history tools ([0771c09](https://github.com/example/mpm/commit/0771c092eec1233f9e3ce3416bddb5aa542ec320))
  > Expose the v1 confidence/evidence foundation over the universal `mpm
  > call` machine interface so OpenClaw agents can add evidence, list it,
  > and inspect the confidence timeline. Five new tools are registered in
  > toolRegistry: add_evidence, list_evidence, query_confidence_history,
  > show_confidence, and recompute_confidence. All use the established
  > internal.NewDatabaseManager("") pattern from the rest of call.go.
  > 
  > Co-Authored-By: Claude Opus 4.7 <noreply@anthropic.com>
- **cli:** mpm ops confidence show/recompute ([127d22b](https://github.com/example/mpm/commit/127d22bf732bf8686a60377457682b218ddd0cef))
- **cli:** add mpm evidence list command ([9872ab9](https://github.com/example/mpm/commit/9872ab959118f92b9a292f4aa26fe3a89d390dfa))
- **cli:** add mpm evidence add command ([316de24](https://github.com/example/mpm/commit/316de2427bf02828fb3130efe97f0bcce889ae19))
- **structs:** add retrieval_priority/importance/confidence to Memory and Lesson ([4efdb99](https://github.com/example/mpm/commit/4efdb999332d59c6aa9ac01c27ceb8e285457143))
  > Add the v1 evidence/confidence fields to the Memory and Lesson structs, set
  > initial values in the AddMemory and AddLesson producers using the new
  > InitialConfidence(artifactType) helper, and write them through the INSERTs
  > so they persist with the row.
  > 
  > Co-Authored-By: Claude Opus 4.7 <noreply@anthropic.com>
- **evidence:** add evidence store, recompute function, and trigger chain ([4bd6e38](https://github.com/example/mpm/commit/4bd6e3896414933dfedada5576e8527b1e3e0b03))
- **schema:** add confidence/evidence foundation tables and views ([975f04c](https://github.com/example/mpm/commit/975f04c7589c567c11a24f5cdc17dee5800f4b9a))
- **confidence:** add pure-Go confidence math ([ecfacee](https://github.com/example/mpm/commit/ecfacee14c3ac61bab560cb5288889dd94c3d249))
- **evidence:** add v1 evidence type registry (Task 1) ([b52f81c](https://github.com/example/mpm/commit/b52f81cc2cf0d61e0025e0e745d4e9d1acf15064))
  > Single source of truth for the six v1 evidence types and their default
  > strengths. Consumed by AddEvidence (later task) to fill in `strength`
  > when callers don't override it.
  > 
  > - observation: 0.4
  > - test: 0.7
  > - reproduction: 0.85
  > - challenge: -0.6 (negative — evidence against the artifact)
  > - decision_outcome: 0.95
  > - external_reference: 0.6
  > 
  > Pure Go, no DB. Spec: docs/superpowers/specs/
  > 2026-06-16-confidence-evidence-foundation-design.md
- **route:** wire mpm route top-level command into CommandRouter ([456bbdd](https://github.com/example/mpm/commit/456bbdd067bd2bb1c6150c398965d78d3efd7ed2))
  > Reads prompt from positional arg or stdin (JSON or literal). Always
  > exits 0; any failure path produces no output so the Claude Code hook
  > can never block the user. Stderr noise is gated on TTY detection.
  > 
  > Co-Authored-By: Claude Opus 4.7 <noreply@anthropic.com>
- **call:** add route tool — JSON-RPC path for OpenClaw/Hermes ([9fd9cca](https://github.com/example/mpm/commit/9fd9cca0bd22cd7f2b7db723d6e667b66bb6fe14))
  > Mirrors cmd/mpm-mcp/tools.go:toolRoute but exposed via the mpm call
  > interface for non-MCP consumers. Returns RoutingReport JSON, surfaces
  > real errors (unlike mpm route text which silently degrades).
  > 
  > Co-Authored-By: Claude Opus 4.7 <noreply@anthropic.com>
- **route:** add main renderer — router invocation + system-reminder ([e9141b9](https://github.com/example/mpm/commit/e9141b99f6439a9792297c34366137c4f59c3ad4))
  > Loads internal.Router for the resolved workspace, evaluates prompt,
  > reads selected mode/persona files, and renders a <system-reminder>
  > block. Graceful fallback on any failure returns empty (not error) so
  > the hook can never block the user.
  > 
  > Co-Authored-By: Claude Opus 4.7 <noreply@anthropic.com>
- **route:** add 9500-char length cap with persona-priority truncation ([ca72782](https://github.com/example/mpm/commit/ca7278240df798f37f9e0a8633cc0229c0fd1039))
  > Mode (operational rules) takes precedence over persona (voice/tone).
  > Persona truncated first; if mode alone still exceeds 9000 chars, truncate
  > mode with a generic marker. Stays under Claude Code's 10K hook stdout cap.
  > 
  > Co-Authored-By: Claude Opus 4.7 <noreply@anthropic.com>
- **route:** add opt-out checks (empty, /noroute, MPM_ROUTE=off) ([c6449a1](https://github.com/example/mpm/commit/c6449a15e0748acfb27aa4ef297e17ada937045f))
  > Checked in fixed precedence: empty > /noroute > env. Reason returned
  > for logging/diagnostics. Env-var lookup is parameterized for testing.
  > 
  > Co-Authored-By: Claude Opus 4.7 <noreply@anthropic.com>
- **route:** add prompt extraction (arg / JSON stdin / literal stdin) ([4a8a363](https://github.com/example/mpm/commit/4a8a363e12b1060a91a7b5cca153961822c8a9d8))
  > Precedence: positional arg > JSON stdin > literal stdin. Malformed JSON
  > falls back to literal stdin so the binary remains usable in pipes.
  > 
  > Co-Authored-By: Claude Opus 4.7 <noreply@anthropic.com>
- **route:** add workspace resolution helper for mpm route CLI ([dce0a46](https://github.com/example/mpm/commit/dce0a46565fd73b7ce877d59c5900eb791fb9aac))
  > Uses MPM_ROUTE_WORKSPACE env var with '.' fallback. Distinct from
  > MPM_WORKSPACE so the hook's workspace doesn't leak into other MPM
  > contexts (MCP server, mpm call, etc.).
  > 
  > Co-Authored-By: Claude Opus 4.7 <noreply@anthropic.com>
- **mpm-mcp:** native Go MCP server, all 18 tools, provenance injection ([ee98188](https://github.com/example/mpm/commit/ee981889662ea9849c9e9cc43db3d63cab02794e))
  > Full migration from Python MCP wrapper to native Go binary:
  > 
  > - Add cmd/mpm-mcp/: main.go + tools.go (18 MCP tools, mark3labs/mcp-go)
  > - Wire MPM_ACTIVE_MODE/MPM_ACTIVE_PERSONA env into ActiveContext on startup
  > - Refactor cmd/mpm/call.go to dispatch to internal dm methods
  > - Add internal/call_helpers.go: 14 high-level dm methods shared by CLI and MCP
  > - Add internal/wake_context.go, cmd/mpm/{ingest,stream,web,versioning_cmds}.go
  > - Add docs/superpowers/plans/2026-06-15-mcp-tool-port.md
  > - Fix schema: add source_db/source_id/promoted_at to memories table
  > - Drop legacy Python plugin (claudecode-mpm-plugin/) and .claude/mpm-mcp/
  > - Clean up gitignore, remove ghost db at cmd/mpm/src/db/mpm.db
  > 
  > Build: clean, go vet clean, go test -race ./... green.
- **mpm-mcp:** port all 18 tools to native Go MCP server ([6d34260](https://github.com/example/mpm/commit/6d3426050f3af176435fee66ff92aef3d67981a6))
  > - tools.go defines all 18 tools via mcp.NewTool with descriptions
  >   and arg schemas copied from opencode-mpm-plugin/src/index.ts
  > - Each handler extracts args via type assertion, calls the
  >   corresponding dm method, wraps the result in mcp.NewToolResultText
  > - main.go now calls RegisterAllTools(s, dm) instead of inline
  >   read_wake_context registration
  > 
  > Co-Authored-By: Claude Opus 4.7 <noreply@anthropic.com>
- **claudecode-mpm-plugin:** add install.sh (symlink/copy/uninstall) ([b385b8b](https://github.com/example/mpm/commit/b385b8b03f61d1c8a50056745696ab84ee2f4a63))
- **claudecode-mpm-plugin:** add MPM skill for agent workflow ([441b3cf](https://github.com/example/mpm/commit/441b3cf8358296d4c00e2c99efb1d5ac97840de4))
- **claudecode-mpm-plugin:** add .mcp.json template ([696d815](https://github.com/example/mpm/commit/696d81577ff29bacdac58fc423a5efdc19db7c35))
- **claudecode-mpm-plugin:** wire query_long_term_memory to run_mpm ([c8f2066](https://github.com/example/mpm/commit/c8f206652f9ce8a843d83b9c81e304de7f2500a6))
- **claudecode-mpm-plugin:** wire read_wake_context to run_mpm ([d18054b](https://github.com/example/mpm/commit/d18054bd3758d78a4799c9dcb3cee677e633b6a6))
- **claudecode-mpm-plugin:** scaffold FastMCP server with 2 tool stubs ([48b17d6](https://github.com/example/mpm/commit/48b17d65dac1ffa19db406d20d5acfa5777601a1))
- **claudecode-mpm-plugin:** add Pydantic input models for first 2 tools ([3c64662](https://github.com/example/mpm/commit/3c646620f9a07452c864f0028f04501124c0b4db))
- **claudecode-mpm-plugin:** enforce MAX_BUFFER post-read cap in run_mpm ([f06670e](https://github.com/example/mpm/commit/f06670e77c279993e40d6ae3c887e1c7ef339227))
  > Code review noted: MAX_BUFFER (10 MiB) was defined and parse_mpm_result
  > already had the wake_context_truncated branch (exit 125 + 'output
  > exceeded'), but run_mpm never produced that exit code. A runaway mpm
  > call could OOM the host.
  > 
  > Added a post-read check on stdout length. If stdout exceeds MAX_BUFFER,
  > return exit_code=125 with '[output exceeded <N> bytes]' stderr. This
  > is best-effort: true OOM prevention would require streaming reads
  > with manual stderr drain, but the downstream signal is sufficient for
  > parse_mpm_result to flag wake_context_truncated to the agent.
- **claudecode-mpm-plugin:** implement run_mpm with timeout clamp and FileNotFoundError handling ([fb3eeb6](https://github.com/example/mpm/commit/fb3eeb6b020f6fe807150540d0f9cd0fe2f8a247))
- **claudecode-mpm-plugin:** implement debug_log ([7406d66](https://github.com/example/mpm/commit/7406d663e8025ee352cdc6b46f4cef848d6588f8))
- **claudecode-mpm-plugin:** implement format_age ([d0b561c](https://github.com/example/mpm/commit/d0b561c58c6b6d84bf7baba228b2618dcfac3761))
- **claudecode-mpm-plugin:** implement parse_mpm_result error paths ([4557af9](https://github.com/example/mpm/commit/4557af981520e32cc3ee2c9133b0cafc482b5953))
- **claudecode-mpm-plugin:** implement parse_mpm_result happy path ([65c1b4f](https://github.com/example/mpm/commit/65c1b4f3ce553b76e0680239ddf35a38a422f6b5))
- **claudecode-mpm-plugin:** add requirements and venv setup ([f502d69](https://github.com/example/mpm/commit/f502d694f43d933cfe98cbd2f7dbeb69ecbedd38))
- **claudecode-mpm-plugin:** scaffold dir and .gitignore ([e61e528](https://github.com/example/mpm/commit/e61e5281af3438e3a73d7fe613040849268f6a7c))

### Changed

- **reference:** unify ReferenceDB into DatabaseManager ([7623c76](https://github.com/example/mpm/commit/7623c765bdc179e4873d62164dddc8ce2c3e3f19))
  > The legacy *ReferenceDB struct opened its own *SQLiteConnection
  > separate from DatabaseManager, while every production reference write
  > already went through *DatabaseManager.AddReference. Two write surfaces
  > sharing a schema was the trapdoor behind the legacy-JSON-vs-chunked-
  > SQLite divergence bug fixed earlier today (commit 09d0de6).
  > 
  > This collapses the second surface:
  > 
  > - Delete ReferenceDB struct, NewReferenceDB constructor, and all
  >   methods (Init, AddReference, AddChunk, GetReference, GetChunksByDocID,
  >   ListReferences, DeleteReference, GetReferenceStats, GetReferenceCount).
  > - Delete *ReferenceStore.MetadataDB field — severs the trapdoor path
  >   without removing the legacy JSON store, which v wants kept as a
  >   transitional read/migration surface.
  > - Delete *ReferenceStore.GetReferenceCount (only delegated to
  >   MetadataDB; callers needing the SQLite count go through
  >   internal.CountReferences(*sql.DB)).
  > - Delete dead getReferenceStore() and getStatusCounts() in
  >   cmd/mpm/handlers.go (had no other callers).
  > - Move AddReference + DeleteReference from web_db.go into a new
  >   internal/reference_db.go and route them through dm.WithTx(fn) so
  >   they inherit: watchdog.jsonl telemetry via ExecTracked, automatic
  >   panic recovery, and the single canonical tx lifecycle shared by
  >   every other DatabaseManager multi-statement op.
  > - New internal/reference_query.go: read-side free functions taking
  >   *sql.DB (GetReferenceDoc, ListReferenceDocs, GetReferenceChunksByDocID,
  >   CountReferences, GetReferenceStats). Matches the existing
  >   ShredMemory / DeleteByID / ShredSession / ShredTopic family.
  > - Tests rewired via newTestRefDM(t): per-test unique in-memory SQLite
  >   DSN (file:ref_<hex>?mode=memory&cache=shared) wrapped in
  >   NewDatabaseManagerForDB + InitSchema. Each test owns its shared
  >   cache — no cross-test contamination, no disk.
  > - DatabaseManager.AddReference wraps ErrAlreadyExists on duplicate id
  >   (same typed sentinel ReferenceDB used). Chunks use
  >   ON CONFLICT(id) DO NOTHING so partial-pipeline retries are safe.
  > - DatabaseManager.DeleteReference fans out to reference_chunks +
  >   reference_interactions + admission_log in one tx.
  > 
  > Net: -446 lines (294 added, 740 deleted) across 10 files.
  > 
  > Production callers unchanged: cmd/mpm/simple_cmds.go, call_helpers.go,
  > recall.go, cmd/mpm-mcp/tools.go all already used DatabaseManager
  > methods. Zero call-site edits.
  > 
  > Build clean, vet zero new warnings, full test suite green.
  > 
  > Also: catch src/db/watchdog.jsonl in .gitignore (runtime artifact
  > written on every query via DatabaseManager.logWatchdog — was previously
  > untracked).
- **reference:** consolidate schema + harden ReferenceDB writes ([09d0de6](https://github.com/example/mpm/commit/09d0de6b36de84b9c37e0e07dd735bcedcc14628))
  > Carves the reference library schema out of BaseTables into a named
  > slice (ReferenceTables + ReferenceIndexes) so the unified
  > DatabaseManager startup, the legacy MemoryStore startup, and the
  > isolated ReferenceDB.Init() all run the same CREATE TABLE statements.
  > Eliminates the drift where ReferenceDB had a stale inline subset
  > that lacked reference_interactions, admission_log, ON DELETE CASCADE
  > on reference_chunks, and the content / content_hash / import_reason
  > columns the canonical schema defines.
  > 
  > ReferenceDB changes:
  > 
  >   Init():     stripped inline SQL. Now runs ReferenceTables and
  >               ReferenceIndexes from schema.go — same source as the
  >               production DatabaseManager path.
  > 
  >   AddReference: dropped INSERT OR REPLACE. Uses plain INSERT.
  >               Returns ErrAlreadyExists (new package-level sentinel)
  >               on PK conflict. Writes all schema columns including
  >               content, content_hash, and import_reason — previously
  >               these were present in the schema but AddReference
  >               omitted them, masking a real divergence bug under the
  >               old inline-Init() schema.
  > 
  >   GetReference: collapsed to a single SELECT covering id, title,
  >               file_path, source_type, tags, content, content_hash,
  >               import_reason, total_chunks, last_indexed, created_at.
  >               The previous two-query path silently dropped
  >               import_reason on read. json_group_array was tried first
  >               but it wraps the stored JSON string in a second array
  >               and double-escapes the inner quotes; a plain column
  >               select avoids the tax and is the correct single-trip
  >               path for a single-row lookup.
  > 
  >   AddChunk:   INSERT ... ON CONFLICT(id) DO NOTHING. Re-running an
  >               ingest pipeline against a partially-populated chunk
  >               table no longer duplicates rows or fails on the
  >               primary-key constraint.
  > 
  >   DeleteReference: wrapped in sql.Tx. Deletes reference_chunks,
  >               reference_interactions, admission_log, and the doc row
  >               atomically. Without the transaction, a failure between
  >               any two deletes would leave orphans — CASCADE only
  >               covers chunks, not the audit tables.
  > 
  > Schema gap fix in schema.go: added source_path column to
  > reference_docs (struct field name, previously unread on the
  > canonical schema). content is now TEXT NOT NULL DEFAULT '' so
  > legacy callers that do not supply content still work under the
  > NOT NULL constraint.
  > 
  > Tests added in internal/reference_test.go:
  > 
  >   TestAddReferenceRejectsDuplicate
  >     Locks in ErrAlreadyExists: the new sentinel must be wrapped in
  >     the error so callers can errors.Is() it. Title must be preserved
  >     on conflict — no silent overwrite.
  > 
  >   TestAddChunkIdempotent
  >     Re-running AddChunk with the same id must be a no-op, not a
  >     duplicate insert.
  > 
  >   TestDeleteReferenceAtomic
  >     Seeds a chunk + audit row, deletes the doc, verifies both
  >     vanish and the connection is still usable afterwards (no
  >     leftover tx state).
  > 
  >   TestGetReferenceReturnsImportReason
  >     Guards the regression where the old two-query path dropped
  >     import_reason on read. Also asserts tags survive the single-
  >     trip path.
  > 
  > go build ./...              clean
  > go vet ./internal/...       clean
  > go test ./internal/ -run TestReference  PASS (existing 5 + 4 new)
  > Pre-existing FTS5-driven lesson test failures confirmed unrelated
  > to this change (HEAD had them before edits).
- **handlers:** remove dead handleReference* functions ([790daa4](https://github.com/example/mpm/commit/790daa48caaa3f9ae4c7c1ff9afa3f7d1936dd3c))
  > Eight orphan functions in cmd/mpm/handlers.go had zero call sites
  > outside their own file (verified via grep -rn). They were the legacy
  > pre-DatabaseManager reference CLI surface; the active paths live in
  > cmd/mpm/simple_cmds.go under handleRef / handleRefAdd / handleRefSearch
  > / handleRefList / handleRefShow / handleRefShred and route through the
  > chunked SQLite store.
  > 
  > The mpm reference top-level command still exists for backward
  > compatibility but already routes through handleEntityDeprecation at
  > router.go:213, which prints a deprecation warning and forwards to
  > mpm kb reference. The deleted handlers were simply unreachable.
  > 
  > Removed:
  > 
  >   handleReference        (router stub)
  >   handleReferenceHelp    (used only by mpm help reference)
  >   handleReferenceList
  >   handleReferenceAdd
  >   handleReferenceSearch
  >   handleReferenceGet
  >   handleReferenceShred
  >   handleReferenceScan
  > 
  > cmd/mpm/router.go:342 still needs a helpFunc for 'mpm help reference'.
  > Point it at the existing printRefHelp in simple_cmds.go. That function
  > gains a return value (int) so it can serve both surfaces uniformly;
  > its two existing call sites in handleRef are updated to discard the
  > return value with '_ ='.
  > 
  > 308 lines deleted. Build clean, vet clean (modulo pre-existing
  > warning at handlers.go:388 unrelated to this change). Live check
  > confirmed:
  > 
  >   $ mpm help reference   -> prints the mpm kb reference help text
  >   $ mpm reference help   -> still shows the deprecation banner
- merge reference-sources/ into reference/ ([915147b](https://github.com/example/mpm/commit/915147b9f0b95bf3a5cc80294b0ad76587671556))
  > Consolidate reference text corpus into a single reference/ folder.
  > The 6 pre-existing files in reference/ (Alice, Machiavelli, Meditations,
  > The Art of War, The Prince) were already tracked; the 8 from
  > reference-sources/ (divine_comedy, doll_house, dorian_gray,
  > great_expectations, meditations, monte_cristo, pseudomonad,
  > zarathustra) move in via git mv (rename-detected).
- move openclaw/ into agent-plugins/openclaw-mpm-plugin/ ([9ff292f](https://github.com/example/mpm/commit/9ff292f58ab68dc973fa64508854217eae7b80c2))
  > Same consolidation pass as the previous commit. Rename openclaw/
  > to openclaw-mpm-plugin/ to match the hermes/opencode naming convention.
  > 
  > Changes:
  >   - git mv openclaw/ -> agent-plugins/openclaw-mpm-plugin/
  >   - update README.md (3 references: install pointer, dir tree, link)
  >   - update agent-plugins/hermes-mpm-plugin/install.md cross-ref
  >   - update agent-plugins/openclaw-mpm-plugin/OPENCLAW.md internal paths
  > 
  > Side effect (intentional): agent-plugins/.gitignore now drops
  > node_modules/ and dist/ for this plugin, removing ~280 vendor files
  > (~510k lines) from git tracking. Anyone cloning will need
  > `npm install && npm run build` to rebuild the plugin's dist/.
  > 
  > Verified: go build ./... clean.
- consolidate agent plugins under agent-plugins/ ([aff7bf7](https://github.com/example/mpm/commit/aff7bf7036a320c46ed15dda4145d25f6be2d0f0))
  > Move hermes-mpm-plugin/ and opencode-mpm-plugin/ into a single
  > agent-plugins/ folder for tidiness. Naming convention preserved
  > (hermes-mpm-plugin, opencode-mpm-plugin) — no renames.
  > 
  > Changes:
  >   - git mv hermes-mpm-plugin/    -> agent-plugins/hermes-mpm-plugin/
  >   - git mv opencode-mpm-plugin/  -> agent-plugins/opencode-mpm-plugin/
  >   - new agent-plugins/.gitignore covering common plugin noise
  >     (node_modules, dist, __pycache__, .venv, .mcp.json)
  >   - update README.md docs and dir tree to reflect new layout
  >   - update cmd/mpm-mcp/tools.go source-of-truth comment
  >   - update hermes install.md PLUGIN_SRC path
  > 
  > Verified: go build ./... clean. History preserved via git mv.
- **db:** drop ghost confidence/evidence triggers ([ed30b49](https://github.com/example/mpm/commit/ed30b497e9d961373de0baa5b8aa72b892be2264))
  > The evidence table had three AFTER INSERT/UPDATE/DELETE triggers
  > (evidence_ai/au/ad) that delegated to a Go-registered
  > `confidence_recompute` SQL function. The function was a no-op due
  > to SQLite's connection-locking model (RegisterFunc callbacks
  > deadlock when they attempt subsequent writes), and the actual
  > recompute was always driven from Go via RecomputeConfidence.
  > 
  > The triggers were kept "for forward compatibility and audit clarity"
  > but in practice they misled anyone reading the schema into thinking
  > the database enforced recompute. Worse, existing databases already
  > have the orphan triggers — they fire on every evidence INSERT and
  > fail with "no such function: confidence_recompute" because the
  > no-op registration has been removed.
  > 
  > Delete initConfidenceTriggers and registerConfidenceRecompute.
  > Add a one-shot DROP TRIGGER IF EXISTS in initUnifiedSchema so
  > existing databases self-heal on next startup. Update doc comments
  > in evidence_store.go (AddEvidence and RecomputeConfidence) to
  > match the actual architecture: Go is the authoritative entry
  > point. v2 will wrap insert+recompute in a single transaction.
  > 
  > Co-Authored-By: Claude Opus 4.7 <noreply@anthropic.com>
- **call:** return RoutingReport struct from callRoute ([4eb0a3f](https://github.com/example/mpm/commit/4eb0a3f3383214c7f9b31b35b05fce4da392757c))
  > Removes manual map reconstruction that duplicated the struct's JSON
  > tags. Mirrors cmd/mpm-mcp/tools.go:handleRoute (which uses jsonResult
  > on the struct). The mpm call JSON envelope at call.go:91 handles
  > serialization via the struct tags.
  > 
  > Also renames 'router init' error to 'new router' to match Go
  > convention (function name as error prefix) and surrounding db: %w style.
  > 
  > Test now round-trips through JSON to verify the wire shape — the
  > contract that OpenClaw/Hermes actually depend on.
  > 
  > Co-Authored-By: Claude Opus 4.7 <noreply@anthropic.com>
- **call.go:** dispatch to internal dm methods ([14c0e8c](https://github.com/example/mpm/commit/14c0e8c45148dc7b9e3de450d07ab4a5ec75ae42))
  > Each of the 18 callXxx handlers now dispatches into the new
  > internal/call_helpers.go dm methods (added in d51f03b). Single
  > source of truth for the tool surface. Internal.ParseStringOr /
  > ParseStringSliceOr / ParseFloatOr are the exported wrappers over
  > the lowercase helpers.
  > 
  > Co-Authored-By: Claude Opus 4.7 <noreply@anthropic.com>
- **internal:** add 14 high-level dm methods to call_helpers.go ([d51f03b](https://github.com/example/mpm/commit/d51f03bbde2f5a701519922a6186b958c4311ab0))
  > Mirrors the wake_context.go pattern. These methods back both the
  > mpm call CLI and the new cmd/mpm-mcp MCP server. Single source of
  > truth for the tool surface.
  > 
  > Co-Authored-By: Claude Opus 4.7 <noreply@anthropic.com>
- **claudecode-mpm-plugin:** drop unused MpmRunResult import ([2e4257d](https://github.com/example/mpm/commit/2e4257de62132e25346308a0b6cbdb6a4b22f490))
  > Code review noted: MpmRunResult is imported in server.py but not used
  > at module level. Consumed only transitively via parse_mpm_result's
  > return type. Removing the dead import.
- **claudecode-mpm-plugin:** drop redundant isdir precheck in debug_log ([cc7e528](https://github.com/example/mpm/commit/cc7e5288802d8401438445ee4f87091c18736f72))
  > Code review noted: os.makedirs(parent, exist_ok=True) is idempotent and
  > handles the 'parent already exists' case. The os.path.isdir precheck was
  > redundant and added a TOCTOU window. The OSError swallow still catches
  > NotADirectoryError if parent is a file (Test 3's path).

### Fixed

- **docs:** sync README + code comments to synthesis engine shipped state ([746b72d](https://github.com/example/mpm/commit/746b72dc702284ae21e26544d13a29394989763d))
  > Four drift points caught after 687f1b7:
  > 
  > 1. README Release Notes intro still framed the changelog.json as input
  >    to the future synthesis engine. Rewrote to present tense and surface
  >    the --with-synthesis flag.
  > 2. Documented flags block for 'mpm ops changelog build' was missing
  >    --with-synthesis entirely. Added with orphan-handling contract inline.
  > 3. Schema paragraph still framed the synthesis engine as future
  >    (shaped for); rewrote to 'is the join contract'.
  > 4. Roadmap still listed Changelog Synthesis Engine as the next arc.
  >    Removed - shipped in v1.1.0.
  > 
  > Plus one stale code comment in cmd/mpm/changelog.go:7 pointing at the
  > JSON sibling as future-engine input. Rewrote to reference the
  > --with-synthesis join.
  > 
  > Twist rule still holds: zero hardcoded tool counts introduced.
  > Build clean, full suite green.
- **docs:** kill tool-count drift + wire log_to_changelog into CLI ([78c1c9e](https://github.com/example/mpm/commit/78c1c9ea4e50dd45d25dd310cbb2d1aea13d378b))
  > Hardcoded tool counts in prose/comments are forbidden. The list
  > of s.AddTool(...) calls in cmd/mpm-mcp/tools.go is now the
  > canonical count, and any prose claim about a specific number is
  > decoration that will drift.
  > 
  > Three live claims existed before this fix, all stale:
  > 
  >   - README.md: "21 MPM tools"
  >   - cmd/mpm-mcp/tools.go header: "18 tools"
  >   - cmd/mpm-mcp/main.go header: "All 18 tools"
  > 
  > The actual count was 20 MCP tools after adding log_to_changelog,
  > 28 in the call.go CLI dispatch (different count because CLI
  > exposes more helpers). Three numbers, all wrong, all in the same
  > project.
  > 
  > Fixes:
  > 
  > 1. CLI/MCP parity for log_to_changelog.
  > 
  >    Wired into cmd/mpm/call.go via toolRegistry["log_to_changelog"]
  >    + callLogToChangelog handler. The CLI and MCP surfaces now
  >    agree on which tools exist and what their contracts are; both
  >    route through DatabaseManager.LogChangelogEntry, which holds
  >    the strict retrospective contract (full 40-char SHA-1
  >    required).
  > 
  >    5 new CLI tests (call_log_to_changelog_test.go): happy path,
  >    requires fact, requires commit_hash, rejects malformed
  >    commit, tool is registered in toolRegistry.
  > 
  > 2. Killed hardcoded counts in live docs/comments.
  > 
  >    - cmd/mpm-mcp/tools.go: header rewritten to point at the
  >      s.AddTool call list as the source of truth. RegisterAllTools
  >      comment likewise.
  >    - cmd/mpm-mcp/main.go: header rewritten the same way.
  >    - README.md: "21 MPM tools" -> "a comprehensive suite of
  >      MPM tools"; visual block updated to include log_to_changelog.
  >    - agent-plugins/openclaw-mpm-plugin/OPENCLAW.md: dropped
  >      "21 tools registered" from the plugin.json section header,
  >      replaced the stale 21-name Expected output list with a
  >      pointer to cmd/mpm-mcp/tools.go (the inline list was
  >      already wrong by 6 tools — explain_confidence, the
  >      query_confidence_* family, query_memory_quality, route,
  >      log_to_changelog), dropped all "(N tools)" subsection
  >      counts, dropped "(21 tools total)" from the Tool Reference
  >      header.
  >    - agent-plugins/hermes-mpm-plugin/install.md: dropped
  >      "19 tools across..." from the lede and "with 19 tools"
  >      from the Expected output.
  > 
  > 3. Historical files left alone.
  > 
  >    CHANGELOG.md and docs/superpowers/* record the past
  >    accurately. Mutating them would be rewriting history.
  > 
  > Discipline for future:
  > 
  >   NEVER put a hardcoded tool count in prose, comments, or
  >   section headers. When adding a new tool, update one place
  >   (the s.AddTool call). Docs and prose auto-follow because they
  >   no longer carry a number.
  > 
  > Build clean, vet clean, full test suite green.
- **ingest:** route MCP add_reference through chunked SQLite path ([f840908](https://github.com/example/mpm/commit/f840908ea3511472657d80e6dfe30b1e518007af))
  > DatabaseManager.AddReferenceFromFile previously wrote to the legacy
  > ReferenceStore (JSON at memory/reference/references.json) with no
  > chunking and no FTS indexing. Meanwhile, MCP search_references and
  > CLI search both query the chunked reference_chunks table in mpm.db.
  > The two surfaces were silently divergent: anything ingested via
  > mpm call add_reference was invisible to mpm call search_references.
  > 
  > Rewrite AddReferenceFromFile to share the same pipeline the CLI uses
  > (mpm kb reference add): parse by extension -> ChunkByTokens -> insert
  > into reference_docs + reference_chunks in one transaction. Adds an
  > extended entry point, AddReferenceFromFileWith, for callers that want
  > to pass tags / import_reason / chunk_size.
  > 
  > Extract parseReferenceFile so the extension dispatch (PDF/EPUB/HTML/
  > text/fallback) lives in one place. Both MCP and CLI must agree on
  > what counts as supported content; the comment in parseReferenceFile
  > calls out that any new branch needs to land in both surfaces.
  > 
  > Regression-locked by three tests in internal/mcp_ingest_test.go:
  > 
  >   TestMCPAddReferenceIsSearchable
  >     Ingest via AddReferenceFromFile, search via SearchReferenceChunks
  >     on the same DatabaseManager. The marker word 'zylophlox' is
  >     nonsense on purpose so the test is hermetic across runs and does
  >     not collide with other fixtures.
  > 
  >   TestMCPAddReferenceWithTagsAndReason
  >     Confirms tags and import_reason survive the chunked write path.
  >     The legacy JSON store accepted both; the unified path must
  >     persist them in the reference_docs columns.
  > 
  >   TestParseReferenceFileDispatch
  >     Exercises all five extension branches (.txt/.md/.html/.unknown/
  >     .pdf). The .pdf case expects an error because the synthetic file
  >     is not a valid PDF; the others expect non-empty text.
  > 
  > Live verification with the rebuilt CLI:
  > 
  >   $ mpm call add_reference --payload '{"filepath":"/tmp/x","title":"t"}'
  >   {"id":"928a563428e0b480","success":true,"total_chunks":1}
  > 
  >   $ mpm call search_references --payload '{"query":"qorlix"}'
  >   {"count":1,"results":[{...,"doc_title":"MCP Path Test"}]}
  > 
  > Before this fix, the second call returned count: 0.
- **search:** drop threshold for FTS5-only matches + fix recall float scan ([1d65133](https://github.com/example/mpm/commit/1d65133edea01e1b1e01f6d2f3517aa4677abe24))
  > Two related bugs found while checking query_long_term_memory output.
  > 
  > Bug 1: mpm call query_long_term_memory (and the MCP tool) returned 0
  > results for many queries that should match. Example: query='germany'
  > returns 3 results via raw FTS5 SQL and via mpm recall, but 0 via
  > mpm call.
  > 
  > Root cause: HybridSearch's retrieval threshold (cfg.RetrievalThreshold,
  > default -3.0) was applied uniformly to all matches. But the score math
  > for FTS5-only results is:
  >     combinedScore = bm25_score * (1 - VectorWeight)
  >                   = bm25_score * 0.5
  > BM25 scores for typical matches are -5 to -15 (negative, lower is
  > better). After halving they become -2.5 to -7.5 — which the -3.0
  > threshold filtered almost entirely. The threshold was tuned for hybrid
  > FTS5+vector scores (which land in 0-1 range) and has no business being
  > applied to raw BM25.
  > 
  > Fix: only apply the threshold to true hybrid (fts+vec) matches. FTS5-only
  > matches trust the BM25 ranking, which is already a quality ordering, and
  > cfg.Limit caps the result count anyway.
  > 
  > Verified:
  >   germany    -> 1 (was 0) — FTS5 finds 3, threshold now skipped
  >   world cup  -> 1 (was 0) — BM25 -6.26 was being halved to -3.13
  >   world      -> 1 (still works)
  >   fifa       -> 1 (still works)
  >   2026       -> 5 (still works)
  > 
  > Bug 2: mpm recall silently dropped rows where the weight column was
  > non-integer. The schema stores weight as REAL (floating point), but
  > the recall Scan used int64. Integer-valued floats (6.0) scanned fine;
  > fractional values (6.5, 7.0, etc.) failed the scan and were silently
  > discarded via . This was a slow-motion bug: every recall call
  > bumps weight by 0.5 via implicit reinforcement, so as more memories
  > acquire fractional weights, more results vanish from recall.
  > 
  > Reproduced: mpm recall --collection memories germany returned 0 even
  > though raw FTS5 found 2 matches. The World Cup memory had weight 6.5
  > from prior reinforcement bumps.
  > 
  > Fix: change scan targets from int64 to float64. The recallEntry struct
  > field is int, so round at the assignment: int(weight + 0.5).
  > 
  > This bug and Bug 1 were discovered together while testing the
  > query code-path inconsistency noted in the previous lesson.
  > 
  > All within feature freeze: both fixes restore documented search
  > behavior that was quietly broken.
- **reflex-engine:** sync config/current_* when active.json changes ([5e8852f](https://github.com/example/mpm/commit/5e8852ffe3065000ee867912efa8f2aef284e55f))
  > The CLI surface (mpm persona set / mpm mode add) and the actual memory
  > metadata injection were reading from two stores that had drifted apart
  > since May 15. User-facing commands wrote active.json; the injection
  > point (detectActiveContext in cmd/mpm/handlers.go:84) read
  > config/current_persona and config/current_mode. The two were never
  > synced, so 'mpm persona set default && mpm kb memory add foo' stamped
  > metadata with whatever was in the stale config/ files — not what the
  > user just selected.
  > 
  > This is the 'Reflex Engine' surface the README documents
  > (README.md, 'The Reflex Engine' section) but the wiring between
  > active.json and config/ was never completed.
  > 
  > Changes:
  >   - internal/mode.go: ModeManager.SetActive now mirrors the first
  >     non-'auto' mode to config/current_mode. The 'auto' sentinel is
  >     treated as a feature flag, not a real mode (matches xitl.go's
  >     IsAutoActive logic). Sets config/current_mode to nothing if no
  >     real mode is selected.
  >   - internal/persona.go: PersonaManager.SetActive now mirrors the
  >     active persona to config/current_persona, with the same 'auto'
  >     sentinel handling.
  >   - cmd/mpm/handlers.go: handleModeAdd and handleModeRemove were
  >     calling AddMode/RemoveMode (a stub and a destructive file-deletion
  >     helper, respectively). Replaced with active-list mutations that
  >     validate, dedupe, and route through SetActive. The .md files
  >     remain managed via the file system, as the error message in
  >     AddMode already suggested.
  > 
  > Verified end-to-end:
  >   - mpm mode add programming
  >     -> config/current_mode = 'programming'
  >     -> mpm mode active shows 'programming'
  >   - mpm persona set default
  >     -> config/current_persona = 'default'
  >   - mpm kb memory add <fact>
  >     -> saved memory's metadata has active_mode=programming,
  >        active_persona=default (verified via sqlite json_extract)
  >   - mpm mode clear / mpm persona clear
  >     -> config/ files removed, no stale injection
  >   - mpm mode add <unknown> -> 'Unknown mode: <name>' (no silent fail)
  >   - mpm mode remove <not-active> -> 'Mode not active: <name>'
  > 
  > All within feature freeze: this is making existing documented
  > behavior actually work, not adding new surface.
- **cli:** restore UX error + reimplement mpm backup/restore-db ([3e58c0f](https://github.com/example/mpm/commit/3e58c0fa90bc1d647fb43975eeba0ae74bf36161))
  > Two related fixes that were advertised but dead:
  > 
  > 1. mpm restore error UX (cmd/mpm/handlers.go)
  >    - Before: 'sql: no rows in result set' for missing IDs (raw SQL leak)
  >    - After: 'Memory not found: <id>' (matches the existing affected==0 branch)
  > 
  > 2. mpm backup + mpm restore-db reimplementation (cmd/mpm/handlers_backup.go)
  >    - Both commands were registered in router.go but had no dispatchers
  >      and no handler implementations since the file was lost in May.
  >      Commit 0ce7dc8 removed the dead dispatchers to unblock compilation.
  >    - Now: handlerBackup shells out to 'sqlite3 .dump' for portability
  >      and writes to a timestamped mpm-backup-<UTC>.sql next to the DB
  >      (or to a path arg if provided). Handles WAL flush first so the
  >      dump captures all writes.
  >    - handlerRestoreDB closes connections, clears *.db-wal/*.db-shm,
  >      confirms with the user (y/N), then 'sqlite3 .read <dump>' over
  >      the live DB. Order matters — WAL/SHM cleanup BEFORE the import
  >      so SQLite doesn't merge stale WAL pages on top.
  > 
  > 3. Wired dispatcher cases back into router.go (the four removed by
  >    0ce7dc8, restored in spirit of the original implementation).
  > 
  > 4. Added DatabaseManager.DBPath() so handlers can resolve the file
  >    path without re-running config.GetMPMDir().
  > 
  > Verified:
  >   - mpm restore <bad-id>  -> 'Memory not found: <id>'
  >   - mpm restore <valid>   -> 'restored: <id>' (happy path unchanged)
  >   - mpm backup            -> 8.9MB timestamped .sql next to DB
  >   - mpm restore-db <file> -> y/N prompt, aborts on 'n', error on
  >                              missing file
  >   - Dump round-trips: piped into fresh sqlite, recovers all 4410
  >     memories from the live DB.
  > 
  > All within feature freeze (bug-fix shaped: registered-and-broken
  > surface, not new functionality).
- bug-hunt sweep + MPM weight propagation repair ([b51066f](https://github.com/example/mpm/commit/b51066f23bd12abb0a5697f399d3cbbd9c1d85ed))
  > Day of focused bug hunting during feature freeze (2026-06-18).
  > Bug fixes only, no new surface.
  > 
  > Critical fix (808):
  >   save_to_memory parsed weight, stashed it as metadata.weight_intent,
  >   but AddMemory neither accepted nor wrote the weight column. mem.Weight
  >   stayed at Go zero value (0), so the response lied. Three-layer repair:
  >     - new MemoryStore.AddMemoryWithWeight() writes weight column directly
  >     - SaveMemoryWithContext routed through it (fixes both CLI mpm call
  >       and MCP handleSaveTo_memory in one move — both go through here)
  >     - legacy AddMemory now reflects DB default (1) back to caller
  >   Encoding: float [0.0, 1.0] -> int [1, 100] via int(weight*10).
  >   Matches existing ReinforceMemory/WeakenMemory scale.
  >   Verified: 0.7->7, 1.0->10, omitted/0->5, all persisted to DB.
  > 
  > Other fixes from bug hunt (v):
  >   - internal/evidence_store, idle_dream, hybrid_search, ingest,
  >     schema, web_db, embeddings, db, call.go, etc.
  >   - plugin and config touch-ups across hermes-mpm-plugin and openclaw
  > 
  > Plugin cleanup:
  >   - Retire claudecode-mpm-plugin/ (moved away from claudecode)
  >   - Drop mpm-agent/CLAUDE.md, NEW_README.md
  >   - Add opencode-mpm-plugin/ source (src/, package.json, tsconfig)
  >   - Add hermes-mpm-plugin/evidence_tools.py
  > 
  > New tests:
  >   - internal/idle_dream_drift_test.go
  >   - internal/idle_dream_retrieval_test.go
  > 
  > Excluded from commit (build artifacts / local config):
  >   - mpm, mpm-mcp (root binaries)
  >   - .mcp.json, .opencode/ (local tool state)
  >   - src/db/mpm.db, mirror.jsonl, watchdog.jsonl runtime drift
  >     (restored via 'git restore' — 12k lines of session noise)
- **directives:** unify web UI + mpm-agent read paths via OR clause ([c2c1a5e](https://github.com/example/mpm/commit/c2c1a5e3371ba265cf25f223d152cc526cdf7cd4))
  > The previous OR-clause fix covered only the MCP path
  > (internal/call_helpers.go) and the CLI handler (cmd/mpm/handlers.go).
  > Two readers still queried by single identifiers, so a directive
  > ingested via save_to_memory (collection='directives') was invisible
  > to those surfaces:
  > 
  > - internal/web_db.go QueryMemories/SearchMemories with primeOnly=true:
  >   matched metadata/tags LIKE '%is_prime_directive%' only — never saw
  >   collection-only directives.
  > - mpm-agent/core/agent.go retrieveDirectives: matched metadata LIKE
  >   '%is_prime_directive%' only — never saw collection-only directives.
  > 
  > Apply the same OR clause to both:
  >   (metadata LIKE '%is_prime_directive%' OR collection = 'directives')
  > 
  > (Web path also keeps tags LIKE for the case where an old UI tagged a
  > directive via metadata['is_prime_directive']=1 rather than the column.)
  > 
  > Add directives_parity_test.go: builds a DM with three directive-style
  > rows marked by each of the three possible identifiers plus one regular
  > memory, then asserts every read path returns the same set and the same
  > count. A future query optimization that quietly fractures the parity
  > will fail this test.
  > 
  > Verified: full MPM test suite green; mpm-agent core tests green with
  > the Makefile's CGO_LDFLAGS=-lm. CLI shows all 3 Prime Directives after
  > rebuild.
  > 
  > Co-Authored-By: Claude Opus 4.7 <noreply@anthropic.com>
- **directives:** unify CLI and MCP read paths via OR clause ([16e6628](https://github.com/example/mpm/commit/16e6628d29db539e8bb942c3b4757787d1d50951))
  > Previously `mpm ops directives` (CLI) read by `is_prime_directive = 1`
  > while `mpm call read_directives` (MCP) read by `collection = 'directives'`.
  > A directive ingested via `save_to_memory` set only the collection, so the
  > CLI never saw it — verified just now: 1 row visible to CLI, 2 to MCP,
  > of 3 total Prime Directives.
  > 
  > Update both readers to query
  >   WHERE (collection = 'directives' OR is_prime_directive = 1)
  >     AND deleted_at IS NULL
  > so a directive is visible from every consumer regardless of which
  > identifier was set.
  > 
  > Also: move the "Decision Tracing" section from research.md to
  > architect.md. The router uses body-text keyword matching for mode
  > selection, and the research.md copy added the words "new" and
  > "architecture" to its body — making any prompt containing those words
  > trigger research mode in addition to architect/programming, pushing the
  > combined mode+persona text past the 9500-char cap and silently dropping
  > the persona. architect.md already triggers on those words, so adding
  > the section there reinforces selection without changing the mode set.
  > 
  > Verified: `mpm ops directives` and `mpm call read_directives` now both
  > return all 3 Prime Directives; TestRenderRoute passes; full suite green.
  > 
  > Co-Authored-By: Claude Opus 4.7 <noreply@anthropic.com>
- **idle_dream:** wrap decay recompute in per-artifact transaction ([7e6fd9b](https://github.com/example/mpm/commit/7e6fd9b8b5738ded41dc9c9f038548fc523a9ed3))
  > ConfidenceDecayCycle called RecomputeConfidence as auto-commit statements.
  > A crash mid-write left the confidence column changed with no matching
  > decay_tick row in confidence_history, desyncing the artifact from its
  > audit trail.
  > 
  > Each candidate now runs inside dm.WithTx so the confidence update and
  > history insert succeed or roll back together. Failures are still logged
  > and skipped, preserving the loop's existing error-isolation behavior.
  > 
  > Co-Authored-By: Claude Opus 4.7 <noreply@anthropic.com>
- **evidence:** scan notes for secrets/poison in AddEvidence ([f273029](https://github.com/example/mpm/commit/f273029650784e55801cc3ed0b97715f7858a2b2))
  > AddEvidence bypassed the 20-pattern secret/poison scanner that
  > MemoryStore.AddMemory runs. A user could persist "sk-..." in
  > `notes` and it would land in the DB unchallenged — the exact
  > bypass pattern the CLAUDE.md audit flagged for memory writes.
  > 
  > Apply the same isSensitiveContent + isPoisoned checks to
  > `notes` (the only free-form text on an evidence row). Add a
  > regression test that exercises the scanner with an API key
  > canary and verifies nothing is persisted.
  > 
  > Co-Authored-By: Claude Opus 4.7 <noreply@anthropic.com>
- **migration:** gate backfill on per-table SQL default, not 0.5 ([18bfd45](https://github.com/example/mpm/commit/18bfd45522986616818816a0c60acaf4fb2ace7f))
  > SafeMigrations added confidence at 0.8 (memories) and 0.7 (lessons),
  > not 0.5 as the spec assumed. The backfill's WHERE confidence = 0.5
  > gate matched zero rows in production. Theory/decision rows inherited
  > the memory default of 0.8 and stayed there.
  > 
  > Gate the backfill on the actual per-table default instead. Memory
  > and lesson rows are no-ops (already at the correct initial); theory
  > rows migrate 0.8 -> 0.5; decision rows migrate 0.8 -> 0.6.
  > 
  > Adds TestMigrateFoundation_MigratesTheoriesAndDecisions as the
  > production-state regression test.
  > 
  > Co-Authored-By: Claude Opus 4.7 <noreply@anthropic.com>
- **confidence:** decayLambda keyed on artifact type, not collection ([bd54221](https://github.com/example/mpm/commit/bd542210baf2642a93680d09b4e1fae670d54a99))
  > computeConfidence passes artifactType ("memory", "decision", ...) to
  > decayLambda, but the function switched on collection strings
  > ("memories", "decisions", ...). The two never matched, so the switch
  > always fell through to the default 0.01. Fix: key decayLambda on the
  > artifact type, with the same per-type values from the spec.
  > 
  > This is a Task 2 bug surfaced by Task 4's evidence_store wiring.
- **evidence:** restore source_group required check, add to test fixtures ([ce30ff3](https://github.com/example/mpm/commit/ce30ff3b4b181de99d0d423034fdccb60368773c))
  > The Task 4 implementer dropped the `source_group required` check from
  > AddEvidence because the spec's test fixtures 3 and 4 omitted SourceGroup.
  > This restores the Go-level validation (the DB NOT NULL constraint is the
  > backstop, but Go validation gives a better error) and updates the
  > affected tests to provide a real source group.
- **route:** restore persona-priority truncation path ([3b0c4e6](https://github.com/example/mpm/commit/3b0c4e6a403c437454515cbc8696757125b7d01b))
  > The renderer's applyRouteLengthCap call passed the combined body in
  > the first slot and an empty string in the second, making the
  > persona-priority branch dead code. A 9,500-10,000 char body with a
  > long persona and short mode would slip past the cap and risk the
  > 10,000-char hook limit.
  > 
  > Refactor applyRouteLengthCap to return (truncatedMode, truncatedPersona)
  > so renderRoute can rebuild the labeled body from the two pieces. The
  > persona-priority semantic is now actually enforced.
  > 
  > Also adds an integration subtest to TestRenderRoute that exercises a
  > 9,500-10,000 char body to catch this regression class going forward.
  > 
  > Co-Authored-By: Claude Opus 4.7 <noreply@anthropic.com>
- **router:** remove dead backup/restore-db dispatcher references ([0ce7dc8](https://github.com/example/mpm/commit/0ce7dc8e2cbbc34e57b4187613bc0f647c11e1e4))
  > cmd/mpm/router.go referenced handleBackup and handleRestoreDB in two
  > switch blocks (lines 157, 161, 438, 440) but no such functions exist
  > in the tree or git history — cmd/mpm/handlers_backup.go was never
  > committed. The package failed to compile, blocking `go test ./cmd/mpm/`
  > for every other task.
  > 
  > Removing the four case statements restores the build. The `mpm backup`
  > and `mpm restore-db` commands will now return 'unknown command' at
  > runtime — same direction as the prior compile error. Re-implementing
  > these handlers is a separate task and out of scope here.
  > 
  > Co-Authored-By: Claude Opus 4.7 <noreply@anthropic.com>
- **readme:** correct tool list ordering to match tools.go, revert tool count to 19 ([014a544](https://github.com/example/mpm/commit/014a544e0fd03b58377b6d1edaae23863377c853))
- **schema:** add source_db/source_id/promoted_at columns to memories ([2d00a44](https://github.com/example/mpm/commit/2d00a444f03ecc66357d812f0cec8e8714c75e51))
  > GetMemory and several other queries in web_db.go (and ingest.go's
  > PromoteRawMemory) reference source_db, source_id, and promoted_at
  > columns on the memories table. They were never declared in BaseTables
  > or SafeMigrations, so any DB created via InitSchema (including all
  > tests and the production DB) is missing them.
  > 
  > This was previously masked: an earlier fix (e2a747e) removed the
  > columns from the GetMemory SELECT, but f055bcc re-added them while
  > adding the ingest path. Net effect: any caller exercising GetMemory
  > hits "no such column: source_db".
  > 
  > Add the three columns to BaseTables.memories for fresh DBs, and to
  > SafeMigrations so existing DBs gain them on next InitSchema (idempotent
  > ALTER TABLE ADD COLUMN, already filtered by isDuplicateColumnError).
  > Types match the SQL in web_db.go (TEXT, TEXT, REAL) and are nullable
  > so existing rows are unaffected.
  > 
  > TestFeedbackWeightAdjustment and TestFeedbackChallengeAndReinforce
  > now pass; full internal and cmd/mpm suites green.
  > 
  > Co-Authored-By: Claude Opus 4.7 <noreply@anthropic.com>
- **claudecode-mpm-plugin:** render .mcp.json at project root ([d5c018c](https://github.com/example/mpm/commit/d5c018c8ab1b9511f6cbafe392851c63aa5cd2ed))
  > Claude Code's MCP registry reads from .mcp.json at the project root, not
  > from .claude/mcp.json. Putting the rendered config in .claude/ made the
  > plugin invisible to the loader — install succeeded but no tools ever
  > appeared.
  > 
  > - install.sh: write to $SRC/../.mcp.json instead of $DST/mcp.json
  > - install.sh: auto-remove stale .claude/mcp.json on re-install
  > - install.sh: --uninstall removes the project-root .mcp.json
  > - README: Install/Uninstall/Troubleshooting now point at project-root .mcp.json
  > 
  > Co-Authored-By: Claude Opus 4.7 <noreply@anthropic.com>
- **claudecode-mpm-plugin:** correct step count + working smoke test in README ([8c0d479](https://github.com/example/mpm/commit/8c0d4795db0861c3f5c14bc31a9ebff8c1f7dc92))
  > - README: "5-step fallback" → "4-step fallback" (matches install.sh)
  > - README: smoke test now uses the full MCP initialize handshake
  >   (the previous snippet produced a JSON-RPC error and would have
  >   misled any developer trying to verify the server works)
  > - install.sh: matching comment fix (5 → 4)
  > 
  > Found by code-quality review on Task 25.
- **claudecode-mpm-plugin:** make format_age robust to tz-aware ISO input ([37fd062](https://github.com/example/mpm/commit/37fd062a2536dcd23a7aa9a6ad72b8f5645a59b6))
  > Code review caught: 'datetime.fromisoformat("2026-06-15T12:34:56+00:00")'
  > returns a tz-aware datetime, and datetime.utcnow() is naive. The
  > subtraction raises TypeError, which the existing try/except (ValueError
  > only) did not catch.
  > 
  > Unified the parse+subtract into a single try/except (ValueError, TypeError)
  > and added a regression test for tz-aware input. Both pass.
- **claudecode-mpm-plugin:** make SQLITE_BUSY check case-insensitive ([4953c5f](https://github.com/example/mpm/commit/4953c5fee6d5ba9f01e9e6fab33cb4dab31df98b))
  > Code review caught: stderr.lower() was only applied to the first half of
  > the database-locked check. The SQLITE_BUSY half used 'SQLITE_BUSY' in stderr
  > (without .lower()), which would miss any lowercase or mixed-case variant.
  > 
  > Refactored to assign stderr_lc once and use it for both substring checks,
  > catching all case variants of SQLITE_BUSY. Added a regression test using
  > lowercase 'sqlite_busy' to lock down the fix.
  > 
  > Also: deviations from Task 6 plan recorded in plan file (see follow-up).

### Other

- **readme:** align reference surface + add Release Notes section ([fa41a13](https://github.com/example/mpm/commit/fa41a13e71c1952798fcf67473d900e9968d22f1))
  > Three surface mismatches found and resolved:
  > 
  > 1. Reference CLI examples (kb reference block) showed
  >    --tag tags only on add, missing --reason (seed of the
  >    admission justification chain) and --chunk-size flags that
  >    the actual surface accepts. Added the four subcommands
  >    that shipped since the README was written (used,
  >    interactions, admit, ls renamed from list).
  > 
  > 2. Reference Library prose still described pre-arc state
  >    (Smart Fence chunking) without mentioning the chunk-hash
  >    diff, the embedding split, or the Shelf-vs-Mind
  >    architecture. Updated to reflect current state briefly:
  >    re-ingest of unchanged content costs zero embedding work;
  >    embedding is a separate phase from chunk insert; references
  >    are a shelf, admission is per-consult.
  > 
  > 3. Changelog had no README coverage at all. Added a Release
  >    Notes section between Architecture and Configuration with:
  >    - pointer to CHANGELOG.md and changelog.json
  >    - the mpm ops changelog build flags
  >    - the schema contract (commit_hash + mpm_memory_ids join)
  >    - the log_to_changelog MCP tool example
  >    - the strict retrospective contract (full 40-char SHA-1
  >      required)
  > 
  > Also added
  > mpm ops — Engine Room: maintenance, diagnostics, and power tools
  > 
  > Usage: mpm ops <subcommand> [arguments]
  > 
  > Subcommands:
  >   doctor [--explain]     Run diagnostics (--explain for FTS5 query plan)
  >   maintain               Self-maintenance: decay, consolidate, prune
  >   synthesize [--dry-run] LLM synthesis on all memories
  >   gc [--dry-run/--review/--purge/--shred-negative] Memory decay sweep
  >   backfill-embeddings [--batch-size/--collection/--dry-run] Backfill embeddings for existing memories
  >   dlq:review [review/clear/retry] Dead letter queue — failed synth events
  >   watch                  Start/stop/status watcher daemon
  >   web                    Start web UI server
  >   review                 Spaced reinforcement review
  >   stats                  Memory statistics
  >   prune                  Prune expired memories
  >   export                 Export memories to JSON
  >   backup [path]          Database backup (.sql dump)
  >   restore-db <path>      Restore database from .sql dump
  >   ingest                 Import memories from external SQLite
  >   switch                 Interactive persona/mode switcher
  >   directives             Show behavioral directives
  >   mode                   Mode operations
  >   persona                Persona operations
  >   topic                  Topic operations
  >   lesson                 Lesson operations
  >   session                Session operations
  >   memory                 Memory operations
  >   reference              Reference library
  >   wake                   Show last session context
  >   gateway                Gateway control
  >   status                 System status dashboard
  >   stance assume <mode> <persona> <rationale> XITL: hot-swap existing persona when auto active
  >   stance synthesize <name> [flags] XITL: generate JIT persona for novel edge cases
  >   promote                XITL: promote ephemeral persona to permanent disk file
  >   confidence show|recompute Confidence/evidence engine: snapshot or trigger recompute
  >   help                   Show this help
  > 
  > All ops subcommands also work at the root level for
  > backwards compatibility (e.g. `mpm doctor` = `mpm ops doctor`). to the ops subcommand
  > block under Core engine so the operator finds it.
  > 
  > Roadmap updated to call out the Changelog Synthesis Engine
  > as the next arc: the schema and the MCP tool are in place;
  > the engine itself (the pure function that joins git-sourced
  > entries with agent-written #changelog memories) is the next
  > commit.
  > 
  > No hardcoded tool counts in the prose. The Twist rule from
  > the previous commit held: only the s.AddTool call list in
  > cmd/mpm-mcp/tools.go carries a count, and the README points
  > at the source of truth instead of duplicating it.
- **gitignore:** catch SQLite WAL/SHM rollover files ([b8ababe](https://github.com/example/mpm/commit/b8ababe344d86f387e7348e26ed6b86ba691f51c))
  > The current rules (*.db-wal, *.db-shm) match the active WAL and
  > SHM sidecars but not the .old-wal / .old-shm rollover files SQLite
  > leaves behind after a checkpoint. Those regenerate automatically
  > and were showing up as untracked after every live CLI test that
  > touched the database.
  > 
  > Also leaves the existing rules untouched for compatibility.
- sync local working state ([21ef8c3](https://github.com/example/mpm/commit/21ef8c337491ccaa56f4ed56c115fcf41b36f91c))
  > User-asserted correct and intentional. Captures the working tree as it
  > stood at the end of the 2026-06-19 session before further cleanup.
  > 
  > Mostly mode-only churn (the 0-line diffs are from the earlier
  > git-stash fat-finger); real content changes are:
  > 
  >   mpm_config.json     base_url: .../anthropic -> .../anthropic/v1
  >   src/db/mirror.jsonl     runtime ingest log (+149 lines)
  >   src/db/watchdog.jsonl   runtime watchdog log (+1272 lines)
  >   src/db/mpm.db           runtime database (9MB -> 15MB from live
  >                           tests of the new chunked SQLite ingest path)
  > 
  > Followups worth doing but not done here:
  >   - git rm --cached mpm_config.json to stop tracking a file that
  >     contains an API key. .gitignore already lists it; the secret
  >     is already in history from a prior commit.
  >   - git rm --cached src/db/{mpm.db,mirror.jsonl,watchdog.jsonl}
  >     to stop tracking runtime artifacts. .gitignore already lists
  >     them; they were committed before the rules were tightened.
- stop tracking mpm-agent/ (now a sibling project at flowbyte-com/mpm-agent) ([8374cdb](https://github.com/example/mpm/commit/8374cdb6d7f3b0e11b4b42caf9d378a232da7b6a))
  > The 53 mpm-agent/* paths in this index are dropped; the directory itself
  > no longer lives under mpm/ (it was extracted to ../mpm-agent/). A defensive
  > mpm-agent/ rule is added to .gitignore in case the dir is ever recreated
  > inside mpm/.
  > 
  > Other uncommitted changes (WIP and the .claude/skills/mpm + hermes-mpm-plugin
  > deletions) are left untouched — this commit isolates the mpm-agent detach
  > so it can land independently.
- **gitignore:** fix broken mpm_config.json pattern ([1c4ad26](https://github.com/example/mpm/commit/1c4ad2624282b7784424eabafb73f9b605f3de60))
  > The previous line 'mpm_config.jsonsrc/db/mirror.jsonl' was a single
  > pattern with a missing newline, so neither mpm_config.json nor
  > src/db/mirror.jsonl were being ignored. mpm_config.json contains the
  > LLM vendor API key; keeping it out of the working tree prevents future
  > accidental commits before the secret-scrub pass.
  > 
  > Splits the broken pattern into three explicit entries and adds
  > mpm_config.json.bak to the ignore list. (v plans a sweep to scrub
  > mpm_config.json from history before the public transition; this just
  > prevents re-commits in the meantime.)
- extend Makefile to handle mpm-mcp alongside mpm ([8c4c051](https://github.com/example/mpm/commit/8c4c051d55ed4f4b672bd54dd00f870887064322))
  > Before: Makefile only knew about mpm (make build -> bin/mpm, make
  > install -> /usr/local/bin/mpm). mpm-mcp was built manually, dropped
  > wherever (mostly at project root as a 16MB orphan). Stale mpm-mcp
  > binaries were easy to ship because nothing tracked them.
  > 
  > After:
  >   - make build   -> bin/mpm + bin/mpm-mcp
  >   - make install -> /usr/local/bin/mpm + /usr/local/bin/mpm-mcp
  >   - make clean   -> both (unchanged behavior, rm -rf bin/)
  >   - MCP_BINARY var so future renames touch one place
  >   - GO auto-detection (PATH first, /usr/local/go/bin fallback) so
  >     make works in non-interactive shells where ~/.bashrc isn't sourced
  >   - help target updated
  > 
  > Verified: make clean && make build produces fresh bin/mpm + bin/mpm-mcp.
- **readme:** document Epistemology Engine tools + Prime/Mode split ([79e28ec](https://github.com/example/mpm/commit/79e28ec2643751b500c097597562ca8e09834381))
  > Four updates:
  > - JSON Boundary examples: add the three new MCP tools (add_evidence,
  >   list_evidence, query_confidence_history) so they're discoverable.
  > - Confidence/Evidence commands table: pair each CLI command with its
  >   MCP counterpart; add artifact_type enum note; add atomicity guarantee
  >   paragraph documenting the WithTx/DBNode wrapper.
  > - Directives section: replace the single-tier description with the
  >   two-tier Reflex Engine split (Prime Directives in DB collection,
  >   Mode Directives in mode/*.md files); fix the elevation example
  >   to drop the is_prime_directive field (save_to_memory silently
  >   ignores it) and use the working collection='directives' path.
  > - Document the two-path inconsistency honestly: mpm ops directives
  >   reads by is_prime_directive=1 column; mpm call read_directives
  >   reads by collection='directives'. They currently cover disjoint
  >   sets — flagged for follow-up so readers don't expect parity.
  > 
  > Co-Authored-By: Claude Opus 4.7 <noreply@anthropic.com>
- **readme:** add confidence and evidence section ([b0a742f](https://github.com/example/mpm/commit/b0a742fd2deca7e05a8e492bcb403f27a9ef9a0c))
- **decisions:** lock initial confidence for RecordDecision and ProposeTheory ([bd8271d](https://github.com/example/mpm/commit/bd8271d7ae989056c3e1751fc02d0b88f8507265))
  > Both functions already route through MemoryStore.AddMemory with the
  > correct collection ("decisions" / "theories"), and Task 5's
  > MemoryStore.AddMemory wires InitialConfidence per collection. No
  > production code change needed in call_helpers.go — just the
  > production-path tests that lock the contract.
- **structs:** add production-path tests for initial confidence persistence ([8e89a77](https://github.com/example/mpm/commit/8e89a77c889c99a7a98b100ec923456b1253c982))
  > The Task 5 spec gap: TestAddMemory_SetsInitialConfidenceByCollection
  > exercises only the AddMemoryViaDM helper, not MemoryStore.AddMemory
  > itself. A regression in the producer that drops the InitialConfidence
  > assignment would not be caught by the helper test.
  > 
  > This adds two tests that route through the real producers
  > (MemoryStore.AddMemory, DatabaseManager.AddLesson) and read the
  > confidence column back from SQLite to lock the production contract.
- **plan:** add v2 follow-up task for real trigger-driven recompute ([fda3172](https://github.com/example/mpm/commit/fda317213e77446aa0cec8ba38a3c604fed61d0c))
  > The v1 ships with the trigger as a no-op and recompute driven from Go
  > (due to SQLite connection-locking deadlock constraint). This adds
  > Task 14 documenting the v2 design options and acceptance criteria so
  > the work isn't lost.
- **plan:** confidence and evidence foundation implementation plan ([696e2c2](https://github.com/example/mpm/commit/696e2c26a8cbb657d06f1d8b36f289e5ac47a308))
- **spec:** revise confidence foundation per design review ([6822f5a](https://github.com/example/mpm/commit/6822f5adabc3391d6e2fb358282036e0ddbc2d52))
  > Address seven review points on the foundation design:
  > 
  > - Switch confidence from GENERATED column to regular column updated
  >   by triggers + idle_dream worker. Generated column calling a
  >   function that reads another table is fragile across SQLite
  >   versions and can't encode time-dependent decay in a deterministic
  >   expression. The invariant ("confidence only rises with evidence")
  >   plus code-path discipline replaces the schema-enforced purity.
  > 
  > - Add per-type initial confidence (memory 0.8, theory 0.5, decision
  >   0.6, lesson 0.7) as a decision rather than an open question.
  >   Memory and theory have different epistemic starting points.
  > 
  > - Add the invariant explicitly: "Confidence is allowed to decrease
  >   automatically. Confidence is never allowed to increase without
  >   evidence." This is now the structural claim; the schema enforces
  >   it via the trigger chain's writers, not the column type.
  > 
  > - Add created_by to evidence for provenance. Calibration will need
  >   to ask "which evidence sources produce durable confidence?"
  > 
  > - Add known future dimension: urgency. Documented as a candidate
  >   third axis (alongside importance and retrieval_priority) so the
  >   schema work doesn't paint us into a corner.
  > 
  > - Add callout distinguishing Challenge (workflow event) from
  >   Negative Evidence (data contradicting). Collapsed in v1, split
  >   in the Challenge refactor spec.
  > 
  > - Add "do not optimize away" callout on confidence_history. The
  >   trend analysis substrate (belief collapse / formation) is the
  >   most valuable table in the design.
  > 
  > Co-Authored-By: Claude Opus 4.7 <noreply@anthropic.com>
- **spec:** confidence and evidence foundation design ([e517b54](https://github.com/example/mpm/commit/e517b5493a1938ed8b454fb374e9580893412ac9))
  > Draft spec for the schema foundation that decouples weight into
  > retrieval_priority + importance, adds confidence as a derived
  > (generated) column, and makes evidence a typed first-class entity.
  > Establishes "knowledge and confidence are independent" as the
  > architectural principle: confidence may change without modifying
  > knowledge, knowledge may change without modifying confidence, and
  > historical states are preserved.
  > 
  > This is the foundation for the follow-up work (calibration tracking,
  > challenge refactor, hindsight annotations, forgetting log) but does
  > not include any of those — each is a separate spec.
  > 
  > Co-Authored-By: Claude Opus 4.7 <noreply@anthropic.com>
- reframe Reflex Engine section — platform-agnostic, Claude Code as one hook implementation ([8c0f22e](https://github.com/example/mpm/commit/8c0f22eebbe08b03b40afb3c37bf5e01c109c986))
- add The Reflex Engine section — auto mode/persona router ([147e1df](https://github.com/example/mpm/commit/147e1df218f2793fdf47bbb596be5256d4c50765))
  > Names and describes the zero-latency pre-prompt router that picks
  > mode + persona before the LLM sees the prompt. Placed right before
  > the Claude Code integration section so the engine is introduced
  > before its main consumer. Cross-links to the existing
  > Auto-Selection (route tool) section for scoring rules, anti-pattern
  > handling, and hot-reload details.
  > 
  > Co-Authored-By: Claude Opus 4.7 <noreply@anthropic.com>
- add Directives section to README — storage, access, path note, elevation ([6c861d0](https://github.com/example/mpm/commit/6c861d03e8ad2830cc64801275cd7b0fb0a440c4))
- clarify Claude Code integration — stdout protocol and truncation markers ([be40bf0](https://github.com/example/mpm/commit/be40bf0ff92809cf1c2fc0c809d8940760e57124))
  > Distinguishes the two truncation markers (persona marker references the
  > on-disk file; mode-hard-cap marker does not). Adds a one-sentence note
  > clarifying that plain stdout is the correct protocol for UserPromptSubmit
  > hooks per the Claude Code docs, preempting the ambiguity some readers
  > hit when assuming JSON is required.
  > 
  > Co-Authored-By: Claude Opus 4.7 <noreply@anthropic.com>
- add Claude Code integration section + route tool row ([2d6b253](https://github.com/example/mpm/commit/2d6b253b365b8b0e5efb70ee1fc7aa9ab65c7eb8))
  > Documents the UserPromptSubmit hook config, opt-out mechanisms, and
  > verify-it-works examples. Adds 'route' to the call-tool table at the
  > correct position (19th tool, per recent README fix in 014a544).
  > 
  > Co-Authored-By: Claude Opus 4.7 <noreply@anthropic.com>
- **plan:** fix Task 4 reference code — mode-hard-cap check before persona ([f71b916](https://github.com/example/mpm/commit/f71b916d4a2162b38d40ecd2dd4c7718e619fba3))
  > The original plan placed the persona-truncation check before the
  > mode-hard-cap check, which would have let a 10000-char mode escape
  > truncation whenever a persona was present. The correct order is:
  > combined-cap → mode-hard-cap → persona-truncation → fallback.
  > 
  > Task 4's implementer caught this and shipped the corrected logic in
  > ca72782. The plan now matches the implementation.
  > 
  > Co-Authored-By: Claude Opus 4.7 <noreply@anthropic.com>
- implementation plan for Claude Code auto-routing hook ([798acd4](https://github.com/example/mpm/commit/798acd41f31ed5fd628976f0f50f69e6140afc9c))
  > 8 tasks: workspace resolution, input parsing, opt-out checks,
  > length cap, main renderer, mpm call route JSON handler, mpm route
  > CLI command, and README integration section. TDD throughout with
  > frequent commits.
  > 
  > Co-Authored-By: Claude Opus 4.7 <noreply@anthropic.com>
- fix spec — both mode and persona files are .md, not mode=.json ([e26484c](https://github.com/example/mpm/commit/e26484c1dd9454a0b9f3f15ef3815d90dc6c6242))
  > Verified against working examples: mode/architect.md and persona/default.md
  > are both Markdown with YAML frontmatter. Spec example output updated to
  > show both files as .md.
  > 
  > Co-Authored-By: Claude Opus 4.7 <noreply@anthropic.com>
- design spec for Claude Code auto-routing via UserPromptSubmit hook ([24ccb46](https://github.com/example/mpm/commit/24ccb46ac8890aeca7137c27563c15f6999297f1))
  > Adds new top-level `mpm route` command (text renderer) and `mpm call route`
  > JSON-RPC entry, both backed by the existing `internal.Router`. The hook
  > config is documented in README only — no install helper. Supersedes the
  > deleted claudecode-mpm-plugin (Python MCP-server approach).
  > 
  > Co-Authored-By: Claude Opus 4.7 <noreply@anthropic.com>
- add route tool to README — auto-selection architecture and scoring rules ([0acb63b](https://github.com/example/mpm/commit/0acb63b902d584f3fbd8620d00f302684379044e))
- router: add hot-reload via directory mtime check ([b17b15d](https://github.com/example/mpm/commit/b17b15d338e502246515f0e95fd3d18d2c9cac5c))
  > maybeReload() runs on every Evaluate() call. It stats the mode/ and
  > persona/ directories; if any file's mtime is newer than the last cached
  > mtime, the full reload() fires — reparses YAML, recompiles all regex.
  > 
  > No-op for the common case (no files changed). New mode/persona files
  > or pattern edits are picked up automatically without restarting mpm-mcp.
- Add route MCP tool — zero-latency heuristic mode/persona auto-selection ([3131e29](https://github.com/example/mpm/commit/3131e29d2988d1e7f1fdfc06ca0f35453cf27216))
  > New 'route' tool evaluates prompts against pre-compiled regex from all
  > mode/*.md and persona/*.md files. Scoring:
  > - Modes: threshold filter (any score ≥1 activates; multi-select)
  > - Personas: max-pooling (highest score wins if ≥1; single-select)
  > - Explicit frontmatter patterns: weight 2 per match
  > - Body-text implicit patterns: weight 1 per match
  > - Anti-patterns: -1 per match (suppresses false positives)
  > 
  > Router is built once at mpm-mcp boot — all regex compiled then, so
  > Evaluate() is pure string matching with zero parsing overhead.
  > 
  > Files:
  > - internal/router.go: scoring engine + Router struct
  > - internal/router_loader.go: YAML frontmatter parser (handles both
  >   comma-separated string and []string for patterns/anti_patterns),
  >   body-text implicit pattern extraction
  > - internal/router_test.go: 5 test cases covering write/architect/
  >   research modes and persona selection
  > - cmd/mpm-mcp/main.go: Router bootstrapped at server boot
  > - cmd/mpm-mcp/tools.go: route tool spec + handleRoute handler
  > 
  > Also updated mode/write.md and mode/research.md with explicit patterns
  > fields so those modes reliably trigger on relevant prompts.
- mode(write): add synthetic transitions to anti-patterns ([031b542](https://github.com/example/mpm/commit/031b5420cf3bcc7186f679a592ceeea63ff8af77))
  > Formulaic bridge phrases: That being said, Moving forward,
  > It is worth noting, On a related note — structural hand-waving
  > that signals flow without providing it.
- mode(write): add Write mode with AI writing detection patterns ([5e0c70c](https://github.com/example/mpm/commit/5e0c70c92911abfc60da0f352227c585d5c0dc5a))
  > Based on Wikipedia's Signs of AI Writing field guide (PNAS 2025,
  > ACL 2025, Science Advances 2025). Covers significance inflation,
  > AI vocabulary clustering, superficial analysis appends, copulative
  > avoidance, generic erosion, notability claims, and knowledge cutoff
  > disclaimers. Pre-output check and rewrite rule included.
- add MPM whitepapers from ChatGPT and DeepSeek ([2d96562](https://github.com/example/mpm/commit/2d96562e1aac506fa50325bfb3a605ca59ec7874))
  > - MPM_Technical_Architecture_Whitepaper.docx (ChatGPT, ~834w)
  > - MPM_Whitepaper.docx (ChatGPT, ~433w)
  > - MPM_Whitepaper_Extended.docx (ChatGPT, ~930w)
  > - mpm deepseek whitepaper.odt (DeepSeek, ~2768w)
- **gitignore:** drop 'mpm' rule that was blocking cmd/mpm/call.go ([7323f54](https://github.com/example/mpm/commit/7323f549bd73f9c8ec1fbb7cc30b39bf5470d749))
  > The bare 'mpm' pattern matched both the compiled binary and
  > cmd/mpm/call.go, forcing the call.go refactor to be committed
  > with 'git add -f'. Removing the rule means future edits to
  > cmd/mpm/ files are no longer gitignored.
- **claudecode-mpm-plugin:** add README ([abc0b81](https://github.com/example/mpm/commit/abc0b8122e81b85f8275cadbdb70aa1057b61c96))
- **claudecode-mpm-plugin:** drop unused FastMCP import ([30f1c8c](https://github.com/example/mpm/commit/30f1c8c83f5e409049546e8f4d7a7df7031d4b0a))
  > Code review noted: 'from mcp.server.fastmcp import FastMCP' was
  > imported in test_server.py but never used (we only import 'mcp'
  > from server.py and introspect mcp._tool_manager._tools).
- **claudecode-mpm-plugin:** add server integration test ([422a418](https://github.com/example/mpm/commit/422a41883c28cf6c74f00e9f2b3c2e81c9769e79))
- **claudecode-mpm-plugin:** actually verify run_mpm timeout clamping ([633da05](https://github.com/example/mpm/commit/633da05520a795b2589d5463a72cc44b4b43f34c))
  > Code review caught: the original test_run_mpm_clamps_timeout_to_max
  > only asserted that the mock was called, not that the timeout was
  > actually clamped. Replaced with a test that patches asyncio.wait_for
  > and asserts the captured timeout equals MAX_TIMEOUT_MS / 1000 when
  > 1 hour (3600s) is requested.
  > 
  > The test mocks wait_for with a side_effect that captures the timeout
  > arg and re-invokes the real wait_for with timeout=None so the
  > underlying coroutine resolves immediately.
- **claudecode-mpm-plugin:** add failing tests for run_mpm ([7e062b0](https://github.com/example/mpm/commit/7e062b093b0cd7cedd3d4035cff229b508ea36fd))
- **claudecode-mpm-plugin:** make test_debug_log_swallows_io_errors actually exercise OSError ([1cf269a](https://github.com/example/mpm/commit/1cf269af04a37006f53aedfbab1358c864a7d11a))
  > Code review caught: bad_path.mkdir() created a directory, so
  > os.makedirs(parent) succeeded and the test passed trivially. Switched
  > to bad_path.touch() (a file), so any code that tries to makedirs
  > the parent raises NotADirectoryError (an OSError subclass) — that's
  > the path the swallow-OSError impl must handle.
- **claudecode-mpm-plugin:** add failing tests for debug_log ([27dc30c](https://github.com/example/mpm/commit/27dc30cba2013aae8220467f35110d2282d4e800))
- **claudecode-mpm-plugin:** add failing tests for format_age ([ea8c62c](https://github.com/example/mpm/commit/ea8c62c8f2dcd09d4427d8d99d492fbc8341c0fa))
- **plan:** correct parse_mpm_result deviations found during Task 6 ([298109f](https://github.com/example/mpm/commit/298109fc2f2eeb5fb7154e16e33bfe01b6f885e0))
  > - 'exceeds' -> 'exceeded' in wake_context_truncated message
  >   (the test at line 314 asserts 'exceeded' in message; the literal
  >   code block was inconsistent with the test).
  > - SQLITE_BUSY check: assign stderr.lower() once, use it for both
  >   substrings. Catches mixed-case variants in real-world stderr.
- **claudecode-mpm-plugin:** add parse_mpm_result edge-case tests ([b7e8550](https://github.com/example/mpm/commit/b7e8550a6daefca0944c6c5d356643ac1697ef07))
- **claudecode-mpm-plugin:** add failing test for parse_mpm_result happy path ([4b4ff71](https://github.com/example/mpm/commit/4b4ff7172fd24f7bed6acbf529134b224187bc8f))
- design spec for claudecode-mpm-plugin (workspace MCP plugin) ([28497a9](https://github.com/example/mpm/commit/28497a9d40ccbae9b0d640fbdaaf032b5d86f3e5))
  > Scaffolds a workspace-level Claude Code plugin that exposes MPM's
  > reasoning primitives to Claude Code via the Model Context Protocol.
  > Mirrors the 18-tool surface of opencode-mpm-plugin (TypeScript →
  > Python with the official mcp SDK) and adds a skills/mpm/SKILL.md
  > that anchors agent workflow.
  > 
  > End state: 18 tools (memory, lessons, topics, references, session,
  > epistemology, proactive). Implementation is phased: 2 tools in
  > Phase 1, scale to 18 in Phase 3.
  > 
  > Source of truth lives in a sibling claudecode-mpm-plugin/ folder;
  > install.sh creates a project-local venv and symlinks into .claude/.
  > 
  > Co-Authored-By: Claude Opus 4.7 <noreply@anthropic.com>

## [orphans] - 2026-06-19

### Other

- orphan memory id=798f068e4794b548
  > commit hash claimed: deadbeefdeadbeefdeadbeefdeadbeefdeadbeef
  > 
  > [commit: deadbeefdeadbeefdeadbeefdeadbeefdeadbeef]
  > 
  > orphan test - this hash matches no commit in the log

