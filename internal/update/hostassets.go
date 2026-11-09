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

// hostAssetProblems checks a bundle against its own host-asset
// declaration and returns one message per problem.
//
// It is FAIL-CLOSED. A bundle that carries no declaration is refused:
// without one there is no way to know whether the integration assets
// were provisioned or omitted, and a bundle that omits them installs
// an installation whose managed block points at missing files.
//
// The rules are:
//
//  1. the bundle declares its host assets (config/runtime-assets.json);
//  2. the declaration is a version this build understands;
//  3. the declaration names at least one asset — an empty declaration is
//     a bundle that provisioned nothing;
//  4. every declared asset is present in the bundle as a regular file
//     with exactly the declared digest;
//  5. every file the bundle carries under agent_installation/ is
//     declared — by the asset declaration, or by the bundle's runtime
//     payload declaration. An undeclared file is a file nobody
//     provisioned and nobody owns.
func hostAssetProblems(bundleDir string) []string {
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
		declared[filepath.ToSlash(rel)] = true
		full := filepath.Join(bundleDir, filepath.FromSlash(rel))
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
