// tests/mcp_launch_provenance.test.js — Stage 2B regression for the
// OpenClaw MCP launch environment contract.
//
// The shell-out path is covered by resolve_exec_env hooks (see
// integration.test.js and runtime_injection.test.js). The MCP path
// is covered at the mpm-mcp launch boundary: the canonical .mcp.json
// reference MUST set MPM_PROVENANCE_FRAMEWORK=openclaw so any host
// that adopts it preserves the framework identity through MCP
// transport. Without this, recent_activity and artifact_provenance
// attribute OpenClaw-driven calls to framework_name="mcp" (the MCP
// transport default) instead of "openclaw".
//
// This test pins the .mcp.json contract so a future edit that drops
// the env var is caught before it reaches production.

import { test } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

const __filename = fileURLToPath(import.meta.url);
const __dirname = path.dirname(__filename);
const MCP_JSON = path.resolve(__dirname, "..", ".mcp.json");

function loadMcpJson() {
  const raw = fs.readFileSync(MCP_JSON, "utf8");
  return JSON.parse(raw);
}

test("canonical .mcp.json declares the mpm MCP server", () => {
  const cfg = loadMcpJson();
  assert.ok(cfg.mcpServers, "mcpServers block missing");
  assert.ok(cfg.mcpServers.mpm, "mpm MCP server entry missing");
  assert.strictEqual(cfg.mcpServers.mpm.command, "$HOME/.mpm/bin/mpm-mcp",
    "mpm-mcp command path drifted from canonical install layout");
  assert.ok(cfg.mcpServers.mpm.env, "mpm MCP env block missing");
  assert.strictEqual(cfg.mcpServers.mpm.env.MPM_WORKSPACE, "$HOME/.mpm",
    "MPM_WORKSPACE drifted from canonical install layout");
});

test("canonical .mcp.json sets MPM_PROVENANCE_FRAMEWORK=openclaw so MCP transport retains framework identity", () => {
  const cfg = loadMcpJson();
  const env = cfg.mcpServers.mpm.env;
  assert.strictEqual(
    env.MPM_PROVENANCE_FRAMEWORK,
    "openclaw",
    "OpenClaw MCP launch env must declare MPM_PROVENANCE_FRAMEWORK=openclaw. " +
    "Without this, recent_activity records framework_name='mcp' (transport " +
    "default) for every OpenClaw-driven call instead of the actual host."
  );
});

test("canonical .mcp.json does not fabricate MPM_PROVENANCE_MODEL", () => {
  const cfg = loadMcpJson();
  const env = cfg.mcpServers.mpm.env;
  // OpenClaw's MCP launch cannot authoritatively know the model at
  // boot time — the active model is dynamic per turn. Setting a
  // static value here would be a fabrication.
  assert.strictEqual(
    env.MPM_PROVENANCE_MODEL,
    undefined,
    "OpenClaw MCP launch must not statically set MPM_PROVENANCE_MODEL; " +
    "the host does not know which model executed this call."
  );
});
