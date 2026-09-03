# Embedding Configuration

The MPM embedding subsystem resolves configuration from
`mpm_config.json`'s `components["embedding"]` binding. The reserved
sentinel `"disabled"` opts out cleanly. The legacy env-var fallback
(`OLLAMA_ENDPOINT` / `OLLAMA_MODEL`) is preserved for backward
compatibility.

## The four states

| State | Meaning | Action |
|---|---|---|
| `configured` | A profile is bound and the provider is reachable | None |
| `unavailable` | A profile is bound but the provider is unreachable | Fix the provider |
| `misconfigured` | A profile is bound but is invalid (missing fields) | Fix the profile |
| `disabled` | Operator chose no embeddings | None |
| `absent` | No profile, no env fallback | Configure one |

## Canonical precedence

See [spec §4.1](../superpowers/specs/2026-09-03-mpm-embedding-provider-design.md#41-configuration-precedence) for the full resolution table.

1. `components.embedding == "disabled"` → `disabled`
2. `components.embedding == "<profile>"` → use the profile
3. `components.embedding` absent → check `OLLAMA_ENDPOINT` / `OLLAMA_MODEL`
4. Neither → NullProvider (no embeddings)

## Configuring Ollama

```json
{
  "profiles": {
    "local-ollama": {
      "provider": "ollama",
      "model": "nomic-embed-text",
      "base_url": "http://localhost:11434"
    }
  },
  "components": {
    "embedding": "local-ollama"
  }
}
```

## Disabling embeddings

```json
{ "components": { "embedding": "disabled" } }
```

## Migrating from env-only setups

Run `mpm config detect-embedding --apply <name>` to discover what's
reachable and write a profile + component binding.

## Running `mpm ops migrate-embeddings`

Classifies legacy HashEmbed rows, marks synthetic theories,
provenance-gates un-challenge of the 194 affected memories,
writes an audit trail, and creates a pre-migration backup.
Idempotent. Reversible via `--undo <timestamp>`.

## Env-var fallback (legacy)

When `components.embedding` is absent, MPM falls back to reading
`OLLAMA_ENDPOINT` and `OLLAMA_MODEL` from the environment. This path
is retained for operators who set these variables before the
profile-based system existed.

**Operators should migrate to the profile-based workflow.** The env
fallback will be removed in a future release. Use `mpm config
detect-embedding --apply <name>` to migrate.
