// hitl_log.go — the single append + retention path shared by every writer of
// the two human-in-the-loop JSONL files (`mirror.jsonl`, `watchdog.jsonl`).
//
// The HITL design (docs/archive/2026-10-03-hitl-log-redesign.md §12B) makes
// this file load-bearing rather than tidy-up: BEFORE it, rotation was
// fragmented — `ChallengeMemoryAsync` owned an inline rotate-and-append under
// the shared watchdog mutex, `appendToMirror`/`appendBlockedAttempt` never
// rotated at all, and the watchdog primitives rotated on every single write.
// Three behaviours, one filename pair. After it there is exactly one append
// path per stream, and every writer goes through it.
//
// What a stream owns (see hitlStream.Append):
//
//   - 0600 open/append;
//   - size-OR-age rotation, gzip, truncate (§9.2);
//   - MAX_AGE expiry and COUNT cap enforcement (§9.2);
//   - rotation perms 0600 and a collision-free rotated filename;
//   - the "a log failure never fails the write that produced it" contract —
//     callers decide whether to swallow, and no caller is escalated to fatal.
//
// The design's grep-gate (no v2 writer may open the JSONL path directly) is
// enforced by TestHITLStream_AllWritersGoThroughTheSharedPath, which parses
// every non-test Go file with go/ast and requires that any function naming
// "mirror.jsonl" or "watchdog.jsonl" is one of the allow-listed
// path-resolvers here or in the database manager constructor.
//
// Determinism seam: every policy decision takes an explicit `now`, and
// hitlStream carries its own clock (defaulting to time.Now). Retention tests
// therefore drive 30-day / 90-day / 365-day windows by passing a synthetic
// clock instead of sleeping.
package internal

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/flowbyte-com/mpm-core/config"
)

// ==================== Per-stream retention policy ====================
//
// Values are the design's table verbatim (§9.2). They are named constants
// rather than magic numbers so the derivation in the design stays checkable
// against the code, and so the acceptance tests can assert the shipped
// policy without restating it.
const (
	// mirrorRotateBytes is mirror's SIZE_THRESHOLD. 5 MiB, unchanged from
	// the pre-v2 default.
	mirrorRotateBytes int64 = 5 * 1024 * 1024
	// mirrorMaxActiveAge is MAX_ACTIVE_AGE: rotate the active file once it
	// has been open this long, so a low-volume stream still stays fresh.
	mirrorMaxActiveAge = 30 * 24 * time.Hour
	// mirrorMaxAge is the enforced expiry for mirror rotations. 365 days
	// is the cognitive-audit window; a memory's id, preview and digest are
	// still a record of what was known and when.
	mirrorMaxAge = 365 * 24 * time.Hour
	// mirrorRotationCap is the COUNT safety bound. Normally age binds first
	// (~12 monthly rotations ≈ 365 days); the cap only bites on bursts.
	mirrorRotationCap = 12

	// watchdogRotateBytes is watchdog's SIZE_THRESHOLD. 2 MiB, half the
	// mirror threshold: watchdog lines are higher cardinality and older
	// ones are worth less.
	watchdogRotateBytes int64 = 2 * 1024 * 1024
	// watchdogMaxActiveAge is MAX_ACTIVE_AGE for watchdog.
	watchdogMaxActiveAge = 30 * 24 * time.Hour
	// watchdogMaxAge is the enforced expiry for watchdog rotations.
	// 90 days aligns with the 30-day audit-table TTL while keeping a 3×
	// file window for tail forensics.
	watchdogMaxAge = 90 * 24 * time.Hour
	// watchdogRotationCap is the COUNT safety bound, ≈10 MB worst case.
	watchdogRotationCap = 5
)

// streamPolicy is one HITL stream's retention rules. Immutable after
// construction; the resolved instance lives on the hitlStream.
type streamPolicy struct {
	// Name is "mirror" or "watchdog". Used in log messages only.
	Name string
	// SizeThreshold rotates the active file at or above this size.
	SizeThreshold int64
	// MaxActiveAge rotates the active file once its mtime is this old.
	// Zero disables the age trigger.
	MaxActiveAge time.Duration
	// MaxAge expires (deletes) rotations older than this. Zero disables
	// expiry.
	MaxAge time.Duration
	// CountCap deletes the oldest rotations beyond this many survivors.
	// Zero disables the cap.
	CountCap int
}

// mirrorPolicy and watchdogPolicy are the shipped policies. They are values
// (not pointers) so a caller cannot mutate a live stream's rules.
var (
	mirrorPolicy = streamPolicy{
		Name:          "mirror",
		SizeThreshold: mirrorRotateBytes,
		MaxActiveAge:  mirrorMaxActiveAge,
		MaxAge:        mirrorMaxAge,
		CountCap:      mirrorRotationCap,
	}
	watchdogPolicy = streamPolicy{
		Name:          "watchdog",
		SizeThreshold: watchdogRotateBytes,
		MaxActiveAge:  watchdogMaxActiveAge,
		MaxAge:        watchdogMaxAge,
		CountCap:      watchdogRotationCap,
	}
)

// PolicyForLogName maps a CLI-facing log name to its policy. Unknown names
// return ok=false rather than a zero policy, so a typo cannot silently
// disable retention.
func PolicyForLogName(name string) (streamPolicy, bool) {
	switch name {
	case "mirror":
		return mirrorPolicy, true
	case "watchdog":
		return watchdogPolicy, true
	default:
		return streamPolicy{}, false
	}
}

// logRotateThresholdOverride reports the operator's MPM_LOG_ROTATE_BYTES
// override, if any. The override applies to BOTH streams' SIZE_THRESHOLD —
// it is a single global knob that predates per-stream thresholds — but NOT
// to the age trigger, expiry, or cap, which the design pins.
//
// Returning the override separately from logRotateThresholdBytes matters:
// that function collapses "unset" and "explicitly set to 5 MiB" onto the
// same 5 MiB default, which is exactly mirror's policy threshold. Reading
// the override directly keeps watchdog on its own 2 MiB policy.
func logRotateThresholdOverride() (int64, bool) {
	raw := strings.TrimSpace(os.Getenv("MPM_LOG_ROTATE_BYTES"))
	if raw == "" {
		return 0, false
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

// resolvedPolicy returns pol with the operator's size override applied.
// Called once per stream registry entry, so the env is read at first use
// rather than on every append.
func resolvedPolicy(pol streamPolicy) streamPolicy {
	if override, ok := logRotateThresholdOverride(); ok {
		pol.SizeThreshold = override
	}
	return pol
}

// ==================== The shared stream ====================

// hitlStream is the single append path for one HITL JSONL file. Writers call
// Append; nothing else opens the file.
type hitlStream struct {
	path  string
	pol   streamPolicy
	mu    sync.Mutex
	clock func() time.Time
}

var (
	hitlStreamsMu sync.Mutex
	// hitlStreams is keyed by absolute path. Two writers that resolve the
	// same file — a DatabaseManager and a MemoryStore handed the same
	// mirrorPath, say — must serialise against each other, so the mutex is
	// owned by the path rather than by the caller.
	hitlStreams = map[string]*hitlStream{}
)

// hitlStreamFor returns the process-wide stream for path, creating it on
// first use. An empty path returns nil, which every method treats as a
// silent no-op: a manager wrapping an in-memory database owns no log
// directory, and that is not an error.
func hitlStreamFor(path string, pol streamPolicy) *hitlStream {
	if path == "" {
		return nil
	}
	hitlStreamsMu.Lock()
	defer hitlStreamsMu.Unlock()
	if s, ok := hitlStreams[path]; ok {
		return s
	}
	s := &hitlStream{
		path:  path,
		pol:   resolvedPolicy(pol),
		clock: time.Now,
	}
	hitlStreams[path] = s
	return s
}

// now returns the stream's current time, tolerating a stream built without a
// clock (tests construct hitlStream values directly).
func (s *hitlStream) now() time.Time {
	if s.clock == nil {
		return time.Now()
	}
	return s.clock()
}

// Append writes one JSONL record: rotate-if-due, enforce retention, append
// the line, done — all under the stream mutex so concurrent writers cannot
// interleave a rotation with an append.
//
// The returned error is for the CALLER to decide what to do with. The
// existing contract is that a log failure never fails the write that
// produced it, and no v2 caller is escalated to fatal; the error exists so
// `mpm ops logs` and the tests can report a genuinely broken log rather than
// discovering it months later as a mystery.
func (s *hitlStream) Append(line []byte) error {
	if s == nil || s.path == "" {
		return nil
	}
	record := bytes.TrimRight(line, "\n")
	if len(bytes.TrimSpace(record)) == 0 {
		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	if _, err := rotateHITLIfNeeded(s.path, s.pol, now); err != nil {
		slog.Warn("hitl log rotation failed", "stream", s.pol.Name, "err", err)
	}
	if err := enforceHITLRetention(s.path, s.pol, now); err != nil {
		slog.Warn("hitl log retention failed", "stream", s.pol.Name, "err", err)
	}

	buf := make([]byte, 0, len(record)+1)
	buf = append(buf, record...)
	buf = append(buf, '\n')

	f, err := os.OpenFile(s.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open %s log for append: %w", s.pol.Name, err)
	}
	if _, err := f.Write(buf); err != nil {
		f.Close()
		return fmt.Errorf("append to %s log: %w", s.pol.Name, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close %s log: %w", s.pol.Name, err)
	}
	return nil
}

// ==================== Rotation ====================

// rotateHITLIfNeeded rotates path when EITHER trigger fires: size at or
// above the policy threshold, OR the file's mtime at or beyond
// MaxActiveAge. Returns whether a rotation actually happened.
//
// Size-only rotation is wrong once the streams stop recording every SQL
// statement: the daily volume falls by two orders of magnitude and the
// active file would otherwise stay open for a year, growing unbounded and
// useless to `tail`. The age trigger is what keeps it a journal.
//
// A zero-length file never rotates — there is nothing to archive, and
// emitting an empty .gz per append would be pure noise.
func rotateHITLIfNeeded(path string, pol streamPolicy, now time.Time) (bool, error) {
	if path == "" {
		return false, nil
	}
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("stat %s log for rotation: %w", pol.Name, err)
	}
	if info.Size() == 0 {
		return false, nil
	}
	sizeDue := pol.SizeThreshold > 0 && info.Size() >= pol.SizeThreshold
	ageDue := pol.MaxActiveAge > 0 && now.Sub(info.ModTime()) >= pol.MaxActiveAge
	if !sizeDue && !ageDue {
		return false, nil
	}
	if err := rotateLogAt(path, now); err != nil {
		return false, err
	}
	return true, nil
}

// rotateLogIfNeeded is the pre-v2 size-only primitive, kept because
// `mpm ops logs rotate --threshold N` needs "rotate only if at least N
// bytes" as an operator-facing override that the age trigger must not
// override. The HITL streams use rotateHITLIfNeeded instead.
func rotateLogIfNeeded(path string, thresholdBytes int64) error {
	if path == "" {
		return nil
	}
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("stat log for rotation: %w", err)
	}
	if info.Size() < thresholdBytes {
		return nil
	}
	return rotateLogAt(path, time.Now())
}

// rotateLogAt gzips path to a timestamped sibling and truncates the
// original in place, so any open O_APPEND handle keeps writing at offset 0.
//
// Two behaviours are deliberate and were wrong before:
//
//   - The rotated file is created 0600. os.Create uses 0666&^umask, which
//     is 0644 on a default umask — the design requires rotations to be as
//     private as the active file, and config/fileperms.go already lists
//     `mirror.jsonl*` as 0600 material.
//   - The rotated name is de-duplicated. Two rotations inside the same
//     second previously produced the same filename, and the second
//     os.Create silently truncated the first — destroying the archive it
//     had just written. With an age trigger, a process that writes once
//     per second and hits a 30-day boundary can rotate repeatedly, so this
//     is reachable, not theoretical.
//
// Atomicity: the read-then-truncate is not atomic across processes. That is
// accepted — the ownership contract is one process owns one workspace and
// its logs (SPEC §Runtime state ownership). Cross-process mutual exclusion
// for these files is a later hardening item, not this one.
func rotateLogAt(path string, now time.Time) error {
	timestamp := now.UTC().Format("20060102-150405")
	rotated := uniqueRotationPath(path, timestamp)

	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read log for rotation: %w", err)
	}
	gz, err := os.OpenFile(rotated, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create rotated log: %w", err)
	}
	gzWriter := gzip.NewWriter(gz)
	if _, err := gzWriter.Write(data); err != nil {
		_ = gzWriter.Close()
		_ = gz.Close()
		_ = os.Remove(rotated)
		return fmt.Errorf("gzip write: %w", err)
	}
	if err := gzWriter.Close(); err != nil {
		_ = gz.Close()
		_ = os.Remove(rotated)
		return fmt.Errorf("gzip close: %w", err)
	}
	if err := gz.Close(); err != nil {
		return fmt.Errorf("close rotated log: %w", err)
	}
	if err := os.Chmod(rotated, 0o600); err != nil {
		// A rotation that succeeded but landed with the wrong mode is
		// still better than one that failed; report and continue.
		slog.Warn("hitl rotation chmod failed", "path", rotated, "err", err)
	}
	// Truncate in place so the existing O_APPEND handle (if any) keeps
	// appending at offset 0.
	if err := os.Truncate(path, 0); err != nil {
		return fmt.Errorf("truncate after rotation: %w", err)
	}
	return nil
}

// uniqueRotationPath returns path.<timestamp>.gz, appending -1, -2, …
// until the name is free.
func uniqueRotationPath(path, timestamp string) string {
	candidate := path + "." + timestamp + ".gz"
	for i := 1; ; i++ {
		if _, err := os.Stat(candidate); os.IsNotExist(err) {
			return candidate
		}
		candidate = fmt.Sprintf("%s.%s-%d.gz", path, timestamp, i)
	}
}

// ==================== Expiry and count cap ====================

// rotationFile is a rotation's name and mtime, used for the sort that makes
// "keep the N newest" well-defined.
type rotationFile struct {
	path    string
	modTime time.Time
}

// listRotations returns every gzip rotation belonging to path, newest first.
func listRotations(path string) ([]rotationFile, error) {
	matches, err := filepath.Glob(path + ".*.gz")
	if err != nil {
		return nil, fmt.Errorf("list %s rotations: %w", path, err)
	}
	out := make([]rotationFile, 0, len(matches))
	for _, m := range matches {
		info, statErr := os.Stat(m)
		if statErr != nil {
			// A rotation that vanished (or became unreadable) between the
			// glob and the stat is not this function's problem to solve.
			continue
		}
		out = append(out, rotationFile{path: m, modTime: info.ModTime()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].modTime.After(out[j].modTime) })
	return out, nil
}

// enforceHITLRetention deletes rotations that are older than MaxAge, then
// deletes the oldest beyond CountCap so at most CountCap survive.
//
// Both bounds are enforced behaviour, not advisory text: the count cap is
// the safety bound for a burst that would otherwise fill the disk, and the
// age bound is what makes the disk usage a function of time rather than of
// accumulated history.
//
// The cap is applied AFTER expiry so a stream that is merely old is trimmed
// by age, and a stream that is merely large is trimmed by count — applying
// them in the other order would let a cap-sized backlog of ancient rotations
// survive on count alone.
//
// Rotations are matched by filename pattern only. The policy is
// stream-scoped, not version-scoped: a v1 rotation is a rotation, and giving
// the current format a grace period the policy never mentions would be a
// silent exemption. `mpm ops logs purge --scope` is the supported way to
// remove history deliberately.
func enforceHITLRetention(path string, pol streamPolicy, now time.Time) error {
	if path == "" || (pol.MaxAge <= 0 && pol.CountCap <= 0) {
		return nil
	}
	rots, err := listRotations(path)
	if err != nil {
		return err
	}

	survivors := make([]rotationFile, 0, len(rots))
	for _, r := range rots {
		if pol.MaxAge > 0 && now.Sub(r.modTime) > pol.MaxAge {
			if rmErr := os.Remove(r.path); rmErr != nil {
				slog.Warn("hitl rotation expiry failed", "path", r.path, "err", rmErr)
				continue
			}
			continue
		}
		survivors = append(survivors, r)
	}

	if pol.CountCap > 0 && len(survivors) > pol.CountCap {
		for _, r := range survivors[pol.CountCap:] {
			if rmErr := os.Remove(r.path); rmErr != nil {
				slog.Warn("hitl rotation cap failed", "path", r.path, "err", rmErr)
			}
		}
	}
	return nil
}

// ==================== Explicit purge (log scope only) ====================

// PurgeResult reports what a purge removed.
type PurgeResult struct {
	// Truncated is true when an active file was emptied.
	Truncated bool
	// Rotations is the number of gzip rotations deleted.
	Rotations int
}

// purgeHITLStream empties the active file and removes every rotation
// belonging to it.
//
// This is deliberately LOG-SCOPE ONLY. It does not touch mpm.db, the audit
// table, backups, or any other cognitive store: the operator asking to drop
// the log copies is not asking for the memories themselves to disappear.
// `mpm memory shred <id>` and `mpm work item purge <id>` are the paths for
// that, and the former is the only one that is a hard delete.
func purgeHITLStream(path string) (PurgeResult, error) {
	var res PurgeResult
	if path == "" {
		return res, nil
	}
	if _, err := os.Stat(path); err == nil {
		if err := os.Truncate(path, 0); err != nil {
			return res, fmt.Errorf("truncate %s: %w", path, err)
		}
		res.Truncated = true
	} else if !os.IsNotExist(err) {
		return res, fmt.Errorf("stat %s: %w", path, err)
	}

	rots, err := listRotations(path)
	if err != nil {
		return res, err
	}
	for _, r := range rots {
		if rmErr := os.Remove(r.path); rmErr != nil {
			return res, fmt.Errorf("remove rotation %s: %w", r.path, rmErr)
		}
		res.Rotations++
	}
	return res, nil
}

// PurgeHITLLogForCLI is the CLI entry point behind `mpm ops logs purge
// --scope mirror|watchdog`. It resolves the stream by log name so the CLI
// cannot name a path outside the managed pair.
func PurgeHITLLogForCLI(logName string) (PurgeResult, error) {
	pol, ok := PolicyForLogName(logName)
	if !ok {
		return PurgeResult{}, fmt.Errorf("unknown log name %q (want mirror | watchdog)", logName)
	}
	return purgeHITLStream(DefaultLogPath(pol.Name))
}

// DefaultLogPath returns the ambient canonical path for a managed log. Only
// the CLI uses it; a DatabaseManager always derives its own paths from its
// own database directory, so nothing in the write path ever calls this.
func DefaultLogPath(name string) string {
	return filepath.Join(filepath.Dir(config.GetMirrorPath()), name+".jsonl")
}

// ==================== SQL shape normalisation (security invariant) ====================
//
// The design's SQL secrecy contract (§7) is a hard invariant on par with
// F-4: a watchdog line may describe a statement but must never carry its
// data. Without it the redesigned watchdog recreates the secondary-sensitive
// datastore the redesign removes.
//
//   - never log bound values (they are Go-level args and never reach here);
//   - never log interpolated literals (these DO reach here, because MPM's
//     writers build some SQL with fmt.Sprintf);
//   - never log row content (never available here at all — this function
//     only ever sees the statement text).
//
// What it may log is the statement's SHAPE: keyword and identifier skeleton
// with every literal replaced by "?". That is what an operator needs to
// recognise "the INSERT INTO memories statement is failing" from a tail.

const sqlShapeMax = 120

// normalizeSQLShape returns the statement shape with every literal replaced
// by "?", whitespace collapsed, and the result capped at sqlShapeMax
// characters.
//
// It is a scanner rather than a regexp because the cases are lexical:
// SQLite string literals with doubled-quote escapes, quoted identifiers,
// bracketed identifiers, hex blobs, line comments, block comments and
// numeric literals each need different treatment, and a regexp that tries to
// cover all of them either misses one or eats identifiers.
func normalizeSQLShape(query string) string {
	if query == "" {
		return ""
	}
	r := []rune(query)
	var b strings.Builder
	b.Grow(len(query))

	emitQuestion := func() { b.WriteByte('?') }

	for i := 0; i < len(r); {
		c := r[i]
		switch {
		case c == '\'':
			// String literal. SQLite escapes ' as ''.
			j := i + 1
			for j < len(r) {
				if r[j] == '\'' {
					if j+1 < len(r) && r[j+1] == '\'' {
						j += 2
						continue
					}
					break
				}
				j++
			}
			emitQuestion()
			if j < len(r) {
				j++ // consume the closing quote
			}
			i = j

		case c == '"' || c == '`':
			// Quoted identifier. Kept verbatim when it is plainly an
			// identifier; replaced with ? otherwise, so a secret pasted
			// into a quoted span cannot ride along on the assumption that
			// "double quotes are always identifiers".
			closing := c
			j := i + 1
			for j < len(r) && r[j] != closing {
				j++
			}
			span := string(r[i:min(j+1, len(r))])
			if quotedSpanIsIdentifier(span) {
				b.WriteString(span)
			} else {
				emitQuestion()
			}
			if j < len(r) {
				j++
			}
			i = j

		case c == '[':
			// SQLite bracketed identifier — a literal-delimiter form.
			j := i + 1
			for j < len(r) && r[j] != ']' {
				j++
			}
			span := string(r[i:min(j+1, len(r))])
			if quotedSpanIsIdentifier(span) {
				b.WriteString(span)
			} else {
				emitQuestion()
			}
			if j < len(r) {
				j++
			}
			i = j

		case c == '-' && i+1 < len(r) && r[i+1] == '-':
			for i < len(r) && r[i] != '\n' {
				i++
			}

		case c == '/' && i+1 < len(r) && r[i+1] == '*':
			i += 2
			for i+1 < len(r) && !(r[i] == '*' && r[i+1] == '/') {
				i++
			}
			i = min(i+2, len(r))

		case isDigit(c) || (c == '.' && i+1 < len(r) && isDigit(r[i+1])):
			j := i
			if c == '0' && i+1 < len(r) && (r[i+1] == 'x' || r[i+1] == 'X') {
				j = i + 2
				for j < len(r) && isHexDigit(r[j]) {
					j++
				}
			} else {
				// Integer part. For a leading "." (e.g. ".5") i is the
				// dot itself and the integer part is empty.
				if c != '.' {
					for j < len(r) && isDigit(r[j]) {
						j++
					}
				}
				if j < len(r) && r[j] == '.' {
					j++ // consume the dot
					for j < len(r) && isDigit(r[j]) {
						j++
					}
				}
			}
			emitQuestion()
			i = j

		case c == '?':
			// An existing bound-parameter placeholder: already the shape.
			emitQuestion()
			i++

		default:
			b.WriteRune(c)
			i++
		}
	}

	return truncateShape(collapseSpaces(b.String()))
}

// quotedSpanIsIdentifier reports whether a quoted span is a plain SQL
// identifier. Anything with a space, a dot-led name, or a non-identifier
// rune is treated as a literal, because the cost of over-normalising is a
// slightly less informative shape and the cost of under-normalising is a
// secret in an operational log.
func quotedSpanIsIdentifier(span string) bool {
	if len(span) < 2 {
		return false
	}
	for _, c := range span[1 : len(span)-1] {
		if !isIdentRune(c) {
			return false
		}
	}
	return true
}

func isDigit(c rune) bool { return c >= '0' && c <= '9' }
func isHexDigit(c rune) bool {
	return isDigit(c) || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

func isIdentRune(c rune) bool {
	return c == '_' || c == '.' || unicode.IsLetter(c) || unicode.IsDigit(c)
}

// collapseSpaces reduces every run of whitespace to a single space and trims
// the ends, so a multi-line formatted statement produces one readable line.
func collapseSpaces(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	space := false
	for _, c := range s {
		if unicode.IsSpace(c) {
			space = true
			continue
		}
		if space && b.Len() > 0 {
			b.WriteByte(' ')
		}
		space = false
		b.WriteRune(c)
	}
	return b.String()
}

// truncateShape caps s at sqlShapeMax characters, cutting at the last space
// so the shape does not end mid-token, and marking the cut.
func truncateShape(s string) string {
	r := []rune(s)
	if len(r) <= sqlShapeMax {
		return s
	}
	cut := sqlShapeMax - 1
	if idx := strings.LastIndex(string(r[:cut]), " "); idx > cut*3/4 {
		cut = idx
	}
	return strings.TrimRight(string(r[:cut]), " ") + "…"
}

// redactSensitiveText masks anything the secret scanner recognises in text
// that is about to be written to watchdog. Used on `error` and `detail`,
// which are the only free-form fields in a v2 watchdog record.
//
// This is defence in depth, not the primary control: a bound value never
// reaches a watchdog line at all, and normalizeSQLShape already removes
// interpolated literals from the statement. It closes the remaining door —
// an error string that a driver chose to echo the offending value into, and
// a synthesis detail that quotes content.
func redactSensitiveText(s string) string {
	if s == "" {
		return ""
	}
	for _, p := range sensitivePatterns {
		if p.pattern == nil {
			continue
		}
		s = p.pattern.ReplaceAllString(s, "[redacted:"+p.name+"]")
	}
	return s
}

// ==================== CLI-facing policy projection ====================

// HITLRetention is the display projection of a stream's retention policy.
// It exists so the CLI can print the contract without the core package
// exporting its internal policy type: the fields an operator reads are
// days and bytes, and the runtime carries time.Duration.
type HITLRetention struct {
	Name             string
	SizeThreshold    int64
	MaxActiveAgeDays int
	MaxAgeDays       int
	CountCap         int
}

// RetentionForLogName returns the retention contract for a managed log.
func RetentionForLogName(name string) (HITLRetention, bool) {
	pol, ok := PolicyForLogName(name)
	if !ok {
		return HITLRetention{}, false
	}
	return HITLRetention{
		Name:             pol.Name,
		SizeThreshold:    pol.SizeThreshold,
		MaxActiveAgeDays: int(pol.MaxActiveAge.Hours() / 24),
		MaxAgeDays:       int(pol.MaxAge.Hours() / 24),
		CountCap:         pol.CountCap,
	}, true
}

// LogRotateThresholdOverrideForCLI reports the MPM_LOG_ROTATE_BYTES
// override, if one is set. `mpm ops logs status` prints it alongside the
// per-stream defaults so an operator can see which numbers are in force.
func LogRotateThresholdOverrideForCLI() (int64, bool) { return logRotateThresholdOverride() }

// ListRotationsForCLI returns the gzip rotation paths belonging to an
// active log, newest first. Used by `mpm ops logs status`.
func ListRotationsForCLI(path string) ([]string, error) {
	rots, err := listRotations(path)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(rots))
	for _, r := range rots {
		out = append(out, r.path)
	}
	return out, nil
}
