package main

// runWorkItemArgsForTest invokes handleWorkItem directly with the
// same shape the CLI router gives it. Returns the exit code.
//
// The CLI receives args after the "item" subcommand has already been
// stripped. handleWorkItem expects just the trailing slice.
func runWorkItemArgsForTest(args []string) int {
	return handleWorkItem(args)
}