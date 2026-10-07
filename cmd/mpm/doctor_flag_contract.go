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
package main

import "fmt"

// doctorOpts is the parsed, validated flag set for one doctor run.
type doctorOpts struct {
	json     bool
	explain  bool
	deepScan bool
	fix      bool
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
//
// Exit-code note: rejections return an error here, and handleDoctor maps
// that to exit 1, matching the pre-existing unknown-flag behaviour.
func parseDoctorFlags(args []string) (doctorOpts, error) {
	var o doctorOpts
	var sawAll bool

	for _, a := range args {
		switch a {
		case "--json", "-j":
			o.json = true
		case "--explain":
			o.explain = true
		case "--deep-scan":
			o.deepScan = true
		case "--fix":
			o.fix = true
		case "--all":
			sawAll = true
		default:
			return o, fmt.Errorf("doctor: unknown flag %q "+
				"(accepted: --json, --explain, --deep-scan, --deep-scan --fix)", a)
		}
	}

	if sawAll {
		return o, fmt.Errorf("doctor: --all is not supported; " +
			"`mpm doctor` already runs every available check. " +
			"Use `mpm doctor --deep-scan` for the on-demand FTS/integrity audit")
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
