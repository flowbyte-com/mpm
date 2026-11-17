# The narrow delta review — and its fix

> **What this file is.** The independent security review of the A3
> implementation reached **VERDICT: APPROVE** on the implementation head.
> Between that review and the merge gate, one commit changed the
> NAMESPACE — the code that decides what the reviewer can see. The
> operator's rule for that case is exact: commission **one narrow
> independent delta review of that change only**, and do not repeat the
> complete review.
>
> This file records that delta review, the blocking finding it produced,
> the fix, and the reviewer's verification of the fix.
>
> **This is a record, not an implementation.** Nothing here changes the
> substrate's behaviour.

---

## The delta under review

Commit `a19b1a78` — *"fix(reviewer-isolation): bind the toolchain from
wherever the HOST keeps it"*.

It touched three code files:

| file | what changed |
|---|---|
| `agent_installation/mpm-opencode/scripts/reviewer_bwrap.py` | the executable change: the sandbox now **discovers** the host's Go toolchain on the host's own `PATH`/`GOROOT` and binds it READ-ONLY, so a reviewer on a CI runner (whose toolchain lives in a hosted cache, not under `/usr`) still has a compiler |
| `agent_installation/tests/test_reviewer_bwrap_sandbox.py` | the shape tests |
| `agent_installation/tests/test_reviewer_sandbox_exec_path.py` | the execution-path tests |

plus `docs/SPEC.md` and the completion report (documentation).

**It changed no permission rule, no tool registration and no
installation logic.** That was confirmed by the reviewer by reading
`agent_installation/mpm-opencode/templates/agents/mpm-reviewer.md` and
`agent_installation/mpm-opencode/src/sandbox-exec.ts`, and by the diff
stat itself.

---

## Round 1 — the delta review

**VERDICT: REQUEST CHANGES**

### CRITICAL — the discovered-root protection check can bind the operator's ENTIRE home read-only

Two facts combine into a full home exposure.

1. **`_is_protected_path` did not protect the home.** It iterated
   `protected_roots()`, which enumerates `home/{.mpm,.config,.local,.ssh,.aws,…,src,projects,code,work,dev}`
   plus the system directories — but **never the home itself**, and never
   a home subdirectory outside that list (`~/go`, `~/sdk`, `~/bin`,
   `~/.opencode`, `~/.nvm`). Measured on the real host:
   `_is_protected_path('<operator-home>')` → `False`; `<operator-home>/go`, `<operator-home>/sdk`,
   `<operator-home>/bin`, `<operator-home>/.opencode` → all `False`; `.ssh`, `.config`,
   `.local`, `.mpm` → `True`.

2. **The `<root>/bin/<program>` derivation was wrong for a
   home-relative PATH entry.** The code computed `resolved.parent.parent`,
   i.e. it assumed the tool lives at `<root>/bin/go`. For a PATH entry
   that is a direct child of the home — `~/bin` — `parent.parent` is
   **the home itself**. A host whose `PATH` contains `~/bin` with a real
   `go` therefore yields root `<operator-home>`, which then passes every guard:
   `<operator-home>/bin` exists; it is not under `/usr`; it is not under a
   fresh-mount path; and `_is_protected_path('<operator-home>')` was `False`.

   The namespace then carried `--ro-bind <operator-home> <operator-home>`.

**The concrete attack.** On any host whose Go toolchain lives under the
home — exactly the scenario the delta was written for — the reviewer
agent reads the operator's `.ssh/id_rsa`, `.git-credentials`, `.gnupg`,
`.config`, `.local` and the live runtime root `.mpm` with a plain
`cat /home/<op>/.ssh/id_rsa` from inside the namespace. A read-only
bind prevents *tampering*, not *disclosure*, and disclosure of the
credentials is the thing the boundary exists to prevent.

A milder form of the same defect fires with the default `GOPATH`:
`~/go/bin` on `PATH` yields root `~/go`, binding the operator's entire
GOPATH (private source) read-only. The operator's own `PATH` on the
measured host carried `/home/<op>/.opencode/bin`,
`/home/<op>/.nvm/versions/…/bin` and another account's
`/home/<other>/.linuxbrew/.linuxbrew/bin` — none of which the old check
would have refused.

### MAJOR — the toolchain check was weaker than the base check it claimed to mirror

The docstring claimed a discovered root is refused *"exactly like the
base"*. It was not:

- `resolve_base` refuses a base that **is** the operator's home (it
  consults `home_roots()`); `_is_protected_path` did not.
- `resolve_base` refuses **any symlinked component** of the base
  (`_assert_no_symlink`); `_is_protected_path` *resolved* symlinks with
  `realpath` and then bound the resolved target **by path**, leaving a
  host-side check-to-mount window that the descriptor-bound reviewer's
  tree does not have.

The reviewer rated this MAJOR *"as a model-asymmetry defect that is the
root cause of the CRITICAL finding rather than an independently
exploitable hole"*: the swap needs a second actor on the host, which is
outside the reviewer's reach.

### MINOR — a discovered `bin` directory is front-loaded ahead of `/usr/bin`

`sandbox_path` put every discovered root's `bin` **before** `/usr/bin`,
and the bind is a *live* read-only mount. A discovered root writable by
another local user would let that user shadow the commands the reviewer
runs by name. Not reachable by the reviewer itself (it cannot write
host-side, has no setuid, no network, no capabilities) — latent, not
exploitable within the stated threat model.

### Confirmations

The reviewer explicitly **confirmed** the properties the brief listed as
already known, and did not re-report them: the discovery is host-side
and model-independent; a root inside a protected *subdirectory* is
refused; the namespace `PATH` does not include the reviewer's own tree;
the mount-table metadata disclosure is unchanged; and the clone
sanitisation controls (the origin remote, the reflog and `FETCH_HEAD`
removal) are undisturbed by the delta.

---

## The reproduction

The finding was reproduced **live**, in a private mount namespace that
carries its own tmpfs `/home`, so nothing outside it was touched and the
operator's own home was never read, written or bound.

Before the fix:

```
discovered roots:       [PosixPath('<fake-home>')]
is the home protected?  False
binds the namespace would carry: ['/usr', '/usr/local', '<fake-home>']
RESULT: VULNERABLE — the shipped discovery would bind the home into the namespace.
DISPOSABLE FAKE SECRET
```

After the fix:

```
discovered roots:       []
is the home protected?  True
binds the namespace would carry: ['/usr', '/usr/local']
RESULT: CLOSED — the shipped discovery refuses the home, so the
         namespace never carries it and the home's credentials
         (secret.txt, .ssh/id_rsa) are not reachable from inside.
```

---

## The fix

Commit `fc1af83f` — *"fix(reviewer-isolation): a PATH entry must never
make the HOME a bind root"* — implements the reviewer's minimum change
list:

1. **`_is_protected_path` refuses the home ITSELF, everything inside it,
   every ancestor of it, the filesystem root, and every other account's
   home under `/home`.** The rule is deliberately **stricter** than the
   base's, and the difference is the point: the base may live inside a
   harmless part of the home (the plugin puts it at
   `~/.cache/mpm-opencode/review-sandbox`, and a guard that refused
   every base inside the home would refuse the one the plugin uses),
   while a **discovered root is a directory the reviewer can READ**, so
   no root under a home may be bound at all.
2. **`_toolchain_root_for` strips only a directory literally named
   `bin`**, so a PATH entry can never collapse onto something larger
   than the directory that actually holds the tool.
3. **A discovered root is bound BY DESCRIPTOR** (`--ro-bind-fd`, from an
   `O_RDONLY|O_DIRECTORY|O_NOFOLLOW` descriptor), exactly like the
   reviewer's own tree — no path-based bind, no check-to-mount window.
4. **The discovered roots are computed ONCE** and read from one place by
   the bind, the descriptor table and the `PATH`, so the three can never
   disagree.
5. **A `roots` subcommand** answers the discovery without touching
   anything, so a host can be asked about without being changed.

The reviewer's three non-blocking observations were closed in the same
commit: the dead `--probe-roots` flag was removed, the symlink check's
comment now says what actually closes the window (the descriptor bind),
and a new test pins the **positive** descriptor-bind path — which is
inert on a host whose Go lives under `/usr`, as here, and would
otherwise never be exercised.

---

## Round 2 — the verification of the fix

**VERDICT: APPROVE**

> *"The fix closes the CRITICAL and opens nothing new. I verified each of
> the minimum-change items against the working tree, ran both test files
> (23 + 27 pass) and the live reproduction (CLOSED), and independently
> exercised the one path the test suite cannot reach on this host."*

The reviewer answered each question of the verification brief:

| question | the reviewer's answer |
|---|---|
| (a) is the CRITICAL closed? | **Yes.** `_is_protected_path` refuses `/`, every protected root, the home itself, everything inside it and every ancestor of it — confirmed against the real host. *"No PATH entry on any host can now produce a discovered root that IS the home, inside the home, an ancestor of the home, or `/`."* |
| (b) can the derivation still collapse? | **No.** Only the intended `<root>/bin` case strips a level, and it is fenced by the whole-home + ancestor + `/` protection. `/x/bin`→`/x`, `/x/y/bin`→`/x/y`, `/x`→`/x`, and `GOROOT` all land in the now-sound `_is_protected_path`. |
| (c) is the descriptor bind sound? | **Yes.** `build_tree` computes the roots once and stores them; `open_tree`, `bwrap_args` and `sandbox_path` read *that same list in the same order*, so index ↔ descriptor always agree. A root disappearing between discovery and open makes `open_tree` close every descriptor it opened and **refuse** — fail-closed, no descriptor leak. The reviewer verified `--ro-bind-fd` with a **deep** destination against bubblewrap 0.11.1 directly (the case the test suite cannot reach on this host). |
| (d) does the stricter check break the base or a legitimate case? | **No.** `resolve_base` is unchanged and does not call `_is_protected_path`; the plugin's own default base still resolves; the CI hosted cache under `/opt` is unaffected. |
| (e) did the fix open anything new? | **No.** No new path to the host, no fail-open, no route to bind the reviewer's own writable tree (the base is under the home → refused; under `/tmp` → fresh-mount-skipped). The `roots` subcommand returns before the base is resolved, touches nothing, and is not reachable from the reviewer's `sandbox_exec` path. |
| (f) are the new tests meaningful pins? | **Yes.** *"They assert properties, not implementation text."* |

The reviewer also confirmed that the two other files in the fix commit
(`install_reviewer_agent.py` and `mpm-reviewer.md`) are **documentation
clarifications only** — *"no permission rule, registration, or
installation logic changed"* — and that the wording correction states
the measured fact (the host refuses every call) rather than the
previously unverified visibility claim.

### What the reviewer could not test

- A positive end-to-end bind of a real non-`/usr` toolchain through the
  full `build_tree → open_tree → bwrap_args → bwrap` pipeline, because
  this host's `go` is at `/usr/local/go` and the discovery returns
  nothing. The reviewer verified the `--ro-bind-fd` deep-destination
  mechanics directly with a hand-built bubblewrap invocation instead.
  The merge gate closes the remaining gap with a synthetic test that
  points the discovery at a disposable root and checks the descriptor
  against the inode it names.
- A concurrent **host-side** symlink swap between `realpath` and
  `os.open` — the residual TOCTOU. It needs a second actor on the host,
  which is outside the reviewer's threat model.

### One scope note the reviewer raised, and the fix closed

A discovered root that is **another account's** home — for example
`/home/<other>/.linuxbrew/.linuxbrew`, which is on this operator's
`PATH` — was still bindable, because the first version of the fix
protected the operator's own home and its ancestors but not sibling
accounts. The reviewer rated it *"the same class as the accepted `/opt`
shared-toolchain grant and was not part of my CRITICAL"*; the fix
closed it anyway, by refusing every path under `/home`. The cost is a
functional limitation — a host that keeps its toolchain inside a home
directory gives the reviewer no compiler there — which is a limitation
rather than a hole.
