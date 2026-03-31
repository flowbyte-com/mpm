# Native Document Ingestion

> 📄 **Pure Go PDF/EPUB parsing.** No Python, no Calibre, no external dependencies.

## Overview

MPM supports native document parsing using optimized pure Go implementations. This provides:

- **No external dependencies** — No Python, Calibre, or pdfplumber
- **Fast parsing** — ~1.5s for 10MB PDF, ~0.8s for 5MB EPUB
- **Small binary** — No bundled language runtimes
- **Cross-platform** — Compiles anywhere Go works

## Supported Formats

| Format | Extension | Library | Status |
|--------|-----------|---------|--------|
| Plain Text | `.txt` | Built-in `os` | ✅ Production |
| Markdown | `.md` | Built-in | ✅ Production |
| JSON | `.json` | Built-in `encoding/json` | ✅ Production |
| PDF | `.pdf` | `github.com/ledongthuc/pdf` v0.5+ | ✅ Production |
| EPUB | `.epub` | `archive/zip` + `golang.org/x/net/html` | ✅ Production |
| HTML | `.html` | `golang.org/x/net/html` | ✅ Production |

---

## PDF Parsing

### Library Details

| Property | Value |
|----------|-------|
| Package | `github.com/ledongthuc/pdf` |
| Version | v0.5+ (check `go.mod`) |
| Method | Pure Go, no cgo |
| Dependencies | None beyond standard library |
| Limitations | Scanned/image PDFs not supported (no OCR) |

### Implementation

```go
func ParsePDF(filePath string) (string, error) {
    // Open the PDF file
    f, r, err := pdf.Open(filePath)
    if err != nil {
        return "", fmt.Errorf("failed to open PDF: %w", err)
    }
    defer f.Close()

    // Get plain text content
    buf, err := r.GetPlainText()
    if err != nil {
        return "", fmt.Errorf("failed to extract text: %w", err)
    }
    
    extracted := buf.String()
    if extracted == "" {
        return "", fmt.Errorf("PDF contains no extractable text (possibly scanned)")
    }
    
    return extracted, nil
}
```

### Error Handling

| Error | Cause | Solution |
|-------|-------|----------|
| `failed to open PDF` | File doesn't exist or corrupted | Verify file integrity |
| `failed to extract text` | PDF is scanned/image-only | OCR required (out of scope) |
| `no extractable text` | Empty PDF or image-based | Use OCR tool before ingestion |
| `invalid PDF structure` | Malformed PDF | Repair with `pdftk` or similar |

### Performance

| Document Size | Parse Time | Notes |
|---------------|------------|-------|
| 1 MB PDF | ~0.3s | Simple text document |
| 10 MB PDF | ~1.5s | Mixed text and images |
| 50 MB PDF | ~7s | Large document |
| 100+ MB PDF | Not recommended | Split into smaller files |

---

## EPUB Parsing

### Method

EPUB files are ZIP archives containing XHTML/HTML content. MPM:

1. Opens the ZIP archive
2. Extracts all `.html` and `.xhtml` files
3. Strips HTML tags using `golang.org/x/net/html`
4. Concatenates text with section separators

### Implementation

```go
func ParseEPUB(filePath string) (string, error) {
    r, err := zip.OpenReader(filePath)
    if err != nil {
        return "", fmt.Errorf("failed to open EPUB: %w", err)
    }
    defer r.Close()

    var extractedText strings.Builder
    
    for _, f := range r.File {
        name := strings.ToLower(f.Name)
        if !strings.HasSuffix(name, ".html") && !strings.HasSuffix(name, ".xhtml") {
            continue
        }
        
        rc, err := f.Open()
        if err != nil {
            continue  // Skip unreadable files
        }
        
        text := stripHTML(rc)
        if text != "" {
            extractedText.WriteString(text)
            extractedText.WriteString("\n\n---\n\n")
        }
        rc.Close()
    }
    
    if extractedText.Len() == 0 {
        return "", fmt.Errorf("no readable text in EPUB: %s", filePath)
    }
    
    return extractedText.String(), nil
}

func stripHTML(rc io.ReadCloser) string {
    doc, err := html.Parse(rc)
    if err != nil {
        return ""
    }
    
    var buf strings.Builder
    var f func(*html.Node)
    f = func(n *html.Node) {
        if n.Type == html.TextNode {
            buf.WriteString(n.Data)
        }
        for c := n.FirstChild; c != nil; c = c.NextSibling {
            f(c)
        }
        if n.Type == html.ElementNode {
            buf.WriteString(" ")
        }
    }
    f(doc)
    return buf.String()
}
```

### Error Handling

| Error | Cause | Solution |
|-------|-------|----------|
| `failed to open EPUB` | Invalid ZIP or corrupted | Verify file integrity |
| `no readable text` | Malformed XHTML or DRM | Check if DRM-protected |
| Empty content | Malformed EPUB structure | TryCalibre or repair |

---

## Chunking Strategy

Documents are automatically chunked for embedding and search.

### Default Configuration

| Setting | Default | Description |
|---------|---------|-------------|
| Chunk size | 2000 characters | Target size per chunk |
| Chunk overlap | 200 characters | Overlap between chunks |
| Min chunk size | 100 characters | Skip very small chunks |

### How Chunking Works

```
Document: "Chapter 1: Introduction to Docker..."
    ↓
1. Split by section headers (##, ###)
2. Split by paragraph boundaries
3. Split by character limit (2000)
4. Add overlap between chunks (200)
    ↓
Chunks: [
    "Chapter 1: Introduction to Docker...\n\nDocker is...",
    "...Docker is a containerization tool.\n\n## Chapter 2...",
    "## Chapter 2: Installation\n\nTo install..."
]
```

### Customizing Chunk Size

```bash
# Add with custom chunk size
mpm reference add /path/to/doc.pdf --chunk-size 3000

# Add with smaller chunks (more precise search)
mpm reference add /path/to/doc.pdf --chunk-size 1000
```

> 💡 **Chunking Best Practices:**
> - **Smaller chunks** (500-1000): More precise search, better for Q&A
> - **Larger chunks** (2000-4000): Better context preservation, better for summaries
> - **Technical docs**: Smaller chunks (1000-1500) work well
> - **Narrative prose**: Larger chunks (2000-3000) preserve flow

---

## Integration

### Adding Documents

```bash
# Add a PDF
mpm reference add /path/to/document.pdf

# Add an EPUB with custom title
mpm reference add /path/to/book.epub --title "Book Title"

# Add with tags
mpm reference add /path/to/doc.pdf --tags "docker,containers,devops"

# Add with custom chunk size
mpm reference add /path/to/doc.pdf --chunk-size 2500
```

### Listing References

```bash
mpm reference list
```

### Searching References

```bash
# Keyword search (title and tags)
mpm reference search "docker"

# Semantic search (uses embeddings)
mpm memory search "docker containers"
```

---

## Performance Comparison

| Task | Before (Python) | After (Go) | Improvement |
|------|-----------------|------------|-------------|
| PDF: 10MB | ~2s | ~1.5s | 25% faster |
| EPUB: 5MB | ~1s | ~0.8s | 20% faster |
| Binary size | +50MB (Python) | +5MB | 90% smaller |
| Startup time | ~2s (Python init) | ~0s | Instant |

---

## Security Considerations

### During Ingestion

Sensitive content blocking runs **before** text is stored:

```
Document → Parse PDF/EPUB → Extract text → Sensitive check → Chunk → Store
                                                    ↓
                                           Block if secret found
```

### What Gets Blocked

The same 17 regex patterns used for memory operations apply during ingestion:
- API keys (OpenAI, GitHub, AWS, Stripe)
- Passwords and secrets
- Private keys
- Database connection strings

### Limitations

| Limitation | Impact |
|------------|--------|
| No OCR | Scanned PDFs cannot be ingested |
| No DRM removal | Protected EPUBs fail |
| No image extraction | Images in PDFs are ignored |
| No password-protected PDFs | Must provide password externally |

---

## Extensibility

### Adding New Formats

To add support for a new format:

1. Create a parser function in `internal/parser.go`
2. Register the extension in the parser registry
3. Add tests
4. Update this document

```go
func ParseDOCX(filePath string) (string, error) {
    // Implementation here
}

func init() {
    RegisterParser(".docx", ParseDOCX)
}
```

### External Dependencies

If pure Go isn't sufficient:

1. **OCR:** Use Tesseract via `os/exec` calls
2. **Word docs:** Use `unoconv` or `libreoffice` CLI
3. **Complex EPUBs:** Pre-process with Calibre CLI

---

**Last Updated:** 2026-03-29  
**MPM Version:** 6.0.0
