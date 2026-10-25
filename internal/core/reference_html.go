// reference_html.go — structured HTML → documentation text.
//
// The old path was StripHTML: concatenate every text node, discard
// everything else. That loses exactly the information documentation is
// made of — which paragraph is a function signature, which block is a
// code sample and in which language, which callout is a warning, which
// numbers belong to a table — and it keeps exactly the information an
// agent does not need: navigation menus, cookie banners, "Was this
// helpful?" widgets and repeated footers.
//
// This extractor keeps a deliberately small contract:
//
//   - it does NOT render a page or run JavaScript;
//   - it does NOT summarise or invent content;
//   - it prefers an explicitly-marked main content region when the page
//     has one, and says so when it has to guess;
//   - it reports what it dropped, so an extraction that lost a warning
//     block is visible rather than silently lossy.
//
// Output is Markdown-flavoured text: headings, fenced code with language
// hints, lists, tables, blockquotes and links. Markdown is the format the
// rest of the pipeline already understands, and it preserves structure
// through chunking without a second representation.
package internal

import (
	"fmt"
	"io"
	"net/url"
	"strings"

	"golang.org/x/net/html"
)

// ExtractedDocument is the result of structured HTML extraction.
type ExtractedDocument struct {
	// Title is the document title: <h1> of the main region when present,
	// else og:title, else <title>.
	Title string
	// Body is Markdown-flavoured text with headings, code fences, lists,
	// tables and links preserved.
	Body string
	// Notes describe what was removed or how the region was chosen.
	Notes []string
	// Byline / SiteName carry publisher identity when the page advertises
	// it (og:site_name, meta author) so attribution is available without
	// polluting the chunk body.
	Byline   string
	SiteName string
	// License is the detected licence statement (rel="license",
	// <link rel="license">, or a footer line mentioning a licence).
	License string
}

// htmlDropTags are removed entirely: they carry no documentation value
// and their text is pure chrome or code.
var htmlDropTags = map[string]bool{
	"script":   true,
	"style":    true,
	"noscript": true,
	"template": true,
	"iframe":   true,
	"svg":      true,
	"canvas":   true,
	"form":     true,
	"input":    true,
	"button":   true,
	"select":   true,
	"textarea": true,
	"nav":      true,
	"aside":    true,
	"footer":   true,
	"dialog":   true,
	"menu":     true,
}

// htmlChromeHints are substrings (matched case-insensitively against
// class and id) that reliably mark non-content regions on documentation
// sites. They are heuristics, and they only ever remove a subtree that
// is nested inside chrome — never the whole page.
var htmlChromeHints = []string{
	"cookie", "consent", "gdpr", "newsletter", "subscribe", "ads-",
	"advert", "social", "share-", "sharing", "breadcrumb", "pagination",
	"sidebar", "site-nav", "navbar", "skip-link", "announcement",
	"banner", "promo", "feedback", "was-this-helpful", "toc-",
	"table-of-contents", "edit-page", "edit-this", "breadcrumbs",
}

// htmlMainHints mark the content region.
var htmlMainHints = []string{"main", "article", "documentation", "markdown-body", "entry-content", "post-content", "content"}

// ExtractHTMLDocument converts an HTML document into structured
// Markdown-flavoured text.
//
// baseURL, when non-nil, resolves relative links so citations are
// absolute. A nil baseURL keeps link text but drops the href, because a
// bare relative href is not a usable citation from outside the site.
func ExtractHTMLDocument(r io.Reader, baseURL *url.URL) (*ExtractedDocument, error) {
	root, err := html.Parse(r)
	if err != nil {
		return nil, fmt.Errorf("parse html: %w", err)
	}

	doc := &ExtractedDocument{}
	var notes []string

	// Collect document-level metadata before pruning: og:title and
	// rel=license often live in <head>, which never reaches the body.
	meta := collectHTMLMetadata(root)
	doc.SiteName = meta.siteName
	doc.Byline = meta.byline
	doc.License = meta.license

	main := findMainRegion(root)
	if main == nil {
		main = findBody(root)
		notes = append(notes, "no <main>/<article> region found; extracted from <body>")
	} else {
		notes = append(notes, "extracted from the page's main content region")
	}
	if main == nil {
		return nil, fmt.Errorf("html document has no body")
	}

	dropped := pruneChrome(main)
	if dropped > 0 {
		notes = append(notes, fmt.Sprintf("removed %d navigation/boilerplate subtree(s)", dropped))
	}

	if doc.Title == "" {
		if h1 := firstHeadingText(main); h1 != "" {
			doc.Title = h1
		} else if meta.ogTitle != "" {
			doc.Title = meta.ogTitle
		} else if meta.title != "" {
			doc.Title = meta.title
		}
	}

	var b strings.Builder
	renderNode(&b, main, baseURL, 0)
	doc.Body = normaliseMarkdown(sanitizeReferenceText(b.String()))
	doc.Notes = notes

	if strings.TrimSpace(doc.Body) == "" {
		return nil, fmt.Errorf("html document has no extractable text after removing navigation and scripts")
	}
	return doc, nil
}

type htmlMetadata struct {
	title    string
	ogTitle  string
	siteName string
	byline   string
	license  string
}

// collectHTMLMetadata walks the whole tree (head included) for the
// cheap, reliable metadata signals.
func collectHTMLMetadata(root *html.Node) htmlMetadata {
	var m htmlMetadata
	walkHTML(root, func(n *html.Node) bool {
		if n.Type != html.ElementNode {
			return true
		}
		switch n.Data {
		case "title":
			if m.title == "" {
				m.title = collapseWhitespace(textContent(n))
			}
		case "meta":
			name := strings.ToLower(attr(n, "name"))
			prop := strings.ToLower(attr(n, "property"))
			content := attr(n, "content")
			switch {
			case name == "author" || prop == "article:author":
				if m.byline == "" {
					m.byline = collapseWhitespace(content)
				}
			case name == "license" || name == "copyright":
				if m.license == "" {
					m.license = collapseWhitespace(content)
				}
			case prop == "og:title" || name == "og:title" || name == "twitter:title":
				if m.ogTitle == "" {
					m.ogTitle = collapseWhitespace(content)
				}
			case prop == "og:site_name" || name == "og:site_name":
				if m.siteName == "" {
					m.siteName = collapseWhitespace(content)
				}
			}
		case "link":
			rel := strings.ToLower(attr(n, "rel"))
			if m.license == "" && strings.Contains(rel, "license") {
				m.license = collapseWhitespace(attr(n, "href"))
			}
		}
		return true
	})
	return m
}

// findMainRegion returns the most specific content container, or nil.
//
// Preference order: <main>, <article>, then an element whose id/class
// mentions a known content container. The first match by document order
// wins so the result is deterministic.
func findMainRegion(root *html.Node) *html.Node {
	if n := firstElement(root, "main"); n != nil {
		return n
	}
	if n := firstElement(root, "article"); n != nil {
		return n
	}
	var found *html.Node
	walkHTML(root, func(n *html.Node) bool {
		if found != nil || n.Type != html.ElementNode {
			return found == nil
		}
		tag := strings.ToLower(n.Data)
		if tag == "body" || tag == "html" {
			return true
		}
		identity := strings.ToLower(attr(n, "id") + " " + attr(n, "class") + " " + attr(n, "role"))
		if attr(n, "role") == "main" {
			found = n
			return false
		}
		for _, hint := range htmlMainHints {
			if strings.Contains(identity, hint) {
				found = n
				return false
			}
		}
		return true
	})
	return found
}

func findBody(root *html.Node) *html.Node {
	if n := firstElement(root, "body"); n != nil {
		return n
	}
	return root
}

// pruneChrome removes non-content subtrees from the region and returns
// how many subtrees were dropped.
func pruneChrome(root *html.Node) int {
	dropped := 0
	var prune func(n *html.Node)
	prune = func(n *html.Node) {
		for child := n.FirstChild; child != nil; {
			next := child.NextSibling
			if child.Type == html.ElementNode {
				if shouldDropElement(child) {
					n.RemoveChild(child)
					dropped++
				} else {
					prune(child)
				}
			}
			child = next
		}
	}
	prune(root)
	return dropped
}

func shouldDropElement(n *html.Node) bool {
	tag := strings.ToLower(n.Data)
	if htmlDropTags[tag] {
		return true
	}
	// A <header> inside the content region is usually a page header, but
	// <header> inside an <article> often holds the title. Only drop it
	// when it looks like chrome.
	if tag == "header" {
		identity := strings.ToLower(attr(n, "id") + " " + attr(n, "class"))
		if identity == "" || identity == " " {
			return false
		}
		for _, hint := range htmlChromeHints {
			if strings.Contains(identity, hint) {
				return true
			}
		}
		return false
	}
	if attr(n, "aria-hidden") == "true" {
		return true
	}
	if strings.Contains(strings.ToLower(attr(n, "role")), "navigation") {
		return true
	}
	identity := strings.ToLower(attr(n, "id") + " " + attr(n, "class"))
	if identity == " " || identity == "" {
		return false
	}
	for _, hint := range htmlChromeHints {
		if strings.Contains(identity, hint) {
			return true
		}
	}
	return false
}

// renderNode writes a node's content as Markdown-flavoured text.
func renderNode(b *strings.Builder, n *html.Node, base *url.URL, listDepth int) {
	switch n.Type {
	case html.TextNode:
		text := n.Data
		if strings.TrimSpace(text) == "" {
			return
		}
		b.WriteString(collapseInlineWhitespace(text))
		return
	case html.DocumentNode:
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			renderNode(b, c, base, listDepth)
		}
		return
	case html.ElementNode:
	default:
		return
	}

	tag := strings.ToLower(n.Data)

	// Callout detection runs first: a warning is usually a <div>, and the
	// generic block case would consume it as ordinary prose and lose the
	// fact that it is a warning. Headings/pre are excluded because their
	// class names can also say "note" (e.g. "note-heading").
	if tag != "h1" && tag != "h2" && tag != "h3" && tag != "h4" && tag != "h5" &&
		tag != "h6" && tag != "pre" && tag != "code" {
		if kind := calloutKind(n); kind != "" {
			text := strings.TrimSpace(normaliseMarkdown(renderChildren(n, base, listDepth)))
			if text == "" {
				return
			}
			ensureBlankLine(b)
			lines := strings.Split(text, "\n")
			b.WriteString("> **" + kind + ":** " + lines[0] + "\n")
			for _, line := range lines[1:] {
				b.WriteString("> " + line + "\n")
			}
			b.WriteString("\n")
			return
		}
	}

	switch tag {
	case "h1", "h2", "h3", "h4", "h5", "h6":
		level := int(tag[1] - '0')
		text := collapseWhitespace(textContent(n))
		if text == "" {
			return
		}
		ensureBlankLine(b)
		b.WriteString(strings.Repeat("#", level) + " " + text)
		b.WriteString("\n\n")
		return
	case "pre":
		lang := codeLanguage(n)
		ensureBlankLine(b)
		b.WriteString("```" + lang + "\n")
		b.WriteString(strings.TrimRight(rawTextContent(n), "\n"))
		b.WriteString("\n```\n\n")
		return
	case "code", "kbd", "samp", "tt":
		text := rawTextContent(n)
		if strings.Contains(text, "\n") {
			b.WriteString("`" + strings.ReplaceAll(text, "`", "'") + "`")
			return
		}
		b.WriteString("`" + text + "`")
		return
	case "br":
		b.WriteString("\n")
		return
	case "hr":
		ensureBlankLine(b)
		b.WriteString("---\n\n")
		return
	case "p", "div", "section", "article", "main", "figure", "details", "summary", "dl", "dd", "dt":
		ensureBlankLine(b)
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			renderNode(b, c, base, listDepth)
		}
		trimTrailingSpaces(b)
		b.WriteString("\n\n")
		return
	case "ul", "ol":
		ensureBlankLine(b)
		index := 0
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			if c.Type != html.ElementNode || strings.ToLower(c.Data) != "li" {
				continue
			}
			index++
			marker := "- "
			if tag == "ol" {
				marker = fmt.Sprintf("%d. ", index)
			}
			b.WriteString(strings.Repeat("  ", listDepth) + marker)
			renderListItem(b, c, base, listDepth+1)
			b.WriteString("\n")
		}
		b.WriteString("\n")
		return
	case "li":
		renderListItem(b, n, base, listDepth+1)
		b.WriteString("\n")
		return
	case "table":
		renderTable(b, n, base)
		return
	case "blockquote":
		var inner strings.Builder
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			renderNode(&inner, c, base, listDepth)
		}
		text := strings.TrimSpace(normaliseMarkdown(inner.String()))
		if text == "" {
			return
		}
		ensureBlankLine(b)
		for _, line := range strings.Split(text, "\n") {
			b.WriteString("> " + line + "\n")
		}
		b.WriteString("\n")
		return
	case "a":
		href := attr(n, "href")
		text := collapseWhitespace(textContent(n))
		if text == "" {
			return
		}
		abs := absoluteLink(href, base)
		if abs == "" || abs == text || strings.HasPrefix(href, "#") {
			b.WriteString(text)
			return
		}
		b.WriteString("[" + text + "](" + abs + ")")
		return
	case "img":
		alt := collapseWhitespace(attr(n, "alt"))
		if alt == "" {
			return
		}
		b.WriteString("![" + alt + "]")
		return
	case "strong", "b":
		text := collapseWhitespace(textContent(n))
		if text == "" {
			return
		}
		b.WriteString("**" + text + "**")
		return
	case "em", "i":
		text := collapseWhitespace(textContent(n))
		if text == "" {
			return
		}
		b.WriteString("*" + text + "*")
		return
	}

	for c := n.FirstChild; c != nil; c = c.NextSibling {
		renderNode(b, c, base, listDepth)
	}
}

// calloutKind classifies an element as a warning/note/tip callout, or
// returns "".
func calloutKind(n *html.Node) string {
	if n.Type != html.ElementNode {
		return ""
	}
	identity := strings.ToLower(attr(n, "class") + " " + attr(n, "id") + " " + attr(n, "role"))
	switch {
	case strings.Contains(identity, "warning") || strings.Contains(identity, "danger") || strings.Contains(identity, "caution"):
		return "Warning"
	case strings.Contains(identity, "important"):
		return "Important"
	case strings.Contains(identity, "note") || strings.Contains(identity, "callout"):
		return "Note"
	case strings.Contains(identity, "tip") || strings.Contains(identity, "hint"):
		return "Tip"
	}
	return ""
}

func renderChildren(n *html.Node, base *url.URL, listDepth int) string {
	var b strings.Builder
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		renderNode(&b, c, base, listDepth)
	}
	return b.String()
}

// renderListItem renders the inline content of a list item, recursing
// into nested lists with a deeper marker.
func renderListItem(b *strings.Builder, li *html.Node, base *url.URL, depth int) {
	first := true
	for c := li.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.ElementNode {
			tag := strings.ToLower(c.Data)
			if tag == "ul" || tag == "ol" {
				b.WriteString("\n")
				renderNode(b, c, base, depth)
				first = false
				continue
			}
			if tag == "p" {
				if !first {
					b.WriteString(" ")
				}
				b.WriteString(strings.TrimSpace(renderChildren(c, base, depth)))
				first = false
				continue
			}
		}
		if c.Type == html.TextNode && strings.TrimSpace(c.Data) == "" {
			continue
		}
		before := b.Len()
		renderNode(b, c, base, depth)
		if b.Len() > before {
			first = false
		}
	}
	trimTrailingSpaces(b)
}

// renderTable renders a table as a Markdown table when it has a header
// row, or as labelled rows otherwise. Tables in documentation frequently
// carry parameter lists, so dropping them loses content.
func renderTable(b *strings.Builder, table *html.Node, base *url.URL) {
	var rows [][]string
	var header []string
	walkHTML(table, func(n *html.Node) bool {
		if n.Type != html.ElementNode {
			return true
		}
		if strings.ToLower(n.Data) == "tr" {
			var cells []string
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				if c.Type != html.ElementNode {
					continue
				}
				tag := strings.ToLower(c.Data)
				if tag != "td" && tag != "th" {
					continue
				}
				cells = append(cells, escapeTableCell(flattenTableCell(renderChildren(c, base, 0))))
			}
			if len(cells) == 0 {
				return true
			}
			if header == nil && hasHeaderCells(n) {
				header = cells
				return true
			}
			rows = append(rows, cells)
			return true
		}
		return true
	})
	if len(rows) == 0 && len(header) == 0 {
		return
	}
	ensureBlankLine(b)
	if header != nil {
		b.WriteString("| " + strings.Join(header, " | ") + " |\n")
		seps := make([]string, len(header))
		for i := range seps {
			seps[i] = "---"
		}
		b.WriteString("| " + strings.Join(seps, " | ") + " |\n")
	}
	for _, row := range rows {
		b.WriteString("| " + strings.Join(row, " | ") + " |\n")
	}
	b.WriteString("\n")
}

func hasHeaderCells(tr *html.Node) bool {
	for c := tr.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.ElementNode && strings.ToLower(c.Data) == "th" {
			return true
		}
	}
	return false
}

func escapeTableCell(s string) string {
	return strings.ReplaceAll(strings.TrimSpace(s), "|", "\\|")
}

// flattenTableCell turns rendered cell Markdown into one line. Inline
// code and links survive; block structure (lists, fences) is flattened
// because a Markdown table cell cannot hold it.
func flattenTableCell(s string) string {
	lines := strings.Split(s, "\n")
	var parts []string
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line != "" {
			parts = append(parts, line)
		}
	}
	return strings.Join(parts, " ")
}

// codeLanguage derives a fenced-code language hint from the usual class
// conventions: `language-x`, `lang-x`, `brush: x`, `highlight-source-x`,
// or a data-lang attribute.
func codeLanguage(pre *html.Node) string {
	candidates := []string{attr(pre, "data-lang"), attr(pre, "data-language")}
	if code := firstElement(pre, "code"); code != nil {
		candidates = append(candidates, attr(code, "class"), attr(code, "data-lang"), attr(code, "data-language"))
	}
	candidates = append(candidates, attr(pre, "class"))
	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}
		for _, field := range strings.Fields(candidate) {
			lower := strings.ToLower(field)
			for _, prefix := range []string{"language-", "lang-", "brush:", "highlight-source-", "highlight-"} {
				if strings.HasPrefix(lower, prefix) {
					lang := strings.TrimPrefix(lower, prefix)
					lang = strings.Trim(lang, " :;")
					if lang != "" && isPlausibleLanguage(lang) {
						return lang
					}
				}
			}
		}
	}
	return ""
}

// isPlausibleLanguage rejects class fragments that are styling, not
// language identifiers.
func isPlausibleLanguage(s string) bool {
	if len(s) == 0 || len(s) > 24 {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9') && r != '+' && r != '#' && r != '-' {
			return false
		}
	}
	switch s {
	case "highlight", "hljs", "code", "pre", "sourcecode", "line", "lines":
		return false
	}
	return true
}

func absoluteLink(href string, base *url.URL) string {
	href = strings.TrimSpace(href)
	if href == "" || strings.HasPrefix(href, "#") {
		return ""
	}
	u, err := url.Parse(href)
	if err != nil {
		return ""
	}
	if u.Scheme != "" && u.Scheme != "http" && u.Scheme != "https" {
		return "" // mailto:, javascript:, data: — not citations
	}
	if base == nil {
		return ""
	}
	return base.ResolveReference(u).String()
}

// walkHTML visits nodes depth-first; returning false skips children.
func walkHTML(n *html.Node, visit func(*html.Node) bool) {
	if n == nil {
		return
	}
	if !visit(n) {
		return
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		walkHTML(c, visit)
	}
}

func firstElement(root *html.Node, tag string) *html.Node {
	var found *html.Node
	walkHTML(root, func(n *html.Node) bool {
		if found != nil {
			return false
		}
		if n.Type == html.ElementNode && strings.ToLower(n.Data) == tag {
			found = n
			return false
		}
		return true
	})
	return found
}

func firstHeadingText(root *html.Node) string {
	for _, tag := range []string{"h1", "h2"} {
		if n := firstElement(root, tag); n != nil {
			if text := collapseWhitespace(textContent(n)); text != "" {
				return text
			}
		}
	}
	return ""
}

func attr(n *html.Node, name string) string {
	for _, a := range n.Attr {
		if strings.EqualFold(a.Key, name) {
			return a.Val
		}
	}
	return ""
}

// textContent concatenates descendant text with a space separator so
// inline elements do not fuse their words.
func textContent(n *html.Node) string {
	var b strings.Builder
	walkHTML(n, func(c *html.Node) bool {
		if c.Type == html.TextNode {
			b.WriteString(c.Data)
			return true
		}
		if c.Type == html.ElementNode && (strings.ToLower(c.Data) == "br" || strings.ToLower(c.Data) == "p") {
			b.WriteString(" ")
		}
		return true
	})
	return b.String()
}

// rawTextContent returns descendant text verbatim (for <pre>).
func rawTextContent(n *html.Node) string {
	var b strings.Builder
	walkHTML(n, func(c *html.Node) bool {
		if c.Type == html.TextNode {
			b.WriteString(c.Data)
		}
		return true
	})
	return b.String()
}

func collapseWhitespace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// collapseInlineWhitespace keeps single newlines out of inline text but
// preserves explicit spacing.
func collapseInlineWhitespace(s string) string {
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return ""
	}
	out := strings.Join(fields, " ")
	// Boundaries matter: `before <code>x</code> has` is three tokens, and
	// dropping the surrounding spaces fuses them into
	// `before`x`has`.
	if isHTMLSpace(s[0]) {
		out = " " + out
	}
	if isHTMLSpace(s[len(s)-1]) {
		out += " "
	}
	return out
}

func isHTMLSpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r' || b == '\f'
}

func ensureBlankLine(b *strings.Builder) {
	s := b.String()
	if s == "" {
		return
	}
	if strings.HasSuffix(s, "\n\n") {
		return
	}
	if strings.HasSuffix(s, "\n") {
		b.WriteString("\n")
		return
	}
	b.WriteString("\n\n")
}

func trimTrailingSpaces(b *strings.Builder) {
	s := b.String()
	trimmed := strings.TrimRight(s, " \t")
	if len(trimmed) != len(s) {
		b.Reset()
		b.WriteString(trimmed)
	}
}

// normaliseMarkdown collapses runs of blank lines and stray trailing
// spaces so chunk boundaries fall on real content.
func normaliseMarkdown(s string) string {
	lines := strings.Split(s, "\n")
	var out []string
	blank := 0
	for _, line := range lines {
		line = strings.TrimRight(line, " \t")
		if strings.TrimSpace(line) == "" {
			blank++
			if blank > 1 {
				continue
			}
			out = append(out, "")
			continue
		}
		blank = 0
		out = append(out, line)
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}
