// mpm-opencode — OpenCode 2 directory-plugin entry shim.
//
// OpenCode's plugin-source scanner resolves a directory plugin by trying
// module names "server" then "index" AT THE DIRECTORY ROOT (verified
// against opencode v2.0.30's loader: it never consults package.json
// main/exports, and a directory registered without such a root module is
// dropped silently — no warning, empty plugin registry). The real plugin
// payload lives in dist/index.v2.js, so this root shim re-exports it.
//
// Stage only the files in install.sh's OPENCODE_RUNTIME_MANIFEST — this
// shim must sit at the STAGED PACKAGE ROOT for the registration to
// resolve.
export { default } from "./dist/index.v2.js";
