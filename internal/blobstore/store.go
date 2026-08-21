package blobstore

import (
	"context"
	"errors"
	"io"
	"time"
)

// Pointer is duplicated from internal/pointer to avoid an import cycle.
// resolve.go imports blobstore, so blobstore cannot import pointer.
type Pointer struct {
	Kind string
	ID   string
}

type Metadata struct {
	SourceTool    string
	SourceCallID  string
	SessionID     string
	SizeBytes     int64
	ContentType   string
	CreatedAt     time.Time
	ExpiresAt     time.Time
	Checksum      string // sha256, nullable Phase 1
}

type GetOptions struct {
	Offset   int64
	MaxBytes int64 // 0 = read to EOF
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

type GCStats struct {
	Scanned        int
	ExpiredDeleted int
	OrphansDeleted int
	OrphansSkipped int
	FreedBytes     int64
	DurationMs     int64
	Success        bool
}

var (
	ErrBlobMissing  = errors.New("blobstore: file missing (DB row exists)")
	ErrBlobNotFound = errors.New("blobstore: no DB row for blob")
)

type BlobStore interface {
	Put(ctx context.Context, content io.Reader, meta Metadata) (Pointer, error)
	Get(ctx context.Context, id string, opts GetOptions) (io.ReadCloser, Metadata, error)
	Delete(ctx context.Context, id string) error
	Search(ctx context.Context, id string, query SearchQuery) ([]Match, error)
	GCExpired(ctx context.Context, now time.Time) (GCStats, error)
	GCSweepOrphans(ctx context.Context, grace time.Duration) (GCStats, error)
}
