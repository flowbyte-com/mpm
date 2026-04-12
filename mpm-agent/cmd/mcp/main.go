// mpm-agent-mcp is an MCP server that exposes MPM tools to Claude Code.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"strings"

	"mpm-agent/core"
)

// MCP JSON-RPC types
type jsonRPCRequest struct {
	JSONRPC string                 `json:"jsonrpc"`
	ID     interface{}            `json:"id"`
	Method  string                 `json:"method"`
	Params  map[string]interface{} `json:"params,omitempty"`
}

type jsonRPCResponse struct {
	JSONRPC string      `json:"jsonrpc"`
	ID     interface{} `json:"id"`
	Result interface{} `json:"result,omitempty"`
	Error  *jsonError   `json:"error,omitempty"`
}

type jsonError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    interface{} `json:"data,omitempty"`
}

// Token authentication state
var (
	expectedToken string
	tokenChecked  bool
)

func main() {
	// Load config to check if token auth is required
	cfg, err := core.LoadConfig()
	if err == nil && cfg.APIToken != "" {
		expectedToken = cfg.APIToken
		log.Printf("[mcp] API token auth enabled")
	}

	db, err := core.OpenDB()
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	defer db.Close()

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
			continue // ignore malformed lines
		}

		var resp jsonRPCResponse
		resp.JSONRPC = "2.0"
		resp.ID = req.ID

		// Token validation on initialize
		if req.Method == "initialize" && expectedToken != "" && !tokenChecked {
			token := os.Getenv("MPM_API_TOKEN")
			if token != expectedToken {
				log.Printf("[mcp] invalid token from %v", req.ID)
				resp.Error = &jsonError{Code: -32000, Message: "unauthorized: invalid API token"}
				respBytes, _ := json.Marshal(resp)
				fmt.Println(string(respBytes))
				continue
			}
			tokenChecked = true
			log.Printf("[mcp] token validated for %v", req.ID)
		}

		switch req.Method {
		case "initialize":
			resp.Result = map[string]interface{}{
				"protocolVersion": "2025-11-25",
				"capabilities": map[string]interface{}{
					"tools": map[string]interface{}{},
				},
				"serverInfo": map[string]interface{}{
					"name":    "mpm",
					"version": "1.0.0",
				},
			}
		case "tools/list":
			tools := core.ToolDefinitions()
			// Convert mpm-agent tool format to MCP tool format
			mcpTools := make([]map[string]interface{}, 0, len(tools))
			for _, t := range tools {
				fn, ok := t["function"].(map[string]interface{})
				if !ok {
					continue
				}
				name, _ := fn["name"].(string)
				desc, _ := fn["description"].(string)
				params, _ := fn["parameters"].(map[string]interface{})
				mcpTools = append(mcpTools, map[string]interface{}{
					"name":        name,
					"description": desc,
					"inputSchema": params,
				})
			}
			resp.Result = map[string]interface{}{
				"tools": mcpTools,
			}
		case "tools/call":
			name, _ := req.Params["name"].(string)
			rawArgs, _ := req.Params["arguments"].(map[string]interface{})
			if rawArgs == nil {
				rawArgs = make(map[string]interface{})
			}
			// Core expects args as map[string]interface{}
			result, err := core.CallTool(name, rawArgs, db)
			if err != nil {
				resp.Result = map[string]interface{}{
					"content": []map[string]interface{}{
						{"type": "text", "text": fmt.Sprintf("error: %v", err)},
					},
					"isError": true,
				}
			} else {
				resp.Result = map[string]interface{}{
					"content": []map[string]interface{}{
						{"type": "text", "text": result},
					},
					"isError": false,
				}
			}
		default:
			resp.Error = &jsonError{Code: -32601, Message: fmt.Sprintf("method not found: %s", req.Method)}
		}

		respBytes, _ := json.Marshal(resp)
		fmt.Println(string(respBytes))
	}
}
