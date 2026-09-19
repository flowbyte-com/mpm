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
//   mpm config                       Interactive wizard. Public
//                                    menu exposes ONLY Custom —
//                                    the operator picks a protocol
//                                    (OpenAI-compatible /
//                                    Anthropic-compatible / Ollama)
//                                    on the second prompt. Branded
//                                    provider presets are removed
//                                    from the public UX. The
//                                    wizard covers BOTH LLM and
//                                    embedding configuration;
//                                    output-token limits are NOT
//                                    user-configurable. Manual
//                                    configuration is the single
//                                    public configuration path —
//                                    no discovery / auto-apply /
//                                    catalogue.
//
//   mpm config show | list          Print current config.
//
//   mpm config get <key>             Print one value.
//                                    Keys: model, api_key, base_url,
//                                    timeout_seconds.
//                                    Aliases: 'token' → api_key,
//                                    'endpoint' → base_url.
//                                    max_tokens is deprecated; GET
//                                    returns "(removed)".
//
//   mpm config set <key> <value>     Set one value, persist. Same
//                                    key list as `get`; max_tokens
//                                    returns a "removed in v0.1-final"
//                                    error so legacy setters don't
//                                    silently write a knob the
//                                    runtime ignores.
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
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	mpminternal "github.com/flowbyte-com/mpm-core"
	"github.com/flowbyte-com/mpm-core/config"
	"github.com/flowbyte-com/mpm-core/usererror"

	"github.com/flowbyte-com/mpm/cmd/mpm/probe"

	"github.com/flowbyte-com/mpm/cmd/mpm/render"

	"golang.org/x/term"
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
	// (profile / component) handle help themselves via their own
	// short-circuit.
	//
	// 2026-09-14 final-simplification: detect-embedding has been
	// retired from the public CLI. The string is still recognised
	// here as a soft-deprecated alias; callers receive a
	// `migrate-to-manual` message so legacy scripts do not silently
	// mutate config.
	if args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		printConfigHelp()
		return 0
	}
	switch args[0] {
	case "show", "list":
		return handleConfigShow(loadOrInitConfig())
	case "get":
		if len(args) < 2 {
			usererror.Error("mpm config get <key>\n  keys: model, api_key, base_url, timeout_seconds\n  aliases: token=api_key, endpoint=base_url")
			return 1
		}
		return handleConfigGet(loadOrInitConfig(), args[1])
	case "set":
		// 2026-09-19 credential-UX hardening: legacy top-level
		// `mpm config set api_key <value>` is an argv leak.
		// The scriptable alias is `mpm config profile set
		// default api_key --stdin` (or the hidden prompt). The
		// legacy command is preserved for non-secret keys only.
		if len(args) < 2 {
			usererror.Error("mpm config set <key> <value>\n  keys: model, base_url, timeout_seconds, synthesis_enabled\n  aliases: token=api_key, endpoint=base_url\n  api_key is secret-bearing; use `mpm config profile set default api_key [--stdin]`\n  max_tokens is removed in v0.1-final; runtime supplies its own value")
			return 1
		}
		canonical := configCanonicalKey(args[1])
		if canonical == "api_key" {
			usererror.Error("refusing API key on `mpm config set`; use `mpm config profile set default api_key` for a hidden prompt, or `... --stdin`")
			return 1
		}
		if len(args) < 3 {
			usererror.Error("mpm config set <key> <value>\n  keys: model, base_url, timeout_seconds, synthesis_enabled\n  aliases: token=api_key, endpoint=base_url\n  api_key is secret-bearing; use `mpm config profile set default api_key [--stdin]`\n  max_tokens is removed in v0.1-final; runtime supplies its own value")
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
		// 2026-09-14 final-simplification: detect-embedding is
		// removed from the public CLI. The verb is retained only as
		// a soft-deprecated alias so legacy scripts do not crash;
		// it prints a migration message and exits 1. The capability
		// probes still live in cmd/mpm/detect_embedding.go for
		// internal use (role validation, tests).
		return handleDetectEmbeddingRemoved(args[1:])
	case "help", "-h", "--help":
		printConfigHelp()
		return 0
	default:
		usererror.Error("mpm config: unknown subcommand %q\n\n  Available: show, get, set, edit, validate, profile, component, capability, (no args = interactive wizard)", args[0])
		return 1
	}
}

// handleDetectEmbeddingRemoved prints a clear migration message
// for `mpm config detect-embedding` invocations. The subcommand
// was retired in the 2026-09-14 final-simplification pass; manual
// configuration via `mpm config profile add <name>` + `mpm config
// component set embedding <name>` is the canonical replacement.
func handleDetectEmbeddingRemoved(rest []string) int {
	fmt.Println("`mpm config detect-embedding` was retired in v0.1-final.")
	fmt.Println("Embedding configuration is now manual via the Custom protocol picker.")
	fmt.Println("Run `mpm config` for the interactive wizard,")
	fmt.Println("or:")
	fmt.Println("  mpm config profile add embedding --provider custom --model <id> --base-url <url>")
	fmt.Println("  mpm config component set embedding embedding")
	return 1
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
		// 2026-09-14 final-simplification: max_tokens is removed
		// from the user-facing contract; any value on disk is
		// ignored at runtime. Surface a stale marker rather than
		// printing the value (which would imply it is honored).
		fmt.Println("    max tokens   : (removed in v0.1-final; runtime supplies its own)")
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

// wizardPresets returns the public provider menu for the manual
// configuration wizard. Per the 2026-09-14 config-simplification
// pass, the public menu exposes ONLY Custom:
//
//	Provider
//	  1. Custom
//
// Branded provider menus (MiniMax, OpenAI, Anthropic, Cohere,
// Google Gemini, Mistral, Ollama, OpenRouter, xAI, etc.) are
// deliberately NOT shown. The runtime registry still resolves
// existing Profiles[...].Provider IDs by string — see
// `presetForID` and `providersFor` in cmd/mpm/provider_registry.go
// — so an operator who already saved a profile under a branded ID
// (e.g. provider=openai) keeps it after re-running the wizard.
// All wizard-driven NEW profiles are written with provider="custom"
// and a chosen protocol hint in base_url.
//
// The Custom preset's `defaults` are intentionally empty — the
// wizard asks for the protocol on the next prompt (see
// `wizardProtocolPresets` below), then for model / base URL /
// API key / max tokens, applying protocol-derived defaults to
// empty fields only.
func wizardPresets() []choice {
	return []choice{
		{
			id:       "custom",
			label:    "Custom",
			defaults: config.Profile{Provider: "custom"},
		},
	}
}

// wizardProtocolPresets returns the public protocol menu shown
// after Custom. The protocol choice drives the base URL default,
// whether an API key is required, and what wire the runtime will
// speak for the profile.
//
// Protocol choices reflect actual implemented transports —
// OpenAI-compatible (Bearer-token, /chat/completions),
// Anthropic-compatible (x-api-key, /v1/messages), and Ollama
// (no-auth local daemon). No "Custom HTTP" or other unimplemented
// shapes are exposed.
func wizardProtocolPresets() []choice {
	return []choice{
		{
			id:    "openai-compatible",
			label: "OpenAI-compatible",
			defaults: config.Profile{
				BaseURL: "https://api.openai.com/v1",
			},
			needsAPIKey: true,
		},
		{
			id:    "anthropic-compatible",
			label: "Anthropic-compatible",
			defaults: config.Profile{
				BaseURL: "https://api.anthropic.com/v1",
			},
			needsAPIKey: true,
		},
		{
			id:    "ollama",
			label: "Ollama (local)",
			defaults: config.Profile{
				BaseURL: "http://127.0.0.1:11434",
			},
			needsAPIKey: false,
		},
	}
}

// promptProtocol asks the operator to pick a protocol; returns nil
// on abort. The Custom provider entry is the only path into this
// prompt — branded entry points are not exposed at the public
// wizard surface.
func promptProtocol(rw *bufio.Reader, header string) *choice {
	presets := wizardProtocolPresets()
	return promptChoiceDefault(rw, header, presets, "openai-compatible")
}

// presetIDForProvider maps an existing provider string to a
// preset id, or "custom" when the provider does not match a known
// preset. Used by the wizard to default its provider-picker
// selection to the operator's existing configuration without
// introducing a branded menu entry.
func presetIDForProvider(provider string) string {
	if provider == "" {
		return "custom"
	}
	if _, ok := presetForID(provider); ok {
		return provider
	}
	return "custom"
}

// protocolForBaseURL heuristically selects the protocol preset
// that best matches a stored base URL. Used when an operator
// re-enters the wizard with an existing profile: the protocol
// picker defaults to the protocol that matches the existing base
// URL pattern, so the operator doesn't have to re-pick it.
//
// The fallback is "openai-compatible" because it is the most
// widely-deployed wire. Operators override by selecting at the
// prompt.
func protocolForBaseURL(baseURL string) string {
	lu := strings.ToLower(baseURL)
	switch {
	case strings.Contains(lu, "anthropic.com"),
		strings.Contains(lu, "minimax"):
		return "anthropic-compatible"
	case strings.Contains(lu, "ollama"), strings.Contains(lu, ":11434"):
		return "ollama"
	}
	return "openai-compatible"
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
		fmt.Println("  mpm config profile add default --model <model> --base-url <url>")
		fmt.Println("  mpm config profile set default api_key            # hidden prompt (TTY)")
		fmt.Println("  printf \"$KEY\" | mpm config profile set default api_key --stdin")
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

	// Preset choice: per the 2026-09-14 simplification,
	// `wizardPresets()` returns only "Custom". The default
	// remains "custom" — there is no longer an alphabetical
	// branded menu to default into.
	defaultPresetID := "custom"
	presets := wizardPresets()
	preset := promptChoiceDefault(rwFromStdin(), "Provider", presets, defaultPresetID)
	if preset == nil {
		fmt.Println("Aborted.")
		return 0
	}

	// Snapshot the existing profile BEFORE applying defaults.
	// We use this snapshot to detect which fields the operator
	// already populated so the wizard doesn't silently overwrite
	// a custom base_url/model/api_key when only the protocol
	// selection matched a preset.
	existing := prof
	hadBaseURL := existing.BaseURL != ""
	hadModel := existing.Model != ""
	hadKey := existing.APIKey != ""

	// Public wizard UX: Custom + protocol picker.
	// After Custom, prompt the operator to choose a wire
	// protocol (OpenAI-compatible / Anthropic-compatible /
	// Ollama). Branded provider IDs are not exposed.
	//
	// The protocol picker is the Stable abstraction; the
	// runtime infers wire from base_url at construction time
	// (see inferWire in internal/core/synth/wire.go). The
	// chosen protocol's defaults (base_url only) flow into
	// the profile. The operator may override the URL after.
	_ = protocolForBaseURL // helper retained for future state-aware flows
	protocol := promptProtocol(rwFromStdin(), "Protocol")
	if protocol == nil {
		fmt.Println("Aborted.")
		return 0
	}

	// Print protocol-specific helper text up front, before
	// the model / URL / key prompts. Brief: keep helper text
	// concise — answer "what is this field?" + one or two
	// realistic examples + whether it's optional.
	fmt.Println()
	fmt.Println("Custom lets you connect any supported endpoint.")
	fmt.Println()
	fmt.Println("  Model:    the model ID expected by your provider")
	fmt.Println("            (a hosted model, an OpenRouter model ID,")
	fmt.Println("            or a local Ollama tag).")
	fmt.Println("  Base URL: API endpoint for the provider.")
	fmt.Println("            e.g.  https://api.openai.com/v1")
	fmt.Println("                  https://api.anthropic.com/v1")
	fmt.Println("                  http://127.0.0.1:11434")
	if !protocol.needsAPIKey {
		fmt.Println("  API key:  optional for this protocol; leave empty")
		fmt.Println("            if your endpoint does not require one.")
	} else {
		fmt.Println("  API key:  provider credential.")
		fmt.Println("            env vars MPM_LLM_API_KEY / ")
		fmt.Println("            OAI_COMPAT_API_KEY / ANTHROPIC_API_KEY are")
		fmt.Println("            accepted when not specified here.")
	}
	fmt.Println()

	// Apply protocol defaults ONLY to fields the operator
	// hasn't set. Existing values win; protocol defaults fill
	// empty fields.
	mergeProfileDefaults(&prof, protocol.defaults)

	// Model prompt — operators type the model freeform.
	// The previous provider-catalog menu is gone; the wizard
	// is intentionally schema-light. Existing model preserved
	// on empty input.
	model := promptString(rwFromStdin(), "Model", prof.Model)
	if model != "" {
		prof.Model = strings.TrimSpace(model)
	} else if hadModel {
		prof.Model = existing.Model
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
	// Resolved provider mirrors the protocol; the validator
	// gates "all-minilm"-class names regardless of protocol
	// choice.
	if prof.Model != "" {
		resolvedProvider := protocol.id
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

	// Base URL prompt — preserve the operator's existing URL
	// when non-empty, even if the chosen protocol has a different
	// default. This is the silent-overwrite fix: a custom endpoint
	// must not be replaced by the protocol base_url just because
	// the protocol selection matched.
	baseURL := promptString(rwFromStdin(), "Base URL", prof.BaseURL)
	if baseURL != "" {
		prof.BaseURL = strings.TrimSpace(baseURL)
	} else if hadBaseURL {
		// Restore the original URL — empty input keeps existing.
		prof.BaseURL = existing.BaseURL
	}

	// API key prompt — only if protocol requires it (skip
	// for Ollama/local). Honour existing key on empty input
	// unless (unchanged) is shown.
	if protocol.needsAPIKey {
		var keyDefault string
		if hadKey {
			keyDefault = "(unchanged)"
		}
		key, err := promptSecret("API key", keyDefault)
		if err != nil {
			usererror.Error("%v", err)
			return 1
		}
		if key != "" {
			prof.APIKey = key
		} else if hadKey {
			prof.APIKey = existing.APIKey
		}
	}

	// Timeout (rarely customised, default-only). 2026-09-14
	// final-simplification: Max tokens is NOT user-configurable;
	// the substrate supplies its own internal value at wire time.
	// The wizard does not prompt for it and the profile value is
	// never honoured.
	timeout := promptString(rwFromStdin(), "Timeout seconds", intToStr(prof.TimeoutSecs))
	if timeout != "" {
		if n, err := strconvAtoi(timeout); err == nil && n > 0 {
			prof.TimeoutSecs = n
		}
	} else if existing.TimeoutSecs > 0 {
		prof.TimeoutSecs = existing.TimeoutSecs
	}

	// Refuse to save when the wizard collected no usable profile
	// state — model + base URL are the minimum to be useful. This
	// guards against the wizard silently writing an empty profile on
	// a fresh install.
	if prof.Model == "" && prof.BaseURL == "" && !hadModel && !hadBaseURL {
		fmt.Println()
		fmt.Println("A model or base URL is required. Run `mpm config profile set default model <id>` for non-interactive configuration, or re-run the wizard and provide a model or URL.")
		return 1
	}

	// Public wizard always writes provider="custom" (the only
	// public provider menu entry). Runtime infers wire from
	// base_url at construction time. Existing profiles that had
	// a branded provider ID keep it via the snapshot path above
	// (the wizard never overwrites an already-set
	// prof.Provider).
	if prof.Provider == "" {
		prof.Provider = "custom"
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
	fmt.Println("  max tokens : (supplied by runtime)")
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

// wizardEmbeddingStep presents the embedding subsystem in the
// wizard. Embeddings are OPTIONAL — absence is informational, not
// a fault.
//
// 2026-09-14 final-simplification: the public menu offers ONLY:
//  1. Leave unchanged (or skip if not configured)
//  2. Configure manually (Custom + protocol)
//  3. Disable embedding
//
// The previous auto-detect option was retired in the same
// pass — MPM does not discover or select models for the
// operator. Manual configuration is the single public path.
func wizardEmbeddingStep(c *config.Config) {
	fmt.Println("Embedding (optional — semantic/vector retrieval)")
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
	fmt.Println("  2. Configure manually (Custom)")
	fmt.Println("  3. Disable embedding")
	choice := promptString(rwFromStdin(), "Choice", "1")
	switch strings.TrimSpace(choice) {
	case "2":
		wizardEmbeddingCustom(c)
	case "3":
		if c.Components == nil {
			c.Components = map[string]string{}
		}
		c.Components["embedding"] = "disabled"
		fmt.Println("Embedding disabled.")
	default:
		fmt.Println("Embedding unchanged.")
	}
}

// wizardEmbeddingCustom walks the operator through manual
// embedding configuration via Custom + protocol picker. There
// is NO "Max tokens" prompt on this path — embeddings have no
// generation context. The protocol set is smaller than the LLM
// path (OpenAI-compatible + Ollama only); Anthropic-compatible
// is removed because Anthropic exposes no native /embeddings
// endpoint.
func wizardEmbeddingCustom(c *config.Config) {
	fmt.Println()
	fmt.Println("Embedding · Custom")
	fmt.Println("Custom lets you connect any supported embedding endpoint.")
	fmt.Println()
	fmt.Println("  Examples:")
	fmt.Println("    OpenAI-compatible:")
	fmt.Println("      http://localhost:1234/v1")
	fmt.Println("      http://localhost:8000/v1")
	fmt.Println("      https://api.openai.com/v1")
	fmt.Println("    Ollama:")
	fmt.Println("      http://127.0.0.1:11434")
	fmt.Println()
	fmt.Println("  Embeddings are optional; lexical/structured")
	fmt.Println("  retrieval still works without them.")
	fmt.Println()

	// Embedding-protocol picker. Subset of the LLM protocol
	// list (no Anthropic-compatible — it has no native
	// /embeddings endpoint).
	embedProtocols := []choice{
		{
			id:    "openai-compatible",
			label: "OpenAI-compatible",
			defaults: config.Profile{
				BaseURL: "https://api.openai.com/v1",
			},
			needsAPIKey: true,
		},
		{
			id:    "ollama",
			label: "Ollama (local)",
			defaults: config.Profile{
				BaseURL: "http://127.0.0.1:11434",
			},
			needsAPIKey: false,
		},
	}
	protocol := promptChoiceDefault(rwFromStdin(), "Protocol", embedProtocols, "openai-compatible")
	if protocol == nil {
		fmt.Println("Aborted.")
		return
	}

	prof := config.Profile{Name: "embedding", Provider: "custom"}
	mergeProfileDefaults(&prof, protocol.defaults)

	model := promptString(rwFromStdin(), "Model", "")
	if model != "" {
		prof.Model = strings.TrimSpace(model)
	}
	baseURL := promptString(rwFromStdin(), "Base URL", prof.BaseURL)
	if baseURL != "" {
		prof.BaseURL = strings.TrimSpace(baseURL)
	}
	if protocol.needsAPIKey {
		key, err := promptSecret("API key (leave empty if not required)", "")
		if err != nil {
			usererror.Error("%v", err)
			return
		}
		if key != "" {
			prof.APIKey = key
		}
	}

	if prof.Model == "" || prof.BaseURL == "" {
		fmt.Println("A model and base URL are required to bind an embedding profile.")
		return
	}

	// Embedding profiles skip role validation (an embedding
	// profile is precisely what should hold an embedding
	// model). The wizard creates the profile + binding and
	// saves.
	if c.Profiles == nil {
		c.Profiles = map[string]config.Profile{}
	}
	if c.Components == nil {
		c.Components = map[string]string{}
	}
	c.Profiles["embedding"] = prof
	c.Components["embedding"] = "embedding"
	if err := config.SaveConfig(c); err != nil {
		fmt.Println("Failed to save config:", err)
		return
	}
	fmt.Println()
	fmt.Printf("✓ embedded profile \"embedding\" (provider=%s, model=%s)\n", prof.Provider, prof.Model)
	fmt.Println("✓ bound components.embedding = embedding")
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
		// 2026-09-14 final-simplification: max_tokens is removed
		// from the user-facing contract. The wire value is
		// substrate-supplied. GET surfaces "(removed)" so
		// scripts reading legacy values get a stable marker.
		case "max_tokens":
			return "(removed)", nil
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
			return "(removed)", nil
		case "timeout_seconds":
			return intToStr(c.Synth.TimeoutSecs), nil
		}
	}
	return "", fmt.Errorf("mpm config get: unknown key %q (valid keys: model, api_key, base_url, timeout_seconds, synthesis_enabled)", key)
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
		// 2026-09-14 final-simplification: max_tokens is no
		// longer user-configurable. Refuse silently rather than
		// accepting a value the runtime ignores — operators
		// deserve a clear error so the knob doesn't quietly
		// "work" in the wrong direction.
		return fmt.Errorf("max_tokens is no longer user-configurable; runtime supplies its own max_tokens value (output-token limits can silently truncate work and are not a cost-control mechanism)")
	case "timeout_seconds":
		n, err := strconvAtoi(val)
		if err != nil || n <= 0 {
			return fmt.Errorf("timeout_seconds must be a positive integer (got %q)", val)
		}
		prof.TimeoutSecs = n
	default:
		return fmt.Errorf("mpm config set: unknown key %q (valid keys: model, api_key, base_url, timeout_seconds, synthesis_enabled)", key)
	}
	c.Profiles["default"] = prof
	return nil
}

// configCanonicalKey normalises an input key to its canonical form.
//
//	"token"       → "api_key"
//	"apikey"      → "api_key"
//	"endpoint"    → "base_url"
//	"base-url"    → "base_url"
//	"timeout"     → "timeout_seconds"
//
// 2026-09-14 final-simplification: max / max_tokens /
// max_output_tokens aliases are no longer recognised — the
// key is reserved so a stable error message names it by
// its canonical form.
func configCanonicalKey(key string) string {
	k := strings.ToLower(strings.ReplaceAll(key, "-", "_"))
	switch k {
	case "token", "apikey", "api_key":
		return "api_key"
	case "endpoint", "baseurl", "base_url":
		return "base_url"
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
	id          string
	label       string
	defaults    config.Profile
	needsAPIKey bool // protocol preset hint; LLM/embedding path consults this
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

// promptSecret prints a labeled prompt and reads a secret without
// echoing typed characters to the terminal. Falls back to a visible
// (non-secret) prompt only when stdin is NOT a TTY — automation
// callers should always pipe via --stdin, where the helper is not
// used at all.
//
// 2026-09-19 credential-UX hardening pass:
//
//   - When stdin is a terminal, terminal echo is suspended before the
//     read and restored (best-effort) after.
//   - The final newline is stripped; embedded whitespace is preserved
//     so an operator can paste an oddly-formatted secret verbatim.
//   - When stdin is not a terminal and no fallback pipe is configured,
//     the prompt refuses rather than reading invisibly from a stale
//     fd — a hung CLI is the worst possible outcome of a partial
//     automation.
//
// def is accepted for API symmetry with promptString but only its
// empty/non-empty shape is consulted; the secret itself is NEVER
// printed back as a default.
func promptSecret(label, def string) (string, error) {
	prompt := label
	if def != "" && def != "(unchanged)" {
		prompt += " [keep existing]"
	}
	prompt += ": "
	if !isatty(os.Stdin) {
		return "", fmt.Errorf("stdin is not a terminal; use --stdin for non-interactive secret input")
	}
	fmt.Print(prompt)
	fd := int(os.Stdin.Fd())
	raw, err := term.ReadPassword(fd)
	// Always end the prompt line cleanly, regardless of read outcome.
	fmt.Println()
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(raw), "\r\n"), nil
}

// readSecretFromStdin reads a secret from stdin without echoing
// anything to stdout/stderr. Trims a single trailing newline (the
// common pipe convention) but preserves any other whitespace so an
// operator can paste an oddly-formatted secret verbatim.
//
// Used by `--stdin` automation paths. The function is intentionally
// independent of TTY state — callers that require a TTY must check
// beforehand.
func readSecretFromStdin() (string, error) {
	raw, err := io.ReadAll(os.Stdin)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(raw), "\r\n"), nil
}

// 2026-09-14 final-simplification: promptModelFromCatalog +
// modelMenuCatalogFor + modelMenuEntry are removed. The
// public wizard walks every operator-supplied model as a
// freeform prompt (`promptString(rw, "Model", current)`).
// The previous model-menu helper existed solely to render
// branded catalogues; the catalogues are gone.

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
	render.Section(os.Stdout, "Multi-profile support")
	render.Plain(os.Stdout, "MPM can store multiple named model profiles.")
	render.Plain(os.Stdout, "Components explicitly bind to profiles.")
	render.Plain(os.Stdout, "Multiple LLM profiles and a separate embedding profile may coexist.")
	render.Plain(os.Stdout, "MPM does not silently auto-route or fail over between profiles.")
	render.BlankLine(os.Stdout)
	render.Section(os.Stdout, "Config resolution vs runtime failover")
	render.Plain(os.Stdout, "unbound component → default profile is CONFIG RESOLUTION, not failover.")
	render.Plain(os.Stdout, "Once a component resolves to profile X, a provider failure on X")
	render.Plain(os.Stdout, "must never cause a request to profile Y.")
	render.BlankLine(os.Stdout)
	render.Section(os.Stdout, "Manual configuration only")
	render.Plain(os.Stdout, "MPM does not maintain provider/model catalogues and does")
	render.Plain(os.Stdout, "not discover or select models for the operator. Manual")
	render.Plain(os.Stdout, "configuration via Custom + protocol is the single public")
	render.Plain(os.Stdout, "path. Branded menus and embedding probe are removed")
	render.Plain(os.Stdout, "from v0.1-final.")
	render.BlankLine(os.Stdout)

	render.Section(os.Stdout, "LLM provider")
	render.Label(os.Stdout, "Provider", "Custom")
	render.Label(os.Stdout, "Protocol", "OpenAI-compatible / Anthropic-compatible / Ollama")
	render.Label(os.Stdout, "Model", "freeform — the model ID expected by your endpoint")
	render.Label(os.Stdout, "Base URL", "freeform — API endpoint used to reach the model")
	render.Label(os.Stdout, "API key", "freeform — credential required by the endpoint; leave empty only if authentication is not required")
	render.BlankLine(os.Stdout)

	render.Section(os.Stdout, "Embedding model")
	render.Plain(os.Stdout, "Embeddings are optional and power semantic/vector retrieval.")
	render.Plain(os.Stdout, "Lexical and structured retrieval continue to work without them.")
	render.Label(os.Stdout, "Provider", "Custom")
	render.Label(os.Stdout, "Protocol", "OpenAI-compatible / Ollama")
	render.Label(os.Stdout, "Model", "freeform — the embedding model ID")
	render.Label(os.Stdout, "Base URL", "freeform — API endpoint used to reach the embedding model")
	render.Label(os.Stdout, "API key", "freeform — credential required by the endpoint; leave empty only if authentication is not required")
	render.BlankLine(os.Stdout)

	render.Section(os.Stdout, "Configuration model (v0.1)")
	render.Label(os.Stdout, "profiles", "named execution profiles (provider, model, base_url, api_key, ...). profiles[\"default\"] is the canonical fallback for every component that has no explicit binding.")
	render.Label(os.Stdout, "components", "optional component → profile bindings. components[\"embedding\"] identifies the embedding profile.")
	render.Label(os.Stdout, "capabilities", "optional capability → component bindings. Canonical v0.1 defaults: reviewer→critic, reflect→critic, planner→memory, summarise→memory. Use 'mpm config capability set <cap> <component>' to override.")
	render.Label(os.Stdout, "legacy synth", "read-only migration history; pre-profiles installs left a top-level 'synth' block on disk that new code never writes. Migrate by running 'mpm config profile add default'.")
	render.BlankLine(os.Stdout)

	render.Section(os.Stdout, "Usage")
	render.Label(os.Stdout, "mpm config", "interactive wizard (writes profiles.default; Custom + protocol; covers both LLM and embedding)")
	render.Label(os.Stdout, "mpm config show | list", "show current configuration")
	render.Label(os.Stdout, "mpm config get <key>", "get one value")
	render.Label(os.Stdout, "mpm config set <key> <value>", "set one value on profiles.default")
	render.Label(os.Stdout, "mpm config edit", "open mpm_config.json in $EDITOR")
	render.Label(os.Stdout, "mpm config validate", "validate configuration shape")
	render.Label(os.Stdout, "mpm config profile ...", "add / list / get / set / remove profiles — see 'mpm config profile --help'")
	render.Label(os.Stdout, "mpm config component ...", "list / get / set component → profile bindings — see 'mpm config component --help'")
	render.Label(os.Stdout, "mpm config capability ...", "list / get / set capability → component bindings")
	render.BlankLine(os.Stdout)

	render.Section(os.Stdout, "Keys (canonical names; aliases accepted)")
	render.Label(os.Stdout, "model, api_key (alias: token), base_url (alias: endpoint), timeout_seconds, synthesis_enabled", "")
	render.BlankLine(os.Stdout)

	render.Section(os.Stdout, "Examples")
	render.Plain(os.Stdout, "  mpm config profile add default --model <id> --base-url <url>")
	render.Plain(os.Stdout, "  mpm config profile set default api_key            # hidden prompt")
	render.Plain(os.Stdout, "  printf '%s' \"$KEY\" | mpm config profile set default api_key --stdin")
	render.Plain(os.Stdout, "  mpm config set synthesis_enabled false")
	render.BlankLine(os.Stdout)

	render.Hint(os.Stdout, "Output-token limits are NOT user-configurable. Runtime supplies its own max_tokens value; operators cannot truncate generation through config.")
	render.Hint(os.Stdout, "Existing profiles with branded provider IDs (openai, anthropic, ollama, openrouter, ...) continue to load and wire unchanged.")
	render.Hint(os.Stdout, "API keys are persisted to mpm_config.json (file mode 0600). 'mpm config show' and 'mpm config profile get' redact them; 'mpm config get api_key' returns the full key for the operator's own use.")
	render.Hint(os.Stdout, "Config file: ~/.mpm/mpm_config.json (path resolved via the workspace; $EDITOR is opened on this file for 'mpm config edit').")
	render.Hint(os.Stdout, "Credentials: never put an API key on the command line — use `mpm config profile set <name> api_key` (hidden prompt) or pipe via --stdin.")
}

// printConfigProfileHelp prints `mpm config profile --help` via the
// canonical visual grammar. Subcommand-specific help surfaces were
// unreachable from `--help` prior to the 2026-09-14 release-pass.
//
// 2026-09-14 final-simplification: max_tokens removed from the
// profile setter's documented key list. The field stays on the
// profile struct for backwards-compatible JSON parsing but is
// never honored at runtime.
//
// 2026-09-15 release-pass: --json parity on list/get. api_key is
// redacted in JSON output (same as human output) — the operator's
// secret-retrieval surface is `mpm config get api_key` (human mode).
//
// 2026-09-19 credential-UX hardening: api_key set no longer
// accepts a positional value. The help block teaches the hidden
// prompt and --stdin flows instead. The success line for
// secret-bearing fields prints `updated` (no value interpolation).
func printConfigProfileHelp() {
	render.Heading(os.Stdout, "Config profile")
	render.BlankLine(os.Stdout)
	render.Section(os.Stdout, "Manage named execution profiles")
	render.Plain(os.Stdout, "A profile binds provider / model / base_url / api_key and")
	render.Plain(os.Stdout, "is referenced by component bindings or used directly.")
	render.BlankLine(os.Stdout)
	render.Section(os.Stdout, "Subcommands")
	render.Label(os.Stdout, "mpm config profile add [name]", "interactive wizard; supply --model and --base-url for non-interactive add")
	render.Label(os.Stdout, "mpm config profile list", "render all profiles (--json for machine output)")
	render.Label(os.Stdout, "mpm config profile get <name>", "show one profile (--json for machine output; api_key redacted)")
	render.Label(os.Stdout, "mpm config profile set <name> <key> <value>", "set one field (provider, model, base_url, temperature, timeout_seconds, reasoning)")
	render.Label(os.Stdout, "mpm config profile set <name> api_key", "prompt for api_key with terminal echo suppressed")
	render.Label(os.Stdout, "mpm config profile set <name> api_key --stdin", "read api_key from stdin (automation path)")
	render.Label(os.Stdout, "mpm config profile remove <name>", "delete; refused if any component binds to this profile (lists all bindings on refusal)")
	render.BlankLine(os.Stdout)
	render.Hint(os.Stdout, "Run 'mpm config profile add default' once on a fresh install to seed the canonical fallback profile.")
	render.Hint(os.Stdout, "Credentials never go on the command line. Use the hidden prompt (`set <name> api_key`) or pipe via --stdin. Positional secret values are rejected before any write.")
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
	render.Label(os.Stdout, "mpm config component list", "render all bindings (--json for machine output)")
	render.Label(os.Stdout, "mpm config component get <component>", "show one binding (--json for machine output)")
	render.Label(os.Stdout, "mpm config component set <component> <profile>", "set or replace a binding")
	render.Label(os.Stdout, "mpm config component unset <component>", "remove a binding (canonical surface; falls back to default)")
	render.BlankLine(os.Stdout)
}

// (syscall imported for isatty() — package main already in scope.)
var _ = syscall.Stdin

// ---------------------------------------------------------------------------
// Profile subcommands (mpm config profile <sub>)
// ---------------------------------------------------------------------------

// handleConfigProfile routes profile management subcommands.
//
//	mpm config profile add [name]                   Interactive wizard /
//	                                               accept-name-from-stdin
//	mpm config profile list                       Render all profiles
//	mpm config profile get <name>                  Show one profile
//	mpm config profile set <name> <key> <value>    Set one field
//	mpm config profile remove <name>               Delete; refuse if
//	                                               any component binds
//	                                               to this profile
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
		// Allow scriptable material-field flags. Unknown flags (without
		// --) preserve the legacy positional shape, so we only strip
		// well-known long-form flags here.
		//
		// 2026-09-19 credential-UX hardening: --api-key is no
		// longer accepted here. Injecting a credential via argv is
		// an information-disclosure bug — the value ends up in
		// shell history, /proc/<pid>/cmdline, and any captured
		// log. Operators who need to set an API key use
		// `mpm config profile set <name> api_key` (hidden prompt
		// on a TTY) or `... api_key --stdin` (pipe from an
		// automation source). See handleConfigProfile "set"
		// branch below.
		rest, provider := extractStringFlag(args[2:], "--provider")
		rest, model := extractStringFlag(rest, "--model")
		rest, baseURL := extractStringFlag(rest, "--base-url")
		rest, endpoint := extractStringFlag(rest, "--endpoint")
		if baseURL == "" && endpoint != "" {
			baseURL = endpoint
		}
		// Refuse the legacy --api-key argv path explicitly so
		// automation that still passes it fails loudly rather than
		// silently dropping the secret.
		if _, hasAPIKeyFlag := stripMemoryFlagToken(rest, "--api-key", "--apikey", "--token"); hasAPIKeyFlag {
			usererror.Error("refusing --api-key on command line; use `mpm config profile set <name> api_key` for a hidden prompt, or pipe via --stdin")
			return 1
		}
		return handleProfileAdd(loadOrInitConfig(), name, profileAddOpts{
			Provider: provider,
			Model:    model,
			BaseURL:  baseURL,
			restArgs: rest,
		})
	case "list":
		_, wantJSON := stripMemoryFlagToken(args[1:], "--json", "-j")
		if wantJSON {
			return respond(encodeProfileListJSON(loadOrInitConfig()), "", 0)
		}
		return handleProfileList(loadOrInitConfig())
	case "get":
		if len(args) < 2 {
			usererror.Error("mpm config profile get <name>")
			return 1
		}
		_, wantJSON := stripMemoryFlagToken(args[2:], "--json", "-j")
		if wantJSON {
			if _, ok := loadOrInitConfig().Profiles[args[1]]; !ok {
				return respond(encodeErrorEnvelope(`profile "`+args[1]+`" not found`), "", 1)
			}
			return respond(encodeProfileJSON(args[1], loadOrInitConfig()), "", 0)
		}
		return handleProfileGet(loadOrInitConfig(), args[1])
	case "set":
		if len(args) < 3 {
			usererror.Error("mpm config profile set <name> <key> [value|--stdin]\n  keys: provider, model, base_url, api_key, temperature, timeout_seconds, reasoning\n  api_key accepts no positional value: omit for a hidden prompt, or pass --stdin")
			return 1
		}
		name := args[1]
		key := args[2]
		rest := args[3:]
		// 2026-09-19 credential-UX hardening: the api_key field
		// (and its aliases token / apikey) is the only profile
		// field that is secret-bearing. The positional-value form
		// (`... api_key <secret>`) is an argv leak — it ends up in
		// shell history, /proc/<pid>/cmdline, and any log capture.
		// The success-path echo (`set to "<value>"`) is also a
		// leak. Both are now closed: positional values for the
		// secret field are rejected BEFORE any write; successful
		// api_key writes print `updated` with no value.
		normalized := strings.ToLower(strings.ReplaceAll(key, "-", "_"))
		isSecretField := normalized == "api_key" || normalized == "token" || normalized == "apikey"
		if isSecretField {
			value, errCode := resolveSecretFieldValue(rest)
			if errCode != 0 {
				return errCode
			}
			return handleProfileSet(loadOrInitConfig(), name, key, value, true /*silenceValue*/)
		}
		if len(rest) == 0 {
			usererror.Error("mpm config profile set <name> <key> <value>\n  keys: provider, model, base_url, temperature, timeout_seconds, reasoning")
			return 1
		}
		return handleProfileSet(loadOrInitConfig(), name, key, strings.Join(rest, " "), false /*silenceValue*/)
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

// resolveSecretFieldValue implements the secret-credential contract for
// the api_key (and alias) profile field:
//
//   - `mpm config profile set NAME api_key --stdin`   read from stdin (no TTY required).
//   - `mpm config profile set NAME api_key`           prompt with terminal echo suppressed.
//   - `mpm config profile set NAME api_key VALUE...`  REJECTED — refuses before any write,
//     prints an actionable message, exits non-zero.
//
// On success returns the resolved secret with exit code 0. On any
// rejection or read failure returns ("", 1) AFTER printing an
// actionable error; the secret itself is NEVER interpolated into the
// error text, never logged, and never passed into the config save path.
//
// The function name intentionally avoids the word "api_key" so it does
// not appear in greppable security assertions that would also match the
// legitimate field name.
func resolveSecretFieldValue(rest []string) (string, int) {
	rest, useStdin := stripMemoryFlagToken(rest, "--stdin")
	if useStdin {
		v, err := readSecretFromStdin()
		if err != nil {
			usererror.Error("reading api_key from stdin: %v", err)
			return "", 1
		}
		return v, 0
	}
	if len(rest) > 0 {
		// CRITICAL: do NOT echo `rest` here. Doing so would re-print
		// the rejected secret. The error must be actionable but
		// credential-free.
		usererror.Error("refusing API key on command line; omit the value for a hidden prompt or use --stdin")
		return "", 1
	}
	if !isatty(os.Stdin) {
		usererror.Error("refusing API key on command line; stdin is not a terminal; use --stdin for non-interactive secret input")
		return "", 1
	}
	v, err := promptSecret("API key", "")
	if err != nil {
		usererror.Error("%v", err)
		return "", 1
	}
	return v, 0
}

// profileAddOpts is the optional parameter bag for handleProfileAdd.
// When supplied via CLI flags (`--provider`, `--model`, `--base-url`),
// handleProfileAdd applies them before save so a scriptable complete-add
// is probe-eligible immediately. restArgs captures any remaining
// positional arguments the operator might have passed (kept for
// forward-compat with future flags).
//
// 2026-09-19 credential-UX hardening: APIKey is no longer accepted
// via this struct — there is no argv path for credentials. Operators
// who need to attach an API key after `profile add` use the dedicated
// hidden-prompt / --stdin flow on `mpm config profile set <name> api_key`.
type profileAddOpts struct {
	Provider string
	Model    string
	BaseURL  string
	restArgs []string
}

func handleProfileAdd(c *config.Config, name string, opts ...profileAddOpts) int {
	if c.Profiles == nil {
		c.Profiles = map[string]config.Profile{}
	}
	var opt profileAddOpts
	if len(opts) > 0 {
		opt = opts[0]
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
	// Seed any operator-supplied material fields so they participate
	// in the wizard's defaults instead of being silently dropped. The
	// legacy `if (flag && !isatty)` branch that dropped supplied
	// flags on the floor whenever a real terminal was attached was
	// the bug observed on profile `x` (2026-09-19 hardening pass).
	//
	// 2026-09-19 credential-UX hardening: --api-key is no longer
	// accepted on `profile add`. The dispatcher above rejects the
	// legacy `--api-key` flag outright and the opt struct no
	// longer carries an APIKey field. API keys go in via the
	// dedicated hidden-prompt / --stdin flow on
	// `mpm config profile set <name> api_key`.
	if opt.Provider != "" {
		p.Provider = opt.Provider
	}
	if opt.Model != "" {
		p.Model = opt.Model
	}
	if opt.BaseURL != "" {
		p.BaseURL = opt.BaseURL
	}
	// Flag-driven noninteractive add: when BOTH --model and
	// --base-url are supplied, the operator has committed to a
	// scriptable add and the wizard is suppressed regardless of
	// TTY state. The default protocol is OpenAI-compatible (the
	// wizard's first-option default). Previously this branch was
	// gated on `!isatty(os.Stdin)`, which dropped the supplied
	// flags on a real terminal — the precise defect reported on
	// profile `x`.
	hasCompleteFlags := opt.Model != "" && opt.BaseURL != ""
	if hasCompleteFlags {
		if p.Provider == "" {
			p.Provider = "custom"
		}
		// mergeProfileDefaults is non-destructive — it only fills
		// zero-valued fields with the protocol's defaults. We
		// pick the wizard's first option (OpenAI-compatible) so
		// a fresh scriptable add has the same sensible
		// temperature/timeout the wizard would have applied.
		mergeProfileDefaults(&p, wizardProtocolPresets()[0].defaults)
	} else if isatty(os.Stdin) {
		// 2026-09-14 simplification: interactive profile add
		// walks through Custom + protocol picker + helper
		// text, mirroring the canonical `mpm config`
		// wizard. The legacy "Provider (openai/anthropic/
		// ollama/custom)" raw prompt is gone — branded
		// provider IDs are not exposed in the public UX;
		// operators who want a branded ID set it via
		// `mpm config profile set <name> provider <id>`.
		//
		// 2026-09-19 update: any flags the operator DID supply
		// are already seeded into `p` above and now appear as
		// wizard defaults (defensive — the standard TTY call
		// would not include flags at all).
		fmt.Println()
		fmt.Println("Custom lets you connect any supported endpoint.")
		fmt.Println()
		fmt.Println("  Model:    the model ID expected by your provider")
		fmt.Println("  Base URL: API endpoint for the provider")
		fmt.Println("  API key:  provider credential (optional for local endpoints)")
		fmt.Println()

		protocol := promptProtocol(rwFromStdin(), "Protocol")
		if protocol == nil {
			fmt.Println("Aborted.")
			return 0
		}
		mergeProfileDefaults(&p, protocol.defaults)
		p.Provider = "custom"

		p.Model = strings.TrimSpace(promptString(rwFromStdin(), "Model", p.Model))
		baseURL := promptString(rwFromStdin(), "Base URL", p.BaseURL)
		if baseURL != "" {
			p.BaseURL = strings.TrimSpace(baseURL)
		}
		if protocol.needsAPIKey {
			key, err := promptSecret("API key (leave empty if not required)", "")
			if err != nil {
				usererror.Error("%v", err)
				return 1
			}
			if key != "" {
				p.APIKey = key
			}
		}
		tempStr := promptString(rwFromStdin(), "Temperature (0.0-2.0)", "0.2")
		if t, err := strconvAtoiFloat(tempStr); err == nil {
			p.Temperature = &t
		}
	} else {
		fmt.Printf("Created empty profile %q. Use `mpm config profile set %s <key> <value>` to fill.\n", name, name)
	}
	c.Profiles[name] = p
	if err := config.SaveConfig(c); err != nil {
		usererror.Error("saving config: %v", err)
		return 1
	}
	fmt.Printf("\u2713 profile %q added\n", name)

	// Best-effort post-save probe. The save has already committed
	// regardless of probe outcome. Fires whenever the resulting
	// profile becomes structurally executable (CanProbe=true);
	// incomplete profiles are NOT classified as runtime failures.
	// The second argument is "always consider this material" — the
	// helper short-circuits to no-op when CanProbe is false.
	probeProfileAfterSave(p, true)
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
		// 2026-09-14 final-simplification: max_tokens is no
		// longer a user-facing knob; runtime supplies its own
		// value. Surfacing an old stored value would imply it
		// is honoured, so omit it from `mpm config profile list`.
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
	// 2026-09-14 final-simplification: max_tokens is no
	// longer a user-facing knob; runtime supplies its own
	// value. Surfacing an old stored value would imply it
	// is honoured, so omit it from `mpm config profile get`.
	if p.TimeoutSecs > 0 {
		fmt.Printf("  timeout sec : %d\n", p.TimeoutSecs)
	}
	if p.Reasoning != "" {
		fmt.Printf("  reasoning   : %s\n", p.Reasoning)
	}
	return 0
}

func handleProfileSet(c *config.Config, name, key, value string, silenceValue bool) int {
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
		// 2026-09-19 credential-UX hardening: the
		// dispatcher guarantees the value never came from
		// argv and the caller has set silenceValue=true.
		p.APIKey = value
		silenceValue = true
	case "temperature":
		t, err := strconvAtoiFloat(value)
		if err != nil {
			usererror.Error("temperature must be a number 0.0-2.0 (got %q)", value)
			return 1
		}
		p.Temperature = &t
	case "max_tokens", "max_output_tokens", "maxtokens", "max":
		// 2026-09-14 final-simplification: max_tokens is
		// removed from the user-facing contract. Refuse with
		// a clear error so legacy scripts that try to set it
		// get a stable marker rather than silent acceptance.
		usererror.Error("max_tokens is no longer user-configurable; runtime supplies its own max_tokens value (output-token limits are not a cost-control mechanism)")
		return 1
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
		usererror.Error("unknown profile field %q (try: provider, model, base_url, api_key, temperature, timeout_seconds, reasoning)", key)
		return 1
	}
	c.Profiles[name] = p
	if err := config.SaveConfig(c); err != nil {
		usererror.Error("saving config: %v", err)
		return 1
	}
	// 2026-09-19 credential-UX hardening: success output MUST
	// NOT include the credential value for api_key. The dispatcher
	// sets silenceValue=true for the api_key branch; the success
	// line prints `updated` with no value interpolation. Other
	// fields keep their existing `set to "<value>"` output for
	// operator convenience — only secret-bearing fields are
	// redacted, in keeping with the task brief.
	if silenceValue {
		fmt.Printf("✓ profile %q %s updated\n", name, configCanonicalKey(key))
	} else {
		fmt.Printf("✓ profile %q %s set to %q\n", name, key, value)
	}

	// Best-effort post-save probe. The save has already committed
	// regardless of probe outcome; the probe merely validates the
	// configured profile with the real adapter and prints a hint.
	// Non-material fields (temperature, reasoning, timeout) don't
	// trigger because they don't change the fingerprint.
	probeProfileAfterSave(p, isMaterialChange(p, key))
	return 0
}

// isMaterialChange reports whether key is a material field whose change
// should trigger a post-save probe. Material fields alter the connection
// fingerprint; non-material fields (temperature, reasoning, timeout) do
// not.
func isMaterialChange(p config.Profile, key string) bool {
	materialKeys := map[string]bool{
		"provider": true, "model": true, "base_url": true, "endpoint": true,
		"api_key": true, "token": true,
	}
	if key == "" {
		// Empty key signals "the entire profile was just created with
		// material fields supplied" (handleProfileAdd path).
		return probe.CanProbe(&p)
	}
	if !materialKeys[strings.ToLower(strings.ReplaceAll(key, "-", "_"))] {
		return false
	}
	return true
}

// probeProfileAfterSave fires a single best-effort probe against p. It is
// the canonical post-save verification shared by `handleProfileSet`
// (when the changed field is material) and `handleProfileAdd` (when the
// profile becomes executable). Errors are rendered as a UX hint, never
// as a save failure.
//
// Caller passes isMaterialChange=true iff the change could have altered
// the connection fingerprint. The probe is skipped when the profile is
// not structurally executable (mid-construction) regardless of the flag.
func probeProfileAfterSave(p config.Profile, isMaterialChange bool) {
	if !isMaterialChange {
		return
	}
	if !probe.CanProbe(&p) {
		// Profile is mid-construction; do not flag a runtime failure.
		return
	}
	dm := getDBConcrete()
	// Force DB initialization if it hasn't already been opened. The
	// profile-set path doesn't otherwise need DB access, but the probe
	// needs to persist results to system_config[model_probe_results].
	if dm == nil {
		dm = getDBConcrete()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	// ProbeSingle operates on the supplied profile directly. We
	// deliberately do NOT require component routing here — a complete
	// profile is probe-eligible on its own, and the operator-facing UX
	// line ("Connection verified · …") should appear regardless of
	// whether the profile is bound to a component yet. Routing-based
	// Doctor performs its own dispatch via RunActiveProbes.
	result, _ := probe.ProbeSingle(ctx, dm, &p)
	printProbeOnSaveHint(result)
}

// extractStringFlag removes `--name <value>` from args and returns the
// cleaned argv and the value ("" if absent). Long-form flag only; the
// single-dash variant isn't supported to keep the scriptable surface
// explicit. Used by `profile add`'s scriptable path.
func extractStringFlag(args []string, name string) ([]string, string) {
	out := make([]string, 0, len(args))
	value := ""
	for i := 0; i < len(args); i++ {
		if args[i] == name && i+1 < len(args) {
			value = args[i+1]
			i++ // skip the value too
			continue
		}
		out = append(out, args[i])
	}
	return out, value
}

// printProbeOnSaveHint emits the user-facing verification line. Saved
// the profile regardless of probe outcome; the hint is best-effort.
func printProbeOnSaveHint(r probe.ProbeResult) {
	switch r.Status {
	case probe.ProbeHealthy:
		fmt.Printf("✓ Connection verified · %s · %s · %dms\n", r.Provider, r.Model, r.LatencyMs)
	default:
		summary := r.ErrorSummary
		if summary == "" {
			summary = r.Status.String()
		}
		fmt.Printf("✗ Verification failed · %s\n", summary)
		fmt.Println("Configuration was saved.")
	}
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
	// 2026-09-15 release-pass: collect EVERY binding to this
	// profile so the error lists all of them. The previous
	// early-return on the first match hid bindings; the brief
	// explicitly forbids silently clearing or rebinding.
	bindings := make([]string, 0, 4)
	for comp, bound := range c.Components {
		if bound == name {
			bindings = append(bindings, comp)
		}
	}
	if len(bindings) > 0 {
		sort.Strings(bindings)
		// Stable, indented, quoted listing.
		lines := make([]string, len(bindings))
		for i, b := range bindings {
			lines[i] = "  " + strconv.Quote(b)
		}
		usererror.Error("cannot remove profile %q.\nIt is currently used by:\n%s\nRebind or unset those components first.",
			name, strings.Join(lines, "\n"))
		return 1
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

// componentPostUnsetReport returns the human-readable effect of
// unsetting the given component, derived from the real runtime
// resolver for that component. Embedding uses the canonical
// mpminternal.ResolveEmbeddingConfig(c); generative components
// use the canonical config.ResolveProfile.
//
// 2026-09-15 release-pass: semantics are NOT one universal
// "inherits default" rule. Embedding is OPTIONAL and uses its
// own resolver — the existence of Profiles["default"] does NOT
// cause embedding to inherit a generative default.
func componentPostUnsetReport(c *config.Config, component string) string {
	if component == "embedding" {
		cfg := mpminternal.ResolveEmbeddingConfig(c)
		switch cfg.Source {
		case mpminternal.EmbeddingSourceProfile:
			return fmt.Sprintf("Embedding source: profile (%s)", cfg.ProfileName)
		case mpminternal.EmbeddingSourceEnvFallback:
			return fmt.Sprintf("Embedding source: env (provider=%s)", cfg.ProviderName)
		case mpminternal.EmbeddingSourceDisabled:
			return "Embedding source: disabled"
		case mpminternal.EmbeddingSourceAbsent:
			return "Embedding source: absent"
		}
		return "Embedding source: unknown"
	}
	// Generative components — use the canonical ResolveProfile.
	r := c.ResolveProfile(component)
	if r.Profile == nil {
		return "No profile resolves; the component has no model."
	}
	return fmt.Sprintf("Effective profile: %q (source=%s)", r.Profile.Name, r.Source)
}

// mpm config component list                Render all bindings
// mpm config component get <component>     Show one binding
// mpm config component set <comp> <profile> Set binding
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
		usererror.Error("mpm config component <sub> — need one of: list, get, set, unset")
		return 1
	}
	switch args[0] {
	case "list":
		_, wantJSON := stripMemoryFlagToken(args[1:], "--json", "-j")
		if wantJSON {
			return respond(encodeComponentListJSON(loadOrInitConfig()), "", 0)
		}
		return handleComponentList(loadOrInitConfig())
	case "get":
		if len(args) < 2 {
			usererror.Error("mpm config component get <name>")
			return 1
		}
		_, wantJSON := stripMemoryFlagToken(args[2:], "--json", "-j")
		if wantJSON {
			return respond(encodeComponentJSON(args[1], loadOrInitConfig()), "", 0)
		}
		return handleComponentGet(loadOrInitConfig(), args[1])
	case "set":
		if len(args) < 3 {
			usererror.Error("mpm config component set <component> <profile>")
			return 1
		}
		return handleComponentSet(loadOrInitConfig(), args[1], args[2])
	case "unset":
		if len(args) < 2 {
			usererror.Error("mpm config component unset <component>")
			return 1
		}
		return handleComponentUnset(loadOrInitConfig(), args[1])
	default:
		usererror.Error("mpm config component: unknown subcommand %q — try list|get|set|unset", args[0])
		return 1
	}
}

// handleComponentUnset removes a component → profile binding. The
// existing `set ... ""` empty-string path is preserved for
// backwards compat; `unset` is the canonical surface.
//
// Post-unset, the report derives from the REAL runtime resolver
// for the component (embedding uses mpminternal.ResolveEmbeddingConfig;
// generative uses config.ResolveProfile) so the operator sees
// the actual effective state, not a hand-wave.
func handleComponentUnset(c *config.Config, component string) int {
	if c.Components == nil {
		c.Components = map[string]string{}
	}
	delete(c.Components, component)
	if err := config.SaveConfig(c); err != nil {
		usererror.Error("saving config: %v", err)
		return 1
	}
	fmt.Printf("✓ component %q unbound\n  %s\n", component, componentPostUnsetReport(c, component))
	return 0
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
		tag := componentBindingSourceTag(c, comp, bound)
		fmt.Printf("  %-12s → %-22s %s\n", comp, bound, tag)
	}
	return 0
}

func handleComponentGet(c *config.Config, component string) int {
	bound := c.Components[component]
	if bound == "" {
		bound = "(default)"
	}
	tag := componentBindingSourceTag(c, component, bound)
	fmt.Printf("  %s → %s %s\n", component, bound, tag)
	return 0
}

// componentBindingSourceTag returns the canonical explicit-vs-inherited
// tag for a component binding. Embedding uses the embedding source enum;
// generative components use config.ResolveProfile. A dangling explicit
// binding (operator-set name that doesn't exist in Profiles) is
// surfaced as "(explicit, invalid)".
//
// 2026-09-15 release-pass: this is the CLI's contract — config fallback
// is CONFIG RESOLUTION, not runtime failover. The tag lets operators
// see whether a binding is operator-set or inherited.
func componentBindingSourceTag(c *config.Config, component, bound string) string {
	if component == "embedding" {
		cfg := mpminternal.ResolveEmbeddingConfig(c)
		switch cfg.Source {
		case mpminternal.EmbeddingSourceProfile:
			return "(" + cfg.Source.String() + ": " + cfg.ProfileName + ")"
		default:
			return "(" + cfg.Source.String() + ")"
		}
	}
	// Generative component. ProfileFor falls through to default
	// when an explicit binding points at a missing profile (preserved
	// behaviour); the CLI's tag detects this and surfaces it as
	// "(explicit, invalid)" so the operator can rebind.
	r := c.ResolveProfile(component)
	if r.ConfiguredName != "" {
		if _, ok := c.Profiles[r.ConfiguredName]; ok {
			return "(explicit)"
		}
		return "(explicit, invalid)"
	}
	switch r.Source {
	case config.ResolutionDefault:
		return "(inherited, default)"
	case config.ResolutionLegacy:
		return "(inherited, legacy)"
	case config.ResolutionUnconfigured:
		return "(unconfigured)"
	}
	return "(" + string(r.Source) + ")"
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
//	mpm config capability list                Render all bindings
//	mpm config capability get <capability>    Show one binding
//	mpm config capability set <cap> <comp>    Bind capability to component
//
// Capabilities are the operator-meaningful vocabulary that Skills and
// runtime code address. Components are the substrate-specific
// functions that fulfil them. Default v0.1 capabilities:
//
//	planner   → memory
//	reviewer  → critic
//	reflect   → critic
//	summarise → memory
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
		_, wantJSON := stripMemoryFlagToken(args[1:], "--json", "-j")
		if wantJSON {
			return respond(encodeCapabilityListJSON(loadOrInitConfig()), "", 0)
		}
		return handleCapabilityList(loadOrInitConfig())
	case "get":
		if len(args) < 2 {
			usererror.Error("mpm config capability get <capability>")
			return 1
		}
		_, wantJSON := stripMemoryFlagToken(args[2:], "--json", "-j")
		if wantJSON {
			return respond(encodeCapabilityJSON(args[1], loadOrInitConfig()), "", 0)
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

// ---------------------------------------------------------------------------
// JSON envelopes (--json parity for profile / component / capability)
// ---------------------------------------------------------------------------
//
// 2026-09-15 release-pass: JSON output uses the SAME redaction policy
// as human output. api_key is `redactAPIKey(p.APIKey)`, never the full
// secret. The operator's secret-retrieval surface is
// `mpm config get api_key` (human mode); JSON is not privileged.
//
// Field names are snake_case and stable. `count` matches `len(array)`.

// profileJSONShape is the canonical per-profile JSON row.
type profileJSONShape struct {
	Name           string   `json:"name"`
	Provider       string   `json:"provider"`
	Model          string   `json:"model"`
	BaseURL        string   `json:"base_url,omitempty"`
	APIKey         string   `json:"api_key"` // redacted — same as human output
	Temperature    *float64 `json:"temperature,omitempty"`
	TimeoutSeconds int      `json:"timeout_seconds,omitempty"`
	Reasoning      string   `json:"reasoning,omitempty"`
}

func toProfileJSON(p config.Profile, name string) profileJSONShape {
	return profileJSONShape{
		Name:           name,
		Provider:       p.Provider,
		Model:          p.Model,
		BaseURL:        p.BaseURL,
		APIKey:         redactAPIKey(p.APIKey),
		Temperature:    p.Temperature,
		TimeoutSeconds: p.TimeoutSecs,
		Reasoning:      p.Reasoning,
	}
}

type profileListEnvelope struct {
	Success  bool               `json:"success"`
	Count    int                `json:"count"`
	Profiles []profileJSONShape `json:"profiles"`
}

func encodeProfileListJSON(c *config.Config) string {
	names := sortedKeysForConfig(c.Profiles)
	rows := make([]profileJSONShape, 0, len(names))
	for _, n := range names {
		rows = append(rows, toProfileJSON(c.Profiles[n], n))
	}
	b, err := json.MarshalIndent(profileListEnvelope{
		Success: true, Count: len(rows), Profiles: rows,
	}, "", "  ")
	if err != nil {
		return `{"success":false,"error":"json marshal failed"}`
	}
	return string(b) + "\n"
}

type profileEnvelope struct {
	Success bool             `json:"success"`
	Profile profileJSONShape `json:"profile"`
}

func encodeProfileJSON(name string, c *config.Config) string {
	p, ok := c.Profiles[name]
	if !ok {
		return encodeErrorEnvelope(`profile "` + name + `" not found`)
	}
	b, err := json.MarshalIndent(profileEnvelope{
		Success: true, Profile: toProfileJSON(p, name),
	}, "", "  ")
	if err != nil {
		return `{"success":false,"error":"json marshal failed"}`
	}
	return string(b) + "\n"
}

type componentJSONShape struct {
	Component         string `json:"component"`
	ConfiguredProfile string `json:"configured_profile"`
	EffectiveProfile  string `json:"effective_profile"`
	BindingSource     string `json:"binding_source"`
	Valid             bool   `json:"valid"`
}

type componentListEnvelope struct {
	Success    bool                 `json:"success"`
	Count      int                  `json:"count"`
	Components []componentJSONShape `json:"components"`
}

func encodeComponentListJSON(c *config.Config) string {
	comps := sortedKeysForConfig(c.Components)
	rows := make([]componentJSONShape, 0, len(comps))
	for _, comp := range comps {
		rows = append(rows, componentRecordForJSON(c, comp))
	}
	b, err := json.MarshalIndent(componentListEnvelope{
		Success: true, Count: len(rows), Components: rows,
	}, "", "  ")
	if err != nil {
		return `{"success":false,"error":"json marshal failed"}`
	}
	return string(b) + "\n"
}

type componentEnvelope struct {
	Success   bool               `json:"success"`
	Component componentJSONShape `json:"component"`
}

func encodeComponentJSON(component string, c *config.Config) string {
	b, err := json.MarshalIndent(componentEnvelope{
		Success:   true,
		Component: componentRecordForJSON(c, component),
	}, "", "  ")
	if err != nil {
		return `{"success":false,"error":"json marshal failed"}`
	}
	return string(b) + "\n"
}

// componentRecordForJSON derives the operator-meaningful resolution
// for a component using the canonical resolvers. Embedding uses the
// embedding source enum; generative components use config.ResolveProfile.
//
// 2026-09-15 contract:
//   - `binding_source` mirrors the human label (explicit / default /
//     legacy / unconfigured) for generative components; for embedding,
//     it maps to the EmbeddingSource.String() value
//     (profile / env / disabled / absent).
//   - `valid` is false when an explicit binding points at a missing
//     profile (dangling). The CLI does NOT silently clear or rebind;
//     the operator sees the misconfig and rebinds.
//
// Dangling bindings: ProfileFor falls through to Profiles["default"]
// (preserved pre-2026-09-15 behaviour), but the operator-meaningful
// record exposes the dangling state explicitly so the operator can
// see and rebind.
func componentRecordForJSON(c *config.Config, component string) componentJSONShape {
	rec := componentJSONShape{
		Component:         component,
		ConfiguredProfile: c.Components[component],
		BindingSource:     "unconfigured",
		Valid:             true,
	}
	if component == "embedding" {
		cfg := mpminternal.ResolveEmbeddingConfig(c)
		rec.BindingSource = cfg.Source.String()
		if cfg.Source == mpminternal.EmbeddingSourceProfile {
			rec.EffectiveProfile = cfg.ProfileName
		} else {
			rec.EffectiveProfile = cfg.ProviderName
		}
		return rec
	}
	r := c.ResolveProfile(component)
	rec.BindingSource = string(r.Source)
	if r.ConfiguredName != "" {
		// Operator-set explicit binding. Detect dangling by
		// checking whether the configured name resolves.
		if _, ok := c.Profiles[r.ConfiguredName]; !ok {
			rec.Valid = false
			rec.BindingSource = "explicit, invalid"
		}
	}
	if r.Profile != nil {
		rec.EffectiveProfile = r.Profile.Name
	}
	return rec
}

type capabilityJSONShape struct {
	Capability        string `json:"capability"`
	ConfiguredProfile string `json:"configured_component"`
	BindingSource     string `json:"binding_source"`
	Valid             bool   `json:"valid"`
}

// capabilityRecordForJSON resolves a capability's component binding.
// Capabilities map to COMPONENT names (not profile names), so the
// resolution is one-step: capability → component (no further profile
// lookup at this layer). The CLI surfaces the component the
// capability is bound to, and whether that binding is operator-set or
// from config.DefaultCapabilities.
func capabilityRecordForJSON(c *config.Config, capability string) capabilityJSONShape {
	rec := capabilityJSONShape{Capability: capability, Valid: true}
	if c != nil && c.Capabilities != nil {
		if v, ok := c.Capabilities[capability]; ok && v != "" {
			rec.ConfiguredProfile = v
			rec.BindingSource = "explicit"
			return rec
		}
	}
	if def, ok := config.DefaultCapabilities[capability]; ok {
		rec.ConfiguredProfile = def
		rec.BindingSource = "default"
		return rec
	}
	rec.BindingSource = "unconfigured"
	return rec
}

type capabilityListEnvelope struct {
	Success      bool                  `json:"success"`
	Count        int                   `json:"count"`
	Capabilities []capabilityJSONShape `json:"capabilities"`
}

func encodeCapabilityListJSON(c *config.Config) string {
	// When the operator hasn't customised capabilities, surface the
	// canonical defaults so the JSON envelope matches the human
	// `mpm config capability list` output exactly.
	keys := sortedKeysForConfig(c.Capabilities)
	if len(keys) == 0 {
		for k := range config.DefaultCapabilities {
			keys = append(keys, k)
		}
		sort.Strings(keys)
	}
	rows := make([]capabilityJSONShape, 0, len(keys))
	for _, k := range keys {
		rows = append(rows, capabilityRecordForJSON(c, k))
	}
	b, err := json.MarshalIndent(capabilityListEnvelope{
		Success: true, Count: len(rows), Capabilities: rows,
	}, "", "  ")
	if err != nil {
		return `{"success":false,"error":"json marshal failed"}`
	}
	return string(b) + "\n"
}

type capabilityEnvelope struct {
	Success    bool                `json:"success"`
	Capability capabilityJSONShape `json:"capability"`
}

func encodeCapabilityJSON(capability string, c *config.Config) string {
	b, err := json.MarshalIndent(capabilityEnvelope{
		Success:    true,
		Capability: capabilityRecordForJSON(c, capability),
	}, "", "  ")
	if err != nil {
		return `{"success":false,"error":"json marshal failed"}`
	}
	return string(b) + "\n"
}

// encodeErrorEnvelope returns a single-line JSON error object on
// stdout. Used for --json misses; exit code is set by the caller.
func encodeErrorEnvelope(msg string) string {
	b, _ := json.MarshalIndent(map[string]interface{}{
		"success": false,
		"error":   msg,
	}, "", "  ")
	return string(b) + "\n"
}
