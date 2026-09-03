package main

import (
	"flag"
	"fmt"
	"os"

	mpminternal "github.com/flowbyte-com/mpm-core"
)

// MigrateEmbeddingsCmd implements `mpm ops migrate-embeddings`.
// The --undo flag is reserved for Task 13 (undo path); the struct field
// is present for forward-compatibility but the forward path is all that
// is implemented here.
type MigrateEmbeddingsCmd struct {
	UndoTimestamp string // reserved for Task 13 undo implementation
}

// Run executes the embedding migration or the undo path.
func (c *MigrateEmbeddingsCmd) Run() int {
	dm := getDBConcrete()
	if dm == nil {
		return 1
	}
	defer dm.Close()

	if c.UndoTimestamp != "" {
		if err := mpminternal.UndoMigration(dm, c.UndoTimestamp); err != nil {
			fmt.Fprintf(os.Stderr, "migrate-embeddings --undo: %v\n", err)
			return 1
		}
		fmt.Println("Migration undone.")
		return 0
	}

	// Check idempotency before running.
	if already, err := mpminternal.IsAlreadyApplied(dm); err != nil {
		fmt.Fprintf(os.Stderr, "migrate-embeddings: idempotency check: %v\n", err)
		return 1
	} else if already {
		fmt.Println("Migration already applied.")
		return 0
	}

	if err := mpminternal.RunMigration(dm); err != nil {
		fmt.Fprintf(os.Stderr, "migrate-embeddings: %v\n", err)
		return 1
	}
	fmt.Println("Migration applied.")
	return 0
}

// handleMigrateEmbeddings is the CLI entry point for `mpm ops migrate-embeddings`.
func handleMigrateEmbeddings(args []string) int {
	fs := flag.NewFlagSet("migrate-embeddings", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Println("Usage: mpm ops migrate-embeddings [flags]")
		fmt.Println("\nFlags:")
		fmt.Println("  --undo <timestamp>   Undo a migration (reserved for Task 13)")
	}
	if err := fs.Parse(args); err != nil {
		return 1
	}

	cmd := &MigrateEmbeddingsCmd{
		UndoTimestamp: "",
	}

	// Parse --undo if present.
	// flag package doesn't handle non-flag args elegantly for subcommands,
	// so we scan for --undo ourselves.
	for i := 0; i < len(args); i++ {
		if args[i] == "--undo" && i+1 < len(args) {
			cmd.UndoTimestamp = args[i+1]
			break
		}
	}

	return cmd.Run()
}
