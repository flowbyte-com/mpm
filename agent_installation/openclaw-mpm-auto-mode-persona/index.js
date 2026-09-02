// @openclaw/mpm-auto-mode-persona — Per-turn mode/persona auto-switch via MPM.
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
import { withWorkspace } from "./lib/workspace.js";
import { definePluginEntry } from "openclaw/plugin-sdk/plugin-entry";
import { registerInternalHook } from "openclaw/plugin-sdk/hook-runtime";

// `openclaw/plugin-sdk` (root) is not a valid subpath in the current SDK
// (see node_modules/openclaw/package.json `exports`), and `DEFAULT_SOUL_FILENAME`
// is not re-exported from any plugin-sdk/* subpath either. The constant's
// value is "SOUL.md" (the canonical bootstrap file name). Hardcoding matches
// the SDK source (dist/workspace-CAteGiRq.js:63) and keeps the import surface
// minimal.
const DEFAULT_SOUL_FILENAME = "SOUL.md";

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
        env: withWorkspace(),
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
  id: "openclaw-mpm-auto-mode-persona",
  register(api) {
    // OpenClaw plugin config lives under entries[id].config (not entries[id]
    // directly). Reading from the bare entry silently picks up nothing — the
    // configured mpmBin/timeoutMs values never reach this code, and `mpm` is
    // resolved from PATH only. Fixes finding (A) of the 2026-09-02 forensic
    // audit; same nesting is used by openclaw-mpm-memory.
    const entry =
      api?.config?.plugins?.entries?.["openclaw-mpm-auto-mode-persona"];
    const cfg = entry?.config ?? {};
    const enabled = cfg.enabled !== false; // default true
    const mpmBin = typeof cfg.mpmBin === "string" && cfg.mpmBin ? cfg.mpmBin : "mpm";
    const timeoutMs = typeof cfg.timeoutMs === "number" ? cfg.timeoutMs : 5000;

    if (!enabled) {
      api?.log?.info?.("[openclaw-mpm-auto-mode-persona] disabled by config");
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
      // Consume-and-clear: bounds the Map at "concurrently processing turns"
      // (typically ≤1 per session) and prevents stale reminders from a prior
      // turn being injected when the current turn's `mpm route` returned empty.
      sessionReminders.delete(sessionKey);

      const ctx = event.context;
      const files = ctx?.bootstrapFiles;
      if (!Array.isArray(files)) return;

      const soulIdx = files.findIndex((f) => f?.name === DEFAULT_SOUL_FILENAME);

      if (soulIdx === -1) {
        // No SOUL.md in the bootstrap set — synthesize a slot.
        files.push({
          name: DEFAULT_SOUL_FILENAME,
          path: "<openclaw-mpm-auto-mode-persona>",
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
      `[openclaw-mpm-auto-mode-persona] registered (mpmBin=${mpmBin}, timeoutMs=${timeoutMs})`,
    );
  },
});

// Test surface — used by route-adapter.test.js if shipped alongside.
// Not part of the plugin contract; OpenClaw ignores these exports.
export { getCurrentReminder, clearReminder };