// reference_extract.go — turning a source into text that is worth
// chunking.
//
// Two things matter beyond "the words are there":
//
//  1. Section structure. ChunkByTokens produces anonymous slabs of text,
//     so every search result says "section: (empty)" and an agent cannot
//     tell a function signature from the prose three headings later.
//     Markdown is split at headings first and each section is chunked
//     independently, so each chunk carries the heading it lives under.
//     Chunk indices stay sequential across the document.
//
//  2. Titles. A document imported as `wp_nav_menu.md` should be titled
//     "wp_nav_menu()" when the document says so. The filename is the
//     fallback, not the answer.
//
// Both transformations are deterministic and content-only: the same
// bytes always produce the same chunks and the same title, so the
// content-hash diff keeps working.
package internal

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"unicode"
)

// parseAbsoluteURL is the small parser used for title derivation and
// URL display paths.
func parseAbsoluteURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	return u, nil
}

// parseReferenceDocument reads and extracts one local document.
//
// The returned notes describe extraction decisions the caller should be
// able to see (for example, a stripped front-matter block). They are
// reported, not acted on.
func parseReferenceDocument(path string) (string, []string, error) {
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".pdf":
		text, err := ParsePDF(path)
		if err != nil {
			return "", nil, fmt.Errorf("parse pdf %q: %w", path, err)
		}
		return text, []string{"pdf: text extracted without layout; tables and figures are not preserved"}, nil
	case ".epub":
		text, err := ParseEPUB(path)
		if err != nil {
			return "", nil, fmt.Errorf("parse epub %q: %w", path, err)
		}
		return text, []string{"epub: chapter HTML flattened to text; heading levels are approximate"}, nil
	case ".html", ".xhtml":
		data, err := os.ReadFile(path)
		if err != nil {
			return "", nil, fmt.Errorf("read %q: %w", path, err)
		}
		doc, err := ExtractHTMLDocument(strings.NewReader(string(data)), nil)
		if err != nil {
			// Fall back to the flat stripper rather than refusing a
			// document we could partially read.
			return StripHTML(string(data)), []string{"html: structured extraction failed (" + err.Error() + "); fell back to plain text"}, nil
		}
		notes := append([]string{"html: structured extraction"}, doc.Notes...)
		return doc.Body, notes, nil
	case ".md", ".markdown":
		data, err := os.ReadFile(path)
		if err != nil {
			return "", nil, fmt.Errorf("read %q: %w", path, err)
		}
		body, notes := prepareMarkdown(string(data))
		return body, notes, nil
	case ".txt", "":
		data, err := os.ReadFile(path)
		if err != nil {
			return "", nil, fmt.Errorf("read %q: %w", path, err)
		}
		return string(data), nil, nil
	default:
		// Best-effort: read as text. A binary file produces replacement
		// characters and the emptiness check upstream rejects it.
		data, err := os.ReadFile(path)
		if err != nil {
			return "", nil, fmt.Errorf("unsupported file type %q: %w", ext, err)
		}
		return string(data), []string{"unknown extension " + ext + ": read as plain text"}, nil
	}
}

// prepareMarkdown turns raw Markdown into document text.
//
// Four transformations, each reported:
//
//  1. sanitizeReferenceText — non-breaking spaces become spaces and
//     Unicode format/tag characters are dropped. Documentation exports
//     routinely carry "copy link" glyphs (U+E0001 … U+E007F) and NBSP
//     runs from code samples; both corrupt section names and tokenization.
//  2. stripFrontMatter — a `---`/`+++` delimited metadata block.
//  3. stripLabelPreamble — a `Title: …` / `Published: …` header ending in
//     `---`, which is how several documentation exporters (WordPress
//     among them) render their Markdown rendition.
//  4. dropLeadingTOC — an "In this article" / "Contents" section that is
//     nothing but anchor links.
//
// None of these touch prose: each fires only on a shape it can recognise,
// and every removal is reported so a caller can see what was dropped.
func prepareMarkdown(content string) (string, []string) {
	text := sanitizeReferenceText(content)
	var notes []string
	text, frontNotes := stripFrontMatter(text)
	notes = append(notes, frontNotes...)
	text, preambleNotes := stripLabelPreamble(text)
	notes = append(notes, preambleNotes...)
	text, tocNotes := dropLeadingTOC(text)
	notes = append(notes, tocNotes...)
	return text, notes
}

// sanitizeReferenceText normalises characters that corrupt retrieval
// without carrying content.
func sanitizeReferenceText(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r == '\u00a0':
			b.WriteRune(' ')
		case r == '\ufeff':
			// byte-order mark
		case r >= 0xE0000 && r <= 0xE007F:
			// Unicode tag characters (invisible link markers)
		case unicode.Is(unicode.Cf, r):
			// zero-width joiners, directional marks, soft hyphens
		case r == '\r':
			// normalise line endings below
		default:
			b.WriteRune(r)
		}
	}
	out := strings.ReplaceAll(b.String(), "\r\n", "\n")
	return strings.ReplaceAll(out, "\r", "\n")
}

// stripLabelPreamble removes a `Key: value` header block terminated by a
// horizontal rule.
//
// The shape is: at least two consecutive `Label: value` lines, then a
// `---` line, before any heading. That is metadata (title, publication
// date, modification date), not documentation, and on a long export it
// occupies the whole first chunk.
func stripLabelPreamble(content string) (string, []string) {
	lines := strings.Split(content, "\n")
	labels := 0
	terminator := -1
	for i, line := range lines {
		if i > 12 {
			break
		}
		trimmed := strings.TrimSpace(line)
		if trimmed == "---" && labels >= 2 {
			terminator = i
			break
		}
		if trimmed == "" && labels == 0 {
			return content, nil // leading blank line: not this shape
		}
		if trimmed == "" {
			continue
		}
		if !isMetadataLabelLine(trimmed) {
			return content, nil
		}
		labels++
	}
	if terminator < 0 || labels < 2 {
		return content, nil
	}
	rest := strings.TrimLeft(strings.Join(lines[terminator+1:], "\n"), "\n")
	if strings.TrimSpace(rest) == "" {
		return content, nil
	}
	return rest, []string{fmt.Sprintf("markdown: removed a %d-line metadata header (title/date labels) so it does not dominate the first chunk", labels+1)}
}

// isMetadataLabelLine reports whether a line looks like `Label: value`
// with a short capitalised label — the shape of an exported metadata
// header, not of prose that happens to contain a colon.
func isMetadataLabelLine(line string) bool {
	i := strings.Index(line, ":")
	if i < 2 || i > 24 {
		return false
	}
	label := line[:i]
	for _, r := range label {
		if !(r >= 'A' && r <= 'Z') && !(r >= 'a' && r <= 'z') && r != ' ' {
			return false
		}
	}
	// Uppercase first letter, letters and spaces only: `Last modified`,
	// `Published`, `Title`. Prose with a colon ("Note: this is prose")
	// also matches, which is why at least two such lines and a closing
	// rule are required before anything is removed.
	return label[0] >= 'A' && label[0] <= 'Z'
}

// tocHeadingNames are headings whose body is navigation rather than
// documentation when the body is almost entirely links.
var tocHeadingNames = []string{"in this article", "in this section", "on this page", "contents", "table of contents", "sections"}

// dropLeadingTOC removes a leading table-of-contents section.
//
// The test is deliberately narrow: the heading must be one of the known
// names, the body must contain at least three link list items, and
// almost all of the body must be those links. A "Contents" section that
// explains something is left alone.
func dropLeadingTOC(content string) (string, []string) {
	lines := strings.Split(content, "\n")
	// Look for a TOC-ish heading among the first few headings. A document
	// may open with its own h1 before the contents block (WordPress does),
	// so stopping at the first heading would miss it entirely.
	start, level, seen := -1, 0, 0
	for i, line := range lines {
		l, text, ok := parseATXHeading(line)
		if !ok {
			continue
		}
		seen++
		if seen > 4 {
			break
		}
		lower := strings.ToLower(strings.TrimSpace(plainHeadingText(text)))
		for _, name := range tocHeadingNames {
			if lower == name {
				start, level = i, l
				break
			}
		}
		if start >= 0 {
			break
		}
	}
	if start < 0 {
		return content, nil
	}
	// Body runs until the next heading of the same or higher level.
	end := len(lines)
	for i := start + 1; i < len(lines); i++ {
		l, _, ok := parseATXHeading(lines[i])
		if ok && l <= level {
			end = i
			break
		}
	}
	body := lines[start+1 : end]
	linkItems, other := 0, 0
	for _, line := range body {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if (strings.HasPrefix(trimmed, "* ") || strings.HasPrefix(trimmed, "- ") || strings.HasPrefix(trimmed, "+ ")) && strings.Contains(trimmed, "](") {
			linkItems++
			continue
		}
		if strings.HasPrefix(trimmed, "* ") || strings.HasPrefix(trimmed, "- ") || strings.HasPrefix(trimmed, "+ ") {
			// nested list item belonging to a link entry
			if linkItems > 0 {
				continue
			}
		}
		other += len(strings.Fields(trimmed))
	}
	if linkItems < 3 || other > 20 {
		return content, nil
	}
	kept := append(append([]string{}, lines[:start]...), lines[end:]...)
	out := strings.Join(kept, "\n")
	if strings.TrimSpace(out) == "" {
		return content, nil
	}
	return out, []string{fmt.Sprintf("markdown: removed a %d-entry table-of-contents section", linkItems)}
}

// maxFrontMatterBytes bounds the YAML/TOML front-matter block that is
// treated as metadata rather than content. A Jekyll/Hugo page can carry
// kilobytes of front matter; those bytes are not documentation, and
// they dominate the first chunk if left in place.
const maxFrontMatterBytes = 1500

// stripFrontMatter removes a leading `---` … `---` (or `+++`) block when
// it looks like metadata rather than content.
//
// The rule is deliberately conservative: the block must open on the
// first line and be terminated, and it must either be large enough to
// dominate a chunk (> maxFrontMatterBytes) or look like YAML (every
// non-empty line is `key: value` / a list item under a key). A short
// `---`-delimited note that happens to be prose is left alone, because
// silently deleting documentation is worse than a slightly noisy chunk.
func stripFrontMatter(content string) (string, []string) {
	if !strings.HasPrefix(content, "---\n") && !strings.HasPrefix(content, "+++\n") {
		return content, nil
	}
	delim := content[:3]
	rest := content[4:]
	end := strings.Index(rest, "\n"+delim)
	if end < 0 {
		return content, nil
	}
	block := rest[:end]
	after := rest[end+1+len(delim):]
	if strings.TrimSpace(after) == "" {
		// The whole document is one delimited block; keep it.
		return content, nil
	}
	if len(block) <= maxFrontMatterBytes && !looksLikeYAML(block) {
		return content, nil
	}
	return strings.TrimLeft(after, "\n"), []string{
		fmt.Sprintf("markdown: removed a %d-byte front-matter block so it does not dominate the first chunk", len(block)),
	}
}

// looksLikeYAML reports whether a block plausibly is key/value metadata.
func looksLikeYAML(block string) bool {
	lines := strings.Split(block, "\n")
	kv := 0
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.HasPrefix(trimmed, "- ") {
			kv++
			continue
		}
		if i := strings.Index(trimmed, ":"); i > 0 {
			key := trimmed[:i]
			if !strings.ContainsAny(key, " \t") {
				kv++
				continue
			}
		}
		return false
	}
	return kv > 0
}

// titleFromReferenceContent derives a human title for a document.
//
// Markdown: the first level-1 heading, else the first heading at any
// level, else the filename without extension. Other formats: the
// filename without extension, because a PDF's first line is frequently
// a running header rather than a title.
func titleFromReferenceContent(path, content string) string {
	ext := strings.ToLower(filepath.Ext(path))
	if ext == ".md" || ext == ".markdown" {
		inFence := false
		var firstAny string
		for _, line := range strings.Split(content, "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
				inFence = !inFence
				continue
			}
			if inFence {
				continue
			}
			level, text, ok := parseATXHeading(line)
			if !ok {
				continue
			}
			if level == 1 {
				return text
			}
			if firstAny == "" {
				firstAny = text
			}
		}
		if firstAny != "" {
			return firstAny
		}
	}
	base := filepath.Base(path)
	if ext != "" {
		base = strings.TrimSuffix(base, filepath.Ext(base))
	}
	return strings.ReplaceAll(base, "-", " ")
}

// parseATXHeading parses a Markdown ATX heading line (`## text`).
func parseATXHeading(line string) (level int, text string, ok bool) {
	trimmed := strings.TrimLeft(line, " \t")
	if trimmed == "" || trimmed[0] != '#' {
		return 0, "", false
	}
	level = 0
	for level < len(trimmed) && trimmed[level] == '#' {
		level++
	}
	if level == 0 || level > 6 {
		return 0, "", false
	}
	if level < len(trimmed) && trimmed[level] != ' ' && trimmed[level] != '\t' {
		return 0, "", false // `#hashtag`, not a heading
	}
	text = strings.TrimSpace(strings.TrimRight(trimmed[level:], "#"))
	text = strings.TrimSpace(text)
	if text == "" {
		return 0, "", false
	}
	return level, text, true
}

// chunkReferenceDocument chunks extracted text, carrying section headings
// where the format has them.
//
// Markdown is split at headings (levels 1–3 form the section path; deeper
// headings stay inline as content) and each section is chunked on its
// own. Chunk indices are then renumbered sequentially so chunk identity
// stays a function of (doc, position, content) exactly as before.
func chunkReferenceDocument(content, sourceType string, chunkSize int) ([]Chunk, error) {
	if sourceType != "markdown" {
		return ChunkByTokens(content, chunkSize)
	}
	sections := splitMarkdownSections(content)
	if len(sections) == 0 {
		return ChunkByTokens(content, chunkSize)
	}
	var out []Chunk
	for _, sec := range sections {
		body := strings.TrimSpace(sec.Body)
		if body == "" && sec.Section == "" {
			continue
		}
		// Keep the heading in the chunk text: a chunk that says
		// "wp_nav_menu()" above the signature is materially easier to
		// retrieve and to cite than one that does not.
		text := body
		if sec.Leaf != "" {
			heading := strings.Repeat("#", max(sec.Level, 1)) + " " + sec.Leaf
			if !strings.HasPrefix(strings.TrimSpace(body), strings.Repeat("#", max(sec.Level, 1))) {
				text = heading + "\n\n" + body
			}
		}
		if strings.TrimSpace(text) == "" {
			continue
		}
		pieces, err := ChunkByTokens(text, chunkSize)
		if err != nil {
			return nil, err
		}
		for _, p := range pieces {
			out = append(out, Chunk{
				Index:   len(out),
				Section: sec.Section,
				Content: p.Content,
			})
		}
	}
	if len(out) == 0 {
		return ChunkByTokens(content, chunkSize)
	}
	return out, nil
}

// markdownSection is one heading-delimited span of a Markdown document.
type markdownSection struct {
	// Section is the full heading path ("A › B").
	Section string
	// Leaf is just this section's own heading text, used for the heading
	// line written into the chunk so the chunk does not open with a
	// breadcrumb.
	Leaf  string
	Level int
	Body  string
}

// splitMarkdownSections splits content at ATX headings, tracking the
// heading path so a chunk under "## Validation" inside "# Turnstile"
// reports "Turnstile › Validation".
//
// Fenced code blocks are respected: `# comment` inside a fence is a
// comment, not a heading.
func splitMarkdownSections(content string) []markdownSection {
	lines := strings.Split(content, "\n")
	var (
		out      []markdownSection
		current  markdownSection
		body     strings.Builder
		path     [4]string // stack for levels 1..3
		inFence  bool
		haveAny  bool
		fenceTag string
	)
	flush := func() {
		current.Body = body.String()
		out = append(out, current)
		body.Reset()
	}
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			tag := trimmed[:3]
			if !inFence {
				inFence = true
				fenceTag = tag
			} else if tag == fenceTag {
				inFence = false
			}
			body.WriteString(line)
			body.WriteString("\n")
			continue
		}
		if !inFence {
			if level, text, ok := parseATXHeading(line); ok {
				if haveAny {
					flush()
				}
				haveAny = true
				if level <= 3 {
					path[level-1] = plainHeadingText(text)
					for i := level; i < len(path); i++ {
						path[i] = ""
					}
				}
				current = markdownSection{
					Section: joinHeadingPath(path[:]),
					Leaf:    plainHeadingText(text),
					Level:   level,
				}
				// The heading line itself is re-added by the caller.
				continue
			}
		}
		body.WriteString(line)
		body.WriteString("\n")
	}
	if haveAny {
		flush()
	}
	return out
}

// plainHeadingText strips the Markdown decoration that documentation
// exports put inside headings, so a section label reads
// "Parameters" rather than "[Parameters](https://…#parameters)".
func plainHeadingText(text string) string {
	out := text
	// [label](target) → label
	for {
		open := strings.Index(out, "[")
		if open < 0 {
			break
		}
		close := strings.Index(out[open:], "]")
		if close < 0 {
			break
		}
		close += open
		label := out[open+1 : close]
		rest := out[close+1:]
		if !strings.HasPrefix(rest, "(") {
			break
		}
		end := strings.Index(rest, ")")
		if end < 0 {
			break
		}
		out = out[:open] + label + rest[end+1:]
	}
	out = strings.ReplaceAll(out, "`", "")
	out = strings.ReplaceAll(out, "**", "")
	out = strings.ReplaceAll(out, "__", "")
	out = strings.ReplaceAll(out, "*", "")
	return strings.TrimSpace(collapseWhitespace(out))
}

// joinHeadingPath renders the heading stack as "A › B › C".
func joinHeadingPath(path []string) string {
	var parts []string
	for _, p := range path {
		if strings.TrimSpace(p) != "" {
			parts = append(parts, p)
		}
	}
	return strings.Join(parts, " › ")
}
