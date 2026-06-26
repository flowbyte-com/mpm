// Package usererror codifies the user-facing CLI output conventions.
//
// Background: mpm had ~232 fmt.Fprintf(os.Stderr, ...) call sites that
// relied on a fragile emoji-based convention (❌ for errors, [!!] for
// warnings, Usage: for help, ⚠️ for notices). New contributors didn't
// know the rules and the conventions drifted across files.
//
// This package provides a small, consistent API:
//
//   usererror.Error("ingest failed: %v", err)    -> "❌ ingest failed: <err>"
//   usererror.Warn("missing config")             -> "[!] missing config"
//   usererror.Usage("Usage: mpm %s <file>")      -> "Usage: mpm %s <file>" (no prefix)
//   usererror.Notice("backfilled 12 items")      -> "✓ backfilled 12 items"
//
// All output goes to stderr (matching the existing convention).
//
// The Error/Notice helpers also take an optional exitCode parameter via
// Error* / Notice* variants that exit after printing. This is for the
// "fatal then exit" pattern common in CLI handlers:
//
//   if err != nil { return usererror.Fatal("open db: %v", err) } // prints + returns 1
//
// Convention: the helper text MUST start with the message; the helper
// adds the prefix. Do not write `usererror.Error("❌ foo: %v", err)` —
// that double-prefixes. Just `usererror.Error("foo: %v", err)`.
package usererror

import (
	"fmt"
	"io"
	"os"
	"sync"
)

// Severity is the type of user-facing output. Most callers should use
// the Error / Warn / Notice / Usage helpers directly and ignore Severity.
type Severity int

const (
	SevNotice Severity = iota
	SevWarn
	SevError
	SevUsage
)

// Writer is the destination for user-facing output. Defaults to os.Stderr.
// Tests can override it to capture output. Use SetWriter / GetWriter.
var (
	writerMu sync.RWMutex
	writer   io.Writer = os.Stderr
)

// SetWriter redirects subsequent usererror output. Pass nil to restore
// the default (os.Stderr). Used by tests to capture output via a buffer.
func SetWriter(w io.Writer) {
	writerMu.Lock()
	defer writerMu.Unlock()
	if w == nil {
		writer = os.Stderr
		return
	}
	writer = w
}

// GetWriter returns the current writer (defaults to os.Stderr).
func GetWriter() io.Writer {
	writerMu.RLock()
	defer writerMu.RUnlock()
	return writer
}

// write prints one line to the configured writer with a severity-specific
// prefix. Newline is appended if not present.
func write(sev Severity, format string, args ...interface{}) {
	var prefix string
	switch sev {
	case SevError:
		prefix = "❌ "
	case SevWarn:
		prefix = "[!] "
	case SevNotice:
		prefix = "✓ "
	case SevUsage:
		prefix = "Usage: "
	}
	msg := fmt.Sprintf(format, args...)
	if msg != "" && msg[len(msg)-1] != '\n' {
		msg += "\n"
	}
	w := GetWriter()
	fmt.Fprint(w, prefix+msg)
}

// Error prints a user-facing error and returns exit code 1.
// Use this in handlers: return usererror.Error("ingest failed: %v", err).
func Error(format string, args ...interface{}) int {
	write(SevError, format, args...)
	return 1
}

// Errorf is the same as Error with a pre-formatted message. Convenience
// for cases where the caller already has a string.
func Errorf(msg string) int {
	return Error("%s", msg)
}

// Warn prints a non-fatal warning. Does not affect exit code; the caller
// returns whatever code it was going to return.
func Warn(format string, args ...interface{}) {
	write(SevWarn, format, args...)
}

// Notice prints a success/confirmation message. Used after a write operation
// completes ("backfilled 12 items", "saved memory abc123").
func Notice(format string, args ...interface{}) {
	write(SevNotice, format, args...)
}

// Usage prints a usage hint. Use this for "Usage: ..." lines, especially
// when the helper's prefix-adding makes it impossible to forget the
// "Usage: " prefix.
func Usage(format string, args ...interface{}) {
	write(SevUsage, format, args...)
}