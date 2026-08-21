package blobstore

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
)

// FilesystemBackend implements BlobStore by storing blobs as files on the filesystem
// and maintaining metadata in a SQLite table named "blobs".
type FilesystemBackend struct {
	db      *sql.DB
	blobDir string
	ttl     time.Duration
}

// NewFilesystemBackend creates a FilesystemBackend that stores blobs under
// blobDir and reads/writes metadata to the provided SQLite db.
//
// The db is expected to already contain a "blobs" table with the schema:
//   CREATE TABLE blobs (
//       id             TEXT PRIMARY KEY,
//       source_tool    TEXT NOT NULL,
//       source_call_id TEXT,
//       session_id     TEXT,
//       size_bytes     INTEGER NOT NULL,
//       content_type   TEXT NOT NULL DEFAULT 'application/json',
//       created_at     INTEGER NOT NULL,
//       expires_at     INTEGER NOT NULL,
//       checksum       TEXT
//   );
//
// blobDir is created if it does not exist.
func NewFilesystemBackend(db *sql.DB, blobDir string, ttl time.Duration) (*FilesystemBackend, error) {
	if err := os.MkdirAll(blobDir, 0o700); err != nil {
		return nil, err
	}
	return &FilesystemBackend{db: db, blobDir: blobDir, ttl: ttl}, nil
}

// payloadPath returns the absolute path to the blob file for the given id.
func (f *FilesystemBackend) payloadPath(id string) string {
	return filepath.Join(f.blobDir, id)
}

// TTL returns the configured time-to-live for new blobs.
func (f *FilesystemBackend) TTL() time.Duration {
	return f.ttl
}

// textContentType returns true if the given content-type should receive UTF-8
// boundary protection when reading partial ranges.
func textContentType(ct string) bool {
	ct = strings.ToLower(ct)
	return strings.HasPrefix(ct, "text/") ||
		ct == "application/json"
}

// validUTF8Boundary scans backward from the cut point and returns the
// first valid UTF-8 rune boundary before cutPoint.
// A valid boundary is either:
//   - An ASCII byte where the boundary is i+1
//   - A multi-byte start where the rune [i, i+width) ends before cutPoint
//
// The scan respects maxScan: it will not look more than maxScan bytes
// before cutPoint. If no boundary is found within range, returns 0.
// validUTF8Boundary scans backward from the cut point and returns the
// first valid UTF-8 rune boundary before cutPoint.
// A valid boundary is either:
//   - An ASCII byte where the boundary is i+1
//   - A multi-byte start where the rune [i, i+width) ends before cutPoint
//
// The scan respects maxScan: it will not look more than maxScan bytes
// before cutPoint. If no boundary is found within range, returns 0.
func validUTF8Boundary(data []byte, cutPoint int64, maxScan int) int64 {
	if cutPoint <= 0 {
		return 0
	}
	if cutPoint > int64(len(data)) {
		cutPoint = int64(len(data))
	}

	i := cutPoint - 1
	scanLimit := cutPoint - int64(maxScan)
	if scanLimit < 0 {
		scanLimit = 0
	}

	for i >= scanLimit {
		b := data[i]

		if b < 0x80 {
			// ASCII: valid boundary.
			return i + 1
		}

		width := 0
		for fb := b; fb&0x80 != 0; fb <<= 1 {
			width++
		}

		if width == 1 {
			// Continuation byte: skip and keep looking for rune start.
			i--
			continue
		}

		// Multi-byte start.
		if i+int64(width) <= cutPoint {
			// Rune ends before cut: valid boundary.
			return i + int64(width)
		} else {
			// Rune would be split by the cut. The rune's start position is the
			// last safe boundary before the split.
			return i
		}
	}

	// No boundary found in range.
	return 0
}

// readChunk reads up to maxBytes from the file at path starting at offset,
// applying UTF-8 boundary protection for text content types.
// Returns (content, nextOffset, error).
func readChunk(path string, offset, maxBytes int64, contentType string) ([]byte, int64, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, offset, err
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return nil, offset, err
	}
	size := info.Size()

	if offset >= size {
		return []byte{}, offset, nil
	}

	if maxBytes <= 0 {
		maxBytes = size - offset
	}

	// Seek to offset.
	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		return nil, offset, err
	}

	limit := offset + maxBytes
	if limit > size {
		limit = size
	}

	// Read up to limit.
	buf := make([]byte, limit-offset)
	n, err := file.Read(buf)
	if err != nil && err != io.EOF {
		return nil, offset, err
	}
	buf = buf[:n]

	// Apply UTF-8 boundary protection for text types.
	// We read up to 4 bytes past the nominal end looking for a rune boundary.
	if textContentType(contentType) && int64(len(buf)) == maxBytes && maxBytes > 0 {
		// cutPoint = nominal end of read; scan up to 4 bytes past it.
		boundary := validUTF8Boundary(buf, int64(len(buf)), 4)
		if boundary < int64(len(buf)) {
			buf = buf[:boundary]
		}
	}

	nextOffset := offset + int64(len(buf))
	return buf, nextOffset, nil
}

// Put stores content from the reader, writes it to a temp file, atomically
// renames it into the blob directory, and inserts metadata into the db.
// Returns a Pointer{Kind:"blob", ID:<uuid>}.
func (f *FilesystemBackend) Put(ctx context.Context, content io.Reader, meta Metadata) (Pointer, error) {
	const maxRetries = 3

	for attempt := 0; attempt < maxRetries; attempt++ {
		id := uuid.New().String()
		tmpPath := filepath.Join(f.blobDir, id+".tmp")
		finalPath := filepath.Join(f.blobDir, id)

		// Open tmp file for writing.
		tmpFile, err := os.OpenFile(tmpPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
		if err != nil {
			return Pointer{}, err
		}

		// Copy content to tmp file.
		n, err := io.Copy(tmpFile, content)
		if err != nil {
			tmpFile.Close()
			os.Remove(tmpPath)
			return Pointer{}, err
		}

		// Fsync to durable storage.
		if err := tmpFile.Sync(); err != nil {
			tmpFile.Close()
			// Leave stale .tmp for GC.
			return Pointer{}, err
		}

		if err := tmpFile.Close(); err != nil {
			os.Remove(tmpPath)
			return Pointer{}, err
		}

		// Atomic rename.
		if err := os.Rename(tmpPath, finalPath); err != nil {
			if os.IsExist(err) {
				// UUID collision — retry with new UUID.
				os.Remove(tmpPath)
				continue
			}
			// Other rename error — leave .tmp for GC.
			return Pointer{}, err
		}

		// Compute expiry.
		expiresAt := meta.ExpiresAt
		if expiresAt.IsZero() {
			if f.ttl > 0 {
				expiresAt = time.Now().Add(f.ttl)
			} else {
				expiresAt = time.Now().Add(24 * time.Hour) // sensible default
			}
		}

		createdAt := meta.CreatedAt
		if createdAt.IsZero() {
			createdAt = time.Now()
		}

		// Insert metadata.
		_, err = f.db.ExecContext(ctx, `
			INSERT INTO blobs (id, source_tool, source_call_id, session_id, size_bytes, content_type, created_at, expires_at, checksum)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			id, meta.SourceTool, meta.SourceCallID, meta.SessionID,
			n, meta.ContentType, createdAt.Unix(), expiresAt.Unix(), meta.Checksum)
		if err != nil {
			// Attempt to clean up the orphaned file.
			os.Remove(finalPath)
			return Pointer{}, err
		}

		return Pointer{Kind: "blob", ID: id}, nil
	}

	return Pointer{}, errors.New("blobstore: put failed after 3 UUID retries (unlikely)")
}

// Get returns a reader for the blob identified by id, starting at offset and
// reading at most maxBytes (0 = to EOF). For text/* and application/json
// content, reading stops at a valid UTF-8 rune boundary to avoid splitting
// multi-byte characters.
func (f *FilesystemBackend) Get(ctx context.Context, id string, opts GetOptions) (io.ReadCloser, Metadata, error) {
	var meta Metadata
	var sizeBytes int64
	var contentType string
	var sourceTool, sourceCallID, sessionID string
	var expiresAt int64
	var createdAt int64
	var checksum sql.NullString

	err := f.db.QueryRowContext(ctx, `
		SELECT size_bytes, content_type, source_tool,
		       COALESCE(source_call_id, ''), COALESCE(session_id, ''),
		       created_at, expires_at, checksum
		FROM blobs WHERE id = ?`, id).Scan(
		&sizeBytes, &contentType, &sourceTool, &sourceCallID,
		&sessionID, &createdAt, &expiresAt, &checksum)
	if err == sql.ErrNoRows {
		return nil, Metadata{}, ErrBlobNotFound
	}
	if err != nil {
		return nil, Metadata{}, err
	}

	meta.SourceTool = sourceTool
	meta.SourceCallID = sourceCallID
	meta.SessionID = sessionID
	if checksum.Valid {
		meta.Checksum = checksum.String
	}

	path := f.payloadPath(id)

	// Check if file exists.
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, Metadata{}, ErrBlobMissing
		}
		return nil, Metadata{}, err
	}

	actualSize := info.Size()

	// Handle truncated file: return bytes up to actual size.
	if actualSize < sizeBytes {
		sizeBytes = actualSize
	}

	// Handle EOF.
	if opts.Offset >= sizeBytes {
		return io.NopCloser(strings.NewReader("")), Metadata{
			ContentType: contentType,
			SizeBytes:   sizeBytes,
		}, nil
	}

	// Read the requested range.
	data, nextOffset, err := readChunk(path, opts.Offset, opts.MaxBytes, contentType)
	if err != nil {
		return nil, Metadata{}, err
	}

	meta.SizeBytes = sizeBytes
	meta.ContentType = contentType
	meta.CreatedAt = time.Unix(createdAt, 0)
	meta.ExpiresAt = time.Unix(expiresAt, 0)

	// Build a reader that reports byte count via Close.
	return &readerCloser{
		Reader:      strings.NewReader(string(data)),
		size:       int64(len(data)),
		nextOffset: nextOffset,
		hasMore:    nextOffset < sizeBytes,
	}, meta, nil
}

// readerCloser wraps a strings.Reader and tracks how many bytes were read.
type readerCloser struct {
	*strings.Reader
	size       int64
	nextOffset int64
	hasMore    bool
}

func (r *readerCloser) Close() error {
	return nil
}

// Delete removes the blob metadata row and the payload file.
// It is best-effort and idempotent: if either the DB row or the file is
// already gone, Delete returns nil.
func (f *FilesystemBackend) Delete(ctx context.Context, id string) error {
	path := f.payloadPath(id)

	// Delete DB row first.
	_, _ = f.db.ExecContext(ctx, `DELETE FROM blobs WHERE id = ?`, id)

	// Remove payload file.
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		// File missing is acceptable — GC or manual cleanup may have removed it.
		return err
	}
	return nil
}

// Search reads the payload file for id and returns matches against query.
// It uses RE2 regex when Regex=true, otherwise does a literal substring search.
// CaseInsensitive applies to both modes.
func (f *FilesystemBackend) Search(ctx context.Context, id string, query SearchQuery) ([]Match, error) {
	path := f.payloadPath(id)

	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrBlobNotFound
		}
		return nil, err
	}
	defer file.Close()

	var matcher *regexp.Regexp
	if query.Regex {
		flags := ""
		if query.CaseInsensitive {
			flags = "(?i)"
		}
		matcher, err = regexp.Compile(flags + query.Query)
		if err != nil {
			return nil, err
		}
	}

	// Determine max bytes to scan.
	maxBytes := query.MaxBytes
	if maxBytes <= 0 {
		fi, _ := file.Stat()
		if fi != nil {
			maxBytes = fi.Size()
		}
	}

	var matches []Match
	var offset int64
	lineNo := 1
	lineStart := int64(0)
	buf := make([]byte, 32*1024)
	bytesScanned := int64(0)

	for bytesScanned < maxBytes {
		n, err := file.ReadAt(buf, offset)
		if n > 0 {
			chunkEnd := bytesScanned + int64(n)
			for i := 0; i < n; i++ {
				if buf[i] == '\n' {
					line := buf[:i]
					if query.Regex {
						if matcher.Match(line) {
							matches = append(matches, Match{
								LineNo:     lineNo,
								ByteOffset: lineStart,
								Snippet:    string(line),
							})
						}
					} else {
						haystack := string(line)
						needle := query.Query
						if query.CaseInsensitive {
							haystack = strings.ToLower(haystack)
							needle = strings.ToLower(needle)
						}
						if strings.Contains(haystack, needle) {
							matches = append(matches, Match{
								LineNo:     lineNo,
								ByteOffset: lineStart,
								Snippet:    string(line),
							})
						}
					}
					if query.MaxMatches > 0 && len(matches) >= query.MaxMatches {
						return matches, nil
					}
					lineNo++
					lineStart = bytesScanned + int64(i) + 1
				}
			}
			// If no newline found in this chunk, lineStart stays at current value
			// (line spans across chunks).
			bytesScanned = chunkEnd
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			break
		}
		offset += int64(n)
	}

	return matches, nil
}

// GCExpired removes blobs whose expires_at is before now.
// It deletes payload files first, then the DB rows.
func (f *FilesystemBackend) GCExpired(ctx context.Context, now time.Time) (GCStats, error) {
	start := time.Now()
	stats := GCStats{}

	rows, err := f.db.QueryContext(ctx, `
		SELECT id, size_bytes FROM blobs WHERE expires_at < ?`, now.Unix())
	if err != nil {
		return stats, err
	}
	defer rows.Close()

	var expired []struct {
		id        string
		sizeBytes int64
	}
	for rows.Next() {
		var id string
		var sizeBytes int64
		if err := rows.Scan(&id, &sizeBytes); err != nil {
			rows.Close()
			return stats, err
		}
		expired = append(expired, struct {
			id        string
			sizeBytes int64
		}{id, sizeBytes})
	}
	if err := rows.Err(); err != nil {
		return stats, err
	}
	stats.Scanned = len(expired)

	for _, e := range expired {
		path := f.payloadPath(e.id)
		if err := os.Remove(path); err == nil || os.IsNotExist(err) {
			stats.FreedBytes += e.sizeBytes
		}
	}

	if len(expired) > 0 {
		_, err = f.db.ExecContext(ctx, `DELETE FROM blobs WHERE expires_at < ?`, now.Unix())
		if err != nil {
			return stats, err
		}
		stats.ExpiredDeleted = len(expired)
	}

	stats.DurationMs = time.Since(start).Milliseconds()
	stats.Success = true
	return stats, nil
}

// GCSweepOrphans walks the blob directory and removes:
//  1. Files older than grace with no corresponding DB row.
//  2. DB rows with no corresponding file (logged as orphan warnings).
func (f *FilesystemBackend) GCSweepOrphans(ctx context.Context, grace time.Duration) (GCStats, error) {
	start := time.Now()
	stats := GCStats{}

	entries, err := os.ReadDir(f.blobDir)
	if err != nil {
		return stats, err
	}

	now := time.Now()
	graceCutoff := now.Add(-grace)

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if name == "" || strings.HasSuffix(name, ".tmp") {
			continue
		}

		info, err := entry.Info()
		if err != nil {
			continue
		}

		stats.Scanned++

		// Check if file is older than grace.
		if info.ModTime().Before(graceCutoff) {
			// Verify no DB row exists.
			var exists int
			err := f.db.QueryRowContext(ctx, `SELECT 1 FROM blobs WHERE id = ?`, name).Scan(&exists)
			if err == sql.ErrNoRows {
				// Orphan — delete file.
				path := f.payloadPath(name)
				if err := os.Remove(path); err == nil {
					stats.OrphansDeleted++
				}
			} else if err == nil {
				// DB row exists — skip.
				stats.OrphansSkipped++
			}
		} else {
			stats.OrphansSkipped++
		}
	}

	// Find DB rows with no corresponding file.
	rows, err := f.db.QueryContext(ctx, `SELECT id FROM blobs`)
	if err != nil {
		return stats, err
	}
	defer rows.Close()

	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			continue
		}
		path := f.payloadPath(id)
		if _, err := os.Stat(path); os.IsNotExist(err) {
			log.Printf("blobstore: orphan DB row %q has no payload file, removing", id)
			_, _ = f.db.ExecContext(ctx, `DELETE FROM blobs WHERE id = ?`, id)
			stats.OrphansDeleted++
		}
	}

	stats.DurationMs = time.Since(start).Milliseconds()
	stats.Success = true
	return stats, nil
}

// syscall.Fsync wrapper for cross-platform compatibility.
var fsync = func(f *os.File) error {
	return syscall.Fsync(int(f.Fd()))
}
