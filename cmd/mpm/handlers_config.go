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
	"strconv"
	"strings"
	"syscall"

	"github.com/flowbyte-com/mpm-core/config"
	mpminternal "github.com/flowbyte-com/mpm-core"
	"github.com/flowbyte-com/mpm-core/usererror"

	"github.com/flowbyte-com/mpm/cmd/mpm/render"
)

// handleConfig is the entry point for `mpm config [...]`. Dispatches
// to the appropriate subcommand handler.
func handleConfig(args []string) int {
	if len(args) == 0 {
		return handleConfigInteractive(loadOrInitConfig())
	}
	// 2026-09-14 release-pass: `--help` / `-h` / "help" (the
	// parseFlags rewrite) at the dispatcher level short-circuits
	// to the canonical config help. Only when the FIRST positional
	// is a help flag — subcommands with their own help pages
	// (profile / component / detect-embedding) handle help
	// themselves via their own short-circuit.
	if args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		printConfigHelp()
		return 0
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
	case "profile":
		return handleConfigProfile(args[1:])
	case "component":
		return handleConfigComponent(args[1:])
	case "capability":
		return handleConfigCapability(args[1:])
	case "detect-embedding":
		// 2026-09-14 release-pass: detect-embedding has its own help
		// page (`mpm config detect-embedding --help`).
		for _, a := range args[1:] {
			if a == "-h" || a == "--help" || a == "help" {
				printConfigDetectEmbeddingHelp()
				return 0
			}
		}
		cmd := &DetectEmbeddingCmd{}
		for i := 1; i < len(args); i++ {
			switch {
			case args[i] == "--apply" && i+1 < len(args):
				cmd.Apply = args[i+1]
				i++
			case args[i] == "--force":
				cmd.Force = true
			}
		}
		return cmd.Run()
	case "help", "-h", "--help":
		printConfigHelp()
		return 0
	default:
		usererror.Error("mpm config: unknown subcommand %q\n\n  Available: show, get, set, edit, validate, profile, component, capability, detect-embedding, (no args = interactive wizard)", args[0])
		return 1
	}
}

// loadOrInitConfig returns the loaded config. The wizard / set / get paths
// always operate on the loaded struct; helpers nil-check before writing.
//
// Note: this no longer auto-initialises the legacy top-level `synth` block.
// The wizard writes to Profiles["default"] (canonical). The legacy
// `synth` block is preserved on disk if present (one-way compatibility)
// but new code does not create it.
//
// On JSON parse failure, prints an actionable hint pointing the
// operator at the file path and a JSON validation tool. Doesn't pull
// in a JSON parser dependency.
func loadOrInitConfig() *config.Config {
	c, err := config.LoadConfig()
	if err != nil {
		usererror.Error("failed to load %s: %v\nCheck mpm_config.json with:\n  python3 -m json.tool %s",
			config.ConfigPath(), err, config.ConfigPath())
		os.Exit(1)
	}
	return c
}

// ---------------------------------------------------------------------------
// Subcommand handlers
// ---------------------------------------------------------------------------

func handleConfigShow(c *config.Config) int {
	fmt.Println("Configuration")
	fmt.Println(strings.Repeat("─", 60))

	// Synthesis engine kill switch — top-level state, independent
	// of profiles/components (which are concerns under the legacy
	// synth block). Default-enabled; a missing field means "on".
	synStatus := "enabled"
	if c.SynthesisEnabled != nil && !*c.SynthesisEnabled {
		synStatus = "DISABLED (background synthesis is off)"
	}
	fmt.Println()
	fmt.Printf("  Synthesis engine: %s\n", synStatus)

	// Embedding — canonical embedding configuration per spec §6.1.
	// Four operator-meaningful states must be clearly distinguishable:
	//   - absent (no provider configured, no env fallback)
	//   - intentionally disabled (components.embedding="disabled")
	//   - profile/configured (a real profile is bound)
	//   - env legacy fallback (OLLAMA_* env vars resolved directly)
	cfg := mpminternal.DefaultEmbeddingConfig()
	fmt.Println()
	fmt.Println("  Embedding")
	if cfg.IntentionallyDisabled {
		fmt.Printf("    source:  intentionally disabled\n")
	} else {
		srcLabel := cfg.Source.String()
		if cfg.Source == mpminternal.EmbeddingSourceEnvFallback {
			srcLabel = "env (legacy fallback)"
		}
		fmt.Printf("    source:  %s\n", srcLabel)
		// Only show the profile line when one was actually bound. The
		// env-fallback path has no profile (the env vars resolved to a
		// provider directly) — emitting "profile: env" would falsely
		// suggest a profile binding exists.
		if cfg.ProfileName != "" {
			fmt.Printf("    profile: %s\n", cfg.ProfileName)
		}
		// For the absent state, "provider: null" / "status: null"
		// reads as a Go-internal sentinel rather than a meaningful
		// operator signal. Surface the diagnostic explicitly.
		if cfg.Source == mpminternal.EmbeddingSourceAbsent {
			fmt.Printf("    provider: (none configured — set components[\"embedding\"] in mpm_config.json or OLLAMA_ENDPOINT/OLLAMA_MODEL env vars)\n")
		} else {
			// Split "provider:model" into separate lines for readability.
			// Format: "ollama:nomic-embed-text" → provider="ollama", model="nomic-embed-text"
			provider, model := splitProviderModel(cfg.ProviderName)
			fmt.Printf("    provider: %s\n", provider)
			if model != "" {
				fmt.Printf("    model:    %s\n", model)
			}
			fmt.Printf("    status:  %s\n", cfg.Status)
			if cfg.LastError != nil {
				fmt.Printf("    error:   %v\n", cfg.LastError)
			}
		}
	}

	// Profiles — the operator-facing execution-profile abstraction.
	if len(c.Profiles) > 0 {
		fmt.Println()
		fmt.Println("  Profiles")
		for _, name := range sortedKeysForConfig(c.Profiles) {
			p := c.Profiles[name]
			fmt.Printf("    %-12s : provider=%s · model=%s\n", name, p.Provider, p.Model)
			if p.BaseURL != "" {
				fmt.Printf("    %-12s   base_url=%s\n", "", p.BaseURL)
			}
			if p.APIKey != "" {
				fmt.Printf("    %-12s   api_key=%s\n", "", redactAPIKey(p.APIKey))
			}
		}
	}

	// Components — substrate functions bound to profiles.
	if len(c.Components) > 0 {
		fmt.Println()
		fmt.Println("  Components")
		for _, comp := range sortedKeysForConfig(c.Components) {
			bound := c.Components[comp]
			if bound == "" {
				bound = "(default)"
			}
			fmt.Printf("    %-12s → %s\n", comp, bound)
		}
	}

	// Capabilities — operator-meaningful vocabulary that Skills
	// and runtime code address, mapped to substrate components.
	if len(c.Capabilities) > 0 {
		fmt.Println()
		fmt.Println("  Capabilities")
		for _, cap := range sortedKeysForConfig(c.Capabilities) {
			bound := c.Capabilities[cap]
			if bound == "" {
				bound = "(default)"
			}
			fmt.Printf("    %-12s → %s\n", cap, bound)
		}
	}

	// Legacy synth block — still surfaced for operators with the
	// pre-profiles config (or who haven't migrated). Only show when
	// there's actual legacy content to migrate (not an empty struct
	// after the key was removed from the JSON).
	if c.Synth != nil && (c.Synth.Model != "" || c.Synth.APIKey != "" || c.Synth.BaseURL != "") {
		s := c.Synth
		fmt.Println()
		fmt.Println("  Legacy synth block (migrate via 'mpm config profile add')")
		fmt.Printf("    provider     : %s\n", synthProviderLabel(s))
		fmt.Printf("    model        : %s\n", s.Model)
		fmt.Printf("    api key      : %s\n", redactAPIKey(s.APIKey))
		fmt.Printf("    base url     : %s\n", s.BaseURL)
		fmt.Printf("    max tokens   : %d\n", s.MaxTokens)
		fmt.Printf("    timeout secs : %d\n", s.TimeoutSecs)
		fmt.Printf("    vendor chain : %d vendor(s)\n", len(s.Vendors))
	}

	if len(c.Profiles) == 0 && (c.Synth == nil || c.Synth.Model == "") {
		fmt.Println()
		fmt.Println("  (no AI provider configured — run `mpm config` to set one up)")
	}

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

// handleConfigSet writes a single canonical key to Profiles["default"].
// The legacy top-level `synth` block is no longer written by this
// command — operators migrating an old install run
// `mpm config profile add default` once, then continue with `set` for
// per-field updates.
func handleConfigSet(c *config.Config, key, val string) int {
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
	// Robust structural validation. Walks Profiles, Components,
	// Capabilities, and the legacy Synth fallback to surface
	// actionable errors. Per v (operator) request, this becomes
	// the cognitive equivalent of `go vet` / `cargo check` /
	// `terraform validate` — a single command that builds
	// confidence before operators deploy config changes.
	//
	// Scope: structural only. Live provider connectivity (HTTP
	// pings, model listing) is a future-RFC feature -- it would
	// need per-vendor ping primitives and error tolerance for
	// flaky networks. Today's validator catches "this binding
	// won't work because the profile is missing a model" without
	// leaving the operator's machine.
	if c == nil {
		fmt.Println("✗ no config loaded")
		return 1
	}
	issues := 0

	fmt.Println("Configuration validation")
	fmt.Println(strings.Repeat("─", 60))

	// Check 1: at least one usable provider is configured.
	hasProvider := (c.Synth != nil && (c.Synth.Model != "" || c.Synth.BaseURL != "")) ||
		len(c.Profiles) > 0
	if hasProvider {
		fmt.Println("  ✓ provider configuration present")
	} else {
		fmt.Println("  ✗ no provider configured")
		fmt.Println("      → run `mpm config` to set one up")
		issues++
	}

	// Check 2: every profile has provider + model set.
	if len(c.Profiles) > 0 {
		profileOK := 0
		for _, name := range sortedKeysForConfig(c.Profiles) {
			p := c.Profiles[name]
			if p.Provider == "" || p.Model == "" {
				fmt.Printf("  ✗ profile %q missing %s\n", name, missingField(p))
				issues++
			} else {
				profileOK++
			}
		}
		if profileOK > 0 && profileOK == len(c.Profiles) {
			fmt.Printf("  ✓ profiles: %d / %d valid\n", profileOK, len(c.Profiles))
		}
	}

	// Check 3: every component bound to a profile that resolves.
	if len(c.Components) > 0 {
		componentOK := 0
		for _, comp := range sortedKeysForConfig(c.Components) {
			bound := c.Components[comp]
			if bound == "" {
				fmt.Printf("  ⚠ component %q unbound (falls back to ProfileFor default chain)\n", comp)
				continue
			}
			if _, ok := c.Profiles[bound]; !ok {
				// Distinguish "explicit binding broken" from "binding
				// resolves via default fallback". The runtime silently
				// falls through to Profiles["default"] when an
				// explicit binding points at a missing profile; that's
				// a UX footgun. Surface the broken binding even when
				// default exists so operators can fix it.
				if _, hasDefault := c.Profiles["default"]; hasDefault {
					fmt.Printf("  ⚠ component %q explicitly bound to missing profile %q (runtime would fall back to default)\n", comp, bound)
					fmt.Printf("      → define the profile or unbind: `mpm config profile add %s` or `mpm config component unset %q`\n", bound, comp)
					continue
				}
				fmt.Printf("  ✗ component %q bound to missing profile %q\n", comp, bound)
				fmt.Printf("      → define the profile or unbind: `mpm config profile add %s` or `mpm config component unset %q`\n", bound, comp)
				issues++
				continue
			}
			componentOK++
		}
		if componentOK == len(c.Components) && len(c.Components) > 0 {
			fmt.Printf("  ✓ components: %d / %d bound\n", componentOK, len(c.Components))
		}
	}

	// Check 4: every capability maps to a real component.
	//
	// "Real" here means: the capability's bound component name
	// resolves to either a known component (memory, critic, ...)
	// or to a component explicitly bound in Config.Components.
	// Capability → component → profile is the chain; the
	// validator only checks the first hop (the component name
	// exists), since downstream checks are already covered above.
	if len(c.Capabilities) > 0 {
		capOK := 0
		knownSet := map[string]bool{}
		for _, k := range knownComponents {
			knownSet[k] = true
		}
		for _, cap := range sortedKeysForConfig(c.Capabilities) {
			bound := c.Capabilities[cap]
			if bound == "" {
				fmt.Printf("  ⚠ capability %q unbound (will use default if defined)\n", cap)
				continue
			}
			if !knownSet[bound] {
				if _, ok := c.Components[bound]; !ok {
					fmt.Printf("  ✗ capability %q bound to missing component %q\n", cap, bound)
					fmt.Printf("      → bind to a known component: `mpm config capability set %q <component>`\n", cap)
					issues++
					continue
				}
			}
			capOK++
		}
		if capOK == len(c.Capabilities) && len(c.Capabilities) > 0 {
			fmt.Printf("  ✓ capabilities: %d / %d resolve\n", capOK, len(c.Capabilities))
		}
	}

	// Check 5: known components (memory, critic, scheduler) all
	// resolve to a Profile via the ProfileFor chain. Catches the
	// 'shadow components' case where the operator intended to bind
	// them but forgot.
	if c.Synth != nil || len(c.Profiles) > 0 {
		missingComponent := 0
		for _, comp := range knownComponents {
			if c.ProfileFor(comp) == nil {
				fmt.Printf("  ✗ component %q cannot resolve to a profile\n", comp)
				missingComponent++
				issues++
			}
		}
		if missingComponent == 0 && (c.Synth != nil || len(c.Profiles) > 0) {
			fmt.Printf("  ✓ known components: %s resolve to profiles\n", strings.Join(knownComponents, ", "))
		}
	}

	fmt.Println()
	if issues == 0 {
		fmt.Println("Validation: ✓ OK")
		return 0
	}
	fmt.Printf("Validation: ✗ FAIL (%d issue%s)\n", issues, pluralForN(issues))
	fmt.Println()
	fmt.Println("Live provider checks (--live flag) are a future RFC.")
	return 1
}

// missingField returns the first missing required field in p
// (provider or model). The CLI surface surfaces these in the
// validator output so operators know which field to set.
func missingField(p config.Profile) string {
	if p.Provider == "" {
		return "provider"
	}
	if p.Model == "" {
		return "model"
	}
	return ""
}

// pluralForN returns "s" for non-1 counts, "" for 1. Tiny helper
// to keep the validator output grammatical without pulling in
// golang.org/x/text.
func pluralForN(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// ---------------------------------------------------------------------------
// Interactive wizard
// ---------------------------------------------------------------------------

// wizardPresets enumerates the canned provider presets the wizard offers.
// Each preset carries the canonical base_url + model that an operator can
// accept by default or override per-field. "custom" is the no-default
// fallback for operators who know what they're doing.
var wizardPresets = []choice{
	{id: "minimax", label: "MiniMax (anthropic-compatible)", defaults: config.Profile{
		Provider: "minimax",
		Model:    "MiniMax-M2.7",
		BaseURL:  "https://api.minimax.io/anthropic/v1",
	}},
	{id: "openai", label: "OpenAI", defaults: config.Profile{
		Provider: "openai",
		Model:    "gpt-4o",
		BaseURL:  "https://api.openai.com/v1",
	}},
	{id: "ollama", label: "Ollama (local)", defaults: config.Profile{
		Provider: "ollama",
		Model:    "llama3",
		BaseURL:  "http://localhost:11434/v1",
	}},
	{id: "anthropic", label: "Anthropic direct", defaults: config.Profile{
		Provider: "anthropic",
		Model:    "claude-3-5-sonnet",
		BaseURL:  "https://api.anthropic.com/v1",
	}},
	{id: "custom", label: "Custom (I know what I'm doing)", defaults: config.Profile{}},
}

// presetIDForProvider maps an existing provider string to a preset id,
// or "custom" when the provider does not match a known preset.
func presetIDForProvider(provider string) string {
	for _, p := range wizardPresets {
		if p.id == provider {
			return p.id
		}
	}
	return "custom"
}

// handleConfigInteractive runs the wizard. Each prompt uses stdin;
// defaults are accepted by hitting enter. The wizard is non-TTY-safe
// (refuses to run with stdin redirected, points operator at the
// scripting interface).
//
// The wizard is state-aware: it reads the current configuration first
// and only asks about missing or operator-changed fields. Existing
// non-empty values are preserved unless the operator explicitly
// replaces them. The wizard does not erase component bindings,
// capability overrides, embedding configuration, the synthesis flag,
// or unrelated profiles.
//
// The wizard writes to Profiles["default"] (the canonical surface for
// the default LLM profile). The legacy top-level `synth` block is no
// longer created or updated by the wizard; it remains readable for
// backward compatibility (ProfileFor falls through to it as a one-way
// migration path), but new installs should configure
// Profiles["default"].
func handleConfigInteractive(c *config.Config) int {
	if !isatty(os.Stdin) {
		fmt.Println("Configure MPM")
		fmt.Println()
		fmt.Println("Non-interactive mode detected (stdin isn't a terminal).")
		fmt.Println("Use the scriptable interface instead:")
		fmt.Println()
		fmt.Println("  mpm config profile add default --provider <name> --model <model> --base-url <url>")
		fmt.Println("  mpm config profile set default api_key <key>")
		fmt.Println("  mpm config component set memory default")
		fmt.Println()
		fmt.Println("Or run `mpm config` interactively from a real terminal.")
		return 0
	}

	fmt.Println("Configure MPM")
	fmt.Println()

	// State-aware preamble: surface current configuration so the
	// operator sees what is already set before any prompt runs.
	wizardShowCurrentState(c)

	// Detect legacy-only configurations and offer a one-line
	// explanation so the operator knows a new profiles.default will
	// be created alongside the legacy block (compatibility is
	// preserved; modern takes precedence at runtime).
	if isLegacyOnlyConfig(c) {
		fmt.Println()
		fmt.Println("Legacy configuration detected.")
		fmt.Println("Your existing provider will be used as the default.")
		fmt.Println("Saving will create modern profiles.default configuration;")
		fmt.Println("the legacy synth block remains readable for compatibility.")
		fmt.Println()
	}

	// Ensure Profiles["default"] exists. The wizard writes here; the
	// legacy top-level `synth` block is no longer written by the wizard.
	if c.Profiles == nil {
		c.Profiles = map[string]config.Profile{}
	}
	prof := c.Profiles["default"]

	// Preset choice: default to the preset matching the existing
	// provider (or "custom" if no preset matches). This avoids the
	// silent-overwrite behaviour of always-defaulting-to-MiniMax.
	defaultPresetID := "custom"
	if prof.Provider != "" {
		defaultPresetID = presetIDForProvider(prof.Provider)
	}
	preset := promptChoiceDefault(rwFromStdin(), "Provider", wizardPresets, defaultPresetID)
	if preset == nil {
		fmt.Println("Aborted.")
		return 0
	}

	// Snapshot the existing profile BEFORE applying preset defaults.
	// We use this snapshot to detect which fields the operator
	// already populated so the wizard doesn't silently overwrite a
	// custom base_url when only the provider matched a preset.
	existing := prof
	hadBaseURL := existing.BaseURL != ""
	hadKey := existing.APIKey != ""

	// Apply preset defaults ONLY to fields the operator hasn't set.
	// Existing values win; preset defaults fill empty fields.
	mergeProfileDefaults(&prof, preset.defaults)

	// Model prompt — preserve existing model on empty input.
	model := promptString(rwFromStdin(), "Model", prof.Model)
	if model != "" {
		prof.Model = strings.TrimSpace(model)
	}

	// 2026-09-14 release-pass: LLM-role validation. If the
	// selected model is positively identified as embedding-only,
	// reject BEFORE the generation-specific prompts (Max
	// tokens). The rejection message names the model verbatim
	// and points at the embedding configuration path. We probe
	// the runtime's capability metadata (authoritative) and
	// fall back to a small name list when the probe is
	// unavailable. Unknown capability = accept (preserves
	// custom-provider flexibility per the brief).
	//
	// We resolve provider/base_url from the live prompt state
	// rather than prof.Provider/prof.BaseURL so the validator
	// sees the same values the operator just typed. This keeps
	// "Custom" + http://127.0.0.1:11434/ — Ollama-compatible —
	// inside the probe path.
	if prof.Model != "" {
		resolvedProvider := preset.id
		resolvedBaseURL := prof.BaseURL
		switch ValidateLLMRole(resolvedProvider, prof.Model, resolvedBaseURL) {
		case RoleEmbeddingOnly:
			fmt.Println()
			fmt.Println(RejectEmbeddingOnlyLLM(prof.Model))
			fmt.Println()
			fmt.Println("Aborted.")
			return 1
		}
	}

	// Base URL prompt — preserve the operator's existing URL when
	// non-empty, even if the chosen preset has a different default.
	// This is the silent-overwrite fix: a custom endpoint must not
	// be replaced by the preset base_url just because the provider
	// name happens to match.
	baseURL := promptString(rwFromStdin(), "Base URL", prof.BaseURL)
	if baseURL != "" {
		prof.BaseURL = strings.TrimSpace(baseURL)
	} else if hadBaseURL {
		// Restore the original URL — empty input keeps existing.
		prof.BaseURL = existing.BaseURL
	}

	// API key prompt — only if preset needs it (skip for Ollama).
	// Also skip when the provider is "ollama" by existing config.
	needsKey := preset.id != "ollama" && prof.Provider != "ollama"
	if needsKey {
		var keyDefault string
		if hadKey {
			keyDefault = "(unchanged)"
		}
		key := promptSecret(rwFromStdin(), "API key", keyDefault)
		if key != "" {
			prof.APIKey = strings.TrimSpace(key)
		} else if hadKey {
			prof.APIKey = existing.APIKey
		}
	}

	// Max tokens + timeout (rarely customised, default-only).
	tokens := promptString(rwFromStdin(), "Max tokens", intToStr(prof.MaxTokens))
	if tokens != "" {
		if n, err := strconvAtoi(tokens); err == nil && n > 0 {
			prof.MaxTokens = n
		}
	} else if existing.MaxTokens > 0 {
		prof.MaxTokens = existing.MaxTokens
	}
	timeout := promptString(rwFromStdin(), "Timeout seconds", intToStr(prof.TimeoutSecs))
	if timeout != "" {
		if n, err := strconvAtoi(timeout); err == nil && n > 0 {
			prof.TimeoutSecs = n
		}
	} else if existing.TimeoutSecs > 0 {
		prof.TimeoutSecs = existing.TimeoutSecs
	}

	// If the operator had no provider set AND the preset didn't supply
	// one (custom), the wizard is in a degenerate state — refuse to
	// save and tell the operator what they need to do.
	if prof.Provider == "" && preset.id == "custom" {
		fmt.Println()
		fmt.Println("A provider is required. Use `mpm config profile set default provider <name>` for non-interactive configuration, or re-run the wizard and choose a preset.")
		return 1
	}

	// Embedding step: surface the embedding subsystem, default to
	// "leave unchanged" when already configured.
	fmt.Println()
	wizardEmbeddingStep(c)

	// Specialist (critic) step: optional, default to "no" when the
	// operator hasn't already configured a critic profile.
	fmt.Println()
	wizardCriticStep(c)

	// Capability informational block.
	fmt.Println()
	wizardShowCapabilities()

	// Synthesis state.
	fmt.Println()
	wizardShowSynthesis(c)

	// Confirm + save.
	fmt.Println()
	fmt.Println("Preview (profile \"default\"):")
	fmt.Printf("  provider   : %s\n", prof.Provider)
	fmt.Printf("  model      : %s\n", prof.Model)
	fmt.Printf("  base url   : %s\n", prof.BaseURL)
	fmt.Printf("  api key    : %s\n", redactAPIKey(prof.APIKey))
	fmt.Printf("  max tokens : %d\n", prof.MaxTokens)
	fmt.Printf("  timeout    : %d\n", prof.TimeoutSecs)
	fmt.Println()
	if !confirmPrompt(rwFromStdin(), "Save?") {
		fmt.Println("Aborted.")
		return 0
	}
	c.Profiles["default"] = prof
	if err := config.SaveConfig(c); err != nil {
		usererror.Error("saving config: %v", err)
		return 1
	}
	fmt.Println()
	fmt.Println("✓ Configuration saved to " + config.ConfigPath())
	return 0
}

// isLegacyOnlyConfig returns true when the only provider configuration
// is a legacy top-level `synth` block (no Profiles["default"], no other
// profiles). Used to surface a migration note in the wizard.
func isLegacyOnlyConfig(c *config.Config) bool {
	if c == nil {
		return false
	}
	if c.Synth == nil || c.Synth.Model == "" {
		return false
	}
	hasProfiles := len(c.Profiles) > 0
	return !hasProfiles
}

// wizardShowCurrentState prints a one-paragraph preamble of the
// current configuration so the operator sees what is already set
// before any prompt runs.
func wizardShowCurrentState(c *config.Config) {
	if c == nil {
		return
	}
	if def, ok := c.Profiles["default"]; ok {
		fmt.Println("Current state:")
		fmt.Printf("  default profile : %s/%s\n",
			displayProviderOrEmpty(def.Provider), displayModelOrEmpty(def.Model))
		if def.BaseURL != "" {
			fmt.Printf("  base url       : %s\n", def.BaseURL)
		}
		if def.APIKey != "" {
			fmt.Printf("  api key        : %s\n", redactAPIKey(def.APIKey))
		}
	} else if c.Synth != nil && c.Synth.Model != "" {
		fmt.Println("Current state:")
		fmt.Printf("  legacy synth   : %s/%s\n",
			displayProviderOrEmpty(c.Synth.BaseURL), c.Synth.Model)
	} else {
		fmt.Println("No configuration found.")
	}
	if len(c.Profiles) > 1 {
		fmt.Printf("  profiles       : %s\n", strings.Join(sortedKeysForConfig(c.Profiles), ", "))
	}
	if emb, ok := c.Profiles["embedding"]; ok {
		fmt.Printf("  embedding       : %s/%s\n", displayProviderOrEmpty(emb.Provider), displayModelOrEmpty(emb.Model))
	}
	if c.SynthesisEnabled != nil && !*c.SynthesisEnabled {
		fmt.Println("  synthesis       : disabled")
	}
	fmt.Println()
}

func displayProviderOrEmpty(p string) string {
	if p == "" {
		return "(unset)"
	}
	return p
}

func displayModelOrEmpty(m string) string {
	if m == "" {
		return "(unset)"
	}
	return m
}

// wizardEmbeddingStep presents the embedding subsystem in the wizard
// without re-implementing detect-embedding. When embedding is already
// configured, defaults to "leave unchanged". Offers auto-detect as a
// sub-prompt that delegates to the same Ollama probe used by
// `mpm config detect-embedding`.
func wizardEmbeddingStep(c *config.Config) {
	fmt.Println("Embedding")
	embedCfg := mpminternal.DefaultEmbeddingConfig()
	if embedCfg.IntentionallyDisabled {
		fmt.Println("  current: intentionally disabled")
	} else if embedCfg.Source == mpminternal.EmbeddingSourceProfile {
		fmt.Printf("  current: %s/%s (profile: %s)\n",
			embedCfg.ProviderName, embedCfg.ProfileName, embedCfg.ProfileName)
	} else if embedCfg.Source == mpminternal.EmbeddingSourceEnvFallback {
		fmt.Println("  current: env (OLLAMA_ENDPOINT/OLLAMA_MODEL)")
	} else {
		fmt.Println("  current: not configured")
	}
	fmt.Println("  1. Leave unchanged")
	fmt.Println("  2. Detect local Ollama embedding model")
	fmt.Println("  3. Configure manually (`mpm config profile ...` + `mpm config component set embedding <name>`)")
	fmt.Println("  4. Disable embedding")
	choice := promptString(rwFromStdin(), "Choice", "1")
	switch strings.TrimSpace(choice) {
	case "2":
		// Delegate to the canonical detect-embedding probe.
		cmd := &DetectEmbeddingCmd{}
		cmd.Run()
	case "4":
		if c.Components == nil {
			c.Components = map[string]string{}
		}
		c.Components["embedding"] = "disabled"
		fmt.Println("Embedding disabled.")
	default:
		fmt.Println("Embedding unchanged.")
	}
}

// wizardCriticStep presents the only specialist model that matters in
// v0.1: critic. When the operator already has a critic profile bound,
// defaults to "no change". When the operator accepts, configures
// profiles.critic and binds components.critic = "critic".
func wizardCriticStep(c *config.Config) {
	boundName, hasCriticBinding := c.Components["critic"]
	_, hasCriticProfile := c.Profiles["critic"]
	fmt.Println("Specialised models")
	if hasCriticBinding && hasCriticProfile {
		fmt.Printf("  critic profile  : %s/%s\n",
			displayProviderOrEmpty(c.Profiles[boundName].Provider),
			displayModelOrEmpty(c.Profiles[boundName].Model))
		fmt.Println("  Configure a separate critic model? [y/N]")
	} else {
		fmt.Println("  critic profile  : (using default model)")
		fmt.Println("  Configure a separate critic model? [y/N]")
	}
	answer := promptString(rwFromStdin(), "", "N")
	if strings.EqualFold(strings.TrimSpace(answer), "y") {
		fmt.Println("  Profile name [critic]:")
		name := strings.TrimSpace(promptString(rwFromStdin(), "", "critic"))
		if name == "" {
			name = "critic"
		}
		if c.Profiles == nil {
			c.Profiles = map[string]config.Profile{}
		}
		c.Profiles[name] = config.Profile{Name: name}
		if c.Components == nil {
			c.Components = map[string]string{}
		}
		c.Components["critic"] = name
		fmt.Printf("Created empty profile %q. Use `mpm config profile set %s provider ...` to fill.\n", name, name)
		fmt.Println("(Component bound. Run `mpm config validate` after filling the profile.)")
	} else {
		fmt.Println("Critic unchanged.")
	}
}

// wizardShowCapabilities prints the built-in capability mappings so
// the operator understands the semantic-routing layer without having
// to discover it through `mpm config capability list`.
func wizardShowCapabilities() {
	fmt.Println("Capabilities (built-in routing defaults)")
	keys := sortedKeysForConfig(config.DefaultCapabilities)
	for _, k := range keys {
		fmt.Printf("  %-9s → %s\n", k, config.DefaultCapabilities[k])
	}
	fmt.Println("Use `mpm config capability ...` to override.")
}

// wizardShowSynthesis displays the current synthesis state and points
// at `mpm config set synthesis_enabled` for changes. The wizard does
// not force a synthesis decision on every run.
func wizardShowSynthesis(c *config.Config) {
	state := "enabled"
	if c.SynthesisEnabled != nil && !*c.SynthesisEnabled {
		state = "disabled"
	}
	fmt.Printf("Background synthesis: %s\n", state)
	fmt.Println("Use `mpm config set synthesis_enabled true|false` to change.")
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// configLookup resolves a key (incl. aliases) against the loaded
// config and returns the canonical display value.
//
// Read path: Profiles["default"] is canonical. The legacy top-level
// `synth` block is consulted as a last-resort fallback for old
// installs that haven't migrated yet.
func configLookup(c *config.Config, key string) (string, error) {
	if c == nil {
		return "", fmt.Errorf("no config loaded")
	}
	canon := configCanonicalKey(key)
	// Top-level keys — synthesis kill switch.
	if canon == "synthesis_enabled" {
		if c.SynthesisEnabled == nil {
			return "true", nil
		}
		if *c.SynthesisEnabled {
			return "true", nil
		}
		return "false", nil
	}
	// Profile-scoped keys — read from Profiles["default"].
	if prof, ok := c.Profiles["default"]; ok {
		switch canon {
		case "model":
			return prof.Model, nil
		case "api_key":
			return prof.APIKey, nil
		case "base_url":
			return prof.BaseURL, nil
		case "max_tokens":
			return intToStr(prof.MaxTokens), nil
		case "timeout_seconds":
			return intToStr(prof.TimeoutSecs), nil
		}
	}
	// Legacy fallback — Synth block when Profiles["default"] is missing.
	if c.Synth != nil {
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
	}
	return "", fmt.Errorf("mpm config get: unknown key %q (valid keys: model, api_key, base_url, max_tokens, timeout_seconds, synthesis_enabled)", key)
}

// configApply mutates the loaded config in place. Pure mutation
// helper; persistence happens in handleConfigSet via SaveConfig.
//
// Write path: Profiles["default"] is the canonical destination for
// LLM-related keys. The legacy top-level `synth` block is no longer
// written by this command — operators with pre-profiles installs
// should run `mpm config profile add default` once.
func configApply(c *config.Config, key, val string) error {
	canon := configCanonicalKey(key)
	// Top-level keys (not inside a profile).
	switch canon {
	case "synthesis_enabled":
		b, err := strconv.ParseBool(val)
		if err != nil {
			return fmt.Errorf("synthesis_enabled must be a boolean (true|false); got %q", val)
		}
		c.SynthesisEnabled = &b
		return nil
	}
	// Profile-scoped keys — write to Profiles["default"].
	if c.Profiles == nil {
		c.Profiles = map[string]config.Profile{}
	}
	prof := c.Profiles["default"]
	if prof.Name == "" {
		prof.Name = "default"
	}
	switch canon {
	case "model":
		prof.Model = val
	case "api_key":
		prof.APIKey = val
	case "base_url":
		prof.BaseURL = val
	case "max_tokens":
		n, err := strconvAtoi(val)
		if err != nil || n <= 0 {
			return fmt.Errorf("max_tokens must be a positive integer (got %q)", val)
		}
		prof.MaxTokens = n
	case "timeout_seconds":
		n, err := strconvAtoi(val)
		if err != nil || n <= 0 {
			return fmt.Errorf("timeout_seconds must be a positive integer (got %q)", val)
		}
		prof.TimeoutSecs = n
	default:
		return fmt.Errorf("mpm config set: unknown key %q (valid keys: model, api_key, base_url, max_tokens, timeout_seconds, synthesis_enabled)", key)
	}
	c.Profiles["default"] = prof
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

// mergeProfileDefaults fills empty fields in target with values from src.
// Non-empty fields in target are preserved. The Profile-aware counterpart
// of mergeDefaults; the wizard uses it when writing to Profiles["default"].
func mergeProfileDefaults(target *config.Profile, src config.Profile) {
	if target == nil {
		return
	}
	if target.Provider == "" {
		target.Provider = src.Provider
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
	defaults config.Profile
}

// rwFromStdin returns a buffered reader around stdin. Used by
// every prompt.
func rwFromStdin() *bufio.Reader {
	return bufio.NewReader(os.Stdin)
}

// promptChoice prints a numbered list and reads a selection.
func promptChoice(rw *bufio.Reader, header string, choices []choice) *choice {
	return promptChoiceDefault(rw, header, choices, "1")
}

// promptChoiceDefault is promptChoice with a configurable default
// choice. The default choice is shown in the prompt and used when the
// operator presses enter. State-aware wizards pass the preset id that
// matches the existing configuration so re-running the wizard doesn't
// silently switch the operator's provider.
func promptChoiceDefault(rw *bufio.Reader, header string, choices []choice, defaultID string) *choice {
	defaultIdx := 1
	for i, c := range choices {
		if c.id == defaultID {
			defaultIdx = i + 1
			break
		}
	}
	fmt.Printf("\n  %s\n", header)
	for i, c := range choices {
		fmt.Printf("    %d. %s\n", i+1, c.label)
	}
	fmt.Printf("\n  Choose [%d-%d] (default: %d): ", 1, len(choices), defaultIdx)
	raw, _ := rw.ReadString('\n')
	raw = strings.TrimSpace(raw)
	if raw == "" {
		raw = strconv.Itoa(defaultIdx)
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

// splitProviderModel splits EmbeddingConfig.ProviderName ("provider:model")
// into separate provider/model strings for human-readable display. If the
// input has no colon (e.g. "null" or unexpected shapes), the entire input
// is returned as the provider with an empty model. The display formatter
// always renders the provider line; the model line is suppressed when empty.
func splitProviderModel(s string) (provider, model string) {
	if s == "" || s == "null" {
		return s, ""
	}
	for i := 0; i < len(s); i++ {
		if s[i] == ':' {
			return s[:i], s[i+1:]
		}
	}
	return s, ""
}

// ---------------------------------------------------------------------------
// Help
// ---------------------------------------------------------------------------

// printConfigHelp prints the mpm config help block via the canonical
// visual grammar (cmd/mpm/render). 2026-09-14 release-pass:
//
//   - Heading is `MPM · Config` (was `mpm config — Configure the AI
//     provider`).
//   - Wording distinguishes "LLM provider" from "Embedding model".
//   - The legacy synth block is documented as read-only migration
//     history, not a configuration surface.
func printConfigHelp() {
	render.Heading(os.Stdout, "Config")
	render.BlankLine(os.Stdout)
	render.Section(os.Stdout, "LLM provider and embedding model configuration")
	render.Plain(os.Stdout, "MPM distinguishes two provider roles:")
	render.Label(os.Stdout, "LLM provider", "used for generation and reasoning-backed capabilities (synthesis, critic/review)")
	render.Label(os.Stdout, "Embedding model", "used for semantic / vector similarity retrieval — OPTIONAL; absence is informational, not a defect")
	render.BlankLine(os.Stdout)

	render.Section(os.Stdout, "Configuration model (v0.1)")
	render.Label(os.Stdout, "profiles", "named execution profiles (provider, model, base_url, api_key, ...). profiles[\"default\"] is the canonical fallback for every component that has no explicit binding.")
	render.Label(os.Stdout, "components", "optional component → profile bindings. components[\"embedding\"] identifies the embedding profile.")
	render.Label(os.Stdout, "capabilities", "optional capability → component bindings. Canonical v0.1 defaults: reviewer→critic, reflect→critic, planner→memory, summarise→memory. Use 'mpm config capability set <cap> <component>' to override.")
	render.Label(os.Stdout, "legacy synth", "read-only migration history; pre-profiles installs left a top-level 'synth' block on disk that new code never writes. Migrate by running 'mpm config profile add default'.")
	render.BlankLine(os.Stdout)

	render.Section(os.Stdout, "Usage")
	render.Label(os.Stdout, "mpm config", "interactive wizard (writes profiles.default)")
	render.Label(os.Stdout, "mpm config show | list", "show current configuration")
	render.Label(os.Stdout, "mpm config get <key>", "get one value")
	render.Label(os.Stdout, "mpm config set <key> <value>", "set one value on profiles.default")
	render.Label(os.Stdout, "mpm config edit", "open mpm_config.json in $EDITOR")
	render.Label(os.Stdout, "mpm config validate", "validate configuration shape")
	render.Label(os.Stdout, "mpm config profile ...", "add / list / get / set / remove profiles — see 'mpm config profile --help'")
	render.Label(os.Stdout, "mpm config component ...", "list / get / set component → profile bindings — see 'mpm config component --help'")
	render.Label(os.Stdout, "mpm config capability ...", "list / get / set capability → component bindings")
	render.Label(os.Stdout, "mpm config detect-embedding [--apply <name>] [--force]", "probe Ollama for embedding-capable models; --apply writes a profile and binds components.embedding. See 'mpm config detect-embedding --help'")
	render.BlankLine(os.Stdout)

	render.Section(os.Stdout, "Keys (canonical names; aliases accepted)")
	render.Label(os.Stdout, "model, api_key (alias: token), base_url (alias: endpoint), max_tokens, timeout_seconds, synthesis_enabled", "")
	render.BlankLine(os.Stdout)

	render.Section(os.Stdout, "Examples")
	render.Plain(os.Stdout, "  mpm config set api_key $OPENAI_API_KEY")
	render.Plain(os.Stdout, "  mpm config set model gpt-4o")
	render.Plain(os.Stdout, "  mpm config set endpoint https://api.openai.com/v1")
	render.Plain(os.Stdout, "  mpm config set synthesis_enabled false")
	render.BlankLine(os.Stdout)

	render.Hint(os.Stdout, "API keys are persisted to mpm_config.json (file mode 0600). 'mpm config show' redacts them; 'mpm config get api_key' returns the full key for the operator's own use.")
	render.Hint(os.Stdout, "Config file: ~/.mpm/mpm_config.json (path resolved via the workspace; $EDITOR is opened on this file for 'mpm config edit').")
}

// printConfigProfileHelp prints `mpm config profile --help` via the
// canonical visual grammar. Subcommand-specific help surfaces were
// unreachable from `--help` prior to the 2026-09-14 release-pass.
func printConfigProfileHelp() {
	render.Heading(os.Stdout, "Config profile")
	render.BlankLine(os.Stdout)
	render.Section(os.Stdout, "Manage named execution profiles")
	render.Plain(os.Stdout, "A profile binds provider / model / base_url / api_key and")
	render.Plain(os.Stdout, "is referenced by component bindings or used directly.")
	render.BlankLine(os.Stdout)
	render.Section(os.Stdout, "Subcommands")
	render.Label(os.Stdout, "mpm config profile add [name]", "interactive wizard; pass a name to skip the prompt")
	render.Label(os.Stdout, "mpm config profile list", "render all profiles")
	render.Label(os.Stdout, "mpm config profile get <name>", "show one profile")
	render.Label(os.Stdout, "mpm config profile set <name> <key> <value>", "set one field (provider, model, base_url, api_key, temperature, max_tokens, timeout_seconds, reasoning)")
	render.Label(os.Stdout, "mpm config profile remove <name>", "delete; refused if any component binds to this profile")
	render.BlankLine(os.Stdout)
	render.Hint(os.Stdout, "Run 'mpm config profile add default' once on a fresh install to seed the canonical fallback profile.")
}

// printConfigComponentHelp prints `mpm config component --help`.
func printConfigComponentHelp() {
	render.Heading(os.Stdout, "Config component")
	render.BlankLine(os.Stdout)
	render.Section(os.Stdout, "Component → profile bindings")
	render.Plain(os.Stdout, "Components are runtime subsystems (memory, critic, scheduler).")
	render.Plain(os.Stdout, "A binding maps a component to a profile; absent bindings")
	render.Plain(os.Stdout, "fall back to profiles[\"default\"].")
	render.BlankLine(os.Stdout)
	render.Section(os.Stdout, "Subcommands")
	render.Label(os.Stdout, "mpm config component list", "render all bindings")
	render.Label(os.Stdout, "mpm config component get <component>", "show one binding")
	render.Label(os.Stdout, "mpm config component set <component> <profile>", "set or replace a binding")
	render.BlankLine(os.Stdout)
}

// printConfigDetectEmbeddingHelp prints `mpm config detect-embedding --help`.
func printConfigDetectEmbeddingHelp() {
	render.Heading(os.Stdout, "Config detect-embedding")
	render.BlankLine(os.Stdout)
	render.Section(os.Stdout, "Probe Ollama for embedding-capable models")
	render.Plain(os.Stdout, "Detection enumerates candidates — it does NOT imply")
	render.Plain(os.Stdout, "configuration. To persist a choice, pass --apply.")
	render.BlankLine(os.Stdout)
	render.Section(os.Stdout, "Flags")
	render.Label(os.Stdout, "--apply <name>", "write a profile named <name> and bind components.embedding to it")
	render.Label(os.Stdout, "--force", "overwrite an existing profile or rebind an existing component")
	render.BlankLine(os.Stdout)
	render.Hint(os.Stdout, "Without --apply, the probe is informational only and does not modify mpm_config.json.")
}

// (syscall imported for isatty() — package main already in scope.)
var _ = syscall.Stdin

// ---------------------------------------------------------------------------
// Profile subcommands (mpm config profile <sub>)
// ---------------------------------------------------------------------------

// handleConfigProfile routes profile management subcommands.
//
//   mpm config profile add [name]                   Interactive wizard /
//                                                  accept-name-from-stdin
//   mpm config profile list                       Render all profiles
//   mpm config profile get <name>                  Show one profile
//   mpm config profile set <name> <key> <value>    Set one field
//   mpm config profile remove <name>               Delete; refuse if
//                                                  any component binds
//                                                  to this profile
func handleConfigProfile(args []string) int {
	// 2026-09-14 release-pass: --help / -h / "help" (the
	// parseFlags rewrite) at any position short-circuits to the
	// canonical profile help page. Without this, `mpm config
	// profile --help` collapsed to the generic registry fallback.
	for _, a := range args {
		if a == "-h" || a == "--help" || a == "help" {
			printConfigProfileHelp()
			return 0
		}
	}
	if len(args) == 0 {
		usererror.Error("mpm config profile <sub> — need one of: add, list, get, set, remove")
		return 1
	}
	switch args[0] {
	case "add":
		name := ""
		if len(args) >= 2 {
			name = args[1]
		}
		return handleProfileAdd(loadOrInitConfig(), name)
	case "list":
		return handleProfileList(loadOrInitConfig())
	case "get":
		if len(args) < 2 {
			usererror.Error("mpm config profile get <name>")
			return 1
		}
		return handleProfileGet(loadOrInitConfig(), args[1])
	case "set":
		if len(args) < 4 {
			usererror.Error("mpm config profile set <name> <key> <value>\n  keys: provider, model, base_url, api_key, temperature, max_tokens, timeout_seconds, reasoning")
			return 1
		}
		return handleProfileSet(loadOrInitConfig(), args[1], args[2], strings.Join(args[3:], " "))
	case "remove":
		if len(args) < 2 {
			usererror.Error("mpm config profile remove <name>")
			return 1
		}
		return handleProfileRemove(loadOrInitConfig(), args[1])
	default:
		usererror.Error("mpm config profile: unknown subcommand %q — try add|list|get|set|remove", args[0])
		return 1
	}
}

func handleProfileAdd(c *config.Config, name string) int {
	if c.Profiles == nil {
		c.Profiles = map[string]config.Profile{}
	}
	if name == "" {
		if !isatty(os.Stdin) {
			usererror.Error("mpm config profile add requires a name in non-interactive mode")
			return 1
		}
		name = strings.TrimSpace(promptString(rwFromStdin(), "Profile name", ""))
		if name == "" {
			fmt.Println("Aborted.")
			return 0
		}
	}
	if _, exists := c.Profiles[name]; exists {
		usererror.Error("profile %q already exists — use 'mpm config profile set' to update fields", name)
		return 1
	}
	p := config.Profile{Name: name}
	if isatty(os.Stdin) {
		p.Provider = strings.TrimSpace(promptString(rwFromStdin(), "Provider (openai/anthropic/ollama/custom)", "custom"))
		p.Model = strings.TrimSpace(promptString(rwFromStdin(), "Model", ""))
		p.BaseURL = strings.TrimSpace(promptString(rwFromStdin(), "Base URL", ""))
		if p.Provider != "ollama" {
			p.APIKey = strings.TrimSpace(promptSecret(rwFromStdin(), "API key", ""))
		}
		tempStr := promptString(rwFromStdin(), "Temperature (0.0-2.0)", "0.2")
		if t, err := strconvAtoiFloat(tempStr); err == nil {
			p.Temperature = &t
		}
	} else {
		fmt.Printf("Created empty profile %q. Use 'mpm config profile set %s <key> <value>' to fill.\n", name, name)
	}
	c.Profiles[name] = p
	if err := config.SaveConfig(c); err != nil {
		usererror.Error("saving config: %v", err)
		return 1
	}
	fmt.Printf("✓ profile %q added\n", name)
	return 0
}

func handleProfileList(c *config.Config) int {
	fmt.Println("Execution profiles")
	fmt.Println(strings.Repeat("─", 60))
	if len(c.Profiles) == 0 {
		fmt.Println("  (no profiles configured — run `mpm config profile add <name>` or `mpm config` to set one up)")
		return 0
	}
	for _, name := range sortedKeysForConfig(c.Profiles) {
		p := c.Profiles[name]
		fmt.Printf("  %s\n", name)
		fmt.Printf("    provider    : %s\n", p.Provider)
		fmt.Printf("    model       : %s\n", p.Model)
		if p.BaseURL != "" {
			fmt.Printf("    base url    : %s\n", p.BaseURL)
		}
		if p.APIKey != "" {
			fmt.Printf("    api key     : %s\n", redactAPIKey(p.APIKey))
		}
		if p.Temperature != nil {
			fmt.Printf("    temperature : %.2f\n", *p.Temperature)
		}
		if p.MaxTokens > 0 {
			fmt.Printf("    max tokens  : %d\n", p.MaxTokens)
		}
		if p.TimeoutSecs > 0 {
			fmt.Printf("    timeout sec : %d\n", p.TimeoutSecs)
		}
		if p.Reasoning != "" {
			fmt.Printf("    reasoning   : %s\n", p.Reasoning)
		}
		fmt.Println()
	}
	return 0
}

func handleProfileGet(c *config.Config, name string) int {
	p, ok := c.Profiles[name]
	if !ok {
		usererror.Error("profile %q not found", name)
		return 1
	}
	fmt.Printf("profile %q:\n", name)
	fmt.Printf("  provider    : %s\n", p.Provider)
	fmt.Printf("  model       : %s\n", p.Model)
	if p.BaseURL != "" {
		fmt.Printf("  base url    : %s\n", p.BaseURL)
	}
	if p.APIKey != "" {
		fmt.Printf("  api key     : %s\n", redactAPIKey(p.APIKey))
	}
	if p.Temperature != nil {
		fmt.Printf("  temperature : %.2f\n", *p.Temperature)
	}
	if p.MaxTokens > 0 {
		fmt.Printf("  max tokens  : %d\n", p.MaxTokens)
	}
	if p.TimeoutSecs > 0 {
		fmt.Printf("  timeout sec : %d\n", p.TimeoutSecs)
	}
	if p.Reasoning != "" {
		fmt.Printf("  reasoning   : %s\n", p.Reasoning)
	}
	return 0
}

func handleProfileSet(c *config.Config, name, key, value string) int {
	if c.Profiles == nil {
		c.Profiles = map[string]config.Profile{}
	}
	p, ok := c.Profiles[name]
	if !ok {
		usererror.Error("profile %q not found — use 'mpm config profile add %s' first", name, name)
		return 1
	}
	switch strings.ToLower(strings.ReplaceAll(key, "-", "_")) {
	case "provider":
		p.Provider = value
	case "model":
		// 2026-09-14 release-pass: route the `model` setter
		// through the same role-validation boundary the
		// wizard uses. An embedding-only model assigned to an
		// LLM-bound profile is rejected before the config is
		// written — the brief explicitly forbids an
		// embedding model reaching the LLM prompt path
		// through a normal public surface.
		//
		// The validator is skipped when the profile is
		// explicitly bound to `Components["embedding"]` (the
		// operator wants an embedding model there) or when the
		// provider is not Ollama-like (custom remote endpoints
		// with unknown capability are accepted to preserve
		// flexibility).
		if !isEmbeddingBoundProfile(c, name) {
			if decision := ValidateLLMRole(p.Provider, value, p.BaseURL); decision == RoleEmbeddingOnly {
				fmt.Println(RejectEmbeddingOnlyLLM(value))
				return 1
			}
		}
		p.Model = value
	case "base_url", "endpoint", "baseurl":
		p.BaseURL = value
	case "api_key", "token", "apikey":
		p.APIKey = value
	case "temperature":
		t, err := strconvAtoiFloat(value)
		if err != nil {
			usererror.Error("temperature must be a number 0.0-2.0 (got %q)", value)
			return 1
		}
		p.Temperature = &t
	case "max_tokens", "maxtokens", "max":
		n, err := strconvAtoi(value)
		if err != nil || n <= 0 {
			usererror.Error("max_tokens must be a positive integer (got %q)", value)
			return 1
		}
		p.MaxTokens = n
	case "timeout_seconds", "timeout":
		n, err := strconvAtoi(value)
		if err != nil || n <= 0 {
			usererror.Error("timeout_seconds must be a positive integer (got %q)", value)
			return 1
		}
		p.TimeoutSecs = n
	case "reasoning":
		p.Reasoning = value
	default:
		usererror.Error("unknown profile field %q (try: provider, model, base_url, api_key, temperature, max_tokens, timeout_seconds, reasoning)", key)
		return 1
	}
	c.Profiles[name] = p
	if err := config.SaveConfig(c); err != nil {
		usererror.Error("saving config: %v", err)
		return 1
	}
	fmt.Printf("✓ profile %q %s set to %q\n", name, key, value)
	return 0
}

// isEmbeddingBoundProfile reports whether the profile is
// the active binding for Components["embedding"]. When true,
// the LLM-role validator is bypassed — the operator wants an
// embedding model in this profile by design.
func isEmbeddingBoundProfile(c *config.Config, profileName string) bool {
	if c == nil || c.Components == nil {
		return false
	}
	return c.Components["embedding"] == profileName
}

func handleProfileRemove(c *config.Config, name string) int {
	if _, ok := c.Profiles[name]; !ok {
		usererror.Error("profile %q not found", name)
		return 1
	}
	for comp, bound := range c.Components {
		if bound == name {
			usererror.Error("cannot remove profile %q: component %q is bound to it\n  unbind first: 'mpm config component set %s <other-profile>'", name, comp, comp)
			return 1
		}
	}
	delete(c.Profiles, name)
	if err := config.SaveConfig(c); err != nil {
		usererror.Error("saving config: %v", err)
		return 1
	}
	fmt.Printf("✓ profile %q removed\n", name)
	return 0
}

// sortedKeys returns the map's keys in lexical order. Used to give
// profile + component listings a stable output shape.
func sortedKeysForConfig[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1] > out[j]; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}

// strconvAtoiFloat parses a string as a float without importing
// strconv twice. Keeps the imports lean.
func strconvAtoiFloat(s string) (float64, error) {
	n := 0.0
	frac := 1.0
	seenDot := false
	for _, c := range s {
		switch {
		case c == '.' && !seenDot:
			seenDot = true
			frac = 1
		case c >= '0' && c <= '9':
			d := float64(c - '0')
			if seenDot {
				frac *= 10
				n += d / frac
			} else {
				n = n*10 + d
			}
		default:
			return 0, fmt.Errorf("not a number: %q", s)
		}
	}
	return n, nil
}

// ---------------------------------------------------------------------------
// Component subcommands (mpm config component <sub>)
// ---------------------------------------------------------------------------

// handleConfigComponent routes component binding subcommands.
//
//   mpm config component list                Render all bindings
//   mpm config component get <component>     Show one binding
//   mpm config component set <comp> <profile> Set binding
func handleConfigComponent(args []string) int {
	// 2026-09-14 release-pass: --help / -h / "help" (the
	// parseFlags rewrite) short-circuit.
	for _, a := range args {
		if a == "-h" || a == "--help" || a == "help" {
			printConfigComponentHelp()
			return 0
		}
	}
	if len(args) == 0 {
		usererror.Error("mpm config component <sub> — need one of: list, get, set")
		return 1
	}
	switch args[0] {
	case "list":
		return handleComponentList(loadOrInitConfig())
	case "get":
		if len(args) < 2 {
			usererror.Error("mpm config component get <name>")
			return 1
		}
		return handleComponentGet(loadOrInitConfig(), args[1])
	case "set":
		if len(args) < 3 {
			usererror.Error("mpm config component set <component> <profile>")
			return 1
		}
		return handleComponentSet(loadOrInitConfig(), args[1], args[2])
	default:
		usererror.Error("mpm config component: unknown subcommand %q — try list|get|set", args[0])
		return 1
	}
}

// knownComponents is the v0.1 allow-list for components. The
// underlying map accepts any name, but the visible component list
// surfaces these in stable order. Future RFCs append names here.
var knownComponents = []string{"memory", "critic", "scheduler"}

func handleComponentList(c *config.Config) int {
	fmt.Println("Component bindings")
	fmt.Println(strings.Repeat("─", 60))
	if len(c.Components) == 0 {
		fmt.Println("  (no components bound — defaults to 'default' profile via ProfileFor fallback)")
		return 0
	}
	for _, comp := range sortedKeysForConfig(c.Components) {
		bound := c.Components[comp]
		if bound == "" {
			bound = "(default)"
		}
		fmt.Printf("  %-12s → %s\n", comp, bound)
	}
	return 0
}

func handleComponentGet(c *config.Config, component string) int {
	bound := c.Components[component]
	if bound == "" {
		bound = "(default)"
	}
	fmt.Printf("  %s → %s\n", component, bound)
	return 0
}

func handleComponentSet(c *config.Config, component, profile string) int {
	if c.Components == nil {
		c.Components = map[string]string{}
	}
	// Empty profile string → unset binding (component falls back
	// to ProfileFor's default / Synth legacy chain).
	if profile == "" {
		delete(c.Components, component)
		if err := config.SaveConfig(c); err != nil {
			usererror.Error("saving config: %v", err)
			return 1
		}
		fmt.Printf("✓ component %q unbound\n", component)
		return 0
	}
	if _, ok := c.Profiles[profile]; !ok {
		usererror.Error("profile %q not found — define it first with 'mpm config profile add %s'", profile, profile)
		return 1
	}
	c.Components[component] = profile
	if err := config.SaveConfig(c); err != nil {
		usererror.Error("saving config: %v", err)
		return 1
	}
	fmt.Printf("✓ component %q → profile %q\n", component, profile)
	return 0
}

// ---------------------------------------------------------------------------
// Capability subcommands (mpm config capability <sub>)
// ---------------------------------------------------------------------------

// handleConfigCapability routes capability binding subcommands.
//
//   mpm config capability list                Render all bindings
//   mpm config capability get <capability>    Show one binding
//   mpm config capability set <cap> <comp>    Bind capability to component
//
// Capabilities are the operator-meaningful vocabulary that Skills and
// runtime code address. Components are the substrate-specific
// functions that fulfil them. Default v0.1 capabilities:
//
//   planner   → memory
//   reviewer  → critic
//   reflect   → critic
//   summarise → memory
//
// Operators can override any of these or add their own. The runtime
// resolves at skill-execution time: skill says 'I need reviewer',
// config says reviewer → critic, ProfileFor(critic) returns the
// model.
func handleConfigCapability(args []string) int {
	if len(args) == 0 {
		usererror.Error("mpm config capability <sub> — need one of: list, get, set")
		return 1
	}
	switch args[0] {
	case "list":
		return handleCapabilityList(loadOrInitConfig())
	case "get":
		if len(args) < 2 {
			usererror.Error("mpm config capability get <capability>")
			return 1
		}
		return handleCapabilityGet(loadOrInitConfig(), args[1])
	case "set":
		if len(args) < 3 {
			usererror.Error("mpm config capability set <capability> <component>")
			return 1
		}
		return handleCapabilitySet(loadOrInitConfig(), args[1], args[2])
	default:
		usererror.Error("mpm config capability: unknown subcommand %q — try list|get|set", args[0])
		return 1
	}
}

// handleCapabilityList renders the capability registry. The CLI uses
// Config.CapabilityFor (which falls back to config.DefaultCapabilities)
// so explicit overrides and defaults render consistently.
func handleCapabilityList(c *config.Config) int {
	fmt.Println("Capability registry")
	fmt.Println(strings.Repeat("─", 60))
	if c.Capabilities == nil {
		fmt.Println("  (no explicit capabilities — defaults loaded on first use)")
		fmt.Println()
		for _, cap := range sortedKeysForConfig(config.DefaultCapabilities) {
			fmt.Printf("  default  %-12s → %s\n", cap, config.DefaultCapabilities[cap])
		}
		return 0
	}
	for _, cap := range sortedKeysForConfig(c.Capabilities) {
		bound := c.CapabilityFor(cap)
		if bound == "" {
			bound = "(unbound)"
		}
		fmt.Printf("  %-12s → %s\n", cap, bound)
	}
	return 0
}

func handleCapabilityGet(c *config.Config, capability string) int {
	bound := c.CapabilityFor(capability)
	if bound == "" {
		fmt.Printf("  %s → (unbound)\n", capability)
		return 0
	}
	fmt.Printf("  %s → %s\n", capability, bound)
	return 0
}

func handleCapabilitySet(c *config.Config, capability, component string) int {
	if c.Capabilities == nil {
		c.Capabilities = map[string]string{}
	}
	// Empty component → remove the binding (use defaults).
	if component == "" {
		delete(c.Capabilities, capability)
		if err := config.SaveConfig(c); err != nil {
			usererror.Error("saving config: %v", err)
			return 1
		}
		fmt.Printf("✓ capability %q unbound (will use default)\n", capability)
		return 0
	}
	c.Capabilities[capability] = component
	if err := config.SaveConfig(c); err != nil {
		usererror.Error("saving config: %v", err)
		return 1
	}
	fmt.Printf("✓ capability %q → component %q\n", capability, component)
	return 0
}
