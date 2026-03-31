# Quick Start Guide

> 🚀 **New to MPM?** This guide walks you through installation, verification, and basic operations.

## Prerequisites

| Requirement | Minimum | Recommended |
|-------------|---------|-------------|
| Operating System | Linux, macOS, Windows (with Go) | Linux x86_64 |
| Go Version | 1.18+ | Latest stable |
| OpenClaw | 0.1+ | Latest stable |
| Disk Space | 50 MB | 500 MB (for documents) |

> **Note:** MPM is a pure Go binary with no external dependencies. No Python, no database servers, no external services.

---

## Installation

MPM is included in your OpenClaw workspace at:
```
/home/node/.openclaw/workspace/projects/mpm/
```

### 1. Add to PATH

Add this to your `~/.bashrc` or `~/.zshrc`:

```bash
# MPM - Memory-Persona-Mode Manager
export PATH="/home/node/.openclaw/workspace/projects/mpm:$PATH"

# OpenClaw workspace (required for correct path detection)
export MPM_WORKSPACE="/home/node/.openclaw/workspace"
```

### 2. Reload Configuration

```bash
source ~/.bashrc  # or source ~/.zshrc
```

### 3. Verify Installation

> ✅ **Expected output on success:**

```bash
$ mpm config show
┌─────────────────────────────────────────────────────────────┐
│ MPM Configuration                                          │
├─────────────────────────────────────────────────────────────┤
│  Workspace:     /home/node/.openclaw/workspace             │
│  Database:      /home/node/.openclaw/workspace/projects/mpm/src/db/mpm_memory.db
│  Sessions:      /home/node/.openclaw/agents/main/sessions  │
│  Memory:        /home/node/.openclaw/workspace/memory      │
└─────────────────────────────────────────────────────────────┘

$ mpm status
┌─────────────────────────────────────────────────────────────┐
│ MPM Status                                                  │
├─────────────────────────────────────────────────────────────┤
│  Version:     6.0.0                                        │
│  Database:    OK                                           │
│  Memories:    0                                            │
│  Sessions:    0                                            │
│  Topics:      0                                            │
│  Personas:   default (active)                              │
│  Modes:       none                                          │
└─────────────────────────────────────────────────────────────┘
```

---

## Basic Usage

### Personas

Personas define **who** the assistant is:

```bash
mpm persona list           # List all personas
mpm persona set <name>     # Set active persona
mpm persona active         # Show current persona
```

| Command | What it does | Example |
|---------|--------------|---------|
| `mpm persona list` | Show all personas | `default`, `dev`, `support` |
| `mpm persona set <name>` | Switch persona | `mpm persona set dev` |
| `mpm persona active` | Show current | `default` |

### Modes

Modes define **how** the assistant behaves:

```bash
mpm mode list              # List all modes
mpm mode add <name>        # Add mode to stack
mpm mode active            # Show active modes
```

| Command | What it does | Example |
|---------|--------------|---------|
| `mpm mode list` | Show all modes | `debug`, `concise`, `technical` |
| `mpm mode add <name>` | Enable mode | `mpm mode add debug` |
| `mpm mode active` | Show active | `debug + concise` |

### Quick Shortcuts

```bash
~p.<name>    # Quick persona switch (e.g., ~p.default)
~m+<name>    # Quick mode add (e.g., ~m+debug)
~m.clr       # Clear all modes
```

### Memory Operations

```bash
mpm memory list            # List memories
mpm memory search <query>  # Search with FTS5 highlighting
mpm memory add "content"   # Add new memory
mpm memory stats           # Show statistics
```

**Example search output:**
```bash
$ mpm memory search "docker"
┌─────────────────────────────────────────────────────────────┐
│ docker containers                                          │
├─────────────────────────────────────────────────────────────┤
│ Learn about <<docker>> <<containers>> for...               │
└─────────────────────────────────────────────────────────────┘
```

### Reference Library

```bash
mpm reference add <path>   # Add document (PDF, EPUB, MD, etc.)
mpm reference list         # List all references
mpm reference search <query> # Search references
mpm reference get <id>     # Retrieve full document
```

### Sessions

```bash
mpm ss                     # Save current session (shortcut)
mpm session list           # List all sessions
mpm session search <query> # Search sessions with FTS5
```

### Topics

```bash
mpm topic list             # List all topics
mpm topic add              # Create new topic (interactive)
mpm topic search <query>   # Search topics
mpm topic members <id>     # List topic members
```

---

## Troubleshooting

### Common Issues

| Problem | Cause | Solution |
|---------|-------|---------|
| `command not found: mpm` | PATH not set | Add MPM to PATH (see Installation step 1) |
| `Wrong workspace` | MPM_WORKSPACE not set | `export MPM_WORKSPACE="/home/node/.openclaw/workspace"` |
| `Database not found` | First run, no DB yet | Run `mpm status` to initialize |
| `Permission denied` | File permissions | `chmod 755 /home/node/.openclaw/workspace/projects/mpm/mpm` |
| `FTS5 not available` | SQLite version | MPM uses built-in FTS5; update SQLite if issue persists |

### Diagnostic Commands

```bash
# Check PATH
echo $PATH | grep mpm

# Check workspace
mpm config show

# Verify database
ls -la ~/.openclaw/workspace/projects/mpm/src/db/

# Test search
mpm memory search "test"
```

---

## Next Steps

Ready to dive deeper? Here's where to go:

| Goal | Document |
|------|----------|
| Learn all commands | [CLI Commands](commands/) |
| Understand FTS5 search | [FTS5 Search](advanced/ft5-search.md) |
| Permanent data deletion | [Shred Protocol](advanced/shred-protocol.md) |
| Topic lifecycle | [Topic System](advanced/topic-system.md) |
| PDF/EPUB ingestion | [Native Ingestion](advanced/native-ingestion.md) |
| Security details | [Security Model](security/security.md) |

---

## Onboarding Checklist

> ☐ Add MPM to PATH  
> ☐ Source shell config  
> ☐ Run `mpm config show`  
> ☐ Run `mpm status`  
> ☐ Set active persona (`mpm persona set default`)  
> ☐ Test memory add (`mpm memory add "Hello MPM"`)  
> ☐ Test memory search (`mpm memory search "Hello"`)  

---

**Last Updated:** 2026-03-29  
**MPM Version:** 6.0.0
