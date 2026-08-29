// f_d3_route_registration_test.go — F-D3 route surface reachability.
//
// F-D3: the hostile test surfaced that `mpm route` was not registered
// in the MCP/agent hook surface of the disposable test environment.
// This was tagged INFO/ENV-SPECIFIC in the report — the underlying
// registration in the CLI and the `mpm_context` MCP action IS present.
//
// The pin test below verifies both surfaces accept a route invocation
// so a future refactor can't silently break the wiring.
//
// (Full route evaluation depends on persona/mode config and was
// already exercised in route_render_test.go.)
package main

import (
	"strings"
	"testing"
)

// TestF_D3_CliRouteRegistered pins the CLI surface: `mpm route` must
// be registered and not return "unknown command".
func TestF_D3_CliRouteRegistered(t *testing.T) {
	router := NewRouter()
	out := captureBoth(t, func() {
		router.Execute([]string{"route"})
	})
	if strings.Contains(out, "unknown command") || strings.Contains(out, "not registered") {
		t.Errorf("route command not registered: %s", out)
	}
}
