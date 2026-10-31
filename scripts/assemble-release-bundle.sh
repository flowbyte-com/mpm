#!/usr/bin/env bash
# scripts/assemble-release-bundle.sh — build a verified, publishable
# release bundle from this checkout.
#
# This is the ONLY supported way to produce the tarball the updater
# downloads. It reuses the existing promotion primitives rather than
# redefining them:
#
#   - scripts/build-provenance.sh names the version, commit and epoch;
#   - scripts/lib/runtime_bundle.sh stages the payload (rb_stage_payloads),
#     writes the identity (rb_write_release) and validates the result
#     (rb_validate_bundle);
#   - scripts/install_runtime_assets.py provisions the host assets.
#
# Usage:
#   scripts/assemble-release-bundle.sh --tag <tag> --out <dir>
#
# Output in <out>:
#   mpm_<tag>_linux_amd64.tar.gz   the bundle (bin/, agent_installation/,
#                                  RELEASE.json, schema/)
#   SHA256SUMS                     checksums of the tarball
#
# The bundle directory inside the tarball is the bundle ROOT (no
# top-level wrapper directory): the updater extracts it and validates
# the result with the same five checks install.sh applies.
#
# Exit codes: 0 success, 1 refusal (the reason is on stderr).
set -u
TAG=""
OUT=""
while [ $# -gt 0 ]; do
  case "$1" in
    --tag) TAG="${2:-}"; shift 2 ;;
    --out) OUT="${2:-}"; shift 2 ;;
    -h|--help) sed -n '1,30p' "$0"; exit 0 ;;
    *) echo "assemble-release-bundle: unknown argument: $1" >&2; exit 1 ;;
  esac
done
[ -n "$TAG" ] || { echo "assemble-release-bundle: --tag is required (e.g. --tag v0.2.0)" >&2; exit 1; }
[ -n "$OUT" ] || { echo "assemble-release-bundle: --out is required" >&2; exit 1; }

PROJECT_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BUNDLE_LIB="$PROJECT_ROOT/scripts/lib/runtime_bundle.sh"
[ -f "$BUNDLE_LIB" ] || { echo "assemble-release-bundle: missing $BUNDLE_LIB" >&2; exit 1; }
# shellcheck source=scripts/lib/runtime_bundle.sh
. "$BUNDLE_LIB"

case "$TAG" in
  v*.*) ;;
  *) echo "assemble-release-bundle: tag must look like v0.2.0, got '$TAG'" >&2; exit 1 ;;
esac
case "$(uname -s)-$(uname -m)" in
  Linux-x86_64) PLAT="linux_amd64" ;;
  *) echo "assemble-release-bundle: supported platform is Linux x86_64; refusing to advertise an untested platform" >&2; exit 1 ;;
esac

COMMIT="$(bash "$PROJECT_ROOT/scripts/build-provenance.sh" commit)" || exit 1
VERSION="$(bash "$PROJECT_ROOT/scripts/build-provenance.sh" version)" || exit 1
EPOCH="$(bash "$PROJECT_ROOT/scripts/build-provenance.sh" build-epoch)" || exit 1
case "$EPOCH" in ''|*[!0-9]*) EPOCH=0 ;; esac

WORK="$(mktemp -d "${TMPDIR:-/tmp}/mpm-assemble.XXXXXX")"
CANDIDATE="$WORK/bundle"
mkdir -p "$CANDIDATE/bin" "$OUT"

echo "assembling $TAG ($COMMIT) for $PLAT"

# 1. Binaries, from the developer build. `make build` must have run;
#    the assembler never builds — provenance belongs to one place.
#
#    The OpenCode payload must have been compiled first
#    (`npm ci && npm run build` in agent_installation/mpm-opencode):
#    the bundle ships the COMPILED plugin, and staging a source tree
#    without its build output would register an empty plugin.
for b in mpm mpm-scheduler mpm-critic mpm-mcp mpm-telemetry mpm-update; do
  src="$PROJECT_ROOT/.build/bin/$b"
  [ -x "$src" ] || { echo "assemble-release-bundle: missing $src — run 'make build' first" >&2; rm -rf "$WORK"; exit 1; }
  install -m 0755 "$src" "$CANDIDATE/bin/$b"
done

# 2. Release-scoped host assets.
if ! python3 "$PROJECT_ROOT/scripts/install_runtime_assets.py" \
    --source-root "$PROJECT_ROOT" \
    --runtime-root "$CANDIDATE" \
    --groups host-assets; then
  echo "assemble-release-bundle: host assets failed" >&2; rm -rf "$WORK"; exit 1
fi

# 3. The host runtime payloads (fail-closed, same rule as install.sh).
if ! rb_stage_payloads "$PROJECT_ROOT" "$CANDIDATE"; then
  rm -rf "$WORK"; exit 1
fi

# 4. The identity. The revision is the TAG: releases/<tag> is what the
#    updater installs, and the exact commit rides alongside it.
rb_write_release "$CANDIDATE" "$TAG" "$COMMIT" "$EPOCH" "$VERSION"

# 5. The substrate's own schema, so the preflight travels with the bundle.
if [ -r "$PROJECT_ROOT/scripts/lib/schema_preflight.py" ] && [ -r "$PROJECT_ROOT/internal/core/schema.go" ]; then
  mkdir -p "$CANDIDATE/schema"
  python3 "$PROJECT_ROOT/scripts/lib/schema_preflight.py" extract \
    --go "$PROJECT_ROOT/internal/core/schema.go" \
    --table lifecycle_events \
    --out "$CANDIDATE/schema/lifecycle_events.sql" || { rm -rf "$WORK"; exit 1; }
fi

# 6. Validate with the canonical validator before anything is published.
#    The updater applies the same checks on download; a bundle that
#    fails here would fail there, so refuse it at the source.
if ! rb_validate_bundle "$CANDIDATE" "$TAG"; then
  rm -rf "$WORK"; exit 1
fi

ARCHIVE="mpm_${TAG}_${PLAT}.tar.gz"
tar -C "$CANDIDATE" -czf "$OUT/$ARCHIVE" bin agent_installation RELEASE.json schema 2>/dev/null || \
tar -C "$CANDIDATE" -czf "$OUT/$ARCHIVE" bin agent_installation RELEASE.json || { rm -rf "$WORK"; exit 1; }
(cd "$OUT" && sha256sum "$ARCHIVE" > SHA256SUMS)

echo "wrote $OUT/$ARCHIVE"
cat "$OUT/SHA256SUMS"
rm -rf "$WORK"
