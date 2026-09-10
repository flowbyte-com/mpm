// src/workspace.ts — canonical MPM_WORKSPACE resolver + provenance env
// builder for the pi-mpm spawn surface.
//
// The workspace resolver mirrors mpm-critic/main.go:126 (the gold-standard
// pattern). MPM_WORKSPACE is read from process.env first, and only falls
// back to a default ($HOME/.mpm, matching config.GetWorkspace() in
// internal/core/config) when the env var is unset. This closes the
// "long-lived daemon started without MPM_WORKSPACE inherits an empty
// value, mpm resolves to '.' (mpmcli default)" silent-orphan-db failure
// mode.
//
// The provenance builder is the canonical MPM_PROVENANCE_FRAMEWORK
// attribution layer. Every Pi-originated `mpm call` spawn inherits:
//
//   MPM_PROVENANCE_FRAMEWORK=pi
//
// which mpmcli.ActiveContextFromEnv reads first (cmd/mpm/call.go and
// internal/core/mpmcli/mpmcli.go) to populate
// ActiveContext.FrameworkName. That, in turn, stamps
// artifact_provenance.framework_name="pi" on every memory, decision,
// lesson, handoff, and tool_invocation the Pi agent writes — and
// filters mpm_context.read_directives via ReadDirectivesForFramework
// so framework-scoped directives reach Pi (and do not leak into other
// frameworks).
//
// Why process.env is preserved through withProvenance: Node.js spawn()
// with an explicit `env:` option REPLACES the parent environment. So if
// we only set MPM_WORKSPACE, the mpm subprocess loses PATH, HOME,
// TZ, etc. — and `mpm` itself can't be found by name. process.env is
// the base; provenance and workspace overlay on top; caller-supplied
// overrides win last.
//
// Audit: M-1 (post-M3, 2026-08-31). Provenance addition 2026-09-10
// (Pi alignment pass) mirrors opencode-mpm/src/index.ts:buildProvenanceEnv
// and openclaw-mpm-memory:resolve_exec_env.
//
// What this layer intentionally does NOT set:
//   - MPM_PROVENANCE_MODEL: the active model is dynamic per turn; a
//     static value would be a fabrication. Pi does not currently
//     expose model on session_start; future iterations can plumb it
//     from ExtensionContext.model.
//   - MPM_PROVENANCE_INVOCATION_ID / MPM_PROVENANCE_PARENT_INVOCATION_ID:
//     per-call correlation IDs that must be generated per invocation.
//     The mpm call dispatcher generates a fresh UUID per call when
//     unset (cmd/mpm/call.go:131); Pi leaves it to mpm to mint.
//   - MPM_PROVENANCE_ACTOR_KIND: hardcoded "agent" by mpm-critic's
//     audit hook; setting it would imply a contract the codebase does
//     not implement.

import * as path from "node:path";
import * as os from "node:os";

/** Canonical framework identifier for Pi in MPM provenance attribution. */
export const MPM_FRAMEWORK_ID = "pi";

export function resolveWorkspace(): string {
  const env = process.env.MPM_WORKSPACE;
  if (env && env.trim() !== "") return env;
  return path.join(os.homedir(), ".mpm");
}

/**
 * Build the canonical Pi provenance env contribution. MPM_PROVENANCE_FRAMEWORK
 * is the only field this layer owns — model, invocation_id, and
 * parent_invocation_id are per-call/per-turn and intentionally NOT
 * populated here (see header comment).
 */
export function buildProvenanceEnv(): Record<string, string> {
  return {
    MPM_PROVENANCE_FRAMEWORK: MPM_FRAMEWORK_ID,
  };
}

/**
 * Compose the full child-process env: process.env (so PATH/HOME/TZ/etc.
 * are inherited) → provenance overlay → workspace overlay → caller
 * overrides.
 *
 * Caller-supplied `extra` wins last so future per-call knobs (e.g. an
 * MPM_PROVENANCE_SESSION_KEY when session_id is available) can be
 * added without changing the call sites.
 */
export function withProvenance(
  extra: Record<string, string> = {},
): Record<string, string> {
  // 1. Base = process.env, with undefined values filtered out.
  const env: Record<string, string> = {};
  for (const [key, value] of Object.entries(process.env)) {
    if (value !== undefined) env[key] = value;
  }
  // 2. Overlay canonical provenance.
  Object.assign(env, buildProvenanceEnv());
  // 3. Overlay caller-supplied env (per-call knobs).
  Object.assign(env, extra);
  // 4. Pin MPM_WORKSPACE last — caller can override but never drop it.
  env.MPM_WORKSPACE = env.MPM_WORKSPACE ?? resolveWorkspace();
  return env;
}

/**
 * Back-compat thin wrapper preserved for any caller that imported the
 * old name. New callers should use withProvenance so process.env is
 * preserved and provenance attribution is stamped.
 *
 * @deprecated Prefer withProvenance — the helper that adds
 * MPM_PROVENANCE_FRAMEWORK=pi and inherits process.env.
 */
export function withWorkspace(
  env: Record<string, string> = {},
): Record<string, string> {
  return { ...env, MPM_WORKSPACE: env.MPM_WORKSPACE ?? resolveWorkspace() };
}
