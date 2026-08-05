package capability

import (
	"fmt"
	"regexp"
	"strings"
)

// =============================================================================
// forge_scanner.go — capability source poison scanner (spec §7.1)
//
// Twelve regex-based patterns that reject proposals which would, if
// executed, destroy the host, exfiltrate secrets, or open remote
// shells. The scanner is a security GATE: any match hard-rejects
// the proposal. There is no "warn" tier — destructive and
// exfiltration patterns are not edge cases an agent can be trusted
// to handle responsibly.
//
// Why regex (not an LLM judge):
//
//   * Deterministic. Same source always produces the same verdict.
//   * Auditable. A failed proposal points to a specific line +
//     specific pattern. The agent can see WHY and fix it.
//   * Fast. 12 patterns run in microseconds per proposal.
//   * No external dependency. The scanner cannot be unreachable,
//     rate-limited, or compromised by the LLM provider.
//
// This is structurally separate from the secret/poison scanner in
// `internal/core/memory.go` (which protects the MPM database from
// accidental key writes). The two scanners protect different layers
// and have different pattern sets; conflating them would let an
// agent smuggle a destructive script past the secret scanner just
// because it contained no API keys.
// =============================================================================

// PoisonPattern is one entry in the §7.1 pattern table. Each is a
// compiled regex with a human-readable name, message, and a step
// tag for the rejection record.
type PoisonPattern struct {
	Name    string
	Regex   *regexp.Regexp
	Message string
	Step    string // matches spec §3.1.2 step names: "scanner:rm_rf_root"
}

// poisonPatterns is the §7.1 list. The order is irrelevant — every
// pattern runs against every line, and matches across different
// patterns accumulate. Patterns are compiled once at package init.
var poisonPatterns []PoisonPattern

func init() {
	poisonPatterns = mustCompilePoisonPatterns()
}

// poisonSpec is the intermediate shape for mustCompilePoisonPatterns.
// Defined as a named type so the literal uses named field syntax —
// positional literals are easy to misalign (one missing name field
// silently shifts the whole table).
type poisonSpec struct {
	Name    string
	Pattern string
	Message string
}

// mustCompilePoisonPatterns panics on regex compile failure. Regex
// syntax is hard-coded by the program, so a failure here is a
// developer bug, not user input. Panicking is the right escape —
// fail closed at startup, never at proposal time.
func mustCompilePoisonPatterns() []PoisonPattern {
	specs := []poisonSpec{
		// 1. rm -rf / — direct root deletion. Flags in any order,
		//    including --no-preserve-root, --force, and the
		//    `--` terminator. We accept any short or long flag
		//    in the prefix because shell toolchains have wildly
		//    different flag conventions (GNU coreutils, BSD rm,
		//    busybox); the "destructive intent" signal is the
		//    leading `/`, not the specific flag set.
		{
			Name:    "rm_rf_root",
			Pattern: `\brm\s+(?:--?\S+\s+)*/\s*(?:$|[^a-zA-Z0-9_/])`,
			Message: "destructive: rm targeting filesystem root",
		},
		// 2. rm -rf $VAR — risky unquoted variable deletion. The
		//    variable could be empty or "/" if the env is hostile.
		{
			Name:    "rm_rf_variable",
			Pattern: `\brm\s+(?:-[rRfF]{1,2}\s+)+\$[A-Za-z_][A-Za-z0-9_]*`,
			Message: "destructive: rm -rf with unquoted variable (could expand to /)",
		},
		// 3. curl | sh and curl | bash — pipe remote content
		//    directly into a shell. Even with HTTPS, the server
		//    can return attacker-controlled code.
		{
			Name:    "curl_pipe_sh",
			Pattern: `\bcurl\b[^|;\n]*\|\s*(?:ba)?sh\b`,
			Message: "exfiltration risk: curl | sh executes remote code with no review",
		},
		// 4. wget | sh — same as #3 with wget.
		{
			Name:    "wget_pipe_sh",
			Pattern: `\bwget\b[^|;\n]*\|\s*(?:ba)?sh\b`,
			Message: "exfiltration risk: wget | sh executes remote code with no review",
		},
		// 5. Reverse shell via bash /dev/tcp. The /dev/tcp
		//    pseudo-device is bash-specific and almost always
		//    malicious outside of a handful of well-known
		//    debugging tricks.
		{
			Name:    "reverse_shell_bash",
			Pattern: `(?:/dev/tcp/[^\s]+|bash\s+-i\s+>&\s*/dev/tcp/)`,
			Message: "reverse shell: bash /dev/tcp pattern",
		},
		// 6. Reverse shell via netcat. nc -e and ncat -e are the
		//    canonical one-liner shells.
		{
			Name:    "reverse_shell_nc",
			Pattern: `\b(?:nc|ncat)\b\s+(?:-[a-zA-Z]+\s+)*-[a-zA-Z]*e\b`,
			Message: "reverse shell: netcat -e flag",
		},
		// 7. Hardcoded AWS access key. The AKIA prefix is the
		//    canonical marker; 16 uppercase alphanumerics after
		//    it. This is the highest-confidence credential leak
		//    pattern in the rule set.
		{
			Name:    "hardcoded_aws_key",
			Pattern: `\bAKIA[0-9A-Z]{16}\b`,
			Message: "credential leak: AWS access key in source code",
		},
		// 8. Hardcoded private key. The PEM header is
		//    unambiguous; any private key block in proposal source
		//    is by definition a leak.
		{
			Name:    "hardcoded_private_key",
			Pattern: `-----BEGIN (?:RSA |EC |DSA |OPENSSH |PGP )?PRIVATE KEY-----`,
			Message: "credential leak: PEM private key block in source code",
		},
		// 9. Absolute write to a system path. Writes to /etc/,
		//    /boot/, /sys/, /proc/ are persistent system
		//    modifications that require operator domain (which
		//    agents can never self-propose anyway).
		{
			Name:    "absolute_path_write_root",
			Pattern: `(?i)(?:>|>>|tee\s+)\s*/(?:etc|boot|sys|proc)/[^\s'"]+`,
			Message: "system write: redirection to /etc, /boot, /sys, or /proc",
		},
		// 10. chmod 777. World-writable is never the right
		//     default; this almost always pairs with a missing
		//     understanding of the access model.
		{
			Name:    "chmod_777",
			Pattern: `\bchmod\s+(?:-R\s+)?777\b`,
			Message: "permission widening: chmod 777 makes a file world-writable",
		},
		// 11. dd to a raw block device. This is the "wipe a disk"
		//     primitive; matches the canonical "of=/dev/sd*"
		//     form (sd, nvme, hd, vd, mmcblk).
		{
			Name:    "dd_destructive",
			Pattern: `\bdd\b[^;\n]*\bof=/dev/(?:sd|nvme|hd|vd|mmcblk)[a-z0-9]+`,
			Message: "destructive: dd writing to raw block device",
		},
		// 12. mkfs on a raw block device without a mountpoint
		//     check. The Forge can never prove the device is
		//     safe to format.
		{
			Name:    "mkfs_unmounted",
			Pattern: `\bmkfs(?:\.[a-z0-9]+)?\s+/dev/(?:sd|nvme|hd|vd|mmcblk)[a-z0-9]+`,
			Message: "destructive: mkfs on raw block device",
		},
	}

	patterns := make([]PoisonPattern, 0, len(specs))
	for _, s := range specs {
		re, err := regexp.Compile(s.Pattern)
		if err != nil {
			panic(fmt.Sprintf("forge_scanner: compile %s: %v", s.Name, err))
		}
		patterns = append(patterns, PoisonPattern{
			Name:    s.Name,
			Regex:   re,
			Message: s.Message,
			Step:    "scanner:" + s.Name,
		})
	}
	return patterns
}

// ScanSource runs every §7.1 pattern against source and returns one
// FieldError per match. Empty slice means the source is scanner-clean.
//
// The line number is computed by counting newlines before the match
// offset. The snippet is the matched substring plus a 16-char
// window on each side, elided with "…" if it runs off the line.
//
// The scanner never panics and never short-circuits — it accumulates
// every match so the agent can see ALL the problems at once.
func ScanSource(source string) []FieldError {
	if source == "" {
		return nil
	}
	var findings []FieldError
	for _, p := range poisonPatterns {
		locs := p.Regex.FindAllStringIndex(source, -1)
		for _, loc := range locs {
			start, end := loc[0], loc[1]
			line, col := lineColumn(source, start)
			findings = append(findings, FieldError{
				Step:    p.Step,
				Field:   "source_code",
				Message: p.Message,
				Line:    line,
				Snippet: snippet(source, start, end, col),
			})
		}
	}
	return findings
}

// ScanSourceOrError is the same as ScanSource but returns the
// findings wrapped in a *PayloadValidationError when non-empty.
// Handlers can then use errors.As to extract a structured reason
// list without re-wrapping.
func ScanSourceOrError(source string) error {
	findings := ScanSource(source)
	if len(findings) == 0 {
		return nil
	}
	return &PayloadValidationError{Errors: findings}
}

// lineColumn returns the 1-indexed line and column for a byte
// offset. Treats \n as the only line terminator (no CRLF
// normalisation — most editors store LF in Git-tracked source, and
// CRLF would just inflate line numbers by 1 every 4 KB).
func lineColumn(s string, offset int) (int, int) {
	if offset < 0 || offset > len(s) {
		return 0, 0
	}
	line := 1
	col := 1
	for i := 0; i < offset; i++ {
		if s[i] == '\n' {
			line++
			col = 1
		} else {
			col++
		}
	}
	return line, col
}

// snippet returns a windowed view of the matched text, used in
// error messages. The window is centered on the match and includes
// the leading 16 bytes and trailing 16 bytes; the match itself is
// preserved verbatim. If the window is truncated, the edges get a
// "…" marker.
func snippet(s string, start, end, _ int) string {
	const window = 16
	lo := start - window
	hi := end + window
	if lo < 0 {
		lo = 0
	}
	if hi > len(s) {
		hi = len(s)
	}
	prefix := ""
	suffix := ""
	if lo > 0 {
		prefix = "…"
	}
	if hi < len(s) {
		suffix = "…"
	}
	// Collapse newlines so the snippet stays single-line.
	cleaned := strings.ReplaceAll(s[lo:hi], "\n", "⏎")
	return prefix + cleaned + suffix
}
