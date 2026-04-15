package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"
)

// repl runs the interactive REPL for mini-bot
func repl(cfg *Config) {
	fmt.Println("mini-bot REPL (Ctrl+C to exit)")
	fmt.Println("Type your query and press Enter.")
	fmt.Println()
	reader := bufio.NewReader(os.Stdin)
	for {
		fmt.Print("> ")
		line, err := reader.ReadString('\n')
		if err != nil {
			if err == io.EOF || strings.Contains(err.Error(), "closed") {
				break
			}
			continue
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if line == "exit" || line == "quit" {
			break
		}
		fmt.Println()
		if err := runAgentLoop(line, true, cfg); err != nil {
			fmt.Fprintf(os.Stderr, "\nerror: %v\n", err)
		}
		fmt.Println()
	}
}