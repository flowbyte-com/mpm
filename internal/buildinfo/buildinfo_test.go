// buildinfo_test.go — contracts for the build provenance embedded in every
// MPM binary.
//
// The provenance is stamped at link time, so these tests assert two
// separable things:
//
//   - the SHAPE of the provenance (fields, defaults, JSON keys) — pure
//     unit tests over the package API;
//   - the STAMPING itself (that a linker flag actually reaches the
//     binary) — a build-artifact test, pinned in the per-binary packages.
//
// The defaults are deliberately visible placeholders. A binary that was
// built without an explicit stamp must say so rather than masquerade as
// a real release.
package buildinfo

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCurrent_UnstampedBinaryReportsVisiblePlaceholders(t *testing.T) {
	info := Current("mpm")

	if info.Version != "dev" {
		t.Errorf("unstamped Version = %q, want %q (a build without a stamp must be visible as such)",
			info.Version, "dev")
	}
	if info.Commit != "unknown" {
		t.Errorf("unstamped Commit = %q, want %q", info.Commit, "unknown")
	}
	if info.ShortCommit != "unknown" {
		t.Errorf("unstamped ShortCommit = %q, want %q", info.ShortCommit, "unknown")
	}
	if info.Dirty {
		t.Error("unstamped Dirty = true, want false")
	}
	if info.Release {
		t.Error("unstamped Release = true, want false")
	}
	if info.BuildType != "development" {
		t.Errorf("unstamped BuildType = %q, want %q", info.BuildType, "development")
	}
	if info.Binary != "mpm" {
		t.Errorf("Binary = %q, want %q", info.Binary, "mpm")
	}
}

func TestCurrent_ShortCommitDerivedFromFullCommitWhenNotStamped(t *testing.T) {
	// A build that stamps only the full SHA must still render a short
	// SHA; otherwise the human output would show a bare 40-char hash.
	restore := swapStamps(t, map[string]string{
		"Commit": "06c029ba11ce18c7f10aa60b0f0497ece2edda7c",
	})
	defer restore()

	info := Current("mpm")
	if info.ShortCommit != "06c029ba" {
		t.Errorf("ShortCommit = %q, want %q (derived from the full commit)", info.ShortCommit, "06c029ba")
	}
}

func TestCurrent_ExplicitShortCommitIsNotOverridden(t *testing.T) {
	restore := swapStamps(t, map[string]string{
		"Commit":      "06c029ba11ce18c7f10aa60b0f0497ece2edda7c",
		"ShortCommit": "abc1234",
	})
	defer restore()

	info := Current("mpm")
	if info.ShortCommit != "abc1234" {
		t.Errorf("ShortCommit = %q, want %q (an explicit stamp must win)", info.ShortCommit, "abc1234")
	}
}

func TestCurrent_DirtyStampIsParsed(t *testing.T) {
	restore := swapStamps(t, map[string]string{"Dirty": "true"})
	defer restore()

	if !Current("mpm").Dirty {
		t.Error("Dirty = false, want true (the stamped dirty flag was ignored)")
	}

	restore2 := swapStamps(t, map[string]string{"Dirty": "false"})
	defer restore2()
	if Current("mpm").Dirty {
		t.Error("Dirty = true, want false")
	}
}

func TestCurrent_ReleaseBuildTypeImpliesRelease(t *testing.T) {
	restore := swapStamps(t, map[string]string{"BuildType": "release"})
	defer restore()

	info := Current("mpm")
	if !info.Release {
		t.Error("Release = false, want true (build_type=release must set release=true)")
	}
	if info.BuildType != "release" {
		t.Errorf("BuildType = %q, want %q", info.BuildType, "release")
	}
}

func TestCurrent_DevelopmentBuildTypeIsNotRelease(t *testing.T) {
	restore := swapStamps(t, map[string]string{"BuildType": "development"})
	defer restore()

	if Current("mpm").Release {
		t.Error("Release = true, want false for a development build")
	}
}

func TestInfo_JSONHasTheDocumentedKeys(t *testing.T) {
	restore := swapStamps(t, map[string]string{
		"Version":     "0.1.0-dev",
		"Commit":      "06c029ba11ce18c7f10aa60b0f0497ece2edda7c",
		"ShortCommit": "06c029ba",
		"Dirty":       "false",
		"BuildType":   "development",
	})
	defer restore()

	raw, err := FormatJSON(Current("mpm"))
	if err != nil {
		t.Fatalf("FormatJSON: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("version JSON is not valid JSON: %v\n%s", err, raw)
	}

	for _, key := range []string{
		"version", "commit", "short_commit", "dirty", "release",
		"build_type", "repository", "binary",
	} {
		if _, ok := got[key]; !ok {
			t.Errorf("version JSON is missing the %q key:\n%s", key, raw)
		}
	}

	if got["version"] != "0.1.0-dev" {
		t.Errorf("version = %v, want 0.1.0-dev", got["version"])
	}
	if got["dirty"] != false {
		t.Errorf("dirty = %v (%T), want the boolean false", got["dirty"], got["dirty"])
	}
	if got["release"] != false {
		t.Errorf("release = %v (%T), want the boolean false", got["release"], got["release"])
	}
}

func TestFormatHuman_FirstLineIsStableAndCarriesVersionAndShortCommit(t *testing.T) {
	restore := swapStamps(t, map[string]string{
		"Version":     "0.1.0-dev",
		"Commit":      "06c029ba11ce18c7f10aa60b0f0497ece2edda7c",
		"ShortCommit": "06c029ba",
		"BuildType":   "development",
	})
	defer restore()

	out := FormatHuman(Current("mpm"))
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) == 0 {
		t.Fatal("FormatHuman produced no output")
	}

	if want := "MPM v0.1.0-dev (06c029ba)"; lines[0] != want {
		t.Errorf("first line = %q, want %q", lines[0], want)
	}

	body := out
	for _, want := range []string{
		"06c029ba11ce18c7f10aa60b0f0497ece2edda7c",
		"development",
		"dirty",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("human output is missing %q:\n%s", want, out)
		}
	}
}

func TestFormatHuman_DirtyBuildIsVisible(t *testing.T) {
	restore := swapStamps(t, map[string]string{
		"Version": "0.1.0-dev",
		"Dirty":   "true",
	})
	defer restore()

	out := FormatHuman(Current("mpm"))
	if !strings.Contains(out, "yes") {
		t.Errorf("a dirty build must be visibly marked dirty:\n%s", out)
	}
}

func TestFormatHuman_CleanBuildIsNotMarkedDirty(t *testing.T) {
	restore := swapStamps(t, map[string]string{"Dirty": "false"})
	defer restore()

	out := FormatHuman(Current("mpm"))
	if strings.Contains(out, "dirty:   yes") {
		t.Errorf("a clean build must not be marked dirty:\n%s", out)
	}
}

// swapStamps temporarily replaces the linker-stamped package variables so a
// test can exercise the stamping semantics without relinking. The returned
// closure restores the previous values.
func swapStamps(t *testing.T, values map[string]string) func() {
	t.Helper()

	previous := map[string]string{}
	switch {
	case values["Version"] != "":
		previous["Version"] = Version
		Version = values["Version"]
	case values["Version"] == "":
	}
	if v, ok := values["Commit"]; ok {
		previous["Commit"] = Commit
		Commit = v
	}
	if v, ok := values["ShortCommit"]; ok {
		previous["ShortCommit"] = ShortCommit
		ShortCommit = v
	}
	if v, ok := values["Dirty"]; ok {
		previous["Dirty"] = Dirty
		Dirty = v
	}
	if v, ok := values["BuildType"]; ok {
		previous["BuildType"] = BuildType
		BuildType = v
	}

	return func() {
		for k, v := range previous {
			switch k {
			case "Version":
				Version = v
			case "Commit":
				Commit = v
			case "ShortCommit":
				ShortCommit = v
			case "Dirty":
				Dirty = v
			case "BuildType":
				BuildType = v
			}
		}
	}
}
