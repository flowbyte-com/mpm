// cli_args_separator.go — shared POSIX `--` separator helper.
//
// Stage S1 of the approved CLI refactor (see
// docs/archive/mpm-cli-overhaul-investigation-2026-09-06.md §9). One
// place that defines how `--` ends option processing for every CLI
// command. This addresses the historical class where legitimate
// content beginning with dashes was interpreted as an unknown flag
// (D3 defect, fixed locally in handlers_memory.go before this stage).
//
// Convention (POSIX, getopt(3), pflag, Go stdlib flag): once the
// literal token `--` is encountered, every subsequent token is raw
// positional input and must not be parsed as a flag. Only the first
// `--` is the separator — any further `--` tokens belong to raw and
// are returned verbatim.
//
// The router-level `parseFlags` (router.go:474) does NOT handle this
// separator; it only rewrites four global tokens (`-h`/`--help`/
// `-v`/`--version`/`-f`/`--force`/`-i`/`--interactive`). Per-handler
// flag pre-scans are responsible for honouring the separator before
// their flag switch runs. This helper is the single canonical split.
package main

// splitOnDashDash returns (flags, raw) per POSIX `--` separator
// convention.
//
// flags holds every token before the first `--` and is intended for
// flag parsing by the caller. raw holds every token after the first
// `--` and must be treated as positional content — never parsed as a
// flag. If no `--` appears in the input, raw is nil and flags equals
// the original slice (no allocation).
//
// Repeated `--` tokens after the first are preserved in raw. Only
// the first `--` ends option processing; subsequent `--` tokens are
// just raw input. This matches getopt(3), pflag, and Go stdlib flag.
//
// Examples:
//
//	splitOnDashDash(["--json", "--", "-foo"]) → (["--json"], ["-foo"])
//	splitOnDashDash(["--json", "-j"])        → (["--json", "-j"], nil)
//	splitOnDashDash(["--"])                  → ([], nil)
//	splitOnDashDash(["--", "--", "-foo"])    → ([], ["--", "-foo"])
//	splitOnDashDash([])                      → ([], nil)
//
// The caller is responsible for joining `raw` into the eventual
// content payload. The helper does not mutate the input slice or
// perform any allocation when no separator is present; when a
// separator IS present, the two returned slices are slices into the
// original backing array (no copy).
func splitOnDashDash(args []string) (flags, raw []string) {
	for i, a := range args {
		if a == "--" {
			// Per POSIX, the separator itself is consumed.
			// Subsequent `--` tokens remain in raw (they are content,
			// not another separator).
			return args[:i], args[i+1:]
		}
	}
	return args, nil
}