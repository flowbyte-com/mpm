package pointer

import (
	"errors"
	"regexp"
)

var (
	ErrPointerMalformed   = errors.New("pointer: malformed URI")
	ErrPointerWrongScheme = errors.New("pointer: wrong scheme")
	// ErrUnsupportedKind is defined in github.com/flowbyte-com/mpm-core/tools
	// and used by resolve.go to maintain a single sentinel value across
	// the pointer and tools packages.
)

// idPattern matches valid blob/file IDs: lowercase alphanumeric plus hyphens.
var idPattern = regexp.MustCompile(`^[a-z0-9-]+$`)

type Pointer struct {
	Kind string
	ID   string
}

// Parse accepts only mpm://<kind>/<id> where kind is non-empty,
// id matches [a-z0-9-]+, and uri must not contain a fragment or query string.
// Returns ErrPointerMalformed for mpm://, mpm://blob/, mpm://blob/abc!def.
// Returns ErrPointerWrongScheme for non-mpm:// URIs.
func Parse(uri string) (Pointer, error) {
	// Reject URIs with query strings or fragments before scheme check.
	if indexOfQueryOrFragment(uri) >= 0 {
		return Pointer{}, ErrPointerMalformed
	}

	// Scheme must be exactly "mpm://".
	const scheme = "mpm://"
	if len(uri) < len(scheme) || uri[:len(scheme)] != scheme {
		return Pointer{}, ErrPointerWrongScheme
	}

	// Strip scheme.
	path := uri[len(scheme):]

	// Find the first '/' separating kind and id.
	slashIdx := -1
	for i := 0; i < len(path); i++ {
		if path[i] == '/' {
			slashIdx = i
			break
		}
	}
	if slashIdx <= 0 {
		// No slash, or kind is empty.
		return Pointer{}, ErrPointerMalformed
	}

	kind := path[:slashIdx]
	id := path[slashIdx+1:]

	if kind == "" || id == "" {
		return Pointer{}, ErrPointerMalformed
	}

	if !idPattern.MatchString(id) {
		return Pointer{}, ErrPointerMalformed
	}

	return Pointer{Kind: kind, ID: id}, nil
}

// indexOfQueryOrFragment returns the index of '?' or '#' in uri,
// or -1 if neither is present.
func indexOfQueryOrFragment(uri string) int {
	for i := 0; i < len(uri); i++ {
		switch uri[i] {
		case '?', '#':
			return i
		}
	}
	return -1
}

// URI reconstructs the canonical mpm:// URI for this pointer.
func (p Pointer) URI() string {
	return "mpm://" + p.Kind + "/" + p.ID
}
