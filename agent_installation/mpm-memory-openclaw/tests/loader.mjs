// Loader that intercepts openclaw SDK imports and stubs them for
// the runtime-injection test driver. Invoked via
// `--experimental-loader ./tests/loader.mjs` (or `--loader` on
// older Node versions).

const HANDLED = new Set([
  "openclaw/plugin-sdk/plugin-entry",
  "openclaw/plugin-sdk/tool-results",
  "openclaw/plugin-sdk/hook-runtime",
]);

const URL_TO_SOURCE = {
  "openclaw-stub://plugin-entry.js": `export function definePluginEntry(entry) {
  globalThis.__capturedPlugin = entry;
  return entry;
}`,
  "openclaw-stub://tool-results.js": `export function jsonResult(o) { return { jsonResult: true, value: o }; }
export function textResult(s) { return { textResult: true, value: s }; }`,
  "openclaw-stub://hook-runtime.js": `export function registerInternalHook() {}
export function triggerInternalHook() {}`,
};

export async function resolve(specifier, context, nextResolve) {
  if (HANDLED.has(specifier)) {
    const stub =
      specifier === "openclaw/plugin-sdk/plugin-entry"
        ? "openclaw-stub://plugin-entry.js"
        : specifier === "openclaw/plugin-sdk/tool-results"
        ? "openclaw-stub://tool-results.js"
        : "openclaw-stub://hook-runtime.js";
    return {
      url: stub,
      shortCircuit: true,
      format: "module",
    };
  }
  return nextResolve(specifier, context);
}

export async function load(url, context, nextLoad) {
  if (url in URL_TO_SOURCE) {
    return {
      format: "module",
      shortCircuit: true,
      source: URL_TO_SOURCE[url],
    };
  }
  return nextLoad(url, context);
}