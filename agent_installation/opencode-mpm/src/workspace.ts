// src/workspace.ts — canonical MPM_WORKSPACE resolver for the
// opencode-mpm spawn surface.
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
// Audit: M-1 (post-M3, 2026-08-31).

import * as path from "node:path";
import * as os from "node:os";

export function resolveWorkspace(): string {
  const env = process.env.MPM_WORKSPACE;
  if (env && env.trim() !== "") return env;
  return path.join(os.homedir(), ".mpm");
}

/**
 * Build a child-process env map with MPM_WORKSPACE pinned to the
 * canonical resolver. Caller-supplied env takes precedence over
 * process.env defaults; the workspace helper is the final fallback
 * so any caller can override but none can accidentally drop it.
 */
export function withWorkspace(
  env: Record<string, string> = {},
): Record<string, string> {
  return { ...env, MPM_WORKSPACE: env.MPM_WORKSPACE ?? resolveWorkspace() };
}
