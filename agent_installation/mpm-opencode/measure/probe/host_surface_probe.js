// Session C — OpenCode v2.1.4 host-surface probe, round 2.
//
// Round 1 established the ctx surface. Round 2 answers the decisive
// questions with the hook names the host actually fires:
//
//   ctx.tool.hook("execute.before" | "execute.after")
//   ctx.session.hook("context" | "compaction" | "generate" | "model.request" |
//                    "title" | "http.request" | "http.response" | "retry")
//   ctx.permission.hook("evaluate")
//   ctx.shell.hook("create.before")
//   ctx.event.subscribe(cb)
//
// and captures ctx.location (project identity) in full.
//
// The host's tool-hook dispatcher only understands "execute.before" and
// "execute.after" (verified statically in the v2.1.4 plugin-context builder:
// `hook:(o,r)=>I(e.tool.hook(o,(n)=>oe[o](()=>Promise.resolve(r(n)))))` where
// `oe` has exactly those two keys). Registering any other name there throws
// a TypeError the first time a tool executes — so round 1's bulk
// registration is NOT repeated.

import { appendFileSync } from "node:fs"

const OUT = process.env.PROBE_OUT || "/tmp/opencode/probe-c/out/probe2.jsonl"
const PID = process.pid

function emit(kind, payload) {
  try {
    appendFileSync(OUT, JSON.stringify({ t: Date.now(), pid: PID, kind, ...payload }) + "\n")
  } catch {
    /* probe must never break the host */
  }
}

function deep(v, depth = 0, seen = new WeakSet()) {
  if (v === null) return null
  if (typeof v !== "object") {
    if (typeof v === "function") return "[fn]"
    if (typeof v === "string" && v.length > 300) return v.slice(0, 300) + "…"
    return v
  }
  if (seen.has(v)) return "[circular]"
  if (depth >= 4) return "[depth]"
  seen.add(v)
  if (Array.isArray(v)) return v.slice(0, 12).map((x) => deep(x, depth + 1, seen))
  const out = {}
  for (const k of Object.keys(v)) out[k] = deep(v[k], depth + 1, seen)
  return out
}

export default {
  id: "session-c-probe-2",

  async setup(ctx) {
    emit("boot", { argv: process.argv.slice(1, 6), cwd: process.cwd() })

    // ---- ctx.location — the project identity surface ------------------------
    const loc = {}
    for (const k of ["directory", "workspaceID", "project"]) {
      try {
        loc[k] = deep(ctx.location?.[k])
      } catch (e) {
        loc[k] = { error: String(e) }
      }
    }
    // the project object may carry more than the three named fields
    try {
      loc.locationOwnKeys = Object.keys(ctx.location ?? {})
      loc.projectOwnKeys = ctx.location?.project ? Object.getOwnPropertyNames(ctx.location.project) : null
      loc.projectProtoKeys = ctx.location?.project
        ? Object.getOwnPropertyNames(Object.getPrototypeOf(ctx.location.project) ?? {})
        : null
      loc.directoryCtor = ctx.location?.directory?.constructor?.name
      loc.directoryProtoKeys = ctx.location?.directory
        ? Object.getOwnPropertyNames(Object.getPrototypeOf(ctx.location.directory) ?? {})
        : null
    } catch (e) {
      loc.probeError = String(e)
    }
    emit("location", { loc })

    // ---- ctx.event.subscribe — the host event stream -----------------------
    if (ctx.event && typeof ctx.event.subscribe === "function") {
      try {
        const disp = ctx.event.subscribe((...args) => {
          emit("host.event", {
            argc: args.length,
            arg0Keys: args[0] && typeof args[0] === "object" ? Object.keys(args[0]) : null,
            arg0: deep(args[0]),
            arg1: deep(args[1]),
          })
        })
        emit("event.subscribe", { status: "ok", dispType: typeof disp, dispKeys: disp && typeof disp === "object" ? Object.keys(disp) : null })
      } catch (e) {
        emit("event.subscribe", { status: "error", error: String(e) })
      }
    }

    // ---- ctx.tool.hook — the only two valid names --------------------------
    const toolHookNames = ["execute.before", "execute.after"]
    for (const name of toolHookNames) {
      try {
        await ctx.tool.hook(name, (payload) => {
          emit("tool.hook", { hook: name, payloadKeys: payload && typeof payload === "object" ? Object.keys(payload) : null, payload: deep(payload) })
        })
        emit("tool.hook.registered", { hook: name, status: "ok" })
      } catch (e) {
        emit("tool.hook.registered", { hook: name, status: "error", error: String(e) })
      }
    }

    // ---- ctx.session.hook — every name the host fires ----------------------
    const sessionHookNames = [
      "context", "compaction", "generate", "model.request",
      "title", "http.request", "http.response", "retry",
      "experimental.ws.handshake",
    ]
    for (const name of sessionHookNames) {
      try {
        await ctx.session.hook(name, (payload) => {
          emit("session.hook", {
            hook: name,
            payloadKeys: payload && typeof payload === "object" ? Object.keys(payload) : null,
            payload: deep(payload),
          })
        })
        emit("session.hook.registered", { hook: name, status: "ok" })
      } catch (e) {
        emit("session.hook.registered", { hook: name, status: "error", error: String(e) })
      }
    }

    // ---- ctx.permission.hook("evaluate") -----------------------------------
    try {
      await ctx.permission.hook("evaluate", (payload) => {
        emit("permission.hook", { hook: "evaluate", payload: deep(payload) })
      })
      emit("permission.hook.registered", { status: "ok" })
    } catch (e) {
      emit("permission.hook.registered", { status: "error", error: String(e) })
    }

    // ---- ctx.shell.hook("create.before") — the hook MPM already uses -------
    try {
      await ctx.shell.hook("create.before", (payload) => {
        emit("shell.hook", {
          hook: "create.before",
          payloadKeys: payload && typeof payload === "object" ? Object.keys(payload) : null,
          hasSessionID: "sessionID" in (payload ?? {}),
          payload: deep(payload),
        })
      })
      emit("shell.hook.registered", { status: "ok" })
    } catch (e) {
      emit("shell.hook.registered", { status: "error", error: String(e) })
    }

    // ---- a probe tool so that tool.execute.before/after actually fire ------
    await ctx.tool.transform((editor) => {
      editor.add({
        name: "probe_tool",
        description: "Session C host probe.",
        input: { type: "object", properties: { note: { type: "string" } } },
        async execute(input, toolCtx) {
          const vals = {}
          if (toolCtx && typeof toolCtx === "object") {
            for (const k of Object.getOwnPropertyNames(toolCtx)) {
              try {
                const v = toolCtx[k]
                vals[k] = typeof v === "function" ? "[fn]" : deep(v)
              } catch (e) {
                vals[k] = { error: String(e) }
              }
            }
          }
          emit("tool.execute_ctx", { input, vals })
          return { content: "probe_ok" }
        },
      })
    })

    // ---- try to force a compaction so the compaction hook fires ------------
    // ctx.session.compact({sessionID}) — best effort, failures are recorded.
    try {
      const sessions = await ctx.session.list()
      emit("session.list", { n: Array.isArray(sessions) ? sessions.length : null, sample: deep(Array.isArray(sessions) ? sessions[0] : sessions) })
    } catch (e) {
      emit("session.list", { error: String(e) })
    }

    emit("setup.complete", {})
    return () => emit("teardown", {})
  },
}
