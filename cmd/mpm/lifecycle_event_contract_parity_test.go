// lifecycle_event_contract_parity_test.go — the mpm-event/0.1 ingest
// contract, pinned ACROSS the two implementations.
//
// WHY THIS FILE EXISTS.
//
// The ingest protocol has two sides: the Go core (which stores an
// observation and returns a receipt) and the TypeScript adapter (which
// calls the core and interprets what comes back). They live in different
// languages, in different packages, and are merged by different people.
//
// A "we agreed on it in a comment" contract rots exactly the way the
// P2b hazard did: the two sides drift, and nothing notices until a
// caller receives a `malformed_receipt` in production.
//
// So the agreement is made CHECKABLE instead. This test reads the
// reference client's TRANSPORT SOURCE and asserts that the core's
// behaviour matches what that source actually reads. If Session C changes
// the transport — moves the receipt, renames a field, changes the action
// name, or adds a field the core does not provide — this test goes red.
//
// It is a source-parity test, not a runtime test, deliberately: it must
// run in the Go suite without a Node toolchain, and it must fail on a
// COSMETIC refactor too. A false failure is safe — it forces a human to
// look. A false PASS is not, so the assertions below are on the
// invariants, not on incidental formatting.
//
// THE THREE INVARIANTS
//
//  1. THE ACTION. The client calls `mpm_system` with
//     `action: "event_ingest"` and `params: { schema_version, event }`.
//     The core's registry must advertise that action and that action's
//     params, or the call fails before it reaches the handler.
//
//  2. THE RECEIPT IS AT THE TOP LEVEL. The client does
//     `const payload = res.payload;` and then `const receipt =
//     payload.receipt;`, where `res.payload` is the WHOLE parsed
//     `mpm call` stdout object. So the receipt must be a top-level
//     SIBLING of `success` in that object — not nested under a
//     `payload` key. This is also the substrate's own handler
//     convention, but the client's expectation is the binding one.
//
//  3. THE REJECTION IS NOT MISTAKEN FOR UNAVAILABILITY. The client maps
//     a non-empty `error` to `rejected`, EXCEPT when the message
//     matches /unknown action|unknown tool|unrecognized action/i, which
//     it maps to `unavailable` (the endpoint is absent in that build).
//     So every core-side rejection reason must NOT match that pattern —
//     otherwise a genuine rejection would be reported to the operator as
//     a transport outage, and the count would be wrong.

package main

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	mpminternal "github.com/flowbyte-com/mpm-core"
)

// lifecycleClientSource is the reference client's transport, relative to
// the repo root.
const lifecycleClientSource = "agent_installation/mpm-opencode/src/event-subscriber.ts"

// lifecycleClientSchemaSource is the reference client's envelope
// definition, relative to the repo root.
const lifecycleClientSchemaSource = "agent_installation/mpm-opencode/src/event-envelope.ts"

// readRepoFile reads a file relative to the repo root. The cmd/mpm
// package's tests run with cwd = cmd/mpm, so the root is two levels up.
func readRepoFile(t *testing.T, rel string) string {
	t.Helper()
	path := filepath.Join("..", "..", rel)
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(body)
}

// stripTSComments removes TypeScript line and block comments, leaving
// STRING LITERALS intact.
//
// It exists because a comment must not satisfy a code assertion. The
// client's transport carries a doc comment that spells out the very
// request shape the assertions below check — and a naive
// strings.Contains on the raw source would accept that comment as
// evidence that the CODE still sends the shape. That is exactly the
// kind of false pass this file exists to prevent.
//
// String literals are preserved because a `//` inside a string (a URL,
// a path) is not a comment. The scanner is deliberately small: it
// handles the three TypeScript string forms and nothing else, and it
// does not try to parse TypeScript.
func stripTSComments(src string) string {
	var out strings.Builder
	runes := []rune(src)
	for i := 0; i < len(runes); i++ {
		c := runes[i]
		switch {
		case c == '/' && i+1 < len(runes) && runes[i+1] == '/':
			// A line comment: skip to the end of the line, keeping the
			// newline so line structure survives.
			for i < len(runes) && runes[i] != '\n' {
				i++
			}
			if i < len(runes) {
				out.WriteRune('\n')
			}
		case c == '/' && i+1 < len(runes) && runes[i+1] == '*':
			// A block comment: skip to the closing marker.
			i += 2
			for i+1 < len(runes) && !(runes[i] == '*' && runes[i+1] == '/') {
				i++
			}
			i++ // land on the '/'
		case c == '\'' || c == '"' || c == '`':
			// A string literal: copy it verbatim, honouring backslash
			// escapes so an escaped quote does not end it early.
			quote := c
			out.WriteRune(c)
			for i++; i < len(runes); i++ {
				out.WriteRune(runes[i])
				if runes[i] == '\\' && i+1 < len(runes) {
					i++
					out.WriteRune(runes[i])
					continue
				}
				if runes[i] == quote {
					break
				}
			}
		default:
			out.WriteRune(c)
		}
	}
	return out.String()
}

// TestLifecycleEventContract_ClientReadsWhatTheCoreProvides pins the
// action and the params the client sends.
func TestLifecycleEventContract_ClientReadsWhatTheCoreProvides(t *testing.T) {
	// The CODE, with the comments stripped: a doc comment that spells
	// out the request shape must not satisfy the assertion.
	src := stripTSComments(readRepoFile(t, lifecycleClientSource))

	// The action name.
	if !strings.Contains(src, `action: "event_ingest"`) {
		t.Errorf("%s must call mpm_system with action \"event_ingest\"", lifecycleClientSource)
	}

	// The params envelope: both keys, and both from the request.
	for _, want := range []string{"schema_version:", "event: envelope"} {
		if !strings.Contains(src, want) {
			t.Errorf("%s must send %q in the event_ingest params", lifecycleClientSource, want)
		}
	}

	// The schema version the client sends must be the one the core
	// accepts. The client names it in a constant; the core names it in
	// Go. The parity check below compares them.
	schemaSrc := readRepoFile(t, lifecycleClientSchemaSource)
	m := regexp.MustCompile(`SCHEMA_VERSION\s*=\s*"([^"]+)"`).FindStringSubmatch(schemaSrc)
	if m == nil {
		t.Fatalf("%s must declare SCHEMA_VERSION", lifecycleClientSchemaSource)
	}
	if m[1] != lifecycleCoreSchemaVersion {
		t.Errorf("the client's schema version is %q, the core accepts %q — "+
			"the client would send a version the core rejects", m[1], lifecycleCoreSchemaVersion)
	}
}

// lifecycleCoreSchemaVersion is the version the core accepts. It is
// duplicated here deliberately: the test is about two independent
// implementations agreeing, so it must not import the constant from the
// side it is checking.
const lifecycleCoreSchemaVersion = "mpm-event/0.1"

// TestLifecycleEventContract_ReceiptIsReadFromTheTopLevel pins
// invariant 2.
//
// The client's read is `payload.receipt`, where `payload` is
// `res.payload` — the whole parsed `mpm call` object. If the client ever
// changes to read `payload.payload.receipt`, the receipt must move under
// a `payload` key, and the core must change with it.
func TestLifecycleEventContract_ReceiptIsReadFromTheTopLevel(t *testing.T) {
	src := stripTSComments(readRepoFile(t, lifecycleClientSource))

	if !strings.Contains(src, "const receipt = payload.receipt;") {
		t.Errorf("%s must read the receipt from the top level of the parsed "+
			"envelope (payload.receipt). If the receipt moved under a \"payload\" key, "+
			"the core must move it too — update this test and the core together.",
			lifecycleClientSource)
	}
	if strings.Contains(src, "payload.payload.receipt") {
		t.Errorf("%s reads the receipt from a NESTED payload — the core returns "+
			"the receipt as a top-level sibling of \"success\"", lifecycleClientSource)
	}

	// The receipt's fields the client validates. The core must provide
	// exactly these.
	for _, field := range []string{"event_id", "ingested_at_ms", "duplicate"} {
		if !strings.Contains(src, "r."+field) {
			t.Errorf("%s validates receipt.%s — the core must provide it", lifecycleClientSource, field)
		}
	}
}

// TestLifecycleEventContract_TheClientAsksForNothingMore checks the
// OTHER direction, and it is the direction a one-sided test misses.
//
// The assertions above ask "does the core provide what the client
// reads?". This one asks "does the client read anything the core does
// not provide?".
//
// The answer matters because the client treats a receipt it cannot
// validate as `malformed_receipt` — a silent drop from the caller's
// point of view. If C adds a required field to the receipt and the core
// does not provide it, every ingest would be reported as malformed, and
// nothing in the Go suite would notice unless something compared the
// two field sets.
//
// The core's field set is read from its own struct by REFLECTION, so it
// cannot drift from what the core actually returns.
func TestLifecycleEventContract_TheClientAsksForNothingMore(t *testing.T) {
	src := stripTSComments(readRepoFile(t, lifecycleClientSource))

	// Every `r.<field>` the client reads off the receipt.
	clientFields := map[string]bool{}
	for _, m := range regexp.MustCompile(`\br\.([A-Za-z_][A-Za-z0-9_]*)`).FindAllStringSubmatch(src, -1) {
		clientFields[m[1]] = true
	}
	if len(clientFields) == 0 {
		t.Fatalf("%s must read at least one field off the receipt — the guard "+
			"below would be vacuous", lifecycleClientSource)
	}

	// Every field the core's receipt actually carries, by reflection.
	rt := reflect.TypeOf(mpminternal.LifecycleEventReceipt{})
	coreFields := map[string]bool{}
	for i := 0; i < rt.NumField(); i++ {
		tag := rt.Field(i).Tag.Get("json")
		name := strings.Split(tag, ",")[0]
		if name == "" || name == "-" {
			continue
		}
		coreFields[name] = true
	}

	for field := range clientFields {
		if !coreFields[field] {
			t.Errorf("the client reads receipt.%s, but the core's receipt does not "+
				"carry it — the client would report every ingest as malformed_receipt",
				field)
		}
	}
	for field := range coreFields {
		if !clientFields[field] {
			t.Errorf("the core's receipt carries %q, but the client never reads it — "+
				"the field is dead weight on the wire", field)
		}
	}
}

// TestLifecycleEventContract_RejectionsAreNotMistakenForUnavailability
// pins invariant 3.
//
// The client maps a non-empty `error` to `rejected`, except when the
// message matches /unknown action|unknown tool|unrecognized action/i,
// which it maps to `unavailable`.
//
// Every core-side rejection reason must therefore NOT match that
// pattern. If one did, a genuine rejection — an envelope that violates
// the contract — would be reported to the operator as a transport
// outage, and the rejection counter would not move.
func TestLifecycleEventContract_RejectionsAreNotMistakenForUnavailability(t *testing.T) {
	src := stripTSComments(readRepoFile(t, lifecycleClientSource))

	// Lift the client's own pattern out of its source, so this test
	// tracks the client rather than a copy of it.
	m := regexp.MustCompile("/([^/`]*unknown[^/`]*)/i").FindStringSubmatch(src)
	if m == nil {
		t.Fatalf("%s must carry the pattern that maps an absent endpoint to \"unavailable\"",
			lifecycleClientSource)
	}
	unavailable := regexp.MustCompile(m[1])

	for _, reason := range lifecycleCoreRejectionReasons {
		if unavailable.MatchString(reason) {
			t.Errorf("the core's rejection reason %q matches the client's "+
				"\"unavailable\" pattern %q — the client would report a genuine "+
				"rejection as a transport outage", reason, m[1])
		}
	}
}

// TestLifecycleEventContract_CoreRejectionReasonsMatch keeps the copied
// list above in sync with the core's actual vocabulary.
//
// This is a MAINTENANCE check, not a parity check: the other tests in
// this file deliberately do not import the core's values, so that they
// compare two INDEPENDENT implementations. This one does import them —
// because the copy has to stay true, or the parity check would be
// checking a fiction.
func TestLifecycleEventContract_CoreRejectionReasonsMatch(t *testing.T) {
	core := mpminternal.LifecycleEventRejectionReasons
	if len(core) != len(lifecycleCoreRejectionReasons) {
		t.Fatalf("the core has %d rejection reasons, this test lists %d: %v vs %v",
			len(core), len(lifecycleCoreRejectionReasons), core, lifecycleCoreRejectionReasons)
	}
	for i, want := range lifecycleCoreRejectionReasons {
		if core[i] != want {
			t.Errorf("rejection reason %d: the core says %q, this test lists %q",
				i, core[i], want)
		}
	}
	if mpminternal.LifecycleEventSchemaVersion != lifecycleCoreSchemaVersion {
		t.Errorf("the core's schema version is %q, this test expects %q",
			mpminternal.LifecycleEventSchemaVersion, lifecycleCoreSchemaVersion)
	}
}

// lifecycleCoreRejectionReasons are the reason tokens the core can
// return, copied from internal/core/lifecycle_events.go.
//
// They are duplicated here for the same reason the schema version is:
// the test is about two independent implementations agreeing, so it must
// not import the values from the side it is checking. The core's own
// test pins that its vocabulary and this list agree — see
// TestLifecycleEventContract_CoreRejectionReasonsMatch.
var lifecycleCoreRejectionReasons = []string{
	"unknown_version",
	"unknown_event_type",
	"invalid_envelope",
	"limit_exceeded",
}
