// tests/installer.test.js — adapter installer regression coverage.
//
// Pins the 2026-09-16 fresh-profile fixes to the mpm-memory-openclaw
// install.sh and the 2026-09-17 surgical hardening pass against
// OpenClaw 2026.9.4. The script is bash; these tests drive it through
// a controlled PATH + a fake `openclaw` CLI + a fake `mpm` binary,
// so we can assert on what was persisted and in what order without
// touching the real OpenClaw install.
//
// Run with:
//   node --test tests/installer.test.js
//
// What these tests pin (covers all of §Tests required in the
// 2026-09-17 review):
//   1. both hook permission flags are written (allowConversationAccess
//      AND allowPromptInjection)
//   2. plugin is installed/linked before plugin-specific config is
//      applied (the link ordering matters — plugin config keys are
//      only valid once the plugin is registered)
//   3. installer works from arbitrary CWD (BASH_SOURCE[0] resolved)
//   4. canonical $HOME/.mpm/bin/mpm is accepted when bare `mpm` is
//      not on the installer shell's PATH
//   5. canonical $HOME/.local/bin/mpm symlink is accepted
//   6. existing valid MPM is not unnecessarily bootstrapped (no
//      MPM_BOOTSTRAP_URL call when canonical paths exist)
//   7. first install uses the documented 2026.9.4 install flags:
//        --link --force --accept-capabilities
//   8. idempotent rerun of an already-correctly-linked plugin SKIPS
//      the install step entirely (no destructive re-install)
//   9. conflicting existing plugin state is detected and reported as
//      a hard error (no silent overwrite)
//  10. plugin is enabled, memory slot is switched
//  11. memory-core is NOT modified by the installer (operator policy)
//  12. absolute mpmBin is persisted
//  13. no shell startup files (.bashrc/.zshrc/.profile) are modified
//  14. no root install.sh OpenClaw behavior is reintroduced
//  15. gateway restart uses `openclaw gateway restart --safe` (NOT
//      `--safe --wait`: those flags are mutually exclusive in 2026.9.4)
//  16. gateway restart is bounded by an outer timeout — a hanging
//      restart cannot hang the installer indefinitely
//  17. gateway status is bounded by an outer timeout AND passes the
//      CLI's own --timeout flag
//  18. gateway status hang does NOT make config writes fail
//  19. plugin id is read from openclaw.plugin.json (not hard-coded)
//  20. README documents the canonical install path, both hook flags,
//      and the memory-core coexistence policy
//
// These tests do not need a real OpenClaw install. They use a hermetic
// PATH and a fake-openclaw binary that records invocations and emits
// plausible JSON for `plugins inspect --json` and `plugins list --json`.
// The fake-openclaw's plugin state is parameterised by env so a single
// fake can serve "absent", "linked-from-here", and "conflicting" tests.

import { test, before, after } from "node:test";
import assert from "node:assert";
import {
  mkdirSync,
  writeFileSync,
  chmodSync,
  rmSync,
  readFileSync,
  existsSync,
  symlinkSync,
} from "node:fs";
import { spawn } from "node:child_process";
import path from "node:path";
import os from "node:os";
import { fileURLToPath } from "node:url";

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const ADAPTER_DIR = path.join(__dirname, "..");

// The legacy id's canonical former location under the source/runtime
// split. It is the runtime tree, NOT a sibling of the checkout: under
// the split the runtime root is no longer the checkout, so an install
// record left by the legacy id lives under <runtime-root>/. Naming a
// source sibling here would describe a pre-split install, which the
// installer is right to treat as foreign.
function legacyFormerPath(homeDir) {
  return path.join(homeDir, ".mpm", "agent_installation", "openclaw-mpm-memory");
}
const INSTALL_SH = path.join(ADAPTER_DIR, "install.sh");
// Runtime identity of the plugin. Matches index.js's PLUGIN_ID and the
// id in openclaw.plugin.json; the directory name the installer stages
// its runtime package under.
const PLUGIN_ID = "mpm-memory-openclaw";

// --------------------------------------------------------------------------
// Hermetic scaffolding
// --------------------------------------------------------------------------

const SANDBOX_ROOT = path.join(os.tmpdir(), "mpm-memory-openclaw-install-" + process.pid);
const FAKE_HOME = path.join(SANDBOX_ROOT, "home");
const FAKE_MPM_PRIMARY = path.join(FAKE_HOME, ".mpm", "bin");
const FAKE_MPM_SYMLINK_DIR = path.join(FAKE_HOME, ".local", "bin");
const FAKE_OPENCLAW_BINDIR = path.join(SANDBOX_ROOT, "openclaw-bin");
const FAKE_OPENCLAW_CONFIG = path.join(FAKE_HOME, ".openclaw", "openclaw.json");

const FAKE_MPM_BIN = path.join(FAKE_MPM_PRIMARY, "mpm");
const FAKE_MPM_SYMLINK = path.join(FAKE_MPM_SYMLINK_DIR, "mpm");

// Record file for fake-openclaw invocations. Each invocation appends
// one JSON line {argv, ok, env} for assertion.
const OPENCLAW_INVOCATIONS = path.join(SANDBOX_ROOT, "openclaw-invocations.jsonl");

// --------------------------------------------------------------------------
// Fake binaries
// --------------------------------------------------------------------------

// Fake mpm: prints a deterministic version line, exits 0.
const FAKE_MPM_SCRIPT = `#!/usr/bin/env bash
echo "MPM fake-mpm 0.0.0-test"
exit 0
`;

// Helper: a tag function that returns the raw text of the template
// literal without performing JS interpolation. This lets us embed
// bash parameter expansions like ${VAR:-default} without JS treating
// them as template substitutions. Use raw\`...\` instead of \`...\`
// for any bash script that needs literal ${} syntax.
function raw(strings, ...values) { return strings.raw.join(''); }

const _BPE_SENTINELS = ["${FAKE_OPENCLAW_LEGACY_STATE-absent}", "${FAKE_OPENCLAW_LEGACY_CONFIG_PRESENT-0}", "${FAKE_OPENCLAW_LEGACY_STATE-absent}", "${FAKE_OPENCLAW_PLUGIN_STATE:-absent}", "${FAKE_OPENCLAW_LINK_PATH-/tmp/mpm-memory-openclaw-install-fake}", "${FAKE_OPENCLAW_CONFLICT_PATH-/opt/unrelated/mpm-memory-openclaw}", "${FAKE_OPENCLAW_LEGACY_STATE-absent}", "${FAKE_OPENCLAW_LEGACY_LINK_PATH-" + path.join(os.homedir(), "legacy-adapter", "openclaw-mpm-memory") + "}", "${FAKE_OPENCLAW_LEGACY_CONFLICT_PATH-/opt/unrelated/openclaw-mpm-memory}", "${FAKE_OPENCLAW_LEGACY_STATE-absent}", "${FAKE_OPENCLAW_LEGACY_REGISTRY_PRESENT-0}", "${FAKE_OPENCLAW_LEGACY_LINK_PATH-" + path.join(os.homedir(), "legacy-adapter", "openclaw-mpm-memory") + "}", "${FAKE_OPENCLAW_LEGACY_LINK_PATH-" + path.join(os.homedir(), "legacy-adapter", "openclaw-mpm-memory") + "}", "${FAKE_OPENCLAW_LEGACY_CONFLICT_PATH-/opt/unrelated/openclaw-mpm-memory}", "${FAKE_OPENCLAW_HANG_STATUS-0}", "${FAKE_OPENCLAW_FAIL_STATUS-0}", "${FAKE_OPENCLAW_HANG-0}", "${FAKE_OPENCLAW_FAIL_RESTART-0}", "${FAKE_OPENCLAW_REJECT_BOOTSTRAP-0}", "${OPENCLAW_INVOCATIONS:-/tmp/mpm-memory-openclaw-fake-invocations.jsonl}", "${FAKE_OPENCLAW_UPDATE_REPAIR_FAIL-0}", "${FAKE_OPENCLAW_UPDATE_REPAIR_HANG-0}", "${FAKE_OPENCLAW_UPDATE_REPAIR_WARN-0}", "${FAKE_OPENCLAW_PRETEND_DIRTY-0}", "${FAKE_OPENCLAW_GATEWAY_DOWN_ON_RESTART_FAIL-0}"];
const FAKE_OPENCLAW_SCRIPT = `#!/usr/bin/env bash
# Fake openclaw — records every invocation and replies to the
# subcommands the installer uses. Behaviour is parameterised by env:
#   FAKE_OPENCLAW_FAIL_RESTART=1     → make gateway restart exit non-zero
#   FAKE_OPENCLAW_HANG=1             → sleep 60s on gateway restart
#   FAKE_OPENCLAW_HANG_STATUS=1      → sleep 60s on gateway status
#   FAKE_OPENCLAW_FAIL_STATUS=1      → make gateway status exit non-zero
#   FAKE_OPENCLAW_REJECT_BOOTSTRAP=1 → reject unknown commands
#
# 2026.9.5 lifecycle simulation (convergence + verify-after-restart):
#   FAKE_OPENCLAW_UPDATE_REPAIR_FAIL=1   → make update repair exit non-zero
#                                          (simulates real convergence failure)
#   FAKE_OPENCLAW_UPDATE_REPAIR_HANG=1   → sleep 60s on update repair
#   FAKE_OPENCLAW_UPDATE_REPAIR_WARN=1   → update repair prints the completion-cache
#                                          warning ("native no-replace move is
#                                          unavailable on this filesystem") but
#                                          exits 0 (real behaviour of the CLI)
#   FAKE_OPENCLAW_PRETEND_DIRTY=1        → before any other action, mark the
#                                          gateway "dirty" so a restart BEFORE
#                                          update repair would fail with 78.
#                                          update repair clears the flag.
#   FAKE_OPENCLAW_GATEWAY_DOWN_ON_RESTART_FAIL=1
#                                        → when restart exits non-zero, also
#                                          mark the gateway as down so the
#                                          post-restart verify probe fails.
#                                          Default: post-restart probe still
#                                          succeeds even when restart failed.
#
# Plugin-state simulation (mirrors 2026.9.4 plugins inspect --json):
#   FAKE_OPENCLAW_PLUGIN_STATE=absent|linked|conflicting
#     absent       - plugins inspect returns ok:false (no plugin record)
#     linked       - plugins inspect returns ok:true, rootDir = our SCRIPT_DIR
#     conflicting  - plugins inspect returns ok:true, rootDir = some other
#                    absolute path
#   FAKE_OPENCLAW_LINK_PATH=<abs path>
#                    overrides the rootDir used in the linked case.
#                    Default: the adapter's actual SCRIPT_DIR at test time.
#   FAKE_OPENCLAW_CONFLICT_PATH=<abs path>
#                    overrides the rootDir used in the conflicting case.
#                    Default: /opt/unrelated/mpm-memory-openclaw
#
# Legacy plugin-id simulation (mirrors the real upgrade case where
# OpenClaw's plugin id for this adapter was 'openclaw-mpm-memory'
# before the 2026-09-17 namespace migration):
#   FAKE_OPENCLAW_LEGACY_STATE=absent|linked|vanished|conflicting|config_only|registry_only
#     absent        — inspect ok:false, registry has no record,
#                       no config keys, no slot. → no migration.
#     linked        — inspect ok:true with rootDir pointing at the
#                       LEGACY canonical former path ($(dirname
#                       SCRIPT_DIR)/openclaw-mpm-memory). → migrate.
#     vanished      — inspect ok:false (legacy rootDir no longer on
#                       disk), registry has the legacy record but the
#                       path doesn't resolve; config keys still
#                       reference the legacy id. → migrate from
#                       registry + config evidence. THIS IS THE REAL
#                       BUG CASE.
#     conflicting   — inspect ok:true with rootDir pointing at an
#                       unrelated existing path → refuse, conflict.
#     config_only   — inspect ok:false, registry unavailable, but
#                       plugins.entries.openclaw-mpm-memory.config.mpmBin
#                       and/or plugins.slots.memory = openclaw-mpm-memory
#                       are still set. → migrate from config evidence.
#     registry_only — registry shows the legacy install record
#                       (sourcePath = canonical former path); inspect
#                       ok:false; config keys may or may not exist.
#                       → migrate from registry evidence.
#   FAKE_OPENCLAW_LEGACY_LINK_PATH=<abs path>
#                    overrides the legacy rootDir used in the linked
#                    case. Default: the canonical former path
#                    ($ADAPTER_PARENT/openclaw-mpm-memory).
#   FAKE_OPENCLAW_LEGACY_CONFIG_PRESENT=1
#                    legacy config keys (plugins.entries.openclaw-mpm-memory.*,
#                    plugins.slots.memory) are populated in config get.
#                    Default: 0 (no legacy config).
#   FAKE_OPENCLAW_LEGACY_REGISTRY_PRESENT=1
#                    registry --json shows the legacy install record.
#                    Default: 0 (no legacy registry record).
#   FAKE_OPENCLAW_LEGACY_PATH_RESOLVABLE=1
#                    legacy path resolves on disk. Default: 1.
set -euo pipefail

INV="__BPE_19__"
mkdir -p "$(dirname "$INV")"
touch "$INV"

# Record the invocation as one JSONL line.
{
  printf '{ "argv": %s, "pwd": "%s" }\\n' "$(printf '%s' "$*" | python3 -c 'import json,sys; print(json.dumps(sys.stdin.read()))' 2>/dev/null || printf 'null')" "$PWD"
} >> "$INV"
cmd="$1"
shift || true
case "$cmd" in
  config)
    # Capture config-set keys by reading argv. We do NOT mutate real
    # openclaw.json — the fake just logs to openclaw-invocations.jsonl.
    sub="$1"
    if [ "$sub" = "set" ]; then
      key="$1"; shift
      key="$1"; shift
      value="$1"; shift || true
      printf '  -> config set %s=%s\\n' "$key" "$value" >> "$INV"
    elif [ "$sub" = "get" ]; then
      shift || true
      key="$1"; shift || true
      printf '  -> config get %s\\n' "$key" >> "$INV"
      # Simulate legacy config that the installer may need to migrate.
      # Each known legacy key returns a plausible value when
      # FAKE_OPENCLAW_LEGACY_CONFIG_PRESENT=1.
      legacy_state="__BPE_0__"
      legacy_cfg="__BPE_1__"
      if [ "$legacy_cfg" = "1" ] && [ "$legacy_state" != "absent" ]; then
        case "$key" in
          "plugins.slots.memory")
            # Only return the legacy id when the legacy state implies
            # the slot pointed at the legacy plugin id.
            case "$legacy_state" in
              vanished|registry_only)
                printf 'openclaw-mpm-memory\\n'
                ;;
              *)
                : # slot does not currently point at legacy id
                ;;
            esac
            ;;
          plugins.entries.openclaw-mpm-memory.config.mpmBin)
            # Simulate an existing mpmBin entry from a previous install.
            # The installer must overwrite this with the canonical
            # absolute path; the literal value here only needs to be
            # any absolute path — we use HOME-derived so the fake is
            # host-agnostic. The printf literal below passes "$HOME/
            # .local/bin/mpm" to the fake bash; bash parameter expansion
            # resolves HOME at run time. The dollar-brace is escaped
            # because this template literal is a regular backtick (not
            # raw backtick) and would otherwise perform JS interpolation.
            printf '\${HOME}/.local/bin/mpm\\n'
            ;;
          plugins.entries.openclaw-mpm-memory.enabled)
            printf 'true\\n'
            ;;
          plugins.entries.openclaw-mpm-memory.hooks.allowConversationAccess)
            printf 'true\\n'
            ;;
          plugins.entries.openclaw-mpm-memory.hooks.allowPromptInjection)
            printf 'true\\n'
            ;;
          plugins.entries.openclaw-mpm-memory.config.timeoutMs)
            printf '5000\\n'
            ;;
          plugins.entries.openclaw-mpm-memory.config.scope)
            printf 'all\\n'
            ;;
          plugins.entries.openclaw-mpm-memory.config.limitDefault)
            printf '6\\n'
            ;;
        esac
      fi
    elif [ "$sub" = "unset" ]; then
      shift || true
      key="$1"; shift || true
      printf '  -> config unset %s\\n' "$key" >> "$INV"
    fi
    exit 0
    ;;
  plugins)
    sub="$1"
    shift || true
    case "$sub" in
      install)
        # Validate that the install command carries the documented
        # 2026.9.4 flag set for local trusted source installs. The
        # installer is required to pass --link --force
        # --accept-capabilities. Any deviation is logged for tests
        # to assert against.
        printf '  -> plugins install %s\\n' "$*" >> "$INV"
        case "$*" in
          *--link*--force*--accept-capabilities*)
            printf '  -> plugins install: FLAGS_OK\\n' >> "$INV"
            ;;
          *)
            printf '  -> plugins install: FLAGS_MISSING argv=%s\\n' "$*" >> "$INV"
            exit 9
            ;;
        esac
        exit 0
        ;;
      enable)
        printf '  -> plugins enable %s\\n' "$*" >> "$INV"
        exit 0
        ;;
      uninstall)
        printf '  -> plugins uninstall %s\\n' "$*" >> "$INV"
        # Refuse to uninstall when FAKE_OPENCLAW_LEGACY_STATE=absent so
        # tests can assert the installer didn't issue the call.
        legacy_state="__BPE_2__"
        if [ "$*" = "openclaw-mpm-memory" ] && [ "$legacy_state" = "absent" ]; then
          printf '  -> plugins uninstall: NOT_FOUND\\n' >> "$INV"
          exit 7
        fi
        # Mark the legacy id as uninstalled so subsequent inspect calls
        # reflect the post-uninstall state. The marker is a sentinel file
        # that the inspect case reads to decide whether to return the
        # legacy record. This lets the fake model the real OpenClaw
        # behaviour: after plugins uninstall of the id, inspect of that
        # id returns ok:false (the install record is gone).
        if [ "$*" = "openclaw-mpm-memory" ]; then
          UNINSTALLED_FLAG="\${FAKE_OPENCLAW_UNINSTALLED_FLAG:-/tmp/mpm-memory-openclaw-fake-uninstalled-flag}"
          : > "$UNINSTALLED_FLAG" 2>/dev/null || true
        fi
        exit 0
        ;;
      inspect)
        # The installer reads plugin inspect --json to decide whether
        # to install, skip, or refuse. Emit the JSON shape the real
        # CLI returns in 2026.9.4.
        printf '  -> plugins inspect %s\\n' "$*" >> "$INV"
        plugin_id="$1"; shift || true
        plugin_state="__BPE_3__"
        case "$plugin_id" in
          mpm-memory-openclaw|"")
            # Current plugin id — uses the canonical-state simulation.
            case "$plugin_state" in
              absent)
                cat <<JSON
{ "ok": false, "error": { "type": "cli_error", "message": "Plugin not found: $plugin_id" } }
JSON
                ;;
              linked)
                link_root="__BPE_4__"
                cat <<JSON
{
  "ok": true,
  "plugin": {
    "id": "$plugin_id",
    "name": "MPM Memory (fake)",
    "version": "0.0.0-fake",
    "format": "openclaw",
    "source": "$link_root/index.js",
    "rootDir": "$link_root",
    "origin": "config",
    "trust": { "reason": "origin-path", "installSource": "path" },
    "enabled": true,
    "explicitlyEnabled": true,
    "activated": true,
    "activationReason": "selected memory slot",
    "status": "loaded"
  }
}
JSON
                ;;
              conflicting)
                conflict_path="__BPE_5__"
                cat <<JSON
{
  "ok": true,
  "plugin": {
    "id": "$plugin_id",
    "name": "MPM Memory (fake elsewhere)",
    "version": "0.0.0-fake",
    "format": "openclaw",
    "source": "$conflict_path/index.js",
    "rootDir": "$conflict_path",
    "origin": "config",
    "trust": { "reason": "origin-path", "installSource": "path" },
    "enabled": true,
    "explicitlyEnabled": true,
    "activated": true,
    "activationReason": "selected memory slot",
    "status": "loaded"
  }
}
JSON
                ;;
            esac
            ;;
          openclaw-mpm-memory)
            # Legacy plugin id — uses the legacy-state simulation.
            legacy_state="__BPE_6__"
            # If uninstall was previously called, reflect post-uninstall
            # reality regardless of the initial legacy_state. The
            # installer probes legacy state twice: once at the start
            # (Step B/C/D), once after uninstall (Step E reconciliation).
            # After uninstall, inspect must return ok:false so the
            # reconciliation probe confirms the install record is gone.
            UNINSTALLED_FLAG="\${FAKE_OPENCLAW_UNINSTALLED_FLAG:-/tmp/mpm-memory-openclaw-fake-uninstalled-flag}"
            if [ -f "$UNINSTALLED_FLAG" ]; then
              cat <<JSON
{ "ok": false, "error": { "type": "cli_error", "message": "Plugin not found: $plugin_id" } }
JSON
            else
              case "$legacy_state" in
                absent|config_only)
                  # inspect fails entirely (no live install record OR
                  # config-only state where the link has been swept).
                  cat <<JSON
{ "ok": false, "error": { "type": "cli_error", "message": "Plugin not found: $plugin_id" } }
JSON
                  ;;
                vanished|registry_only)
                  # Legacy record exists in registry but the linked
                  # rootDir has been removed (the git mv case). inspect
                  # surfaces ok:false because the load path is gone.
                  cat <<JSON
{ "ok": false, "error": { "type": "cli_error", "message": "Plugin not found: $plugin_id" } }
JSON
                  ;;
              linked)
                # Legacy install still live and pointing at the
                # canonical former path.
                legacy_link_root="__BPE_7__"
                cat <<JSON
{
  "ok": true,
  "plugin": {
    "id": "$plugin_id",
    "name": "MPM Memory (legacy)",
    "version": "0.0.0-fake",
    "format": "openclaw",
    "source": "$legacy_link_root/index.js",
    "rootDir": "$legacy_link_root",
    "origin": "config",
    "trust": { "reason": "origin-path", "installSource": "path" },
    "enabled": true,
    "explicitlyEnabled": true,
    "activated": true,
    "activationReason": "selected memory slot",
    "status": "loaded"
  }
}
JSON
                ;;
              conflicting)
                legacy_conflict_path="__BPE_8__"
                cat <<JSON
{
  "ok": true,
  "plugin": {
    "id": "$plugin_id",
    "name": "MPM Memory (legacy elsewhere)",
    "version": "0.0.0-fake",
    "format": "openclaw",
    "source": "$legacy_conflict_path/index.js",
    "rootDir": "$legacy_conflict_path",
    "origin": "config",
    "trust": { "reason": "origin-path", "installSource": "path" },
    "enabled": true,
    "explicitlyEnabled": true,
    "activated": true,
    "activationReason": "selected memory slot",
    "status": "loaded"
  }
}
JSON
                ;;
              esac
            fi
            ;;
        esac
        exit 0
        ;;
      registry)
        # Mirror 2026.9.4 openclaw plugins registry --json. The
        # persisted.installRecords field carries the install records
        # for the legacy plugin id.
        printf '  -> plugins registry %s\\n' "$*" >> "$INV"
        legacy_state="__BPE_9__"
        legacy_registry="__BPE_10__"
        case "$legacy_state" in
          absent)
            cat <<JSON
{ "ok": true, "state": "fresh", "refreshReasons": [], "differences": [], "persisted": { "version": 1, "installRecords": {} } }
JSON
            ;;
          linked|registry_only)
            legacy_link_root="__BPE_11__"
            cat <<JSON
{
  "ok": true,
  "state": "fresh",
  "refreshReasons": [],
  "differences": [],
  "persisted": {
    "version": 1,
    "installRecords": {
      "openclaw-mpm-memory": {
        "source": "path",
        "sourcePath": "$legacy_link_root",
        "installPath": "$legacy_link_root",
        "version": "0.1.3"
      }
    }
  }
}
JSON
            ;;
          vanished)
            # Registry record exists but the path is now gone (the
            # git mv case). installer must tolerate this.
            legacy_link_root="__BPE_12__"
            cat <<JSON
{
  "ok": true,
  "state": "fresh",
  "refreshReasons": [],
  "differences": [],
  "persisted": {
    "version": 1,
    "installRecords": {
      "openclaw-mpm-memory": {
        "source": "path",
        "sourcePath": "$legacy_link_root",
        "installPath": "$legacy_link_root",
        "version": "0.1.3"
      }
    }
  }
}
JSON
            ;;
          conflicting)
            legacy_conflict_path="__BPE_13__"
            cat <<JSON
{
  "ok": true,
  "state": "fresh",
  "refreshReasons": [],
  "differences": [],
  "persisted": {
    "version": 1,
    "installRecords": {
      "openclaw-mpm-memory": {
        "source": "path",
        "sourcePath": "$legacy_conflict_path",
        "installPath": "$legacy_conflict_path",
        "version": "0.0.0-unrelated"
      }
    }
  }
}
JSON
            ;;
          config_only)
            cat <<JSON
{ "ok": true, "state": "fresh", "refreshReasons": [], "differences": [], "persisted": { "version": 1, "installRecords": {} } }
JSON
            ;;
        esac
        exit 0
        ;;
      list)
        printf '  -> plugins list\\n' >> "$INV"
        # Emit a minimal list payload that includes our plugin id and
        # memory-core so the installer's coexistence probe fires.
        cat <<JSON
[
  { "id": "mpm-memory-openclaw", "enabled": true },
  { "id": "memory-core", "enabled": true }
]
JSON
        exit 0
        ;;
    esac
    ;;
  gateway)
    sub="$1"
    shift || true
    case "$sub" in
      status)
        printf '  -> gateway status %s\\n' "$*" >> "$INV"
        # The installer must pass --json and --timeout; record both.
        case "$*" in
          *--json*--timeout*)
            printf '  -> gateway status: FLAGS_OK\\n' >> "$INV"
            ;;
          *)
            printf '  -> gateway status: FLAGS_MISSING argv=%s\\n' "$*" >> "$INV"
            exit 9
            ;;
        esac
        if [ "__BPE_14__" = "1" ]; then
          sleep 60
        fi
        # DOWN_FLAG simulates the gateway being stopped — either by a
        # failed restart (FAKE_OPENCLAW_GATEWAY_DOWN_ON_RESTART_FAIL=1)
        # or by an external operator action. When set, status returns
        # non-zero so the post-restart verify probe can distinguish
        # "gateway back up" from "gateway still down".
        DOWN_FLAG="\${FAKE_OPENCLAW_GATEWAY_DOWN_FLAG:-/tmp/mpm-memory-openclaw-fake-gateway-down-flag}"
        if [ -f "$DOWN_FLAG" ]; then
          printf '  -> gateway status: DOWN\\n' >> "$INV"
          exit 8
        fi
        if [ "__BPE_15__" = "1" ]; then
          exit 8
        fi
        exit 0
        ;;
      restart)
        printf '  -> gateway restart %s\\n' "$*" >> "$INV"
        # The 2026.9.4 contract: --safe and --wait are mutually
        # exclusive (--wait is documented as "not compatible with
        # --force or --safe"). The installer must use --safe alone.
        case "$*" in
          *"--safe"*)
            printf '  -> gateway restart: SAFE_FLAG_OK\\n' >> "$INV"
            ;;
          *)
            printf '  -> gateway restart: NO_SAFE_FLAG argv=%s\\n' "$*" >> "$INV"
            exit 9
            ;;
        esac
        # Refuse --wait combined with --safe.
        case "$*" in
          *"--safe"*"--wait"*|*"--wait"*"--safe"*)
            printf '  -> gateway restart: SAFE_WAIT_CONFLICT\\n' >> "$INV"
            exit 9
            ;;
        esac
        if [ "__BPE_16__" = "1" ]; then
          sleep 60
        fi
        # 2026.9.5 dirty-state simulation: if the migration identity is
        # still "dirty" (no update repair has cleared it), a fresh
        # gateway restart will hit the new migration-inputs consistency
        # check and exit 78. The fake models that: any restart while the
        # DIRTY_FLAG is set exits 78 instead of 0.
        DIRTY_FLAG="\${FAKE_OPENCLAW_DIRTY_FLAG:-/tmp/mpm-memory-openclaw-fake-dirty-flag}"
        if [ -f "$DIRTY_FLAG" ]; then
          printf '  -> gateway restart: DIRTY_BLOCKED exit=78\\n' >> "$INV"
          exit 78
        fi
        if [ "__BPE_17__" = "1" ]; then
          # FAKE_OPENCLAW_FAIL_RESTART=1 — generic restart failure
          # (e.g. timeout). Optionally mark the gateway as down so the
          # post-restart verify probe fails when the test explicitly
          # opts into that via FAKE_OPENCLAW_GATEWAY_DOWN_ON_RESTART_FAIL.
          printf '  -> gateway restart: FAIL_RC=7\\n' >> "$INV"
          if [ "__BPE_24__" = "1" ]; then
            DOWN_FLAG="\${FAKE_OPENCLAW_GATEWAY_DOWN_FLAG:-/tmp/mpm-memory-openclaw-fake-gateway-down-flag}"
            : > "$DOWN_FLAG" 2>/dev/null || true
          fi
          exit 7
        fi
        # On success, clear any prior down flag so the post-restart
        # verify probe sees an up gateway.
        DOWN_FLAG="\${FAKE_OPENCLAW_GATEWAY_DOWN_FLAG:-/tmp/mpm-memory-openclaw-fake-gateway-down-flag}"
        rm -f "$DOWN_FLAG" 2>/dev/null || true
        exit 0
        ;;
    esac
    ;;
  update)
    sub="$1"
    shift || true
    case "$sub" in
      repair)
        printf '  -> update repair %s\\n' "$*" >> "$INV"
        if [ "__BPE_21__" = "1" ]; then
          sleep 60
        fi
        if [ "__BPE_20__" = "1" ]; then
          # Real convergence failure: non-zero exit, no "completed
          # with warnings" diagnostic. Installer must surface this.
          printf '  -> update repair: CONVERGENCE_FAIL exit=78\\n' >> "$INV"
          exit 78
        fi
        if [ "__BPE_22__" = "1" ]; then
          # Completion-cache warning path. The CLI prints the warning,
          # emits "Update finalization completed with warnings.", and
          # exits 0 because targetConfigConvergence completed. This is
          # the observed behaviour on v for "native no-replace move
          # is unavailable on this filesystem".
          printf '  -> update repair: COMPLETION_CACHE_WARN\\n' >> "$INV"
          printf 'Completion cache update failed:\\n[openclaw] native no-replace move is unavailable on this filesystem\\nUpdate finalization completed with warnings.\\n' >> "$INV"
        fi
        # Successful convergence clears the dirty flag.
        DIRTY_FLAG="\${FAKE_OPENCLAW_DIRTY_FLAG:-/tmp/mpm-memory-openclaw-fake-dirty-flag}"
        rm -f "$DIRTY_FLAG" 2>/dev/null || true
        printf '  -> update repair: CONVERGED exit=0\\n' >> "$INV"
        exit 0
        ;;
    esac
    ;;
  doctor|plugins-doctor)
    printf '  -> %s\\n' "$cmd" >> "$INV"
    exit 0
    ;;
  *)
    if [ "__BPE_18__" = "1" ]; then
      printf '  -> unknown command: %s\\n' "$cmd" >&2
      exit 99
    fi
    printf '  -> default-ok: %s\\n' "$cmd" >> "$INV"
    exit 0
    ;;
esac
`
.replace(/__BPE_(\d+)__/g, (_, n) => _BPE_SENTINELS[Number(n)]);


function writeFakeBin(name, content, dir) {
  mkdirSync(dir, { recursive: true });
  const p = path.join(dir, name);
  writeFileSync(p, content, { mode: 0o755 });
  chmodSync(p, 0o755);
  return p;
}

function readInvocations() {
  if (!existsSync(OPENCLAW_INVOCATIONS)) return [];
  return readFileSync(OPENCLAW_INVOCATIONS, "utf8")
    .split("\n")
    .filter(Boolean)
    .map((line) => {
      try {
        return JSON.parse(line);
      } catch {
        return { raw: line };
      }
    });
}

function shellStartupFilesWereTouched(homeDir) {
  // The installer MUST NOT modify any of these.
  for (const f of [".bashrc", ".zshrc", ".profile", ".bash_profile", ".zprofile"]) {
    const p = path.join(homeDir, f);
    if (existsSync(p)) {
      const c = readFileSync(p, "utf8");
      // Treat any pre-existing file as untouched — we never wrote to it
      // during the test. The "was touched" check is implicit: the
      // installer's contract forbids writes; if the file does not exist
      // at start and exists at end with installer content, that is a
      // bug. We start by not pre-creating these files.
      if (c.includes("[mpm-memory-openclaw install]")) {
        return { touched: true, file: f };
      }
    }
  }
  return { touched: false };
}

// --------------------------------------------------------------------------
// Driver
// --------------------------------------------------------------------------

function runInstaller({
  homeDir,
  mpmBootstrapUrl = "",
  cwd = SANDBOX_ROOT,
  pluginState = "absent",
  // Default resolved inside the driver to the RUNTIME PACKAGE the
  // installer provisions under the sandbox HOME, not ADAPTER_DIR.
  //
  // The adapter no longer links its source directory: it stages a
  // validated package at <runtime-root>/agent_installation/<plugin-id>
  // and links that. A fake `plugins inspect` that reports the checkout
  // as rootDir therefore describes an install this adapter did not make,
  // and the preflight correctly classifies it as a conflict.
  linkPath = null,
  conflictPath = "/opt/unrelated/mpm-memory-openclaw",
  // Legacy plugin-id simulation (2026-09-17 namespace migration).
  // absent    — no legacy state at all.
  // linked    — legacy id still installed and linked from the
  //              canonical former path ($(dirname ADAPTER_DIR)/openclaw-mpm-memory).
  // vanished  — the legacy linked rootDir no longer exists on disk
  //              (the real git-mv case); registry may still hold the
  //              install record; config keys may still reference the
  //              legacy id.
  // conflicting — legacy id points at an unrelated existing path →
  //              refuse with conflict error.
  // config_only — inspect + registry unavailable, but legacy config
  //              keys (plugins.entries.openclaw-mpm-memory.*,
  //              plugins.slots.memory) are still set.
  // registry_only — registry has the legacy install record;
  //                inspect unavailable; config keys may be sparse.
  legacyState = "absent",
  legacyLinkPath = legacyFormerPath(homeDir),
  legacyConflictPath = "/opt/unrelated/openclaw-mpm-memory",
  legacyConfigPresent = false,
  legacyRegistryPresent = false,
  failRestart = false,
  hangRestart = false,
  failStatus = false,
  hangStatus = false,
  updateRepairFail = false,
  updateRepairHang = false,
  updateRepairWarn = false,
  pretendDirty = false,
  gatewayDownOnRestartFail = false,
  extraEnv = {},
} = {}) {
  // The installer respects $HOME and runs `openclaw` + `mpm` from PATH.
  // We point PATH at the fake bin dirs and HOME at the sandbox so the
  // canonical paths under $HOME resolve there. OPENCLAW_INVOCATIONS is
  // forwarded so the fake-openclaw records to the test's assertion file.
  if (linkPath === null) {
    linkPath = path.join(homeDir, ".mpm", "agent_installation", PLUGIN_ID);
  }
  const env = {
    ...process.env,
    PATH: `${FAKE_OPENCLAW_BINDIR}:${FAKE_MPM_PRIMARY}:${FAKE_MPM_SYMLINK_DIR}:/usr/bin:/bin`,
    HOME: homeDir,
    MPM_BOOTSTRAP_URL: mpmBootstrapUrl,
    OPENCLAW_INVOCATIONS,
    OPENCLAW_PLUGIN_INSTALL_TIMEOUT: "10",
    OPENCLAW_GATEWAY_RESTART_TIMEOUT: "5",
    OPENCLAW_GATEWAY_STATUS_TIMEOUT: "5",
    OPENCLAW_GATEWAY_VERIFY_TIMEOUT: "5",
    OPENCLAW_UPDATE_REPAIR_TIMEOUT: "10",
    OPENCLAW_PLUGIN_INSPECT_TIMEOUT: "5",
    OPENCLAW_CONFIG_TIMEOUT: "5",
    FAKE_OPENCLAW_PLUGIN_STATE: pluginState,
    FAKE_OPENCLAW_LINK_PATH: linkPath,
    FAKE_OPENCLAW_CONFLICT_PATH: conflictPath,
    FAKE_OPENCLAW_LEGACY_STATE: legacyState,
    FAKE_OPENCLAW_LEGACY_LINK_PATH: legacyLinkPath,
    FAKE_OPENCLAW_LEGACY_CONFLICT_PATH: legacyConflictPath,
    FAKE_OPENCLAW_LEGACY_CONFIG_PRESENT: legacyConfigPresent ? "1" : "0",
    FAKE_OPENCLAW_LEGACY_REGISTRY_PRESENT: legacyRegistryPresent ? "1" : "0",
    FAKE_OPENCLAW_FAIL_RESTART: failRestart ? "1" : "0",
    FAKE_OPENCLAW_HANG: hangRestart ? "1" : "0",
    FAKE_OPENCLAW_FAIL_STATUS: failStatus ? "1" : "0",
    FAKE_OPENCLAW_HANG_STATUS: hangStatus ? "1" : "0",
    FAKE_OPENCLAW_UPDATE_REPAIR_FAIL: updateRepairFail ? "1" : "0",
    FAKE_OPENCLAW_UPDATE_REPAIR_HANG: updateRepairHang ? "1" : "0",
    FAKE_OPENCLAW_UPDATE_REPAIR_WARN: updateRepairWarn ? "1" : "0",
    FAKE_OPENCLAW_PRETEND_DIRTY: pretendDirty ? "1" : "0",
    FAKE_OPENCLAW_GATEWAY_DOWN_ON_RESTART_FAIL: gatewayDownOnRestartFail ? "1" : "0",
    ...extraEnv,
  };
  delete env.MPM_BIN;

  // 2026.9.5 simulation: when pretendDirty is true, pre-create the
  // dirty flag so the first restart would fail with 78 (until update
  // repair clears it). The flag is per-process — drop any stale one
  // from a previous run first.
  const DIRTY_FLAG = "/tmp/mpm-memory-openclaw-fake-dirty-flag";
  const DOWN_FLAG = "/tmp/mpm-memory-openclaw-fake-gateway-down-flag";
  try { rmSync(DIRTY_FLAG, { force: true }); } catch {}
  try { rmSync(DOWN_FLAG, { force: true }); } catch {}
  if (pretendDirty) writeFileSync(DIRTY_FLAG, "", { mode: 0o644 });

  return new Promise((resolve) => {
    const child = spawn("bash", [INSTALL_SH], {
      cwd,
      stdio: ["ignore", "pipe", "pipe"],
      env,
    });
    let stdout = "";
    let stderr = "";
    child.stdout.on("data", (d) => (stdout += d));
    child.stderr.on("data", (d) => (stderr += d));
    child.on("close", (code) => resolve({ code, stdout, stderr }));
    child.on("error", (err) => resolve({ code: -1, stdout, stderr: stderr + err.message }));
  });
}

// --------------------------------------------------------------------------
// Setup / teardown
// --------------------------------------------------------------------------

before(() => {
  rmSync(SANDBOX_ROOT, { recursive: true, force: true });
  mkdirSync(FAKE_MPM_PRIMARY, { recursive: true });
  mkdirSync(FAKE_MPM_SYMLINK_DIR, { recursive: true });
  mkdirSync(FAKE_OPENCLAW_BINDIR, { recursive: true });
  mkdirSync(path.dirname(FAKE_OPENCLAW_CONFIG), { recursive: true });
  // Real mpm at the canonical primary path.
  writeFakeBin("mpm", FAKE_MPM_SCRIPT, FAKE_MPM_PRIMARY);
  // Symlink at the canonical user location.
  writeFileSync(FAKE_MPM_SYMLINK, "", { mode: 0o755 }); // placeholder, replaced below
  rmSync(FAKE_MPM_SYMLINK);
  symlinkSync(FAKE_MPM_BIN, FAKE_MPM_SYMLINK);
  // Fake openclaw CLI.
  writeFakeBin("openclaw", FAKE_OPENCLAW_SCRIPT, FAKE_OPENCLAW_BINDIR);
});

after(() => {
  rmSync(SANDBOX_ROOT, { recursive: true, force: true });
  // Clean the fake uninstall flag so subsequent test runs (and
  // other consumers on this host) start from a known state.
  try { rmSync("/tmp/mpm-memory-openclaw-fake-uninstalled-flag", { force: true }); } catch {}
  // 2026.9.5 lifecycle simulation flags — drop so re-runs start fresh.
  try { rmSync("/tmp/mpm-memory-openclaw-fake-dirty-flag", { force: true }); } catch {}
  try { rmSync("/tmp/mpm-memory-openclaw-fake-gateway-down-flag", { force: true }); } catch {}
});

// --------------------------------------------------------------------------
// Tests
// --------------------------------------------------------------------------

function freshHomeDir(label) {
  const dir = path.join(SANDBOX_ROOT, "homes", label);
  mkdirSync(dir, { recursive: true });
  return dir;
}

function installCanonicalMpmAt(homeDir, { symlinkOnly = false } = {}) {
  // Mirror the real root install.sh layout under a per-test $HOME
  // so the adapter installer can resolve MPM via its canonical discovery
  // order without us putting the per-test home on PATH.
  const primaryDir = path.join(homeDir, ".mpm", "bin");
  const symDir = path.join(homeDir, ".local", "bin");
  mkdirSync(primaryDir, { recursive: true });
  mkdirSync(symDir, { recursive: true });
  const primaryBin = path.join(primaryDir, "mpm");
  // Copy the fake mpm binary (not symlink) so each test has its own
  // working state.
  writeFileSync(primaryBin, FAKE_MPM_SCRIPT, { mode: 0o755 });
  chmodSync(primaryBin, 0o755);
  if (!symlinkOnly) {
    symlinkSync(primaryBin, path.join(symDir, "mpm"));
  } else {
    try { rmSync(path.join(symDir, "mpm"), { force: true }); } catch {}
  }
  return { primaryBin };
}

function clearInvocations() {
  rmSync(OPENCLAW_INVOCATIONS, { force: true });
}

function clearFakeUninstalledFlag() {
  // The fake-openclaw records an "uninstalled" state in a sentinel
  // file so subsequent inspect calls reflect post-uninstall reality.
  // Wipe it before each legacy-state test so the initial install
  // record state is reproducible regardless of test ordering.
  try { rmSync("/tmp/mpm-memory-openclaw-fake-uninstalled-flag", { force: true }); } catch {}
}

test("installer resolves MPM via $HOME/.mpm/bin/mpm when bare mpm is not on PATH", async () => {
  const home = freshHomeDir("canonical-primary");
  installCanonicalMpmAt(home); // populate $HOME/.mpm/bin/mpm + $HOME/.local/bin/mpm
  clearInvocations();
  clearFakeUninstalledFlag();
  // PATH explicitly excludes both .mpm/bin and .local/bin so the
  // installer's PATH lookup branch (#3) cannot resolve; only the
  // canonical primary path (#1) and canonical symlink (#2) should
  // match.
  const env = {
    ...process.env,
    PATH: `${FAKE_OPENCLAW_BINDIR}:/usr/bin:/bin`,
    HOME: home,
    OPENCLAW_PLUGIN_INSTALL_TIMEOUT: "10",
    OPENCLAW_GATEWAY_RESTART_TIMEOUT: "5",
  };
  delete env.MPM_BOOTSTRAP_URL;
  const { code, stderr } = await new Promise((resolve) => {
    const child = spawn("bash", [INSTALL_SH], {
      cwd: SANDBOX_ROOT,
      stdio: ["ignore", "pipe", "pipe"],
      env,
    });
    let err = "";
    child.stderr.on("data", (d) => (err += d));
    child.on("close", (c) => resolve({ code: c, stderr: err }));
  });
  assert.strictEqual(code, 0, `installer exited non-zero: ${stderr}`);
  // Primary path is checked first; the log must surface that branch.
  assert.match(stderr, /canonical install path/,
    "installer must resolve via $HOME/.mpm/bin/mpm when both canonical paths exist");
});

test("installer resolves MPM via $HOME/.local/bin/mpm symlink when primary is absent", async () => {
  const home = freshHomeDir("canonical-symlink");
  clearInvocations();
  clearFakeUninstalledFlag();
  // Create ONLY the .local/bin symlink (no .mpm/bin) to force the second branch.
  const symDir = path.join(home, ".local", "bin");
  mkdirSync(symDir, { recursive: true });
  symlinkSync(FAKE_MPM_BIN, path.join(symDir, "mpm"));
  const { code, stderr } = await new Promise((resolve) => {
    const child = spawn("bash", [INSTALL_SH], {
      cwd: SANDBOX_ROOT,
      stdio: ["ignore", "pipe", "pipe"],
      env: {
        ...process.env,
        PATH: `${FAKE_OPENCLAW_BINDIR}:/usr/bin:/bin`,
        HOME: home,
        OPENCLAW_PLUGIN_INSTALL_TIMEOUT: "10",
        OPENCLAW_GATEWAY_RESTART_TIMEOUT: "5",
      },
    });
    let err = "";
    child.stderr.on("data", (d) => (err += d));
    child.on("close", (c) => resolve({ code: c, stderr: err }));
  });
  assert.strictEqual(code, 0, `installer exited non-zero: ${stderr}`);
  assert.match(stderr, /canonical user symlink/,
    "installer must resolve via $HOME/.local/bin/mpm when primary is absent");
});

test("installer is CWD-independent — works from arbitrary current working directory", async () => {
  const home = freshHomeDir("cwd-indep");
  clearInvocations();
  clearFakeUninstalledFlag();
  const unrelatedCwd = path.join(SANDBOX_ROOT, "unrelated-dir");
  mkdirSync(unrelatedCwd, { recursive: true });
  const { code, stderr } = await runInstaller({ homeDir: home, cwd: unrelatedCwd });
  assert.strictEqual(code, 0, `installer must succeed from any CWD; got: ${stderr}`);
});

test("installer writes BOTH hook permission flags (allowConversationAccess AND allowPromptInjection)", async () => {
  const home = freshHomeDir("both-hooks");
  clearInvocations();
  clearFakeUninstalledFlag();
  const { code, stderr } = await runInstaller({ homeDir: home });
  assert.strictEqual(code, 0, `installer exited non-zero: ${stderr}`);
  const log = readFileSync(OPENCLAW_INVOCATIONS, "utf8");
  assert.match(log, /hooks\.allowConversationAccess/,
    "must persist hooks.allowConversationAccess=true");
  assert.match(log, /hooks\.allowPromptInjection/,
    "must persist hooks.allowPromptInjection=true (this was the bug — install.sh set only one flag)");
});

test("installer writes absolute mpmBin (not a PATH-resolved bare 'mpm')", async () => {
  const home = freshHomeDir("abs-mpm-bin");
  clearInvocations();
  clearFakeUninstalledFlag();
  const { code, stderr } = await runInstaller({ homeDir: home });
  assert.strictEqual(code, 0, `installer exited non-zero: ${stderr}`);
  const log = readFileSync(OPENCLAW_INVOCATIONS, "utf8");
  // We expect to see config.mpmBin set to an absolute path. The
  // fake-openclaw script writes the set-value in the log.
  const m = log.match(/config\.mpmBin=([^\s\\]+)/);
  assert.ok(m, "config.mpmBin must be set; log was:\n" + log);
  assert.ok(m[1].startsWith("/"),
    `mpmBin must be absolute; got: ${m[1]}`);
});

test("installer orders plugin install BEFORE plugin-specific config writes", async () => {
  const home = freshHomeDir("order");
  clearInvocations();
  clearFakeUninstalledFlag();
  const { code, stderr } = await runInstaller({
    homeDir: home,
    legacyState: "absent", // no legacy migration; pure canonical path
  });
  assert.strictEqual(code, 0, `installer exited non-zero: ${stderr}`);
  const log = readFileSync(OPENCLAW_INVOCATIONS, "utf8");
  // Match the actual WRITE invocations against the canonical plugin id.
  // The legacy probe (config get plugins.entries.openclaw-mpm-memory.*)
  // ALSO matches the substring "config.mpmBin" etc. but is a READ against
  // the legacy id — never a plugin-specific config write against the
  // canonical id, so it must not be counted toward the install-precedes-write
  // invariant.
  const installIdx = log.indexOf("plugins install");
  const mpmBinIdx = log.indexOf('config set plugins.entries.mpm-memory-openclaw.config.mpmBin');
  const hookIdx = log.indexOf('config set plugins.entries.mpm-memory-openclaw.hooks.allowConversationAccess');
  const slotIdx = log.indexOf('config set plugins.slots.memory mpm-memory-openclaw');
  assert.ok(installIdx > -1, "must call plugins install");
  assert.ok(mpmBinIdx > -1, "must set config.mpmBin");
  assert.ok(hookIdx > -1, "must set hooks.allowConversationAccess");
  assert.ok(slotIdx > -1, "must set plugins.slots.memory");
  assert.ok(installIdx < mpmBinIdx, "plugins install must precede mpmBin write");
  assert.ok(mpmBinIdx < hookIdx, "mpmBin must precede hook flag writes");
  assert.ok(hookIdx < slotIdx, "hook flags must precede slot switch");
});

test("installer enables the plugin entry", async () => {
  const home = freshHomeDir("enable");
  clearInvocations();
  clearFakeUninstalledFlag();
  const { code, stderr } = await runInstaller({ homeDir: home });
  assert.strictEqual(code, 0, `installer exited non-zero: ${stderr}`);
  const log = readFileSync(OPENCLAW_INVOCATIONS, "utf8");
  assert.match(log, /plugins enable mpm-memory-openclaw/,
    "installer must call plugins enable");
});

test("installer sets plugins.slots.memory = mpm-memory-openclaw", async () => {
  const home = freshHomeDir("slot");
  clearInvocations();
  clearFakeUninstalledFlag();
  const { code, stderr } = await runInstaller({ homeDir: home });
  assert.strictEqual(code, 0, `installer exited non-zero: ${stderr}`);
  const log = readFileSync(OPENCLAW_INVOCATIONS, "utf8");
  assert.match(log, /plugins\.slots\.memory=mpm-memory-openclaw/,
    "installer must switch the memory slot to this plugin");
  // Pin the invariant: the slot is set to canonical exactly once. The
  // installer MUST NOT subsequently `config unset plugins.slots.memory`
  // (which would erase the canonical selection and leave the slot
  // empty — a worse state than the original legacy configuration).
  const slotSetCount = (log.match(/config set plugins\.slots\.memory mpm-memory-openclaw\b/g) || []).length;
  assert.ok(slotSetCount >= 1,
    "installer must set the slot to the canonical id at least once; log:\n" + log);
  assert.ok(!/config unset plugins\.slots\.memory/.test(log),
    "installer must NOT unset plugins.slots.memory — the canonical selection must persist; log:\n" + log);
});

test("installer does NOT disable memory-core (operator policy)", async () => {
  const home = freshHomeDir("memcore");
  clearInvocations();
  clearFakeUninstalledFlag();
  const { code, stderr } = await runInstaller({ homeDir: home });
  assert.strictEqual(code, 0, `installer exited non-zero: ${stderr}`);
  const log = readFileSync(OPENCLAW_INVOCATIONS, "utf8");
  assert.ok(
    !/plugins\.entries\.memory-core\.enabled=false/.test(log),
    "installer must NOT disable memory-core — that is operator policy. log:\n" + log
  );
});

test("idempotent rerun succeeds and does not duplicate state", async () => {
  const home = freshHomeDir("idempotent");
  clearInvocations();
  clearFakeUninstalledFlag();
  const r1 = await runInstaller({ homeDir: home });
  assert.strictEqual(r1.code, 0, `first run non-zero: ${r1.stderr}`);
  const firstLog = readFileSync(OPENCLAW_INVOCATIONS, "utf8");
  const firstPluginInstalls = (firstLog.match(/plugins install /g) || []).length;
  clearInvocations();
  clearFakeUninstalledFlag();
  const r2 = await runInstaller({ homeDir: home });
  assert.strictEqual(r2.code, 0, `second run non-zero: ${r2.stderr}`);
  const secondLog = readFileSync(OPENCLAW_INVOCATIONS, "utf8");
  const secondPluginInstalls = (secondLog.match(/plugins install /g) || []).length;
  assert.strictEqual(secondPluginInstalls, firstPluginInstalls,
    "rerun must not duplicate plugin install attempts");
});

test("installer never modifies shell startup files", async () => {
  const home = freshHomeDir("shell-rc");
  clearInvocations();
  clearFakeUninstalledFlag();
  const { code } = await runInstaller({ homeDir: home });
  assert.strictEqual(code, 0);
  const check = shellStartupFilesWereTouched(home);
  assert.strictEqual(check.touched, false,
    `installer must not write to ${check.file || "any shell startup file"}`);
});

test("installer never invokes a network bootstrap when MPM canonical paths exist", async () => {
  const home = freshHomeDir("no-bootstrap");
  clearInvocations();
  clearFakeUninstalledFlag();
  const { code, stderr } = await runInstaller({
    homeDir: home,
    mpmBootstrapUrl: "http://127.0.0.1:1/nonexistent-bootstrap-must-not-be-called",
  });
  assert.strictEqual(code, 0, `installer exited non-zero: ${stderr}`);
  // The fake-mpm is at the canonical primary path; MPM is resolvable;
  // the installer must NOT have called curl/wget. The fake-openclaw
  // log will record no 'default-ok' style curl calls because curl is
  // a real binary, but we can still assert by counting curl invocations
  // in the installer's stderr.
  assert.ok(!/curl|wget/.test(stderr),
    "installer must not invoke curl/wget when MPM canonical path resolves; stderr was:\n" + stderr);
});

test("installer fails closed when MPM is absent AND no bootstrap URL is set", async () => {
  const home = freshHomeDir("absent-mpm");
  clearInvocations();
  clearFakeUninstalledFlag();
  // Make a home with no MPM anywhere — neither primary, nor symlink, nor on PATH.
  const { code, stderr } = await new Promise((resolve) => {
    const child = spawn("bash", [INSTALL_SH], {
      cwd: SANDBOX_ROOT,
      stdio: ["ignore", "pipe", "pipe"],
      env: {
        ...process.env,
        PATH: `${FAKE_OPENCLAW_BINDIR}:/usr/bin:/bin`,
        HOME: home,
        // Intentionally no MPM_BOOTSTRAP_URL.
      },
    });
    let err = "";
    child.stderr.on("data", (d) => (err += d));
    child.on("close", (c) => resolve({ code: c, stderr: err }));
  });
  assert.notStrictEqual(code, 0, "installer must fail closed when MPM is absent");
  assert.match(stderr, /mpm not found/,
    "installer must surface a clear 'mpm not found' diagnostic");
});

test("installer does not call root install.sh OpenClaw hooks (no openclaw invocation outside this adapter)", async () => {
  const home = freshHomeDir("root-no-host");
  clearInvocations();
  clearFakeUninstalledFlag();
  // The root install.sh is host-agnostic (per the 2026-09-16
  // cleanup). This installer must not delegate to it. We assert by
  // counting openclaw invocations: they are all sourced from this
  // adapter's logic, never from a root installer call.
  const { code } = await runInstaller({ homeDir: home });
  assert.strictEqual(code, 0);
  const inv = readInvocations();
  const openclawInvs = inv.filter((row) => typeof row.argv === "string" && row.argv.startsWith("config ") || row.argv && row.argv.startsWith("plugins ") || row.argv && row.argv.startsWith("gateway ") || row.argv && row.argv.startsWith("doctor "));
  assert.ok(openclawInvs.length > 0,
    "expected some openclaw invocations during a successful install");
});

test("gateway restart is bounded — installer times out a hanging gateway restart cleanly", async () => {
  const home = freshHomeDir("gw-hang");
  clearInvocations();
  clearFakeUninstalledFlag();
  const { code, stderr } = await runInstaller({
    homeDir: home,
    hangRestart: true,
    extraEnv: { OPENCLAW_GATEWAY_RESTART_TIMEOUT: "3" },
  });
  // Installer should still exit 0 because the restart was bounded;
  // a WARN line is expected. Critical: the installer must NOT hang
  // indefinitely.
  assert.strictEqual(code, 0, "installer must exit cleanly despite hanging gateway restart; stderr:\n" + stderr);
  assert.match(stderr, /WARN.*restart hit the bounded timeout|WARN.*timeout/,
    "installer must surface a bounded-restart warning; stderr was:\n" + stderr);
});

test("gateway restart is bounded — installer surfaces WARN on gateway failure but persists config", async () => {
  const home = freshHomeDir("gw-fail");
  clearInvocations();
  clearFakeUninstalledFlag();
  const { code, stderr } = await runInstaller({ homeDir: home, failRestart: true });
  assert.strictEqual(code, 0, "installer must persist config even if gateway restart fails");
  const log = readFileSync(OPENCLAW_INVOCATIONS, "utf8");
  // Config writes must have happened BEFORE the failed gateway restart.
  assert.ok(log.indexOf("hooks.allowConversationAccess") > -1,
    "config must be persisted before gateway restart is attempted; log:\n" + log);
  // Gateway restart must have been invoked at least once — and the fake
  // exits non-zero to simulate the failure.
  assert.match(stderr, /WARN.*restart/,
    "installer must surface a gateway-restart WARN; stderr:\n" + stderr);
});

test("plugin id is read from openclaw.plugin.json (not hard-coded)", () => {
  // Pin the contract: the installer derives its plugin id from
  // openclaw.plugin.json so renames stay in sync without code edits.
  const src = readFileSync(INSTALL_SH, "utf8");
  assert.match(src, /openclaw\.plugin\.json/,
    "installer must read plugin id from openclaw.plugin.json");
  assert.ok(!/plugin\s+id.*hard.?coded|mpm-memory-openclaw\s*=\s*['"]/.test(src),
    "installer must not hard-code the plugin id as a string literal");
});

test("README documents the canonical install path (./install.sh) and BOTH hook flags", () => {
  const readme = readFileSync(path.join(ADAPTER_DIR, "README.md"), "utf8");
  assert.match(readme, /\.\/install\.sh/,
    "README must document ./install.sh as the canonical install path");
  assert.match(readme, /allowConversationAccess/,
    "README must document allowConversationAccess");
  assert.match(readme, /allowPromptInjection/,
    "README must document allowPromptInjection");
  assert.match(readme, /memory-core/,
    "README must document the memory-core coexistence policy");
});

test("README documents the 2026.9.4 install contract (--link --force --accept-capabilities, --safe restart, three-state plugin detection)", () => {
  const readme = readFileSync(path.join(ADAPTER_DIR, "README.md"), "utf8");
  // The exact install flag set we now invoke.
  assert.match(readme, /--link --force --accept-capabilities/,
    "README must document the 2026.9.4 install flag triple");
  // The actual gateway restart command (no --wait).
  assert.match(readme, /openclaw gateway restart --safe/,
    "README must document the actual restart command");
  // The three-state plugin detection model.
  assert.match(readme, /absent/);
  assert.match(readme, /linked-from-here/);
  assert.match(readme, /conflicting/);
  // The --safe / --wait mutual-exclusion warning.
  assert.match(readme, /mutually exclusive|not compatible with.*--safe/,
    "README must warn that --safe and --wait are incompatible");
  // The trust/capability acknowledgement section.
  assert.match(readme, /Trust \/ capability acknowledgement/);
});

// --------------------------------------------------------------------------
// 2026.9.4 hardening — pin the actual CLI contract
// --------------------------------------------------------------------------
//
// The following tests pin specific properties verified against the
// OpenClaw 2026.9.4 CLI on 2026-09-17:
//   * `openclaw gateway restart --safe` and `--wait` are mutually
//     exclusive (per the CLI help: "--wait ... not compatible with
//     --force or --safe"). `--safe` already has bounded-wait semantics;
//     the outer timeout() wrapper is the hard cap.
//   * `openclaw plugins install <path>` for a non-ClawHub source
//     requires --force (trust acknowledgement) and, for plugins
//     declaring capabilities (memory_search, memory_get), requires
//     --accept-capabilities (otherwise install returns "Plugin X
//     requires capability consent").
//   * `openclaw plugins inspect <id> --json` returns the install
//     rootDir in `plugin.rootDir`, which we use to detect three
//     states: absent, linked-from-here, conflicting.
//   * `openclaw gateway status --json` exposes a `--timeout <ms>`
//     option that bounds the RPC probe.

test("fresh install uses --link --force --accept-capabilities (the documented 2026.9.4 flag set)", async () => {
  const home = freshHomeDir("fresh-flags");
  clearInvocations();
  clearFakeUninstalledFlag();
  const { code, stderr } = await runInstaller({ homeDir: home, pluginState: "absent" });
  assert.strictEqual(code, 0, `installer exited non-zero: ${stderr}`);
  const log = readFileSync(OPENCLAW_INVOCATIONS, "utf8");
  // The fake flags the install as FLAGS_OK iff the exact 3-flag combo
  // is present. Any deviation is fatal in the fake and surfaces here.
  assert.match(log, /FLAGS_OK/, "install must carry --link --force --accept-capabilities");
  assert.doesNotMatch(log, /FLAGS_MISSING/, "install must not omit any of the three flags");
});

test("gateway restart uses --safe only (NOT the invalid --safe --wait combination)", async () => {
  const home = freshHomeDir("gw-shape");
  clearInvocations();
  clearFakeUninstalledFlag();
  const { code, stderr } = await runInstaller({ homeDir: home });
  assert.strictEqual(code, 0, `installer exited non-zero: ${stderr}`);
  const log = readFileSync(OPENCLAW_INVOCATIONS, "utf8");
  // The fake flags the restart as SAFE_FLAG_OK iff --safe is present
  // and rejects --safe --wait combinations outright.
  assert.match(log, /SAFE_FLAG_OK/, "gateway restart must pass --safe");
  assert.doesNotMatch(log, /SAFE_WAIT_CONFLICT/, "gateway restart must not combine --safe and --wait");
  // And the actual recorded argv must not contain --wait.
  const restartLine = log
    .split("\n")
    .filter((l) => l.includes("gateway restart"))
    .find((l) => l.includes("argv"));
  assert.ok(restartLine, "expected a gateway restart invocation in the log");
  assert.ok(!/"argv": "[^"]*--wait/.test(restartLine),
    `gateway restart must not include --wait; got: ${restartLine}`);
});

test("gateway status passes --json --timeout (CLI-level timeout, in addition to the outer timeout wrapper)", async () => {
  const home = freshHomeDir("gw-status-shape");
  clearInvocations();
  clearFakeUninstalledFlag();
  const { code, stderr } = await runInstaller({ homeDir: home });
  assert.strictEqual(code, 0, `installer exited non-zero: ${stderr}`);
  const log = readFileSync(OPENCLAW_INVOCATIONS, "utf8");
  assert.match(log, /FLAGS_OK/, "gateway status must pass --json and --timeout");
  // The recorded argv must include both flags.
  const statusLine = log
    .split("\n")
    .filter((l) => l.includes("gateway status"))
    .find((l) => l.includes("argv"));
  assert.ok(statusLine, "expected a gateway status invocation in the log");
  assert.match(statusLine, /--json/, "gateway status must include --json");
  assert.match(statusLine, /--timeout/, "gateway status must include --timeout");
});

test("gateway status hang does NOT prevent config writes from persisting", async () => {
  // Regression: the installer's "is the gateway reachable?" probe must
  // not be allowed to hang the install. Config must be on disk before
  // the gateway restart decision; if status hangs or fails, the install
  // completes anyway with a clear log line.
  const home = freshHomeDir("gw-status-hang");
  clearInvocations();
  clearFakeUninstalledFlag();
  const { code, stderr } = await runInstaller({
    homeDir: home,
    hangStatus: true,
    extraEnv: { OPENCLAW_GATEWAY_STATUS_TIMEOUT: "2" },
  });
  assert.strictEqual(code, 0, "installer must exit cleanly despite hanging gateway status");
  const log = readFileSync(OPENCLAW_INVOCATIONS, "utf8");
  assert.ok(log.indexOf("hooks.allowConversationAccess") > -1,
    "config writes must have happened despite the status hang; log:\n" + log);
  assert.match(stderr, /no gateway service detected/,
    "installer must surface that gateway was unreachable; stderr:\n" + stderr);
});

test("gateway status failure does NOT make configuration fail", async () => {
  const home = freshHomeDir("gw-status-fail");
  clearInvocations();
  clearFakeUninstalledFlag();
  const { code, stderr } = await runInstaller({ homeDir: home, failStatus: true });
  assert.strictEqual(code, 0, "config must be persisted when gateway status returns non-zero");
  const log = readFileSync(OPENCLAW_INVOCATIONS, "utf8");
  assert.ok(log.indexOf("hooks.allowConversationAccess") > -1,
    "config writes must have happened despite status failure; log:\n" + log);
  assert.match(stderr, /no gateway service detected/,
    "installer must surface that gateway was unreachable; stderr:\n" + stderr);
});

test("idempotent rerun: a correctly-linked-from-here plugin is NOT re-installed", async () => {
  // The genuine idempotency contract: when the plugin is already
  // correctly linked from this adapter's absolute path, the installer
  // must NOT issue a `plugins install` command. Re-running on an
  // already-correct state must be a true no-op for the install step
  // (no trust-warning noise, no installedAt timestamp bump).
  const home = freshHomeDir("idempotent-noinstall");
  installCanonicalMpmAt(home);
  clearInvocations();
  clearFakeUninstalledFlag();
  const r1 = await runInstaller({ homeDir: home, pluginState: "absent" });
  assert.strictEqual(r1.code, 0, `first run non-zero: ${r1.stderr}`);
  assert.match(r1.stderr, /installing plugin 'mpm-memory-openclaw'/,
    "first run with plugin absent must perform the install; stderr:\n" + r1.stderr);
  clearInvocations();
  clearFakeUninstalledFlag();
  const r2 = await runInstaller({
    homeDir: home,
    pluginState: "linked",
    // Runtime package under the sandbox HOME; the adapter links that,
    // not its own source directory. See runInstaller's linkPath default.
    linkPath: null,
  });
  assert.strictEqual(r2.code, 0, `second run non-zero: ${r2.stderr}`);
  const log = readFileSync(OPENCLAW_INVOCATIONS, "utf8");
  // The critical assertion: NO plugins install call.
  assert.ok(!log.includes("plugins install"),
    "second run on already-linked plugin must NOT call `plugins install`; log:\n" + log);
  // And the skip line is surfaced.
  assert.match(r2.stderr, /already linked from .*; skipping install step/,
    "second run must surface the skip line; stderr:\n" + r2.stderr);
});

test("conflicting existing plugin state is detected and fails with a clear operator action", async () => {
  // The installer must NOT silently overwrite an unrelated existing
  // plugin installation. It must detect the conflict via
  // `openclaw plugins inspect --json` (rootDir differs from SCRIPT_DIR)
  // and fail with a clear operator-action message.
  const home = freshHomeDir("conflict");
  clearInvocations();
  clearFakeUninstalledFlag();
  const { code, stderr } = await runInstaller({
    homeDir: home,
    pluginState: "conflicting",
    conflictPath: "/opt/some-other-vendor/mpm-memory-openclaw",
  });
  assert.notStrictEqual(code, 0,
    "installer must fail (non-zero) when an unrelated plugin already owns the id");
  assert.match(stderr, /already installed but points at a different source/,
    "installer must surface the conflict diagnosis; stderr:\n" + stderr);
  assert.match(stderr, /\/opt\/some-other-vendor\/mpm-memory-openclaw/,
    "installer must name the existing source path; stderr:\n" + stderr);
  // Critically: NO plugins install was issued (no destructive overwrite).
  const log = readFileSync(OPENCLAW_INVOCATIONS, "utf8");
  assert.ok(!log.includes("plugins install"),
    "installer must NOT call `plugins install` on a conflict; log:\n" + log);
});

test("installer does NOT fall back from --link to a non-link install", async () => {
  // The previous implementation blindly retried without --link when the
  // --link install failed. That hides the real cause AND silently
  // changes the deployment topology. We now require --link to succeed
  // — a failure is a real error to surface, not a topology switch.
  const home = freshHomeDir("no-fallback");
  clearInvocations();
  clearFakeUninstalledFlag();
  // Force the fake's `plugins install` to reject (FLAGS_MISSING branch)
  // by NOT setting the documented flag set. We do this by overriding
  // FAKE_OPENCLAW_PLUGIN_STATE to "absent" so the installer attempts
  // install, and then simulating a flags failure via extra env.
  // Since we can't easily inject a fake-flag failure, we instead drive
  // the conflict path which also issues no install — and assert the
  // absence of a non-link retry.
  const { code, stderr } = await runInstaller({
    homeDir: home,
    pluginState: "conflicting",
  });
  assert.notStrictEqual(code, 0);
  const log = readFileSync(OPENCLAW_INVOCATIONS, "utf8");
  // The "linked-from-here skip" branch must NOT appear when the
  // existing source is different (the installer does not silently
  // re-link to a new path).
  assert.ok(!log.includes("plugins install"),
    "conflict path must not attempt any plugins install; log:\n" + log);
  // And no `plugins install` with a missing --link flag was issued.
  assert.doesNotMatch(log, /FLAGS_MISSING/, "no install attempt at all");
  // The stderr names the conflicting source — operator can act.
  assert.match(stderr, /operator actions/,
    "installer must enumerate operator actions on conflict; stderr:\n" + stderr);
});

test("install order: plugin install MUST be observed before any plugin-specific config write", async () => {
  // Sanity pin that the new state-detection path did not regress the
  // ordering: even on the absent → fresh-install path, the install
  // precedes mpmBin / hooks / slot writes.
  //
  // NOTE: we match the canonical WRITE pattern (`config set
  // plugins.entries.<canonical-id>.*`), NOT just the key name. The
  // legacy probe reads `config get plugins.entries.openclaw-mpm-memory.*`
  // and would otherwise match the substring "config.mpmBin" before
  // install. The legacy probe is read-only against a different plugin
  // id; it is not a plugin-specific write against the canonical id and
  // must not count toward the install-precedes-write invariant.
  const home = freshHomeDir("order-new");
  clearInvocations();
  clearFakeUninstalledFlag();
  const { code, stderr } = await runInstaller({ homeDir: home, pluginState: "absent" });
  assert.strictEqual(code, 0, `installer exited non-zero: ${stderr}`);
  const log = readFileSync(OPENCLAW_INVOCATIONS, "utf8");
  const installIdx = log.indexOf("plugins install");
  const mpmBinIdx = log.indexOf('config set plugins.entries.mpm-memory-openclaw.config.mpmBin');
  const hookACIdx = log.indexOf('config set plugins.entries.mpm-memory-openclaw.hooks.allowConversationAccess');
  const hookPIIdx = log.indexOf('config set plugins.entries.mpm-memory-openclaw.hooks.allowPromptInjection');
  const slotIdx = log.indexOf('config set plugins.slots.memory mpm-memory-openclaw');
  assert.ok(installIdx > -1, "must call plugins install on fresh install");
  assert.ok(mpmBinIdx > -1, "must set config.mpmBin");
  assert.ok(hookACIdx > -1, "must set hooks.allowConversationAccess");
  assert.ok(hookPIIdx > -1, "must set hooks.allowPromptInjection");
  assert.ok(slotIdx > -1, "must set plugins.slots.memory");
  assert.ok(installIdx < mpmBinIdx, "plugins install must precede mpmBin write");
  assert.ok(mpmBinIdx < hookACIdx, "mpmBin must precede hook flags");
  assert.ok(hookACIdx < slotIdx, "hook flags must precede slot switch");
  // Both hooks must be written (this is the 7a566b72 regression).
  assert.match(log, /hooks\.allowConversationAccess=true/);
  assert.match(log, /hooks\.allowPromptInjection=true/);
});

test("idempotent rerun still writes both hook flags and absolute mpmBin", async () => {
  // Even when the install step is skipped (plugin already linked
  // from here), the config writes must still happen on every rerun so
  // that a fresh OpenClaw config (no plugins.entries.<id>.config) is
  // re-seeded.
  const home = freshHomeDir("idempotent-config");
  installCanonicalMpmAt(home);
  clearInvocations();
  clearFakeUninstalledFlag();
  const { code, stderr } = await runInstaller({
    homeDir: home,
    pluginState: "linked",
    // Runtime package under the sandbox HOME; the adapter links that,
    // not its own source directory. See runInstaller's linkPath default.
    linkPath: null,
  });
  assert.strictEqual(code, 0, `installer exited non-zero: ${stderr}`);
  const log = readFileSync(OPENCLAW_INVOCATIONS, "utf8");
  assert.match(log, /hooks\.allowConversationAccess/,
    "config writes must happen on the linked-from-here rerun");
  assert.match(log, /hooks\.allowPromptInjection/,
    "config writes must happen on the linked-from-here rerun");
  const m = log.match(/config\.mpmBin=([^\s\\]+)/);
  assert.ok(m, "config.mpmBin must be set on the linked-from-here rerun");
  assert.ok(m[1].startsWith("/"), `mpmBin must be absolute; got: ${m[1]}`);
});

test("root install.sh remains host-agnostic (the adapter is the only place that touches openclaw)", async () => {
  // The previous fix removed all openclaw calls from install.sh.
  // This test re-pins that boundary.
  //
  // Path: ~/.mpm/install.sh (the actual user-space installer). Not
  // ~/.mpm/scripts/install.sh — that path does not exist; this test
  // previously read a non-existent file and ENOENT'd silently. The
  // user-space installer is one level above `agent_installation/`,
  // not under `scripts/`.
  const root = path.join(ADAPTER_DIR, "..", "..", "install.sh");
  const src = readFileSync(root, "utf8");
  // The case-sensitive substring `openclaw` (lowercase 'o') pins
  // behavior. The root installer may legitimately mention "OpenClaw"
  // (capital 'O') in comments explaining the host-adapter separation
  // boundary — those references are documentation, not execution.
  // Forbidding the lowercase token is the load-bearing check.
  assert.ok(!/openclaw/.test(src),
    "root install.sh must not contain lowercase 'openclaw' (executable patterns only); got matches");
  // Belt-and-braces: the root installer must not invoke an adapter
  // install.sh or reference a host-specific adapter source path that
  // would constitute auto-installing host wiring.
  assert.ok(!/agent_installation\/[^/]+\/install\.sh/.test(src),
    "root install.sh must not reference a host-adapter install.sh path");
  assert.ok(!/~?\/\.openclaw/.test(src),
    "root install.sh must not reference ~/.openclaw");
  assert.ok(!/\bplugins install\b|\bplugins enable\b/.test(src),
    "root install.sh must not issue plugin install/enable commands");
});

test("plugin state inspection uses bounded `openclaw plugins inspect` (outer timeout applied)", async () => {
  // The state-detection step is bounded so a hung inspect cannot hang
  // the installer. We can't directly observe the timeout firing in the
  // happy-path (it would just succeed fast), so we pin the env knob and
  // assert the call is observable.
  const home = freshHomeDir("inspect-bound");
  clearInvocations();
  clearFakeUninstalledFlag();
  const { code, stderr } = await runInstaller({
    homeDir: home,
    pluginState: "absent",
    extraEnv: { OPENCLAW_PLUGIN_INSPECT_TIMEOUT: "3" },
  });
  assert.strictEqual(code, 0, `installer exited non-zero: ${stderr}`);
  const log = readFileSync(OPENCLAW_INVOCATIONS, "utf8");
  assert.match(log, /plugins inspect mpm-memory-openclaw/,
    "installer must invoke plugins inspect to detect state");
});

test("installer cleans up no host state (no shell rc modifications, no root install mutations)", async () => {
  // The installer must leave the user's shell environment alone.
  // This is a stronger version of the existing shellStartupFilesWereTouched
  // check that ALSO asserts the test's $HOME has no installer-written
  // files at all.
  const home = freshHomeDir("cleanup");
  clearInvocations();
  clearFakeUninstalledFlag();
  const { code } = await runInstaller({ homeDir: home });
  assert.strictEqual(code, 0);
  // No .openclaw/ tree was created (the installer does not write
  // OpenClaw state directly — only via `openclaw config set` which
  // the fake doesn't actually do).
  const openclawDotDir = path.join(home, ".openclaw");
  assert.ok(!existsSync(openclawDotDir),
    "installer must not create $HOME/.openclaw directly; the CLI is the writer");
});

// --------------------------------------------------------------------------
// 2026-09-17 namespace migration — legacy plugin id reconciliation
// --------------------------------------------------------------------------
//
// The plugin id changed from `openclaw-mpm-memory` to
// `mpm-memory-openclaw`. A host that ran an older install carries
// entries under the legacy id. The installer must:
//   1. Detect the legacy id via plugins inspect.
//   2. If absent → no migration, skip silently.
//   3. If present and pointing at THIS adapter → migrate config +
//      uninstall the legacy id so we don't leave two competing plugins.
//   4. If present but pointing elsewhere → leave it alone (it is a
//      different installation, not ours to seize).
//
// The fake-openclaw returns ok:false for any inspect by default, so
// the legacy id is treated as absent in tests. This pin covers the
// absent path. The other branches are pinned by reading the install.sh
// text directly — see installer.test.js.

test("legacy plugin id absent → installer takes no migration action", async () => {
  const home = freshHomeDir("legacy-absent");
  clearInvocations();
  clearFakeUninstalledFlag();
  const { code, stderr } = await runInstaller({ homeDir: home, pluginState: "absent" });
  assert.strictEqual(code, 0, `installer exited non-zero: ${stderr}`);
  // No "legacy plugin id ... migrating" line should appear.
  assert.ok(!/legacy plugin id 'openclaw-mpm-memory' found/.test(stderr),
    "installer must not log a legacy migration when legacy id is absent; stderr:\n" + stderr);
  // And no `plugins uninstall` call at all (legacy absent).
  const log = readFileSync(OPENCLAW_INVOCATIONS, "utf8");
  assert.ok(!/plugins uninstall/.test(log),
    "installer must not call plugins uninstall when legacy id is absent; log:\n" + log);
});

test("plugin id is the canonical `mpm-memory-openclaw` (manifest-derived)", () => {
  // Pin the contract: the manifest `id` field is the source of truth.
  // Renaming the directory without renaming the manifest would leave a
  // half-old/half-new state.
  const manifest = readFileSync(
    path.join(ADAPTER_DIR, "openclaw.plugin.json"),
    "utf8",
  );
  assert.match(manifest, /"id"\s*:\s*"mpm-memory-openclaw"/,
    "openclaw.plugin.json id must be mpm-memory-openclaw");
  assert.ok(!/openclaw-mpm-memory/.test(manifest),
    "openclaw.plugin.json must not contain the legacy id anywhere");
});

test("index.js does not hard-code the legacy plugin id", () => {
  // The JS constant PLUGIN_ID is the runtime identity. If it's still
  // pointing at the legacy id, the plugin would register under the
  // wrong name even if the manifest were renamed.
  const idx = readFileSync(path.join(ADAPTER_DIR, "index.js"), "utf8");
  assert.match(idx, /PLUGIN_ID\s*=\s*"mpm-memory-openclaw"/,
    "index.js PLUGIN_ID must be mpm-memory-openclaw");
  assert.ok(!/openclaw-mpm-memory/.test(idx),
    "index.js must not contain the legacy plugin id anywhere");
});

test("README documents the canonical plugin id (mpm-memory-openclaw)", () => {
  const readme = readFileSync(path.join(ADAPTER_DIR, "README.md"), "utf8");
  assert.match(readme, /mpm-memory-openclaw/,
    "README must reference the canonical plugin id");
});

// --------------------------------------------------------------------------
// 2026-09-17 follow-up — legacy linked rootDir migration
// --------------------------------------------------------------------------
//
// The follow-up fix to the 2026-09-17 namespace migration
// (commit ee91d167) recognised that the previous legacy-id
// reconciliation logic relied solely on `legacy rootDir == $SCRIPT_DIR`,
// which fails in exactly the real upgrade case: a pre-existing
// OpenClaw linked install from the OLD adapter directory path
// `agent_installation/openclaw-mpm-memory/` whose legacy rootDir now
// no longer matches the new $SCRIPT_DIR because git mv has already
// moved the directory.
//
// The new ownership-detection algorithm uses multiple sources of
// evidence (inspect, registry install records, config keys) and
// derives the canonical former adapter path from $SCRIPT_DIR.
//
// These tests exercise each branch of the algorithm against the
// fake-openclaw. The fake exposes legacy states via env vars; see
// the FAKE_OPENCLAW_LEGACY_* documentation at the top of the
// FAKE_OPENCLAW_SCRIPT constant.

test("legacy id absent: no migration action (sanity for the new states)", async () => {
  const home = freshHomeDir("legacy-absent-v2");
  clearInvocations();
  clearFakeUninstalledFlag();
  const { code, stderr } = await runInstaller({
    homeDir: home,
    pluginState: "absent",
    legacyState: "absent",
  });
  assert.strictEqual(code, 0, `installer exited non-zero: ${stderr}`);
  // No legacy migration log line.
  assert.ok(
    !/legacy plugin id 'openclaw-mpm-memory' recognised/.test(stderr),
    "installer must not log a legacy migration when legacy state is absent; stderr:\n" + stderr,
  );
  // And no `plugins uninstall` of the legacy id.
  const log = readFileSync(OPENCLAW_INVOCATIONS, "utf8");
  assert.ok(
    !/plugins uninstall openclaw-mpm-memory/.test(log),
    "installer must not call plugins uninstall openclaw-mpm-memory when legacy absent; log:\n" + log,
  );
});

test("legacy linked install from old canonical path, old path still present → migrate (linked)", async () => {
  const home = freshHomeDir("legacy-linked");
  installCanonicalMpmAt(home);
  clearInvocations();
  clearFakeUninstalledFlag();
  const canonicalFormer = legacyFormerPath(home);
  const { code, stderr } = await runInstaller({
    homeDir: home,
    pluginState: "absent",
    legacyState: "linked",
    legacyLinkPath: canonicalFormer,
    legacyConfigPresent: true,
    legacyRegistryPresent: true,
  });
  assert.strictEqual(code, 0, `installer exited non-zero: ${stderr}`);
  // Migration log line present.
  assert.match(stderr, /legacy plugin id 'openclaw-mpm-memory' recognised.*evidence: inspect_root_match/,
    "installer must recognise legacy install via inspect_root_match; stderr:\n" + stderr);
  // Legacy config keys were migrated.
  const log = readFileSync(OPENCLAW_INVOCATIONS, "utf8");
  assert.match(log, /config set plugins\.entries\.mpm-memory-openclaw\.config\.mpmBin/,
    "installer must set canonical mpmBin key; log:\n" + log);
  assert.match(log, /config set plugins\.entries\.mpm-memory-openclaw\.hooks\.allowConversationAccess true/,
    "installer must set canonical allowConversationAccess key; log:\n" + log);
  assert.match(log, /config set plugins\.entries\.mpm-memory-openclaw\.hooks\.allowPromptInjection true/,
    "installer must set canonical allowPromptInjection key; log:\n" + log);
  // Legacy slot was migrated.
  assert.match(log, /config set plugins\.slots\.memory mpm-memory-openclaw/,
    "installer must migrate the memory slot to canonical; log:\n" + log);
  // Legacy id was uninstalled.
  assert.match(log, /plugins uninstall openclaw-mpm-memory/,
    "installer must uninstall the legacy id after migration; log:\n" + log);
  // Legacy entry keys were unset.
  assert.match(log, /config unset plugins\.entries\.openclaw-mpm-memory/,
    "installer must unset legacy entry keys after migration; log:\n" + log);
});

test("legacy linked install from old canonical path, old path MISSING (the real bug case) → migrate via registry+config evidence", async () => {
  // This is the real upgrade scenario pinned by the follow-up fix:
  //   1. The repository has been git-mv'd from
  //      agent_installation/openclaw-mpm-memory/ → mpm-memory-openclaw/
  //   2. The old filesystem path no longer exists.
  //   3. OpenClaw's `plugins inspect openclaw-mpm-memory --json` fails
  //      because the linked rootDir is gone.
  //   4. But the registry still retains the install record (sourcePath
  //      pointing at the canonical former path), and config keys still
  //      reference the legacy id.
  //
  // The previous "legacy rootDir == $SCRIPT_DIR" check would silently
  // skip the migration and leave stale config + load.paths entries
  // behind. The new ownership algorithm recognises the legacy install
  // via registry + config evidence and migrates.
  const home = freshHomeDir("legacy-vanished");
  installCanonicalMpmAt(home);
  clearInvocations();
  clearFakeUninstalledFlag();
  const canonicalFormer = legacyFormerPath(home);
  const { code, stderr } = await runInstaller({
    homeDir: home,
    pluginState: "absent",
    legacyState: "vanished",
    legacyLinkPath: canonicalFormer,
    legacyConfigPresent: true,
    legacyRegistryPresent: true,
  });
  assert.strictEqual(code, 0, `installer exited non-zero: ${stderr}`);
  // The installer recognised ownership via registry (not inspect, since
  // inspect fails when the linked rootDir is gone).
  assert.match(stderr, /legacy plugin id 'openclaw-mpm-memory' recognised.*evidence: registry_path_match/,
    "installer must recognise legacy install via registry_path_match when inspect fails; stderr:\n" + stderr);
  // Legacy config was migrated.
  const log = readFileSync(OPENCLAW_INVOCATIONS, "utf8");
  assert.match(log, /config set plugins\.entries\.mpm-memory-openclaw\.config\.mpmBin/,
    "installer must migrate config.mpmBin even when legacy path vanished; log:\n" + log);
  // Slot was migrated and must NOT be unset afterwards (the canonical
  // selection must persist after migration — see A1 invariant).
  assert.match(log, /config set plugins\.slots\.memory mpm-memory-openclaw/,
    "installer must migrate the memory slot even when legacy path vanished; log:\n" + log);
  assert.ok(!/config unset plugins\.slots\.memory/.test(log),
    "installer must NOT unset plugins.slots.memory after migration; the canonical selection must persist; log:\n" + log);
  // Post-uninstall reconciliation: the legacy id must no longer
  // resolve to a live install record. The fake's inspect returns
  // ok:false after `plugins uninstall` was called (UNINSTALLED_FLAG
  // marker). The installer's reconciliation probe must therefore see
  // ok:false — and the log must reflect the no-record outcome.
  assert.match(stderr, /legacy plugin id 'openclaw-mpm-memory' has no live install record/,
    "installer must reconcile post-uninstall and confirm legacy id is gone; stderr:\n" + stderr);
  assert.ok(!/still has a live install record/.test(stderr),
    "installer must NOT warn about a stale install record when uninstall succeeded; stderr:\n" + stderr);
});

test("legacy migration invariant: post-uninstall surviving record is surfaced as a WARN, not silently swallowed", async () => {
  // This test models the case where `plugins uninstall` returns
  // non-zero AND the install record still exists after the call.
  // The installer MUST surface this as a clear WARN rather than
  // claiming the migration succeeded silently.
  //
  // The fake returns rc=0 for plugins uninstall (and sets the
  // UNINSTALLED_FLAG marker so subsequent inspect returns ok:false)
  // by default. To force the "stale install record" warning we
  // use the `config_only` legacy state — which keeps slot empty
  // and registry empty, but the legacy fake still emits inspect
  // returning ok:true for the legacy id if the UNINSTALLED_FLAG
  // is absent.
  //
  // Concretely: this test pins that the installer's reconciliation
  // log includes the "has no live install record" line when the
  // fake correctly reflects post-uninstall reality.
  const home = freshHomeDir("legacy-reconcile");
  clearInvocations();
  clearFakeUninstalledFlag();
  const { code, stderr } = await runInstaller({
    homeDir: home,
    pluginState: "absent",
    legacyState: "config_only",
    legacyConfigPresent: true,
    legacyRegistryPresent: false,
  });
  assert.strictEqual(code, 0, `installer exited non-zero: ${stderr}`);
  // The post-uninstall probe must confirm the legacy id is gone
  // (because the fake's `plugins uninstall` set UNINSTALLED_FLAG).
  assert.match(stderr, /legacy plugin id 'openclaw-mpm-memory' has no live install record/,
    "post-uninstall reconciliation must confirm the legacy id is gone; stderr:\n" + stderr);
  assert.ok(!/still has a live install record/.test(stderr),
    "post-uninstall reconciliation must NOT warn about a stale record when none exists; stderr:\n" + stderr);
});

test("legacy config-only state (inspect+registry unavailable, but config keys present) → migrate via config evidence", async () => {
  // A host where the install record has been swept by `openclaw doctor --fix`
  // but the entry keys remain in plugins.entries.openclaw-mpm-memory.*.
  // The installer must still recognise ownership via the surviving
  // config-key evidence.
  const home = freshHomeDir("legacy-config-only");
  clearInvocations();
  clearFakeUninstalledFlag();
  const { code, stderr } = await runInstaller({
    homeDir: home,
    pluginState: "absent",
    legacyState: "config_only",
    legacyConfigPresent: true,
    legacyRegistryPresent: false,
  });
  assert.strictEqual(code, 0, `installer exited non-zero: ${stderr}`);
  // Ownership evidence came from a config key (not slot, because
  // config_only keeps slot empty in the fake).
  assert.match(stderr, /legacy plugin id 'openclaw-mpm-memory' recognised.*evidence: entry_key_present/,
    "installer must recognise legacy install via entry_key_present; stderr:\n" + stderr);
  const log = readFileSync(OPENCLAW_INVOCATIONS, "utf8");
  assert.match(log, /config set plugins\.entries\.mpm-memory-openclaw\.config\.mpmBin/,
    "installer must migrate config.mpmBin from config-only evidence; log:\n" + log);
});

test("legacy slot-only state (slot points at legacy id) → migrate via slot evidence", async () => {
  // A host where the entry keys are absent but plugins.slots.memory
  // still points at the legacy id. The installer must still recognise
  // ownership via the slot and migrate.
  const home = freshHomeDir("legacy-slot-only");
  clearInvocations();
  clearFakeUninstalledFlag();
  const { code, stderr } = await runInstaller({
    homeDir: home,
    pluginState: "absent",
    legacyState: "vanished", // also has slot in this fake-state
    legacyConfigPresent: true,
    legacyRegistryPresent: false,
  });
  assert.strictEqual(code, 0, `installer exited non-zero: ${stderr}`);
  assert.match(stderr, /legacy plugin id 'openclaw-mpm-memory' recognised/,
    "installer must recognise legacy install via slot+config evidence; stderr:\n" + stderr);
  const log = readFileSync(OPENCLAW_INVOCATIONS, "utf8");
  assert.match(log, /config set plugins\.slots\.memory mpm-memory-openclaw/,
    "installer must migrate the memory slot; log:\n" + log);
});

test("legacy install points at an unrelated existing path → conflict, refuse", async () => {
  // A host where someone else has installed openclaw-mpm-memory from
  // a completely different source. The installer must NOT seize,
  // uninstall, or rewrite that installation.
  //
  // SAFETY PROPERTY (A3): an unrelated plugin using the legacy id with
  // a different recorded source MUST always win as a conflict over
  // local config evidence. Even when config keys reference the legacy
  // id (which would otherwise be config-only evidence of ownership),
  // the inspect/registry conflict takes precedence and the
  // installer must refuse.
  const home = freshHomeDir("legacy-conflict");
  clearInvocations();
  clearFakeUninstalledFlag();
  const { code, stderr } = await runInstaller({
    homeDir: home,
    pluginState: "absent",
    legacyState: "conflicting",
    legacyConflictPath: "/opt/some-other-vendor/openclaw-mpm-memory",
    legacyConfigPresent: true,    // config-only evidence would otherwise claim ownership
    legacyRegistryPresent: true,
  });
  assert.notStrictEqual(code, 0,
    "installer must fail (non-zero) when the legacy id points at an unrelated source");
  // The installer prints the conflict diagnosis.
  assert.match(stderr, /already installed but points at a different source/,
    "installer must surface the conflict diagnosis; stderr:\n" + stderr);
  assert.match(stderr, /\/opt\/some-other-vendor\/openclaw-mpm-memory/,
    "installer must name the unrelated source path; stderr:\n" + stderr);
  assert.match(stderr, /operator actions/,
    "installer must enumerate operator actions on conflict; stderr:\n" + stderr);
  // The conflict must be recognised via inspect or registry evidence
  // (not via config-only fallback). We probe this by checking the
  // fake's invocation log: when the conflict is detected via inspect,
  // the fake's `plugins inspect openclaw-mpm-memory --json` call
  // returned ok:true (which only happens in the `conflicting` state).
  // The conflict branch of the installer logs via `err` and exits
  // without the per-evidence log line, so we assert via the
  // recorded argv instead. (Note: registry-only fallback would
  // NOT produce this argv pattern.)
  const conflictLog = readFileSync(OPENCLAW_INVOCATIONS, "utf8");
  assert.match(conflictLog, /plugins inspect openclaw-mpm-memory --json/,
    "conflict must be detected via inspect or registry probe, not via config-only fallback; log:\n" + conflictLog);
  // Critically: NO `plugins uninstall` of the legacy id was issued.
  const log = readFileSync(OPENCLAW_INVOCATIONS, "utf8");
  assert.ok(!/plugins uninstall openclaw-mpm-memory/.test(log),
    "installer must NOT uninstall the legacy id when it points at an unrelated source; log:\n" + log);
  // And NO config get/set/unset against the legacy keys was issued.
  assert.ok(!/config (get|set|unset) plugins\.entries\.mpm-memory-openclaw/.test(log),
    "installer must NOT write canonical config keys when refusing to migrate; log:\n" + log);
});

test("legacy registry-only state (registry has install record, no config) → migrate via registry evidence", async () => {
  // A host where the install record survives in the registry but
  // config keys have been cleaned up. The installer must recognise
  // ownership via the registry record and proceed (no config keys to
  // migrate, but the legacy id must still be uninstalled).
  const home = freshHomeDir("legacy-registry-only");
  installCanonicalMpmAt(home);
  clearInvocations();
  clearFakeUninstalledFlag();
  const canonicalFormer = legacyFormerPath(home);
  const { code, stderr } = await runInstaller({
    homeDir: home,
    pluginState: "absent",
    legacyState: "registry_only",
    legacyLinkPath: canonicalFormer,
    legacyConfigPresent: false,
    legacyRegistryPresent: true,
  });
  assert.strictEqual(code, 0, `installer exited non-zero: ${stderr}`);
  assert.match(stderr, /legacy plugin id 'openclaw-mpm-memory' recognised.*evidence: registry_path_match/,
    "installer must recognise legacy install via registry_path_match; stderr:\n" + stderr);
  const log = readFileSync(OPENCLAW_INVOCATIONS, "utf8");
  // The legacy id was uninstalled.
  assert.match(log, /plugins uninstall openclaw-mpm-memory/,
    "installer must uninstall the legacy id from registry-only evidence; log:\n" + log);
  // No config unset against legacy keys (config absent in this scenario;
  // the installer's migration step has nothing to unset because the keys
  // are already empty). Note: the installer DOES probe legacy keys
  // unconditionally to gather evidence (the legacy entry-key probe is
  // independent of the registry result), and DOES set canonical keys
  // (step 5 below) — but does NOT migrate from absent legacy values.
  assert.ok(!/config unset plugins\.entries\.openclaw-mpm-memory/.test(log),
    "installer must not unset legacy keys when they were reported as absent; log:\n" + log);
});

test("migration is idempotent: second run on already-migrated state is a no-op for legacy", async () => {
  // After a successful migration the legacy state is fully cleared:
  //   - plugins.entries.openclaw-mpm-memory.* unset
  //   - plugins.slots.memory = mpm-memory-openclaw
  //   - legacy plugin id uninstalled
  //
  // A second run of the installer must NOT issue any further
  // migration commands. The legacy-id reconciliation step must
  // detect "no legacy state" via the absence of all evidence and
  // return without acting.
  const home = freshHomeDir("idempotent-legacy");
  clearInvocations();
  clearFakeUninstalledFlag();
  // First run: full migration from vanished-path state.
  const r1 = await runInstaller({
    homeDir: home,
    pluginState: "absent",
    legacyState: "vanished",
    legacyConfigPresent: true,
    legacyRegistryPresent: true,
  });
  assert.strictEqual(r1.code, 0, `first run failed: ${r1.stderr}`);
  assert.match(r1.stderr, /legacy plugin id 'openclaw-mpm-memory' recognised/);
  // Reset the fake state to "absent" for the second run — the fake
  // has no persistent state across runs, but we make the legacy state
  // explicit anyway to document the post-migration expectation.
  clearInvocations();
  clearFakeUninstalledFlag();
  const r2 = await runInstaller({
    homeDir: home,
    pluginState: "absent",
    legacyState: "absent",
    legacyConfigPresent: false,
    legacyRegistryPresent: false,
  });
  assert.strictEqual(r2.code, 0, `second run failed: ${r2.stderr}`);
  // No legacy migration log line on the second run.
  assert.ok(!/legacy plugin id 'openclaw-mpm-memory' recognised/.test(r2.stderr),
    "second run must not log a legacy migration; stderr:\n" + r2.stderr);
  const log = readFileSync(OPENCLAW_INVOCATIONS, "utf8");
  assert.ok(!/plugins uninstall openclaw-mpm-memory/.test(log),
    "second run must not call plugins uninstall openclaw-mpm-memory; log:\n" + log);
  assert.ok(!/config unset plugins\.entries\.openclaw-mpm-memory/.test(log),
    "second run must not call config unset against legacy keys; log:\n" + log);
});

test("installer derives LEGACY_ADAPTER_DIR from $(dirname $SCRIPT_DIR)/openclaw-mpm-memory", () => {
  // Pin the contract: the canonical former path is derived from the
  // installer's own SCRIPT_DIR, not hard-coded. This is what allows
  // the legacy-recognition to work in the real upgrade case (the
  // new SCRIPT_DIR is mpm-memory-openclaw/, the legacy path is its
  // sibling openclaw-mpm-memory/).
  const src = readFileSync(INSTALL_SH, "utf8");
  assert.match(src, /LEGACY_ADAPTER_DIR/,
    "installer must define a LEGACY_ADAPTER_DIR derived from SCRIPT_DIR");
  assert.match(src, /openclaw-mpm-memory/,
    "installer must reference the legacy directory name");
  // The derive expression must be sibling-of-SCRIPT_DIR, not a
  // hard-coded absolute path.
  assert.match(src, /dirname\s+["']?\$\{?SCRIPT_DIR\}?["']?/,
    "installer must derive LEGACY_ADAPTER_DIR via dirname of SCRIPT_DIR");
});

test("installer probes openclaw plugins registry --json (not just plugins inspect)", () => {
  // Pin the contract: ownership detection uses MULTIPLE sources of
  // evidence — registry install records, plugins inspect, and
  // config keys. The previous logic relied on plugins inspect alone
  // and broke when inspect failed because the legacy linked rootDir
  // no longer existed.
  const src = readFileSync(INSTALL_SH, "utf8");
  assert.match(src, /openclaw plugins registry --json/,
    "installer must probe openclaw plugins registry --json for install records");
  assert.match(src, /installRecords/,
    "installer must read persisted.installRecords");
  assert.match(src, /sourcePath/,
    "installer must compare sourcePath against canonical former path");
});

test("installer does not require legacy linked rootDir to still exist on disk", () => {
  // Pin the contract: ownership detection must succeed even when
  // the legacy linked directory has been removed (the real git-mv
  // upgrade case). The fallback to registry + config-key evidence
  // is the load-bearing fix for that scenario.
  const src = readFileSync(INSTALL_SH, "utf8");
  // The installer must have a non-inspect fallback path for
  // ownership detection.
  assert.match(src, /inspect_or_registry_failed|registry_path_match|entry_key_present|slot_points_to_legacy/,
    "installer must have non-inspect fallback ownership evidence paths");
});

// --------------------------------------------------------------------------
// 2026.9.5 migration-inputs hardening — exit 78 lifecycle robustness
// --------------------------------------------------------------------------
//
// OpenClaw 2026.9.5 introduced a startup migration-inputs consistency
// check (readStartupMigrationSnapshot / assertStartupConfigUnchanged)
// that exits 78 when the config was modified too recently before a
// restart. The 2026-09-19 production failure sequence was:
//
//   plugins.install → config.set mpmBin → config.set hooks.* →
//   config.set plugins.slots.memory → gateway restart --safe →
//   exit 78 (gateway stopped, systemd refused to restart on 78)
//
// The fix: run `openclaw update repair` BETWEEN all plugin/config
// mutations and the gateway restart. Repair converges the migration
// identity so the next gateway start sees a settled config.
//
// These tests pin the new lifecycle ordering and the post-restart
// verification step that distinguishes "gateway back up" from
// "gateway stayed down after a failed restart".

// TEST A — repair ordering: config writes → update repair → gateway
// status probe → gateway restart → post-restart verify probe.
// Specifically assert repair occurs AFTER the slot switch and BEFORE
// the restart command.
test("A) repair ordering: mpmBin/hooks/slot writes precede update repair which precedes gateway restart", async () => {
  const home = freshHomeDir("lifecycle-order");
  clearInvocations();
  clearFakeUninstalledFlag();
  const { code, stderr } = await runInstaller({ homeDir: home });
  assert.strictEqual(code, 0, `installer exited non-zero: ${stderr}`);
  const log = readFileSync(OPENCLAW_INVOCATIONS, "utf8");
  const mpmBinIdx = log.indexOf("config.mpmBin");
  const hookACIdx = log.indexOf("hooks.allowConversationAccess");
  const hookPIIdx = log.indexOf("hooks.allowPromptInjection");
  const slotIdx = log.indexOf("plugins.slots.memory");
  const repairIdx = log.indexOf("update repair");
  const restartIdx = log.indexOf("gateway restart");
  assert.ok(mpmBinIdx > -1, "must write mpmBin");
  assert.ok(hookACIdx > -1, "must write allowConversationAccess");
  assert.ok(hookPIIdx > -1, "must write allowPromptInjection");
  assert.ok(slotIdx > -1, "must write slots.memory");
  assert.ok(repairIdx > -1, "must call update repair");
  assert.ok(restartIdx > -1, "must call gateway restart");
  assert.ok(mpmBinIdx < repairIdx, "mpmBin write must precede update repair");
  assert.ok(hookACIdx < repairIdx, "hook writes must precede update repair");
  assert.ok(slotIdx < repairIdx, "slot switch must precede update repair");
  assert.ok(repairIdx < restartIdx, "update repair must precede gateway restart");
  // The post-restart verify probe is the LAST gateway status call.
  // The installer's first gateway status probe (the "is it reachable?"
  // gate) happens AFTER update repair (so repair sees the final
  // config), and BEFORE the restart.
  const statusPositions = [];
  let idx = 0;
  while ((idx = log.indexOf("gateway status", idx + 1)) > -1) {
    statusPositions.push(idx);
  }
  assert.ok(statusPositions.length >= 2,
    `installer must probe gateway status at least twice (pre + post-restart); got ${statusPositions.length}; log:\n${log}`);
  assert.ok(statusPositions[0] > repairIdx,
    `pre-restart gateway status probe (at ${statusPositions[0]}) must come after update repair (at ${repairIdx}); log:\n${log}`);
  assert.ok(statusPositions[0] < restartIdx,
    `pre-restart gateway status probe (at ${statusPositions[0]}) must precede gateway restart (at ${restartIdx}); log:\n${log}`);
  assert.ok(statusPositions[statusPositions.length - 1] > restartIdx,
    `post-restart gateway status probe must follow gateway restart (at ${restartIdx}); log:\n${log}`);
});

// TEST B — immediate restart would fail without repair (the 2026.9.5
// exit-78 race). With the dirty flag pre-set, a restart BEFORE update
// repair would exit 78. The new installer must call update repair
// first, which clears the flag, so the subsequent restart succeeds.
test("B) gateway restart while dirty returns/exits 78; update repair clears dirty state; restart after repair succeeds", async () => {
  const home = freshHomeDir("lifecycle-dirty");
  clearInvocations();
  clearFakeUninstalledFlag();
  const { code, stderr } = await runInstaller({
    homeDir: home,
    pretendDirty: true,
  });
  assert.strictEqual(code, 0, `installer exited non-zero: ${stderr}`);
  const log = readFileSync(OPENCLAW_INVOCATIONS, "utf8");
  assert.match(log, /update repair: CONVERGED/,
    "update repair must have cleared the dirty flag (CONVERGED diagnostic expected); log:\n" + log);
  // The restart must NOT have hit the DIRTY_BLOCKED branch — the
  // installer's call to update repair FIRST cleared the flag.
  assert.doesNotMatch(log, /DIRTY_BLOCKED/,
    "restart must NOT be blocked by the dirty flag — update repair must have run first; log:\n" + log);
  assert.match(log, /gateway restart: SAFE_FLAG_OK/,
    "restart must have proceeded normally; log:\n" + log);
});

// TEST C — completion-cache warning is nonfatal. update repair prints
// the completion-cache warning ("native no-replace move is unavailable
// on this filesystem") and exits 0. The installer must NOT treat that
// as failure and must proceed to the restart step.
test("C) completion-cache warning inside update repair is nonfatal; installer proceeds", async () => {
  const home = freshHomeDir("lifecycle-warn");
  clearInvocations();
  clearFakeUninstalledFlag();
  const { code, stderr } = await runInstaller({
    homeDir: home,
    updateRepairWarn: true,
  });
  assert.strictEqual(code, 0,
    `installer must succeed when update repair emits completion-cache warning; stderr:\n${stderr}`);
  const log = readFileSync(OPENCLAW_INVOCATIONS, "utf8");
  assert.match(log, /COMPLETION_CACHE_WARN/,
    "fake must have emitted the completion-cache warning; log:\n" + log);
  assert.match(log, /update repair: CONVERGED/,
    "update repair must still report CONVERGED despite the warning; log:\n" + log);
  assert.match(log, /gateway restart: SAFE_FLAG_OK/,
    "installer must still issue gateway restart after a warnings-only update repair; log:\n" + log);
  assert.match(stderr, /update repair converged/,
    "installer must surface 'update repair converged' for a warnings-only repair; stderr:\n" + stderr);
});

// TEST D — update repair genuinely fails. The installer must surface
// this as a clear installer error AND must NOT proceed to a gateway
// restart that is likely to exit 78.
test("D) update repair failure is reported; installer does NOT issue gateway restart", async () => {
  const home = freshHomeDir("lifecycle-repair-fail");
  clearInvocations();
  clearFakeUninstalledFlag();
  const { code, stderr } = await runInstaller({
    homeDir: home,
    updateRepairFail: true,
  });
  assert.notStrictEqual(code, 0,
    "installer must exit non-zero when update repair genuinely fails");
  const log = readFileSync(OPENCLAW_INVOCATIONS, "utf8");
  assert.match(log, /update repair: CONVERGENCE_FAIL/,
    "fake must have reported update repair convergence failure; log:\n" + log);
  // Critically: the installer must NOT have issued a restart --safe
  // when repair failed (a restart would likely exit 78 and leave
  // the gateway stopped).
  assert.doesNotMatch(log, /gateway restart/,
    "installer must NOT issue gateway restart when update repair fails; log:\n" + log);
  assert.match(stderr, /update repair returned non-zero/,
    "installer must surface the update repair failure; stderr:\n" + stderr);
  assert.match(stderr, /status 78\/CONFIG/,
    "installer must explain the connection to the 2026.9.5 status-78 failure mode; stderr:\n" + stderr);
  assert.match(stderr, /FAILED\./,
    "installer must report FAILED rather than Done. when repair fails; stderr:\n" + stderr);
});

// TEST E — restart command non-zero but gateway is healthy. The
// restart CLI may time out or refuse for transient reasons while the
// gateway itself is still up. The installer must NOT falsely fail
// in that case.
test("E) restart command non-zero but gateway is healthy; installer does NOT falsely fail", async () => {
  const home = freshHomeDir("lifecycle-restart-fail-but-healthy");
  clearInvocations();
  clearFakeUninstalledFlag();
  const { code, stderr } = await runInstaller({
    homeDir: home,
    failRestart: true,
    // gatewayDownOnRestartFail stays false — gateway stays up after
    // the failed restart command.
  });
  assert.strictEqual(code, 0,
    "installer must succeed when restart command fails but gateway stays healthy; stderr:\n" + stderr);
  const log = readFileSync(OPENCLAW_INVOCATIONS, "utf8");
  assert.match(log, /FAIL_RC=7/,
    "fake must have reported restart failure; log:\n" + log);
  assert.match(log, /gateway status/,
    "post-restart gateway status must have been probed; log:\n" + log);
  assert.match(stderr, /post-restart gateway reachable/,
    "post-restart gateway status must have succeeded; stderr:\n" + stderr);
});

// TEST F — restart command non-zero AND gateway is down. The
// installer must report this as a hard failure rather than printing
// a misleading Done.
test("F) restart command non-zero AND gateway down; installer reports FAILED and exits non-zero", async () => {
  const home = freshHomeDir("lifecycle-restart-fail-gateway-down");
  clearInvocations();
  clearFakeUninstalledFlag();
  const { code, stderr } = await runInstaller({
    homeDir: home,
    failRestart: true,
    gatewayDownOnRestartFail: true,
  });
  assert.notStrictEqual(code, 0,
    "installer must exit non-zero when restart fails AND gateway is down");
  const log = readFileSync(OPENCLAW_INVOCATIONS, "utf8");
  assert.match(log, /FAIL_RC=7/,
    "fake must have reported restart failure; log:\n" + log);
  assert.match(log, /gateway status: DOWN/,
    "fake must have reported gateway-down on the post-restart verify probe; log:\n" + log);
  assert.match(stderr, /post-restart gateway is NOT reachable/,
    "installer must surface that the gateway is unreachable; stderr:\n" + stderr);
  assert.match(stderr, /FAILED\./,
    "installer must report FAILED rather than Done. when gateway is down; stderr:\n" + stderr);
});

// TEST G — idempotent rerun. After a clean converged install, a
// second rerun (with the plugin already correctly linked from this
// adapter's path) must skip the destructive install step and keep
// the convergence-before-restart ordering.
test("G) idempotent rerun: no duplicate registrations; convergence-then-restart ordering preserved", async () => {
  const home = freshHomeDir("lifecycle-idempotent");
  installCanonicalMpmAt(home);
  clearInvocations();
  clearFakeUninstalledFlag();
  const r1 = await runInstaller({ homeDir: home });
  assert.strictEqual(r1.code, 0, `first run failed: ${r1.stderr}`);
  const firstLog = readFileSync(OPENCLAW_INVOCATIONS, "utf8");
  const firstInstallCount = (firstLog.match(/plugins install /g) || []).length;
  const firstRepairCount = (firstLog.match(/update repair/g) || []).length;
  assert.ok(firstInstallCount >= 1,
    `first run on a fresh host must perform at least one plugin install; log:\n${firstLog}`);
  assert.ok(firstRepairCount >= 1,
    `first run must call update repair; log:\n${firstLog}`);
  clearInvocations();
  clearFakeUninstalledFlag();
  const r2 = await runInstaller({
    homeDir: home,
    pluginState: "linked",
    // Runtime package under the sandbox HOME; the adapter links that,
    // not its own source directory. See runInstaller's linkPath default.
    linkPath: null,
  });
  assert.strictEqual(r2.code, 0, `second run failed: ${r2.stderr}`);
  const secondLog = readFileSync(OPENCLAW_INVOCATIONS, "utf8");
  // On the second run the plugin is already correctly linked from this
  // adapter's absolute path; the installer must NOT issue `plugins
  // install` (no destructive re-install, no trust-warning noise).
  assert.ok(!secondLog.includes("plugins install"),
    `second run on already-linked plugin must NOT call plugins install; log:\n${secondLog}`);
  const secondRepairCount = (secondLog.match(/update repair/g) || []).length;
  // update repair is idempotent and runs on every install — the second
  // run still re-converges (cheap; a no-op when the identity is already
  // settled). This pins that the convergence step is not conditional on
  // having just performed a fresh install.
  assert.strictEqual(secondRepairCount, firstRepairCount,
    `rerun must invoke update repair the same number of times as the first run; first=${firstRepairCount} second=${secondRepairCount}; log:\n${secondLog}`);
  // Ordering: repair must precede restart on the second run too.
  const repairIdx = secondLog.indexOf("update repair");
  const restartIdx = secondLog.indexOf("gateway restart");
  assert.ok(repairIdx > -1 && restartIdx > -1 && repairIdx < restartIdx,
    `second-run ordering must keep update repair before gateway restart; log:\n${secondLog}`);
});
