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

31 entries. Sorted so current material comes first, then by status and name.

| Entry | Status | Why it is here |
|---|---|---|
| [`CONTEXT_EXPOSURE.md`](CONTEXT_EXPOSURE.md) | Current | Why the default MCP surface is exactly three tools, and the measurement that justified it. |
| [`tool-behavioral-contract.md`](tool-behavioral-contract.md) | Current rules, historical audit | The project-wide answers to tool-semantics questions that are otherwise decided per-tool: not-found rules by verb class, id naming, silent-coercion policy, idempotency classes. |
| [`shared-epistemology.md`](shared-epistemology.md) | Current — architecture shipped | The SQLite-ATTACH shared-epistemology architecture and its operator-only rule for shared writes. |
| [`f71-challenge-restoration.md`](f71-challenge-restoration.md) | Current — concept vocabulary | Synonym anchors for the F7.1 challenge/restoration fix; also benchmark vocabulary. |
| [`f81-cancel-verification.md`](f81-cancel-verification.md) | Current — concept vocabulary | Synonym anchors for the F8.1 cancel/verification fix; also benchmark vocabulary. |
| [`provenance-env.md`](provenance-env.md) | Current — contract | The provenance environment-variable contract: two channels, one canonical variable, and their precedence. |
| [`concept-vocabulary-authoring.md`](concept-vocabulary-authoring.md) | Current — convention | How to author memories that survive paraphrased retrieval instead of only exact identifiers. |
| [`2026-08-28-mpm-skill-workshop-design.md`](2026-08-28-mpm-skill-workshop-design.md) | Current — implemented | The Skill Workshop: how a skill is reviewed, proposed and accepted. |
| [`2026-09-30-compact-refusal-lifecycle.md`](2026-09-30-compact-refusal-lifecycle.md) | Current — implemented | How a refused compaction is recorded, recovered and retried. |
| [`2026-09-30-work-archive-and-purge.md`](2026-09-30-work-archive-and-purge.md) | Current — implemented | Work-archive and purge semantics: what is archived, what is purged, and what is never destroyed. |
| [`epistemic-cascades.md`](epistemic-cascades.md) | Current — operator guide | Operator guide to epistemic cascades, including the positive-direction cascade added 2026-09-07. |
| [`epistemic-confirmation.md`](epistemic-confirmation.md) | Current — operator guide | Operator guide to epistemic confirmation and contradiction. |
| [`2026-08-08-artifact-provenance-design.md`](2026-08-08-artifact-provenance-design.md) | Current — schema rationale | The provenance data model — cited as the spec by the schema and the provenance handlers. |
| [`directives.md`](directives.md) | Current — scoping semantics | Directive scoping and multi-framework filtering: why the substrate, not the host, filters directives. |
| [`capability-lifecycle.md`](capability-lifecycle.md) | Current — specification | Capability lifecycle semantics — the authority the `internal/core/capability/` package cites. |
| [`2026-08-21-telemetry-binary-design.md`](2026-08-21-telemetry-binary-design.md) | Current — wire schema | The telemetry wire format that `internal/telemetry/frame.go` mirrors. |
| [`full-tool-behavioural-audit-2026-09-05.md`](full-tool-behavioural-audit-2026-09-05.md) | Historical — audit | The defect audit that produced the contract. Evidence base for its four rules; its defect list is closed. |
| [`onboarding-mcp-native-audit-2026-09-05.md`](onboarding-mcp-native-audit-2026-09-05.md) | Historical — audit | Audit of MPM's onboarding channels against MCP-native discovery, and the one deliberate omission it justifies. |
| [`embedding-config.md`](embedding-config.md) | Historical — config schema | The embedding configuration file schema. Historical — check `docs/CONFIGURATION.md` and the code for the current shape. |
| [`2026-10-03-hitl-log-redesign.md`](2026-10-03-hitl-log-redesign.md) | Historical — design | The reasoning behind the human-in-the-loop log's shape. |
| [`constructive-cascade-design.md`](constructive-cascade-design.md) | Historical — design investigation | The deferred design for positive-direction cascades, and the three prerequisites it named. Source of the polarity-column rationale. |
| [`pointer-indirection-audit-2026-09-05.md`](pointer-indirection-audit-2026-09-05.md) | Historical — findings partly applied | Pointer-indirection findings behind the output policy and the spill-roundtrip test. |
| [`pointer-indirection-sweep-2026-09-05.md`](pointer-indirection-sweep-2026-09-05.md) | Historical — findings partly applied | The exhaustive sweep that followed it; source of finding F-S-2. |
| [`2026-06-16-confidence-evidence-foundation-design.md`](2026-06-16-confidence-evidence-foundation-design.md) | Historical — implemented | The design that made confidence evidence-derived rather than asserted. |
| [`2026-07-25-skills-in-mpm-design.md`](2026-07-25-skills-in-mpm-design.md) | Historical — implemented | The original skills-in-MPM design. Superseded in shape by the Skill Workshop, but still the source of the skill data model. |
| [`2026-07-30-unix-epoch-timestamps-design.md`](2026-07-30-unix-epoch-timestamps-design.md) | Historical — implemented | The migration that unified timestamps on Unix-epoch seconds, and the manifest-typing bug it closed. |
| [`2026-08-04-epistemic-cascades-design.md`](2026-08-04-epistemic-cascades-design.md) | Historical — implemented | The schema rationale behind cascades for decisions and theories. |
| [`2026-09-03-mpm-embedding-provider-design.md`](2026-09-03-mpm-embedding-provider-design.md) | Historical — implemented | Why the embedding subsystem is explicit and four-state instead of a silent env-only fallback chain. |
| [`blob-dir-permissions-incident-2026-09-05.md`](blob-dir-permissions-incident-2026-09-05.md) | Historical — incident, constraint live | A blob-directory permission incident whose fix is pinned by a live test — explains why the check exists. |
| [`CONTEXT_ECONOMICS.md`](CONTEXT_ECONOMICS.md) | Historical — measurements | How much model-facing context MPM costs, and what the fixed cost buys. Method and harness are current; the numbers are not. |
| [`final-pass-lifecycle-matrix.md`](final-pass-lifecycle-matrix.md) | Historical — release matrix | Which lifecycle verb exists per artifact, which is deliberately unavailable, and why. Pre-rename command names. |

## What is *not* here

Two categories were deliberately left out, and both are recoverable from git
history:

* **Closed sprint reports and per-defect work logs.** They record that work
  happened, not why the system is shaped the way it is. `docs/dev/` holds the
  live ones; the rest are history.
* **Superseded sketches.** A design that was replaced before implementation
  teaches nothing that the replacement does not.

The one exception worth naming: `full-tool-behavioural-audit-2026-09-05.md`
*is* a per-defect log, and it is here because the tool contract cites it as the
evidence for its rules. A reader who follows that citation must land somewhere.
