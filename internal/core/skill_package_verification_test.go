// skill_package_verification_test.go — the post-commit verification's
// actual guarantee.
//
// The review (comment 6864706276) noted that the post-commit
// verification established the blobs' PRESENCE and SIZE but not their
// CONTENT INTEGRITY: the store is content-addressed, and nothing
// re-hashed the stored bytes to confirm they hash to the address the
// file row records.
//
// The verification now re-hashes. This test pins that it actually
// detects a blob whose bytes do not hash to its recorded address.
package internal

import (
	"testing"
)

// TestP4_TheVerificationEstablishesBlobContentIntegrity proves the
// post-commit verification establishes CONTENT INTEGRITY, not merely
// presence and size.
//
// The blob is corrupted IN PLACE after the import, keeping its recorded
// size — so a verification that only checked presence and size would
// pass it.
func TestP4_TheVerificationEstablishesBlobContentIntegrity(t *testing.T) {
	dm := p3DM(t)

	root := p4PackageTree(t, map[string]string{
		"SKILL.md":        p4SkillDoc("integrity", "1.0.0", "body\n"),
		"references/a.md": "# A\n",
	})
	res, err := dm.ImportSkillPackageFromDir(PackageImportOptions{Path: root})
	if err != nil {
		t.Fatalf("import: %v", err)
	}

	// The verification passes on a healthy import.
	if err := dm.verifyPackageImport(res); err != nil {
		t.Fatalf("the verification rejects a healthy import: %v", err)
	}

	// Corrupt one blob IN PLACE, keeping its recorded size.
	var target string
	for _, f := range res.Files {
		if f.Path == "references/a.md" {
			target = f.SHA256
		}
	}
	if target == "" {
		t.Fatal("the reference file row was not found")
	}
	corrupt := make([]byte, 4)
	copy(corrupt, "ZZZZ")
	if _, err := dm.db.Exec(`UPDATE skill_package_blobs SET content = ? WHERE sha256 = ?`,
		corrupt, target); err != nil {
		t.Fatalf("corrupt the blob: %v", err)
	}

	// The recorded size still matches the file row's size...
	var size int64
	if err := dm.db.QueryRow(`SELECT size_bytes FROM skill_package_blobs WHERE sha256 = ?`,
		target).Scan(&size); err != nil {
		t.Fatalf("read the blob size: %v", err)
	}
	if size != 4 {
		t.Fatalf("the corrupted blob's recorded size is %d, want 4", size)
	}

	// ...but the verification now FAILS, because the bytes do not hash
	// to their recorded address.
	err = dm.verifyPackageImport(res)
	if err == nil {
		t.Fatal("the verification accepted a blob whose bytes do not hash to its recorded address")
	}
	t.Logf("the verification rejected the corrupted blob: %v", err)
}
