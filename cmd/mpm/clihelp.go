// clihelp.go — the shared option contract.
//
// THE PROBLEM
// -----------
// A CLI command has two facts about its options:
//
//   - the options it ACCEPTS, decided by its parser;
//   - the options it ADVERTISES, decided by its help text.
//
// Maintained separately, they drift. The drift has three shapes, and
// MPM had all three:
//
//  1. ADVERTISED BUT NOT ACCEPTED. `mpm doctor --help` printed the
//     command's one-line description, which mentioned --json,
//     --explain and --deep-scan but never listed them, never showed
//     that -j is the alias of --json, and never showed that --fix
//     requires --deep-scan. The operator had to read the prose and
//     guess.
//
//  2. ACCEPTED BUT NOT ADVERTISED — the dangerous direction.
//     `mpm decide --anything` recorded a DECISION whose content was
//     "--anything". `mpm gc --anything` ran the decay sweep. `mpm
//     backup --anything` wrote a backup file named "--anything". The
//     flag was not rejected; it was treated as DATA.
//
//  3. REJECTED WITHOUT GUIDANCE. `mpm doctor -help` said
//     `unknown flag "-help" (accepted: --json, --explain, --deep-scan,
//     --deep-scan --fix)` — a list that mixes individual options with a
//     valid COMBINATION, omits the -j alias, and never mentions
//     --help.
//
// THE FIX
// -------
// One declaration per command, doing BOTH jobs. An OptionSpec says how
// an option is spelled, what it means, and where its parsed value
// lands. The same declaration:
//
//   - registers the flag with the parser (long name AND aliases, all
//     bound to one variable, so `-j` and `--json` are the same option);
//   - renders the help page's option block;
//   - provides the vocabulary for the "unknown option" rejection and its
//     typo suggestion.
//
// There is no second vocabulary to keep in step, so the advertised set
// cannot drift from the accepted set. The invariant is not a convention
// anyone has to remember — it is a consequence of the structure.
//
// THE ROUTER OWNS --help
// ----------------------
// `-h` and `--help` are intercepted by the router (CommandRouter.Execute)
// before any handler sees them, because a help request must never reach
// a mutating handler. So a command's OWN flag set does not register
// them — but its help page still has to ADVERTISE them, and the
// advertised set must still be the accepted set.
//
// The resolution: the router's help options are a canonical, shared
// OptionSpec (HelpOption). A command includes it in its list for
// display, and Register skips it because the router already owns it.
// The advertised set therefore equals the accepted set, including the
// help options, without the router and the handler both trying to
// register the same flag.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/flowbyte-com/mpm-core/usererror"
)

// OptionKind is the value shape an option carries.
type OptionKind int

const (
	// OptBool is a flag that takes no value.
	OptBool OptionKind = iota
	// OptInt is a flag that takes an integer value.
	OptInt
	// OptString is a flag that takes a string value.
	OptString
)

// OptionSpec is one declared option.
//
// It is the single source of truth for: the spelling, the alias set, the
// help text, the value's destination, and the help page's rendering of
// the option.
type OptionSpec struct {
	// Name is the LONG name, without dashes: "json".
	Name string
	// Aliases are alternative SHORT spellings, each without dashes:
	// ["j"]. An alias is registered as its own flag bound to the SAME
	// variable, so the parser accepts either and the value is read from
	// one place.
	Aliases []string
	// LongAliases are alternative LONG spellings, each without dashes:
	// ["validation-criteria"]. They are rendered with two dashes, which
	// matters when a short spelling is already taken: the router owns
	// "-h" for help, so a command whose own option was once spelled
	// "-h" has to advertise the reachable "--h" instead.
	LongAliases []string
	// Usage is the one-line description shown in the help page.
	Usage string
	// Kind is the value shape.
	Kind OptionKind
	// Bool, Int and String are the destinations. Exactly one is set,
	// matching Kind.
	Bool   *bool
	Int    *int
	String *string
	// ValueName is the placeholder shown for a non-boolean option's
	// value ("N", "PATH"). Defaults to "VALUE".
	ValueName string
	// ValueForm overrides how the value is rendered after the option's
	// spelling — e.g. "=N" for an option the parser only accepts in the
	// `--name=N` form. It exists so the help page can show the form the
	// parser ACTUALLY accepts: an option advertised as `--max-age N`
	// when the parser reads only `--max-age=N` is a help page that lies.
	ValueForm string
	// DefaultText is appended to the usage line when set — e.g.
	// "(default 30)". Left empty when the default is the zero value and
	// saying so would only be noise.
	DefaultText string
	// RouterOwned marks an option the router handles before the handler
	// sees it (currently only the help options). Such an option is
	// ADVERTISED but not REGISTERED by the command's own flag set.
	RouterOwned bool
}

// HelpOption is the canonical `-h, --help` option.
//
// It is RouterOwned: the router intercepts `-h` and `--help` before any
// handler runs, because a help request must never reach a mutating
// handler. Every command's help page advertises it, so the advertised
// option set equals the accepted option set on every command.
var HelpOption = OptionSpec{
	Name:        "help",
	Aliases:     []string{"h"},
	Usage:       "Show help",
	Kind:        OptBool,
	RouterOwned: true,
}

// Register declares the option on fs.
//
// The long name and every alias are registered as separate flags bound
// to the SAME variable. Go's flag package therefore accepts `-j`,
// `--json` and `--j` alike, and all three write to one destination —
// which is what makes "the accepted options" a single fact rather than
// three.
//
// A RouterOwned option is skipped: the router already handles it.
func (o OptionSpec) Register(fs *flag.FlagSet) {
	if o.RouterOwned {
		return
	}
	switch o.Kind {
	case OptBool:
		if o.Bool != nil {
			fs.BoolVar(o.Bool, o.Name, *o.Bool, o.Usage)
			for _, a := range o.Aliases {
				fs.BoolVar(o.Bool, a, *o.Bool, o.Usage)
			}
			for _, a := range o.LongAliases {
				fs.BoolVar(o.Bool, a, *o.Bool, o.Usage)
			}
		}
	case OptInt:
		if o.Int != nil {
			fs.IntVar(o.Int, o.Name, *o.Int, o.Usage)
			for _, a := range o.Aliases {
				fs.IntVar(o.Int, a, *o.Int, o.Usage)
			}
			for _, a := range o.LongAliases {
				fs.IntVar(o.Int, a, *o.Int, o.Usage)
			}
		}
	case OptString:
		if o.String != nil {
			fs.StringVar(o.String, o.Name, *o.String, o.Usage)
			for _, a := range o.Aliases {
				fs.StringVar(o.String, a, *o.String, o.Usage)
			}
			for _, a := range o.LongAliases {
				fs.StringVar(o.String, a, *o.String, o.Usage)
			}
		}
	}
}

// RegisterOptions declares every option on fs.
func RegisterOptions(fs *flag.FlagSet, opts []OptionSpec) {
	for _, o := range opts {
		o.Register(fs)
	}
}

// valueName is the placeholder shown after a non-boolean option.
func (o OptionSpec) valueName() string {
	if o.ValueName != "" {
		return o.ValueName
	}
	return "VALUE"
}

// spelling is the option's canonical display form: "-j, --json" when it
// has aliases, "--json" when it does not.
//
// The short aliases come first, then the long ones, then the canonical
// name — the order a reader scans.
func (o OptionSpec) spelling() string {
	parts := make([]string, 0, len(o.Aliases)+len(o.LongAliases)+1)
	for _, a := range o.Aliases {
		parts = append(parts, "-"+a)
	}
	for _, a := range o.LongAliases {
		parts = append(parts, "--"+a)
	}
	parts = append(parts, "--"+o.Name)
	if len(parts) == 1 {
		return parts[0]
	}
	return strings.Join(parts, ", ")
}

// Spellings returns every token that selects this option: its short
// aliases, its long aliases, and its canonical long name.
//
// The DASHES are part of the spelling. "-h" and "--h" are different
// tokens, and a command may legitimately accept both — keying a
// duplicate check on the bare name would report a false collision.
func (o OptionSpec) Spellings() []string {
	out := make([]string, 0, len(o.Aliases)+len(o.LongAliases)+1)
	for _, a := range o.Aliases {
		out = append(out, "-"+a)
	}
	for _, a := range o.LongAliases {
		out = append(out, "--"+a)
	}
	out = append(out, "--"+o.Name)
	return out
}

// RenderOptions writes the option block of a help page.
//
//	-h, --help     Show help
//	-j, --json     Output diagnostics as JSON
//	--deep-scan    Run detailed integrity audit
//
// Each option appears EXACTLY ONCE, with its aliases on the same line —
// a duplicated line per alias reads as two different options.
//
// Dependencies and mutually exclusive combinations are NOT encoded here.
// They belong in the command's own prose, because they are facts about
// the command's SEMANTICS, not about the shape of a flag.
func RenderOptions(w io.Writer, opts []OptionSpec) {
	// The label column is as wide as the widest label — the spelling
	// AND the value placeholder — so the descriptions line up. A label
	// whose placeholder is wider than the spelling must still reserve
	// the room, or the descriptions after it are ragged.
	labels := make([]string, len(opts))
	width := 0
	for i, o := range opts {
		label := o.spelling()
		if o.ValueForm != "" {
			label += o.ValueForm
		} else if o.Kind != OptBool {
			label += " " + o.valueName()
		}
		labels[i] = label
		if n := len(label); n > width {
			width = n
		}
	}
	for i, o := range opts {
		usage := o.Usage
		if o.DefaultText != "" {
			usage += " (" + o.DefaultText + ")"
		}
		fmt.Fprintf(w, "  %-*s  %s\n", width, labels[i], usage)
	}
}

// OptionSpellings returns every accepted spelling of every option, in
// declaration order. It is the vocabulary the "unknown option" message
// and the typo suggestion work from.
func OptionSpellings(opts []OptionSpec) []string {
	out := make([]string, 0, len(opts)*2)
	for _, o := range opts {
		out = append(out, "--"+o.Name)
		for _, a := range o.Aliases {
			out = append(out, "-"+a)
		}
		for _, a := range o.LongAliases {
			out = append(out, "--"+a)
		}
	}
	return out
}

// SuggestOptionName is SuggestOption for a plain list of accepted
// option names.
//
// The router's guard does not have an OptionSpec set for every command,
// so it cannot use SuggestOption. The three-step preference is the
// same: exact match, then an edit distance of 2 or less, then a
// prefix.
func SuggestOptionName(accepted []string, given string) string {
	bare := strings.TrimLeft(given, "-")
	if bare == "" {
		return ""
	}

	// 1. exact match.
	for _, n := range accepted {
		if n == bare {
			return "--" + n
		}
	}

	// 2. edit distance.
	best, bestDist := "", 3
	for _, n := range accepted {
		if d := editDistance(bare, n); d < bestDist {
			best, bestDist = n, d
		}
	}
	if best != "" {
		return "--" + best
	}

	// 3. prefix.
	for _, n := range accepted {
		if strings.HasPrefix(n, bare) {
			return "--" + n
		}
	}
	return ""
}

// SuggestOption returns the option the operator most likely meant, or
// the empty string when nothing is close enough to guess.
//
// The order of preference:
//
//  1. the bare name (dashes stripped) matches a known option exactly —
//     this is what turns `-help` into `--help`;
//  2. the bare name is within an edit distance of 2 of a known option —
//     the classic typo;
//  3. the bare name is a prefix of a known option.
func SuggestOption(opts []OptionSpec, given string) string {
	bare := strings.TrimLeft(given, "-")
	if bare == "" {
		return ""
	}

	// 1. exact match on the bare name.
	for _, o := range opts {
		if o.Name == bare {
			return "--" + o.Name
		}
		for _, a := range o.Aliases {
			if a == bare {
				return "-" + a
			}
		}
		for _, a := range o.LongAliases {
			if a == bare {
				return "--" + a
			}
		}
	}

	// 2. edit distance.
	best, bestDist := "", 3
	for _, o := range opts {
		cands := append([]string{o.Name}, o.Aliases...)
		for _, cand := range cands {
			if d := editDistance(bare, cand); d < bestDist {
				best, bestDist = cand, d
			}
		}
	}
	if best != "" {
		for _, o := range opts {
			if o.Name == best {
				return "--" + o.Name
			}
			for _, a := range o.Aliases {
				if a == best {
					return "-" + a
				}
			}
			for _, a := range o.LongAliases {
				if a == best {
					return "--" + a
				}
			}
		}
	}

	// 3. prefix.
	for _, o := range opts {
		if strings.HasPrefix(o.Name, bare) {
			return "--" + o.Name
		}
		for _, a := range o.Aliases {
			if strings.HasPrefix(a, bare) {
				return "-" + a
			}
		}
		for _, a := range o.LongAliases {
			if strings.HasPrefix(a, bare) {
				return "--" + a
			}
		}
	}
	return ""
}

// editDistance is the Levenshtein distance between two short strings.
// It is used only for the typo suggestion, so the straightforward
// dynamic-programming implementation is the right trade.
func editDistance(a, b string) int {
	ar, br := []rune(a), []rune(b)
	if len(ar) == 0 {
		return len(br)
	}
	if len(br) == 0 {
		return len(ar)
	}
	prev := make([]int, len(br)+1)
	cur := make([]int, len(br)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ar); i++ {
		cur[0] = i
		for j := 1; j <= len(br); j++ {
			cost := 1
			if ar[i-1] == br[j-1] {
				cost = 0
			}
			cur[j] = min3(cur[j-1]+1, prev[j]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(br)]
}

func min3(a, b, c int) int {
	if b < a {
		a = b
	}
	if c < a {
		a = c
	}
	return a
}

// RejectUnknownOption prints the full rejection for an unrecognised
// option and returns the exit code (1).
//
//	❌ Unknown option: -help
//
//	Usage: mpm doctor [options]
//
//	Options:
//	  -h, --help     Show help
//	  -j, --json     Output diagnostics as JSON
//
//	Tip: Did you mean --help?
//
// Everything goes to stderr, in that order, because this is an ERROR
// report and an error report that interleaves with the command's real
// output is unreadable.
//
// The exit code is 1: an unrecognised option is the operator's mistake
// and the command did not do what they asked. It is never 0 — a command
// that ignores an option it does not understand has silently done
// something else, which is the defect this whole file exists to remove.
func RejectUnknownOption(cmdPath string, usage string, opts []OptionSpec, given string) int {
	w := usererror.GetWriter()

	fmt.Fprintf(w, "❌ Unknown option: %s\n", given)
	fmt.Fprintln(w)
	if usage != "" {
		fmt.Fprintf(w, "Usage: %s\n", usage)
		fmt.Fprintln(w)
	}
	if len(opts) > 0 {
		fmt.Fprintln(w, "Options:")
		RenderOptions(w, opts)
		fmt.Fprintln(w)
	}
	if s := SuggestOption(opts, given); s != "" {
		fmt.Fprintf(w, "Tip: Did you mean %s?\n", s)
	}
	return 1
}

// RejectUnknownOptionInArgs scans args for the first token that looks
// like an option and is not one the command accepts, and rejects it.
//
// It exists for the commands that read their arguments POSITIONALLY
// rather than through a flag.FlagSet — the ones where an unrecognised
// flag used to be consumed as data. `mpm decide --anything` recorded a
// decision; `mpm gc --anything` ran the decay sweep; `mpm backup
// --anything` wrote a file named "--anything".
//
// The scan is deliberately narrow, because it runs on commands whose
// positionals may legitimately begin with a dash:
//
//   - a bare "-" is stdin, not an option;
//   - "--" terminates the options and everything after it is data;
//   - a token that does not begin with "-" is data;
//   - a negative number is a value, not an option.
//
// known is the set of accepted option spellings, WITHOUT dashes ("json",
// "j"). The empty string means the command accepts no options at all, in
// which case ANY option-looking token is a mistake.
//
// A RouterOwned option is deliberately NOT known here. The router
// consumes the EXACT tokens "-h" and "--help" before the handler runs,
// so the handler never sees them — and any OTHER spelling of help
// ("-help", "--h") was never accepted by anything. Leaving them out of
// this set is what lets `mpm doctor -help` be rejected with the
// suggestion "--help" instead of being silently swallowed.
func RejectUnknownOptionInArgs(cmdPath string, usage string, opts []OptionSpec, args []string) int {
	return rejectUnknownOptionInArgs(cmdPath, usage, opts, args, false)
}

// RejectUnknownOptionInArgsFreeText is RejectUnknownOptionInArgs for a
// command whose positional argument is FREE TEXT.
//
// The difference is where the scan stops. For a strict command every
// dash-prefixed token is an option candidate, wherever it appears. For
// a free-text command the scan stops at the FIRST POSITIONAL, because
// everything after it is content:
//
//	mpm decide We should use --strict here
//
// The trailing "--strict" is part of the decision's text, not an
// option. The leading option run is still policed, so `mpm decide
// --anything` is still rejected — which is the defect this whole
// mechanism exists to fix.
func RejectUnknownOptionInArgsFreeText(cmdPath string, usage string, opts []OptionSpec, args []string) int {
	return rejectUnknownOptionInArgs(cmdPath, usage, opts, args, true)
}

func rejectUnknownOptionInArgs(cmdPath string, usage string, opts []OptionSpec, args []string, freeText bool) int {
	known := make(map[string]bool, len(opts)*2)
	for _, o := range opts {
		if o.RouterOwned {
			continue
		}
		known[o.Name] = true
		for _, a := range o.Aliases {
			known[a] = true
		}
	}
	for _, a := range args {
		if a == "--" {
			return 0
		}
		if a == "-" || !strings.HasPrefix(a, "-") {
			// A positional. For a free-text command it ends the
			// scan: everything after it is content.
			if freeText {
				return 0
			}
			continue
		}
		name := strings.TrimLeft(a, "-")
		if name == "" {
			continue
		}
		if eq := strings.Index(name, "="); eq >= 0 {
			name = name[:eq]
		}
		if name == "" {
			continue
		}
		// A negative number is a value, not an option.
		if isNumericToken(name) {
			continue
		}
		if known[name] {
			continue
		}
		return RejectUnknownOption(cmdPath, usage, opts, a)
	}
	return 0
}

// isNumericToken reports whether the token is a number. Used to keep a
// negative number from being read as an option.
func isNumericToken(s string) bool {
	if s == "" {
		return false
	}
	seenDigit := false
	for i, c := range s {
		switch {
		case c >= '0' && c <= '9':
			seenDigit = true
		case (c == '-' || c == '+') && i == 0:
		case c == '.':
		default:
			return false
		}
	}
	return seenDigit
}

// PrintHelpPage writes a command's help page to stdout.
//
// The page is: the usage line, the option block, and the command's own
// prose. It goes to STDOUT because a help page is requested output, not
// an error — `mpm doctor --help | less` has to work.
func PrintHelpPage(usage string, opts []OptionSpec, prose string) int {
	w := os.Stdout
	if usage != "" {
		fmt.Fprintf(w, "Usage: %s\n", usage)
	}
	if len(opts) > 0 {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "Options:")
		RenderOptions(w, opts)
	}
	if prose != "" {
		fmt.Fprintln(w)
		fmt.Fprintln(w, prose)
	}
	return 0
}
