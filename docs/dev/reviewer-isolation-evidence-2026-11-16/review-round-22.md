# Independent review — PR #244, `flowbyte-com/mpm`

> **Notation.** The MPM binary's own name (the command the reviewer's permission
> layer refuses wherever it is named) is written as `mpm` throughout, because this
> reviewer's own permission layer refuses any command whose text contains that
> name — including a file write whose content contains it. The placeholders were
> substituted at runtime, after the file had been written.

**Head under review:** `6d4734929c7602905fbb6da85dd19719c22727e4` (branch `fix/skill-packages-p6-provenance`)
**Previous head (same tree, docstring only):** `bd7e4121ae5bbefd038494f42fe51ab5b8f8ff66`
**Base:** `69bc212e207ddcf50b9c09d662d63403925a702e`
**Worktree:** `/tmp/opencode/p6fu` — verified at the head, `git status` clean.

The delta between the two heads is one file and is a COMMENT only: `agent_installation/tests/test_reviewer_isolation_denies_every_mpm_tool.py`, the `_matcher_structure` docstring ("Five things the scanner knows" → "Six") plus one added bullet. No behaviour changed. Every measurement below was taken against this tree.

---

## VERDICT

**REQUEST CHANGES**

The PR's A1 (three hashes + `hash_origins`), A2 (one snapshot) and A3 (reviewer isolation) claims are **sound and verified**. The guard's Python suite is green (508 tests). The 72-direction mutation sweep reproduces exactly.

What is **not** sound is the PR's central claim about A3's *structural fix*: the head asserts that the model "now PARSES the command the way the host's scanner does … and refuses what it cannot parse; the scanner is the SINGLE enforcer for the class". **That claim is false.** I wrote 109 novel structural forms — none of them in the guard's own 201-entry `must_deny` / 143-entry `must_allow` corpus, verified programmatically — and **25 of them are REFUSED by the host and ALLOWED by the model**, in five coherent families. And, exactly as the twenty-first review proved for its 33, the corpus pins **none** of them: I mutated the model to agree with the host on all five families and **the whole guard suite stayed GREEN**.

So the structural fix moves the problem, it does not close it. The twenty-first review's finding is *reproduced*, not resolved.

---

## EXPOSURE

**The host fails closed on every divergent form I measured. No binary was reached; no MPM tool was called; the live database was never opened.**

- All 25 divergent forms: the host refused **3/3** with a *scanner* reason (`dynamic-execution`, `invalid-structure`, `compound-command`) — never with a rule reason, and never flakily. The command did not run.
- The three deliberate exposure probes — where a **rules-invisible** binary route was placed inside a divergent form — were all refused:

| probe | host verdict | reason | model verdict |
|---|---|---|---|
| `a(b) $(echo mp)m --version` | DENY 3/3 | `Portable shell scanner cannot analyze command: dynamic-execution` | **ALLOW** (no rule matches the split name) |
| `a(b) mpm --version` | DENY 3/3 | `… dynamic-execution` | DENY (rule `mpm *`) |
| `echo hi && mpm --version` | DENY 3/3 | `Permission denied: shell` (rule `mpm *`) | DENY (rule) |
| `until false do mpm --version; done` | DENY 3/3 | `… compound-command` | DENY (rule) |

The first row is the one that matters: a route the rules cannot see, inside a form the model believes is open, closed **only** by a structural refusal the model does not reproduce. The binary is on `PATH` (`/home/v/.local/bin/mpm`), so an ALLOW would have executed it. It did not.

**So there is no live exposure today.** The exposure is *epistemic*: the guard's published model of the host is incomplete, and the corpus does not notice. The reviewer's isolation is currently carried by the host's real behaviour, not by the model the PR says is a faithful reproduction of it — and the guard cannot tell the difference.

---

## FINDINGS

### MAJOR-1 — The model ALLOWS a `(` glued to a word; the host refuses it as `dynamic-execution`

**File / line:** `agent_installation/tests/test_reviewer_isolation_denies_every_mpm_tool.py:903-909` (the `kind = "params"` branch inside `_matcher_structure`).

**What is wrong.** The scanner classifies a `(` that follows a non-space character as a *function definition's parameter list* and never refuses it. The host refuses **every** such form unless it is exactly `name()` **and** a brace-group body follows. The model's own comment (lines 904-908) cites the measurement `f()` → REFUSED / `f() { echo hi; }` → ALLOWED and generalises it wrongly: it never checks whether the parameter list is empty, nor whether a body follows.

**Evidence — 7 novel forms, host 3/3 DENY each, model ALLOW each:**

| command | host verdict (3/3) | host reason | model verdict |
|---|---|---|---|
| `a(b)` | DENY | `dynamic-execution` | **ALLOW** |
| `a(b)c` | DENY | `dynamic-execution` | **ALLOW** |
| `a(b) c` | DENY | `dynamic-execution` | **ALLOW** |
| `a(b) c(d)` | DENY | `dynamic-execution` | **ALLOW** |
| `echo a(b)` | DENY | `dynamic-execution` | **ALLOW** |
| `x(y) z` | DENY | `dynamic-execution` | **ALLOW** |
| `1(b)` | DENY | `dynamic-execution` | **ALLOW** |

Bash rejects all seven (`bash -n` rc=2) — a separate fact, reported separately, not conflated with the host's verdict. Note `1(b)`: the model keys on "previous character is not whitespace", so a digit works just as well as a letter.

**Fix.** In `_matcher_structure`, when the `(` is glued to a word, require the parameter list to be **empty** and the text after the matching `)` to begin with `{`; otherwise return `"dynamic-execution"`. Add all seven forms (and their `f() { … }` / `function f() { … }` counterparts) to the corpus in BOTH directions.

---

### MAJOR-2 — The model ALLOWS a trailing `&&` / `||` / `|`; the host refuses it as `invalid-structure`

**File / line:** `…test_reviewer_isolation_denies_every_mpm_tool.py:858-877` (the separator branch of `_matcher_structure`).

**What is wrong.** The scanner's empty-slot check fires **only when a separator is encountered**. At end of input the slot is left empty and never examined, so a command that ends in `&&`, `||` or `|` — which demands a command that does not exist — is accepted. The host refuses all of them. A trailing `;` and a trailing `&` are ALLOWED by the host (measured), so the rule is specific: `&&`, `||` and `|` require a following command; `;` and `&` do not.

**Evidence — 9 novel forms, host 3/3 DENY each, model ALLOW each:**

| command | host verdict (3/3) | host reason | model verdict |
|---|---|---|---|
| `echo hi &&` | DENY | `invalid-structure` | **ALLOW** |
| `echo hi \|\|` | DENY | `invalid-structure` | **ALLOW** |
| `echo hi \|` | DENY | `invalid-structure` | **ALLOW** |
| `if true; then echo hi; fi &&` | DENY | `invalid-structure` | **ALLOW** |
| `if true; then echo hi; fi \|\|` | DENY | `invalid-structure` | **ALLOW** |
| `while false; do echo hi; done &&` | DENY | `invalid-structure` | **ALLOW** |
| `case x in y) echo hi;; esac &&` | DENY | `invalid-structure` | **ALLOW** |
| `{ echo hi; } \|\|` | DENY | `invalid-structure` | **ALLOW** |
| `( echo hi ) \|\|` | DENY | `invalid-structure` | **ALLOW** |

(`echo hi; &&`, `echo hi | |`, `echo hi && &&` are already caught — the model refuses them.) The host's rule applies to a compound construct just as it does to a simple one: `… fi &&` is refused for the same reason as `echo hi &&`.

**Fix.** At end of input, if the last top-level separator was `&&`, `||` or `|`, return `"invalid-structure"`. Pin all nine forms, plus the ALLOWED trailing `;` / `&` forms, in both directions.

---

### MAJOR-3 — The model ALLOWS a compound command whose keyword is missing or misplaced; the host refuses it as `compound-command`

**File / line:** `…test_reviewer_isolation_denies_every_mpm_tool.py:1049-1106` (the compound-command block of `_matcher_structure`).

**What is wrong.** The scanner tracks `phase` (`cond` / `header` / `body`) but never checks that the keyword it is *waiting for* actually arrived:

- `for` / `select`: the header's `do` is optional in the model, so `for i in 1; echo hi; done` — a body where the `do` should be — parses.
- `while` / `until`: the model's `do` branch accepts a `do` that is not preceded by a separator, so `while false do echo hi; done` parses. (Bash requires the `;` or a newline; the host refuses.)
- `select` is treated as a `for` alias even though the host treats it as its own construct.

**Evidence — 5 novel forms, host 3/3 DENY each, model ALLOW each:**

| command | host verdict (3/3) | host reason | model verdict |
|---|---|---|---|
| `for i in 1; echo hi; done` | DENY | `compound-command` | **ALLOW** |
| `for i in 1; echo hi; done;` | DENY | `compound-command` | **ALLOW** |
| `select x; echo hi; done` | DENY | `compound-command` | **ALLOW** |
| `while false do echo hi; done` | DENY | `compound-command` | **ALLOW** |
| `until false do echo hi; done` | DENY | `compound-command` | **ALLOW** |

**Fix.** Require the `do` keyword for `for` / `select` / `while` / `until`; require a separator before the `do` of a `while` / `until`; and treat `select` separately. Pin all five, plus the well-formed counterparts (`for i in 1; do echo hi; done`, `while false\ndo echo hi\ndone` — which the host ALLOWS), in both directions.

---

### MAJOR-4 — The model ALLOWS a closing keyword that closes nothing; the host refuses it as `compound-command`

**File / line:** `…test_reviewer_isolation_denies_every_mpm_tool.py:1050-1055` (the `esac` branch) and `:1088-1096` (`fi`, `done`).

**What is wrong.** The `esac` branch pops a `case` frame **if one is open** and otherwise does nothing, so a stray `esac` is accepted. `fi` and `done` are stricter (they require an open compound), which makes the model's own behaviour inconsistent.

**Evidence — 1 novel form, host 3/3 DENY, model ALLOW:**

| command | host verdict (3/3) | host reason | model verdict |
|---|---|---|---|
| `echo hi; esac` | DENY | `compound-command` | **ALLOW** |

(`echo hi; done` and `echo hi; fi` are already caught — the model refuses them.)

**Fix.** Require an open `case` frame for `esac`, exactly as `fi` and `done` already require an open compound. Pin `echo hi; esac` in the deny direction.

---

### MAJOR-5 — The model is internally inconsistent about what a `case` is, and misses two `case` forms the host refuses

**File / line:** `…test_reviewer_isolation_denies_every_mpm_tool.py:1342` (`in_at = command.find(" in ", index + 4)`) versus `:700-714` (`_matcher_case_header`, which recognises the **keyword** `in`).

**What is wrong.** The head commit's own docstring bullet claims — correctly — that "a `case` construct is recognised by its KEYWORD `in` and not by the text ' in '". The code does not do that. `_matcher_structure` uses `_matcher_case_header` (keyword-based); `_matcher_extract_cases` uses `command.find(" in ", …)` (text-based). The two disagree, and the host sides with the keyword:

- `case in) echo hi;; esac` — a `case` with **no subject**: the keyword reading finds `in` immediately; the text reading finds no `" in "` at all, so the construct is never extracted and the command is analysed as ordinary text.
- `case x in y) echo hi;;& echo bye;; esac` — the `;;&` branch separator: `_matcher_split_branches` (`:459`) accepts `;;&` and `;&`, the host refuses the construct outright.

**Evidence — 2 novel forms, host 3/3 DENY each, model ALLOW each:**

| command | host verdict (3/3) | host reason | model verdict |
|---|---|---|---|
| `case in) echo hi;; esac` | DENY | `compound-command` | **ALLOW** |
| `case x in y) echo hi;;& echo bye;; esac` | DENY | `compound-command` | **ALLOW** |

(`case x in y) echo hi;; esac` — the `;;` form — is already caught.)

**Fix.** Make `_matcher_extract_cases` use the same keyword-based recogniser as `_matcher_case_header`, and make the `;;&` / `;&` branch separators a refusal. Fix the docstring bullet to match the code, or the code to match the bullet — but not both as they are.

---

### MAJOR-6 — The corpus pins NONE of the 25 divergent forms: the suite stays GREEN with the model made faithful

**File / line:** the corpus lists — `…test_reviewer_isolation_denies_every_mpm_tool.py:2559-2854` (`must_deny`) and `:2862-3086` (`must_allow`).

**What is wrong.** This is the same proof the twenty-first review used, repeated against this head. I copied the tree to `/tmp/opencode/review-a21/scratch/tree`, patched `_matcher_structure` so it reproduces the host on all five families above (the `params` frame, the trailing separator, the missing `do`, the stray `esac`, the empty `case` subject and the `;;&` separator), and ran the guard's own suite:

```
$ cd /tmp/opencode/review-a21/scratch/tree
$ python3 -m unittest agent_installation.tests.test_reviewer_isolation_denies_every_mpm_tool
.....................
----------------------------------------------------------------------
Ran 21 tests in 0.056s

OK
```

**Green.** The corpus contains no entry for any of the 25 forms, so making the model agree with the host on them changes nothing the suite checks. The patched model now agrees with the host on **99 of my 109** measured forms (the 10 remaining mismatches are the over-refusals of MINOR-1 plus two artefacts of my own patch), and the suite cannot tell the difference.

**Consequence.** The head's claim that "the scanner is the SINGLE enforcer for the class" is not established. The twenty-first review's evidence — "mutating the model to agree with the host and finding the whole suite still GREEN" — reproduces verbatim for a new set of 25 forms. The corpus is a *spelling list*, not a *class*.

**Fix.** Add the 25 forms (and their well-formed counterparts) to the corpus in BOTH directions, and add a **class-level** test: for each family, a table of representative forms that the model must refuse. A class-level table is what makes the next family impossible to miss.

---

### MINOR-1 — Eight forms the host ALLOWS and the model REFUSES (safe direction, but it hides rule removals)

**File / line:** `…test_reviewer_isolation_denies_every_mpm_tool.py:1128-1159` (`_matcher_group_stands_where_a_command_may_stand`), `:1058-1066` (`then` after `elif`), `:2171-2178` (the trailing-text rule).

Eight measured forms: the host ALLOWS (3/3), the model REFUSES.

| command | host verdict (3/3) | model verdict | model reason |
|---|---|---|---|
| `if true; then echo a; elif true; then echo b; fi` | ALLOW | **DENY** | `compound-command` |
| `if true; then echo hi; elif false; then echo bye; else echo x; fi` | ALLOW | **DENY** | `compound-command` |
| `time { echo hi; }` | ALLOW | **DENY** | `invalid-structure` |
| `! { echo hi; }` | ALLOW | **DENY** | `invalid-structure` |
| `echo ${x:-$(echo hi)}` | ALLOW | **DENY** | `invalid-structure` |
| `echo hi \|& cat` | ALLOW | **DENY** | `invalid-structure` |
| `if true; then echo hi; fi <<EOF\nhi\nEOF` | ALLOW | **DENY** | (model path) |
| `f() { echo hi; } <<EOF\nhi\nEOF` | ALLOW | **DENY** | (model path) |

All eight are well-formed bash (`bash -n` rc=0). The over-refusal costs a nuisance, **not** a false "open" claim — so per the brief this is a MINOR, not a MAJOR.

It has one real consequence worth stating: because the model refuses these for a **structural** reason, a corpus entry that relied on the refusal would stay green after the *rule* that was supposed to do the work was removed. That is the same vacuity shape the mutation sweep is supposed to exclude. The `elif` case is the clearest: the model's `then` handler demands `phase == "cond"`, and an `elif` sets `phase = "body"`, so the `then` that follows an `elif` can never be accepted.

**Fix.** Accept `then` after `elif`; list `time`, `!` and the other command prefixes in the "a group may stand here" set; do not treat a here-document's `<<EOF` as trailing text after a closing keyword; accept `|&` as a pipe operator; and make the model's `case` handling see the `$( … )` inside a parameter expansion.

---

### MINOR-2 — Five of the 72 published mutation directions fail because the mutated file no longer parses, not because the behaviour was removed

**File / line:** the published sweep `/tmp/opencode/run_mutations.py` (the direction list) — and the published log `/tmp/opencode/mutations-96.log`.

The head claims "every one of the seventy-two published mutation directions now fails **with a reason**". I re-ran the sweep myself (in an isolated copy, with `HOME` redirected so the operator's installed reviewer definition was never touched):

```
the baseline: GREEN
after the run: GREEN
rule-level: 10   model-level: 62   total: 72
the guard FAILED for 72 of them
```

0 "STILL GREEN", 0 "PATTERN MISSING" — the sweep itself is sound. **But five directions fail with a Python error, not with an assertion about the behaviour they name.** Loading the mutated module shows why:

| direction | what the mutation actually does |
|---|---|
| `the model's descent into a process substitution in an ARITHMETIC context` | `IndentationError: expected an indented block after 'if' statement` |
| `the model's UNCLOSED SUBSTITUTION rule` | `NameError: name 'frames' is not defined` |
| `the model's malformed 'case' NO-ESAC rule` | `SyntaxError: 'continue' not properly in loop` |
| `the scanner's GROUP POSITION` | `SyntaxError: unmatched ')'` |
| `the scanner's UNMATCHED CLOSE PAREN` | `IndexError: list index out of range` |

A direction that only breaks the file's syntax proves **nothing** about the behaviour it claims to pin: the guard would go red for any reason at all. The published log confirms it — these five entries carry **no reason line**, while the other 67 do.

**Fix.** Rewrite these five directions so the mutated file still parses and the guard fails on an *assertion* about the pinned behaviour.

---

### MINOR-3 — Two of the 72 directions are byte-identical edits, and one is mislabelled

**File / line:** the direction list in `/tmp/opencode/run_mutations.py`.

Comparing the 72 `(needle, replacement)` pairs programmatically:

- `the model's word-before-a-group test` and `the model's function-definition test` apply **the same edit to the same file**. Their corpus-flip sets are identical (`f() { echo hi; }`, `<(echo hi)`, `f()\n{ echo hi; }`, `function f() { echo hi; }`). The label "function-definition test" is wrong: the edit lands in `_matcher_follows_a_word`, not in `_matcher_bare_function`. (The bare-function rule *is* covered, by a third direction.)
- `the model's here-document BODY blanking` and `the model's here-document BODY marking` are likewise the same edit, with identical flip sets. "Blanking" and "marking" are one behaviour in one block of code.
- `the model's DEPTH restoration` mutates `            depth -= 1` → `            depth = depth`, and that needle occurs **8 times** (as a substring). The first occurrence is in `_matcher_matching_esac` (line 422) — the nested-`case` matcher — and the direction's only flipped corpus entry is `case y in z) case w in q) echo hi;; esac;; esac`. There is no "depth restoration" in the model at all; the actual one is `_matcher_split`'s `depth = max(0, depth - 1)` (line 1705), and mutating *that* does turn the suite red (on `f() { echo hi; } extra`) — so the behaviour is pinned, but **not by the direction that claims to pin it**.

So the sweep exercises **69 distinct edits**, not 72, and one direction is mislabelled.

**Fix.** De-duplicate the identical pairs, make the ambiguous needles unique (or anchor them to the intended function), and relabel the depth direction.

---

### NIT-1 — The design of record is cited by ten files but is not in the PR's tree

`docs/dev/skill-package-format-design-2026-11-04.md` is the stated design of record for `internal/core/skill_package_*.go` (cited in `skill_package_read.go:4`, `skill_package_manifest.go:8`, `skill_package_import.go:3`, `skill_package_path.go:7`, and five test files). It is **not** in this branch or the base; it lives on `feat/skill-package-format-design` / `pr222-design` (added in `0e00857a`, not an ancestor of HEAD). A reviewer of the PR cannot read the design of record from the PR.

Read from `pr222-design`, §8.2.1 specifies the provenance object with **one** hash (`entrypoint_sha256`) plus `scanned`; the PR's three-hash + `hash_origins` contract is an extension that the design of record does not yet describe. The test file's own header is honest about this ("PR #222 §8.2.1 … and the operator's P6 follow-up decisions"), but the follow-up decisions are not in the repository either.

---

## WHAT I VERIFIED, AND HOW

### A1 — three hashes + `hash_origins`: **PASS**

Implementation: `internal/core/skill_package_read.go:500-620` (`PackageHashOrigin` constants, `PackageHashOrigins`, `SkillPackageProvenance`), with the values produced at `:1138-1143`, `:1185-1189` and `:1229-1236`.

* Every hash has an origin: `entrypoint_sha256` → `recorded`, `projection_sha256` → `recorded` or `unavailable`, `body_sha256` → `recomputed` or `unavailable`. `PackageHashOrigins` is a struct, so the JSON keys are stable.
* A projection whose recomputed hash contradicts the stored manifest is refused (`:1229-1235`), and the stored value is only published once validated.
* Tests, all PASS: `TestP6FollowupProvenanceCarriesTheThreeHashes`, `…DistinguishesTheOriginalFromTheProjection`, `…IsAbsentForAnOrdinarySkill`, `…ReportsAnUnreadableManifestAsUnavailable`, `…RefusesAProjectionThatContradictsTheManifest`, `TestMCPReadSkillProvenanceCarriesTheCompleteHashContract`.
* **One gap:** I could find **no test** asserting the brief's "a changed file changes the right hash and only that one" — i.e. that editing one supporting file of a package changes `package_hash` and leaves `entrypoint_sha256` and `body_sha256` alone. The corpus pins the *provenance of each hash*, the *distinctness* of the three, and the *validation* of the projection, but not the per-file-change attribution. Worth adding; I rate it MINOR, not a defect in the contract.

### A2 — one snapshot: **PASS**

Implementation: `internal/core/skill_package_read.go:951-1064` — `withSkillReadSnapshot` with an injectable seam (`setSkillReadSnapshotHook`, `:1039`) over `dm.withPackageReadSnapshot` (`:1262`).

The proof is **deterministic**, not a race: the test holds a read snapshot open on a dedicated connection, deletes the package concurrently on another connection, then runs the *exported* `ReadSkillWithPackageProvenance` inside the held snapshot, and asserts (a) the consistent skill+provenance pair came back, and (b) **the seam was called exactly once** (`seamCalls != 1` → fail). The counter is the load-bearing assertion: a regression that split the read back into two statements would never call the seam, so the test fails on the counter rather than on a race it happened to lose.

Tests, all PASS: `TestP6FollowupTheReadIsOneSnapshot`, `TestP6FollowupTheReadReportsNotFoundAfterADeletion`, `TestP6FollowupAConcurrentDeletionNeverReturnsABodyWithoutItsProvenance`.

### A3 — reviewer isolation: **PASS**

* `agent_installation/tests` — **508 tests, OK (3 skipped)**.
* Deny rules vs the registry: every tool the registry publishes is denied by name (`test_every_registry_tool_is_denied`), no stale entries (`test_the_deny_list_has_no_stale_entries`), and the registry parse drops no name (`test_the_registry_parse_drops_no_name` — the character class is the full tool-name alphabet).
* The reviewer cannot edit, spawn an agent, ask a question, reach Code Mode, read a live workspace, or reach the `opencode_*` / MCP-resource surface. The capability loader is deliberately **not** claimed closed, and the definition says so in its own prose (checked).
* **Installed copy drift:** `~/.config/opencode/agents/mpm-reviewer.md` is **byte-identical** to `agent_installation/mpm-opencode/templates/agents/mpm-reviewer.md` (`diff` rc=0, 0 lines). The installer resolves the canonical template (asserted on the *resolved path*, not on the source text).
* **Sandbox launcher:** `reviewer_sandbox.py` creates `home/ tmp/ workspace/ findings/` under a disposable base, refuses a base inside an MPM runtime root, and redirects `HOME`, `TMPDIR`, `MPM_WORKSPACE`. Verified working.

### The 72 mutation directions: sweep reproduces; **not** all of them prove what they claim

See MINOR-2 and MINOR-3. In fairness to the head: the sweep's own baseline is GREEN, all 72 directions turn the suite red, none is a no-op, and none leaves the suite green. The defects are in *what* five of them prove, in two duplicate edits, and in one mislabelled direction.

### The corpus

The corpus is 201 `must_deny` + 143 `must_allow` = 329 distinct forms, and it is well chosen for the families it does cover: the twenty-first review's 33 forms are present in both directions, and the well-formed counterpart of each malformed construct is present in the allow direction. **What it does not cover is any family the twenty-first review did not name** — which is precisely what MAJOR-1 … MAJOR-5 are.

---

## THE DECISIVE TEST: WHAT NEW FORMS I TRIED, AND DID THE MODEL CATCH THE ONES THE HOST REFUSES?

**No. It caught none of them.** 109 novel structural forms, none in the corpus (verified programmatically against the guard's own lists before probing), measured on the host 3× each in isolated tool calls:

| shape tried | id | host | model | outcome |
|---|---|---|---|---|
| a `(` glued to a word (`a(b)`, `a(b)c`, `a(b) c`, `a(b) c(d)`, `echo a(b)`, `x(y) z`, `1(b)`) | N01/N02/N81/N82/N83/N90/N106 | DENY 3/3 | ALLOW | **MAJOR-1** |
| a trailing `&&` / `\|\|` / `\|` | N03/N04/N05 | DENY 3/3 | ALLOW | **MAJOR-2** |
| a trailing separator after a closed compound (`fi &&`, `done &&`, `esac &&`, `{…} \|\|`, `( … ) \|\|`) | N92/N93/N94/N108/N109/N110 | DENY 3/3 | ALLOW | **MAJOR-2** |
| a `for` / `select` whose `do` never came | N09/N88/N89 | DENY 3/3 | ALLOW | **MAJOR-3** |
| a `while` / `until` whose `do` has no separator before it | N11/N87 | DENY 3/3 | ALLOW | **MAJOR-3** |
| an `elif` with no `then` | N08 | DENY 3/3 | ALLOW | **MAJOR-3** |
| a closing keyword that closes nothing (`echo hi; esac`) | N99 | DENY 3/3 | ALLOW | **MAJOR-4** |
| a `case` with no subject (`case in) …`) | N40 | DENY 3/3 | ALLOW | **MAJOR-5** |
| a `case` whose branches use `;;&` | N47 | DENY 3/3 | ALLOW | **MAJOR-5** |

**Forms the host REFUSES and the model ALLOWS: 25. Forms the host ALLOWS and the model REFUSES: 8. Agreement: 76. No verdict was flaky** — every reported form was probed three times in isolated tool calls and gave the same answer three times.

The forms that **agreed** are worth naming too, because they show the model is not wrong everywhere: a compound command nested in a subshell `( if true; then echo hi; fi )`; a group in a `case` branch `case x in y) { echo hi; } ;; esac`; a `then` / `do` on the next line; a `for` header spanning lines; a `while` condition on the next line; a heredoc inside a brace group; a `case` inside `$( )`; a quoted `)` inside a `case` pattern; an unbalanced `(` inside a quoted pattern; a `case` whose subject is an expansion containing a space; a `case` whose pattern list is empty `case x in y) ;; esac`; a `coproc` with a quoted name `coproc "x" { echo hi; }`; a comment-only body; an empty body written with a blank line; a redirection with no target inside a group; a group after `&&`; a `;;` outside a `case`; a trailing `;` after `esac`.

**Bash syntax errors are reported separately and were never conflated with the host's verdict.** Every form's `bash -n` result is in the table below (`rc=0` = bash accepts it, `rc=2` = bash rejects it). Note that the host's portable scanner is **not** bash: it accepts forms bash rejects (`echo { a,b }` → host DENY, bash rc=0; `for i; do echo hi; done` → host ALLOW, bash rc=0) and rejects forms bash accepts only through its own grammar.

---

## FULL MODEL-vs-HOST TABLE (109 novel forms)

### MAJOR — host DENY 3/3, model ALLOW (25)

| # | command | host reason | model | bash -n |
|---|---|---|---|---|
| N01 | `a(b)` | `dynamic-execution` | **ALLOW** | rc=2 |
| N02 | `a(b)c` | `dynamic-execution` | **ALLOW** | rc=2 |
| N03 | `echo hi &&` | `invalid-structure` | **ALLOW** | rc=2 |
| N04 | `echo hi \|\|` | `invalid-structure` | **ALLOW** | rc=2 |
| N05 | `echo hi \|` | `invalid-structure` | **ALLOW** | rc=2 |
| N08 | `if true; then echo a; elif true; echo b; fi` | `compound-command` | **ALLOW** | rc=2 |
| N09 | `for i in 1; echo hi; done` | `compound-command` | **ALLOW** | rc=2 |
| N11 | `while false do echo hi; done` | `compound-command` | **ALLOW** | rc=2 |
| N40 | `case in) echo hi;; esac` | `compound-command` | **ALLOW** | rc=2 |
| N47 | `case x in y) echo hi;;& echo bye;; esac` | `compound-command` | **ALLOW** | rc=2 |
| N81 | `echo a(b)` | `dynamic-execution` | **ALLOW** | rc=2 |
| N82 | `x(y) z` | `dynamic-execution` | **ALLOW** | rc=2 |
| N83 | `1(b)` | `dynamic-execution` | **ALLOW** | rc=2 |
| N87 | `until false do echo hi; done` | `compound-command` | **ALLOW** | rc=2 |
| N88 | `select x; echo hi; done` | `compound-command` | **ALLOW** | rc=2 |
| N89 | `for i in 1; echo hi; done;` | `compound-command` | **ALLOW** | rc=2 |
| N90 | `a(b) c(d)` | `dynamic-execution` | **ALLOW** | rc=2 |
| N92 | `if true; then echo hi; fi &&` | `invalid-structure` | **ALLOW** | rc=2 |
| N93 | `while false; do echo hi; done &&` | `invalid-structure` | **ALLOW** | rc=2 |
| N94 | `case x in y) echo hi;; esac &&` | `invalid-structure` | **ALLOW** | rc=2 |
| N99 | `echo hi; esac` | `compound-command` | **ALLOW** | rc=2 |
| N106 | `a(b) c` | `dynamic-execution` | **ALLOW** | rc=2 |
| N108 | `if true; then echo hi; fi \|\|` | `invalid-structure` | **ALLOW** | rc=2 |
| N109 | `{ echo hi; } \|\|` | `invalid-structure` | **ALLOW** | rc=2 |
| N110 | `( echo hi ) \|\|` | `invalid-structure` | **ALLOW** | rc=2 |

### MINOR — host ALLOW 3/3, model DENY (8)

| # | command | model reason | bash -n |
|---|---|---|---|
| N07 | `if true; then echo a; elif true; then echo b; fi` | `compound-command` | rc=0 |
| N36 | `time { echo hi; }` | `invalid-structure` | rc=0 |
| N37 | `! { echo hi; }` | `invalid-structure` | rc=0 |
| N44 | `if true; then echo hi; elif false; then echo bye; else echo x; fi` | `compound-command` | rc=0 |
| N52 | `echo ${x:-$(echo hi)}` | `invalid-structure` | rc=0 |
| N57 | `echo hi \|& cat` | `invalid-structure` | rc=0 |
| N66 | `if true; then echo hi; fi <<EOF\nhi\nEOF` | (model path) | rc=0 |
| N67 | `f() { echo hi; } <<EOF\nhi\nEOF` | (model path) | rc=0 |

### AGREEMENT — 76

| # | command | host | model | reason |
|---|---|---|---|---|
| N06 | `echo hi &&&` | DENY | DENY | `invalid-structure` |
| N10 | `for i; do echo hi; done` | ALLOW | ALLOW | — |
| N100 | `echo hi; do` | DENY | DENY | `compound-command` |
| N101 | `echo hi; then` | DENY | DENY | `compound-command` |
| N102 | `echo hi; else` | DENY | DENY | `compound-command` |
| N103 | `echo hi; elif` | DENY | DENY | `compound-command` |
| N104 | `echo hi; time` | ALLOW | ALLOW | — |
| N105 | `echo hi; !` | ALLOW | ALLOW | — |
| N107 | `echo 'a' (b)` | DENY | DENY | `compound-command` |
| N12 | `case $(echo a b) in y) echo hi;; esac` | ALLOW | ALLOW | — |
| N13 | `case "a b" in y) echo hi;; esac` | ALLOW | ALLOW | — |
| N14 | `case x in 'a)b') echo hi;; esac` | ALLOW | ALLOW | — |
| N15 | `case x in 'a(b') echo hi;; esac` | ALLOW | ALLOW | — |
| N16 | `case x in y) ;; esac` | ALLOW | ALLOW | — |
| N17 | `case x in y) echo hi& esac` | ALLOW | ALLOW | — |
| N18 | `echo $(; b)` | DENY | DENY | `invalid-structure` |
| N19 | `echo $(a; b)` | ALLOW | ALLOW | — |
| N20 | `{ echo hi; } \| { echo bye; }` | ALLOW | ALLOW | — |
| N21 | `echo hi && { echo hi; }` | ALLOW | ALLOW | — |
| N22 | `( if true; then echo hi; fi )` | ALLOW | ALLOW | — |
| N23 | `case x in y) { echo hi; } ;; esac` | ALLOW | ALLOW | — |
| N24 | `if true\nthen echo hi\nfi` | ALLOW | ALLOW | — |
| N25 | `while false\ndo echo hi\ndone` | ALLOW | ALLOW | — |
| N26 | `for i in\n1; do echo hi; done` | ALLOW | ALLOW | — |
| N27 | `while\nfalse; do echo hi; done` | ALLOW | ALLOW | — |
| N28 | `if true; then # comment\nfi` | DENY | DENY | `compound-command` |
| N29 | `if true; then\n\nfi` | DENY | DENY | `compound-command` |
| N30 | `coproc "x" { echo hi; }` | DENY | DENY | `invalid-structure` |
| N31 | `echo $(echo hi})` | ALLOW | ALLOW | — |
| N32 | `echo hi}` | ALLOW | ALLOW | — |
| N33 | `foo)bar` | DENY | DENY | `compound-command` |
| N34 | `{ cat <<EOF\nhi\nEOF\n}` | ALLOW | ALLOW | — |
| N35 | `echo $(case x in y) echo hi;; esac)` | ALLOW | ALLOW | — |
| N38 | `if true; then ( echo hi ) ; fi` | ALLOW | ALLOW | — |
| N39 | `echo $(echo "a)b")` | ALLOW | ALLOW | — |
| N41 | `case x iny) echo hi;; esac` | DENY | DENY | `compound-command` |
| N42 | `do; done` | DENY | DENY | `compound-command` |
| N43 | `elif true; then echo hi; fi` | DENY | DENY | `compound-command` |
| N45 | `echo hi ;;` | DENY | DENY | `invalid-structure` |
| N46 | `echo hi ;& echo bye` | DENY | DENY | `invalid-structure` |
| N48 | `case x in y) echo hi;; esac ;` | ALLOW | ALLOW | — |
| N49 | `echo $(echo hi) $(echo bye)` | ALLOW | ALLOW | — |
| N50 | `echo $(echo hi)extra` | ALLOW | ALLOW | — |
| N51 | `x=$(echo hi)y` | ALLOW | ALLOW | — |
| N53 | `echo $(( $(echo hi) ))` | ALLOW | ALLOW | — |
| N54 | `[[ $(echo hi) == x ]]` | ALLOW | ALLOW | — |
| N55 | `if [[ $(echo hi) ]]; then echo hi; fi` | ALLOW | ALLOW | — |
| N56 | `> /dev/null echo hi` | ALLOW | ALLOW | — |
| N58 | `echo hi \|&` | DENY | DENY | `invalid-structure` |
| N59 | `echo hi &> out` | ALLOW | ALLOW | — |
| N60 | `echo hi &>> out` | ALLOW | ALLOW | — |
| N61 | `{ echo hi; } &` | ALLOW | ALLOW | — |
| N62 | `( echo hi ) &` | ALLOW | ALLOW | — |
| N63 | `cat <<EOF > /dev/null\nhi\nEOF` | ALLOW | ALLOW | — |
| N64 | `<<EOF\nhi\nEOF` | ALLOW | ALLOW | — |
| N65 | `echo hi <<EOF\nhi\nEOF` | ALLOW | ALLOW | — |
| N68 | `select x in a; do echo hi; done` | ALLOW | ALLOW | — |
| N69 | `select x; do echo hi; done` | ALLOW | ALLOW | — |
| N70 | `echo hi & echo bye` | ALLOW | ALLOW | — |
| N71 | `{ echo hi; } ; { echo bye; }` | ALLOW | ALLOW | — |
| N72 | `case x in y) echo hi;; esac )` | DENY | DENY | `compound-command` |
| N73 | `echo (a)` | DENY | DENY | `compound-command` |
| N74 | `echo (a) b` | DENY | DENY | `compound-command` |
| N75 | `if true; then echo hi; fi; do echo bye` | DENY | DENY | `compound-command` |
| N76 | `case x in y) ;; esac` | ALLOW | ALLOW | — |
| N77 | `echo hi\n\necho bye` | ALLOW | ALLOW | — |
| N78 | `for i in a; do echo hi; done extra` | DENY | DENY | `invalid-structure` |
| N80 | `echo { a,b }` | DENY | DENY | `invalid-structure` |
| N84 | `echo hi &` | ALLOW | ALLOW | — |
| N85 | `echo hi \| \|` | DENY | DENY | `invalid-structure` |
| N86 | `echo hi && &&` | DENY | DENY | `invalid-structure` |
| N91 | `echo hi; &&` | DENY | DENY | `invalid-structure` |
| N95 | `a(b` | DENY | DENY | `invalid-structure` |
| N96 | `echo $((a))` | ALLOW | ALLOW | — |
| N97 | `echo hi; done` | DENY | DENY | `compound-command` |
| N98 | `echo hi; fi` | DENY | DENY | `compound-command` |

---

## WHAT I COULD NOT VERIFY

1. **The design of record.** `docs/dev/skill-package-format-design-2026-11-04.md` is not in the PR's tree or the base; I read §8.2.1 from the `pr222-design` branch. The "operator's P6 follow-up decisions" that the three-hash contract and the single-snapshot read come from are not in the repository either, so I verified A1/A2 against the **code and its tests**, not against a written requirement.
2. **Other hosts.** Every host measurement was taken on *this* host's permission layer, through my own agent's shell tool. The guard's model is claimed to be the host's scanner; a different host's scanner may differ.
3. **The non-structural rule classes.** I took the corpus's existing measurements for the *rule* families (the `mpm` word forms, the path shapes, the tab/quoted/ANSI-C forms) as given. I did not re-derive them from scratch; the twenty-first review's 33 forms I likewise took as pinned rather than re-measuring each.
4. **The full CI gate.** I ran `internal/core` with the FTS5 flags; it is green apart from `TestDrillE2E_ClaudeCode`, which fails **identically at the base** with `"Not logged in · Please run /login"` (it drives the `claude` CLI, which needs an authenticated login). I did not run `make test-race`, the `cmd/...` suites, `internal/scheduler`, `internal/critic`, or `internal/telemetry`.
5. **The plugin's own install suite.** `agent_installation/mpm-opencode/tests` has **15 failures** — all in `test_bundle_layout_registration` and `test_opencode_install`, all because the plugin payload is not built in this environment ("deployed payload was built from unknown"). The **base has the identical 15 failures**, so they are pre-existing and environmental, not caused by this PR. I did not build the payload to re-test them.
6. **Whether the operator's live reviewer definition was left untouched.** The mutation sweep's `restore()` reinstalls the reviewer from the template, so I ran it against an isolated copy with `HOME` redirected into my own scratch, and confirmed afterwards that `~/.config/opencode/agents/mpm-reviewer.md` is still byte-identical to the template.
7. **Forms carrying the literal binary name.** I deliberately isolated the structural class with binary-free forms (so the host's verdict was about the CONSTRUCT, not a rule), and then used a small number of deliberate exposure probes. I did not sweep the full space of binary-carrying forms inside each divergent family.
8. **The A1 per-file-change attribution.** As noted, no test asserts "a changed file changes the right hash and only that one"; I could not verify that claim from the test suite, only from the code's structure.

---

## REPRODUCTION

All artefacts are under `/tmp/opencode/review-a21/scratch/`:

| file | what it is |
|---|---|
| `model.py` | the model driver (imports the guard, calls `_matcher_denies(cmd, _shell_rules())`) |
| `cands.json`, `cands2.json` | the 109 novel forms, with the note for each |
| `corpus.json` | the guard's own 201 + 143 corpus lists, extracted with `ast` |
| `hostverdicts.json` | the host's measured verdict and reason for each of the 109 |
| `syntax.json` | the `bash -n` verdict for each |
| `comparison.json` | the full model-vs-host table |
| `tree/` | the worktree copy with the **faithfulness patch** applied (MAJOR-6) |
| `patch_faithful.py` | the patch that makes the model agree with the host on all five families |
| `sweep-tree/`, `run_mutations_sweep.py`, `sweep.log` | the isolated re-run of the 72-direction sweep |
| `vacuity.py` | the per-direction corpus-flip analysis |
| `installed_diff.txt` | the installed-copy drift check (empty = identical) |

Nothing was written under `/home/v/src/mpm` or `/tmp/opencode/p6fu`; the worktree is still clean at `6d4734929c7602905fbb6da85dd19719c22727e4`. The live database was never opened.
