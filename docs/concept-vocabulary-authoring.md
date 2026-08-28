# Concept-Vocabulary Authoring Convention

A small, repeatable authoring discipline for high-value engineering memories so that future agents can find them via paraphrased natural-language queries — not just exact identifiers.

## When to apply this convention

Apply when recording any of:

- **P0/P1 bug fixes** (release blockers, alpha blockers, security audit findings)
- **Architectural decisions** with cross-cutting impact (e.g. lifecycle ↔ trust coupling)
- **Release-gate remediation** (anything that closes or opens a release gate)
- **Load-bearing invariants** that future contributors might violate unintentionally

Do not apply to: ephemeral debugging notes, routine commits, ordinary lessons, and small fixes. The convention is intentionally reserved for memories where the cost of being unfindable is high.

## Required structure

Each canonical artifact of the above kind MUST record:

| Field | Purpose | Example |
|---|---|---|
| **Canonical title** | Identifies the artifact internally | "F8.1 — Cancel / Verification Coupling" |
| **Problem** | The user-visible failure mode | "A cancelled work row could carry `verification='verified'`" |
| **Root cause** | Why the system let it happen | "lifecycle columns and verification columns were written/read independently" |
| **Architectural invariant** | The rule the fix enforces | "Cancelled status locks verification below `verified`" |
| **Fix** | What changed | "`DeriveWorkVerification` consults `works.status` first" |
| **Implementation location** | File:function paths | `internal/core/db.go:DeriveWorkVerification` |
| **Regression tests** | Test file names | `internal/core/f71_f81_cancellation_challenge_test.go` |
| **Validation** | How it was proven | "831 core tests pass, 0 failures" |
| **Release impact** | What moved on the release gate | "Test 8: PASS WITH ANOMALIES → PASS" |
| **Concept vocabulary** | Synonym-rich natural-language terms | See rules below |

## Concept vocabulary rules

The vocabulary MUST be:

1. **Human-readable** — terms a domain-aware person would naturally write when reasoning about the problem.
2. **Searchable** — each term is a token that FTS5 can index. No stopword-only terms ("the", "of", "after"). No punctuation-only terms.
3. **Stable** — do not invent new jargon. Prefer the terms a code reviewer would write in a sentence.
4. **Domain-relevant** — every term must connect to the underlying bug, not the fix code itself.
5. **Small** — typically 10–25 terms per artifact. Authors must use the convention, so bloat kills adoption.

The vocabulary MUST include:

- **Problem-side terms**: things a fresh agent might say when describing the bug ("abandoned operation", "stopped task", "false success").
- **Solution-side terms**: the principle the fix enforces ("evidence neutralization", "lifecycle gates verification").
- **Cross-domain synonyms**: phrases that mean the same thing in different vocabularies.

The vocabulary MUST NOT include:

- The canonical identifier (F7.1, F8.1) — those are already in the title and are reachable directly.
- Internal function/file names — those belong in "Implementation location", not "Concept vocabulary".
- Negative-only terms ("bad", "broken", "wrong") — they pollute retrieval of legitimate uses.
- More than 30 terms — past that point authors stop being able to maintain the list.

## Worked examples

The first artifacts to use this convention are the F7.1 and F8.1 alpha-p1 fixes. See:

- `docs/archive/concept-vocabularies/f71-challenge-restoration.md`
- `docs/archive/concept-vocabularies/f81-cancel-verification.md`

Each applies the convention by listing problem-side terms, solution-side terms, and cross-domain synonyms — all distinct from the canonical identifiers and code paths already recorded elsewhere.

## Why this convention

Cross-session archaeology queries are often paraphrased. A fresh agent may ask "Why might an abandoned task still appear successful?" when the canonical record says "F8.1 — Cancel / Verification Coupling". Without synonym-rich vocabulary, BM25 finds nothing because the words don't overlap.

The convention does NOT replace semantic retrieval; it gives semantic retrieval something to retrieve. Where the substrate already supports vector embeddings, the vocabulary terms become anchors that embeddings cluster around — even if BM25 misses them.

If retrieval stays lexical-only (BM25/FTS5 without embeddings enabled), this vocabulary is still valuable because it directly enables paraphrased natural-language queries to hit the right memory on first try.

## Reviewing compliance

When reviewing a PR that adds a memory matching the "when to apply" criteria above, reviewers must check:

- Does the artifact include all 10 fields, with the vocabulary distinct from identifiers/code paths?
- Are there at least 10 vocabulary terms?
- Are at least 5 terms phrased as problem-side natural language (what a fresh agent would type)?
- Is each term a single token FTS5 can index (not a phrase, not a sentence)?
