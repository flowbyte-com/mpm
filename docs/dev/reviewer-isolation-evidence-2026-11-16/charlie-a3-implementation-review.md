# The independent external review of the A3 implementation

> **What this is.** The security review of the A3 implementation — the
> reviewer-isolation boundary — carried out by an **independent reviewer
> subagent** on a **different model** from the one that wrote the
> implementation. The reviewer was given the files to read, the claims to
> attack, and the questions to try to break; it was told **not** to fix
> anything, so that the record is a review and not a second
> implementation.
>
> **Date:** 2026-11-17. **Model:** `deepseek/deepseek-v4-pro` (the
> implementation was written by a different model).
> **Verdict:** **APPROVE**.
>
> **This is a record, not an implementation.** Nothing here changes the
> substrate's behaviour. The code these findings examined lives on the
> branch that carries this directory.

## The brief the reviewer was given

The reviewer was asked to:

- attack the boundary: find the ways the less-trusted reviewer agent
  ("Charlie") could reach the operator's data, and find any claim in the
  completion report that the code does not support;
- escape the namespace by **any** route — a symlink in the reviewer's own
  writable tree, a bind of the host root through an argument the model
  controls, `/proc`, `/dev`, `/sys`, the merged-usr symlinks, a setuid or
  capability trick, a nested user namespace, `open_by_handle_at`, the
  `/etc/alternatives` copy, the Go caches, the disposable copy itself, and
  the sandbox base itself;
- find **any** path where a command runs without the sandbox, and any
  field, flag or environment variable that changes the mode;
- find any action or tool the host knows that the definition does not
  close, and any rule-ordering mistake that would defeat the host's
  "last matching rule wins" evaluation;
- make the install **fail open**; and
- find a claim in `a3-completion-report.md` that the code does not
  support.

The reviewer was also given the findings that were **already known**, so
that it would confirm or refute them rather than re-report them.

---

## VERDICT: APPROVE

> The boundary holds against every attack I could construct. The namespace
> is genuinely confined (zero capabilities, `no_new_privs` set, nested user
> namespaces blocked, host paths absent, no leaked file descriptors), the
> permission definition is correctly ordered for the host's `findLast`
> evaluation, and the installer fails closed. The findings below are
> metadata/scope disclosures, not routes to the operator's data.

---

## Findings

### 1. MINOR — the reviewer learns the operator's username and home path (metadata)

**Where:** `agent_installation/mpm-opencode/scripts/reviewer_bwrap.py` —
the `--ro-bind-fd` / `--bind-fd` mounts, and the `git clone` of the
repository under review.

Two independent channels disclosed the home path to the less-trusted
model:

- The writable tree and the synthetic `/etc` are bound **by file
  descriptor**, but bubblewrap resolves the descriptor to its real path
  before mounting. Inside the namespace, `/proc/self/mountinfo` therefore
  records the host source path. The reviewer reproduced this: the mount
  table showed the base's real on-disk path (in production
  `~/.cache/mpm-opencode/review-sandbox/…`), disclosing `/home/<user>` and
  the username.
- The disposable repository was a `git clone`; its `.git/config` carried
  `remote.origin.url = <the operator's checkout path>` — an absolute path
  under the home, disclosing the home directory and the username.

The reviewer was explicit about the weight of this: it **does not grant
access to any file** — the revealed paths are not bound, and the mount
table is the reviewer's own — but the test docstring in
`test_reviewer_sandbox_exec_path.py` claimed that "the operator's account
name and home path are not disclosed", which the code does not fully
support. **It is a one-way metadata leak, not a data breach.**

### 2. NIT — the disposable copy is the full git history

**Where:** the `git clone --no-hardlinks` in the copy step.

The copy includes all refs and the complete history, so secrets that were
committed and later deleted or rotated remain readable through
`git log -p` inside the sandbox. The reviewer judged this to be within
"review the repository" scope, but wider than the report's framing of the
copy as "what the operator is actually looking at", and asked for an
explicit line in the residual-risk list.

### 3. NIT — a misleading comment about `--clearenv` ordering

**Where:** the `--clearenv` / `--setenv` construction.

The comment said "`--clearenv` comes FIRST: bwrap processes its arguments
in order". That rationale is wrong: bwrap collects `--clearenv` and
`--setenv` and always clears-then-sets regardless of argument order. The
**behaviour** is correct — the reviewer confirmed the child environment
contains only the sandbox's own variables — but the comment could mislead
a future edit into "fixing" a non-problem.

---

## Claims the reviewer could not break

The reviewer confirmed each of the implementation's numbered claims
against the executing code, not against its text:

| claim | the reviewer's finding |
|---|---|
| **1** — default-deny, exactly one allowed action | **confirmed** — the definition puts `action:"*"` deny **first** and `sandbox_exec` allow **second**, which is the correct order for the host's `findLast` evaluation |
| **2** — always sandboxed, no privileged mode | **confirmed** — `sandbox-exec.ts` spawns only the Python script; the base, the source and the protected set are host-resolved; no flag, environment variable or field reaches the mode |
| **3** — JSON on stdin, the `--` separator, a leading `-` refused | **confirmed** in code **and empirically** |
| **4** — the namespace's shape | **confirmed empirically** — `/usr` read-only, `/sandbox` writable, `/etc` synthetic, `/tmp` a tmpfs, the network unshared, and `/home`, `/root`, `/sys`, `/etc/shadow`, `/etc/sudoers` all **absent** |
| **5** — `O_NOFOLLOW` descriptor binding, passwd-derived protection, symlinked base refused | **confirmed** in code |
| **6** — no fallback | **confirmed** — `execute` refuses when bubblewrap is missing; the command never runs |
| **7** — the installer fails closed, `install.sh` treats a failed install as fatal | **confirmed** — the installer parses, verifies and reads the copy back; `install.sh` exits 1 |
| **8** — the scope limits | **confirmed** |
| **§5.2** — the option-injection escape is closed | **confirmed** — the `--` separator, the JSON-on-stdin payload and the leading-`-` refusal are all present and tested |

**The one statement the code did not fully support** was the test
docstring's claim that the operator's account name and home path are not
disclosed — Finding 1. That was a test/prose statement rather than one of
the report's numbered claims, and it was a metadata disclosure rather
than data access.

---

## What the reviewer could not test

Stated so that the limit of this review is visible rather than implied:

- **The host's permission service (`findLast`) semantics.** The reviewer
  confirmed the definition's *rule order* is correct for last-match-wins,
  but could not re-run the §3.1 measurement, because no OpenCode server
  was available to it. The "one action allowed" outcome rests on the host
  treating `action:"*"` and `resource:"*"` as wildcards, which the
  reviewer could only infer, not measure. **The measurement exists
  elsewhere**: `host-verdicts-a3.json`, taken from the host's own
  permission service, with the built-in agents as controls.
- **The `--assert-userns-disabled` fail-closed path.** The reviewer
  confirmed that `--disable-userns` blocks `unshare(CLONE_NEWUSER)`
  (seccomp; observed `unshare: … No space left on device`), but could not
  remove seccomp support to verify the "refuses to start" branch.
- **`install.sh` end to end.** Running it would write into
  `~/.config/opencode` and `~/.mpm`, which the reviewer must not do; it
  reviewed the control flow statically instead.

The reviewer also reported a **process** observation worth keeping: its
`read` tool returned a **stale** copy of `reviewer_bwrap.py` (showing a
whole-home-protection variant that no longer exists). It established
ground truth by importing the actual files and re-reading them. A review
that trusts a cached read is a review of the wrong code.

---

## The disposition of the findings

What was done about each finding, and what was deliberately not done.

| finding | disposition |
|---|---|
| **1 (MINOR)** — the username and home path are disclosed | **Partly closed, partly disclosed.** The **clone's** channel is closed: the copy's origin remote, its reflog and its `FETCH_HEAD` are now **removed**, so the copy names no host location, and a test pins that. The **mount-table** channel remains: bubblewrap resolves a descriptor to its real path before mounting, so `/proc/self/mountinfo` inside the namespace records the base's host path. It cannot be closed without giving up the descriptor binding — which is the property that stops a symlink swap from changing what is mounted — or without relocating the base off the operator's filesystem, which trades a metadata disclosure for a weaker base. It is therefore **disclosed as a residual risk**, and the test docstring that over-claimed is corrected to say exactly what is true: the account database is not readable inside, and the paths the mount table names are not bound. |
| **2 (NIT)** — the copy is the full history | **Accepted as scope, disclosed.** The history is the review material; a reviewer that could not read the history could not review the change. The **report** now states it as a residual risk: a secret that was committed and later deleted or rotated is still readable in the copy. |
| **3 (NIT)** — the `--clearenv` comment's rationale | **Fixed.** The comment now states what bwrap actually does — it clears and then sets, whatever order the flags appear in — and says that the order is kept because it *reads* correctly, so that nobody "fixes" a non-problem. The behaviour was already right and is unchanged. |

The review's verdict and its findings are also summarised in
`a3-completion-report.md` §9 and §10, and the review's disposition is
recorded in the branch history.
