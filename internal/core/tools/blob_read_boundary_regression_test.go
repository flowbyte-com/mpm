package tools

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/flowbyte-com/mpm-core"
)

// fakeBlobStore is a minimal in-memory blobStoreInterface for handler tests.
type fakeBlobStore struct {
	content     string
	contentType string
	size        int64
	gotMaxBytes int64
}

func (f *fakeBlobStore) Get(ctx context.Context, id string, opts GetOptions) (io.ReadCloser, Metadata, error) {
	f.gotMaxBytes = opts.MaxBytes
	start := int64(0)
	if opts.Offset > 0 && opts.Offset < int64(len(f.content)) {
		start = opts.Offset
	}
	end := start + opts.MaxBytes
	if end > int64(len(f.content)) || opts.MaxBytes <= 0 && false {
		end = int64(len(f.content))
	}
	if opts.MaxBytes > 0 && end > int64(len(f.content)) {
		end = int64(len(f.content))
	}
	if opts.MaxBytes <= 0 {
		end = int64(len(f.content))
	}
	reader := io.NopCloser(strings.NewReader(f.content[start:end]))
	return reader, Metadata{ContentType: f.contentType, SizeBytes: f.size}, nil
}

func (f *fakeBlobStore) Search(ctx context.Context, id string, query SearchQuery) ([]Match, error) {
	return nil, nil
}

// TestBlobRead_DefaultPageCooperatesWithOutputBoundary is the D4 regression
// test (2026-08-25).
//
// Bug: handleMpmBlobRead defaulted to a 50 KB page while the MCP output
// boundary defaults to 10 KB. Any unbounded read of a blob larger than the
// boundary therefore produced a serialized result that itself spilled — the
// client received a pointer to the READ RESULT (recursive indirection)
// instead of content.
//
// Invariant: the default page must fit under DefaultOutputThresholdBytes so
// an unbounded read returns inline content + has_more pagination. Explicit
// max_bytes above the boundary remains allowed (caller's choice; the spill
// envelope is then the honest bounded answer). The guillotine itself is NOT
// weakened — Apply stays the enforcement point at the MCP adapter.
func TestBlobRead_DefaultPageCooperatesWithOutputBoundary(t *testing.T) {
	boundary := DefaultOutputThresholdBytes()

	big := strings.Repeat("A", boundary*6) // 6x boundary: old 50KB default would return it all
	fake := &fakeBlobStore{content: big, contentType: "application/json", size: int64(len(big))}
	oldBS := blobStoreForHandlers
	SetBlobStore(fake)
	defer SetBlobStore(oldBS)

	res, err := handleMpmBlobRead(nil, internal.ActiveContext{}, map[string]interface{}{"id": "b1"})
	if err != nil {
		t.Fatalf("blob read: %v", err)
	}
	m := res.(map[string]interface{})

	returned := m["bytes_returned"].(int)
	if returned > boundary-1024 {
		t.Errorf("default page returned %d bytes; must stay under boundary(%d)-reserve for envelope", returned, boundary)
	}
	if hasMore, _ := m["has_more"].(bool); !hasMore {
		t.Errorf("has_more = false after a partial default read of a %d-byte blob; want true", len(big))
	}
	if fake.gotMaxBytes != int64(boundary-1024) {
		t.Errorf("handler used max_bytes=%d, want boundary-reserve (%d)", fake.gotMaxBytes, boundary-1024)
	}

	// Explicit max_bytes still honored up to the server ceiling: caller
	// explicitly asking large accepts responsibility for the spill path.
	if _, err := handleMpmBlobRead(nil, internal.ActiveContext{}, map[string]interface{}{"id": "b1", "max_bytes": float64(200 * 1024)}); err != nil {
		t.Fatalf("explicit large read: %v", err)
	}
	if fake.gotMaxBytes != 200*1024 {
		t.Errorf("explicit max_bytes not honored: got %d", fake.gotMaxBytes)
	}
}
