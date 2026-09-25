// Loader that stubs the openclaw SDK imports for the persona idempotency
// test driver. Records every handler passed to registerInternalHook in
// globalThis.__MPM_HOOKS so the driver can invoke them deterministically.

const HANDLED = new Set([
  "openclaw/plugin-sdk/plugin-entry",
  "openclaw/plugin-sdk/hook-runtime",
]);

const HOOKS = new Map();
globalThis.__MPM_HOOKS = HOOKS;

const SOURCES = {
  "openclaw-stub://plugin-entry.js":
    "export function definePluginEntry(entry) { return entry; }",
  "openclaw-stub://hook-runtime.js":
    [
      "const HOOKS = new Map();",
      "globalThis.__MPM_HOOKS = HOOKS;",
      "export function registerInternalHook(name, fn) {",
      "  if (!HOOKS.has(name)) HOOKS.set(name, []);",
      "  HOOKS.get(name).push(fn);",
      "}",
      "export async function triggerInternalHook(name, event) {",
      "  const list = HOOKS.get(name) ?? [];",
      "  for (const fn of list) await fn(event);",
      "}",
      "export function clearInternalHooks() { HOOKS.clear(); }",
    ].join("\n"),
};

export async function resolve(specifier, context, nextResolve) {
  if (HANDLED.has(specifier)) {
    let stub;
    if (specifier.endsWith("plugin-entry")) {
      stub = "openclaw-stub://plugin-entry.js";
    } else {
      stub = "openclaw-stub://hook-runtime.js";
    }
    return { url: stub, shortCircuit: true, format: "module" };
  }
  return nextResolve(specifier, context);
}

export async function load(url, context, nextLoad) {
  if (url in SOURCES) {
    return { format: "module", shortCircuit: true, source: SOURCES[url] };
  }
  return nextLoad(url, context);
}
