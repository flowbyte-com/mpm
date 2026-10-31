package update

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// SourceKind names where a candidate comes from.
type SourceKind string

const (
	// SourceGitHub is the default: a published GitHub release.
	SourceGitHub SourceKind = "github"
	// SourceGit is an explicit development mode: a branch or commit.
	SourceGit SourceKind = "git"
	// SourceLocal is an explicit development mode: a local checkout.
	SourceLocal SourceKind = "local"
)

// Source is a resolved candidate: what to install and where it came
// from. Resolve it with ResolveSource; build it with BuildCandidate.
type Source struct {
	Kind SourceKind
	// Ref is the operator's request: version tag, branch, commit SHA or
	// directory. Empty means "the default" (latest stable release).
	Ref string
	// Rev is the immutable revision label the candidate will install
	// as (a tag like v0.2.0 for releases, a commit SHA otherwise).
	Rev string
	// Commit is the exact commit the candidate was built from.
	Commit string
	// Version is the product version string.
	Version string
}

// Candidate is a staged, unvalidated bundle directory plus its source
// identity.
type Candidate struct {
	Source Source
	// Dir is the assembled bundle directory, ready for ValidateBundle.
	Dir string
	// WorkDir is scratch the caller must remove (the download or clone
	// area). Empty when there is nothing to clean.
	WorkDir string
}

// ReleaseFetcher resolves and downloads published releases. The
// production implementation talks to the GitHub API; tests substitute a
// local HTTP server.
type ReleaseFetcher interface {
	// LatestStable resolves the newest published, non-draft,
	// non-prerelease release for the repository, or the exact release
	// named by tag ("" means latest).
	LatestStable(tag string) (ReleaseInfo, error)
	// DownloadBundle fetches and verifies the platform bundle for a
	// release into dir, returning the extracted bundle directory.
	DownloadBundle(info ReleaseInfo, dir string) (string, error)
}

// ReleaseInfo describes one published release.
type ReleaseInfo struct {
	Tag     string
	Commit  string
	Version string
	// Assets maps asset names to download URLs.
	Assets map[string]string
	// Checksums maps file names inside the bundle archive to hex
	// digests, parsed from the release's SHA256SUMS.
	Checksums map[string]string
}

// Builder turns a source checkout into a candidate bundle directory.
// The production implementation shells out to git/make; tests
// substitute a fake that assembles fixture bundles.
type Builder interface {
	// BuildFromCheckout builds the checkout at srcDir into a bundle at
	// dstDir (creating bin/, RELEASE.json, payload, schema DDL),
	// returning the exact commit the build came from.
	BuildFromCheckout(srcDir, dstDir string) (commit string, err error)
}

// FTS5BuildFlags are the three flags that must ALL be set for a working
// build (see AGENTS.md §1 — the third, -lm, is the one that gets
// missed). The updater builds with the same discipline as `make build`.
var (
	fts5CFLAGS  = "-DSQLITE_ENABLE_FTS5=1"
	fts5Tags    = "fts5"
	fts5Ldflags = "-lm"
)

// GoBuilder builds with the Go toolchain directly. It is what
// --branch/--commit/--from use: `make build` would work, but the
// updater must not assume a Makefile target it does not own, and must
// control the output directory precisely.
type GoBuilder struct {
	// Go is the go binary ("go" resolves via PATH).
	Go string
	// BuildFlags carries -tags/-ldflags provenance. Defaults apply the
	// FTS5 discipline and stamp buildinfo from the checkout.
	BuildFlags func(srcDir string) (cgoCFLAGS, cgoLDFLAGS, tags, ldflags string)
	// Binaries are the binaries to build, in promotion order. Defaults
	// to the five runtime binaries plus the standalone updater.
	Binaries []string
	// PayloadCheck, when true (default), requires the OpenCode payload
	// to stage — same fail-closed rule as validation.
	PayloadCheck bool
}

func (b GoBuilder) binaries() []string {
	if len(b.Binaries) > 0 {
		return b.Binaries
	}
	return append(append([]string{}, runtimeBinaries...), UpdaterBinaryName)
}

// BuildFromCheckout implements Builder.
func (b GoBuilder) BuildFromCheckout(srcDir, dstDir string) (string, error) {
	commit, err := gitHeadCommit(srcDir)
	if err != nil {
		return "", err
	}
	version := readVersionFile(srcDir)
	goBin := b.Go
	if goBin == "" {
		goBin = "go"
	}
	binDir := filepath.Join(dstDir, "bin")
	if err := os.MkdirAll(binDir, 0755); err != nil {
		return "", err
	}
	cflags, ldflags, tags, ld := b.BuildFlagsOrDefault(srcDir)
	// The build cache lives in the caller's work area, never in the
	// operator's HOME. The updater inherits the operator's environment,
	// and the operator may run with HOME pointed at the data root
	// (isolated updates, tests); a GOCACHE under HOME would scatter
	// read-only module files across the installation and break
	// cleanup. The module cache itself is shared read-only, which is
	// safe and fast.
	cacheDir := filepath.Join(filepath.Dir(dstDir), "gocache")
	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		return "", err
	}
	for _, bin := range b.binaries() {
		out := filepath.Join(binDir, bin)
		cmd := exec.Command(goBin, "build", "-tags", tags, "-ldflags", ld, "-o", out, "./cmd/"+bin) //nolint:gosec
		cmd.Dir = srcDir
		cmd.Env = append(os.Environ(),
			"CGO_CFLAGS="+cflags, "CGO_LDFLAGS="+ldflags, "GOCACHE="+cacheDir)
		if raw, err := cmd.CombinedOutput(); err != nil {
			return "", fmt.Errorf("building %s failed: %v\n%s", bin, err, raw)
		}
	}
	if err := WriteRelease(dstDir, commit, commit, time.Now().Unix(), version); err != nil {
		return "", err
	}
	if err := stagePayloadFromCheckout(srcDir, dstDir); err != nil {
		if b.payloadRequired() {
			return "", err
		}
	}
	if err := stageSchemaDDL(srcDir, dstDir); err != nil {
		return "", err
	}
	return commit, nil
}

// BuildFlagsOrDefault applies the FTS5 discipline with buildinfo
// stamping from the checkout.
//
// The ldflags come from scripts/build-provenance.sh — the same single
// source of truth `make build` uses — so a binary the updater builds
// carries the same identity a Makefile-built binary would. An
// unstamped binary would fail the anti-mixture check at validation
// time (fail closed); stamping here is what makes a development build
// installable.
func (b GoBuilder) BuildFlagsOrDefault(srcDir string) (cgoCFLAGS, cgoLDFLAGS, tags, ldflags string) {
	if b.BuildFlags != nil {
		return b.BuildFlags(srcDir)
	}
	return fts5CFLAGS, fts5Ldflags, fts5Tags, provenanceLdflags(srcDir)
}

// provenanceLdflags asks the checkout's own build-provenance script for
// the linker stamp. Empty on any failure: the candidate then builds
// unstamped and validation refuses it, rather than stamping a guess.
func provenanceLdflags(srcDir string) string {
	script := filepath.Join(srcDir, "scripts", "build-provenance.sh")
	out, err := exec.Command("bash", script, "ldflags").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func (b GoBuilder) payloadRequired() bool { return true }

// gitHeadCommit resolves a checkout to its immutable HEAD SHA without
// changing anything.
func gitHeadCommit(srcDir string) (string, error) {
	out, err := exec.Command("git", "-C", srcDir, "rev-parse", "HEAD").Output() //nolint:gosec
	if err != nil {
		return "", fmt.Errorf("could not resolve the checkout at %s to a commit: %w", srcDir, err)
	}
	sha := strings.TrimSpace(string(out))
	if len(sha) != 40 || strings.Trim(sha, "0123456789abcdef") != "" {
		return "", fmt.Errorf("the checkout at %s resolved to %q, which is not a commit SHA", srcDir, sha)
	}
	return sha, nil
}

// gitIsDirty reports uncommitted changes (tracked files) and untracked
// files that are not ignored. A dirty tree is refused by default: an
// update built from it would record a commit SHA that does not describe
// what was actually built.
func gitIsDirty(srcDir string) (bool, string, error) {
	out, err := exec.Command("git", "-C", srcDir, "status", "--porcelain").Output() //nolint:gosec
	if err != nil {
		return false, "", fmt.Errorf("could not inspect the checkout at %s: %w", srcDir, err)
	}
	s := strings.TrimSpace(string(out))
	return s != "", s, nil
}

func readVersionFile(srcDir string) string {
	raw, err := os.ReadFile(filepath.Join(srcDir, "VERSION"))
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(raw))
}

// stagePayloadFromCheckout copies the OpenCode payload through the same
// rule validation uses: server.js plus dist/index.v2.js, both non-empty.
func stagePayloadFromCheckout(srcDir, dstDir string) error {
	for _, f := range []string{
		"agent_installation/mpm-opencode/server.js",
		"agent_installation/mpm-opencode/dist/index.v2.js",
	} {
		src := filepath.Join(srcDir, f)
		fi, err := os.Stat(src)
		if err != nil || fi.IsDir() || fi.Size() == 0 {
			return fmt.Errorf("the checkout has no usable OpenCode payload (%s missing or empty)", f)
		}
		dst := filepath.Join(dstDir, f)
		if err := os.MkdirAll(filepath.Join(dstDir, "agent_installation", "mpm-opencode", "dist"), 0755); err != nil {
			return err
		}
		raw, err := os.ReadFile(src)
		if err != nil {
			return err
		}
		if err := os.WriteFile(dst, raw, 0644); err != nil {
			return err
		}
	}
	return nil
}

// stageSchemaDDL embeds the substrate's lifecycle_events DDL so the
// preflight travels with the bundle (same rule install.sh applies).
func stageSchemaDDL(srcDir, dstDir string) error {
	raw, err := os.ReadFile(filepath.Join(srcDir, "internal", "core", "schema.go"))
	if err != nil {
		return nil // a checkout without the Go schema ships no DDL; preflight degrades gracefully
	}
	ddl, err := extractLifecycleDDL(string(raw))
	if err != nil {
		return nil
	}
	dir := filepath.Join(dstDir, "schema")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "lifecycle_events.sql"), []byte(ddl), 0644)
}

// GitHubFetcher is the production ReleaseFetcher: the public GitHub API
// for metadata, release asset URLs for the bundle. Ordinary users need
// no Go, Node or Git — just HTTPS to github.com.
type GitHubFetcher struct {
	// APIBase is the API root (default https://api.github.com).
	APIBase string
	// Repository is "owner/name" (default flowbyte-com/mpm).
	Repository string
	// Client bounds the whole operation. Defaults: 30s per request,
	// 512MiB download cap.
	Client *http.Client
	// PlatformAsset selects the bundle asset, e.g.
	// "mpm_v0.2.0_linux_amd64.tar.gz". Default: current platform.
	PlatformAsset func(tag string) string
	// Now supplies timestamps for provenance records.
	Now func() time.Time
}

func (f GitHubFetcher) apiBase() string {
	if f.APIBase != "" {
		return strings.TrimRight(f.APIBase, "/")
	}
	return "https://api.github.com"
}

func (f GitHubFetcher) repository() string {
	if f.Repository != "" {
		return f.Repository
	}
	return "flowbyte-com/mpm"
}

func (f GitHubFetcher) client() *http.Client {
	if f.Client != nil {
		return f.Client
	}
	return &http.Client{Timeout: 30 * time.Second}
}

// maxDownloadBytes bounds a bundle download. A release bundle is tens
// of megabytes; half a gigabyte is already absurd, and an unbounded
// read is an unnecessary way to be hurt by someone else's response.
const maxDownloadBytes = 512 << 20

func (f GitHubFetcher) getJSON(url string, v interface{}) error {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "mpm-update")
	resp, err := f.client().Do(req)
	if err != nil {
		return fmt.Errorf("could not reach the release service: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("the release service answered HTTP %d for %s", resp.StatusCode, url)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, v)
}

// LatestStable implements ReleaseFetcher.
func (f GitHubFetcher) LatestStable(tag string) (ReleaseInfo, error) {
	var info ReleaseInfo
	base := f.apiBase() + "/repos/" + f.repository()
	if tag != "" {
		var rel githubRelease
		if err := f.getJSON(base+"/releases/tags/"+tag, &rel); err != nil {
			return info, fmt.Errorf("could not resolve release %s: %w", tag, err)
		}
		if rel.Draft {
			return info, fmt.Errorf("release %s is a draft; drafts are never installed", tag)
		}
		if rel.Prerelease {
			return info, fmt.Errorf("release %s is a prerelease; pass an explicit development source instead", tag)
		}
		return releaseInfoFrom(rel), nil
	}
	var rels []githubRelease
	if err := f.getJSON(base+"/releases?per_page=20", &rels); err != nil {
		return info, err
	}
	for _, rel := range rels {
		if rel.Draft || rel.Prerelease {
			continue
		}
		return releaseInfoFrom(rel), nil
	}
	return info, fmt.Errorf("no published stable release found for %s", f.repository())
}

type githubRelease struct {
	TagName    string `json:"tag_name"`
	Target     string `json:"target_commitish"`
	Prerelease bool   `json:"prerelease"`
	Draft      bool   `json:"draft"`
	Assets     []struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
	} `json:"assets"`
}

func releaseInfoFrom(rel githubRelease) ReleaseInfo {
	info := ReleaseInfo{Tag: rel.TagName, Commit: rel.Target, Assets: map[string]string{}, Checksums: map[string]string{}}
	for _, a := range rel.Assets {
		info.Assets[a.Name] = a.URL
	}
	return info
}

// DownloadBundle implements ReleaseFetcher.
func (f GitHubFetcher) DownloadBundle(info ReleaseInfo, dir string) (string, error) {
	asset := f.PlatformAssetOrDefault(info.Tag)
	url, ok := info.Assets[asset]
	if !ok {
		return "", fmt.Errorf("release %s has no bundle for this platform (looked for %s)", info.Tag, asset)
	}
	sumsURL, ok := info.Assets["SHA256SUMS"]
	if !ok {
		return "", fmt.Errorf("release %s publishes no SHA256SUMS; refusing an unverifiable bundle", info.Tag)
	}
	work, err := os.MkdirTemp(dir, "download.")
	if err != nil {
		return "", err
	}
	archive := filepath.Join(work, asset)
	if err := f.downloadFile(url, archive); err != nil {
		os.RemoveAll(work)
		return "", err
	}
	sums, err := f.downloadBytes(sumsURL, 1<<20)
	if err != nil {
		os.RemoveAll(work)
		return "", fmt.Errorf("could not fetch SHA256SUMS: %w", err)
	}
	digest, ok := parseChecksumLine(string(sums), asset)
	if !ok {
		os.RemoveAll(work)
		return "", fmt.Errorf("SHA256SUMS does not cover %s; refusing an unverifiable bundle", asset)
	}
	if err := verifyFileSHA256(archive, digest); err != nil {
		os.RemoveAll(work)
		return "", err
	}
	extracted := filepath.Join(work, "bundle")
	if err := extractTarGz(archive, extracted); err != nil {
		os.RemoveAll(work)
		return "", fmt.Errorf("the release archive is not a safe bundle: %w", err)
	}
	return extracted, nil
}

// PlatformAssetOrDefault names the bundle asset for this platform.
func (f GitHubFetcher) PlatformAssetOrDefault(tag string) string {
	if f.PlatformAsset != nil {
		return f.PlatformAsset(tag)
	}
	return fmt.Sprintf("mpm_%s_linux_amd64.tar.gz", tag)
}

func (f GitHubFetcher) downloadFile(url, dst string) error {
	raw, err := f.downloadBytes(url, maxDownloadBytes+1)
	if err != nil {
		return err
	}
	if int64(len(raw)) > maxDownloadBytes {
		return fmt.Errorf("the download exceeds the %d-byte bound; refusing", maxDownloadBytes)
	}
	return os.WriteFile(dst, raw, 0600)
}

func (f GitHubFetcher) downloadBytes(url string, limit int64) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "mpm-update")
	resp, err := f.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("could not download %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download failed with HTTP %d for %s", resp.StatusCode, url)
	}
	return io.ReadAll(io.LimitReader(resp.Body, limit))
}

func parseChecksumLine(sums, asset string) (string, bool) {
	for _, line := range strings.Split(sums, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		name := strings.TrimPrefix(fields[1], "*")
		if name == asset && len(fields[0]) == 64 && isHex(fields[0]) {
			return strings.ToLower(fields[0]), true
		}
	}
	return "", false
}

func isHex(s string) bool {
	_, err := hex.DecodeString(s)
	return err == nil
}

func verifyFileSHA256(path, want string) error {
	fh, err := os.Open(path)
	if err != nil {
		return err
	}
	defer fh.Close()
	h := sha256.New()
	if _, err := io.Copy(h, fh); err != nil {
		return err
	}
	got := hex.EncodeToString(h.Sum(nil))
	if got != want {
		return fmt.Errorf("checksum mismatch: downloaded %s, expected %s; refusing a corrupt bundle", got, want)
	}
	return nil
}

// extractTarGz unpacks a bundle archive with fail-closed rules: no
// absolute paths, no "..", no symlinks or hardlinks, per-file and total
// size caps. A release archive that escapes its directory is an attack,
// not a bundle.
func extractTarGz(archive, dst string) error {
	fh, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer fh.Close()
	gz, err := gzip.NewReader(io.LimitReader(fh, maxDownloadBytes+1))
	if err != nil {
		return fmt.Errorf("not a gzip archive: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	const maxTotal = maxDownloadBytes
	var total int64
	if err := os.MkdirAll(dst, 0755); err != nil {
		return err
	}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if hdr.Typeflag == tar.TypeSymlink || hdr.Typeflag == tar.TypeLink {
			return fmt.Errorf("archive contains a link %q; refusing", hdr.Name)
		}
		if hdr.Typeflag != tar.TypeReg && hdr.Typeflag != tar.TypeDir {
			continue
		}
		clean := filepath.Clean("/" + hdr.Name)[1:]
		if clean == "" || clean == "." || strings.HasPrefix(clean, "..") {
			return fmt.Errorf("archive contains an unsafe path %q; refusing", hdr.Name)
		}
		target := filepath.Join(dst, clean)
		if hdr.Typeflag == tar.TypeDir {
			if err := os.MkdirAll(target, 0755); err != nil {
				return err
			}
			continue
		}
		if hdr.Size > 256<<20 {
			return fmt.Errorf("archive member %q exceeds the per-file bound; refusing", hdr.Name)
		}
		total += hdr.Size
		if total > maxTotal {
			return fmt.Errorf("archive exceeds the total size bound; refusing")
		}
		if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
			return err
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
		if err != nil {
			return err
		}
		_, err = io.CopyN(out, tr, hdr.Size)
		cerr := out.Close()
		if err != nil || cerr != nil {
			return fmt.Errorf("could not extract %q", hdr.Name)
		}
	}
}
