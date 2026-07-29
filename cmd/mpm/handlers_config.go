// cmd/mpm/handlers_config.go — `mpm config` family.
//
// v (operator) design session, Wed 2026-07-29:
//
//   "A user should never have to manually edit config.json for normal
//   setup. Adding a setup wizard is one of those tiny bits of
//   friction that instantly makes a project feel 'developer tool'
//   rather than 'finished product'."
//
// `mpm config` provides the operator-facing front door for the
// substrate's existing config block (internal/core/config/config.go).
// The substrate already has well-formed synth / alias / memory_dir
// configuration; operators just shouldn't have to hand-edit JSON to
// set the API key and model.
//
// Surface:
//
//   mpm config                       Interactive wizard over the
//                                    synth block. Preset choices
//                                    for MiniMax / OpenAI / Ollama
//                                    / Anthropic / Custom.
//
//   mpm config show | list          Print current synth block.
//
//   mpm config get <key>             Print one value.
//                                    Keys: model, api_key, base_url,
//                                    max_tokens, timeout_seconds.
//                                    Aliases: 'token' → api_key,
//                                    'endpoint' → base_url.
//
//   mpm config set <key> <value>     Set one value, persist.
//
//   mpm config edit                  Open mpm_config.json in $EDITOR.
//
//   mpm config validate              Substrate-side validation
//                                    placeholder for v0.1 (just confirms
//                                    the file loaded and prints OK).
//
// Architecture: handlers_config.go composes internal/core/config
// (LoadConfig for read, the new SaveConfig for write). No new
// substrate. No service / store / renderer.
//
// UI choice: instead of pulling in promptui or bubbletea for a TUI,
// this implementation uses numbered-choice prompts over stdin. The
// operator types '1' or hits enter for default. Reasoning: zero new
// dependencies, works in any TTY, scripts well via `--non-interactive`
// with stdin from /dev/null. A real TUI can land later via the
// operators who actually want arrow-key navigation; the numbered-
// choice approach covers all the v0.1 UX goals already.
//
// Non-interactivity: every subcommand's stdin read is guarded by
// isatty(os.Stdin). When stdin isn't a terminal (CI / agent), the
// wizard aborts with a friendly message pointing at `mpm config
// set` for scripting. This keeps CI runs deterministic.

package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"

	"github.com/flowbyte-com/mpm-core/config"
	"github.com/flowbyte-com/mpm-core/usererror"
)

// handleConfig is the entry point for `mpm config [...]`. Dispatches
// to the appropriate subcommand handler.
func handleConfig(args []string) int {
	if len(args) == 0 {
		return handleConfigInteractive(loadOrInitConfig())
	}
	switch args[0] {
	case "show", "list":
		return handleConfigShow(loadOrInitConfig())
	case "get":
		if len(args) < 2 {
			usererror.Error("mpm config get <key>\n  keys: model, api_key, base_url, max_tokens, timeout_seconds\n  aliases: token=api_key, endpoint=base_url")
			return 1
		}
		return handleConfigGet(loadOrInitConfig(), args[1])
	case "set":
		if len(args) < 3 {
			usererror.Error("mpm config set <key> <value>\n  keys: model, api_key, base_url, max_tokens, timeout_seconds")
			return 1
		}
		return handleConfigSet(loadOrInitConfig(), args[1], strings.Join(args[2:], " "))
	case "edit":
		return handleConfigEdit()
	case "validate":
		return handleConfigValidate(loadOrInitConfig())
	case "help", "-h", "--help":
		printConfigHelp()
		return 0
	default:
		usererror.Error("mpm config: unknown subcommand %q\n\n  Available: show, get, set, edit, validate, (no args = interactive wizard)", args[0])
		return 1
	}
}

// loadOrInitConfig returns the loaded config, creating an empty
// SynthConfig if missing so the wizard / set / get paths always
// operate on a non-nil struct. nil-check happens in the helpers.
func loadOrInitConfig() *config.Config {
	c, err := config.LoadConfig()
	if err != nil {
		usererror.Error("loading config: %v", err)
		os.Exit(1)
	}
	if c.Synth == nil {
		c.Synth = &config.SynthConfig{}
	}
	return c
}

// ---------------------------------------------------------------------------
// Subcommand handlers
// ---------------------------------------------------------------------------

func handleConfigShow(c *config.Config) int {
	fmt.Println("Configuration")
	fmt.Println(strings.Repeat("─", 60))
	if c.Synth == nil {
		fmt.Println("  (no synth block)")
		fmt.Println()
		fmt.Println("  Run `mpm config` to set up an AI provider.")
		return 0
	}
	s := c.Synth
	fmt.Printf("  provider       : %s\n", synthProviderLabel(s))
	fmt.Printf("  model          : %s\n", s.Model)
	fmt.Printf("  api key        : %s\n", redactAPIKey(s.APIKey))
	fmt.Printf("  base url       : %s\n", s.BaseURL)
	fmt.Printf("  max tokens     : %d\n", s.MaxTokens)
	fmt.Printf("  timeout secs   : %d\n", s.TimeoutSecs)
	fmt.Printf("  fallback chain : %d vendor(s)\n", len(s.Vendors))
	fmt.Println()
	fmt.Println("Config file:", config.ConfigPath())
	return 0
}

func handleConfigGet(c *config.Config, key string) int {
	val, err := configLookup(c, key)
	if err != nil {
		usererror.Error("%v", err)
		return 1
	}
	fmt.Println(val)
	return 0
}

func handleConfigSet(c *config.Config, key, val string) int {
	if c.Synth == nil {
		c.Synth = &config.SynthConfig{}
	}
	if err := configApply(c, key, val); err != nil {
		usererror.Error("%v", err)
		return 1
	}
	if err := config.SaveConfig(c); err != nil {
		usererror.Error("saving config: %v", err)
		return 1
	}
	fmt.Printf("✓ %s set\n", configCanonicalKey(key))
	return 0
}

func handleConfigEdit() int {
	editor := os.Getenv("EDITOR")
	if editor == "" {
		editor = "vi"
	}
	path := config.ConfigPath()
	cmd := exec.Command(editor, path)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		usererror.Error("editor exited: %v", err)
		return 1
	}
	fmt.Println("✓ Configuration reloaded")
	return 0
}

func handleConfigValidate(c *config.Config) int {
	// v0.1 validation is intentionally minimal. Substrate config
	// shape is the substrate's contract; this handler only
	// confirms the file loaded and that required synth fields are
	// non-empty. Future RFCs can add live-provider pings.
	if c == nil {
		fmt.Println("✗ no config loaded")
		return 1
	}
	if c.Synth == nil {
		fmt.Println("✗ synth block missing")
		fmt.Println("  run `mpm config` to set up a provider")
		return 1
	}
	if c.Synth.APIKey == "" && c.Synth.BaseURL == "" {
		fmt.Println("✗ no api_key and no base_url set")
		fmt.Println("  run `mpm config` to set up a provider")
		return 1
	}
	fmt.Println("✓ configuration shape looks OK")
	fmt.Println("  (live provider connectivity check is a follow-up RFC)")
	return 0
}

// ---------------------------------------------------------------------------
// Interactive wizard
// ---------------------------------------------------------------------------

// handleConfigInteractive runs the wizard. Each prompt uses stdin;
// defaults are accepted by hitting enter. The wizard is non-TTY-safe
// (refuses to run with stdin redirected, points operator at the
// scripting interface).
func handleConfigInteractive(c *config.Config) int {
	if !isatty(os.Stdin) {
		fmt.Println("Configure MPM")
		fmt.Println()
		fmt.Println("Non-interactive mode detected (stdin isn't a terminal).")
		fmt.Println("Use the scriptable interface instead:")
		fmt.Println()
		fmt.Println("  mpm config set api_key $OPENAI_API_KEY")
		fmt.Println("  mpm config set model gpt-5.5")
		fmt.Println("  mpm config set base_url https://api.openai.com/v1")
		fmt.Println()
		fmt.Println("Or run `mpm config` interactively from a real terminal.")
		return 0
	}

	fmt.Println("Configure MPM")
	fmt.Println()
	fmt.Println("Each preset fills in the right default base_url + model")
	fmt.Println("for that provider. You'll be asked to confirm before saving.")
	fmt.Println()

	// Ensure synth block exists.
	if c.Synth == nil {
		c.Synth = &config.SynthConfig{}
	}

	// Preset choice.
	preset := promptChoice(rwFromStdin(), "Provider", []choice{
		{id: "minimax", label: "MiniMax (anthropic-compatible)", defaults: config.SynthConfig{
			Model:   "MiniMax-M2.7",
			BaseURL: "https://api.minimax.io/anthropic/v1",
		}},
		{id: "openai", label: "OpenAI", defaults: config.SynthConfig{
			Model:   "gpt-4o",
			BaseURL: "https://api.openai.com/v1/v1",
		}},
		{id: "ollama", label: "Ollama (local)", defaults: config.SynthConfig{
			Model:   "llama3",
			BaseURL: "http://localhost:11434/v1",
		}},
		{id: "anthropic", label: "Anthropic direct", defaults: config.SynthConfig{
			Model:   "claude-3-5-sonnet",
			BaseURL: "https://api.anthropic.com/v1",
		}},
		{id: "custom", label: "Custom (I know what I'm doing)", defaults: config.SynthConfig{}},
	})
	if preset == nil {
		fmt.Println("Aborted.")
		return 0
	}

	// Apply preset defaults to the struct (filling empty fields
	// only — operators can override per-field in the prompts
	// below).
	mergeDefaults(c.Synth, preset.defaults)

	// Model prompt.
	model := promptString(rwFromStdin(), "Model", c.Synth.Model)
	if model != "" {
		c.Synth.Model = strings.TrimSpace(model)
	}

	// Base URL prompt.
	baseURL := promptString(rwFromStdin(), "Base URL", c.Synth.BaseURL)
	if baseURL != "" {
		c.Synth.BaseURL = strings.TrimSpace(baseURL)
	}

	// API key prompt — only if preset needs it (skip for Ollama).
	needsKey := preset.id != "ollama"
	if needsKey {
		existing := c.Synth.APIKey
		var labelDefault string
		if existing != "" {
			labelDefault = "(unchanged)"
		}
		key := promptSecret(rwFromStdin(), "API key", labelDefault)
		if key != "" {
			c.Synth.APIKey = strings.TrimSpace(key)
		}
	}

	// Max tokens + timeout (rarely customised, default-only).
	tokens := promptString(rwFromStdin(), "Max tokens", intToStr(c.Synth.MaxTokens))
	if tokens != "" {
		if n, err := strconvAtoi(tokens); err == nil && n > 0 {
			c.Synth.MaxTokens = n
		}
	}
	timeout := promptString(rwFromStdin(), "Timeout seconds", intToStr(c.Synth.TimeoutSecs))
	if timeout != "" {
		if n, err := strconvAtoi(timeout); err == nil && n > 0 {
			c.Synth.TimeoutSecs = n
		}
	}

	// Confirm + save.
	fmt.Println()
	fmt.Println("Preview:")
	fmt.Printf("  model      : %s\n", c.Synth.Model)
	fmt.Printf("  base url   : %s\n", c.Synth.BaseURL)
	fmt.Printf("  api key    : %s\n", redactAPIKey(c.Synth.APIKey))
	fmt.Printf("  max tokens : %d\n", c.Synth.MaxTokens)
	fmt.Printf("  timeout    : %d\n", c.Synth.TimeoutSecs)
	fmt.Println()
	if !confirmPrompt(rwFromStdin(), "Save?") {
		fmt.Println("Aborted.")
		return 0
	}
	if err := config.SaveConfig(c); err != nil {
		usererror.Error("saving config: %v", err)
		return 1
	}
	fmt.Println()
	fmt.Println("✓ Configuration saved to " + config.ConfigPath())
	return 0
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// configLookup resolves a key (incl. aliases) against the loaded
// config and returns the canonical display value.
func configLookup(c *config.Config, key string) (string, error) {
	if c == nil || c.Synth == nil {
		return "", fmt.Errorf("no synth block configured")
	}
	canon := configCanonicalKey(key)
	switch canon {
	case "model":
		return c.Synth.Model, nil
	case "api_key":
		return c.Synth.APIKey, nil
	case "base_url":
		return c.Synth.BaseURL, nil
	case "max_tokens":
		return intToStr(c.Synth.MaxTokens), nil
	case "timeout_seconds":
		return intToStr(c.Synth.TimeoutSecs), nil
	}
	return "", fmt.Errorf("unknown key %q (try: model, api_key, base_url, max_tokens, timeout_seconds)", key)
}

// configApply mutates the loaded config in place. Pure mutation
// helper; persistence happens in handleConfigSet via SaveConfig.
func configApply(c *config.Config, key, val string) error {
	if c.Synth == nil {
		return fmt.Errorf("synth block missing")
	}
	canon := configCanonicalKey(key)
	switch canon {
	case "model":
		c.Synth.Model = val
	case "api_key":
		c.Synth.APIKey = val
	case "base_url":
		c.Synth.BaseURL = val
	case "max_tokens":
		n, err := strconvAtoi(val)
		if err != nil || n <= 0 {
			return fmt.Errorf("max_tokens must be a positive integer (got %q)", val)
		}
		c.Synth.MaxTokens = n
	case "timeout_seconds":
		n, err := strconvAtoi(val)
		if err != nil || n <= 0 {
			return fmt.Errorf("timeout_seconds must be a positive integer (got %q)", val)
		}
		c.Synth.TimeoutSecs = n
	default:
		return fmt.Errorf("unknown key %q (try: model, api_key, base_url, max_tokens, timeout_seconds)", key)
	}
	return nil
}

// configCanonicalKey normalises an input key to its canonical form.
//   "token"       → "api_key"
//   "apikey"      → "api_key"
//   "endpoint"    → "base_url"
//   "base-url"    → "base_url"
//   "max"         → "max_tokens"
//   "timeout"     → "timeout_seconds"
func configCanonicalKey(key string) string {
	k := strings.ToLower(strings.ReplaceAll(key, "-", "_"))
	switch k {
	case "token", "apikey", "api_key":
		return "api_key"
	case "endpoint", "baseurl", "base_url":
		return "base_url"
	case "max", "maxtokens", "max_tokens":
		return "max_tokens"
	case "timeout", "timeoutsecs", "timeout_seconds":
		return "timeout_seconds"
	case "model":
		return "model"
	}
	return k
}

// synthProviderLabel heuristically labels the configured provider
// from base_url. Substrate doesn't store the provider explicitly;
// the base_url is the operative signal. Used for `mpm config show`.
func synthProviderLabel(s *config.SynthConfig) string {
	if s == nil {
		return "(none)"
	}
	u := strings.ToLower(s.BaseURL)
	switch {
	case strings.Contains(u, "minimax"):
		return "MiniMax"
	case strings.Contains(u, "openai"):
		return "OpenAI"
	case strings.Contains(u, "ollama") || strings.Contains(u, "11434"):
		return "Ollama"
	case strings.Contains(u, "anthropic"):
		return "Anthropic"
	case s.BaseURL == "" && s.Model == "":
		return "(not configured)"
	}
	if s.BaseURL != "" {
		return s.BaseURL
	}
	return "Custom"
}

// redactAPIKey returns a printable representation of an API key
// without leaking it. Shows the first 4 chars + '...' + last 4
// chars when long enough; otherwise '****' or '(unset)'.
func redactAPIKey(k string) string {
	if k == "" {
		return "(unset)"
	}
	if len(k) <= 12 {
		return "****"
	}
	return k[:4] + "..." + k[len(k)-4:]
}

// mergeDefaults fills empty fields in target with values from src.
// Non-empty fields in target are preserved. Used by the wizard
// to apply preset defaults without clobbering operator edits.
func mergeDefaults(target *config.SynthConfig, src config.SynthConfig) {
	if target == nil {
		return
	}
	if target.Model == "" {
		target.Model = src.Model
	}
	if target.BaseURL == "" {
		target.BaseURL = src.BaseURL
	}
}

// ---------------------------------------------------------------------------
// stdin prompts (numbered-choice + line-reader)
// ---------------------------------------------------------------------------

// choice is one row in a numbered-choice prompt.
type choice struct {
	id       string
	label    string
	defaults config.SynthConfig
}

// rwFromStdin returns a buffered reader around stdin. Used by
// every prompt.
func rwFromStdin() *bufio.Reader {
	return bufio.NewReader(os.Stdin)
}

// promptChoice prints a numbered list and reads a selection.
func promptChoice(rw *bufio.Reader, header string, choices []choice) *choice {
	fmt.Printf("\n  %s\n", header)
	for i, c := range choices {
		fmt.Printf("    %d. %s\n", i+1, c.label)
	}
	fmt.Printf("\n  Choose [%d-%d] (default: 1): ", 1, len(choices))
	raw, _ := rw.ReadString('\n')
	raw = strings.TrimSpace(raw)
	if raw == "" {
		raw = "1"
	}
	n, err := strconvAtoi(raw)
	if err != nil || n < 1 || n > len(choices) {
		fmt.Println("  invalid choice; aborting")
		return nil
	}
	return &choices[n-1]
}

// promptString prints a labeled prompt and reads a line. Returns
// the default value (labelDefault) when the user enters nothing.
func promptString(rw *bufio.Reader, label, def string) string {
	prompt := label
	if def != "" {
		prompt = label + " [" + def + "]"
	}
	prompt += ": "
	fmt.Print(prompt)
	raw, _ := rw.ReadString('\n')
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	return raw
}

// promptSecret prints a labeled prompt and reads a line. The input
// is echoed if stdin is a TTY (terminal-side visibility) because
// we're not pulling in a TUI library — the operator's terminal
// handles input visibility. def is non-empty when there's an
// existing key; the prompt says "press enter to keep" rather than
// show the secret.
func promptSecret(rw *bufio.Reader, label, def string) string {
	prompt := label
	if def != "(unchanged)" && def != "" {
		prompt = label + " [keep existing]"
	}
	prompt += ": "
	fmt.Print(prompt)
	raw, _ := rw.ReadString('\n')
	return strings.TrimSpace(raw)
}

// confirmPrompt reads a Y/n confirmation.
func confirmPrompt(rw *bufio.Reader, label string) bool {
	fmt.Printf("  %s [Y/n]: ", label)
	raw, _ := rw.ReadString('\n')
	raw = strings.TrimSpace(strings.ToLower(raw))
	if raw == "" || raw == "y" || raw == "yes" {
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// Small helpers (kept package-private to avoid touching stdlib imports)
// ---------------------------------------------------------------------------

// intToStr formats an int as a string, returning "" for 0. Used to
// surface a default placeholder only.
func intToStr(n int) string {
	if n == 0 {
		return ""
	}
	return fmt.Sprintf("%d", n)
}

// strconvAtoi wraps strconv.Atoi to keep imports slim. The wizard
// uses this only for the two int fields.
func strconvAtoi(s string) (int, error) {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("not a number: %q", s)
		}
		n = n*10 + int(c-'0')
	}
	return n, nil
}

// ---------------------------------------------------------------------------
// Help
// ---------------------------------------------------------------------------

// printConfigHelp prints the mpm config help block.
func printConfigHelp() {
	fmt.Println(`mpm config — Configure the AI provider

Usage:
  mpm config                       Interactive wizard
  mpm config show | list            Show current configuration
  mpm config get <key>             Get one value
  mpm config set <key> <value>     Set one value
  mpm config edit                  Open mpm_config.json in $EDITOR
  mpm config validate              Validate configuration shape

Keys (canonical names; aliases accepted):
  model, api_key (alias: token), base_url (alias: endpoint),
  max_tokens, timeout_seconds

Examples:
  mpm config set api_key $OPENAI_API_KEY
  mpm config set model gpt-4o
  mpm config set endpoint https://api.openai.com/v1

Config file: ~/.mpm/mpm_config.json (path resolved via the
workspace; $EDITOR is opened on this file for 'mpm config edit'.)`)
}

// (syscall imported for isatty() — package main already in scope.)
var _ = syscall.Stdin
