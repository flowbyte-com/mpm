# A3 — the completion-gate report

> **What this is.** The evidence-backed report for the focused A3 security
> work on PR #247: the boundary around the less-trusted reviewer agent
> ("Charlie"). It states what is **enforced**, what is **not**, what was
> **measured**, and what remains **at risk**. It is written to be read by
> somebody who will not trust it — every claim below is either a measurement
> with a reproduction or a statement about what is *not* enforced.
>
> **Date:** 2026-11-17. **Branch:** `feat/reviewer-sandbox-isolation`.
> **Status:** the branch is **draft** and **not merged**, **not deployed**, and
> **not installed** on the operator's machine. Merging and deployment require
> the operator's explicit approval.

---

## 1. The trust model this implements

| party | trust | what it may do |
|---|---|---|
| **Alpha** — the operator's own session | **trusted** | ordinary engineering: the host shell, the host's file tools, the network, the MPM substrate, the scheduler. Unchanged by this work. |
| **Charlie** — the configured reviewer subagent | **less trusted** | read and run code **inside a bubblewrap mount namespace**, and nothing else. Its model output, its commands and the repository content it reads are **not** trusted to respect OS boundaries. |

The consequence of "less trusted" is that Charlie's boundary cannot be a
**pattern match on a command string**. A permission rule is a matcher; a route
the matcher does not name is still a route, and twenty-three rounds of matcher
fidelity work did not change that — it established it. So Charlie's boundary
is a **capability removal** plus an **OS-level** boundary.

---

## 2. The architecture, in one paragraph

Charlie's agent definition is **default-deny**: `action: "*"`, `resource: "*"`,
`effect: deny`, with exactly **one** action re-opened — `sandbox_exec`. That
tool is registered **host-side** by the installed plugin, it is **always**
sandboxed, it has **no privileged mode**, and it takes its sandbox base, its
repository copy and its protected set from the **host**. Everything the model
controls is the **command**, and the command is **data** to the sandbox, never
configuration for it: it arrives as a JSON array on stdin, it is separated from
the namespace's options by `--`, and a command whose first element begins with
`-` is refused outright. There is **no unsandboxed fallback**: if bubblewrap is
missing or the namespace cannot be built, the command does **not** run and the
model receives the refusal.

The **alternative** — rewriting the command in the host's shell hook and
identifying the reviewer's command by a session identifier — was **measured and
rejected**: the hook's event carries the command, the working directory, the
shell, the timeout and the environment, but **no session identifier**, and the
environment's `OPENCODE_SESSION_ID` is **agent-influenceable**. A security
authority the agent can influence is not an authority.

---

## 3. What is ENFORCED, and by what

### 3.1 The tool catalogue — enforced by the host's permission service

**MEASURED** on the operator's installed host (OpenCode 2.3.0), in an isolated
server whose `HOME`, `XDG_*` and `MPM_WORKSPACE` all pointed at disposable
directories. The measurement asks the **host's own permission service** for its
verdict, for each action, after switching a session to the agent. The record is
`host-verdicts-a3.json`.

| action | the host's verdict for `mpm-reviewer` | control: `build` | control: `explore` | control: `general` |
|---|---|---|---|---|
| `shell` | **deny** | allow | allow | allow |
| `read` | **deny** | allow | allow | allow |
| `write` | **deny** | allow | deny | allow |
| `edit` | **deny** | allow | deny | allow |
| `patch` | **deny** | allow | deny | allow |
| `grep` | **deny** | allow | allow | allow |
| `glob` | **deny** | allow | allow | allow |
| `list` | **deny** | allow | deny | allow |
| `external_directory` | **deny** | allow | allow | allow |
| `execute` (Code Mode) | **deny** | allow | deny | allow |
| `browser` | **deny** | deny | deny | deny |
| `skill` (capability loader) | **deny** | allow | deny | allow |
| `webfetch` | **deny** | allow | allow | allow |
| `websearch` | **deny** | allow | allow | allow |
| `subagent` | **deny** | allow | deny | allow |
| `question` | **deny** | allow | deny | allow |
| `sandbox_exec` | **ALLOW** | allow | deny | allow |
| all 21 `mpm_*` tools | **deny** | allow | deny | allow |

**One action is allowed: `sandbox_exec`.** The controls are what make the
refusals meaningful: they were measured in the same pass, through the same
mechanism, and they keep their ordinary permissions. Without them, a
"everything is denied" result would be indistinguishable from a broken
mechanism.

A measurement note that matters for anyone repeating it: the host loads its
agent documents through a **watcher-driven** configuration pass, and a freshly
started server does not always load them before the first question is asked. An
agent the host does not know is refused **every** action — indistinguishable,
from outside, from a definition that denies everything. The harness therefore
**touches** the definition before measuring, which fires the watcher and makes
the host reload its agent registry. That is why the earlier all-deny readings
were not the definition's verdict, and why the definition's **one** allowed
action is the tell.

### 3.2 The filesystem and the network — enforced by the kernel

`agent_installation/mpm-opencode/scripts/reviewer_bwrap.py` builds a bubblewrap
mount namespace. Inside it:

| thing | state inside the namespace | why it matters |
|---|---|---|
| `/usr`, `/usr/local` | bound **read-only** | a writable runtime is not confinement |
| `/sandbox/repo` | the **disposable copy** of the repository under review, read-write | the operator's checkout is never the writable tree |
| `/sandbox/home`, `/sandbox/tmp`, `/sandbox/workspace`, `/sandbox/findings` | the reviewer's own scratch, read-write | `HOME`, `TMPDIR` and `MPM_WORKSPACE` point inside |
| `/etc` | **synthetic**: a passwd and a group file naming `reviewer`, and a copy of the host's `alternatives` symlinks | the operator's account database, the trust store at `/etc/ssl` and the private keys under `/etc/ssl/private` are **not readable** |
| `/tmp` | a **private tmpfs** | the host's `/tmp` is invisible |
| the network | **unshared** | no MCP endpoint, no remote service |
| every other path | **not bound at all** | an unbound path is not refused — it is **absent** |

The writable tree is bound by **file descriptor**, opened with `O_NOFOLLOW`: a
descriptor names the **inode**, so a path swapped for a symlink after the check
cannot change what is mounted. The sandbox **refuses** a base that resolves
inside a protected root, and the protected list is built from the **passwd
database**, not from `$HOME` — an environment variable cannot decide what is
protected. (`$HOME` is consulted as an *addition*, so forging it adds a
protected root and removes none.)

### 3.3 The install — enforced by the installer, failing closed

`install_reviewer_agent.py` parses the canonical template, checks its **policy**
(the default deny, the one allowed action, every host-side capability, and every
MPM tool the registry publishes), **refuses** a definition that does not satisfy
it, writes the copy atomically with mode `0600` in a `0700` directory, reads it
**back**, compares it byte-for-byte with the template, checks its policy again,
and exits **non-zero** if any of that fails. `install.sh` treats a failed
reviewer install as **fatal**: a deployment step that fails open converts an
error into a **silent downgrade** of the guarantee.

---

## 4. What is NOT enforced, and what Charlie does instead

Stated rather than glossed.

| # | not enforced | what it means | what Charlie does instead |
|---|---|---|---|
| 1 | **The operator's own session** | the boundary protects the operator's data from a less-trusted reviewer; it does not protect the operator from themselves | nothing — Alpha is trusted by design |
| 2 | **The built-in `explore` and `general` subagents** | the boundary covers the **configured reviewer environment** only; those agents run with the host's ordinary tool surface | they are not used for review in this design; a common sandbox policy for other delegated agents is a **future** decision, not a property of this one |
| 3 | **The Go module cache** | not bound by default, so a build that needs a dependency the sandbox has not cached **fails** | the reviewer runs the tests it can; an operator who wants the cache bound can bind it explicitly, which is a capability grant with a disclosure cost |
| 4 | **The sandbox base on the host's filesystem** | the operator's own account can read it | the boundary protects the reviewer from the host, not the host from the reviewer; the base is `0700` and holds only a disposable copy |
| 5 | **The host's own `opencode_*` and MCP resource surface inside the operator's session** | these are not the reviewer's to reach either — they are closed for it by name | nothing |
| 6 | **A compromised or malicious privileged Alpha** | out of scope by construction; Alpha is trusted | nothing |

**The scope, stated plainly:** this is an OS boundary around **the configured
reviewer execution environment**. It is **not** automatically a boundary around
every OpenCode subagent, and it makes no claim about the operator's own
session.

---

## 5. The escape probes

Every probe below ran **inside the namespace**, through the **exact invocation
the plugin makes** (`python3 reviewer_bwrap.py --base … --protect … exec`, with
the command as JSON on stdin and the result envelope on file descriptor 3), and
every probe uses **disposable** sentinels. The live substrate, the operator's
credentials and the operator's checkout are **never opened** — they are
**named** at most, and naming a path is not opening it.

### 5.1 The probes that FAILED to escape (the boundary held)

| probe | result |
|---|---|
| read a disposable sentinel file outside the sandbox | **refused** — the path does not exist inside |
| read a disposable sentinel database outside the sandbox | **refused** |
| list the protected directory | **refused** — not even in the listing |
| list the operator's home directory | **refused** |
| read a file under the live runtime root (`~/.mpm/…`) | **refused** |
| list `/etc/ssl` (the trust store) | **refused** |
| read the host's `/etc/passwd` | **the sandbox's own** passwd is returned — the host's is not bound |
| resolve `example.com` (the network) | **no resolution** — the network is unshared |
| `ls /home` (a bare escape attempt) | **refused** |
| `--bind / /` — mount the host's root into the namespace | **refused**: the command's first element begins with `-` |
| a command whose first element is `--version` | **refused** |
| a working directory outside the sandbox | **refused** |
| a missing bubblewrap | the command **does not run on the host** — the marker file is absent |

### 5.2 The escape that DID work, and was closed

**This is the important one.** The pre-hardening script's `run` path appended
the command to the bubblewrap argument list **without a separator**, so a
command vector could carry an option into bubblewrap's own parser:

```
reviewer_bwrap.py --base <base> run -- --bind / / /bin/sh -c 'ls /home; cat /etc/passwd'
```

**Reproduced against the pre-hardening script**: bubblewrap mounted the host's
root into the namespace, and the command read `/home` and `/etc/passwd` — a
**complete escape**. It was closed by three independent changes, each of which
is tested: the `--` separator (the namespace's options and the command are no
longer in the same argument space), the command arriving as **JSON on stdin**
(it never reaches a command-line parser at all), and the refusal of a command
whose first element begins with `-`.

The same probe against the hardened script: **refused**, and nothing ran.

### 5.3 The probes that SUCCEEDED, deliberately

| probe | result | why it is not a finding |
|---|---|---|
| `cat /sandbox/repo/README.md` | **succeeds** | the reviewer's job is to read the code under review; the copy is disposable and the operator's checkout is not the writable tree |
| write into `/sandbox/findings` | **succeeds** | the findings have to go somewhere, and that somewhere is the reviewer's own tree |
| `go version`, `python3 -V`, `git --version`, `cc --version` | **succeed** | the toolchain is bound read-only so a review can still run the tests |
| the Go module cache | **absent** | a build needing an uncached dependency fails; that is the disclosed cost, not a hole |

---

## 6. The integration tests

Three suites, all in `agent_installation/tests/`, plus the TypeScript half.

| suite | what it pins | result |
|---|---|---|
| `test_reviewer_isolation.py` | the definition's default deny; **exactly one** allowed action; the host shell closed; the host-side file tools closed; Code Mode, the browser and the capability loader closed by name; every registry tool denied; no stale deny rules; the installer's fail-closed behaviour (a definition without the default deny, one that re-opens the shell, one that omits the sandboxed tool, and a symlinked target are all **refused**); the plugin registers the tool and has **no privileged mode**; the install flow treats a failed reviewer install as **fatal**; the sandbox script's hardening (the trust store is not bound, the writable tree is bound by descriptor, the `--` separator is present, a leading-`-` command is refused, the protection list comes from the passwd database, a forged `HOME` does not unprotect the home, a symlinked base is refused) | **38 tests, all pass** |
| `test_reviewer_sandbox_exec_path.py` | the reviewer's **actual execution path**, driven through the invocation the plugin makes, against disposable sentinels: the sentinel is unreachable, the operator's home is unreachable, the trust store is unreachable, there is no network, the option-injection vector is refused, the working directory cannot be chosen, the disposable copy is present, the tree is writable, the environment is the disposable one, the toolchain is present, **there is no unsandboxed fallback** (a missing bubblewrap and a refused command leave the marker file absent), and the only allowed action is the sandboxed tool | **22 tests, all pass** |
| `test_reviewer_bwrap_sandbox.py` | the namespace's **shape**: the runtime is bound read-only, only the reviewer's tree is writable, nothing is bound read-write by path, `--clearenv` comes **before** the `--setenv` flags, the network and privileges are dropped, a nested user namespace is refused, `/tmp` is private, `/etc` is synthetic, a live runtime root is refused — and, live, that the protected tree, the runtime root, the operator's home and the trust store are **invisible** | **17 tests, all pass** |
| `test_sandbox_exec.test.ts` | the TypeScript half: the tool's name is the one the definition opens, the default deny is present, a non-array command is refused, a working directory outside the sandbox is refused, a missing sandbox script **fails closed**, a command inside the sandbox cannot reach the sentinel, and the tool has **no privileged mode** | **6 tests, all pass** |

The whole `agent_installation` suite: **564 tests, all pass** (1 skipped — the
skipped case needs a condition this host does not provide).

---

## 7. The other gates

| gate | result |
|---|---|
| `go build -tags fts5 ./...` (root and `internal/core`) | clean |
| `go vet -tags fts5` | clean |
| `make docs-check` | OK — 13 checks, 0 errors |
| `mpm-lint --gate` | **PASS** (mutex, sql, fd, imports) |
| `npx tsc --noEmit` (strict) | clean |
| `npx tsc -p tsconfig.tests.json` + `node --test` | 6/6 pass |

---

## 8. CI on the exact head

The branch head is pushed to `github` and CI is run on it by the
`pull_request` trigger of `.github/workflows/build-test.yml` (which fires for
every pull request, whatever its base).

- **code head:** `2521dfb4` + `58cb88d7` (the browser and capability-loader
  denies) — see §9 for the run.
- The report commit itself is **documentation only**; its run is recorded here
  too.

> The CI result is filled in from the run itself — the run URL and its
> conclusion — in §9 below, and it is the run on the **exact** head that
> carries this report.

---

## 9. The independent external review

The A3 work was reviewed by **Charlie**, independently, on a **different
model** from the one that wrote the implementation. Its findings are recorded
in `charlie-external-review.md` (the review that produced the five corrections
and the architecture decision implemented here) and, for the implementation
itself, in the review commissioned with this report — see
`charlie-a3-implementation-review.md`.

---

## 10. Residual risks

Numbered, and each one is a real thing a reader could get wrong.

1. **The boundary covers one agent.** `explore` and `general` are **not**
   confined. A future decision may make a common sandbox policy; it is not
   made here, and the gap is deliberate and documented.
2. **Alpha is trusted.** Nothing here protects the operator from a compromised
   or malicious privileged session.
3. **The module cache is withheld.** A reviewer cannot build a dependency the
   sandbox has not already cached. Binding it is a capability grant, and the
   disclosure cost should be decided explicitly.
4. **The sandbox base is host-readable.** The operator's own account can read
   the disposable copy and the findings. The boundary is one-directional.
5. **The definition is only a boundary while it is installed.** The installer
   fails closed and `install.sh` treats a failed install as fatal, but a
   definition that is **deleted** after installation leaves the host's last
   loaded copy in force — and the host reloads agents from that file, so a
   deleted file restores the built-in agents, not the reviewer. The reviewer's
   absence is visible in the host's agent registry.
6. **Bubblewrap must exist and must be able to build a namespace.** Where it
   cannot, the command does not run — a **refusal**, not a degraded mode. The
   refusal is a functional failure for the reviewer, and it is the correct
   failure.
7. **The host's permission service is the enforcement point for the tool
   catalogue**, and it is the host's, not ours. A host that changes its rule
   semantics (the host's own implementation is `findLast` — the **last**
   matching rule wins) could change what the definition means. The definition
   therefore puts the default deny **first** and the single allow **after** it,
   which is the order the host evaluates.
8. **The Go module cache, the runtime, and the reviewer definition are all
   host-side artefacts**; a host-side compromise reaches them. The boundary is
   against a **less-trusted agent**, not against a less-trusted host.
9. **The `--` separator, the JSON payload and the leading-`-` refusal are three
   independent locks on the same door.** They are not independent *risks*: if
   one were removed, the other two remain. The risk is that a future change
   removes all three at once — which is why the contract test pins the
   separator's presence and the refusal's behaviour rather than the
   implementation.
10. **The namespace's `/usr` is read-only but present.** A binary in `/usr` is
    executable inside the sandbox. That is deliberate (the reviewer must run
    the tests) and it is a capability: the reviewer can execute the host's
    toolchain, but only over the sandbox's own tree, with no network.

---

## 11. What was NOT done, deliberately

- **No merge, no deployment, no install.** PR #247 stays **draft**. Nothing
  was installed into the operator's `~/.config/opencode` or `~/.mpm`.
- **The operator's live substrate was never opened.** Every probe used
  disposable sentinels; the MPM database, the scheduler and the operator's
  checkout are untouched.
- **No `mpm_*` substrate tool was called** by this work.
- **The stray memory `d0fece05fe6ed8d8`** remains outstanding for a separate,
  operator-controlled procedure.
- **The shell-matcher campaign was not resumed.** Its guard was deleted, and
  its record stays in this directory.
