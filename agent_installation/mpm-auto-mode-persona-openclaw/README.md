# @openclaw/mpm-auto-mode-persona

Per-turn auto mode/persona switching for OpenClaw, driven by [MPM](https://flowbyte.com/mpm)'s `mpm route` command.

> **Note (2026-09-02):** Renamed from `mpm-auto-route` to `mpm-auto-mode-persona-openclaw` for clarity — "route" alone didn't say *what* is being routed. Old name still appears in pre-2026-09-02 docs and the `docs/archive/*` historical trail.

## What it does

On every inbound user message, captures the prompt and shells out to `mpm route --apply "<prompt>"`. MPM returns a `<system-reminder>...</system-reminder>` block (or empty stdout if `~/.mpm/active.json` is blank). The plugin caches the block and, on the next `agent:bootstrap` event, appends it to the SOUL.md bootstrap file content.

Result: the LLM sees the selected mode/persona directives inline, switching behavior per turn without manual intervention.

## Why a plugin (not core)

Zero core modification. Pure transport adapter. MPM owns the selector (three-state gate: blank / `auto` / manual). This plugin is just plumbing.

## Install

```bash
# From this directory (CWD-independent):
./install.sh
```

`./install.sh` is the canonical entry point and performs the full
OpenClaw-specific setup in the correct order:

1. Locate MPM via the canonical install paths
   (`$HOME/.mpm/bin/mpm` first, then `$HOME/.local/bin/mpm`).
   The OpenClaw gateway runs under systemd --user with a stripped
   PATH; a PATH-resolved `mpm` (the manifest's documented default)
   fails at runtime with `spawn mpm ENOENT`. `install.sh` persists
   the resolved absolute path into `plugins.entries.<id>.config.mpmBin`.
2. Inspect existing plugin state via `openclaw plugins inspect --json`.
   The installer distinguishes three cases (verified against OpenClaw
   2026.9.5):
     absent              — plugin id not in the registry → fresh install.
     linked-from-here    — plugin id registered and rootDir equals
                           this adapter's directory → skip install step.
     conflicting         — plugin id registered but rootDir points
                           elsewhere → hard error, no overwrite.
3. Install (only when state was "absent"):
     openclaw plugins install . --link --force --accept-capabilities
   The three flags are the documented 2026.9.4 contract for installing
   a non-ClawHub local source that declares capabilities.
4. Persist absolute mpmBin (PATH-gotcha mitigation).
5. Converge pending state migration
   (`openclaw update repair`) — otherwise the install record stays
   in "state migration pending" state and `openclaw doctor` warns.
6. Bounded safe gateway restart (when reachable; if update repair
   stopped the gateway, systemd --user will auto-restart it).
7. Verify (openclaw plugins inspect + plugins list).

### Re-running (idempotency)

Re-running `./install.sh` on a host where the plugin is already
correctly linked from THIS adapter's directory is genuinely idempotent
for the install step: no `openclaw plugins install` is reissued, no
trust warning is emitted, no `installedAt` timestamp is bumped.
The `update repair` step IS reissued on every run, which is harmless
(idempotent) but may briefly surface a "stopping the managed
gateway" log line.

### Manual override of the install

If you cannot run `install.sh`, the bare-minimum sequence the
adapter installer performs is:

```bash
openclaw plugins install . --link --force --accept-capabilities
openclaw config set plugins.entries.mpm-auto-mode-persona-openclaw.config.mpmBin "$HOME/.mpm/bin/mpm"
openclaw update repair    # converge pending state migration
```

The bounded gateway restart is then left to systemd. `install.sh`
is the supported path; the bare CLI sequence above is for
diagnostics only.

### Verify after install

```bash
openclaw plugins inspect mpm-auto-mode-persona-openclaw --json     # confirm enabled, rootDir matches
openclaw doctor --lint --json | grep -i migration                   # confirm no pending migration
```

### Uninstall

The plugin is part of the host-level MPM OpenClaw integration. To
remove BOTH this plugin and `mpm-memory-openclaw` atomically, use the
host-level uninstaller at
`agent_installation/uninstall-openclaw.sh`. Do NOT just
`openclaw plugins uninstall` this id by hand — the plugin entries
and any sibling plugin state are tied together and a partial
uninstall leaves a broken state. The host-level uninstaller is
idempotent and refuses to seize unrelated plugin installations.

Requires MPM installed at the canonical location `$HOME/.mpm/bin/mpm`
(alpha default), or reachable on PATH (or set the absolute path via
`config.mpmBin` — see [Configuration](#configuration)). The
auto-switch is silently a no-op when `mpm` cannot be resolved.

## Configuration

```yaml
plugins:
  entries:
    mpm-auto-mode-persona-openclaw:
      config:
        enabled: true          # default true. Set false to no-op the plugin.
        mpmBin: mpm            # path to mpm binary. Default 'mpm' (PATH-resolved).
        timeoutMs: 5000        # subprocess timeout. Default 5000ms.
```

`plugins install` enables the plugin but `enabled: false` disables it at runtime (vs. `plugins disable` which removes the registration entirely).

## Manual override behavior

All driven by `~/.mpm/active.json` — OpenClaw doesn't care:

- `mpm switch greybeard` → `persona: "greybeard"` → `mpm route --apply` emits a manual `<system-reminder>` with that persona's content → injected into SOUL.md.
- `mpm ops stance assume auto auto` → `persona: "auto"` → router picks per turn.
- Default (active.json blank) → `mpm route --apply` emits empty stdout → plugin is a no-op for that turn.

## Failure modes

All silent, fail-open:

- `mpm` missing from PATH → spawn errors, no injection.
- active.json missing/malformed → MPM handles internally; whatever it emits is injected.
- Subprocess timeout → child killed, partial stdout discarded.
- Stale reminder from prior turn → cleared when next `mpm route --apply` returns empty stdout.

## Manual override of the plugin itself

```bash
openclaw plugins disable mpm-auto-mode-persona-openclaw   # disable the plugin (config preserved)
openclaw plugins enable mpm-auto-mode-persona-openclaw    # re-enable
openclaw plugins uninstall mpm-auto-mode-persona-openclaw # remove entirely
```

## Uninstall

This plugin is part of the host-level MPM OpenClaw integration. To
remove BOTH this plugin and `mpm-memory-openclaw` atomically, use the
host-level uninstaller at
`agent_installation/uninstall-openclaw.sh`. Do NOT just
`openclaw plugins uninstall` this id by hand — the plugin entries
and any sibling plugin state are tied together and a partial
uninstall leaves a broken state. The host-level uninstaller is
idempotent and refuses to seize unrelated plugin installations.

## Post-alpha cleanup

- ~~Module-level `sessionReminders` Map → proper session-context plumbing~~ **Closed 2026-08-05.** Consume-and-clear in the `agent:bootstrap` hook bounds the Map at "concurrently processing turns" (typically ≤1 per session). Leak + stale-injection both closed. Proper per-session plumbing is still architecturally cleaner if/when sub-agents start sharing the cache.
- Configurable timeout and binary path (already supported via `configSchema`).
- Test suite (`index.test.js`) covering the public surface.