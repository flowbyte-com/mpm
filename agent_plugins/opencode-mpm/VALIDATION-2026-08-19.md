# OpenCode ↔ MPM Integration — Alpha Validation 2026-08-19

Verdict: **READY**

Adversarial onboarding test run by an OpenCode agent against the local MPM
installation (`~/.mpm` → `/home/v/workspace/projects/mpm`). Same prompt used for
the OpenClaw canary earlier the same day (see
`../openclaw-mpm-memory/VALIDATION-2026-08-19.md`).

## Integration

| Item | Value |
|---|---|
| Agent / framework | OpenCode 1.18.18 (`/home/v/.opencode/bin/opencode`) |
| Integration path | `~/.mpm/agent_plugins/opencode-mpm/` |
| Native mechanism | OpenCode plugin (TypeScript, `plugin` config array) |
| MPM interface used | `mpm call <tool> --payload '<json>'` (16 tools: 13 domain + 3 standalone) |
| Auto-load mechanism | Global config `~/.config/opencode/opencode.jsonc` → `"plugin": ["file:///home/v/.mpm/agent_plugins/opencode-mpm/dist/index.js"]` |
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