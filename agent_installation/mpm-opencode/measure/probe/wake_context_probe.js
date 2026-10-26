// wake_context_probe.js — end-to-end witness for the wake-context fix.
//
// Loaded ALONGSIDE the mpm-opencode plugin in an isolated OpenCode host.
// It registers `ctx.session.hook("context")` and records the system-prompt
// parts it can see. Because the host hands the same mutable event object to
// each registered hook, a plugin that pushes a part is observable here.
//
// It records twice:
//   * immediately (in registration order) — catches an earlier-registered
//     plugin that already pushed, and
//   * by array reference at teardown — catches a later-registered plugin
//     that pushes after this hook returns.
//
// Never throws into the host; never writes anywhere but PROBE_LOG.

import fs from "node:fs"

const LOG = process.env.WAKE_PROBE_LOG || "/tmp/opencode/wake-e2e/probe/context.jsonl"
const seen = []

function emit(kind, extra) {
  try {
    fs.appendFileSync(LOG, JSON.stringify({ t: Date.now(), pid: process.pid, kind, ...extra }) + "\n")
  } catch {
    /* never break the host */
  }
}

function texts(system) {
  if (!Array.isArray(system)) return null
  return system.map((p) => (typeof p?.text === "string" ? p.text : String(p?.text ?? "")))
}

async function setup(ctx) {
  emit("probe.init", { app: ctx.app ?? null })

  try {
    await ctx.session.hook("context", (event) => {
      seen.push(event.system)
      emit("context.hook", {
        sessionID: event.sessionID ?? null,
        partCount: Array.isArray(event.system) ? event.system.length : null,
        texts: texts(event.system),
      })
    })
  } catch (e) {
    emit("probe.error", { where: "session.hook", error: String(e) })
  }

  return () => {
    // Dump the live arrays again at teardown, in case a later hook mutated
    // them after this one returned.
    emit("probe.teardown", {
      hookCount: seen.length,
      snapshots: seen.map((s) => ({ partCount: Array.isArray(s) ? s.length : null, texts: texts(s) })),
    })
  }
}

export default { id: "wake-context-probe", setup }
