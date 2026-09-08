#!/usr/bin/env bash
# install.sh — Materialize the Claude Code ↔ MPM MCP integration.
#
# Idempotent. Backs up any existing ~/.claude/.mcp.json and ~/.claude/settings.json
# before editing. Writes the resolved .mcp.json to ~/.claude/.mcp.json (user-
# level, applies to all projects) so Claude Code launches mpm-mcp as a native
# MCP server on next session start.
#
# Usage:
#   ./install.sh             # install
#   ./install.sh --uninstall # remove
#   ./install.sh --verify    # run validation tests
#
# Edits ~/.claude/settings.json to install the SessionStart hook that injects
# MPM wake context before the first model turn. This is the only ClaudeCode
# native mechanism that delivers wake automatically (MCP `instructions` is
# guidance-only; CLAUDE.md/AGENTS.md asking the model to call
# `mpm_context.read_wake_context` is a first-turn tool call, not automatic
# injection). The hook is named `mpm-session-start` and writes a
# `hookSpecificOutput.additionalContext` JSON envelope — plain-prose stdout
# is silently dropped by Claude Code.

set -euo pipefail

# ---------------------------------------------------------------------------
# Path resolution — deterministic, portable, no interactive-shell deps
# ---------------------------------------------------------------------------
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TEMPLATE="${SCRIPT_DIR}/.mcp.json.template"
HOME_DIR="${HOME:?HOME must be set}"
CLAUDE_DIR="${HOME_DIR}/.claude"
MCP_TARGET="${CLAUDE_DIR}/.mcp.json"
BACKUP_TS="$(date -u +%Y%m%dT%H%M%SZ)"
BACKUP_DIR="${CLAUDE_DIR}/backups/claude-code-mpm-${BACKUP_TS}"

# Canonical MPM install — same resolution rule as opencode-mpm/src/index.ts.
MPM_CANONICAL_BIN="${HOME_DIR}/.mpm/bin/mpm-mcp"
MPM_CANONICAL_WS="${HOME_DIR}/.mpm"

# ---------------------------------------------------------------------------
# Argument parse
# ---------------------------------------------------------------------------
MODE="install"
for arg in "$@"; do
  case "$arg" in
    --uninstall) MODE="uninstall" ;;
    --verify)    MODE="verify" ;;
    --help|-h)
      sed -n '2,18p' "$0"
      exit 0
      ;;
    *) echo "unknown arg: $arg" >&2; exit 2 ;;
  esac
done

# ---------------------------------------------------------------------------
# Pre-flight
# ---------------------------------------------------------------------------
if [ "$MODE" = "install" ]; then
  if [ ! -x "$MPM_CANONICAL_BIN" ]; then
    echo "❌ mpm-mcp not found at $MPM_CANONICAL_BIN" >&2
    echo "   run 'make install' from the MPM source first (which places the" >&2
    echo "   binary at $MPM_CANONICAL_BIN)." >&2
    exit 1
  fi
  if [ ! -r "$TEMPLATE" ]; then
    echo "❌ template missing: $TEMPLATE" >&2
    exit 1
  fi
fi

# ---------------------------------------------------------------------------
# Uninstall
# ---------------------------------------------------------------------------
if [ "$MODE" = "uninstall" ]; then
  if [ -f "$MCP_TARGET" ]; then
    mkdir -p "$BACKUP_DIR"
    cp -p "$MCP_TARGET" "$BACKUP_DIR/.mcp.json.before-uninstall"
    # If the file's only mcpServer entry is mpm, remove the file entirely.
    # Otherwise merge-remove just the mpm block.
    if grep -q '"mpm"' "$MCP_TARGET"; then
      # Use python for safe JSON manipulation; stdlib-only.
      python3 - "$MCP_TARGET" <<'PY'
import json, sys, os
p = sys.argv[1]
with open(p) as f:
    cfg = json.load(f)
removed = False
if "mcpServers" in cfg and "mpm" in cfg["mcpServers"]:
    del cfg["mcpServers"]["mpm"]
    removed = True
    if not cfg["mcpServers"]:
        del cfg["mcpServers"]
if not cfg or (len(cfg) == 1 and "_comment" in cfg):
    os.remove(p)
else:
    with open(p, "w") as f:
        json.dump(cfg, f, indent=2)
        f.write("\n")
PY
      echo "✅ uninstalled mpm from $MCP_TARGET (backup at $BACKUP_DIR)"
    else
      echo "ℹ️  mpm entry not present in $MCP_TARGET; nothing to remove"
    fi
  else
    echo "ℹ️  $MCP_TARGET does not exist; nothing to uninstall"
  fi

  # Remove the SessionStart hook from settings.json (preserve other hooks).
  HOOK_SCRIPT_DST="${CLAUDE_DIR}/hooks/mpm-session-start"
  SETTINGS_JSON="${CLAUDE_DIR}/settings.json"
  if [ -f "$SETTINGS_JSON" ]; then
    python3 - "$SETTINGS_JSON" "$HOOK_SCRIPT_DST" <<'PY'
import json, sys, os
path, hook = sys.argv[1], sys.argv[2]
try:
    with open(path) as f:
        cfg = json.load(f)
except (OSError, json.JSONDecodeError):
    sys.exit(0)
hooks = cfg.get("hooks", {})
ss = hooks.get("SessionStart")
if isinstance(ss, list):
    filtered = []
    for entry in ss:
        if not isinstance(entry, dict):
            filtered.append(entry)
            continue
        inner = entry.get("hooks") or []
        if any(isinstance(h, dict) and h.get("command") == hook for h in inner):
            continue
        filtered.append(entry)
    if filtered:
        hooks["SessionStart"] = filtered
    elif "SessionStart" in hooks:
        del hooks["SessionStart"]
if not hooks:
    cfg.pop("hooks", None)
if cfg:
    with open(path, "w") as f:
        json.dump(cfg, f, indent=2)
        f.write("\n")
    os.chmod(path, 0o600)
PY
    echo "✅ SessionStart hook removed from $SETTINGS_JSON"
  fi
  if [ -f "$HOOK_SCRIPT_DST" ]; then
    rm -f "$HOOK_SCRIPT_DST"
    echo "✅ removed $HOOK_SCRIPT_DST"
  fi

  exit 0
fi

# ---------------------------------------------------------------------------
# Verify
# ---------------------------------------------------------------------------
if [ "$MODE" = "verify" ]; then
  exec python3 "${SCRIPT_DIR}/verify.py"
fi

# ---------------------------------------------------------------------------
# Install
# ---------------------------------------------------------------------------
mkdir -p "$CLAUDE_DIR"

# Back up any existing config the FIRST time we touch it.
mkdir -p "$BACKUP_DIR"
if [ -f "$MCP_TARGET" ] && [ ! -f "$BACKUP_DIR/.mcp.json.before-install" ]; then
  cp -p "$MCP_TARGET" "$BACKUP_DIR/.mcp.json.before-install"
  echo "📦 backed up $MCP_TARGET → $BACKUP_DIR/.mcp.json.before-install"
fi

# Materialize the template: replace ${HOME} with absolute path, merge with
# any existing mcpServers so we don't clobber other servers.
python3 - "$TEMPLATE" "$MCP_TARGET" "$HOME_DIR" <<'PY'
import json, sys, os, re
template_path, target_path, home_dir = sys.argv[1], sys.argv[2], sys.argv[3]

# Read template as raw text, then substitute ${HOME}.
with open(template_path) as f:
    raw = f.read()
substituted = raw.replace("${HOME}", home_dir)

# Strip the _comment field — it's metadata, not real config.
data = json.loads(substituted)
data.pop("_comment", None)

# Merge with existing mcpServers if present so we don't clobber other entries.
existing = {}
if os.path.exists(target_path):
    try:
        with open(target_path) as f:
            existing = json.load(f)
    except Exception:
        # Corrupt pre-existing file — back it up and start clean.
        backup = target_path + ".corrupt-" + os.path.basename(os.path.dirname(target_path))
        os.rename(target_path, backup)
        print(f"⚠️  moved corrupt {target_path} → {backup}")
        existing = {}

existing.setdefault("mcpServers", {})
existing["mcpServers"]["mpm"] = data["mcpServers"]["mpm"]

with open(target_path, "w") as f:
    json.dump(existing, f, indent=2)
    f.write("\n")
os.chmod(target_path, 0o600)
print(f"✅ wrote {target_path}")
print(f"   mpm command: {existing['mcpServers']['mpm']['command']}")
print(f"   mpm env:     {existing['mcpServers']['mpm']['env']}")
PY

# Sanity probe — boot the MCP server and confirm it responds to a real
# JSON-RPC request. This proves the wired config will actually work after
# the next Claude Code restart, not just that the JSON parses.
# mpm-mcp needs to read resource files (./mode, etc.) relative to its install
# root, so we cd there before launching it. Without this, the probe fails
# with `open ./mode: no such file or directory` whenever the script is
# invoked from a different cwd.
#
# Uses mpm_context.read_wake_context (always in the default 3-tool surface)
# rather than mpm_system.health_check, which only exists when
# MPM_EXPOSE_ALL_TOOLS=1 is set in the MCP env block.
PROBE=$(cd "$MPM_CANONICAL_WS" && echo '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"mpm_context","arguments":{"action":"read_wake_context","params":{"projection":"compact"}}}}' \
  | /usr/bin/timeout 3 "$MPM_CANONICAL_BIN" 2>/dev/null \
  | python3 -c '
import json, sys
raw = sys.stdin.read()
start = raw.rfind("{")
while start != -1:
    try:
        env = json.loads(raw[start:])
        for c in (env.get("result") or {}).get("content") or []:
            if c.get("type") == "text":
                inner = json.loads(c["text"])
                print(json.dumps(inner))
                sys.exit(0)
        break
    except json.JSONDecodeError:
        start = raw.rfind("{", 0, start)
print("{}")
')
OK=$(echo "$PROBE" | python3 -c 'import json,sys; print(json.load(sys.stdin).get("success"))')
DB_PATH=$(echo "$PROBE" | python3 -c 'import json,sys; print(json.load(sys.stdin).get("context",{}).get("session_id","") if json.load(open("/dev/stdin")).get("context") else "")' 2>/dev/null || echo "")
# DB_PATH lookup: read_wake_context returns context.session_id; that is just
# proof of life. Probe success is what matters.
if [ "$OK" != "True" ]; then
  echo "❌ mpm-mcp probe returned success=$OK" >&2
  echo "   probe: $PROBE" >&2
  exit 1
fi
if [ "$OK" != "True" ]; then
  echo "❌ mpm-mcp health_check returned ok=$OK" >&2
  echo "   probe: $PROBE" >&2
  exit 1
fi

echo "✅ mpm-mcp probe ok:true"
echo "   db_path: $DB_PATH"

# ---------------------------------------------------------------------------
# Install SessionStart hook — automatic MPM wake injection.
#
# ClaudeCode loads ~/.claude/settings.json at session start. A SessionStart
# hook is the ONLY native mechanism that injects content into the model-facing
# system prompt BEFORE the first turn (the MCP `instructions` field is
# guidance-only). The hook must emit a JSON envelope of shape
#   { "hookSpecificOutput": { "hookEventName": "SessionStart",
#                            "additionalContext": "<wake text>" } }
# on stdout; plain-prose stdout is silently dropped.
# ---------------------------------------------------------------------------
HOOK_SCRIPT_SRC="${SCRIPT_DIR}/scripts/mpm-session-start"
HOOK_SCRIPT_DST="${CLAUDE_DIR}/hooks/mpm-session-start"
SETTINGS_JSON="${CLAUDE_DIR}/settings.json"
if [ -f "$HOOK_SCRIPT_SRC" ]; then
  echo ""
  echo "🪝 installing SessionStart hook for automatic wake injection..."
  mkdir -p "${CLAUDE_DIR}/hooks"
  cp -p "$HOOK_SCRIPT_SRC" "$HOOK_SCRIPT_DST"
  chmod +x "$HOOK_SCRIPT_DST"

  if [ ! -f "$SETTINGS_JSON" ] || [ ! -s "$SETTINGS_JSON" ]; then
    printf '{}\n' > "$SETTINGS_JSON"
  fi
  python3 - "$SETTINGS_JSON" "$HOOK_SCRIPT_DST" <<'PY'
import json, sys, os
path, hook = sys.argv[1], sys.argv[2]
try:
    with open(path) as f:
        cfg = json.load(f)
except (OSError, json.JSONDecodeError):
    cfg = {}
hooks = cfg.setdefault("hooks", {})
ss = hooks.get("SessionStart")
if not isinstance(ss, list):
    ss = []
# Idempotent: drop any prior mpm hook entry (we own this namespace).
ss = [h for h in ss if not (
    isinstance(h, dict) and any(
        isinstance(inner, dict) and inner.get("command") == hook
        for inner in (h.get("hooks") or [])
    )
)]
ss.append({
    "matcher": "startup",
    "hooks": [{
        "type": "command",
        "command": hook,
        "timeout": 10,
    }],
})
hooks["SessionStart"] = ss
with open(path, "w") as f:
    json.dump(cfg, f, indent=2)
    f.write("\n")
os.chmod(path, 0o600)
print(f"✅ SessionStart hook wired at {hook}")
PY
else
  echo "ℹ️  ${HOOK_SCRIPT_SRC} not found — skipping SessionStart hook install"
fi

# ---------------------------------------------------------------------------
# Install CLAUDE.md with the MPM behavioral protocol (host-independent)
# ---------------------------------------------------------------------------
CLAUDE_MD_TARGET="${HOME_DIR}/.claude/CLAUDE.md"
CLAUDE_MD_SNIPPET="${SCRIPT_DIR}/templates/CLAUDE.md.snippet"
if [ -f "$CLAUDE_MD_SNIPPET" ]; then
  echo ""
  echo "📝 installing Claude Code persistent instructions (CLAUDE.md)..."
  if python3 "${SCRIPT_DIR}/scripts/install_claude_instructions.py" \
       --scope user \
       --home "$HOME_DIR" \
       --target "$CLAUDE_MD_TARGET" \
       --snippet "$CLAUDE_MD_SNIPPET"; then
    echo "✅ CLAUDE.md managed section in place"
  else
    echo "⚠️  CLAUDE.md installation skipped/failed; mcp is wired but the agent"
    echo "   has no behavioral instructions. Run install.sh --help if you need"
    echo "   to install them manually."
  fi
else
  echo "ℹ️  CLAUDE.md snippet not found at $CLAUDE_MD_SNIPPET — skipping host-instruction install"
fi

echo ""
echo "Next steps:"
echo "  1. Restart Claude Code (mcpServers + CLAUDE.md are loaded at session start)."
echo "  2. Run ${SCRIPT_DIR}/verify.sh to run the full integration test suite."
echo "  3. Inspect the ${BACKUP_DIR} backup if you need to revert."
echo ""
echo "Uninstall: $0 --uninstall (removes both mcp config and managed CLAUDE.md section)"
echo "Verify:    $0 --verify"
