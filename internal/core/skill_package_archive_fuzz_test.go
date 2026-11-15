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

// TestP5_TheCentralDirectoryCountNeverFallsBelowTheReaders is the
// soundness invariant of the central-directory bound, checked by
// DIFFERENTIAL FUZZING.
//
// The bound is: the importer's record count is never lower than the
// number of records `archive/zip` will parse, and it is equal to it
// for every well-formed archive. The count is what caps the reader's
// allocation, so a count that is lower than the reader's defeats the
// bound — and that is exactly the defect class this stage has been
// through.
//
// The fuzz mutates a real archive in the shapes that have historically
// diverged — the declared offset, the declared size, a crafted header
// with absurd lengths, a crafted header carrying a malformed ZIP64
// extra field, and a lying declared count — and compares the two
// counts for every mutation the reader accepts.
//
// It is a differential test: it measures the importer against the
// reader, not against a hand-written expectation, so it cannot
// disagree with `archive/zip` by construction.
func TestP5_TheCentralDirectoryCountNeverFallsBelowTheReaders(t *testing.T) {
	dir := t.TempDir()

	// A real archive: an entrypoint and a few reference files.
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for i := 0; i < 40; i++ {
		w, err := zw.Create(fmt.Sprintf("references/f%03d.md", i))
		if err != nil {
			t.Fatalf("create the archive entry: %v", err)
		}
		if _, err := w.Write([]byte("# F\n")); err != nil {
			t.Fatalf("write the archive entry: %v", err)
		}
	}
	w, err := zw.Create("SKILL.md")
	if err != nil {
		t.Fatalf("create the archive entry: %v", err)
	}
	if _, err := w.Write([]byte("---\nname: fuzz\n---\nbody\n")); err != nil {
		t.Fatalf("write the archive entry: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close the archive: %v", err)
	}
	base := buf.Bytes()

	baseEOCD := bytes.LastIndex(base, []byte{0x50, 0x4b, 0x05, 0x06})
	if baseEOCD < 0 {
		t.Fatal("the archive has no end-of-central-directory record")
	}
	realCDOffset := int(binary.LittleEndian.Uint32(base[baseEOCD+16:]))

	// A fixed seed: the fuzz is a regression guard, so it must be
	// reproducible.
	rng := rand.New(rand.NewSource(20261115))

	var comparable, diverged, lower int

	for iteration := 0; iteration < 2000; iteration++ {
		out := append([]byte{}, base...)
		cdOffset := realCDOffset
		pointAt := 0
		pointed := false

		// The mutation class.
		class := rng.Intn(6)
		switch class {
		case 0, 1, 2:
			// The declared offset and/or the declared size. Patched
			// below, once the EOCD's own position is known.
		case 3:
			// A crafted header spliced in, with absurd lengths, and
			// the declared offset pointed at it — the inconsistent-
			// offsets shape.
			crafted := make([]byte, archiveCentralDirectoryRecordMinBytes)
			binary.LittleEndian.PutUint32(crafted[0:], archiveCentralDirectorySignature)
			binary.LittleEndian.PutUint16(crafted[28:], uint16(rng.Intn(65535)))
			binary.LittleEndian.PutUint16(crafted[30:], uint16(rng.Intn(65535)))
			binary.LittleEndian.PutUint16(crafted[32:], uint16(rng.Intn(65535)))
			at := rng.Intn(cdOffset)
			out = append(out[:at], append(crafted, out[at:]...)...)
			cdOffset += archiveCentralDirectoryRecordMinBytes
			pointAt = at
			pointed = true
		case 4:
			// A crafted header with a SATURATED size and a ZIP64
			// extra field shorter than eight bytes — the FOURTH way
			// `archive/zip` refuses a header.
			crafted := make([]byte, archiveCentralDirectoryRecordMinBytes+4)
			binary.LittleEndian.PutUint32(crafted[0:], archiveCentralDirectorySignature)
			binary.LittleEndian.PutUint32(crafted[20:], 0xffffffff)
			binary.LittleEndian.PutUint32(crafted[24:], 0xffffffff)
			binary.LittleEndian.PutUint32(crafted[42:], 0xffffffff)
			binary.LittleEndian.PutUint16(crafted[28:], 0)
			binary.LittleEndian.PutUint16(crafted[30:], 4)
			binary.LittleEndian.PutUint16(crafted[32:], 0)
			binary.LittleEndian.PutUint16(crafted[archiveCentralDirectoryRecordMinBytes:], archiveZip64ExtraID)
			binary.LittleEndian.PutUint16(crafted[archiveCentralDirectoryRecordMinBytes+2:], uint16(rng.Intn(8)))
			at := rng.Intn(cdOffset)
			out = append(out[:at], append(crafted, out[at:]...)...)
			cdOffset += archiveCentralDirectoryRecordMinBytes + 4
			pointAt = at
			pointed = true
		case 5:
			// The declared count lies.
		}

		// The EOCD, in the MUTATED bytes.
		eocd := bytes.LastIndex(out, []byte{0x50, 0x4b, 0x05, 0x06})
		if eocd < 0 {
			continue
		}
		cdSize := int(binary.LittleEndian.Uint32(out[eocd+12:]))

		switch class {
		case 0:
			binary.LittleEndian.PutUint32(out[eocd+16:], uint32(rng.Intn(cdOffset+1)))
		case 1:
			size := cdSize + rng.Intn(2000) - 1000
			if size < 0 {
				size = 0
			}
			binary.LittleEndian.PutUint32(out[eocd+12:], uint32(size))
		case 2:
			binary.LittleEndian.PutUint32(out[eocd+16:], uint32(rng.Intn(cdOffset+1)))
			size := cdSize + rng.Intn(2000) - 1000
			if size < 0 {
				size = 0
			}
			binary.LittleEndian.PutUint32(out[eocd+12:], uint32(size))
		case 5:
			binary.LittleEndian.PutUint16(out[eocd+10:], uint16(rng.Intn(65535)))
		}
		if pointed {
			// The declared offset points at the crafted header, and
			// the declared size makes `endOffset - size` the REAL
			// central directory's start.
			binary.LittleEndian.PutUint32(out[eocd+16:], uint32(pointAt))
			binary.LittleEndian.PutUint32(out[eocd+12:], uint32(eocd-cdOffset))
		}

		path := filepath.Join(dir, "fuzz.skill")
		if err := os.WriteFile(path, out, 0o600); err != nil {
			t.Fatalf("write the mutated archive: %v", err)
		}

		f, err := os.Open(path)
		if err != nil {
			t.Fatalf("open the mutated archive: %v", err)
		}
		info, err := f.Stat()
		if err != nil {
			f.Close()
			t.Fatalf("stat the mutated archive: %v", err)
		}

		// The reader's count. The reader refuses many of these
		// archives; those are skipped, because there is no count to
		// compare against.
		readerCount := int64(-1)
		if reader, err := zip.NewReader(f, info.Size()); err == nil {
			readerCount = int64(len(reader.File))
		}

		// The importer's count.
		importerCount := int64(-2)
		if d, err := readPackageArchiveDirectory(f, info.Size()); err == nil {
			if n, err := d.parseRecordCount(f, info.Size(), 1<<20); err == nil {
				importerCount = n
			}
		}
		f.Close()

		if readerCount < 0 {
			continue
		}
		comparable++
		if importerCount != readerCount {
			diverged++
		}
		if importerCount < readerCount {
			lower++
			if lower <= 5 {
				t.Errorf("the importer counted %d records, the reader parsed %d — "+
					"the count must never be lower (mutation class %d)",
					importerCount, readerCount, class)
			}
		}
	}

	t.Logf("%d comparable mutations: %d diverged from the reader's count, %d of them lower",
		comparable, diverged, lower)
	if lower != 0 {
		t.Errorf("the count was lower than the reader's in %d of %d mutations", lower, comparable)
	}
}
