# MPM Plugin for Hermes Agent

A Hermes Agent tool plugin that exposes [MPM (Memory Protocol Manager)](https://github.com/v4alpha/mpm) as native agent tools. 19 tools across memory, lessons, topics, references, session context, recall hints, and epistemology.

## Prerequisites

- **MPM binary** built and available in `PATH` (or set `MPM_BINARY` env var)
- **Hermes Agent** v0.15+ installed
- **Python 3.11+** (Hermes's bundled Python)

## Quick Install

### 1. Set up the compatibility shim

Hermes Agent's editable install does not expose a top-level `hermes_agent` package — the source root is `tools/`. The plugin needs `from hermes_agent.tools import registry` to work. Create a shim:

```bash
# Find your hermes-agent install
HERMES_ROOT=$(python -c "import HermesAgent; print(HermesAgent.__path__[0])" 2>/dev/null || echo "$HOME/.hermes/hermes-agent")
echo "Hermes root: $HERMES_ROOT"
```

Create the shim directories and file:

```bash
mkdir -p "$HERMES_ROOT/hermes_agent/tools"

# hermes_agent/__init__.py — main namespace shim
cat > "$HERMES_ROOT/hermes_agent/__init__.py" << 'EOF'
"""Compatibility shim: provide a hermes_agent namespace backed by tools.registry."""
import sys as _sys
from pathlib import Path as _Path

# Point hermes_agent at the actual hermes-agent source root
_actual_root = _Path(__file__).parent.parent.parent  # hermes-agent/
_tools_src = _actual_root / "tools"

if str(_tools_src) not in _sys.path:
    _sys.path.insert(0, str(_actual_root))

# Backwards-compat re-exports from tools.registry
from tools.registry import registry, tool_error, tool_result, ToolEntry

class Tool:
    """Minimal Tool base for compatibility — not used by the functional register() API."""
    name: str = ""
    description: str = ""

__all__ = ["registry", "tool_error", "tool_result", "ToolEntry", "Tool"]
EOF

# hermes_agent/tools/__init__.py — tools sub-package shim
cat > "$HERMES_ROOT/hermes_agent/tools/__init__.py" << 'EOF'
"""Compatibility shim for hermes_agent.tools namespace."""
from tools.registry import registry, tool_error, tool_result, ToolEntry

__all__ = ["registry", "tool_error", "tool_result", "ToolEntry"]
EOF
```

### 2. Install the MPM plugin

Symlink the plugin into Hermes's tool discovery directory:

```bash
HERMES_ROOT=${HERMES_ROOT:-~/.hermes/hermes-agent}
PLUGIN_SRC=/home/v/workspace/projects/mpm/agent-plugins/hermes-mpm-plugin  # ← adjust to your path

ln -sf "$PLUGIN_SRC" "$HERMES_ROOT/tools/mpm_plugin"
```

### 3. Configure environment

Set required env vars before starting Hermes Agent:

```bash
export MPM_WORKSPACE=/home/v/workspace/projects/mpm   # your MPM data dir
export MPM_BINARY=$MPM_WORKSPACE/bin/mpm              # or leave unset if mpm is in PATH
```

### 4. Verify

```bash
hermes tools list
```

You should see `mpm` listed with 19 tools:

```
mpm
  🧠 query_long_term_memory   Search MPM long-term memory
  💾 save_to_memory          Persist a fact/lesson/decision to MPM
  ⚔️ challenge_memory        Challenge an existing memory with evidence
  📚 save_lesson              Persist a lesson to MPM
  🔍 search_lessons           Search MPM lessons
  📋 list_lessons             List all lessons
  🏷️ create_topic             Create a topic
  🔍 search_topics            Search topics
  🔗 link_topic               Link a memory to a topic
  📄 add_reference            Ingest a document
  🔍 search_references        Search references
  📚 list_references          List all references
  🌅 read_wake_context        Read session wake context
  📜 read_directives          Read prime directives
  💡 proactive_recall_hint    Check conversation for decision/theory matches
  ⚖️ record_decision           Record an architectural decision
  🧪 propose_theory            Log a hypothesis
  🔬 resolve_theory            Close the loop on a pending theory
```

## Troubleshooting

### `Import "hermes_agent.tools" could not be resolved`

The shim wasn't created correctly, or Hermes Agent restarted with a different Python environment. Re-run step 1 and restart Hermes Agent.

### `ModuleNotFoundError: No module named 'tools'`

The shim needs to add the hermes-agent root to `sys.path`. The shim created in step 1 handles this — verify it exists at `$HERMES_ROOT/hermes_agent/__init__.py`.

### `mpm` toolset not appearing in `hermes tools list`

- Check that the symlink was created: `ls -la ~/.hermes/hermes-agent/tools/`
- The plugin imports at tool discovery time — any Python error during import will silently skip the plugin. Run with verbose logging or check Hermes Agent startup output.
- Make sure `MPM_WORKSPACE` is set before starting Hermes Agent.

### MPM commands time out or fail silently

- Verify `mpm --version` works in your terminal
- Check `MPM_WORKSPACE` is set to the directory containing your MPM data
- Run `mpm recall --json -- "test" 1` manually to confirm the binary is working

## Architecture Notes

- The plugin uses **functional** `registry.register()` calls, not class decorators. Hermes Agent's tool registry expects `registry.register(name, toolset, schema, handler)`.
- Each tool module (`memory_tools.py`, `lesson_tools.py`, etc.) calls `registry.register()` at import time. The `__init__.py` imports all modules to trigger registration.
- Tool handlers (`_handle_*` functions) receive `(args, **kw)` and call `run_mpm()` in `base.py`, which wraps `subprocess.run()` with JSON I/O.
- The OpenClaw version of this plugin (in `agent-plugins/openclaw-mpm-plugin/mpm-plugin/`) uses TypeScript class decorators — a different SDK. The Hermes Agent version here is a separate, compatible implementation.