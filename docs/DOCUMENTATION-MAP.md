# Documentation map

Which document owns which fact, what the freshness gate checks, and what to do
when it asks you a question.

**A stale document is a defect, not a cosmetic issue.** An agent that follows an
obsolete instruction calls a tool that does not exist, configures a host that
was retired, or believes a planned feature is deployed. Documentation drift
causes the same class of failure as a broken test, but silently: the reader
blames themselves and guesses.

---

## 1. Who owns what

Each fact has exactly one home. When a fact changes, update the owner and
delete any duplicate — two descriptions of one contract is how drift begins.

| Fact | Owner | Notes |
|---|---|---|
| What MPM is, who it is for, how to start | [`README.md`](../README.md) | The managed block inside it is generated; never hand-edit |
| Product behaviour, semantics, commands, architecture | [`docs/SPEC.md`](./SPEC.md) | The canonical contract. If a library entry disagrees about *current* behaviour, the spec wins |
| Repository-working rules, invariants, build flags | [`AGENTS.md`](../AGENTS.md) | Loaded into every coding session |
| Contributor safety boundaries | [`CLAUDE.md`](../CLAUDE.md) | Loaded by Claude Code. Distinct from the retired MPM Claude Code adapter |
| CLI conventions (output, help, exit codes, caps) | [`CLI_AGENT_RULES.md`](../CLI_AGENT_RULES.md) | Normative for any CLI change |
| Tool semantics that are project-wide rather than per-tool | [`docs/library/tool-behavioral-contract.md`](library/tool-behavioral-contract.md) | Rules current; per-action tables historical |
| How to install, verify, upgrade, uninstall | [`agent_installation/INSTALL.md`](../agent_installation/INSTALL.md) | Operator procedure |
| What an agent must do to use MPM correctly | [`agent_installation/mpm-agent-protocol.md`](../agent_installation/mpm-agent-protocol.md) | Principles; the exact wording lives in the snippets file |
| The exact wording delivered into a host's instructions | [`agent_installation/MPM_AGENT_INTEGRATION_SNIPPETS.md`](../agent_installation/MPM_AGENT_INTEGRATION_SNIPPETS.md) | Canonical source; rendered by `agent_installation/scripts/render_managed_blocks.py` |
| Configuration file shape and precedence | [`docs/CONFIGURATION.md`](./CONFIGURATION.md) | |
| Security scope, reporting, credential rules | [`docs/SECURITY.md`](./SECURITY.md) | |
| Where work is tracked and how findings are classified | [`docs/ISSUES.md`](./ISSUES.md) | GitHub Issues is the backlog; this file is the policy |
| Direction the project intends to take | [`docs/ROADMAP.md`](./ROADMAP.md) | Future tense by design. **Not** a status report — parts of it have shipped |
| Why the system is shaped this way | [`docs/library/`](library/README.md) | Reference only. Never a description of current behaviour |
| Live session and coordination records | `docs/dev/` | Owned by their authors; append your own, do not edit theirs |

### Why `docs/library/` and `docs/dev/` are separate

They answer different questions and rot at different rates.

`docs/library/` holds **reference material with lasting value**: design
rationale, implementation research, compatibility findings, incident
post-mortems. It carries a status label on every entry, and those labels are
what make it safe to keep: a document labelled `Historical` cannot mislead a
reader about today, because it says so at the top.

`docs/dev/` holds **work in progress**. It describes what someone was doing on
a particular day. It is expected to be inaccurate about the future and is
deleted or superseded when the work lands.

Neither belongs in `docs/SPEC.md`, and neither is allowed to override it.

---

## 2. What the gate checks

`make docs-check` runs `scripts/docs_check`. Run it before proposing a
documentation change; CI runs it on every pull request.

### Objective defects — these fail the build

| Check | What it proves |
|---|---|
| `links` | every relative Markdown link resolves to a file in the tree |
| `anchors` | every `#fragment` resolves to a heading or an explicit `<a name>` |
| `missing-paths` | a backticked repo path names something that exists |
| `tool-names` | `mpm call <tool>` names a tool in `internal/core/tools/registry_list.go` |
| `tool-actions` | `<tool> action=<verb>` is inside that tool's registered enum |
| `make-targets` | `make <target>` names a target the Makefile declares |
| `build-paths` | `bin/mpm` is written as `.build/bin/mpm` or `~/.mpm/bin/mpm` |
| `managed-block` | the rendered managed block matches its canonical source |
| `library-status` | every `docs/library/` entry has a status label **and** a row in the index |
| `retired-adapter-paths` | no current document points into a retired adapter directory |

Each of these is demonstrable without judgement. The failure message names the
file, the line, and the correct value, so the fix does not require knowing how
the check works.

### Review prompts — these never fail the build

| Check | What it asks |
|---|---|
| `retired-host-mentions` | a current document mentions a retired host. Still presented as supported? |
| `stale-status` | a current document says "awaiting approval" / "pending implementation". Is that still true? |
| `docs-impact` | a contract-bearing code path changed and no mapped document changed |

These require reading the sentence and knowing the intent, which a script
cannot do reliably. A gate that fails on them would be wrong often enough to be
switched off, and a switched-off gate protects nothing. They are printed, and
in CI they appear as warnings, so they are visible without being blocking.

The gate also refuses to pass vacuously: if it cannot parse the registry, or
cannot parse a single Makefile target, or cannot diff against the requested
base, it says so instead of reporting success.

### What it does not do

It never opens a database, reads MPM state, contacts a service, or uses
credentials. It reads files in the working tree and, for `docs-impact`,
`git diff` and `git log`. It is safe on a clean checkout, in a container, and
in CI.

---

## 3. The dependency map

Some code changes make a document wrong by construction. This table says which.

| When you change… | Review these documents | Because |
|---|---|---|
| `internal/core/tools/registry_list.go`, `cmd/mpm-mcp/tools.go`, `cmd/mpm-mcp/main.go`, the compact-surface test | `docs/SPEC.md`, `AGENTS.md`, `agent_installation/mpm-agent-protocol.md`, `docs/library/CONTEXT_EXPOSURE.md` | the tool registry and MCP surface define what an agent is told it can call |
| `agent_installation/MPM_AGENT_INTEGRATION_SNIPPETS.md`, `render_managed_blocks.py`, the rendered snippet, `README.md` | `agent_installation/README.md`, `agent_installation/mpm-agent-protocol.md`, `AGENTS.md` | the managed block is what every agent reads at session start |
| `install.sh`, `uninstall.sh`, `scripts/deploy.sh`, `Makefile` | `docs/INSTALL.md`, `agent_installation/INSTALL.md`, `README.md`, `AGENTS.md` | install paths, prefixes, commands |
| `internal/core/schema.go`, `db.go`, `evidence.go`, `confidence/` | `docs/SPEC.md`, `docs/library/2026-06-16-confidence-evidence-foundation-design.md` | the storage contract and the confidence model |
| `internal/telemetry/`, `cmd/mpm-telemetry/`, the telemetry adapter | `docs/SPEC.md`, `docs/SECURITY.md`, `docs/library/2026-08-21-telemetry-binary-design.md` | wire format and the trust boundary |
| `internal/scheduler/`, `internal/critic/` | `docs/SPEC.md`, `CLAUDE.md` | safety gates and cadence |
| `cmd/mpm/` | `CLI_AGENT_RULES.md`, `docs/SPEC.md` | the normative CLI contract and the command reference |

`make docs-check` compares the branch against `DOCS_CHECK_BASE` (default
`origin/main`) and emits a review finding for each row whose code changed
without a matching document.

### Discharging a finding

**If a document should change**, change it. That is the whole point.

**If no documentation change is warranted**, say so in a commit trailer. The
justification is mandatory — a bare `none` is not accepted, because "none"
without a reason is indistinguishable from "I did not look":

```
Docs-Impact: none — the registry entry is a rename of an untouched action
```

**If the change affects a different document than the map suggests**, name it:

```
Docs-Impact: docs/CONFIGURATION.md
```

Both forms are read from the commit range, so they survive rebases and are
visible in `git log` next to the change they justify.

---

## 4. Library status labels

Every file in `docs/library/` (except its `README.md`) carries one line:

```
> **Library status: <label>.**
```

| Label | Means | Trust it for |
|---|---|---|
| `Current` | the rules or facts still hold | following them |
| `Current rules, historical audit` | the stated rules hold; per-item tables were taken at a point in time | the rules, after checking a row against the live code |
| `Historical` | describes a state that has changed | *why*, never *what* |
| `Superseded` | a later document replaced this | nothing — read the successor |

The label overrides any status line in the document body. Many library entries
still say "Draft" or "awaiting review" from before the work landed; the header
is what makes restoring them honest rather than misleading.

The gate checks that the label is present and that the entry is listed in the
[library index](library/README.md). It does **not** check that a historical
entry describes current behaviour — by definition it does not.

---

## 5. When a check is wrong

Every check has an explicit exemption registry in
[`scripts/docs_check/exemptions.py`](../scripts/docs_check/exemptions.py), with
a mandatory justification per entry.

If the gate is wrong about a case, add an entry and say why in the same
sentence. Do not add an entry to turn a red build green — that converts a real
defect into a permanent exemption, which is how a gate stops meaning anything.

Two live examples of correct exemptions:

- `docs/x.md`, `docs/guide.md` and friends are **illustrative filenames** in
  reference-identity tests. They never existed and must not be created just to
  satisfy a link checker.
- `mpm_context action=write_handoff` is not in the registered action enum, but
  the compact MCP wrapper serves it by rewriting it to `mpm_handoff write`.
  The gate accepts both surfaces and would be wrong if it did not.
- `agent_installation/measure/fixtures/` holds byte-for-byte copies of an older
  managed block, protocol and snippets, used to compare an older block against
  the current one. Their links point at paths that existed then. **Every**
  structural check skips that directory, because editing a snapshot to satisfy
  a link checker would destroy the reason it is kept. A dead link in a snapshot
  is as frozen as a dead adapter name in one.

---

## 6. Non-vacuity

The gate's own tests are in
[`scripts/tests/test_docs_check.py`](../scripts/tests/test_docs_check.py). Every
check is driven against a deliberately broken fixture and asserted to fail, and
against the repaired fixture and asserted to pass. There are also negative
controls: English prose that uses the verb "make" must not be read as a
Makefile invocation, and a link inside a code fence must not be read as a link.

Run them with `make test-docs-check`, or as part of `make test-scripts`.
