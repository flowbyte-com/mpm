# Reference Library Commands

> 📚 **Document ingestion.** Store and search PDF, EPUB, Markdown, and other documents.

## Overview

The reference library stores documents (txt, md, pdf, epub, json) in SQLite for persistent storage and retrieval. Documents are automatically chunked and indexed for search.

### Key Capabilities

| Feature | Description |
|---------|-------------|
| **Multiple Formats** | PDF, EPUB, MD, TXT, JSON, HTML |
| **Native Parsing** | Pure Go, no external tools |
| **Automatic Chunking** | Configurable chunk sizes |
| **Full-text Search** | FTS5 indexing |
| **Vector Search** | SHA256-based embeddings (extensible) |

---

## CLI Commands

### Add a Reference
```bash
mpm reference add <path> [--title <title>] [--tags <tags>] [--chunk-size <n>]
```

Ingests a document into the reference library.

**Options:**

| Option | Default | Description |
|--------|---------|-------------|
| `--title` | Filename | Document title |
| `--tags` | None | Comma-separated tags |
| `--chunk-size` | 2000 | Characters per chunk |

**Examples:**
```bash
# Basic add
mpm reference add /path/to/doc.pdf

# With title and tags
mpm reference add /path/to/doc.pdf \
  --title "Docker Best Practices" \
  --tags "docker,containers,devops"

# Custom chunk size
mpm reference add /path/to/long-doc.pdf --chunk-size 3000
```

### List References
```bash
mpm reference list
```

Shows all documents with ID, title, source path, type, chunk count, and timestamp.

**Example output:**
```
$ mpm reference list

┌─────────────────────────────────────────────────────────────┐
│ References                                                  │
├─────────────────────────────────────────────────────────────┤
│ ID: ref_001 | Docker Best Practices                        │
│          | pdf | 45 chunks | 2026-03-28                   │
│                                                          │
│ ID: ref_002 | API Design Guide                             │
│          | epub | 32 chunks | 2026-03-27                  │
└─────────────────────────────────────────────────────────────┘
```

### Search References
```bash
mpm reference search <query> [--limit <n>]
```

Performs keyword matching against title and tags.

> 💡 **For semantic search** (meaning-based), use `mpm memory search` which uses vector embeddings.

**Example:**
```bash
$ mpm reference search "docker"

┌─────────────────────────────────────────────────────────────┐
│ docker                                                     │
├─────────────────────────────────────────────────────────────┤
│ ref_001 | Docker Best Practices | docker,containers,devops│
└─────────────────────────────────────────────────────────────┘
```

### Get Full Document
```bash
mpm reference get <id>
```

Retrieves the full document content with markdown section headers preserved.

**Example:**
```bash
$ mpm reference get ref_001

# Document: Docker Best Practices

## Chapter 1: Introduction

Docker is a containerization platform...

## Chapter 2: Installation

To install Docker...
```

### Delete Reference (Soft)
```bash
mpm reference delete <id>
```

Prompts for confirmation. Soft delete only.

### Shred Reference (Hard Delete)
```bash
mpm reference shred <id>
```

True irreversible erasure with `DELETE` + `VACUUM`.

### Statistics
```bash
mpm reference stats
```

Shows total documents and chunks.

---

## File Format Support

| Format | Extension | Parser | Notes |
|--------|-----------|--------|-------|
| Plain Text | `.txt` | Built-in | Raw text |
| Markdown | `.md` | Built-in | Sections extracted |
| JSON | `.json` | Built-in | Structure preserved |
| PDF | `.pdf` | `github.com/ledongthuc/pdf` | Text extracted, no OCR |
| EPUB | `.epub` | Pure Go ZIP + HTML | Text extracted |
| HTML | `.html` | `golang.org/x/net/html` | Basic parsing |

### Format Notes

| Format | Limitations |
|--------|-------------|
| PDF | Scanned/image PDFs not supported (no OCR) |
| EPUB | DRM-protected files not supported |
| JSON | Nested structures flattened |
| HTML | Scripts and styles stripped |

---

## Chunking Strategy

Documents are automatically split into chunks for embedding and search.

### How It Works

```
Document
    ↓
1. Split by section headers (##, ###)
2. Split by paragraph boundaries
3. Split by character limit (default 2000)
4. Add overlap (200 chars)
    ↓
Chunks stored with metadata
```

### Chunk Configuration

| Setting | Default | Use Case |
|---------|---------|----------|
| `--chunk-size 1000` | Fine-grained | Q&A, precise search |
| `--chunk-size 2000` | Balanced | General purpose |
| `--chunk-size 4000` | Large context | Summaries, long documents |

### Chunk Metadata

Each chunk stores:
- Reference ID (parent document)
- Chunk index (order)
- Section header (if available)
- Character count
- Content text

---

## Embeddings

### Built-in Embeddings

MPM includes basic SHA256-based embeddings:

| Property | Value |
|----------|-------|
| Algorithm | SHA256 hash → 32 float64 values |
| Dimension | 32 |
| Range | [-1, 1] normalized |
| Semantic Accuracy | Limited (hash-based) |

> ⚠️ **Limitation:** SHA256-based embeddings are deterministic but not semantically meaningful. For true semantic search, configure an external embedding API.

### External Embedding API

For better semantic search, configure an external provider:

```bash
export OPENAI_API_KEY="sk-..."
# Or configure in mpm_config.json
```

**Supported providers:**
- OpenAI (text-embedding-3-small)
- Local LLM (Ollama, LM Studio)

### Semantic Search

```bash
# Search references semantically (uses embeddings)
mpm memory search "docker container orchestration"
```

---

## Examples

### Add Documents
```bash
# Add a PDF
mpm reference add ~/docs/docker-guide.pdf \
  --title "Docker Guide" \
  --tags "docker,containers"

# Add an EPUB
mpm reference add ~/books/api-design.epub \
  --title "API Design Book"

# Add Markdown
mpm reference add ~/notes/architecture.md
```

### Search and Retrieve
```bash
# List all references
mpm reference list

# Search by keyword
mpm reference search "docker"

# Get full document
mpm reference get ref_001
```

### Manage Documents
```bash
# Check stats
mpm reference stats

# Delete old documents
mpm reference shred ref_old
```

---

## Security Considerations

### Sensitive Content Blocking

During ingestion, content is scanned for sensitive data **before** storage:

```
Document → Parse → Extract text → Sensitive check → Chunk → Store
                                              ↓
                                     Block if secret found
```

The same 17 regex patterns used for memory operations apply during ingestion:
- API keys, passwords, tokens
- Private keys, SSH keys
- Database connection strings

### File Permissions

Reference files remain at their original locations. MPM stores:
- File path (not the file itself)
- Extracted text chunks
- Metadata

---

## Troubleshooting

| Issue | Solution |
|-------|----------|
| `Failed to parse PDF` | File may be scanned/image-only (no OCR) |
| `No text in EPUB` | File may be DRM-protected |
| `Chunk size too large` | Reduce with `--chunk-size 1000` |
| `Slow ingestion` | Normal for large PDFs (~1.5s per 10MB) |

---

**Last Updated:** 2026-03-29  
**MPM Version:** 6.0.0
