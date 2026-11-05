# MPM Skill Packages — a multi-file skill format

> **Status: Proposed — at the third independent review gate.**
>
> This document is a design proposal. It contains no implementation, no
> schema migration, and no behaviour change. Nothing in the product changes
> until the decision in §1 is taken and the stages in §10 are merged
> separately.
>
> **Base commit:** `f8d4b95aefe60a0880409a8905ce2c4d92d22c00` (`main`).
> **Scope:** the skill collection only.

---

**Revision 3 — remediation of the second independent review.** This revision
incorporates the five corrections (C-1–C-5) required by the second independent
review of revision 2, and **preserves the twelve R-corrections and the
accepted foundation unchanged**.

**The architecture is unchanged.** Option B — a manifest, file rows and
content-addressed blobs in three additive SQLite tables — is retained, as are
the blobstore hard rule, the per-file scanning model, the compatibility
requirements in §3.3, the evidence base in §11 and every R1–R12 resolution in
§15. All five corrections are document-local: three are cases where the
document's own acceptance rows contradicted its own normative text and would
have failed on the day they were written (C-1, C-2, C-4), one corrects a
statement about Go that was measured to be wrong (C-3), and one specifies
where three DDL statements go (C-5). **Two make the design stronger than it
was**: the root derivation is now hash-neutral across the two accepted
archive layouts (C-1), and the FAT-creator case is safer than the document
claimed (C-3).

**One correction produces an operator decision rather than a settled row.**
C-2 is a compatibility change for **every** existing skill, so this revision
states the recommendation and the cost and records the choice as open
(**O17**, §12.2). It does **not** record an approval the operator has not
given.

**§16 is the C-1–C-5 disposition matrix** — for each correction: the original
problem, the revised specification section, the resolution, the supporting
evidence, and the remaining risk. **§15 remains the R1–R12 disposition
matrix**, and **§12 separates the decisions this revision resolves** from the
choices that still require the operator's answer.

---

**Revision 2 — remediation of the first independent review.** This revision
incorporates the twelve corrections (R1–R12) required by the independent review
of revision 1.

**The architecture is unchanged.** Option B — a manifest, file rows and
content-addressed blobs in three additive SQLite tables — is retained, as are
the blobstore hard rule, the per-file scanning model, the compatibility
requirements in §3.3, and the evidence base in §11. Every correction is a
localised change to the *contract*, not a redesign: three corrections (R1, R2,
R7) change what an existing write path must refuse to do, five (R3, R4, R8,
R9, R11) correct statements the document made about itself, and four (R5, R6,
R10, R12) correct the threat model.

**§15 is the R1–R12 disposition matrix** — for each correction: the original
problem, the revised specification section, the resolution, the supporting
evidence, and the remaining risk. **§12 separates the decisions this revision
resolves** from the choices that still require the operator's answer.

---

## 1. The decision requested

MPM skills are single Markdown documents. Real skills are packages: an
entrypoint plus supporting references, scripts and assets, distributed as a
ZIP archive. This document proposes a portable, versioned, multi-file skill
format and the persistence, security and retrieval contracts that go with it.

**The recommendation is:** keep the existing single-document skill exactly as
it is, and add a *package* layer beside it rather than replacing it. A package
is a manifest plus immutable file contents, stored in three new SQLite tables
that are **additive** — no existing table, row, index or trigger is modified.
The two rollbacks are different operations and are named separately in §6.1: a
**binary rollback** (lossless, blind) and a **schema rollback** (destructive,
and the only one that destroys package bytes).

**What is being asked for:** approval of the design in §4–§10, and answers to
the operator decisions still open in §12.2. The decisions this revision
resolves — the effective version of a versionless package, the immutability of
a published package, the scope of the agent-facing surface, the behaviour of a
missing scanner, the **invariant** that a prune must never destroy package
bytes, and the shape of the export contract — are settled in §12.1 with their
rationale and evidence, and are not re-opened here.

**What is deliberately not decided here:** the cross-workspace shared-database
extension (decision **O4**, §12.2), binary-asset defaults (**O3**, §12.2), the
size-cap constants (**O6**, §12.2), the accepted archive layouts (**O9**,
§12.2), and **the form of the prune exemption** (**O17**, §12.2). Each has a
recommendation and a cost. **O17 is the one this revision is most careful
about:** the blanket exemption it recommends is a behaviour change for every
existing skill — after it lands, `mpm prune --older-than` removes no skill at
all — so the operator's confirmation is recorded as outstanding rather than
assumed.

**One correction is not a design choice.** The first revision asserted, in
§5.8, that a later `save --force` "resurrects and replaces" a packaged
skill's projection. The repository does not do that, and cannot: the legacy
write path replaces `memories.content` in place and knows nothing about
package rows. Revision 2 makes the legacy path **fail closed** against a
packaged skill rather than assert a behaviour the substrate does not have
(**R1**). The same applies to the atomicity claim (**R2**) and to the
restore-allow-list claim (**R8**).

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

The same holds for the *effective* version this design assigns to a
versionless package (§5.2), which is a pre-release identifier rather than a
plain release. Measured against MPM's own helpers:

```
SkillIDForNameAndVersion("system-prompt-writer", "1.0.0-unversioned")
  → "skill:system-prompt-writer-v1.0.0-unversioned"
ParseNameAndVersionFromID("skill:system-prompt-writer-v1.0.0-unversioned")
  → name="system-prompt-writer" version="1.0.0-unversioned"   (round-trips)
```

The round trip works because the id format splits on the **first** `-v`, and a
skill name may not contain `-v` (§3.1, `internal/core/skill.go:194-200`). The
pre-release suffix therefore does not disturb identity parsing. The ordering
that makes it usable is measured in §5.2.

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
| The skill write path is **not** composable into a caller's transaction: the new-row path owns its own `db.Begin()`/`tx.Commit()`, and the force path is a bare `db.Exec` with no transaction | `internal/core/skill_db.go:652`, `735`, `624` |
| The transaction-aware write primitive that *is* composable is `saveMemoryRow`, reached through the `txNode` adapter — this is the substrate's own tx-aware insert | `internal/core/db.go:4472`, `internal/core/skill_db.go:730` |
| The force path replaces `memories.content` **in place** under the same id, and knows nothing about package rows | `internal/core/skill_db.go:624-631` |
| The MCP `save` handler passes `force` straight from the payload to that force path | `internal/core/tools/handlers.go:3758` |
| The MCP `read` handler returns `skill.Body` inlined, with no provenance envelope | `internal/core/tools/handlers.go:3794-3818` |
| `mpm prune --older-than` is a **collection-blind hard delete** — no `collection` filter, only directives are excluded | `internal/core/web_db.go:1781-1792` |
| The poison-phrase scanner reads an operator-editable **file** and **fails open** if that file cannot be read; the secret scanner is in-process and cannot | `internal/core/memory.go:845-851` |
| There is **no** schema-version gate anywhere in the tree — `PRAGMA user_version` appears in no file — so an old binary opens a newer database without complaint | (absent from the tree) |
| The MCP registry's **only** path-shaped parameter in the whole tool surface is `filepath` on `mpm_references`; `mpm_skills` has none, and there is no path-confinement helper in the tool layer | `internal/core/tools/registry_list.go:217` |

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
* **`mpm prune --older-than` is a collection-blind hard delete.**
  `DELETE FROM memories WHERE created_at < ? AND deleted_at IS NULL AND
  <DirectiveExclusionClause>` — there is **no `collection` filter**; only
  directives are excluded (`internal/core/web_db.go:1781-1792`, reached from
  `cmd/mpm/maint_cmds.go:139-141`). This is an *explicit operator command*, not
  an automatic expiry, and it removes skill rows of any age. **Package bytes
  have no second copy**: they came from a third-party archive and exist
  nowhere else, and the foreign-key cascade in §9 takes the package row and
  the file rows with the skill row. The bytes are then gone.
  (`PruneNeverAccessed` is safe — it requires `is_long_term = 0`, and skills are
  written `is_long_term = 1`. The default `mpm prune` is `PruneExpired`, which
  skills do not match because `expires_at` is NULL.)
* **The read paths disagree about expiry.** `MemoryExpireClause` is applied by
  export and search but **not** by `ReadSkill` or `ListSkills`
  (`internal/core/web_db.go:42` versus `internal/core/skill_db.go:268`, `370`).
  An expired skill would vanish from `mpm export` while remaining readable.

**Conclusion, carried into §4:** no byte of a skill package may ever enter
`internal/blobstore` or the blob directory. Permanent skill assets and a
24-hour TTL are not compatible, and the failure is silent.

**A second conclusion, carried into §5.8 and §6:** a package's bytes are
unrecoverable once the owning skill row is deleted, because there is no second
copy and no upstream archive MPM can re-fetch. Any deletion path that is
collection-blind must therefore be taught about packages before packages exist
(§6, §12.1 **O13**, §12.2 **O17**).

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
| **C10** | **A legacy write can never silently diverge from a package.** A `save --force` (CLI or MCP) against a skill that owns a package is **refused** with a distinct status; it does not replace the projection (**R1**) |
| **C11** | **A package import is one transaction.** The manifest row, the file rows, the blob rows, the `memories` projection and the `is_latest` flip commit together or not at all (**R2**) |
| **C12** | **A package import cannot report success without a complete scan.** If a required scanner cannot run, the import is refused (**R10**) |
| **C13** | **The agent-facing surface cannot create packages.** Import is operator-driven; the agent-facing package surface is read-only (**R4**) |
| **C14** | **An export never silently drops package content.** A skills export that cannot carry package bytes says so, in the response and in the CLI output (**R11**) |
| **C15** | **A packaged skill is not silently destroyed by an age-based prune** (**R7**) |

C10–C15 are the acceptance criteria for the corrections themselves. They are
not new features: each one is a *refusal* or a *disclosure* that the
corresponding correction adds to an existing path.

### 3.4 Extension points

| | extension point |
|---|---|
| **E1** | New tables. `reference_docs` / `reference_chunks` is the precedent: a parent document table, a child table with `ON DELETE CASCADE`, a `content_hash` column for diffing |
| **E2** | New actions on the `mpm_skills` aggregator, plus new CLI subcommands |
| **E3** | `metadata` JSON on the skill row as the pointer to the package — the same idiom as `derived_from_skill_id` |
| **E4** | `isSensitiveContent` and `isPoisonedAt` are reusable per file, not just per document |
| **E5** | **`saveMemoryRow`, reached through the `txNode` adapter, is the canonical tx-aware write primitive for the projection** — the definition is `internal/core/db.go:4472` and the skills call site is `internal/core/skill_db.go:730`. The import owns the transaction and writes the projection through it. `SaveSkillWithInvocation` is **not** the primitive: its new-row path opens and commits its own transaction (`:652`, `:735`) and its force path is a bare `db.Exec` with no transaction at all (`:624`), so it cannot be composed into a caller's transaction. A thin tx-aware skill writer may be extracted from it, but the transaction must belong to the importer (**R2**) |
| **E6** | The force path is the one in-place content writer for a skills row in the whole tree (`internal/core/skill_db.go:625`), so the package-immutability guard is a single branch in a single function, plus its CLI and MCP surfaces (**R1**) |
| **E7** | `DirectiveExclusionClause` is the precedent for a collection-scoped exemption in a hard delete; the skills exemption follows the same shape (**R7**) |

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
all-or-nothing by construction. **The importer owns that transaction** and
writes the projection through the tx-aware primitive in §3.4 E5, because
`SaveSkillWithInvocation` cannot be composed into it (**R2**).

*Content immutability* — blob rows are insert-only. A published file's bytes
are never rewritten in place; a change is a new row. A published package is
**never** replaced in place at all: the legacy force path is refused against
it (§5.8, **R1**).

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
binary ignores the tables; a new binary creates them at boot.

*Rollback* — **two different operations, with opposite characteristics**, and
the document uses one word for both. They are named separately here and
throughout (**R9**):

| operation | package bytes | everything else |
|---|---|---|
| **Binary rollback (lossless, blind)** — an old binary runs against a database that has the package tables | **retained but invisible**: the rows are still in the file, and they reappear when the new binary returns | untouched |
| **Schema rollback (destructive)** — `DROP TABLE` on the three tables | **irreversibly destroyed**, including third-party files that exist nowhere else | untouched |

Binary rollback is *blind* in a specific sense: the old binary has no schema
gate to warn it, so it opens the newer database without complaint (§3.1) and
simply does not know the tables are there. It is **not** free of
consequences: an old binary's own backup contains the package tables, and the
old binary's `restore-db` allow-list does not accept them — see §6 (**R8**).

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
primary key, plus two hard rules:

> **Rule 1 — the blobstore.** No byte of a skill package ever enters
> `internal/blobstore` or the blob directory. Skill assets are permanent; the
> blobstore's default TTL is 24 hours and its GC has no referential check.

> **Rule 2 — the filesystem.** No byte of a skill package is ever
> **materialised as an intermediate file**. An importer reads bytes into memory
> and writes them into a database column — never a staging file, never a
> temporary file, never a file in the workspace. The **only** place package
> bytes reach the filesystem is the `.skill` archive an operator explicitly
> asks for with `mpm skill export`, and that is the *artefact*, not an
> intermediate.

Rule 2 is the real defence for **both** import paths, and it is what makes the
ZIP entry-type check's limitation harmless rather than surprising. A symlink
entry in an archive is a string; a directory entry is a string; a device entry
is a string. There is no path by which an archive entry can become a
filesystem object, so the attack the entry-type check was written to stop
cannot occur. The entry-type check remains as a **secondary, conditional**
guard (§7, threat 3) — not as the control.

Rule 1 is pinned by a regression test (§10, P9; §13 T31). Rule 2 is pinned by
the same stage's assertion that an **import** writes nothing to the
filesystem.

**One consequence, stated so it is not a surprise later.** Because Rule 2
holds, the ZIP entry-type check is *redundant for safety* on the import path
— it can only cause a refusal, never enable an escape. That is the correct
direction: a false refusal costs the operator one archive, a false acceptance
would have cost a filesystem. The export path is not covered by Rule 2 — it
writes a real file — so the export writer is the one place where the archive's
entry types are read *again* on the way out, and it writes the canonical paths
and the stored bytes, not the archive's original entry names or modes.

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

* Default effective version: **`1.0.0-unversioned`** (§12.1, **O1**).
* The operator may override it: `--version 2.1.0`.
* The record is `version_override: {"declared": null, "effective":
  "1.0.0-unversioned", "reason": "package_declares_no_version"}`.

**Why a pre-release identifier and not `1.0.0`.** The first revision used
`1.0.0`, and that is wrong in a way that only shows up later. If the
effective version is `1.0.0`, then when the package's author later ships a
**genuine** `1.0.0` — now with a `version:` field — the import collides with
the versionless import: same `(name, version)`, different `package_hash`, so
§5.7 rejects it as `version_conflict`. Semver has no way to say "this `1.0.0`
is newer than that `1.0.0`", and the author cannot publish `1.0.1` without
lying about the version. The **only** way to install the genuine release would
be to force-overwrite a published id in place — precisely the mutation §5.8
refuses — leaving one id carrying two `package_hash` values over its lifetime
(**R3**).

`1.0.0-unversioned` is a semver **pre-release**, and pre-releases sort
*below* the corresponding release. Measured against the repository's own
`golang.org/x/mod/semver`:

```
semver.IsValid("v1.0.0-unversioned")            = true
semver.Compare("v1.0.0-unversioned", "v1.0.0")  = -1   ← a later genuine 1.0.0 WINS
semver.Compare("v1.0.0-unversioned", "v0.9.0")  = +1   ← still above every older release
semver.Compare("v1.0.0-unversioned", "v1.0.0-rc1") = +1
```

So the effective version is **installable alongside** a later genuine `1.0.0`
(the genuine release becomes `is_latest`, and both packages coexist), it
remains discoverable by name among genuinely versioned skills, and it cannot
collide with an author-declared `1.0.0` because the pre-release suffix is
distinguishable. The id round-trips (§2.5).

**The first revision's justification was mis-cited and is withdrawn.** It
claimed `1.0.0` was chosen because `cmd/mpm/r9_t52_skill_version_test.go:16-18`
"explicitly rejects" defaulting to `0.0.0`. Lines 16–18 of that file are a
**rationale note in a test header**, not a constraint:

```
// We do NOT default-version to "0.0.0" / content-hash: that would
// obscure authoring intent and break downstream tooling that parses
// the id suffix.
```

Nothing in the substrate forbids `0.0.0`: `validateSkillNameAndVersion`
(`internal/core/skill.go:181-203`) rejects only an empty version, a colon,
whitespace, and `-v` in the *name*, and `semver.IsValid("v0.0.0")` is `true`.
The note's actual objection — that `0.0.0` "sorts below every genuinely
versioned skill" — is answered by the pre-release form, which sorts above
every older genuine release and below the genuine `1.0.0`. `0.0.0` is dropped
from consideration.

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

**The projection hash is a live invariant, not a record.** Because the legacy
write path can replace `memories.content` in place (§3.1), the two hashes
could diverge — and `projection_sha256` would become a false statement. That
is exactly why the legacy force path is refused against a packaged skill
(§5.8, **R1**): the refusal is what keeps this table true.


### 5.3 Entrypoint discovery

**Rule:** the entrypoint is `SKILL.md` at the package root.

The **package root** is the directory that the canonical paths are relative to
(§5.4). There are exactly **two** accepted archive layouts, and the root is
derived from the archive's own top-level shape:

| | the **flat** layout | the **wrapped** layout |
|---|---|---|
| shape | `SKILL.md`, `references/…` at the archive's top level | everything under one top-level directory: `pkg/SKILL.md`, `pkg/references/…` |
| package root | the **archive root** | the **single top-level directory** |
| what is stripped | **nothing** | that one directory segment, from every path |
| produced by | **MPM's own export writer** (§5.6) | a third-party archive, or the operator's own `.skill` |

The operator's archive is in the wrapped form — `system-prompt-writer/` holds
`SKILL.md` and the two reference files — so the root is that directory and the
entrypoint is `system-prompt-writer/SKILL.md` within the archive, `SKILL.md`
within the package. **MPM's own export is in the flat form** (§5.6): it
writes the canonical paths, so `SKILL.md` and `references/…` sit at the
archive's top level. Both are accepted, and the derivation below is what
makes the export round trip work by construction rather than by accident.

#### 5.3.1 The root-normalisation rules, exactly

The derivation runs over the archive's **file** entries, and it runs in this
order:

1. **Detect and skip directory entries — first.** An entry that is a directory
   — its name ends with `/`, or `Mode()` reports a directory — is **not a
   file**: it is skipped, not stored, and it **does not participate** in the
   derivation (§7.1). Any bytes a writer happened to attach to a directory
   entry are discarded with it. This step comes **before** canonicalisation,
   and it must: a directory entry's name is *not* a canonical path, because it
   carries a trailing `/` and §5.4 forbids that in a file path. Canonicalising
   a directory entry would reject it as non-canonical — which is the exact
   misreading §7.1 corrects, and it would reject MPM's own archives.
2. **Canonicalise the remaining file entries.** Every file entry's name is
   canonicalised under §5.4's rules. An entry that fails is rejected before
   the root is derived, so the derivation never sees an unsafe path.
3. **Classify the top level.** Over the file entries, a first path segment is
   a **directory prefix** if some file entry has two or more segments and
   begins with it; every other first segment is a **top-level file**.
4. **Derive the root** from that classification:

   | top-level shape | package root | canonical paths |
   |---|---|---|
   | no directory prefix — every file entry has exactly one segment | the archive root | the entry names, unchanged |
   | exactly **one** directory prefix and **no** top-level file | that directory | the first segment is stripped from every path |
   | exactly **one** directory prefix **and** at least one top-level file | the archive root | the entry names, unchanged — **this is MPM's own export layout** |
   | **two or more** directory prefixes | — | rejected as **`ambiguous_package_root`** |

5. **Stripping is by whole segment.** The root segment is removed only when it
   is *equal* to a path's first segment. A path whose first segment merely
   shares a prefix with the root — `pkg-extra/x.md` against a root of `pkg` —
   is **not** stripped.
6. **The derivation is order-independent** and uses the complete entry set, so
   two archives carrying the same entries in a different order derive the same
   root and the same canonical paths.
7. **The root is not stored.** It is not a file row, and it is not part of any
   canonical path. It survives only in `raw_path` (§5.4), so the original
   archive layout stays auditable.

**The derivation is hash-neutral, and that is the property the round trip
rests on.** The same package imported from a flat archive and from a wrapped
archive yields the **same canonical paths, the same file rows and the same
`package_hash`**; only `raw_path` differs. So `export → import → export`
reproduces the same `package_hash` whichever layout the import arrived in, and
a third-party wrapped archive and MPM's own flat export are the same package
to MPM. The operator's `system-prompt-writer/` archive and the export MPM
writes from it have the same canonical paths — which is why T03, T04 and T07
can all assert the same hash.

**Multiple roots are rejected.** An archive with two or more top-level
directories is refused with `ambiguous_package_root` (decision **O9**,
§12.2): no rule says which of them is the package, and guessing would import
the wrong tree. The refusal is about the *root*, not about the entrypoint —
an archive with two `SKILL.md` files is refused for the same reason.

**The operator may name the root explicitly.** `--entrypoint <path>` names the
entrypoint; the package root becomes that path's **parent directory** (the
archive root if `<path>` has no directory component), and every other file
entry must lie under it — an entry outside it is rejected with
`path_outside_root`. The choice is recorded in the manifest. This is the
escape hatch for an archive that is neither flat nor wrapped, and it cannot be
used to smuggle in a second tree: an entry outside the named root is still
refused.

**If no `SKILL.md` exists at the root**, the import fails with
`entrypoint_not_found` and lists the candidate Markdown files found, so the
author can point at the right one instead of guessing. An archive with no file
entries at all fails the same way.

**The directory import path has no ambiguity to resolve.** The operator names
the root when they name the directory, so §5.3.1 governs the ZIP path only;
the two paths agree on the canonical paths, which is what T03 and T04 assert.

### 5.4 Canonical relative paths

A canonical path is:

* relative to the package root — the archive root in the flat layout, the
  single wrapped directory otherwise (§5.3);
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

**The canonical path is the identity; `raw_path` is a display artefact.**
§9's `PRIMARY KEY (skill_id, path)` stores the **canonical** path — the
NFC-normalised, case-collision-checked, POSIX-separated form defined above.
Two package files are the same file exactly when their canonical paths are
equal, and nothing else. `raw_path` records the name as it appeared in the
source archive, and it is **display-only**: it is never used to address a
file, never compared for identity, and never written to by an export. The
practical consequence: a package imported from an NFD-named archive and the
same package re-imported from an NFC-named archive produce the **same** rows
and the same `package_hash`, and the display-only `raw_path` differs between
them.

### 5.5 The file manifest

```json
{
  "format": "mpm-skill-package/1",
  "name": "system-prompt-writer",
  "version": "1.0.0-unversioned",
  "declared_version": null,
  "version_override": {
    "effective": "1.0.0-unversioned",
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
      "executable": false,
      "scanned": {"secret": true, "poison": true}
    },
    {
      "path": "references/principles.md",
      "sha256": "982e290090180f4ac2d22160f6be1bccee302549ae35302783dd8bfa82727447",
      "size_bytes": 11011,
      "content_type": "text/markdown; charset=utf-8",
      "role": "reference",
      "executable": false,
      "scanned": {"secret": true, "poison": true}
    },
    {
      "path": "references/templates.md",
      "sha256": "d5086d2156bfbdb6a6ab5c9d9b7975e0a080317ff2c697db087ef8a83a05dfab",
      "size_bytes": 8176,
      "content_type": "text/markdown; charset=utf-8",
      "role": "reference",
      "executable": false,
      "scanned": {"secret": true, "poison": true}
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
* **`scanned` records what actually ran, per file.** It is not a constant
  `true`: it is set from the outcome of the scan, and it is `false` for a
  scanner that did not run. For a binary asset the poison scan does not apply
  (§7.1), so the entry is `{"secret": true, "poison": false}` — a **disclosed
  skip**, not a claim of safety. A scan that could not run at all is an
  import **refusal**, not a `false` entry: nothing is written, so no row can
  ever record a scan that did not happen (**R10**, §7 threat 14).
* `package_hash` is the SHA-256 of the **canonical manifest JSON**: object
  keys sorted, no insignificant whitespace, UTF-8, `files` sorted by path. It
  deliberately does **not** depend on the archive container, so
  export → import is stable across ZIP writers, compression levels and
  timestamps. The `scanned` block is part of the manifest and therefore part
  of the hash: two packages with identical bytes but different scan outcomes
  are not the same package.

### 5.6 Import and export fidelity

Export writes a ZIP with the canonical paths, the exact bytes, a deterministic
file order (`SKILL.md` first, then the rest sorted by path) and a fixed
timestamp. Two exports of the same package are byte-identical, and
export → import → export reproduces the same `package_hash`.

**The export's archive is in the flat layout.** The canonical paths sit at the
archive's top level, so the archive has one directory prefix (`references`)
and one top-level file (`SKILL.md`) — §5.3.1's third row. The importer
derives the archive root as the package root and strips nothing, so the
archive this writer produces is **accepted by construction** rather than by
coincidence. That is the property the round trip rests on, and it is why the
export writer and the importer's root rule must be specified against each
other rather than independently.

### 5.7 Duplicate detection and version replacement

| situation | behaviour |
|---|---|
| Same `(name, effective version)`, same `package_hash` | **Idempotent.** No write. The response reports `already_installed`. If the skill was soft-shredded, the re-import **resurrects** it — the import path is not the legacy force path (§5.8.1), so R1's refusal does not apply to it. |
| Same `(name, effective version)`, different `package_hash` | **Conflict.** Rejected with `version_conflict`. There is no `--force` escape: a published package is immutable (§5.8, **R1**). |
| A **versionless** package and a later **genuine** `1.0.0` of the same name | **Two packages, not a conflict.** The versionless one has effective version `1.0.0-unversioned` and the genuine one has `1.0.0`; they are different `(name, version)` pairs, so both install, and the genuine release becomes `is_latest` (§5.2, **R3**). |
| A different version of the same name | A new package. The prior version stays, exactly as today's skills behave. `is_latest` flips inside the same transaction. |
| A file whose SHA-256 already exists in the blob table | Stored once and shared. This is deduplication, not a duplicate-package signal. |
| A different skill name with the same entrypoint hash | Advisory only: the response carries `possible_duplicate_content`. It does not block. |

The idempotence rule mirrors the existing reference-manifest import
behaviour — re-running the same manifest reports `unchanged` and writes
nothing (`internal/core/reference_batch.go:1-31`).

**Why the conflict row has no `--force`.** The first revision allowed a
forced overwrite of a published package, by analogy with `save --force`. That
analogy was the error R1 identified: `save --force` overwrites
`memories.content` in place under the same id, so a forced package
overwrite would have to reproduce the import's scanning, hashing, manifest and
version logic in a second code path — or leave the manifest describing the
previous document. Refusing is smaller, safer, and consistent with §5.8. An
operator who needs to change a package publishes a **new version**.

### 5.8 Deletion, shredding and immutability

* **Soft shred** (`mpm skill delete`) stamps `deleted_at` on the skill row. The
  package row and its file rows stay — recoverable, exactly as today's soft
  shred is. **A later `save --force` does *not* resurrect a packaged skill**:
  it is refused by the guard in §5.8.1, so the package stays soft-shredded
  until it is explicitly re-imported — the import path is not the legacy
  force path, and a re-import of the same `package_hash` resurrects the row
  (§5.7). That is a consequence of R1's refusal, and it is stated here rather
  than discovered at runtime.
* **Hard shred** (`mpm skill shred`) deletes the `memories` row. The foreign
  keys cascade: package row, then file rows. The blob rows become
  unreferenced and are garbage-collection candidates.

On immutability, precisely:

* A file's bytes are **never mutated in place**. Blob rows are insert-only.
* A published `(name, version)` always resolves to the same bytes.
* **A published package is never replaced in place, by any path.** Not by a
  package import (§5.7), and not by the legacy write path — see below.

#### 5.8.1 The legacy write path is refused against a packaged skill (**R1**)

**What the substrate does today.** `SaveSkillWithInvocation`'s force branch
replaces `memories.content` **in place under the same id**
(`internal/core/skill_db.go:624-631`). It knows nothing about packages — it
cannot, the tables do not exist yet — and it is reached with `force` taken
straight from the MCP payload (`internal/core/tools/handlers.go:3758`) and
from `SaveSkill` (`internal/core/skill_db.go:476`). It is the **only** in-place
content writer for a skills row in the whole tree.

**What would happen without a guard.** After a legacy `save --force` against a
packaged skill:

* `read` returns the **new** body and `read_file(path=SKILL.md)` returns the
  **old** bytes — one skill id, two different entrypoint bodies, depending on
  which API is asked;
* `projection_sha256` (§5.2) no longer matches `memories.content`, so the
  derivation is no longer auditable;
* the package row, the file rows and the blobs all still describe the
  **previous** document.

**The correction: fail closed.** When a `skill_packages` row exists for the
skill id, a legacy `save --force` — from the CLI or from the MCP surface —
is **refused** with a distinct status:

```
package_immutable: skill "system-prompt-writer" v1.0.0-unversioned is a
package; its entrypoint and manifest are immutable. Publish a new version
instead of force-overwriting the projection.
```

**Nothing is written.** The refusal happens before the projection write, so
the id, the manifest, the file rows and the hashes all continue to describe
the same document. There is no partial state, no diverged projection, and no
silent divergence to discover later.

**Why refusal, and not package-aware rewriting.** Teaching the legacy path to
rewrite package rows would require reproducing the import's scanner, hashing,
manifest and version logic in a second code path — a second place where a
security control could be forgotten. Refusing is smaller, has one behaviour,
and is consistent with §5.7. It also makes §12.1 **O2** decidable: the answer
is *no*, and the answer is enforced rather than merely recommended.

**Scope.** The guard is one branch in one function (`internal/core/skill_db.go`),
plus its CLI and MCP surfaces. `SaveSkillAndDeprecatePrior`
(`internal/core/skill_db.go:1021-1065`) touches `metadata` only, and the
workshop's save call passes `force=false` — neither is an in-place content
write and neither needs the guard.

**Consequence for the operator.** `save --force` on a packaged skill no
longer behaves as it does for a legacy single-document skill. That is a
deliberate divergence, it is stated here rather than discovered later, and
it is the direct cost of keeping one id from carrying two documents.

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

**§6 names the path that keeps this promise.** The package tables are created
in a **local-only** DDL slice that `attachShared` does not touch, so no
shared database ever receives them — not even empty ones. **T50** proves it.

---

## 6. Compatibility and migration

**No existing row is touched.** The package tables are empty for every skill
that is not a package. Existing single-document skills keep working with no
change at all, and can optionally be *re-imported* as a one-file package
(§13 T02) to gain the package surface.

**Migration mechanics — the package tables are LOCAL-ONLY.** Three
`CREATE TABLE IF NOT EXISTS` statements and one `CREATE INDEX`, applied at boot
in a **local-only** DDL group. They are additive: an old binary ignores them, a
new binary creates them.

**They must not go in `BaseTables`.** `attachShared` applies **every**
`CREATE TABLE` in `BaseTables` to the shared schema, rewriting the table name
to `shared.<name>` (`internal/core/db.go:2132-2143`), and a second loop does
the same for the `CREATE INDEX` statements in that slice (`:2158-2167`). A
package table in `BaseTables` would therefore be created as
`shared.skill_packages`, `shared.skill_package_files` and
`shared.skill_package_blobs` in **every** shared database — the precise thing
§5.9 says this design does not do.

**The local-only path, named exactly.** The three statements and the index go
in a **new `SkillPackageTables` slice** in `internal/core/schema.go`, applied
at boot by the same function that applies `ReferenceTables` and `WorkTables`
(`internal/core/db.go:2899`, `initUnifiedSchema`; the `ReferenceTables` loop
at `:2937`) and **outside** the `attachShared` loops. It is a sibling of
`ReferenceTables` — **not** a member of `BaseTables`.

**The precedent the first revision cited is already local-only.**
`reference_docs` (`internal/core/schema.go:953`) and `reference_chunks`
(`:1002`) live in **`ReferenceTables`** (`schema.go:949-1047`), **not** in
`BaseTables` (`schema.go:38-936`), and `attachShared` touches only
`BaseTables`. The repository says so in its own words at
`internal/core/db.go:2318`: *"reference_* tables live in ReferenceTables, not
BaseTables — Phase 1"*. Measured at runtime — a `DatabaseManager` with a
shared database attached, listing `shared.sqlite_master`:

```
C5: the SHARED schema has 35 tables
C5: reference_docs         in the shared schema: false
C5: reference_chunks       in the shared schema: false
C5: memories               in the shared schema: true
C5: sessions               in the shared schema: true
C5: skill_packages         in the shared schema: false
```

So the precedent is not *"put it in `BaseTables`"* — it is **"put it in a
slice `attachShared` does not touch"**, and that is the rule P1 follows. The
defect in the first revision was ambiguity, not a wrong precedent: *"applied
at boot alongside the existing table groups, following the `reference_docs`
precedent"* named a precedent without naming the slice, and an implementer who
read *"the existing table groups"* as `BaseTables` would have created the
tables in every shared database. **T50** proves the shared schema is
unaffected.

**The restore allow-list is a hard gate.** `mpm restore-db` rejects any dump
naming a table that is not on `CanonicalMPMSchema`
(`internal/core/sql_dump_validator.go:530+`), and `TestCanonicalSchemaSync`
(`internal/core/sql_dump_canonical_schema_test.go:22-52`) requires every table
in the schema sources to appear on that list. The three new tables must be
registered in both places, or **restore fails**. This is a stage-P1
requirement, not a follow-up.

**Backup improves.** Because the bytes live in the database, `mpm backup`
(`sqlite3 .dump`) carries them and `mpm restore-db` restores them. This is
the first MPM store whose payload is genuinely covered by the backup.

### 6.1 The two rollbacks are different operations (**R9**)

The first revision used one word — "rollback" — for two operations with
opposite characteristics. They are named separately here, and the runbook in
§10 P1 carries both.

| | **binary rollback (lossless, blind)** | **schema rollback (destructive)** |
|---|---|---|
| what it is | an old binary runs against a database that has the package tables | `DROP TABLE` on the three package tables |
| package bytes | **retained but invisible** — still in the file; they reappear when the new binary returns | **irreversibly destroyed**, including third-party files that exist nowhere else |
| everything else | untouched | untouched |
| operator-visible signal | none — there is no schema-version gate anywhere in the tree (§3.1), so the old binary opens the newer database without complaint | the tables are gone |
| pre-condition | **export the packages first** (§6.2) | **export the packages first** — this is the only copy |

**"Lossless" applies to the database, not to the operator's ability to use
it.** A binary rollback preserves the bytes on disk, but it also creates the
restore incompatibility in §6.2. The two are not independent.

**"Blind" is the accurate word for the old binary's view.** An old binary has
no way to know the three tables exist; it does not warn, it does not refuse,
and it does not adapt. Nothing in the substrate would tell an operator that a
package was present.

### 6.2 After a binary rollback, `mpm restore-db` of a new-binary backup fails closed (**R8**)

This is the correction the first revision missed, and it has a real cost.

**The mechanism.** `mpm backup` is `sqlite3 .dump` — **generic**. It does not
know which tables exist. So:

* a backup taken by the **new** binary **contains** the three package tables;
* an **old** binary's `mpm restore-db` rejects any dump naming a table that
  is not on its `CanonicalMPMSchema` allow-list (§3.1) — and the three package
  tables are not on it.

Therefore:

> **After a binary rollback, restoring a backup taken by the new binary
> fails closed.** The old binary can produce a backup it cannot restore, and
> it can refuse a backup it produced itself once the new binary has run.

**This is a property of the existing allow-list, and the allow-list is not
weakened to hide it.** Relaxing `restore-db` to skip unknown tables with a
warning would trade a loud, safe refusal for a silent, partial restore —
exactly the class of failure the validator exists to prevent. It is also a
change to an existing fail-closed component, which is outside this design's
scope and would need its own decision and its own review.

**The operator procedure is therefore mandatory, not advisory:**

1. **Before any binary rollback**, export every package to a `.skill` archive:
   `mpm skill export <name> --output <file>.skill` for each package, or the
   batch form delivered by stage P7 (§6.3).
2. Verify the exports exist and are non-empty.
3. Only then roll the binary back.
4. The old binary's `restore-db` will still refuse a new-binary dump. Restore
   with the **old** binary from a backup taken **before** packages existed, or
   restore with the new binary.

**Why this is stated rather than designed away.** The alternative — making the
old binary able to restore a package-bearing dump — is impossible without
changing the old binary, which by definition is not being changed. The
incompatibility is a property of the rollback, not a defect to be papered
over.

### 6.3 The export contract: no silent loss (**R11**)

The first revision contradicted itself on `mpm export --collection skills`:
the compatibility matrix said the package metadata is not exported, while
**O8** recommended carrying it "as a follow-up after P6". The matrix's
version is the one an implementer would have built, and it is a **silent data
loss** path: an operator's "export my skills" workflow loses every reference
file, and so does the workspace-to-workspace move that **O4** nominates as the
transport. The bytes exist nowhere else.

**The contract, in two stages:**

| stage | behaviour |
|---|---|
| **Before full package export exists** (from P4 — the first stage in which a package can exist — until P7) | `mpm export --collection skills` **carries the skill row only**, and it **says so**: the export response and the CLI output carry an explicit **loss marker** naming every skill whose package was not included, with the count. The marker is not a warning that can be missed — it names the affected skills. |
| **Full package export** (stage P7) | `mpm export --collection skills` emits, alongside the skill rows, one `.skill` archive per package, using the same deterministic writer as `mpm skill export` (§5.6), so the archive is byte-identical to one produced by the single-package command. |

**The marker is the contract, not a stopgap.** Before P7, an export that
drops package content must be **explicit about it**. A caller that reads the
export response learns that reference files were omitted and which skills
were affected; a caller that does not read it still sees the marker on the
CLI's standard output. There is no configuration in which the loss is
silent.

**Stage P4 delivers the marker**, because P4 is the first stage in which a
package can exist — the instant it does, `mpm export --collection skills`
becomes a loss path, and the marker must exist in the same release that
introduces the loss. P7 then removes the need for the marker by making the
export complete. The stage order is therefore dependency-clean: no stage
depends on behaviour delivered only by a later one.

### 6.4 Age-based pruning must not remove a packaged skill (**R7**)

`mpm prune --older-than` is a **collection-blind hard delete**
(`internal/core/web_db.go:1781-1792`): no `collection` filter, only
directives excluded. It removes a skill row of any age, and the foreign-key
cascade takes the package row and the file rows with it. A package's bytes
have no second copy, so the bytes are then gone (§3.2).

**The correction: `collection = 'skills'` is excluded from `PruneOlderThan`.**
This mirrors the existing directive exemption — `DirectiveExclusionClause` is
already the exclusion mechanism in that statement (§3.4 E7) — and it is the
smaller of the two available corrections. The alternative, "require the
operator to export before pruning", would require a per-row check inside a
bulk delete and would still leave a race between the check and the delete.

**The blast radius is wider than "packages", and it is stated here rather than
discovered by an operator.** The exclusion is a **`collection` filter on the
delete statement itself**, so it excludes **every** skill, packaged or not.
Once P3 lands:

> `mpm prune --older-than 90d` can no longer remove **any** skill — packaged
> or not.

That is a behaviour change for every existing skill in every workspace, not
only for the skills that own a package. An operator who has been using
`--older-than` to reclaim space from stale skills will find that it silently
stops doing anything at all — no error, no warning, no count of skipped
rows. The §6 compatibility matrix carries the same statement, and the choice
is an **open operator decision** (**O17**, §12.2): this revision states the
recommendation and the cost, and does **not** record an approval that the
operator has not given.

**The package-aware alternative, and its cost.** Excluding only the
*packaged* skills — an `AND NOT EXISTS (SELECT 1 FROM skill_packages …)`
clause — would preserve the operator's age-based pruning for ordinary skills.
It is **not** the specification, for two reasons: it puts a per-row check
inside a bulk delete, which leaves a race between the check and the delete (a
package row could be created in between), and it is a larger change to a
statement that today carries exactly one exclusion clause. **O17** is where
the operator confirms the blanket form, and it must be answered **before P3**.

**What is *not* changed.** `PruneNeverAccessed` is already safe (it requires
`is_long_term = 0`; skills are written `is_long_term = 1`), and the default
`mpm prune` is `PruneExpired`, which skills do not match because
`expires_at` is NULL. Only `PruneOlderThan` needs the exemption.

**The operator can still remove a packaged skill deliberately.** A hard shred
(`mpm skill shred`) still cascades and still destroys the package bytes — that
is its purpose. What is removed is the *unintentional*, *bulk*, *age-based*
destruction of third-party content that exists nowhere else. Under the
blanket exemption the same statement holds for **ordinary** skills: they are
no longer removed by age either, and an operator who wants a stale skill gone
removes it deliberately.

### Compatibility matrix

| existing surface | effect |
|---|---|
| `mpm save-skill --file` | unchanged |
| `mpm call mpm_skills` with `save` | unchanged for every skill that is not a package; **refused** with `package_immutable` for a packaged skill (**R1**, C10) |
| `mpm read-skill` | unchanged |
| `mpm call mpm_skills` with `read` | existing fields unchanged; gains one `package` object, **present whenever the skill is a package** (**R6**) and absent otherwise |
| `mpm call mpm_skills` with `list` | unchanged — `SkillSummary` is not modified at all |
| `mpm list-skills` | unchanged |
| Discovery and the wake context | unchanged — no package byte is ever inlined |
| `mpm skill delete` / `mpm skill shred` | unchanged semantics; the package cascades on a hard shred |
| `mpm call mpm_skills` with `promote_to_global` | unchanged semantics; also flips the package row |
| `mpm skill workshop` | unchanged |
| `mpm backup` / `mpm restore-db` | extended to carry packages; an **old** binary refuses a new-binary dump (§6.2) |
| `mpm export --collection skills` | carries the skill row; **explicitly marks** every package it cannot carry (§6.3); full package export follows in P7 |
| `mpm prune --older-than` | **excludes** `collection = 'skills'` (§6.4, **R7**). **The exclusion is blanket: once P3 lands this command can no longer remove *any* skill, packaged or not**, so an operator's existing age-based skill pruning stops working — silently, with no error. The operator's confirmation of that blast radius is **O17** (§12.2), and it must be answered **before P3** |
| The agent-facing package surface | **read-only** — no import action (**R4**, C13) |

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
| 3 | **Symlinks, FIFOs, devices** | **Best-effort and conditional, not the control.** `zip.File.Mode()` exposes the entry's type bits *only when the archive's central-directory creator byte says Unix*. An entry whose type is neither regular nor directory is rejected as `unsupported_entry_type` — **but that rejection is not available for every archive** (**R5**). A ZIP written without Unix attributes carries a FAT/MS-DOS creator byte, and `Mode()` then reports every **non-directory** entry as `0666` regular: a symlink, a FIFO and a device all **pass** the type check as regular files. A **directory** entry is still detected, because the trailing `/` in its name identifies it (§7.1). The real defence is Rule 2 in §4.4: an import writes package bytes into a database column and never materialises them as a file, so an entry of any type cannot become a filesystem object. The **directory** path has a genuinely stronger guarantee — `os.Lstat` is reliable — so the two paths are described separately below. |
| 4 | **Windows drive letters and separators** | Rejected by an explicit check, **not** by `filepath.IsAbs`. On Linux `/etc/passwd` is absolute but `C:\Windows\System32\evil.dll` is not — a Linux-only absolute-path test would let it through. Backslashes are rejected in any position. |
| 5 | **Hardlinks** | Not representable in ZIP. On the directory-import path, `os.Lstat` is used, never `os.Stat`, so a symlink is never followed; a hardlinked file is simply a regular file, and MPM stores bytes, not inodes. |
| 6 | **Duplicate paths** | Two entries with the same canonical path are rejected as `duplicate_path`. |
| 7 | **Case-colliding paths** | Two canonical paths differing only by ASCII case are rejected as `path_case_collision`. This is what makes an exported package safe on a case-insensitive filesystem. |
| 8 | **Unicode normalisation** | Paths are normalised to NFC; two names that normalise to the same path are rejected as `path_unicode_collision`. Measured: `café.md` in NFC is 12 bytes and in NFD is 13, they are distinct Go strings, and lowercasing does not unify them — the normalisation has to be done explicitly. Control characters and NUL are rejected. |
| 9 | **ZIP bombs / extreme ratios** | Three independent caps: a per-entry declared-size cap, a **count of actual bytes read** through a limited reader, and a total-uncompressed cap across the package. The byte count is not optional: a measured 4 MiB zero file compresses to 4 195 bytes — a **999.8:1** ratio — and the declared size is an attacker-controlled header, so the only trustworthy number is the one the importer counts. |
| 10 | **Lying size headers** | `archive/zip` catches a declared size *smaller* than the real payload (it reports `zip: not a valid zip file` after reading 0 bytes) and a declared size *larger* than the payload (it reads what is there and then reports `unexpected EOF`). Both are treated as `archive_invalid`, but the byte cap stands independently because the header can be made self-consistent. |
| 11 | **Excessive file count or total size** | Caps: 512 files, 32 MiB total uncompressed, 4 MiB per file, and 256 KiB for the entrypoint. The entrypoint cap matters because `ListSkills` parses frontmatter out of every skill row's full `content` on every call — a multi-megabyte entrypoint would slow every skill listing in the workspace. **The caps are enforced *during* traversal, not after enumeration** (**R12**): the walk stops at the 513th entry, and the byte counters are checked as bytes are read, so a directory holding ten million entries is never fully walked, `Lstat`'d and measured. On the ZIP path the archive itself bounds the entry count, but the same in-flight counters apply. |
| 12 | **Malformed archives** | Reader errors are surfaced as `archive_invalid`. Every entry is read **to EOF**, which is what makes the CRC check run (§13). |
| 13 | **Corrupt stored bytes** | `archive/zip` **does** verify the CRC — a single flipped content byte yields `zip: checksum error` — **but only when the entry is read to EOF**. A bounded read stops early and skips the check entirely. Therefore: the importer reads every entry to EOF, and the bounded read API (§8) **does not claim** CRC verification. Separately, every stored file's SHA-256 is re-verified on read, and a mismatch is refused rather than returned. |
| 14 | **Secret leakage** | **Every file** is scanned with MPM's existing `isSensitiveContent` (30 patterns) and `isPoisonedAt` (20 default phrases) before insertion. Today the scanner sees only `memories.content` — the entrypoint. A package's reference files would bypass it entirely without this change. This is the most important gap the design closes. Binary files are scanned for the high-signal secret patterns only, since scanning arbitrary binary bytes for the full pattern set produces false positives. **The poison scanner can fail open** — it reads an operator-editable file and returns "not poisoned" if that file cannot be read — so a package import **fails closed** with `scanner_unavailable` when the phrase file is unreadable (**R10**, §7.3). A skipped scan is never reported as a successful scan. |
| 15 | **Prompt injection** | The poison-phrase scanner runs on every file. It is a backstop, not a guarantee: it matches twenty fixed phrases, so an oblique injection would pass. The behavioural control is that package content is returned as **data**, not as instructions — and **the provenance is carried on the response that inlines the content**: the `read` response carries a `package` object with the entrypoint's `entrypoint_sha256` and its `scanned` status, and it is **present whenever the skill is a package**, not optional (**R6**). The `read_file` response carries the file's own hash and scan status. The **honest limit** of this control: the entrypoint body is still inlined into the agent's context without a per-read textual banner, so the guarantee is *machine-checkable provenance on every response*, not *a human-readable warning in the text*. |
| 16 | **Unsupported executable payloads** | Scripts are stored but flagged `executable: true` in the manifest, and **never executed**. MPM has no code path from package bytes to execution. Executable *binaries* (ELF, PE, Mach-O magic) are rejected by default, with an explicit opt-in that still only stores bytes (decision **O3**, §12.2). |
| 17 | **Partial imports** | One transaction covers the manifest row, the file rows, the blob rows, the `memories` projection and the `is_latest` flip. **The importer owns that transaction** and writes the projection through the tx-aware primitive in §3.4 E5, so the claim is true rather than aspirational (**R2**). Validation runs *before* the transaction opens, so a rejected archive writes nothing at all. A failed import leaves zero rows — verified in §13, T08. |
| 18 | **An import that passes the entrypoint but poisons a reference** | Blocked by threat 14, and the *whole* import rolls back, not just the offending file. |
| 19 | **A directory walk that escapes the import root** | The walk uses `os.Lstat` and never follows a symlink; it **refuses to descend into a symlinked directory** and **refuses to leave the resolved root** after each hop, so a symlink inside the tree cannot be used to pull in bytes from elsewhere on the filesystem (**R12**, **R4**). The resolved root is computed once with `filepath.EvalSymlinks` and compared against every candidate before it is read. |

### 7.1 The entry-type check, stated exactly (**R5**)

The two import paths do **not** have the same guarantee, and the first
revision stated them as if they did.

| | ZIP path | Directory path |
|---|---|---|
| how the entry's type is determined | `zip.File.Mode()`, which interprets the external attributes **only when the central-directory creator byte says Unix** | `os.Lstat` on the real filesystem |
| symlink entry detected? | **only** for a Unix-attribute archive | yes, always |
| FIFO / device entry detected? | **only** for a Unix-attribute archive | yes, always |
| FAT/MS-DOS creator byte | `Mode()` reports `0666` regular for every **non-directory** entry — symlink, FIFO and device all pass the type check. A **directory entry is still detected**: the trailing `/` in its name identifies it, independently of the external attributes | not applicable |
| what actually makes it safe | **Rule 2** (§4.4): an import writes the bytes into a database column and never materialises them as a file | **Rule 2**, plus `Lstat` |

**Measured.** The same six entries, under a Unix creator byte and under a
FAT creator byte:

```
-- creator = Unix --
  "pkg/SKILL.md"  mode=-rw-r--r--  type=----------  regular=true   dir=false
  "pkg/link.md"   mode=Lrwxrwxrwx  type=L---------  regular=false  dir=false
  "pkg/fifo"      mode=prw-r--r--  type=p---------  regular=false  dir=false
  "pkg/dev"       mode=Drw-r--r--  type=D---------  regular=false  dir=false
  "pkg/sub/"      mode=drwxr-xr-x  type=d---------  regular=false  dir=true   → skipped
  "pkg/nodir"     mode=-rw-r--r--  type=----------  regular=true   dir=false

-- creator = FAT/MS-DOS (the same archive, the central-directory creator byte rewritten) --
  "pkg/SKILL.md"  mode=-rw-rw-rw-  type=----------  regular=true   dir=false
  "pkg/link.md"   mode=-rw-rw-rw-  type=----------  regular=true   dir=false  → ACCEPTED AS A REGULAR FILE
  "pkg/fifo"      mode=-rw-rw-rw-  type=----------  regular=true   dir=false  → ACCEPTED AS A REGULAR FILE
  "pkg/dev"       mode=-rw-rw-rw-  type=----------  regular=true   dir=false  → ACCEPTED AS A REGULAR FILE
  "pkg/sub/"      mode=drwxrwxrwx  type=d---------  regular=false  dir=true   → skipped
  "pkg/nodir"     mode=-rw-rw-rw-  type=----------  regular=true   dir=false
```

The permission bits differ between the two blocks — Go synthesises `0666` for
a FAT entry and `0777` for a FAT directory — but the load-bearing fields are
`type` and `dir`, and those agree with the intent: **the symlink, the FIFO and
the device degrade to regular files under a FAT creator byte; the directory
entry does not.**

The type check's availability depends on **an attacker-chosen header byte**.
It is therefore a defence in depth, not a boundary. The boundary is Rule 2.

**What follows for the implementation.** The type check stays, and its
`unsupported_entry_type` refusal stays — it is free, and it rejects the
common case. But the *contract* is Rule 2, and no acceptance criterion may
depend on the type check being available. §13 T16 and T35 test exactly this:
T16 is the Unix-attribute case, T35 is the FAT-attribute case, and **both are
accepted** — with the FAT case accepted *because* the bytes never reach the
filesystem, not because the type check caught it.

**Why the directory entry survives the creator rewrite.** Go's
`FileHeader.Mode()` (`archive/zip/struct.go:317-328`) reads the external
attributes only for a Unix, MacOSX, NTFS, VFAT or FAT creator byte, and then
applies one rule **unconditionally**:

```go
if len(h.Name) > 0 && h.Name[len(h.Name)-1] == '/' {
        mode |= fs.ModeDir
}
```

So a directory entry is identified by **its name**, not by its attributes.
Measured with the MS-DOS directory attribute deliberately cleared, and again
with it set on a name that has no trailing slash:

```
-- FAT, MS-DOS directory attribute CLEARED, name still ends in "/" --
  "pkg/sub/"      mode=drw-rw-rw-  type=d---------  regular=false  dir=true   → skipped
  "pkg/nodir"     mode=-rw-rw-rw-  type=----------  regular=true   dir=false

-- FAT, MS-DOS directory attribute SET on a name with no trailing "/" --
  "pkg/nodir"     mode=drwxrwxrwx  type=d---------  regular=false  dir=true   → skipped
```

Both signals identify a directory independently: the trailing `/` and the
MS-DOS directory attribute. **A directory entry is therefore detected as a
directory under both creator bytes**, and it is **skipped** — it is not a file,
it is not stored, and it does not participate in the root derivation (§5.3.1
rule 1). The entries that degrade to regular files are the symlink, the FIFO
and the device: they have neither signal.

This is **safer** than the first revision claimed, and it is the one place
where the FAT case is *better* than the Unix case rather than worse. An
implementer must therefore **not** write the FAT rule as "a trailing-slash
entry falls through to the canonical path rules and is rejected as
non-canonical". It is skipped, and §13's T35 expects the FAT archive to be
**accepted** — the two must be implemented together. A directory entry named
`pkg/sub` (no trailing slash) is simply a zero-byte file, and it is stored as
one: a zero-length row, and no filesystem object. Both outcomes are safe, and
neither depends on the type check being available.

### 7.2 The scanner's fail-open path, and the fail-closed import (**R10**)

The two scanners are not symmetric, and the difference is load-bearing.

| scanner | how it works | can it fail open? |
|---|---|---|
| `isSensitiveContent` — the secret scanner | 30 in-process regular expressions | **no.** There is no external dependency to become unreadable |
| `isPoisonedAt` — the poison scanner | reads an **operator-editable file** of toxic phrases | **yes.** If that file cannot be read, the function returns "not poisoned" (`internal/core/memory.go:845-851`) and the scan **passes everything** |

That asymmetry is invisible from the outside, which is exactly why it needs a
stated contract: a skipped scan and a successful scan look identical to the
caller.

**The contract: a package import fails closed.**

> If the toxic-phrase scanner cannot load its phrase file, the import is
> **refused** with `scanner_unavailable`, and **nothing is written** — no
> manifest row, no file row, no blob row, no projection.

**Why refusal rather than a warning.** A warning would let an import report
`success: true` on content that was never checked against the one scanner
that looks for prompt injection. The design's threats 14, 15 and 18 all rest
on that scan; an import that skipped it would be exactly the "skipped scan
reported as successful" outcome the correction forbids. Refusing costs the
operator one command and one clear message; it removes a silent hole.

**The asymmetry is stated rather than fixed here.** Closing the poison
scanner's own fail-open path is a change to an existing component with its
own callers, and it is outside this design's scope. The package import does
not depend on that being fixed — it checks for itself.

**What a successful import therefore guarantees.** For every file it stores,
the manifest records which scanners actually ran (§5.5). A binary asset
records a **disclosed skip** (`poison: false` — the poison scan does not apply
to binary bytes, §7.3). There is no way for a row to record a scan that did
not happen, because an import that could not scan does not write a row at all.

### 7.3 Scanner coverage by file class

| class | detection | stored | secret scan | poison scan | FTS-indexed | executable |
|---|---|---|---|---|---|---|
| entrypoint Markdown | `SKILL.md` at the root | yes | full | full | yes, via the `memories` row | no |
| reference Markdown | `references/**.md` | yes | full | full | **no** | no |
| other text | `.txt`, `.json`, `.yaml`, `.yml` | yes | full | full | no | no |
| script | `.sh`, `.py`, `.js`, `.rb`, `.pl` | yes, flagged | full | full | no | **never** |
| binary asset | `.png`, `.jpg`, `.svg`, `.pdf`, `.woff2` | opt-in | secret patterns only | **no — a disclosed skip** | no | no |
| executable binary | ELF / PE / Mach-O | **rejected** by default | — | — | — | — |

The "no" in the poison column for a binary asset is a **recorded skip**, not a
pass and not a failure: the poison scanner matches prose phrases, and running
it over arbitrary binary bytes produces meaningless output. The manifest
records it as `{"secret": true, "poison": false}` so a reader can tell the
difference between "scanned and clean" and "not applicable" (§5.5).

### 7.4 Trust boundaries

* **B1 — archive to MPM.** Untrusted. Everything is canonicalised, bounded and
  scanned before a single row is written. The scan is **fail-closed**: if a
  required scanner cannot run, the import is refused (§7.2).
* **B2 — MPM store to agent.** Semi-trusted. The bytes were scanned at import,
  but the scanner is a backstop against a fixed phrase list, not a proof of
  safety. Every response that carries package content carries **machine-checkable
  provenance** — the file's hash and its scan status — so an agent can tell
  package content from MPM-authored content without parsing a banner out of a
  prose body (§7, threat 15; §8.2; §8.5).
* **B3 — agent to execution.** None. MPM does not execute package content.
  Any future "run this skill's script" feature is a separate design with its
  own gate, and is explicitly out of scope here.
* **B4 — agent to the filesystem.** **None in the first cut.** The
  agent-facing surface cannot create a package, cannot name a filesystem path
  and cannot cause a byte to be read from disk (**R4**, §8.3). Import is
  operator-driven.

---

## 8. The retrieval API

### 8.1 Backward-compatibility rules

* The `read` action with no new parameter returns **today's fields
  unchanged**; for a skill that **is** a package it gains one object (§8.2)
  that a caller that ignores it never sees. The `list` action and
  `SkillSummary` are **not modified at all**.
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
  line added, and the response carries a `version_override` object describing
  exactly that. No existing field changes.
* **No package file other than the entrypoint is ever inlined by `read`.**
  The agent must ask for a specific file.

#### 8.2.1 The provenance object is **not** optional for a packaged skill (**R6**)

The first revision described the `package` object as *optional*, and it
carried only `entrypoint`, `file_count`, `package_hash` and `version_override`
— no per-file hash and no scan status. Meanwhile the `read` action inlines the
entrypoint body straight into the agent's context
(`internal/core/tools/handlers.go:3814` returns `skill.Body`), with no
provenance marker on it at all. So the one piece of package content most
likely to carry an injection — the entrypoint, the only file inlined by
default — came back with nothing an agent could check.

**The correction.** The `package` object is **present whenever the skill is a
package**, and it carries the entrypoint's hash and its scan status:

```json
{
  "success": true,
  "id": "skill:system-prompt-writer-v1.0.0-unversioned",
  "name": "system-prompt-writer",
  "version": "1.0.0-unversioned",
  "body": "…",
  "is_global": false,
  "package": {
    "entrypoint": "SKILL.md",
    "entrypoint_sha256": "783ece4b3869ae42d2d46f540d01d1da0f61147a8eada519eff15941b46d423d",
    "file_count": 3,
    "package_hash": "…",
    "scanned": true,
    "version_override": {"declared": null, "effective": "1.0.0-unversioned",
                         "reason": "package_declares_no_version"}
  }
}
```

* `entrypoint_sha256` is the hash of the **original** file bytes, as supplied
  (§5.2) — not of the projection. The caller can therefore verify that the
  body it is looking at derives from that file.
* `scanned` is `true` only when every scanner that applies to the entrypoint
  actually ran and passed. It is not a constant: a package whose import
  recorded a disclosed skip reports that skip in the `files` listing, and the
  import would have been refused outright if a required scanner could not run
  (§7.2).
* For a skill that is **not** a package, the object is **absent** — so C1
  holds exactly: a caller of today's API sees today's response byte-for-byte
  for every skill that is not a package.

**What this guarantee is, and is not.** It is **machine-checkable provenance on
every response that carries package content**: a hash and a scan status, on
the `read` response and on the `read_file` response, that an agent or a tool
can verify and act on. It is **not** a human-readable warning banner inside
the entrypoint prose — the body is returned as the same string it is today.
The poison scanner remains a backstop against a fixed phrase list, and this
design does not claim it is more than that (§7, threat 15).

`list` is deliberately **not** enriched. `SkillSummary` has no JSON tags, so it
serialises with Go field names; adding a field would change the shape of a
structure that the wake context, the proactive-recall hint and `mpm info` all
consume. Discovery stays byte-identical.

### 8.3 New actions — and what is **not** on the agent-facing surface (**R4**)

| action | purpose | on the agent-facing surface? |
|---|---|---|
| `files` | list a package's files — **metadata only, no content** | **yes** — read-only |
| `read_file` | read one file, optionally bounded | **yes** — read-only |
| `verify` | re-hash every stored file and report integrity | **yes** — read-only |
| `export` | write the package back out as a `.skill` archive | **no** in the first cut — it writes to the filesystem |
| `import` | import a package from a `.skill` archive or a directory | **no** in the first cut — **CLI only** |

**Why there is no agent-facing `import` action.** The first revision gave the
MCP surface an `import` action with a required `path` parameter. That would
have been the **first** action in the whole tool surface to accept a
filesystem path that may name a **directory**, and it specified no
confinement, no allow-listed root and no operator-consent gate.

The scale of that is worth stating plainly, because it is the substance of the
correction rather than a stylistic preference:

* The MCP registry's **only** path-shaped parameter today is `filepath` on
  `mpm_references` (`internal/core/tools/registry_list.go:217`), and that one
  names a **file** with an extension allow-list. `mpm_skills` has no path
  parameter at all. There is **no path-confinement helper anywhere in the
  tool layer** — no `confine`, no `safePath`, no `resolveWithin`, no
  `ensureUnder`, and no `filepath.EvalSymlinks`.
* An agent could therefore have pointed `path` at any directory the MPM
  process can read — the live workspace, `~/.ssh`, `/etc` — and imported up to
  512 files and 32 MiB of it.
* Those bytes are then stored **durably**, and are surfaced to **other
  agents** through discovery, `read` and `read_file`.

That is a read primitive, an amplifier (durable, and replicated into other
agents' contexts) and a persistence mechanism, in one action, on the surface
whose stated purpose is to give an agent a *bounded* vocabulary.

**The correction: import is operator-driven.** The CLI
(`mpm skill import <path>`) keeps an arbitrary path, because the CLI *is* the
operator. The agent-facing package surface is **read-only** — `files`,
`read_file` and `verify` — and can neither create a package nor name a
filesystem path (§7.4, B4). Nothing in this design's motivation requires an
agent to *create* packages; the motivation is that agents should be able to
*use* them, progressively and with provenance.

**The deferred question, for the operator.** Whether a *later* stage should
add an agent-facing import — and if so, confined to an operator-configured
import root, with the same explicit consent gate that `promote_to_global`
already uses (`internal/core/skill_db.go:788-796`) — is **§12.2 O14**. It is
not answered here, and nothing in stages P1–P9 depends on the answer.

**The directory-walk confinement that applies to the CLI path anyway.**
Operator-driven does not mean unconfined. The CLI's directory import must
still: use `os.Lstat` and never follow a symlink; **refuse to descend into a
symlinked directory**; **refuse to leave the resolved import root** after each
hop; and enforce the file-count and byte caps **during** the walk, not after
enumeration (§7, threats 11 and 19; **R12**). The walk's resolved root is
computed once with `filepath.EvalSymlinks` and every candidate is compared
against it before it is read.

Parameters (all additive; the `params` object is already
`additionalProperties: true`):

| action | parameters |
|---|---|
| `files` | `skill_id`, or `name` + `version` |
| `read_file` | `skill_id`, or `name` + `version`; `path` (required); `offset`, `limit`, `max_bytes`, `raw` |
| `verify` | `skill_id`, or `name` + `version` |
| `import` *(CLI only)* | `path` (required), `version`, `name`, `entrypoint`, `allow_binary` |
| `export` *(CLI only in the first cut)* | `skill_id`, or `name` + `version`; `output` |

Note that `import` has **no `force` parameter** (§5.7, §5.8): a published
package is immutable, so there is nothing for a force flag to do.

### 8.4 Command line

```bash
mpm skill import <path> [--version V] [--entrypoint P] [--name N]
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

`mpm skill import` is **operator-driven** and is the only import surface in
the first cut (§8.3).

### 8.5 Response shapes

`files` — metadata only, including the scan status of each file:

```json
{
  "success": true,
  "package_hash": "…",
  "count": 3,
  "files": [
    {"path": "SKILL.md", "sha256": "783ece4b…", "size_bytes": 12976,
     "content_type": "text/markdown; charset=utf-8", "role": "entrypoint",
     "executable": false,
     "scanned": {"secret": true, "poison": true}}
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
  "scanned": {"secret": true, "poison": true}
}
```

`import` — the full disclosure:

```json
{
  "success": true,
  "status": "installed",
  "skill_id": "skill:system-prompt-writer-v1.0.0-unversioned",
  "name": "system-prompt-writer",
  "version": "1.0.0-unversioned",
  "declared_version": null,
  "version_override": {"effective": "1.0.0-unversioned", "reason": "package_declares_no_version"},
  "entrypoint": "SKILL.md",
  "entrypoint_sha256": "783ece4b…",
  "projection_sha256": "…",
  "package_hash": "…",
  "file_count": 3,
  "warnings": ["missing_when_to_use"],
  "errors": []
}
```

`status` is one of `installed` or `already_installed` (idempotent re-import).
There is no `replaced`: a published package is immutable (§5.7, §5.8). A
refused import reports `success: false` with a reason code and writes nothing.

### 8.6 Bounded and partial reads

`read_file` accepts `offset` (a **line index**), `limit` (a **line count**) and
`max_bytes` (a **byte cap**). The default byte cap is 64 KiB and the hard cap
is 1 MiB. A truncated response carries `truncated: true` and a `next_offset`
the caller can resume from.

**`next_offset` is defined precisely**, because the three parameters use
different units and the interaction was left implicit in the first revision:

* `offset` and `next_offset` are **line indices**, zero-based, into the file's
  line sequence. `offset: 0` is the first line. `next_offset` is always the
  index of the **first line that was not returned**, so a caller resumes with
  `offset = next_offset` and no line is ever skipped or repeated.
* **`next_offset` names a line, not a byte.** The byte cap is applied *after*
  the line range is selected, and it never selects a different range.
* **A line cut in half by the byte cap is not returned.** The implementation
  reads whole lines only: the byte cap truncates at a line boundary, and the
  line that would have crossed the cap is left for the next call. A partially
  returned line would corrupt the caller's line-index bookkeeping, and a
  partially returned *Markdown* line could change its meaning.
* **The byte cap therefore reduces the number of lines returned**, not their
  integrity.
* `next_offset` is `null` when the response is not truncated.
* `limit` bounds the line count; `max_bytes` bounds the bytes. Whichever
  binds first ends the read, and `next_offset` reflects that.

**The terminal case, stated exactly.** A file whose first line alone exceeds
the cap returns **zero** lines, `truncated: true`, and `next_offset` equal to
the original `offset`. Zero lines returned with an unchanged `next_offset` is
the one response from which **resuming cannot make progress**: the caller
would ask for the same offset and get the same empty response, forever. So
that response is **terminal**, and the contract says so in three parts:

* **The server marks it.** The terminal response carries the reason code
  **`line_too_large`**, in addition to `truncated: true` and
  `next_offset == offset`. A caller never has to infer it.
* **The caller does not resume.** A caller that receives `truncated: true`
  with `next_offset == offset` must report that **this file cannot be served
  by this API at this byte cap**, and stop. It must not resume. Raising
  `max_bytes` — up to the 1 MiB hard cap — is the only way forward, and it
  works only if the line fits.
* **The server does not produce it for any other reason.** `truncated: true`
  with `next_offset == offset` and no `line_too_large` is a **contract
  violation**, because every other truncation returns at least one line.

**The progress guarantee.** Every non-terminal response satisfies
`next_offset > offset`: at least one line was returned and the offset
strictly advances. A caller that follows "resume with `offset = next_offset`"
therefore always makes progress, and the only response that could loop is the
terminal one — which is marked as terminal, carries `line_too_large`, and
forbids resuming. **A caller must never receive an unchanged `next_offset`
that invites it to loop.**

This is also why the default cap is 64 KiB rather than something small: a
single line longer than the cap is a file this API cannot serve, and that is
a rare shape rather than a normal one.

**A bounded read does not verify the CRC** — the standard library only checks
it at end-of-stream (§7, threat 13). The response's `sha256` is the hash of
the **stored** bytes, which was verified at import and can be re-verified with
`verify`; it is not a claim that this particular read was end-to-end. The
per-file SHA-256 in the response is re-verified against the stored bytes on
every read, so a corrupted stored file is refused rather than returned
(§13 T34).

### 8.7 Progressive retrieval and context economy

| call | bytes loaded |
|---|---|
| discovery / `list` / wake context | **zero** package bytes |
| `read` | the entrypoint projection only, plus its hash and scan status |
| `files` | manifest metadata only |
| `read_file` | exactly one file, optionally bounded |
| `export` *(CLI only in the first cut)* | everything, but to a file, not to the agent's context |

Nothing in the API returns a whole package into an agent's context by
default. The reference package's own convention (§2.3) — an entrypoint that
names its references — is exactly the progressive pattern this API supports:
read the entrypoint, see `references/templates.md`, fetch that file.

**And nothing on the agent-facing surface can *add* package bytes.** There is
no import action, no filesystem path parameter, and no way for an agent to
cause a byte to be read from disk (§8.3, §7.4 B4). The progressive pattern
runs one way: an operator installs, an agent reads.

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
    skill_id          TEXT PRIMARY KEY,
    name              TEXT NOT NULL,
    version           TEXT NOT NULL,
    declared_version  TEXT,
    version_source    TEXT NOT NULL,
    format            TEXT NOT NULL DEFAULT 'mpm-skill-package/1',
    entrypoint        TEXT NOT NULL,
    entrypoint_sha256 TEXT NOT NULL,
    manifest          TEXT NOT NULL,
    package_hash      TEXT NOT NULL,
    source_sha256     TEXT,
    is_global         INTEGER NOT NULL DEFAULT 0,
    created_at        INTEGER NOT NULL DEFAULT (CAST(strftime('%s','now') AS INTEGER)),
    updated_at        INTEGER,
    FOREIGN KEY (skill_id) REFERENCES memories(id) ON DELETE CASCADE
);

-- One row per file in a package.
CREATE TABLE IF NOT EXISTS skill_package_files (
    skill_id         TEXT NOT NULL,
    path             TEXT NOT NULL,
    sha256           TEXT NOT NULL,
    size_bytes       INTEGER NOT NULL,
    content_type     TEXT NOT NULL,
    role             TEXT NOT NULL,
    executable       INTEGER NOT NULL DEFAULT 0,
    secret_scanned   INTEGER NOT NULL DEFAULT 0,
    poison_scanned   INTEGER NOT NULL DEFAULT 0,
    raw_path         TEXT,
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
* `path` is the **canonical** path — NFC-normalised, POSIX-separated,
  collision-checked — and it is the file's identity. `raw_path` is the name as
  it appeared in the source archive and is **display-only**: it never
  addresses a file and is never compared for identity (§5.4).
* `entrypoint_sha256` is the hash of the **original** entrypoint bytes, as
  supplied. `projection_sha256` lives inside the manifest JSON, because it is
  a property of the derived projection rather than of the package.
* `declared_version` and `version_source` record **how** the effective
  version was arrived at — `declared` (the file carried a `version:`) or
  `package_declares_no_version` (the pre-release default of §5.2, or an
  operator override). A row therefore always says whether its version came
  from the package or from MPM.
* `secret_scanned` and `poison_scanned` record **which scanners actually ran
  on this file** (§5.5, §7.2). A binary asset has `poison_scanned = 0` — a
  disclosed skip. A row can never claim a scan that did not happen, because an
  import that could not scan does not write a row at all.
* `ON DELETE NO ACTION` on the blob foreign key means a blob that is still
  referenced by a file row **cannot** be deleted. The garbage collector
  therefore deletes file rows first and unreferenced blobs second.
* `idx_skill_package_files_sha256` is required: a foreign key indexes the
  parent side, but the collector needs the child side to answer "which files
  reference this blob" cheaply.
* The tables are created in dependency order — blobs, packages, files — so no
  foreign key ever points at a table that does not exist yet.
* The three tables and their index live in a **local-only** DDL slice
  (`SkillPackageTables`), **not** in `BaseTables` (§6). `attachShared`
  propagates every `CREATE TABLE` in `BaseTables` into the shared schema, so a
  package table placed there would be created as `shared.skill_packages` in
  every shared database — the change §5.9 promises does not happen.
* The package tables are **not** FTS-indexed, deliberately (§12.2 **O5**).
* `is_global` on the package row mirrors the skill row, so promotion is a
  single consistent flip.

**The collector's safety depends on the import's atomicity.** A blob row and
the file row that references it are committed **together**, in the one
transaction the importer owns (§3.4 E5, §4.2, **R2**). That is what makes the
reference check sound: the collector can never observe a blob row whose
referencing file row is not yet committed, and can therefore never decide that
a blob is unreferenced while an import is in flight. A future refactor that
splits the import across two transactions would break the collector's safety
guarantee **without breaking any test that does not specifically check for
it** — which is why the dependency is stated here rather than left implicit.

---

## 10. Stages

Each stage is a separate pull request, independently testable, and green on
its own.

**The ordering rule.** No stage depends on behaviour delivered only by a
later stage. The dependency graph below is therefore written out explicitly,
and each stage's *depends on* column names only stages that precede it.

The stage order was **re-ordered in revision 2**. The first revision's order
put the read API and the export writer *before* the CLI surface, which was
fine, but it also put the **export loss marker** nowhere — and the marker is
needed in the same release that first makes a package exist (§6.3). It also
left the write primitives implicit: the first revision's "directory import"
stage was supposed to produce an atomic import, but the tx-aware write
primitive it depends on (§3.4 E5) was never given a stage of its own, which
made the atomicity claim in threat 17 unimplementable as staged.

| stage | content | depends on | acceptance |
|---|---|---|---|
| **P1** | The three tables, the **local-only** DDL slice (`SkillPackageTables` — **not** `BaseTables`, §6), the `CanonicalMPMSchema` registration, the `TestCanonicalSchemaSync` guard, and the **rollback runbook** (§6.1, §6.2) | — | A fresh database has the tables; an old database is upgraded in place; a dump containing the tables restores; the sync guard is green; the runbook names both rollbacks and states the export-before-rollback precondition; **no shared database receives the package tables** (**T50**, **C-5**) |
| **P2** | Path canonicalisation, collision detection, NFC normalisation, the manifest type, canonical JSON, `package_hash` — pure functions | — | Unit tests for every rejection rule in §7, threats 1, 4, 6, 7, 8; the canonical path is the identity and `raw_path` is display-only (§5.4) |
| **P3** | **The write primitives and the guards.** The tx-aware skill writer (a thin extraction from `SaveSkillWithInvocation` that writes the projection through `saveMemoryRow` via `txNode`), the **package-immutability guard** on the legacy force path, and the **skills exemption** from `PruneOlderThan` | P1 | The guard refuses a `save --force` against a packaged skill with `package_immutable` and writes nothing; `mpm prune --older-than` does not remove a skill that owns a package **and does not remove an ordinary skill either** — the exclusion is blanket (T41); the projection can be written inside a caller's transaction (**R1**, **R2**, **R7**, **C-2**). **The exemption's form must be confirmed by the operator first (**O17**) |
| **P4** | **Directory import**: path rules, limits enforced *during* the walk, symlinked-directory refusal, root confinement, per-file scanning with the **fail-closed** scanner check, the derived projection with the pre-release effective version, the single-transaction write, idempotence — **and the export loss marker** (§6.3) | P1, P2, P3 | The operator's package imported from an extracted directory produces the §2.5 hashes and the expected projection; a rejected file leaves nothing behind; an unreadable toxic-phrase file refuses the import with `scanner_unavailable` and writes nothing; `mpm export --collection skills` names every package it cannot carry (**R3**, **R10**, **R11**) |
| **P5** | **ZIP import**: the **root derivation** (§5.3.1 — the flat and wrapped layouts, and the `ambiguous_package_root` refusal), entry-type rejection (best-effort), path rejection, byte caps, ratio caps, CRC verification by full read | P1, P2, P3, P4 | The operator's `.skill` imports with the same `package_hash` as the directory import; **both accepted layouts produce the same hash**, and a multi-root archive is refused (**T48**); the malicious fixture set is rejected with the right reason codes; **the FAT-creator archive is accepted, its directory entry is skipped, and no byte reaches the filesystem** (**R5**, **C-3**) |
| **P6** | **The read API**: `files`, `read_file`, bounded reads with the defined `next_offset` **and its terminal case**, hash verification, the non-optional provenance object | P1, P2 | A reference read returns the exact bytes; a corrupted stored file is refused with `content_hash_mismatch`; the `read` response for a packaged skill carries `entrypoint_sha256` and `scanned`; `next_offset` names a line and never repeats or skips one; an over-long first line is reported as `line_too_large` and is **not** a resumable truncation (**R6**, **C-4**) |
| **P7** | **The export writer**: deterministic ZIP output, the round trip, export of a legacy single-document skill as a one-file package, and the **full skills export** that emits one `.skill` archive per package (§6.3) | P1, P2, P4, P5 | `package_hash` is stable across export → import; two exports of one package are byte-identical; `mpm export --collection skills` now carries every package and no longer needs the loss marker |
| **P8** | **The CLI and MCP surfaces**: the CLI subcommands, the read-only `mpm_skills` action extension (`files`, `read_file`, `verify`), the registry description, the schema-guard pins | P4, P5, P6, P7 | The documentation gate is green; the existing `save` / `read` / `list` pins still pass; the new actions are pinned by new guards; **no agent-facing action accepts a filesystem path** (**R4**) |
| **P9** | **Promotion, garbage collection and the spec**: promotion of packages, the operator-invoked blob collector with its reference check, the blobstore-isolation test, the "no byte reaches the filesystem" test, the `docs/SPEC.md` skill section, the agent-protocol note that package content is data, and the promotion of this document out of `docs/dev/` | P1, P4 | `is_global` flips on both rows; the collector never deletes a referenced blob; `mpm blob gc` leaves the package tables untouched; an import writes nothing to the filesystem; the spec describes the shipped behaviour; the library entry carries a status label |

**Why P3 exists as its own stage.** The corrections in R1, R2 and R7 are all
changes to **existing write paths**, not new features:

* R1 changes what the legacy force path does when a package row exists. That
  is a change to `SaveSkillWithInvocation`, which is used by the CLI, by the
  MCP surface and by the workshop.
* R2 creates the tx-aware write primitive the importer depends on. Extracting
  it from `SaveSkillWithInvocation` is a refactor of that function, and it
  must land — and be green — **before** the importer that uses it.
* R7 changes a hard delete that runs on a completely different code path
  (`mpm prune --older-than`).

Landing them together with the importer would have put three unrelated
behavioural changes into one pull request, each of which could regress
independently. Separated, each is a small, reviewable, independently green
change, and the importer stage can then assume them.

**Why the export loss marker is in P4 and not P7.** The marker exists because
packages exist. P4 is the first stage in which a package can exist, so P4 is
the first stage in which `mpm export --collection skills` becomes a loss
path. Shipping the loss without the marker would be a silent data-loss
release; shipping the marker a stage early would be a warning about a
condition that cannot yet occur. P4 is exactly the right point, and it does
not depend on anything from P5–P7.

**Why P8 comes after the read API.** The MCP action extension exposes
`files`, `read_file` and `verify` — all of which are P6's behaviour. P8
therefore consumes P6 and cannot be reordered before it.

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

**Entry types are detectable — but only under a Unix creator byte.** With the
correct `fs.FileMode` values, the reader reports `L---------` for a symlink,
`p---------` for a FIFO, `D---------` for a device and `d---------` for a
directory. **Rewrite the archive's creator byte from Unix to FAT/MS-DOS** —
one byte per central-directory record, which is what a ZIP written without
Unix attributes carries — and the same entries become:

```
link -rw-rw-rw-  type=----------  regular=true   → passes the type check
fifo -rw-rw-rw-  type=----------  regular=true   → passes the type check
dev  -rw-rw-rw-  type=----------  regular=true   → passes the type check
dir  -rw-rw-rw-  type=----------  regular=true   → passes the type check
```

`FileHeader.Mode()` interprets the external attributes **only** when the
creator byte says Unix. This is the measurement behind **R5** and §7.1, and it
is the reason the entry-type check is stated as best-effort and the "no byte
reaches the filesystem" rule (Rule 2, §4.4) is stated as the actual
boundary.

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

### 11.1 The corrections, and the measurement behind each

The twelve corrections are not stylistic. Each one is a statement the
repository contradicts, and each was verified against the code at
`f8d4b95a` before this revision was written.

| correction | what was measured | where |
|---|---|---|
| **R1** | The force branch replaces `memories.content` **in place** under the same id — `UPDATE memories SET content = ? … WHERE id = ?` — and it is the only in-place content writer for a skills row in the tree. `force` reaches it straight from the MCP payload. | `internal/core/skill_db.go:624-631`, `internal/core/tools/handlers.go:3758` |
| **R2** | `SaveSkillWithInvocation` cannot be composed into a caller's transaction: the new-row path owns its own `db.Begin()`/`tx.Commit()`, and the force path is a bare `db.Exec` with no transaction at all. The tx-aware primitive that *is* composable is `saveMemoryRow`, reached through the `txNode` adapter. | `internal/core/skill_db.go:652`, `735`, `624`, `730`; `internal/core/db.go:4472` |
| **R3** | `semver.IsValid("v1.0.0-unversioned")` is `true`; `semver.Compare` against `v1.0.0` is **-1** and against `v0.9.0` is **+1**. The id `skill:system-prompt-writer-v1.0.0-unversioned` round-trips to name `system-prompt-writer`, version `1.0.0-unversioned`. | `internal/core/skill.go:174-202`; §2.5, §5.2 |
| **R4** | The MCP registry contains **exactly one** path-shaped parameter in the whole tool surface — `filepath` on `mpm_references`, which names a *file* with an extension allow-list. `mpm_skills` has none. There is no path-confinement helper in the tool layer: no `confine`, `safePath`, `resolveWithin`, `ensureUnder`, and no `filepath.EvalSymlinks`. | `internal/core/tools/registry_list.go:217`, `337-376` |
| **R5** | The creator-byte measurement above: under a FAT/MS-DOS creator byte, `Mode()` reports every **non-directory** entry as `0666` regular, and a symlink, a FIFO and a device all **pass** the type check. A directory entry is still detected, because `FileHeader.Mode()` forces `fs.ModeDir` from the trailing `/` in the entry name, independently of the creator byte. | §7.1, §11 above |
| **R6** | The MCP read handler returns `"body": skill.Body` with **no** provenance envelope — no hash, no scan status. The `package` object the first revision proposed carried `entrypoint`, `file_count`, `package_hash` and `version_override`, and was optional. | `internal/core/tools/handlers.go:3794-3818` |
| **R7** | `PruneOlderThan` is `DELETE FROM memories WHERE created_at < ? AND deleted_at IS NULL AND <DirectiveExclusionClause>` — there is **no `collection` filter**. Only directives are excluded. | `internal/core/web_db.go:1781-1792` |
| **R8** | `PRAGMA user_version` appears **nowhere** in the tree: there is no schema-version gate, so an old binary opens a newer database without complaint. `mpm backup` is `sqlite3 .dump` — generic — and `restore-db` rejects any table not on `CanonicalMPMSchema`. | `internal/core/sql_dump_validator.go:530+`; §6.2 |
| **R9** | The two operations are materially different: the binary rollback leaves the rows in the file, the schema rollback destroys them. The first revision used one word for both. | §6.1 |
| **R10** | `isPoisonedAt` returns `(false, "")` when `loadPoisonPhrasesAt` fails, so an unreadable phrase file makes the poison scan **pass everything**. The secret scanner is 30 in-process regexes and cannot fail this way. | `internal/core/memory.go:845-851` |
| **R11** | The first revision contradicted itself: the compatibility matrix said the package metadata is not exported, while **O8** recommended carrying it "as a follow-up after P6". | §6.3 |
| **R12** | The directory path has no archive to bound it: a directory holding ten million entries would be fully walked, `Lstat`'d and measured before the first rejection, unless the caps are enforced in flight. | §7, threats 11 and 19 |

**Two measurements are deliberately *not* claimed.** The first revision's
rationale note for `1.0.0` was a **comment** in a test header, not a
constraint, and is withdrawn in §5.2. And no acceptance criterion in this
document depends on the ZIP entry-type check being available — T35 exists
specifically to prove that (§13).

### 11.2 The C-1–C-5 measurements

The five corrections of the second independent review were each verified
against the substrate at `f8d4b95a` before this revision was written, with
the same discipline as §11.1: a disposable probe, a measurement, and the
measurement quoted rather than asserted.

| correction | what was measured | where |
|---|---|---|
| **C-1** | The reference package's canonical paths are `SKILL.md`, `references/templates.md`, `references/principles.md`. Their distinct top-level components are **2** (`SKILL.md` and `references`) — so the export's own output does **not** share one top-level directory, and the first revision's rule rejected it. The operator's archive (`system-prompt-writer/…`) is the wrapped form and strips to the same three canonical paths, so the two layouts are hash-equivalent. | §5.3.1, §5.6, §16 C-1 |
| **C-2** | `PruneOlderThan` is `DELETE FROM memories WHERE created_at < ? AND deleted_at IS NULL AND <DirectiveExclusionClause>` — **no `collection` filter** (`internal/core/web_db.go:1781-1792`). The exclusion the design specifies is therefore a **statement-level** clause, and it excludes **every** skill row, not only the packaged ones. | §6.4, §12.2 **O17**, §16 C-2 |
| **C-3** | The same archive under a Unix creator byte and under a FAT creator byte (the central-directory creator byte rewritten): `pkg/link.md`, `pkg/fifo` and `pkg/dev` become `type=---------- regular=true` under FAT — they **pass** the type check. `pkg/sub/` remains `type=d--------- dir=true` under FAT. Two further variants isolate the signals: with the MS-DOS directory attribute **cleared** and the trailing `/` kept, `pkg/sub/` is still `type=d--------- dir=true`; with the attribute **set** on a name with no trailing slash, `pkg/nodir` is `type=d--------- dir=true`. Go's source: `archive/zip/struct.go:317-328` applies `mode \|= fs.ModeDir` for a trailing `/` **after** the creator switch, and `msdosModeToFileMode` (`:348-358`) reads only the MS-DOS attribute byte. | §7.1, §16 C-3 |
| **C-4** | §8.6's two bullets, taken together, define `(lines: [], truncated: true, next_offset: offset)` for a first line larger than the byte cap, on every call, with the section's own resume rule (`offset = next_offset`) recommending the loop. The byte cap applies *after* the line range is selected, so it cannot select a different range. | §8.6, §16 C-4 |
| **C-5** | A `DatabaseManager` with a shared database attached, listing `shared.sqlite_master`: the shared schema has **35** tables; `reference_docs` and `reference_chunks` are **absent** from it, while `memories` and `sessions` are present. `reference_docs` (`internal/core/schema.go:953`) and `reference_chunks` (`:1002`) are in **`ReferenceTables`** (`:949-1047`), not `BaseTables` (`:38-936`); `attachShared` iterates `BaseTables` only (`internal/core/db.go:2132-2143`, `:2158-2167`), and the repository says so at `internal/core/db.go:2318`. | §6, §5.9, §16 C-5 |

---

## 12. Operator decisions

The decisions are split by **status**, not by topic. §12.1 records the
decisions this revision **resolves** — each with its rationale, the evidence
behind it, and the correction that settled it. §12.2 records the choices that
**still require the operator's answer**.

### 12.1 Resolved architectural decisions

These are settled by this revision. They are recorded here rather than left
implicit in the prose so that a reader can see exactly what was decided, on
what evidence, and by which correction.

| | decision | resolution | rationale and evidence | settled by |
|---|---|---|---|---|
| **O1** | The effective version for a package that declares none | **`1.0.0-unversioned`**, overridable with `--version` | A pre-release sorts **below** the genuine `1.0.0` and **above** every older genuine release (`semver.Compare` = -1 against `v1.0.0`, +1 against `v0.9.0`), so a later genuine release can be installed alongside it without a force-overwrite, and the package stays discoverable by name. The id round-trips. The `1.0.0` default is withdrawn: it collides with a later genuine `1.0.0` of the same package. `0.0.0` is withdrawn: its only stated objection was discoverability, which the pre-release form solves, and the constraint it was justified against turned out to be a comment in a test header rather than a rule. | **R3**, §5.2 |
| **O2** | May a published package be replaced in place by a force-overwrite? | **No.** The legacy `save --force` against a packaged skill is **refused** with `package_immutable`, and nothing is written | A published package has one projection, one manifest and one set of file rows; an in-place rewrite would have to reproduce the import's scanner, hashing, manifest and version logic in a second code path, or leave the manifest describing the previous document. Refusal is smaller, has one behaviour, and is consistent with §5.7. The divergence from today's `save --force` is deliberate and stated. | **R1**, §5.8.1 |
| **O7** | Should `read` gain a `package` object? | **Yes, and it is not optional for a packaged skill.** It carries `entrypoint_sha256` and `scanned` | The `read` action inlines the entrypoint body — the one piece of package content most likely to carry an injection — and it came back with nothing an agent could check. Making the object present-when-packaged and carrying the hash and the scan status gives every response that carries package content machine-checkable provenance, while leaving every non-packaged skill's response byte-identical to today's. | **R6**, §8.2.1 |
| **O8** | Does `mpm export --collection skills` carry the package? | **It must not lose it silently.** Before full package export exists, the export **names** every package it cannot carry; stage P7 then makes the export complete | The first revision contradicted itself on this row, and the matrix's version — the one an implementer would have built — is a silent data-loss path for content that exists nowhere else. The marker is the contract until the export is complete. | **R11**, §6.3 |
| **O11** | Does the agent-facing surface get an `import` action in the first cut? | **No.** Import is **CLI-only**; the agent-facing package surface is read-only (`files`, `read_file`, `verify`) | An agent-facing `import` would be the first action in the tool surface to accept a path that may name a directory, with no confinement helper anywhere in the tool layer. It would be a read primitive, an amplifier (durable, and replicated into other agents' contexts) and a persistence mechanism in one action. Nothing in the design's motivation requires an agent to *create* packages. | **R4**, §8.3 |
| **O12** | If the toxic-phrase file is unreadable, does the import refuse or warn? | **Refuse**, with `scanner_unavailable`, and write nothing | A warning would let an import report success on content never checked against the one scanner that looks for prompt injection. The secret scanner cannot fail open; the poison scanner can, because it reads an operator-editable file. The refusal costs one command; the alternative costs a silent hole. | **R10**, §7.2 |
| **O13** | May `mpm prune --older-than` remove a skill that owns a package? | **No.** A prune must never destroy a packaged skill's bytes. This is a settled **safety invariant**, and this revision does not reopen it. **The mechanism** that enforces it — a blanket `collection = 'skills'` exclusion versus a package-aware check — is a separate and still-open choice, because the two have very different blast radii; see **O17** (§12.2) | The prune is a **collection-blind hard delete** with no `collection` filter, and a package's bytes have no second copy. The exclusion mirrors the existing directive exemption. A deliberate hard shred still cascades — what is removed is unintentional bulk destruction of third-party content. §6.4 specifies the blanket form as the implementation to build and states its blast radius; the operator's confirmation of that blast radius is outstanding (**O17**), so the invariant is settled while the mechanism is not. | **R7**, §6.4, **C-2** |

### 12.2 Open operator decisions

These are **not** settled by this revision. Each has a recommendation and a
cost, and each needs the operator's answer before the stage that implements it
begins.

| | decision | recommendation | cost of the alternative |
|---|---|---|---|
| **O3** | Are binary assets stored by default? | Reject by default; `--allow-binary` opts in | Storing them by default puts unscannable bytes in the database for a collection whose value is textual. |
| **O4** | Cross-workspace sharing: extend the shared database, or use the archive as the transport? | The archive. No shared-schema change in the first cut | Extending the shared database means adding the three tables to the shared schema, a migration on every shared database, and deciding whether the shared copy is a reference or a copy. |
| **O5** | Should package files be searchable through FTS? | No. FTS stays on the entrypoint only | Yes makes `mpm skill search` find a phrase buried in a reference, but every reference file becomes an indexed memory row — the Option A problem (§4.1). |
| **O6** | Are the size caps right — 512 files, 32 MiB total, 4 MiB per file, 256 KiB entrypoint? | Adopt as defaults; make them constants | Larger caps slow `ListSkills`, which parses every skill row's full content. |
| **O9** | Package layouts: a single root directory, or files at the archive's top level? | Accept exactly one — the **flat** layout (the canonical paths at the archive's top level) **or** the **wrapped** layout (one top-level directory holding everything). Multiple roots are rejected with `ambiguous_package_root`. **This must be answered before P2/P5** | Accepting multiple roots needs a rule for which file is the entrypoint, which is guesswork. **The flat layout is not optional**: it is the form MPM's own export writer produces (§5.6), so refusing it would refuse MPM's own archives and break the round trip (T07, T11) and the archive-as-transport that **O4** nominates. §5.3.1 specifies the derivation. |
| **O10** | The operator's package has no `when_to_use`. MPM's validator emits `missing_when_to_use` as a **warning**, so the import succeeds with a warning. Should a package import require it? | Warn, do not block — but the operator should decide, because requiring it would reject the reference package | Requiring it would enforce discovery quality, at the cost of rejecting legitimate third-party packages that use a prose description instead |
| **O14** | Should a *later* stage add an agent-facing import, and if so, confined to what root and with what consent gate? | **Defer.** If it is ever added, confine it to an operator-configured import root and reuse the `confirm=true` gate that `promote_to_global` already uses, rather than inventing a new convention | Adding it sooner re-opens the **R4** finding on the agent-facing surface. Nothing in stages P1–P9 depends on this answer. |
| **O15** | Where does the full skills export put its per-package `.skill` archives (§6.3, stage P7)? | A sibling directory named for the export, one archive per package, named `<name>-<version>.skill` | A single flat archive would need a multi-package container format, which is a new format with its own identity and import rules — larger than the correction requires. |
| **O16** | Should the `ListSkills` read path be refactored so it no longer parses frontmatter out of every skill row's full content? | **Out of scope for this design.** The 256 KiB entrypoint cap (O6) stands as the mitigation | The refactor would remove the cap's *reason* and make package entrypoints cheap regardless of size. It touches a hot read path used by discovery, the wake context and `mpm info`, and it is not required by any correction in this revision. It is recorded here so it is not lost. |
| **O17** | **The prune exemption's form: a blanket `collection = 'skills'` exclusion, or a package-aware exclusion that spares ordinary skills?** | **Blanket.** Exclude `collection = 'skills'` from `PruneOlderThan` in one clause, mirroring the existing directive exemption. §6.4 specifies this as the implementation to build, and T41 asserts it | The alternative — `AND NOT EXISTS (SELECT 1 FROM skill_packages …)` — would preserve the operator's age-based pruning for ordinary skills, at the cost of a per-row check inside a bulk delete (which leaves a race between the check and the delete) and a larger change to a statement that today carries exactly one exclusion clause. **The blanket form has its own cost: `mpm prune --older-than` can no longer remove *any* skill, packaged or not** (§6.4, §6 matrix) — a behaviour change for every existing skill, and a silent one. **This must be answered before P3**, the stage that changes the prune statement. This revision states the recommendation and the cost; it does **not** record an operator approval that has not been given. |

---

## 13. Regression matrix

Every row is an executable acceptance test. The stage that delivers it is
named.

| # | scenario | expected | stage |
|---|---|---|---|
| **T01** | An existing single-document skill, unchanged | Save, read and list behave exactly as today; no package row is created | P1 |
| **T02** | Import a legacy single-document skill as a package | One file row; the projection equals the original bytes; `projection_sha256 == entrypoint_sha256` | P4 |
| **T03** | Directory import of the reference package | Three file rows with the §2 hashes; the entrypoint is `SKILL.md` | P4 |
| **T04** | ZIP import of the reference package | Identical result to T03, including `package_hash` | P5 |
| **T05** | A package whose entrypoint declares no version | The effective version is **`1.0.0-unversioned`**; `version_override` is recorded; **the stored `SKILL.md` bytes are unchanged**; the projection carries `version: 1.0.0-unversioned` | P4 |
| **T06** | A relative reference read | `read_file` with `path=references/templates.md` returns the exact bytes; the hash matches; no other file is returned | P6 |
| **T07** | Identical-content round trip | export → import preserves `package_hash` and every per-file hash; two exports are byte-identical; the export's **flat** layout is **accepted** by the importer (§5.3.1) | P7 |
| **T08** | Atomic failure — a secret in the third file | Zero package rows, zero blob rows, zero `memories` rows; the database is byte-identical to before | P4 |
| **T09** | Backup and restore | A restored database has the same package bytes, hashes, manifest and `memories` projection | P1 |
| **T10** | Cross-agent sharing — promotion | `is_global=1` on both the skill row and the package row; the file rows are shared by reference, not copied; `ListSkills` with the shared scope shows the skill | P9 |
| **T11** | Cross-workspace sharing | Export from one workspace, import into another; the `package_hash` matches. The emitted `.skill` archive is in the **flat** layout, which §5.3.1 accepts, so the archive-as-transport of **O4** works | P7 |
| **T12** | Version upgrade | v1.0.0 and v1.1.0 both exist; `is_latest` moves to v1.1.0; the v1.0.0 files are intact | P4 |
| **T13** | Malicious: ZIP slip | An entry named `../../etc/passwd` is rejected with `path_traversal`; nothing is written | P5 |
| **T14** | Malicious: absolute path | An entry named `/etc/passwd` is rejected with `absolute_path` | P5 |
| **T15** | Malicious: Windows path | An entry named `C:\Windows\x.dll` is rejected — a Linux-only absolute check would miss this, so the test is specific | P5 |
| **T16** | Malicious: symlink entry — **Unix-attribute archive** | An entry carrying the symlink mode bit is rejected with `unsupported_entry_type` | P5 |
| **T17** | Malicious: duplicate path | Two entries with the same name are rejected with `duplicate_path` | P5 |
| **T18** | Malicious: case collision | `Ref.md` and `ref.md` are rejected with `path_case_collision` | P5 |
| **T19** | Malicious: Unicode collision | NFC and NFD forms of one name are rejected with `path_unicode_collision` | P5 |
| **T20** | Malicious: ZIP bomb | A 64 MiB zero entry is rejected with `size_limit`; the process's memory stays bounded | P5 |
| **T21** | Malicious: lying header | A declared size of 10 against a 200 000-byte payload is rejected with `archive_invalid` | P5 |
| **T22** | Malicious: CRC corruption | A flipped content byte is rejected with `checksum_mismatch` — which requires the importer to read to EOF | P5 |
| **T23** | Malicious: a secret in a reference file | An API key in `references/x.md` is rejected with `scanner_secret`, and the whole import rolls back | P4 |
| **T24** | Malicious: a poison phrase in a reference file | Rejected with `scanner_poison` | P4 |
| **T25** | Malicious: an executable payload | A script with the executable bit is stored, flagged `executable: true`, and **never executed**; no code path connects package content to execution | P4 |
| **T26** | Malformed archive | A truncated or non-ZIP file is rejected with `archive_invalid`; no partial state | P5 |
| **T27** | Idempotence | Importing the same package twice reports `already_installed` the second time and creates no new rows | P4 |
| **T28** | A different version of the same name | Both exist; the latest flips | P4 |
| **T29** | The same version with different content | Rejected with `version_conflict`; there is **no** force escape | P4 |
| **T30** | Shredding | Soft shred leaves the package recoverable; hard shred removes the package and file rows, and the blob becomes a collection candidate | P9 |
| **T31** | Blobstore isolation | `mpm blob gc` and `mpm ops gc` leave the package tables and the blob rows untouched | P9 |
| **T32** | Discovery weight | With 200 skills each carrying a package, `list` and the wake context contain no package byte, and the `available_skills` shedding behaviour is unchanged | P8 |
| **T33** | Bounded read | `read_file` with a byte cap returns no more than the cap, sets `truncated: true`, and returns a `next_offset` | P6 |
| **T34** | A corrupted stored file | Mutating a blob row's bytes makes `read_file` refuse with `content_hash_mismatch` | P6 |

### 13.1 The corrections' acceptance rows

Rows **T35–T50** exist because the corrections make claims that the original
matrix did not test. Each one is a statement the first revision made that the
repository contradicts, so each has a row.

| # | scenario | expected | stage | correction |
|---|---|---|---|---|
| **T35** | **Malicious: a FAT/MS-DOS creator byte** — the same symlink, FIFO and device entries **and a directory entry**, with the central-directory creator byte rewritten from Unix to FAT | The symlink, FIFO and device entries are **accepted** as regular files — the type check is not available for them — and the import **succeeds**, because **no byte of the package reaches the filesystem** (Rule 2). The **directory** entry is **detected as a directory and skipped** under both creator bytes, because the trailing `/` in its name identifies it. The test asserts all three halves: the import did not fail on the entry type, the directory entry was not stored, and no file was created anywhere | P5 | **R5**, §7.1, **C-3** |
| **T36** | **The legacy write path against a packaged skill** | `save --force` — CLI **and** MCP — is refused with `package_immutable`; the `memories` row, the manifest, the file rows and both hashes are **unchanged**; the `read` body and the `read_file` entrypoint bytes still agree | P3 | **R1**, §5.8.1 |
| **T37** | **Atomicity across the whole import** | A failure injected after the blob rows are written and before the commit leaves **zero** rows of every kind, and no blob row is visible to the collector | P3, P4 | **R2**, §3.4 E5 |
| **T38** | **A versionless package and a later genuine `1.0.0` of the same name** | Both import successfully; the ids are `…-v1.0.0-unversioned` and `…-v1.0.0`; `is_latest` points at the genuine `1.0.0`; the versionless package's files are intact | P4 | **R3**, §5.2 |
| **T39** | **The agent-facing surface** | The `mpm_skills` registry carries **no** action that accepts a filesystem path; there is no import action; `files`, `read_file` and `verify` take only a skill id or a name and version | P8 | **R4**, §8.3 |
| **T40** | **The provenance on `read`** | The `read` response for a packaged skill **always** carries the `package` object with `entrypoint_sha256` and `scanned`; for a skill that is not a package the object is **absent** and every other field is byte-identical to today's response | P6 | **R6**, §8.2.1 |
| **T41** | **Age-based prune against a packaged skill** | `mpm prune --older-than 1s` leaves the packaged skill and its package row intact. It **also** leaves an ordinary, non-packaged skill of the same age in place — the exclusion is **blanket**, so the prune cannot tell the two apart. The test asserts **both** halves: the packaged skill survives, **and** an ordinary skill of the same age is **not** removed | P3 | **R7**, §6.4, **C-2**, **O17** |
| **T42** | **The restore allow-list is not weakened** | A dump naming the three package tables is still **rejected** by a binary whose `CanonicalMPMSchema` does not list them — the refusal is unchanged and loud | P1 | **R8**, §6.2 |
| **T43** | **The scanner cannot be skipped** | With the toxic-phrase file made unreadable, the import is refused with `scanner_unavailable` and **nothing is written**; the response does not report success, and no row records a scan that did not happen | P4 | **R10**, §7.2 |
| **T44** | **The export never loses a package silently** | Before full package export exists, `mpm export --collection skills` **names** every package it did not carry, in the response and on standard output; after P7 it carries every package and the marker is gone | P4, P7 | **R11**, §6.3 |
| **T45** | **The walk's limits are enforced in flight** | A directory holding more than 512 files is refused **during** the walk — the walk stops at the 513th entry rather than enumerating the tree — and the total-byte cap is checked as bytes are read | P4 | **R12**, §7 threat 11 |
| **T46** | **The walk refuses to leave the import root** | A symlinked directory inside the import root is **not descended into**, and a symlink pointing outside the resolved root is refused; the refusal is `symlinked_directory` or `path_escapes_root` | P4 | **R12**, §7 threat 19 |
| **T47** | **The bounded read's `next_offset`** | `next_offset` names a **line** index; resuming from it returns the next unread line with no gap and no repeat; a line cut by the byte cap is not partially returned; and every **non-terminal** response advances the offset (`next_offset > offset`) | P6 | O-b, §8.6 |
| **T48** | **Both accepted package layouts** | The same package imported from a **flat** archive (MPM's own export — `SKILL.md`, `references/…` at the top level) and from a **wrapped** archive (`pkg/SKILL.md`, `pkg/references/…`) produces the **same canonical paths, the same file rows and the same `package_hash`**; only `raw_path` differs. An archive with **two** top-level directories is rejected with `ambiguous_package_root`. The wrapped root is stripped **by whole segment**, so `pkg-extra/x.md` is not stripped against a root of `pkg` | P5 | **C-1**, §5.3.1 |
| **T49** | **The bounded read's terminal case** | A file whose first line alone exceeds the byte cap returns **zero** lines, `truncated: true`, `next_offset` equal to the requested `offset`, and the reason code **`line_too_large`**. The caller must report that the file cannot be served at this cap and **must not resume** — and the test asserts that resuming would return the same empty response, so the refusal is the only way out. No response ever carries `truncated: true` with `next_offset == offset` and a different reason | P6 | **C-4**, §8.6 |
| **T50** | **The package tables are local-only** | With a shared database attached, the local schema has `skill_packages`, `skill_package_files` and `skill_package_blobs`, and the shared schema has **none** of them: `shared.sqlite_master` names no `skill_packages`, no `skill_package_files` and no `skill_package_blobs`. The test also asserts that none of the three appears in `BaseTables` — the slice `attachShared` propagates — and that the shared schema's table set is identical to the set a database without packages would have | P1 | **C-5**, §6, §5.9 |

**One row is pinned to an open decision.** T41 asserts the **blanket** form of
the prune exemption, because that is the form §6.4 specifies. If the operator
answers **O17** (§12.2) with the package-aware form, T41's second clause
changes and no other row moves. The row is written against the specification
as it stands, and the specification as it stands says which clause to change.

### 13.2 Fixture strategy

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
* **The creator-byte pair.** The same hostile archive generated **twice**: once
  with Unix attributes and once with the central-directory creator byte
  rewritten to FAT/MS-DOS. The generator must write the archive with
  `zip.Writer` and then patch the byte, because `zip.Writer` **overwrites**
  `CreatorVersion`, `ExternalAttrs` and the size fields — hand-rolling them
  proves nothing. This pair is what makes T16 and T35 meaningful rather than
  vacuous (§7.1). The archive must carry a **directory entry** — a name
  ending in `/` — so that T35 can assert the directory is detected and
  skipped under **both** creator bytes, and a **non-directory** entry whose
  name carries no trailing slash, so the test can show that the trailing `/`
  and the MS-DOS attribute are independent signals.
* **The symlinked-directory fixture.** A directory tree containing a symlink to
  a directory inside the root and a symlink to a directory outside it, for
  T46. The fixture is built on disk by the test, not committed as a symlink,
  so it survives a checkout that does not preserve symlinks.
* **The unreadable-scanner fixture.** A toxic-phrase file that cannot be
  read, for T43. It is created by the test at a path the test then makes
  unreadable — never by committing an unreadable file.
* **Data-driven expectations.** Each fixture carries a manifest describing the
  expected outcome — the reason code, and whether anything at all was written
  — so the table above is data rather than fifty hand-written assertions.
* **Isolation.** Every test uses `t.TempDir()` databases, a real SQLite file,
  a real ZIP on disk, no network, and no live MPM state.

---

## 14. Summary of the proposal

| | |
|---|---|
| **Architecture** | Three new additive SQLite tables. No existing table, row, index or trigger is modified. |
| **Persistence** | A manifest per package version, one row per file, and content-addressed blob rows for the bytes. Referential integrity by foreign key, atomic import by transaction, deduplication by content hash. |
| **Hard rules** | **Rule 1:** no package byte ever enters the blobstore — its 24-hour TTL and its referential-check-free GC would delete permanent skill assets silently. **Rule 2:** an import never materialises package bytes as a file — the bytes are a database column, which is what makes the ZIP entry-type check's limitation harmless rather than surprising. The only file package bytes reach is the `.skill` archive an operator explicitly asks for. |
| **Compatibility** | Existing single-document skills are untouched. The `read` action keeps today's fields and gains a `package` object **only** for a packaged skill; `list` and `SkillSummary` are not modified; discovery carries no package byte. |
| **Immutability** | A published package is never replaced in place, by any path. A legacy `save --force` against a packaged skill is **refused** with `package_immutable` and writes nothing. |
| **Third-party integrity** | The operator's `SKILL.md` is stored byte-exact. The `memories` projection is a separate, labelled derivation that adds a `version` line when the original lacks one, with both hashes recorded. A versionless package's effective version is `1.0.0-unversioned`, so a later genuine `1.0.0` installs alongside it. |
| **Security** | Every file is path-canonicalised, bounded and scanned — per file, which is the control that today covers only the entrypoint. The scan is **fail-closed**: an import that cannot scan writes nothing. The **agent-facing surface cannot create packages**; import is operator-driven. Importing never executes anything. |
| **Retrieval** | Progressive: discovery reads nothing, `read` reads the entrypoint plus its hash and scan status, `files` reads metadata, `read_file` reads one bounded file. |
| **Lifecycle** | A packaged skill is not removed by an age-based prune. The exemption is **blanket**, so once it lands `mpm prune --older-than` removes **no** skill, packaged or not — the operator's confirmation of that blast radius is **O17** (§12.2), outstanding. A binary rollback (lossless, blind) and a schema rollback (destructive) are separate operations, and a binary rollback makes `restore-db` of a new-binary backup fail closed — so packages are exported before a rollback. |
| **Decisions** | Seven resolved in §12.1, with their rationale and evidence; ten still open in §12.2, with a recommendation and a cost for each. |
| **Corrections** | Twelve required corrections (R1–R12) are dispositioned in §15; five (C-1–C-5) are dispositioned in §16. |

---

## 15. The R1–R12 disposition matrix

One row per required correction. Each row states the **original problem**, the
**revised specification section**, the **resolution**, the **supporting
evidence**, and the **remaining risk**.

The corrections were all verified against the code at `f8d4b95a` before this
revision was written; the evidence column cites the measurement, not the
reviewer's assertion of it. §11.1 is the evidence table; this is the
disposition.

| # | original problem | revised section | resolution | supporting evidence | remaining risk |
|---|---|---|---|---|---|
| **R1** | §5.8 asserted that "a later `save --force` resurrects and replaces" a packaged skill's projection. The legacy force path replaces `memories.content` **in place** under the same id and knows nothing about package rows, so the projection and the manifest would silently diverge: `read` would return the new body while `read_file` returned the old bytes, and `projection_sha256` would become false. | §5.8.1 (new), §5.7, §3.4 E6, §3.3 C10, §6 matrix, §12.1 **O2** | **Fail closed.** When a `skill_packages` row exists for the id, a legacy `save --force` — CLI and MCP — is **refused** with `package_immutable`, before any write. Nothing is written, so the id, the manifest, the file rows and the hashes all keep describing the same document. A published package is never replaced in place by any path. | The force branch is `UPDATE memories SET content = ? … WHERE id = ?` at `internal/core/skill_db.go:624-631`; it is the **only** in-place content writer for a skills row in the tree; `force` reaches it straight from the MCP payload at `internal/core/tools/handlers.go:3758`; `SaveSkillAndDeprecatePrior` touches `metadata` only and the workshop passes `force=false`. | **Deliberate divergence:** `save --force` on a packaged skill no longer behaves as it does for a legacy skill. An operator who relied on it will now get a refusal and must publish a new version. This is stated in §5.8.1 rather than discovered at runtime. The guard must also be applied to *every* future in-place writer of a skills row; the design names the one that exists today, and a code comment should point at the package tables so a later writer does not add a second. |
| **R2** | §4.2 and threat 17 claimed "one transaction covers the manifest row, the file rows, the blob rows, the `memories` projection and the `is_latest` flip", while §3.4 E5 named `SaveSkillWithInvocation` as the write path. That method cannot be composed into a caller's transaction, so either the import was not atomic or E5 was false — the two could not both hold. | §3.4 E5, §4.2, §7 threat 17, §9 note, §10 P3, §13 T37 | **E5 now names the tx-aware primitive.** The importer **owns** the transaction and writes the projection through `saveMemoryRow` reached via the `txNode` adapter. A thin tx-aware skill writer may be extracted from `SaveSkillWithInvocation`, but the transaction belongs to the importer. The collector's safety is stated as depending on this (§9). | `SaveSkillWithInvocation`'s new-row path opens and commits **its own** transaction (`db.Begin()` at `internal/core/skill_db.go:652`, `tx.Commit()` at `:735`); its force path is a bare `db.Exec` with **no transaction at all** (`:624`). The composable primitive is `saveMemoryRow` via `txNode` (`:730`). | The extraction is a refactor of a function used by the CLI, the MCP surface and the workshop. It is isolated in its own stage (P3) so it can be reviewed and can regress independently. A future refactor that splits the import across two transactions would break the collector's safety **without breaking any test that does not specifically check for it** — which is why the dependency is stated in §9 rather than left implicit. |
| **R3** | §5.2's effective version `1.0.0` collides with a later **genuine** `1.0.0` of the same package. The import would be rejected as `version_conflict`, semver cannot order two `1.0.0`s, and the only escape would be a force-overwrite of a published id — the very mutation R1 forbids. The stated justification was also mis-cited: a comment in a test header was presented as a hard constraint. | §5.2, §2.5, §5.5, §5.7, §12.1 **O1**, §13 T05, T38 | **The effective version is `1.0.0-unversioned`**, a semver pre-release. It sorts **below** a genuine `1.0.0` and **above** every older genuine release, so both can coexist and the genuine release becomes `is_latest`. Explicit `--version` overrides are retained. The `1.0.0` and `0.0.0` options are withdrawn and the mis-citation is corrected in place. | `semver.IsValid("v1.0.0-unversioned")` is `true`; `semver.Compare` is **-1** against `v1.0.0` and **+1** against `v0.9.0`. The id `skill:system-prompt-writer-v1.0.0-unversioned` round-trips to name `system-prompt-writer`, version `1.0.0-unversioned`. `validateSkillNameAndVersion` (`internal/core/skill.go:181-203`) rejects only an empty version, a colon, whitespace and `-v` in the *name* — nothing forbids a pre-release suffix. | The version string is slightly awkward in an id, and an operator who reads `1.0.0-unversioned` as "a version below 1.0.0" is right. It is a **prerelease**, so a package that declares `1.0.0-rc1` would be ordered **above** it — an unlikely but real ordering oddity. The `--version` override exists for the operator who does not want the default. |
| **R4** | §8.3 gave the MCP surface an `import` action with a required `path` parameter. It would have been the first action in the whole tool surface to accept a path naming a **directory**, with no confinement, no allow-listed root and no consent gate — an agent could have imported up to 512 files / 32 MiB from anywhere the MPM process can read, stored them durably, and surfaced them to other agents. | §8.3 (rewritten), §8.1, §8.7, §7.4 B4, §3.3 C13, §12.1 **O11**, §12.2 **O14**, §13 T39 | **Import is CLI-only in the first cut.** The agent-facing package surface is read-only — `files`, `read_file` and `verify` — and no agent-facing action accepts a filesystem path. The CLI keeps an arbitrary path because the CLI *is* the operator, and the CLI's directory walk still uses `Lstat`, refuses symlinked directories and refuses to leave the resolved root. | The MCP registry's **only** path-shaped parameter is `filepath` on `mpm_references` (`internal/core/tools/registry_list.go:217`), which names a *file* with an extension allow-list; `mpm_skills` has none (`registry_list.go:337-376`). There is **no** path-confinement helper in the tool layer — no `confine`, `safePath`, `resolveWithin`, `ensureUnder`, and no `filepath.EvalSymlinks`. | The operator loses the ability to have an agent install a package. That is the intended trade, and the deferred question (whether a later stage adds a **confined** import with a consent gate) is recorded as **O14** rather than answered here. If a future stage adds an agent-facing import, it must re-derive the confinement from scratch — the current design deliberately does not build that machinery. |
| **R5** | §7 threat 3 stated the ZIP entry-type rejection unconditionally. `zip.File.Mode()` interprets the external attributes **only** when the archive's creator byte says Unix, so in a FAT/MS-DOS archive a symlink, a FIFO and a device all **pass** the type check as regular files. The design's stated defence was the wrong one. | §4.4 Rule 2 (new), §7 threat 3, §7.1 (new), §7.4, §13 T16, T35 | **The real invariant is stated first:** an import never materialises package bytes as a file, so an entry of any type cannot become a filesystem object. The entry-type check is retained as a **secondary, best-effort** guard, explicitly conditional on Unix attributes. The directory path's guarantee is stated separately — `os.Lstat` is reliable. | Under a Unix creator byte: `link` → `L---------`, `fifo` → `p---------`, `dev` → `D---------`. With the creator byte rewritten to FAT: all three become `-rw-rw-rw-` with `type=----------` and `regular=true` — they **pass** the type check. `FileHeader.Mode()` reads the external attributes only for a Unix creator — **but it forces `fs.ModeDir` from a trailing `/` in the entry name unconditionally**, so a directory entry is detected under both creator bytes and is skipped. Measured with the MS-DOS directory attribute cleared and, separately, set on a name with no trailing slash; both leave the entry classified as a directory (§7.1). | The type check is available for exactly the archives an attacker is most likely to write *with* Unix attributes. The protection therefore rests entirely on Rule 2, and Rule 2 must hold for **both** import paths. T35 exists specifically to prove the FAT case is safe, and P9 asserts that an import writes nothing to the filesystem. A future feature that materialises package bytes on disk during an import would void the whole argument — which is why Rule 2 is stated as a hard rule beside the blobstore rule rather than as a note in the threat table. |
| **R6** | §7 threat 15 claimed "every read carries a provenance envelope … with its hash and its scanned status". The `read` action returns the entrypoint body **inlined**, and the optional `package` object carried no per-file hash and no scan status — so the content most likely to carry an injection came back with nothing checkable. | §8.2.1 (new), §8.2, §8.5, §5.5, §9 schema columns, §12.1 **O7**, §13 T40 | **The `package` object is present whenever the skill is a package** and carries `entrypoint_sha256` and `scanned`. The manifest records per-file `secret_scanned` / `poison_scanned`, and the `files` and `read_file` responses carry the file's own status. A skill that is not a package has **no** object, so today's response is byte-identical. The honest limit is stated: this is machine-checkable provenance on every response, not a textual banner in the prose body. | The MCP read handler returns `"body": skill.Body` with no envelope at all (`internal/core/tools/handlers.go:3794-3818`). The first revision's proposed object carried `entrypoint`, `file_count`, `package_hash` and `version_override` — no hash, no scan status. | The poison scanner remains a backstop against a fixed twenty-phrase list, and an oblique injection still passes it. The provenance tells an agent *what* it is reading and *whether it was scanned*; it does not tell the agent the content is safe. The textual banner is still absent from the body, which means a human reading a transcript sees no marker. This is stated rather than glossed. |
| **R7** | §3.2's prune analysis covered only the `expires_at` path. `mpm prune --older-than` is a **collection-blind hard delete** — no `collection` filter, only directives excluded — so it removes skill rows of any age, the cascade takes the package rows with them, and the third-party bytes are then gone: they exist nowhere else. | §3.2, §6.4 (new), §3.3 C15, §12.1 **O13**, §13 T41 | **`collection = 'skills'` is excluded from `PruneOlderThan`**, mirroring the existing directive exemption. `PruneNeverAccessed` and the default `PruneExpired` are already safe and are not changed. A deliberate hard shred still cascades — what is removed is unintentional bulk destruction. | `PruneOlderThan` is `DELETE FROM memories WHERE created_at < ? AND deleted_at IS NULL AND <DirectiveExclusionClause>` — **no `collection` filter** (`internal/core/web_db.go:1781-1792`). `PruneNeverAccessed` requires `is_long_term = 0` and skills are written `is_long_term = 1`; the default prune is `PruneExpired` and skills have `expires_at` NULL. | The exemption is a **blanket** one: a packaged skill can no longer be removed by age, and an operator with many legacy skills must remove them deliberately (shred, or export-then-delete) — which is more work for them. The blanket form is also wider than "packages": it stops `mpm prune --older-than` from removing **any** skill, packaged or not (§6.4, §6 matrix). The alternative (a per-row check inside a bulk delete) was rejected as larger and racy. **O13** settles the invariant — a prune must not destroy package bytes — and the operator's answer on the exemption's **form** is **O17** (§12.2), which must be given before P3; this revision states the recommendation and the cost and does **not** record an approval. |
| **R8** | §4.2's "an old binary ignores the tables" is true for *operation* and false for *restore*. `mpm backup` is a generic `sqlite3 .dump`, so an old binary's backup **contains** the package tables, and the old binary's `restore-db` rejects any table not on its allow-list. The document implied rollback was uneventful. | §6.2 (new), §6.1, §3.1, §10 P1 runbook, §13 T42 | **Stated explicitly, and the allow-list is not weakened.** After a binary rollback, `mpm restore-db` of a new-binary backup **fails closed**. The operator procedure is mandatory: export every package to a `.skill` archive **before** rolling back, verify the exports, then roll back; and restore with the new binary or from a pre-package backup. Relaxing the validator to skip unknown tables is explicitly out of scope and would be a change to an existing fail-closed component. | `PRAGMA user_version` appears **nowhere** in the tree, so there is no schema gate and an old binary opens a newer database without complaint. `restore-db` rejects any dump naming a table not on `CanonicalMPMSchema` (`internal/core/sql_dump_validator.go:530+`), and the first revision's own §3.1 already recorded that a new table not on the list makes restore fail. | The operator can still create the situation — take a backup with the new binary, roll back, and be unable to restore it with the old binary. The runbook (P1) and §6.2 state the procedure, but the procedure is procedural, not enforced. The alternative — making the old binary able to restore a package-bearing dump — is impossible without changing the old binary, which by definition is not being changed. |
| **R9** | §4.2, §6 and §14 used one word — "rollback" — for two operations with opposite characteristics: a binary rollback (lossless but blind) and a schema rollback (destructive). "Rollback is total and lossless for everything that existed before" read as though no data were at risk. | §6.1 (new), §4.2, §14, §10 P1 | **Named separately throughout**: *binary rollback (lossless, blind)* and *schema rollback (destructive)*, with a table contrasting what happens to the package bytes, to everything else, to the operator-visible signal, and to the pre-condition. §6.2's restore incompatibility is stated as part of the binary rollback, not as a separate surprise. | The two operations are materially different: the binary rollback leaves the rows in the file (they reappear when the new binary returns) while the schema rollback destroys them. The absence of a schema gate (§R8) is what makes the binary rollback "blind". | The naming does not remove the risk of the destructive operation — it makes it legible. The schema rollback still destroys third-party content that exists nowhere else, which is why §6.1 makes "export the packages first" the pre-condition for **both** rollbacks. |
| **R10** | §7 threats 14, 15, 18 and acceptance row T24 all rest on the poison scan, but `isPoisonedAt` **fails open**: if the operator's toxic-phrase file cannot be read, it returns "not poisoned" and the scan passes everything. The design did not say so, and a skipped scan is indistinguishable from a successful one. | §7.2 (new), §7 threat 14, §7.3, §5.5, §9 schema columns, §12.1 **O12**, §13 T43 | **The import fails closed** with `scanner_unavailable` and writes nothing. The fail-open behaviour of the underlying scanner is disclosed rather than fixed here (closing it is a change to an existing component with other callers). The manifest records **which scanners actually ran** per file, so a disclosed skip (a binary asset) is distinguishable from a pass. | `isPoisonedAt` returns `(false, "")` when `loadPoisonPhrasesAt` fails (`internal/core/memory.go:845-851`). The asymmetry is real: `isSensitiveContent` is 30 in-process regexes and cannot fail this way; the poison scanner reads an operator-editable **file** and can. | An operator whose phrase file is unreadable cannot import packages at all until they fix it. That is the intended direction — a refusal is visible, a skipped scan is not — but it is a new hard failure for an operator who did not know the file was unreadable. The message must say which file is unreadable. The underlying fail-open path in `isPoisonedAt` remains open to *other* callers; this design does not close it and does not claim to. |
| **R11** | §6's compatibility matrix said `mpm export --collection skills` does not export package metadata, while **O8** recommended carrying it "as a follow-up after P6". The document contradicted itself, and the matrix's version — the one an implementer would have built — is a **silent data loss** path for content that exists nowhere else, and it defeats the archive-as-transport that **O4** nominates. | §6.3 (new), §6 matrix, §10 P4 and P7, §12.1 **O8**, §13 T44 | **The contradiction is resolved and the loss is made explicit in two stages.** Before full package export exists, the export **names** every package it cannot carry — in the response and on the CLI's standard output. Stage P7 then makes the export complete, emitting one `.skill` archive per package. The marker ships in **P4**, the first stage in which a package can exist, so the loss is never silent in any release. | The first revision's matrix row and its **O8** row said different things about the same surface. The bytes' single-copy property is established in §3.2 and §6.4. | Until P7 lands, an operator's "export my skills" workflow still loses the reference files — it just now **says so**, and names the affected skills. The marker's presence is a design decision about how loudly to fail, and its exact wording is not specified. Where the P7 archives are placed is **O15**. |
| **R12** | §7 threat 11's caps were described as if enforced when files are *accepted*. The directory path has no archive to bound it, so a directory holding ten million entries would be fully walked, `Lstat`'d and measured before the first rejection. | §7 threats 11 and 19 (new), §8.3, §10 P4, §13 T45, T46 | **The caps are enforced *during* traversal** — the walk stops at the 513th entry and the byte counters are checked as bytes are read. The walk **refuses to descend into a symlinked directory** and **refuses to leave the resolved root**, with the root resolved once via `filepath.EvalSymlinks` and every candidate compared against it before it is read. This also closes part of **R4**. | The ZIP path is naturally bounded by the archive; the directory path is not, and nothing in the substrate bounds it. `os.Lstat` never follows a symlink, which is what makes the symlinked-directory refusal implementable. | The refusals are new reason codes (`symlinked_directory`, `path_escapes_root`) that an operator may meet on a legitimate package that happens to symlink its assets — a real possibility for a package that vendors a shared directory. The refusal is safe and loud; whether an operator can override it is **not** decided here, and is left for the operator's judgment. |

### 15.1 The seven optional improvements

The review offered seven optional improvements. Their disposition is recorded
here so that none is silently dropped.

| | improvement | disposition |
|---|---|---|
| **O-a** | Fix `ListSkills` so it no longer parses frontmatter out of every skill row's full content; the entrypoint cap then becomes unnecessary | **Not adopted.** The 256 KiB entrypoint cap (O6) stands as the mitigation. The refactor touches a hot read path used by discovery, the wake context and `mpm info`, and no correction in this revision requires it. Recorded as **O16** so it is not lost. |
| **O-b** | Specify the bounded-read interaction: which line `next_offset` names, and whether a partially-returned line is flagged | **Adopted.** §8.6 now defines `offset`/`next_offset` as zero-based **line** indices, states that `next_offset` names the first line not returned, that a line cut by the byte cap is **not** partially returned, and what happens when a single line exceeds the cap. Pinned by T47. |
| **O-c** | State the blob collector's dependence on the single-transaction import | **Adopted.** §9 states that a blob row and its referencing file row are committed together in the importer's one transaction, and that splitting the import would break the collector's safety **without** breaking any test that does not specifically check for it. |
| **O-d** | Say that the canonical path is the primary key and `raw_path` is display-only | **Adopted.** §5.4 states it, §9 restates it against the DDL, and P2's acceptance names it. |
| **O-e** | Promote the "bytes never touch the filesystem" invariant to a first-class rule beside the blobstore hard rule | **Adopted.** It is **Rule 2** in §4.4, and it is the real defence for both import paths (**R5**). It is also asserted by a test in P9. |
| **O-f** | Name `saveMemoryRow` in E5 so the tx-aware write primitive is discoverable | **Adopted.** §3.4 E5 names `saveMemoryRow` and the `txNode` adapter, and explains why `SaveSkillWithInvocation` cannot serve (**R2**). |
| **O-g** | Add the two `archive/zip` behaviours that are not rows: the FAT-creator archive and the directory walk's symlinked-directory refusal | **Adopted.** T35 is the FAT-creator row (and it asserts the *acceptance*, not the rejection) and T46 is the symlinked-directory refusal. §13.2 records the fixture strategy for both, including the generator constraint that `zip.Writer` overwrites the creator byte — so a hand-rolled fixture cannot make T35 vacuous. |

### 15.2 What this revision did not do

* **It did not redesign the subsystem.** Option B, the blobstore rule, the
  per-file scanning model and the compatibility requirements in §3.3 are
  unchanged.
* **It did not implement anything.** No production code, no migration, no
  schema change. The document remains a proposal at the review gate.
* **It did not refactor `ListSkills`.** See **O-a** and **O16**.
* **It did not weaken the restore allow-list** to hide the binary-rollback
  incompatibility (**R8**).
* **It did not close the poison scanner's own fail-open path** (**R10**); it
  made the package import fail closed instead, and said so.
* **It did not mark the architecture approved.** Approval is the operator's
  decision and the reviewers'; this revision stops at the review gate.

---

## 16. The C-1–C-5 disposition matrix

The five corrections required by the second independent review. Each row states
the **original problem**, the **revised specification section**, the
**resolution**, the **supporting evidence**, and the **remaining risk**. As
with §15, the evidence column cites a measurement, not an assertion.

| # | original problem | revised section | resolution | supporting evidence | remaining risk |
|---|---|---|---|---|---|
| **C-1** | §5.3's normative rule — *"An archive whose entries do not all share one top-level directory is rejected as `ambiguous_package_root`"* — rejected exactly what §5.6 mandates the export writer to produce. The canonical paths are `SKILL.md`, `references/templates.md`, `references/principles.md`; their top-level components are `SKILL.md` and `references`, so they do **not** share one top-level directory. T07, T11 and §6.3's P7 contract all failed, and O4's archive-as-transport had no valid archive to transport. §5.3 also cited **O9**, which says the opposite. | §5.3 (rewritten), §5.3.1 (new), §5.4, §5.6, §12.2 **O9**, §13 T07, T11, **T48**, §10 P5, §7.1 rule 2 | **Two layouts are accepted, and the root derivation is specified exactly.** The **flat** layout — the canonical paths at the archive's top level — is the form **MPM's own export writer produces** (§5.6), and the **wrapped** layout — one top-level directory holding everything — is the conventional third-party form. §5.3.1 gives the derivation: skip directory entries **first** — before canonicalisation, because a directory entry's name is not a canonical path — then canonicalise the file entries, classify the top level into *directory prefixes* and *top-level files*, and derive the root from a four-row table; two or more directory prefixes are rejected with `ambiguous_package_root`. Stripping is by whole segment; the derivation is order-independent; the root is not stored. The derivation is **hash-neutral** — the same package from either layout yields the same canonical paths, the same file rows and the same `package_hash`, with only `raw_path` differing — so T03, T04 and T07 assert the same hash. | The reference package's canonical paths are `SKILL.md`, `references/templates.md`, `references/principles.md`; distinct top-level components = 2 (`SKILL.md`, `references`), so the old rule rejected the export's own output. The operator's archive (`system-prompt-writer/…`) is the wrapped form and strips to the same three canonical paths. §5.3.1's four rows are exhaustive over the two classifications, and the third row is precisely the export's shape. | **O9 remains open** and must be answered before P2/P5 — but the flat layout is not really optional, because it is MPM's own output format; refusing it would refuse MPM's own archives. A third layout (a wrapped root plus a stray top-level file) is handled by the flat rule and lands the entrypoint at `pkg/SKILL.md`, which is `entrypoint_not_found` with a candidate list, or an explicit `--entrypoint`. The derivation is new code in P5, so a mistake in it fails T48 rather than corrupting data. |
| **C-2** | T41 expected `mpm prune --older-than 1s` to *"still remove an ordinary, non-packaged skill of the same age"*, while §6.4 and **O13** specify a **blanket** `collection = 'skills'` exclusion. Under the specified implementation the ordinary skill is **also** not removed, so T41 would fail — and it fails in the direction that matters: an implementer writing to satisfy T41 has to build the per-row check inside a bulk delete that §6.4 explicitly rejects as "larger and racy". | §6.4, §6 matrix, §12.1 **O13**, §12.2 **O17** (new), §13 T41, §10 P3, §15 R7 row | **The blanket form is kept as the specification, T41 is corrected to match it, and the operator's choice is made explicit.** T41 now asserts **both** halves: the packaged skill survives **and** an ordinary skill of the same age is not removed. §6.4 states the blast radius in a block quote — *after P3, `mpm prune --older-than` can no longer remove **any** skill, packaged or not* — and §6's matrix row carries it. **O13** is re-scoped to settle the **safety invariant** (a prune must never destroy package bytes) and points the **mechanism** at a new open decision, **O17**, which records the recommendation (blanket), the package-aware alternative and its two costs (a per-row check inside a bulk delete leaves a race; a larger change to a statement with one exclusion clause), and states that it must be answered **before P3**. | `PruneOlderThan` is `DELETE FROM memories WHERE created_at < ? AND deleted_at IS NULL AND <DirectiveExclusionClause>` — no `collection` filter (`internal/core/web_db.go:1781-1792`) — so adding `collection <> 'skills'` excludes **every** skill row, not only packaged ones. The exclusion is a statement-level clause, not a per-row predicate, which is what makes the blast radius total. | **The operator's approval of the blast radius is outstanding, and this revision does not invent it.** A behaviour change for every existing skill — one that stops working silently — is exactly the kind of compatibility change that needs an explicit answer, so it is carried as an open decision (**O17**) rather than as a settled row. If the operator chooses the package-aware form, **one clause of T41 changes** and nothing else in the document moves; §13.1 says so. |
| **C-3** | §7.1's measured FAT block said a directory entry is `type=---------- regular=true → treated as a regular file`, and the paragraph beneath said a directory entry *"is not detected as a directory either, so it falls through to the canonical path rules"* and is *"rejected as a non-canonical path"*. Both are wrong about Go: `FileHeader.Mode()` forces `fs.ModeDir` from a trailing `/` in the entry **name**, independently of the creator byte. An implementer following the paragraph would write the FAT rule as "a trailing-slash entry is rejected as non-canonical", and **T35 would fail** — T35 expects the FAT archive to be **accepted**. | §7.1 (rewritten), §7 threat 3, §11.1 R5 row, §15 R5 row, §13 **T35**, §13.2 fixture strategy | **The FAT block is corrected and the paragraph is replaced with the accurate mechanism.** Under a FAT creator byte the symlink, the FIFO and the device still degrade to `0666` regular and pass the type check — that is the substance of R5 and it stands. A **directory** entry is still detected and **skipped**, because the trailing `/` in its name identifies it. §7.1 now carries the Go source and the measured isolation of the two signals. The FAT case is therefore **safer** than the first revision claimed, not worse. T35 now asserts all three halves: the import did not fail on the entry type, the directory entry was **not stored**, and no file was created anywhere. | Measured on the same archive, creator byte rewritten from Unix to FAT: `pkg/link.md`, `pkg/fifo` and `pkg/dev` become `type=---------- regular=true`; `pkg/sub/` remains `type=d--------- dir=true`. Two further variants isolate the signals: with the MS-DOS directory attribute **cleared** and the trailing `/` kept, `pkg/sub/` is still `type=d--------- dir=true`; with the attribute **set** on a name with no trailing slash, `pkg/nodir` is `type=d--------- dir=true`. Go's source: `archive/zip/struct.go:317-328` applies `mode |= fs.ModeDir` for a trailing `/` **after** the creator switch, and `msdosModeToFileMode` (`:348-358`) reads only the MS-DOS attribute byte. | None material. The correction makes the FAT case *stronger* than the document claimed, and T35 now pins the behaviour that was previously wrong. The residue is that the type check remains unavailable for the symlink/FIFO/device entries under a FAT creator byte — which is R5's standing limit, and is why Rule 2 is the boundary, not the type check. |
| **C-4** | §8.6's `next_offset` bullets defined, individually reasonably, an infinite loop: `next_offset` is always *"the index of the first line that was not returned"*, and a file whose first line alone exceeds the cap returns *"zero lines, `truncated: true`, and `next_offset` equal to the original `offset`"*. Zero lines with an unchanged `next_offset` means the caller — which the same section tells to resume with `offset = next_offset` — gets the same empty response forever. | §8.6 (rewritten), §13 T47, **T49** (new) | **The terminal case is stated explicitly, in three parts.** The server **marks** it (`line_too_large`, alongside `truncated: true` and `next_offset == offset`); the caller **does not resume** and reports that the file cannot be served at this cap; and the server **does not produce it for any other reason** — `truncated: true` with `next_offset == offset` and no `line_too_large` is a contract violation, because every other truncation returns at least one line. The **progress guarantee** follows: every non-terminal response satisfies `next_offset > offset`, so a caller that resumes always advances. | Both bullets are in §8.6 as written; with `offset = next_offset = 0` and a first line larger than the cap, the response is `(lines: [], truncated: true, next_offset: 0)` on every call. The byte cap applies *after* the line range is selected (§8.6), so the cap cannot select a different range — which is why the only escape is to raise `max_bytes` to the 1 MiB hard cap. | Raising `max_bytes` works only if the line fits under 1 MiB; a file with a single line larger than that is unreadable through this API at any cap. That is stated rather than hidden, and the reason code names it. `line_too_large` is a new reason code in the `read_file` response, and it is the only place the bounded-read contract adds one. |
| **C-5** | §6's migration mechanics said the three tables are *"applied at boot alongside the existing table groups, following the `reference_docs` precedent"*, while §5.9 promised the shared database is not extended. `attachShared` applies **every** `CREATE TABLE` in `BaseTables` to the shared schema, so an implementer who reads "the existing table groups" as `BaseTables` creates `shared.skill_packages`, `shared.skill_package_files` and `shared.skill_package_blobs` in **every** shared database — the precise thing §5.9 says does not happen. It lands in **P1**, the first stage. | §6 (migration mechanics rewritten), §5.9, §9 notes, §10 P1, §13 **T50** (new) | **One local-only path is named exactly.** The three statements and the index go in a **new `SkillPackageTables` slice** in `internal/core/schema.go`, applied at boot by the same function that applies `ReferenceTables` and `WorkTables` and **outside** the `attachShared` loops — a sibling of `ReferenceTables`, **not** a member of `BaseTables`. §5.9's promise is kept and now points at §6; §9's notes carry the constraint; P1's acceptance names it; **T50** proves the shared schema is unaffected. | `attachShared` rewrites every `CREATE TABLE` in `BaseTables` to `shared.<name>` (`internal/core/db.go:2132-2143`) and does the same for that slice's `CREATE INDEX` statements (`:2158-2167`). `reference_docs` (`internal/core/schema.go:953`) and `reference_chunks` (`:1002`) are in **`ReferenceTables`** (`schema.go:949-1047`), not `BaseTables` (`schema.go:38-936`); the repository says so at `internal/core/db.go:2318`. Measured at runtime with a shared database attached: the shared schema has 35 tables, `reference_docs` and `reference_chunks` are **absent** from it, `memories` and `sessions` are present, and `skill_packages` is absent. | The cited precedent was already local-only — the defect was **ambiguity**, not a wrong precedent, and this correction removes it by naming the slice. The new slice is one more thing for P1 to add to the schema sources, and `TestCanonicalSchemaSync` requires every table in the schema sources to be on `CanonicalMPMSchema`, so the registration is required either way. T50 asserts the shared schema's table set is identical to a package-free database's, so a later refactor that moves the tables into `BaseTables` fails the test rather than silently reaching every shared database. |

### 16.1 What this revision did not do

* **It did not begin P1.** No implementation of any kind. The stages in §10
  remain proposals.
* **It did not choose the prune mechanism for the operator.** **O17** is an
  open decision with a recommendation and a cost; the approval is the
  operator's.
* **It did not reopen the twelve R-corrections or the accepted foundation.**
  Option B, the blobstore rule, the per-file scanning model, §3.3's
  compatibility requirements and C1–C15 are unchanged.
* **It did not answer O9.** §5.3 now states the recommendation as the
  specification, and O9 remains the operator's to confirm — but the flat
  layout is MPM's own output format, so the recommendation is not a free
  choice.
* **It did not weaken any control to make a test pass.** The FAT case is
  *stronger* after C-3; the prune row is stronger after C-2; the shared-schema
  row is new after C-5.
