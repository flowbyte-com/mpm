# Provenance Environment Variables

## Two channels, one canonical var

| Channel | Reader | Precedence | Default |
|---|---|---|---|
| Artifact write (memory/scan) | `internal/core/provenance.go::readFromEnv` | JSON > `MPM_PROVENANCE_FRAMEWORK` > `MPM_FRAMEWORK` | unset |
| Artifact write (call handler) | `cmd/mpm/call.go:116-122` | `MPM_PROVENANCE_FRAMEWORK` > `MPM_FRAMEWORK` | `"mpm-cli"` |
| Active context (sessions/modes) | `internal/core/mpmcli/mpmcli.go::ActiveContextFromEnv` | `MPM_PROVENANCE_FRAMEWORK` > `MPM_FRAMEWORK` (after M-2 fix) | `"mcp"` |

## What `MPM_FRAMEWORK` is

`MPM_FRAMEWORK` is the **legacy** framework-name env var used by all five `agent_installation` entries (claude-code-mpm, openclaw-mpm-memory, hermes-mpm, opencode-mpm, pi-mpm) via their MCP `.mcp.json` / `config.yaml` / `resolve_exec_env` hooks. It predates the `MPM_PROVENANCE_*` family and remains the supported fallback.

## Migration

- New integrations SHOULD set `MPM_PROVENANCE_FRAMEWORK`.
- Existing integrations keep using `MPM_FRAMEWORK`; no forced migration.
- The artifact-write channel (`provenance.go`) has honored both since 2026-08.
- The active-context channel (`mpmcli.go`) was patched in this remediation pass.

## Why two channels

The artifact-write channel records which model/framework produced a memory (audit trail). The active-context channel records which session/mode/persona is currently active (handoff/routing). They are different concerns; merging them would lose the active-session semantics.

## Audit trail

- M-2 (post-M3, 2026-08-31): "Dual provenance systems — Five readers with inconsistent precedence. `mpmcli.ActiveContextFromEnv` reads ONLY `MPM_FRAMEWORK` and NEVER consults `MPM_PROVENANCE_FRAMEWORK`; defaults to `'mcp'`. `cmd/mpm/call.go` and `provenance.go` read canonical first. Add fallback chain to `mpmcli.go`."
- Fix: `internal/core/mpmcli/mpmcli.go::ActiveContextFromEnv` now reads `MPM_PROVENANCE_FRAMEWORK` first, falls back to `MPM_FRAMEWORK`, then defaults to `"mcp"`.
- Test: `internal/core/mpmcli/mpmcli_test.go::TestActiveContextFromEnv_FrameworkPrecedence` (4 subtests) pins the precedence matrix.
