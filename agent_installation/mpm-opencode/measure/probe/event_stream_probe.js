// event_probe.js — Session C, second pass.
//
// Purpose: determine whether the host's plugin-facing event stream is
// actually reachable, and what it delivers.
//
// The first-pass probe called ctx.event.subscribe(callback) and recorded
// `dispKeys: []`. Static inspection of the host shows the implementation is
//
//   event:{subscribe:(o)=>z(e.event.subscribe().pipe(...), o)}
//
// where z = Plugin.Event.makeStreams, which returns an object whose only
// public member is [Symbol.asyncIterator]. So the API is
// `ctx.event.subscribe({signal}) -> AsyncIterable`, and the first probe
// never iterated it.
//
// This probe registers the callback form AND consumes the async-iterable
// form, so the two are distinguishable in the capture.

import fs from "node:fs"

const LOG = process.env.PROBE_LOG || "/tmp/opencode/probe-d/out/events.jsonl"

function emit(kind, extra) {
  try {
    fs.appendFileSync(
      LOG,
      JSON.stringify({ t: Date.now(), pid: process.pid, kind, ...extra }) + "\n",
    )
  } catch {
    /* never break the host */
  }
}

// Bounded deep copy so a large payload cannot explode the log.
function deep(v, depth = 0) {
  if (depth > 3) return "[depth]"
  if (v === null || v === undefined) return v
  const t = typeof v
  if (t === "string") return v.length > 300 ? v.slice(0, 300) + "…" : v
  if (t === "number" || t === "boolean") return v
  if (t === "function") return "[fn]"
  if (Array.isArray(v)) return v.slice(0, 8).map((x) => deep(x, depth + 1))
  if (t === "object") {
    const out = {}
    let n = 0
    for (const k of Object.keys(v)) {
      if (n++ >= 20) {
        out["…"] = "[truncated]"
        break
      }
      try {
        out[k] = deep(v[k], depth + 1)
      } catch {
        out[k] = "[err]"
      }
    }
    return out
  }
  return String(v)
}

async function setup(ctx) {
  emit("plugin.init", {
    app: deep(ctx.app),
    location: deep(ctx.location),
    hasEvent: !!ctx.event,
    eventKeys: ctx.event ? Object.keys(ctx.event) : null,
    subscribeArity: ctx.event?.subscribe?.length ?? null,
  })

  if (!ctx.event || typeof ctx.event.subscribe !== "function") {
    emit("probe.error", { error: "ctx.event.subscribe absent" })
    return
  }

  // ---- FORM A: callback style (what the first probe did) -----------------
  try {
    const a = ctx.event.subscribe((...args) => {
      emit("formA.callback.invoked", { argc: args.length, arg0: deep(args[0]) })
    })
    emit("formA.result", {
      type: typeof a,
      keys: a && typeof a === "object" ? Object.keys(a) : null,
      symbols:
        a && typeof a === "object"
          ? Object.getOwnPropertySymbols(a).map((s) => String(s))
          : null,
      isAsyncIterable: !!(a && typeof a[Symbol.asyncIterator] === "function"),
    })
  } catch (e) {
    emit("formA.error", { error: String(e) })
  }

  // ---- FORM B: consume the async iterable --------------------------------
  const counts = new Map()
  try {
    const stream = ctx.event.subscribe({})
    emit("formB.subscribe", {
      type: typeof stream,
      isAsyncIterable: !!(stream && typeof stream[Symbol.asyncIterator] === "function"),
    })
    ;(async () => {
      let n = 0
      try {
        for await (const ev of stream) {
          n++
          const type = ev?.type ?? "(no type)"
          counts.set(type, (counts.get(type) || 0) + 1)
          // On the first successful execution, exercise the compaction path
          // so its event names are captured live rather than inferred.
          if (type === "session.step.started" && !globalThis.__cmp) {
            globalThis.__cmp = true
            const sid = ev?.data?.sessionID
            setTimeout(async () => {
              try {
                emit("compact.attempt", { sessionID: sid })
                const r = await ctx.session.compact({ sessionID: sid })
                emit("compact.result", { ok: true, result: deep(r) })
              } catch (e) {
                emit("compact.error", { error: String(e) })
              }
            }, 200)
          }
          emit("formB.event", {
            n,
            type,
            keys: ev && typeof ev === "object" ? Object.keys(ev) : null,
            id: ev?.id ?? null,
            created: ev?.created ?? null,
            dataKeys: ev?.data && typeof ev.data === "object" ? Object.keys(ev.data) : null,
            // Session-scoped events are the ones that matter for the contract.
            data: type.startsWith("session.") ? deep(ev?.data) : undefined,
          })
        }
        emit("formB.ended", { total: n })
      } catch (e) {
        emit("formB.iterator.error", { error: String(e), after: n })
      }
    })()
  } catch (e) {
    emit("formB.error", { error: String(e) })
  }

  // Flush a summary at teardown, which the host calls on shutdown.
  return () => {
    emit("probe.teardown", {
      distinctTypes: counts.size,
      counts: Object.fromEntries(counts),
    })
  }
}

export default { id: "session-c-event-probe", setup }
