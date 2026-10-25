// reference_html_test.go — pins the structured HTML extraction contract.
//
// The pre-fix path (StripHTML) concatenated every text node: a function
// signature became a paragraph, a `php` code sample lost its language and
// its line breaks, a "Warning" callout became ordinary prose, and the
// site navigation and cookie banner ended up in the searchable chunk set.
package internal

import (
	"net/url"
	"strings"
	"testing"
)

const exampleDocHTML = `<!doctype html>
<html lang="en">
<head>
  <title>wp_nav_menu() – WordPress Developer Resources</title>
  <meta property="og:site_name" content="WordPress Developer Resources">
  <meta name="author" content="WordPress Contributors">
  <link rel="license" href="https://creativecommons.org/licenses/by-sa/4.0/">
</head>
<body>
  <div class="cookie-banner">We use cookies to improve your experience. Accept all.</div>
  <nav class="site-nav"><a href="/">Home</a><a href="/reference/">Code Reference</a></nav>
  <main id="main-content">
    <h1>wp_nav_menu()</h1>
    <p>Displays a navigation menu.</p>
    <h2>Parameters</h2>
    <table>
      <tr><th>Parameter</th><th>Description</th></tr>
      <tr><td><code>$args</code></td><td>Array of arguments.</td></tr>
    </table>
    <h3>Example</h3>
    <pre class="language-php"><code>wp_nav_menu( array(
    'theme_location' =&gt; 'primary',
    'depth'          =&gt; 2,
) );</code></pre>
    <div class="warning callout">Calling this before <code>after_setup_theme</code> has no effect.</div>
    <p>See also <a href="/reference/functions/register_nav_menus/">register_nav_menus()</a>.</p>
  </main>
  <aside class="sidebar">Related functions</aside>
  <footer>© WordPress Foundation</footer>
</body>
</html>`

// TestExtractHTMLDocument_PreservesStructureAndDropsChrome is the
// primary extraction contract.
func TestExtractHTMLDocument_PreservesStructureAndDropsChrome(t *testing.T) {
	base, _ := url.Parse("https://developer.wordpress.org/reference/functions/wp_nav_menu/")
	doc, err := ExtractHTMLDocument(strings.NewReader(exampleDocHTML), base)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}

	if doc.Title != "wp_nav_menu()" {
		t.Errorf("title = %q, want the page's <h1> %q", doc.Title, "wp_nav_menu()")
	}

	for _, want := range []string{
		"# wp_nav_menu()",
		"## Parameters",
		"### Example",
		"```php",
		"'theme_location' => 'primary'",
		"| Parameter | Description |",
		"`$args`",
		"**Warning:**",
		"after_setup_theme",
		"https://developer.wordpress.org/reference/functions/register_nav_menus/",
	} {
		if !strings.Contains(doc.Body, want) {
			t.Errorf("extracted body is missing %q\n---\n%s", want, doc.Body)
		}
	}

	for _, unwanted := range []string{
		"We use cookies",
		"Related functions",
		"WordPress Foundation",
		"Code Reference",
	} {
		if strings.Contains(doc.Body, unwanted) {
			t.Errorf("extracted body still contains chrome %q\n---\n%s", unwanted, doc.Body)
		}
	}

	if doc.SiteName != "WordPress Developer Resources" {
		t.Errorf("site name = %q", doc.SiteName)
	}
	if doc.Byline != "WordPress Contributors" {
		t.Errorf("byline = %q", doc.Byline)
	}
	if !strings.Contains(doc.License, "creativecommons.org") {
		t.Errorf("licence = %q, want the rel=license target", doc.License)
	}
	if len(doc.Notes) == 0 {
		t.Error("extraction reported no notes; the region choice and dropped subtrees should be visible")
	}
}

// TestExtractHTMLDocument_NoMainRegionIsHonest pins that a page without
// a content region still extracts, and says it had to guess.
func TestExtractHTMLDocument_NoMainRegionIsHonest(t *testing.T) {
	html := `<html><head><title>Plain page</title></head><body>
	<h1>Plain page</h1><p>Body text without a main element.</p></body></html>`
	doc, err := ExtractHTMLDocument(strings.NewReader(html), nil)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if !strings.Contains(doc.Body, "Body text without a main element.") {
		t.Errorf("body lost main text: %q", doc.Body)
	}
	found := false
	for _, n := range doc.Notes {
		if strings.Contains(n, "no <main>") {
			found = true
		}
	}
	if !found {
		t.Errorf("extraction did not disclose that it guessed the region: %v", doc.Notes)
	}
}

// TestExtractHTMLDocument_RelativeLinksNeedABase pins that a relative
// href is not presented as an absolute citation when no base is known.
func TestExtractHTMLDocument_RelativeLinksNeedABase(t *testing.T) {
	html := `<html><body><main><p>See <a href="/docs/x">the docs</a>.</p></main></body></html>`
	doc, err := ExtractHTMLDocument(strings.NewReader(html), nil)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if strings.Contains(doc.Body, "](/docs/x)") {
		t.Errorf("relative href kept without a base URL: %q", doc.Body)
	}
	if !strings.Contains(doc.Body, "the docs") {
		t.Errorf("link text was dropped: %q", doc.Body)
	}
}

// TestStripFrontMatter_RemovesMetadataNotProse pins the two branches of
// the front-matter rule.
func TestStripFrontMatter_RemovesMetadataNotProse(t *testing.T) {
	big := "---\ntitle: Nav Menus\n" + strings.Repeat("description: generated filler line\n", 60) + "---\n\n# Nav Menus\n\nReal content.\n"
	body, notes := stripFrontMatter(big)
	if strings.Contains(body, "description: generated filler line") {
		t.Error("a large front-matter block was left in the chunk content")
	}
	if !strings.Contains(body, "Real content.") {
		t.Error("content after the front-matter block was lost")
	}
	if len(notes) == 0 {
		t.Error("stripping front matter was not reported")
	}

	prose := "---\n\nThis is just a horizontal rule and some prose.\n\n---\n\nMore prose.\n"
	kept, _ := stripFrontMatter(prose)
	if !strings.Contains(kept, "This is just a horizontal rule") {
		t.Error("prose between horizontal rules was deleted as front matter")
	}
}
