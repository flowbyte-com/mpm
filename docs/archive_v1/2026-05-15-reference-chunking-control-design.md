# MPM Reference Chunking Control — Phase 1 Technical Design

**Date:** 2026-05-15
**Author:** 808 (∓808)
**Status:** Draft for review

---

## Overview

Adding `--chunk-size` (token count) to `mpm reference add` for configurable reference chunking. The hardcoded `1000` (character-based) is replaced with a token-based approach using `tiktoken` (cl100k_base encoding), which matches GPT-4 context window calculations.

**Design principle:** No new tables. No Go schema migration. The CLI flag is the protocol; the tokenization library is an implementation detail.

---

## 1. The Problem with Character-Based Chunking

Currently `ChunkReference(content, 1000)` in `internal/reference_new.go:537` uses **character count**, not token count. This is inaccurate because:

- English text: ~4 chars per token on average (cl100k_base)
- Code: ~3-4 chars per token
- CJK characters: ~1-2 chars per token

A 1000-character chunk could be 250 tokens or 500 tokens depending on content type, making it impossible to predict LLM context window usage.

---

## 2. Tokenization Approach

### Library: `github.com/pkoukk/tiktoken-go`

`cl100k_base` is the encoding used by GPT-4, GPT-3.5-turbo, and embedding models. This is the correct encoding for MPM's use case (context window sizing + embedding).

**Why not `tiktoken`:**
- Popular Go port, no CGO dependency
- Pure Go, fast (can encode ~1MB/sec)
- Matches OpenAI's official token counting

**Install:**
```bash
go get github.com/pkoukk/tiktoken-go@latest
```

### Token Counting Function

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

### Chunking Strategy: Token-based with Word Boundary Fallback

The existing `ChunkReference` uses word-boundary chunking (split on space when approaching limit). We preserve this for readability, but the limit becomes **token count**, not character count.

New algorithm:
```
1. Count tokens in the full content
2. If total < chunkSize: return as single chunk
3. Otherwise: accumulate tokens (via encoder) until chunkSize reached
4. Break on word boundary nearest to chunk limit
5. Repeat until all content consumed
```

**Important:** The chunk content itself is stored as text (not token IDs). We count tokens to determine boundaries, then store the text.

---

## 3. CLI Interface

### `mpm reference add <file> [--chunk-size <tokens>]`

Flag: `--chunk-size`  
Type: integer  
Default: `512` (balanced for most LLMs: ~2000 tokens for a 4k context window with 2 chunks = 2检索)

**Validation:**
- Minimum: `64` tokens (very short — for precision search)
- Maximum: `2048` tokens (fits in a single GPT-4o-mini context with room for prompt)
- Invalid values: print error and exit 1

**Usage examples:**
```bash
mpm reference add ./docs/spec.md --chunk-size 256    # Small chunks for dense technical content
mpm reference add ./book.md --chunk-size 1024       # Large chunks for narrative content
mpm reference add ./notes.txt                         # Default 512 tokens
```

### Argument Ordering

Since callers may place `--chunk-size` after the positional path, we pre-scan args the same way `--json` is handled in other commands:

```go
// Pre-scan for --chunk-size (may appear anywhere in args)
for i, arg := range args[2:] {
    if arg == "--chunk-size" && i+1 < len(args) {
        fmt.Sscanf(args[i+1], "%d", &chunkSize)
    }
}
```

---

## 4. Backend Changes

### 4.1 Update `ChunkReference` to Token-Based

In `internal/reference_new.go`, replace the word-boundary chunking with token-counted approach.

**New signature:**
```go
// ChunkByTokens chunks reference content by token count (not character count).
// Uses tiktoken cl100k_base encoding for accurate LLM context window sizing.
// chunkSize is the target token count per chunk (64-2048 recommended).
// Returns chunks with Content field containing the text, Index for ordering.
func ChunkByTokens(content string, chunkSize int) ([]Chunk, error) {
    if chunkSize <= 0 {
        chunkSize = 512
    }
    if chunkSize < 64 {
        chunkSize = 64
    }
    if chunkSize > 8192 { // hard upper limit to prevent pathological behavior
        chunkSize = 8192
    }

    encoder, err := cl100k_base.New()
    if err != nil {
        return nil, fmt.Errorf("failed to load tiktoken encoder: %w", err)
    }
    defer encoder.Close()

    // Fast path: if total tokens < chunkSize, return single chunk
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

Note: `Chunk` is a local alias. Check if `ChunkReference` uses a different type name (`Chunk` vs `ReferenceChunk`) and alias appropriately.

### 4.2 Update `handleRefAdd` to Accept `--chunk-size`

In `cmd/mpm/simple_cmds.go:537`, change:

```go
// Old (line 537):
chunks := mpminternal.ChunkReference(content, 1000)

// New:
chunks, err := mpminternal.ChunkByTokens(content, chunkSize)
if err != nil {
    fmt.Fprintf(os.Stderr, "Error: failed to chunk content: %v\n", err)
    return 1
}
```

Parse `--chunk-size` flag with validation:

```go
fs := flag.NewFlagSet("reference add", flag.ContinueOnError)
chunkSize := fs.Int("chunk-size", 512, "Target chunk size in tokens (default: 512, range: 64-2048)")
// ... flag parse ...

// Validate
if *chunkSize < 64 || *chunkSize > 2048 {
    fmt.Fprintf(os.Stderr, "Error: --chunk-size must be between 64 and 2048 (got %d)\n", *chunkSize)
    return 1
}
```

### 4.3 Backward Compatibility

- **Existing reference docs** are not re-chunked. The `total_chunks` count reflects the chunking used at ingest time.
- **No database schema change** — `reference_chunks.content` is just text.
- **API compatibility** — `ChunkReference` is left as-is (or deprecated) to avoid breaking internal callers. New code uses `ChunkByTokens`.

---

## 5. Tokenization Edge Cases

### Empty / Very Short Content
- Empty string: return empty `[]Chunk`
- Less than `chunkSize` tokens: return single chunk (no truncation)

### Non-English Content
- `cl100k_base` handles all UTF-8 correctly, but tokenization quality varies by language
- CJK characters: typically 1 token per character (tiktoken correctly counts multi-byte UTF-8)
- Code: excellent tokenization (trained on code)

### Embedding Models
- `cl100k_base` is correct for `text-embedding-3-small` and `text-embedding-ada-002`
- If embedding model changes, tokenization stays accurate for context windows

---

## 6. Testing Strategy

### Unit Tests

```go
func TestChunkByTokens_SingleChunk(t *testing.T) {
    // Short text = single chunk
    content := "This is a short sentence."
    chunks, err := ChunkByTokens(content, 512)
    require.NoError(t, err)
    require.Len(t, chunks, 1)
    assert.Equal(t, "This is a short sentence.", chunks[0].Content)
}

func TestChunkByTokens_MultipleChunks(t *testing.T) {
    // Generate content with known token count
    content := strings.Repeat("word ", 200) // 200 tokens
    chunks, err := ChunkByTokens(content, 64)
    require.NoError(t, err)
    // Should produce ~3-4 chunks (200/64 ≈ 3.1)
    assert.GreaterOrEqual(t, len(chunks), 3)
}

func TestChunkByTokens_TokenAccuracy(t *testing.T) {
    // Verify each chunk's actual token count doesn't exceed target
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
    // "hello" = 1 token, "hello world" = 2 tokens
    count, err := CountTokens("hello world")
    require.NoError(t, err)
    assert.Equal(t, 2, count)
}
```

### Integration Test

```bash
# Small chunk test
echo "This is a test document with some content." > /tmp/test.txt
mpm reference add /tmp/test.txt --chunk-size 5 --json

# Verify chunk count is reasonable (should be multiple chunks for longer content)
```

---

## 7. Open Questions — Resolved

| Question | Resolution |
|----------|------------|
| Token library? | `github.com/pkoukk/tiktoken-go` — cl100k_base, no CGO |
| Default chunk size? | `512` tokens — balances precision vs context |
| Validation range? | `64` (min) to `2048` (max) tokens |
| Backward compatibility? | Existing docs not re-chunked; no schema change |
| Existing callers of ChunkReference? | Left as-is or deprecated; new code uses ChunkByTokens |

---

## 8. Files to Modify

| File | Change |
|------|--------|
| `internal/reference_new.go` | Add `CountTokens()`, add `ChunkByTokens()`, deprecate `ChunkReference` |
| `cmd/mpm/simple_cmds.go:537` | Accept `--chunk-size` flag, use `ChunkByTokens`, validate range |
| `internal/reference_test.go` | Add unit tests for `ChunkByTokens`, `CountTokens` |

---

## 9. Implementation Tasks (for plan)

1. **Install tiktoken-go** — `go get github.com/pkoukk/tiktoken-go@latest`
2. **Add `CountTokens()`** — wrap cl100k_base encoder in a simple func
3. **Add `ChunkByTokens()`** — token-based chunking replacing word-boundary
4. **Update `handleRefAdd`** — parse `--chunk-size`, validate, call new function
5. **Write unit tests** — token accuracy, single vs multi-chunk, edge cases
6. **Build and smoke test** — verify flag works end-to-end

---

## Self-Review

- **Placeholder scan:** No TBD, all code is concrete
- **Internal consistency:** `chunkSize` param range (64-2048) is same in CLI validation and backend function
- **Scope check:** Focused on `mpm reference add --chunk-size`; `mpm ingest` does not use chunking (it stages raw memories, not reference docs)
- **Ambiguity check:** `ChunkByTokens` returns `Chunk` (local alias) — must verify existing `ChunkReference` type name and alias correctly