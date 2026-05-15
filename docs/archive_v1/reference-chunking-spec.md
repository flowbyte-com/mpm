# Reference Chunking Control — Technical Specification

**Status:** Reviewed and finalised
**Date:** 2026-05-15
**Wishlist Item:** High-Priority #2 — Reference chunking control
**Sources:** Original draft + Claude's review

---

## 1. Overview

Add a `--chunk-size` flag (integer tokens) to `mpm reference add` and `mpm reference scan` commands, replacing the hardcoded `1000`-character fallback chunking in `ChunkReference()`. Enables precision (64 tokens) vs. context (2048 tokens) optimisation.

**Design principle:** No new tables. No Go schema migration. No FTS re-index triggers.

---

## 2. Problem Statement

`ChunkReference()` in `internal/reference_new.go:434` uses byte-length as the chunk boundary heuristic (`currentChunk.Len()+len(word)+1 > chunkSize`). This is a rough proxy — accurate enough for fixed small chunks but wrong at the boundaries when `--chunk-size` varies widely.

`handleRefAdd` at `simple_cmds.go:537` hardcodes `mpminternal.ChunkReference(content, 1000)` — no user-facing flag.

**Goal:** Accept `--chunk-size=N` where N is a token count; chunk to that target accurately using the cl100k_base encoding (GPT-4/GPT-3.5-turbo tokenisation).

---

## 3. Requirements

### 3.1 CLI Interface

**Commands:**
```
mpm reference add <file> [--chunk-size <tokens>] [--tag tag1,tag2] [--json]
mpm reference scan [--chunk-size <tokens>]
```

| Flag | Default | Safe Range | Description |
|------|---------|------------|-------------|
| `--chunk-size` | `512` | `64–2048` | Target chunk size in tokens |

**Validation:**
- `< 64` → reject with "must be at least 64 tokens"
- `> 2048` → reject with "must not exceed 2048 tokens"
- Non-integer → standard flag error
- Flag can appear anywhere among args (pre-scan pattern, consistent with `--json`)

### 3.2 Tokenization

**Library:** `github.com/pkoukk/tiktoken-go` — pure Go, cl100k_base encoding, no CGO dependency.

**Why tiktoken over heuristic:**
- cl100k_base is the exact encoding used by GPT-4, GPT-3.5-turbo, and embedding models (`text-embedding-3-small`, `text-embedding-ada-002`)
- Pure Go, no CGO, installs with `go get`
- Encodes ~1MB/sec — negligible overhead vs. file I/O
- Character-ratio heuristic is ±15% error; tiktoken is ±1-2%

**Token counting function:**
```go
// CountTokens returns the number of cl100k_base tokens in a string.
// Used for accurate chunk sizing based on LLM context windows.
func CountTokens(text string) (int, error) {
    encoder, err := cl100k_base.New()
    if err != nil {
        return 0, fmt.Errorf("failed to load tiktoken encoder: %w", err)
    }
    defer encoder.Close()
    tokens := encoder.Encode(text, nil, nil)
    return len(tokens), nil
}
```

**Note on encoder lifecycle:** Create a new encoder per call. The encoder is lightweight and thread-safe within a single call. For batch processing (scan), a cached encoder may be used but is not required for correctness.

---

## 4. Design Decisions

| Decision | Choice | Rationale |
|----------|--------|-----------|
| Default chunk size | `512` | Middle ground: ~4 chunks per 2k context window |
| Safe range | `64–2048` | Below 64: too fragmentary. Above 2048: exceeds most single-context windows |
| Tokenization | `pkoukk/tiktoken-go` (cl100k_base) | Correct encoding for GPT models; pure Go, no CGO |
| Section-based chunks | Unchanged | `ChunkReference` tries markdown-header sections first; token limit applies to word-boundary fallback |
| Existing callers | `ChunkReference` left as-is | Deprecate via comment; new code uses `ChunkByTokens` |
| Backward compat | Existing docs not re-chunked; no schema change | `reference_chunks.content` is just text |
| Encoder caching | Not required | Per-call creation is fast enough; complexity not justified |

---

## 5. Implementation Plan

### Task 1 — Install tiktoken-go

```bash
cd /home/v/workspace/projects/mpm
go get github.com/pkoukk/tiktoken-go@latest
```

### Task 2 — Add `CountTokens()` to `internal/reference_new.go`

**Location:** `internal/reference_new.go` — add near the `ChunkReference` function.

```go
import (
    "fmt"
    "strings"
    "github.com/pkoukk/tiktoken-go/cl100k_base"
)

// CountTokens returns the number of cl100k_base tokens in a string.
func CountTokens(text string) (int, error) {
    encoder, err := cl100k_base.New()
    if err != nil {
        return 0, fmt.Errorf("failed to load tiktoken encoder: %w", err)
    }
    defer encoder.Close()
    tokens := encoder.Encode(text, nil, nil)
    return len(tokens), nil
}
```

### Task 3 — Add `ChunkByTokens()` to `internal/reference_new.go`

**Location:** `internal/reference_new.go` — add after `CountTokens`. Uses the same `Chunk` struct (line 364).

```go
// ChunkByTokens chunks reference content by token count using cl100k_base encoding.
// chunkSize is the target token count per chunk (64–2048 recommended).
// Returns chunks with Content field containing the text, Index for ordering.
func ChunkByTokens(content string, chunkSize int) ([]Chunk, error) {
    if chunkSize <= 0 {
        chunkSize = 512
    }
    if chunkSize < 64 {
        chunkSize = 64
    }
    if chunkSize > 8192 {
        chunkSize = 8192 // hard upper limit to prevent pathological behaviour
    }

    encoder, err := cl100k_base.New()
    if err != nil {
        return nil, fmt.Errorf("failed to load tiktoken encoder: %w", err)
    }
    defer encoder.Close()

    // Fast path: single chunk if total tokens <= chunkSize
    totalTokens := encoder.Encode(content, nil, nil)
    if len(totalTokens) <= chunkSize {
        return []Chunk{{Index: 0, Content: strings.TrimSpace(content)}}, nil
    }

    // Multi-chunk: accumulate tokens, break on word boundary
    var chunks []Chunk
    var currentTokens []int
    var currentText strings.Builder

    words := strings.Fields(content)
    for _, word := range words {
        wordTokens := encoder.Encode(word, nil, nil)
        projectedLen := len(currentTokens) + len(wordTokens)

        if projectedLen > chunkSize && len(currentTokens) > 0 {
            // Flush current chunk
            chunks = append(chunks, Chunk{
                Index:   len(chunks),
                Content: strings.TrimSpace(currentText.String()),
            })
            currentTokens = nil
            currentText.Reset()
        }

        if currentText.Len() > 0 {
            currentText.WriteString(" ")
        }
        currentText.WriteString(word)
        currentTokens = append(currentTokens, wordTokens...)
    }

    if currentText.Len() > 0 {
        chunks = append(chunks, Chunk{
            Index:   len(chunks),
            Content: strings.TrimSpace(currentText.String()),
        })
    }

    return chunks, nil
}
```

**Deprecate `ChunkReference`:** Add a comment above the function:
```go
// Deprecated: use ChunkByTokens for accurate token-based chunking.
// ChunkReference uses character count, which is inaccurate for LLM context windows.
func ChunkReference(content string, chunkSize int) []Chunk {
```

### Task 4 — Update `handleRefAdd` in `cmd/mpm/simple_cmds.go`

**File:** `cmd/mpm/simple_cmds.go` around lines 467–537

**Changes:**
1. Add `--chunk-size` flag to the flag set
2. Pre-scan args for `--chunk-size` value (consistent with `--json` pattern)
3. Validate range (64–2048)
4. Replace `mpminternal.ChunkReference(content, 1000)` with `mpminternal.ChunkByTokens(content, chunkSize)`
5. Handle the returned error

```go
// Pre-scan for --chunk-size (may appear anywhere in args)
chunkSize := 512
for i, arg := range args[2:] {
    if arg == "--chunk-size" && i+1 < len(args) {
        fmt.Sscanf(args[i+1], "%d", &chunkSize)
        break
    }
}

if chunkSize < 64 || chunkSize > 2048 {
    fmt.Fprintf(os.Stderr, "Error: --chunk-size must be between 64 and 2048 (got %d)\n", chunkSize)
    return 1
}

// ... existing content loading ...

chunks, err := mpminternal.ChunkByTokens(content, chunkSize)
if err != nil {
    fmt.Fprintf(os.Stderr, "Error: failed to chunk content: %v\n", err)
    return 1
}
```

### Task 5 — Update `handleReferenceScan` in `cmd/mpm/handlers.go`

**File:** `cmd/mpm/handlers.go` around line 1474

**Changes:**
1. Pre-scan `args` for `--chunk-size` value
2. Default to 512 if absent
3. Pass chunk size through to the per-file ingestion calls

### Task 6 — Write unit tests

**File:** `internal/reference_test.go`

```go
func TestChunkByTokens_SingleChunk(t *testing.T) {
    content := "This is a short sentence."
    chunks, err := ChunkByTokens(content, 512)
    require.NoError(t, err)
    require.Len(t, chunks, 1)
    assert.Equal(t, "This is a short sentence.", chunks[0].Content)
}

func TestChunkByTokens_MultipleChunks(t *testing.T) {
    content := strings.Repeat("word ", 200) // ~200 tokens
    chunks, err := ChunkByTokens(content, 64)
    require.NoError(t, err)
    assert.GreaterOrEqual(t, len(chunks), 3)
}

func TestChunkByTokens_TokenAccuracy(t *testing.T) {
    content := strings.Repeat("word ", 300)
    for _, tc := range []int{64, 128, 256} {
        chunks, err := ChunkByTokens(content, tc)
        require.NoError(t, err)
        for i, chunk := range chunks {
            count, _ := CountTokens(chunk.Content)
            assert.LessOrEqual(t, count, tc+10, "chunk %d exceeds target by >10 tokens", i)
        }
    }
}

func TestCountTokens_English(t *testing.T) {
    count, err := CountTokens("hello world")
    require.NoError(t, err)
    assert.Equal(t, 2, count)
}
```

### Task 7 — Build and smoke test

```bash
cd /home/v/workspace/projects/mpm
/usr/local/go/bin/go build -tags fts5 -o bin/mpm ./cmd/mpm

# Small chunk test
echo "This is a test document with some content that we need to chunk properly." > /tmp/test.txt
MPM_WORKSPACE=/home/v/workspace/projects/mpm ./bin/mpm reference add /tmp/test.txt --chunk-size 5 --json

# Default chunk test
MPM_WORKSPACE=/home/v/workspace/projects/mpm ./bin/mpm reference add /tmp/test.txt --json

# Validation test (too small)
MPM_WORKSPACE=/home/v/workspace/projects/mpm ./bin/mpm reference add /tmp/test.txt --chunk-size 32  # should reject

# Validation test (too large)
MPM_WORKSPACE=/home/v/workspace/projects/mpm ./bin/mpm reference add /tmp/test.txt --chunk-size 4096  # should reject
```

---

## 6. File Summary

| File | Change |
|------|--------|
| `go.mod` | Add `github.com/pkoukk/tiktoken-go` |
| `internal/reference_new.go` | Add `CountTokens()`, add `ChunkByTokens()`, deprecate `ChunkReference` with comment |
| `cmd/mpm/simple_cmds.go` | Add `--chunk-size` pre-scan, validation, replace `ChunkReference` with `ChunkByTokens` |
| `cmd/mpm/handlers.go` | Add `--chunk-size` pre-scan to `handleReferenceScan`, propagate to per-file processing |
| `internal/reference_test.go` | Add `TestChunkByTokens_*` and `TestCountTokens` unit tests |

**No schema changes. No FTS re-index triggers.**

---

## 7. Validation Criteria

- [ ] `go get github.com/pkoukk/tiktoken-go@latest` installs without error
- [ ] `go build -tags fts5` compiles clean with no new warnings
- [ ] `mpm reference add <file> --chunk-size 128` ingests with ~128 token chunks
- [ ] `mpm reference add <file>` (no flag) uses default 512 tokens
- [ ] `mpm reference add <file> --chunk-size 32` rejects with error
- [ ] `mpm reference add <file> --chunk-size 4096` rejects with error
- [ ] `mpm reference scan --chunk-size 256` propagates to all ingested files
- [ ] `ChunkReference` still works (deprecated but not broken)
- [ ] Unit tests pass: `go test ./internal/...`