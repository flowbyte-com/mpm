package pointer

import (
	"context"
	"io"

	"github.com/flowbyte-com/mpm-core/tools"
)

type ResolveOptions struct {
	MaxBytes int64 // 0 = no limit
}

type Resolution struct {
	ContentType string
	Reader      io.ReadCloser
	Metadata    map[string]any
}

// Resolver resolves Pointers to their content.
type Resolver interface {
	Resolve(ctx context.Context, p Pointer, opts ResolveOptions) (Resolution, error)
}

// Resolve calls r.Resolve if r is non-nil.
// When r is nil (Phase 1 — blobstore not yet wired), it returns tools.ErrUnsupportedKind.
func Resolve(ctx context.Context, r Resolver, p Pointer, opts ResolveOptions) (Resolution, error) {
	if r == nil {
		return Resolution{}, tools.ErrUnsupportedKind
	}
	return r.Resolve(ctx, p, opts)
}
