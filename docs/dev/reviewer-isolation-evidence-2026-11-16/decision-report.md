# P6 follow-up — the convergence decision

**PR A** `fix/skill-packages-p6-provenance` → #244 · head `173e50e9f2dca00b36d8c8a56fed0283c20bed53` (31 commits, 23 files) — OPEN, unmerged
**PR B** `feat/skill-packages-p7` → #245 · head `bd0236e375c9d4711ee9aaf3a7c3ff952e864265` — APPROVED, open, unmerged, unchanged

Nothing was merged, deployed or scheduled. The live substrate was never opened.
The operator's checkout (`/home/v/src/mpm`) is untouched: 20 dirty files, HEAD `c2ed9e82`, branch `feat/runtime-migration-cutover`.

---

## 1. What is already complete and verified

### A1 — the complete hash provenance · **COMPLETE**

Three hashes, each with a recorded origin — `projection_sha256` (recorded when the package is written, validated against the package manifest on read), `body_sha256` (recomputed from the body on read), `entrypoint_sha256` (the package's original entrypoint hash) — plus a `hash_origins` object that names which origin each hash carries, so a reader never guesses. A manifest that validates but **contradicts** the stored projection fails **closed** (`integrity_failure`); an unparseable manifest is **tolerated** (`unavailable`), because a malformed manifest is not evidence of corruption.

Verified across 23 review rounds. The mutation proof holds: rewriting the exported read makes the deterministic tests fail.

### A2 — the single-snapshot read · **COMPLETE**

`ReadSkillWithPackageProvenance` reads the skill body and its package provenance from **one** consistent database snapshot, so a concurrent writer cannot make the two halves disagree. The mutation proof holds: rewriting the read as two independent reads fails both deterministic tests (`opened 2 snapshot(s), want exactly 1`).

### A3 — reviewer isolation · **ONE PART COMPLETE, ONE PART OPEN**

**Complete and verified** (enforced by the host, not by prose):

* the MPM tools are removed from the reviewer's **catalogue** — every registry tool, checked against `internal/core/tools/registry_list.go` by a guard test that fails when a tool is added without a deny rule;
* `edit`, `subagent`, `question`, Code Mode (`execute`), the MCP resource surface and the session/worktree mutations are denied — they are absent from the catalogue, not merely discouraged;
* the `read` / `glob` / `external_directory` boundary of a runtime root is closed;
* the shell rules deny the binaries wherever they are **named** — seven forms per binary plus three shared forms — and deny the installers and the service-control commands;
* the reviewer is required to run with an isolated `HOME` / `TMPDIR` / `MPM_WORKSPACE` and to record findings in a **filesystem scratchpad**;
* the guard test proves the rule set non-vacuous in **91 directions** (10 rule-level, 81 model-level), each failing with an **assertion** about the behaviour it names, with the baseline green before and after;
* the model of the host's matcher agrees with the host on **414 measured pairs**, with 5 recorded divergences, all in the safe direction (the model refuses what the host allows).

**Open**: the *shell* is not confined by the operating system. The permission layer is a pattern match on a command string, so ten measured residual routes remain, and the documents say so.

### This head's new work (round 23's remediation, already committed)

The structural scanner was rewritten as a **parser** and made the **single enforcer** for the structural class; the class-level table carries a **reason per form** (the host's reason depends on the construct that holds the defect); round 23's 34 divergent forms are pinned in the corpus in both directions; the housekeeping assertion now sees module-level definitions; the definition's family list is machine-readable and checked against the model.

**And — the work this report is about — a namespace sandbox**, `agent_installation/mpm-opencode/scripts/reviewer_bwrap.py`, with its contract pinned by twelve tests. It is **inert**: it is not wired into the reviewer's shell yet.

---

## 2. Which guarantees are actually enforced by the host or the OS

| Guarantee | Enforced by | Kind |
|---|---|---|
| The MPM tools are not in the reviewer's catalogue | the host's permission layer | **capability removal** — enforced |
| `edit`, `subagent`, `question`, Code Mode, the MCP surface | the host | **capability removal** — enforced |
| A live runtime root is closed to `read`/`glob`/`external_directory` | the host's path rules | pattern — enforced for the paths it names |
| The binaries are refused in the ten named forms | the host's command matcher | pattern — enforced for the forms it names |
| The names reach the binary in **no other form** | — | **NOT closed** — ten measured residuals |
| `HOME`/`TMPDIR`/`MPM_WORKSPACE` point at scratch | the sandbox script | **resolution** — the process still sees the operator's files |
| The namespace (when used) hides every unbound path | **the kernel** | **boundary** — enforced |
| The namespace (when used) has no network, no capabilities | **the kernel** | **boundary** — enforced |

## 3. Which protections are advisory or bypassable

* **The shell matcher.** Ten measured routes still reach the binary: a name split by a substitution (`$(echo mp)m`) or by a quote (`mp""m`), a name held in a variable, a script file, a shell glob, a brace expansion, a name glued to punctuation, a backslash escape, a package-directory pattern, and the host's refusal to match a `cd` segment's text. **Advisory** — every one of them is a pattern the rules do not name.
* **The environment sandbox.** A resolution rule. The process can still *read* the operator's files.
* **`grep` and `glob`.** Their resources are the pattern and the glob, not a path, so they are not path-restricted.
* **The browser family.** Not governed by the permission layer on this host; a connected desktop browser could open a `file://` URL. (The host's own `webfetch` refuses `file://` — measured.)
* **The capability loader.** Not gated by any permission action on this host: a `skill` call runs. The rule that claimed otherwise was removed, and the definition now says a reviewer must not load one — an instruction, not a control.
* **The write path.** The permission layer reads the content of a write, so a document that merely mentions a name is refused. Deliberate, and a practical cost: a report that must quote the binary literally cannot be written by a shell command.
* **The model's fidelity.** Corrected against the host by measurement, never a proof about the host.

**Classification today: the shell protection is BEST-EFFORT. The tool-catalogue protection is ENFORCED.**

---

## 4. Is genuine sandbox isolation feasible with the installed tooling? — **YES, MEASURED**

The host has **bubblewrap 0.11.1** (`/usr/bin/bwrap`), unprivileged user namespaces enabled (`unprivileged_userns_clone=1`, `max_user_namespaces=122793`), and no container runtime (no podman, docker, firejail or nsjail) — and it does not need one: bwrap needs no root.

**The sentinel experiment** (`reviewer_bwrap.py probe`, against disposable sentinels — never the real substrate):

```
INVISIBLE (GOOD): the disposable protected tree
INVISIBLE (GOOD): the live runtime root
INVISIBLE (GOOD): the operator's home
WRITABLE (GOOD):  the disposable workspace
NO NETWORK (GOOD)
NO ESCAPE (GOOD)
```

The review can still do its job inside: `go1.27.3`, `python3 3.14.4`, `git 2.53.0` all work, and the guard suite ran inside the namespace (24 tests, OK).

**The enforcement path is measured too.** A probe plugin installed in a disposable OpenCode instance showed that:

* the host **executes what the `create.before` shell hook hands it** — the hook rewrote a command to `echo MPM_PROBE_REWRITE_MARKER; echo ORIGINAL_COMMAND_RAN` and the shell ran exactly that. So a plugin *can* wrap every shell command;
* the hook's event carries `command`, `cwd`, `timeout`, `shell`, `env` — and **no session identifier**, so a plugin cannot yet tell a reviewer's command from the operator's. The environment does carry `OPENCODE_SESSION_ID`, so the correlation is possible.

**One caveat, stated plainly:** the host-side file tools (`read`, `write`, `grep`, `glob`) run in the **host's** process, outside any namespace. Confining *code execution* is solved; confining *host-side file access* is not, and the permission layer's path patterns remain the only control there. The way to close that is **capability removal**: deny the reviewer the file tools and give it file access through the sandbox.

---

## 5. The smallest remaining changes for a defensible deliverable

1. **Wire the namespace** (plugin change, in the `mpm-opencode` plugin):
   either (a) the `create.before` hook rewrites a reviewer's command into `reviewer_bwrap.py run -- …`, identifying the reviewer by `OPENCODE_SESSION_ID` correlated with the session hook; or (b) — simpler and stronger — the plugin registers a **sandboxed-exec tool** and the reviewer's permission set **denies the raw `shell` action**, so there is no unsandboxed execution path at all. **(b) is the recommendation**: it removes a capability instead of matching a string.
2. **Close the host-side file tools for the reviewer** — deny `read`/`write`/`edit`/`grep`/`glob` and let the reviewer reach files through the sandbox. `webfetch`/`websearch` may stay (no filesystem access; `file://` is refused — measured).
3. **Publish the classification** in the definition, the SPEC and the PR body: what is enforced, what is best-effort, and what the namespace closes. (This head already states it; the wiring commit must keep it true.)
4. **Test it**: the namespace contract (done — 12 tests) plus an integration test that a reviewer's shell command *is* wrapped, and one that the unwrapped operator's command is not.

That is the whole of it. It does not require another grammar pass, another mutation campaign, or another review round of the matcher.

---

## 6. Should PR #244 proceed intact, or should A3 be separated? — **SEPARATE**

**Recommendation: land A1 + A2 now; carry A3 forward as its own PR whose blocking scope is the namespace wiring.**

Why:

* A1 and A2 are **complete**, have no open findings, and depend on nothing in A3. They are mergeable on their own merits.
* A3's remaining work is a **different kind of work** — a plugin and a capability design — with a different reviewer brief and a different convergence criterion: *a demonstrated escape*, not *another shell form*. Keeping it in the same PR makes every A1/A2 change wait for it, and it makes the PR's review surface unbounded, which is exactly what 23 rounds have demonstrated.
* The matcher work that A3 already contains is **defence in depth**, not the critical path. Its evidence stays (the corpus, the 414 measured pairs, the 91 directions, the family table) and is worth keeping — but it should not be the thing that blocks the merge.
* The operator's own criterion applies: A3 *can* reach a defensible isolation guarantee, but it needs a new implementation cycle, not a new review round.

**Proposed shape**

| PR | Contents | Gate |
|---|---|---|
| #244 (revised) | A1 + A2 + the guard/corpus evidence | the operator's blocking list; no open finding |
| new PR | A3's namespace wiring: the plugin change, the capability removal, the classification | a demonstrated escape blocks; a matcher discrepancy does not |

Both stay open; neither merges without the operator's approval.

### The finite convergence gate, mapped onto this work

**Blocking** — a demonstrated production exposure **across the actual isolation boundary** (a command that runs *inside the namespace* and reaches the operator's data); incorrect A1/A2 behaviour; a regression in an existing public contract; a failed required test or CI check; an untruthful security or compatibility claim.
**Non-blocking unless a concrete failure is demonstrated** — further matcher grammar coverage; already-documented host/model divergences; harness refinements that do not touch the boundary; cosmetic or low-impact findings.

The ten documented residual routes stay documented, and they become **closed by construction** once the namespace is in force — not by adding grammar.

---

## What was done in this session

* **Committed and pushed** `2f85712a` — the round-23 remediation (the scanner as a parser and the single enforcer; the reason per form; the review's forms pinned in both directions; the housekeeping assertion widened; the definition's family list checked against the model). 91 sweep directions, all failing with an assertion; 414 measured pairs; 523 tests OK; `make docs-check` OK; `go vet` ×3 clean; `go build -tags fts5` clean; `mpm-lint --gate` PASS. CI at that SHA: "Full test suite" **PASS**, "Build + core tests" **PASS**, "Release gate" running.
* **Committed and pushed** `173e50e9` — the namespace sandbox and its twelve-test contract, the three-layer classification in both READMEs and the SPEC, and the honest statement in the PR body that the namespace is **not wired in yet** and that today's shell protection is **best-effort**.
* **No review round was commissioned.** The round-24 brief was written (`/tmp/opencode/review-a24-brief.md`) and **not** used, per the direction to stop the matcher-fidelity cycle.
* The live substrate was never opened; the probes used disposable trees and disposable sentinels only.
