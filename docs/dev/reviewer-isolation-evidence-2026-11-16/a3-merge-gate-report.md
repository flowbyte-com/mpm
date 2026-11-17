# A3 — the final merge gate for PR #247

> **What this file is.** The merge-readiness gate for the reviewer-sandbox
> work on PR #247, run against the final head. It states the exact
> revisions, the capability inventory, the integration and adversarial
> results, the CI state, the residual risks and the recommendation.
>
> **This is a record, not an implementation.** Nothing here changes the
> substrate's behaviour. The code it describes lives on the branch that
> carries this directory.
>
> **Nothing was merged, installed or activated.** The gate produces a
> recommendation; the merge is the operator's.

---

## 0. The recommendation

**APPROVE** — PR #247 is **ready for operator-authorised merge**.

Every blocking class in the gate's convergence rule is empty:

| blocking class | result |
|---|---|
| a demonstrated boundary escape | **none** — and the one escape that WAS found during the gate (the home-bind defect, below) was reproduced live, fixed, and re-verified independently |
| an unconfined tool or delegated execution route | **none** — the host's own permission service refuses 23 of 24 actions for the reviewer; the one action it opens is confined by the kernel |
| a sandbox or installer that fails open | **none** — 8 refusal paths measured, each refusing without running the command |
| an inaccurate security guarantee | **one found and corrected** — the definition claimed a denied tool is *"removed from your tool list entirely … cannot see one"*, which was never verified; it now states the measured fact |
| a failed required test or CI check | **none** — every local gate and every CI run at the reviewed head is green |
| an unexpected implementation change affecting the security boundary | **one found during the gate** — the post-review toolchain-discovery delta; it was submitted to a narrow independent delta review, which returned REQUEST CHANGES, and the fix was re-verified to APPROVE |

---

## 1. The exact revisions

| | |
|---|---|
| **PR #247 head (the reviewed code)** | `fc1af83f13de783e269e66da25b952b9d419778a` |
| **PR #247 head (with this report added)** | one commit later — documentation only; see §6 for the CI run at the final head |
| **current `main` (authoritative, from `git ls-remote`)** | `1da6777c68112e589fc8a52af387621073bb71fe` |
| **base** | `main` — `1da6777c68112e589fc8a52af387621073bb71fe` |
| **mergeability** | `mergeable: true`, `mergeable_state: clean` |
| **state** | open, **draft** |
| **diff** | 39 files, +9193 / −12 |
| **the commit that brought `main` in** | `28a40e93` (*Merge main into feat/reviewer-sandbox-isolation*) |

`main` was verified by `git ls-remote` (the authority) and not by a local
tracking ref. `git merge-base --is-ancestor 1da6777c HEAD` succeeds, so
the approved `main` is already integrated and the branch needs no
further merge.

### 1.1 The source-file guard conflict, resolved without losing coverage

The merge resolved one conflict: `internal/core/skill_package_read_guard_test.go`.
The resolution keeps **main's** version, and the coverage is identical:

- the file on the branch is **byte-identical to `main`** (`git diff --quiet` → clean);
- **5** test functions on both sides;
- the four-entry `p6ReaderSourceFiles` list is intact — `skill_package_read.go`,
  `skill_package_read_errors.go`, `skill_document.go`, `skill_package_export.go`.

Every P6/P7 source file is untouched by the branch
(`git diff --stat 1da6777c HEAD -- internal/core/` shows only
`internal/core/tools/output_policy_test.go`, and that is PR #244's
hermeticity fix, not A3). **No P6/P7 functionality is regressed.**

### 1.2 The final A3-only diff

The PR diff is A3's work plus the campaign work of PR #244, which is
still **open and unmerged** (`#244`: 23 files, head `173e50e9`), so its
preserved record travels inside this PR's diff.

**A3-only — 16 files, not touched by #244:**

```
agent_installation/mpm-opencode/src/index.v2.ts
agent_installation/mpm-opencode/src/sandbox-exec.ts
agent_installation/mpm-opencode/tests/test_opencode_install.py
agent_installation/mpm-opencode/tests/test_sandbox_exec.test.ts
agent_installation/tests/test_reviewer_isolation.py
agent_installation/tests/test_reviewer_sandbox_exec_path.py
agent_installation/tests/test_runtime_package_provisioning.py
docs/dev/reviewer-isolation-evidence-2026-11-16/a3-completion-report.md
docs/dev/reviewer-isolation-evidence-2026-11-16/charlie-a3-implementation-review.md
docs/dev/reviewer-isolation-evidence-2026-11-16/host-verdicts-a3.json
internal/update/payload_test.go
scripts/runtime_payloads.json
scripts/tests/test_install_updater_bootstrap.py
scripts/tests/test_migration_gate.py
scripts/tests/test_runtime_bundle.py
```

**Inherited from #244 and untouched by A3 — 15 files** (the preserved
review record: the round-22/23 reports and their corpora, the decision
report, the final report, `reviewer_sandbox.py`, `output_policy_test.go`,
`uninstall.sh`, `Makefile`, …).

**Inherited from #244 and extended by A3 — 9 files:**
`agent_installation/README.md`, `agent_installation/mpm-opencode/README.md`,
`agent_installation/mpm-opencode/install.sh`,
`agent_installation/mpm-opencode/scripts/install_reviewer_agent.py`,
`agent_installation/mpm-opencode/scripts/reviewer_bwrap.py`,
`agent_installation/mpm-opencode/templates/agents/mpm-reviewer.md`,
`agent_installation/tests/test_reviewer_bwrap_sandbox.py`, `docs/SPEC.md`,
and this directory's `README.md`.

**No duplication:** exactly one `templates/agents/mpm-reviewer.md`, one
`src/sandbox-exec.ts`, one `scripts/reviewer_bwrap.py` and one
`scripts/reviewer_sandbox.py` in the tree.

---

## 2. The independent review, and the delta

### 2.1 The review chain

| round | scope | verdict |
|---|---|---|
| **Full independent implementation review** (a different model from the one that wrote the code) | the whole A3 implementation | **APPROVE** — three findings (a metadata disclosure of the operator's home path, the full-history copy, a wrong comment), each dispositioned and recorded in `charlie-a3-implementation-review.md` |
| **Narrow delta review** | commit `a19b1a78` only — the one post-review commit that changed the namespace | **REQUEST CHANGES** — one CRITICAL, one MAJOR, one MINOR (see below) |
| **Narrow verification** | the fix, `fc1af83f` | **APPROVE** |

The full review's evidence is `charlie-a3-implementation-review.md`; the
two delta rounds are `delta-review-a19b1a78.md`.

### 2.2 The delta, classified commit by commit

The independent review's scope ended at `23ee0404`. Everything after it
was classified:

| commit | what it changed | security-relevant? | how it was verified |
|---|---|---|---|
| `8fa97292` | the runtime payload manifest (+ its three mirrors) | no — packaging | focused tests: the Go staging fixture (3/3), the three manifest mirrors |
| `267c868a` | the clone's own metadata scrub | no — disclosure *reduction* | focused tests: `DisposableCopyMetadataTest` 4/4 |
| `a19b1a78` | **the toolchain discovery — the namespace changed** | **YES** | **one narrow independent delta review** → REQUEST CHANGES → fixed → re-verified → APPROVE |
| `a810a2ce` | the fake `tsc` in a test stub | no | the install tests, with the fake `tsc` (7/7) |
| `4e8a052b`, `1b4ba218`, `23ffcaa4` | documentation | no | — |
| `fc1af83f` | **the delta fix + the wording correction** | **YES** | the same reviewer, narrow verification → APPROVE |

**The full security review was not repeated.** Only the two narrow
rounds above were commissioned, each scoped to one change.

### 2.3 The blocking finding, and its fix

The delta review found a **CRITICAL** defect that the gate's own
reproduction then confirmed live: a PATH entry whose `bin` directory is a
direct child of a home — `~/bin` — made the toolchain discovery derive
**the home itself** as a bind root, and the home was then bound
READ-ONLY into the namespace. A reviewer could read the operator's
`.ssh/id_rsa`, `.gnupg`, `.config` and the live runtime root `.mpm` with
a plain `cat`.

The fix (`fc1af83f`) refuses the home itself, everything inside it,
every ancestor of it, the filesystem root and every other account's home;
strips only a literal `bin` directory in the derivation; binds the
discovered root **by descriptor**; computes the roots once; and adds a
`roots` subcommand that answers the discovery without touching anything.

**On this operator's host the discovery yields no roots either way** —
`go` lives at `/usr/local/go`, already covered by the static runtime
list — so the namespace at the final head is the same shape the
independent review saw. The fix's effect is measured, not assumed: the
same reproduction against the pre-fix code reports
`discovered roots: [<fake-home>]` and a `--ro-bind` of the home; against
the final head it reports `discovered roots: []`.

**One wording inaccuracy was corrected in the same commit.** The
definition and the installer's docstring claimed that a denied tool is
*"removed from your tool list entirely, so you cannot call one and cannot
see one"*. The plugin registers the MPM tools for **every** agent; the
**host** refuses every call to them. The enforcement was always correct;
the *visibility* claim was never verified. The wording now states the
measured fact. No permission rule, tool registration or installation
logic changed — `check_policy()` inspects rule tuples only, and the
installer's own tests confirm it.

---

## 3. The capability inventory

Every row is a **measurement**, not a claim. The authority for the
permission column is the host's own permission service, asked over its
own API from outside the host, with the built-in agents measured in the
same pass as controls. The corpus is `host-verdicts-final.json`.

| capability | state | enforced by | evidence |
|---|---|---|---|
| **Raw host shell** (`shell`) | **DENIED** | the host's permission service | verdict `deny`; the definition denies it by name; the host's own `build` agent allows it, so the denial is the reviewer's, not the host's |
| **Sandboxed execution tool** (`sandbox_exec`) | **AVAILABLE — and always confined** | the kernel (bubblewrap) | the only `allow` in the reviewer's 24 verdicts; the tool has no privileged mode and no unsandboxed fallback |
| **Host-side file tools** (`read`, `write`, `edit`, `patch`, `grep`, `glob`, `list`, `external_directory`) | **DENIED** | the host's permission service | 8 × `deny`; these run in the host's process, outside any namespace, so the sandbox could not confine them — they are closed rather than confined |
| **MCP tools** (`mpm_*`, 20 named actions) | **DENIED** | the host's permission service | 20 × `deny`; the plugin registers them for every agent, the host refuses the calls |
| **Code Mode** (`execute`) | **DENIED** | the host's permission service | verdict `deny` — it is the bridge to every tool the host publishes |
| **Browser** (`browser`) | **DENIED by name** | the host's permission service | verdict `deny`; a `file://` URL and the upload/download tools move server-local bytes, and no rule confines them |
| **Skill loading** (`skill`) | **DENIED by name** | the host's permission service | verdict `deny`; a capability is text from elsewhere |
| **Network tools** (`webfetch`, `websearch`) | **DENIED** | the host's permission service | 2 × `deny`; and the namespace has no network at all |
| **Subagent delegation** (`subagent`) | **DENIED** | the host's permission service | verdict `deny`; and from inside the namespace there is no agent binary, no MCP endpoint and no socket to delegate to (§4, items 9a–9c) |
| **Session mutation / operator prompt** (`question`) | **DENIED** | the host's permission service | verdict `deny` |
| **MCP resources** (`opencode_*`) | **DENIED** | the definition | named deny; the default deny already covers it |
| **Filesystem access** | **CONFINED by the kernel** | bubblewrap | the writable tree is `/sandbox/{repo,home,tmp,workspace,findings}`, bound READ-WRITE **by descriptor** (`O_NOFOLLOW`); `/etc` is synthetic; every unbound path **does not exist** inside |
| **Process access** | **CONFINED by the kernel** | bubblewrap | its own PID namespace (`/proc/1` is the namespace's init), `--new-session`, `--die-with-parent`, no capabilities (`CapEff: 0`) |
| **Network access** | **CONFINED by the kernel** | bubblewrap | `--unshare-all`; no name resolution, no default route; the host's `/tmp` is a private tmpfs, so the host's sockets are absent |
| **Privilege / namespace escape** | **CONFINED by the kernel** | bubblewrap | `--unshare-user --disable-userns --assert-userns-disabled`; a nested `bwrap` and a nested `unshare -U` are both refused |
| **The reviewer's own tree** | **AVAILABLE, read-write** | the kernel | `/sandbox/repo` (a disposable copy), `/sandbox/findings` (where findings go) |
| **Alpha's own tools** | **UNCHANGED** | — | the `build` agent's 23 allowances and 1 host-default denial are identical before and after; §4 item 8 exercises the plugin's MPM tool route end to end |

### 3.1 The blocking condition

> *Charlie must have no executable path to an unrestricted host shell, a
> privileged filesystem tool or an unconfined subagent.*

**Satisfied, and measured rather than argued:**

- **No unrestricted host shell.** The shell is denied by the host; the
  only execution route is `sandbox_exec`, which is always confined.
- **No privileged filesystem tool.** All eight host-side file tools are
  denied by the host, and the kernel confines what remains.
- **No unconfined subagent.** `subagent` is denied by the host, and the
  namespace holds no agent binary, no MCP endpoint and no socket.

### 3.2 No security authority from model instructions, session ids or agent-controllable environment variables

- **Model instructions** carry no authority: the boundary is the host's
  permission service plus the kernel. The definition's prose is
  advisory; every control it describes is enforced elsewhere.
- **Session identifiers** carry no authority: the session-correlation
  approach was measured and **rejected** — the shell hook's event
  carries no session id, and `OPENCODE_SESSION_ID` is
  agent-influenceable. `sandbox_exec` does not identify its caller at
  all, because it does not need to: it is always confined.
- **Agent-controllable environment variables** carry no authority: the
  protection list comes from the **passwd database** (`pwd.getpwuid`),
  with `$HOME` consulted only as an *additional* protected root, never
  as a replacement. An attacker who sets `HOME=/tmp/attacker` adds a
  protected root and removes none.
- **The model contributes only the command.** The base, the repository
  copy and the protected set are host-resolved; the command arrives as
  a JSON array on stdin, is separated from bubblewrap's options by `--`,
  and a command whose first element begins with `-` is refused.

---

## 4. The integration and adversarial results

**46 checks, 46 PASS, 0 FAIL.** The full transcript is
`gate4-integration-results.txt`.

The test exercised the **actual reviewer execution path** — the
compiled plugin module `dist/sandbox-exec.js` (the `SandboxManager` that
`src/index.v2.ts` registers), which spawns `scripts/reviewer_bwrap.py`
and builds the bubblewrap namespace. It did not exercise the bubblewrap
helper on its own.

Everything it touched was disposable: a throwaway git repository, a
throwaway sandbox base, a throwaway sentinel tree, a throwaway runtime
root, a throwaway `HOME` and a throwaway MPM database. The operator's
checkout, the live MPM database and the operator's home were only ever
READ, and only to answer *"is this absent inside the namespace?"*.

| # | the gate's requirement | result |
|---|---|---|
| 1 | the reviewer can inspect the project, run Go and Python tests, and write findings | **PASS** — the disposable project is visible; `go test ./...` passes; `python3 -m unittest discover` passes; the finding lands in the host's findings directory |
| 2 | every reviewer command executes inside the namespace | **PASS** — a different mount namespace (`mnt:[4026531832]` → `mnt:[4026533010]`), the working directory is `/sandbox/repo`, the namespace mounts the reviewer's tree and not the host's repository, and the process is in the namespace's own PID namespace |
| 3 | no escape through crafted arguments | **PASS** — a command beginning with an option is refused; a nested `bwrap` is refused (`ENOSPC`); a nested `unshare -U` is refused; a traversal out of `/sandbox` lands in the namespace root; `/proc/1/root` is the namespace's init; no credential-shaped variable is in the environment |
| 4 | protected directories and credentials are unavailable | **PASS** — the host-only sentinel, the operator's home, the live runtime root, `/etc/ssl` and the operator's checkout path are all **ABSENT**; `/etc` is synthetic and names `reviewer`, not the operator; the disposable copy's own metadata names no host location |
| 5 | network and mount restrictions are enforced | **PASS** — no name resolution, no default route, `mount` fails (`must be superuser`), `CapEff: 0`, and the host's `/tmp` is a private tmpfs whose sockets are absent |
| 6 | sandbox-creation failure refuses execution, with no fallback | **PASS** — a base inside a protected directory is refused and the command does **not** run; a missing sandbox script is refused; a working directory outside the sandbox's trees is refused; a NUL in the command is refused |
| 7 | installation verifies the actual installed definition and fails closed | **PASS** — the canonical definition installs byte-identical at mode 0600; a definition without the default deny is refused and nothing is installed; an existing definition is not overwritten without `--force`; **a corrupted write is caught by the read-back** (the installer's verification is against the *installed* bytes, and it refuses to report success); a definition that re-opens the host shell is refused |
| 8 | Alpha's ordinary engineering tools remain functional | **PASS** — the plugin's own binary resolution and workspace pinning drive `mpm_memory save`/`query` and `mpm_system health_check` end to end against a **disposable** database (`db_path` = the disposable workspace), and no database was created outside it. The `build` agent's permission verdicts are unchanged: 23 allowances, and the single denial is the host's own default for `browser` |
| 9 | the reviewer cannot delegate to an unrestricted agent | **PASS** — `subagent` is denied by the host; from inside the namespace there is no agent binary, no MCP or host session variable, and no runtime socket |
| 10 | the installed plugin and its bundled files work together | **PASS** — the payload is staged through the **real** staging library with the **real** manifest into a disposable runtime root; the staged package carries `dist/sandbox-exec.js` **and** `scripts/reviewer_bwrap.py`; the staged module resolves the **staged** script and runs a command through it |

### 4.1 The local gates at the final head

| gate | result |
|---|---|
| `agent_installation` suite | **581 OK** (2 skipped) |
| the adapter's Python suite | **162 OK** |
| the adapter's TypeScript suite | **121 pass, 0 fail** |
| `scripts/tests` | **912 OK** (1 skipped) |
| `go test -tags fts5 ./...` | **EXIT 0** |
| `go vet -tags fts5 ./...` | **clean** |
| `go build` / `tsc` | **clean** |
| `mpm-lint --gate` | **PASS** |
| `make docs-check` | **OK — 13 checks, 0 errors** |

---

## 5. The CI state

| run | head | result |
|---|---|---|
| `44081964509` | `a810a2ce` (the implementation head) | **success** — all 3 jobs |
| `44092644993` | `23ffcaa4` (the report head) | **success** — all 3 jobs |
| `44107514553` | `fc1af83f` (**the reviewed code head**) | **success** — all 3 jobs |
| the final head (this report) | one commit later | see the PR body — documentation only |

Two earlier runs failed (`44063808134`, `44075419731`); both were
diagnosed and fixed, and both fixes are in the history (`8fa97292`,
`a810a2ce`).

---

## 6. The remaining documented risks

### 6.1 Accepted by the operator

1. **Mount-table metadata disclosure.** The namespace's own mount table
   names the host path of every mount it was given. The source path is
   relative to the filesystem the base lives on: on the operator's root
   filesystem it reads `/home/<user>/.cache/…` (the full path and the
   account name); on a tmpfs base it reads only the path within that
   tmpfs. **Limited host-path metadata only — no access to the
   referenced resources.** This remains documented in
   `a3-completion-report.md`.
2. **The full Git history in the disposable clone.** Accepted for this
   stage, provided the sanitisation controls remain effective — they do:
   the origin remote, the reflog and `FETCH_HEAD` are removed, and the
   copy's own metadata names no host location (measured, item 4g).
3. **The sandbox covers the configured Charlie reviewer only.**
   `explore`, `general` and Alpha remain outside the guarantee, by the
   operator's explicit scope. The measurements in
   `host-verdicts-final.json` show their verdicts unchanged.

### 6.2 New or changed by this gate

4. **The toolchain discovery is inert on this host.** `go` lives at
   `/usr/local/go`, already covered by the static runtime list, so the
   discovery finds nothing and the namespace shape is unchanged. On a
   host that keeps its toolchain elsewhere (a CI runner's hosted cache
   under `/opt`) the discovery binds it read-only **by descriptor**.
5. **A host that keeps its toolchain inside a home directory gives the
   reviewer no compiler there.** This is the deliberate cost of
   refusing every path under `/home`. A limitation, not a hole.
6. **The residual host-side TOCTOU.** The discovered root is resolved
   with `realpath` and then bound by descriptor, so the reviewer's tree
   and the toolchain root share the same guarantee. A concurrent
   host-side swap between the resolution and the open remains possible
   and needs a second actor on the host — outside the reviewer's reach,
   and outside the threat model.
7. **The sandbox base is readable by the operator's account.** The base
   holds the disposable copy; the operator can read it, which is the
   operator's own tree.
8. **The Go module cache is withheld** (`GOPROXY=off`), so a build that
   needs a dependency it does not already have fails. Expected, and
   stated in the definition.
9. **The toolchain and the runtime inside the namespace are executable
   by design.** That is the point of the sandbox: the reviewer must be
   able to run the project's tests.

### 6.3 Not in scope, and not opened

- The shell-matcher campaign remains **closed**. Its 4361-line guard was
  deleted; its record stays in this directory. No matcher work was
  restarted.
- This PR is **not** a general subagent-sandbox project. Only the
  configured Charlie reviewer is covered.

---

## 7. What was not touched

- The **operator's checkout** (`<operator-checkout>`): 20 dirty files at
  `c2ed9e82` before and after the gate, unmodified.
- The **installed agent definition** (`~/.config/opencode/agents/mpm-reviewer.md`):
  md5 `89c9aadfd8ae970be1a2ec81d96aa22a`, unchanged. The gate's
  installs went to disposable directories.
- The **production MPM database**: never opened. The only database the
  gate created is the disposable one under the disposable workspace.
- The **scheduler**: never signalled.
- The **live substrate**: no `mpm_*` tool was called on it.

---

## 8. The artefacts

| file | what it is |
|---|---|
| `a3-merge-gate-report.md` | this report |
| `delta-review-a19b1a78.md` | the narrow delta review (REQUEST CHANGES), the live reproduction, the fix, and the verification of the fix (APPROVE) |
| `host-verdicts-final.json` | the host's verdicts for every agent and action, measured at the final head |
| `gate4-integration-results.txt` | the full transcript of the 46-check integration and adversarial suite |
| `charlie-a3-implementation-review.md` | the independent review of the implementation — APPROVE |
| `host-verdicts-a3.json` | the earlier verdict measurement, kept as the record of the implementation head |
| `a3-completion-report.md` | the completion-gate report for the implementation |
| the round-22/23 reports, `decision-report.md`, `final-report.md` | the preserved campaign record |
