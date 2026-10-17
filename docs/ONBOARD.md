# `mpm onboard` — design

**Status:** approved (2026-10-13, §11); all three stages landed.

| Stage | Scope | State |
|---|---|---|
| 1 | command structure, read-only environment/readiness reporting | **landed** |
| 2 | guided model configuration | **landed** |
| 3 | opt-in host installation behind explicit consent | **landed** |

Stage 1 ships `cmd/mpm/onboard_cmds.go` with the full mode surface
(`report`, `models`, `hosts`, `--dry-run`, `--json`, `--yes`). The
report renderer, the JSON contract and the honesty caveats are final in
stage 1 because later stages only add what happens *before* the report
runs.

Stage 2 adds `cmd/mpm/onboard_models.go`. It opens **only** the model
gate (`onboardOptions.mutateModelsAllowed()`); `mutateHostsAllowed()`
stayed false at the time and was pinned by a test, so opening one gate
could not implicitly open the other. Stage 3 then opened it deliberately
and changed that test on purpose. The step decides *whether* to run the
existing `mpm config` wizard, prints what it would change, obtains
consent, and delegates — there is no second configuration system.

Four gates stand between the operator and a write, and all must open:
there is work to do; the run is not `--dry-run`; there is a terminal to
approve on; and the operator approved explicitly. A bare Enter is **not**
approval — deliberately stricter than `confirmPrompt`, which defaults to
yes. Without a terminal onboarding refuses rather than assuming consent,
and prints the scriptable equivalent instead.

A disabled embedding is a decision, not a gap, and is never
re-offered. A run that saved a generative profile but left the optional
embedding unconfigured is reported as **partial**: the wizard's
embedding step defaults to "leave unchanged", so that outcome is the
common one, and presenting it as total success would be the failure this
design is most concerned with.

Supersedes the earlier `mpm bootstrap` proposal; the command is
`mpm onboard`.

Stage 3 adds `cmd/mpm/onboard_hosts.go`. It opens the host gate
(`onboardOptions.mutateHostsAllowed()`), which means *capable*, not
*permitted*: the only hosts it will touch are the ones the operator
named on the command line, and `parseOnboardArgs` refuses the
auto-sweep vocabulary (`auto`, `all`, `detected`, …) **by name**, so
`mpm onboard --hosts install auto` is an error with a reason rather
than a silent no-op. Detection feeds the report; installation consumes
only the command line. `decideHostInstall` is the whole consent
surface and is a pure function, so the rejected-approval and
drifted-without-`--repair` branches are testable without a pty.

Three preservation rules are enforced rather than merely intended:
a `PASS` host is left byte-identical even when `--yes` is given; a
`WARN` (drifted) host is reported and **not** rewritten unless
`--repair` was also named; and after an install the inventory rows are
re-inspected, so a run that exits 0 without producing a managed block
is reported as unconfirmed rather than as installed. Rollback is
per-adapter and the report says so every time — see §6.7.

**Guiding principle:** *MPM explains its requirements; the operator chooses
the models.* Onboard may say what an embedding model is for and what breaks
without one. It may not name a vendor, suggest a model, discover one, or
pick a default.

---

## 1. What already exists (this design adds almost no new machinery)

Onboarding is largely **composition of existing surfaces**, not a new
system. This is deliberate — the brief forbids introducing a competing
configuration system, and the existing one is complete.

| Need | Existing surface | New code? |
|---|---|---|
| Guided model entry | `mpm config` (no args = interactive wizard, `handlers_config.go:708`) | **No** — delegate |
| Named profiles | `mpm config profile add/set/get/list/remove` | **No** — delegate |
| Component binding | `mpm config component set <c> <p>` | **No** — delegate |
| Capability binding | `mpm config capability set <cap> <component>` | **No** — delegate |
| Structural validation | `mpm config validate` (5 checks) | **No** — delegate |
| Current config state | `mpm config show` / `list` | **No** — delegate |
| Readiness verdict | `ReadReadiness(dm)` + `readinessOverall`, `readiness.go:107` | **No** — delegate |
| Diagnostic checks | `DoctorService.Check()` + `Tally()`, `service_doctor.go:86` | **No** — delegate |
| Endpoint locality | `orchestration/model_factory.go:103-110` | **No** — delegate |
| Credential precedence | `synth.ResolveAPIKey` / `APIKeyEnvVars`, `credentials.go:47` | **No** — delegate |
| TTY detection | `isatty()`, `router.go:1530` | **No** — delegate |
| Output formatting | `render` package + `ExtractJSONFlag` | **No** — delegate |

The genuinely new code is: the command family file, the report renderer,
and a host-detection bridge. That is all.

---

## 2. Provider posture — local/remote only

The v0.1-final decision stands and is **not** reversed here:

> "MPM does not maintain provider/model catalogues and does not discover or
> select models for the operator. Manual configuration via Custom +
> protocol is the single public path. Branded menus and embedding probe are
> removed from v0.1-final."

Onboard therefore shows **no vendor names, no model suggestions, no model
discovery, no implicit defaults.**

The local/remote distinction is **explanatory framing, not a configuration
field**. It is derived from the endpoint, using the locality rule MPM
already applies when deciding whether a credential is required
(`model_factory.go:103-110`: `ollama`, `11434`, `lmstudio`, `localhost`,
`127.0.0.1`). Onboard asks the question because it changes what the
operator must supply — nothing is persisted from it.

| | Local | Remote |
|---|---|---|
| Credential | optional | usually required |
| Supported protocols | Ollama, OpenAI-compatible | OpenAI-compatible, Anthropic-compatible |
| Latency/privacy note | data stays on host | data leaves host |
| Model ID | operator-supplied | operator-supplied |

Operator supplies: **base URL, protocol, model ID, credential** (via
`mpm config profile set <name> api_key`, which prompts with echo
suppressed, or `--stdin`). Credentials are never passed as argv — that is
already enforced at `handlers_config.go:1843-1846`.

**The removed embedding probe stays removed.** `mpm config detect-embedding`
is retired and exits 1 (`handleDetectEmbeddingRemoved`,
`handlers_config.go:185`). Onboard does not restore it, and does not call
an embedding endpoint to validate.

---

## 3. Required vs optional — the actual runtime contract

This is not invented for onboarding; it is the existing, deliberate
asymmetry documented at `service_doctor.go:738-744`:

> "Memory LLM is classified as REQUIRED … absence surfaces as WARN (not
> INFO) … This asymmetry vs Embedding (INFO when absent) is intentional and
> matches the runtime contract: synthesis is required for memory
> consolidation; embedding is a retrieval-augmentation feature the substrate
> degrades gracefully without."

| Capability | Bound to | Needs generative | Needs embedding |
|---|---|---|---|
| `planner` | `memory` | yes | no |
| `summarise` | `memory` | yes | no |
| `reviewer` | `critic` | yes | no |
| `reflect` | `critic` | yes | no |
| `--semantic` recall | — | no | **yes** |
| Semantic half of hybrid ranking | — | no | **yes** |
| Vector index / IVF clustering | — | no | **yes** |
| Dedup cosine similarity | — | no | **yes** |
| `mpm ops backfill-embeddings` | — | no | **yes** |
| BM25/FTS5 recall, memory CRUD, citations, scheduling, audit, doctor | — | no | no |

**Consequence statements onboard makes verbatim:**

- *Skipping the generative profile* → synthesis, admission, compact lesson
  synthesis and critic review are unavailable. Memory add/recall, BM25
  search and scheduling all still work. Doctor reports **WARN**.
- *Skipping the embedding profile* → semantic recall, vector ranking, the
  vector index and embedding backfill are unavailable. BM25/FTS5 and all
  structured retrieval continue unchanged. Doctor reports **INFO**, by
  design — absence of an optional feature must not make the substrate look
  unhealthy.

Note the asymmetry is also why skipping is genuinely different for the two:
skipping generative is a real functional loss; skipping embeddings is a
quality upgrade declined.

---

## 4. Validation — restricted to the existing contract

Per the brief, onboard performs **no live model calls of its own**. What it
runs is exactly what already exists:

1. **`mpm config validate`** — structural only: provider present; profiles
   carry `provider` + `model`; components resolve; capabilities resolve to
   a known component; known components resolve to a profile. No IO.
2. **Resolution outcome** — `ResolveProfile(component)` for generative and
   `ResolveEmbeddingConfig(cfg)` for embedding. These are pure resolution
   over on-disk config; they report `configured` / `unavailable` /
   `misconfigured` / `absent` / `disabled`. Embedding is **never** resolved
   through `ResolveProfile`, and an unset embedding binding must not inherit
   `profiles.default`.
3. **Stored-embedding heterogeneity** — a local read of
   `memories.embedding_dimension` / `embedding_source`, reporting the
   distribution and flagging heterogeneity. This is a SQL count over
   existing indexed columns, not a model call.

### What onboarding deliberately cannot verify

Stated plainly in the report rather than glossed:

- **Live reachability / auth of the configured endpoints.** `mpm doctor`
  covers this via `probe.RunActiveProbes`; onboard points at that command
  instead of duplicating it.
- **Embedding dimension against a live provider.** The probe path
  explicitly does not check dimension (`probe/embedding.go:9-11`: "today's
  contract does not encode a known dimension"). Onboard cannot verify it
  without restoring the removed probe, so it does not claim to.

### Embedding model fingerprint on `memories`

`memories` now carries a full `(model, dimension)` fingerprint on every
row (`internal/core/schema.go:1569`); the write path records it
(`internal/core/db.go:3742`) and `VectorSearch` filters on it
(`internal/core/db.go:4270`). The memories backfill is model-aware: a
row without a fingerprint is pending rather than skipped
(`cmd/mpm/backfill_embeddings.go:412-422`), and ordinary model-mismatch
recovery is automatic — no `--force`-class flag is needed. The explicit
operator override is `--force-reembed`
(`cmd/mpm/backfill_embeddings.go:44`), used when the operator wants the
full corpus re-embedded under a known provider, not when remediation
fails closed on its own.

The remaining fingerprint asymmetry is on `sessions`, `topics`, and
`capabilities`: those three declare an `embedding` column but no
`embedding_model`, and hold zero vectors. They are the live gap
onboarding surfaces rather than `memories`. See
[`NOMIC-MIGRATION.md`](./NOMIC-MIGRATION.md) §1 ("Tables deliberately
not migrated") for the current state, and [`CLOSEOUT.md`](./CLOSEOUT.md)
§1.2, §1.4 and §1.9 for the design rationale and the lessons that
produced it.

---

## 5. Flag reconciliation

The proposed flags are consistent with current architecture. Two
adjustments follow from the real CLI.

| Proposed | Status | Notes |
|---|---|---|
| `mpm onboard` | adopt | Full guided run: models → hosts → report |
| `mpm onboard --models` | adopt | Model step only. Skips hosts entirely — satisfies "rerun just model setup without repeating framework installation" |
| `mpm onboard --hosts` | adopt, **with explicit-consent boundary** | Host step only. See §6. There is no `--hosts auto`. |
| `mpm onboard --dry-run` | adopt | Established convention — `changelog.go:84`, `backfill_embeddings.go:41`, `install.sh --dry-run`. `install.sh` also has `--check`/`--validate`/`--yes`; onboard borrows the same vocabulary |

Additional flags required by the architecture, not by the brief:

- `--json` — mandatory, via the shared `ExtractJSONFlag` helper
  (`router.go:1404`). Policy is explicit-or-error: a flag that silently
  does nothing is a defect (`doctor_flag_contract.go:3-6`).
- `--yes` — non-interactive confirmation, matching `install.sh`.
- `--help` — via the shared `requireHelpShortCircuit` helper
  (`handlers_helpers.go:42`), which exists because nine commands previously
  mutated on `mpm <cmd> --help`.

`--models` and `--hosts` are mutually exclusive in effect: whichever is
passed runs alone, and neither runs the other. Neither implies re-running
the step that was skipped.

---

## 6. Host step — installable, behind an explicit-consent boundary

**Approved: adopt-as-designed, with a strict explicit-consent boundary.**
Onboarding *may* install new framework integrations. Framework **detection
alone must never trigger installation.**

### 6.1 The invariant that is preserved, not weakened

`agent_installation/scripts/reconcile_managed_blocks.py:15-36`:

> "MPM must never opt a machine into a host it does not already use… When
> `reconcile_if` is present and NO probe matches, the adapter is skipped as
> 'integration not installed' — the machine is left exactly as it was. **MPM
> never creates the integration it would then have to reconcile.**"

The rationale is that MPM is installed on any machine with any MPM
integration, so a "does the `mpm` binary exist" probe would be satisfied on
a host that has never heard of MPM. An auto-sweep would defeat the gate.

**This invariant stands unchanged.** The reconcile guard keeps its current
behaviour. Onboarding does not add an automatic installation path through
host discovery, and no probe is weakened or removed.

### 6.2 The distinction, stated explicitly

| Activity | Mutating? | Rule |
|---|---|---|
| Framework detection | **read-only** | Never installs. Discovers; proposes; stops. |
| Existing-integration inspection | **read-only** | Never repairs, rewrites or replaces. |
| Offering installation of a missing adapter | no | A proposal only. Recording a proposal is not consent. |
| Installing a selected adapter | **yes** | Requires the operator to select **that host** and approve the mutation. |
| Reconciling an existing integration | **yes** | Only under an approved host target. Preserves the guard above. |

Detection and consent are two separate steps with an operator decision
between them. Nothing discovered by step one flows into step three without
passing through step two.

### 6.3 Unattended operation

Unattended use (`--yes`, piped stdin, agent-invoked) requires **both**:

1. **Explicit host targets** — named hosts, never a discovered set. There is
   deliberately **no `--hosts auto`**, and no flag that expands a detected
   set into install targets.
2. **Explicit mutation approval** — a separate acknowledgement that
   installation is authorised.

A host present in the detected set but absent from the explicit target list
is **skipped, not installed**. Absence of a target is never treated as
consent.

### 6.4 Preservation of existing adapters

An adapter already integrated is **left as it is** unless the operator
approves repair or replacement. Onboard does not:

- overwrite a working managed section because a newer one is available
  upstream,
- replace an adapter to "standardise" the installation,
- remove or reconfigure a host the operator did not name.

Drift is *reported*; acting on it is a separate, named approval.

### 6.5 Delegation

Installation delegates to the **existing** adapter installers and the
shared runtime staging mechanism. Onboard adds no second payload path, no
bespoke copy step, and no new manifest format. The per-adapter installers
already perform the `<file>.bak.<TS>` backup; onboard reports those paths
and does not invent its own.

### 6.6 Skipping is a supported outcome

Declining an adapter, or having none applicable, is a **successful**
onboarding outcome, reported under *deliberately skipped* — not a failure,
and not something requiring remediation. `opt_in: true` adapters
(`mpm-auto-mode-persona-openclaw`, `mpm-hermes`) are skipped by the existing
gate and onboard honours that.

### 6.7 Rollback gap that onboarding must not paper over

`uninstall.sh` deliberately scopes to `~/.mpm`, `~/.local/bin`,
`~/.config/systemd/user`, `~/.config/autostart`, `~/.config/mpm` and
**never touches host instruction files**. Those are removable only by
re-running each adapter's installer with `--uninstall`, one adapter at a
time. There is **no orchestrator** mirroring the reconcile sweep, and
**no compensation pass** if adapter 3 of 5 fails.

Onboard must therefore:
- report per-adapter backup paths as it writes them,
- state that rollback is per-adapter, not atomic,
- **not** claim a single-command undo.

Closing this gap (an adapter-aware uninstall orchestrator plus a
compensation pass) is separate work and is **not** part of this design.

### 6.8 Required documentation change

Because onboarding is now a deliberate, bounded exception rather than an
accidental one, `reconcile_managed_blocks.py`'s header comment gains a
carve-out paragraph stating that `mpm onboard` may install into
operator-selected hosts, that detection is read-only, and that the
reconciliation guard itself is unchanged. This is a **documentation change
only** — no behavioural change to the reconcile path.

---

## 7. Report contract

Distinguishes three states, per the brief:

- **Operational** — configured and structurally valid.
- **Deliberately skipped** — operator chose to skip; consequences printed.
- **Needs attention** — configured but failing resolution or validation.

Structure:

```
MPM · Onboarding report

Models
  ✓ Generative profile    memory  → minimax        configured
  ○ Embedding profile     embedding → embedding    not configured (optional)
      skipped: semantic recall, vector ranking, vector index, backfill unavailable
      BM25/FTS5 recall and all structured retrieval continue
      configure later:  mpm config

Hosts
  ✓ Claude Code     ~/.claude/CLAUDE.md          MPM section present
  ○ OpenCode        not detected
  ! Pi              ~/.pi/agent/AGENTS.md        installed, section drifted
      remedy:       make refresh-installed

Attention
  ! Pi managed section has drifted from the source of truth
```

`--json` mirrors this with `success`, `models`, `hosts`, `attention`,
`skipped` keys, reusing doctor's `{"success": true, ...}` convention and
normalising nil slices to `[]` so scripts can iterate unconditionally.

**Not-TTY behaviour.** Following the `mpm tour` precedent
(`handlers_tour.go:20-24`), onboard pauses for input only when stdin is a
terminal; piped or agent-invoked it emits the whole stream without
blocking. Non-interactive without a TTY **refuses to prompt** and prints
the equivalent non-interactive commands, matching
`handleConfigInteractive`'s existing refusal (`handlers_config.go:708-712`).

---

## 8. Idempotency and resumability

- Onboard **never overwrites a working configuration**. If a component
  already resolves to a valid profile, onboard reports it and moves on;
  it does not re-run the wizard for it.
- Re-running `mpm onboard` on a configured install produces a report with
  no writes.
- `mpm onboard --models` after a complete install touches models only.
- A fresh install and an established install take the same code path —
  onboarding detects state rather than branching on "first run", which MPM
  has no first-run state for.

---

## 9. Approval and safety rules

Never, without explicit operator approval:

| Action | Rule |
|---|---|
| Downloading models | never performed |
| Installing runtimes | never performed |
| Overwriting a working config | never — report and skip |
| Live model calls | not performed by onboard; `mpm doctor` probes on its own terms |
| Paid API calls | none — onboard issues no model request |
| Adapter install into a host | only for a host the operator explicitly names |
| Credentials on argv | already impossible; the CLI rejects it |

`--dry-run` prints the full plan — including every file that would be
written — and changes nothing. This is the default recommended first run.

---

## 10. Registration checklist

Four touchpoints, all required (missing any one leaves `onboard` partly
broken):

1. `r.Commands` registry map — `router.go:46`. **Keep the entry on one line
   with a literal value**: `cmd/gen-cli/main.go` parses this map with
   `go/ast` to generate README documentation and will silently miss a
   multi-line entry.
2. `Execute` dispatch switch — `router.go:278`.
3. `handleCommandHelp` — `router.go:709` (for `mpm onboard --help`).
4. `commandsWithSubcommandDispatch` — `router.go:834` (var) / `:835` (map literal), needed so
   `--help` after a step name is not swallowed.

Plus help text: add a case to `handleHelp` — `router.go:569` if
`mpm help onboard` should resolve specifically.

**Files:** `onboard_cmds.go` (whole family, matching the
`<noun>_cmds.go` convention) with a header comment stating the subcommand
table, as every family file does. Tests in `onboard_cmds_test.go`, next to
source. Registration is pinned by an existing convention test
(`f_d3_route_registration_test.go:21`).

**Relationship to `mpm tour`:** `mpm tour` is described in the router as an
"onboarding walkthrough" (`router.go:106`), and its demo strings call it the
recommended way to onboard a new operator (`handlers_tour.go:82`). It is a
cognitive-verb walkthrough; `onboard` is a setup-and-verify flow. They
should be distinguished in help rather than merged — `mpm tour` teaches
the verbs, `mpm onboard` configures and verifies the substrate.

---

## 11. Resolved decision

**Approved 2026-10-13: option (A), adopt-as-designed, with a strict
explicit-consent boundary.** See §6 for the full rules.

The binding constraints, restated so they survive refactoring:

- Detection is read-only. Detection never installs.
- Existing-integration inspection is read-only. It never repairs or replaces.
- Installing a missing adapter requires the operator to select **that host**.
- Unattended operation requires explicit host targets **and** explicit
  mutation approval. There is no `--hosts auto`.
- Existing adapters are preserved unless repair or replacement is approved.
- Reconciliation must never silently expand MPM into additional frameworks.
- The `reconcile_managed_blocks.py` guard is **not** weakened; no automatic
  installation path exists through host discovery.
- Installation delegates to the existing adapter installers and shared
  runtime staging.
- Skipping an integration is a supported outcome, not a failure.

The residual, accepted limitation is §6.7: adapter rollback is per-adapter
with no orchestrator and no compensation pass. Closing it is separate work.

---

## Related

- `docs/CONFIGURATION.md` — full configuration reference
- `docs/INSTALL.md` — §1d bootstrap cognitive state
- `docs/RECALL-MUTATION.md` — recall-time mutation behaviour