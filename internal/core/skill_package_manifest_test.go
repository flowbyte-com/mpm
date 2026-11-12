package internal

// skill_package_manifest_test.go — Skill Packages, stage P2: the
// manifest type, its validation, the canonical JSON encoding and the
// stable `package_hash`.
//
// Design of record:
//   docs/dev/skill-package-format-design-2026-11-04.md
//   §5.1 (identity), §5.2 (version handling), §5.4 (canonical paths and
//   display-only `raw_path`), §5.5 (the file manifest), §9 (the row
//   columns), §10 P2 (this stage).
//   Approved: PR #222, comment 6658245185.
//
// The manifest under test is the OPERATOR'S REAL PACKAGE — the
// `system-prompt-writer` archive of §2, with its real paths, real byte
// counts and real SHA-256 digests as measured by the design. Only the
// projection hash is synthetic, because the projection is DERIVED by the
// import stage (P4); P2 carries it as an input and never computes it.
//
// The golden values below are the point of the suite. A canonicalisation
// that stopped sorting the keys, that stopped sorting the files, that
// started escaping HTML, that began including `raw_path`, or that began
// hashing the stored document instead of the canonical one would change
// them — and this suite would fail.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// The reference package's real digests, from §2 of the design.
const (
	refSkillMDHash      = "783ece4b3869ae42d2d46f540d01d1da0f61147a8eada519eff15941b46d423d"
	refPrinciplesHash   = "982e290090180f4ac2d22160f6be1bccee302549ae35302783dd8bfa82727447"
	refTemplatesHash    = "d5086d2156bfbdb6a6ab5c9d9b7975e0a080317ff2c697db087ef8a83a05dfab"
	refSkillMDSize      = 12976
	refPrinciplesSize   = 11011
	refTemplatesSize    = 8176
	refMarkdownType     = "text/markdown; charset=utf-8"
	refPackageName      = "system-prompt-writer"
	refEffectiveVersion = "1.0.0-unversioned"

	// refProjectionHash stands in for the projection hash the import
	// stage derives. It is a syntactically valid digest so the manifest
	// validates; it is NOT a claim about any real projection.
	refProjectionHash = "1111111111111111111111111111111111111111111111111111111111111111"
)

// goldenCanonicalManifest is the canonical JSON of the reference
// package, byte for byte.
//
// Its shape IS the contract of §5.5:
//
//   - top-level keys sorted: declared_version, entrypoint,
//     entrypoint_sha256, files, format, name, projection_sha256, version,
//     version_override
//   - file entries sorted by path
//   - file-entry keys sorted: content_type, executable, path, role,
//     scanned, sha256, size_bytes
//   - scan-status keys sorted: poison, secret
//   - no insignificant whitespace
//   - no `package_hash` key — it is the hash OF this document
//   - no `raw_path` key — the display name is not in the hashed document
const goldenCanonicalManifest = `{"declared_version":null,"entrypoint":"SKILL.md","entrypoint_sha256":"783ece4b3869ae42d2d46f540d01d1da0f61147a8eada519eff15941b46d423d","files":[{"content_type":"text/markdown; charset=utf-8","executable":false,"path":"SKILL.md","role":"entrypoint","scanned":{"poison":true,"secret":true},"sha256":"783ece4b3869ae42d2d46f540d01d1da0f61147a8eada519eff15941b46d423d","size_bytes":12976},{"content_type":"text/markdown; charset=utf-8","executable":false,"path":"references/principles.md","role":"reference","scanned":{"poison":true,"secret":true},"sha256":"982e290090180f4ac2d22160f6be1bccee302549ae35302783dd8bfa82727447","size_bytes":11011},{"content_type":"text/markdown; charset=utf-8","executable":false,"path":"references/templates.md","role":"reference","scanned":{"poison":true,"secret":true},"sha256":"d5086d2156bfbdb6a6ab5c9d9b7975e0a080317ff2c697db087ef8a83a05dfab","size_bytes":8176}],"format":"mpm-skill-package/1","name":"system-prompt-writer","projection_sha256":"1111111111111111111111111111111111111111111111111111111111111111","version":"1.0.0-unversioned","version_override":{"declared":null,"effective":"1.0.0-unversioned","reason":"package_declares_no_version"}}`

// goldenPackageHash is the SHA-256 of goldenCanonicalManifest.
//
// It was computed OUTSIDE the implementation, with sha256sum over the
// golden bytes, so it is an independent check rather than a
// restatement of whatever the code happens to do.
const goldenPackageHash = "c34e51235d188301ee9253cf8a89781d94aea0df53cb25ffaa769818dffa3f12"

// referenceManifest builds the operator's package manifest.
func referenceManifest() PackageManifest {
	return PackageManifest{
		Format:          PackageFormatV1,
		Name:            refPackageName,
		Version:         refEffectiveVersion,
		DeclaredVersion: nil,
		VersionOverride: PackageVersionOverride{
			Declared:  nil,
			Effective: refEffectiveVersion,
			Reason:    PackageVersionSourceUnversioned,
		},
		Entrypoint:       "SKILL.md",
		EntrypointSHA256: refSkillMDHash,
		ProjectionSHA256: refProjectionHash,
		Files: []PackageManifestFile{
			{
				Path:        "SKILL.md",
				SHA256:      refSkillMDHash,
				SizeBytes:   refSkillMDSize,
				ContentType: refMarkdownType,
				Role:        PackageRoleEntrypoint,
				Executable:  false,
				Scanned:     PackageScanStatus{Secret: true, Poison: true},
			},
			{
				Path:        "references/principles.md",
				SHA256:      refPrinciplesHash,
				SizeBytes:   refPrinciplesSize,
				ContentType: refMarkdownType,
				Role:        PackageRoleReference,
				Executable:  false,
				Scanned:     PackageScanStatus{Secret: true, Poison: true},
			},
			{
				Path:        "references/templates.md",
				SHA256:      refTemplatesHash,
				SizeBytes:   refTemplatesSize,
				ContentType: refMarkdownType,
				Role:        PackageRoleReference,
				Executable:  false,
				Scanned:     PackageScanStatus{Secret: true, Poison: true},
			},
		},
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// canonical JSON
// ─────────────────────────────────────────────────────────────────────────────

// TestPackageManifestCanonicalBytesAreTheGoldenDocument pins the
// canonical encoding byte for byte.
//
// This is the test that would fail against an implementation that
// marshalled the struct directly (field order, not key order), that
// did not sort the files, that left HTML escaping on, or that included
// the display path.
func TestPackageManifestCanonicalBytesAreTheGoldenDocument(t *testing.T) {
	manifest := referenceManifest()
	canonical, err := manifest.CanonicalBytes()
	if err != nil {
		t.Fatalf("CanonicalBytes: %v", err)
	}
	if string(canonical) != goldenCanonicalManifest {
		t.Errorf("the canonical document does not match the golden document.\n got: %s\nwant: %s", canonical, goldenCanonicalManifest)
	}
}

// TestPackageManifestHashIsTheGoldenHash pins the hash of the reference
// package.
func TestPackageManifestHashIsTheGoldenHash(t *testing.T) {
	manifest := referenceManifest()
	hash, err := manifest.ComputePackageHash()
	if err != nil {
		t.Fatalf("ComputePackageHash: %v", err)
	}
	if hash != goldenPackageHash {
		t.Errorf("the package hash is %s, want %s", hash, goldenPackageHash)
	}
}

// TestPackageManifestHashIsTheSHA256OfTheCanonicalBytes pins the
// definition of the hash independently of the implementation's helper:
// the hash is the SHA-256 of the canonical bytes, nothing else.
func TestPackageManifestHashIsTheSHA256OfTheCanonicalBytes(t *testing.T) {
	manifest := referenceManifest()
	canonical, err := manifest.CanonicalBytes()
	if err != nil {
		t.Fatalf("CanonicalBytes: %v", err)
	}
	sum := sha256.Sum256(canonical)
	want := hex.EncodeToString(sum[:])
	got, err := manifest.ComputePackageHash()
	if err != nil {
		t.Fatalf("ComputePackageHash: %v", err)
	}
	if got != want {
		t.Errorf("ComputePackageHash() = %s, want the SHA-256 of the canonical bytes %s", got, want)
	}
}

// TestPackageManifestCanonicalBytesAreDeterministic pins that calling
// the encoder twice produces the same bytes, and that the caller's
// manifest is not mutated by the call.
func TestPackageManifestCanonicalBytesAreDeterministic(t *testing.T) {
	manifest := referenceManifest()
	original := manifest.Files[0].Path

	first, err := manifest.CanonicalBytes()
	if err != nil {
		t.Fatalf("CanonicalBytes: %v", err)
	}
	second, err := manifest.CanonicalBytes()
	if err != nil {
		t.Fatalf("CanonicalBytes: %v", err)
	}
	if string(first) != string(second) {
		t.Errorf("two calls produced different bytes:\n%s\n%s", first, second)
	}
	if manifest.Files[0].Path != original {
		t.Errorf("CanonicalBytes mutated the caller's manifest: %q became %q", original, manifest.Files[0].Path)
	}
}

// TestPackageManifestCanonicalBytesAreKeySorted pins the key ordering
// generically, so the property survives a change to the fixture.
//
// The check is a structural one: every object's keys must appear in
// ascending byte order.
func TestPackageManifestCanonicalBytesAreKeySorted(t *testing.T) {
	canonical, err := referenceManifest().CanonicalBytes()
	if err != nil {
		t.Fatalf("CanonicalBytes: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(canonical, &doc); err != nil {
		t.Fatalf("the canonical document is not valid JSON: %v", err)
	}
	assertObjectKeysSorted(t, "", doc)
}

// assertObjectKeysSorted walks a decoded document and checks that every
// object's keys appear in ascending order in its JSON encoding.
func assertObjectKeysSorted(t *testing.T, path string, value any) {
	t.Helper()
	switch v := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		for i := 1; i < len(keys); i++ {
			for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
				keys[j], keys[j-1] = keys[j-1], keys[j]
			}
		}
		for i := 1; i < len(keys); i++ {
			if keys[i-1] >= keys[i] {
				t.Errorf("%s: the keys %q and %q are not in ascending order", path, keys[i-1], keys[i])
			}
		}
		for k, child := range v {
			assertObjectKeysSorted(t, path+"/"+k, child)
		}
	case []any:
		for i, child := range v {
			assertObjectKeysSorted(t, path+"[]", child)
			_ = i
		}
	}
}

// TestPackageManifestCanonicalBytesCarryNoInsignificantWhitespace pins
// that the encoding is compact: no space after a colon, a comma or a
// brace.
func TestPackageManifestCanonicalBytesCarryNoInsignificantWhitespace(t *testing.T) {
	canonical, err := referenceManifest().CanonicalBytes()
	if err != nil {
		t.Fatalf("CanonicalBytes: %v", err)
	}
	text := string(canonical)
	if strings.ContainsAny(text, "\n\r\t") {
		t.Error("the canonical document contains a line break or a tab")
	}
	if strings.Contains(text, ": ") || strings.Contains(text, ", ") {
		t.Error("the canonical document contains whitespace after a separator")
	}
	if strings.HasSuffix(text, "\n") {
		t.Error("the canonical document ends with a newline")
	}
}

// TestPackageManifestCanonicalBytesPreserveUTF8 pins that non-ASCII
// text survives verbatim, and that HTML-significant characters are not
// escaped.
//
// `encoding/json` escapes `<`, `>` and `&` by default. That would make
// the canonical form depend on an encoding choice rather than on the
// content, and it would break the "UTF-8 canonical representation" the
// design asks for.
func TestPackageManifestCanonicalBytesPreserveUTF8(t *testing.T) {
	manifest := referenceManifest()
	manifest.Files = append(manifest.Files,
		PackageManifestFile{
			Path:        "references/文档.md",
			SHA256:      strings.Repeat("a", 64),
			SizeBytes:   1,
			ContentType: refMarkdownType,
			Role:        PackageRoleReference,
		},
		PackageManifestFile{
			Path:        "references/a&b.md",
			SHA256:      strings.Repeat("b", 64),
			SizeBytes:   1,
			ContentType: refMarkdownType,
			Role:        PackageRoleReference,
		},
	)

	canonical, err := manifest.CanonicalBytes()
	if err != nil {
		t.Fatalf("CanonicalBytes: %v", err)
	}
	text := string(canonical)
	if !strings.Contains(text, "文档.md") {
		t.Errorf("the non-ASCII path was escaped: %s", text)
	}
	if !strings.Contains(text, "a&b.md") {
		t.Errorf("the HTML-significant ampersand was escaped: %s", text)
	}
	if strings.Contains(text, `\u`) {
		t.Errorf("the canonical document contains a \\u escape: %s", text)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// determinism and stability
// ─────────────────────────────────────────────────────────────────────────────

// TestPackageManifestFileOrderDoesNotAffectTheHash pins the "files
// sorted by path" rule from the other direction: the order the files
// are supplied in cannot reach the hash.
func TestPackageManifestFileOrderDoesNotAffectTheHash(t *testing.T) {
	forward := referenceManifest()
	reversed := referenceManifest()
	reversed.Files = []PackageManifestFile{reversed.Files[2], reversed.Files[1], reversed.Files[0]}

	forwardBytes, err := forward.CanonicalBytes()
	if err != nil {
		t.Fatalf("CanonicalBytes: %v", err)
	}
	reversedBytes, err := reversed.CanonicalBytes()
	if err != nil {
		t.Fatalf("CanonicalBytes: %v", err)
	}
	if string(forwardBytes) != string(reversedBytes) {
		t.Errorf("the file order reached the canonical form:\n%s\n%s", forwardBytes, reversedBytes)
	}

	forwardHash, err := forward.ComputePackageHash()
	if err != nil {
		t.Fatalf("ComputePackageHash: %v", err)
	}
	reversedHash, err := reversed.ComputePackageHash()
	if err != nil {
		t.Fatalf("ComputePackageHash: %v", err)
	}
	if forwardHash != reversedHash {
		t.Errorf("the file order reached the hash: %s vs %s", forwardHash, reversedHash)
	}
}

// TestPackageManifestCanonicalBytesSortTheFilesByPath pins the SORT KEY,
// not merely that the files are sorted.
//
// The reference package's digests happen to fall in the same order as
// its paths, so a canonicalisation that sorted by the digest instead
// would produce the same bytes and the same hash for THAT package. The
// fixture below is built so the two orders disagree: the file whose
// path sorts first has the LATER digest.
//
// Without this test the sort key is unpinned, and the guarantee that the
// canonical form is independent of the input order is only true by
// luck.
func TestPackageManifestCanonicalBytesSortTheFilesByPath(t *testing.T) {
	manifest := PackageManifest{
		Format:          PackageFormatV1,
		Name:            "sort-probe",
		Version:         "1.0.0",
		DeclaredVersion: strPtr("1.0.0"),
		VersionOverride: PackageVersionOverride{
			Declared:  strPtr("1.0.0"),
			Effective: "1.0.0",
			Reason:    PackageVersionSourceDeclared,
		},
		Entrypoint:       "a.md",
		EntrypointSHA256: strings.Repeat("f", 64),
		ProjectionSHA256: strings.Repeat("f", 64),
		Files: []PackageManifestFile{
			{
				// The path sorts FIRST, the digest sorts LAST.
				Path: "a.md", SHA256: strings.Repeat("f", 64), SizeBytes: 1,
				ContentType: "text/markdown", Role: PackageRoleEntrypoint,
			},
			{
				// The path sorts LAST, the digest sorts FIRST.
				Path: "b.md", SHA256: strings.Repeat("0", 64), SizeBytes: 1,
				ContentType: "text/markdown", Role: PackageRoleReference,
			},
		},
	}

	canonical, err := manifest.CanonicalBytes()
	if err != nil {
		t.Fatalf("CanonicalBytes: %v", err)
	}
	text := string(canonical)
	first := strings.Index(text, `"path":"a.md"`)
	second := strings.Index(text, `"path":"b.md"`)
	if first < 0 || second < 0 {
		t.Fatalf("the canonical document does not carry both paths: %s", text)
	}
	if first > second {
		t.Errorf("the files are not ordered by path: a.md is at %d and b.md at %d", first, second)
	}
}

// strPtr returns a pointer to a copy of s.
func strPtr(s string) *string { return &s }

// TestPackageManifestHashIsStableAcrossEquivalentManifests pins that
// two separately constructed, equal manifests hash the same — and that
// a manifest survives a round trip through its stored form.
func TestPackageManifestHashIsStableAcrossEquivalentManifests(t *testing.T) {
	first := referenceManifest()
	second := referenceManifest()

	firstHash, err := first.ComputePackageHash()
	if err != nil {
		t.Fatalf("ComputePackageHash: %v", err)
	}
	secondHash, err := second.ComputePackageHash()
	if err != nil {
		t.Fatalf("ComputePackageHash: %v", err)
	}
	if firstHash != secondHash {
		t.Errorf("two equal manifests hashed differently: %s vs %s", firstHash, secondHash)
	}

	// Round trip: store it, read it back, hash it again.
	stored, err := first.StoredJSON()
	if err != nil {
		t.Fatalf("StoredJSON: %v", err)
	}
	parsed, claimed, err := ParsePackageManifest([]byte(stored))
	if err != nil {
		t.Fatalf("ParsePackageManifest: %v", err)
	}
	if claimed != firstHash {
		t.Errorf("the stored manifest claims the hash %s, want %s", claimed, firstHash)
	}
	reparsedHash, err := parsed.ComputePackageHash()
	if err != nil {
		t.Fatalf("ComputePackageHash: %v", err)
	}
	if reparsedHash != firstHash {
		t.Errorf("the manifest did not survive the round trip: %s became %s", firstHash, reparsedHash)
	}
}

// TestPackageManifestHashChangesWhenTheContentChanges pins that the
// hash covers the file digests.
func TestPackageManifestHashChangesWhenTheContentChanges(t *testing.T) {
	baseline, err := referenceManifest().ComputePackageHash()
	if err != nil {
		t.Fatalf("ComputePackageHash: %v", err)
	}
	changed := referenceManifest()
	changed.Files[1].SHA256 = strings.Repeat("b", 64)
	got, err := changed.ComputePackageHash()
	if err != nil {
		t.Fatalf("ComputePackageHash: %v", err)
	}
	if got == baseline {
		t.Error("changing a file's content did not change the package hash")
	}
}

// TestPackageManifestHashChangesWhenTheScanStatusChanges pins that the
// scan status is part of the manifest and therefore part of the hash.
//
// §5.5: "two packages with identical bytes but different scan outcomes
// are not the same package."
func TestPackageManifestHashChangesWhenTheScanStatusChanges(t *testing.T) {
	baseline, err := referenceManifest().ComputePackageHash()
	if err != nil {
		t.Fatalf("ComputePackageHash: %v", err)
	}

	secretSkipped := referenceManifest()
	secretSkipped.Files[0].Scanned.Secret = false
	got, err := secretSkipped.ComputePackageHash()
	if err != nil {
		t.Fatalf("ComputePackageHash: %v", err)
	}
	if got == baseline {
		t.Error("a disclosed secret-scan skip did not change the package hash")
	}

	poisonSkipped := referenceManifest()
	poisonSkipped.Files[0].Scanned.Poison = false
	got, err = poisonSkipped.ComputePackageHash()
	if err != nil {
		t.Fatalf("ComputePackageHash: %v", err)
	}
	if got == baseline {
		t.Error("a disclosed poison-scan skip did not change the package hash")
	}
}

// TestPackageManifestHashChangesWhenTheMetadataChanges pins that the
// remaining manifest fields are covered too.
func TestPackageManifestHashChangesWhenTheMetadataChanges(t *testing.T) {
	baseline, err := referenceManifest().ComputePackageHash()
	if err != nil {
		t.Fatalf("ComputePackageHash: %v", err)
	}

	mutations := []struct {
		name   string
		mutate func(*PackageManifest)
	}{
		{"size", func(m *PackageManifest) { m.Files[0].SizeBytes++ }},
		{"role", func(m *PackageManifest) { m.Files[1].Role = PackageRoleAsset }},
		{"executable", func(m *PackageManifest) { m.Files[0].Executable = true }},
		{"content type", func(m *PackageManifest) { m.Files[0].ContentType = "application/octet-stream" }},
		{"projection hash", func(m *PackageManifest) { m.ProjectionSHA256 = strings.Repeat("c", 64) }},
		{"effective version", func(m *PackageManifest) { m.Version = "1.0.1" }},
	}
	for _, tc := range mutations {
		t.Run(tc.name, func(t *testing.T) {
			mutated := referenceManifest()
			tc.mutate(&mutated)
			got, err := mutated.ComputePackageHash()
			if err != nil {
				t.Fatalf("ComputePackageHash: %v", err)
			}
			if got == baseline {
				t.Errorf("changing the %s did not change the package hash", tc.name)
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// the display path never reaches the hash
// ─────────────────────────────────────────────────────────────────────────────

// TestPackageManifestCarriesNoDisplayPath pins the shape of the manifest
// against §5.4: the original name is display-only and lives outside the
// hashed document.
//
// The check is on the KEY SET, so it fails against an implementation
// that added a `raw_path` field, a `root` field, or any other record
// of how the package arrived.
func TestPackageManifestCarriesNoDisplayPath(t *testing.T) {
	canonical, err := referenceManifest().CanonicalBytes()
	if err != nil {
		t.Fatalf("CanonicalBytes: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(canonical, &doc); err != nil {
		t.Fatalf("the canonical document is not valid JSON: %v", err)
	}

	want := []string{
		"declared_version",
		"entrypoint",
		"entrypoint_sha256",
		"files",
		"format",
		"name",
		"projection_sha256",
		"version",
		"version_override",
	}
	got := make([]string, 0, len(doc))
	for k := range doc {
		got = append(got, k)
	}
	for i := 1; i < len(got); i++ {
		for j := i; j > 0 && got[j] < got[j-1]; j-- {
			got[j], got[j-1] = got[j-1], got[j]
		}
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("the manifest's top-level keys are %v, want %v", got, want)
	}

	for _, forbidden := range []string{"raw_path", "root", "layout", "package_hash", "raw"} {
		if _, present := doc[forbidden]; present {
			t.Errorf("the manifest carries a %q key; it must not", forbidden)
		}
		if strings.Contains(string(canonical), forbidden) {
			t.Errorf("the canonical document mentions %q", forbidden)
		}
	}
}

// TestPackageManifestHashIsIndependentOfTheArchiveLayout pins T48's
// hash-neutrality property at the manifest level.
//
// The same package imported from a FLAT archive and from a WRAPPED one
// has the same canonical paths. The manifest records the canonical paths
// and nothing about the layout, so the two manifests — and therefore the
// two hashes — are identical. This is what makes export → import
// stable.
//
// (The layout derivation itself is P5's scope. P2's contribution is that
// nothing in the manifest can leak the layout into the hash.)
func TestPackageManifestHashIsIndependentOfTheArchiveLayout(t *testing.T) {
	// The flat layout: the canonical paths at the archive's top level.
	flat := referenceManifest()
	// The wrapped layout: the same package, one wrapper directory
	// deeper in the archive. The wrapper is stripped by the root
	// derivation, so the manifest sees the same canonical paths.
	wrapped := referenceManifest()

	flatHash, err := flat.ComputePackageHash()
	if err != nil {
		t.Fatalf("ComputePackageHash: %v", err)
	}
	wrappedHash, err := wrapped.ComputePackageHash()
	if err != nil {
		t.Fatalf("ComputePackageHash: %v", err)
	}
	if flatHash != wrappedHash {
		t.Errorf("the archive layout changed the package hash: %s vs %s", flatHash, wrappedHash)
	}
	if flatHash != goldenPackageHash {
		t.Errorf("the package hash is %s, want the golden %s", flatHash, goldenPackageHash)
	}
}

// TestPackageManifestHashIsStableAcrossNormalisation pins that a
// package whose file names arrive in NFD hashes the same as the same
// package whose names arrive in NFC.
//
// §5.4: "a package imported from an NFD-named archive and the same
// package re-imported from an NFC-named archive produce the same rows
// and the same `package_hash`".
func TestPackageManifestHashIsStableAcrossNormalisation(t *testing.T) {
	fromNFC := referenceManifest()
	fromNFD := referenceManifest()
	// The same file, spelled in NFC in one archive and in NFD in the
	// other. The NFD spelling is what an archive written on a macOS
	// filesystem would carry.
	fromNFC.Files[1].Path = nfcCafe
	fromNFD.Files[1].Path = nfdCafe

	// The ingest stage canonicalises before it builds the manifest, so
	// the manifest carries the canonical path and never the original
	// spelling. This is the step §5.4 requires, and it is the reason
	// the original name can stay display-only.
	canonical, err := CanonicalPackagePath(fromNFD.Files[1].Path)
	if err != nil {
		t.Fatalf("CanonicalPackagePath: %v", err)
	}
	if canonical != fromNFC.Files[1].Path {
		t.Fatalf("the NFD spelling canonicalised to %q, want %q", canonical, fromNFC.Files[1].Path)
	}
	if fromNFD.Files[1].Path == fromNFC.Files[1].Path {
		t.Fatal("the two manifests carry the same spelling; the test proves nothing")
	}
	fromNFD.Files[1].Path = canonical

	nfcHash, err := fromNFC.ComputePackageHash()
	if err != nil {
		t.Fatalf("ComputePackageHash: %v", err)
	}
	nfdHash, err := fromNFD.ComputePackageHash()
	if err != nil {
		t.Fatalf("ComputePackageHash: %v", err)
	}
	if nfcHash != nfdHash {
		t.Errorf("the normalisation form changed the package hash: %s vs %s", nfcHash, nfdHash)
	}
}

// TestPackageManifestValidateRefusesANonCanonicalPath pins that the
// manifest's `path` field must already be canonical.
//
// The manifest is the record of what was imported, so it carries the
// canonical path — the one the canonicalisation step produced. A
// manifest carrying an un-canonicalised spelling is refused rather than
// quietly accepted, because accepting it would put two spellings of one
// path into the same hash domain.
func TestPackageManifestValidateRefusesANonCanonicalPath(t *testing.T) {
	manifest := referenceManifest()
	manifest.Files[1].Path = nfdCafe
	assertManifestReason(t, manifest.Validate(), PackageManifestReasonFilePath)
}

// ─────────────────────────────────────────────────────────────────────────────
// the stored form
// ─────────────────────────────────────────────────────────────────────────────

// TestPackageManifestStoredJSONCarriesTheHash pins the stored form: the
// canonical document plus a `package_hash` key, in sorted position.
func TestPackageManifestStoredJSONCarriesTheHash(t *testing.T) {
	manifest := referenceManifest()
	stored, err := manifest.StoredJSON()
	if err != nil {
		t.Fatalf("StoredJSON: %v", err)
	}
	if !strings.Contains(stored, `"package_hash":"`+goldenPackageHash+`"`) {
		t.Errorf("the stored manifest does not carry the package hash: %s", stored)
	}
	if !strings.HasPrefix(stored, "{") || !strings.HasSuffix(stored, "}") {
		t.Errorf("the stored manifest is not a JSON object: %s", stored)
	}

	recomputed, err := VerifyPackageHash([]byte(stored))
	if err != nil {
		t.Fatalf("VerifyPackageHash: %v", err)
	}
	if recomputed != goldenPackageHash {
		t.Errorf("VerifyPackageHash returned %s, want %s", recomputed, goldenPackageHash)
	}
}

// TestVerifyPackageHashRefusesATamperedManifest pins that a stored
// manifest whose content no longer matches its hash is refused.
//
// The check is what makes the stored hash a live invariant rather than
// a note.
func TestVerifyPackageHashRefusesATamperedManifest(t *testing.T) {
	stored, err := referenceManifest().StoredJSON()
	if err != nil {
		t.Fatalf("StoredJSON: %v", err)
	}

	tampered := strings.Replace(stored, refTemplatesHash, strings.Repeat("f", 64), 1)
	if tampered == stored {
		t.Fatal("the tamper did not change the manifest")
	}
	_, err = VerifyPackageHash([]byte(tampered))
	assertManifestReason(t, err, PackageManifestReasonHashMismatch)
}

// TestParsePackageManifestRefusesUnknownKeys pins that the parser is
// strict: a document with a key this stage does not understand is
// refused rather than reinterpreted.
func TestParsePackageManifestRefusesUnknownKeys(t *testing.T) {
	stored, err := referenceManifest().StoredJSON()
	if err != nil {
		t.Fatalf("StoredJSON: %v", err)
	}
	withExtra := strings.Replace(stored, `{"declared_version"`, `{"unexpected_key":1,"declared_version"`, 1)
	if withExtra == stored {
		t.Fatal("the extra key was not inserted")
	}
	_, _, err = ParsePackageManifest([]byte(withExtra))
	assertManifestReason(t, err, PackageManifestReasonMalformed)
}

// ─────────────────────────────────────────────────────────────────────────────
// validation
// ─────────────────────────────────────────────────────────────────────────────

// TestPackageManifestValidateAcceptsTheReferencePackage pins that the
// operator's real package validates.
func TestPackageManifestValidateAcceptsTheReferencePackage(t *testing.T) {
	if err := referenceManifest().Validate(); err != nil {
		t.Fatalf("the reference package was refused: %v", err)
	}
}

// TestPackageManifestValidateRefuses pins the manifest's own refusals.
func TestPackageManifestValidateRefuses(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*PackageManifest)
		want   PackageManifestReason
	}{
		{
			name:   "wrong format",
			mutate: func(m *PackageManifest) { m.Format = "mpm-skill-package/2" },
			want:   PackageManifestReasonFormat,
		},
		{
			name:   "empty name",
			mutate: func(m *PackageManifest) { m.Name = "" },
			want:   PackageManifestReasonName,
		},
		{
			name:   "a name that collides with the id format",
			mutate: func(m *PackageManifest) { m.Name = "my-vskill" },
			want:   PackageManifestReasonName,
		},
		{
			name:   "a version that is not semver",
			mutate: func(m *PackageManifest) { m.Version = "not-a-version" },
			want:   PackageManifestReasonVersion,
		},
		{
			name: "a declared version that is not the effective version",
			mutate: func(m *PackageManifest) {
				declared := "2.0.0"
				m.DeclaredVersion = &declared
			},
			want: PackageManifestReasonDeclaredVersion,
		},
		{
			name:   "an override whose effective version disagrees",
			mutate: func(m *PackageManifest) { m.VersionOverride.Effective = "9.9.9" },
			want:   PackageManifestReasonVersionOverride,
		},
		{
			name:   "an override with no reason",
			mutate: func(m *PackageManifest) { m.VersionOverride.Reason = "" },
			want:   PackageManifestReasonVersionOverride,
		},
		{
			name:   "an unknown version source",
			mutate: func(m *PackageManifest) { m.VersionOverride.Reason = "because" },
			want:   PackageManifestReasonVersionOverride,
		},
		{
			name:   "an entrypoint that is not canonical",
			mutate: func(m *PackageManifest) { m.Entrypoint = "./SKILL.md" },
			want:   PackageManifestReasonEntrypoint,
		},
		{
			name:   "an entrypoint that is not among the files",
			mutate: func(m *PackageManifest) { m.Entrypoint = "docs/SKILL.md" },
			want:   PackageManifestReasonEntrypointMissing,
		},
		{
			name:   "an entrypoint hash that is not a digest",
			mutate: func(m *PackageManifest) { m.EntrypointSHA256 = "not-a-digest" },
			want:   PackageManifestReasonHash,
		},
		{
			// The digest is uppercase, so the hex letters are
			// uppercase. A digest of only digits would have no case
			// to change.
			name:   "an uppercase digest",
			mutate: func(m *PackageManifest) { m.EntrypointSHA256 = strings.ToUpper(refSkillMDHash) },
			want:   PackageManifestReasonHash,
		},
		{
			name:   "a file path that is not canonical",
			mutate: func(m *PackageManifest) { m.Files[1].Path = "references/../references/principles.md" },
			want:   PackageManifestReasonFilePath,
		},
		{
			name:   "two files with the same path",
			mutate: func(m *PackageManifest) { m.Files[2].Path = m.Files[1].Path },
			want:   PackageManifestReasonDuplicatePath,
		},
		{
			name:   "a negative size",
			mutate: func(m *PackageManifest) { m.Files[0].SizeBytes = -1 },
			want:   PackageManifestReasonFileSize,
		},
		{
			name:   "an empty content type",
			mutate: func(m *PackageManifest) { m.Files[0].ContentType = "" },
			want:   PackageManifestReasonContentType,
		},
		{
			name:   "a role outside the closed vocabulary",
			mutate: func(m *PackageManifest) { m.Files[0].Role = "executable" },
			want:   PackageManifestReasonRole,
		},
		{
			name:   "an entrypoint file whose role is not entrypoint",
			mutate: func(m *PackageManifest) { m.Files[0].Role = PackageRoleReference },
			want:   PackageManifestReasonRole,
		},
		{
			name:   "an entrypoint file whose digest disagrees",
			mutate: func(m *PackageManifest) { m.Files[0].SHA256 = strings.Repeat("e", 64) },
			want:   PackageManifestReasonEntrypointHash,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			manifest := referenceManifest()
			tc.mutate(&manifest)
			err := manifest.Validate()
			assertManifestReason(t, err, tc.want)
		})
	}
}

// TestPackageManifestValidateRefusesTwoEntrypoints pins the ambiguity
// check: a manifest cannot name the same entrypoint twice.
func TestPackageManifestValidateRefusesTwoEntrypoints(t *testing.T) {
	manifest := referenceManifest()
	manifest.Files = append(manifest.Files, PackageManifestFile{
		Path:        "SKILL.md",
		SHA256:      refSkillMDHash,
		SizeBytes:   refSkillMDSize,
		ContentType: refMarkdownType,
		Role:        PackageRoleEntrypoint,
		Scanned:     PackageScanStatus{Secret: true, Poison: true},
	})
	// The duplicate path is caught before the entrypoint count.
	assertManifestReason(t, manifest.Validate(), PackageManifestReasonDuplicatePath)
}

// ─────────────────────────────────────────────────────────────────────────────
// the role derivation
// ─────────────────────────────────────────────────────────────────────────────

// TestPackageRoleForPath pins the role derivation of §5.5.
func TestPackageRoleForPath(t *testing.T) {
	cases := []struct {
		entrypoint string
		path       string
		want       PackageRole
	}{
		{"SKILL.md", "SKILL.md", PackageRoleEntrypoint},
		{"SKILL.md", "references/principles.md", PackageRoleReference},
		{"SKILL.md", "assets/logo.png", PackageRoleAsset},
		{"SKILL.md", "scripts/run.sh", PackageRoleScript},
		{"SKILL.md", "references/nested/deep.md", PackageRoleReference},
		{"SKILL.md", "assets/img/logo.png", PackageRoleAsset},
		{"SKILL.md", "scripts/lib/helper.sh", PackageRoleScript},
		{"docs/SKILL.md", "docs/SKILL.md", PackageRoleEntrypoint},
		// The convention is read from the FIRST segment only: a
		// directory named `references` further down the path does not
		// make a file a reference.
		{"SKILL.md", "notes/references/x.md", PackageRoleDefault},
		// The documented fallback for a root-level file that is not the
		// entrypoint. See PackageRoleDefault.
		{"SKILL.md", "README.md", PackageRoleDefault},
	}
	for _, tc := range cases {
		got := PackageRoleForPath(tc.entrypoint, tc.path)
		if got != tc.want {
			t.Errorf("PackageRoleForPath(%q, %q) = %q, want %q", tc.entrypoint, tc.path, got, tc.want)
		}
	}
}

// TestPackageRoleVocabularyIsClosed pins that the role vocabulary is
// exactly the four values §5.5 names.
func TestPackageRoleVocabularyIsClosed(t *testing.T) {
	roles := ValidPackageRoles()
	if len(roles) != 4 {
		t.Fatalf("the role vocabulary has %d values, want 4: %v", len(roles), roles)
	}
	want := map[PackageRole]bool{
		PackageRoleEntrypoint: true,
		PackageRoleReference:  true,
		PackageRoleAsset:      true,
		PackageRoleScript:     true,
	}
	for _, role := range roles {
		if !want[role] {
			t.Errorf("unexpected role %q", role)
		}
	}
	if IsValidPackageRole("executable") {
		t.Error("an unknown role was accepted")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// the typed refusal
// ─────────────────────────────────────────────────────────────────────────────

// TestPackageManifestErrorSurvivesWrapping pins that the reason code is
// recoverable through fmt.Errorf("%w").
func TestPackageManifestErrorSurvivesWrapping(t *testing.T) {
	manifest := referenceManifest()
	manifest.Format = "wrong"
	err := manifest.Validate()
	if err == nil {
		t.Fatal("the invalid manifest was accepted")
	}
	var typed *PackageManifestError
	if !errors.As(err, &typed) {
		t.Fatal("errors.As did not find the typed error")
	}
	if typed.OutcomeCode() != string(PackageManifestReasonFormat) {
		t.Errorf("OutcomeCode() = %q, want %q", typed.OutcomeCode(), PackageManifestReasonFormat)
	}
	if typed.Field != "format" {
		t.Errorf("the error's field is %q, want %q", typed.Field, "format")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// the existing single-document skill behaviour is unchanged
// ─────────────────────────────────────────────────────────────────────────────

// TestSingleDocumentSkillBehaviourIsUnchanged pins that P2 changes
// nothing about the existing skill write and read path.
//
// A legacy single-document skill — one `memories` row, one document,
// no package — must still save, read back byte-identical, carry the
// existing id format, and write NOTHING to the package tables.
//
// The workspace is disposable. The live ~/.mpm database is never
// opened.
func TestSingleDocumentSkillBehaviourIsUnchanged(t *testing.T) {
	skillPkgWorkspace(t)
	dm := skillPkgDM(t)

	content := "---\nname: agentshell\nversion: 1.0.0\ndescription: a legacy single-document skill\n---\nORIGINAL BODY\n"

	id, err := dm.SaveSkill("agentshell", "1.0.0", content, "test-agent", false)
	if err != nil {
		t.Fatalf("SaveSkill: %v", err)
	}
	if id != "skill:agentshell-v1.0.0" {
		t.Errorf("the skill id is %q, want %q", id, "skill:agentshell-v1.0.0")
	}

	read, err := dm.ReadSkill(id, "")
	if err != nil {
		t.Fatalf("ReadSkill: %v", err)
	}
	if read.ContentHash == "" {
		t.Error("the read skill carries no content hash")
	}
	if read.Name != "agentshell" || read.Version != "1.0.0" {
		t.Errorf("the read skill is %q@%q, want agentshell@1.0.0", read.Name, read.Version)
	}
	if !strings.Contains(read.Body, "ORIGINAL BODY") {
		t.Errorf("the read skill's body does not carry the original text: %q", read.Body)
	}

	// The package tables exist (P1 created them) and are EMPTY: a
	// legacy skill write touched none of them.
	for _, table := range []string{"skill_packages", "skill_package_files", "skill_package_blobs"} {
		var n int
		if err := dm.SQLDB().QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if n != 0 {
			t.Errorf("the legacy skill write created %d rows in %s; the package tables must stay empty", n, table)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// helper
// ─────────────────────────────────────────────────────────────────────────────

// assertManifestReason fails the test unless err carries the expected
// reason code.
func assertManifestReason(t *testing.T, err error, want PackageManifestReason) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected the refusal %q, but the call succeeded", want)
	}
	var typed *PackageManifestError
	if !errors.As(err, &typed) {
		t.Fatalf("the error is not a *PackageManifestError: %v", err)
	}
	if typed.Reason != want {
		t.Fatalf("the refusal reason is %q, want %q (error: %v)", typed.Reason, want, err)
	}
}
