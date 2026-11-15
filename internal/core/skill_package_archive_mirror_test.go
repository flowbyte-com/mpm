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

// TestP5_TheHeaderMirrorIsStructural pins the RULE the mirror applies.
//
// The mirror decides which of the reader's two candidate parse starts
// the importer uses, and so it decides the parse WINDOW — the bytes
// `archive/zip` may scan as central-directory records.
//
// The rule must never be STRICTER than the reader's: a stricter rule
// would move the importer's parse start away from the reader's, and
// the importer's window would then be smaller than the window the
// reader actually scans.
//
// So the rule is the STRUCTURAL one, and this test pins it: the
// mirror's verdict must equal the structural rule, computed here
// independently — the fixed header is readable, the signature is the
// central-directory one, and the declared name, extra field and
// comment are readable.
//
// The test also asserts that the mirror is MORE PERMISSIVE than the
// refinements `archive/zip` applies on top of the structural rule: it
// accepts a header whose compressed size or local-header offset is
// saturated, and one whose ZIP64 extra field is short.
//
// Those are exactly the refinements that DIFFER between Go versions —
// Go 1.26.6 refuses the saturated sizes and Go 1.27.3 refuses the
// short second ZIP64 field — so accepting both is what makes the
// mirror version-independent.
//
// A mirror pinned to one Go version's internals would pass on that
// version and fail on the other. That is not hypothetical: it is
// exactly what this stage's CI caught.
func TestP5_TheHeaderMirrorIsStructural(t *testing.T) {
	rng := rand.New(rand.NewSource(20261115))

	const iterations = 200000
	var accepted, refused int

	for iteration := 0; iteration < iterations; iteration++ {
		// A randomised header.
		header := p5RandomHeader(rng)

		mirror := p5MirrorAccepts(header)

		want := p5StructurallyValid(header)
		if mirror != want {
			t.Fatalf("the mirror accepts = %v, the structural rule says = %v (%x)",
				mirror, want, header)
		}
		if mirror {
			accepted++
		} else {
			refused++
		}
	}

	t.Logf("%d randomised headers: %d accepted, %d refused", iterations, accepted, refused)
	if accepted == 0 || refused == 0 {
		t.Fatalf("the generator produced %d accepted and %d refused headers — "+
			"it is not exercising both verdicts", accepted, refused)
	}

	// The mirror must be MORE PERMISSIVE than the refinements
	// `archive/zip` applies, because those refinements are what
	// differ between Go versions.
	//
	// Go 1.26.6 refuses a saturated compressed size or local-header
	// offset with no ZIP64 extra field; Go 1.27.3 refuses a second
	// ZIP64 field shorter than the eight bytes it would need. The
	// mirror accepts both.
	for _, c := range []struct {
		description string
		header      []byte
	}{
		{"a saturated compressed size with no extra field",
			p5CandidateHeader(true, false, false, nil)},
		{"a saturated local-header offset with no extra field",
			p5CandidateHeader(false, false, true, nil)},
		{"every value saturated with no extra field",
			p5CandidateHeader(true, true, true, nil)},
		{"a short ZIP64 extra field",
			p5CandidateHeader(false, false, false, p5Zip64Extra(4))},
	} {
		if !p5MirrorAccepts(c.header) {
			t.Errorf("the mirror refuses %s — it must be more permissive than "+
				"`archive/zip`, whose refinements differ between Go versions", c.description)
		}
	}

	// And it must still REFUSE the structurally invalid headers —
	// otherwise it would be vacuously permissive.
	wrongSignature := p5CandidateHeader(false, false, false, nil)
	binary.LittleEndian.PutUint32(wrongSignature[0:], 0x04034b50)
	if p5MirrorAccepts(wrongSignature) {
		t.Error("the mirror accepts a header with the wrong signature — it must refuse it")
	}

	truncated := p5CandidateHeader(false, false, false, nil)[:20]
	if p5MirrorAccepts(truncated) {
		t.Error("the mirror accepts a truncated header — it must refuse it")
	}

	// A header whose declared name runs past the end of the file.
	overrun := p5CandidateHeader(false, false, false, nil)
	binary.LittleEndian.PutUint16(overrun[28:], 0xffff)
	if p5MirrorAccepts(overrun) {
		t.Error("the mirror accepts a header whose name runs past the end of the file — " +
			"it must refuse it")
	}
}

// TestP5_TheHeaderMirrorNeverRefusesWhatTheReaderParses is the
// mirror's differential test against the REAL `archive/zip`.
//
// The property it checks is the one the bound needs, and the only one
// that can hold across Go versions: the mirror must never REFUSE a
// header the reader PARSES.
//
// It may refuse less — that is the safe direction, and it is what the
// structural rule gives — but it must never refuse more.
//
// The oracle is the reader itself. The archive is built so that the
// reader's two possible parse starts give DIFFERENT results:
//
//   - the declared offset points at the candidate header, spliced into
//     the local-header region, and the declared size makes the NORMAL
//     start land in the local-header region too — where there is no
//     record at all. So a reader that does NOT take the candidate
//     parses no records and fails its own count check;
//   - a reader that DOES take the candidate parses exactly one record,
//     which is the count the archive declares, and succeeds.
//
// So the reader's success means "the reader parses this header".
func TestP5_TheHeaderMirrorNeverRefusesWhatTheReaderParses(t *testing.T) {
	// The candidate headers: the plain shapes, and then the ZIP64
	// extra field over a range of field sizes — the sizes that decide
	// how many saturated values one field can satisfy.
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

	// TWO ZIP64 fields — the class where the two Go versions diverge
	// most sharply.
	//
	// Go 1.26.6 clears the saturation when it resolves a value, so a
	// second field is judged against the resolved value and a short one
	// is accepted. Go 1.27.3 does not clear the SIZE saturations, so a
	// second field must carry eight bytes again, and a short one is
	// refused.
	for _, c := range []struct {
		description string
		saturated   [3]bool
		first       int
		second      int
	}{
		{"a saturated uncompressed size with two ZIP64 fields, the second short",
			[3]bool{false, true, false}, 8, 4},
		{"a saturated compressed size with two ZIP64 fields, the second short",
			[3]bool{true, false, false}, 8, 4},
		{"a saturated local-header offset with two ZIP64 fields, the second short",
			[3]bool{false, false, true}, 8, 4},
		{"every value saturated with two ZIP64 fields, the second short",
			[3]bool{true, true, true}, 24, 4},
		{"every value saturated with two ZIP64 fields, the second long enough",
			[3]bool{true, true, true}, 24, 8},
	} {
		cases = append(cases, candidate{
			description: c.description,
			header: p5CandidateHeader(c.saturated[0], c.saturated[1], c.saturated[2],
				p5Zip64ExtraPair(c.first, c.second)),
		})
	}

	for _, size := range []int{0, 4, 8, 12, 16, 20, 24, 28} {
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
				header:      p5CandidateHeader(saturated[0], saturated[1], saturated[2], p5Zip64Extra(size)),
			})
		}
	}

	var readerAccepted int

	for _, c := range cases {
		readerParses := p5ReaderParsesHeader(t, c.header)
		mirrorAccepts := p5MirrorAccepts(c.header)

		if readerParses {
			readerAccepted++
		}

		// THE PROPERTY: the mirror never refuses what the reader
		// parses.
		if readerParses && !mirrorAccepts {
			t.Errorf("%s: the reader parses it, the mirror refuses it — "+
				"the mirror must never be stricter than the reader (%x)",
				c.description, c.header)
		}
	}

	// The oracle must not be vacuous: the reader accepts at least the
	// plain headers, on every Go version.
	if readerAccepted == 0 {
		t.Errorf("the reader refused every candidate header — the oracle proves nothing")
	}
	t.Logf("%d candidates: the reader parsed %d of them", len(cases), readerAccepted)
}

// p5ReaderParsesHeader reports whether `archive/zip` would PARSE the
// given central-directory header — using the REAL reader as the
// oracle.
//
// The archive is built so that the reader's two possible parse starts
// give different results: the declared offset points at the candidate
// header, and the declared size makes the normal start land in the
// local-header region, where there is no record. So a reader that does
// not take the candidate parses no records and fails its own count
// check, while a reader that does take it parses exactly one record —
// the count the archive declares — and succeeds.
//
// A success therefore means "the reader parses this header".
func p5ReaderParsesHeader(t *testing.T, header []byte) bool {
	t.Helper()

	// A base archive. The filler entry makes the local-header region
	// comfortably larger than the candidate header, so the normal start
	// — which must land in that region — always has room. The filler is
	// INCOMPRESSIBLE, so the region really is that large.
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	filler := make([]byte, 16*1024)
	if _, err := rand.New(rand.NewSource(1)).Read(filler); err != nil {
		t.Fatalf("build the filler: %v", err)
	}
	for _, entry := range []struct {
		name    string
		content []byte
	}{
		{"SKILL.md", []byte("x\n")},
		{"references/a.md", []byte("x\n")},
		{"filler.txt", filler},
	} {
		w, err := zw.Create(entry.name)
		if err != nil {
			t.Fatalf("create the archive entry: %v", err)
		}
		if _, err := w.Write(entry.content); err != nil {
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

	// The NORMAL start — `endOffset - size` — must land in the
	// local-header region, where there is no record at all. So:
	//
	//     size = endOffset - normalStart
	//
	// and the normal start is the byte just after the candidate.
	normalStart := insertAt + len(header)
	if normalStart >= cdOffset {
		t.Fatalf("the candidate header is too long for the fixture")
	}
	binary.LittleEndian.PutUint32(out[newEOCD+12:], uint32(newEOCD-normalStart))

	// The FALLBACK start — the declared offset — points at the
	// candidate.
	binary.LittleEndian.PutUint32(out[newEOCD+16:], uint32(insertAt))

	// The declared count is ONE: the reader succeeds only if it parsed
	// the candidate and nothing else.
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

// p5MirrorAccepts reports the importer's own verdict on the given
// central-directory header, read at offset zero.
func p5MirrorAccepts(header []byte) bool {
	d := packageArchiveDirectory{offset: 0}
	return d.centralDirectoryHeaderIsStructurallyValid(bytes.NewReader(header), int64(len(header)))
}

// p5StructurallyValid is the structural rule, computed independently
// of the importer's own implementation.
//
// A header is structurally valid when its 46-byte fixed header is
// readable, its signature is the central-directory one, and the name,
// extra field and comment it declares are readable.
func p5StructurallyValid(header []byte) bool {
	const headerLen = archiveCentralDirectoryRecordMinBytes
	if len(header) < headerLen {
		return false
	}
	if binary.LittleEndian.Uint32(header[0:]) != archiveCentralDirectorySignature {
		return false
	}
	nameLen := int(binary.LittleEndian.Uint16(header[28:]))
	extraLen := int(binary.LittleEndian.Uint16(header[30:]))
	commentLen := int(binary.LittleEndian.Uint16(header[32:]))
	return headerLen+nameLen+extraLen+commentLen <= len(header)
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

// p5Zip64ExtraPair builds TWO ZIP64 extra fields, of the given sizes.
//
// The pair is what makes the two Go versions differ: Go 1.26.6 clears
// a saturation when it resolves a value, so the second field is judged
// against the resolved value; Go 1.27.3 leaves the SIZE saturations
// in place, so the second field must carry eight bytes again.
func p5Zip64ExtraPair(firstSize, secondSize int) []byte {
	extra := p5Zip64Extra(firstSize)
	return append(extra, p5Zip64Extra(secondSize)...)
}

// p5Zip64Extra builds a ZIP64 extra field of the given size.
func p5Zip64Extra(size int) []byte {
	extra := make([]byte, 4+size)
	binary.LittleEndian.PutUint16(extra[0:], archiveZip64ExtraID)
	binary.LittleEndian.PutUint16(extra[2:], uint16(size))
	for i := range extra[4:] {
		extra[4+i] = byte(i * 7)
	}
	return extra
}

// p5RandomHeader builds a randomised central-directory header: the
// signature, the three lengths, the saturations, and a randomised
// extra field.
func p5RandomHeader(rng *rand.Rand) []byte {
	header := make([]byte, archiveCentralDirectoryRecordMinBytes)
	binary.LittleEndian.PutUint32(header[0:], archiveCentralDirectorySignature)

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
	binary.LittleEndian.PutUint32(header[20:], saturate())
	binary.LittleEndian.PutUint32(header[24:], saturate())
	binary.LittleEndian.PutUint32(header[42:], saturate())

	// The DECLARED name, extra field and comment lengths. Sometimes
	// they run past the end of the header, which is one of the
	// structural refusals.
	declaredName := rng.Intn(8)
	declaredExtra := rng.Intn(24)
	declaredComment := rng.Intn(8)
	if rng.Intn(10) == 0 {
		declaredName = 0xffff
	}
	if rng.Intn(10) == 0 {
		declaredExtra = 0xffff
	}
	if rng.Intn(10) == 0 {
		declaredComment = 0xffff
	}
	binary.LittleEndian.PutUint16(header[28:], uint16(declaredName))
	binary.LittleEndian.PutUint16(header[30:], uint16(declaredExtra))
	binary.LittleEndian.PutUint16(header[32:], uint16(declaredComment))

	// The variable part. The declared lengths are what the rule
	// compares against the header's size, so the bytes themselves only
	// need to be long enough to make that comparison meaningful: a
	// declared length of 0xffff is refused whether the bytes are
	// there or not.
	materialise := func(declared int) int {
		if declared > 32 {
			return 32
		}
		return declared
	}
	out := append([]byte{}, header...)
	out = append(out, make([]byte, materialise(declaredName))...)
	for i := range out[len(header):] {
		out[len(header)+i] = byte(rng.Intn(256))
	}
	out = append(out, make([]byte, materialise(declaredExtra))...)
	out = append(out, make([]byte, materialise(declaredComment))...)

	// Sometimes the header is truncated.
	if rng.Intn(8) == 0 && len(out) > 4 {
		out = out[:rng.Intn(len(out))]
	}

	return out
}
