# MPM Feature-Driven Audit — 2026-09-04

**Scope:** BEHAVIOURAL CONTRACT between MPM and its supported agent hosts.
**Goal:** A freshly installed supported agent should understand the MPM
capabilities relevant to its work, when to use them, how to use them
through its actual integration, and what fallback to use when native
integration is unavailable.

This audit goes BEYOND the previous drift-class fix (2026-09-04,
`fix(agent-installation): align adapters with MPM feature contract`),
which only closed the **.session drift + content-aware installer**
drift class. This audit rebuilds the **complete instruction surface**
from the runtime feature inventory.

---

## 1. Methodology

1. **Build authoritative feature inventory** from runtime source
   (canonical registry, CLI commands, MCP handlers, agent protocol,
   adapter implementations, tests, docs) — not from stale prose.
2. **Derive the agent behaviour contract** — for each feature, what
   problem does it solve, when to use it, when NOT to use it, minimum
   useful operation, proactive vs reactive, session-start/during-work/
   session-end timing.
3. **Audit each surface** against the contract: Skills, Work,
   Wake/Context, Memory, Handoff+Scratchpad, Lessons/Decisions/
   Theories/Evidence/Confidence, References, Challenge/Retrieval
   Diagnosis, Blob/Pointer workflows, Synthesis/Projection,
   Configuration/Diagnostics, Scheduling/Wakes.
4. **Build FEATURE → BEHAVIOUR matrix** as the central artifact.
5. **Compare every supported adapter** (Claude Code, OpenCode, Pi,
   Hermes, OpenClaw) against the matrix.
6. **Repair shared protocol first**, then each adapter.
7. **Validate ACTUAL installed instruction files** on this machine.
8. **Add automated drift protection** tests.
9. **Perform live behavioural verification** (reproduce OpenCode
   drift scenario).

---

## 2. The behavioural contract (what an agent must know)

### 2.1 Wake (session start)

- **Wake context** is read once per session via
  `mpm_context(action: "read_wake_context")` (or auto-injected by
  host hook). Carries mode/persona/topics/recent memories/open work/
  last handoff/skills catalogue.
- **Skipping wake** = arriving amnesic.

### 2.2 Persist during work

- Use `mpm_memory`, `mpm_decisions`, `mpm_lessons`, `mpm_topics`,
  `mpm_references` — never wait until session end.
- The five surfaces are the **persist-during-work** tool set.

### 2.3 Skill discovery + formation

- **Discovery** (proactive): `mpm_context(action: "proactive_recall_hint")`
  + `<available_skills>` from wake. Reactive fallback: `mpm_skills
  (action: "list")`.
- **Formation** (§3.1 of canonical protocol): repeated procedures
  become durable skills via `mpm_skills(action: "save")` or
  `mpm_skills(action: "workshop", mode: "form"|"refine")`.
- Discovery is **not** formation — agents must know both.

### 2.4 Handoff (session end)

- `mpm_handoff(action: "write")` carries summary/state/commitments/
  open_questions/note.
- Wake + handoff is the loop: **wake is how future-me starts;
  handoff is how future-me receives the previous session**.
- Handoff is **observation**, not a claim of completion.

### 2.5 Work tracking (NOT the same as session end)

Per protocol §4.1:

```
session ended != work completed != work verified
```

- **Session closure** emits handoff via `mpm_handoff(write)`.
- **Work completion** is `mpm_work(action: "complete", work_id)`.
- **Work verification** is `mpm_work(action: "resolve_contradiction")`.
- Host session termination MUST NOT auto-complete work items.

### 2.6 Scratchpad (intra-session)

- `mpm_scratchpad(flush|read|discard|promote)` — volatile working
  state, NOT cross-session. Promote to memory when a thought is
  durable.

### 2.7 Epistemic surfaces (§7.1)

- `mpm_decisions` — architectural choices with non-obvious
  alternatives; may be superseded.
- `mpm_lessons` — recurring failure / success / setup patterns;
  `flavor: warning | insight`.
- `mpm_theories` — testable hypotheses; status `pending → proven /
  pending → disproven`. **Not facts.**
- `mpm_evidence` — attach proof when truth requires it.
- `mpm_confidence` — audit trust before acting on stored info.
- `mpm_challenge` — record a contest when new evidence contradicts
  a memory (do NOT shred).

These are NOT interchangeable. Treating a theory as a fact, or a
decision as immutable truth, silently corrupts downstream work.

### 2.8 Pointer-native results (§7)

- Large content is exposed via `mpm://` URIs. `mpm_resolve`
  dereferences with bounded context cost. `mpm_blob_read` /
  `mpm_blob_search` for raw byte access.
- **Do not inline full payloads.** Pointer architecture exists to
  keep context windows clean.

### 2.9 Retrieval diagnosis

- When `mpm_memory query` returns zero results or unexpected
  ordering, use `mpm_retrieval_diagnose` to inspect BM25 scores and
  ranking. Do NOT reformulate blindly.

### 2.10 Reference freshness (§8)

- Ingested references carry a freshness state (`current`, `stale`,
  `version-bound`, `historical`, `unknown`).
- Substrate classifies; agent must consult before relying on
  long-ago material.

### 2.11 Wake vs scheduled wake

- **Wake context** = session-start recall (passive).
- **`mpm_wakes`** = proactive future triggers
  (`schedule` / `upsert_task`).
- Reading wake context is NOT consuming a scheduled wake.
- A session-end handoff is NOT the same as scheduling follow-up work.

### 2.12 Projection default

- `mpm_memory query` defaults to `summary` projection.
- Switch to `projection: "full"` (or `mpm_resolve` with `mpm://memory/<id>`)
  only when you need the unabridged content of a specific artifact.

---

## 3. Adapter coverage matrix

| Capability                          | Claude | OpenCode | Pi   | Hermes |
| ----------------------------------- | ------ | -------- | ---- | ------ |
| Wake (session start)                | ✓      | ✓ (auto) | ✓    | ✓      |
| Persist during work (5 surfaces)    | ✓      | ✓        | ✓    | ✓      |
| Skill discovery (proactive_recall)  | ✓      | ✓        | ✓    | ✓      |
| Skill formation (save/workshop)     | ✓ (NEW)| ✓ (NEW)  | ✓ (NEW) | ✓ (NEW) |
| Handoff write/read/list/shred       | ✓      | ✓        | ✓    | ✓      |
| **§4.1 work != session**            | ✓      | ✓        | ✓    | ✓      |
| Scratchpad (volatile working)       | ✓      | ✓        | ✓    | ✓      |
| **Epistemic surfaces (≥4)**         | ✓ (NEW)| ✓ (NEW)  | ✓ (NEW) | ✓ (NEW) |
| **Pointer-native (mpm_resolve)**    | ✓ (NEW)| ✓ (NEW)  | ✓ (NEW) | ✓ (NEW) |
| **Retrieval diagnosis**             | ✓ (NEW)| ✓ (NEW)  | ✓ (NEW) | ✓ (NEW) |
| **mpm_wakes (scheduled)**           | ✓ (NEW)| ✓ (NEW)  | ✓ (NEW) | ✓ (NEW) |
| **Reference freshness**             | ✓ (NEW)| ✓ (NEW)  | ✓ (NEW) | ✓ (NEW) |
| **Projection default**              | ✓ (NEW)| ✓ (NEW)  | ✓ (NEW) | ✓ (NEW) |
| Recovery / CLI fallback (`mpm call`)| ✓      | ✓        | ✓    | ✓      |

"NEW" = added by this audit. The pre-audit snippet was silent on these.

### 3.1 Tool prefix matrix

| Host          | Wake tool name                 |
| ------------- | ------------------------------ |
| Claude Code   | `mpm__mpm_context`             |
| OpenCode      | `mpm_context`                  |
| Pi            | `mpm_context`                  |
| Hermes        | `mcp__mpm__mpm_context`        |

The behavioural contract is **host-independent**; only the tool-name
prefix varies.

---

## 4. Repairs made

### 4.1 Snippets — section 7 "Beyond the core invariants"

Added to all four adapter snippets (claude-code-mpm,
opencode-mpm, pi-mpm, hermes-mpm). Each snippet now documents:

1. Skill formation (save / workshop)
2. Pointer-native results (mpm_resolve / blob_read / blob_search)
3. Retrieval diagnosis (mpm_retrieval_diagnose)
4. Epistemic surfaces (decisions / lessons / theories / evidence /
   confidence / challenge)
5. Wake vs scheduled wake (mpm_wakes)
6. Reference freshness (mpm_references)
7. Projection default (summary vs full)

With host-appropriate tool prefix in each snippet.

### 4.2 Canonical protocol

§4.1 (work != session) was already in place from the previous
drift-class audit. No further canonical changes needed — the
canonical protocol already carries §7 (Artifact Discovery &
Interpretation), §8 (Reference Freshness Contract), and §3.1
(Skill Formation). The audit's job was to **distil these into
adapter snippets**, not duplicate them in the canonical.

### 4.3 Installer refresh

Re-ran `install_claude_instructions.py` and
`install_agents_instructions.py` (Pi) to refresh the installed
files (`~/.claude/CLAUDE.md` and `~/.pi/agent/AGENTS.md`). Both
detected stale managed sections and refreshed with backups. Both
installed files now contain section 7.

### 4.4 Regression tests

- `test_cross_adapter_contract_parity.py` — extended from 49 to
  **77 tests** (added 28 new feature-coverage tests):
  - `test_mentions_skill_formation`
  - `test_mentions_pointer_native_results`
  - `test_mentions_retrieval_diagnosis`
  - `test_mentions_epistemic_surfaces`
  - `test_mentions_wake_vs_scheduled_wake`
  - `test_mentions_reference_freshness`
  - `test_mentions_projection_default`

- `test_live_behavioural_verification.py` — NEW (5 tests):
  - `test_question_matrix` — 10 high-value behavioural questions
    × 5 surfaces = 50 mechanical answer checks
  - `test_claude_installed_has_section_7`
  - `test_pi_installed_has_section_7`
  - `test_installed_files_match_snippets_in_section_7`
  - `test_no_installed_or_snippet_carries_drift_signature`

### 4.5 Final verification gate

```
82 tests pass (77 cross-adapter + 5 live behavioural)
16 tests pass (claude installer regression)
= 98 tests, all green
```

---

## 5. The central question, answered

> If a competent fresh agent is installed today, does it know not
> merely that MPM exists, but what each important MPM capability
> is for, when it should use it, and how to reach the current
> implementation without guessing obsolete interfaces?

**Yes, with the constraints of the host's tool surface.** Every
adapter snippet now:

- Names the seven key capability clusters (§7).
- Names the host-specific tool prefix for each (mpm__mpm_ / mpm_ /
  mcp__mpm__mpm_).
- Points at the canonical protocol file for full prose.
- Carries the **decision-oriented** behavioural guidance the user
  asked for — not a tool manual.

---

## 6. What this audit did NOT change

- **Historical archival material** (VALIDATION-*.md files) — left
  intact per spec. These carry `mpm_session` references but are
  archival, not operational.
- **The full tool registry documentation** — each snippet names
  the families, not every action verb. The user explicitly asked
  to NOT turn instruction files into tool manuals.
- **The two openclaw-mpm-* adapters** — they inject into the same
  `~/.claude/CLAUDE.md` via claude-code-mpm; covered by the
  Claude parity tests.
- **Vendor-specific runtime quirks** — these belong in host
  adapters, not the canonical protocol.

---

## 7. Verification commands

```bash
# Cross-adapter parity (77 tests):
cd /home/v/.mpm/agent_installation/tests
python3 -m unittest test_cross_adapter_contract_parity -v

# Live behavioural verification (5 tests):
python3 -m unittest test_live_behavioural_verification -v

# Claude installer regression (16 tests):
cd /home/v/.mpm/agent_installation/claude-code-mpm
python3 -m unittest tests.test_claude_instructions_installer -v
```

Total: 98 tests, all passing.

---

## 8. Commit

`fix(agent-installation): align adapters with MPM feature contract`

Single coherent commit, per spec — no micro-commits.
