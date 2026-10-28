# `docs/library/` — curated reference material

This directory holds documents that are **worth keeping but are not a
description of current behaviour**. It exists so that an agent can tell the
difference between *"this is how MPM works today"* and *"this is why MPM works
this way"*.

That distinction is the whole point. A design rationale that is three months
old is still valuable; a command reference that is three months old is a
hazard. Mixing the two is how a reader ends up following an instruction that
has not been true since a refactor.

## The rule for entry

A document belongs here if **at least one** of these is true:

1. **Current code or an authoritative document points at it** as the definition
   or rationale of behaviour that still exists. (A dangling `Spec: docs/…`
   pointer in a Go comment is the clearest signal: the code is telling you where
   its semantics are written down.)
2. **It records a design decision and the reasoning behind it**, and that
   reasoning still constrains what the code may do.
3. **It records an empirical finding** — a benchmark, a compatibility result, an
   incident post-mortem — whose conclusion is still cited or still explains a
   constraint.

A document does **not** belong here merely because it exists. Sprint reports,
superseded sketches, per-defect work logs and anything whose useful content is
already carried by `docs/SPEC.md` are deleted; git history keeps them.

## Status labels

Every entry carries a status label at the top. The label is a claim, and it is
reviewed like one.

| Label | Means | Trust it for |
|---|---|---|
| **Current** | The rules or facts still hold. | Following the rules. |
| **Current rules, historical audit** | The stated rules hold; the per-item tables were taken at a point in time and may contain superseded rows. | The rules, after checking a row against the live registry. |
| **Historical** | Describes a state of the system that has changed. Valuable for *why*, never for *what*. | Understanding a constraint's origin. |
| **Superseded** | A later document or decision replaced this. Kept because the reasoning still explains the replacement. | Nothing — read the successor. |

**A library entry never overrides `docs/SPEC.md`.** If a library entry and the
spec disagree about *current* behaviour, the spec wins and the library entry
needs a correction or a superseded label.

## What the freshness gate checks here

`make docs-check` validates the **links and metadata** of library entries — that
every relative link resolves, and that every entry carries a status label. It
does **not** check that a historical entry describes current behaviour, because
by definition it does not.

## Index

| Entry | Status | Why it is here |
|---|---|---|
| [`tool-behavioral-contract.md`](tool-behavioral-contract.md) | Current rules, historical audit | The project-wide answers to tool-semantics questions that are otherwise decided per-tool: not-found rules by verb class, id naming, silent coercion, idempotency classes. Cited as the authority by `docs/CONTRIBUTING.md` and by `internal/core/db.go`, `internal/core/schema.go` and several tests. |
