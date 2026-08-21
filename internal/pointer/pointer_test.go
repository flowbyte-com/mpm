package pointer

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParse_ValidURIs(t *testing.T) {
	cases := []struct {
		uri      string
		wantKind string
		wantID   string
	}{
		{"mpm://blob/abc", "blob", "abc"},
		{"mpm://blob/123", "blob", "123"},
		{"mpm://blob/a1b2c3", "blob", "a1b2c3"},
		{"mpm://blob/my-id", "blob", "my-id"},
		{"mpm://blob/abc-def-123", "blob", "abc-def-123"},
		{"mpm://file/some-long-id-42", "file", "some-long-id-42"},
		{"mpm://ref/root", "ref", "root"},
	}

	for _, c := range cases {
		t.Run(c.uri, func(t *testing.T) {
			p, err := Parse(c.uri)
			require.NoError(t, err, "Parse(%q) should succeed", c.uri)
			require.Equal(t, c.wantKind, p.Kind, "Kind mismatch for %q", c.uri)
			require.Equal(t, c.wantID, p.ID, "ID mismatch for %q", c.uri)
		})
	}
}

func TestParse_URI(t *testing.T) {
	p, err := Parse("mpm://blob/test-id")
	require.NoError(t, err)
	require.Equal(t, "mpm://blob/test-id", p.URI())
}

func TestParse_ErrPointerWrongScheme(t *testing.T) {
	nonMPM := []string{
		"http://blob/abc",
		"https://blob/abc",
		"file:///blob/abc",
		"blob/abc",
		"/blob/abc",
		"mpm blob abc",
		"",
	}
	for _, uri := range nonMPM {
		t.Run(uri, func(t *testing.T) {
			_, err := Parse(uri)
			require.ErrorIs(t, err, ErrPointerWrongScheme, "Parse(%q) should return ErrPointerWrongScheme", uri)
		})
	}
}

func TestParse_ErrPointerMalformed(t *testing.T) {
	malformed := []string{
		// No kind
		"mpm://",
		// No id
		"mpm://blob/",
		"mpm://blob",
		"mpm:///",
		// Empty kind
		"mpm:///id",
		// Invalid id characters
		"mpm://blob/abc!def",
		"mpm://blob/abc@def",
		"mpm://blob/abc def",
		"mpm://blob/ABC",     // uppercase not allowed
		"mpm://blob/abc_DEF", // uppercase not allowed
		"mpm://blob/abc.def", // dots not allowed
		"mpm://blob/abc_def", // underscores not allowed
		// Double slash inside path (not a valid "mpm://kind/id")
		"mpm://blob//id",
	}
	for _, uri := range malformed {
		t.Run(uri, func(t *testing.T) {
			_, err := Parse(uri)
			require.ErrorIs(t, err, ErrPointerMalformed, "Parse(%q) should return ErrPointerMalformed", uri)
		})
	}
}

func TestParse_ErrPointerMalformed_QueryAndFragment(t *testing.T) {
	queryAndFragment := []string{
		"mpm://blob/abc?query=val",
		"mpm://blob/abc#fragment",
		"mpm://blob/abc?query=val#frag",
		"mpm://blob/abc#anchor?query",
	}
	for _, uri := range queryAndFragment {
		t.Run(uri, func(t *testing.T) {
			_, err := Parse(uri)
			require.ErrorIs(t, err, ErrPointerMalformed, "Parse(%q) with query/fragment should return ErrPointerMalformed", uri)
		})
	}
}
