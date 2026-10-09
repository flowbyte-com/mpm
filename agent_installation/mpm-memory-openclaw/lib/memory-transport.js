// lib/memory-transport.js — read/write routing, lazy startup, and the
// bounded fallback policy for the persistent mpm-mcp transport.
//
// ---------------------------------------------------------------------------
// Read vs write routing
// ---------------------------------------------------------------------------
// `mpm-mcp` reads its provenance/active-context environment ONCE at
// startup (cmd/mpm-mcp/main.go:190, `mpmcli.ActiveContextFromEnv()`),
// whereas the subprocess path re-derives it on every call. That is
// harmless for reads — nothing about a query result depends on when the
// process started — and it is exactly why this module refuses to route
// writes through the persistent child.
//
// In practice this plugin has no writes at all: both memory tools issue
// `mpm_memory` `action:"query"`, the session hooks issue
// `mpm_context` `action:"read_wake_context"`, and the boot health check
// issues `mpm_system` `action:"health_check"`. The allowlist below names
// those three explicitly rather than trusting that observation to stay
// true, so a future edit that adds a write fails closed onto the
// subprocess path instead of silently inheriting boot-time environment.
//
// ---------------------------------------------------------------------------
// Startup point
// ---------------------------------------------------------------------------
// The persistent child starts LAZILY on the first routed call, not in
// register(). Register() is not a safe spawn point under current
// OpenClaw: it runs for `openclaw plugins list / inspect / doctor` (the
// `health_check ok` line those commands print originates in register()'s
// boot IIFE), and in `cli-metadata` mode the runtime is deliberately an
// unavailable proxy. A register-time spawn would leak a child per
// inspection command. One child per *active plugin lifecycle* is still
// achieved: the first memory read starts it, the last one keeps it.
//
// ---------------------------------------------------------------------------
// Fallback policy
// ---------------------------------------------------------------------------
// On any transport failure the call is retried once through the
// canonical `mpm call` subprocess, so a broken or missing mpm-mcp
// degrades to today's behaviour rather than to an error. Two guards keep
// that from becoming a restart loop:
//
//   - a NON-fatal failure (request timeout) leaves the child alone
//   - a FATAL failure tears the child down and increments a restart
//     budget; after one restart the fast path is disabled for the rest of
//     the plugin lifetime and everything goes to the subprocess
//
// Both counters are surfaced through `stats()` so tests can assert them
// rather than infer them from timing.

import { MpmMcpClient, McpTransportError, resolveMcpBin } from "./mcp-client.js";

/**
 * Tools whose MCP dispatch is provably equivalent to the subprocess path
 * (read-only, no write-time environment dependency). Anything not listed
 * here stays on `mpm call` unconditionally.
 */
export const READ_ROUTED_TOOLS = Object.freeze(new Set([
  "mpm_memory",
  "mpm_context",
  "mpm_system",
]));

/** How many times a fatally-crashed child may be respawned in one lifecycle. */
const MAX_RESTARTS = 1;

export function createMemoryTransport(opts) {
  const {
    mpmBin,
    timeoutMs,
    log = () => {},
    callMpmTool,
    mcpBin = null,
    env = process.env,
  } = opts;

  const stats = {
    mcpCalls: 0,
    subprocessCalls: 0,
    mcpFailures: 0,
    spillFallbacks: 0,
    restarts: 0,
    permanentDisable: false,
  };

  let client = null;

  function diagnostic(message) {
    // Normal operation is silent; only lifecycle anomalies reach the host.
    if (typeof log.warn === "function") log.warn(`mpm-memory-openclaw: ${message}`);
  }

  function canUseMcp(tool) {
    return mcpBin !== null && READ_ROUTED_TOOLS.has(tool) && !stats.permanentDisable;
  }

  async function getClient() {
    if (!client) {
      client = new MpmMcpClient({
        command: mcpBin,
        env,
        // The handshake opens the database and compiles the mode/persona
        // router, so it needs more headroom than a plain read.
        initTimeoutMs: Math.max(timeoutMs, 15000),
        requestTimeoutMs: Math.max(timeoutMs, 15000),
        onDiagnostic: diagnostic,
      });
    }
    return client;
  }

  async function shutdownClient() {
    const c = client;
    client = null;
    if (c) {
      try {
        await c.close();
      } catch (e) {
        diagnostic(`mpm-mcp shutdown failed: ${e.message || e}`);
      }
    }
  }

  async function callViaMcp(tool, envelope) {
    const c = await getClient();
    const { envelope: result, spill } = await c.callTool(
      tool,
      envelope,
      { timeoutMs: Math.max(timeoutMs, 15000) }
    );
    if (spill) {
      // The MCP output policy replaced the result with a blob pointer.
      // Returning that would change the tool's visible shape, so this one
      // call goes back through the subprocess, which has no such policy.
      stats.spillFallbacks += 1;
      return null;
    }
    if (result === null) {
      throw new McpTransportError(
        `mpm-mcp ${tool} returned no JSON envelope`,
        { fatal: false }
      );
    }
    return result;
  }

  /**
   * Route one tool call. Tries the persistent child, then the canonical
   * subprocess. Always resolves with the same envelope shape the
   * subprocess path would have produced.
   */
  async function call(tool, payload) {
    if (!canUseMcp(tool)) {
      stats.subprocessCalls += 1;
      return callMpmTool(tool, payload, { mpmBin, timeoutMs });
    }

    // Match callMpmTool's envelope normalization exactly, so both
    // transports hand the handler the identical payload shape.
    const envelope =
      payload && typeof payload === "object" && payload.params && typeof payload.params === "object"
        ? payload
        : { ...(payload || {}), params: {} };

    try {
      const result = await callViaMcp(tool, envelope);
      if (result !== null) {
        stats.mcpCalls += 1;
        return result;
      }
      // Spill fallback: fall through to the subprocess below.
      stats.subprocessCalls += 1;
      return callMpmTool(tool, payload, { mpmBin, timeoutMs });
    } catch (e) {
      stats.mcpFailures += 1;
      const fatal = e instanceof McpTransportError ? e.fatal : true;
      diagnostic(`mpm-mcp ${tool} failed (${fatal ? "fatal" : "transient"}): ${e.message || e}`);
      if (fatal) {
        await shutdownClient();
        if (stats.restarts < MAX_RESTARTS) {
          stats.restarts += 1;
          diagnostic(
            `restarting mpm-mcp once (restart ${stats.restarts}/${MAX_RESTARTS})`
          );
        } else {
          stats.permanentDisable = true;
          diagnostic(
            `mpm-mcp restart budget exhausted — memory reads stay on ` +
            `\`mpm call\` for the rest of this plugin lifecycle`
          );
        }
      }
      stats.subprocessCalls += 1;
      return callMpmTool(tool, payload, { mpmBin, timeoutMs });
    }
  }

  /**
   * Terminate the persistent child. Registered as the plugin's
   * `registerRuntimeLifecycle` cleanup so gateway shutdown and plugin
   * reload both reap it (probe D).
   */
  async function close() {
    await shutdownClient();
  }

  return {
    call,
    close,
    stats,
    get client() {
      return client;
    },
    get mcpBin() {
      return mcpBin;
    },
  };
}

/**
 * Resolve the `mpm-mcp` sibling of a configured `mpm` binary.
 *
 * Re-exported here so callers wiring a transport have a single import
 * for transport concerns, while the resolution rule itself stays
 * testable on its own (custom runtime prefixes, PATH fallback, and
 * source-checkout rejection in probe G).
 */
export { resolveMcpBin };