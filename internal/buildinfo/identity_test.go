// identity_test.go — contracts for the BUILD IDENTITY.
//
// The identity is the one-line answer to "which build am I running?":
// the product version plus the build metadata that says which commit the
// binary came from, whether the source tree was clean, and when it was
// built.
//
// The properties that matter are not cosmetic:
//
//   - it is a valid Semantic Versioning version, so a version can be
//     compared and sorted by ordinary SemVer tooling;
//   - it never contains an INVENTED fact. A commit the build could not
//     establish is "gunknown", not a hash that looks real;
//   - it never presents an UNVERIFIED source tree as a clean one;
//   - a release keeps the conventional bare version.
//
// The tests here exercise the identity as a pure function over stamped
// values. The stamping itself — that the identity survives into a built
// binary and out of a Git checkout — is covered in cmd/mpm.
package buildinfo

import (
	"regexp"
	"strings"
	"testing"
)

// semverPattern is the official Semantic Versioning 2.0.0 pattern.
//
// It is spelled out here rather than pulled from a dependency: the
// identity is a version string, and the property under test is that it
// IS a version string. A hand-written approximation would test the
// approximation.
var semverPattern = regexp.MustCompile(`^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(?:-((?:0|[1-9]\d*|\d*[a-zA-Z-][0-9a-zA-Z-]*)(?:\.(?:0|[1-9]\d*|\d*[a-zA-Z-][0-9a-zA-Z-]*))*))?(?:\+([0-9a-zA-Z-]+(?:\.[0-9a-zA-Z-]+)*))?$`)

// identityComponents splits an identity's build metadata into its
// dot-separated components.
//
// The split is on "." so that "dirty-unknown" stays ONE component: it is
// a single marker, not the marker "dirty" followed by something else.
func identityComponents(t *testing.T, identity string) []string {
	t.Helper()

	_, metadata, ok := strings.Cut(identity, "+")
	if !ok {
		t.Fatalf("identity %q carries no build metadata", identity)
	}
	return strings.Split(metadata, ".")
}

// TestIdentity_DevelopmentBuildCarriesTheCommitAndTheBuildTime pins the
// agreed development identity format.
func TestIdentity_DevelopmentBuildCarriesTheCommitAndTheBuildTime(t *testing.T) {
	restore := swapStamps(t, map[string]string{
		"Version":     "0.1.0-dev",
		"Commit":      "3f8a21c0d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8a9",
		"ShortCommit": "3f8a21c0",
		"Dirty":       "false",
		"BuildType":   "development",
		"BuildEpoch":  "1792679720",
	})
	defer restore()

	info := Current("mpm")

	want := "0.1.0-dev+g3f8a21c0.t1792679720"
	if info.BuildIdentity != want {
		t.Errorf("BuildIdentity = %q, want %q", info.BuildIdentity, want)
	}
}

// TestIdentity_ADirtyBuildIsMarkedDirty proves the dirty marker appears
// only for a build that is KNOWN to differ from its commit.
func TestIdentity_ADirtyBuildIsMarkedDirty(t *testing.T) {
	restore := swapStamps(t, map[string]string{
		"Version":     "0.1.0-dev",
		"ShortCommit": "3f8a21c0",
		"Dirty":       "true",
		"BuildType":   "development",
		"BuildEpoch":  "1792679720",
	})
	defer restore()

	info := Current("mpm")

	components := identityComponents(t, info.BuildIdentity)
	if !contains(components, "dirty") {
		t.Errorf("a dirty build must carry the %q component: %v", "dirty", components)
	}
}

// TestIdentity_AnUnverifiedTreeIsNotPresentedAsClean is the property that
// makes the dirty marker trustworthy.
//
// A build whose source tree could not be compared against a revision is
// NOT a clean build. It must not carry the clean identity, and it must
// not claim to be dirty either — the difference between "the tree
// differed" and "nobody could tell" is the whole reason the dirty state
// is three-valued.
func TestIdentity_AnUnverifiedTreeIsNotPresentedAsClean(t *testing.T) {
	restore := swapStamps(t, map[string]string{
		"Version":     "0.1.0-dev",
		"ShortCommit": "3f8a21c0",
		"Dirty":       "unknown",
		"BuildType":   "development",
		"BuildEpoch":  "1792679720",
	})
	defer restore()

	info := Current("mpm")

	components := identityComponents(t, info.BuildIdentity)
	if contains(components, "dirty") {
		t.Errorf("an unverifiable tree must not claim to be dirty: %v", components)
	}
	if !contains(components, "dirty-unknown") {
		t.Errorf("an unverifiable tree must say so: %v", components)
	}
}

// TestIdentity_TheCleanIdentityCarriesNoDirtyMarker proves the clean
// build is distinguishable from the other two states.
func TestIdentity_TheCleanIdentityCarriesNoDirtyMarker(t *testing.T) {
	restore := swapStamps(t, map[string]string{
		"Version":     "0.1.0-dev",
		"ShortCommit": "3f8a21c0",
		"Dirty":       "false",
		"BuildType":   "development",
		"BuildEpoch":  "1792679720",
	})
	defer restore()

	info := Current("mpm")

	components := identityComponents(t, info.BuildIdentity)
	for _, marker := range []string{"dirty", "dirty-unknown"} {
		if contains(components, marker) {
			t.Errorf("a clean build must not carry the %q component: %v", marker, components)
		}
	}
}

// TestIdentity_AnUnknownCommitIsNotManufactured proves the identity never
// invents a revision.
//
// A build from an exported source tree has no Git metadata. Its identity
// says so; it does not carry a plausible-looking hash, because an
// operator could not tell a fabricated hash from a real one.
func TestIdentity_AnUnknownCommitIsNotManufactured(t *testing.T) {
	restore := swapStamps(t, map[string]string{
		"Version":     "0.1.0-dev",
		"Commit":      "unknown",
		"ShortCommit": "unknown",
		"Dirty":       "unknown",
		"BuildType":   "development",
		"BuildEpoch":  "unknown",
	})
	defer restore()

	info := Current("mpm")

	components := identityComponents(t, info.BuildIdentity)
	if !contains(components, "gunknown") {
		t.Errorf("a build with no Git metadata must say %q: %v", "gunknown", components)
	}
	if !contains(components, "tunknown") {
		t.Errorf("a build with no build time must say %q: %v", "tunknown", components)
	}

	// Nothing else is present: every fact the build could not establish
	// is named, and no fact that WAS established is missing. A
	// fabricated hash or timestamp would show up here as an unexpected
	// component.
	want := []string{"gunknown", "dirty-unknown", "tunknown"}
	if len(components) != len(want) {
		t.Fatalf("identity components = %v, want exactly %v", components, want)
	}
	for _, component := range want {
		if !contains(components, component) {
			t.Errorf("identity components = %v, want %q among them", components, component)
		}
	}
}

// TestIdentity_TheAbbreviatedCommitIsAtLeastEightHexCharacters proves
// the commit component is unambiguous.
//
// Seven hexadecimal characters are not enough to identify a revision on
// their own, and a value that is not hexadecimal at all is not a commit.
func TestIdentity_TheAbbreviatedCommitIsAtLeastEightHexCharacters(t *testing.T) {
	cases := []struct {
		name       string
		short      string
		full       string
		wantCommit string
	}{
		{
			name:       "eight characters are accepted",
			short:      "3f8a21c0",
			full:       "3f8a21c0d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8a9",
			wantCommit: "g3f8a21c0",
		},
		{
			name:       "a shorter abbreviation is taken from the full SHA",
			short:      "3f8a21c",
			full:       "3f8a21c0d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8a9",
			wantCommit: "g3f8a21c0",
		},
		{
			name:       "a non-hexadecimal value is not a commit",
			short:      "notasha!",
			full:       "notasha!",
			wantCommit: "gunknown",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			restore := swapStamps(t, map[string]string{
				"Version":     "0.1.0-dev",
				"Commit":      tc.full,
				"ShortCommit": tc.short,
				"Dirty":       "false",
				"BuildType":   "development",
				"BuildEpoch":  "1792679720",
			})
			defer restore()

			components := identityComponents(t, Current("mpm").BuildIdentity)
			if !contains(components, tc.wantCommit) {
				t.Errorf("identity components = %v, want %q", components, tc.wantCommit)
			}
		})
	}
}

// TestIdentity_AReleaseCarriesTheVersionAlone proves the conventional
// stable-release output is preserved.
//
// A release is identified by its version; the commit and the build time
// are still reported in the detail lines and in the JSON form.
func TestIdentity_AReleaseCarriesTheVersionAlone(t *testing.T) {
	restore := swapStamps(t, map[string]string{
		"Version":     "0.1.0",
		"Commit":      "3f8a21c0d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8a9",
		"ShortCommit": "3f8a21c0",
		"Dirty":       "false",
		"BuildType":   "release",
		"BuildEpoch":  "1792679720",
	})
	defer restore()

	info := Current("mpm")

	if !info.Release {
		t.Fatal("a clean release build must report release=true")
	}
	if info.BuildIdentity != "0.1.0" {
		t.Errorf("BuildIdentity = %q, want %q", info.BuildIdentity, "0.1.0")
	}
}

// TestIdentity_IsAValidSemanticVersion proves every identity this
// package can produce is a SemVer version.
//
// The identity is meant to be usable with ordinary version tooling, so
// the build metadata has to obey SemVer's identifier rules.
func TestIdentity_IsAValidSemanticVersion(t *testing.T) {
	cases := []struct {
		name  string
		stamp map[string]string
	}{
		{
			name: "clean development build",
			stamp: map[string]string{
				"Version": "0.1.0-dev", "ShortCommit": "3f8a21c0",
				"Dirty": "false", "BuildType": "development", "BuildEpoch": "1792679720",
			},
		},
		{
			name: "dirty development build",
			stamp: map[string]string{
				"Version": "0.1.0-dev", "ShortCommit": "3f8a21c0",
				"Dirty": "true", "BuildType": "development", "BuildEpoch": "1792679720",
			},
		},
		{
			name: "unverifiable development build",
			stamp: map[string]string{
				"Version": "0.1.0-dev", "ShortCommit": "unknown",
				"Dirty": "unknown", "BuildType": "development", "BuildEpoch": "unknown",
			},
		},
		{
			name: "release build",
			stamp: map[string]string{
				"Version": "0.1.0", "ShortCommit": "3f8a21c0",
				"Dirty": "false", "BuildType": "release", "BuildEpoch": "1792679720",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			restore := swapStamps(t, tc.stamp)
			defer restore()

			identity := Current("mpm").BuildIdentity
			if !semverPattern.MatchString(identity) {
				t.Errorf("identity %q is not a valid Semantic Version", identity)
			}
		})
	}
}

// TestCurrent_BuildEpochIsReadFromTheStamp proves the captured build
// time reaches the machine-readable contract.
func TestCurrent_BuildEpochIsReadFromTheStamp(t *testing.T) {
	restore := swapStamps(t, map[string]string{"BuildEpoch": "1792679720"})
	defer restore()

	if got := Current("mpm").BuildEpoch; got != 1792679720 {
		t.Errorf("BuildEpoch = %d, want %d", got, 1792679720)
	}
}

// TestCurrent_AnUnparsableBuildEpochIsUnknown proves a mangled stamp is
// never turned into a plausible timestamp.
func TestCurrent_AnUnparsableBuildEpochIsUnknown(t *testing.T) {
	for _, stamp := range []string{"unknown", "", "not-a-number", "-5", "12.5"} {
		t.Run(stamp, func(t *testing.T) {
			restore := swapStamps(t, map[string]string{"BuildEpoch": stamp})
			defer restore()

			info := Current("mpm")
			if info.BuildEpoch != 0 {
				t.Errorf("BuildEpoch = %d, want 0 for the stamp %q", info.BuildEpoch, stamp)
			}
			if !contains(identityComponents(t, info.BuildIdentity), "tunknown") {
				t.Errorf("identity %q must report an unknown build time", info.BuildIdentity)
			}
		})
	}
}

// contains reports whether the slice holds the value.
func contains(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}
