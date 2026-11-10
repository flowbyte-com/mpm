package update

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// HostAssetManifestRelPath is where a bundle declares the release-scoped
// host assets it carries.
//
// WHY THE BUNDLE DECLARES THEM. The host assets are the per-host
// adapters, the shared behavioural protocol and the canonical managed
// block source. The installed managed block advertises two of them by
// path ("~/.mpm/agent_installation/MPM_AGENT_INTEGRATION_SNIPPETS.md"
// and ".../mpm-agent-protocol.md"), and the per-adapter installers
// import the shared scripts by path. A bundle that omits them installs
// an installation whose own instructions point at files that do not
// exist.
//
// The manifest is written by the SAME installer the full installer uses
// (scripts/install_runtime_assets.py), so the declaration is produced by
// the authority, not by a second list that can drift from it. The
// bundle therefore carries its own description of what it contains, and
// a validator can check the bundle against that description without a
// source tree.
const HostAssetManifestRelPath = "config/runtime-assets.json"

// hostAssetManifestVersion is the manifest format this build reads.
// It matches scripts/install_runtime_assets.py's MANIFEST_VERSION.
const hostAssetManifestVersion = 1

// HostAssetManifest is a bundle's declaration of the host assets it
// carries. Every named asset is present in the bundle with exactly the
// recorded digest.
type HostAssetManifest struct {
	Version int `json:"version"`
	Assets  map[string]HostAssetRecord `json:"assets"`
}

// HostAssetRecord is one declared asset and its digest.
type HostAssetRecord struct {
	SHA256 string `json:"sha256"`
}

// stageHostAssets provisions the release-scoped host assets into a
// candidate bundle.
//
// IT USES THE SAME INSTALLER THE FULL INSTALLER USES. The host-asset
// set is defined by scripts/install_runtime_assets.py — an enumerated
// list of shared modules plus a scan for per-adapter manifests,
// templates and installer scripts. Re-implementing that discovery in Go
// would create a SECOND list, and a second list is precisely the
// failure this function exists to prevent: a shared module added to the
// installer but not to the copy silently stops shipping.
//
// So the builder runs the installer. The bundle it produces is the same
// bundle install.sh produces from the same checkout, byte for byte in
// the asset set, and it carries the installer's own manifest.
//
// ORDERING. The host assets are staged BEFORE the runtime payload,
// exactly as install.sh and scripts/assemble-release-bundle.sh do it:
// the payload is a build product that lands in the same adapter
// directory, and the payload validator only requires the files the
// payload manifest names, so the two sets coexist.
func stageHostAssets(srcDir, dstDir string) error {
	script := filepath.Join(srcDir, "scripts", "install_runtime_assets.py")
	if _, err := os.Stat(script); err != nil {
		return fmt.Errorf("the checkout has no host-asset installer at %s; a bundle without the host assets would install an installation whose own instructions point at files that do not exist", script)
	}
	python, err := pythonBinary()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dstDir, 0755); err != nil {
		return fmt.Errorf("could not create the candidate bundle directory %s: %w", dstDir, err)
	}
	cmd := exec.Command(python, script, //nolint:gosec
		"--source-root", srcDir,
		"--runtime-root", dstDir,
		"--groups", "host-assets")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("the host assets could not be provisioned into the candidate bundle: %v\n%s", err, out)
	}
	manifest := filepath.Join(dstDir, filepath.FromSlash(HostAssetManifestRelPath))
	if _, err := os.Stat(manifest); err != nil {
		return fmt.Errorf("the host-asset installer did not write %s, so the bundle would not declare the integration assets it carries", manifest)
	}
	return nil
}

// pythonBinary resolves the interpreter that runs the host-asset
// installer.
//
// "python3" is tried first and "python" second: some distributions
// install the interpreter under the unversioned name only, and
// refusing to build a candidate there would be a portability
// regression for a tool that already requires bash, git, go and npm
// for a checkout build.
func pythonBinary() (string, error) {
	for _, name := range []string{"python3", "python"} {
		if path, err := exec.LookPath(name); err == nil {
			return path, nil
		}
	}
	return "", fmt.Errorf("no python interpreter was found on PATH; a checkout build provisions the host integration assets with scripts/install_runtime_assets.py, which needs one (install python3 and retry)")
}

// BundleScope says WHAT a bundle validation is for.
//
// The two scopes answer different questions, and a bundle that
// answers one of them correctly can fail the other. Collapsing them is
// how an immutable rollback target ends up refused for the sake of a
// file the runtime itself wrote into it.
//
// SCOPE CANDIDATE — "is this bundle COMPLETE?"
//
// Every file the bundle carries must be accounted for. A file nobody
// provisioned is a file nobody owns, and a file that was provisioned
// and then lost is a bundle that installs an installation whose own
// instructions point at nothing.
//
// SCOPE INSTALLED — "is this bundle INTACT?"
//
// The assets the bundle declares must still be present with the
// digests it declares. The completeness question does NOT apply: a
// release directory is not immutable in practice. The runtime writes
// into it after the bundle was validated — Python's bytecode caches,
// the adapter's build-provenance sidecar — and a release that fails
// the completeness rule for that reason is not corrupt. Refusing to
// reuse it would push the operator towards deleting an immutable
// rollback target, which is the one thing a release directory must
// never be asked to be.
type BundleScope int

const (
	// ScopeCandidate is a bundle being assembled for installation.
	ScopeCandidate BundleScope = iota
	// ScopeInstalled is a bundle that was already installed.
	ScopeInstalled
)

// Strict reports whether the scope applies the construction rules
// (completeness, mandatory schema preflight) or only the integrity
// ones.
func (s BundleScope) Strict() bool { return s == ScopeCandidate }

// hostAssetProblems checks a bundle against its own host-asset
// declaration and returns one message per problem.
//
// It is FAIL-CLOSED. A bundle that carries no declaration is refused:
// without one there is no way to know whether the integration assets
// were provisioned or omitted, and a bundle that omits them installs
// an installation whose own instructions point at files that do not
// exist.
//
// The rules are:
//
//  1. the bundle declares its host assets (config/runtime-assets.json);
//  2. the declaration is a version this build understands;
//  3. the declaration names at least one asset — an empty declaration is
//     a bundle that provisioned nothing;
//  4. every declared key is a SAFE bundle-relative path (see
//     hostAssetKeyProblem) — a key that could point outside the bundle
//     would let a corrupted declaration hide a real omission;
//  5. every declared asset is present in the bundle as a regular file
//     with exactly the declared digest;
//  6. (candidate scope only) every file the bundle carries under
//     agent_installation/ is declared — by the asset declaration, or by
//     the bundle's runtime payload declaration. An undeclared file is a
//     file nobody provisioned and nobody owns.
func hostAssetProblems(bundleDir string, scope BundleScope) []string {
	var problems []string

	manifest, err := readHostAssetManifest(bundleDir)
	if err != nil {
		return []string{err.Error()}
	}
	if manifest.Version != hostAssetManifestVersion {
		problems = append(problems, fmt.Sprintf("the bundle's host-asset declaration is version %d; this build understands version %d", manifest.Version, hostAssetManifestVersion))
		return problems
	}
	if len(manifest.Assets) == 0 {
		problems = append(problems, "the bundle's host-asset declaration names no assets, so the integration assets were never provisioned")
		return problems
	}

	declared := map[string]bool{}
	for rel, rec := range manifest.Assets {
		if why := hostAssetKeyProblem(rel); why != "" {
			problems = append(problems, why)
			continue
		}
		declared[filepath.ToSlash(rel)] = true
		full := filepath.Join(bundleDir, filepath.FromSlash(rel))
		if !pathStaysInside(bundleDir, full) {
			problems = append(problems, fmt.Sprintf("the bundle declares the host asset %q, which resolves outside the bundle root", rel))
			continue
		}
		fi, err := os.Lstat(full)
		if err != nil {
			problems = append(problems, fmt.Sprintf("the bundle declares the host asset %q but does not carry it", rel))
			continue
		}
		if !fi.Mode().IsRegular() {
			problems = append(problems, fmt.Sprintf("the bundle declares the host asset %q but it is not a regular file", rel))
			continue
		}
		digest, err := fileSHA256(full)
		if err != nil {
			problems = append(problems, fmt.Sprintf("the bundle's host asset %q could not be read: %v", rel, err))
			continue
		}
		if rec.SHA256 == "" {
			problems = append(problems, fmt.Sprintf("the bundle declares the host asset %q with no digest", rel))
			continue
		}
		if !strings.EqualFold(digest, rec.SHA256) {
			problems = append(problems, fmt.Sprintf("the bundle's host asset %q has digest %s but the declaration says %s", rel, digest, rec.SHA256))
		}
	}

	// The completeness rule is a CONSTRUCTION rule. An installed
	// release carries files the runtime wrote into it after
	// validation, and refusing it for those would be refusing it for
	// being used.
	if !scope.Strict() {
		return problems
	}

	// The payload files are declared by the bundle's own runtime
	// payload declaration, not by the asset declaration.
	payloadDeclared, err := bundlePayloadDeclaredPaths(bundleDir)
	if err != nil {
		problems = append(problems, err.Error())
	}
	for rel := range payloadDeclared {
		declared[rel] = true
	}

	undeclared, err := undeclaredHostAssetFiles(bundleDir, declared)
	if err != nil {
		problems = append(problems, err.Error())
	}
	for _, rel := range undeclared {
		problems = append(problems, fmt.Sprintf("the bundle carries %q under agent_installation/ but no declaration names it", rel))
	}
	return problems
}

// hostAssetKeyProblem describes why a declaration key is unsafe, or
// returns "" when the key is safe.
//
// IT MIRRORS THE AUTHORITY'S OWN RULE. scripts/install_runtime_assets.py
// refuses a manifest entry that "is not a safe runtime-relative path":
// an absolute path, or one carrying a '..' segment. The bundle
// validators must not be WEAKER than the authority they adopted, or a
// corrupted declaration could steer a validator outside the bundle —
// and a traversal key hides a genuine omission behind a file that is
// not there.
//
// The rules:
//
//  1. the key is not empty;
//  2. the key is relative: no leading '/', and no Windows drive
//     prefix ("C:...");
//  3. no path segment is '..';
//  4. the key uses '/' as its separator and carries no '\'. A
//     backslash can never name a real asset on any platform, and its
//     presence is a sign of a corrupted declaration;
//  5. the key is in NORMAL FORM: no './' prefix, no empty segment, no
//     trailing '/'. The authority never writes such a key, so
//     accepting one would only ever widen the surface.
//
// Rule 5 makes the rule deliberate instead of accidental: without it,
// "./agent_installation/x" and "agent_installation/x" name the same
// file but are different map keys, so the completeness scan would
// report a declared file as undeclared for no reason a reader could
// see.
func hostAssetKeyProblem(rel string) string {
	if rel == "" {
		return "the bundle's host-asset declaration names an empty path"
	}
	if strings.ContainsRune(rel, '\\') {
		return fmt.Sprintf("the bundle's host-asset declaration names %q, which contains a backslash — an asset path uses '/' as its separator", rel)
	}
	if path.IsAbs(rel) || strings.HasPrefix(rel, "/") {
		return fmt.Sprintf("the bundle's host-asset declaration names %q, which is an absolute path — an asset path is relative to the bundle root", rel)
	}
	if len(rel) >= 2 && rel[1] == ':' {
		return fmt.Sprintf("the bundle's host-asset declaration names %q, which carries a drive prefix — an asset path is relative to the bundle root", rel)
	}
	for _, seg := range strings.Split(rel, "/") {
		if seg == ".." {
			return fmt.Sprintf("the bundle's host-asset declaration names %q, which contains a '..' segment — an asset path cannot leave the bundle root", rel)
		}
	}
	if path.Clean(rel) != rel {
		return fmt.Sprintf("the bundle's host-asset declaration names %q, which is not a plain relative path — the declaration must name the asset exactly as it sits in the bundle", rel)
	}
	return ""
}

// pathStaysInside reports whether a resolved path is inside a root.
//
// It is the containment backstop for the key rules above: even a key
// that passes them is resolved against the bundle root, and the result
// must remain under it.
func pathStaysInside(root, resolved string) bool {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return false
	}
	absPath, err := filepath.Abs(resolved)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(absRoot, absPath)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// readHostAssetManifest reads a bundle's host-asset declaration.
func readHostAssetManifest(bundleDir string) (HostAssetManifest, error) {
	var manifest HostAssetManifest
	path := filepath.Join(bundleDir, filepath.FromSlash(HostAssetManifestRelPath))
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return manifest, fmt.Errorf("the bundle carries no host-asset declaration (%s); the integration assets the installed instructions point at would be missing", HostAssetManifestRelPath)
		}
		return manifest, fmt.Errorf("the bundle's host-asset declaration could not be read: %v", err)
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return manifest, fmt.Errorf("the bundle's host-asset declaration is not readable JSON: %v", err)
	}
	return manifest, nil
}

// bundlePayloadDeclaredPaths returns the bundle-relative paths the
// bundle's runtime payload declaration names, plus the declaration
// itself.
func bundlePayloadDeclaredPaths(bundleDir string) (map[string]bool, error) {
	declared := map[string]bool{}
	manifestPath := filepath.Join(bundleDir, filepath.FromSlash(bundlePayloadManifestRelPath))
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("the bundle carries no runtime payload declaration (%s)", bundlePayloadManifestRelPath)
		}
		return nil, fmt.Errorf("the bundle's runtime payload declaration could not be read: %v", err)
	}
	declared[bundlePayloadManifestRelPath] = true
	var manifest payloadManifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return nil, fmt.Errorf("the bundle's runtime payload declaration is not readable JSON: %v", err)
	}
	for _, p := range manifest.Payloads {
		for _, f := range p.Files {
			declared[path.Join("agent_installation", p.Adapter, f)] = true
		}
	}
	return declared, nil
}

// undeclaredHostAssetFiles lists the files under a bundle's
// agent_installation/ that no declaration names.
func undeclaredHostAssetFiles(bundleDir string, declared map[string]bool) ([]string, error) {
	root := filepath.Join(bundleDir, "agent_installation")
	var undeclared []string
	err := filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if fi.IsDir() {
			return nil
		}
		rel, rerr := filepath.Rel(bundleDir, p)
		if rerr != nil {
			rel = p
		}
		if !declared[filepath.ToSlash(rel)] {
			undeclared = append(undeclared, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("the bundle's agent_installation/ could not be inspected: %v", err)
	}
	sort.Strings(undeclared)
	return undeclared, nil
}

// fileSHA256 returns the hex sha256 of a file.
func fileSHA256(path string) (string, error) {
	f, err := os.Open(path) //nolint:gosec
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
