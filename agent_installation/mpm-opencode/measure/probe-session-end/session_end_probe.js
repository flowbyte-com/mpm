// session_end_probe.js — Session C, session-end investigation.
//
// PURPOSE.
//
// Determine what the REAL OpenCode v2.1.4 host publishes at a session
// boundary, and whether ANY of it establishes a session TERMINAL
// boundary (as opposed to a turn boundary, or a host-process boundary).
//
// WHAT THIS PROBE RECORDS.
//
//   * every event the plugin's own `ctx.event.subscribe({signal})`
//     stream delivers, in order, with a monotonic sequence number and
//     the wall clock;
//   * for the boundary-relevant events only, the small scalar fields
//     that carry the boundary's MEANING (`sessionID`, `reason`,
//     `outcome`, `archived`);
//   * when the plugin's SYNCHRONOUS cleanup callback runs;
//   * the shape of `ctx.session`, so the design proposal can say what a
//     plugin could call if it wanted to.
//
// WHAT THIS PROBE DOES NOT RECORD.
//
// No prompt text, no assistant message, no tool input, no tool output,
// no compaction summary. Only data KEYS are recorded for every event
// except the four boundary-relevant scalars listed above. The P4
// structural allowlist is the rule for what MPM PERSISTS; this probe
// holds itself to the same discipline so the investigation itself does
// not become the leak it is investigating.
//
// The probe is a WITNESS: it observes. It never mutates the session and
// never writes to MPM.

import fs from "node:fs"

const LOG = process.env.PROBE_LOG || "/tmp/opencode/probe-d/out/events.jsonl"
const T0 = Date.now()

function emit(kind, extra) {
  try {
    fs.appendFileSync(
      LOG,
      JSON.stringify({ t: Date.now(), dt: Date.now() - T0, pid: process.pid, kind, ...extra }) + "\n",
    )
  } catch {
    /* never break the host */
  }
}

/**
 * The only event fields this probe is allowed to look at.
 *
 * Everything else about an event is recorded as its KEY NAMES, so the
 * shape of the stream is known without any content being captured.
 */
const BOUNDARY_SCALARS = ["sessionID", "reason", "outcome", "archived"]

function boundaryScalars(data) {
  if (!data || typeof data !== "object" || Array.isArray(data)) return undefined
  const out = {}
  for (const k of BOUNDARY_SCALARS) {
    const v = data[k]
    if (typeof v === "string" || typeof v === "number" || typeof v === "boolean") out[k] = v
  }
  return Object.keys(out).length > 0 ? out : undefined
}

async function setup(ctx) {
  emit("plugin.init", {
    hasEvent: !!ctx.event,
    eventKeys: ctx.event ? Object.keys(ctx.event) : null,
    // The surface a plugin could use to ACT on a session. Recorded as
    // KEY NAMES only.
    ctxKeys: Object.keys(ctx).sort(),
    sessionKeys: ctx.session ? Object.keys(ctx.session).sort() : null,
    sessionHookNames: ctx.session?.hook ? "present" : "absent",
    locationKeys: ctx.location ? Object.keys(ctx.location).sort() : null,
    appKeys: ctx.app ? Object.keys(ctx.app).sort() : null,
  })

  // Process-level teardown signals: do they fire on a graceful host
  // exit, and on a SIGKILL?
  for (const sig of ["SIGTERM", "SIGINT", "SIGHUP"]) {
    try {
      process.on(sig, () => emit("process.signal", { sig }))
    } catch {
      /* not all signals are registerable */
    }
  }
  process.on("exit", (code) => emit("process.exit", { code }))

  if (!ctx.event || typeof ctx.event.subscribe !== "function") {
    emit("probe.error", { error: "ctx.event.subscribe absent" })
    return
  }

  let seq = 0
  const counts = new Map()
  try {
    const stream = ctx.event.subscribe({})
    ;(async () => {
      try {
        for await (const ev of stream) {
          seq++
          const type = ev?.type ?? "(no type)"
          counts.set(type, (counts.get(type) || 0) + 1)
          emit("event", {
            seq,
            type,
            id: ev?.id ?? null,
            created: ev?.created ?? null,
            dataKeys:
              ev?.data && typeof ev.data === "object" ? Object.keys(ev.data).sort() : null,
            boundary: boundaryScalars(ev?.data),
          })
        }
        emit("stream.ended", { total: seq })
      } catch (e) {
        emit("stream.error", { error: String(e), after: seq })
      }
    })()
  } catch (e) {
    emit("subscribe.error", { error: String(e) })
  }

  // The host's cleanup contract: a SYNCHRONOUS `() => void`. The host
  // calls it and does NOT await it.
  return () => {
    emit("cleanup.callback", {
      seq,
      distinctTypes: counts.size,
      counts: Object.fromEntries(counts),
    })
  }
}

export default { id: "session-c-session-end-probe", setup }
