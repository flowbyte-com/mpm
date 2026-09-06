// cli_args_json.go — canonical CLI `--json` / `-j` flag extraction.
//
// Stage S2 of the approved CLI refactor (see
// docs/archive/mpm-cli-overhaul-investigation-2026-09-06.md §9). One
// place that defines how the CLI recognises the machine-output flag.
//
// This file replaces four duplicate patterns that existed across the
// codebase as of 2026-09-06:
//
//	1. ExtractJSONFlag (router.go:1310) — the original helper, now
//	   promoted here.
//	2. stripMemoryFlagToken — a per-handler variadic scanner that
//	   handled --json / -j as a special case.
//	3. Manual `case "--json", "-j"` switches inside handler-level
//	   pre-scan loops (handlers_epistemology, ops_*, drill_report).
//	4. `fs.Bool("json", false, ...)` declared on a stdlib flag.FlagSet
//	   in handlers that ALSO call ExtractJSONFlag (the BOTH pattern);
//	   the FlagSet declaration was dead code that was overwritten by
//	   the ExtractJSONFlag result.
//
// Per the S2 plan, the canonical contract is:
//
//   - Recognised tokens: `--json` and `-j` (exact match).
//   - Forms NOT supported (intentionally — keep narrow to match
//     pre-S2 behaviour for the 22 existing ExtractJSONFlag callers):
//     `--json=true`, `--json=false`, `--json=value`, `--json <value>`.
//     Handlers that previously accepted those forms via stdlib flag
//     or stripMemoryFlagToken retain their existing behaviour; the
//     migration does not change the contract for handlers that were
//     NOT using ExtractJSONFlag before.
//   - Does NOT match `foo=json` or any token containing the substring
//     "json" — exact match only, per the S2 plan's "distinguish an
//     actual --json flag from unrelated strings containing json"
//     requirement.
//   - Repeats: every occurrence of `--json` or `-j` sets the result
//     to true (idempotent); all are removed from the returned slice.
//   - Order: preserved on the returned slice. Original slice is never
//     mutated.
//   - Allocations: always one allocation for the returned slice (sized
//     to len(args)); no allocation when no flag is present in spirit
//     (the helper still constructs an empty slice — the caller can
//     detect no-strip via `len(cleaned) == len(args)`).
//
// Interaction with S1 (`splitOnDashDash`): the canonical split for the
// `--` separator remains owned by the caller. ExtractJSONFlag does not
// know about the separator — callers that also honour the `--` POSIX
// convention must invoke `splitOnDashDash` first and pass only the
// flags-bearing portion to ExtractJSONFlag. Conversely, tokens after
// the separator are NEVER scanned for `--json` (they are raw positional
// input per S1 semantics). See `TestExtractJSONFlag_PreservedS1Separator`
// for the cross-helper invariant.
package main

// ExtractJSONFlag scans args for `--json` or `-j` (exact match),
// removes every occurrence from the slice, and returns
// (jsonRequested, cleanedArgs).
//
// jsonRequested is true iff at least one `--json` or `-j` token
// appeared in args. cleanedArgs contains args in their original
// order with every `--json` / `-j` removed. The original slice is
// never mutated.
//
// This is the canonical CLI `--json` extractor for handlers that
// also parse other flags via a hand-rolled pre-scan switch (the
// dominant pattern across cmd/mpm/). Handlers that delegate entirely
// to stdlib `flag.NewFlagSet` continue to use `fs.Bool("json", false, ...)`
// directly — that path does not duplicate any logic in this helper.
func ExtractJSONFlag(args []string) (jsonRequested bool, cleanedArgs []string) {
	jsonRequested = false
	cleanedArgs = make([]string, 0, len(args))
	for _, arg := range args {
		if arg == "--json" || arg == "-j" {
			jsonRequested = true
			continue
		}
		cleanedArgs = append(cleanedArgs, arg)
	}
	return jsonRequested, cleanedArgs
}