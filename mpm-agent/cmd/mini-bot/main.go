package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/chzyer/readline"
	_ "github.com/mattn/go-sqlite3"
	"mpm-agent/core"
)

// ============================================================================
// Persistence (stateless per-run CLI)
// ============================================================================

// cliPersistence implements core.Persistence for the CLI REPL.
// CLI is stateless per-run — no persisted session history.
type cliPersistence struct {
	sessionID string
}

func (p *cliPersistence) Get() ([]map[string]interface{}, error) { return nil, nil }
func (p *cliPersistence) Save([]map[string]interface{}) error     { return nil }
func (p *cliPersistence) SessionID() string                      { return p.sessionID }

// ============================================================================
// Identity Resolver
// ============================================================================

// identityResolverFunc wraps core identity functions.
type identityResolverFunc struct{}

func (i *identityResolverFunc) ResolveIdentityPath(binaryDir, configured string) string {
	return core.ResolveIdentityPath(binaryDir, configured)
}
func (i *identityResolverFunc) LoadIdentity(path string) (*core.Identity, error) {
	return core.LoadIdentity(path)
}

// ============================================================================
// Main
// ============================================================================

func main() {
	pid := os.Getpid()
	os.Args[0] = fmt.Sprintf("mini-bot[%d]", pid)

	// Resolve paths
	dbPath := core.ResolveMiniBotDBPath()
	binaryDir := core.GetBinaryDir()

	// Open database
	db, err := core.OpenDBForPath(dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mini-bot[%d]: cannot open db at %s: %v\n", pid, dbPath, err)
		os.Exit(1)
	}
	defer db.Close()

	// Load identity
	identity, _ := core.LoadIdentity(filepath.Join(binaryDir, "IDENTITY.md"))
	identityName, identityVersion := "unknown", "unknown"
	if identity != nil {
		identityName, identityVersion = identity.Name, identity.Version
	}
	fmt.Fprintf(os.Stderr, "mini-bot[%d]: identity: %s v%s | db: %s\n", pid, identityName, identityVersion, dbPath)

	// Create session runner
	sessionID := core.GenerateID()
	transport := NewTerminalTransport()
	persistence := &cliPersistence{sessionID: sessionID}
	identityResolver := &identityResolverFunc{}

	runner := core.NewSessionRunner(transport, sessionID, "chat", db, persistence, identityResolver)

	// Setup readline
	home, _ := os.UserHomeDir()
	historyPath := filepath.Join(home, ".mpm-agent-history")
	rl, err := readline.NewEx(&readline.Config{
		Prompt:          "mpm> ",
		HistoryFile:     historyPath,
		InterruptPrompt: "^C",
		EOFPrompt:       "exit",
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "mini-bot[%d]: readline: %v\n", pid, err)
		os.Exit(1)
	}
	defer rl.Close()

	// Ensure history file exists
	dir := filepath.Dir(historyPath)
	os.MkdirAll(dir, 0700)

	// Context for cancellation
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handle SIGINT gracefully
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigChan
		cancel()
		rl.Close()
	}()

	// Multiline state
	var multilineBuffer strings.Builder
	inMultiline := false

	// REPL loop
	for {
		line, err := rl.Readline()
		if err != nil {
			break
		}

		// Triple-backtick toggles multiline mode
		if strings.TrimSpace(line) == "```" {
			if !inMultiline {
				// Enter multiline mode
				inMultiline = true
				multilineBuffer.Reset()
				rl.SetPrompt("... ")
				continue
			}
			// Exit multiline mode — deliver content
			inMultiline = false
			rl.SetPrompt("mpm> ")
			content := multilineBuffer.String()
			multilineBuffer.Reset()
			if content != "" {
				if err := runner.HandleInput(ctx, content); err != nil {
					fmt.Fprintf(os.Stderr, "\nerror: %v\n", err)
				}
			}
			continue
		}

		if inMultiline {
			multilineBuffer.WriteString(line)
			multilineBuffer.WriteString("\n")
			continue
		}

		// Single-line mode — deliver directly
		if err := runner.HandleInput(ctx, line); err != nil {
			fmt.Fprintf(os.Stderr, "\nerror: %v\n", err)
		}
	}
}