// agentdef.go — the worker's agent definition, embedded.
//
// # WHY EMBEDDED
//
// The adapter builds a DISPOSABLE OpenCode configuration for every run and
// writes the agent definition into it. If it read the definition from a
// path on disk, that path would be an input the operator could change
// without the adapter noticing — and the worker's containment would then
// depend on a file the adapter never reviewed.
//
// So the definition the sandbox receives is the one compiled into the
// binary, and a test pins the embedded copy byte-for-byte to the canonical
// template in agent_installation/, so the two cannot drift.
package opencode

import _ "embed"

// WorkerAgentDefinition is the canonical worker definition.
//
//go:embed mpm-worker.md
var WorkerAgentDefinition []byte

// WorkerAgentName is the definition's name, and the only name the worker's
// allow-list contains.
const WorkerAgentName = "mpm-worker"
