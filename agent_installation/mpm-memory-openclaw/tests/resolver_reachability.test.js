// tests/resolver_reachability.test.js — §13 tool-discovery guard.
//
// The transport underneath mpm_memory_search / mpm_memory_get can change
// freely; what must NOT change is that OpenClaw can still resolve those
// two tools out of the plugin registry. This test drives OpenClaw's own
// `resolvePluginTools` — the same function the agent tool array is built
// from — against a hermetic config, and asserts both names materialize.
//
// It deliberately does NOT assert the tools appear in a model's flat
// declared tool array. Under `compat.codeMode: "preferred"`
// (MiniMax-M3) OpenClaw exposes tool_search / tool_describe / tool_call
// and declares concrete tools on demand, so "directly declared by name"
// is not the user-visible contract. The contract is discoverable and
// invokable, and that is what is pinned here.
//
// Run with:
//   node --test tests/resolver_reachability.test.js
//
// Skips (rather than fails) when the OpenClaw runtime is not installed,
// so the plugin's own test suite stays runnable without a host.

import { test, describe } from "node:test";
import assert from "node:assert/strict";
import path from "node:path";
import fs from "node:fs";
import os from "node:os";
import { fileURLToPath, pathToFileURL } from "node:url";
import { execFileSync } from "node:child_process";

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const PKG_ROOT = path.resolve(__dirname, "..");

/** Locate OpenClaw's dist directory, if this host has one. */
function findOpenClawDist() {
  // Explicit override always wins, so CI can point at a pinned install.
  if (process.env.OPENCLAW_DIST && fs.existsSync(process.env.OPENCLAW_DIST)) {
    return process.env.OPENCLAW_DIST;
  }
  // Global npm root for this node version.
  try {
    const root = execFileSync("npm", ["root", "-g"], { encoding: "utf8" }).trim();
    const d = path.join(root, "openclaw/dist");
    if (fs.existsSync(d)) return d;
  } catch { /* npm unavailable */ }
  // Fall back to scanning every nvm-managed node version.
  const nvmBase = path.join(os.homedir(), ".nvm/versions/node");
  if (fs.existsSync(nvmBase)) {
    for (const v of fs.readdirSync(nvmBase).sort().reverse()) {
      const d = path.join(nvmBase, v, "lib/node_modules/openclaw/dist");
      if (fs.existsSync(d)) return d;
    }
  }
  return null;
}

/**
 * Find the chunk that re-exports the plugin-tool resolver. OpenClaw
 * minifies and content-hashes its dist files on every build, so the
 * filename is not stable across versions — but the export block is:
 *
 *   export { resolvePluginTools as i,
 *            acquireStandalonePluginToolRegistry as n,
 *            ensureStandalonePluginToolRegistryLoaded as r,
 *            acquirePluginToolInspectionRegistry as t };
 *
 * Several chunks re-export a `resolvePluginTools` under alias `i`; only
 * this one also carries the standalone-registry loader, so match on the
 * whole block rather than on either name alone.
 */
async function loadResolver(distDir) {
  const files = fs.readdirSync(distDir).filter((f) => f.endsWith(".mjs"));
  for (const f of files) {
    let text;
    try {
      text = fs.readFileSync(path.join(distDir, f), "utf8");
    } catch { continue; }
    const hasResolve = /resolvePluginTools as i\b/.test(text);
    const hasLoader = /ensureStandalonePluginToolRegistryLoaded as r\b/.test(text);
    if (!hasResolve || !hasLoader) continue;
    try {
      const mod = await import(pathToFileURL(path.join(distDir, f)).href);
      if (typeof mod.i === "function" && typeof mod.r === "function") {
        return { resolvePluginTools: mod.i, ensureRegistryLoaded: mod.r };
      }
    } catch { /* not the chunk we want */ }
  }
  return null;
}

const DIST = findOpenClawDist();
const resolverPromise = DIST ? loadResolver(DIST) : Promise.resolve(null);

describe("OpenClaw plugin-tool resolver reachability", { skip: DIST ? false : "OpenClaw dist not found on this host" }, () => {
  test("mpm_memory_search and mpm_memory_get materialize in the resolved plugin-tool set", async () => {
    const resolver = await resolverPromise;
    assert.ok(resolver, "could not load OpenClaw's resolvePluginTools from dist");

    // Hermetic HOME so no live OpenClaw state, install records or
    // workspace files participate.
    const tmp = fs.mkdtempSync(path.join(os.tmpdir(), "mpm-reach-"));
    const home = path.join(tmp, "home");
    const workspaceDir = path.join(tmp, "ws");
    fs.mkdirSync(workspaceDir, { recursive: true });

    const config = {
      plugins: {
        allow: ["mpm-memory-openclaw"],
        load: { paths: [PKG_ROOT] },
        entries: {
          "mpm-memory-openclaw": {
            enabled: true,
            config: { mpmBin: "/nonexistent/bin/mpm" },
          },
        },
        slots: { memory: "mpm-memory-openclaw" },
      },
    };

    const context = {
      config,
      workspaceDir,
      agentDir: workspaceDir,
      agentId: "main",
      env: { ...process.env, HOME: home },
    };

    const previousHome = process.env.HOME;
    process.env.HOME = home;
    try {
      const registry = resolver.ensureRegistryLoaded({
        context,
        toolAllowlist: ["group:plugins"],
        allowGatewaySubagentBinding: true,
      });
      const tools = resolver.resolvePluginTools({
        context,
        existingToolNames: new Set(),
        toolAllowlist: ["group:plugins"],
        suppressNameConflicts: true,
        allowGatewaySubagentBinding: true,
        runtimeRegistry: registry,
      });
      const names = tools.map((t) => t.name).sort();
      assert.ok(
        names.includes("mpm_memory_search"),
        `mpm_memory_search missing from resolved plugin tools: ${JSON.stringify(names)}`
      );
      assert.ok(
        names.includes("mpm_memory_get"),
        `mpm_memory_get missing from resolved plugin tools: ${JSON.stringify(names)}`
      );
    } finally {
      process.env.HOME = previousHome;
      fs.rmSync(tmp, { recursive: true, force: true });
    }
  });
});