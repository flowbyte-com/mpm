#!/usr/bin/env python3
"""Mutation harness for the Skill Packages P2 contract.

Each mutation is an INTENTIONALLY INCORRECT variant of the P2
implementation. The harness applies one mutation to a throwaway copy of
the checkout, runs the whole P2 test suite against it, and records how
the suite responded.

The point is the NEGATIVE case. A test suite that passes proves only
that the implementation and the tests agree; it does not prove that the
tests would notice if the implementation were wrong. This harness is
what turns "the tests pass" into "the tests would fail if the
implementation were wrong".

OUTCOMES

  rejected by behavioural test   the suite ran and an assertion failed
  rejected by compilation failure the mutated code did not compile
  survived                       the suite ran and PASSED — a hole
  skipped                        the mutation did not apply

A COMPILATION FAILURE IS A WEAKER RESULT, NOT A STRONGER ONE. A
mutation that only breaks the build proves that the mutant differs from
the original, not that the contract is enforced. So every mutation here
is written to COMPILE and to be caught by an assertion, and the harness
reports the two kinds of rejection separately so the distinction is
visible. The target is zero survived and zero compilation-failure
detections.

Usage

    python3 scripts/skill_package_mutation_check.py [--repo ROOT]

The harness needs the Go toolchain and the module cache. It does not
need the network, and it never touches the live MPM database, a
service, or any remote ref.
"""

import argparse
import os
import shutil
import subprocess
import sys
import tempfile

# The whole P2 suite. A mutation is judged by whether ANY of these fail.
SUITE = (
    "TestCanonicalPackagePath|TestPackagePath|TestPackageManifest|"
    "TestParsePackageManifest|TestVerifyPackageHash|TestPackageRole|"
    "TestSingleDocumentSkill|TestMalformed|TestStoredRoundTrip"
)

PATH_FILE = "internal/core/skill_package_path.go"
MANIFEST_FILE = "internal/core/skill_package_manifest.go"

# (name, file, old, new)
MUTATIONS = [
    # ── the canonical form ────────────────────────────────────────────
    (
        "the files are not sorted in the canonical form",
        MANIFEST_FILE,
        "\tsort.SliceStable(files, func(i, j int) bool {\n\t\treturn files[i].Path < files[j].Path\n\t})\n",
        "\t_ = files\n\t_ = sort.SliceStable\n",
    ),
    (
        "the canonical form is the struct marshalled directly",
        MANIFEST_FILE,
        "\tdecoder.UseNumber()\n\tvar doc map[string]any\n\tif err := decoder.Decode(&doc); err != nil {\n\t\treturn nil, fmt.Errorf(\"package manifest: decode: %w\", err)\n\t}\n\treturn doc, nil\n",
        "\t_ = decoder\n\treturn nil, nil\n",
    ),
    (
        "HTML escaping is left on",
        MANIFEST_FILE,
        "\tencoder.SetEscapeHTML(false)\n",
        "",
    ),
    (
        "the hashed document carries the package_hash key",
        MANIFEST_FILE,
        "\treturn doc, nil\n}\n",
        "\tdoc[\"package_hash\"] = \"\"\n\treturn doc, nil\n}\n",
    ),
    (
        "the hashed document carries the display path",
        MANIFEST_FILE,
        "\t// Executable flags a script. A flagged script is STORED and never\n\t// executed (§7 threat 16).\n\tExecutable bool `json:\"executable\"`\n",
        "\t// Executable flags a script. A flagged script is STORED and never\n\t// executed (§7 threat 16).\n\tExecutable bool `json:\"executable\"`\n\n\t// RawPath is the display-only original name.\n\tRawPath string `json:\"raw_path\"`\n",
    ),
    (
        "the scan status is not part of the manifest",
        MANIFEST_FILE,
        "\t// Scanned records which scanners actually ran on this file.\n\tScanned PackageScanStatus `json:\"scanned\"`\n",
        "\t// Scanned records which scanners actually ran on this file.\n\tScanned PackageScanStatus `json:\"-\"`\n",
    ),
    (
        "the file list is sorted by digest rather than by path",
        MANIFEST_FILE,
        "\tsort.SliceStable(files, func(i, j int) bool {\n\t\treturn files[i].Path < files[j].Path\n\t})\n",
        "\tsort.SliceStable(files, func(i, j int) bool {\n\t\treturn files[i].SHA256 < files[j].SHA256\n\t})\n",
    ),
    (
        "the canonical JSON keeps a trailing newline",
        MANIFEST_FILE,
        "\treturn bytes.TrimRight(buf.Bytes(), \"\\n\"), nil\n",
        "\treturn buf.Bytes(), nil\n",
    ),
    (
        "the canonical JSON is indented",
        MANIFEST_FILE,
        "\tencoder.SetEscapeHTML(false)\n",
        "\tencoder.SetEscapeHTML(false)\n\tencoder.SetIndent(\"\", \"  \")\n",
    ),
    (
        "the manifest construction boundary drops its UTF-8 guard",
        MANIFEST_FILE,
        "\tif !utf8.ValidString(m.Entrypoint) {\n\t\treturn manifestErrorf(PackageManifestReasonInvalidUTF8, \"entrypoint\",\n\t\t\t\"entrypoint: is not valid UTF-8 and cannot be represented in a manifest\")\n\t}\n",
        "\t_ = utf8.ValidString\n",
    ),
    (
        "the manifest construction boundary drops its provenance guard",
        MANIFEST_FILE,
        "\treturn m.validateVersionProvenance()\n}\n",
        "\treturn nil\n}\n",
    ),

    # ── the path grammar ──────────────────────────────────────────────
    (
        "the paths are not normalised to NFC",
        PATH_FILE,
        "\tcanonical := norm.NFC.String(raw)\n",
        "\tcanonical := raw\n\t_ = norm.NFC\n",
    ),
    (
        "the malformed UTF-8 check is removed",
        PATH_FILE,
        "\tif !utf8.ValidString(raw) {\n\t\treturn \"\", &PackagePathError{Reason: PackagePathReasonInvalidUTF8, Path: raw}\n\t}\n",
        "\t_ = utf8.ValidString\n",
    ),
    (
        "the depth limit is not enforced",
        PATH_FILE,
        "\tif len(segments) > MaxPackagePathDepth {\n\t\treturn \"\", &PackagePathError{Reason: PackagePathReasonTooDeep, Path: raw}\n\t}\n",
        "",
    ),
    (
        "the length limit is not enforced",
        PATH_FILE,
        "\tif len(canonical) > MaxPackagePathBytes {\n\t\treturn \"\", &PackagePathError{Reason: PackagePathReasonTooLong, Path: raw}\n\t}\n",
        "",
    ),
    (
        "the leading-slash rule is dropped",
        PATH_FILE,
        "\tif strings.HasPrefix(raw, \"/\") {\n\t\treturn \"\", &PackagePathError{Reason: PackagePathReasonAbsolute, Path: raw}\n\t}\n",
        "",
    ),
    (
        "the drive-letter rule is dropped",
        PATH_FILE,
        "\tif hasDriveLetterPrefix(raw) {\n\t\treturn \"\", &PackagePathError{Reason: PackagePathReasonDriveLetter, Path: raw}\n\t}\n",
        "",
    ),
    (
        "the UNC rule is dropped",
        PATH_FILE,
        "\tif strings.HasPrefix(raw, `\\\\`) {\n\t\treturn \"\", &PackagePathError{Reason: PackagePathReasonUNC, Path: raw}\n\t}\n",
        "",
    ),
    (
        "the backslash rule is dropped",
        PATH_FILE,
        "\tif strings.ContainsRune(raw, '\\\\') {\n\t\treturn \"\", &PackagePathError{Reason: PackagePathReasonBackslash, Path: raw}\n\t}\n",
        "",
    ),
    (
        "the traversal rule is dropped",
        PATH_FILE,
        "\t\tcase \"..\":\n\t\t\treturn \"\", &PackagePathError{Reason: PackagePathReasonTraversal, Path: raw}\n",
        "",
    ),
    (
        "the dot-segment rule is dropped",
        PATH_FILE,
        "\t\tcase \".\":\n\t\t\treturn \"\", &PackagePathError{Reason: PackagePathReasonDotSegment, Path: raw}\n",
        "",
    ),
    (
        "the empty-segment rule is dropped",
        PATH_FILE,
        "\t\tcase \"\":\n\t\t\treturn \"\", &PackagePathError{Reason: PackagePathReasonEmptySegment, Path: raw}\n",
        "",
    ),
    (
        "the trailing-slash rule is dropped",
        PATH_FILE,
        "\tif strings.HasSuffix(canonical, \"/\") {\n\t\treturn \"\", &PackagePathError{Reason: PackagePathReasonTrailingSlash, Path: raw}\n\t}\n",
        "",
    ),
    (
        "the NUL rule is dropped",
        PATH_FILE,
        "\tif strings.IndexByte(raw, 0) >= 0 {\n\t\treturn \"\", &PackagePathError{Reason: PackagePathReasonNUL, Path: raw}\n\t}\n",
        "",
    ),
    (
        "the control-character rule is dropped",
        PATH_FILE,
        "\tfor _, r := range raw {\n\t\tif unicode.IsControl(r) {\n\t\t\treturn \"\", &PackagePathError{Reason: PackagePathReasonControlCharacter, Path: raw}\n\t\t}\n\t}\n",
        "\t_ = unicode.IsControl\n",
    ),

    # ── the collisions ────────────────────────────────────────────────
    (
        "case folding uses full Unicode lowercasing",
        PATH_FILE,
        "\tb := []byte(s)\n\tfor i := range b {\n\t\tif b[i] >= 'A' && b[i] <= 'Z' {\n\t\t\tb[i] += 'a' - 'A'\n\t\t}\n\t}\n\treturn string(b)\n",
        "\treturn strings.ToLower(s)\n",
    ),
    (
        "a duplicate is reported as a unicode collision",
        PATH_FILE,
        "\t\tif existing == raw {\n\t\t\treason = PackagePathReasonDuplicate\n\t\t}\n",
        "\t\t_ = existing\n",
    ),
    (
        "the case collision is not detected",
        PATH_FILE,
        "\tif owner, ok := s.folded[folded]; ok {\n\t\treturn \"\", &PackagePathError{\n\t\t\tReason: PackagePathReasonCaseCollision,\n\t\t\tPath:   raw,\n\t\t\tDetail: owner,\n\t\t}\n\t}\n",
        "\t_ = folded\n",
    ),
    (
        "the unicode collision is not detected",
        PATH_FILE,
        "\t\treason := PackagePathReasonUnicodeCollision\n\t\tif existing == raw {\n\t\t\treason = PackagePathReasonDuplicate\n\t\t}\n",
        "\t\treason := PackagePathReasonDuplicate\n\t\t_ = existing\n",
    ),
]


def apply_mutation(path, old, new):
    with open(path, encoding="utf-8") as fh:
        src = fh.read()
    n = src.count(old)
    if n != 1:
        return False, "the anchor matched %d times" % n
    with open(path, "w", encoding="utf-8") as fh:
        fh.write(src.replace(old, new))
    return True, ""


def run_suite(core_dir):
    env = dict(os.environ)
    env.setdefault("CGO_CFLAGS", "-DSQLITE_ENABLE_FTS5=1")
    env.setdefault("CGO_LDFLAGS", "-lm")
    proc = subprocess.run(
        ["go", "test", "-tags", "fts5", "-count=1", "-run", SUITE, "."],
        cwd=core_dir, env=env, capture_output=True, text=True, timeout=1800,
    )
    return proc.returncode, proc.stdout + proc.stderr


def classify(rc, out):
    """Classify how the suite responded to a mutation."""
    if rc == 0:
        return "survived", ""
    if "[build failed]" in out or "build constraints exclude" in out:
        return "rejected by compilation failure", ""
    for line in out.splitlines():
        if line.startswith("--- FAIL"):
            return "rejected by behavioural test", line.strip()
    if "panic:" in out:
        return "rejected by behavioural test", "the suite panicked"
    return "rejected by behavioural test", "the suite failed"


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--repo", default=None,
                        help="the checkout to mutate (default: the repository root)")
    args = parser.parse_args()

    repo = os.path.abspath(args.repo or os.path.join(os.path.dirname(__file__), os.pardir))
    if not os.path.isdir(os.path.join(repo, "internal", "core")):
        print("not an MPM checkout:", repo)
        return 2

    outcomes = {
        "rejected by behavioural test": [],
        "rejected by compilation failure": [],
        "survived": [],
        "skipped": [],
    }

    with tempfile.TemporaryDirectory(prefix="mpm-p2-mutation-") as workdir:
        for name, relfile, old, new in MUTATIONS:
            target = os.path.join(workdir, "tree")
            if os.path.exists(target):
                shutil.rmtree(target)
            shutil.copytree(repo, target, symlinks=True,
                            ignore=shutil.ignore_patterns(".git", ".build"))
            path = os.path.join(target, relfile)
            ok, why = apply_mutation(path, old, new)
            if not ok:
                print("  SKIPPED    %-58s (%s)" % (name, why))
                outcomes["skipped"].append((name, why))
                continue
            rc, out = run_suite(os.path.join(target, "internal", "core"))
            outcome, detail = classify(rc, out)
            print("  %-10s %-58s %s" % (outcome.upper(), name, detail))
            outcomes[outcome].append((name, detail))

    print("")
    print("mutations: %d" % len(MUTATIONS))
    for key in ("rejected by behavioural test", "rejected by compilation failure",
                "survived", "skipped"):
        print("  %-32s %d" % (key, len(outcomes[key])))
    for name, detail in outcomes["survived"]:
        print("    SURVIVED: %s" % name)
    for name, detail in outcomes["skipped"]:
        print("    SKIPPED:  %s (%s)" % (name, detail))

    # A surviving mutation is a hole in the contract. A compilation
    # failure is a weaker result than a behavioural rejection, so it is
    # reported separately rather than counted as a pass.
    if outcomes["survived"]:
        return 1
    if outcomes["rejected by compilation failure"]:
        return 2
    if outcomes["skipped"]:
        return 3
    return 0


if __name__ == "__main__":
    sys.exit(main())
