// drill_loader.go — YAML drill-spec loader.
//
// Reads DrillSpec files from ~/.mpm/drills/*.yaml. Validation is
// deliberately strict on the loader side (empty tools_required is
// rejected) so the scorer can assume a well-formed Expect.

package internal

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// LoadDrill parses one drill YAML file. Returns an error if the file
// is missing, malformed, or fails structural validation (id/framework
// required; tools_required non-empty).
func LoadDrill(path string) (*DrillSpec, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var d DrillSpec
	if err := yaml.Unmarshal(data, &d); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if d.ID == "" {
		return nil, fmt.Errorf("%s: missing id", path)
	}
	if d.Framework == "" {
		return nil, fmt.Errorf("%s: missing framework", path)
	}
	if len(d.Expect.ToolsRequired) == 0 {
		return nil, fmt.Errorf("%s: expect.tools_required must be non-empty", path)
	}
	return &d, nil
}

// LoadAllDrills reads every *.yaml file in dir, parses each via
// LoadDrill, and returns the slice of parsed specs. A single bad file
// aborts the whole load — strict-by-design so the operator notices
// typos immediately rather than running a half-broken matrix.
func LoadAllDrills(dir string) ([]DrillSpec, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read dir %s: %w", dir, err)
	}
	var drills []DrillSpec
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".yaml" {
			continue
		}
		d, err := LoadDrill(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		drills = append(drills, *d)
	}
	return drills, nil
}
