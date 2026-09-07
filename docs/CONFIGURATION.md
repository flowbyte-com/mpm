# MPM Configuration (v0.1)

`mpm_config.json` is the canonical runtime configuration file. It lives at
`$MPM_WORKSPACE/mpm_config.json` (default `$HOME/.mpm/mpm_config.json`).

## Mental model

Four concepts, in increasing specificity:

```
profiles      provider/model parameters (the model itself)
    ↓
components    substrate-component → profile bindings (which model does what)
    ↓
capabilities  human-facing vocabulary → component mappings
              (skills say "reviewer"; the substrate says "critic")
    ↓
legacy synth  pre-profiles one-shot block (still readable; never written)
```

`mpm config show` renders each layer.

## Profiles

A named (provider, model, parameters) tuple. The substrate routes every
substrate function through `ProfileFor(component)`.

```json
{
  "profiles": {
    "default": {
      "provider": "openai",
      "model": "gpt-4o",
      "base_url": "https://api.openai.com/v1",
      "api_key": "...",
      "max_tokens": 4096,
      "timeout_seconds": 300,
      "temperature": 0.7
    }
  }
}
```

`profiles["default"]` is the canonical fallback. Every component that has no
explicit binding (and is not a capability alias) resolves through `default`.

Naming convention: short lowercase names (`default`, `fast`, `critic`,
`local-ollama`, `embedding`).

## Components

Optional substrate-component → profile bindings. The substrate has three
named components in v0.1: `memory`, `critic`, `scheduler`. The embedding
subsystem is a separate binding key.

```json
{
  "components": {
    "embedding": "local-ollama",
    "critic":    "critic"
  }
}
```

Any component not bound here resolves through `profiles["default"]`. Setting
`components["embedding"] = "disabled"` opts out of embedding entirely.

## Capabilities

Optional capability → component bindings. Capability names are the
operator-meaningful vocabulary that skills/skills-shaped callers use.
Default v0.1 mappings (built-in, override with `mpm config capability set`):

| capability | default component |
|-----------|-------------------|
| `planner`   | `memory` |
| `reviewer`  | `critic` |
| `reflect`   | `critic` |
| `summarise` | `memory` |

Override an entry:

```bash
mpm config capability set reviewer memory
```

Remove an override:

```bash
mpm config capability set reviewer ""   # falls back to built-in default
```

Capability names that don't match any binding are passed through as
component names — direct component routing (`components=["memory","critic"]`)
continues to work unchanged.

## Legacy `synth`

Pre-profiles installs used a top-level `synth` block:

```json
{
  "synth": {
    "model": "...",
    "api_key": "...",
    "base_url": "..."
  }
}
```

This block is **still readable** (ProfileFor step 3 falls through to it as
a last-resort migration path), but **never written** by the wizard or any
new config command. Modern installs go through `profiles["default"]`.

When both `profiles["default"]` and the legacy `synth` block are present,
the canonical profile wins.

## API keys

API keys are persisted to `mpm_config.json` (file mode 0600). `mpm config show`
redacts keys; `mpm config get api_key` returns the full key for the operator's
own use.

For alpha, persisting keys in the config file is acceptable. Operators who
prefer not to persist secrets should use the environment variable fallback
(`MINIMAX_API_KEY`, `OPENAI_API_KEY`, `OPENROUTER_API_KEY`).

## Background synthesis

```json
{
  "synthesis_enabled": true
}
```

When `false`, `AutoSynthesize` short-circuits before any LLM call. Default
`true` (a missing field doesn't accidentally disable synthesis). Set with:

```bash
mpm config set synthesis_enabled false
```

## Common operations

| Goal | Command |
|---|---|
| Set up a default model | `mpm config` (interactive wizard) |
| Change the default model | `mpm config set model <model>` |
| Bind a different model to a component | `mpm config profile add <name>` then `mpm config component set <component> <profile>` |
| Configure embedding | `mpm config detect-embedding --apply local-ollama` |
| Override a capability mapping | `mpm config capability set <cap> <component>` |
| Validate | `mpm config validate` |
| Inspect | `mpm config show` |
