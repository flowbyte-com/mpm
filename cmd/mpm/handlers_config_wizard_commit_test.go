// handlers_config_wizard_commit_test.go — HAL-9001 Finding 4.
//
// The interactive wizard had exactly one commit point: the final
// "Save?" confirmation. One step broke that contract. The embedding
// step called config.SaveConfig itself, in the MIDDLE of the wizard.
//
// The consequence is not cosmetic. An operator who configured an
// embedding endpoint and then declined the final save was left with a
// configuration on disk they had declined — and the wizard's own
// closing message said "Aborted." while their configuration had in
// fact changed. Nothing about the flow told them.
//
// These tests pin the contract:
//
//   - no wizard STEP writes; the final save is the only commit;
//   - the wizard as a whole commits exactly once;
//   - that commit happens AFTER the operator approves.
package main

import (
	"bufio"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"

	"github.com/flowbyte-com/mpm-core/config"
)

// wizardStepFunctions are the wizard's STEPS: the functions that run
// before the final "Save?" decision.
//
// None of them may write the configuration. A step that writes commits
// the operator's configuration before they have agreed to it.
var wizardStepFunctions = []string{
	"wizardEmbeddingStep",
	"wizardEmbeddingCustom",
	"wizardEmbeddingCustomWith",
	"wizardCriticStep",
	"wizardShowCurrentState",
	"wizardShowCapabilities",
	"wizardShowSynthesis",
}

// parseWizardSource parses the wizard's source file.
//
// The test runs with the package directory as its working directory,
// which is where `go test` puts it.
func parseWizardSource(t *testing.T) *ast.File {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "handlers_config.go", nil, 0)
	if err != nil {
		t.Fatalf("parse handlers_config.go: %v", err)
	}
	return file
}

// findFunc returns the named function declaration, or nil.
func findFunc(file *ast.File, name string) *ast.FuncDecl {
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok && fn.Name.Name == name {
			return fn
		}
	}
	return nil
}

// isConfigSaveCall reports whether a call expression is config.SaveConfig.
//
// It matches the call by its shape — config.SaveConfig — rather than
// by its text, so a comment or a string mentioning the function is not
// counted as a write.
func isConfigSaveCall(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	recv, ok := sel.X.(*ast.Ident)
	if !ok {
		return false
	}
	return recv.Name == "config" && sel.Sel.Name == "SaveConfig"
}

// saveConfigCallsIn returns the positions of the config.SaveConfig
// calls inside a function body.
func saveConfigCallsIn(fn *ast.FuncDecl) []token.Pos {
	if fn == nil {
		return nil
	}
	var out []token.Pos
	ast.Inspect(fn, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if ok && isConfigSaveCall(call) {
			out = append(out, call.Pos())
		}
		return true
	})
	return out
}

// TestWizardStepsDoNotWrite pins the save semantics.
//
// The embedding step used to write. Declining the final "Save?" then
// left the operator with a configuration they had declined.
func TestWizardStepsDoNotWrite(t *testing.T) {
	file := parseWizardSource(t)
	for _, name := range wizardStepFunctions {
		fn := findFunc(file, name)
		if fn == nil {
			t.Errorf("wizard step %q no longer exists; the step list must track the wizard", name)
			continue
		}
		if n := len(saveConfigCallsIn(fn)); n != 0 {
			t.Errorf("wizard step %q writes the configuration %d time(s); a step that runs before the "+
				"final \"Save?\" decision commits a configuration the operator may decline. "+
				"Only the final save may write.", name, n)
		}
	}
}

// TestWizardCommitsExactlyOnce pins that the wizard itself writes
// once: at the end, after the operator has approved.
func TestWizardCommitsExactlyOnce(t *testing.T) {
	file := parseWizardSource(t)
	fn := findFunc(file, "handleConfigInteractive")
	if fn == nil {
		t.Fatal("handleConfigInteractive no longer exists")
	}
	if n := len(saveConfigCallsIn(fn)); n != 1 {
		t.Errorf("handleConfigInteractive writes the configuration %d time(s), want exactly 1 — "+
			"the wizard must have a single commit point", n)
	}
}

// TestWizardSaveFollowsTheConfirmation pins the ORDER, not just the
// count: the write must come after the operator's approval.
//
// A save that happens before the confirmation is a save the operator
// did not ask for.
func TestWizardSaveFollowsTheConfirmation(t *testing.T) {
	file := parseWizardSource(t)
	fn := findFunc(file, "handleConfigInteractive")
	if fn == nil {
		t.Fatal("handleConfigInteractive no longer exists")
	}

	confirmAt := token.NoPos
	ast.Inspect(fn, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		ident, ok := call.Fun.(*ast.Ident)
		if !ok || ident.Name != "confirmPrompt" {
			return true
		}
		for _, a := range call.Args {
			lit, ok := a.(*ast.BasicLit)
			if ok && lit.Kind == token.STRING && strings.Contains(lit.Value, "Save?") {
				confirmAt = call.Pos()
			}
		}
		return true
	})
	if confirmAt == token.NoPos {
		t.Fatal("the wizard no longer asks for a \"Save?\" confirmation")
	}

	saves := saveConfigCallsIn(fn)
	if len(saves) != 1 {
		t.Fatalf("the wizard saves the configuration %d time(s), want 1", len(saves))
	}
	if saves[0] < confirmAt {
		t.Errorf("the wizard saves the configuration BEFORE it asks for confirmation "+
			"(save at %v, confirmation at %v)", saves[0], confirmAt)
	}
}

// TestWizardEmbeddingStepWritesNothing is the behavioural companion
// to the source-level checks above: the step is driven with a scripted
// session and the configuration file is inspected afterwards.
//
// The script selects the Ollama protocol, so no credential is
// prompted for and the session needs no secret.
func TestWizardEmbeddingStepWritesNothing(t *testing.T) {
	isolatedEmbeddingWorkspace(t)
	c := &config.Config{}

	session := "2\nnomic-embed-text\nhttp://127.0.0.1:11434\n"
	wizardEmbeddingCustomWith(bufio.NewReader(strings.NewReader(session)), c)

	if _, ok := c.Profiles["embedding"]; !ok {
		t.Fatalf("the wizard step did not create an embedding profile; profiles: %v", c.Profiles)
	}
	if _, err := os.Stat(config.ConfigPath()); err == nil {
		t.Errorf("the wizard step wrote %s before the final save", config.ConfigPath())
	}
}

// TestWizardEmbeddingStepIsIdempotent pins that re-running the step
// produces the same configuration rather than compounding it.
//
// A rerun is the normal way an operator corrects a typo, so the step
// must not accumulate state.
func TestWizardEmbeddingStepIsIdempotent(t *testing.T) {
	isolatedEmbeddingWorkspace(t)
	c := &config.Config{}

	session := "2\nnomic-embed-text\nhttp://127.0.0.1:11434\n"
	wizardEmbeddingCustomWith(bufio.NewReader(strings.NewReader(session)), c)
	first := c.Profiles["embedding"]

	wizardEmbeddingCustomWith(bufio.NewReader(strings.NewReader(session)), c)
	second := c.Profiles["embedding"]

	if first != second {
		t.Errorf("re-running the embedding step changed the profile:\nfirst:  %+v\nsecond: %+v",
			first, second)
	}
	if len(c.Profiles) != 1 {
		t.Errorf("re-running the embedding step created extra profiles: %v", c.Profiles)
	}
}
