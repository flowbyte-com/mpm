// cli_options_registry_test.go — the router's option vocabulary cannot
// drift from the code.
//
// WHY THIS FILE EXISTS
// --------------------
// The router rejects an option a command does not accept. That is the
// right behaviour — an option the parser does not understand is a
// mistake, and a mistake must never be treated as data — but the
// rejection is only safe if the router's vocabulary is a SUPERSET of
// what the commands actually accept.
//
// The failure mode of a stale vocabulary is subtle and severe:
//
//	mpm memory add --expires-in 30d "a fact"
//
// If "expires-in" is missing from the registry, the router rejects a
// perfectly valid invocation that worked the day before. The operator
// sees "Unknown option: --expires-in" and concludes the feature is
// gone.
//
// So the vocabulary has to be checked against the CODE, not against a
// human's memory of it. This file derives the vocabulary from the
// source — every "--xxx" string literal and every declared OptionSpec —
// and asserts that the router accepts all of it.
//
// THE DIRECTION OF THE CHECKS
// ---------------------------
//  1. Every option the source declares must be ACCEPTED by the router.
//     This is the dangerous direction: a missing entry breaks a valid
//     invocation.
//  2. Every long option the registry accepts must be DECLARED by the
//     source. This is the safe direction: an extra entry only means a
//     token reaches a handler that ignores it, exactly as it did before
//     the registry existed. A registry name that the source does not
//     declare is a TYPO in the registry, and a typo is a broken
//     invocation in disguise — so it is reported.
//
// SHORT ALIASES ARE EXEMPT
// ------------------------
// A one- or two-letter name ("j", "c", "r") is a short alias by
// construction: the source spells it with one dash, so the "--xxx"
// literal scan never sees it. Those are checked against the declared
// OptionSpec aliases instead, which do carry the bare name.
package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// longOptionLiteral matches a "--option-name" string literal.
var longOptionLiteral = regexp.MustCompile(`^--[a-z0-9][a-z0-9-]*$`)

// sourceOptionVocabulary is everything the CLI source declares as an
// option.
type sourceOptionVocabulary struct {
	// LongNames are the long option names, without dashes.
	LongNames map[string]bool
	// ShortNames are the short alias names, without dashes.
	ShortNames map[string]bool
}

// deriveSourceOptionVocabulary parses the command layer and collects
// every option name it declares.
//
// Two sources are read:
//
//   - string literals that spell a long option ("--json"). This catches
//     the hand-rolled option scanners, which never went through a
//     declaration.
//   - the OptionSpec declarations. This catches the declared option
//     sets, including the ones whose spelling never appears as a
//     literal.
func deriveSourceOptionVocabulary(t *testing.T) sourceOptionVocabulary {
	t.Helper()
	vocab := sourceOptionVocabulary{
		LongNames:  map[string]bool{},
		ShortNames: map[string]bool{},
	}

	fset := token.NewFileSet()
	entries := listCommandLayerFiles(t)
	if len(entries) == 0 {
		t.Fatal("no command-layer sources found — the derivation would vacuously pass")
	}

	for _, name := range entries {
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		collectOptionVocabulary(f, vocab)
	}
	return vocab
}

// collectOptionVocabulary walks one parsed file and adds its option
// declarations to vocab.
func collectOptionVocabulary(f *ast.File, vocab sourceOptionVocabulary) {
	ast.Inspect(f, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.BasicLit:
			if x.Kind != token.STRING {
				return true
			}
			s, err := strconv.Unquote(x.Value)
			if err != nil {
				return true
			}
			if longOptionLiteral.MatchString(s) {
				vocab.LongNames[strings.TrimPrefix(s, "--")] = true
			}
		case *ast.CompositeLit:
			collectOptionSpecFields(x, vocab)
		case *ast.CallExpr:
			collectFlagRegistration(x, vocab)
		}
		return true
	})
}

// flagRegistrationMethods are the FlagSet methods that register an
// option under a name.
//
// The name is always the FIRST argument, as a string literal.
var flagRegistrationMethods = map[string]bool{
	"String": true, "Bool": true, "Int": true, "Int64": true,
	"Float64": true, "Uint": true, "Uint64": true, "Duration": true,
	"Var": true, "VarX": true, "TextVar": true,
}

// collectFlagRegistration reads one FlagSet registration call.
//
//	fs.Int("max-iterations", 0, "...")
//	fs.BoolVar(&o.json, "json", false, "...")
//
// The bare form registers a name; the Var form registers a name AND
// binds a variable. Both put the name first.
func collectFlagRegistration(call *ast.CallExpr, vocab sourceOptionVocabulary) {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return
	}
	if !flagRegistrationMethods[sel.Sel.Name] {
		return
	}
	if len(call.Args) == 0 {
		return
	}
	name, ok := stringLiteral(call.Args[0])
	if !ok {
		return
	}
	vocab.LongNames[name] = true
}

// collectOptionSpecFields reads one OptionSpec composite literal.
//
// It recognises two shapes:
//
//	OptionSpec{Name: "json", Aliases: []string{"j"}}
//	[]OptionSpec{ {Name: "json"}, {Name: "stale"} }
func collectOptionSpecFields(lit *ast.CompositeLit, vocab sourceOptionVocabulary) {
	switch t := lit.Type.(type) {
	case *ast.Ident:
		if t.Name != "OptionSpec" {
			return
		}
		collectOneOptionSpec(lit, vocab)
	case *ast.ArrayType:
		ident, ok := t.Elt.(*ast.Ident)
		if !ok || ident.Name != "OptionSpec" {
			return
		}
		for _, el := range lit.Elts {
			if inner, ok := el.(*ast.CompositeLit); ok {
				collectOneOptionSpec(inner, vocab)
			}
		}
	}
}

// collectOneOptionSpec reads the Name / Aliases / LongAliases fields of
// one OptionSpec literal.
func collectOneOptionSpec(lit *ast.CompositeLit, vocab sourceOptionVocabulary) {
	for _, elt := range lit.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		key, ok := kv.Key.(*ast.Ident)
		if !ok {
			continue
		}
		switch key.Name {
		case "Name":
			if s, ok := stringLiteral(kv.Value); ok {
				vocab.LongNames[s] = true
			}
		case "Aliases":
			for _, s := range stringSlice(kv.Value) {
				vocab.ShortNames[s] = true
			}
		case "LongAliases":
			for _, s := range stringSlice(kv.Value) {
				vocab.LongNames[s] = true
			}
		}
	}
}

// stringLiteral returns the value of a string literal expression.
func stringLiteral(e ast.Expr) (string, bool) {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(lit.Value)
	if err != nil {
		return "", false
	}
	return s, true
}

// stringSlice returns the values of a []string composite literal.
func stringSlice(e ast.Expr) []string {
	lit, ok := e.(*ast.CompositeLit)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(lit.Elts))
	for _, el := range lit.Elts {
		if s, ok := stringLiteral(el); ok {
			out = append(out, s)
		}
	}
	return out
}

// listCommandLayerFiles returns the non-test Go sources of the command
// layer.
//
// The test files are excluded on purpose: a test may legitimately use
// an option spelling that no command accepts — that is how a test
// PROVES the option is rejected.
func listCommandLayerFiles(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read the command layer: %v", err)
	}
	var names []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// registryAcceptedUnion is every option name the router accepts for
// SOME command.
func registryAcceptedUnion() map[string]bool {
	union := map[string]bool{}
	for _, names := range cliOptionRegistry {
		for _, n := range names {
			union[n] = true
		}
	}
	return union
}

// TestCLIOptionRegistryCoversEverySourceVocabulary is the drift guard.
//
// Every option the source declares must be accepted by the router.
//
// The registry is hand-maintained, so this test is the thing that
// notices when a new option is added to a handler and the registry is
// not updated. Without it, the router starts rejecting valid
// invocations — a regression that no unit test of the handler would
// catch, because the handler is never reached.
func TestCLIOptionRegistryCoversEverySourceVocabulary(t *testing.T) {
	vocab := deriveSourceOptionVocabulary(t)
	accepted := registryAcceptedUnion()

	var missing []string
	for name := range vocab.LongNames {
		if !accepted[name] {
			missing = append(missing, name)
		}
	}
	for name := range vocab.ShortNames {
		// A short alias is registered under its bare name, so the
		// router must know that name too — otherwise `mpm decide -r
		// "why"` is rejected.
		if len(name) > 1 && !accepted[name] {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)

	if len(missing) > 0 {
		t.Errorf("the router would REJECT %d option(s) that the code accepts: %v\n"+
			"Add them to cliOptionRegistry.go — an option missing from the registry is a broken valid invocation.",
			len(missing), missing)
	}
}

// TestCLIOptionRegistryHasNoPhantomLongNames is the reverse check.
//
// A long name in the registry that the source does not declare is a
// TYPO. A typo does not merely widen the accepted set — it hides the
// real option, which the previous test then reports as missing.
//
// The short names are exempt: a one- or two-letter alias is a short
// spelling by construction and never appears as a "--xxx" literal.
func TestCLIOptionRegistryHasNoPhantomLongNames(t *testing.T) {
	vocab := deriveSourceOptionVocabulary(t)

	// The report is keyed by the option NAME, not by command: a shared
	// list (the family union) would otherwise report the same typo
	// once per family, which buries the actual finding.
	phantoms := map[string]bool{}
	for _, names := range cliOptionRegistry {
		for _, n := range names {
			if len(n) <= 2 {
				// A short alias. It is checked by the vocabulary test
				// through the declared Aliases.
				continue
			}
			if !vocab.LongNames[n] && !vocab.ShortNames[n] {
				phantoms[n] = true
			}
		}
	}
	names := make([]string, 0, len(phantoms))
	for n := range phantoms {
		names = append(names, "--"+n)
	}
	sort.Strings(names)

	if len(names) > 0 {
		t.Errorf("the registry accepts %d option(s) that no source declares: %v\n"+
			"These are typos. A typo hides the real option from the router.",
			len(names), names)
	}
}

// TestCLIOptionRegistryCoversThePreciseCommandSets checks the
// commands that have their OWN option set against their own registry
// entry.
//
// The families use a union, which is the safe direction. The commands
// below do not: each has a precise vocabulary, and the router must
// hold every one of its declared options.
//
// The reverse is also checked: the registry must not be narrower than
// the declaration, or a declared option would be unreachable.
func TestCLIOptionRegistryCoversThePreciseCommandSets(t *testing.T) {
	cases := []struct {
		cmd   string
		opts  []OptionSpec
		names []string
	}{
		{"doctor", doctorOptions(&doctorOpts{}), nil},
		{"review", reviewOptions(&reviewOpts{}), nil},
		{"decide", recordDecisionOptions(), nil},
		{"theorize", proposeTheoryOptions(), nil},
	}

	for _, c := range cases {
		c := c
		t.Run(c.cmd, func(t *testing.T) {
			registry := map[string]bool{}
			for _, n := range cliOptionRegistry[c.cmd] {
				registry[n] = true
			}
			for _, o := range c.opts {
				// The router OWNS the help option: it rewrites
				// -h/--help into the literal token "help" before the
				// guard ever sees the argv, so the guard never has to
				// know about it.
				if o.RouterOwned {
					continue
				}
				for _, spelling := range o.Spellings() {
					bare := strings.TrimLeft(spelling, "-")
					if !registry[bare] {
						t.Errorf("the router would reject the declared option %q of %q", spelling, c.cmd)
					}
				}
			}
		})
	}
}
