// cmd/mpm/encoders/json_encoder.go — Encoder layer for Wave 1.
//
// Encoders produce machine-readable output for `--json` flags and
// scripting pipelines. They are NOT renderers — renderers write to
// terminal output; encoders serialize.
//
// Layering contract (RFC §7):
//   Encoders sit alongside Formatters — they consume the same view
//   shapes but produce a different output format.
//
// This file implements only `JSONEncoder`. Future Wave 2+ work could
// add YAMLEncoder or similar if a non-JSON scripting consumer appears.

package main

import (
	"encoding/json"
	"fmt"
	"io"
)

// JSONEncoder is the production encoder for --json outputs. It writes
// to a writer so tests can capture output to a buffer instead of stdout.
//
// JSONEncoder is intentionally trivial — encoding/json does the work.
// The point of having this type at all is the layering: a Renderer and
// an Encoder have the same shape contract (consume a value, produce
// output) but target different output mediums. The triangle of
// Formatter → Renderer, Formatter → Encoder, Service → Formatter is
// what makes "presentation is not the model" defensible.
type JSONEncoder struct {
	out io.Writer
	// Indent controls json.NewEncoder().SetIndent. Empty string →
	// compact JSON for piping. Anything else → indented for humans.
	Indent string
}

// NewJSONEncoder returns a JSONEncoder writing to out. If out is nil,
// the encoder writes to a no-op io.Discard (so calls don't panic;
// callers should validate out before relying on output).
func NewJSONEncoder(out io.Writer, indent string) *JSONEncoder {
	if out == nil {
		out = io.Discard
	}
	return &JSONEncoder{out: out, Indent: indent}
}

// Encode writes value as JSON to the encoder's writer. Returns the
// number of bytes written (useful for tests) or an error.
func (e *JSONEncoder) Encode(value interface{}) (int, error) {
	enc := json.NewEncoder(e.out)
	if e.Indent != "" {
		enc.SetIndent("", e.Indent)
	}
	// Snapshot to a buffer so we can return byte count + write atomically.
	buf := &writeCounter{writer: e.out}
	enc.SetIndent("", e.Indent) // re-set after swapping writer
	enc2 := json.NewEncoder(buf)
	if e.Indent != "" {
		enc2.SetIndent("", e.Indent)
	}
	if err := enc2.Encode(value); err != nil {
		return 0, fmt.Errorf("json encode: %w", err)
	}
	return buf.n, nil
}

// writeCounter wraps an io.Writer to count bytes written through it.
type writeCounter struct {
	writer io.Writer
	n      int
}

func (c *writeCounter) Write(p []byte) (int, error) {
	n, err := c.writer.Write(p)
	c.n += n
	return n, err
}
