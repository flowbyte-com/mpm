// server.js — the directory-plugin entrypoint.
//
// The OpenCode v2 loader resolves a DIRECTORY plugin by a root-level
// `server.js` (or `index.js`) and never consults `package.json`. A probe
// directory without one is silently skipped: "the probe saw nothing" and
// "the probe never loaded" look identical in an empty log file.
export { default } from "./session_end_probe.js"
