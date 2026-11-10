package update

import (
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------
// The runtime must not write into a release directory
// ---------------------------------------------------------------------
//
// A release directory is versioned and immutable: it is what a rollback
// gives back, and it is validated against its own declaration. Anything
// written into it after installation is a file no declaration names.
//
// The two files in question are the runtime's own droppings: Python's
// bytecode cache, and the adapter's build-provenance sidecar.

// TestTheReleaseStillValidatesAfterTheInstructionRefresh is the B1
// end-to-end regression.
//
// The recovery procedure's instruction-refresh step runs the INSTALLED
// refresher, from inside the release directory. Python compiles the
// sibling module it imports and caches the result — inside the
// release — and the release's own validator then refuses the release
// for carrying a file no declaration names.
//
// So the release must still validate after the refresh. The refresher
// must not write bytecode into a versioned, immutable release
// directory.
func TestTheReleaseStillValidatesAfterTheInstructionRefresh(t *testing.T) {
	requireTools(t)
	dev := sharedDevSource(t)

	dst := filepath.Join(t.TempDir(), "bundle")
	b := GoBuilder{CacheDir: dev.cache}
	if _, err := b.BuildFromCheckout(dev.checkout, dst); err != nil {
		t.Fatalf("the development build failed: %v", err)
	}
	if rep := ValidateBundle(dst, dev.commit); !rep.Valid {
		t.Fatalf("the built bundle does not validate: %v", rep.ValidationError())
	}

	// Run the INSTALLED refresher, exactly as the recovery procedure
	// does: from inside the release, against a managed-block target.
	target := filepath.Join(t.TempDir(), "AGENTS.md")
	refresher := filepath.Join(dst, "agent_installation", "mpm-opencode", "scripts", "install_agents_instructions.py")
	snippet := filepath.Join(dst, "agent_installation", "mpm-opencode", "templates", "AGENTS.md.snippet")
	cmd := exec.Command("python3", refresher, //nolint:gosec
		"--scope", "user", "--target", target, "--snippet", snippet)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the installed refresher failed: %v\n%s", err, out)
	}

	// The release must STILL validate.
	rep := ValidateBundle(dst, dev.commit)
	if !rep.Valid {
		t.Fatalf("the release stopped validating after the instruction refresh: %v", rep.ValidationError())
	}
	// And nothing was written into the release's asset tree.
	droppings := runtimeDroppingsIn(dst)
	if len(droppings) > 0 {
		t.Errorf("the instruction refresh wrote into the release directory: %v", droppings)
	}
}

// runtimeDroppingsIn lists the files the runtime wrote into a bundle's
// agent_installation tree: Python's bytecode caches and the adapter's
// build-provenance sidecar.
func runtimeDroppingsIn(dir string) []string {
	var found []string
	root := filepath.Join(dir, "agent_installation")
	filepath.Walk(root, func(p string, fi os.FileInfo, err error) error { //nolint:errcheck
		if err != nil || fi.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		base := filepath.Base(p)
		if base == "mpm-build-provenance.json" || strings.HasSuffix(base, ".pyc") || strings.HasSuffix(base, ".pyo") {
			found = append(found, filepath.ToSlash(rel))
		}
		return nil
	})
	sort.Strings(found)
	return found
}
