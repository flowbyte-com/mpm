// tests/provenance.test.js — Stage 2B regression for the
// mpm-auto-mode-persona-openclaw adapter's provenance propagation.
//
// Before this fix, the adapter spawned `mpm route --apply` without
// setting any MPM_PROVENANCE_* env vars, so every audit row landed
// with framework_name falling through to the transport default
// ("mcp" via MCP dispatch, "mpm-cli" via CLI). recent_activity
// consequently recorded these calls as either generic agent or
// operator-on-shell traffic, losing the OpenClaw host identity.
//
// This test pins the contract that withWorkspace() stamps the
// adapter's MPM_FRAMEWORK_ID as MPM_PROVENANCE_FRAMEWORK on every
// spawn, AND that caller-supplied provenance is never overwritten
// (so future adapter nesting preserves outer host identity).

import { test } from "node:test";
import assert from "node:assert/strict";
import {
  resolveWorkspace,
  withWorkspace,
  buildProvenanceEnv,
  MPM_FRAMEWORK_ID,
} from "../lib/workspace.js";

test("MPM_FRAMEWORK_ID is the canonical 'openclaw' framework name", () => {
  assert.strictEqual(MPM_FRAMEWORK_ID, "openclaw");
});

test("withWorkspace() stamps MPM_PROVENANCE_FRAMEWORK=openclaw", () => {
  const env = withWorkspace({});
  assert.strictEqual(
    env.MPM_PROVENANCE_FRAMEWORK,
    "openclaw",
    "every mpm subprocess invoked from this adapter must carry the openclaw " +
    "framework identity; without it, recent_activity loses host attribution."
  );
});

test("withWorkspace() preserves caller-supplied MPM_PROVENANCE_FRAMEWORK", () => {
  const env = withWorkspace({ MPM_PROVENANCE_FRAMEWORK: "outer-host" });
  assert.strictEqual(
    env.MPM_PROVENANCE_FRAMEWORK,
    "outer-host",
    "caller-supplied framework identity must win so future adapter nesting " +
    "preserves outer-host provenance"
  );
});

test("withWorkspace() still pins MPM_WORKSPACE", () => {
  const env = withWorkspace({});
  assert.ok(env.MPM_WORKSPACE, "MPM_WORKSPACE must remain pinned");
  // resolveWorkspace() default is $HOME/.mpm; just confirm a non-empty
  // path-like value.
  assert.match(env.MPM_WORKSPACE, /[/\\]/);
});

test("buildProvenanceEnv({}) is sufficient when caller has no extras", () => {
  const env = buildProvenanceEnv();
  assert.strictEqual(env.MPM_PROVENANCE_FRAMEWORK, "openclaw");
});

test("withWorkspace() does not fabricate MPM_PROVENANCE_MODEL", () => {
  // OpenClaw's hook context does not expose model identity at boot.
  // The auto-mode/persona selector adapter has no model context either;
  // it would be a fabrication to set one here.
  const env = withWorkspace({});
  assert.strictEqual(
    env.MPM_PROVENANCE_MODEL,
    undefined,
    "auto-mode/persona adapter must not fabricate MPM_PROVENANCE_MODEL"
  );
});
