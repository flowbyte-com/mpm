# v2 calibration corpus — manual semantic review

This file is the manual semantic review of every negative
query's `label_rationale` in `bench/corpus/v2/queries.json`.
The contract is enforced at load time by
`bench/tests/test_corpus_v2.py::TestV2LabelRationale`, which
checks that every negative query has a non-empty rationale of
at least 20 characters and that at least 3 hard negatives are
marked. This file documents the per-query reasoning that
informs the labels and is the audit trail for the corpus
author's intent.

The v1 corpus's negative-query labels (the historical 30-query
benchmark at `bench/corpus/v1/`) did not have this audit
trail; v1 queries have empty `label_rationale` by design and
the v2 contract is not applied to v1. The v1 label defect
flagged by HAL is documented in
`docs/ISSUE-15-PHASE-A-REPORT.md` §10 (v1 measurement
preserved) and A2.1 (v1 measurement marked unsuitable for
calibration).

## Negative queries (20 total)

| qid | Query | Family | Hard? | Rationale summary |
|---|---|---|---|---|
| q03001 | "What programming language is MPM written in?" | all | no | Genuine negative; answer (Go) is not in any v2 document. Verified by lexical search. |
| q03002 | "What is MPM's license?" | all | no | Genuine negative; corpus has no document about licensing. |
| q03003 | "Who created MPM?" | all | no | Genuine negative; corpus is about MPM internals, not authors. |
| q03004 | "What is the size limit of a memory content?" | all | yes | Hard negative: lexically close to d02014 (the content column), but the size limit is not stated. Tests "related but unanswered". |
| q03005 | "How do I install MPM?" | all | no | Genuine negative; corpus has no document about installation. |
| q03006 | "What is the latest version of MPM?" | all | no | Genuine negative; corpus has no document about versioning. |
| q03007 | "Does MPM support Windows?" | all | no | Genuine negative; corpus has no document about platform support. |
| q03008 | "What is the MPM release cadence?" | all | no | Genuine negative; corpus has no document about releases. |
| q03009 | "Does MPM have a web UI?" | all | no | Genuine negative; corpus has no document about UI. |
| q03010 | "What is the MPM API rate limit?" | all | no | Genuine negative; corpus has no document about rate limits. |
| q03011 | "How does MPM handle encrypted memories?" | all | yes | Hard negative: lexically close to d02019 (identity hash) and d02038 (embedding BLOB), but encryption is not addressed. Tests "lexically close but unanswered". |
| q03012 | "What is MPM's consensus protocol?" | all | yes | Hard negative: MPM does not have a consensus protocol. The corpus documents WAL mode (d02033) but no consensus layer. Tests "related but wrong direction". |
| q03013 | "How does MPM replicate across regions?" | all | yes | Hard negative: MPM does not have a replication layer. The corpus documents SQLite WAL (d02033) but no cross-region replication. |
| q03014 | "How does MPM integrate with Kubernetes?" | all | no | Genuine negative; corpus has no document about Kubernetes. |
| q03015 | "What is the difference between MPM and Redis?" | all | no | Genuine negative; corpus has no document comparing MPM to other systems. |
| q03016 | "What is MPM's pricing?" | all | no | Genuine negative; corpus has no document about pricing. |
| q03017 | "Does MPM offer a hosted version?" | all | no | Genuine negative; corpus has no document about hosting. |
| q03018 | "What is MPM's SLA?" | all | no | Genuine negative; corpus has no document about SLAs. |
| q03019 | "How does MPM compare to Elasticsearch?" | all | no | Genuine negative; corpus has no document comparing MPM to other systems. |
| q03020 | "Does MPM support GraphQL?" | all | no | Genuine negative; corpus has no document about GraphQL. |

## Hard negative semantics

A "hard negative" is a query that is MPM-related (so a naive
lexical or topical match would surface candidate documents)
but whose specific answer is NOT in the corpus. The intent is
to test the ranker's ability to detect "related but
unanswered" rather than just "orthogonal and answered".

The four hard negatives in v2 are:

1. **q03004 (size limit of memory content)**: the corpus
   documents the `content` column (d02014) but does not
   state a size limit. A BM25 lexical search will find
   d02014, but a real answer requires a size-limit document
   that is not in the corpus.
2. **q03011 (encrypted memories)**: the corpus documents
   identity hashing (d02019) and embedding BLOBs (d02038),
   both of which lexically match "encrypted" or "secure"
   terms. The corpus does not document an encryption layer.
3. **q03012 (consensus protocol)**: the corpus documents
   SQLite WAL mode (d02033), which lexically matches
   "consensus" and "replication" terms. MPM does not have a
   consensus protocol; the WAL mode is local-only.
4. **q03013 (cross-region replication)**: same as q03012;
   the corpus documents WAL (d02033) but no replication
   layer.

These four queries are the closest thing in v2 to a "the
corpus has related content but does not actually answer
this" test. The Option D policy evaluation in
`docs/ISSUE-15-PHASE-A-REPORT.md` A2.4 shows that the
production ranker produces 10 vector results for every
negative query (including the four hard negatives), with
top-1 cosine 0.57-0.78. The hard negatives' top-1 cosines
are within the same range as the genuine negatives'
(0.57-0.78); the ranker does not distinguish "related but
unanswered" from "unrelated and unanswered" on top-1
similarity alone.

## Topic families and split

The 5 topic families in v2 are:

- `retrieval_core` (12 docs, d02001-d02012): documents
  about MPM's hybrid search, FTS5, vector retrieval, score
  blending.
- `memory_lifecycle` (12 docs, d02013-d02024): documents
  about memory insertion, retrieval, confidence, weight,
  identity hashing, deletion, contradiction.
- `challenge` (8 docs, d02025-d02032): documents about the
  challenge / restore / adjudicate workflow.
- `schema_persistence` (8 docs, d02033-d02040): documents
  about the SQLite schema, FTS5 shadow tables, WAL,
  embedding storage.
- `search_operators` (10 docs, d02041-d02050): documents
  about MPM's `mpm` subcommands (recall, memory, challenge,
  ops, doctor, etc.). This is the held-out topic family.

The held-out split isolates `search_operators` from the
calibration scope. The harness ingests all 50 documents; the
held-out queries target docs in the held-out family only.
Calibration POSITIVE queries may target held-out documents
(the ranker is being calibrated on the held-out doc set),
but calibration NEGATIVE queries must have zero relevant
docs in the held-out set (a negative query with a relevant
held-out doc would be a label defect).

## Paraphrase pairs (9 pairs)

Each paraphrase targets the same `relevant_doc_ids` as its
base. The 9 pairs are recorded in `queries.json` via
`paraphrase_of`; the test
`bench/tests/test_corpus_v2.py::TestV2Paraphrases` enforces
the contract. The pairs test whether the ranker can find
the same document when the query is rephrased — a
robustness test, not a calibration test.

## Verification status

| Check | Status | Reference |
|---|---|---|
| Every negative has non-empty rationale | passed | `TestV2LabelRationale::test_every_negative_has_rationale` |
| Every rationale ≥ 20 characters | passed | `TestV2LabelRationale::test_every_rationale_mentions_unanswerable` |
| At least 3 hard negatives | passed (4 present) | `TestV2LabelRationale::test_hard_negatives_marked` |
| Every `paraphrase_of` is a valid positive | passed | `TestV2Paraphrases::test_every_paraphrase_has_matching_base` |
| 9 paraphrase pairs present | passed | `TestV2Paraphrases::test_paraphrase_count` |
| v1 unchanged | passed | `TestV1Preserved::test_v1_hashes_match_recorded` |
| v1 not retroactively validated against v2 contract | passed | `TestV1Preserved::test_v1_no_v2_contract_enforced` |

All 12 tests in `bench/tests/test_corpus_v2.py` pass.
