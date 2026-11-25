package opencode

import (
	"strings"
	"testing"
)

// TestRelayProbeTarget covers the address conversion the readiness probe
// depends on. The converted value is concatenated into a single-quoted shell
// word, so the test checks BOTH that the supported form is produced and that
// unsupported input is rejected rather than escaped.
func TestRelayProbeTarget(t *testing.T) {
	valid := []struct{ in, want string }{
		{"127.0.0.1:18777", "127.0.0.1/18777"},
		{"localhost:18777", "localhost/18777"},
		{"relay.internal:8080", "relay.internal/8080"},
		{"relay_1:1", "relay_1/1"},
		{"127.0.0.1:65535", "127.0.0.1/65535"},
		// A non-canonical numeric port is normalised, not passed through.
		{"127.0.0.1:018777", "127.0.0.1/18777"},
	}
	for _, c := range valid {
		got, err := relayProbeTarget(c.in)
		if err != nil {
			t.Errorf("relayProbeTarget(%q) errored: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("relayProbeTarget(%q) = %q, want %q", c.in, got, c.want)
		}
	}

	invalid := []string{
		"",
		"127.0.0.1",                 // no port
		":18777",                    // no host
		"127.0.0.1:",                // no port value
		"127.0.0.1:0",               // port 0 is not a listenable target
		"127.0.0.1:65536",           // out of range
		"127.0.0.1:-1",              // negative
		"127.0.0.1:abc",             // not numeric
		"[::1]:18777",               // IPv6 is not representable for bash /dev/tcp here
		"a/b:18777",                 // a '/' in the host would split the probe target
		"127.0.0.1:1/2",             // a '/' in the port
		"ho st:18777",               // whitespace
		"127.0.0.1:18777; rm -rf /", // shell metacharacters
		"127.0.0.1:18777`id`",
		"127.0.0.1:18777'",
		"127.0.0.1:18777\n",
		"$(id):18777",
	}
	for _, in := range invalid {
		if got, err := relayProbeTarget(in); err == nil {
			t.Errorf("relayProbeTarget(%q) was accepted as %q; it must be rejected", in, got)
		}
	}
}

// TestRelayProbeTargetCannotCarryShellSyntax is the injection property: no
// accepted value may contain a character that changes the shell word it is
// concatenated into.
func TestRelayProbeTargetCannotCarryShellSyntax(t *testing.T) {
	hostile := []string{
		"127.0.0.1:18777;id",
		"127.0.0.1:18777|id",
		"127.0.0.1:18777&&id",
		"127.0.0.1:18777`id`",
		"127.0.0.1:18777$(id)",
		"'127.0.0.1':18777",
		"127.0.0.1:18777' ; id ; '",
		"127.0.0.1:18777\nid",
	}
	for _, in := range hostile {
		got, err := relayProbeTarget(in)
		if err != nil {
			continue
		}
		if strings.ContainsAny(got, "'\"`$;|& \t\n\\(){}<>*?[]") {
			t.Errorf("relayProbeTarget(%q) returned shell-active characters: %q", in, got)
		}
	}
}
