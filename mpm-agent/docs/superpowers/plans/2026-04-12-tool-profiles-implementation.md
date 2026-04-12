# Tool Profiles Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a tool profile system to mini-bot Telegram. Standard profile provides: file tools (read_file, write_file, ReadFileSemantic, ReadFileCompare), web search (WebSynthesize), jq, and full MPM CLI access. AI agent can call tools. `/tools` command switches profiles via inline keyboard.

**Architecture:** MCP server exposes single `execute_mpm_command` tool (LLM passes raw command string). Mini-bot core/tools.go holds local/sandbox tools (file, web, jq) and MCP passthrough. Tool loop in RunAgent: call API with tools → execute tool → re-call until no more tool calls. Profile switching per-chat via chatSettings.

**Tech Stack:** Go 1.25, telego (Telegram), go-sqlite3, net/http (MCP client), encoding/json

---

## File Map

| File | Action |
|------|--------|
| `cmd/mcp/mcp.go` | DELETE (duplicate/broken) |
| `cmd/mcp/main.go` | MODIFY — fix build errors, implement MCP server with `execute_mpm_command` |
| `core/tools.go` | MODIFY — add ToolDefinition/registry, expand executeTool, add MPM exec wrappers |
| `core/config.go` | MODIFY — add Profiles to MiniBotConfig |
| `core/agent.go` | MODIFY — add tool loop to RunAgent |
| `cmd/telegram/handler.go` | MODIFY — add /tools command with inline keyboard |
| `cmd/telegram/session.go` | MODIFY — add toolProfile field to chatSettings |
| `mini-bot-config.json.example` | MODIFY — add profiles config |

---

## Task 1: Delete Duplicate MCP File

**Files:**
- Delete: `cmd/mcp/mcp.go`

- [ ] **Step 1: Delete cmd/mcp/mcp.go**

Run: `rm cmd/mcp/mcp.go`

- [ ] **Step 2: Verify build still fails on cmd/mcp/main.go undefined references only**

Run: `go build ./cmd/mcp 2>&1`
Expected: errors about `core.LoadConfig`, `core.OpenDB`, `core.ToolDefinitions`, `core.CallTool` (not about duplicate main)

---

## Task 2: Fix cmd/mcp/main.go Build Errors

**Files:**
- Modify: `cmd/mcp/main.go` (complete rewrite)

The current `cmd/mcp/main.go` calls:
- `core.LoadConfig()` → does not exist, replace with `core.LoadMiniBotConfig(core.GetConfigPath())`
- `core.OpenDB()` → does not exist, replace with `core.OpenDBForPath(core.ResolveMiniBotDBPath())`
- `core.ToolDefinitions()` → does not exist, implement in core/tools.go
- `core.CallTool()` → does not exist, implement in core/tools.go

The MCP server exposes a **single tool** to the LLM:
- `execute_mpm_command` — takes `{"command": "<raw mpm command string>"}`, execs `mpm <command>`, returns stdout

- [ ] **Step 1: Rewrite cmd/mcp/main.go with correct imports and JSON-RPC loop**

Replace the entire file content:

```go
// mini-bot-mcp is an MCP server that exposes MPM tools over stdio.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strings"
)

// MCP JSON-RPC types
type jsonRPCRequest struct {
	JSONRPC string                 `json:"jsonrpc"`
	ID     interface{}             `json:"id"`
	Method  string                 `json:"method"`
	Params  map[string]interface{} `json:"params,omitempty"`
}

type jsonRPCResponse struct {
	JSONRPC string      `json:"jsonrpc"`
	ID     interface{} `json:"id"`
	Result interface{} `json:"result,omitempty"`
	Error  *jsonError  `json:"error,omitempty"`
}

type jsonError struct {
	Code    int           `json:"code"`
	Message string        `json:"message"`
	Data    interface{}  `json:"data,omitempty"`
}

// MCP server capabilities
var serverCapabilities = map[string]interface{}{
	"tools": map[string]interface{}{},
}

func main() {
	log.SetFlags(0)
	log.SetOutput(os.Stderr)

	reader := bufio.NewReader(os.Stdin)
	for {
		line, err := reader.ReadString('\n')
		if err == io.EOF {
			break
		}
		if err != nil {
			log.Printf("read error: %v", err)
			break
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		var req jsonRPCRequest
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			continue
		}

		resp := handleRequest(req)
		respBytes, _ := json.Marshal(resp)
		fmt.Println(string(respBytes))
	}
}

func handleRequest(req jsonRPCRequest) jsonRPCResponse {
	resp := jsonRPCResponse{JSONRPC: "2.0", ID: req.ID}

	switch req.Method {
	case "initialize":
		resp.Result = map[string]interface{}{
			"protocolVersion": "2025-11-25",
			"capabilities":    serverCapabilities,
			"serverInfo": map[string]interface{}{
				"name":    "mini-bot-mcp",
				"version": "1.0.0",
			},
		}
	case "tools/list":
		// Single tool: execute_mpm_command
		resp.Result = map[string]interface{}{
			"tools": []map[string]interface{}{
				{
					"name":        "execute_mpm_command",
					"description": "Execute an MPM CLI command. Pass the full command string after 'mpm'. Example: 'recall what is my name' to run 'mpm recall what is my name'.",
					"inputSchema": map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"command": map[string]interface{}{
								"type":        "string",
								"description": "The MPM command arguments (e.g., 'recall hello' runs 'mpm recall hello')",
							},
						},
						"required": []string{"command"},
					},
				},
			},
		}
	case "tools/call":
		name, _ := req.Params["name"].(string)
		arguments, _ := req.Params["arguments"].(map[string]interface{})
		if arguments == nil {
			arguments = map[string]interface{}{}
		}

		if name == "execute_mpm_command" {
			cmdStr, _ := arguments["command"].(string)
			output, err := execMPM(cmdStr)
			if err != nil {
				resp.Result = map[string]interface{}{
					"content": []map[string]interface{}{
						{"type": "text", "text": fmt.Sprintf("error: %v\n%v", err, output)},
					},
					"isError": true,
				}
			} else {
				resp.Result = map[string]interface{}{
					"content": []map[string]interface{}{
						{"type": "text", "text": output},
					},
					"isError": false,
				}
			}
		} else {
			resp.Result = map[string]interface{}{
				"content": []map[string]interface{}{
					{"type": "text", "text": fmt.Sprintf("unknown tool: %s", name)},
				},
				"isError": true,
			}
		}
	default:
		resp.Error = &jsonError{Code: -32601, Message: fmt.Sprintf("method not found: %s", req.Method)}
	}

	return resp
}

// execMPM runs "mpm <cmd>" and returns stdout.
func execMPM(cmd string) (string, error) {
	args := strings.Fields(cmd)
	if len(args) == 0 {
		return "", fmt.Errorf("empty command")
	}
	cmdExec := exec.Command("mpm", args...)
	cmdExec.Env = append(os.Environ(), "MPM_WORKSPACE="+os.Getenv("MPM_WORKSPACE"))
	out, err := cmdExec.CombinedOutput()
	return string(out), err
}
```

- [ ] **Step 2: Verify cmd/mcp builds without errors**

Run: `go build ./cmd/mcp 2>&1`
Expected: no output (success)

- [ ] **Step 3: Commit**

```bash
git rm cmd/mcp/mcp.go
git add cmd/mcp/main.go
git commit -m "fix(mcp): clean up main.go, implement execute_mpm_command tool

- Remove duplicate mcp.go (two main() declarations)
- Rewrite main.go with correct stdio JSON-RPC loop
- Implement execute_mpm_command: execs 'mpm <args>' via shell
- Use LoadMiniBotConfig/OpenDBForPath (core funcs exist)"
```

---

## Task 3: Implement Tool Registry in core/tools.go

**Files:**
- Modify: `core/tools.go`

**New types:**
```go
type ToolDefinition struct {
    Name        string
    Description string
    InputSchema map[string]interface{}
}
```

**Tool registry (package-level map + RWMutex):**
```go
var toolRegistry = make(map[string]ToolDefinition)
var toolsMu sync.RWMutex

func RegisterTool(name string, def ToolDefinition)
func GetTool(name string) (ToolDefinition, bool)
func ListTools() []ToolDefinition
func ListToolsByProfile(profile []string) []ToolDefinition
```

**executeTool expansions** (add to existing switch):
```go
case "mpm_exec":
    // passthrough to MCP server
    // Returns raw output (MCP client handles this)
case "WebSynthesize":
    // existing function
case "ReadFileSemantic":
    // existing function
case "ReadFileCompare":
    // existing function
case "jq":
    // validate *.json/*.jsonl in mpm-agent/, exec jq
case "read_file":
    // existing (add path restriction)
case "write_file":
    // existing (add path restriction)
```

**Path restriction helper** (shared by file tools and jq):
```go
// restrictPath validates path is within mpm-agent tree.
// Returns resolved path or error if outside sandbox.
func restrictPath(path string) (string, error)
```

- [ ] **Step 1: Read current core/tools.go to understand existing code**

Run: `cat core/tools.go | head -130`

- [ ] **Step 2: Add imports and tool registry to core/tools.go**

Add `sync` to imports. Add after the linkPattern vars:

```go
// ToolDefinition describes a callable tool.
type ToolDefinition struct {
    Name        string
    Description string
    InputSchema map[string]interface{}
}

// toolRegistry stores all registered tools.
var toolRegistry = make(map[string]ToolDefinition)
var toolsMu sync.RWMutex

// RegisterTool adds a tool to the registry.
func RegisterTool(name string, def ToolDefinition) {
    toolsMu.Lock()
    defer toolsMu.Unlock()
    toolRegistry[name] = def
}

// GetTool returns a tool definition by name.
func GetTool(name string) (ToolDefinition, bool) {
    toolsMu.RLock()
    defer toolsMu.RUnlock()
    def, ok := toolRegistry[name]
    return def, ok
}

// ListTools returns all registered tools.
func ListTools() []ToolDefinition {
    toolsMu.RLock()
    defer toolsMu.RUnlock()
    result := make([]ToolDefinition, 0, len(toolRegistry))
    for _, def := range toolRegistry {
        result = append(result, def)
    }
    return result
}

// ListToolsByProfile returns tools matching the given profile (list of tool names).
func ListToolsByProfile(profile []string) []ToolDefinition {
    toolsMu.RLock()
    defer toolsMu.RUnlock()
    result := make([]ToolDefinition, 0, len(profile))
    for _, name := range profile {
        if def, ok := toolRegistry[name]; ok {
            result = append(result, def)
        }
    }
    return result
}

// restrictPath validates path is within mpm-agent directory tree.
// Returns resolved absolute path or error if outside sandbox.
func restrictPath(path string) (string, error) {
    // Resolve to absolute
    absPath, err := filepath.Abs(path)
    if err != nil {
        return "", fmt.Errorf("invalid path: %w", err)
    }
    // Get mpm-agent root (parent of core/, cmd/, etc.)
    root := filepath.Dir(filepath.Dir(os.Args[0]))
    if root == "." || root == "" {
        // Fallback: use working directory
        root, _ = os.Getwd()
    }
    rootAbs, _ := filepath.Abs(root)
    if !strings.HasPrefix(absPath, rootAbs) {
        return "", fmt.Errorf("path outside mpm-agent sandbox: %s", path)
    }
    return absPath, nil
}
```

- [ ] **Step 3: Expand executeTool switch to include all standard tools**

Replace the `executeTool` function with this complete version:

```go
// executeTool runs a single tool by name with args.
func executeTool(tool string, args map[string]interface{}) (string, error) {
    switch tool {
    case "read_file":
        path, _ := args["path"].(string)
        if path == "" {
            return "", fmt.Errorf("read_file: path is required")
        }
        restricted, err := restrictPath(path)
        if err != nil {
            return "", err
        }
        content, err := os.ReadFile(restricted)
        if err != nil {
            return "", err
        }
        return string(content), nil

    case "write_file":
        path, _ := args["path"].(string)
        content, _ := args["content"].(string)
        if path == "" {
            return "", fmt.Errorf("write_file: path is required")
        }
        restricted, err := restrictPath(path)
        if err != nil {
            return "", err
        }
        // Ensure parent dir exists
        if dir := filepath.Dir(restricted); dir != "" {
            os.MkdirAll(dir, 0755)
        }
        err = os.WriteFile(restricted, []byte(content), 0644)
        if err != nil {
            return "", err
        }
        return fmt.Sprintf("Written %d bytes to %s", len(content), restricted), nil

    case "ReadFileSemantic":
        path, _ := args["path"].(string)
        mode, _ := args["mode"].(string)
        if path == "" {
            return "", fmt.Errorf("ReadFileSemantic: path is required")
        }
        restricted, err := restrictPath(path)
        if err != nil {
            return "", err
        }
        return ReadFileSemantic(restricted, mode), nil

    case "ReadFileCompare":
        pathA, _ := args["pathA"].(string)
        pathB, _ := args["pathB"].(string)
        if pathA == "" || pathB == "" {
            return "", fmt.Errorf("ReadFileCompare: pathA and pathB are required")
        }
        resA, err := restrictPath(pathA)
        if err != nil {
            return "", err
        }
        resB, err := restrictPath(pathB)
        if err != nil {
            return "", err
        }
        return ReadFileCompare(resA, resB), nil

    case "WebSynthesize":
        query, _ := args["query"].(string)
        if query == "" {
            return "", fmt.Errorf("WebSynthesize: query is required")
        }
        result, err := WebSynthesize(query)
        if err != nil {
            return "", err
        }
        return result, nil

    case "jq":
        filter, _ := args["filter"].(string)
        file, _ := args["file"].(string)
        if filter == "" || file == "" {
            return "", fmt.Errorf("jq: filter and file are required")
        }
        restricted, err := restrictPath(file)
        if err != nil {
            return "", err
        }
        // Verify extension
        if !strings.HasSuffix(restricted, ".json") && !strings.HasSuffix(restricted, ".jsonl") {
            return "", fmt.Errorf("jq: only *.json and *.jsonl files allowed")
        }
        // Execute: jq <filter> <file>  (NO shell wrapping)
        out, err := exec.Command("jq", filter, restricted).CombinedOutput()
        if err != nil {
            return "", fmt.Errorf("jq error: %v\n%s", err, string(out))
        }
        return string(out), nil

    default:
        return "", fmt.Errorf("unknown tool: %s", tool)
    }
}
```

Add to imports: `"os/exec"` and `"path/filepath"`

- [ ] **Step 4: Add tool registration init() function**

Add at the end of core/tools.go:

```go
// init registers all standard tools on package load.
func init() {
    RegisterTool("read_file", ToolDefinition{
        Name:        "read_file",
        Description: "Read the full contents of a file within the mpm-agent directory.",
        InputSchema: map[string]interface{}{
            "type": "object",
            "properties": map[string]interface{}{
                "path": map[string]interface{}{
                    "type":        "string",
                    "description": "Relative or absolute path to the file",
                },
            },
            "required": []string{"path"},
        },
    })
    RegisterTool("write_file", ToolDefinition{
        Name:        "write_file",
        Description: "Write content to a file within the mpm-agent directory. Creates or overwrites.",
        InputSchema: map[string]interface{}{
            "type": "object",
            "properties": map[string]interface{}{
                "path": map[string]interface{}{
                    "type":        "string",
                    "description": "Relative or absolute path to the file",
                },
                "content": map[string]interface{}{
                    "type":        "string",
                    "description": "The content to write",
                },
            },
            "required": []string{"path", "content"},
        },
    })
    RegisterTool("ReadFileSemantic", ToolDefinition{
        Name:        "ReadFileSemantic",
        Description: "Read a file with semantic understanding. Modes: summary (structure overview), code (function/type lines), compare (diff two files).",
        InputSchema: map[string]interface{}{
            "type": "object",
            "properties": map[string]interface{}{
                "path": map[string]interface{}{
                    "type":        "string",
                    "description": "Path to the file",
                },
                "mode": map[string]interface{}{
                    "type":        "string",
                    "description": "Mode: summary, code, compare",
                },
            },
            "required": []string{"path"},
        },
    })
    RegisterTool("ReadFileCompare", ToolDefinition{
        Name:        "ReadFileCompare",
        Description: "Compare two files and show a diff-like output with line-level changes.",
        InputSchema: map[string]interface{}{
            "type": "object",
            "properties": map[string]interface{}{
                "pathA": map[string]interface{}{
                    "type":        "string",
                    "description": "Path to the first file",
                },
                "pathB": map[string]interface{}{
                    "type":        "string",
                    "description": "Path to the second file",
                },
            },
            "required": []string{"pathA", "pathB"},
        },
    })
    RegisterTool("WebSynthesize", ToolDefinition{
        Name:        "WebSynthesize",
        Description: "Search the web using DuckDuckGo and produce a synthesized answer with citations.",
        InputSchema: map[string]interface{}{
            "type": "object",
            "properties": map[string]interface{}{
                "query": map[string]interface{}{
                    "type":        "string",
                    "description": "The search query",
                },
            },
            "required": []string{"query"},
        },
    })
    RegisterTool("jq", ToolDefinition{
        Name:        "jq",
        Description: "Execute a jq filter on a JSON file. Only *.json and *.jsonl files within mpm-agent are allowed.",
        InputSchema: map[string]interface{}{
            "type": "object",
            "properties": map[string]interface{}{
                "filter": map[string]interface{}{
                    "type":        "string",
                    "description": "The jq filter expression (e.g., '.name' or '.[]|.id')",
                },
                "file": map[string]interface{}{
                    "type":        "string",
                    "description": "Path to the JSON file (*.json or *.jsonl)",
                },
            },
            "required": []string{"filter", "file"},
        },
    })
}
```

- [ ] **Step 5: Verify build**

Run: `go build ./core 2>&1`
Expected: no output (success)

- [ ] **Step 6: Commit**

```bash
git add core/tools.go
git commit -m "feat(tools): add ToolDefinition registry and all standard tools

- ToolDefinition struct with Name/Description/InputSchema
- Thread-safe tool registry with RegisterTool/GetTool/ListTools
- restrictPath() sandbox: all file ops scoped to mpm-agent/ tree
- executeTool expanded: read_file, write_file, ReadFileSemantic,
  ReadFileCompare, WebSynthesize, jq (no shell wrapping)
- init() registers all standard tools on package load"
```

---

## Task 4: Add Profiles to core/config.go

**Files:**
- Modify: `core/config.go`

Add `Profiles map[string][]string` to `MiniBotConfig`. Default standard profile includes all registered tools (they register themselves via init()).

- [ ] **Step 1: Read current config.go structure**

Run: `grep -n "type.*Config\|struct\|MiniBotConfig" core/config.go | head -20`

- [ ] **Step 2: Add ToolProfile type and Profiles to MiniBotConfig**

Add after `SelfImproveConfig` definition (around line 50):

```go
// ToolProfile defines a named collection of tools.
type ToolProfile struct {
    Name  string
    Tools []string // tool names
}
```

Add `Profiles map[string][]string` to `MiniBotConfig` struct.

- [ ] **Step 3: Add default profiles to DefaultMiniBotConfig()**

After the SelfImprove section, add:

```go
Profiles: map[string][]string{
    "standard": {
        "read_file", "write_file", "ReadFileSemantic", "ReadFileCompare",
        "WebSynthesize", "jq",
    },
},
```

- [ ] **Step 4: Verify build**

Run: `go build ./core 2>&1`
Expected: no output

- [ ] **Step 5: Commit**

```bash
git add core/config.go
git commit -m "feat(config): add Profiles to MiniBotConfig

- Add ToolProfile struct and Profiles map
- Default standard profile: read_file, write_file, ReadFileSemantic,
  ReadFileCompare, WebSynthesize, jq"
```

---

## Task 5: Add Tool Loop to RunAgent in core/agent.go

**Files:**
- Modify: `core/agent.go`

Add a tool loop that:
1. Calls the API with `tools` in the request
2. If the response contains a `tool_use` block, execute the tool
3. Re-call API with tool result appended as a `tool_result` content block
4. Repeat until no more tool calls (max 5 iterations)
5. Return final text response

The MCP tool (`execute_mpm_command`) is added to the tool list the AI can call. The tool loop executes it via `execMPM()` (reusing MCP server's exec logic, or we exec directly).

- [ ] **Step 1: Add tool loop types and execMPM function to core/agent.go**

Add after the `apiMessage` type definition (around line 170):

```go
// toolUse represents a tool call from the API.
type toolUse struct {
    Type      string `json:"type"`
    Name      string `json:"name"`
    Input     map[string]interface{} `json:"input"`
}

// toolResult represents a tool result block sent back to the API.
type toolResultBlock struct {
    Type      string `json:"type"`
    ToolUseID string `json:"tool_use_id"`
    Content   string `json:"content"`
}

// mpmExec runs an MPM command and returns stdout.
func mpmExec(cmd string) (string, error) {
    args := strings.Fields(cmd)
    if len(args) == 0 {
        return "", fmt.Errorf("empty command")
    }
    cmdExec := exec.Command("mpm", args...)
    cmdExec.Env = append(os.Environ(), "MPM_WORKSPACE="+os.Getenv("MPM_WORKSPACE"))
    out, err := cmdExec.CombinedOutput()
    return string(out), err
}
```

Add `"os/exec"` to imports.

- [ ] **Step 2: Update RunAgent signature and tool loop**

Replace `RunAgent` with:

```go
// RunAgent runs a single agent query with full identity and context.
// identityPath is the resolved path to IDENTITY.md.
// maxToolIterations limits tool call loops to prevent infinite loops.
func RunAgent(query string, history []map[string]interface{}, db *sql.DB, identityPath string, cfg *SynthConfig, toolProfile []string) (string, error) {
    // Get anchors as high-priority context
    anchors, _ := GetRecentAnchors(db, 5)

    // Get recent lessons for context
    lessons, _ := GetRecentLessons(db, 3)

    // Build context arrays
    memories := retrieveMemories(db, query, 5)
    directives := retrieveDirectives(db)
    references := retrieveReferences(db, query, 3)

    // Build system prompt with identity-first approach
    systemPrompt := BuildSystemPromptWithIdentity(identityPath, "", "",
        memories, directives, references, anchors)

    // Build messages array: history + current user message
    messages := make([]apiMessage, 0, len(history)+1)
    for _, h := range history {
        role, _ := h["role"].(string)
        content, _ := h["content"].(string)
        if role == "" {
            role = "user"
        }
        messages = append(messages, apiMessage{Role: role, Content: content})
    }
    messages = append(messages, apiMessage{Role: "user", Content: query})

    // Build tool list from active profile
    availableTools := buildToolList(toolProfile)

    // Tool loop: call API, execute tools, repeat
    maxIterations := 5
    for iteration := 0; iteration < maxIterations; iteration++ {
        responseText, stopReason, toolCalls, err := callSynthAPIWithTools(systemPrompt, messages, cfg, availableTools)
        if err != nil {
            return "", err
        }

        // If no tool calls, return the text response
        if len(toolCalls) == 0 {
            return responseText, nil
        }

        // Execute each tool call and append results to messages
        for _, tc := range toolCalls {
            var result string
            var err error
            switch tc.Name {
            case "execute_mpm_command":
                cmd, _ := tc.Input["command"].(string)
                result, err = mpmExec(cmd)
            default:
                // Local tools from core/tools.go
                result, err = executeTool(tc.Name, tc.Input)
            }
            if err != nil {
                result = fmt.Sprintf("error: %v", err)
            }
            messages = append(messages, apiMessage{
                Role:    "user",
                Content: fmt.Sprintf(`{"tool_use_id":"%s","content":%s}`,
                    tc.Input["id"], fmt.Sprintf("%q", result)),
            })
        }
    }

    // Max iterations reached
    return "(tool loop limit reached)", nil
}

// buildToolList returns MCP tool + local tools for the given profile.
func buildToolList(profile []string) []map[string]interface{} {
    tools := []map[string]interface{}{
        {
            "name":        "execute_mpm_command",
            "description": "Execute an MPM CLI command. Pass the full command string after 'mpm'. Example: 'recall hello' runs 'mpm recall hello'.",
            "inputSchema": map[string]interface{}{
                "type": "object",
                "properties": map[string]interface{}{
                    "command": map[string]interface{}{
                        "type":        "string",
                        "description": "The MPM command arguments (e.g., 'recall hello' runs 'mpm recall hello')",
                    },
                },
                "required": []string{"command"},
            },
        },
    }
    // Add local tools from registry for this profile
    for _, name := range profile {
        def, ok := GetTool(name)
        if !ok {
            continue
        }
        tools = append(tools, map[string]interface{}{
            "name":        def.Name,
            "description": def.Description,
            "inputSchema": def.InputSchema,
        })
    }
    return tools
}
```

- [ ] **Step 3: Update callSynthAPI to handle tool responses**

Replace `callSynthAPI` with a version that returns tool calls and stop reason. The Anthropic API returns `type: "tool_use"` blocks alongside `type: "text"` blocks. Parse both.

```go
// callSynthAPIWithTools makes an Anthropic API call and returns response text,
// stop reason, and any tool calls.
func callSynthAPIWithTools(systemPrompt string, messages []apiMessage, cfg *SynthConfig, tools []map[string]interface{}) (string, string, []toolUse, error) {
    if cfg.APIKey == "" {
        return "", "", nil, fmt.Errorf("no API key configured")
    }

    type anthropicRequest struct {
        Model     string               `json:"model"`
        MaxTokens int                  `json:"max_tokens"`
        System    string               `json:"system"`
        Messages  []apiMessage         `json:"messages"`
        Tools     []map[string]interface{} `json:"tools,omitempty"`
    }

    type anthropicResponse struct {
        Type      string `json:"type"`
        Content   []struct {
            Type string `json:"type"`
            Text string `json:"text"`
            Name string `json:"name"`
            ID   string `json:"id"`
            Input map[string]interface{} `json:"input"`
        } `json:"content"`
        StopReason string `json:"stop_reason"`
    }

    reqBody := anthropicRequest{
        Model:     cfg.Model,
        MaxTokens: cfg.MaxTokens,
        System:    systemPrompt,
        Messages:  messages,
        Tools:     tools,
    }

    body, err := json.Marshal(reqBody)
    if err != nil {
        return "", "", nil, fmt.Errorf("marshal request: %w", err)
    }

    url := strings.TrimSuffix(cfg.BaseURL, "/") + "/v1/messages"
    httpReq, err := http.NewRequest("POST", url, bytes.NewReader(body))
    if err != nil {
        return "", "", nil, fmt.Errorf("create request: %w", err)
    }
    httpReq.Header.Set("Content-Type", "application/json")
    httpReq.Header.Set("x-api-key", cfg.APIKey)
    httpReq.Header.Set("anthropic-version", "2023-06-01")

    timeout := time.Duration(cfg.TimeoutSecs) * time.Second
    if timeout == 0 {
        timeout = 300 * time.Second
    }
    client := &http.Client{Timeout: timeout}

    resp, err := client.Do(httpReq)
    if err != nil {
        return "", "", nil, fmt.Errorf("API call failed: %w", err)
    }
    defer resp.Body.Close()

    respBody, err := io.ReadAll(resp.Body)
    if err != nil {
        return "", "", nil, fmt.Errorf("read response: %w", err)
    }

    if resp.StatusCode != http.StatusOK {
        return "", "", nil, fmt.Errorf("API error %d: %s", resp.StatusCode, string(respBody))
    }

    var result anthropicResponse
    if err := json.Unmarshal(respBody, &result); err != nil {
        return "", "", nil, fmt.Errorf("parse response: %w", err)
    }

    // Collect text response
    var textResponse string
    var toolCalls []toolUse
    for _, block := range result.Content {
        if block.Type == "text" && block.Text != "" {
            textResponse = block.Text
        } else if block.Type == "tool_use" {
            toolCalls = append(toolCalls, toolUse{
                Type:  block.Type,
                Name:  block.Name,
                Input: block.Input,
            })
        }
    }

    return textResponse, result.StopReason, toolCalls, nil
}
```

Note: `callSynthAPI` becomes `callSynthAPIWithTools`. If callers need just text without tools, pass `nil` for tools.

- [ ] **Step 4: Add "net/http" to imports if not present**

Check: `grep '"net/http"' core/agent.go` — if missing, add to the import block.

- [ ] **Step 5: Verify build**

Run: `go build ./core 2>&1`
Expected: no output

- [ ] **Step 6: Commit**

```bash
git add core/agent.go
git commit -m "feat(agent): add tool loop to RunAgent

- Add toolUse/toolResult types for tool call handling
- Add mpmExec() to run MPM CLI commands
- RunAgent now takes toolProfile arg, calls API with tools,
  executes tool calls, re-calls until no more tools (max 5 iter)
- buildToolList merges execute_mpm_command with local tools
- callSynthAPIWithTools: parse tool_use blocks from response
- Update call sites in handler.go to pass toolProfile"
```

---

## Task 6: Wire ToolProfile into Handler and Add /tools Command

**Files:**
- Modify: `cmd/telegram/handler.go`
- Modify: `cmd/telegram/session.go` (add toolProfile field)

The handler needs to:
1. Store active `toolProfile` per chat in `chatSettings`
2. Pass `toolProfile` to `RunAgent`
3. Handle `/tools` command

- [ ] **Step 1: Add toolProfile field to chatSettings in session.go**

```go
type chatSettings struct {
    thinkLevel  int     // 0=off, 1=brief, 2=normal, 3=verbose
    verbose     bool    // if true, don't strip thinking blocks
    toolProfile string  // active tool profile name, default "standard"
}
```

- [ ] **Step 2: Update handler.go to pass toolProfile to RunAgent**

In `agentReply`, before calling `RunAgent`:

```go
profile := h.getSettings(chatID).toolProfile
if profile == "" {
    profile = "standard"
}
profileTools := h.agentConfig.Profiles[profile]
if profileTools == nil {
    profileTools = []string{} // fallback to empty
}
```

Pass `profileTools` as the new argument to `RunAgent`.

- [ ] **Step 3: Update RunAgent call signature in handler.go**

Change:
```go
responseText, err := core.RunAgent(userText, history, db, identityPath, &h.agentConfig.Synth)
```
To:
```go
responseText, err := core.RunAgent(userText, history, db, identityPath, &h.agentConfig.Synth, profileTools)
```

- [ ] **Step 4: Add /tools command to handleCommand in handler.go**

Add to the switch in `handleCommand`:

```go
case "/tools":
    if len(parts) == 1 {
        // Show profile selection inline keyboard
        profiles := make([]string, 0, len(h.agentConfig.Profiles))
        for name := range h.agentConfig.Profiles {
            profiles = append(profiles, name)
        }
        sort.Strings(profiles)
        return true, h.renderToolsMenu(profiles)
    }
    // /tools <name> — switch profile
    newProfile := parts[1]
    if _, ok := h.agentConfig.Profiles[newProfile]; !ok {
        return true, fmt.Sprintf("Unknown profile: %s. Available: %s", newProfile, strings.Join(profiles, ", "))
    }
    s := h.getSettings(chatID)
    s.toolProfile = newProfile
    return true, fmt.Sprintf("Tool profile: %s", newProfile)
```

Add `sort` to imports.

- [ ] **Step 5: Add renderToolsMenu helper to handler.go**

```go
// renderToolsMenu returns a text listing available profiles.
func (h *Handler) renderToolsMenu(profiles []string) string {
    var sb strings.Builder
    sb.WriteString("Tool Profiles:\n\n")
    current := h.getSettings(chatID).toolProfile // This won't work in a helper; inline into handleCommand instead
    // Note: render inline in handleCommand instead
    return sb.String()
}
```

Actually, implement it inline in `handleCommand` using `tu.InlineKeyboard()`:

```go
case "/tools":
    profiles := make([]string, 0, len(h.agentConfig.Profiles))
    for name := range h.agentConfig.Profiles {
        profiles = append(profiles, name)
    }
    sort.Strings(profiles)

    if len(parts) == 1 {
        // Show current profile and available options
        current := h.getSettings(chatID).toolProfile
        if current == "" {
            current = "standard"
        }
        var sb strings.Builder
        sb.WriteString(fmt.Sprintf("Current: %s\n\nAvailable profiles:\n", current))
        for _, name := range profiles {
            marker := ""
            if name == current {
                marker = " ✓"
            }
            sb.WriteString(fmt.Sprintf("  /tools %s%s\n", name, marker))
        }
        sb.WriteString("\nUse /tools <name> to switch.")
        return true, sb.String()
    }

    newProfile := parts[1]
    if _, ok := h.agentConfig.Profiles[newProfile]; !ok {
        return true, fmt.Sprintf("Unknown profile: %s", newProfile)
    }
    s := h.getSettings(chatID)
    s.toolProfile = newProfile
    return true, fmt.Sprintf("Tool profile: %s", newProfile)
```

Remove `renderToolsMenu` helper — it's inline now.

- [ ] **Step 6: Verify build**

Run: `go build ./cmd/telegram 2>&1`
Expected: no output

- [ ] **Step 7: Commit**

```bash
git add cmd/telegram/handler.go cmd/telegram/session.go
git commit -m "feat(telegram): add /tools command and tool profile switching

- Add toolProfile field to chatSettings (default: standard)
- Pass toolProfile to RunAgent via agentConfig.Profiles lookup
- /tools shows current profile and available profiles
- /tools <name> switches to named profile"
```

---

## Task 7: Update Example Config

**Files:**
- Modify: `mini-bot-config.json.example`

Add the profiles section:

```json
"profiles": {
  "standard": [
    "read_file", "write_file", "ReadFileSemantic", "ReadFileCompare",
    "WebSynthesize", "jq"
  ]
}
```

- [ ] **Step 1: Commit**

```bash
git add mini-bot-config.json.example
git commit -m "docs: add profiles to mini-bot-config.json.example"
```

---

## Spec Coverage Check

- [ ] Standard profile tools (read_file, write_file, ReadFileSemantic, ReadFileCompare, WebSynthesize, jq) — Task 3
- [ ] Tool registry (ToolDefinition, Register, List, Get) — Task 3
- [ ] Tool loop in RunAgent (tool call → execute → re-call) — Task 5
- [ ] execute_mpm_command (single MCP tool) — Task 2
- [ ] Path restriction to mpm-agent/ sandbox — Task 3
- [ ] jq no shell wrapping (exec.Command("jq", ...)) — Task 3
- [ ] Profile switching via /tools — Task 6
- [ ] Profiles stored per-chat in chatSettings — Task 6
- [ ] Profile config in MiniBotConfig — Task 4

## Pre-Implementation Verification

Before starting, run these to establish baseline:

```bash
go build ./cmd/mcp ./cmd/telegram ./cmd/mini-bot 2>&1
# Expected: cmd/mcp fails (undefined refs), others pass
```

After Task 1:
```bash
go build ./cmd/mcp 2>&1
# Should still fail (undefined refs) but NOT duplicate main
```

After Task 2:
```bash
go build ./cmd/mcp 2>&1
# Should pass
```

After all tasks:
```bash
go build ./cmd/mcp ./cmd/telegram ./cmd/mini-bot 2>&1
# Should all pass
go test ./core/... 2>&1
# Should pass (FTS5 tests may fail if SQLite compiled without FTS5 — pre-existing)
```
