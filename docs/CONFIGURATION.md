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

### `timeout_seconds` (advanced)

`timeout_seconds` is how long the substrate waits for a **model request** —
the call that generates text. It is an advanced setting, not an onboarding
decision: the interactive wizard does not ask for it, and a freshly-written
profile carries the substrate default.

```json
{
  "profiles": {
    "default": {
      "provider": "openai",
      "model": "gpt-4o",
      "timeout_seconds": 300
    }
  }
}
```

**Default:** `300` seconds. The default lives in one place —
`config.DefaultTimeoutSeconds` in `internal/core/config/timeout.go` — and
every reader (the wizard, the generated configuration, the synthesis client,
the model factory) resolves the effective value through the same policy
(`config.EffectiveTimeoutSeconds`). The CLI cannot disagree with the runtime:
`mpm config get timeout_seconds` reports the **effective** value, not the raw
value in the file.

**Validation policy.** Only a positive value is valid:

| Configured value | Effective timeout | Notes |
|---|---|---|
| `300` (or any positive) | that value | used exactly as configured |
| absent / `0` | `300` | `timeout_seconds` is `omitempty`, so absent and `0` are indistinguishable once parsed |
| negative (e.g. `-5`) | `300` | **invalid** — see below |

A negative value is invalid rather than honoured because Go's `http.Client`
treats a non-positive `Timeout` as **unlimited**. Passing `-5` through
unchanged would silently turn a bounded request into an unbounded one instead
of producing the error the operator expected.

The entry points reject a non-positive value at the point of entry
(`mpm config set timeout_seconds <value>` and `mpm config profile set <name>
timeout_seconds <value>`), so the fallback above only ever applies to a
hand-edited `mpm_config.json`.

**Upgrades preserve explicit values.** Re-running the wizard — or upgrading
MPM — never silently replaces a timeout the operator already configured. An
existing valid value is kept verbatim; an existing invalid value is replaced
by the default **and the replacement is reported**.

**Connection verification is a different timeout.** `timeout_seconds` bounds a
*model request*. The probe that verifies a provider's credentials uses its own
bound, because a verification is a tiny connectivity question, not a
generation. The two never share a value or a code path: a provider that does
not answer in time is reported as a **timeout**, never as an invalid
credential. A slow provider and a wrong credential are different faults and
are reported differently.

The verification bound is **15 seconds**, and it is ONE number for every
verification surface (`probe.ProbeTimeoutForVerification` in
`cmd/mpm/probe/generative.go`). It was previously two: 12 seconds for
`mpm doctor` and **1.5 seconds** for the post-save verification. The 1.5s
bound was shorter than the provider's own latency, so a **valid** credential
was reported as a timeout — and on a credential surface, a timeout is what an
operator reads as "my key is wrong". Measured against the live MiniMax
Anthropic-compatible surface through the canonical probe path:

| model | min | median | max |
|---|---|---|---|
| `MiniMax-M3` | 415 ms | 560–700 ms | **4761 ms** |
| `MiniMax-M2.7` | 444 ms | 603–1007 ms | 2220 ms |

The slowest observed call was 4761 ms — **more than three times** the old
1.5s bound. The tail is what matters: the median is well under a second, but
a minority of calls take three to five seconds. A bound sized to the median
would fail half the time.

The bound is now 15 seconds: 3.1× the worst observed call, and still
bounded — an endpoint that does not answer is reported as a timeout after 15
seconds, never waited on without a deadline. The cost of the generous bound
is bounded too: an endpoint that *refuses* the connection fails immediately,
so the bound is only spent on an endpoint that accepts the connection and
then stays silent — exactly the case where patience is warranted.

The whole verification is additionally bounded by `probe.ProbeTotalBudget`
(40 seconds), which is the ceiling across the first attempt and the one
bounded recovery attempt a verification may make. That budget must exceed
the per-request bound, and it is the **outer** deadline: if it were shorter,
the outer deadline would fire first and the probe would report a bare context
deadline instead of the provider's own client timeout.

`timeout_seconds` never influences a verification, in either direction.

### `api_key` (credential)

A profile's `api_key` is a **literal secret**. It is presented to the provider
exactly as written. It is not a template, and it is never expanded.

A value shaped like an environment variable — `"$NAME"` or `"${NAME}"` — is
the one exception, and it is resolved as a **reference**:

| Configured value | Meaning |
|---|---|
| `""` (absent) | the endpoint's own credential variable supplies the credential — nothing is stored on disk |
| `"sk-live-abc"` | a literal secret; presented verbatim |
| `"$MINIMAX_API_KEY"` | a **reference** to that environment variable; resolved from the environment |

The reference case exists because the distinction was invisible and expensive.
A profile carrying the literal string `"$MINIMAX_API_KEY"` used to send that
literal to the provider as a bearer token, while the real credential sat
unused in the environment. The provider refused the placeholder, and the
operator was told their credential was invalid — it was not invalid, it was
**shadowed** by a placeholder that looked like a reference and was treated as
a secret. They would rotate a working key and the configuration would keep
the placeholder.

**The expansion is closed.** A reference resolves only when it names a
credential variable **the endpoint itself declares** (`synth.APIKeyEnvVars` —
`MINIMAX_API_KEY` / `ANTHROPIC_API_KEY` for an Anthropic-protocol endpoint,
`OPENAI_API_KEY` for `api.openai.com`, `OAI_COMPAT_API_KEY` for a generic
OpenAI-compatible endpoint, and so on). A reference to any other variable is
**refused**: it is not expanded, and the placeholder is never sent to a
provider.

This is deliberately *not* arbitrary environment expansion. Expanding
`"$ANYTHING"` would let a configuration file name any environment variable
and cause its value to be sent to a provider — a new exfiltration path for
anything able to write the configuration. The closed expansion grants no read
access the substrate did not already have: the resolved variable is exactly
the one the environment fallback would have used anyway.

When a reference cannot be resolved, the failure says which case it is:

- the endpoint declares the variable but it is **not set** — the error names
  the variable to export;
- the endpoint does **not declare** it — the error lists the variables it
  does declare.

An unresolved reference never reaches the provider, so it can never be
reported as an invalid credential.

**No secret appears in a message.** An error may name a variable the
substrate declares; it never echoes the configured value, the resolved
value, or a name the operator typed (a pasted secret can have the shape of a
variable name).

**Onboarding says so.** Every prompt that asks for a credential prints what
the answers mean before it asks: an empty answer uses the endpoint's declared
environment variable, a typed value is stored as a literal secret and is
never expanded, and the variable's name is a reference rather than a secret.

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
| Set the model request timeout (advanced) | `mpm config profile set default timeout_seconds 300` |
| Bind a different model to a component | `mpm config profile add <name>` then `mpm config component set <component> <profile>` |
| Configure embedding | `mpm config detect-embedding --apply local-ollama` |
| Override a capability mapping | `mpm config capability set <cap> <component>` |
| Validate | `mpm config validate` |
| Inspect | `mpm config show` |
