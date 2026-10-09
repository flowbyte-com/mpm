// tests/fake-mcp-server.mjs — a scriptable stand-in for `mpm-mcp`.
//
// Speaks the same transport the real server does (newline-delimited
// JSON-RPC 2.0 on stdio, cmd/mpm-mcp/main.go:237 via mcp-go
// server.ServeStdio) so the client's framing, correlation, timeout and
// crash handling can be driven deterministically without a database.
//
// Behaviour is switched by argv flags:
//   --hang-tools      never answer `tools/call` (initialize still succeeds)
//   --exit-after=N    exit(1) upon receiving the Nth tools/call, without
//                     answering it (drives crash-after-ready tests)
//   --garbage-first   write a non-JSON line before answering anything
//   --no-ready        never answer `initialize`
//   --echo-memories=N return an N-memory result envelope (size tests)
//   --echo-wake       prepend a wake-notification content block
//   --echo-request    stamp the caller's own arguments into the envelope,
//                     so a response delivered to the WRONG caller is
//                     detectable rather than indistinguishable
//   --reverse-flush=N buffer tools/call responses for N ms after the
//                     first, then emit them in reverse arrival order
//   --slow-init=N     delay the initialize response by N ms
//
// Every request it receives is appended as one JSON line to
// --log=<path>, which is how the lifecycle tests assert that a given
// number of memory reads produced exactly one child.

import fs from "node:fs";

const argv = process.argv.slice(2);
const flag = (name) => argv.find((a) => a.startsWith(`--${name}=`))?.split("=").slice(1).join("=");
const has = (name) => argv.includes(`--${name}`);
const num = (name, dflt) => (flag(name) === undefined ? dflt : Number(flag(name)));

const hangTools = has("hang-tools");
const exitAfter = num("exit-after", -1);
const garbageFirst = has("garbage-first");
const noReady = has("no-ready");
const echoMemories = num("echo-memories", 0);
const echoWake = has("echo-wake");
const slowInit = num("slow-init", 0);
const echoRequest = has("echo-request");
const reverseFlush = num("reverse-flush", 0);
const logPath = flag("log");

let received = 0;
let buf = "";
let answeredGarbage = false;

function record(entry) {
  received += 1;
  if (logPath) {
    try {
      fs.appendFileSync(logPath, JSON.stringify(entry) + "\n");
    } catch { /* best effort */ }
  }
}

function send(obj) {
  process.stdout.write(JSON.stringify(obj) + "\n");
}

/** Buffered responses, flushed newest-first when --reverse-flush is set. */
let buffering = [];
let flushTimer = null;

function queueResponse(msg) {
  const text = JSON.stringify(memoriesEnvelope(echoMemories, msg.params?.arguments));
  const content = echoWake
    ? [
        { type: "text", text: "<mpms:wakes><wake id='w1'>due</wake></mpms:wakes>" },
        { type: "text", text },
      ]
    : [{ type: "text", text }];
  const response = { jsonrpc: "2.0", id: msg.id, result: { content } };

  if (reverseFlush <= 0) {
    send(response);
    return;
  }
  buffering.push(response);
  if (flushTimer) return;
  flushTimer = setTimeout(() => {
    flushTimer = null;
    for (const r of buffering.reverse()) send(r);
    buffering = [];
  }, reverseFlush);
  if (typeof flushTimer.unref === "function") flushTimer.unref();
}

function memoriesEnvelope(n, args) {
  const memories = [];
  for (let i = 0; i < n; i++) {
    memories.push({
      id: `mem-${i}`,
      content: `fixture memory ${i}`,
      tags: ["probe"],
      collection: "memories",
      created_at: 1700000000 + i,
      weight: 5,
      score: -0.0001 * i,
    });
  }
  const envelope = { success: true, count: memories.length, memories, query: "probe", scope: "all", mode: "default" };
  // Only under --echo-request: lets a test prove each caller received
  // the response to ITS OWN request.
  if (echoRequest) envelope.echoed = args?.tag ?? null;
  return envelope;
}

process.stdin.setEncoding("utf8");
process.stdin.on("data", (chunk) => {
  buf += chunk;
  let nl;
  while ((nl = buf.indexOf("\n")) !== -1) {
    const line = buf.slice(0, nl).trim();
    buf = buf.slice(nl + 1);
    if (line === "") continue;
    handle(line);
  }
});

function handle(line) {
  let msg;
  try {
    msg = JSON.parse(line);
  } catch {
    return;
  }

  // Record BEFORE honouring --hang so hang tests can still observe that
  // the request arrived.
  if (msg.method === "tools/call") record({ method: msg.method, name: msg.params?.name, arguments: msg.params?.arguments, id: msg.id });

  if (exitAfter >= 0 && received >= exitAfter) {
    // Die on the Nth request WITHOUT answering it: the client must see a
    // crash while a request is genuinely in flight.
    process.exit(1);
  }
  if (garbageFirst && !answeredGarbage) {
    answeredGarbage = true;
    // A stray non-JSON line: the client must treat this as a protocol
    // error, not silently swallow it.
    process.stdout.write("this is not json\n");
    return;
  }
  if (hangTools && msg.method === "tools/call") return;
  if (msg.method === "initialize") {
    if (noReady) return;
    if (slowInit > 0) {
      setTimeout(() => {
        send({ jsonrpc: "2.0", id: msg.id, result: { protocolVersion: "2024-11-05", serverInfo: { name: "fake-mpm-mcp", version: "0.1.0" }, capabilities: { tools: {} } } });
      }, slowInit);
      return;
    }
    send({
      jsonrpc: "2.0",
      id: msg.id,
      result: {
        protocolVersion: "2024-11-05",
        serverInfo: { name: "fake-mpm-mcp", version: "0.1.0" },
        capabilities: { tools: {} },
      },
    });
    return;
  }
  if (msg.method === "tools/call") {
    queueResponse(msg);
  }
}

process.on("SIGTERM", () => process.exit(0));