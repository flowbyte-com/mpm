// reference_identity.go — what makes two imports "the same document".
//
// The reference library used to key identity on the caller-supplied
// source string. That is not an identity: the same file reached through
// a bare name, a relative path, a `./`-prefixed path, a `..`-containing
// path and an absolute path produced four different `file_path` values,
// so four `reference_docs` rows, four chunk sets and four embedding
// passes for one document. Retrieval then competed against its own
// duplicates.
//
// The identity model here is explicit and small:
//
//	file → canonical absolute path with symlinks resolved
//	url  → canonical URL (scheme/host normalized, fragment dropped)
//
// Two spellings that resolve to the same canonical value ARE the same
// document. Two different canonical values are NOT, even when their
// bytes happen to match: two publishers may legitimately publish
// identical text, and merging them by content hash would silently
// conflate their provenance, annotations and retrieval history.
//
// Content hashes are therefore never used to decide identity. They
// decide only whether an already-identified document changed.
//
// Legacy rows may hold non-canonical `file_path` values from before
// this model existed. They are recognised (not rewritten in bulk) by
// the alias rules in resolveReferenceIdentity, so a re-import of the
// same file lands on the legacy row instead of creating another copy.
// Merging *existing* duplicates is deliberately left to the operator:
// `mpm reference duplicates` reports them and prints a repair plan.
package internal

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Reference source kinds. A row with an unrecognised/empty kind is
// treated as a file (the only kind that existed before URL ingestion).
const (
	ReferenceSourceFile = "file"
	ReferenceSourceURL  = "url"
)

// SourceIdentity is the canonical identity of a reference source.
type SourceIdentity struct {
	// Kind is ReferenceSourceFile or ReferenceSourceURL.
	Kind string
	// Canonical is the identity used for storage and lookup: an
	// absolute, symlink-resolved path for files; a normalized URL for
	// URLs. This is also what gets written to reference_docs.file_path,
	// so every existing read path keeps working unchanged.
	Canonical string
	// Raw is the string the caller supplied, preserved for diagnostics
	// only. It is never used for identity.
	Raw string
}

// String renders the identity for error messages and diagnostics.
func (s SourceIdentity) String() string {
	if s.Kind == "" {
		return s.Canonical
	}
	return s.Kind + ":" + s.Canonical
}

// CanonicalizeLocalPath resolves a caller-supplied path to its identity.
//
// The transformation is: make absolute (relative to the process working
// directory — the caller's cwd is meaningful only at this moment), clean
// `.`/`..` segments, then resolve symlinks. Symlinks are resolved so that
// `/docs/x.md` and `/docs-link/x.md` naming the same file are one
// document; the alternative (treating a symlink as a distinct source)
// creates exactly the duplicate class this function exists to prevent.
//
// If the file does not exist, or cannot be resolved, the cleaned
// absolute path is returned instead of an error: the caller's stat/open
// will produce the authoritative error, and identity resolution must not
// turn a missing file into a misleading identity failure. The
// directories that do exist are still resolved, so a symlinked parent
// directory canonicalizes even when the leaf is absent.
func CanonicalizeLocalPath(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", fmt.Errorf("empty source path")
	}
	abs, err := filepath.Abs(trimmed)
	if err != nil {
		return "", fmt.Errorf("resolve %q: %w", raw, err)
	}
	abs = filepath.Clean(abs)

	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return filepath.Clean(resolved), nil
	}
	// Leaf missing (or unreadable): resolve the parent chain and rejoin
	// so a symlinked directory still canonicalizes.
	if dir, err := filepath.EvalSymlinks(filepath.Dir(abs)); err == nil {
		return filepath.Join(dir, filepath.Base(abs)), nil
	}
	return abs, nil
}

// CanonicalizeReferenceURL normalizes an absolute http(s) URL for use as
// a document identity.
//
// What is normalized:
//   - scheme and host are lowercased (URLs are case-insensitive there);
//   - the default port for the scheme is dropped;
//   - the fragment is dropped (it never reaches the server);
//   - empty path becomes "/".
//
// What is deliberately NOT normalized:
//   - the query string, including parameter order. `?output_format=md`
//     and the bare page are different renditions, and reordering query
//     parameters is not provably meaning-preserving for every server.
//   - trailing slashes. `/guide` and `/guide/` may serve different
//     documents, and guessing wrong merges two resources.
//
// Credentials in the URL are rejected rather than stripped: a URL that
// carries a password must not be stored, logged or forwarded.
func CanonicalizeReferenceURL(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", fmt.Errorf("empty URL")
	}
	u, err := url.Parse(trimmed)
	if err != nil {
		return "", fmt.Errorf("parse URL %q: %w", raw, err)
	}
	if u.User != nil {
		return "", fmt.Errorf("URL must not embed credentials: %q", redactURL(raw))
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", fmt.Errorf("unsupported URL scheme %q (only http and https are fetched)", u.Scheme)
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return "", fmt.Errorf("URL has no host: %q", redactURL(raw))
	}
	port := u.Port()
	if (scheme == "http" && port == "80") || (scheme == "https" && port == "443") {
		port = ""
	}
	u.Scheme = scheme
	u.Host = host
	if port != "" {
		u.Host = host + ":" + port
	}
	u.Fragment = ""
	u.RawFragment = ""
	u.User = nil
	if u.Path == "" {
		u.Path = "/"
	}
	return u.String(), nil
}

// redactURL removes any userinfo before a URL is echoed in an error.
func redactURL(raw string) string {
	if u, err := url.Parse(strings.TrimSpace(raw)); err == nil {
		if u.User != nil {
			u.User = url.User("REDACTED")
		}
		return u.String()
	}
	return "<unparseable URL>"
}

// IsURLSource reports whether the caller-supplied source string names an
// HTTP(S) resource rather than a local path. Only an explicit
// scheme-prefixed string counts: a local file may legitimately be named
// `http:foo.md`, and treating it as a URL would be a silent surprise.
func IsURLSource(source string) bool {
	s := strings.ToLower(strings.TrimSpace(source))
	return strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://")
}

// ResolveReferenceIdentity classifies a source string and returns its
// canonical identity. Exactly one of local path / URL is produced.
func ResolveReferenceIdentity(source string) (SourceIdentity, error) {
	if IsURLSource(source) {
		canonical, err := CanonicalizeReferenceURL(source)
		if err != nil {
			return SourceIdentity{}, err
		}
		return SourceIdentity{Kind: ReferenceSourceURL, Canonical: canonical, Raw: source}, nil
	}
	canonical, err := CanonicalizeLocalPath(source)
	if err != nil {
		return SourceIdentity{}, err
	}
	return SourceIdentity{Kind: ReferenceSourceFile, Canonical: canonical, Raw: source}, nil
}

// ReferenceDocIDForIdentity returns the deterministic document id for a
// canonical source identity.
//
// Determinism is the concurrency story. Two processes importing the same
// new file at the same moment cannot see each other's uncommitted row,
// so a "look up then insert" pair always has a window in which both
// allocate different ids and both insert. Deriving the id from the
// identity closes that window without a lock: the second writer targets
// the same primary key and its write is an update of the same document.
//
// The id is stable across processes, machines and re-imports, which is
// also what "stable document IDs across unchanged re-imports" requires.
func ReferenceDocIDForIdentity(id SourceIdentity) string {
	sum := sha256.Sum256([]byte(id.Kind + "\x00" + id.Canonical))
	return hex.EncodeToString(sum[:])[:16]
}

// ReferenceIdentityMatch is the result of looking a canonical identity up
// in the reference library.
type ReferenceIdentityMatch struct {
	// Doc is the single matched document, or nil when none matched.
	Doc *ReferenceDoc
	// AmbiguousIDs lists more than one existing document that resolves to
	// the same identity. When this is non-empty Doc is nil and the caller
	// must NOT create another document: the honest answer is "several
	// copies already exist", plus the ids so the operator can decide.
	AmbiguousIDs []string
	// MatchedVia describes how the match was found ("canonical",
	// "legacy-relative", "legacy-absolute"). Diagnostic only.
	MatchedVia string
	// AliasPath is the stored file_path of the matched row when it is not
	// already canonical. The write path upgrades it so the next import
	// takes the fast canonical path.
	AliasPath string
}

// resolveReferenceIdentity finds the existing document for a canonical
// identity, including rows written before canonicalization existed.
//
// Lookup order:
//  1. exact match on the canonical value (the steady state);
//  2. exact match on the caller's raw string (covers a row written by an
//     older binary from the same working directory);
//  3. legacy alias resolution: a stored relative path is joined against
//     each ancestor directory of the canonical target. A legacy row
//     `docs/guide.md` therefore matches `/corpus/docs/guide.md` when
//     `/corpus` is an ancestor, which is exactly the duplication pattern
//     observed in practice.
//
// Step 3 never uses content hashes. Two documents with identical bytes
// but different canonical paths stay separate.
//
// If more than one row matches, the result is ambiguous: the caller
// reports it instead of picking one, because picking would either hide a
// duplicate or silently rewrite one document's provenance onto another's.
func resolveReferenceIdentity(db *sql.DB, id SourceIdentity) (*ReferenceIdentityMatch, error) {
	if db == nil {
		return nil, fmt.Errorf("resolveReferenceIdentity: database not initialized")
	}
	if id.Canonical == "" {
		return nil, fmt.Errorf("resolveReferenceIdentity: empty identity")
	}

	if doc, err := FindReferenceBySourcePath(db, id.Canonical); err != nil {
		return nil, err
	} else if doc != nil {
		return &ReferenceIdentityMatch{Doc: doc, MatchedVia: "canonical"}, nil
	}

	if id.Raw != "" && id.Raw != id.Canonical {
		if doc, err := FindReferenceBySourcePath(db, id.Raw); err != nil {
			return nil, err
		} else if doc != nil {
			return &ReferenceIdentityMatch{
				Doc:        doc,
				MatchedVia: "raw",
				AliasPath:  doc.SourcePath,
			}, nil
		}
	}

	rows, err := db.Query(`SELECT id, file_path FROM reference_docs`)
	if err != nil {
		return nil, fmt.Errorf("resolveReferenceIdentity: scan sources: %w", err)
	}
	defer rows.Close()

	var (
		aliasIDs   []string
		aliasPath  string
		aliasFound string
	)
	for rows.Next() {
		var (
			docID   string
			rawPath sql.NullString
		)
		if err := rows.Scan(&docID, &rawPath); err != nil {
			return nil, fmt.Errorf("resolveReferenceIdentity: scan row: %w", err)
		}
		if !rawPath.Valid || strings.TrimSpace(rawPath.String) == "" {
			continue
		}
		stored := rawPath.String
		if stored == id.Canonical {
			continue // already handled above; a second row this exact is an ambiguous dup below
		}
		if !legacySourceResolvesTo(id, stored) {
			continue
		}
		aliasIDs = append(aliasIDs, docID)
		if aliasPath == "" {
			aliasPath = stored
			aliasFound = docID
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("resolveReferenceIdentity: rows: %w", err)
	}

	switch len(aliasIDs) {
	case 0:
		return &ReferenceIdentityMatch{}, nil
	case 1:
		doc, err := FindReferenceBySourcePath(db, aliasPath)
		if err != nil {
			return nil, err
		}
		if doc == nil {
			// Row vanished between the two reads (concurrent shred).
			return &ReferenceIdentityMatch{}, nil
		}
		return &ReferenceIdentityMatch{
			Doc:        doc,
			MatchedVia: "legacy-alias",
			AliasPath:  aliasFound,
		}, nil
	default:
		sort.Strings(aliasIDs)
		return &ReferenceIdentityMatch{AmbiguousIDs: aliasIDs, MatchedVia: "legacy-alias"}, nil
	}
}

// legacySourceResolvesTo reports whether a stored (possibly
// non-canonical) source value denotes the same resource as the canonical
// identity.
//
// URLs compare canonically. Absolute filesystem paths compare canonically.
// A relative stored path is resolved against each ancestor directory of
// the canonical target, which is the only resolution that can be
// justified: the row was written relative to *some* working directory,
// and if that directory is an ancestor of the file we are importing, this
// is the same file.
func legacySourceResolvesTo(id SourceIdentity, stored string) bool {
	if id.Kind == ReferenceSourceURL {
		canonical, err := CanonicalizeReferenceURL(stored)
		return err == nil && canonical == id.Canonical
	}
	if filepath.IsAbs(stored) {
		canonical, err := CanonicalizeLocalPath(stored)
		return err == nil && canonical == id.Canonical
	}
	if strings.Contains(stored, "://") {
		return false // a URL stored on a file-identity lookup is a different kind
	}
	if filepath.Base(stored) != filepath.Base(id.Canonical) {
		// Cheap rejection first: ancestor-joining cannot change the leaf.
		return false
	}
	dir := filepath.Dir(id.Canonical)
	for {
		candidate := filepath.Join(dir, stored)
		if canonical, err := CanonicalizeLocalPath(candidate); err == nil && canonical == id.Canonical {
			return true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return false
		}
		dir = parent
	}
}

// ReferenceDuplicateGroup is one set of documents that appear to be the
// same source.
type ReferenceDuplicateGroup struct {
	// Identity is the most specific canonical value in the group.
	Identity string
	// Kind is "file" or "url".
	Kind string
	// Confidence is "certain" when every member resolves to exactly the
	// same canonical identity, and "probable" when legacy relative paths
	// were matched by path suffix (a relative path cannot be resolved
	// after the fact — its working directory is not recorded).
	Confidence string
	// Docs carries every member, newest first as returned by the query.
	Docs []ReferenceDuplicateDoc
	// Note explains a non-certain grouping.
	Note string `json:",omitempty"`
}

// ReferenceDuplicateDoc is one member of a duplicate group.
type ReferenceDuplicateDoc struct {
	ID           string
	Title        string
	SourcePath   string
	TotalChunks  int
	LastIndexed  string
	ImportReason string
	// Preferred marks the member with the most chunks (ties: the newest
	// last_indexed). Reported only as a suggestion; never applied.
	Preferred bool
}

// referenceSourceEntry is one row as seen by the duplicate scan.
type referenceSourceEntry struct {
	doc      ReferenceDuplicateDoc
	kind     string
	relative bool   // the stored path is not absolute
	key      string // canonical value when resolvable
}

// FindReferenceDuplicates reports documents that appear to be the same
// source. Read-only: nothing is merged or deleted.
//
// This exists because the duplicate class was created by an older binary.
// Automatically reconciling those rows would risk losing distinct
// provenance, annotations or retrieval history — a document imported
// twice may have been annotated differently each time, and only the
// operator knows which one is canonical. The diagnostic is therefore a
// report plus a plan, not a migration.
//
// Two grouping rules, with different confidence:
//
//   - certain: absolute paths canonicalize to the same file, or URLs
//     canonicalize to the same resource.
//   - probable: a legacy relative path is a component-suffix of another
//     member's path (`docs/menus.md` ends with `menus.md`). A relative
//     path's original working directory was never recorded, so this is
//     reported as probable rather than asserted.
func FindReferenceDuplicates(db *sql.DB) ([]ReferenceDuplicateGroup, error) {
	if db == nil {
		return nil, fmt.Errorf("FindReferenceDuplicates: database not initialized")
	}
	rows, err := db.Query(`
		SELECT id, title, COALESCE(file_path,''), COALESCE(total_chunks,0),
		       COALESCE(last_indexed,''), COALESCE(import_reason,'')
		FROM reference_docs
		ORDER BY created_at DESC, id DESC
	`)
	if err != nil {
		return nil, fmt.Errorf("FindReferenceDuplicates: query: %w", err)
	}
	defer rows.Close()

	var entries []referenceSourceEntry
	for rows.Next() {
		var e referenceSourceEntry
		if err := rows.Scan(&e.doc.ID, &e.doc.Title, &e.doc.SourcePath,
			&e.doc.TotalChunks, &e.doc.LastIndexed, &e.doc.ImportReason); err != nil {
			return nil, fmt.Errorf("FindReferenceDuplicates: scan: %w", err)
		}
		e.kind = ReferenceSourceFile
		switch {
		case e.doc.SourcePath == "":
			e.key = ""
		case IsURLSource(e.doc.SourcePath):
			e.kind = ReferenceSourceURL
			if c, err := CanonicalizeReferenceURL(e.doc.SourcePath); err == nil {
				e.key = c
			} else {
				e.key = e.doc.SourcePath
			}
		case filepath.IsAbs(e.doc.SourcePath):
			if c, err := CanonicalizeLocalPath(e.doc.SourcePath); err == nil {
				e.key = c
			} else {
				e.key = filepath.Clean(e.doc.SourcePath)
			}
		default:
			e.relative = true
			e.key = filepath.Clean(e.doc.SourcePath)
		}
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("FindReferenceDuplicates: rows: %w", err)
	}

	// Union-find over the entries.
	parent := make([]int, len(entries))
	for i := range parent {
		parent[i] = i
	}
	var find func(int) int
	find = func(i int) int {
		for parent[i] != i {
			parent[i] = parent[parent[i]]
			i = parent[i]
		}
		return i
	}
	union := func(a, b int) {
		ra, rb := find(a), find(b)
		if ra != rb {
			parent[rb] = ra
		}
	}

	for i := 0; i < len(entries); i++ {
		if entries[i].key == "" {
			continue
		}
		for j := i + 1; j < len(entries); j++ {
			if entries[j].key == "" || entries[i].kind != entries[j].kind {
				continue
			}
			if !entries[i].relative && !entries[j].relative {
				if entries[i].key == entries[j].key {
					union(i, j)
				}
				continue
			}
			// At least one side is a legacy relative path.
			if relativeMatches(entries[i], entries[j]) || relativeMatches(entries[j], entries[i]) {
				union(i, j)
			}
		}
	}

	grouped := map[int][]int{}
	var order []int
	for i := range entries {
		r := find(i)
		if _, ok := grouped[r]; !ok {
			order = append(order, r)
		}
		grouped[r] = append(grouped[r], i)
	}

	var out []ReferenceDuplicateGroup
	for _, root := range order {
		members := grouped[root]
		if len(members) < 2 {
			continue
		}
		g := ReferenceDuplicateGroup{Kind: entries[members[0]].kind, Confidence: "certain"}
		for _, idx := range members {
			if entries[idx].relative {
				g.Confidence = "probable"
			}
			g.Docs = append(g.Docs, entries[idx].doc)
			if len(entries[idx].key) > len(g.Identity) {
				g.Identity = entries[idx].key
			}
		}
		if g.Confidence != "certain" {
			g.Note = "a relative path's original working directory was never recorded; matched by path suffix"
		}
		best := 0
		for i := 1; i < len(g.Docs); i++ {
			if g.Docs[i].TotalChunks > g.Docs[best].TotalChunks {
				best = i
				continue
			}
			if g.Docs[i].TotalChunks == g.Docs[best].TotalChunks && g.Docs[i].LastIndexed > g.Docs[best].LastIndexed {
				best = i
			}
		}
		g.Docs[best].Preferred = true
		out = append(out, g)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Identity < out[j].Identity })
	return out, nil
}

// relativeMatches reports whether the legacy relative entry resolves to
// the same source as the other entry, treating the relative path as
// relative to some ancestor of the other.
//
// `docs/menus.md` matches `/corpus/docs/menus.md` (the components of the
// relative path are the tail of the absolute one). This is the same
// relation resolveReferenceIdentity applies at import time, expressed
// without requiring the file to exist.
func relativeMatches(rel, other referenceSourceEntry) bool {
	if !rel.relative || rel.kind != other.kind {
		return false
	}
	return hasPathSuffix(other.key, rel.key)
}

// hasPathSuffix reports whether child ends with the components of parent.
func hasPathSuffix(child, parent string) bool {
	childParts := splitPathComponents(child)
	parentParts := splitPathComponents(parent)
	if len(parentParts) == 0 || len(parentParts) > len(childParts) {
		return false
	}
	offset := len(childParts) - len(parentParts)
	for i := range parentParts {
		if childParts[offset+i] != parentParts[i] {
			return false
		}
	}
	return true
}

func splitPathComponents(p string) []string {
	var out []string
	for _, part := range strings.Split(filepath.ToSlash(p), "/") {
		if part == "" || part == "." {
			continue
		}
		out = append(out, part)
	}
	return out
}

// fileExists is a small helper used by the ingest pipeline to give a
// precise error before any database work happens.
func fileExists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.Mode().IsRegular()
}
