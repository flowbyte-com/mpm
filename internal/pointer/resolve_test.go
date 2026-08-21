package pointer

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/stretchr/testify/require"
)

// mockResolver is a simple Resolver for testing.
type mockResolver struct {
	res Resolution
	err error
}

func (m *mockResolver) Resolve(ctx context.Context, p Pointer, opts ResolveOptions) (Resolution, error) {
	return m.res, m.err
}

func TestResolve_NilResolver(t *testing.T) {
	// When r is nil, Resolve returns ErrUnsupportedKind for any pointer.
	ctx := context.Background()
	p := Pointer{Kind: "blob", ID: "test-id"}
	res, err := Resolve(ctx, nil, p, ResolveOptions{})
	require.ErrorIs(t, err, ErrUnsupportedKind)
	require.Zero(t, res)
}

func TestResolve_DelegatesToResolver(t *testing.T) {
	ctx := context.Background()
	p := Pointer{Kind: "blob", ID: "test-id"}

	wantRes := Resolution{
		ContentType: "application/octet-stream",
		Reader:      io.NopCloser(nil),
		Metadata:    map[string]any{"size": 1024},
	}

	r := &mockResolver{res: wantRes, err: nil}
	res, err := Resolve(ctx, r, p, ResolveOptions{MaxBytes: 0})
	require.NoError(t, err)
	require.Equal(t, wantRes.ContentType, res.ContentType)
	require.Equal(t, wantRes.Metadata, res.Metadata)
}

func TestResolve_ResolverError(t *testing.T) {
	ctx := context.Background()
	p := Pointer{Kind: "blob", ID: "test-id"}

	wantErr := errors.New("resolver internal error")
	r := &mockResolver{res: Resolution{}, err: wantErr}
	res, err := Resolve(ctx, r, p, ResolveOptions{})
	require.ErrorIs(t, err, wantErr)
	require.Zero(t, res)
}

// fakeResolver does not implement Resolver (missing method) — used to
// prove that the interface is properly declared.
type fakeResolver struct{}

func TestResolverInterface(t *testing.T) {
	// Prove that *mockResolver satisfies Resolver.
	var _ Resolver = (*mockResolver)(nil)
	// Prove that a plain struct does NOT satisfy Resolver (compile-time check).
	// Uncommenting the next line should fail to compile:
	// var _ Resolver = (*fakeResolver)(nil)
}
