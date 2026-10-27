# OpenCode-only adapter removal — implementation record

> **Author:** Session A — integration and release authority.
> **Date:** 2026-10-27. **Base:** `origin/main` = `4de89ea`.
> **Branch:** `feat/retire-legacy-adapters`.
> **Status:** implementation record for the removal PR. The **approved
> removal matrix** itself — what is retired, what is preserved, and why —
> lives in PR #126 (`docs/dev/opencode-only-legacy-adapter-removal.md`)
> and is **not** duplicated here. This document records what the
> implementation PR actually did, and the debt it deliberately leaves
> behind.

---

## 1. What this PR is, and what it is not

**It is** the implementation of the OpenCode-only release: the four
retired host adapters are removed from the tree, the registries that
name them are reduced in lockstep, the gates that measured them are
re-measured against what actually ships, and the documentation is
retargeted at the host that remains.

**It is not** a redesign. The substrate — the CLI, the MCP server, the
tool registry, the schema, the scheduler — is untouched. The removal is
surgical: directories, registries, gates and prose.

**It is not** the proposal. PR #126 remains the historical decision
record and is left exactly as it is. This PR is separate, and does not
supersede it.

**It is not** a deployment. Nothing here touches `~/.mpm`, the running
desktop OpenCode install, or any user service.

---

## 2. What was removed

| Path | Was |
|---|---|
| `agent_installation/mpm-claude-code/` | Claude Code MCP bundle + CLAUDE.md managed-block installer |
| `agent_installation/mpm-pi/` | Pi extension + AGENTS.md managed-block installer |
| `agent_installation/mpm-hermes/` | Hermes MCP client + `.hermes.md` managed-block installer |
| `agent_installation/mpm-memory-openclaw/` | OpenClaw memory plugin + SOUL.md managed-block installer |
| `agent_installation/mpm-auto-mode-persona-openclaw/` | OpenClaw auto-mode / persona plugin |
| `agent_installation/uninstall-openclaw.sh` | The OpenClaw adapter's uninstaller |

Together with the suites and fixtures that only existed to test them
(`test_uninstall_openclaw_failure_semantics.py`,
`test_uninstall_openclaw.mjs`, `fixtures/fake-openclaw.sh`,
`test_refresh_installed_openclaw_safety.py`).

## 3. What was preserved, and why

| Path | Why it stays |
|---|---|
| `agent_installation/mpm-opencode/` | The supported host. |
| `agent_installation/lib/telemetry_adapter.py` | **Host-independent.** It belonged to no single host, so retiring a host must not take it with it. Relocated (not copied) in the same commit as the directory removal, with its test at `agent_installation/tests/test_telemetry_adapter.py`. |
| `agent_installation/scripts/` | Host-independent tooling: renderer, reconciler, diagnostic, convergence engine, runtime-package staging. |
| `internal/scheduler/ingest.go` | The ingest path is substrate behaviour, not adapter behaviour — see §6. |
| `cmd/mpm/onboard_cmds.go`'s `displayHostName` | A display-name switch, not a registry. Left intact. |

The registries that named the retired hosts were reduced **in lockstep**
with the directory removal, so no name survives in one place and not the
other:

- `cmd/mpm/onboard_hosts.go` — `onboardHostAdapters` is now
  `{"opencode": "mpm-opencode"}`; `onboardHostLabels` is `["opencode"]`.
- `cmd/mpm/onboard_cmds.go` — `onboardHostLabels` follows.
- `agent_installation/scripts/render_managed_blocks.py` and
  `check_installed_managed_blocks.py` — the Python adapter lists.

`agent_installation/scripts/reconcile_managed_blocks.py` needed **no**
edit: `discover_adapters()` is scan-based, so the removal of the
directories removed the adapters from its discovery automatically. That
is the property that makes the reconciler safe to run against a tree
with an absent adapter — it prints "no adapters matched" and exits 0.

---

## 4. The gates, re-measured against what ships

Every floor below was **measured after the removal**, with the command
and its output recorded in the `Makefile` comment beside the floor, and
set a small margin below the observed population. No floor was lowered
to make a failure disappear.

| Gate | Measured | Floor |
|---|---|---|
| `make test-agent-installation` (umbrella) | 452 tests (1 skipped) — 443 from the removal + 9 from PR #133's merged carrier | `AGENT_INSTALL_TEST_MIN_TESTS := 300` |
| `make test-agent-adapters` — Python | 119 tests | `AGENT_ADAPTER_TEST_MIN_PY_TESTS := 100` |
| `make test-agent-adapters` — TypeScript | 89 passing | `AGENT_ADAPTER_TEST_MIN_TS_TESTS := 15` |
| `make test-agent-adapters` — JavaScript | **no JS family remains** | *(no floor — see below)* |

### 4.1 The empty JavaScript state is explicit, not silent

`AGENT_ADAPTERS_JS` is now empty, because the only JS adapter families
were the two OpenClaw plugins. `AGENT_ADAPTER_TEST_MIN_JS_TESTS` is
therefore also empty.

A `0` floor in either position would be wrong: `0` reads as "the floor
is zero, so anything passes", which is a silent pass wearing a number's
clothing. Instead:

- `AGENT_ADAPTERS_JS_EMPTY_REASON` carries the reason and the date.
- The recipe **prints** the fact rather than passing in silence.
- The recipe **FATALs** in three states: the JS list is non-empty while
  the floor is unset (a JS family appeared without anyone measuring
  it); the JS list is empty while the floor is set (a measurement was
  left behind for a family that no longer exists); and an unwired JS
  suite exists on disk (a JS family was removed from the list but its
  suite was left behind).

The measured counts live in the `Makefile` comments with the command
that produced them, so a future reader can reproduce the number rather
than trust it.

### 4.2 A measured false green, fixed

`scripts/tests/test_makefile_agent_adapter_wiring.py` asserted on a
regex that matched the **following comment line** rather than the
Makefile variable it meant to inspect. The gate was green while
inspecting the wrong text. It now anchors the pattern to its own
variable line, and derives the expected adapter sets from the
filesystem rather than from hard-coded counts.

---

## 5. The coverage that was retargeted rather than retired

Retiring a test suite is easy and lossy. The suites below were
**retargeted**, and the retargeting preserves their original
assertions:

| Suite | Before | After |
|---|---|---|
| Refresh-safety | `test_refresh_installed_openclaw_safety.py` (29 tests) | `test_refresh_installed_absent_adapter_safety.py` — retargeted to the **installer**, every seeded-file assertion kept, plus the additional property "adapter absent ⇒ clean no-op". Includes a non-vacuity control (`test_the_shipped_host_is_still_reconciled`). |
| Fresh-install | *(none)* | `test_fresh_install_configures_only_opencode.py` — a fresh install writes `~/.config/opencode/AGENTS.md` and **nothing else** under `$HOME`. |
| Cross-adapter parity | `test_cross_adapter_contract_parity.py` | Retargeted, not retired. |
| Managed-block convergence | `test_managed_block_convergence.py` | The retired hosts' marker forms are kept as **fixtures**; the subject is retargeted to OpenCode. |
| Adapter schema guard | `internal/core/tools/adapter_schema_guard_test.go` | **Extended, not retired**, so it stays load-bearing for the surviving adapter: it now reads the adapter's tool *declarations* (`name: "mpm_memory"`) as well as `callMpm(...)` call sites — 31 declarations across 17 distinct tools against 21 registry tools. Verified non-vacuous by renaming a declaration and confirming the guard fails. |
| Onboarding consent matrix | `cmd/mpm/onboard_hosts_test.go` | Rewritten for the OpenCode-only registry, using **neutral synthetic labels** (`host-current` / `host-absent` / `host-drifted`) where several hosts are needed to exercise a rule, because the matrix switches on the inspected verdict rather than on the host's name. |

---

## 6. Deferred debt — recorded, not silently carried

### 6.1 The `created_by` rename

`internal/scheduler/ingest.go:241` writes the literal
`"mpm-memory-openclaw-ingest"` as a wake's `created_by`. That literal
names a host MPM no longer supports.

**It is deliberately NOT renamed in this PR.** The value is
operator-visible data: it appears on rows in `scheduled_wakes`, and
renaming it would change what an operator sees for wakes the ingest path
has already created — and would leave the existing rows inconsistent
with the new code. The literal is also pinned by
`internal/scheduler/ingest_test.go`.

The rename is owed. When it is done, it must be done as a **data
migration with its own decision**, not as a drive-by rename: decide
whether old rows keep the historical value, whether the new value is
host-neutral (`mpm-ingest`) or host-specific, and whether the wake rows
are worth rewriting at all.

**The literal is deliberately unchanged in this PR.** A future agent who
finds `mpm-memory-openclaw-ingest` and wonders why it survived an
adapter removal is reading the right thing: it is the debt this section
records.

### 6.2 The ingest handler now has no producer

The same removal has a larger consequence than the literal: the
`openclaw_ingest` scheduler handler watched a file that **only** the
OpenClaw plugin wrote (`~/.mpm/run/ingest.md`, produced by the plugin's
`flushPlanResolver`). With the plugin retired, nothing writes that
file, so the handler now has no producer.

**That is not decided here.** The handler is substrate behaviour with
its own safety rails (path allowlist, symlink refusal, 64 KB cap,
quarantine-on-oversize, atomic rename, source attribution) and its own
operational surface (`cmd/mpm-scheduler/main.go` registers it; its
rows appear in `scheduled_wakes`). Whether to retire it, generalise it
to a host-neutral "external file → wake" bridge, or leave it dormant
is a **separate decision with its own evidence** — and the ingest
handler is explicitly out of this PR's scope.

What this PR does is make the state honest rather than silent: the
handler still exists, it still works, and the literal that names its
former producer is deliberately unchanged. Nothing in the removal
pretends the path is still fed.

### 6.3 The V1 hook name in the OpenCode managed block

The OpenCode managed block still names the V1 hook
`experimental.chat.system.transform`. The shipped adapter is the V2
plugin. Changing the installed block's content is an **operator-visible
contract change** — it rewrites a managed section in every existing
`~/.config/opencode/AGENTS.md` — and it is not part of a removal. It is
flagged here as a follow-up, to be decided with the same care as any
other change to the canonical managed block.

---

## 7. The safety property, and how it is tested

> **Retiring an adapter stops MPM from CONFIGURING that host. It never
> removes, rewrites or disables an integration an operator already has.**

That property is asserted, not asserted-about:

| Property | Where it is tested |
|---|---|
| A fresh install configures OpenCode and nothing else | `agent_installation/tests/test_fresh_install_configures_only_opencode.py` |
| An existing integration is left byte-identical by a refresh | `agent_installation/tests/test_refresh_installed_absent_adapter_safety.py` |
| An existing installation is preserved across the upgrade | PR #133 (`test/legacy-installation-preserved`) — the host-independent carrier, merged into `main` as `75d675c` and carried here verbatim |
| The diagnostic does not report a retired host | `agent_installation/tests/` umbrella |

Every installation and removal test runs against a **disposable
environment** — a temporary `HOME` and a temporary `MPM_WORKSPACE`. No
test reads the operator's real `~/.config`, `~/.claude`, `~/.pi`,
`~/.openclaw` or `~/.hermes.md` as an input, and none writes to them.

### 7.1 Verification against PR #133's carrier

PR #133 (`test/legacy-installation-preserved`) carries the
**host-independent** carrier: a suite that pins the safety property
against a synthetic host with no adapter, so the guarantee survives the
removal of the adapter directories that used to guard it.

It was merged into `main` as `75d675c` while this branch was in
progress, so this branch now carries it verbatim — the removal PR does
not fork it, weaken it, or re-author it.

Verified against the merged carrier:

| Check | Result |
|---|---|
| The umbrella suite (443 from this branch + 9 from PR #133) | **452 tests, OK (1 skipped)** |
| PR #133's carrier on its own | **9 tests, OK** |
| PR #133's diff applied on top of the removal (before the merge) | **clean** — `git apply --check` passed for both the test file and `docs/dev/coordination.md` |

The last row is how the two were verified together before the merge
landed: the removal's tree plus PR #133's diff, applied cleanly, with
the combined umbrella green.

### 7.2 Non-vacuity — demonstrated, not asserted

A gate that cannot fail is a comment. Each of the load-bearing
properties was made to fail on purpose, and each was observed to fail:

| Probe | Observed failure |
|---|---|
| Delete the surviving OpenCode adapter's test suite | `make test-agent-adapters` → **`FATAL: collected 1 Python tests, floor is 100.`** (and `FATAL: agent_installation/mpm-opencode/tests is missing` when the directory is removed outright) |
| Inject a "tidy up retired hosts' leftovers" cleanup step into the shared reconciler | PR #133's carrier **FAILS**, naming the files the cleanup destroyed (`.pi/agent/AGENTS.md`, `.openclaw/workspace/SOUL.md`, …) |
| Make the reconciler write nothing | `test_fresh_install_configures_only_opencode` **FAILS**: *"the fresh install did not write the OpenCode managed block … the 'nothing else was configured' assertions below would be vacuous"* |
| Rename a tool declaration in the surviving adapter (`mpm_memory` → `mpm_memory_typo`) | `TestAdapterCallsites_MatchGoSchema` **FAILS**: *"declared tool "mpm_memory_typo" — not in Registry"* |

The third row is the important one: it shows the fresh-install suite's
"nothing else was configured" assertions are **not** vacuously true.
A reconciler that did nothing at all would fail them.

### 7.3 Two regressions the release gate caught — and what they teach

The gate earned its place twice. Both regressions were invisible to the
umbrella suite, and both are worth recording because the *shape* of them
will recur.

**1. The umbrella suite and the per-adapter suite cover different
directories.** `agent_installation/tests/` and
`agent_installation/mpm-opencode/tests/` are separate trees, and neither
runner reaches the other. Rewriting `agent_installation/INSTALL.md` took a
"14 Domain Tools" statement with it — that statement had lived in the
**Pi** section, and the test that checks it reads the whole document. The
umbrella stayed green (452 tests); the per-adapter suite failed.

The rule that follows: after any change to `agent_installation/INSTALL.md`,
`agent_installation/README.md`,
`agent_installation/MPM_AGENT_INTEGRATION_SNIPPETS.md` or
`mpm_integration_check.md`, run **both** suites. And when a per-host
document section is removed, the facts it carried must be **re-homed to
the host that remains**, not deleted with the section.

**2. A statement can live in an unrelated section and satisfy a test that
appears to be about something else.** That is the same defect seen from
the other side: the test was right, the document was not. A removal that
deletes a section can therefore break a test that reads the whole file
and has nothing to do with the deleted host.

Both regressions were fixed by re-homing the fact to the OpenCode
section, which is where it belongs now that OpenCode is the only host
that exposes those tools.

---

## 8. What was deliberately NOT done

The removal PR does not carry:

- changes to the lifecycle-event transport, the P4/P5 parity guard, or
  the ingest path (see §6.1);
- core CLI or MCP capability changes;
- a redesign of onboarding, packaging or discovery;
- unrelated lifecycle diagnostic fixes;
- new adapter functionality.

Each of those is a separate piece of work with its own decision. Folding
them into a removal would make the removal's diff unreviewable — which
is the whole reason this PR is scoped to the removal and nothing else.
