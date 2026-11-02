package update

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// ---------------------------------------------------------------------
// The canonical runtime payload manifest
// ---------------------------------------------------------------------
//
// CANONICAL SOURCE: scripts/runtime_payloads.json. The shell staging
// path (scripts/lib/runtime_bundle.sh, rb_stage_payloads) reads the
// SAME file, and the adapter's own OPENCODE_RUNTIME_MANIFEST must
// agree with it — scripts/tests/test_runtime_bundle.py fails if the two
// drift apart.
//
// This file is DATA, not code. Nothing in this file names an adapter:
// adding a host adapter is a data change, and the updater keeps up
// without a code change.
//
// # WHY THE UPDATER NEEDS IT AT ALL
//
// The payload of a runtime package is not one file. The OpenCode
// package is a directory whose entrypoint (server.js) re-exports a
// compiled module (dist/index.v2.js) which in turn imports six sibling
// modules. The host registers the DIRECTORY, so a package missing any
// one of those modules loads NOTHING — silently, because the
// registration itself succeeds and the plugin registry simply stays
// empty.
//
// The updater used to stage two of the nine declared files and
// validate those two. A bundle built that way passed every check and
// installed a plugin that could never load.
const payloadManifestRelPath = "scripts/runtime_payloads.json"

// Where the declaration travels inside a bundle.
//
// A bundle must describe its own contents. The validator that checks a
// candidate must not depend on a source tree it does not have (a
// downloaded release) or on an updater binary built from a different
// revision than the bundle (a stale updater checking a newer bundle).
// Shipping the declaration with the thing it declares removes both
// dependencies: the bundle says what it carries, and the validator
// reads that.
//
// The copy is made by the staging step — rb_stage_payloads in the shell
// path, stagePayloads here — so both producers emit the same shape.
const bundlePayloadManifestRelPath = "agent_installation/runtime_payloads.json"

// payloadManifest is the parsed runtime payload manifest.
type payloadManifest struct {
	Version  int             `json:"version"`
	Payloads []payloadRecord `json:"payloads"`
}

// payloadRecord is one declared runtime package payload.
type payloadRecord struct {
	// Adapter is the directory under agent_installation/ that owns the
	// package's source.
	Adapter string `json:"adapter"`
	// PluginID is the id the package is registered under.
	PluginID string `json:"plugin_id"`
	// Validator names the shell validator that proves the staged
	// package is loadable. The Go validator implements the same rules
	// (validatePayloadPackage); the name is carried so the two can be
	// compared rather than assumed to agree.
	Validator string `json:"validator"`
	// Files is the EXPLICIT allowlist of files the package contains.
	Files []string `json:"files"`
}

// loadPayloadManifest reads and validates the manifest at path.
//
// FAIL CLOSED. A manifest that cannot be read, does not parse, or
// declares an incomplete record is an ERROR, never an empty manifest:
// an empty manifest would stage nothing and produce a bundle whose
// plugin loads nothing — the exact silent failure the manifest exists
// to prevent.
func loadPayloadManifest(path string) (payloadManifest, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return payloadManifest{}, fmt.Errorf("the runtime payload manifest could not be read at %s: %w", path, err)
	}
	return parsePayloadManifest(raw, path)
}

// parsePayloadManifest parses manifest bytes and rejects an incomplete
// declaration.
func parsePayloadManifest(raw []byte, origin string) (payloadManifest, error) {
	var m payloadManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return payloadManifest{}, fmt.Errorf("the runtime payload manifest at %s does not parse: %w", origin, err)
	}
	if len(m.Payloads) == 0 {
		return payloadManifest{}, fmt.Errorf("the runtime payload manifest at %s declares no payloads", origin)
	}
	seen := map[string]bool{}
	for i, p := range m.Payloads {
		switch {
		case p.Adapter == "":
			return payloadManifest{}, fmt.Errorf("the runtime payload manifest at %s declares a payload with no adapter (record %d)", origin, i)
		case p.PluginID == "":
			return payloadManifest{}, fmt.Errorf("the runtime payload manifest at %s declares a payload for %q with no plugin id (record %d)", origin, p.Adapter, i)
		case p.Validator == "":
			return payloadManifest{}, fmt.Errorf("the runtime payload manifest at %s declares a payload for %q with no validator (record %d)", origin, p.Adapter, i)
		case len(p.Files) == 0:
			return payloadManifest{}, fmt.Errorf("the runtime payload manifest at %s declares a payload for %q with no files (record %d)", origin, p.Adapter, i)
		}
		if seen[p.Adapter] {
			return payloadManifest{}, fmt.Errorf("the runtime payload manifest at %s declares the adapter %q twice", origin, p.Adapter)
		}
		seen[p.Adapter] = true
		for _, f := range p.Files {
			if err := checkPayloadFileName(f); err != nil {
				return payloadManifest{}, fmt.Errorf("the runtime payload manifest at %s is invalid: %w", origin, err)
			}
		}
	}
	return m, nil
}

// checkPayloadFileName rejects a declared file that would escape the
// adapter's own directory. A manifest is data, and data from a
// checkout is still data: it must not be able to name a path outside
// the package it belongs to.
func checkPayloadFileName(f string) error {
	if f == "" {
		return fmt.Errorf("it declares an empty file name")
	}
	if strings.HasPrefix(f, "/") || filepath.IsAbs(f) {
		return fmt.Errorf("it declares the absolute path %q", f)
	}
	clean := filepath.ToSlash(filepath.Clean(f))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return fmt.Errorf("it declares the path %q, which is outside the package", f)
	}
	return nil
}

// ---------------------------------------------------------------------
// Staging
// ---------------------------------------------------------------------

// payloadSources maps each declared adapter to the directory its
// runtime package must be staged FROM.
//
// For an adapter that needs no build this is the checkout itself. For
// one that does, it is an isolated build tree — see buildRuntimePayloads.
type payloadSources map[string]string

// stagePayloads stages every declared runtime package payload from a
// source checkout into a candidate bundle.
//
// The file list comes from the canonical manifest, so there is no
// second, hand-maintained copy of what a package contains.
//
// FAIL CLOSED. A package the manifest declares but the checkout does
// not contain is a BUILD ERROR: copying a partial set would stage
// exactly the broken state this exists to prevent.
func stagePayloads(srcDir, dstDir string, sources payloadSources) error {
	manifest, err := loadPayloadManifest(filepath.Join(srcDir, filepath.FromSlash(payloadManifestRelPath)))
	if err != nil {
		return err
	}
	for _, p := range manifest.Payloads {
		from := sources[p.Adapter]
		if from == "" {
			from = filepath.Join(srcDir, "agent_installation", p.Adapter)
		}
		if fi, err := os.Stat(from); err != nil || !fi.IsDir() {
			return fmt.Errorf("the release declares a runtime payload for %q but %s does not exist", p.Adapter, from)
		}
		for _, f := range p.Files {
			src := filepath.Join(from, filepath.FromSlash(f))
			dst := filepath.Join(dstDir, "agent_installation", p.Adapter, filepath.FromSlash(f))
			if err := copyPayloadFile(src, dst, p.Adapter, f); err != nil {
				return err
			}
		}
		// The staged package must be able to LOAD. A package that is
		// present but unloadable is the failure this whole path
		// exists to prevent, and it is only detectable after staging.
		if err := validatePayloadPackage(filepath.Join(dstDir, "agent_installation", p.Adapter), p); err != nil {
			return err
		}
	}
	// The bundle carries its own declaration, so the validator that
	// checks it needs neither the source tree nor a matching updater.
	return writeBundlePayloadManifest(dstDir, manifest)
}

// copyPayloadFile copies one declared file, refusing anything that is
// missing, not a regular file, or empty.
func copyPayloadFile(src, dst, adapter, name string) error {
	fi, err := os.Stat(src)
	if err != nil {
		return fmt.Errorf("the runtime payload for %q declares %s, which is missing from the build output", adapter, name)
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("the runtime payload for %q declares %s, which is not a regular file", adapter, name)
	}
	if fi.Size() == 0 {
		return fmt.Errorf("the runtime payload for %q declares %s, which is empty", adapter, name)
	}
	raw, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return err
	}
	return os.WriteFile(dst, raw, 0644)
}

// writeBundlePayloadManifest copies the manifest into the bundle.
func writeBundlePayloadManifest(dstDir string, m payloadManifest) error {
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	dst := filepath.Join(dstDir, filepath.FromSlash(bundlePayloadManifestRelPath))
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return err
	}
	return os.WriteFile(dst, append(raw, '\n'), 0644)
}

// ---------------------------------------------------------------------
// Loadability
// ---------------------------------------------------------------------

// jsImportPatterns are the forms a JavaScript module can use to pull in
// a sibling.
//
// Mirrors the shell validator's extraction
// (agent_installation/scripts/stage_runtime_package.sh,
// mpm_unresolved_relative_imports) so the Go and shell validators agree
// on what "unresolved" means. A divergence here would let a bundle
// through that the installer refuses, or the reverse.
var jsImportPatterns = []*regexp.Regexp{
	regexp.MustCompile(`\bfrom\s+['"]([^'"]+)['"]`),
	regexp.MustCompile(`\bimport\s+['"]([^'"]+)['"]`),
	regexp.MustCompile(`\brequire\s*\(\s*['"]([^'"]+)['"]\s*\)`),
}

// validatePayloadPackage proves a staged runtime package can load.
//
// Two rules, and both exist because their absence is SILENT:
//
//  1. every file the manifest declares is present, regular and
//     non-empty — a declared file that is missing is a package that
//     loads nothing;
//  2. every relative import inside the package resolves to a file in
//     the package — the host registers a DIRECTORY, so a broken import
//     surfaces as an empty plugin registry, not as an error.
//
// The rules are manifest-driven, so they hold for any adapter without
// this code naming one.
func validatePayloadPackage(pkgDir string, p payloadRecord) error {
	if fi, err := os.Stat(pkgDir); err != nil || !fi.IsDir() {
		return fmt.Errorf("the runtime payload for %q is missing at %s", p.Adapter, pkgDir)
	}
	for _, f := range p.Files {
		full := filepath.Join(pkgDir, filepath.FromSlash(f))
		fi, err := os.Stat(full)
		switch {
		case err != nil:
			return fmt.Errorf("the runtime payload for %q is missing the declared file %s", p.Adapter, f)
		case !fi.Mode().IsRegular():
			return fmt.Errorf("the runtime payload for %q declares %s, which is not a regular file", p.Adapter, f)
		case fi.Size() == 0:
			return fmt.Errorf("the runtime payload for %q declares %s, which is empty", p.Adapter, f)
		}
		if strings.HasSuffix(f, ".json") {
			if err := checkJSONPayload(pkgDir, full, p.Adapter, f); err != nil {
				return err
			}
		}
	}
	unresolved, err := unresolvedRelativeImports(pkgDir)
	if err != nil {
		return err
	}
	if len(unresolved) > 0 {
		return fmt.Errorf("the runtime payload for %q has unresolved internal imports: %s", p.Adapter, strings.Join(unresolved, ", "))
	}
	return nil
}

// checkJSONPayload proves a declared JSON file parses, and — for a
// package manifest — that the entrypoint it names is present.
//
// An npm-shaped package whose "main" names a file it does not carry
// resolves to nothing, which is the same silent failure as a missing
// module.
func checkJSONPayload(pkgDir, path, adapter, name string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var doc map[string]interface{}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("the runtime payload for %q declares %s, which does not parse: %w", adapter, name, err)
	}
	if filepath.Base(name) != "package.json" {
		return nil
	}
	main, _ := doc["main"].(string)
	if main == "" {
		return nil
	}
	target := filepath.Join(pkgDir, filepath.FromSlash(strings.TrimPrefix(main, "./")))
	if fi, err := os.Stat(target); err != nil || !fi.Mode().IsRegular() {
		return fmt.Errorf("the runtime payload for %q declares %s, whose entrypoint %q is not in the package", adapter, name, main)
	}
	return nil
}

// unresolvedRelativeImports reports every relative import inside a
// package's .js files that does not resolve to a file in the package.
//
// Only RELATIVE specifiers are ours to satisfy. Bare specifiers
// ("@opencode/plugin", "node:fs") are supplied by the host framework or
// the runtime; they are unresolvable in the source checkout too, and
// checking them would fail every correct package.
//
// The point of the check: the package is registered by DIRECTORY, and a
// module that imports a sibling which is not there loads NOTHING —
// silently, because the registration succeeds.
func unresolvedRelativeImports(pkgDir string) ([]string, error) {
	var bad []string
	err := filepath.Walk(pkgDir, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if fi.IsDir() || !strings.HasSuffix(fi.Name(), ".js") {
			return nil
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		text := string(raw)
		rel, rerr := filepath.Rel(pkgDir, p)
		if rerr != nil {
			rel = p
		}
		for _, pat := range jsImportPatterns {
			for _, m := range pat.FindAllStringSubmatch(text, -1) {
				spec := m[1]
				if !strings.HasPrefix(spec, "./") && !strings.HasPrefix(spec, "../") {
					continue
				}
				if !resolvesInPackage(pkgDir, p, spec) {
					bad = append(bad, fmt.Sprintf("%s -> %s", filepath.ToSlash(rel), spec))
				}
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(bad)
	return bad, nil
}

// resolvesInPackage reports whether a relative import specifier names a
// file that is present in the package.
//
// The candidate set mirrors the shell validator: the specifier as
// written, and — when it carries an extension — the same stem with
// each accepted extension.
func resolvesInPackage(pkgDir, importer, spec string) bool {
	base := filepath.Clean(filepath.Join(filepath.Dir(importer), filepath.FromSlash(spec)))
	candidates := []string{base}
	if ext := filepath.Ext(base); ext != "" {
		candidates = append(candidates, strings.TrimSuffix(base, ext)+".js")
	}
	for _, c := range candidates {
		if fi, err := os.Stat(c); err == nil && fi.Mode().IsRegular() {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------
// Building the payload
// ---------------------------------------------------------------------

// payloadBuildEnv is where a payload build may write.
type payloadBuildEnv struct {
	// Scratch is the isolated build tree: a fresh copy of the
	// adapter's sources, and nothing else. It is never inside the
	// operator's HOME, and never inside the checkout it was built
	// from.
	Scratch string
	// Cache holds the toolchain's caches (npm's). It is never inside
	// the operator's HOME. A caller that builds several candidates can
	// point them at one cache, so a repeated build does not
	// re-download the world.
	Cache string
}

// buildRuntimePayloads builds the runtime package payloads declared by
// the canonical manifest and returns where each adapter's package is to
// be staged from.
//
// # WHY THE BUILD IS NECESSARY
//
// The compiled plugin output (agent_installation/mpm-opencode/dist/) is
// a BUILD PRODUCT: it is not in source control, and it is gitignored.
// A fresh clone — which is exactly what --branch and --commit produce,
// and what a developer who has never run the npm build has — contains
// no payload at all. Staging "whatever is there" either fails or, far
// worse, copies compiled files left behind by another revision.
//
// # WHY THE BUILD IS ISOLATED
//
// An adapter that needs a build is built in a scratch COPY of its
// source tree. The updater promises that a local source is read only
// ("the build writes only to the destination bundle directory"), and
// `npm ci` would break that promise twice over: it creates
// node_modules/ and it rewrites dist/ in a tree the operator owns.
//
// # WHY THERE IS NO FALLBACK
//
// A missing Node, a missing npm, a failed install and a failed build
// are all ERRORS that stop the build. The updater never falls back to
// the installed plugin, and never reuses compiled files from another
// revision: a bundle must carry a payload built from the revision it
// claims, or it must not be built at all.
//
// The PUBLISHED RELEASE path never reaches this code — a release
// bundle is assembled by the release pipeline, which builds the
// payload in its own environment. Updating from a published release
// therefore needs neither Node nor npm.
func buildRuntimePayloads(srcDir string, env payloadBuildEnv) (payloadSources, error) {
	manifest, err := loadPayloadManifest(filepath.Join(srcDir, filepath.FromSlash(payloadManifestRelPath)))
	if err != nil {
		return nil, err
	}
	sources := payloadSources{}
	for _, p := range manifest.Payloads {
		adapterDir := filepath.Join(srcDir, "agent_installation", p.Adapter)
		if fi, err := os.Stat(adapterDir); err != nil || !fi.IsDir() {
			return nil, fmt.Errorf("the release declares a runtime payload for %q but %s does not exist", p.Adapter, adapterDir)
		}
		needsBuild, err := adapterNeedsBuild(adapterDir)
		if err != nil {
			return nil, err
		}
		if !needsBuild {
			sources[p.Adapter] = adapterDir
			continue
		}
		// The toolchain is checked BEFORE anything is copied or run, so
		// an operator without Node is told THAT — not a git error from
		// a step that happens to come earlier in the sequence.
		if _, err := npmBinary(); err != nil {
			return nil, err
		}
		buildDir := filepath.Join(env.Scratch, "agent_installation", p.Adapter)
		if err := copyAdapterForBuild(adapterDir, buildDir, p); err != nil {
			return nil, err
		}
		if err := runAdapterBuild(buildDir, env.Cache); err != nil {
			return nil, err
		}
		sources[p.Adapter] = buildDir
	}
	return sources, nil
}

// adapterNeedsBuild reports whether an adapter's package declares a
// build script, and therefore whether its payload has to be compiled
// before it can be staged.
func adapterNeedsBuild(adapterDir string) (bool, error) {
	pkgPath := filepath.Join(adapterDir, "package.json")
	raw, err := os.ReadFile(pkgPath)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil // no npm package: nothing to build
		}
		return false, err
	}
	var pkg struct {
		Scripts map[string]string `json:"scripts"`
	}
	if err := json.Unmarshal(raw, &pkg); err != nil {
		return false, fmt.Errorf("the adapter's package.json at %s does not parse: %w", pkgPath, err)
	}
	_, ok := pkg.Scripts["build"]
	return ok, nil
}

// copyAdapterForBuild copies an adapter's source tree into a scratch
// build directory, leaving behind exactly the things a build must not
// inherit:
//
//   - node_modules/, which is an install artefact and can be enormous;
//   - the DECLARED files that are not in source control, which are the
//     previous build's outputs. Copying them would stage a stale
//     module if the current source no longer produces it — the exact
//     "stale compiled files" the build exists to avoid.
//
// Everything else is copied verbatim: the build must see the same
// sources the checkout has.
func copyAdapterForBuild(adapterDir, buildDir string, p payloadRecord) error {
	if err := os.MkdirAll(buildDir, 0755); err != nil {
		return err
	}
	tracked, err := trackedFiles(adapterDir)
	if err != nil {
		return err
	}
	return filepath.Walk(adapterDir, func(path string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(adapterDir, path)
		if rerr != nil {
			return rerr
		}
		if rel == "." {
			return nil
		}
		if fi.IsDir() {
			if fi.Name() == "node_modules" {
				return filepath.SkipDir
			}
			return os.MkdirAll(filepath.Join(buildDir, rel), 0755)
		}
		if !fi.Mode().IsRegular() {
			return nil
		}
		// A declared file that is not tracked by git is a build
		// output: it must not be inherited from a previous build.
		if tracked[filepath.ToSlash(rel)] {
			return copyRegularFile(path, filepath.Join(buildDir, rel))
		}
		for _, d := range p.Files {
			if filepath.ToSlash(d) == filepath.ToSlash(rel) {
				return nil
			}
		}
		return copyRegularFile(path, filepath.Join(buildDir, rel))
	})
}

// copyRegularFile copies one file, creating the parent directory.
func copyRegularFile(src, dst string) error {
	raw, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return err
	}
	return os.WriteFile(dst, raw, 0644)
}

// trackedFiles returns the set of files under an adapter directory that
// git tracks, as slash-separated paths relative to that directory.
//
// A file git does not track is a build output or local residue.
//
// The listing is taken from INSIDE the adapter directory, so the paths
// git reports are already relative to it. A pathspec would be resolved
// against that same directory — asking for a repository-root-relative
// prefix from inside a subdirectory matches nothing, and every file
// would then look like an untracked build output.
func trackedFiles(adapterDir string) (map[string]bool, error) {
	out := map[string]bool{}
	cmd := exec.Command("git", "-C", adapterDir, "ls-files", "-z", "--", ".") //nolint:gosec
	raw, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("could not list the tracked files of %s: %w", adapterDir, err)
	}
	for _, name := range strings.Split(string(raw), "\x00") {
		if name == "" {
			continue
		}
		out[filepath.ToSlash(name)] = true
	}
	return out, nil
}

// runAdapterBuild runs the documented build for an adapter:
//
//	npm ci --no-audit --no-fund
//	npm run build
//
// npm ci is used when the adapter ships a lockfile — it is the
// documented command and it installs exactly the locked tree. Without
// a lockfile there is nothing to install reproducibly, so npm install
// is the honest fallback.
//
// The cache lives in the updater's scratch area, never in the
// operator's HOME: an isolated update runs with HOME pointed at the
// data root, and a cache there would scatter downloaded packages
// across the installation.
func runAdapterBuild(buildDir, scratchDir string) error {
	npm, err := npmBinary()
	if err != nil {
		return err
	}
	cache := filepath.Join(scratchDir, "npm-cache")
	if err := os.MkdirAll(cache, 0755); err != nil {
		return err
	}
	install := []string{"ci", "--no-audit", "--no-fund"}
	if _, serr := os.Stat(filepath.Join(buildDir, "package-lock.json")); serr != nil {
		install = []string{"install", "--no-audit", "--no-fund"}
	}
	if err := runNpm(buildDir, cache, npm, install...); err != nil {
		return fmt.Errorf("the runtime payload for %s could not be installed: %w", filepath.Base(buildDir), err)
	}
	if err := runNpm(buildDir, cache, npm, "run", "build"); err != nil {
		return fmt.Errorf("the runtime payload for %s could not be built: %w", filepath.Base(buildDir), err)
	}
	return nil
}

// runNpm runs one npm command in dir and reports its output on
// failure. The output is included because npm's error is almost always
// in it, and an update that fails with "npm failed" and no detail is
// an update the operator cannot act on.
func runNpm(dir, cache, npm string, args ...string) error {
	cmd := exec.Command(npm, args...) //nolint:gosec
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"npm_config_cache="+cache,
		"npm_config_audit=false",
		"npm_config_fund=false",
		"NO_COLOR=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("`npm %s` failed: %v\n%s", strings.Join(args, " "), err, out)
	}
	return nil
}

// npmBinary locates npm and the Node it runs on.
//
// Both are required and both are reported by name: an operator who
// installed a development release without Node needs to be told THAT is
// what is missing, not that "the build failed".
func npmBinary() (string, error) {
	if _, err := exec.LookPath("node"); err != nil {
		return "", fmt.Errorf("the runtime payload must be built before it can be staged, but node was not found on PATH; install Node.js (version 20 or newer) or update from a published release, which needs no build tools")
	}
	npm, err := exec.LookPath("npm")
	if err != nil {
		return "", fmt.Errorf("the runtime payload must be built before it can be staged, but npm was not found on PATH; install Node.js (version 20 or newer) or update from a published release, which needs no build tools")
	}
	return npm, nil
}
