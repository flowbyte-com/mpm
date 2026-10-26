// src/wake-context.ts — wake-context extraction and per-session injection.
//
// WHY THIS MODULE EXISTS
// ----------------------
// `mpm_context / read_wake_context` with `format: "system-prompt"` returns
//
//     {"content": "<prose>", "format": "system-prompt", "success": true}
//
// at the TOP LEVEL of the `mpm call` envelope — there is no `payload`
// wrapper and no `context`/`text` key. The adapter's `callMpm` stores the
// whole parsed envelope in `CallResult.payload`, so the extraction used to
//
//     payload?.context ?? payload?.text ?? ""
//
// which yields "" for every real response. The hook then skipped the push
// (`if (context)`), the per-session cache never populated, and every
// subsequent `context` hook re-spawned `mpm` to fetch a context it
// discarded.
//
// The projection variants remain supported and must stay that way:
//
//   default projection        -> a flat wake-context object (no string body)
//   {projection: "compact"}   -> {"context": {…object…}, "projection": …}
//   {format: "system-prompt"} -> {"content": "<string>"}     <- canonical
//
// `context` in the compact projection is a DICT. Coercing it would push
// "[object Object]" into the system prompt, so extraction only ever returns
// a string, and only when the value really is one.
//
// The injector is separate from the plugin entry so the cache, retry and
// duplicate-suppression rules can be tested without loading the OpenCode
// host module (`src/index.v2.ts` imports `@opencode/plugin`, which is only
// resolvable inside a running host).

/** A part appended to the host's system prompt for the current build. */
export interface SystemPromptPart {
  type: string;
  text: string;
  [key: string]: unknown;
}

/**
 * Ordered candidate keys for the wake-context body, most-canonical first.
 *
 * `content` is what `format: "system-prompt"` actually returns and is the
 * only shape the current substrate emits. `context` and `text` are the
 * legacy/alternate string shapes the adapter has always accepted, and are
 * kept so a substrate-side rename does not silently blank the injection
 * again.
 */
export const WAKE_CONTEXT_TEXT_KEYS = ["content", "context", "text"] as const;

/**
 * Extract the wake-context body from a parsed `mpm call` envelope.
 *
 * Returns `""` — never a non-string coercion — when:
 *   * the envelope is null / not an object,
 *   * none of the candidate keys holds a string, or
 *   * the only candidate is empty or whitespace-only.
 *
 * The returned string is the value verbatim (no trimming), so formatting in
 * the substrate's prose survives.
 */
export function extractWakeContextText(payload: unknown): string {
  if (payload === null || typeof payload !== "object" || Array.isArray(payload)) {
    return "";
  }
  const envelope = payload as Record<string, unknown>;
  for (const key of WAKE_CONTEXT_TEXT_KEYS) {
    const value = envelope[key];
    if (typeof value === "string" && value.trim() !== "") {
      return value;
    }
  }
  return "";
}

export interface WakeContextInjectorDeps {
  /**
   * Performs the substrate call and returns the extracted text. Returning
   * `""` means "this attempt produced nothing" — the session is NOT marked
   * as injected, so a later hook can retry.
   */
  fetch: () => Promise<string>;
  /** Injectable for tests; defaults to a fresh Map. */
  cache?: Map<string, string>;
}

export interface WakeContextInjector {
  /** sessionID -> injected text. Populated only after a successful push. */
  readonly cache: Map<string, string>;
  /**
   * Fetch (if needed) and push the wake context onto `system`.
   *
   * Returns `true` exactly when this call pushed a part.
   *
   * Rules, in order:
   *   1. no `sessionID`            -> no fetch, no push (unattributable)
   *   2. already in the cache      -> no fetch, no push (duplicate suppression)
   *   3. a fetch is already running for the session -> no fetch, no push
   *      (two concurrent context builds must not double-inject)
   *   4. empty/failed fetch        -> no push, NOT cached  -> retry allowed
   *   5. non-empty fetch           -> cache + push         -> later calls skip
   */
  inject(sessionID: string | undefined, system: unknown): Promise<boolean>;
  /** Drops the cache and any in-flight bookkeeping (plugin teardown). */
  clear(): void;
}

export function createWakeContextInjector(
  deps: WakeContextInjectorDeps,
): WakeContextInjector {
  const cache = deps.cache ?? new Map<string, string>();
  const inFlight = new Set<string>();

  return {
    cache,

    async inject(sessionID: string | undefined, system: unknown): Promise<boolean> {
      if (!sessionID) {
        return false;
      }
      if (cache.has(sessionID)) {
        return false;
      }
      if (inFlight.has(sessionID)) {
        return false;
      }

      inFlight.add(sessionID);
      let text: string;
      try {
        text = await deps.fetch();
      } catch {
        // A thrown fetch is a failed attempt, not a permanent verdict: do
        // not cache, so the next context hook can retry.
        return false;
      } finally {
        inFlight.delete(sessionID);
      }

      if (!text) {
        return false;
      }
      if (!Array.isArray(system)) {
        // Nothing to push onto. Do not cache: the next attempt may get a
        // usable event object, and caching here would suppress the
        // injection permanently for this session.
        return false;
      }

      cache.set(sessionID, text);
      system.push({ type: "text", text });
      return true;
    },

    clear(): void {
      cache.clear();
      inFlight.clear();
    },
  };
}
