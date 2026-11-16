# P7 and the P6 follow-up — final report

> **DIRECTION CHANGE (the operator, after round 23).** The matcher-fidelity
> track stops here: no round 24 was commissioned — the brief was written
> and **not** used (`review-a24-brief.md`). The question is no
> longer "does the model agree with the host on one more form" but "does
> the review environment provide a genuine boundary".
>
> **The answer, measured: a boundary is available.** `bubblewrap` 0.11.1 is
> installed and unprivileged user namespaces are enabled, so the reviewer's
> shell can run in a MOUNT NAMESPACE where an unbound path does not exist.
> `agent_installation/mpm-opencode/scripts/reviewer_bwrap.py` builds it, and
> `agent_installation/tests/test_reviewer_bwrap_sandbox.py` pins the
> contract: the disposable sentinel tree, the live runtime root and the
> operator's home are all **invisible** from inside; the disposable tree is
> writable; there is **no network**; and the Go toolchain, Python and Git
> work inside, so a review can still run the test suites. The host's
> enforcement path is measured too: it executes what the `create.before`
> shell hook hands it (a rewritten command was run, marker and all), and
> the hook's event carries no session identifier — so the wiring is known,
> and is not yet done.
>
> **The namespace is INERT.** A review still runs outside it, so the shell
> protection is **best-effort** — stated in both READMEs, in the SPEC and in
> the PR body, rather than claimed as isolation.
>
> **The deliverable for the convergence question is
> `decision-report.md`**, and its recommendation is to
> **separate A1/A2 from A3**: A1 and A2 are complete and verified and
> depend on nothing in A3; A3's remaining work is a new implementation
> cycle whose gate is *a demonstrated escape*, not *another shell form*.


## The two PRs

| | **PR A — #244** | **PR B — #245** |
|---|---|---|
| title | `fix(skill-packages): the complete hash contract, one snapshot, and an isolated reviewer` | `feat(skill-packages): the deterministic .skill writer and the complete skills export` |
| branch | `fix/skill-packages-p6-provenance` | `feat/skill-packages-p7` |
| head | `173e50e9f2dca00b36d8c8a56fed0283c20bed53` | `bd0236e375c9d4711ee9aaf3a7c3ff952e864265` |
| base | `69bc212e207ddcf50b9c09d662d63403925a702e` | `69bc212e207ddcf50b9c09d662d63403925a702e` |
| commits | 31 | 9 |
| files | 23 (+8661 / −171) | 15 (+5570 / −539) |
| state | OPEN, MERGEABLE | OPEN, MERGEABLE |
| CI | running at the reviewed head | **all three checks PASS** at `bd0236e3` |

Neither PR is merged. Neither depends on the other's approval: they touch disjoint files except `docs/SPEC.md` (each adds its own sections), and each stands on its own base commit.

## PR A (#244) — the approved P6 follow-up

**A1 — the complete hash contract.** Three hashes, each with a recorded origin:

* `projection_sha256` — recorded when the package is written, and validated against the package manifest on read;
* `body_sha256` — recomputed from the body on read;
* `entrypoint_sha256` — the package's original entrypoint hash;

plus a `hash_origins` object that names which origin each hash has, so a reader never has to guess. A manifest that validates but *contradicts* the stored projection fails **closed** (`integrity_failure`); an unparseable manifest is **tolerated** (`unavailable`), because a malformed manifest is not evidence of corruption.

**A2 — one snapshot.** `ReadSkillWithPackageProvenance` reads the skill body and its package provenance from one consistent database snapshot, so a concurrent writer cannot make the two halves disagree. The mutation proof — rewriting the exported read as two independent reads — makes both deterministic tests fail (`opened 2 snapshot(s), want exactly 1`).

**A3 — reviewer isolation at the permission layer.** The MPM tools are removed from the reviewer's catalogue by *rules*, not by prose: the reviewer's definition denies every `mpm_*` action, the `read`/`glob`/`external_directory` paths into a live workspace, `edit`, `subagent`, `question`, Code Mode, the MCP resource surface and the session/worktree mutations; the shell rules deny the binaries wherever they are **named**, in **seven forms per binary** plus **three forms shared by every binary** (command position; own word; followed by a tab; last segment of a path; last segment of a path with a trailing slash; inside a Go package pattern; the installed path — plus a tab before the name, the quoted pair and the ANSI-C quoting opener), and deny the installers and the service-control commands. The reviewer also gets an isolated `HOME`/`TMPDIR`/`MPM_WORKSPACE` and a filesystem scratchpad, and a **guard test** that proves the rule set non-vacuously in **ninety-one** directions.

## PR B (#245) — Stage P7

**B1 — the deterministic `.skill` writer.** Flat layout, `SKILL.md` first, the other canonical paths sorted, the ZIP epoch on every entry, `Store` compression, no extra header fields, `0644` (`0755` for a manifest-marked executable).

**B2 — verify before export, in one snapshot.** The exporter verifies each package before it writes it, and reads the batch from one snapshot.

**B3 — round trip.** `package_hash` and every file hash survive export → import → export, byte-identical.

**B4 — the legacy single-document skill** still exports as it always did.

**B5 — the complete export.** The document, a sibling `.packages` directory, one `<name>-<version>.skill` per package, a machine-readable index, `memories` preserved, and `packages_omitted` preserved — zero only when everything was exported.

**B6 — fail closed.** A package-bearing export to stdout, CSV or `json-array` is refused, and `mpm migrate --from` refuses a package-bearing document (twenty-four malformed shapes measured).

**B7 — filesystem safety.** Symlink and non-empty-directory refusals, modes `0644` document / `0600` archives / `0700` directory, staging-then-rename.

**B8 — the tests.** T07, T11, T44, the adversarial corpus, and Go 1.26.6 / 1.27.3.

**Stage boundaries held.** No `mpm skill export`, no new `mpm_skills` MCP action (that is P8), no schema change, no DB write during an export, no P9/HAL content. The exporter's internal interface is exposed for P8.

## Compatibility decisions

**PR A.** The hash contract is **additive**: a package written before this change keeps working, and a reader that does not know `hash_origins` is unaffected. A manifest that contradicts the projection now fails closed where it previously read on — that is the intended change, and it is the only behaviour change on the read path. The one-snapshot read changes no output; it changes only what a concurrent writer can do to the reader. The reviewer's agent definition is a **new file**; the installer gained `--force` (idempotent reinstall) and `uninstall.sh` gained the sandbox teardown. One pre-existing defect was fixed on the way: `internal/core/tools/output_policy_test.go` resolved its own source file through `MPM_WORKSPACE`, so it failed in any sandbox — it now honours `MPM_WORKSPACE` only when that directory actually holds the file.

**PR B.** A document produced before P7 is still accepted by `mpm migrate --from` (the new keys are optional), and the `packages_omitted` marker is kept with `count: 0` rather than removed, for shape stability — the design said the marker would go, and it does not; that is a deliberate compatibility choice, documented in SPEC §6.10/§6.13. The omission vocabulary is complete and each condition has exactly one reason: `unpublishable` for a name that cannot be a file name, `export_failed` as the catch-all.

## The independent reviews

**PR B** — five rounds. The first returned REQUEST CHANGES with three findings; the second APPROVE with three minor findings and three nits; the third APPROVE with three minor findings and two nits; the fourth APPROVE with two minor findings and one nit, and caught that the third round's comparator remediation was only **half-done** (the named comparator was a dead function while the writer kept its own inline copy — reverting the fix now fails the test). The fifth round, of `22665ecb`, returned **APPROVE with no BLOCKING, MAJOR or MINOR finding** — all six remediations verified closed and load-bearing, twenty-four malformed documents refused by the migration guard, determinism and TZ-invariance and round trip confirmed, and no DB write during an export — with one NIT, addressed in `bd0236e3` and confirmed by a closing check.

**PR A** — twenty-three rounds so far. The rounds found, in order: the command line closed only at the command position; the `action: skill` rule was a no-op; the path rule did not close the bare path (`go run ./cmd/mpm` ran the binary); a TAB separates the name from the command; the Go package pattern matched no rule; the shell glob and the brace expansion; the model's disagreement with the host on the separator set, on backtick and process substitutions, on double quotes, on `;` inside `$()` and on nested substitutions; the model did not know the `case` construct, the empty-target redirection, the `&` of a redirect operator, the subshell, the two loop kinds, the function definition, the `[[ ]]` and arithmetic contexts, or the wider set of positions a context is a construct in; a here-document's body is data for the command analysis and still text for the rule matcher; a continuation keyword with nothing to continue is a construct the host cannot analyse; a context the host cannot parse is a fourth refusal reason; the published example was wrong; the model mis-parsed a here-string and a here-document inside a body; a process substitution is a *command*, not an unanalysable form; the guard did not pin the model's depth restoration; the model allowed **thirteen** malformed constructs the host refuses; the SPEC published a residual route the host **closes**; the divergence count was wrong and the sentence that justifies the two directions was **inverted**; the host has a **sixth** refusal reason; the template used a different grouping; the model allowed **eight** more constructs — the **trailing-text** class, a quoted empty pattern, a nested or newline-separated empty group and a comment-only condition — and the last head's own rules had introduced four benign divergences of their own; the model allowed **thirty-three** more — the whole **structural** class, a construct the host's scanner cannot analyse — none of which the corpus pinned, proved by the review's own mutation; and the model allowed **twenty-five** more, in **five families** — a `(` glued to a word, a trailing `&&`/`||`/`|`, a compound command whose keyword never arrived, a closing keyword that closes nothing, and two `case` forms — again none of which the corpus pinned, proved the same way, plus the model's own **inconsistency** about what a `case` is.

Each round's findings are fixed and pinned in the guard's corpus, and each fix is proven load-bearing by mutation.

### The twentieth round, and what this head does about it

The round returned **REQUEST CHANGES** with one MAJOR finding and three MINOR ones. The MAJOR was the same shape as the one before it, one class wider:

* **The model ALLOWED eight constructs the host REFUSES.** Rows 1–4 were ONE class — **trailing text after a closed compound construct** (`invalid-structure`), which the model modelled only for the `case` spelling, so the same shape for an `if`, a brace group, a subshell and a function body fell through. Rows 5–8 were three more instances of the same *family*: a `case` whose pattern list holds a **quoted empty pattern**, a **nested** and a **newline-separated** empty group, and an `if` whose condition is only a **comment**. The corpus pinned none of them — the review proved it by mutating the model to *catch* them and finding the guard still green. **Closed**: the check lives outside the `case` branch now, and every one of the eight forms is a corpus row.
* **The last head's own rules had introduced four benign divergences** — a `case` pattern list with an empty *later* pattern (the host allows it), and a keyword that is an *argument* rather than a condition (`echo if; echo hi`), and a function definition whose body follows on the next line. So the last head was too strict in three places and too lax in one. **Closed**, and two of the rules were then corrected by a dedicated measurement round: a pattern list is refused when its **first** pattern is empty and allowed when a later one is, and the host treats `{}` as an ordinary word and allows it while it refuses `{ }`.
* **MINOR-3 — the recorded divergences were recorded but not pinned.** They now live in a **ledger** inside the guard, each one asserted to be refused in the recorded direction, and `docs/SPEC.md` is asserted to publish the ledger's length — so the published count of five is checked against the code. (Proved: changing the SPEC's "five" to "four" turns the suite red.)

The twenty-third round, of `0ee25aaf`, returned **REQUEST CHANGES** with **seven MAJOR**, two MINOR findings and one NIT — and the exposure was **NONE**: the host **fails closed** on every one of the thirty-six divergent forms the review measured (three isolated runs each, 103 runs, no flaky verdict), no binary was reached, and the live database was never opened. It is described below, and this head is its remediation.

### The twenty-third round, and the parser

The round returned **REQUEST CHANGES** with **seven MAJOR**, two MINOR findings and one NIT — and the exposure was **NONE**: the host **fails closed** on every one of the thirty-six divergent forms the review measured, no binary was reached, and the live database was never opened. The findings were about **fidelity and evidence**.

The review wrote **103 NOVEL** structural forms, checked them against the corpus first so that none of them was already pinned, and measured each on the host: **25** the host REFUSES and the model ALLOWED, **10** the host ALLOWS and the model refused, **68** agreements. The twenty-five were **six families**, all missed because the rule was written for a **shape** rather than for a **class**:

* a **trailing `&&`, `||` or `|` inside a construct** — `{ echo hi && }`, `case x in y) echo hi && ;; esac`, `if true; then echo hi && fi` — implemented only at the top level;
* an **`if` with no `then`** — `if true; echo hi; fi`, `if true echo hi; fi`, `if true then echo hi; fi`: the `do` half of the rule was closed, the `then` half was not;
* an **`else` or an `elif` with no `then`** — `if true; else echo hi; fi`;
* an **empty command slot after a compound keyword** — `if true; then; echo hi; fi`, `while false; do; echo hi; done`, `case x in y) ; esac`;
* a **redirection with no target inside a construct** — `( echo hi > )`, `{ echo hi > }`, `if true; then echo hi > fi`;
* a **`case` whose subject carries a `)`** — `case x) in y) echo hi;; esac`.

The seventh reported form — a `case` followed by `&` — is a **probe artefact**: the review's own `; echo "RC=$?"` suffix turned the `&` into an `&;`, an empty slot, which the host refuses for a different reason. The clean form is **ALLOWED**.

The deepest finding was again the **shape of the evidence**: the class-level table asserted the **model's** reason against its own table, so the mechanism was structurally incapable of noticing that the table's reason was wrong — and one was (`echo hi ;& echo bye` is `invalid-structure`, not `compound-command`). The host's **reason depends on the construct that holds the defect**, so the table now carries a **reason per form**.

So the scanner is now a **PARSER** — it walks the command the way the host's parser does — and it is the **single enforcer** for the structural class: the older machinery keeps only the two rules the scanner does not own, the dynamic execution of `eval` and `alias NAME=value` and a context the host cannot parse. The model can no longer be **internally inconsistent** about what a structural defect is. The review's **34** divergent forms are pinned in the corpus **in both directions**, and every form of every family is pinned in the corpus as well, so the two pieces of evidence cannot drift apart.

The MINORs: the housekeeping assertion could only see **methods**, and the file carried two **module-level** duplicates of its own (`import re` and `from pathlib import Path`, each written twice) — so the assertion now covers every module-level definition and the duplicates are gone. And the review's own probe showed that the author's shell layer and the reviewer's differ, so a measurement about the **name matcher** has to be made in the reviewer's layer. The NIT: the definition's family prose was never checked against the model, and it **had drifted** — so the definition now carries a **machine-readable** family list, and the guard asserts every line of it.

The mutation sweep was rebuilt against the new scanner's code sites: **ninety-one** directions — ten rule-level and eighty-one model-level — and every one of them fails the guard with an **assertion** about the behaviour it names, not with a crash and not with a hang. The baseline is GREEN before the sweep and GREEN after it, so no direction leaves the guard mutated. The **ten** directions that were vacuous — the mutation reverted a behaviour nothing pinned — were fixed by **pinning** the forms they need and by **re-pointing** four of them at the code they actually name.

### The twenty-second round, and the five families

The round returned **REQUEST CHANGES** with **six MAJOR**, three MINOR findings and one NIT — and the exposure was **NONE**: the host **fails closed** on every one of the twenty-five divergent forms the review measured, no binary was reached, and the live database was never opened. The findings were about **fidelity and publication**.

The review wrote **109 NOVEL** structural forms, checked them against the corpus first so that none was already pinned, and measured each on the host **three times in isolated tool calls**: **25** the host REFUSES and the model ALLOWED, **8** the host ALLOWS and the model refused, **76** agreements, and **no flaky verdict**. The twenty-five were **five families**, all missed:

* a `(` **glued** to a word — `a(b)`, `echo a(b)`, `1(b)`, `x(y) z` — is a function definition's **parameter list**, and the host refuses it as `dynamic-execution` unless the list is empty and a brace-group body follows (the **spaced** spelling is refused as `compound-command`: `echo (hello)`, `f ()`);
* a **trailing** `&&`, `||` or `|` demands a command that does not exist, and is refused as `invalid-structure` — while a trailing `;` and a trailing `&` are **allowed**;
* a compound command whose **keyword never arrived** — `for i in 1; echo hi; done`, `while false do echo hi; done` — is refused as `compound-command`;
* a **closing keyword that closes nothing** — `echo hi; esac`, exactly as `echo hi; fi` and `echo hi; done` are — is refused as `compound-command`;
* a `case` with **no subject** (`case in) echo hi;; esac`) and a `;&` / `;;&` **branch separator** are refused as `compound-command`.

The model was also **internally inconsistent**: the scanner read the `case` construct by its **keyword** `in` while the older machinery read it by the text " in ", so the two disagreed — and the head's own docstring described the behaviour the code did not have.

The deepest finding was the **shape of the evidence**. The corpus was a list of **spellings**, so the review patched the model to agree with the host on all five families and the whole guard suite stayed **GREEN**. So the corpus is no longer the only evidence: the guard now carries a **CLASS-LEVEL** table — a **FAMILY**, the **REASON** the host gives it, and representative forms — and every form of every family must be refused **with that reason**, so a family the model stops catching shows up as a **failing reason** rather than as a missing row.

The MINORs: five of the published mutation directions failed because the **mutated file no longer parsed**, so the guard went red for any reason at all; two directions were **byte-identical** edits; and one was **mislabelled** — its needle landed in a different function from the behaviour it claimed to pin. All three are fixed. The eight over-refusals are fixed too: a `then` after an `elif`, a group after a **prefix operator** (`time { … }`, `! { … }`), a parameter expansion that holds a substitution, the `|&` operator, and a here-document that follows a closing keyword are all **allowed** by the host and by the model now.

And the model's **reason** for a `)` that closes a `case` or a group is `compound-command`, not `invalid-structure` — measured against the host and corrected.

The NIT: the design of record is PR #222's §8.2.1, which lives on **that PR's own branch** and is **not in this branch's tree**, and §8.2.1 there specifies **one** hash — the three-hash contract is this PR's approved extension of it. The gap is **stated** in the PR body rather than papered over.

### The twenty-first round, and the structural fix

The round returned **REQUEST CHANGES** with one MAJOR, three MINOR findings and one NIT — and the MAJOR was the same shape as the one before it, one class wider. The model **allowed thirty-three** constructs the host refuses, all one family: a construct the host's portable scanner cannot **ANALYSE**. The corpus pinned **none** of them, which the review proved by mutating the model to *agree* with the host and finding the whole suite still GREEN.

The model's own conclusion is the point: it recognises **spellings**, so every new spelling misses again. So the fix is **structural** — the model now **parses** the command the way the host's scanner does and refuses what it cannot parse: an unbalanced delimiter, an unclosed group, an empty command slot, a brace group that stands where a command may not stand, a `case` whose subject is a command, a compound command that is never closed, a body that is empty, an empty `for`/`select` header, an unterminated quote, a trailing backslash, and a `case` construct read by its keyword `in`. The scanner is the **single enforcer** for the class, so every mutation reverts it and the guard goes red; every one of the seventy-two published directions now fails with a reason, where six were vacuous before. All thirty-three forms are pinned in the corpus **in both directions** — the broken forms refused and the well-formed forms allowed.

The MINORs: the host has **eight** refusal reasons and not six (`unterminated-quote` and `unterminated-escape` were measured and named nowhere); the PR body's test count was stale; and two rows of a measurement round did not reproduce — two earlier rounds and a third round of six repeats each measured them ALLOWED, so the corpus keeps that and the discrepancy is recorded rather than glossed. The NIT: the residual route list gains a measured instance — `mp""m --version` and `m"pm" --version` are allowed and **run the binary**, the same family as the name split by a substitution.

## Adversarial results

**PR B.** The fail-closed migration guard refused all twenty-four malformed shapes (string / fractional / null / missing / negative counts, counts disagreeing with their lists, a non-object index, a non-object marker or scope, missing scope keys, a broken `selected == carried + omitted`, a `carried` that disagrees with the archive index, a negative `total`, a scope that is not an object, the new `export_failed` reason) and accepted exactly the three legitimate ones. The exporter's own arithmetic holds (`selected == carried + omitted`, `carried == package_archives.count`). Two exports are byte-identical; export → import → export is byte-identical; the result is TZ-invariant. The refusals (stdout, CSV, `json-array`), the staging/rename order, the symlink and non-empty-directory refusals, the recovery message and the modes were all measured. The database's row counts **and** its bytes are unchanged across an export.

**PR A.** The guard is proven non-vacuous in **ninety-one** directions — ten rule-level and eighty-one model-level — each failing with an **assertion** about the behaviour it pins when that behaviour is reverted, in one sweep, with the baseline green before and after. No direction is vacuous: a direction that reverts one rule while a **second** rule still enforces the same behaviour proves nothing, and six of them were exactly that before the twenty-first review's remediation made the scanner the **single enforcer** for the structural class. Every direction now fails with a **REASON** — an assertion about the behaviour it names — and not with a parse error and not with an exception: the twenty-second review found five directions that failed only because the mutated file no longer parsed, and the sweep's own log now shows **zero** entries without a reason line; the twenty-third round found two more that failed by a **crash** rather than by an assertion, and both were re-pointed so that every one of the ninety-one directions now fails with a reason. No two directions are byte-identical, and the mislabelled one points at the code it claims to pin.

Because the reviews kept finding the model's verdict disagree with the host's, **nine independent measurement rounds** were run against the host (fourteen, fourteen, eight, twenty-six, twenty-two, seventeen, thirty-eight, twenty and ten commands; three isolated repeats every one, every verdict stable):

* the here-string, the here-document inside a body, the context family, `eval` and `alias`;
* the process-substitution family — a `<(` is a **command** and the names inside it are matched (`echo <(mpm --version)` is refused by the name matcher, `echo <(echo hello)` is allowed), an **arithmetic** context is not analysed at all (`(( <(mpm --version) ))` is allowed), and a process substitution after a command-position context is a structure the host cannot analyse (`[[ x == y ]] <(echo hi)` is refused);
* a `$( … )` inside a context — the host **does** descend into it (`[[ -n $(echo mpm --version) ]]` is refused);
* the malformed-construct class and its well-formed counterparts — thirteen refused with `compound-command`, `invalid-structure`, `command-substitution` and `dynamic-execution`, thirteen allowed;
* trailing text after a closed construct, the `case` pattern-list rules, and the empty-group spellings;
* the empty-group edge forms;
* the **quote** rules, the **doubled separator**, the empty brace body, the `case` subject and the here-document body — which **corrected three** of the model's own hypotheses: a quote pair inside a longer word is *not* a defect, a doubled separator is an *empty command slot* rather than a spelling, and a here-document's body is not analysed at all;
* the **brace group's position** — which corrected a fourth: a group is a compound command, so it may only stand where a command may stand;
* the **newline spellings** of an empty body, the pattern list's own edge forms, and the empty body with a separator — which corrected a fifth: a `case` construct is recognised by its keyword `in`, not by the text " in ".

The model now follows the host on **four hundred and fourteen** measured pairs — the **two hundred and two** measured before, the twenty-second review's own **hundred and nine NOVEL** forms and the twenty-third review's **hundred and three NOVEL** forms, every one measured three times in isolation with no flaky verdict — with a single disagreement, the recorded `coproc` divergence, and the ledger holds five divergences in all, every one of them in the safe direction. The published sixteen-command sample reproduces exactly (rows 6, 7, 8, 9, 10 and 11 refused); the published twenty-four ordinary commands produce zero refusals. Every closure the definition claims is refused by the host (twenty-three forms, the TAB forms verified with a real U+0009). A1's three hashes and origins are honest, the contradicting manifest fails closed and the unparseable one is tolerated; A2's mutation proof fails the deterministic tests and the file is restored byte-identically. The hermeticity fix passes in all three `MPM_WORKSPACE` configurations.

**The twenty-first review's remediation.** The round returned **REQUEST CHANGES** with one MAJOR, three MINOR findings and one NIT — and the MAJOR was the same shape as the one before it, one class wider. The model **allowed thirty-three** constructs the host refuses, all one family: a construct the host's portable scanner cannot **ANALYSE**. The corpus pinned **none** of them, which the review proved by mutating the model to *agree* with the host and finding the whole suite still GREEN. The fix is described in §"the twenty-first round" above.

## Remaining risks

1. **The matcher sees text.** Ten routes are known **not** to be caught, and the list is presented as the set the measurements have found — not as a proof of completeness: a name split by a substitution **or by a quote** (`$(echo mp)m` and `mp""m` both assemble it and run the binary), a name held in a variable, a script file, a shell glob, a brace expansion, a name glued to punctuation, a name behind a backslash escape (a backslash-newline line continuation is the same case), a pattern that names a *directory* of packages, and the host's refusal to match a `cd` segment's text.
2. **One place the host does not look.** The host's scanner does not analyse an **arithmetic** context, so `(( <(mpm --version) ))` is allowed. It is not valid shell syntax (bash rejects it — measured), so nothing is reached, and it is stated in the documents rather than left silent.
3. **The model is a model.** It is corrected against the host by measurement, and it agrees with the host on every form the reviews have measured — **four hundred and fourteen** pairs — but it is not a proof about the host: five measured divergences remain, all in the safe direction (the model refuses what the host allows) — `x=$(( mpm ))`, `coproc mpm { echo hi; }`, and the three `eval` forms. Each round of review has found one more construct class; a new one is a new row, not a hole. The corpus is now backed by a **class-level** table — a family, the reason the host gives it, and its representative forms — so a family the model stops catching shows up as a **failing reason** and not as a missing row; but a table is still a table of what has been thought of, and the twenty-second review's hundred and nine novel forms are what proved the previous one incomplete.
4. **The browser is not governed** by the permission rules — a `file://` URL could open a local file if a desktop browser is connected.
5. **The capability loader is not gated** by any permission action; a `skill` call runs.
6. **File-write content is read**: the permission layer reads the content of a write, so a document that merely mentions a name is refused. That cost is deliberate — a corrupted live database is worse than a findings report that cannot quote a path literally. It has a practical consequence: a report that must name a binary literally cannot be written by a shell command at all.
7. **False denials remain**: `grep -rn "mpm_memory" .` is refused (the quoted rule) while the unquoted form is allowed; `git -C <path>`, `make -C <path>` and a path ending in a binary's name are refused; `printf $'\n'` is refused by the ANSI-C opener.
8. **The host cannot analyse** some commands at all (a redirection with an empty target, a compound command, a malformed construct, trailing text after a closed construct, an unclosed substitution, a dynamic execution, a `case` whose subject is a context, a context it cannot parse, a here-document whose terminator is not found) — those are refused for the scanner's reason, not for the name's.
9. **A package-bearing export to a flat stream is refused**, not degraded: `mpm migrate --from` fails closed rather than importing a partial document.
10. **The residual list is not a jail.** The rules are a barrier, not a jail; the sandbox is the real barrier.

## Live-environment isolation

* The live substrate was **never** opened by this work — not read, not written. `mirror.jsonl` is unchanged in size and content; its mtime advances only because the operator's own `mpm-scheduler --interval 60s` checkpoints the WAL. Verified again at the end of this round: its size and mtime were observed and never changed.
* Every probe ran with `HOME`, `TMPDIR`, `MPM_WORKSPACE` and `GOCACHE` redirected into a disposable tree, with a **stand-in** binary first on `PATH`.
* The operator's checkout was never modified: 20 dirty files, HEAD `c2ed9e82`, branch `feat/runtime-migration-cutover`, the one untracked file (`cmd/dbg_main.go.tmp`) pre-dating this work. Verified again at the end of this round.
* Both worktrees are clean and carry no untracked files.

## Disclosures

* **A reviewer's probe wrote into the live database.** During an early round of PR A's review, a probe ran the CLI with `HOME` and `MPM_WORKSPACE` unset, and the CLI opened `<workspace>/src/db/mpm.db`. The reviewer disclosed it; the memory `fc3b9c0c6d206873` was written. This was the reviewer's error, not the author's, and it is the reason every later brief carries the "set `HOME` and `MPM_WORKSPACE` first, in the same command" rule.
* **Three stray files** (`file`, `out`, `b`) were created in the operator's checkout by a reviewer's redirection probes; they were removed and the checkout is back to its original 20 dirty files.
* **Reviewer probes looped to their timeout.** Three rounds ran an unbounded loop — `until [[ mpm == x ]]`, `for ((;;))` and `until false; do … done` — and the reviews reported large tool logs left under the host's own shell-log directory (309,786,303 and 342,825,588 bytes, and one of "≈104 M lines"), which were left in place under the "delete nothing outside my directory" rule. A check at 06:15 today finds **no file of that size** in the host's shell-log directory — the whole tree is 11 MB and the largest file is 2.4 MB, dated 2026-11-13 — so the harness appears to have discarded or truncated them. **Nothing was deleted by this work.** The briefs for the later rounds forbid unbounded loops explicitly.
* **A stale installed copy in a scratch HOME.** The guard's drift check resolves the installed definition through `Path.home()`, so in an isolated HOME the check SKIPS unless something installed the definition there. A copy left in a scratch HOME by an earlier run was STALE, and it turned the drift check red for a reason that was not a defect. The stale copy was removed (so the sandbox now skips), the operator's own copy was refreshed and is byte-identical to the template, and the later briefs say so explicitly.
* **The approved deletion of the stray memory `d0fece05fe6ed8d8` is NOT part of either PR.**
* A pre-existing, unrelated failure: `scripts/doc-gate-cli-limits.sh` reports `'maxRefBytes' listed in gate is missing from source` at the base commit too (a stale `REQUIRED_CLI_LIMITS` entry at `scripts/doc-gate-cli-limits.sh:74`).
* The reviewer template is installed into `~/.config/opencode/agents/mpm-reviewer.md` by `install_reviewer_agent.py --force`, because the guard test asserts that the installed definition is byte-identical to the repository's template.
* **A stale bytecode cache made the mutation sweep lie once.** The guard's source is rewritten by every mutation, and Python's cache check compares the source's mtime and size — so a rewrite inside the same second, with the same size, leaves the OLD compiled module in place and the guard answers for the previous state. It showed up as a green guard reading as red for a here-string the source allowed, and it would have hidden just as well in the other direction — a direction that should have gone red reading as GREEN. The sweep now clears the cache before every run, and the whole ninety-one-direction sweep was re-run afterwards with the corrected runner.
* **The sweep now restores the files even when it fails — and even when it is KILLED.** A crashed run left the guard MUTATED once, and the next run's baseline read RED for the wrong reason. The runner restores the guard and the template in a `finally`, so a crash can no longer leave a mutation behind. But a run that was **killed** with SIGTERM never ran its `finally` — and the next run copied the mutation into its own backup, so the corruption outlived the run that caused it, twice. The runner now handles SIGTERM and SIGINT and restores on the way out.
* **Two of the published directions failed by a CRASH rather than by an assertion.** A crash shows that the guard died, not that it caught the behaviour the direction names, so it proves less. Both were re-pointed — the `case` with no `in` KEYWORD and the group's position — and the forms they need are now pinned in the corpus, so every one of the ninety-one directions fails with a reason.
* **The guard's own file carried two MODULE-LEVEL duplicates of its own imports** — `import re` and `from pathlib import Path`, each written twice — in the very file that adds the assertion against duplicates. The housekeeping assertion matched only `^    def name(self)`, so it could not see them; it now covers every module-level definition, and the duplicates are gone.
* **A dead copy of a test method was removed.** The script that inserts the class-level family table had been run twice, and Python keeps the LAST definition — so the first copy was never run: it read like a claim and changed nothing. The guard now asserts that **no method is defined twice**, so the same thing cannot happen again unnoticed.
* **The author's own layer is not the reviewer's layer.** The author's shell carries the **scanner** but not the **binary-name deny rules** — `mpm --version` runs in it, and is refused in the reviewer's. So every structural measurement made from the author's layer is a faithful measurement of the *scanner*, while any measurement about the *name matcher* has to be made in the reviewer's layer, where the rules live. The rows that depend on the name matcher were measured there.
* **A `case` PATTERN LIST is exempt from the name matcher at any depth — measured in the reviewer's layer.** `case x in <BIN>) echo hi;; esac`, the nested `case y in z) case w in <BIN>) echo hi;; esac;; esac`, its `;`-spelled sibling, and the single `case` followed by another command are all **ALLOWED 4/4** with no refusal — while `<BIN> --version` in a nested **body**, `" <BIN>)"` in a nested **command position**, `echo $(<BIN>)` and the bare `<BIN> --version` are all **REFUSED 3/3**. So the carve-out is **positional** — a pattern list, at any nesting depth — and it does not extend to a command position one level away. The rule `* <BIN>)*` is the load-bearing one for a pattern-list-looking literal, and the whole-text rule would match the raw text if it were applied blindly: the host parses the construct and excludes the pattern list.
* **The write path and the shell path scan differently.** The same text that is **ALLOWED** as a shell command is **REFUSED** as file content, because the write path applies the raw-text rules without the structural carve-out. A corpus that quotes these commands literally cannot be written by a shell command — the `<BIN>` placeholder exists for that.
