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

// TestCurrent_UnstampedBinaryHasAnUnknownDirtyState proves a binary with
// no provenance at all does not claim a clean tree.
//
// An unstamped build knows nothing about the source it was compiled
// from. Reporting its dirty state as "clean" would be a claim nobody
// can support; "unknown" is the honest answer, and it is what a build
// from an exported source tree (no Git metadata) reports too.
func TestCurrent_UnstampedBinaryHasAnUnknownDirtyState(t *testing.T) {
	info := Current("mpm")

	if info.DirtyState != DirtyStateUnknown {
		t.Errorf("unstamped DirtyState = %q, want %q (a build without a stamp knows nothing about its source tree)",
			info.DirtyState, DirtyStateUnknown)
	}
	if info.Dirty {
		t.Error("unstamped Dirty = true, want false")
	}
}

// TestCurrent_DirtyStateDistinguishesCleanDirtyAndUnknown proves the
// three states survive the trip from the build stamp to the report.
//
// The build script can say "unknown" (an exported source tree with no
// Git metadata), and that is not the same fact as "clean". Collapsing
// the two would tell an operator that a build was verified against a
// revision when nobody ever checked.
func TestCurrent_DirtyStateDistinguishesCleanDirtyAndUnknown(t *testing.T) {
	cases := []struct {
		stamp      string
		wantState  string
		wantDirty  bool
		wantHuman  string
		stampLabel string
	}{
		{stamp: "false", wantState: DirtyStateClean, wantDirty: false, wantHuman: "dirty:   no"},
		{stamp: "true", wantState: DirtyStateDirty, wantDirty: true, wantHuman: "dirty:   yes"},
		{stamp: "unknown", wantState: DirtyStateUnknown, wantDirty: false, wantHuman: "dirty:   unknown"},
	}

	for _, tc := range cases {
		t.Run(tc.stamp, func(t *testing.T) {
			restore := swapStamps(t, map[string]string{"Dirty": tc.stamp})
			defer restore()

			info := Current("mpm")
			if info.DirtyState != tc.wantState {
				t.Errorf("DirtyState = %q, want %q", info.DirtyState, tc.wantState)
			}
			if info.Dirty != tc.wantDirty {
				t.Errorf("Dirty = %v, want %v", info.Dirty, tc.wantDirty)
			}
			if out := FormatHuman(info); !strings.Contains(out, tc.wantHuman) {
				t.Errorf("human output does not contain %q:\n%s", tc.wantHuman, out)
			}
		})
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
	restore := swapStamps(t, map[string]string{"BuildType": "release", "Dirty": "false"})
	defer restore()

	info := Current("mpm")
	if !info.Release {
		t.Error("Release = false, want true (build_type=release must set release=true)")
	}
	if info.BuildType != "release" {
		t.Errorf("BuildType = %q, want %q", info.BuildType, "release")
	}
}

// TestCurrent_DirtyBuildNeverClaimsRelease proves a locally modified
// build cannot present itself as an official release.
//
// The build script refuses to stamp build_type=release for a dirty
// tree — a release tag says which revision the tree is based on, and
// the working tree says whether that revision is what was actually
// compiled. This is the other half of that rule: even if a stamp says
// "release", a binary that reports its own source as modified must not
// claim release status.
func TestCurrent_DirtyBuildNeverClaimsRelease(t *testing.T) {
	restore := swapStamps(t, map[string]string{"BuildType": "release", "Dirty": "true"})
	defer restore()

	info := Current("mpm")
	if info.Release {
		t.Error("Release = true, want false (a build with uncommitted changes is not a release)")
	}
	// The raw stamp is still reported: the revision the build was based
	// on is a fact; whether the build IS that release is the judgement.
	if info.BuildType != "release" {
		t.Errorf("BuildType = %q, want %q", info.BuildType, "release")
	}
	if info.DirtyState != DirtyStateDirty {
		t.Errorf("DirtyState = %q, want %q", info.DirtyState, DirtyStateDirty)
	}
}

// TestCurrent_UnknownDirtyStateNeverClaimsRelease proves an
// unverifiable build is not a release either.
//
// "The tree was clean" and "nobody could tell" are different facts.
// Only the first one may back a release claim.
func TestCurrent_UnknownDirtyStateNeverClaimsRelease(t *testing.T) {
	restore := swapStamps(t, map[string]string{"BuildType": "release", "Dirty": "unknown"})
	defer restore()

	info := Current("mpm")
	if info.Release {
		t.Error("Release = true, want false (a build whose source could not be verified is not a release)")
	}
	if info.DirtyState != DirtyStateUnknown {
		t.Errorf("DirtyState = %q, want %q", info.DirtyState, DirtyStateUnknown)
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
		"version", "commit", "short_commit", "dirty", "dirty_state", "release",
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
	if got["dirty_state"] != DirtyStateClean {
		t.Errorf("dirty_state = %v, want %q", got["dirty_state"], DirtyStateClean)
	}
	if got["release"] != false {
		t.Errorf("release = %v (%T), want the boolean false", got["release"], got["release"])
	}
}

// TestInfo_JSONDirtyStateIsAString proves the tri-state survives into
// the machine-readable contract.
//
// The boolean `dirty` is kept for compatibility; `dirty_state` is the
// precise answer. A consumer that reads only `dirty` still works, and
// a consumer that needs to know whether the state was VERIFIED can
// read `dirty_state`.
func TestInfo_JSONDirtyStateIsAString(t *testing.T) {
	restore := swapStamps(t, map[string]string{"Dirty": "unknown"})
	defer restore()

	raw, err := FormatJSON(Current("mpm"))
	if err != nil {
		t.Fatalf("FormatJSON: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("version JSON is not valid JSON: %v\n%s", err, raw)
	}

	state, ok := got["dirty_state"].(string)
	if !ok {
		t.Fatalf("dirty_state = %v (%T), want a string", got["dirty_state"], got["dirty_state"])
	}
	if state != DirtyStateUnknown {
		t.Errorf("dirty_state = %q, want %q", state, DirtyStateUnknown)
	}
	if got["dirty"] != false {
		t.Errorf("dirty = %v (%T), want the boolean false", got["dirty"], got["dirty"])
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

// TestFormatHuman_UnknownDirtyStateIsVisible proves the human form does
// not report an unverifiable build as a clean one.
//
// "clean" and "unknown" are different facts, and an operator reading
// the human form must be able to tell them apart.
func TestFormatHuman_UnknownDirtyStateIsVisible(t *testing.T) {
	restore := swapStamps(t, map[string]string{"Dirty": "unknown"})
	defer restore()

	out := FormatHuman(Current("mpm"))
	if !strings.Contains(out, "dirty:   unknown") {
		t.Errorf("an unverifiable dirty state must be visible as unknown:\n%s", out)
	}
	if strings.Contains(out, "dirty:   no") {
		t.Errorf("an unverifiable dirty state must not be reported as clean:\n%s", out)
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
