package internal

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
)

// p5GoParsesHeader reports whether `archive/zip` would PARSE the given
// central-directory header — using the REAL reader as the oracle.
//
// It is the oracle the transcription cannot be: a transcription can
// encode the same wrong model of the reader as the code it is meant to
// check, and then the differential test agrees with itself and with
// nothing else. (That is exactly what happened once: both the mirror
// and its transcription read the local-header offset from the FIRST
// eight bytes of the ZIP64 field, where the reader reads it from
// whichever eight bytes the cursor stands on.)
//
// The archive is built so that the reader's two possible parse starts
// give DIFFERENT record counts:
//
//   - the real central directory holds TWO records, and the declared
//     count says ONE;
//   - the candidate header is spliced into the local-header region,
//     and the declared offset points at it, so the reader either
//     takes its fallback and reads the candidate's ONE record, or
//     takes the normal start and reads the real directory's TWO.
//
// The reader's own count check then decides the outcome: it succeeds
// exactly when it took the fallback, and fails when it did not.
//
// So a success means "the reader parses this header".
func p5GoParsesHeader(t *testing.T, header []byte) bool {
	t.Helper()

	// A base archive with TWO records in its central directory.
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, name := range []string{"SKILL.md", "references/a.md"} {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("create the archive entry: %v", err)
		}
		if _, err := w.Write([]byte("x\n")); err != nil {
			t.Fatalf("write the archive entry: %v", err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close the archive: %v", err)
	}
	data := buf.Bytes()

	eocd := bytes.LastIndex(data, []byte{0x50, 0x4b, 0x05, 0x06})
	cdOffset := int(binary.LittleEndian.Uint32(data[eocd+16:]))

	// Splice the candidate header into the local-header region.
	insertAt := len(data) / 3
	out := make([]byte, 0, len(data)+len(header))
	out = append(out, data[:insertAt]...)
	out = append(out, header...)
	out = append(out, data[insertAt:]...)

	newEOCD := eocd + len(header)
	// The declared offset points at the candidate, and the declared
	// size makes `endOffset - size` the REAL central directory's
	// start.
	binary.LittleEndian.PutUint32(out[newEOCD+16:], uint32(insertAt))
	binary.LittleEndian.PutUint32(out[newEOCD+12:], uint32(newEOCD-(cdOffset+len(header))))
	// The declared count says ONE, so the reader succeeds only if it
	// took the fallback and read the candidate's single record.
	binary.LittleEndian.PutUint16(out[newEOCD+10:], 1)

	path := filepath.Join(t.TempDir(), "oracle.skill")
	if err := os.WriteFile(path, out, 0o600); err != nil {
		t.Fatalf("write the archive: %v", err)
	}
	reader, err := zip.OpenReader(path)
	if err != nil {
		return false
	}
	reader.Close()
	return true
}

// TestP5_TheHeaderMirrorMatchesTheRealReader is the mirror's
// differential test against the REAL `archive/zip`.
//
// The committed transcription-based test is broad but not independent:
// a transcription can encode the same wrong model as the code it
// checks. This test uses the reader itself as the oracle, over the
// header shapes that have historically diverged — the ZIP64 extra
// field with a saturated size, with a saturated local-header offset,
// and with several saturated values at once, over a range of field
// sizes.
//
// The mirror must agree with the reader, in BOTH directions.
func TestP5_TheHeaderMirrorMatchesTheRealReader(t *testing.T) {
	// The candidate headers: a name, and an extra field.
	const name = "x.md"

	// The candidate headers: the plain shapes, and then the ZIP64
	// extra field over a range of field sizes — the sizes that decide
	// how many saturated values one field can satisfy.
	//
	// The list holds CONCRETE headers. An earlier version of this
	// test built them with closures whose parameters shadowed the
	// captured loop variables, so every ZIP64 case was silently a
	// plain header and the test proved nothing about the class it
	// named.
	type candidate struct {
		description string
		header      []byte
	}
	var cases []candidate

	for _, plain := range []struct {
		description string
		saturated   [3]bool
	}{
		{"a plain header", [3]bool{false, false, false}},
		{"a saturated compressed size", [3]bool{true, false, false}},
		{"a saturated uncompressed size", [3]bool{false, true, false}},
		{"a saturated local-header offset", [3]bool{false, false, true}},
		{"every value saturated", [3]bool{true, true, true}},
	} {
		cases = append(cases, candidate{
			description: plain.description,
			header:      p5CandidateHeader(plain.saturated[0], plain.saturated[1], plain.saturated[2], nil),
		})
	}

	for _, size := range []int{0, 4, 8, 12, 16, 20, 24, 28} {
		extra := make([]byte, 4+size)
		binary.LittleEndian.PutUint16(extra[0:], archiveZip64ExtraID)
		binary.LittleEndian.PutUint16(extra[2:], uint16(size))
		for i := range extra[4:] {
			extra[4+i] = byte(i * 7)
		}

		for _, saturated := range [][3]bool{
			{true, false, false},
			{false, true, false},
			{false, false, true},
			{true, true, false},
			{true, false, true},
			{false, true, true},
			{true, true, true},
		} {
			cases = append(cases, candidate{
				description: fmt.Sprintf("a ZIP64 field of %d bytes, saturated %v", size, saturated),
				header:      p5CandidateHeader(saturated[0], saturated[1], saturated[2], extra),
			})
		}
	}

	for _, c := range cases {
		header := c.header
		goParses := p5GoParsesHeader(t, header)

		d := packageArchiveDirectory{offset: 0}
		mirrorParses := d.centralDirectoryHeaderParses(bytes.NewReader(header), int64(len(header)))

		if goParses != mirrorParses {
			t.Errorf("%s: the reader parses it = %v, the mirror parses it = %v — they must agree (%x)",
				c.description, goParses, mirrorParses, header)
		}
	}
}

// p5CandidateHeader builds a central-directory header: the fixed
// 46-byte part, then the name, then the extra field.
//
// The three booleans saturate the compressed size, the uncompressed
// size and the local-header offset respectively.
func p5CandidateHeader(compressedSaturated, uncompressedSaturated, offsetSaturated bool, extra []byte) []byte {
	header := make([]byte, archiveCentralDirectoryRecordMinBytes)
	binary.LittleEndian.PutUint32(header[0:], archiveCentralDirectorySignature)
	if compressedSaturated {
		binary.LittleEndian.PutUint32(header[20:], 0xffffffff)
	}
	if uncompressedSaturated {
		binary.LittleEndian.PutUint32(header[24:], 0xffffffff)
	}
	if offsetSaturated {
		binary.LittleEndian.PutUint32(header[42:], 0xffffffff)
	}
	binary.LittleEndian.PutUint16(header[28:], uint16(len("x.md")))
	binary.LittleEndian.PutUint16(header[30:], uint16(len(extra)))
	out := append([]byte{}, header...)
	out = append(out, []byte("x.md")...)
	out = append(out, extra...)
	return out
}

// TestP5_TheHeaderMirrorMatchesArchiveZip is a DIFFERENTIAL test of the
// importer's mirror of `archive/zip`'s central-directory header parse.
//
// The importer decides whether the reader's inconsistent-offsets
// fallback applies by asking whether a complete central-directory
// header is at the declared offset. That question has to be answered
// the SAME way `archive/zip` answers it, or the parse start can
// differ from the reader's.
//
// `goHeaderRefuses` below is a transcription of the refusal logic of
// `archive/zip`'s `readDirectoryHeader` (Go 1.27's `reader.go`). The
// test compares the two over randomised headers and requires them to
// AGREE — in both directions.
//
// It found a real divergence: `archive/zip` resolves a saturated
// local-header offset from the ZIP64 extra field, which CLEARS the
// saturation, so a second ZIP64 field is judged against the resolved
// value. The mirror did not, and so refused 3 924 of 200 000
// randomised headers that the reader accepts.
//
// The direction of that divergence was the safe one — the mirror
// refused, the reader parsed — and the count does not depend on the
// mirror alone. But an exact mirror is better than a safe one, and
// this test is what keeps it exact.
//
// NOTE: if Go changes `readDirectoryHeader`, this transcription must
// change with it. The test will say so: it will fail.
func TestP5_TheHeaderMirrorMatchesArchiveZip(t *testing.T) {
	rng := rand.New(rand.NewSource(4242))

	var bothRefuse, bothParse, mirrorRefusesOnly, goRefusesOnly int

	for iteration := 0; iteration < 200000; iteration++ {
		// A randomised header.
		h := make([]byte, archiveCentralDirectoryRecordMinBytes)
		binary.LittleEndian.PutUint32(h[0:], archiveCentralDirectorySignature)
		saturate := func() uint32 {
			switch rng.Intn(3) {
			case 0:
				return 0xffffffff
			case 1:
				return uint32(rng.Intn(0x10000))
			default:
				return 0
			}
		}
		binary.LittleEndian.PutUint32(h[20:], saturate())
		binary.LittleEndian.PutUint32(h[24:], saturate())
		binary.LittleEndian.PutUint32(h[42:], saturate())

		// A randomised extra field: a random mix of ZIP64 fields and
		// other tags, of random sizes.
		extra := make([]byte, 0, 64)
		for i := 0; i < 1+rng.Intn(3); i++ {
			tag := uint16(archiveZip64ExtraID)
			if rng.Intn(4) == 0 {
				tag = uint16(rng.Intn(0x10000))
			}
			size := rng.Intn(20)
			extra = append(extra, byte(tag), byte(tag>>8), byte(size), byte(size>>8))
			for j := 0; j < size; j++ {
				extra = append(extra, byte(rng.Intn(256)))
			}
		}
		binary.LittleEndian.PutUint16(h[30:], uint16(len(extra)))

		bytes_ := append(append([]byte{}, h...), extra...)

		goRefuses := goHeaderRefuses(bytes_)
		d := packageArchiveDirectory{offset: 0}
		mirrorRefuses := !d.centralDirectoryHeaderParses(bytes.NewReader(bytes_), int64(len(bytes_)))

		switch {
		case goRefuses && mirrorRefuses:
			bothRefuse++
		case !goRefuses && !mirrorRefuses:
			bothParse++
		case mirrorRefuses:
			mirrorRefusesOnly++
			if mirrorRefusesOnly <= 3 {
				t.Errorf("the mirror refuses a header `archive/zip` accepts: %x", bytes_)
			}
		default:
			goRefusesOnly++
			if goRefusesOnly <= 3 {
				t.Errorf("the mirror accepts a header `archive/zip` refuses: %x", bytes_)
			}
		}
	}

	t.Logf("%d randomised headers: %d refused by both, %d parsed by both, "+
		"%d refused only by the mirror, %d refused only by `archive/zip`",
		bothRefuse+bothParse+mirrorRefusesOnly+goRefusesOnly, bothRefuse, bothParse,
		mirrorRefusesOnly, goRefusesOnly)
	if mirrorRefusesOnly != 0 || goRefusesOnly != 0 {
		t.Errorf("the mirror and `archive/zip` disagree on %d headers",
			mirrorRefusesOnly+goRefusesOnly)
	}
}

// goHeaderRefuses is a transcription of the refusal logic of
// `archive/zip`'s `readDirectoryHeader` — Go 1.27's `reader.go` — for
// a central-directory header whose bytes are `buf`.
//
// It returns true when `archive/zip` would REFUSE the header.
//
// The ZIP64 handling has two subtleties that are easy to miss and that
// this transcription keeps:
//
//  1. the field's bytes are consumed SEQUENTIALLY, and each
//     consumption needs eight bytes REMAINING — so a field of exactly
//     eight bytes satisfies ONE saturated value, not two;
//  2. resolving a saturated local-header offset CLEARS its
//     saturation, so a second ZIP64 field is judged against the
//     resolved value, and the offset is read from wherever the cursor
//     stands — the third eight bytes when the two sizes are saturated
//     before it.
//
// The two size saturations are never cleared.
func goHeaderRefuses(buf []byte) bool {
	const headerLen = archiveCentralDirectoryRecordMinBytes
	if len(buf) < headerLen {
		return true
	}
	if binary.LittleEndian.Uint32(buf[0:]) != archiveCentralDirectorySignature {
		return true
	}

	compressedSaturated := binary.LittleEndian.Uint32(buf[20:]) == 0xffffffff
	uncompressedSaturated := binary.LittleEndian.Uint32(buf[24:]) == 0xffffffff
	nameLen := int(binary.LittleEndian.Uint16(buf[28:]))
	extraLen := int(binary.LittleEndian.Uint16(buf[30:]))
	commentLen := int(binary.LittleEndian.Uint16(buf[32:]))
	headerOffset := int64(binary.LittleEndian.Uint32(buf[42:]))
	headerOffsetSaturated := headerOffset == int64(0xffffffff)

	if len(buf) < headerLen+nameLen+extraLen+commentLen {
		return true
	}
	extra := buf[headerLen+nameLen : headerLen+nameLen+extraLen]

	for len(extra) >= 4 {
		tag := binary.LittleEndian.Uint16(extra[0:])
		fieldSize := int(binary.LittleEndian.Uint16(extra[2:]))
		extra = extra[4:]
		if len(extra) < fieldSize {
			break
		}
		fieldBuf := extra[:fieldSize]
		extra = extra[fieldSize:]

		if tag != archiveZip64ExtraID {
			continue
		}
		// The field's bytes are consumed SEQUENTIALLY, and each
		// consumption needs eight bytes REMAINING.
		if uncompressedSaturated {
			if len(fieldBuf) < 8 {
				return true
			}
			fieldBuf = fieldBuf[8:]
		}
		if compressedSaturated {
			if len(fieldBuf) < 8 {
				return true
			}
			fieldBuf = fieldBuf[8:]
		}
		if headerOffsetSaturated {
			if len(fieldBuf) < 8 {
				return true
			}
			headerOffset = int64(binary.LittleEndian.Uint64(fieldBuf[:8]))
			headerOffsetSaturated = headerOffset == int64(0xffffffff)
		}
	}
	return false
}
