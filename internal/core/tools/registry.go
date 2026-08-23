// Package tools is the single source of truth for the MPM tool surface.
//
// Every MPM tool (memory write, recall, lessons, decisions, theories,
// evidence, GC, session handoff, etc.) is registered exactly once in
// this package. Both `mpm call <tool>` (CLI) and the MCP server
// (cmd/mpm-mcp) iterate the same Registry — adding a new tool requires
// touching exactly one file.
//
// Design notes (post-2026-06-26 review):
//
//   - No reflection. Handler signature is explicit: takes the
//     DatabaseManager + ActiveContext + payload map, returns
//     (interface{}, error). Both the CLI and the MCP server call the
//     same function. MCP just wraps the result in *mcp.CallToolResult.
//
//   - Schema is json.RawMessage. Both surfaces can read it: the CLI
//     uses it for `--help`-style introspection (future), the MCP server
//     passes it via NewToolWithRawSchema. Hand-writing the JSON schema
//     is verbose but explicit; the alternative (MCP's WithString/WithNumber
//     builders) requires a separate translation step that adds
//     maintenance for no behavior win.
//
//   - Registry is a package-level slice (not a map). Linear scan is fine
//     for 33 tools; explicit ordering makes the file readable. If we
//     ever need faster lookup, a name→index map can be built at init.
package tools

import (
	"context"
	"encoding/json"
	"errors"
	"io"

	"github.com/flowbyte-com/mpm-core"
)

// HandlerFunc is the canonical tool-execution signature. Both the CLI
// dispatcher and the MCP server use it directly.
//
// dm is the shared DatabaseManager. ac carries the active mode/persona
// that produced the call (CLI: read from package globals; MCP: passed at
// server construction). payload is the JSON-unmarshalled arguments map.
//
// Return value semantics:
//   - (result, nil): success; result is JSON-marshalled for both surfaces.
//   - (nil, err): failure; CLI exits 1 with the error message; MCP returns
//     mcp.NewToolResultErrorFromErr(...).
type HandlerFunc func(dm internal.CoreDB, ac internal.ActiveContext, payload map[string]interface{}) (interface{}, error)

// Tool is one entry in the Registry. The struct is intentionally flat —
// no nested config, no description vs long_description vs hints. Every
// field is required and self-explanatory.
type Tool struct {
	Name        string
	Description string
	Schema      json.RawMessage
	Handler     HandlerFunc
}

// ── Blob store and resolver wiring ───────────────────────────────────────
//
// Phase 1: the MCP server constructs the BlobStore and wires it to the
// tools package via SetBlobStore. Handlers access it via blobStoreForHandlers.
//
// BlobStore and pointer types are defined locally (not imported) because
// the tools package lives in mpm-core and cannot import the main module's
// internal/ packages. The concrete types (blobstore.FilesystemBackend,
// pointer.Resolver) are passed in via SetBlobStore/SetResolver.

var blobStoreForHandlers blobStoreInterface

// blobStoreInterface is the subset of the blobstore.BlobStore interface
// needed by Phase 1 tool handlers. Defined locally so mpm-core's tools
// package does not need to import the main module's internal/blobstore.
type blobStoreInterface interface {
	Get(ctx context.Context, id string, opts GetOptions) (io.ReadCloser, Metadata, error)
	Search(ctx context.Context, id string, query SearchQuery) ([]Match, error)
}

// GetOptions, Metadata, SearchQuery, Match are exported so the adapter in
// cmd/mpm-mcp can construct and reference them when implementing this interface.
type GetOptions struct {
	Offset   int64
	MaxBytes int64
}

type Metadata struct {
	ContentType string
	SizeBytes   int64
}

type SearchQuery struct {
	Query           string
	Regex           bool
	CaseInsensitive bool
	MaxMatches      int
	MaxBytes        int64
}

type Match struct {
	LineNo     int
	ByteOffset int64
	Snippet    string
}

// SetBlobStore makes bs available to Phase 1 tool handlers.
func SetBlobStore(bs blobStoreInterface) { blobStoreForHandlers = bs }

// ResolveOptions, Resolution, and Pointer are the pointer resolver types needed by Phase 1.
type ResolveOptions struct {
	MaxBytes int64
}

type Resolution struct {
	// Pointer is the canonical mpm:// URI for the resolved artifact.
	Pointer string
	ContentType string
	Reader     io.ReadCloser
	Metadata   map[string]interface{}
	// Bounded is true when Reader contains a truncated representation
	// of the artifact (against max_bytes). The original remains fully
	// addressable through Pointer. Never spilled — Phase 1 OutputPolicy
	// handles transport overflow.
	Bounded bool
}

// Pointer is the pointer type used by the resolver interface.
type Pointer struct {
	Kind string
	ID   string
}

// pointerResolverInterface matches the pointer.Resolver signature needed by handleMpmResolve.
type pointerResolverInterface interface {
	Resolve(ctx context.Context, p Pointer, opts ResolveOptions) (Resolution, error)
}

// ErrUnsupportedKind is returned when a pointer kind is not supported.
var ErrUnsupportedKind = errors.New("pointer: unsupported kind")

var globalResolver pointerResolverInterface

// SetResolver wires a pointer.Resolver into the tools package.
func SetResolver(r pointerResolverInterface) { globalResolver = r }

// Sentinel errors matching blobstore.ErrBlobMissing / ErrBlobNotFound so
// handlers can use errors.Is without importing the main module's blobstore.
var (
	errBlobMissing  = errors.New("blobstore: file missing (DB row exists)")
	errBlobNotFound = errors.New("blobstore: no DB row for blob")
)

//go:generate go run ../../cmd/gen-readme
