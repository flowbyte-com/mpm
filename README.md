# MPM

MPM (Managed Persistent Memory) is an observable substrate for long-lived
autonomous systems. It preserves what an agent knows, what it is trying to
accomplish, why it made a decision, and what actually happened across sessions.

> **Technical specification:** See [docs/SPEC.md](docs/SPEC.md) for MPM's
> architecture, behavioural contracts, cognitive model, runtime semantics,
> reliability guarantees, and implementation reference.

## Why MPM?

An agent can remember a conclusion and still lose the reasoning that produced
it. MPM gives memories, decisions, theories, evidence, work, lessons, and skills
distinct lifecycles so future sessions can continue an investigation instead of
starting over.

- **Reasoning with history:** decisions retain their rationale; theories carry
  validation criteria; evidence can support or challenge a belief.
- **Continuity across sessions:** wake context surfaces relevant durable state,
  open work, and the previous handoff. Work completion and session closure are
  separate events.
- **Traceable changes:** provenance and activity records make it possible to
  inspect who produced an artifact and what changed. Invalidated foundations
  surface dependent decisions and theories for re-evaluation.
- **Local persistence:** one SQLite database with FTS5 holds the cognitive
  substrate. Semantic recall can use a local Ollama embedding provider; no
  separate vector database or remote storage service is required.
- **Shared agent access:** the operator CLI, JSON tool calls, and stdio MCP
  server use the same substrate and tool registry.

MPM is a persistence and reasoning layer. Agent runtimes and orchestration
systems integrate with it to retain intellectual progress.

## How it fits

```text
Operators                     Agent hosts
    │                         │         │
    ▼                         ▼         ▼
mpm CLI                  mpm call    mpm-mcp (stdio)
    └─────────────────────────┴─────────┘
                              │
                    Shared core and tools
                              │
                      SQLite substrate
                              ▲
                   Scheduler and critic
```

Five binaries ship together: `mpm`, `mpm-mcp`, `mpm-scheduler`, `mpm-critic`,
and `mpm-telemetry`. The telemetry sidecar records LLM invocation economics in
its own `telemetry.db`, separate from the cognitive substrate.

## Try it

Building requires Go 1.26.6 or newer, Make, and a C toolchain for SQLite/CGO.
The Makefile supplies the required FTS5 build flags.

```bash
git clone https://github.com/flowbyte-com/mpm ~/projects/mpm
cd ~/projects/mpm
make build
./bin/mpm --help
```

For the full user-space install on Linux with systemd:

```bash
./install.sh
mpm ops init directives
mpm doctor
```

Follow [Agent Integration Installation](agent_installation/INSTALL.md) for
prerequisites, host setup, verification, updates, and recovery. It is the
installation and integration authority for supported agent hosts. The
[MPM runtime installation reference](docs/INSTALL.md) covers runtime-specific
configuration and services.

Once installed, inspect the substrate and restore session context:

```bash
mpm status
mpm wake --compact
```

The [specification's Quick Start](docs/SPEC.md#5-quick-start) contains the
CLI-only path, manual service setup, and first cognitive commands.

## Documentation

| Start here | Purpose |
|---|---|
| [Technical specification](docs/SPEC.md) | Canonical architecture, cognitive model, behavioural contracts, CLI reference, and reliability details |
| [Agent installation](agent_installation/INSTALL.md) | Install and verify an integration with your agent host |
| [Supported integrations](agent_installation/README.md) | Host overview and links to adapter-specific documentation |
| [Agent protocol](agent_installation/mpm-agent-protocol.md) | Wake, persistence, skill discovery, handoff, and recovery contract |
| [Contributing](docs/CONTRIBUTING.md) | Repository invariants, testing gates, and contribution expectations |
| [Security policy](docs/SECURITY.md) | Private vulnerability reporting and credential-handling rules |

## Development

```bash
make build
make test-race
```

`make test-race` is the canonical pre-merge validation gate and includes the
required FTS5 flags. Changes to detailed product behaviour belong in
[docs/SPEC.md](docs/SPEC.md); this README stays focused on orientation.

## License

[AGPL-3.0](LICENSE), including its network-use clause. MPM is free software
and may be used commercially under those terms.
