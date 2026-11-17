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
| the host's **toolchain**, wherever the host keeps it | bound **read-only**, and the sandbox's `PATH` points at it | the static list is the *system's* toolchain; a host that keeps its toolchain elsewhere — a CI runner's hosted tool cache — would otherwise give the reviewer no compiler, and the review would quietly degrade to reading code. The discovery is **host-side** (the script reads the host's own `PATH`, before the namespace exists), and a root that resolves inside a protected root, or under a path the namespace mounts fresh, is **not** bound |
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

**What is protected inside the home**, and what is not, is a deliberate
split. The **sensitive** part of the operator's home is protected: `.mpm`,
`.config`, `.local`, `.ssh`, `.aws`, `.gnupg`, the credential files, and the
conventional source roots (`~/src`, `~/projects`, …). The home itself is
refused *as* a base — it holds everything — but a base inside a
**non-sensitive** part of it is accepted, because the base is bound as a
directory: it exposes its own contents, not its parent's. That distinction
is load-bearing rather than cosmetic: the plugin's own default base lives
under `~/.cache`, and a guard that refused every base inside the home
would refuse **the one the tool uses** — `sandbox_exec` would then refuse
every command, forever, with a message that reads like a safeguard.

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
| read the operator's checkout path from the copy's git metadata (`.git/config`, the reflog, `FETCH_HEAD`) | **refused** — the clone's records of its source are removed, so the copy names no host location |
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
| `go version`, `python3 -V`, `git --version`, `cc --version` | **succeed** | the toolchain is bound read-only so a review can still run the tests — from wherever the **host** keeps it, not only from `/usr` |
| the Go module cache | **absent** | a build needing an uncached dependency fails; that is the disclosed cost, not a hole |
| read `/proc/self/mountinfo` | **succeeds** | it discloses the **host path of every mount the namespace was given**, and therefore the operator's username and home path. Those paths are **not bound**, so nothing there can be read through them: it is metadata about the host, not access to it. Residual risk 11 states it; the alternative — giving up the descriptor binding, or the base's location — would cost more than the disclosure is worth |

### 5.4 What the independent reviewer attacked, and what closed it

The reviewer's own conclusion, from the implementation review: *"the
namespace is genuinely confined (zero capabilities, `no_new_privs` set,
nested user namespaces blocked, host paths absent, no leaked file
descriptors)"*. Its probes covered, among others:

| route | what closed it |
|---|---|
| `/home`, `/root`, `/sys`, `/etc/shadow`, `/etc/sudoers` | they are **not bound**, so inside the namespace they do not exist — the absence was observed, not inferred |
| a nested user namespace (`unshare(CLONE_NEWUSER)`) | `--disable-userns` blocks it — observed as `unshare: … No space left on device`. The **fail-closed** branch (`--assert-userns-disabled`) could not be exercised by the reviewer, because it would have needed seccomp support removed; it is verified by reading the code, not by running it |
| setuid binaries and file capabilities | the namespace carries **zero capabilities** and `no_new_privs` |
| a leaked file descriptor | there is none: the namespace is given no descriptor to a host mount it was not given, and the reviewer checked |
| a symlink inside the reviewer's own tree pointing outside it | the tree is bound by **descriptor**, so the symlink resolves inside the namespace, where the target does not exist |
| a bind of the host root through an argument the model controls | the `--` separator, the JSON-on-stdin payload, and the leading-`-` refusal (§5.2) |
| the `/etc/alternatives` copy | it is a copy of the host's symlinks, and the host's targets are not bound, so it names nothing reachable |

One route the reviewer named but did **not** directly test is
`open_by_handle_at`: it needs a file descriptor to a host mount, and the
namespace has none for the mounts it was not given — that is reasoning,
not a measurement, and it is recorded as reasoning.

---

## 6. The integration tests

Three suites, all in `agent_installation/tests/`, plus the TypeScript half.

| suite | what it pins | result |
|---|---|---|
| `test_reviewer_isolation.py` | the definition's default deny; **exactly one** allowed action; the host shell closed; the host-side file tools closed; Code Mode, the browser and the capability loader closed by name; every registry tool denied; no stale deny rules; the installer's fail-closed behaviour (a definition without the default deny, one that re-opens the shell, one that omits the sandboxed tool, and a symlinked target are all **refused**); the plugin registers the tool and has **no privileged mode**; the install flow treats a failed reviewer install as **fatal**; the sandbox script's hardening (the trust store is not bound, the writable tree is bound by descriptor, the `--` separator is present, a leading-`-` command is refused, the protection list comes from the passwd database, a forged `HOME` does not unprotect the home, a symlinked base is refused); and that the **plugin's own default base is one the sandbox accepts** — the defect that would have made the tool refuse every command forever, with a message that reads like a safeguard | **42 tests, all pass** |
| `test_reviewer_sandbox_exec_path.py` | the reviewer's **actual execution path**, driven through the invocation the plugin makes, against disposable sentinels: the sentinel is unreachable, the operator's home is unreachable, the trust store is unreachable, there is no network, the option-injection vector is refused, the working directory cannot be chosen, the disposable copy is present, the tree is writable, the environment is the disposable one, the toolchain is present, **there is no unsandboxed fallback** (a missing bubblewrap and a refused command leave the marker file absent), and the only allowed action is the sandboxed tool; and that the **disposable copy carries no path to the operator** — the clone's origin remote, reflog and `FETCH_HEAD` are removed, the history survives the scrub, and the config names no host location from inside the namespace | **27 tests, all pass** |
| `test_reviewer_bwrap_sandbox.py` | the namespace's **shape**: the runtime is bound read-only, only the reviewer's tree is writable, nothing is bound read-write by path, `--clearenv` comes **before** the `--setenv` flags, the network and privileges are dropped, a nested user namespace is refused, `/tmp` is private, `/etc` is synthetic, a live runtime root is refused — and, live, that the protected tree, the runtime root, the operator's home and the trust store are **invisible** | **17 tests, all pass** |
| `test_sandbox_exec.test.ts` | the TypeScript half: the tool's name is the one the definition opens, the default deny is present, a non-array command is refused, a working directory outside the sandbox is refused, a missing sandbox script **fails closed**, a command inside the sandbox cannot reach the sentinel, and the tool has **no privileged mode** | **6 tests, all pass** |
| `internal/update/payload_test.go` (Go) | the canonical payload manifest, the adapter's own `OPENCODE_RUNTIME_MANIFEST` and the staging fixture all agree — and the fixture **emits and imports** `dist/sandbox-exec.js`, so the shape that broke CI is the shape the fixture reproduces | **the `internal/update` package passes** |

The whole `agent_installation` suite: **574 tests, all pass** (2 skipped — the
skipped cases need a condition this host does not provide).

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

**What CI caught, and what it was.** The runs of the implementation head
**failed**, and every failure was real:

1. **The staged package carried an unresolved import.** The canonical
   runtime payload manifest (`scripts/runtime_payloads.json`) — the
   declaration the updater and the shell staging both read — did not list
   the two new files, `dist/sandbox-exec.js` and
   `scripts/reviewer_bwrap.py`. The staged package therefore carried an
   entrypoint with an **unresolved import** (`dist/index.v2.js ->
   ./sandbox-exec.js`): the plugin the installer would have shipped
   **loads nothing**, silently. The manifest, the adapter's own
   `OPENCODE_RUNTIME_MANIFEST`, the shell-side manifest constant and the
   Go fixture that stages a package were all brought into agreement, and
   the fixture now **emits** `dist/sandbox-exec.js` and **imports** it
   from the entrypoint, so the shape that failed in CI is the shape the
   fixture reproduces.
2. **The sandbox's runtime binding was too narrow.** The execution-path
   test asserted that the Go toolchain is runnable inside the namespace,
   and it hard-coded `/usr/local/go/bin/go`. On a CI runner the toolchain
   lives in the runner's hosted tool cache, which the sandbox did not
   bind — so the assertion failed with `execvp /usr/local/go/bin/go: No
   such file or directory`. The finding was not the test: it was the
   **sandbox**, which gave the reviewer no compiler on any host whose
   toolchain is not under `/usr`. The toolchain is now **discovered on
   the host** and bound read-only, and the test asserts the property that
   is actually guaranteed — *the toolchain the host has is runnable
   inside* — skipping only on a host that has none.
3. **A rebuilt payload was missing a file the manifest names.** The
   adapter's install tests use a **fake `tsc`**, so the build is
   hermetic; the fake emitted a **hand-written list** of `dist/` files
   and the list had not learned about `sandbox-exec.ts`. The rebuilt
   payload was therefore incomplete, the staging **refused** it, and the
   install failed — fifteen tests, with an error message that pointed at
   the payload and not at the stub. The fake now emits **one `.js` per
   `.ts` under `src/`**, which is what the real compiler does, so a
   module added to `src/` is carried into the payload **by
   construction**.

> A green local suite did not catch any of the three. The local tree had
> a built `dist/`, a toolchain under `/usr/local`, and a fake `tsc`
> whose list happened to be complete. **All three were hand-written
> mirrors of a real shape** — a manifest, a path, a file list — and a
> mirror maintained by hand is a mirror that goes stale. Each of them
> now derives from the thing it mirrors, so the shape that broke is the
> shape the gate exercises.

| head | what it carries | the run |
|---|---|---|
| the implementation head | the A3 implementation, the manifest fix, the review's findings and this report | recorded in §8.1 below |
| the report's own head | **documentation only** | the same pipeline; its conclusion is recorded in the PR body, which is where the final state of a run is published without making the report chase its own commit |

### 8.1 The run

- **workflow:** `build-and-test` (`.github/workflows/build-test.yml`), the
  `pull_request` trigger
- **head:** `a810a2ce` — the implementation head: the A3 implementation, the
  manifest fix, the review's findings, and the three CI fixes
- **run:** [44081964509](https://github.com/flowbyte-com/mpm/actions/runs/44081964509)
- **conclusion: SUCCESS** — all three jobs green:

| job | result |
|---|---|
| `Build + core tests` | **success** — the build, `go vet`, the `mpm-lint` gate, the core tests, the renderer-parity suite, the documentation-freshness gate, the installer-contract suite (`scripts/tests`) and the benchmark harness |
| `Full test suite` | **success** — the full Go sweep with the coverage profile |
| `Release gate (cross-agent continuity + public-CLI subprocess suite)` | **success** — `make release-gate`, which is the job that runs the agent_installation suite **with the plugin built** — the job that caught the two sandbox defects |

The head that carries **this report** is **documentation only**, and it runs the
same pipeline; its conclusion is published in the PR body, which is where a
run's final state belongs without the report chasing its own commit.

> Three earlier runs failed, and the record of them is in §8: they are the
> reason the run above is green. A gate that has never failed is a gate that
> has never been tested.

---

## 9. The independent external review

The A3 work was reviewed by **Charlie**, independently, on a **different
model** from the one that wrote the implementation.

Two reviews exist, and they are not the same review:

| record | what it reviewed | verdict |
|---|---|---|
| `charlie-external-review.md` | the **architecture** — it produced the five corrections and the capability-removal decision that this implementation follows | P7 APPROVED, A1/A2 APPROVED, **A3 HOLD** (the implementation was not yet written) |
| `charlie-a3-implementation-review.md` | the **implementation** — it was given the files to read, the claims to attack, and the questions to try to break | **APPROVE** |

The implementation review's verdict was **APPROVE**: the reviewer could
not construct an escape, and confirmed each of the numbered claims above
against the **executing** code rather than against its text — the
namespace's shape (`/home`, `/root`, `/sys`, `/etc/shadow`,
`/etc/sudoers` all absent), the rule order for the host's `findLast`
evaluation, the absence of a privileged mode, the fail-closed installer,
and the closure of the option-injection escape.

It raised **three** findings, and this report does not hide them:

| # | severity | finding | disposition |
|---|---|---|---|
| 1 | **MINOR** | the reviewer can learn the operator's **username and home path** — from the namespace's mount table, which records the host path of a descriptor-bound mount, and (then) from the clone's `remote.origin.url` | **the clone's channel is closed** (the remote, the reflog and `FETCH_HEAD` are removed, and a test pins it); **the mount-table channel remains** and is disclosed as residual risk 11 below. The test docstring that over-claimed is corrected |
| 2 | **NIT** | the disposable copy is the **full git history**, so a secret that was committed and later rotated is still readable in the copy | accepted as scope and disclosed as residual risk 12 below |
| 3 | **NIT** | the `--clearenv` comment's rationale was wrong (bwrap always clears and then sets, whatever the order) | **fixed**: the comment now says what bwrap does, and the behaviour is unchanged |

The review also stated what it **could not** test: the host's `findLast`
semantics (measured elsewhere — `host-verdicts-a3.json`), the
`--assert-userns-disabled` fail-closed branch, and `install.sh` end to
end (because running it would write into the operator's home).

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
10. **The namespace's runtime is read-only but present.** A binary in `/usr` — or in
    the host's toolchain, wherever the host keeps it — is executable inside the
    sandbox. That is deliberate (the reviewer must run the tests) and it is a
    capability: the reviewer can execute the host's toolchain, but only over the
    sandbox's own tree, with no network. The toolchain's discovery is **host-side**
    and the model contributes nothing to it, but it is a grant of "the operator's
    own compiler", not a narrowing of the sandbox.
11. **The operator's username and home path are disclosed as METADATA.** The
    namespace's own mount table records the host path of every mount it was
    given, so a reviewer that reads `/proc/self/mountinfo` learns the base's
    path under the operator's home — and therefore the operator's account
    name. The paths it names are **not bound**, so nothing there can be read
    through them; this is a disclosure about the host, not access to it.
    It is disclosed rather than engineered away because closing it means
    giving up either the descriptor binding (the property that stops a
    symlink swap from changing what is mounted) or the base's location on
    the operator's filesystem (a weaker base). The **clone's** disclosure of
    the same fact — the origin remote — **is** closed.
12. **The disposable copy carries the full history.** A secret that was
    committed and later deleted or rotated is still readable in the copy
    through `git log -p`. The history is the review material, so it is kept;
    a repository whose history contains secrets must be treated as a
    repository whose history contains secrets.
13. **The mount table is the reviewer's own.** It is the namespace's, not the
    host's: it describes what the namespace was given, and it cannot be
    turned into a handle on the host's filesystem — an `open_by_handle_at`
    route needs a file descriptor the reviewer does not have, and the
    namespace has no reference to a host mount it was not given.

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
