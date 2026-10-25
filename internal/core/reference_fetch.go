// reference_fetch.go — HTTP retrieval for reference URLs.
//
// Placeholder seam: the fetch pipeline (scheme enforcement, SSRF
// defence at the dial boundary, bounded download, redirect policy,
// content-type handling and publisher-Markdown preference) is the
// subject of the URL-ingestion change stacked on this one. Until it
// lands, a URL source is refused explicitly — never treated as a local
// path, and never a silent no-op.
package internal

import (
	"context"
	"fmt"
)

// acquireReferenceURL retrieves a URL and extracts its text.
func acquireReferenceURL(ctx context.Context, identity SourceIdentity) (*acquiredReferenceContent, error) {
	return nil, fmt.Errorf("--url is reserved for a future release (FUTURE FEATURE): %s was not fetched. Download the document (curl/wget) and pass the local path to `mpm reference add <file>`.", identity.Canonical)
}
