# MPM Skill Packages — a multi-file skill format

> **Status: Proposed — at the architecture approval gate.**
>
> This document is a design proposal. It contains no implementation, no
> schema migration, and no behaviour change. Nothing in the product changes
> until the decision in §1 is taken and the stages in §10 are merged
> separately.
>
> **Base commit:** `f8d4b95aefe60a0880409a8905ce2c4d92d22c00` (`main`).
> **Scope:** the skill collection only.

---

## 1. The decision requested

MPM skills are single Markdown documents. Real skills are packages: an
entrypoint plus supporting references, scripts and assets, distributed as a
ZIP archive. This document proposes a portable, versioned, multi-file skill
format and the persistence, security and retrieval contracts that go with it.

**The recommendation is:** keep the existing single-document skill exactly as
it is, and add a *package* layer beside it rather than replacing it. A package
is a manifest plus immutable file contents, stored in three new SQLite tables
that are **additive** — no existing table, row, index or trigger is modified,
and rollback is dropping three tables.

**What is being asked for:** approval of the design in §4–§10, and answers to
the ten operator decisions in §12. No code is proposed for merge by this
document.

**What is deliberately not decided here:** the cross-workspace shared-database
extension (decision **O4**), binary-asset defaults (**O3**), and whether a
published package may be replaced in place (**O2**). Each has a recommendation
and a cost.

---

## 2. The reference package

The operator supplied a real third-party skill package. It is a ZIP archive
named `system-prompt-writer.skill`, 14 536 bytes, SHA-256
`ef695a85f0f611d70526a8d8572876149841da6459dbdd511063709fccfd8058`.

It contains exactly three files, all regular mode `0644`, all deflated:

| path | bytes | SHA-256 |
|---|---|---|
| `system-prompt-writer/SKILL.md` | 12 976 | `783ece4b3869ae42d2d46f540d01d1da0f61147a8eada519eff15941b46d423d` |
| `system-prompt-writer/references/templates.md` | 8 176 | `d5086d2156bfbdb6a6ab5c9d9b7975e0a080317ff2c697db087ef8a83a05dfab` |
| `system-prompt-writer/references/principles.md` | 11 011 | `982e290090180f4ac2d22160f6be1bccee302549ae35302783dd8bfa82727447` |

Four properties of this package drive the whole design, and a fifth confirms
the identity choice:

**2.1 The entrypoint has no `version` field.** Its frontmatter block is 702
bytes and contains exactly two keys: `name: system-prompt-writer` and a
long `description:`. MPM's own parser rejects it:

```
ParseSkillFrontmatter(SKILL.md)
  → frontmatter missing required field: version
```

This is the correct behaviour of today's code — `internal/core/skill.go:109-111`
requires `version`, and `cmd/mpm/r9_t52_skill_version_test.go:60-87` pins the
rejection. The design therefore has to handle a package that predates MPM's
version contract **without** weakening that contract and **without** rewriting
a third-party file.

**2.2 The supporting files are not skills.** `references/principles.md` and
`references/templates.md` have no frontmatter at all. Feeding them to
`ParseSkillFrontmatter` gives `frontmatter missing: must start with "---"`.
They are package *files* — content, not procedures — and must never be routed
through the skill frontmatter validator.

**2.3 The entrypoint references its siblings by package-root-relative path.**
`SKILL.md` mentions `references/templates.md` (line 32) and
`references/principles.md` (line 45) as inline code spans, not Markdown links.
The author's own reference convention is therefore *relative to the package
root*, which is what the retrieval contract in §8 adopts.

**2.4 The package is clean under MPM's scanners.** All three files pass
`ScanContentForWrite` — no secret pattern, no poison phrase. This matters
because the scanner is the control that makes storing third-party bytes safe
(§7), and it must be applied per file, not per package.

**2.5 The package name is a valid MPM skill identity.** `system-prompt-writer`
contains no `-v` substring, so it survives the id format's collision rule
(`internal/core/skill.go:194-200`). Measured against MPM's own helpers:

```
SkillIDForNameAndVersion("system-prompt-writer", "1.0.0")
  → "skill:system-prompt-writer-v1.0.0"
ParseNameAndVersionFromID("skill:system-prompt-writer-v1.0.0")
  → name="system-prompt-writer" version="1.0.0"   (round-trips)
```

That is what makes §5.1's choice — the package's primary key *is* the skill
id — workable for this package without any special case.

The package was also probed with Go's `archive/zip` to establish what the
standard library does and does not defend against. Those results are the
evidence base for §7 and are reproduced in §11.

---

## 3. Existing architecture

### 3.1 What exists today

| fact | where |
|---|---|
| A skill is a `memories` row with `collection='skills'` | `internal/core/skill.go:1-6` |
| Frontmatter lives inside the row's `content`; `name` and `version` are both required | `internal/core/skill.go:75-113` |
| Row identity is deterministic: `skill:<name>-v<semver>` | `internal/core/skill.go:174-202` |
| A skill name may not contain `-v` (it would break id round-tripping) | `internal/core/skill.go:194-200` |
| Version comparison is `golang.org/x/mod/semver`, `v`-prefixed at compare time | `internal/core/skill_db.go:159-177` |
| "Latest" is *semver-max over live rows*; `metadata.is_latest` is a mirrored flag, not the authority | `internal/core/skill_db.go:288-305`, `411-436` |
| The frontmatter `name`/`version` must match the arguments passed to the save call | `internal/core/skill_db.go:504-509` |
| Re-saving an existing `(name, version)` requires `force=true`; force overwrites in place and resurrects a tombstone | `internal/core/skill_db.go:517-539`, `572-641` |
| A newer version flips every strictly-lower version's `is_latest` inside the same transaction | `internal/core/skill_db.go:643-737` |
| Every skill write is scanned for secrets and poison phrases — twice: once in skill validation, once in the shared insert primitive | `internal/core/skill_db.go:56-63`, `internal/core/db.go:4488-4506` |
| Scanner coverage over *all* `memories` writers is enforced by a static AST walk | `internal/core/scanner_coverage_test.go` |
| `ListSkills` parses frontmatter out of every skill row's full `content`, then returns a metadata-only summary | `internal/core/skill_db.go:370-451` |
| Discovery carries summaries only — never a body — and the catalogue is capped at 20 entries | `internal/core/wake_skills.go:5-16`, `internal/core/wake_context.go:169-172` |
| `available_skills` is the **first** array shed when the wake payload exceeds 32 KiB | `internal/core/wake_context.go:321-331` |
| Proactive recall matches `when_to_use` by case-insensitive substring | `internal/core/directive_tools.go:216-300` |
| Soft shred stamps `deleted_at`; it does **not** overwrite content | `internal/core/skill_db.go:852-877` |
| Hard shred deletes the row plus `topic_memberships` and `confidence_history` — but **not** `artifact_provenance` and **not** `retrieval_metadata` | `internal/core/skill_db.go:879-911` |
| Promotion flips `is_global=1` on the same row. It is in place, not a copy, and there is no demote | `internal/core/skill_db.go:772-850` |
| Backup is `sqlite3 .dump`; restore validates against an allow-list of tables | `cmd/mpm/handlers_backup.go`, `internal/core/sql_dump_validator.go:141-203` |
| A new table that is not on the allow-list makes `mpm restore-db` **reject** the dump | `internal/core/sql_dump_validator.go:530+` |
| Schema is Go string slices; migrations are additive `ALTER TABLE` plus sentinel-gated one-shot files. **There is no rollback mechanism** | `internal/core/schema.go:1657-1852`, `internal/core/db.go:2967-2976` |
| The MCP surface is a single `mpm_skills` aggregator with an action enum | `internal/core/tools/registry_list.go:337-376` |
| The action enum is pinned to contain `save`, `read`, `list`; **adding** actions is compatible | `internal/core/tools/schema_guard_test.go:548-575` |
| There is no bulk skill import, and no generic ZIP utility with path guards anywhere in the tree | `internal/core/reference_new.go:110-144` is the only `archive/zip` use |

### 3.2 The blobstore hazard

This is the single most important constraint on the design, and it is easy to
get wrong.

`internal/blobstore` stores payloads *outside* the database, in
`<workspace>/blobs/`, with a metadata row in the `blobs` table. Two properties
of that store are incompatible with permanent assets:

* **`GCExpired` is unconditional.** It deletes every blob whose `expires_at`
  has passed, with no referential check and no grace period
  (`internal/blobstore/fs.go:537-587`). The default TTL is 24 hours. A skill
  asset stored as a blob would be silently destroyed a day after import.
* **`GCSweepOrphans` deletes any file in the blob directory that has no
  database row, regardless of age** (`internal/blobstore/fs.go:592-726`).

Two further hazards live in the memory layer:

* **Auto-prune hard-deletes expired memory rows.** `DELETE FROM memories WHERE
  expires_at IS NOT NULL AND expires_at < now` — not collection-aware except
  for directives (`internal/core/memory.go:1791-1801`). Skills are safe today
  only because they are written with `expires_at` NULL.
* **The read paths disagree about expiry.** `MemoryExpireClause` is applied by
  export and search but **not** by `ReadSkill` or `ListSkills`
  (`internal/core/web_db.go:42` versus `internal/core/skill_db.go:268`, `370`).
  An expired skill would vanish from `mpm export` while remaining readable.

**Conclusion, carried into §4:** no byte of a skill package may ever enter
`internal/blobstore` or the blob directory. Permanent skill assets and a
24-hour TTL are not compatible, and the failure is silent.

### 3.3 Compatibility requirements

The design must not break these. They are the acceptance criteria for every
stage in §10.

| | requirement |
|---|---|
| **C1** | An existing single-document skill saves, reads, lists and discovers exactly as it does today |
| **C2** | Skill identity (`skill:<name>-v<semver>`), semver comparison and `is_latest` semantics are unchanged |
| **C3** | Frontmatter validation and the secret/poison scanner behave identically for the entrypoint |
| **C4** | Discovery stays metadata-only. No package file appears in a wake context or a skill list |
| **C5** | The `mpm_skills` action enum grows; existing actions keep their current inputs and outputs |
| **C6** | No existing table, row, index or trigger is modified. No data migration of existing rows |
| **C7** | `mpm backup` / `mpm restore-db` round-trips a database that contains packages |
| **C8** | Shredding a skill takes its package with it |
| **C9** | Promotion semantics are unchanged: shared means `is_global=1`, in place |

### 3.4 Extension points

| | extension point |
|---|---|
| **E1** | New tables. `reference_docs` / `reference_chunks` is the precedent: a parent document table, a child table with `ON DELETE CASCADE`, a `content_hash` column for diffing |
| **E2** | New actions on the `mpm_skills` aggregator, plus new CLI subcommands |
| **E3** | `metadata` JSON on the skill row as the pointer to the package — the same idiom as `derived_from_skill_id` |
| **E4** | `isSensitiveContent` and `isPoisonedAt` are reusable per file, not just per document |
| **E5** | `SaveSkillWithInvocation` is the canonical write path. A package import should route its projection through it rather than reimplementing the write |

---

## 4. Storage architecture

Three approaches were compared against the criteria in the brief.

### 4.1 Option A — one `memories` row per associated file

Each file in the package becomes a `memories` row with `collection='skill_files'`
and a pointer to its parent in `metadata`.

*Referential integrity* — poor. `memories` has no foreign key to a parent, so
deleting a skill orphans its file rows; the cascade would have to be written by
hand, exactly as `PermanentlyShredSkill` already hand-cleans
`topic_memberships` and `confidence_history` today.

*Atomic import* — achievable in one transaction, but every file becomes a
full-weight memory row: 37 columns of mostly-irrelevant data, provenance
rows, retrieval metadata, weight and decay semantics.

*Content immutability* — convention only.

*Deduplication* — `memories.content_hash` plus the `identity_hash` unique
index dedups on content **plus tags plus metadata**, which is not
content-addressing.

*Version ownership* — none. "Which files belong to version X" becomes a
metadata query over rows that also contain other collections.

*Backup and export* — included in the dump, which is good, but a reference
file becomes indistinguishable from a memory.

*Promotion* — `is_global` is a memory concept. Flipping it on a *file* row is
semantically wrong; the files of a promoted skill would appear in the shared
scope as separate memories.

*Performance* — **decisive.** Every reference file becomes an FTS5-indexed
memory row. `mpm skill search`, and every general memory search, would begin
returning reference fragments as if they were memories. That corrupts retrieval
semantics, not just performance.

*Complexity* — low schema cost, high semantic cost.

**Rejected.** The FTS pollution and the absence of referential integrity are
not fixable within the option.

### 4.2 Option B — a manifest plus file contents in SQLite

Three new tables: a manifest row per package version, a file row per file, and
a content-addressed blob table holding the bytes. §9 gives the DDL.

*Referential integrity* — real foreign keys. `skill_package_files.skill_id`
cascades from `skill_packages`, which cascades from `memories`. A blob cannot
be deleted while a file row references it.

*Atomic import* — one SQLite transaction covers the manifest row, the file
rows, the blob rows, the `memories` projection and the `is_latest` flip. It is
all-or-nothing by construction.

*Content immutability* — blob rows are insert-only. A published file's bytes
are never rewritten in place; a change is a new row. (The one exception — an
explicit force-overwrite — is decision **O2**, and is stated plainly in §5.8
rather than glossed.)

*Deduplication* — identical bytes across packages are stored once, in the
blob table, keyed by SHA-256. A shared library of packages does not multiply
storage.

*Version ownership* — the package row owns its file rows; the `memories` skill
row owns the package.

*Crash recovery* — transactional. A crash mid-import rolls back to zero rows.

*Backup and export* — **complete, for the first time.** The bytes are in the
database, so `sqlite3 .dump` carries them. Contrast the blobstore, whose
payload files are not in the backup and whose restored rows point at absent
files.

*Promotion* — flip `is_global` on the package row. The file rows and blobs
are shared by reference. No copy, no divergence, and the existing one-row
one-id contract is preserved.

*Garbage collection* — an explicit, operator-invoked, reference-checked sweep
of blobs with no referencing file row. Deliberately **not** the blobstore's
expiry model.

*Schema migration* — three `CREATE TABLE IF NOT EXISTS` statements. An old
binary ignores the tables; a new binary creates them at boot. Rollback is
`DROP TABLE` on three tables and touches nothing else.

*Performance* — point lookups on primary keys. Package files are deliberately
**not** FTS-indexed, so retrieval cost is unchanged from today.

*Complexity* — three tables, one new Go package, an importer and an exporter.
Moderate and bounded.

**Recommended.**

### 4.3 Option C — a manifest plus content-addressed files on disk

Store file contents at `<workspace>/skills-cas/<sha256>` and keep the manifest
in the database.

*Atomic import* — **not atomic across the database/filesystem boundary.** A
crash between "file written" and "row committed" leaves an orphan file; a
crash between "row committed" and "file renamed" leaves a dangling row. A
correct implementation needs a staging directory and a journal.

*Deduplication* — best in class, by construction.

*Backup and export* — **broken by default.** `mpm backup` is `sqlite3 .dump`.
The files are not in it. A restored database has rows pointing at absent
files — precisely the `ErrBlobMissing` failure the blobstore already
exhibits today.

*Promotion* — requires the content store to be reachable from wherever the
shared database lives. MPM makes no such assumption.

*Garbage collection* — the hard part. A content store must never delete a
referenced object. The blobstore's `GCExpired` has no referential check at
all, so reusing it would delete live skill assets — the exact hazard named in
§3.2. A bespoke reconciling sweeper would have to be written and maintained.

*Complexity* — **highest.** Distributed-systems-grade failure modes in a
single-node tool, for a storage problem that SQLite solves natively.

**Rejected.** The deduplication idea is worth keeping; it is retained inside
SQLite as the blob table's key in Option B.

### 4.4 The recommendation

**Option B**, with Option C's content-addressing kept as the blob table's
primary key, plus one hard rule:

> No byte of a skill package ever enters `internal/blobstore` or the blob
> directory. Skill assets are permanent; the blobstore's default TTL is
> 24 hours and its GC has no referential check.

A regression test (§10, T31) pins this.

---

## 5. The package contract

### 5.1 Identity

A package **is** a version of a skill. The package's primary key is the
skill's own id, `skill:<name>-v<version>`. There is no second identity space.

The consequences are worth stating because they are what makes the design
cheap:

* A package and its skill row have the same lifetime; there is no way to have
  one without the other.
* `ReadSkill(name, version)` resolves exactly as it does today, and then the
  package is one foreign-key hop away.
* Promotion, deletion and shredding need no new identity plumbing at all.

### 5.2 Version handling

**The package declares a version.** If the entrypoint's frontmatter carries
`version:`, that value is used. It must be valid semver and must equal the
effective version.

**The package declares no version.** This is the operator's package (§2.1).
MPM assigns an *effective version* and records the override **separately**,
in the manifest and in the package row — never in the third-party file.

* Default effective version: **`1.0.0`** (decision **O1**).
* The operator may override it: `--version 2.1.0`.
* The record is `version_override: {"declared": null, "effective": "1.0.0",
  "reason": "package_declares_no_version"}`.

`1.0.0` rather than `0.0.0`, because `cmd/mpm/r9_t52_skill_version_test.go:16-18`
explicitly rejects defaulting to `0.0.0` on the grounds that it "obscures
authoring intent". A third-party package with no version is a *released*
artefact from its author's point of view; `1.0.0` is the honest reading.
`0.0.0` would also sort below every genuinely versioned skill in the
semver-max resolution, which would quietly make imported packages
un-discoverable by name.

**The third-party file is never rewritten.** The stored bytes are exactly what
the archive contained.

What *is* derived is the **`memories` projection**: the row that discovery
reads. MPM's frontmatter contract requires a `version`, so the projection
adds one **to its own copy** when the original lacks it. Two hashes are
recorded so the derivation is auditable:

| field | meaning |
|---|---|
| `entrypoint_sha256` | SHA-256 of the original file bytes, as supplied |
| `projection_sha256` | SHA-256 of the bytes written to `memories.content` |

When the package *does* declare a version, the two are equal and no
transformation occurs at all. When it does not, the projection differs by
exactly one inserted frontmatter line, and the difference is disclosed in the
import response.

### 5.3 Entrypoint discovery

**Rule:** the entrypoint is `SKILL.md` at the package root.

*Package root* is derived by stripping a common single top-level directory
prefix. The operator's archive has exactly one top-level directory,
`system-prompt-writer/`, so the root is that directory and the entrypoint is
`system-prompt-writer/SKILL.md` within the archive — `SKILL.md` within the
package.

An archive whose entries do not all share one top-level directory is rejected
as `ambiguous_package_root` (decision **O9**). An operator may name the
entrypoint explicitly with `--entrypoint`, and the choice is recorded in the
manifest.

If no `SKILL.md` exists at the root, the import fails with
`entrypoint_not_found` and lists the candidate Markdown files found, so the
author can point at the right one instead of guessing.

### 5.4 Canonical relative paths

A canonical path is:

* relative to the package root;
* `/`-separated, POSIX style, never a filesystem path;
* free of a leading `/`, of `.` and `..` segments, of empty segments (`//`), of
  backslashes, of a drive-letter prefix, of a trailing `/`, of NUL, and of any
  control character;
* at most 255 bytes long, at most 8 segments deep.

Two paths that differ only by ASCII case **collide** and the import is
rejected. Two paths that differ only by Unicode normalisation form
**collide** and the import is rejected. Paths are normalised to **NFC** on
ingest; the normalised form is what is stored and exported. When the original
name was not already NFC, the original is preserved in the manifest as
`raw_path`.

The consequence is honest and worth stating: for a purely ASCII package —
including the operator's — export and re-import are byte-faithful. For a
package with non-ASCII names, export writes the NFC form, so the round trip is
stable but not byte-identical to a third-party archive that used NFD. §11
records the evidence.

### 5.5 The file manifest

```json
{
  "format": "mpm-skill-package/1",
  "name": "system-prompt-writer",
  "version": "1.0.0",
  "declared_version": null,
  "version_override": {
    "effective": "1.0.0",
    "reason": "package_declares_no_version"
  },
  "entrypoint": "SKILL.md",
  "entrypoint_sha256": "783ece4b3869ae42d2d46f540d01d1da0f61147a8eada519eff15941b46d423d",
  "projection_sha256": "…",
  "files": [
    {
      "path": "SKILL.md",
      "sha256": "783ece4b3869ae42d2d46f540d01d1da0f61147a8eada519eff15941b46d423d",
      "size_bytes": 12976,
      "content_type": "text/markdown; charset=utf-8",
      "role": "entrypoint",
      "executable": false
    },
    {
      "path": "references/principles.md",
      "sha256": "982e290090180f4ac2d22160f6be1bccee302549ae35302783dd8bfa82727447",
      "size_bytes": 11011,
      "content_type": "text/markdown; charset=utf-8",
      "role": "reference",
      "executable": false
    },
    {
      "path": "references/templates.md",
      "sha256": "d5086d2156bfbdb6a6ab5c9d9b7975e0a080317ff2c697db087ef8a83a05dfab",
      "size_bytes": 8176,
      "content_type": "text/markdown; charset=utf-8",
      "role": "reference",
      "executable": false
    }
  ],
  "package_hash": "…"
}
```

* `role` is `entrypoint`, `reference`, `asset` or `script`, derived from the
  path convention (`references/` → reference, `assets/` → asset, `scripts/` →
  script) with the entrypoint special-cased.
* `content_type` comes from a small, explicit allow-list keyed on extension
  plus a content sniff — never a general MIME guesser.
* `package_hash` is the SHA-256 of the **canonical manifest JSON**: object
  keys sorted, no insignificant whitespace, UTF-8, `files` sorted by path. It
  deliberately does **not** depend on the archive container, so
  export → import is stable across ZIP writers, compression levels and
  timestamps.

### 5.6 Import and export fidelity

Export writes a ZIP with the canonical paths, the exact bytes, a deterministic
file order (`SKILL.md` first, then the rest sorted by path) and a fixed
timestamp. Two exports of the same package are byte-identical, and
export → import → export reproduces the same `package_hash`.

### 5.7 Duplicate detection and version replacement

| situation | behaviour |
|---|---|
| Same `(name, effective version)`, same `package_hash` | **Idempotent.** No write. The response reports `already_installed`. |
| Same `(name, effective version)`, different `package_hash` | **Conflict.** Rejected with `version_conflict` unless `--force`. |
| A different version of the same name | A new package. The prior version stays, exactly as today's skills behave. `is_latest` flips inside the same transaction. |
| A file whose SHA-256 already exists in the blob table | Stored once and shared. This is deduplication, not a duplicate-package signal. |
| A different skill name with the same entrypoint hash | Advisory only: the response carries `possible_duplicate_content`. It does not block. |

The idempotence rule mirrors the existing reference-manifest import
behaviour — re-running the same manifest reports `unchanged` and writes
nothing (`internal/core/reference_batch.go:1-31`).

### 5.8 Deletion, shredding and immutability

* **Soft shred** (`mpm skill delete`) stamps `deleted_at` on the skill row. The
  package row and its file rows stay — recoverable, exactly as today's soft
  shred is. A later `save --force` resurrects and replaces both.
* **Hard shred** (`mpm skill shred`) deletes the `memories` row. The foreign
  keys cascade: package row, then file rows. The blob rows become
  unreferenced and are garbage-collection candidates.

On immutability, precisely:

* A file's bytes are **never mutated in place**. Blob rows are insert-only.
* A published `(name, version)` normally always resolves to the same bytes.
* **The exception** is an explicit, audited force-overwrite, which deletes the
  old file rows and inserts the new ones in one transaction. This is the
  direct analogue of today's `SaveSkill(force=true)`, which overwrites
  `memories.content` in place. If the operator wants packaged versions to be
  strictly immutable, that is decision **O2** and the recommended policy is
  to refuse the force-overwrite and require a version bump.

### 5.9 Promotion and copy semantics

Today's promotion is an in-place flip of `is_global` on the skill row, with no
copy and no demote. The package follows the same rule: the same flip on the
package row, with the file rows and blobs **shared by reference**. No copy is
made, so there is no possibility of the copy diverging from the original.

**Cross-workspace sharing is a different problem.** The shared database
(`shared.memories`) is a separate attached file that runs the same schema; the
package tables would have to be added to it too. This design does **not** do
that. Instead the `.skill` archive *is* the portable artefact: an operator
moves a package between workspaces by exporting and importing it, and the
`package_hash` proves the transfer was faithful. That keeps the first cut
free of a shared-schema change. Decision **O4** records the alternative and
its cost.

---

## 6. Compatibility and migration

**No existing row is touched.** The package tables are empty for every skill
that is not a package. Existing single-document skills keep working with no
change at all, and can optionally be *re-imported* as a one-file package
(§10, T02) to gain the package surface.

**Migration mechanics.** Three `CREATE TABLE IF NOT EXISTS` statements, applied
at boot alongside the existing table groups, following the `reference_docs`
precedent. They are additive: an old binary ignores them, a new binary
creates them.

**The restore allow-list is a hard gate.** `mpm restore-db` rejects any dump
naming a table that is not on `CanonicalMPMSchema`
(`internal/core/sql_dump_validator.go:530+`), and `TestCanonicalSchemaSync`
(`internal/core/sql_dump_canonical_schema_test.go:22-52`) requires every table
in the schema sources to appear on that list. The three new tables must be
registered in both places, or **restore fails**. This is a stage-P1
requirement, not a follow-up.

**Rollback** is `DROP TABLE` on three tables. Nothing else in the schema or
the data is affected, so rollback is total and lossless for everything that
existed before. The only loss is the package data itself.

**Backup improves.** Because the bytes live in the database, `mpm backup`
(`sqlite3 .dump`) carries them and `mpm restore-db` restores them. This is
the first MPM store whose payload is genuinely covered by the backup.

### Compatibility matrix

| existing surface | effect |
|---|---|
| `mpm save-skill --file` | unchanged |
| `mpm call mpm_skills` with `save` | unchanged |
| `mpm read-skill` | unchanged |
| `mpm call mpm_skills` with `read` | existing fields unchanged; gains one **optional** `package` object |
| `mpm call mpm_skills` with `list` | unchanged — `SkillSummary` is not modified at all |
| `mpm list-skills` | unchanged |
| Discovery and the wake context | unchanged — no package byte is ever inlined |
| `mpm skill delete` / `mpm skill shred` | unchanged semantics; the package cascades on a hard shred |
| `mpm call mpm_skills` with `promote_to_global` | unchanged semantics; also flips the package row |
| `mpm skill workshop` | unchanged |
| `mpm backup` / `mpm restore-db` | extended to carry packages |
| `mpm export --collection skills` | unchanged; package metadata is not exported (decision **O8**) |

---

## 7. Threat model

**Trust boundary.** An archive or a directory is **untrusted input**. Every
byte that crosses into the database is untrusted until it has been scanned.

**MPM is a data plane.** Importing a skill must never execute any of its
content. There is no code path from package bytes to process execution, and
this design proposes none.

The findings below are not assertions about `archive/zip`; they are measured.
§11 reproduces the probes.

| # | threat | defence |
|---|---|---|
| 1 | **ZIP slip / path traversal** | Every entry name is canonicalised and rejected if it contains a `..` segment, is absolute, or carries a drive-letter prefix. The joined path is then verified to lie inside the root. `archive/zip` performs **no** path sanitisation: `../evil.md` and `/etc/passwd` are read verbatim. |
| 2 | **Absolute paths** | Rejected explicitly. `filepath.IsAbs` catches `/etc/passwd` — but **only on the host platform**; see threat 4. |
| 3 | **Symlinks, FIFOs, devices** | `zip.File.Mode()` exposes the entry's type bits. An entry whose type is neither regular nor directory is rejected as `unsupported_entry_type`. A measured probe shows `L---------` (symlink), `p---------` (FIFO) and `D---------` (device) are all distinguishable. Directory entries are skipped, never stored. |
| 4 | **Windows drive letters and separators** | Rejected by an explicit check, **not** by `filepath.IsAbs`. On Linux `/etc/passwd` is absolute but `C:\Windows\System32\evil.dll` is not — a Linux-only absolute-path test would let it through. Backslashes are rejected in any position. |
| 5 | **Hardlinks** | Not representable in ZIP. On the directory-import path, `os.Lstat` is used, never `os.Stat`, so a symlink is never followed; a hardlinked file is simply a regular file, and MPM stores bytes, not inodes. |
| 6 | **Duplicate paths** | Two entries with the same canonical path are rejected as `duplicate_path`. |
| 7 | **Case-colliding paths** | Two canonical paths differing only by ASCII case are rejected as `path_case_collision`. This is what makes an exported package safe on a case-insensitive filesystem. |
| 8 | **Unicode normalisation** | Paths are normalised to NFC; two names that normalise to the same path are rejected as `path_unicode_collision`. Measured: `café.md` in NFC is 12 bytes and in NFD is 13, they are distinct Go strings, and lowercasing does not unify them — the normalisation has to be done explicitly. Control characters and NUL are rejected. |
| 9 | **ZIP bombs / extreme ratios** | Three independent caps: a per-entry declared-size cap, a **count of actual bytes read** through a limited reader, and a total-uncompressed cap across the package. The byte count is not optional: a measured 4 MiB zero file compresses to 4 195 bytes — a **999.8:1** ratio — and the declared size is an attacker-controlled header, so the only trustworthy number is the one the importer counts. |
| 10 | **Lying size headers** | `archive/zip` catches a declared size *smaller* than the real payload (it reports `zip: not a valid zip file` after reading 0 bytes) and a declared size *larger* than the payload (it reads what is there and then reports `unexpected EOF`). Both are treated as `archive_invalid`, but the byte cap stands independently because the header can be made self-consistent. |
| 11 | **Excessive file count or total size** | Caps: 512 files, 32 MiB total uncompressed, 4 MiB per file, and 256 KiB for the entrypoint. The entrypoint cap matters because `ListSkills` parses frontmatter out of every skill row's full `content` on every call — a multi-megabyte entrypoint would slow every skill listing in the workspace. |
| 12 | **Malformed archives** | Reader errors are surfaced as `archive_invalid`. Every entry is read **to EOF**, which is what makes the CRC check run (§13). |
| 13 | **Corrupt stored bytes** | `archive/zip` **does** verify the CRC — a single flipped content byte yields `zip: checksum error` — **but only when the entry is read to EOF**. A bounded read stops early and skips the check entirely. Therefore: the importer reads every entry to EOF, and the bounded read API (§8) **does not claim** CRC verification. Separately, every stored file's SHA-256 is re-verified on read, and a mismatch is refused rather than returned. |
| 14 | **Secret leakage** | **Every file** is scanned with MPM's existing `isSensitiveContent` (30 patterns) and `isPoisonedAt` (20 default phrases) before insertion. Today the scanner sees only `memories.content` — the entrypoint. A package's reference files would bypass it entirely without this change. This is the most important gap the design closes. Binary files are scanned for the high-signal secret patterns only, since scanning arbitrary binary bytes for the full pattern set produces false positives. |
| 15 | **Prompt injection** | The poison-phrase scanner runs on every file. It is a backstop, not a guarantee: it matches twenty fixed phrases, so an oblique injection would pass. The behavioural control is that package content is returned as **data**, not as instructions — every read carries a provenance envelope naming it as third-party package content with its hash and its scanned status (§8). |
| 16 | **Unsupported executable payloads** | Scripts are stored but flagged `executable: true` in the manifest, and **never executed**. MPM has no code path from package bytes to execution. Executable *binaries* (ELF, PE, Mach-O magic) are rejected by default, with an explicit opt-in that still only stores bytes (decision **O3**). |
| 17 | **Partial imports** | One transaction covers the manifest row, the file rows, the blob rows, the `memories` projection and the `is_latest` flip. Validation runs *before* the transaction opens, so a rejected archive writes nothing at all. A failed import leaves zero rows — verified in §10, T08. |
| 18 | **An import that passes the entrypoint but poisons a reference** | Blocked by threat 14, and the *whole* import rolls back, not just the offending file. |

### 7.1 Scanner coverage by file class

| class | detection | stored | secret scan | poison scan | FTS-indexed | executable |
|---|---|---|---|---|---|---|
| entrypoint Markdown | `SKILL.md` at the root | yes | full | full | yes, via the `memories` row | no |
| reference Markdown | `references/**.md` | yes | full | full | **no** | no |
| other text | `.txt`, `.json`, `.yaml`, `.yml` | yes | full | full | no | no |
| script | `.sh`, `.py`, `.js`, `.rb`, `.pl` | yes, flagged | full | full | no | **never** |
| binary asset | `.png`, `.jpg`, `.svg`, `.pdf`, `.woff2` | opt-in | secret patterns only | no | no | no |
| executable binary | ELF / PE / Mach-O | **rejected** by default | — | — | — | — |

### 7.2 Trust boundaries

* **B1 — archive to MPM.** Untrusted. Everything is canonicalised, bounded and
  scanned before a single row is written.
* **B2 — MPM store to agent.** Semi-trusted. The bytes were scanned at import,
  but the scanner is a backstop against a fixed phrase list, not a proof of
  safety. The read envelope labels provenance so the agent can tell package
  content from MPM-authored content.
* **B3 — agent to execution.** None. MPM does not execute package content.
  Any future "run this skill's script" feature is a separate design with its
  own gate, and is explicitly out of scope here.

---

## 8. The retrieval API

### 8.1 Backward-compatibility rules

* The `read` action with no new parameter returns **today's fields
  unchanged**; it gains one *optional* object (§8.2) that a caller that
  ignores it never sees. The `list` action and `SkillSummary` are **not
  modified at all**.
* The new actions are additive to the `mpm_skills` action enum. The pinned
  `save` / `read` / `list` entries remain, so the existing schema guard
  (`internal/core/tools/schema_guard_test.go:548-575`) still passes.
* Every example below uses the canonical `--payload` invocation form.

### 8.2 The default `read` behaviour, stated explicitly

**`mpm call mpm_skills` with `action=read` and no `path` parameter returns the
body of the derived entrypoint projection — the same string it returns today.**

The *projection* is the stored entrypoint document (§5.2). `read` returns the
part of it after the frontmatter, exactly as it does for any stored skill
today.

* When the package declares a version, the projection is byte-identical to
  the original file, so the returned body is byte-identical to what today's
  `read` would return for that document.
* When the package declares no version, the projection has one frontmatter
  line added, and the response gains an optional `version_override` object
  describing exactly that. No existing field changes.
* **No package file is ever inlined by `read`.** The agent must ask for a
  specific file.

The `read` response gains one optional top-level object when the skill has a
package:

```json
{
  "success": true,
  "id": "skill:system-prompt-writer-v1.0.0",
  "name": "system-prompt-writer",
  "version": "1.0.0",
  "body": "…",
  "is_global": false,
  "package": {
    "entrypoint": "SKILL.md",
    "file_count": 3,
    "package_hash": "…",
    "version_override": {"declared": null, "effective": "1.0.0",
                         "reason": "package_declares_no_version"}
  }
}
```

`list` is deliberately **not** enriched. `SkillSummary` has no JSON tags, so it
serialises with Go field names; adding a field would change the shape of a
structure that the wake context, the proactive-recall hint and `mpm info` all
consume. Discovery stays byte-identical.

### 8.3 New actions

| action | purpose |
|---|---|
| `import` | import a package from a `.skill` archive or a directory |
| `files` | list a package's files — **metadata only, no content** |
| `read_file` | read one file, optionally bounded |
| `export` | write the package back out as a `.skill` archive |
| `verify` | re-hash every stored file and report integrity |

Parameters (all additive; the `params` object is already
`additionalProperties: true`):

| action | parameters |
|---|---|
| `import` | `path` (required), `version`, `name`, `entrypoint`, `force`, `allow_binary` |
| `files` | `skill_id`, or `name` + `version` |
| `read_file` | `skill_id`, or `name` + `version`; `path` (required); `offset`, `limit`, `max_bytes`, `raw` |
| `export` | `skill_id`, or `name` + `version`; `output` |
| `verify` | `skill_id`, or `name` + `version` |

### 8.4 Command line

```bash
mpm skill import <path> [--version V] [--entrypoint P] [--name N] [--force]
                      [--allow-binary] [--json]
mpm skill files  <name|id> [--version V] [--json]
mpm skill read-file <name|id> --path P [--offset N] [--limit N]
                      [--max-bytes N] [--raw] [--json]
mpm skill export <name|id> [--version V] [--output FILE] [--json]
mpm skill verify <name|id> [--version V] [--json]
```

`mpm skill import` accepts three inputs: a `.skill` ZIP archive, a directory
containing a package, or a single Markdown file (which becomes a one-file
package).

### 8.5 Response shapes

`files` — metadata only:

```json
{
  "success": true,
  "package_hash": "…",
  "count": 3,
  "files": [
    {"path": "SKILL.md", "sha256": "783ece4b…", "size_bytes": 12976,
     "content_type": "text/markdown; charset=utf-8", "role": "entrypoint",
     "executable": false}
  ]
}
```

`read_file` — one file, with provenance:

```json
{
  "success": true,
  "path": "references/templates.md",
  "sha256": "d5086d21…",
  "size_bytes": 8176,
  "content": "…",
  "truncated": false,
  "offset": 0,
  "next_offset": null,
  "source": "skill_package",
  "scanned": true
}
```

`import` — the full disclosure:

```json
{
  "success": true,
  "status": "installed",
  "skill_id": "skill:system-prompt-writer-v1.0.0",
  "name": "system-prompt-writer",
  "version": "1.0.0",
  "declared_version": null,
  "version_override": {"effective": "1.0.0", "reason": "package_declares_no_version"},
  "entrypoint": "SKILL.md",
  "entrypoint_sha256": "783ece4b…",
  "projection_sha256": "…",
  "package_hash": "…",
  "file_count": 3,
  "warnings": ["missing_when_to_use"],
  "errors": []
}
```

`status` is one of `installed`, `already_installed` (idempotent re-import) or
`replaced` (a forced overwrite).

### 8.6 Bounded and partial reads

`read_file` accepts `offset` (a line index), `limit` (a line count) and
`max_bytes`. The default byte cap is 64 KiB and the hard cap is 1 MiB. A
truncated response carries `truncated: true` and a `next_offset` the caller can
resume from.

**A bounded read does not verify the CRC** — the standard library only checks
it at end-of-stream (§7, threat 13). The response's `sha256` is the hash of
the **stored** bytes, which was verified at import and can be re-verified with
`verify`; it is not a claim that this particular read was end-to-end.

### 8.7 Progressive retrieval and context economy

| call | bytes loaded |
|---|---|
| discovery / `list` / wake context | **zero** package bytes |
| `read` | the entrypoint projection only |
| `files` | manifest metadata only |
| `read_file` | exactly one file, optionally bounded |
| `export` | everything, but to a file, not to the agent's context |

Nothing in the API returns a whole package into an agent's context by
default. The reference package's own convention (§2.3) — an entrypoint that
names its references — is exactly the progressive pattern this API supports:
read the entrypoint, see `references/templates.md`, fetch that file.

The wake-context catalogue is capped at 20 entries
(`internal/core/wake_skills.go:5-16`) and each entry is a metadata-only
summary. A package therefore costs the wake context exactly what a
single-document skill costs today, no matter how many files it carries.

---

## 9. Proposed schema

Not for merge by this document. Recorded here so the stages in §10 can be
reviewed against it.

```sql
-- Content-addressed storage. One row per distinct byte sequence.
CREATE TABLE IF NOT EXISTS skill_package_blobs (
    sha256      TEXT PRIMARY KEY,
    size_bytes  INTEGER NOT NULL,
    content     BLOB NOT NULL,
    created_at  INTEGER NOT NULL DEFAULT (CAST(strftime('%s','now') AS INTEGER))
);

-- The package manifest. One row per skill version that has a package.
CREATE TABLE IF NOT EXISTS skill_packages (
    skill_id      TEXT PRIMARY KEY,
    name          TEXT NOT NULL,
    version       TEXT NOT NULL,
    format        TEXT NOT NULL DEFAULT 'mpm-skill-package/1',
    entrypoint    TEXT NOT NULL,
    manifest      TEXT NOT NULL,
    package_hash  TEXT NOT NULL,
    source_sha256 TEXT,
    is_global     INTEGER NOT NULL DEFAULT 0,
    created_at    INTEGER NOT NULL DEFAULT (CAST(strftime('%s','now') AS INTEGER)),
    updated_at    INTEGER,
    FOREIGN KEY (skill_id) REFERENCES memories(id) ON DELETE CASCADE
);

-- One row per file in a package.
CREATE TABLE IF NOT EXISTS skill_package_files (
    skill_id      TEXT NOT NULL,
    path          TEXT NOT NULL,
    sha256        TEXT NOT NULL,
    size_bytes    INTEGER NOT NULL,
    content_type  TEXT NOT NULL,
    role          TEXT NOT NULL,
    executable    INTEGER NOT NULL DEFAULT 0,
    raw_path      TEXT,
    PRIMARY KEY (skill_id, path),
    FOREIGN KEY (skill_id) REFERENCES skill_packages(skill_id) ON DELETE CASCADE,
    FOREIGN KEY (sha256) REFERENCES skill_package_blobs(sha256) ON DELETE NO ACTION
);

CREATE INDEX IF NOT EXISTS idx_skill_package_files_sha256
    ON skill_package_files(sha256);
```

Notes on the shape:

* The primary key of `skill_packages` **is** the skill id (§5.1). There is no
  second identity space and no mapping table.
* `ON DELETE NO ACTION` on the blob foreign key means a blob that is still
  referenced by a file row **cannot** be deleted. The garbage collector
  therefore deletes file rows first and unreferenced blobs second.
* `idx_skill_package_files_sha256` is required: a foreign key indexes the
  parent side, but the collector needs the child side to answer "which files
  reference this blob" cheaply.
* The tables are created in dependency order — blobs, packages, files — so no
  foreign key ever points at a table that does not exist yet.
* The package tables are **not** FTS-indexed, deliberately (§12, **O5**).
* `is_global` on the package row mirrors the skill row, so promotion is a
  single consistent flip.

---

## 10. Stages

Each stage is a separate pull request, independently testable, and green on
its own. No stage depends on a later one.

| stage | content | acceptance |
|---|---|---|
| **P1** | The three tables, the DDL, the `CanonicalMPMSchema` registration, and the `TestCanonicalSchemaSync` guard | A fresh database has the tables; an old database is upgraded in place; a dump containing the tables restores; the sync guard is green |
| **P2** | Path canonicalisation, collision detection, NFC normalisation, the manifest type, canonical JSON, `package_hash` — pure functions | Unit tests for every rejection rule in §7, threats 1, 4, 6, 7, 8 |
| **P3** | Directory import: path rules, limits, per-file scanning, the derived projection with the version override, the single-transaction write, idempotence | The operator's package imported from an extracted directory produces the §2.5 hashes and the expected projection; a rejected file leaves nothing behind |
| **P4** | The ZIP reader: entry-type rejection, path rejection, byte caps, ratio caps, CRC verification by full read | The operator's `.skill` imports with the same `package_hash` as the directory import; the malicious fixture set is rejected with the right reason codes |
| **P5** | The read API: `files`, `read_file`, bounded reads, hash verification, the provenance envelope | A reference read returns the exact bytes; a corrupted stored file is refused with `content_hash_mismatch` |
| **P6** | The export writer: deterministic ZIP output, the round trip, export of a legacy single-document skill as a one-file package | `package_hash` is stable across export → import; two exports of one package are byte-identical |
| **P7** | The CLI subcommands, the `mpm_skills` action enum extension, the registry description, the schema-guard pins | The documentation gate is green; the existing `save` / `read` / `list` pins still pass; the new actions are pinned by new guards |
| **P8** | Promotion of packages, the operator-invoked blob collector with its reference check, the blobstore-isolation test | `is_global` flips on both rows; the collector never deletes a referenced blob; `mpm blob gc` leaves the package tables untouched |
| **P9** | The `docs/SPEC.md` skill section, the agent-protocol note that package content is data, and the promotion of this document out of `docs/dev/` | The spec describes the shipped behaviour; the library entry carries a status label |

---

## 11. Evidence

Everything asserted about the reference package and about `archive/zip` was
measured against the operator's archive and against synthetic hostile
archives, using a disposable Go probe outside the repository.

**The real archive.** Three entries, all regular mode `0644`, all deflated,
with the sizes and hashes in §2. No directory entry is present; the shared
`system-prompt-writer/` prefix is implied by the file names.

**Entry names are raw strings.** `../evil.md`, `../../etc/passwd` and
`/etc/passwd` are all read verbatim. `filepath.IsAbs` identifies the absolute
one; the `..` cases need an explicit traversal check; and
`C:\Windows\System32\evil.dll` is **not** absolute on Linux, so a
Linux-only absolute check would miss it.

**Entry types are detectable.** With the correct `fs.FileMode` values, the
reader reports `L---------` for a symlink, `p---------` for a FIFO,
`D---------` for a device and `d---------` for a directory.

**A lying size header is caught, but only at end-of-stream.** A declared size
of 10 bytes against a 200 000-byte payload yields `zip: not a valid zip file`
after reading 0 bytes; a declared size of 1 GiB against a 50 000-byte payload
yields `unexpected EOF` after reading 49 152 bytes. Neither is a substitute
for counting actual bytes.

**The CRC *is* verified — but only on a full read.** A single flipped content
byte in a stored entry produces `zip: checksum error` when the entry is read
to EOF. The same entry read through a 4 KiB limited reader returns 4 096
bytes and **no error**, because the end-of-stream was never reached.

**Compression ratios of ~1000:1 are ordinary.** A 4 MiB zero file compresses
to 4 195 bytes — 999.8:1 — so a ratio cap is a policy choice, not a defence
against an obvious attack.

**Unicode normalisation is not implicit.** `café.md` in NFC is 12 bytes and
in NFD is 13; they are distinct Go strings and `strings.ToLower` does not
unify them.

**MPM's own parser and scanner, run against the real package.** The entrypoint
is rejected with `frontmatter missing required field: version`; the two
reference files are rejected with `frontmatter missing: must start with "---"`,
confirming they are content rather than procedures. All three files pass
`ScanContentForWrite` — no secret pattern, no poison phrase.

**MPM's own scanner applied per file is the control that makes storage safe.**
Today's skill scan covers `memories.content` only. Without a per-file scan, a
package's reference files would reach the database unscanned, and those files
are exactly where an author would put an API key.

---

## 12. Open operator decisions

| | decision | recommendation | cost of the alternative |
|---|---|---|---|
| **O1** | The effective version for a package that declares none | `1.0.0`, overridable with `--version` | `0.0.0` sorts below every versioned skill, which makes imported packages un-discoverable by name. Requiring an explicit `--version` makes the common case a two-step flow. |
| **O2** | May a published package be replaced in place by a force-overwrite? | Allow, with an audit row, and offer an `immutable_packages` policy flag | Refusing outright is simpler and strictly safer, but diverges from today's `save --force` behaviour, which overwrites in place. |
| **O3** | Are binary assets stored by default? | Reject by default; `--allow-binary` opts in | Storing them by default puts unscannable bytes in the database for a collection whose value is textual. |
| **O4** | Cross-workspace sharing: extend the shared database, or use the archive as the transport? | The archive. No shared-schema change in the first cut | Extending the shared database means adding the three tables to the shared schema, a migration on every shared database, and deciding whether the shared copy is a reference or a copy. |
| **O5** | Should package files be searchable through FTS? | No. FTS stays on the entrypoint only | Yes makes `mpm skill search` find a phrase buried in a reference, but every reference file becomes an indexed memory row — the Option A problem (§4.1). |
| **O6** | Are the size caps right — 512 files, 32 MiB total, 4 MiB per file, 256 KiB entrypoint? | Adopt as defaults; make them constants | Larger caps slow `ListSkills`, which parses every skill row's full content. |
| **O7** | Should `read` gain the optional `package` object? | Yes — additive, and it lets an agent discover that references exist without a separate call | Without it, an agent must call `files` speculatively to learn whether a skill is a package. |
| **O8** | Should `mpm export --collection skills` carry package metadata? | Yes, as a follow-up after P6 | Without it, an exported skill row alone is not enough to reconstruct a package. |
| **O9** | Package layouts: a single root directory, or files at the archive's top level? | Accept exactly one — a single root directory, or a flat top level. Multiple roots are rejected | Accepting multiple roots needs a rule for which file is the entrypoint, which is guesswork. |
| **O10** | The operator's package has no `when_to_use`. MPM's validator emits `missing_when_to_use` as a **warning**, so the import succeeds with a warning. Should a package import require it? | Warn, do not block — but the operator should decide, because requiring it would reject the reference package | Requiring it would enforce discovery quality, at the cost of rejecting legitimate third-party packages that use a prose description instead |

---

## 13. Regression matrix

Every row is an executable acceptance test. The stage that delivers it is
named.

| # | scenario | expected | stage |
|---|---|---|---|
| **T01** | An existing single-document skill, unchanged | Save, read and list behave exactly as today; no package row is created | P1 |
| **T02** | Import a legacy single-document skill as a package | One file row; the projection equals the original bytes; `projection_sha256 == entrypoint_sha256` | P3 |
| **T03** | Directory import of the reference package | Three file rows with the §2 hashes; the entrypoint is `SKILL.md` | P3 |
| **T04** | ZIP import of the reference package | Identical result to T03, including `package_hash` | P4 |
| **T05** | A package whose entrypoint declares no version | The effective version is `1.0.0`; `version_override` is recorded; **the stored `SKILL.md` bytes are unchanged**; the projection carries `version: 1.0.0` | P3 |
| **T06** | A relative reference read | `read_file` with `path=references/templates.md` returns the exact bytes; the hash matches; no other file is returned | P5 |
| **T07** | Identical-content round trip | export → import preserves `package_hash` and every per-file hash; two exports are byte-identical | P6 |
| **T08** | Atomic failure — a secret in the third file | Zero package rows, zero blob rows, zero `memories` rows; the database is byte-identical to before | P3 |
| **T09** | Backup and restore | A restored database has the same package bytes, hashes, manifest and `memories` projection | P1 |
| **T10** | Cross-agent sharing — promotion | `is_global=1` on both the skill row and the package row; the file rows are shared by reference, not copied; `ListSkills` with the shared scope shows the skill | P8 |
| **T11** | Cross-workspace sharing | Export from one workspace, import into another; the `package_hash` matches | P6 |
| **T12** | Version upgrade | v1.0.0 and v1.1.0 both exist; `is_latest` moves to v1.1.0; the v1.0.0 files are intact | P3 |
| **T13** | Malicious: ZIP slip | An entry named `../../etc/passwd` is rejected with `path_traversal`; nothing is written | P4 |
| **T14** | Malicious: absolute path | An entry named `/etc/passwd` is rejected with `absolute_path` | P4 |
| **T15** | Malicious: Windows path | An entry named `C:\Windows\x.dll` is rejected — a Linux-only absolute check would miss this, so the test is specific | P4 |
| **T16** | Malicious: symlink entry | An entry carrying the symlink mode bit is rejected with `unsupported_entry_type` | P4 |
| **T17** | Malicious: duplicate path | Two entries with the same name are rejected with `duplicate_path` | P4 |
| **T18** | Malicious: case collision | `Ref.md` and `ref.md` are rejected with `path_case_collision` | P4 |
| **T19** | Malicious: Unicode collision | NFC and NFD forms of one name are rejected with `path_unicode_collision` | P4 |
| **T20** | Malicious: ZIP bomb | A 64 MiB zero entry is rejected with `size_limit`; the process's memory stays bounded | P4 |
| **T21** | Malicious: lying header | A declared size of 10 against a 200 000-byte payload is rejected with `archive_invalid` | P4 |
| **T22** | Malicious: CRC corruption | A flipped content byte is rejected with `checksum_mismatch` — which requires the importer to read to EOF | P4 |
| **T23** | Malicious: a secret in a reference file | An API key in `references/x.md` is rejected with `scanner_secret`, and the whole import rolls back | P3 |
| **T24** | Malicious: a poison phrase in a reference file | Rejected with `scanner_poison` | P3 |
| **T25** | Malicious: an executable payload | A script with the executable bit is stored, flagged `executable: true`, and **never executed**; no code path connects package content to execution | P3 |
| **T26** | Malformed archive | A truncated or non-ZIP file is rejected with `archive_invalid`; no partial state | P4 |
| **T27** | Idempotence | Importing the same package twice reports `already_installed` the second time and creates no new rows | P3 |
| **T28** | A different version of the same name | Both exist; the latest flips | P3 |
| **T29** | The same version with different content, without `--force` | Rejected with `version_conflict` | P3 |
| **T30** | Shredding | Soft shred leaves the package recoverable; hard shred removes the package and file rows, and the blob becomes a collection candidate | P8 |
| **T31** | Blobstore isolation | `mpm blob gc` and `mpm ops gc` leave the package tables and the blob rows untouched | P8 |
| **T32** | Discovery weight | With 200 skills each carrying a package, `list` and the wake context contain no package byte, and the `available_skills` shedding behaviour is unchanged | P7 |
| **T33** | Bounded read | `read_file` with a byte cap returns no more than the cap, sets `truncated: true`, and returns a `next_offset` | P5 |
| **T34** | A corrupted stored file | Mutating a blob row's bytes makes `read_file` refuse with `content_hash_mismatch` | P5 |

### 13.1 Fixture strategy

* **Golden packages.** A `testdata` tree holding the operator's package
  extracted, byte-exact, with a recorded SHA-256 so the test asserts the
  fixture really is the reference package rather than a lookalike.
* **Synthetic hostile archives.** One small archive per threat, generated by a
  Go generator rather than committed as opaque binaries. The generator
  documents each archive's intent next to the code that builds it, which is
  what makes the fixture readable six months later.
* **The one binary fixture.** The operator's `.skill` archive itself, kept
  byte-exact with a recorded hash, because its exact bytes are the
  compatibility contract.
* **Data-driven expectations.** Each fixture carries a manifest describing the
  expected outcome — the reason code, and whether anything at all was written
  — so the table above is data rather than fifty hand-written assertions.
* **Isolation.** Every test uses `t.TempDir()` databases, a real SQLite file,
  a real ZIP on disk, no network, and no live MPM state.

---

## 14. Summary of the proposal

| | |
|---|---|
| **Architecture** | Three new additive SQLite tables. No existing table, row, index or trigger is modified; rollback is dropping three tables. |
| **Persistence** | A manifest per package version, one row per file, and content-addressed blob rows for the bytes. Referential integrity by foreign key, atomic import by transaction, deduplication by content hash. |
| **Hard rule** | No package byte ever enters the blobstore. Its 24-hour TTL and its referential-check-free GC would delete permanent skill assets silently. |
| **Compatibility** | Existing single-document skills are untouched. The `read` action keeps its envelope and gains one optional field; `list` and `SkillSummary` are not modified; discovery carries no package byte. |
| **Third-party integrity** | The operator's `SKILL.md` is stored byte-exact. The `memories` projection is a separate, labelled derivation that adds a `version` line when the original lacks one, with both hashes recorded. |
| **Security** | Every file is path-canonicalised, bounded, type-checked and scanned — per file, which is the control that today covers only the entrypoint. Importing never executes anything. |
| **Retrieval** | Progressive: discovery reads nothing, `read` reads the entrypoint, `files` reads metadata, `read_file` reads one bounded file. |
| **Open** | Ten operator decisions in §12, with a recommendation and a cost for each. |
