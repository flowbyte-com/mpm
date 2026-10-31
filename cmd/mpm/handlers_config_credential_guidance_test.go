package main

// handlers_config_credential_guidance_test.go — onboarding must SAY what
// the two answers to the API-key prompt mean.
//
// THE DEFECT THIS FILE PINS
// --------------------------
// A profile's `api_key` is a LITERAL SECRET. It was never expanded, and
// it was never explained. An operator who wrote
//
//	"api_key": "$MINIMAX_API_KEY"
//
// got the literal string sent to the provider as a bearer token, while
// their real credential sat unused in the environment. The provider
// refused the placeholder, and the operator was told their credential
// was invalid.
//
// Two things were missing:
//
//  1. ONBOARDING SAID NOTHING. The prompt asked for "API key" and the
//     distinction between "a secret" and "the name of an environment
//     variable" was invisible at the prompt. An empty answer and a
//     typed answer are not the same kind of thing, and nothing at the
//     prompt said so.
//
//  2. THE STORED VALUE WAS NEVER INTERPRETED. Nothing in the substrate
//     distinguished a literal secret from a reference.
//
// The second is fixed in internal/core/synth/credential_reference.go.
// This file pins the first.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCredentialGuidance_NamesTheDeclaredVariableAndTheLiteralSecret
// is the behavioural guard.
//
// The guidance has to do three things at once:
//
//   - say that an EMPTY answer uses the environment variable, and NAME
//     it — an operator cannot act on "the environment variable" without
//     knowing which one;
//   - say that a typed value is stored as a LITERAL SECRET;
//   - say that the variable's name is a REFERENCE, not a secret.
//
// The third one is the one that was missing: "$MINIMAX_API_KEY" looks
// exactly like a secret and is exactly what breaks.
func TestCredentialGuidance_NamesTheDeclaredVariableAndTheLiteralSecret(t *testing.T) {
	out := captureStdout(t, func() {
		printCredentialGuidance("https://api.minimax.io/anthropic/v1")
	})

	if !strings.Contains(out, "MINIMAX_API_KEY") {
		t.Errorf("the guidance does not name the endpoint's declared credential variable:\n%s", out)
	}
	if !strings.Contains(out, "empty") {
		t.Errorf("the guidance does not say that an empty answer uses the environment variable:\n%s", out)
	}
	if !strings.Contains(out, "literal secret") {
		t.Errorf("the guidance does not say that a typed value is stored as a literal secret:\n%s", out)
	}
	if !strings.Contains(out, "reference") {
		t.Errorf("the guidance does not say that the variable's name is a reference rather than a secret:\n%s", out)
	}
}

// TestCredentialGuidance_NamesTheEndpointDeclaredVariable pins that the
// guidance names the variable THIS endpoint declares.
//
// The substrate's variable table is keyed off the endpoint's wire, so the
// name differs by endpoint — and a guidance that named the wrong one
// would send the operator to export a variable the endpoint does not
// read.
func TestCredentialGuidance_NamesTheEndpointDeclaredVariable(t *testing.T) {
	cases := []struct {
		name    string
		baseURL string
		want    string
	}{
		{"a MiniMax endpoint", "https://api.minimax.io/anthropic/v1", "MINIMAX_API_KEY"},
		{"an OpenAI endpoint", "https://api.openai.com/v1", "OPENAI_API_KEY"},
		{"a generic OpenAI-compatible endpoint", "http://127.0.0.1:11434/v1", "OAI_COMPAT_API_KEY"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := captureStdout(t, func() {
				printCredentialGuidance(tc.baseURL)
			})
			if !strings.Contains(out, tc.want) {
				t.Errorf("the guidance does not name %s, the variable this endpoint declares:\n%s",
					tc.want, out)
			}
			// And it must not name a variable this endpoint does NOT
			// declare: an operator who exported that would still have
			// no credential.
			for _, other := range []string{"MINIMAX_API_KEY", "OPENAI_API_KEY", "OAI_COMPAT_API_KEY"} {
				if other == tc.want {
					continue
				}
				if strings.Contains(out, other) {
					t.Errorf("the guidance names %s, which this endpoint does not declare:\n%s",
						other, out)
				}
			}
		})
	}
}

// TestCredentialGuidance_PrecedesEveryCredentialPrompt is the
// structural guard.
//
// The guidance is ONE function, so the three onboarding entry points
// cannot drift into saying three different things. But a shared helper
// that only one of them calls is not shared — so this test PARSES the
// command package and fails if any credential prompt is asked without
// the guidance first.
//
// It parses rather than greps, because a textual scan has to guess
// where a call's arguments end, and a wrong guess silently misses the
// very case the guard exists to catch.
func TestCredentialGuidance_PrecedesEveryCredentialPrompt(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parse the command package: %v", err)
	}

	checked := 0
	for _, pkg := range pkgs {
		for name, file := range pkg.Files {
			if !strings.HasSuffix(name, "handlers_config.go") {
				continue
			}
			// Collect the functions that call printCredentialGuidance.
			guided := map[*ast.FuncDecl]bool{}
			ast.Inspect(file, func(n ast.Node) bool {
				fn, ok := n.(*ast.FuncDecl)
				if !ok {
					return true
				}
				ast.Inspect(fn.Body, func(inner ast.Node) bool {
					call, ok := inner.(*ast.CallExpr)
					if !ok {
						return true
					}
					if isCallTo(call, "printCredentialGuidance") {
						guided[fn] = true
					}
					return true
				})
				return true
			})

			// Now find every credential prompt and require that its
			// enclosing function is one of the guided ones.
			ast.Inspect(file, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				if !isCallTo(call, "promptSecret") || len(call.Args) == 0 {
					return true
				}
				lit, ok := call.Args[0].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					return true
				}
				label := strings.Trim(lit.Value, `"`)
				if !strings.Contains(strings.ToLower(label), "api key") {
					return true
				}
				checked++
				enclosing := enclosingFunc(file, call)
				if enclosing == nil {
					t.Errorf("%s: a credential prompt is not inside a function", filepath.Base(name))
					return true
				}
				if !guided[enclosing] {
					t.Errorf("%s:%d: the credential prompt %q is asked without the credential guidance; "+
						"an operator cannot tell a literal secret from an environment-variable reference "+
						"unless the prompt says so",
						filepath.Base(name), fset.Position(call.Pos()).Line, label)
				}
				return true
			})
		}
	}

	if checked == 0 {
		t.Fatal("no credential prompt was found; the guard is vacuous")
	}
	if checked < 3 {
		t.Errorf("found %d credential prompt(s); the wizard, the embedding wizard and `profile add` "+
			"each ask for one", checked)
	}
}

// isCallTo reports whether the call is a call to the named function or
// method. It handles both a plain identifier (promptSecret(...)) and a
// selector (pkg.promptSecret(...)), because a textual scan would have to
// guess which shape a call has — and a wrong guess silently misses the
// very case a guard exists to catch.
func isCallTo(call *ast.CallExpr, name string) bool {
	switch fun := call.Fun.(type) {
	case *ast.Ident:
		return fun.Name == name
	case *ast.SelectorExpr:
		return fun.Sel.Name == name
	}
	return false
}

// enclosingFunc returns the function declaration that contains pos.
func enclosingFunc(file *ast.File, pos ast.Node) *ast.FuncDecl {
	var found *ast.FuncDecl
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		if fn.Pos() <= pos.Pos() && pos.Pos() <= fn.End() {
			found = fn
		}
	}
	return found
}
