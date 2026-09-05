package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	mpminternal "github.com/flowbyte-com/mpm-core"
	"github.com/flowbyte-com/mpm-core/config"
	"github.com/flowbyte-com/mpm-core/tools"
	blobstorefs "github.com/flowbyte-com/mpm/internal/blobstore"
)

// wireToolsGlobals installs the production blob store + pointer resolver
// before a tool handler runs. Called from handleCall (cmd/mpm/call.go) so
// `mpm call mpm_blob_read` and `mpm call mpm_resolve` work the same way
// they do in the MCP server.
//
// This is the CLI counterpart to cmd/mpm-mcp/tools.go RegisterAllTools.
// The wiring is identical: a blobStoreAdapter over the FilesystemBackend,
// an artifactResolverAdapter over the blob adapter + DatabaseManager, and
// both installed via tools.SetBlobStore / tools.SetResolver.
//
// On failure (e.g., workspace not writable) we log and return without
// installing — the handler will surface the canonical
// "blob store not initialized" error to the caller. We do NOT panic
// because wiring failure must not block unrelated tool calls (e.g.,
// `mpm call mpm_memory save` has no blob dependency).
func wireToolsGlobals(dm mpminternal.CoreDB) {
	workspace := resolveWireWorkspace()

	blobDir := filepath.Join(workspace, "blobs")
	if err := os.MkdirAll(blobDir, 0o755); err != nil {
		// Alpha-4 D-004: was log.Printf — downgraded to Debug so the
		// `mpm call` stderr stream stays parseable on the wire. With
		// the machine-mode discard (see cmd/mpm/main.go init), these
		// messages vanish entirely unless MPM_VERBOSE=1 is set.
		slog.Debug("wireToolsGlobals: mkdir blob dir", "dir", blobDir, "err", err)
		return
	}
	bs, err := blobstorefs.NewFilesystemBackend(dm.SQLDB(), blobDir, 24*time.Hour)
	if err != nil {
		slog.Debug("wireToolsGlobals: build blob store", "err", err)
		return
	}

	blobAdapter := &callBlobStoreAdapter{bs: bs}
	tools.SetBlobStore(blobAdapter)
	tools.SetResolver(&callArtifactResolverAdapter{blobBS: blobAdapter, dm: dm})
}

// resolveWireWorkspace returns the workspace root the CLI should use for
// the blob directory. Prefers MPM_WORKSPACE; falls back to the resolved
// mpm dir; final fallback to a per-process temp dir so the wiring never
// fails on a hostile environment. The fallback path is not advertised —
// it only fires if MPM_WORKSPACE is unset AND the resolved mpm dir is
// empty, which the cmd/mpm openCallDM path would also reject.
func resolveWireWorkspace() string {
	if ws := os.Getenv("MPM_WORKSPACE"); ws != "" {
		return ws
	}
	if dir := config.GetMPMDir(); dir != "" {
		return dir
	}
	return filepath.Join(os.TempDir(), "mpm-cli-blobs")
}

// ─── Tool-package adapters ─────────────────────────────────────────────
//
// These mirror cmd/mpm-mcp/tools.go blobStoreAdapter and
// artifactResolverAdapter bit-for-bit (same interface satisfaction, same
// per-method delegation). Defined here because the cmd/mpm main module
// cannot import cmd/mpm-mcp's symbols — the two binaries are distinct
// compilations of the same codebase.

// callBlobStoreAdapter satisfies tools.blobStoreInterface.
type callBlobStoreAdapter struct {
	bs *blobstorefs.FilesystemBackend
}

func (a *callBlobStoreAdapter) Get(ctx context.Context, id string, opts tools.GetOptions) (io.ReadCloser, tools.Metadata, error) {
	reader, meta, err := a.bs.Get(ctx, id, blobstorefs.GetOptions{
		Offset:   opts.Offset,
		MaxBytes: opts.MaxBytes,
	})
	if err != nil {
		return nil, tools.Metadata{}, err
	}
	return reader, tools.Metadata{
		ContentType: meta.ContentType,
		SizeBytes:   meta.SizeBytes,
	}, nil
}

func (a *callBlobStoreAdapter) Search(ctx context.Context, id string, query tools.SearchQuery) ([]tools.Match, error) {
	matches, err := a.bs.Search(ctx, id, blobstorefs.SearchQuery{
		Query:           query.Query,
		Regex:           query.Regex,
		CaseInsensitive: query.CaseInsensitive,
		MaxMatches:      query.MaxMatches,
		MaxBytes:        query.MaxBytes,
	})
	if err != nil {
		return nil, err
	}
	out := make([]tools.Match, 0, len(matches))
	for _, m := range matches {
		out = append(out, tools.Match{
			LineNo:     m.LineNo,
			ByteOffset: m.ByteOffset,
			Snippet:    m.Snippet,
		})
	}
	return out, nil
}

// callArtifactResolverAdapter satisfies tools.pointerResolverInterface.
type callArtifactResolverAdapter struct {
	blobBS *callBlobStoreAdapter
	dm     mpminternal.CoreDB
}

// Resolve dispatches to the per-kind resolver below. Mirrors
// cmd/mpm-mcp/tools.go artifactResolverAdapter.Resolve.
func (a *callArtifactResolverAdapter) Resolve(ctx context.Context, p tools.Pointer, opts tools.ResolveOptions) (tools.Resolution, error) {
	switch p.Kind {
	case "blob":
		return a.resolveBlob(ctx, p, opts)
	case "memory":
		return a.resolveMemory(ctx, p, opts)
	case "lesson":
		return a.resolveLesson(ctx, p, opts)
	case "work":
		return a.resolveWork(ctx, p, opts)
	case "theory":
		return a.resolveTheory(ctx, p, opts)
	default:
		return tools.Resolution{}, tools.ErrUnsupportedKind
	}
}

func (a *callArtifactResolverAdapter) resolveBlob(ctx context.Context, p tools.Pointer, opts tools.ResolveOptions) (tools.Resolution, error) {
	reader, meta, err := a.blobBS.Get(ctx, p.ID, tools.GetOptions{})
	if err != nil {
		return tools.Resolution{}, err
	}
	defer reader.Close()
	content, err := io.ReadAll(reader)
	if err != nil {
		return tools.Resolution{}, err
	}
	return tools.Resolution{
		Pointer:     "mpm://blob/" + p.ID,
		ContentType: meta.ContentType,
		Reader:      io.NopCloser(stringsNewReader(string(content))),
		Bounded:     false,
	}, nil
}

func (a *callArtifactResolverAdapter) resolveMemory(ctx context.Context, p tools.Pointer, opts tools.ResolveOptions) (tools.Resolution, error) {
	mem, err := a.dm.GetMemory(p.ID)
	if err != nil {
		return tools.Resolution{}, err
	}
	content, _ := mem["content"].(string)
	maxBytes := int(opts.MaxBytes)
	if maxBytes <= 0 {
		maxBytes = 512
	}
	bounded := len(content) > maxBytes
	readerContent := content
	if bounded {
		readerContent = mpminternal.SummarizeBounded(content, maxBytes)
	}
	_ = a.dm.RecordRetrieval(p.ID, "memory")
	return tools.Resolution{
		Pointer:     "mpm://memory/" + p.ID,
		ContentType: "text/plain",
		Reader:      io.NopCloser(stringsNewReader(readerContent)),
		Metadata:    mem,
		Bounded:     bounded,
	}, nil
}

func (a *callArtifactResolverAdapter) resolveLesson(ctx context.Context, p tools.Pointer, opts tools.ResolveOptions) (tools.Resolution, error) {
	lesson, err := a.dm.GetLesson(p.ID)
	if err != nil {
		return tools.Resolution{}, err
	}
	_ = a.dm.RecordRetrieval(p.ID, "lesson")
	// 2026-09-05 audit remediation pass 3 defect C.12 (P2): honour
	// max_bytes by computing bounded from content length, matching
	// the MCP resolver path (cmd/mpm-mcp/tools.go) and the CLI
	// fallback path (handlers.go).
	maxBytes := int(opts.MaxBytes)
	if maxBytes <= 0 {
		maxBytes = 512
	}
	bounded := len(lesson.Content) > maxBytes
	if bounded {
		lesson.Content = lesson.Content[:maxBytes]
	}
	return tools.Resolution{
		Pointer:     "mpm://lesson/" + p.ID,
		ContentType: "text/plain",
		Reader:      io.NopCloser(stringsNewReader(lesson.Content)),
		Metadata: map[string]interface{}{
			"id":                  lesson.ID,
			"type":                string(lesson.Type),
			"tags":                lesson.Tags,
			"reinforcement_count": lesson.ReinforcementCount,
			"created":             lesson.Created,
		},
		Bounded: bounded,
	}, nil
}

func (a *callArtifactResolverAdapter) resolveWork(ctx context.Context, p tools.Pointer, opts tools.ResolveOptions) (tools.Resolution, error) {
	w, err := a.dm.GetWork(p.ID)
	if err != nil {
		return tools.Resolution{}, err
	}
	content := w.Title
	if w.Content != "" {
		content = w.Title + "\n\n" + w.Content
	}
	_ = a.dm.RecordRetrieval(p.ID, "work")
	return tools.Resolution{
		Pointer:     "mpm://work/" + p.ID,
		ContentType: "text/plain",
		Reader:      io.NopCloser(stringsNewReader(content)),
		Metadata: map[string]interface{}{
			"id":           w.ID,
			"title":        w.Title,
			"status":       w.Status,
			"verification": w.Verification,
			"created_at":   w.CreatedAt,
			"updated_at":   w.UpdatedAt,
		},
		Bounded: false,
	}, nil
}

func (a *callArtifactResolverAdapter) resolveTheory(ctx context.Context, p tools.Pointer, opts tools.ResolveOptions) (tools.Resolution, error) {
	mem, err := a.dm.GetMemory(p.ID)
	if err != nil {
		return tools.Resolution{}, err
	}
	collection, _ := mem["collection"].(string)
	if collection != "theories" {
		return tools.Resolution{}, tools.ErrUnsupportedKind
	}
	content, _ := mem["content"].(string)
	_ = a.dm.RecordRetrieval(p.ID, "theory")
	// 2026-09-05 audit remediation pass 3 defect C.12 (P2): honour
	// max_bytes — see resolveLesson for the symmetric treatment.
	maxBytes := int(opts.MaxBytes)
	if maxBytes <= 0 {
		maxBytes = 512
	}
	bounded := len(content) > maxBytes
	if bounded {
		content = content[:maxBytes]
	}
	return tools.Resolution{
		Pointer:     "mpm://theory/" + p.ID,
		ContentType: "text/plain",
		Reader:      io.NopCloser(stringsNewReader(content)),
		Metadata:    mem,
		Bounded:     bounded,
	}, nil
}
