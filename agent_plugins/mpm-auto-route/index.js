// @openclaw/mpm-auto-route — Per-turn mode/persona auto-switch via MPM.
//
// Per-framework integration: MPM owns the selector (three-state gate:
// blank / auto / manual — see `~/.mpm/cmd/mpm/router.go::handleRoute`).
// This plugin is purely transport: capture the user prompt, spawn
// `mpm route --apply`, capture stdout, inject into the SOUL.md
// bootstrap file slot via the agent:bootstrap hook.
//
// Cost: one subprocess per turn (~10ms on a warm path). Negligible
// compared to a 2s+ LLM roundtrip.
//
// Failure modes (fail-open, all silent):
//   - `mpm` not on PATH → spawn errors, no stdout captured, no injection.
//   - active.json missing or malformed → handled by MPM (returns "" or
//     routes/renders as appropriate). Stdout captures whatever MPM emits.
//   - subprocess timeout → child killed, partial stdout discarded.

import { spawn } from "node:child_process";
import {
  definePluginEntry,
  registerInternalHook,
  DEFAULT_SOUL_FILENAME,
} from "@openclaw/plugin-sdk";

// sessionKey → most recent `mpm route --apply` stdout
const sessionReminders = new Map();

function getCurrentReminder(sessionKey) {
  return sessionReminders.get(sessionKey) ?? null;
}

function clearReminder(sessionKey) {
  sessionReminders.delete(sessionKey);
}

function runMpmRoute(prompt, mpmBin, timeoutMs) {
  return new Promise((resolve) => {
    let stdout = "";
    let settled = false;
    const finish = (value) => {
      if (settled) return;
      settled = true;
      resolve(value);
    };

    let child;
    try {
      child = spawn(mpmBin, ["route", "--apply", prompt], {
        stdio: ["ignore", "pipe", "pipe"],
      });
    } catch {
      // mpmBin not found, etc.
      finish("");
      return;
    }

    child.stdout.on("data", (chunk) => (stdout += chunk.toString()));
    child.on("error", () => finish(""));
    child.on("close", () => finish(stdout));

    const timer = setTimeout(() => {
      try {
        child.kill("SIGKILL");
      } catch {}
      finish(stdout);
    }, timeoutMs);
    timer.unref();
  });
}

// ─── Hook registrations ──────────────────────────────────────────────────

export default definePluginEntry({
  id: "mpm-auto-route",
  register(api) {
    const cfg = api?.config?.plugins?.entries?.["mpm-auto-route"] ?? {};
    const enabled = cfg.enabled !== false; // default true
    const mpmBin = typeof cfg.mpmBin === "string" && cfg.mpmBin ? cfg.mpmBin : "mpm";
    const timeoutMs = typeof cfg.timeoutMs === "number" ? cfg.timeoutMs : 5000;

    if (!enabled) {
      api?.log?.info?.("[mpm-auto-route] disabled by config");
      return;
    }

    // message:received — fires on every inbound user message.
    // Captures the prompt and runs `mpm route --apply` to populate
    // the per-session reminder. Fire-and-forget — failures are silent
    // (the next agent turn runs without a reminder, same as a fresh
    // install where MPM isn't installed).
    registerInternalHook("message:received", async (event) => {
      const sessionKey = event?.sessionKey;
      if (!sessionKey) return;
      const content = event?.context?.content;
      if (typeof content !== "string") return;
      const prompt = content.trim();
      if (!prompt) return;

      const stdout = await runMpmRoute(prompt, mpmBin, timeoutMs);
      if (stdout.trim()) {
        sessionReminders.set(sessionKey, stdout.trim());
      } else {
        // MPM emitted nothing (blank active.json, mpm missing, etc.) —
        // clear any stale reminder so the bootstrap hook doesn't inject
        // something from a prior turn.
        sessionReminders.delete(sessionKey);
      }
    });

    // agent:bootstrap — fires during prompt assembly.
    // Reads the cached reminder and appends it to the SOUL.md bootstrap
    // file content. No reminder → no mutation.
    registerInternalHook("agent:bootstrap", async (event) => {
      const sessionKey = event?.sessionKey;
      if (!sessionKey) return;

      const reminder = sessionReminders.get(sessionKey);
      if (!reminder) return;

      const ctx = event.context;
      const files = ctx?.bootstrapFiles;
      if (!Array.isArray(files)) return;

      const soulIdx = files.findIndex((f) => f?.name === DEFAULT_SOUL_FILENAME);

      if (soulIdx === -1) {
        // No SOUL.md in the bootstrap set — synthesize a slot.
        files.push({
          name: DEFAULT_SOUL_FILENAME,
          path: "<mpm-auto-route>",
          content: reminder,
          missing: false,
        });
        return;
      }

      const existing = files[soulIdx].content ?? "";
      files[soulIdx] = {
        ...files[soulIdx],
        content: existing ? `${existing}\n\n---\n\n${reminder}` : reminder,
      };
    });

    api?.log?.info?.(
      `[mpm-auto-route] registered (mpmBin=${mpmBin}, timeoutMs=${timeoutMs})`,
    );
  },
});

// Test surface — used by route-adapter.test.js if shipped alongside.
// Not part of the plugin contract; OpenClaw ignores these exports.
export { getCurrentReminder, clearReminder };