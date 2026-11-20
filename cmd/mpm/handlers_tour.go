// cmd/mpm/handlers_tour.go — mpm tour, Wave 4 of the cognitive-interface RFC.
//
// The tour's job is to take a new operator through the cognitive
// verbs in the order the RFC prescribes. Each step:
//
//   1. Reminds them what they're about to see.
//   2. Shows the command they'd run.
//   3. Demonstrates by invoking the command with sample arguments
//      (using --demo) so the operator sees real output.
//
// Without --demo, the tour is dry — it shows the commands and the
// operator can choose to run them. With --demo, every step actually
// exercises the command. The two modes coexist for a reason: a
// fresh-install tour wants demo (the operator can see the system
// work). A re-tour of an existing system often wants dry (the
// operator knows what each command does; they just want a refresher).
//
// TTY awareness: when stdin is a terminal, the tour pauses for an
// enter key between steps. When stdin is not a terminal (CI, agent
// log capture), the tour emits all steps in a single stream — no
// blocking.
//
// Architecture: handlers_tour.go is the only file involved in Wave 4.
// The tour composes existing handlers (handleRemember, handleRecall,
// etc.) via handleRemember-style internal calls. It does NOT
// duplicate logic, does NOT own storage, does NOT own rendering.
// Wave 4 is the last cognitive-interface RFC wave; per the RFC's
// sequencing rule, the tour lands only after the surface has
// stabilised across at least one deploy cycle.

package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// tourStep is one row in the mpm tour walkthrough.
//
//   Title: the cognitive verb being demonstrated (e.g., "remember")
//   Description: one-line explanation of what it does
//   Command: the exact command an operator types
//   DemoArg: when non-empty, the tour passes this as the first arg
//            to handle<Verb>(args) when --demo is set. The demo
//            output is captured-to-stdout-and-streamed inline.
type tourStep struct {
	Title       string
	Description string
	Command     string
	DemoArg     string
}

// tourSteps is the canonical cognitive-verb walkthrough. The order
// matches the RFC §'Wave 4' sketch: remember → recall → learn →
// decide → continue → why.
//
// Adding a new step here is the only change needed to extend the
// tour. The handler iterates the slice and dispatches each step
// through the same prompt-demo-pause loop.
var tourSteps = []tourStep{
	{
		Title:       "remember",
		Description: "Store something. The most fundamental cognitive verb — your first memory lands in the substrate.",
		Command:     `mpm remember "<something worth keeping>"`,
		DemoArg:     "tour-remember: my first memory, captured during the mpm tour onboarding.",
	},
	{
		Title:       "recall",
		Description: "Search memories by meaning. Hybrid FTS + semantic ranking. Type any phrase that should retrieve the memory you just stored.",
		Command:     `mpm recall "<phrase from what you remembered>"`,
		DemoArg:     "first memory",
	},
	{
		Title:       "learn",
		Description: "Curate a lesson. Lessons are distilled insights — the kind of thing you'd put in a personal wiki.",
		Command:     `mpm learn "<a curated insight>"`,
		DemoArg:     "tour-learn: mpm tour is the recommended way to onboard a new operator.",
	},
	{
		Title:       "decide",
		Description: "Record a decision with rationale. The substrate remembers the context, the choice, and the reasoning — so future-you can re-litigate it.",
		Command:     `mpm decide --context "<why now>" --choice "<what>" --rationale "<why this choice>"`,
		DemoArg:     `context="tour step" choice="continue" rationale="because the cognitive interface is now stable across waves 1-4"`,
	},
	{
		Title:       "continue",
		Description: "Resume work. mpm continue composes your working context, wake context, memory stats — one screen, every morning.",
		Command:     `mpm continue`,
		DemoArg:     "",
	},
	{
		Title:       "why",
		Description: "Trace provenance. mpm why <id> shows why an artifact exists — its evidence chain, confidence timeline, retrieval strength.",
		Command:     `mpm why <id>`,
		DemoArg:     "",
	},
}

// handleTour runs the mpm tour walkthrough. Flags:
//
//   --demo      Auto-runs each step's demo argument (skips manual trial).
//   --step <n>  Jump to step n (1-indexed); useful for re-running a step.
//
// TTY detection: when stdin is a TTY AND --demo is NOT set, the
// tour pauses for an enter keypress between steps. When stdin is
// not a TTY (e.g., CI, agent log capture), the tour emits all steps
// without blocking.
//
// Exit code 0 on success; 1 if the operator aborts (Ctrl-C).
func handleTour(args []string) int {
	demo := false
	stepNum := 0
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--demo":
			demo = true
		case "--step":
			if i+1 < len(args) {
				fmt.Sscanf(args[i+1], "%d", &stepNum)
				i++
			}
		}
	}

	tty := isatty(os.Stdin)
	interactive := tty && !demo

	out := os.Stdout
	fmt.Fprintln(out, renderWelcome(demo, stepNum))
	fmt.Fprintln(out)

	reader := bufio.NewReader(os.Stdin)
	for i, step := range tourSteps {
		if stepNum > 0 && (i+1) != stepNum {
			continue
		}
		fmt.Fprintln(out, renderStepHeader(i+1, len(tourSteps), step.Title))
		fmt.Fprintln(out, "  "+step.Description)
		fmt.Fprintln(out)
		fmt.Fprintln(out, "    "+renderCommand(step.Command))

		if demo {
			fmt.Fprintln(out)
			fmt.Fprintln(out, renderDemo("Demo: invoking the command with the sample argument"))
			fmt.Fprintln(out, renderDemoDivider())
			runTourDemo(step)
			fmt.Fprintln(out, renderDemoDivider())
		}

		if interactive {
			fmt.Fprintln(out)
			fmt.Fprintf(out, "    %s",
				lipgloss.NewStyle().Faint(true).Foreground(lipgloss.Color("#999999")).
					Render("(press enter to continue, Ctrl-C to abort)"))
			_, err := reader.ReadString('\n')
			if err != nil {
				fmt.Fprintln(out)
				fmt.Fprintln(out, renderAborted())
				return 1
			}
			fmt.Fprintln(out)
		} else {
			fmt.Fprintln(out)
		}
	}
	fmt.Fprintln(out, renderComplete(tourSteps, len(tourSteps)))
	return 0
}

// runTourDemo invokes the cognitive verb the step is teaching,
// with the step's DemoArg as input. Output streams to stdout,
// preserving the operator's expected command-shape experience.
//
// Today the inline args are limited because some handlers parse
// flags from --flag-value pairs while others expect positional.
// The demo args below match what each handler already accepts.
func runTourDemo(step tourStep) {
	switch step.Title {
	case "remember":
		runDemo("remember", func() int { return handleRemember(strings.Fields(step.DemoArg)) })
	case "recall":
		runDemo("recall", func() int { return handleRecall(strings.Fields(step.DemoArg)) })
	case "learn":
		runDemo("learn", func() int { return handleLearn(strings.Fields(step.DemoArg)) })
	case "decide":
		// decide accepts flags context=..., choice=..., rationale=...
		// Build the proper field slice preserving the key=value tokens.
		args := splitKeyValueArgs(step.DemoArg)
		runDemo("decide", func() int { return handleDecide(args) })
	case "continue":
		runDemo("continue", func() int { return handleContinue(nil) })
	case "why":
		// Pull the most recent memory id from the substrate and use it
		// as the demo argument. If none exists (cold install), print a
		// guidance message instead of failing the tour.
		runDemo("why", func() int {
			dm := getDBConcrete()
			if dm == nil {
				fmt.Fprintln(os.Stdout, "  (no database — skipping why demo)")
				return 0
			}
			var id string
			row := dm.QueryRowTracked(
				`SELECT id FROM memories WHERE deleted_at IS NULL ORDER BY created_at DESC LIMIT 1`,
			)
			if err := row.Scan(&id); err != nil || id == "" {
				fmt.Fprintln(os.Stdout, "  (no memories yet — run 'mpm remember' first, then 'mpm why <id>')")
				return 0
			}
			handleWhy([]string{id})
			return 0
		})
	default:
		fmt.Fprintln(os.Stdout,
			lipgloss.NewStyle().Faint(true).Foreground(lipgloss.Color("#999999")).
				Render("(no demo handler for "+step.Title+")"))
	}
}

// runDemo runs fn and writes a thin "what happened" footer so the
// operator can see whether the demo succeeded.
func runDemo(name string, fn func() int) {
	rc := fn()
	if rc == 0 {
		fmt.Fprintln(os.Stdout,
			lipgloss.NewStyle().Foreground(lipgloss.Color("#84")).
				Render(fmt.Sprintf("(✓ mpm %s demo succeeded)", name)))
	} else {
		fmt.Fprintln(os.Stdout,
			lipgloss.NewStyle().Foreground(lipgloss.Color("#213")).
				Render(fmt.Sprintf("(mpm %s demo exited %d — non-fatal; the tour continues)", name, rc)))
	}
}

// splitKeyValueArgs splits a string like `key1="v1" key2="v2"` into
// a slice of separate tokens suitable for flag.Parse-style handlers.
// Handles quoted values with spaces inside.
func splitKeyValueArgs(s string) []string {
	var out []string
	var cur strings.Builder
	inQuotes := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == ' ' && !inQuotes:
			if cur.Len() > 0 {
				out = append(out, cur.String())
				cur.Reset()
			}
		case c == '"':
			inQuotes = !inQuotes
			// Keep the quote chars intact for flag parses.
			cur.WriteByte(c)
		default:
			cur.WriteByte(c)
		}
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

// TTY-aware renderers — pure functions; no behaviour beyond
// string-construction so unit tests can cover them cheaply.

func renderWelcome(demo bool, stepNum int) string {
	mode := "interactive"
	if demo {
		mode = "demo"
	}
	if stepNum > 0 {
		mode = fmt.Sprintf("step-%d", stepNum)
	}
	gold := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#ffb700"))
	muted := lipgloss.NewStyle().Faint(true).Foreground(lipgloss.Color("#999999"))
	return gold.Render("Welcome to MPM") + "\n" + muted.Render(fmt.Sprintf(
		"An interactive walkthrough of the cognitive verbs. Mode: %s.", mode))
}

func renderStepHeader(num, total int, title string) string {
	gold := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#ffb700"))
	faint := lipgloss.NewStyle().Faint(true).Foreground(lipgloss.Color("#999999"))
	head := gold.Render(fmt.Sprintf("Step %d of %d —", num, total))
	verb := gold.Render(title)
	hint := faint.Render(fmt.Sprintf("(%s is the cognitive verb)", title))
	return head + " " + verb + " " + hint
}

func renderCommand(c string) string {
	cyan := lipgloss.NewStyle().Foreground(lipgloss.Color("87"))
	return cyan.Render("$ " + c)
}

func renderDemo(s string) string {
	gold := lipgloss.NewStyle().Foreground(lipgloss.Color("#ffb700"))
	return gold.Render(s)
}

func renderDemoDivider() string {
	faint := lipgloss.NewStyle().Faint(true).Foreground(lipgloss.Color("#999999"))
	return faint.Render("  ───")
}

func renderComplete(steps []tourStep, total int) string {
	gold := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#ffb700"))
	muted := lipgloss.NewStyle().Faint(true).Foreground(lipgloss.Color("#999999"))
	titles := make([]string, 0, len(steps))
	for _, s := range steps {
		titles = append(titles, s.Title)
	}
	return gold.Render("★ Tour complete.") + "\n" + muted.Render(fmt.Sprintf(
		"You've seen: %s. Run `mpm help` for the cognitive-verb index.",
		strings.Join(titles, ", ")))
}

func renderAborted() string {
	gold := lipgloss.NewStyle().Foreground(lipgloss.Color("#ffb700"))
	return gold.Render("Tour aborted. Resume any time with `mpm tour --step N`.")
}

// (io imported for compile-time reservation in case future print
// variants stream partial lines; today stdout writes are direct.)
var _ = io.Discard
