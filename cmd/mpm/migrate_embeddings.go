package main

import (
	"flag"
	"fmt"

	mpminternal "github.com/flowbyte-com/mpm-core"
	"github.com/flowbyte-com/mpm-core/usererror"
)

// MigrateEmbeddingsCmd implements `mpm ops migrate-embeddings`.
// The --undo flag reverts a previously-applied migration.
type MigrateEmbeddingsCmd struct {
	UndoTimestamp string
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
			return usererror.Errorf(fmt.Sprintf("migrate-embeddings --undo: %v", err))
		}
		fmt.Println("Migration undone.")
		return 0
	}

	// Check idempotency before running.
	if already, err := mpminternal.IsAlreadyApplied(dm); err != nil {
		return usererror.Errorf(fmt.Sprintf("migrate-embeddings: idempotency check: %v", err))
	} else if already {
		fmt.Println("Migration already applied.")
		return 0
	}

	if err := mpminternal.RunMigration(dm); err != nil {
		return usererror.Errorf(fmt.Sprintf("migrate-embeddings: %v", err))
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
		fmt.Println("  --undo <timestamp>   Undo a previously-applied migration")
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
