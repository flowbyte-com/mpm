// lib/workspace.js — canonical MPM_WORKSPACE resolver for the
// mpm-auto-mode-persona-openclaw spawn surface.
//
// Mirrors mpm-critic/main.go:126 (the gold-standard pattern). The
// canonical resolver is process.env.MPM_WORKSPACE; only fall back to
// a default when it is unset (long-lived daemon started without the
// env var). The default mirrors config.GetWorkspace() in
// internal/core/config: $HOME/.mpm.
//
// Why: Node.js spawn() defaults to inheriting process.env, so a
// long-lived daemon started without MPM_WORKSPACE inherits an empty
// value. The mpm subprocess then resolves to "." (mpmcli default),
// which silently targets the wrong workspace. Pinning the default here
// closes the gap without changing the agent surface contract.
//
// PROVENANCE: this adapter is a transport shim for `mpm route --apply`.
// It MUST declare its host framework so the audit hook records
// framework_name="openclaw" instead of falling through to the
// transport default. Without this, every auto-mode/persona selector
// invocation is misattributed and recent_activity cannot distinguish
// openclaw-driven writes from operator-on-shell CLI traffic.
//
// Audit: M-1 (post-M3, 2026-08-31). Stage 2B provenance hardening.
//
// ESM module — the parent package.json declares "type": "module".

import path from "node:path";
import os from "node:os";

export function resolveWorkspace() {
  const env = process.env.MPM_WORKSPACE;
  if (env && env.trim() !== "") return env;
  return path.join(os.homedir(), ".mpm");
}

// MPM_FRAMEWORK_ID is the canonical framework name declared by this
// adapter. It MUST match the value the substrate recognises in
// knownAgentFrameworks (internal/core/activity_classifier.go) so that
// EffectiveActorKind classifies the call as actor_kind="agent"
// rather than the generic "mcp" or "mpm-cli" transport defaults.
export const MPM_FRAMEWORK_ID = "openclaw";

// buildProvenanceEnv returns the env contribution this adapter MUST
// stamp on every `mpm` subprocess invocation. Caller may pass an
// existing env to merge into; this function never overrides a
// caller-supplied framework identity (caller wins — important for
// tests and for any future adapter nesting where an inner call must
// preserve an outer host's provenance).
export function buildProvenanceEnv(extra) {
  const env = {
    ...(extra || {}),
    MPM_PROVENANCE_FRAMEWORK:
      (extra && extra.MPM_PROVENANCE_FRAMEWORK) || MPM_FRAMEWORK_ID,
  };
  return env;
}

// withWorkspace is the canonical "spawn env" helper. It pins
// MPM_WORKSPACE and stamps the adapter's provenance identity. The
// provenance stamp is ONLY applied when the caller has not supplied
// one of its own (caller-supplied MPM_PROVENANCE_FRAMEWORK always
// wins).
export function withWorkspace(env) {
  const base = env || process.env;
  const merged = {
    ...base,
    MPM_WORKSPACE: base.MPM_WORKSPACE || resolveWorkspace(),
  };
  return buildProvenanceEnv(merged);
}
