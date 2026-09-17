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
# From this directory (dev / alpha-MV):
openclaw plugins install ./mpm-auto-mode-persona-openclaw --link

# From a published tarball (later):
openclaw plugins install npm-pack:./mpm-auto-mode-persona-openclaw-0.1.0.tgz

# Verify:
openclaw plugins inspect mpm-auto-mode-persona-openclaw --runtime --json
```

Requires MPM installed at the canonical location `$HOME/.mpm/bin/mpm` (alpha default), or reachable on PATH (or set the absolute path via `config.mpmBin` — see [Configuration](#configuration)). The auto-switch is silently a no-op when `mpm` cannot be resolved.

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

## Post-alpha cleanup

- ~~Module-level `sessionReminders` Map → proper session-context plumbing~~ **Closed 2026-08-05.** Consume-and-clear in the `agent:bootstrap` hook bounds the Map at "concurrently processing turns" (typically ≤1 per session). Leak + stale-injection both closed. Proper per-session plumbing is still architecturally cleaner if/when sub-agents start sharing the cache.
- Configurable timeout and binary path (already supported via `configSchema`).
- Test suite (`index.test.js`) covering the public surface.