package main

import (
	"io"
	"strings"
)

// stringsNewReader is a thin wrapper so wire_tools_globals.go doesn't
// import strings directly (it would otherwise need to import strings
// alongside the package's other imports). Keeping the helper in its own
// file makes the test surface for wire_tools_globals.go minimal.
func stringsNewReader(s string) io.Reader {
	return strings.NewReader(s)
}
