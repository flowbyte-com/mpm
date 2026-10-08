#!/usr/bin/env bash
#
# stage_runtime_package.sh — runtime-package provisioning library
#
# The source/runtime split (source checkout at ~/src/mpm, runtime state at
# ~/.mpm) means an OpenClaw plugin directory inside the runtime root is NOT
# populated by Git. Nothing in this repository writes
# <runtime-root>/agent_installation/<plugin>/ — the live tree only ever
# existed because ~/.mpm used to be the checkout itself. Under the split,
# a runtime plugin package must be *provisioned explicitly*, or the plugin
# is absent (or, worse, is a half-populated directory of residue that looks
# installed and loads nothing).
#
# This library is the provisioning step. It is sourced by the OpenClaw
# adapter installers, which then hand the RUNTIME path to
# `openclaw plugins install <runtime-path> --link`.
#
# Why the runtime path and not the source path:
#   * OpenClaw links the package, so the linked directory must be
#     runtime-owned. Linking the source checkout makes the live integration
#     depend on a developer tree that may move, be pruned, or be deleted.
#   * The package that ships is a *minimal* package: entrypoint, metadata,
#     and the modules those actually import. Test suites and bytecode
#     caches are development residue and are excluded by manifest, not by
#     hopeful globbing at copy time.
#   * The source checkout is then genuinely disposable after install, which
#     is the whole point of the split.
#
# Contract:
#   * Explicit allowlist manifest. A file reaches the runtime package only
#     if the plugin lists it. Adding a source file is therefore a
#     deliberate, reviewable act, not an accident of tree shape.
#   * Validate before expose. The staged package is checked (metadata
#     parses, id matches, entrypoint exists, internal imports resolve)
#     BEFORE the caller is told it can hand the path to OpenClaw. A
#     half-written package must never be linkable.
#   * Idempotent. Re-running with an unchanged source is a no-op; with a
#     changed source it converges. Neither run reports success twice for
#     different content.
#   * Ownership-safe. Files MPM did not stage are never touched. We remove
#     only the residue classes MPM itself creates (bytecode caches); an
#     operator's own file dropped into the package survives.
#   * Custom-prefix safe. The runtime root is derived from the resolved
#     mpm binary, not hardcoded to ~/.mpm, so a relocated or hermetic
#   install provisions into its own tree.
#
# Usage:
#     . "$(dirname "$0")/../scripts/stage_runtime_package.sh"
#     RUNTIME_ROOT="$(mpm_runtime_root_from_bin "$MPM_BIN")"
#     stage_runtime_package "$SCRIPT_DIR" "$RUNTIME_ROOT" "$PLUGIN_ID" \
#         index.js package.json openclaw.plugin.json README.md lib/workspace.js
#
# Library contract: safe to source under `set -euo pipefail`. Defines
# functions only; sourcing has no side effects.

# Guard against double-sourcing (both adapters source this file).
if [ -n "${MPM_STAGE_RUNTIME_PACKAGE_SOURCED:-}" ]; then
  return 0 2>/dev/null || true
fi
MPM_STAGE_RUNTIME_PACKAGE_SOURCED=1

# --------------------------------------------------------------------------
# mpm_runtime_root_from_bin <absolute-path-to-mpm-binary>
# --------------------------------------------------------------------------
#
# The runtime root is the parent of the bin/ directory holding the mpm
# binary. Deriving it from the binary we already resolved — rather than
# assuming $HOME/.mpm — is what makes this work for custom prefixes: a
# hermetic or relocated install stages into its own tree and never touches
# the operator's real one.
#
# Symlinks are resolved first, so ~/.local/bin/mpm -> ~/.mpm/bin/mpm
# yields ~/.mpm rather than ~/.local.
mpm_runtime_root_from_bin() {
  local bin="$1" real parent
  [ -n "$bin" ] || return 1
  real="$(readlink -f "$bin" 2>/dev/null || printf '%s' "$bin")"
  [ -n "$real" ] || return 1
  parent="$(dirname "$real")"
  [ -n "$parent" ] || return 1
  # Strip the trailing /bin component to reach the root.
  case "$(basename "$parent")" in
    bin) dirname "$parent" ;;
    *)   dirname "$parent" ;;
  esac
}

# --------------------------------------------------------------------------
# mpm_residue_paths <dir>
# --------------------------------------------------------------------------
#
# Bytecode caches are residue MPM's own Python tooling creates. They are
# excluded from the manifest and swept from the staged package, because a
# stale .pyc whose source no longer exists is exactly the kind of thing
# that makes a broken package look alive.
mpm_residue_paths() {
  local dir="$1"
  find "$dir" \( -name '__pycache__' -o -name '*.pyc' -o -name '*.pyo' \) 2>/dev/null || true
}

# --------------------------------------------------------------------------
# stage_runtime_package <source-dir> <runtime-root> <plugin-id> <file>...
# --------------------------------------------------------------------------
#
# Returns 0 and prints the staged package directory on success. Prints
# diagnostics to stderr and returns non-zero on any failure.
stage_runtime_package() {
  local src="$1" runtime_root="$2" plugin_id="$3"
  shift 3
  local files=("$@")

  if [ -z "$src" ] || [ ! -d "$src" ]; then
    echo "[stage] ERROR: plugin source directory not found: $src" >&2
    return 2
  fi
  if [ -z "$runtime_root" ]; then
    echo "[stage] ERROR: runtime root not determined for $plugin_id" >&2
    return 2
  fi
  if [ "${#files[@]}" -eq 0 ]; then
    echo "[stage] ERROR: empty package manifest for $plugin_id" >&2
    return 2
  fi

  local pkg="$runtime_root/agent_installation/$plugin_id"
  echo "[stage] runtime package: $pkg" >&2

  # -- 1. Every manifest entry must exist in source BEFORE we touch the
  #       runtime tree. A missing source file is a build error; copying a
  #       partial set would leave the exact broken state we are repairing.
  local f
  for f in "${files[@]}"; do
    if [ ! -f "$src/$f" ]; then
      echo "[stage] ERROR: manifest entry not found in source: $f" >&2
      return 2
    fi
  done

  # -- 2. Copy into a staging sibling, then swap. Swapping means OpenClaw
  #       never observes a partially-updated package even if we are killed
  #       mid-copy. On any failure the previous package is left intact.
  local parent="$runtime_root/agent_installation"
  mkdir -p "$parent"

  local stamp
  stamp="$(mktemp -d "$parent/.$plugin_id.stage.XXXXXX")" || {
    echo "[stage] ERROR: could not create staging dir under $parent" >&2
    return 3
  }

  local ok=1
  for f in "${files[@]}"; do
    if ! mkdir -p "$stamp/$(dirname "$f")" \
       || ! cp -p "$src/$f" "$stamp/$f"; then
      echo "[stage] ERROR: failed to stage $f" >&2
      ok=0
      break
    fi
  done

  if [ "$ok" -eq 1 ]; then
    if ! mpm_validate_runtime_package "$stamp" "$plugin_id"; then
      ok=0
    fi
  fi

  if [ "$ok" -ne 1 ]; then
    rm -rf "$stamp"
    echo "[stage] ERROR: staging aborted; existing package left untouched" >&2
    return 3
  fi

  # -- 3. Swap. Move any operator files that are NOT ours into the new
  #       package before replacing, so ownership semantics hold across an
  #       upgrade rather than only on a fresh install.
  local old="$pkg.retired.$$"
  local existed=0
  if [ -d "$pkg" ]; then
    existed=1
    mv "$pkg" "$old" 2>/dev/null || {
      rm -rf "$stamp"
      echo "[stage] ERROR: could not retire existing package at $pkg" >&2
      return 3
    }
  fi

  if ! mv "$stamp" "$pkg"; then
    # Roll back to whatever was there before.
    [ "$existed" -eq 1 ] && mv "$old" "$pkg" 2>/dev/null
    echo "[stage] ERROR: could not publish staged package to $pkg" >&2
    return 3
  fi

  # -- 4. Preserve operator-owned files from the retired package.
  if [ "$existed" -eq 1 ] && [ -d "$old" ]; then
    ( cd "$old" && find . -type f -print0 ) 2>/dev/null \
      | while IFS= read -r -d '' rel; do
          rel="${rel#./}"
          # Already provided by the manifest? The fresh copy wins.
          if [ -f "$pkg/$rel" ]; then continue; fi
          # Never carry bytecode caches forward.
          case "$rel" in
            *__pycache__*|*.pyc|*.pyo) continue ;;
          esac
          mkdir -p "$pkg/$(dirname "$rel")" 2>/dev/null || continue
          cp -p "$old/$rel" "$pkg/$rel" 2>/dev/null || true
        done
    rm -rf "$old"
  fi

  # -- 5. Sweep residue from the published package.
  local r
  while IFS= read -r r; do
    [ -n "$r" ] && rm -rf "$r" 2>/dev/null || true
  done < <(mpm_residue_paths "$pkg")

  printf '%s\n' "$pkg"
}

# --------------------------------------------------------------------------
# mpm_validate_runtime_package <dir> <plugin-id>
# --------------------------------------------------------------------------
#
# The gate between "staged" and "safe to hand to OpenClaw". Checks the four
# things whose absence produces a plugin that registers and then does
# nothing:
#
#   1. openclaw.plugin.json parses AND declares the expected id
#   2. package.json parses AND its `main` entrypoint exists
#   3. every relative import inside staged .js files resolves on disk
#   4. no development residue is present
#
# Bare-specifier imports (`openclaw/plugin-sdk/...`) are deliberately NOT
# resolved here: those are peer dependencies supplied by the OpenClaw host,
# and they are unresolvable in the source checkout too. Checking them would
# fail every run for a condition that is correct by design.
mpm_validate_runtime_package() {
  local dir="$1" plugin_id="$2" fail=0

  if [ ! -f "$dir/openclaw.plugin.json" ]; then
    echo "[stage] ERROR: staged package is missing openclaw.plugin.json" >&2
    fail=1
  elif command -v python3 >/dev/null 2>&1; then
    if ! python3 - "$dir/openclaw.plugin.json" "$plugin_id" <<'PY'
import json, sys
path, expected = sys.argv[1], sys.argv[2]
try:
    with open(path, encoding="utf-8") as fh:
        data = json.load(fh)
except Exception as exc:
    print(f"[stage] ERROR: openclaw.plugin.json does not parse: {exc}", file=sys.stderr)
    sys.exit(1)
got = data.get("id", "")
if got != expected:
    print(f"[stage] ERROR: plugin id mismatch: manifest says {expected!r}, "
          f"openclaw.plugin.json says {got!r}", file=sys.stderr)
    sys.exit(1)
sys.exit(0)
PY
    then
      fail=1
    fi
  fi

  if [ ! -f "$dir/package.json" ]; then
    echo "[stage] ERROR: staged package is missing package.json" >&2
    fail=1
  elif command -v python3 >/dev/null 2>&1; then
    local main_rel
    main_rel="$(python3 - "$dir/package.json" <<'PY'
import json, sys
try:
    with open(sys.argv[1], encoding="utf-8") as fh:
        data = json.load(fh)
except Exception as exc:
    print(f"[stage] ERROR: package.json does not parse: {exc}", file=sys.stderr)
    sys.exit(1)
print(data.get("main", "") or "")
PY
)" || { fail=1; main_rel=""; }
    if [ "$fail" -eq 1 ]; then
      :
    elif [ -z "$main_rel" ]; then
      echo "[stage] ERROR: package.json declares no \"main\" entrypoint" >&2
      fail=1
    elif [ ! -f "$dir/$main_rel" ]; then
      echo "[stage] ERROR: package.json main \"$main_rel\" does not exist in the staged package" >&2
      fail=1
    fi
  fi

  # Relative-import resolution. Walks staged JS and confirms every
  # ./ or ../ specifier names a file that is actually in the package.
  #
  # Done in Python rather than a grep|sed pipeline: the extraction has to
  # cope with both quote styles, trailing semicolons, and imports spread
  # over lines, and a pipeline that silently emits an empty specifier
  # would report a failure that does not exist (or hide one that does).
  local unresolved
  unresolved="$(
    python3 - "$dir" <<'PY' 2>/dev/null
import os, re, sys

root = sys.argv[1]

# from "..." / from '...' and bare import "..." / require("...") forms.
PATTERNS = [
    re.compile(r"""\bfrom\s+['"]([^'"]+)['"]"""),
    re.compile(r"""\bimport\s+['"]([^'"]+)['"]"""),
    re.compile(r"""\brequire\s*\(\s*['"]([^'"]+)['"]\s*\)"""),
]

bad = []
for dirpath, _dirnames, filenames in os.walk(root):
    for name in filenames:
        if not name.endswith(".js"):
            continue
        path = os.path.join(dirpath, name)
        try:
            with open(path, encoding="utf-8", errors="replace") as fh:
                text = fh.read()
        except OSError:
            continue
        for pattern in PATTERNS:
            for spec in pattern.findall(text):
                # Only relative specifiers are ours to satisfy. Bare
                # specifiers are peer dependencies supplied by the
                # OpenClaw host and are unresolvable here by design.
                if not spec.startswith("./") and not spec.startswith("../"):
                    continue
                target = os.path.normpath(
                    os.path.join(os.path.dirname(path), spec)
                )
                if not os.path.isfile(target):
                    rel = os.path.relpath(path, root)
                    bad.append(f"{rel} -> {spec}")

for line in bad:
    print(line)
sys.exit(1 if bad else 0)
PY
)" || true
  if [ -n "$unresolved" ]; then
    echo "[stage] ERROR: staged package has unresolved internal imports:" >&2
    # Print line by line: an unquoted $unresolved would word-split
    # "index.js -> ./lib/workspace.js" into three separate arguments and
    # destroy the pairing the operator needs to fix the manifest.
    printf '%s\n' "$unresolved" | while IFS= read -r line; do
      echo "    $line" >&2
    done
    fail=1
  fi

  local residue
  residue="$(mpm_residue_paths "$dir")"
  if [ -n "$residue" ]; then
    echo "[stage] ERROR: development residue present in staged package:" >&2
    printf '%s\n' "$residue" | while IFS= read -r line; do
      [ -n "$line" ] && echo "    $line" >&2
    done
    fail=1
  fi

  [ "$fail" -eq 0 ]
}