// argparse.go — argument parsing that cannot silently discard a flag.
//
// The defect this exists to prevent
//
// Go's flag.Parse switches to positional-only mode at the first argument that
// does not begin with a dash. Everything after it becomes fs.Arg(1), fs.Arg(2)
// and so on — not an error, not a warning, just a quietly ignored flag:
//
//	mpm recall "some query" --semantic
//	mpm recall "some query" --limit 3
//	mpm recall "some query" --why
//
// all three run as plain lexical recall. The user's intent is dropped and the
// command succeeds, which is the worst possible failure mode: the output looks
// like a successful answer to a question that was answered differently than
// asked.
//
// This was not hypothetical. A benchmark compared `--semantic` against lexical
// recall and concluded that semantic search reinforced memory weights. It did
// not — the `--semantic` runs were lexical, because the flag was discarded.
// Both flag orders were used in the same experiment; only the flags-first order
// did anything.
//
// Why not just move the flags
//
// Pre-existing fixes for this took the form of hand-written pre-scan loops that
// hoisted a specific list of flags out of the argument list before calling
// flag.Parse. recall.go carried one for --json, --stale-days, --token-budget,
// --weight-below, --before and --projected. Every other flag it declared
// (--semantic, --limit, --why, --since, --until, --collection, --vector-weight,
// --as-of) kept the defect.
//
// The failure mode is structural, not a missing line: a hand-maintained list of
// flags drifts from the FlagSet as flags are added, and the newly-added flag is
// broken from the moment it is declared. Worse, an unknown flag passed straight
// through to flag.Parse after a positional never reaches it, so it is discarded
// without complaint.
//
// The contract here
//
//	parseArgsInterleaved honours every flag it recognises, in any position,
//	and rejects every flag it does not.
//
// Arity is read from the FlagSet rather than from a caller-maintained list, so
// it cannot drift. An unrecognised flag is an error naming the flag, not a
// silent no-op.

package main

import (
	"flag"
	"fmt"
	"strconv"
	"strings"
)

// parseArgsInterleaved parses args with fs, accepting flags before, after or
// interleaved with positional arguments. args must NOT include the program
// name.
//
// Returns an error for an unknown flag or a flag missing its value. Callers
// should print the message and their usage line and return a non-zero exit;
// usererror.Errorf is the usual choice.
//
// fs is populated as usual, and fs.Args() yields the positionals in their
// original order with the flags removed.
func parseArgsInterleaved(fs *flag.FlagSet, args []string) error {
	reordered, err := interleaveReorder(fs, args)
	if err != nil {
		return err
	}
	// Errors are already descriptive; flag.ErrHelp is passed through so the
	// caller can distinguish "user asked for help" from "user got it wrong".
	return fs.Parse(reordered)
}

// interleaveReorder rewrites args so every flag precedes every positional,
// which is the only shape flag.Parse accepts.
//
// A `--` terminator is inserted before the positionals. It is not optional:
// flag.Parse inspects every token itself, so a positional that happens to begin
// with a dash (a query like `-5 to 3`, or any query that mentions a flag name)
// would be re-rejected by the standard library even after we had correctly
// classified it. The terminator tells flag.Parse to stop looking.
//
// The rewrite is lossless: a caller cannot tell afterwards whether the user
// wrote `--limit 3` or `--limit=3`, or which side of the query it sat on.
func interleaveReorder(fs *flag.FlagSet, args []string) ([]string, error) {
	var flags, positionals []string

	i := 0
	for i < len(args) {
		arg := args[i]

		// `--` is the conventional terminator: everything after it is
		// positional, whatever it looks like. Without this, a query
		// beginning with a dash (or one that contains "--") is unparseable.
		if arg == "--" {
			positionals = append(positionals, args[i+1:]...)
			break
		}

		// Not a flag: a positional, or a bare "-" meaning stdin.
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			positionals = append(positionals, arg)
			i++
			continue
		}

		name := strings.TrimLeft(arg, "-")
		value := ""
		hasValue := false
		if eq := strings.Index(name, "="); eq >= 0 {
			value, hasValue = name[eq+1:], true
			name = name[:eq]
		}

		// A negative number is a value, not a flag. `mpm recall --limit -1`
		// and a bare `-5` must not both become "unknown flag: -5".
		if name == "" {
			positionals = append(positionals, arg)
			i++
			continue
		}
		if _, err := strconv.ParseFloat(name, 64); err == nil {
			positionals = append(positionals, arg)
			i++
			continue
		}

		f := fs.Lookup(name)
		if f == nil {
			// The whole point. flag.Parse would either never see this token
			// (it sits after a positional) or would treat it as a positional;
			// either way the user gets no indication their flag was ignored.
			return nil, fmt.Errorf("unknown flag: --%s", name)
		}

		if hasValue {
			flags = append(flags, "--"+name+"="+value)
			i++
			continue
		}

		// A boolean takes no separate argument, so `--json true` must leave
		// "true" as a positional rather than consuming it. This matches
		// flag.Parse's own behaviour, so reordering changes nothing for a
		// caller who wrote the flags in the canonical order.
		if bf, ok := f.Value.(interface{ IsBoolFlag() bool }); ok && bf.IsBoolFlag() {
			flags = append(flags, "--"+name)
			i++
			continue
		}

		if i+1 >= len(args) {
			return nil, fmt.Errorf("flag needs an argument: --%s", name)
		}
		flags = append(flags, "--"+name, args[i+1])
		i += 2
	}

	out := make([]string, 0, len(flags)+len(positionals)+1)
	out = append(out, flags...)
	out = append(out, "--")
	return append(out, positionals...), nil
}
