package tools

import (
	"strings"
	"testing"
)

// TestNamespaceContract_AllRegistryToolsPrefixed enumerates every entry in
// the canonical tools.Registry and asserts each Name begins with "mpm_".
// This is the load-bearing regression guard for the 2026-09-22 namespace
// audit; any future addition that does not honour the prefix fails the
// build with a clear name.
func TestNamespaceContract_AllRegistryToolsPrefixed(t *testing.T) {
	if len(Registry) == 0 {
		t.Fatal("tools.Registry is empty — the namespace contract cannot be evaluated")
	}
	for _, tool := range Registry {
		if !strings.HasPrefix(tool.Name, "mpm_") {
			t.Errorf(
				"tools.Registry entry %q must be prefixed with mpm_; "+
					"every externally-callable MPM tool must occupy a "+
					"namespace under mpm_* to avoid collisions with sibling plugins",
				tool.Name,
			)
		}
	}
}

// TestNamespaceContract_NoBannedNames is the explicit regression guard for
// the OpenClaw memory-core collision and the historical standalones
// (log_to_changelog / request_review) that pre-dated the namespace
// convention. If any future code path reintroduces any of these names as
// a globally-registered tool name, the test fails with the offending
// name.
func TestNamespaceContract_NoBannedNames(t *testing.T) {
	banned := map[string]bool{
		"memory_search":    true,
		"memory_get":       true,
		"log_to_changelog": true,
		"request_review":   true,
		"search":           true,
		"get":              true,
		"status":           true,
	}
	for _, tool := range Registry {
		if banned[tool.Name] {
			t.Errorf(
				"tools.Registry must not register %q globally — "+
					"this name is either MPM-chosen-but-unsafe (collides with "+
					"sibling plugins like memory-core) or must remain an internal "+
					"SDK method name (e.g. runtime.search) rather than a global tool",
				tool.Name,
			)
		}
	}
}