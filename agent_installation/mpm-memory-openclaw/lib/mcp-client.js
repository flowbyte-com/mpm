// lib/mcp-client.js — minimal, dependency-free MCP stdio client for the
// persistent `mpm-mcp` transport used by the memory tools.
//
// ---------------------------------------------------------------------------
// Why this exists
// ---------------------------------------------------------------------------
// The memory tools used to reach MPM through
//
//     spawn(mpm, ["call", <tool>, "--payload", <json>])
//
// which pays a full Go process launch (~17 ms of a ~30 ms call) on every
// single memory read. `mpm-mcp` is the same substrate over a persistent
// stdio MCP session, so one child amortises that cost across every
// subsequent read.
//
// ---------------------------------------------------------------------------
// Why the protocol is what it is (do not "simplify" this)
// ---------------------------------------------------------------------------
// `mpm-mcp` is `github.com/mark3labs/mcp-go/server.ServeStdio` (see
// cmd/mpm-mcp/main.go:237). That transport is JSON-RPC 2.0 with
// NEWLINE-DELIMITED framing — NOT LSP-style Content-Length headers:
//
//     reader.ReadString('\n')            server/stdio.go:489
//     fmt.Fprintf(w, "%s\n", respBytes) server/stdio.go:830
//
// So: one complete JSON value per line, `\n`-terminated, on the child's
// stdin/stdout. Because a pipe delivers arbitrary byte chunks, a single
// chunk may hold half a line or three lines; the reader below buffers
// until it sees a `\n`.
//
// ---------------------------------------------------------------------------
// Handler equivalence (why results match the subprocess path)
// ---------------------------------------------------------------------------
// `mpm call <tool> --payload <json>` and `tools/call` with
// `arguments: <json>` both dispatch to the SAME registry handler:
//
//     cmd/mpm/call.go   tool.Handler(dm, ac, payload)
//     cmd/mpm-mcp/tools.go:534  payload := req.GetArguments()
//                              handler(dm, callAC, payload)
//
// Both call `recordToolInvocation` and both fold pending wakes. The
// result envelope is therefore identical; only the transport wrapping
// differs, and `extractEnvelope` below undoes exactly that wrapping.
//
// ---------------------------------------------------------------------------
// One deliberate asymmetry: result spilling
// ---------------------------------------------------------------------------
// The MCP surface applies `tools.DefaultOutputPolicy()` (20480 bytes,
// internal/core/tools/output_policy.go:38). A larger result is written to
// the blob store and the client receives a
//
//     {"status":"spilled","pointer":"mpm://blob/...","preview":{...}}
//
// envelope INSTEAD of the real result. The CLI path has no such policy.
//
// That would silently change the externally visible shape of a large
// memory search, so `extractEnvelope` reports it and the caller routes
// that one call back through the canonical subprocess transport, which
// returns the full result. Correctness wins over the saved millisecond,
// and only for results that would have been truncated anyway.
//
// ESM module — the parent package.json declares "type": "module".

import { spawn } from "node:child_process";
import { accessSync, constants as fsConstants } from "node:fs";
import path from "node:path";

/** MCP protocol revision this client requests. mcp-go negotiates downwards. */
export const MCP_PROTOCOL_VERSION = "2024-11-05";

/**
 * Lifecycle states. Deliberately a single field rather than a set of
 * booleans — every transition is one assignment and the combination
 * "starting && ready && failed" is unrepresentable by construction.
 *
 *   stopped  no child, nothing in flight
 *   starting child spawned, initialize handshake in flight
 *   ready    handshake complete, tools/call accepted
 *   failed   startup or a later crash killed the child
 */
export const CLIENT_STATE = Object.freeze({
  STOPPED: "stopped",
  STARTING: "starting",
  READY: "ready",
  FAILED: "failed",
});

/** Raised for any condition that should send the caller to the subprocess path. */
export class McpTransportError extends Error {
  constructor(message, { fatal = false } = {}) {
    super(message);
    this.name = "McpTransportError";
    /** `fatal` marks the persistent child unusable for this and later calls. */
    this.fatal = fatal;
  }
}

/**
 * Pull the tool's JSON envelope back out of an MCP CallToolResult.
 *
 * The server returns `result.content[]`; the handler's marshalled result
 * is the text of one block. Two other block kinds can appear alongside
 * it and must NOT be mistaken for the envelope:
 *
 *   - a wake-notification block, prepended when wakes are due
 *     (cmd/mpm-mcp/tools.go:687) — XML-ish text, not JSON
 *   - the spill envelope, which IS JSON but is a pointer to the result
 *     rather than the result itself
 *
 * Returns `{ envelope, spill }`. `spill` is the blob pointer envelope
 * when the server truncated the result, else null.
 */
export function extractEnvelope(callToolResult) {
  const content = Array.isArray(callToolResult?.content)
    ? callToolResult.content
    : [];
  let spill = null;

  for (const block of content) {
    if (!block || block.type !== "text" || typeof block.text !== "string") continue;
    const text = block.text.trim();
    if (!text.startsWith("{")) continue;
    let parsed;
    try {
      parsed = JSON.parse(text);
    } catch {
      continue; // a JSON-looking but unparseable block is not the envelope
    }
    if (parsed === null || typeof parsed !== "object" || Array.isArray(parsed)) continue;
    if (parsed.status === "spilled" && typeof parsed.pointer === "string") {
      spill = parsed;
      continue;
    }
    return { envelope: parsed, spill: null };
  }

  if (spill) return { envelope: null, spill };
  return { envelope: null, spill: null };
}

/**
 * A single persistent `mpm-mcp` child, addressed over stdio MCP.
 *
 * Request/response correlation is by JSON-RPC `id`, allocated from a
 * monotonic counter. Concurrent callers are safe: each `callTool`
 * registers its own pending entry and its own timer, and the single
 * stdout reader dispatches by id.
 */
export class MpmMcpClient {
  #state = CLIENT_STATE.STOPPED;
  #child = null;
  #startPromise = null;
  #pending = new Map();
  #nextId = 0;
  #stdoutBuf = "";
  #stderrTail = "";
  #lastError = null;
  #closed = false;

  /**
   * @param {object} opts
   * @param {string} opts.command       absolute path to the mpm-mcp executable
   * @param {string[]} [opts.args]      extra argv for the child (mpm-mcp itself takes none)
   * @param {object} [opts.env]         child environment (caller pins MPM_WORKSPACE)
   * @param {number} [opts.initTimeoutMs]  budget for the initialize handshake
   * @param {number} [opts.requestTimeoutMs] default per-tools/call budget
   * @param {(msg: string, err?: Error) => void} [opts.onDiagnostic]
   */
  constructor(opts) {
    this.command = opts.command;
    this.args = Array.isArray(opts.args) ? opts.args : [];
    this.env = opts.env || process.env;
    this.initTimeoutMs = opts.initTimeoutMs ?? 10000;
    this.requestTimeoutMs = opts.requestTimeoutMs ?? 15000;
    this.onDiagnostic = typeof opts.onDiagnostic === "function" ? opts.onDiagnostic : () => {};
  }

  get state() {
    return this.#state;
  }

  get lastError() {
    return this.#lastError ? String(this.#lastError.message || this.#lastError) : null;
  }

  /** Tail of the child's stderr — diagnostics only, never parsed as protocol. */
  get stderrTail() {
    return this.#stderrTail;
  }

  /** True when a child currently exists (started or ready). */
  get hasChild() {
    return this.#child !== null;
  }

  get pendingCount() {
    return this.#pending.size;
  }

  /**
   * The live child process handle, or null.
   *
   * Exists so the no-orphan contract can be asserted against the real
   * process object (its exitCode/signalCode after close()) rather than
   * inferred from the client's own bookkeeping — a client that forgot to
   * wait for `exit` would otherwise still look clean.
   */
  _debugChild() {
    return this.#child;
  }

  /**
   * Idempotent, single-flight startup. Concurrent first callers all await
   * the SAME promise, so N simultaneous memory reads spawn exactly one
   * child (probe B).
   */
  async ensureReady() {
    if (this.#closed) throw new McpTransportError("client is closed", { fatal: true });
    if (this.#state === CLIENT_STATE.READY) return;
    if (this.#startPromise) return this.#startPromise;

    this.#startPromise = this.#start().finally(() => {
      this.#startPromise = null;
    });
    return this.#startPromise;
  }

  async #start() {
    this.#state = CLIENT_STATE.STARTING;
    this.#stderrTail = "";

    let child;
    try {
      child = spawn(this.command, this.args, {
        stdio: ["pipe", "pipe", "pipe"],
        env: this.env,
        // Explicitly no shell: `command` is a resolved absolute path and
        // must be exec'd directly, never interpolated through a shell.
        shell: false,
        windowsHide: true,
      });
    } catch (e) {
      this.#state = CLIENT_STATE.FAILED;
      this.#lastError = e;
      throw new McpTransportError(`failed to spawn ${this.command}: ${e.message}`, { fatal: true });
    }
    this.#child = child;

    child.on("error", (e) => {
      // ENOENT lands here: the binary is missing or not executable.
      this.#lastError = e;
      this.#onChildGone(`mpm-mcp child error: ${e.message}`);
    });

    child.on("exit", (code, signal) => {
      if (this.#child !== child) return; // a superseded child; ignore
      this.#onChildGone(
        `mpm-mcp child exited (code=${code}, signal=${signal})` +
          (this.#stderrTail ? `: ${this.#stderrTail}` : "")
      );
    });

    child.stdout.setEncoding("utf8");
    child.stdout.on("data", (chunk) => this.#onStdout(chunk));
    // A pipe whose peer has already exited fails the WRITE asynchronously
    // with EPIPE; the surrounding try/catch cannot see that. Without these
    // handlers an "mpm-mcp exits before ready" race would surface as an
    // unhandled 'error' event and take the gateway down with it. The
    // child's `exit` handler already owns the failure path — it rejects
    // every in-flight request — so here we only record and swallow.
    child.stdin.on("error", (e) => {
      this.#lastError = e;
    });
    child.stdout.on("error", () => {});
    // mpm-mcp discards its own INFO logs to io.Discard unless MPM_VERBOSE
    // is set, so stderr is normally empty. Keep a bounded tail purely so a
    // crash can be explained; never parse it as protocol.
    child.stderr.setEncoding("utf8");
    child.stderr.on("data", (chunk) => {
      this.#stderrTail = (this.#stderrTail + chunk).slice(-2048);
    });
    child.stderr.on("error", () => {});

    try {
      await this.#handshake();
    } catch (e) {
      this.#killChild();
      this.#state = CLIENT_STATE.FAILED;
      this.#lastError = e;
      throw e;
    }
    this.#state = CLIENT_STATE.READY;
  }

  async #handshake() {
    const result = await this.#request(
      "initialize",
      {
        protocolVersion: MCP_PROTOCOL_VERSION,
        capabilities: {},
        clientInfo: { name: "mpm-memory-openclaw", version: "0.1.3" },
      },
      this.initTimeoutMs,
      "initialize"
    );
    const negotiated = result?.protocolVersion;
    this.onDiagnostic(
      negotiated
        ? `mpm-mcp ready (server=${result?.serverInfo?.name ?? "?"} protocol=${negotiated})`
        : "mpm-mcp ready"
    );
    // The MCP spec requires the client to acknowledge initialization
    // before issuing any other request.
    this.#write({ jsonrpc: "2.0", method: "notifications/initialized" });
  }

  /**
   * Call a tool over MCP.
   *
   * Resolves with `{ envelope, spill }`. Rejects with McpTransportError
   * (fatal for child-level problems) so the caller can fall back to the
   * canonical subprocess transport.
   */
  async callTool(name, args, opts = {}) {
    await this.ensureReady();
    const timeoutMs = opts.timeoutMs ?? this.requestTimeoutMs;
    const result = await this.#request(
      "tools/call",
      { name, arguments: args ?? {} },
      timeoutMs,
      name
    );
    if (result?.isError === true) {
      const { envelope } = extractEnvelope(result);
      const message =
        (envelope && typeof envelope.error === "string" && envelope.error) ||
        (Array.isArray(result.content) && result.content[0]?.text) ||
        `${name} failed`;
      // A tool-level error is a well-formed protocol response: the child
      // is still healthy, so this is NOT fatal.
      throw new McpTransportError(`mpm-mcp tools/call ${name}: ${message}`, { fatal: false });
    }
    return extractEnvelope(result);
  }

  /** JSON-RPC round trip with id correlation and a hard timeout. */
  #request(method, params, timeoutMs, label) {
    return new Promise((resolve, reject) => {
      if (!this.#child || this.#child.stdin.destroyed) {
        reject(new McpTransportError(`mpm-mcp unavailable for ${label}`, { fatal: true }));
        return;
      }
      const id = ++this.#nextId;
      const entry = { resolve, reject, timer: null };
      entry.timer = setTimeout(() => {
        // Drop only THIS request. The child stays alive and in sync: the
        // late response, if it ever arrives, is dropped by the id lookup
        // in #onStdout, so no framing corruption leaks into the next call.
        this.#pending.delete(id);
        reject(
          new McpTransportError(
            `mpm-mcp ${label} timed out after ${timeoutMs}ms`,
            { fatal: false }
          )
        );
      }, timeoutMs);
      // Never hold the event loop open for an in-flight request.
      if (typeof entry.timer.unref === "function") entry.timer.unref();
      this.#pending.set(id, entry);

      if (!this.#write({ jsonrpc: "2.0", id, method, params })) {
        this.#pending.delete(id);
        clearTimeout(entry.timer);
        reject(new McpTransportError(`mpm-mcp write failed for ${label}`, { fatal: true }));
      }
    });
  }

  #write(message) {
    const child = this.#child;
    if (!child || child.stdin.destroyed || child.stdin.writableEnded) return false;
    try {
      child.stdin.write(JSON.stringify(message) + "\n");
      return true;
    } catch (e) {
      this.#lastError = e;
      return false;
    }
  }

  /** Newline-delimited framing: buffer until a full line is available. */
  #onStdout(chunk) {
    this.#stdoutBuf += chunk;
    let nl;
    while ((nl = this.#stdoutBuf.indexOf("\n")) !== -1) {
      const line = this.#stdoutBuf.slice(0, nl);
      this.#stdoutBuf = this.#stdoutBuf.slice(nl + 1);
      const trimmed = line.trim();
      if (trimmed === "") continue;
      this.#onLine(trimmed);
    }
  }

  #onLine(line) {
    let message;
    try {
      message = JSON.parse(line);
    } catch (e) {
      // A non-JSON line on stdout means the protocol stream is
      // untrustworthy. Fail loudly rather than silently dropping calls.
      this.#lastError = e;
      this.onDiagnostic(`mpm-mcp protocol error: unparseable line (${line.slice(0, 200)})`);
      this.#failAllPending(`mpm-mcp protocol error: ${e.message}`);
      return;
    }
    if (message === null || typeof message !== "object") return;
    // Server-initiated requests/notifications carry no id from us; the
    // server never issues them in this integration, so drop them.
    if (message.id === undefined || message.id === null) return;

    const entry = this.#pending.get(message.id);
    if (!entry) return; // unknown / already-timed-out id — discard
    this.#pending.delete(message.id);
    clearTimeout(entry.timer);

    if (message.error) {
      const code = message.error?.code;
      const text = message.error?.message || "unknown JSON-RPC error";
      entry.reject(
        new McpTransportError(`mpm-mcp JSON-RPC error ${code ?? "?"}: ${text}`, {
          fatal: code === -32601 /* Method not found */,
        })
      );
      return;
    }
    entry.resolve(message.result);
  }

  /** Child is gone: fail everything in flight and park in FAILED. */
  #onChildGone(reason) {
    this.#child = null;
    this.#state = CLIENT_STATE.FAILED;
    this.#failAllPending(reason);
    this.onDiagnostic(reason);
  }

  #failAllPending(reason) {
    const entries = [...this.#pending.values()];
    this.#pending.clear();
    for (const entry of entries) {
      clearTimeout(entry.timer);
      entry.reject(new McpTransportError(reason, { fatal: true }));
    }
  }

  #killChild() {
    const child = this.#child;
    if (!child) return;
    try { child.kill("SIGKILL"); } catch { /* already dead */ }
    this.#child = null;
  }

  /**
   * Shut the child down and wait for it to be reaped.
   *
   * Order mirrors how mpm-mcp itself is designed to exit
   * (cmd/mpm-mcp/main.go:234 — SIGTERM/SIGINT cancel the serve context):
   * close stdin so the stdio read loop sees EOF, then SIGTERM, then
   * SIGKILL as a backstop. Resolves only once `exit` has fired, so a
   * caller that awaits close() knows there is no surviving child
   * (probe D).
   *
   * @param {number} [graceMs] per-stage wait
   */
  async close(graceMs = 2000) {
    this.#closed = true;
    const child = this.#child;
    if (!child) {
      this.#state = CLIENT_STATE.STOPPED;
      return;
    }

    const exited = new Promise((resolve) => {
      if (child.exitCode !== null || child.signalCode !== null) return resolve();
      child.once("exit", () => resolve());
    });

    try { child.stdin.end(); } catch { /* already closed */ }
    if (!(await raceExit(exited, graceMs))) {
      try { child.kill("SIGTERM"); } catch { /* already dead */ }
      if (!(await raceExit(exited, graceMs))) {
        this.onDiagnostic("mpm-mcp did not exit on SIGTERM; sending SIGKILL");
        try { child.kill("SIGKILL"); } catch { /* already dead */ }
        await raceExit(exited, graceMs);
      }
    }
    if (this.#child === child) this.#child = null;
    this.#failAllPending("mpm-mcp client closed");
    this.#state = CLIENT_STATE.STOPPED;
  }
}

/** Resolve true if `exited` settles first, else false after `ms`. */
function raceExit(exited, ms) {
  return new Promise((resolve) => {
    let done = false;
    const timer = setTimeout(() => {
      if (done) return;
      done = true;
      resolve(false);
    }, ms);
    if (typeof timer.unref === "function") timer.unref();
    exited.then(() => {
      if (done) return;
      done = true;
      clearTimeout(timer);
      resolve(true);
    });
  });
}

/**
 * Derive the `mpm-mcp` executable that sits beside a configured `mpm`.
 *
 * The installation contract places both binaries in the same `bin/`
 * directory (`~/.mpm/bin/mpm` and `~/.mpm/bin/mpm-mcp`), so the sibling
 * path is the deterministic answer and needs no PATH search of its own.
 *
 * Returns null — meaning "no fast path, stay on the subprocess transport"
 * — when the configured binary has no resolvable directory, the sibling
 * is missing, the sibling is not executable, or either path points into a
 * source checkout's `.build/bin` (probe G).
 */
export function resolveMcpBin(mpmBin) {
  if (typeof mpmBin !== "string" || mpmBin.trim() === "") return null;
  const bin = mpmBin.trim();
  if (bin.includes("/.build/bin/") || bin.endsWith("/.build/bin")) return null;

  const dir = path.dirname(bin);
  if (dir === "" || dir === ".") {
    // A bare name gives nothing to derive from, and searching PATH for a
    // sibling would defeat the point of a deterministic mapping.
    return null;
  }
  const candidate = path.join(dir, "mpm-mcp");
  try {
    accessSync(candidate, fsConstants.X_OK);
  } catch {
    return null;
  }
  return candidate;
}