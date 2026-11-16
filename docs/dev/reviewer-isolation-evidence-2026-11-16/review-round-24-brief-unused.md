# Independent review — PR #244 (`flowbyte-com/mpm`), round 24

**HEAD under review:** `2f85712a9fe1af4da9826dda12ba12882a834dec` (branch `fix/skill-packages-p6-provenance`)
**BASE:** `69bc212e207ddcf50b9c09d662d63403925a702e`
**WORKTREE to review:** `/tmp/opencode/p6fu` — confirm it is at that head (`git rev-parse HEAD`) and clean
before and after your experiments.

## What changed in this head

This is the **remediation of round 23**, which returned REQUEST CHANGES with **7 MAJOR, 2 MINOR, 1 NIT**
and **exposure NONE** (the host fails closed on all 36 divergent forms; 103 runs; zero flaky verdicts).

The remediation, in the author's own words:

1. The structural scanner was **rewritten as a PARSER** and made the **single enforcer** for the
   structural class. The older machinery keeps only the two rules the scanner does not own:
   dynamic execution (`eval`, `alias NAME=value`) and a context the host cannot parse
   (`[[ x; y ]]`, `((x;y))`, an unclosed `[[ x`, a process substitution after a command-position
   context).
2. The host's **REASON depends on the CONSTRUCT that holds the defect**, so the class-level family
   table now carries a **reason per FORM**, not one reason per family.
3. Round 23's **34 divergent forms** are pinned in the corpus **in both directions** (the refused
   ones in the deny list, the allowed ones in the allow list), and every form of every family is
   pinned in the corpus as well.
4. The housekeeping assertion now covers **module-level definitions** as well as methods, and the
   file's own two duplicate imports are gone.
5. The definition's (`mpm-reviewer.md`) family list is now **machine-readable** and **checked against
   the model** by a new guard test.
6. The mutation sweep was rebuilt against the new scanner's code sites: **91 directions**
   (10 rule-level, 81 model-level), every one failing the guard with an **assertion** about the
   behaviour it names — not a crash, not a hang — with the baseline GREEN before and after.

## The claims to test

- **A1** — three hashes (`projection_sha256`, `body_sha256`, `entrypoint_sha256`) plus a
  `hash_origins` object that names each hash's origin; a manifest that validates but contradicts
  the stored projection fails **closed**; an unparseable manifest is **tolerated**.
- **A2** — `ReadSkillWithPackageProvenance` reads the body and the package provenance from **one**
  snapshot. Prove it: rewrite the read as two independent reads and the deterministic tests must
  fail (`opened 2 snapshot(s), want exactly 1`).
- **A3** — reviewer isolation at the **permission layer**: every `mpm_*` action denied, the
  `read`/`glob`/`external_directory` paths into a live workspace denied, `edit`, `subagent`,
  `question`, Code Mode and the MCP resource surface denied, the session/worktree mutations
  denied; the shell rules deny the binaries wherever they are **named** — in seven forms per binary
  plus three shared forms — and deny the installers and the service-control commands; an isolated
  `HOME`/`TMPDIR`/`MPM_WORKSPACE` and a filesystem scratchpad; and the guard test.
- **The structural model** — that it agrees with the host on **414 measured pairs** (202 measured
  earlier, round 22's 109 novel forms, round 23's 103 novel forms), with one recorded divergence;
  that the class-level table is closed as a **class**; that the table's reason per form is the
  host's reason; that the scanner is a **single** enforcer; that the **91** sweep directions are
  non-vacuous and each fails with an assertion.

## How to work

Write **novel** forms. The author's corpus already holds 266 denied + 159 allowed rows and 23
families / 95 forms. **Check your forms against the corpus first** — a form already pinned proves
nothing. Then measure each form on the host **three times in isolated tool calls** and check that
the verdict is stable.

To measure the host, use the **export** form, with an isolated environment, from a scratch
directory of your own:

```
export HOME=<your scratch>/home TMPDIR=<your scratch>/tmp MPM_WORKSPACE=<your scratch>/workspace PATH=<shim>:$PATH
<the command>
```

**Rules that matter:**

- **Never** open the live substrate: `/home/v/.mpm/src/db/mpm.db` and `mirror.jsonl` are
  off-limits, including read-only. Set `HOME` and `MPM_WORKSPACE` **first, in the same command**.
- **Never** run an unbounded loop (`until false`, `for ((;;))`, `while true`).
- **Never** append `; echo "RC=$?"` to a probe that ends in `&` — it manufactures an empty-slot
  defect and a false refusal. (Round 23 reported one such artefact; check it yourself.)
- Use `export …`, never `env … <probe>` — the reviewer's layer is what carries the rules, and the
  author's layer does not (it carries the scanner only). A measurement about the **name matcher**
  must be made in **your** layer.
- A file write whose **content** contains the lowercase binary name is refused: use a `<BIN>`
  placeholder in written text and expand it in your own reading.
- The author's shell layer and yours differ. Say which layer each measurement was made in.

## What to report

A verdict — **APPROVE**, **REQUEST CHANGES**, or **COMMENT** — plus findings in severity order
(BLOCKING / MAJOR / MINOR / NIT), each with the file and line, the command, the host's verdict and
reason (three runs), the model's verdict, and the corpus check that shows the form was not already
pinned. State the **exposure** explicitly: for every divergent form, did the host refuse the
command or reach the binary?

Write the review to `/tmp/opencode/review-a24/review.md` and report it back. Keep your scratch under
`/tmp/opencode/review-a24/`. Do not modify `/tmp/opencode/p6fu` — restore anything you change in
it (sha256 the guard and the template before and after), and do not modify the operator's checkout
at `/home/v/src/mpm`.
