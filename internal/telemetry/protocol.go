// internal/telemetry/protocol.go — NDJSON framing + ACCEPTED/DROPPED/REJECTED response shapes.
//
// The protocol is intentionally one-way by default (write-and-go). A
// response is sent only on the same connection, immediately after the
// server's parse-and-persist attempt completes. Per spec §5 the
// response is truthful — a SQLite write failure returns DROPPED, never
// ACCEPTED.

package telemetry

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
)

const (
	StatusAccepted = "ACCEPTED"
	StatusDropped  = "DROPPED"
	StatusRejected = "REJECTED"
)

type Response struct {
	Status   string `json:"status"`
	Inserted bool   `json:"inserted,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

// EncodeResponse writes r as a single NDJSON line (terminated with \n).
func EncodeResponse(w io.Writer, r Response) error {
	b, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("marshal response: %w", err)
	}
	if _, err := w.Write(b); err != nil {
		return err
	}
	if _, err := w.Write([]byte("\n")); err != nil {
		return err
	}
	return nil
}

// DecodeFrame reads one NDJSON line from r and returns the parsed Frame.
// Blank lines are skipped. Returns io.EOF when the reader is exhausted.
func DecodeFrame(r io.Reader) (Frame, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 1024*1024), 8*1024*1024) // 8MB upper bound per frame
	for scanner.Scan() {
		line := scanner.Bytes()
		trimmed := bytesTrim(line)
		if len(trimmed) == 0 {
			continue
		}
		f, err := ParseFrame(trimmed)
		if err != nil {
			return Frame{}, err
		}
		return f, nil
	}
	if err := scanner.Err(); err != nil {
		return Frame{}, fmt.Errorf("scan: %w", err)
	}
	return Frame{}, io.EOF
}

func bytesTrim(b []byte) []byte {
	start, end := 0, len(b)
	for start < end && (b[start] == ' ' || b[start] == '\t' || b[start] == '\r' || b[start] == '\n') {
		start++
	}
	for end > start && (b[end-1] == ' ' || b[end-1] == '\t' || b[end-1] == '\r' || b[end-1] == '\n') {
		end--
	}
	return b[start:end]
}
