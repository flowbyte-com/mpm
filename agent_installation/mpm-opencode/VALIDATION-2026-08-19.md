# OpenCode ↔ MPM Integration — Alpha Validation 2026-08-19

Verdict: **READY**

> **Scope notice (2026-10-16).** This document validates the OpenCode
> **V1** plugin (`@opencode-ai/plugin`) against OpenCode **1.18.18**. It
> does NOT demonstrate plugin loading under OpenCode 2. OpenCode 2
> changed both the plugin API (V2 `@opencode/plugin`) and the loader
> (directory plugins resolve via a root-level `server.*`/`index.*`
> module; a FILE registration is refused, and a directory without a
> root entry is dropped silently). A config carrying the V1-era
> `dist/index.js` file registration therefore passes every claim below's
> *installer-level* checks while loading nothing at runtime. See
> **"2026-10-16 Runtime Validation (OpenCode 2)"** at the bottom for the
> evidenced OpenCode 2 facts; `docs/CLOSEOUT.md` §3.1 remains the
> authoritative 2026-10-13-era position, which this addendum brings
> current. See also issue #19 for the audit trail.

Adversarial onboarding test run by an OpenCode agent against the local MPM
installation (`~/.mpm` → `/home/v/workspace/projects/mpm`). Same prompt used for
the OpenClaw canary earlier the same day (see
`../openclaw-mpm-memory/VALIDATION-2026-08-19.md`).

## Integration

| Item | Value |
|---|---|
| Agent / framework | OpenCode 1.18.18 (`/home/v/.opencode/bin/opencode`) |
| Integration path | `~/.mpm/agent_installation/opencode-mpm/` |
| Native mechanism | OpenCode plugin (TypeScript, `plugin` config array) |
| MPM interface used | `mpm call <tool> --payload '<json>'` (16 tools: 13 domain + 3 standalone) |
| Auto-load mechanism | Global config `~/.config/opencode/opencode.jsonc` → `"plugin": ["file:///home/v/.mpm/agent_installation/opencode-mpm/dist/index.js"]` |
| MPM binary actually resolved | `/home/v/.mpm/bin/mpm` (canonical; PATH-independent fallback) |
| Database actually used | `/home/v/workspace/projects/mpm/src/db/mpm.db` (via `MPM_WORKSPACE=/home/v/.mpm`) |

## Changes (integration defects fixed)

1. **Plugin was never actually loadable by opencode.** The module exported only
   `{ server }`; opencode requires an `id` and refused the path plugin
   (`Path plugin ... must export id`). Added `id: "opencode-mpm"` to the default
   export. This is why the index row said "live" but the plugin was inert.
2. **Unbuilt in place.** `dist/` did not exist; `npm install && npm run build`
   now produce `dist/index.js` (gitignored, rebuild per-machine).
3. **Not wired anywhere.** Global config had no plugin entry; project config
   referenced a nonexistent `./agent-plugins/opencode-mpm-plugin` (legacy pCloud
   layout) plus a bogus `"list"` npm spec. Global config now wires the plugin;
   project config's dead entries removed.
4. **PATH-dependent binary resolution.** `MPM_BINARY ?? "mpm"` relied on the
   interactive shell's PATH. Added deterministic resolution:
   `MPM_BINARY` → `$HOME/.mpm/bin/mpm` (canonical) → `mpm` on PATH.
5. **`MPM_WORKSPACE` was documented but not implemented.** README claimed
   `MPM_WORKSPACE` was read; code never touched it. The spawn now pins
   `MPM_WORKSPACE` (`env` override → `$HOME/.mpm`), so fresh non-interactive
   sessions converge on the same substrate.
6. **No exit-code check.** `callMpm` ignored the child exit status. A binary that
   printed a parseable JSON envelope to stdout and exited non-zero would have
   been treated as success. Now `code === 0` is required.
7. **Error text buried in a raw tail dump.** MPM writes error envelopes to
   stderr; `formatFailure` now extracts the `error` field from the stderr
   envelope (`mpm_memory error: unknown action "x" ...` instead of a 300-byte
   log/JSON dump).

## Verification

| Test                        | Result              | Evidence |
| --------------------------- | ------------------- | -------- |
| MPM discovery               | PASS                | `mpm_system health_check` via plugin → `ok:true`, `db_path` returned |
| Native agent invocation     | PASS                | Fresh `opencode run` session loaded plugin; model invoked `mpm_*` tools directly |
| Memory write                | PASS                | `mpm_memory save` via plugin → `id af0a78543d492515` (verified in DB) |
| Memory retrieval            | PASS                | `mpm_memory query` via plugin → count 1, content + id match |
| Retrieval diagnostics       | PASS                | `explain_retrieval` via plugin → structured per-node diagnostic |
| Cross-session continuity    | PASS                | Handoff `408c06895f8c28ce` written via plugin; fresh session read it back (`agent:main:main:2026-08-19-opencode-alpha-validation`) |
| Missing MPM handling        | PASS                | `MPM_BINARY=/nonexistent` → boot warning + `mpm not reachable: spawn ... ENOENT`, no fabricated success, session usable |
| Malformed response handling | PASS                | stdout is ignored when unparseable; stderr error envelope extracted; fake binary printing JSON to stdout + exit 1 correctly reported as failure |
| Non-zero exit handling      | PASS                | `code === 0` gate; fake binary `success:true` + exit 1 → `mpm call failed` |
| PATH independence           | PASS                | Clean env (`env -i HOME=/home/v`) resolves `/home/v/.mpm/bin/mpm` + `/home/v/.mpm`; health check succeeds |
| Shared substrate            | PASS                | `db_path /home/v/workspace/projects/mpm/src/db/mpm.db` — same DB as OpenClaw/other integrations |

## Findings

### Integration defects fixed

See "Changes" above.

### MPM defects discovered

None confirmed. One observation, **not** a confirmed defect: a fresh
`MPM_WORKSPACE` that points at a directory with a partially-initialized
`src/db/mpm.db` refuses to initialize schema with `timestamps unification
migration deferred: TEXT-affinity columns remain after schema DDL flip`. This
was reproduced once on a stray DB created by a buggy workspace fallback during
this validation; 4/4 genuinely fresh workspaces initialized cleanly. The
defensive refusal is arguably correct behavior for a half-migrated DB. Flagged
for awareness only.

### Limitations

- Live tests used `MPM_WORKSPACE=/home/v/.mpm` and `opencode/deepseek-v4-flash-free`
  (the default provider for `opencode run`). The ollama `kimi-k2.6:cloud`
  provider config is not active in the current global config.
- `opencode run` fresh sessions are one-shot; the continuity test wrote a handoff
  in one session and read it in another, which is the strongest live proof
  available for this agent lifecycle.

### Post-validation correction

At validation time, the test handoff could not be removed — no shred action
existed on `mpm_session` (reported as `MPM-GAP-SHRED-HANDOFF`, matching the
OpenClaw canary's finding). That gap has since been closed by commit `0583bea`
(v0.1.0-prealpha.2, `c7cefb4`): `mpm_session` gained a `shred_handoff` action.
Verified 2026-08-19 with the rebuilt binary:

```bash
mpm call mpm_session --payload '{"action":"shred_handoff","params":{"session_id":"agent:main:main:2026-08-19-opencode-alpha-validation"}}'
# → {"handoff_id":"408c06895f8c28ce","rows_deleted":1,"shredded":true,"success":true}
```

The test handoff `408c06895f8c28ce` was removed via the supported interface;
zero test handoffs remain.

## Cleanup

Removed via supported `mpm_memory shred` interface:

- `5bb6502bb11a7aec` — discovery probe
- `d01e53724d144a7e` — write_one
- `12cce15239c9867f` — write_two
- `19304e3488f61a1f` — failure-path test ("should not persist")
- `f96900795c6baa1d` — failure-path test ("should not persist")
- `af0a78543d492515` — live `opencode run` test memory

Remaining (cannot be removed via supported interface):

- None. The test handoff `408c06895f8c28ce` was removed with the
  `shred_handoff` action added in v0.1.0-prealpha.2 (see post-validation
  correction above).

No MPM core files modified (`git status --short`: only `src/index.ts` modified,
`package-lock.json` untracked; `dist/` and `node_modules/` gitignored). No
commits made. No credentials or config secrets touched.
---

# 2026-10-16 Runtime Validation (OpenCode 2)

Everything below is 2026-10-16 evidence against **OpenCode v2.0.30**
(`/home/v/.opencode/bin/opencode` → v2.0.30) and the ported V2 plugin —
it is deliberately separate from the 2026-08-19 V1-era validation above,
because issue #19 mixes the two eras.

## Facts established by direct loader inspection (decoded from the bun binary)

- Directory plugins are resolved ONLY via root-level module names:
  the loader's resolver tries `server` then `index` at the directory
  ROOT (`resolve(dir, o || "index")`) and never consults package.json
  `main`/`exports`.
- A registered FILE target is refused: `configured plugin path must be
  a directory` (WARN).
- A registered directory WITHOUT a resolvable root entry is dropped at
  scan time (`if (!w.server) return []`) with NO log output; the only
  later symptom is `failed to load plugin` / `Plugin entrypoint not
  found` for a resolved-but-broken entry, or — for a dropped entry —
  nothing at all: `/api/plugin` reports only builtins and every
  `opencode` subcommand still exits 0.
- A loadable plugin must default-export a definition accepted by
  `Plugin.define` (`{ id, setup|effect }`); the V1 payload fails with
  `Plugin must export a default definition with an id and an effect or
  setup function`.
- The SDK specifier `@opencode/plugin` is aliased by the host loader
  (docs examples load with zero node_modules).

## The defect

From 2026-09-17 to 2026-10-16, `install.sh` registered
`file:///…/.mpm/agent_installation/mpm-opencode/dist/index.js` — a FILE
URL. Under the July-2026 opencode config
schema this was refused outright; on 2026-10-16 the runtime showed the
entry silently dropped. Every install and every `--verify` reported
success throughout, because verification compared the registered path
against what the installer expected to write, not against what the
loader resolves. The plugin therefore loaded nothing for ~4 weeks while
the AGENTS.md managed block kept injecting protocol text and 228
historical `framework_name='opencode'` tool rows (recorded via the
`mpm` CLI on the same machine) made the integration LOOK live. (Verification
records from before 2026-10-16 are not evidence of plugin loading.)

## Exact 2026-10-16 runtime evidence (plugin loading WORKS after the fix)

| Claim | Evidence |
| --- | --- |
| Fresh host loads the plugin | `cd /tmp/opencode && opencode run --standalone --auto '…'` — boot log clean (no `configured plugin path must be a directory`, no `failed to load plugin`); plugin module `dist/index.v2.js` executed (banner `mpm-opencode (V2 port) loading; mpm binary: /home/v/.mpm/bin/mpm`), 17 tools registered (14 domain + 3 standalone), id `mpm-opencode` |
| Real agent turns invoke MPM tools | In the same isolated run: `mpm_memory` action `query` returned 2 live memories; `mpm_context` action `read_wake_context` returned live wake content (AgentShell Stage 1b item + consistency probe + Stage 2b handoff) |
| Provenance proves OpenCode origin | MPM `tool_invocations` table (DB COPY `/tmp/opencode/mpm-copy3.db` — live DB never touched for diagnostics): row `tool_name=mpm_memory, action=query, framework_name='opencode', started_at=2026-10-16T12:05:12Z, status=success`. 228 pre-existing `opencode` rows are V1-era/CLI hybrids — the row above is the first proven plugin-invoked row |
| User's persistent server loads it | opencode service restarted (PID 300990, 2026-10-16); post-restart log has zero new WARN/ERROR; the live session toolset contains all 18 `mpm_*` tools; a native `mpm_context read_wake_context` from that session returned live wake data |
| Loader contract verified negative | Registered `dist/index.js` FILE (alone) → WARN `configured plugin path must be a directory`; registered directory without root entry (alone) → registry all-builtin, zero warnings — reproduced during root-cause isolation |

## Post-fix installer verification (installer-level, 2026-10-16)

- Real-tooling end-to-end in a sandbox HOME: real `tsc` build → staged
  package = exactly `server.js`, `package.json`,
  `dist/{index.v2.js,workspace.js,resolve-mpm-binary.js}`;
  fresh config written with the package-directory registration under
  the V2 `plugins` key; `install.sh --verify` → ok.
- `--verify` REJECTS: a surviving `dist/index.js` file registration
  (remediation hint names the defect); a registered package without
  `server.js` ("no root entry server.js ... dropped silently").
- `--uninstall` removes every MPM-owned shape (package dir, file URL,
  legacy `opencode-mpm` variants, legacy `plugin` key) and keeps
  unrelated plugins, comments, trailing commas, CRLF and BOM intact.
- Regression coverage: `tests/test_jsonc_config.py` (31), installer
  `tests/test_opencode_install.py` (35 incl. negative controls), staging
  validator tests in `agent_installation/tests/`
  `test_runtime_package_provisioning.py` (52).

Known limitation (unchanged from V1): `fetchWakeContext` stamps no
provenance env, so wake-context fetch rows record under the default
`mpm-cli` actor; tool-invocation provenance is unaffected.

## Cleanup (2026-10-16)

The isolated `opencode run --standalone` harness used its own scratch
MPM copy; live substrate test rows were queried only. Live
`~/.mpm/agent_installation/mpm-opencode` was upgraded to the ported V2
payload + root `server.js` shim (this machine's instance), registered as
`file:///home/v/.mpm/agent_installation/mpm-opencode`.
