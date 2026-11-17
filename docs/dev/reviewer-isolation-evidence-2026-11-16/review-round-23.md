# Independent review — PR #244, `flowbyte-com/mpm`

**HEAD under review:** `0ee25aaf381cbc35d2760def5b006b6b57b93720` (branch `fix/skill-packages-p6-provenance`)
**BASE:** `69bc212e207ddcf50b9c09d662d63403925a702e`
**WORKTREE reviewed:** the review worktree — confirmed at that head, `git status` clean before and after
every experiment (sha256 of the guard and of the template re-verified; the mutation sweep restores both).

---

## VERDICT: REQUEST CHANGES

The head's own headline claim — *"the model and the host agree on the five families"*, and the
class-level table that is supposed to make the families closed rather than enumerated — **is not
true**. I wrote and measured **103 novel structural forms** that appear in neither the 387-form
corpus nor the class-level family table, and **25 of them are forms the host REFUSES and the
model ALLOWS**. The class is **not closed**; the twenty-second review's finding reproduces.

A1, A2 and A3 all hold up under the checks I ran. The defect is confined to the structural
model — which is the artifact this head exists to fix.

### EXPOSURE: NONE

On **every one** of the 36 divergent or reason-mismatched forms I measured (3 isolated runs each,
103 runs, zero flaky verdicts), **the host refused the command**. No binary was reached, no
substrate was touched, and no live database was opened. Every divergent form I probed names **no
binary at all** — the refusals are about the CONSTRUCT, exactly as the guard argues elsewhere. The
host **fails closed on every divergent form I measured**.

So these findings are **model-fidelity defects, not enforcement holes**. They do not weaken the
reviewer's isolation. They do falsify the PR's structural claim, and they mean the guard cannot be
trusted to catch the *next* regression in this class — which is the whole point of the class-level
table this head adds.

---

## Findings, in severity order

---

### MAJOR 1 — The trailing-operator family is implemented only at the TOP LEVEL

**File/line:** `agent_installation/tests/test_reviewer_isolation_denies_every_mpm_tool.py:1188-1195`

```python
    if frames[-1]["last"] in ("and", "or", "pipe"):
        # A TRAILING `&&`, `||` or `|` demands a command that does not exist.
        return "invalid-structure"
```

`frames[-1]` at the end of the scan is the **top** frame. A trailing `&&`/`||`/`|` inside any
nested construct is recorded in that construct's own frame and then **discarded** when the frame
is popped (`}` at line 966-970, `)` at 907-909, `esac` at 1094-1097). The check therefore never
sees it.

This is precisely the "family implemented as a list of forms, not as a class" failure the
twenty-second review diagnosed — repeated for the very family the head claims to have fixed. The
class-level table (`test_the_structural_families_are_refused_as_a_class`, lines 3375-3520) lists
nine representatives for this family, **all of them top-level** (`echo hi &&`, `{ echo hi; } ||`,
`( echo hi ) ||`, `if true; then echo hi; fi &&`, …). Not one of them puts the trailing operator
*inside* a construct.

**Evidence (host REFUSES / model ALLOWS, 3/3 each):**

| command | host verdict (3/3) | host reason | model verdict |
|---|---|---|---|
| `{ echo hi && }` | DENY 3/3 | `invalid-structure` | ALLOW |
| `{ { echo hi && } }` | DENY 3/3 | `invalid-structure` | ALLOW |
| `f() { echo hi && }` | DENY 3/3 | `invalid-structure` | ALLOW |
| `case x in y) echo hi && ;; esac` | DENY 3/3 | `invalid-structure` | ALLOW |
| `case x in y) echo hi \|\| ;; esac` | DENY 3/3 | `invalid-structure` | ALLOW |
| `if true; then echo hi && fi` | DENY 3/3 | `compound-command` | ALLOW |
| `if true; then echo hi \| fi` | DENY 3/3 | `compound-command` | ALLOW |
| `while false; do echo hi && done` | DENY 3/3 | `compound-command` | ALLOW |
| `for i in 1; do echo hi \|\| done` | DENY 3/3 | `compound-command` | ALLOW |

Probe form (one isolated tool call each, three times):
`export HOME=... TMPDIR=... MPM_WORKSPACE=...; cd the review scratch directory; { echo hi && } ; echo "RC=$?"`

Note the host's reason **varies with the construct** (`invalid-structure` inside a brace group or a
case branch; `compound-command` inside an `if`/`while`/`for` body). A class-level fix therefore
cannot give this family a single reason — the table's one-reason-per-family shape cannot express
it.

**Fix:** track the pending-operator state per frame and check it when the frame is closed (and at
the end of the scan for the top frame); the reason must be derived per construct, not fixed per
family.

---

### MAJOR 2 — The `if` construct does not require a `then`

**File/line:** `...denies_every_mpm_tool.py:1141-1155` (the `fi` handler) vs `:1156-1173` (the `done` handler)

```python
            elif word == "fi":
                if not compound or compound[-1]["kind"] != "if":
                    return "compound-command"
                if compound[-1].get("awaiting_then"):
                    return "compound-command"
                if compound[-1]["phase"] == "body" and not compound[-1]["seen"]:
                    return "compound-command"
                compound.pop()
            elif word == "done":
                ...
                if compound[-1]["phase"] != "body":   # <-- the `done` handler HAS this check
                    return "compound-command"
```

The `done` handler requires `phase == "body"` — i.e. it requires the `do` to have arrived. The
`fi` handler **does not**. If the phase is still `"cond"` (no `then` was seen), `fi` pops the
construct silently and the model reports the command as well-formed.

The template's prose (line 832-834) states the class: *"a COMPOUND COMMAND whose keyword never
arrived — `for i in 1; echo hi; done`, `while false do echo hi; done` — is refused as
`compound-command`"*. The `do` half of that class is closed; the `then` half is not.

**Evidence (host REFUSES / model ALLOWS, 3/3 each):**

| command | host verdict (3/3) | host reason | model verdict |
|---|---|---|---|
| `if true; echo hi; fi` | DENY 3/3 | `compound-command` | ALLOW |
| `if true echo hi; fi` | DENY 3/3 | `compound-command` | ALLOW |
| `if true then echo hi; fi` | DENY 3/3 | `compound-command` | ALLOW |
| `if true` / `echo hi` / `fi` (newline spelling) | DENY 3/3 | `compound-command` | ALLOW |

**Fix:** in the `fi` handler, require `compound[-1]["phase"] == "body"` — the symmetric of the
`done` handler — and add the `then`-missing forms to the family table.

---

### MAJOR 3 — `else` and `elif` do not require a preceding `then`

**File/line:** `...denies_every_mpm_tool.py:1132-1140`

```python
            elif word == "else" or word == "elif":
                if not compound or compound[-1]["kind"] != "if":
                    return "compound-command"
                compound[-1]["phase"] = "body"
                compound[-1]["seen"] = False
                compound[-1]["awaiting_then"] = word == "elif"
```

Only the *kind* is checked. An `else` or `elif` that arrives while the `if` is still in its
`cond` phase — i.e. before any `then` — is accepted.

**Evidence (host REFUSES / model ALLOWS, 3/3 each):**

| command | host verdict (3/3) | host reason | model verdict |
|---|---|---|---|
| `if true; else echo hi; fi` | DENY 3/3 | `compound-command` | ALLOW |
| `if true; echo a; else echo b; fi` | DENY 3/3 | `compound-command` | ALLOW |
| `if true; elif true; then echo b; fi` | DENY 3/3 | `compound-command` | ALLOW |

(The third is worth noting: `bash` accepts `if cmd; elif cmd; then ...; fi`, but **the host does
not** — so the model cannot defer to the shell here.)

**Fix:** require `compound[-1]["phase"] == "body"` (i.e. a `then` has arrived) before accepting
`else`/`elif`.

---

### MAJOR 4 — An empty command slot after a compound keyword is not detected

**File/line:** `...denies_every_mpm_tool.py:786-827` (the separator handler) and `:1086-1181` (the
reserved-word handling)

Two independent causes:

1. **Reserved words fill the command slot.** `then`, `do`, `if`, `while`, `for`, `select`, `else`,
   `elif`, `fi`, `done`, `esac` are read by the scanner as ordinary WORDS, and the word handler
   sets `frames[-1]["empty"] = False`. So the slot after `then`/`do` looks occupied to the
   empty-slot rule.
2. **The `case` frame is exempted wholesale.** Line 810: `if frames[-1]["empty"] and
   frames[-1]["kind"] != "case":` — so an empty slot inside a `case` branch body is never caught,
   even though the host catches it.

**Evidence (host REFUSES / model ALLOWS, 3/3 each):**

| command | host verdict (3/3) | host reason | model verdict |
|---|---|---|---|
| `if true; then; echo hi; fi` | DENY 3/3 | `invalid-structure` | ALLOW |
| `while false; do; echo hi; done` | DENY 3/3 | `invalid-structure` | ALLOW |
| `for i in 1; do; echo hi; done` | DENY 3/3 | `invalid-structure` | ALLOW |
| `case x in y) ; esac` | DENY 3/3 | `invalid-structure` | ALLOW |
| `case x in y) echo hi; ; esac` | DENY 3/3 | `invalid-structure` | ALLOW |

The guard's own corpus already pins the *sibling* form `if true; then ; fi` (must_deny, line
2985) — so the guard knows the empty slot is a defect; it just does not generalise the rule past
the keyword.

Note the boundary the host draws, which the model gets wrong in both directions: `case x in y)
;; esac` is **ALLOWED** by the host (`;;` is the branch separator), while `case x in y) ; esac` is
**REFUSED** (a bare `;` needs a command before it). The model's blanket `case` exemption conflates
the two.

**Fix:** treat the reserved words as keywords that do **not** occupy the command slot, and apply
the empty-slot rule inside a `case` branch to the single `;` while exempting the `;;`/`;&`
separators.

---

### MAJOR 5 — A redirection with no target inside a construct is not detected

**File/line:** `...denies_every_mpm_tool.py:829-841` (the redirection handler) and `:2428-2458`
(`_matcher_empty_redirect`)

The scanner's redirection branch sets `frames[-1]["empty"] = False` and moves on; the
empty-target rule is enforced only by `_matcher_empty_redirect`, which looks at the **segment
text** and stops at the end of the segment. Inside a construct the `>` is followed by the
construct's closer, so the target is not empty *as a word* — but the host still refuses.

**Evidence (host REFUSES / model ALLOWS, 3/3 each):**

| command | host verdict (3/3) | host reason | model verdict |
|---|---|---|---|
| `if true; then echo hi > fi` | DENY 3/3 | `compound-command` | ALLOW |
| `( echo hi > )` | DENY 3/3 | `invalid-redirect` | ALLOW |
| `{ echo hi > }` | DENY 3/3 | `invalid-structure` | ALLOW |

**Fix:** a redirection operator inside a frame must consume a target word, or mark the frame as
structurally broken.

---

### MAJOR 6 — A `case` construct followed by `&` is refused by the host and allowed by the model

**File/line:** `...denies_every_mpm_tool.py:1188` (the trailing-operator check — `&` is *not* in
`("and","or","pipe")`, so it is deliberately allowed at the top level) and `:1094-1097`.

**Evidence:**

| command | host verdict (3/3) | host reason | model verdict |
|---|---|---|---|
| `case x in y) echo hi;; esac &` | DENY 3/3 | `invalid-structure` | ALLOW |

The corpus pins `case x in y) echo hi;; esac (echo bye)` (must_deny, line 3012) and the template
states *"only a separator may follow a closed compound construct"*. `&` is a separator, so the
model allows it; the host does not.

**Fix:** the "only a separator may follow a closed construct" rule needs the same treatment as the
trailing-operator rule: per construct, not only at the top level.

---

### MAJOR 7 — The class-level family table's REASONS are never checked against the host, and one of them is wrong

**File/line:** `...denies_every_mpm_tool.py:3356-3529` (the class-level test) and `:3462-3470`
(the `;&` family)

The class-level test is the head's central new artifact. Its docstring says the table carries
*"the REASON the host gives it"*, and that *"a family that the model stops catching shows up as a
FAILING REASON rather than as a missing row"*.

But the assertion at line 3524 is:

```python
                self.assertEqual(
                    reason,
                    _matcher_reason(command, rules),
```

— the **model** against the **table**. The table is never compared against the **host**. The
mechanism is self-referential and structurally incapable of detecting a wrong reason.

And one of the table's four representatives for the `;&` family already has the **wrong host
reason**:

| command | host verdict (3/3) | host reason | model / table reason |
|---|---|---|---|
| `echo hi ;& echo bye` | DENY 3/3 | **`invalid-structure`** | **`compound-command`** (table line 3468, model line 795) |

(The `;;&` form inside a `case` really is `compound-command` — verified — so the table's reason is
right for that member and wrong for this one. The host's reason depends on whether the construct
is a `case`.)

**Fix:** either give the family table a per-construct reason, or split the `;&` family, or (better)
record the host's reason for every representative in the table itself so a future drift is
visible. As it stands, the table's central claim is unfalsifiable by the test that checks it.

---

### MINOR 1 — The model refuses more than the host in ten places (safe direction)

All measured host-ALLOW / model-DENY, 3/3 each. These cost a nuisance, not a false "open" claim,
so they are MINOR — but they are the same root causes as the MAJORs and are worth fixing in the
same pass.

| command | host (3/3) | model verdict | root cause |
|---|---|---|---|
| `if true; then { echo hi; } fi` | ALLOW 3/3 | DENY `invalid-structure` | `_matcher_group_stands_where_a_command_may_stand` (`:1204-1246`) computes the slot from the last separator, and in a compound body that slot contains the keyword `then`/`do`, so a group in a body looks like it stands in argument position |
| `while false; do { echo hi; } done` | ALLOW 3/3 | DENY `invalid-structure` | same |
| `time case x in y) echo hi;; esac` | ALLOW 3/3 | DENY `compound-command` | `:1036` — the `case` construct is recognised only when `expect_command` is true; a prefix operator has already consumed it, so the `case`'s `)` is then read as an unmatched close paren |
| `! case x in y) echo hi;; esac` | ALLOW 3/3 | DENY `compound-command` | same |
| `if true; then case x in y) echo hi;; esac fi` | ALLOW 3/3 | DENY `compound-command` | the `case` branch `continue`s at `:1083`, before `:1179-1180` marks the enclosing body as non-empty, so `fi` sees an empty body |
| `f() (echo hi)` | ALLOW 3/3 | DENY `dynamic-execution` | `:866-872` — only a brace-group body is accepted as a definition's body |
| `case x in y) echo hi;; z) ;; esac` | ALLOW 3/3 | DENY `compound-command` | the model requires every `case` branch to be non-empty; the host allows an empty branch |
| `case x in y) ;; z) echo hi;; esac` | ALLOW 3/3 | DENY `compound-command` | same |
| `while false; do case x in y) echo hi;; esac; done` | ALLOW 3/3 | DENY `compound-command` | same as the `case`-in-a-body case above |
| `if true; then if false; then echo hi; fi; fi` | ALLOW 3/3 | DENY `compound-command` | reserved words never mark the enclosing body as non-empty (`:1086-1181`) — the same mechanism as MAJOR 4 |

---

### MINOR 2 — The housekeeping assertion cannot see the dead code the file already contains

**File/line:** `...denies_every_mpm_tool.py:3334-3354` (the assertion) and `:76-80` (the defect)

```python
        names = re.findall(r"^    def (\w+)\(self\)", source, re.MULTILINE)
```

The assertion's own docstring describes the hazard exactly:

> *"A second definition SHADOWS the first: Python keeps the last one, so the first is dead code —
> it reads like a claim, it is never run, and editing it changes nothing."*

But the regex only matches **methods** (`    def name(self)`). It cannot see a duplicated
**module-level** definition — and the file contains exactly that, twice over:

```
76: import re
77: import re
78: import unittest
79: from pathlib import Path
80: from pathlib import Path
```

`import re` and `from pathlib import Path` are each imported **twice**. The second import shadows
the first; the first is dead code that reads like a claim. The defect was introduced by commit
`41608c06` ("close the path and tab routes, and assert verdicts") and is still present at HEAD —
and the assertion that was later added to prevent exactly this class of defect was added in the
head commit `0ee25aaf` itself.

**Proof that the assertion is blind to it.** On a scratch copy of the tree (so the worktree was
never touched), I appended a second module-level `_matcher_structure` and `_matcher_reason`:

```
$ python3 -m pytest agent_installation/tests/test_reviewer_isolation_denies_every_mpm_tool.py -q
FAILED ...::test_the_rule_set_denies_what_it_claims_to_deny
FAILED ...::test_the_structural_families_are_refused_as_a_class
2 failed, 21 passed in 0.10s
```

`test_no_method_is_defined_twice` **PASSED**. The duplicate was caught only incidentally, by the
behavioural assertions — a duplicate module-level helper whose shadowing changed nothing would
have been completely invisible.

**Fix:** widen the assertion to module-level `def` names and to duplicate imports, or state
plainly that it covers methods only and fix the two duplicate imports at least.

---

### NIT 1 — The template's five-family prose is not checked against the model at all

`...denies_every_mpm_tool.py` reads `REVIEWER` in only four places (lines 167, 3586, 3872, 3905):
the rules block, the capability-loader prose check, and the two install/drift checks. **No test
compares the template's prose description of the five families to the model's behaviour.** The
prose is therefore free to drift, and has: line 838-840 says a `;&`/`;;&` branch separator "is
refused as `compound-command`", which is true inside a `case` and false outside it (see MAJOR 7).

---

## The decisive test: what NEW forms did I try, and did the model catch them?

**No. The class is not closed.**

**Method.** I read `_matcher_structure` and its helpers in full (`:681-1201` plus the 30-odd
helpers it calls), read the whole 387-form corpus (extracted programmatically with `ast`, so I
could check every candidate against it mechanically), read the class-level family table, and then
wrote **120 candidate forms in the structural class that appear in neither**. I measured the host
on **103** of them (one isolated tool call each, three repetitions each for the 36 divergent /
reason-mismatched ones), and compared against `_matcher_structure`, `_matcher_reason` and
`_matcher_denies` driven through a Python harness that imports the guard itself.

**Result: 25 forms the host refuses and the model allows** (MAJORs 1-6), 10 forms the host allows
and the model refuses (MINOR 1), and 1 form where the model and the table give the wrong reason
(MAJOR 7). **68 forms agreed.**

**Shapes nobody had tried, and what happened to each:**

| novel shape | outcome |
|---|---|
| trailing `&&` **inside** a brace group / function body / case branch / `if` body / loop body | **model MISSES all 9** |
| `if` with **no `then`** (`if true; echo hi; fi`, `if true echo hi; fi`, newline spelling) | **model MISSES all 3** |
| `else` / `elif` **before any `then`** | **model MISSES all 3** |
| empty slot **after `then` / `do` / a `case` branch's `;`** | **model MISSES all 5** |
| `>` with **no target inside** a construct | **model MISSES all 3** |
| `case` closed and followed by `&` | **model MISSES** |
| `case` **after a prefix operator** (`time`, `!`) | model over-refuses (MINOR) |
| `case` **inside a compound body** | model over-refuses (MINOR) |
| brace group **inside a compound body** | model over-refuses (MINOR) |
| nested `if` | model over-refuses (MINOR) |
| function definition with a **subshell body** | model over-refuses (MINOR) |
| `case` with an **empty branch** | model over-refuses (MINOR) |
| `case` subject containing `)` | **model MISSES** |
| `case` in a group / subshell / substitution / nested `case` | model agrees (host ALLOW) |
| `case` with a substitution / space in the subject | model agrees |
| `f ( ) { echo hi; }` (space inside the parameter list) | model agrees (host ALLOW) |
| nested function definition, function definition in a group, after `&&` | model agrees |
| group after `\|` / `\|\|`, group in a case branch | model agrees |
| heredoc in a group / case / substitution | model agrees |
| `;;` in a quoted pattern, `)` in a quoted pattern, unbalanced `(` in quotes | model agrees |
| `select` with a `do`, `select` with an empty header | model agrees |
| `coproc` with a quoted name | model agrees (host REFUSE, model REFUSE) |
| `echo 'a(b'`, `echo 'a(b)c'` | model agrees |
| `esac`/`fi`/`done`/`then`/`do`/`else`/`elif`/`if` alone | model agrees |
| `>&` with no target, `;;&` alone, `&;` alone | model agrees |

**So: the model caught the forms it had been taught, and missed every form that required the
*rule* to be generalised past the shape it was written for.** The five families of the
twenty-second review are still five *lists of forms*, not five *rules*. The head's own words —
*"the corpus pins every form in BOTH directions; and a NEW CLASS-LEVEL table pins FAMILIES with
the REASON the host gives each"* — describe a table of representatives, not a class-level proof.
The table's nine representatives for the trailing-operator family are all top-level; the class
has nine members I did not need to invent.

---

## The full model-vs-host table

Every form below was measured **three times in three isolated tool calls**. `DENY 3/3` means the
permission layer refused all three with the reason shown; `ALLOW 3/3` means all three ran. There
was **no flakiness in any verdict** (103 distinct forms, 36 of them re-measured twice more).

### MAJOR — host REFUSES, model ALLOWS

| # | command | host | host reason | model |
|---|---|---|---|---|
| 1 | `{ echo hi && }` | DENY 3/3 | invalid-structure | ALLOW |
| 2 | `{ { echo hi && } }` | DENY 3/3 | invalid-structure | ALLOW |
| 3 | `f() { echo hi && }` | DENY 3/3 | invalid-structure | ALLOW |
| 4 | `case x in y) echo hi && ;; esac` | DENY 3/3 | invalid-structure | ALLOW |
| 5 | `case x in y) echo hi \|\| ;; esac` | DENY 3/3 | invalid-structure | ALLOW |
| 6 | `if true; then echo hi && fi` | DENY 3/3 | compound-command | ALLOW |
| 7 | `if true; then echo hi \| fi` | DENY 3/3 | compound-command | ALLOW |
| 8 | `while false; do echo hi && done` | DENY 3/3 | compound-command | ALLOW |
| 9 | `for i in 1; do echo hi \|\| done` | DENY 3/3 | compound-command | ALLOW |
| 10 | `if true; echo hi; fi` | DENY 3/3 | compound-command | ALLOW |
| 11 | `if true echo hi; fi` | DENY 3/3 | compound-command | ALLOW |
| 12 | `if true then echo hi; fi` | DENY 3/3 | compound-command | ALLOW |
| 13 | `if true; else echo hi; fi` | DENY 3/3 | compound-command | ALLOW |
| 14 | `if true; echo a; else echo b; fi` | DENY 3/3 | compound-command | ALLOW |
| 15 | `if true; elif true; then echo b; fi` | DENY 3/3 | compound-command | ALLOW |
| 16 | `if true; then; echo hi; fi` | DENY 3/3 | invalid-structure | ALLOW |
| 17 | `while false; do; echo hi; done` | DENY 3/3 | invalid-structure | ALLOW |
| 18 | `for i in 1; do; echo hi; done` | DENY 3/3 | invalid-structure | ALLOW |
| 19 | `case x in y) ; esac` | DENY 3/3 | invalid-structure | ALLOW |
| 20 | `case x in y) echo hi; ; esac` | DENY 3/3 | invalid-structure | ALLOW |
| 21 | `case x) in y) echo hi;; esac` | DENY 3/3 | compound-command | ALLOW |
| 22 | `if true; then echo hi > fi` | DENY 3/3 | compound-command | ALLOW |
| 23 | `( echo hi > )` | DENY 3/3 | invalid-redirect | ALLOW |
| 24 | `{ echo hi > }` | DENY 3/3 | invalid-structure | ALLOW |
| 25 | `case x in y) echo hi;; esac &` | DENY 3/3 | invalid-structure | ALLOW |

### Reason mismatch — host REFUSES, model REFUSES, but for a different reason

| # | command | host | host reason | model / table reason |
|---|---|---|---|---|
| 26 | `echo hi ;& echo bye` | DENY 3/3 | **invalid-structure** | **compound-command** (table line 3468) |

### MINOR — host ALLOWS, model REFUSES (safe direction)

| # | command | host | model verdict |
|---|---|---|---|
| 27 | `if true; then { echo hi; } fi` | ALLOW 3/3 | DENY invalid-structure |
| 28 | `while false; do { echo hi; } done` | ALLOW 3/3 | DENY invalid-structure |
| 29 | `time case x in y) echo hi;; esac` | ALLOW 3/3 | DENY compound-command |
| 30 | `! case x in y) echo hi;; esac` | ALLOW 3/3 | DENY compound-command |
| 31 | `if true; then case x in y) echo hi;; esac fi` | ALLOW 3/3 | DENY compound-command |
| 32 | `f() (echo hi)` | ALLOW 3/3 | DENY dynamic-execution |
| 33 | `case x in y) echo hi;; z) ;; esac` | ALLOW 3/3 | DENY compound-command |
| 34 | `case x in y) ;; z) echo hi;; esac` | ALLOW 3/3 | DENY compound-command |
| 35 | `while false; do case x in y) echo hi;; esac; done` | ALLOW 3/3 | DENY compound-command |
| 36 | `if true; then if false; then echo hi; fi; fi` | ALLOW 3/3 | DENY compound-command |

### Agreed forms (68) — host and model agree

**Family-table representatives, all reasons confirmed against the host:**
`echo hi &&`, `echo hi ||`, `echo hi |`, `echo hi |&`, `if true; then echo hi; fi &&`,
`while false; do echo hi; done &&`, `case x in y) echo hi;; esac &&`, `{ echo hi; } ||`,
`( echo hi ) ||` — all **invalid-structure**;
`a(b)`, `a(b)c`, `a(b) c`, `a(b) c(d)`, `echo a(b)`, `x(y) z`, `1(b)`, `f()`, `echo f()` — all
**dynamic-execution**;
`f ()`, `echo (hello)`, `echo (a) b`, `echo 'a' (b)` — all **compound-command**;
`for i in 1; echo hi; done`, `select x; echo hi; done`, `while false do echo hi; done`,
`until false do echo hi; done`, `if true; then echo a; elif true; echo b; fi`,
`if true; then echo hi`, `while false; do echo hi`, `for i in 1; do echo hi`, `if true; then fi`,
`while false; do done`, `for i in 1; do done` — all **compound-command**;
`echo hi; esac`, `echo hi; fi`, `echo hi; done`, `do echo hi`, `)`, `echo )`, `{ echo hi )`,
`case x in y) echo hi)`, `case x in y) echo hi )`, `case x in y) echo hi) extra`,
`case x in y) (echo hi)` — all **compound-command**;
`case in) echo hi;; esac`, `case x in y) echo hi;;& echo bye;; esac`,
`case echo x in y) echo hi;; esac` — all **compound-command**;
`}`, `echo }`, `;`, `;;`, `echo hi;;`, `{ }`, `( )`, `echo a { echo hi; }` — all
**invalid-structure**;
`echo 'unclosed`, `echo "unclosed` — **unterminated-quote**;
`$(`, `<(`, `echo <(echo "a)b"` — **command-substitution**.

**Other novel forms:**
ALLOW/ALLOW — `case x in y) ;; esac`, `case x in y) esac`, `case x in y)` + newline + `;; esac`,
`case x in y) # c` + newline + `;; esac`, `case "echo hi" in y) echo hi;; esac`,
`case $(echo a b) in y) echo hi;; esac`, `case print in y) echo hi;; esac`,
`f ( ) { echo hi; }`, `f(){ echo hi; }`, `f() { g() { echo hi; }; }`, `{ f() { echo hi; }; }`,
`echo hi && f() { echo hi; }`, `{ echo hi && echo bye; }`,
`case x in y) { echo hi; } ;; esac`, `echo hi | { echo hi; }`, `echo hi || { echo hi; }`,
`f() { { echo hi; }; }`, `{ { echo hi; } }`, `{ case x in y) echo hi;; esac; }`,
`( case x in y) echo hi;; esac )`, `echo $(case x in y) echo hi;; esac)`,
`{ case x in y) case z in w) echo hi;; esac;; esac; }`, `case x in y) echo hi ;; esac`,
`echo a; { echo hi; }`, `echo $(echo $(echo hi))`, `(echo $(echo hi))`, `echo "fi"`,
`echo "a)b"`, `echo 'a(b'`, `case x in "y(z") echo hi;; esac`,
`case x in "y;;z") echo hi;; esac`, `select x in a b; do echo hi; done`,
`coproc n { echo hi; }`, `{ cat <<EOF ... EOF }`,
`case x in y) cat <<EOF ... EOF ;; esac`, `echo $(cat <<EOF ... EOF)`,
`while false; do echo hi; done && echo bye`, `if true; then echo hi; fi | cat`;
DENY/DENY — `f() { echo hi; ; }` (invalid-structure), `{ echo hi; ; }` (invalid-structure),
`if true; then echo hi;; fi` (invalid-structure), `echo hi;; echo bye` (invalid-structure),
`echo hi; ; echo bye` (invalid-structure), `echo hi;;echo bye` (invalid-structure),
`{ echo hi; } { echo bye; }` (invalid-structure), `for i in 1; do echo hi; fi` (compound-command),
`if true; then echo hi; done` (compound-command), `if true; then echo hi; fi extra`
(invalid-structure), `echo hi >&` (invalid-redirect), `( echo hi && )` (invalid-structure),
`{ echo hi | ; }` (invalid-structure), `f (x) { echo hi; }` (compound-command),
`f(x) { echo hi; }` (dynamic-execution), `coproc "n" { echo hi; }` (invalid-structure),
`while` + newline + `do echo hi; done` (compound-command), `echo hi ;& ;` (compound-command),
`echo hi;;& echo bye` (compound-command).

---
## Verification of the PR's three claims

### A1 — three hashes plus a `hash_origins` record — **HOLDS**

* Implementation: `internal/core/skill_package_read.go:516-537` (`PackageHashOrigin` enum,
  `PackageHashOrigins` struct), `:539-620` (`SkillPackageProvenance`), populated at `:1138`.
  `entrypoint_sha256` and `projection_sha256` are **recorded**; `body_sha256` is **recomputed**.
* Every hash has an origin: the struct has one field per hash, so the JSON shape is stable by
  construction.
* `projection_sha256` is obtained from the stored manifest and **validated** against the
  projection being returned (`read.go:913`); a manifest that fails to parse or validate reports the
  origin as `unavailable`, not as an empty string; a manifest that validates but **disagrees** with
  the projection fails closed with `integrity_failure` (`read.go:1183-1210`).
* Tests: `internal/core/skill_package_provenance_followup_test.go:145`
  (`ProvenanceCarriesTheThreeHashes`), `:208`
  (`DistinguishesTheOriginalFromTheProjection` — asserts the entrypoint and projection hashes
  differ for a versionless package and that the body hash equals the digest of the body it
  returns), `:285` (`ReportsAnUnreadableManifestAsUnavailable`), `:331`
  (`RefusesAProjectionThatContradictsTheManifest`); and
  `internal/core/tools/skill_package_read_tools_test.go:327-361` for the MCP surface.
* **"a changed file changes the right hash and only that one"**: the projection hash is the
  manifest's recorded value, validated against the projection actually returned; the body hash is
  recomputed over the bytes returned. A changed entrypoint therefore changes the projection (and
  hence the projection hash) and the body hash, while `entrypoint_sha256` continues to describe the
  **original** file — which is the documented contract, not a leak.
* Result: `go test -tags fts5 -run TestP6 ./...` in `internal/core` → **ok**;
  `go test -race -tags fts5 -run TestP6 .` → **ok**.

### A2 — the single-snapshot read — **HOLDS**

* Implementation: `internal/core/skill_package_read.go:1262-1314` — `withPackageReadSnapshot` opens a
  `sql.TxOptions{ReadOnly: true}` transaction (deferred; the driver issues a plain `BEGIN`, so the
  read-only nature rests on discipline, which the code documents and pins).
* The exported `ReadSkillWithPackageProvenance` (`:969-991`) runs through the **named seam**
  `withSkillReadSnapshot` / `setSkillReadSnapshotHook` (`:1014-1063`), so the concurrency invariant
  is testable deterministically against the *exported* function, not only an internal helper.
* **The proof is a test, and it is a real one**: `followup_test.go:379`
  (`TestP6FollowupTheReadIsOneSnapshot`) opens a real read snapshot on a dedicated connection,
  commits a concurrent `DELETE` on another connection (WAL: a writer does not block a reader), then
  drives the exported read inside the **held** snapshot through the seam and asserts
  **`seamCalls == 1`** — *"A regression that split it back into two independent reads would never
  have called the seam."* `:453` (`TheReadReportsNotFoundAfterADeletion`) pins the other half (a
  committed deletion is wholly visible → the ordinary not-found answer). `:621` covers the
  concurrent-deletion case.
* I ran these: `go test -tags fts5 -run TestP6 ./...` → **ok**;
  `go test -race -tags fts5 -run TestP6 .` → **ok**.
### A3 — reviewer isolation — **HOLDS**

* **Deny rules against the registry**: `internal/core/tools/registry_list.go` publishes 21 tools;
  the template denies all 21 by name (`mpm-reviewer.md:144-201`), with no stale entries.
  `test_every_registry_tool_is_denied` and `test_the_deny_list_has_no_stale_entries` are green,
  and the registry parse is asserted non-empty and lossless.
* **Tool-registration / permission layers**: `question`, `execute` (Code Mode), `opencode_*`
  (session/worktree mutations), `edit`, `read` on a live workspace, and the shell rules are all
  closed; the capability loader is deliberately **not** closed and the definition says so in prose
  (`test_the_definition_does_not_claim_the_capability_loader_is_closed`).
* **Installed-copy drift**: `diff` of the operator's installed
  `~/.config/opencode/agents/mpm-reviewer.md` against
  `agent_installation/mpm-opencode/templates/agents/mpm-reviewer.md` → **identical**
  (sha256 `899d37e9f735af88ae65e2125984fd3e9091b227e8845de545e6bdc17e0397ae`).
* **Sandbox launcher**: `agent_installation/mpm-opencode/scripts/reviewer_sandbox.py` creates
  `home/ tmp/ workspace/ findings/` and runs a command with `HOME`, `TMPDIR`, `MPM_WORKSPACE`
  redirected. I ran `init` and `run -- sh -c '...'` and confirmed the three variables point into the
  sandbox. It **refuses a base inside an MPM runtime root** *before creating anything*:
  `--base <RUNTIME_ROOT>/probe init` printed
  `the sandbox base ... is inside an MPM runtime root (it contains '.mpm'); refusing to create the
  sandbox there`, exited 1, and the directory was not created. `test_reviewer_sandbox.py`: 6 passed.

### The seventy-three mutation directions — **HOLDS**

I re-ran `run_mutations.py` myself (it restores the files it touches; it does not
touch any Go file, so it ran safely alongside the Go tests).

```
the baseline: GREEN
after the run: GREEN
rule-level: 10   model-level: 63   total: 73
the guard FAILED for 73 of them
```

* **Baseline green, restored after the run.** `sha256sum -c` on the guard and the template →
  **OK**; `git status --short` → empty; `git log -1` → still `0ee25aaf`.
* **No direction is vacuous.** All 73 fail (none reports `STILL GREEN`), so no direction reverts a
  rule while a second rule still enforces the same behaviour. The four empty-condition directions
  (`EMPTY CONDITION rule`, `EMPTY CONDITION on the segments`, `` `if` EMPTY CONDITION check``,
  `` `while`/`until` EMPTY CONDITION check``) mutate **four different code sites** — verified by
  reading the mutation table; they are not four copies of one edit.
* **No direction fails for a reason other than an assertion about the behaviour it names.** I
  classified all 73 failure reasons: 48 `ASSERT-not-deny` (*"the rule set does NOT deny '<form>';
  the name reaches the binary through a spelling the rules do not cover"*), 17 `ASSERT-denies`
  (*"the rule set denies '<form>', which the definition states is allowed"*), 3
  `ASSERT-unexpected-present`, 3 `ASSERT-reason-mismatch`, 1 `ASSERT-lists`, and 1
  `ASSERT-spec-text` (the DIVERGENCE LEDGER direction, which is caught by the SPEC's published
  count — a real consequence of shrinking the ledger, not an incidental error).
  **No traceback, no `NameError`, no parse error, no `PATTERN MISSING`.** The five directions the
  twenty-second review found failing because the mutated file no longer parsed are fixed: every
  mutated file still parses and every failure is a behavioural assertion.

### The corpus and the family table — the corpus is complete, the table is not

* I extracted all 387 pinned forms (must_deny + must_allow + the family table + the divergence
  ledger) with an `ast` walk and checked every form I wrote against that set mechanically. **Every
  form the twenty-second review listed is pinned in BOTH directions**, and the corpus's
  must_allow/must_deny split is honest (the guard suite is green: 28 passed, 1 skipped).
* The class-level family table's **reasons** match the host for **every representative except
  one** (`echo hi ;& echo bye` — MAJOR 7). The table's *coverage* is the problem: its
  representatives are all drawn from the shapes the corpus already knew.
* The template's five-family prose (lines 812-845) is a fair description of the HOST for the
  representatives it names, and is wrong or incomplete in the same places the model is
  (a missing `then`; a trailing operator inside a construct).

---

## What I could NOT verify

1. **The full `make test-race` gate.** The `-race` link of the whole `internal/core` package
   exhausted the disk quota in my scratch `TMPDIR` (`disk quota exceeded` from `ld`, on a tmpfs
   already at 79%). This is an environment limit, not a test failure. What I did run:
   `go test -tags fts5 -count=1 ./...` in `internal/core` → **all ok**;
   `go test -race -tags fts5 -count=1 -run TestP6 .` → **ok**;
   `go test -tags fts5 -count=1 -run TestP6 ./...` → **all ok**.
   The `cmd/`, `internal/scheduler/`, `internal/critic/`, `internal/telemetry/` packages were **not**
   run.
2. **The PR's other claims in `docs/SPEC.md`** (the §6.12 cost measurement, the §8.2.1 provenance
   text) were read but not independently re-derived; I checked the code and the tests behind A1/A2.
3. **The host's behaviour on a divergent form that NAMES a binary.** By safety rule 4/5 I
   deliberately probed only construct-only forms. The EXPOSURE conclusion rests on the structural
   refusal, which does not depend on the payload: every divergent form was refused before any
   name was examined.
4. **Whether the host's scanner has other families the model does not know about at all** — i.e.
   whether the model under-approximates the host's *rule set*, not only its parser. I probed the
   families the guard names plus the shapes I could derive from the parser; I did not attempt to
   enumerate the host's scanner exhaustively.
5. **The project linter's `--gate` mode and the git pre-commit hook** were not run; the PR does not
   touch them, and the guard's own gate is the Python suite.

---

## Reproducing this review

* Model driver: import the guard module and call `_matcher_structure(cmd)`,
  `_matcher_reason(cmd, _shell_rules())` and `_matcher_denies(cmd, _shell_rules())` against
  `agent_installation/tests/test_reviewer_isolation_denies_every_mpm_tool.py`.
* Corpus extraction (to avoid re-testing a pinned form): `ast`-walk the guard for `must_deny`,
  `must_allow`, `families` and `_DIVERGENCES` → **387 forms**.
* Host measurement: one isolated shell call per form, with
  `export HOME=... TMPDIR=... MPM_WORKSPACE=...; cd <scratch>; <form>; echo "RC=$?"`. A refusal comes
  back as `{"error":{"type":"unknown","message":"Portable shell scanner cannot analyze command:
  <reason>"}}`.
* No unbounded loop was run; no write command was run; the live MPM database was never opened;
  nothing was created under the operator's checkout or the review worktree (the mutation sweep edits
  and restores the two guard files, and both were sha256-verified afterwards).

---

## Incidental observation about the host's matcher (not a finding against the PR)

While writing this report I hit a host behaviour worth recording, because it is the kind of thing
the guard's own model asserts about. The shell rule that refuses a **write redirect into a runtime
root** is **not anchored on the redirect operator**: a command that contains a redirect character
anywhere, and a runtime-root path component anywhere LATER in the text, is refused — even when
the redirect's own target is unrelated. My own heredoc was refused for exactly that reason. The
host fails closed, so it is harmless, and the guard's `_matcher_u2` reproduces the same unanchored
behaviour, so the model and the host agree here. No action needed; noted so that a future
reviewer does not mistake it for a defect in the report.
