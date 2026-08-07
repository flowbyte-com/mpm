// cmd/mpm-lint — the consolidated static analyzer.
//
// Replaces the eight standalone audit binaries (audit-scans, audit-closes,
// audit-tx, audit-ctx, audit-go, audit-mutex, audit-sql, audit-fd). Every
// rule's classification logic now lives in internal/audit and shares one
// walk: each .go file under the roots is parsed exactly once, and all
// selected rules run concurrently over the same ASTs.
//
// Report and gate output match the old binaries class-for-class:
//
//	mpm-lint                          # markdown report, all rules, stdout
//	mpm-lint --rule=sql,fd            # only the named rules
//	mpm-lint --json                   # JSON document on stdout
//	mpm-lint --gate                   # exit non-zero when a class exceeds
//	                                  # its threshold (stderr verdict)
//	mpm-lint --roots=a,b              # comma-separated root dirs
//	mpm-lint --include-tests          # also scan _test.go files
//
// Threshold flags follow the old per-auditor flag names, namespaced by
// rule: --max-<rule>-<class> (e.g. --max-sql-built, --max-fd-no-close).
// The defaults replicate each old binary's defaults exactly: fatal classes
// default to 0 (zero tolerance), review classes default to -1 (ratchet to
// the measured baseline so existing reviewed sites stay exempt while any
// NEW site fails the commit).
//
// The pre-commit hook runs `mpm-lint --gate` instead of the six separate
// audit gates; --rule is not needed there since all rules gate by default.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/flowbyte-com/mpm/internal/audit"
)

// defaultThresholds mirrors each old auditor's gate defaults:
// 0 = zero-tolerance fatal class; -1 = ratchet to measured baseline
// (ResolveRatchet). Keyed rule -> class -> default.
var defaultThresholds = map[string]map[string]int{
	"scans": {
		audit.ClassSilentContinue: 0,
		audit.ClassFakeHardFail:   0,
		audit.ClassNoCheck:        0,
	},
	"closes": {
		audit.QClassNoClose:       0,
		audit.QClassRowsDiscarded: 0,
		audit.QClassLoopDefer:     0,
		audit.QClassUnknown:       0,
	},
	"tx": {
		audit.TXClassExplicitRollback: 0,
		audit.TXClassCommitOnly:       0,
		audit.TXClassNoRollback:       0,
		audit.TXClassUnknown:          0,
	},
	"ctx": {
		audit.CtxInScopeDrop:  0,
		audit.CtxPlainMissing: -1,
		audit.CtxVoid:         -1,
		audit.CtxUnknown:      0,
	},
	"go": {
		audit.GoDetached: 0,
		audit.GoUnknown:  0,
	},
	"mutex": {
		audit.MuExplicitUnlock: -1,
		audit.MuNoUnlock:       0,
		audit.MuUnknown:        0,
	},
	"sql": {
		audit.SQLBuilt:   0,
		audit.SQLUnknown: -1,
		audit.SQLFmtSafe: -1,
		audit.SQLConst:   -1,
	},
	"fd": {
		audit.FDNoClose:       0,
		audit.FDRemoveOnly:    0,
		audit.FDUnknown:       -1,
		audit.FDExplicitClose: -1,
	},
	"imports": {
		audit.ImportsForbidden: 0,
	},
}

// allRules builds the rule set in the canonical order.
func allRules() []audit.Rule {
	return []audit.Rule{
		audit.NewScansRule(),
		audit.NewClosesRule(),
		audit.NewTXRule(),
		audit.NewCtxRule(),
		audit.NewGoRule(),
		audit.NewMutexRule(),
		audit.NewSQLRule(),
		audit.NewFDRule(),
		audit.NewImportsRule(),
	}
}

func main() {
	var (
		jsonOut      = flag.Bool("json", false, "emit JSON document instead of markdown")
		includeTests = flag.Bool("include-tests", false, "scan _test.go files too")
		rootsFlag    = flag.String("roots", "internal,cmd", "comma-separated root dirs to walk")
		jsonSidecar  = flag.String("json-out", "", "if set, also write JSON to this path")
		gate         = flag.Bool("gate", false, "exit non-zero when a class exceeds its threshold")
		rulesFlag    = flag.String("rule", "", "comma-separated rule names to run (default: all)")
	)

	// Per-class threshold flags, namespaced by rule and replicating the old
	// per-auditor flags (--max-sql-built, --max-fd-no-close, ...). Class
	// names that already carry the rule prefix (tx-*, sql-*, ...) get the
	// prefix stripped so flags stay readable: --max-tx-commit-only.
	thresholdFlags := map[string]*int{}
	for rule, classes := range defaultThresholds {
		for class := range classes {
			tail := strings.TrimPrefix(class, rule+"-")
			name := "max-" + rule + "-" + tail
			thresholdFlags[name] = flag.Int(name, defaultThresholds[rule][class], "threshold for "+class)
		}
	}

	flag.Parse()

	roots := strings.Split(*rootsFlag, ",")
	for i := range roots {
		roots[i] = strings.TrimSpace(roots[i])
	}

	rules := allRules()
	if *rulesFlag != "" {
		selected := map[string]bool{}
		for _, n := range strings.Split(*rulesFlag, ",") {
			selected[strings.TrimSpace(n)] = true
		}
		var filtered []audit.Rule
		for _, r := range rules {
			if selected[r.Name()] {
				filtered = append(filtered, r)
			}
		}
		rules = filtered
	}

	sitesByRule := audit.Run(roots, *includeTests, rules)

	if *jsonOut {
		emitJSON(os.Stdout, sitesByRule)
	} else {
		for _, r := range rules {
			r.EmitMarkdown(os.Stdout, sitesByRule[r.Name()])
		}
	}

	if *jsonSidecar != "" {
		f, err := os.Create(*jsonSidecar)
		if err != nil {
			fmt.Fprintf(os.Stderr, "mpm-lint: create %s: %v\n", *jsonSidecar, err)
			os.Exit(1)
		}
		emitJSON(f, sitesByRule)
		f.Close()
		fmt.Fprintf(os.Stderr, "mpm-lint: wrote JSON to %s\n", *jsonSidecar)
	}

	if *gate {
		exitCode := 0
		for _, r := range rules {
			max := map[string]int{}
			for class, def := range defaultThresholds[r.Name()] {
				max[class] = def
				tail := strings.TrimPrefix(class, r.Name()+"-")
				if v := thresholdFlags["max-"+r.Name()+"-"+tail]; v != nil {
					max[class] = *v
				}
			}
			checks := r.GateChecks(sitesByRule[r.Name()], max)
			if audit.RunGate(r.Name(), checks) != 0 {
				exitCode = 1
			}
		}
		os.Exit(exitCode)
	}
}

// emitJSON writes the per-rule document: {"<rule>": {total, sites}, ...}.
func emitJSON(w *os.File, sitesByRule map[string][]audit.Site) {
	doc := map[string]any{}
	for rule, sites := range sitesByRule {
		doc[rule] = struct {
			Total int          `json:"total"`
			Sites []audit.Site `json:"sites"`
		}{Total: len(sites), Sites: sites}
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(doc)
}
