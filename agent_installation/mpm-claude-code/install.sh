#!/usr/bin/env bash
# install.sh — Materialize the Claude Code ↔ MPM MCP integration.
#
# Idempotent. Registers mpm-mcp as a USER-SCOPE MCP server (applies to every
# project) through Claude Code's own CLI (`claude mcp add --scope user`), and
# edits ~/.claude/settings.json to install the SessionStart hook.
#
# Registration goes through the CLI rather than a hand-written config file on
# purpose. This adapter previously wrote ~/.claude/.mcp.json, which is not a
# Claude Code discovery path in any scope — the config landed on disk and was
# silently never read. Letting the CLI own the file means scope resolution,
# validation, and backing up are the host's problem, not ours.
#
# Usage:
#   ./install.sh                   # install (user scope) + SessionStart hook
#   ./install.sh --uninstall       # remove the user-scope registration + hook
#   ./install.sh --verify          # run validation tests
#   ./install.sh --migrate-shadow  # explicitly retire shadowing local/project
#                                  # registrations (backs up first)
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
CONFIG_HELPER="${SCRIPT_DIR}/scripts/claude_mcp_config.py"
HOME_DIR="${HOME:?HOME must be set}"

# Claude Code's config layout is not a single directory. By default the user
# MCP config is ~/.claude.json while settings.json lives under ~/.claude/. When
# CLAUDE_CONFIG_DIR is set both are flattened into that directory
# ($DIR/.claude.json, $DIR/settings.json). Honour that here so tests can run
# against a fully isolated config, and so we back up the files the CLI will
# actually mutate.
if [ -n "${CLAUDE_CONFIG_DIR:-}" ]; then
  CLAUDE_DIR="${CLAUDE_CONFIG_DIR}"
  CLAUDE_USER_CONFIG="${CLAUDE_CONFIG_DIR}/.claude.json"
else
  CLAUDE_DIR="${HOME_DIR}/.claude"
  CLAUDE_USER_CONFIG="${HOME_DIR}/.claude.json"
fi
PROJECT_DIR="$(pwd -P)"
BACKUP_TS="$(date -u +%Y%m%dT%H%M%SZ)"
BACKUP_DIR="${CLAUDE_DIR}/backups/mpm-claude-code-${BACKUP_TS}"

# Canonical MPM install — same resolution rule as mpm-opencode/src/index.ts.
MPM_CANONICAL_BIN="${HOME_DIR}/.mpm/bin/mpm-mcp"
MPM_CANONICAL_WS="${HOME_DIR}/.mpm"

# ---------------------------------------------------------------------------
# Argument parse
# ---------------------------------------------------------------------------
MODE="install"
for arg in "$@"; do
  case "$arg" in
    --uninstall)      MODE="uninstall" ;;
    --verify)         MODE="verify" ;;
    --migrate-shadow) MODE="migrate-shadow" ;;
    --help|-h)
      sed -n '2,21p' "$0"
      exit 0
      ;;
    *) echo "unknown arg: $arg" >&2; exit 2 ;;
  esac
done

# Registration is only ever performed through Claude Code's CLI. If it is not
# on PATH we fail loudly rather than falling back to writing the config file:
# the hand-written path is precisely what produced the silent no-op this
# adapter is being fixed for.
require_claude_cli() {
  if ! command -v claude >/dev/null 2>&1; then
    echo "❌ 'claude' not found on PATH." >&2
    echo "   This adapter registers MPM through Claude Code's own MCP CLI so" >&2
    echo "   scope and file layout stay correct. Install Claude Code, then" >&2
    echo "   re-run: ./install.sh" >&2
    echo "   To inspect the registration without the CLI, see" >&2
    echo "   README.md (user-scope verification)." >&2
    exit 1
  fi
}

# Copy a config file into the backup dir if we have not already this run.
backup_once() {
  local src="$1" dst="${BACKUP_DIR}/$2"
  [ -f "$src" ] || return 0
  mkdir -p "$BACKUP_DIR"
  [ -f "$dst" ] && return 0
  cp -p "$src" "$dst"
  echo "📦 backed up $src → $dst"
}

# ---------------------------------------------------------------------------
# Pre-flight
# ---------------------------------------------------------------------------
if [ "$MODE" = "install" ] || [ "$MODE" = "uninstall" ] || [ "$MODE" = "migrate-shadow" ]; then
  require_claude_cli
  if [ ! -r "$CONFIG_HELPER" ]; then
    echo "❌ config helper missing: $CONFIG_HELPER" >&2
    exit 1
  fi
fi

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
  # Remove only the USER-scope entry we created. Local and project scope
  # entries were not written by this installer and are never touched here —
  # removing them is what --migrate-shadow is for.
  if python3 "$CONFIG_HELPER" user-entry --config "$CLAUDE_USER_CONFIG" \
       | grep -q '"command"'; then
    backup_once "$CLAUDE_USER_CONFIG" ".claude.json.before-uninstall"
    claude mcp remove mpm --scope user
    echo "✅ uninstalled user-scope mpm from $CLAUDE_USER_CONFIG (backup at $BACKUP_DIR)"
  else
    echo "ℹ️  no user-scope mpm entry in $CLAUDE_USER_CONFIG; nothing to remove"
  fi

  # Retire any obsolete ~/.claude/.mcp.json this adapter used to write. That
  # file was never read by Claude Code, so it is inert rather than harmful —
  # but leaving it behind makes the next install look like it worked while it
  # did not. Move it aside instead of deleting so it can be inspected.
  LEGACY_TARGET="${CLAUDE_DIR}/.mcp.json"
  if [ -f "$LEGACY_TARGET" ]; then
    mkdir -p "$BACKUP_DIR"
    mv "$LEGACY_TARGET" "${BACKUP_DIR}/.mcp.json.legacy"
    echo "✅ moved inert legacy $LEGACY_TARGET → ${BACKUP_DIR}/.mcp.json.legacy"
    echo "   Claude Code never read this path; nothing depended on it."
  fi

  # Warn (do not act) if a shadowing local/project entry will outlive us.
  SHADOWS="$(python3 "$CONFIG_HELPER" find-shadows \
      --config "$CLAUDE_USER_CONFIG" --project-dir "$PROJECT_DIR" \
      --template "$TEMPLATE" --home "$HOME_DIR")"
  if [ "$(echo "$SHADOWS" | python3 -c 'import json,sys; print(len(json.load(sys.stdin)))')" -gt 0 ]; then
    echo ""
    echo "⚠️  shadowing MPM registration(s) remain and will override user scope:"
    echo "$SHADOWS"
    echo "   Retire them explicitly with: ./install.sh --migrate-shadow"
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
# Migrate shadow — explicitly retire obsolete local/project registrations
# ---------------------------------------------------------------------------
# Why this is a separate, opt-in mode rather than part of `install`: removing
# an entry from a shared project `.mcp.json` edits a file other developers
# read, and removing a local-scope entry deletes per-project state that may
# encode a deliberate choice (a teammate pinning an older binary, a throwaway
# checkout). An installer that does this unasked is the bug class this whole
# change exists to close, so the removal is a decision a human makes
# explicitly, with a backup and a printed recovery path.
if [ "$MODE" = "migrate-shadow" ]; then
  ALL_SHADOWS="$(python3 "$CONFIG_HELPER" find-shadows \
      --config "$CLAUDE_USER_CONFIG" --project-dir "$PROJECT_DIR" \
      --template "$TEMPLATE" --home "$HOME_DIR" --all-projects)"
  N="$(echo "$ALL_SHADOWS" | python3 -c 'import json,sys; print(len(json.load(sys.stdin)))')"

  if [ "$N" -eq 0 ]; then
    echo "✅ no shadowing MPM registrations found; nothing to migrate"
    exit 0
  fi

  echo "Shadowing MPM registration(s) found:"
  echo "$ALL_SHADOWS" | sed 's/^/  /'
  echo ""
  echo "About to remove ONLY the entries classified 'legacy' or 'canonical'"
  echo "(this adapter's own wiring). Entries classified 'custom' or 'foreign'"
  echo "are left untouched — they carry configuration this installer did not"
  echo "write, and removing them is your call, not its."
  echo ""

  mkdir -p "$BACKUP_DIR"
  REMOVED=0; SKIPPED=0
  while IFS= read -r rec; do
    [ -z "$rec" ] && continue
    SCOPE="$(echo "$rec" | python3 -c 'import json,sys; print(json.loads(sys.argv[1])["scope"])' "$rec")"
    FILE="$(echo "$rec" | python3 -c 'import json,sys; print(json.loads(sys.argv[1])["file"])' "$rec")"
    CLASS="$(echo "$rec" | python3 -c 'import json,sys; print(json.loads(sys.argv[1])["classification"])' "$rec")"
    PROJ="$(echo "$rec" | python3 -c 'import json,sys; print(json.loads(sys.argv[1])["project"])' "$rec")"

    case "$CLASS" in
      canonical|legacy) ;;
      *) echo "  ⏭️  skipping $SCOPE entry in $FILE (classification: $CLASS)"
         SKIPPED=$((SKIPPED + 1)); continue ;;
    esac

    # Back up the whole file before the CLI rewrites it. The CLI preserves
    # unrelated servers, but the backup is what makes recovery possible if the
    # removal was the wrong call.
    if [ "$SCOPE" = "local" ]; then
      backup_once "$FILE" ".claude.json.before-migrate"
    else
      backup_once "$FILE" "$(echo "$PROJ" | tr '/' '_').mcp.json.before-migrate"
    fi

    if (cd "$PROJ" && claude mcp remove mpm --scope "$SCOPE") >/dev/null 2>&1; then
      echo "  ✅ removed $SCOPE-scope mpm (was in $FILE, project $PROJ)"
      REMOVED=$((REMOVED + 1))
    else
      echo "  ❌ failed to remove $SCOPE-scope mpm from $FILE — left as-is" >&2
      SKIPPED=$((SKIPPED + 1))
    fi
  done < <(echo "$ALL_SHADOWS" | python3 -c '
import json, sys
for r in json.load(sys.stdin):
    print(json.dumps(r))')

  echo ""
  echo "✅ migration complete: $REMOVED removed, $SKIPPED left in place"
  if [ "$REMOVED" -gt 0 ]; then
    echo ""
    echo "Recovery — the pre-migration files are verbatim copies at:"
    echo "  $BACKUP_DIR"
    echo "To roll back, restore the file(s) above over their originals:"
    echo "  cp -p $BACKUP_DIR/<file> <original path>"
  fi
  echo ""
  echo "⚠️  restart Claude Code for the change to take effect — MCP servers are"
  echo "   resolved at session start."
  exit 0
fi

# ---------------------------------------------------------------------------
# Install
# ---------------------------------------------------------------------------
mkdir -p "$CLAUDE_DIR"

# The template stays the single source of truth for the server entry. Read the
# env block out of it and hand that to `claude mcp add` rather than restating
# the variables here, so the template and the registration cannot drift.
DESIRED="$(python3 "$CONFIG_HELPER" desired-entry --template "$TEMPLATE" --home "$HOME_DIR")"
DESIRED_COMMAND="$(echo "$DESIRED" | python3 -c 'import json,sys; print(json.load(sys.stdin)["command"])')"
DESIRED_ENV_ARGS=()
while IFS= read -r kv; do
  [ -n "$kv" ] && DESIRED_ENV_ARGS+=(-e "$kv")
done < <(echo "$DESIRED" | python3 -c '
import json, sys
for k, v in (json.load(sys.stdin).get("env") or {}).items():
    print(f"{k}={v}")')

# Backup before the CLI mutates anything.
backup_once "$CLAUDE_USER_CONFIG" ".claude.json.before-install"

# Decide between no-op / update. `claude mcp add` refuses to overwrite an
# existing name ("already exists") and exits 0, so calling it blind would make
# re-running the installer silently keep a STALE registration — including one
# with the wrong command or no env block. Compare first.
CURRENT="$(python3 "$CONFIG_HELPER" user-entry --config "$CLAUDE_USER_CONFIG")"
STATUS="$(python3 -c '
import json, sys
cur, want = json.loads(sys.argv[1]), json.loads(sys.argv[2])
if not cur:
    print("absent")
elif cur.get("command") == want["command"] and (cur.get("env") or {}) == (want.get("env") or {}):
    print("current")
else:
    print("stale")' "$CURRENT" "$DESIRED")"

if [ "$STATUS" = "current" ]; then
  echo "✅ user-scope mpm already registered and current — no change"
  echo "   command: $DESIRED_COMMAND"
  echo "   config:  $CLAUDE_USER_CONFIG"
else
  if [ "$STATUS" = "stale" ]; then
    echo "🔁 updating stale user-scope mpm registration:"
    echo "$CURRENT" | sed 's/^/   was: /'
    claude mcp remove mpm --scope user >/dev/null
  fi
  # `--` separates Claude Code's flags from the subprocess command, so the
  # command and its own flags are never mistaken for CLI options.
  claude mcp add mpm --scope user "${DESIRED_ENV_ARGS[@]}" -- "$DESIRED_COMMAND"
  echo "✅ registered mpm at user scope"
  echo "   command: $DESIRED_COMMAND"
  echo "   env:     $(echo "$DESIRED" | python3 -c 'import json,sys; print(json.load(sys.stdin).get("env"))')"
fi

# Shadow detection. Claude Code resolves local scope above project scope
# above user scope, so a leftover local/project `mpm` entry silently wins over
# what we just registered. Report it; never remove it here — an entry in a
# shared project `.mcp.json` or another developer's local config is not ours
# to delete, and doing so unasked is the failure mode this installer is
# meant to avoid.
SHADOWS="$(python3 "$CONFIG_HELPER" find-shadows \
    --config "$CLAUDE_USER_CONFIG" --project-dir "$PROJECT_DIR" \
    --template "$TEMPLATE" --home "$HOME_DIR" --all-projects)"
SHADOW_COUNT="$(echo "$SHADOWS" | python3 -c 'import json,sys; print(len(json.load(sys.stdin)))')"
if [ "$SHADOW_COUNT" -gt 0 ]; then
  echo ""
  echo "⚠️  $SHADOW_COUNT shadowing MPM registration(s) found. These override"
  echo "   the user-scope registration above for the projects they cover:"
  echo "$SHADOWS" | sed 's/^/   /'
  echo ""
  echo "   Nothing was removed. To retire them (backs up first, opt-in):"
  echo "     ./install.sh --migrate-shadow"
fi

# ---------------------------------------------------------------------------
# Sanity probe
# ---------------------------------------------------------------------------
# Boot the MCP server and confirm it responds to a real
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
#
# MPM_INSTALL_SKIP_PROBE=1 opts out. The probe is a convenience check on the
# binary, not part of the registration contract: it has no meaning in a test
# sandbox or where the workspace DB is unreachable, and it must not be the
# reason an install fails there.
if [ "${MPM_INSTALL_SKIP_PROBE:-0}" = "1" ]; then
  echo "⏭️  live mpm-mcp probe skipped (MPM_INSTALL_SKIP_PROBE=1)"
else
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

  echo "✅ mpm-mcp probe ok:true"
  echo "   db_path: $DB_PATH"
fi

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
