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
					"description": "Execute an MPM CLI command. Example: 'recall hello' runs 'mpm recall hello'. Supported: add, ls, show, rm, shred, promote, reinforce, weaken, set-weight (memory ops); session add/search/show/shred/list; reference add/ls/show/search/shred; lesson add/list/search/get/shred/stats; persona list/active/set/clear; mode list/active/add/remove/clear; recall <query>; status; stats; compile mode|persona; start; stop; restart; doctor; version; prime-directives.",
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
