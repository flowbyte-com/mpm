// doctor_flag_contract.go — the accepted-flag contract for `mpm doctor`.
//
// Truthfulness rule: a flag the CLI accepts must either materially
// implement the documented behaviour or be rejected explicitly.
// Accepting a flag and ignoring it is a defect, because the user
// reasonably concludes the work was performed.
//
// Four flags were previously accepted and discarded (handlers_doctor.go
// matched them in an empty switch arm). Each is now either wired to the
// implementation that already existed behind `mpm ops doctor`, or
// rejected with a message that says what to do instead.
//
// The option set is declared ONCE, in doctorOptions below. The same
// declaration registers the flags with the parser, renders the help
// page's option block, and provides the vocabulary for the "unknown
// option" rejection. There is no second list to keep in step — see
// clihelp.go for why that matters.
package main

import (
	"errors"
	"flag"
	"fmt"

	"github.com/flowbyte-com/mpm-core/usererror"
)

// doctorOpts is the parsed, validated flag set for one doctor run.
type doctorOpts struct {
	json     bool
	explain  bool
	deepScan bool
	fix      bool
}

// doctorUsage is the usage line the doctor advertises.
const doctorUsage = "mpm doctor [options]"

// doctorProse is the part of the doctor's help page that is not an
// option list: the combinations the flags form, and the ones they do
// not.
//
// The option block above cannot carry these, because they are facts
// about the command's SEMANTICS rather than about the shape of a flag —
// and an option list that mixes "an option" with "a valid combination
// of two options" is exactly what the old rejection message did wrong.
const doctorProse = `The deep-scan modes are mutually exclusive with each other and with --json.

  mpm doctor                      the standard report (read-only)
  mpm doctor --explain            the FTS5 query plan
  mpm doctor --deep-scan          the FTS/integrity audit (read-only)
  mpm doctor --deep-scan --fix    the audit, and the repairs it supports`

// doctorOptions declares every option `mpm doctor` accepts.
//
// The order is the order the help page shows them: the help option
// first (the router owns it), then the diagnostic modes, then the
// remediation flag, then the output-format flag.
//
// --fix carries its dependency in its own usage line, because that is
// where an operator looks for it. The parser enforces the dependency
// too; the usage line is what makes it discoverable.
func doctorOptions(o *doctorOpts) []OptionSpec {
	return []OptionSpec{
		HelpOption,
		{Name: "json", Aliases: []string{"j"}, Usage: "Output diagnostics as JSON", Kind: OptBool, Bool: &o.json},
		{Name: "explain", Usage: "Show the FTS5 query plan", Kind: OptBool, Bool: &o.explain},
		{Name: "deep-scan", Usage: "Run the detailed FTS/integrity audit", Kind: OptBool, Bool: &o.deepScan},
		{Name: "fix", Usage: "Repair the supported findings (requires --deep-scan)", Kind: OptBool, Bool: &o.fix},
	}
}

// printDoctorHelp renders the doctor's own help page.
//
// It is reached by `mpm doctor --help`, `mpm doctor -h` and
// `mpm help doctor` — all three, so they cannot disagree about what the
// command accepts.
func printDoctorHelp() int {
	o := &doctorOpts{}
	return PrintHelpPage(doctorUsage, doctorOptions(o), doctorProse)
}

// parseDoctorFlags validates and interprets the doctor argument list.
//
// Rejection rules, each with a deliberate reason:
//
//   - --all      rejected. There is no extended check set in this
//     architecture: `mpm doctor` runs every check DoctorService defines,
//     plus the model connectivity probes. A flag that widened nothing
//     would be decorative. The other --all in this CLI (`mpm help --all`)
//     means "print the long-form catalogue" and is unrelated.
//   - --fix      rejected on its own. Remediation is implemented only
//     for the deep-scan findings (soft-delete ghosts in memories_fts).
//     The standard report has no mutation path, so `--fix` alone would
//     imply repairs that were never attempted. Pair it with --deep-scan.
//   - --json     rejected alongside --explain/--deep-scan. Those modes
//     render their own output through fmt.Printf and have no structured
//     form; accepting the combination would silently discard --json.
//   - an UNRECOGNISED option is rejected with the full usage block and
//     a typo suggestion, rather than a bare "unknown flag" line. It was
//     `mpm doctor -help` that produced the report this file answers:
//     the rejection named a list that mixed individual options with a
//     valid combination, omitted the -j alias, and never mentioned
//     --help.
//
// Exit-code note: rejections return an error here, and handleDoctor maps
// that to exit 1, matching the pre-existing unknown-flag behaviour.
func parseDoctorFlags(args []string) (doctorOpts, error) {
	var o doctorOpts
	opts := doctorOptions(&o)

	// The router has already consumed -h/--help, so the help option is
	// advertised but never reaches here. It is still declared, so the
	// help page and the accepted set agree.

	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(discardWriter{})
	RegisterOptions(fs, opts)

	// --all is rejected FIRST, with its own message: it explains WHY it
	// is unsupported and names the supported alternative. Letting the
	// parse catch it would replace that explanation with the standard
	// library's "flag provided but not defined".
	if sawAll(args) {
		return o, fmt.Errorf("doctor: --all is not supported; " +
			"`mpm doctor` already runs every available check. " +
			"Use `mpm doctor --deep-scan` for the on-demand FTS/integrity audit")
	}

	// An unrecognised option is rejected BEFORE the parse, so the
	// rejection carries the full option block and a suggestion rather
	// than the standard library's terse message.
	if rc := RejectUnknownOptionInArgs("doctor", doctorUsage, opts, args); rc != 0 {
		return o, errOptionAlreadyReported
	}

	if err := fs.Parse(args); err != nil {
		return o, fmt.Errorf("doctor: %v", err)
	}

	// --fix is only meaningful alongside --deep-scan; see above.
	if o.fix && !o.deepScan {
		return o, fmt.Errorf("doctor: --fix requires --deep-scan; " +
			"the standard report has no remediation path. " +
			"Run `mpm doctor --deep-scan --fix` to clean soft-delete ghosts")
	}

	if o.json && (o.explain || o.deepScan) {
		return o, fmt.Errorf("doctor: --json cannot be combined with --explain or --deep-scan; " +
			"those modes render their own output and have no structured form")
	}

	// --explain and --deep-scan are each a complete audit mode with their
	// own output. Running both would silently drop one of them, which is
	// the exact class of dishonesty this contract exists to prevent, so
	// the pairing is rejected instead of silently ordered.
	if o.explain && o.deepScan {
		return o, fmt.Errorf("doctor: --explain and --deep-scan are separate audit modes; " +
			"run them one at a time")
	}

	return o, nil
}

// sawAll reports whether --all was present.
//
// --all is NOT registered on the FlagSet: it is rejected with its own
// message, and registering it would make it an accepted option, which
// it is not. It is detected here so the rejection can explain WHY it is
// unsupported rather than reporting it as unknown.
func sawAll(args []string) bool {
	for _, a := range args {
		if a == "--all" {
			return true
		}
	}
	return false
}

// errOptionAlreadyReported marks a rejection whose full report — the
// message, the usage block, the option list and the suggestion — has
// ALREADY been printed by the shared helper.
//
// The two doctor entry points (`mpm doctor` and `mpm ops doctor`) must
// not print a second, differently-worded line on top of it. This
// sentinel is how they know.
var errOptionAlreadyReported = errors.New("the option was rejected above")

// doctorFlagError renders a doctor rejection and returns the exit code.
//
// A rejection that already reported itself prints nothing more; every
// other rejection is printed as a one-line error.
func doctorFlagError(err error) int {
	if err == nil {
		return 0
	}
	if errors.Is(err, errOptionAlreadyReported) {
		return 1
	}
	return usererror.Error("%v", err)
}

// discardWriter swallows the standard library's own usage output.
//
// The flag package prints its usage to fs.Output() when a parse fails.
// The doctor renders its own, so the standard library's copy would be a
// second, differently-worded usage block on the same stream.
type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }
