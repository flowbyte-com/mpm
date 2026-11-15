package internal

import (
	"bytes"
	"encoding/binary"
	"math/rand"
	"testing"
)

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
// The ZIP64 handling has one subtlety that is easy to miss and that
// this transcription keeps: resolving a saturated local-header offset
// CLEARS its saturation, so a second ZIP64 field is judged against the
// resolved value. The two size saturations are never cleared.
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
		if uncompressedSaturated && len(fieldBuf) < 8 {
			return true
		}
		if compressedSaturated && len(fieldBuf) < 8 {
			return true
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
