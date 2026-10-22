// call_routed.go — the capabilities `mpm call` dispatches by name.
//
// The `mpm call` CLI dispatches registry-backed tools: it resolves the
// name through the Registry and calls the handler the Registry holds.
// A small number of capabilities are deliberately NOT registry entries,
// because registering them would change the native MCP surface — see
// mpm_help in help.go, which is registered as a native MCP closure
// rather than as a Registry entry.
//
// Those capabilities are still legitimate `mpm call` targets, so the
// CLI routes them by name. This file is the single definition of that
// routing:
//
//   - the CLI dispatcher reads it to decide what it can serve;
//   - the agent protocol guard reads it to check that a documented
//     invocation names a capability that exists and an action it
//     accepts.
//
// Two consequences are deliberate. A call-routed capability does NOT
// appear in tools.Names(), in the CLI's "unknown tool ... available:"
// list, or in `mpm help --all` — it is not a Registry entry. And a name
// that is neither call-routed nor a Registry entry is genuinely
// unknown: the CLI rejects it and the guard rejects it too.
//
// The routing table and the CLI's handler table must agree. A name
// listed here without a CLI handler would be advertised as reachable
// and then fail at call time, so cmd/mpm's regression test dispatches
// every name in this table and requires it to reach a handler.

package tools

// CallRoutedTool describes a capability that `mpm call` dispatches by
// name rather than through the registry.
type CallRoutedTool struct {
	// Name is the tool name an operator passes to `mpm call`.
	Name string
	// Actions are the action names the capability accepts.
	Actions []string
}

// CallRoutedHelp is the name of MPM's capability-discovery surface.
//
// It is registered as a native MCP closure at server init rather than
// as a Registry entry: a Registry entry would double-register under
// MPM_EXPOSE_ALL_TOOLS=1, where the full surface is exposed alongside
// the compact one, and would change tools.Names(), the CLI's
// "unknown tool ... available:" list and `mpm help --all`.
const CallRoutedHelp = "mpm_help"

// callRouted is the routing table. It is read-only after
// initialisation; CallRoutedTools hands out copies.
var callRouted = []CallRoutedTool{
	{Name: CallRoutedHelp, Actions: HelpActions()},
}

// CallRoutedTools returns the capabilities `mpm call` dispatches by
// name rather than through the registry.
//
// The returned slice and the action lists inside it are copies, so a
// caller cannot mutate the routing table.
func CallRoutedTools() []CallRoutedTool {
	out := make([]CallRoutedTool, 0, len(callRouted))
	for _, t := range callRouted {
		out = append(out, CallRoutedTool{
			Name:    t.Name,
			Actions: append([]string(nil), t.Actions...),
		})
	}
	return out
}

// IsCallRouted reports whether name is a capability `mpm call`
// dispatches by name rather than through the registry.
func IsCallRouted(name string) bool {
	_, ok := CallRoutedActions(name)
	return ok
}

// CallRoutedActions returns the actions a call-routed capability
// accepts.
//
// ok is false when name is not call-routed. That is not an error: the
// name may be a Registry tool (resolve it with ByName), and the CLI
// and the documentation guard both try the registry first.
func CallRoutedActions(name string) ([]string, bool) {
	for _, t := range callRouted {
		if t.Name == name {
			return append([]string(nil), t.Actions...), true
		}
	}
	return nil, false
}
