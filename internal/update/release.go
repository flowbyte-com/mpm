package update

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// Release is the parsed content of a bundle's RELEASE.json.
//
// The file is the bundle's identity, readable without Git and without
// running any binary. Field names match what install.sh's
// rb_write_release writes, so either tool reads what the other wrote.
type Release struct {
	Revision     string `json:"revision"`
	Commit       string `json:"commit"`
	ShortCommit  string `json:"short_commit"`
	BuildEpoch   int64  `json:"build_epoch"`
	Version      string `json:"version"`
	BuiltAt      string `json:"built_at"`
	BundleLayout string `json:"bundle_layout"`
}

// ReadRelease reads and parses <bundleDir>/RELEASE.json.
func ReadRelease(bundleDir string) (Release, error) {
	var rel Release
	raw, err := os.ReadFile(bundleDir + "/RELEASE.json")
	if err != nil {
		return rel, fmt.Errorf("bundle has no RELEASE.json: %w", err)
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&rel); err != nil {
		return rel, fmt.Errorf("bundle's RELEASE.json is not readable JSON: %w", err)
	}
	if strings.TrimSpace(rel.Revision) == "" {
		return rel, fmt.Errorf("bundle's RELEASE.json names no revision")
	}
	return rel, nil
}

// WriteRelease writes RELEASE.json into a bundle directory. The shape
// matches rb_write_release exactly.
func WriteRelease(bundleDir, rev, commit string, epoch int64, version string) error {
	short := commit
	if len(short) > 8 {
		short = short[:8]
	}
	rel := Release{
		Revision:     rev,
		Commit:       commit,
		ShortCommit:  short,
		BuildEpoch:   epoch,
		Version:      version,
		BundleLayout: "versioned-bundle/1",
	}
	if epoch > 0 {
		rel.BuiltAt = epochToRFC3339(epoch)
	} else {
		rel.BuiltAt = "unknown"
	}
	raw, err := json.MarshalIndent(rel, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	tmp, err := os.CreateTemp(bundleDir, ".RELEASE.json.*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	return os.Rename(tmpName, bundleDir+"/RELEASE.json")
}

// CurrentReleaseTarget reads the `current` symlink: "releases/<rev>".
// The second return value is the bare revision. An error means there is
// no active pointer (fresh data root), not a failure.
func CurrentReleaseTarget(dataRoot string) (target, rev string, err error) {
	link := NewLayout(dataRoot).Current()
	dest, err := os.Readlink(link)
	if err != nil {
		return "", "", err
	}
	rev = dest
	if i := strings.LastIndex(dest, "/"); i >= 0 {
		rev = dest[i+1:]
	}
	return dest, rev, nil
}
