package pager

import (
	"encoding/binary"
	"errors"
	"hash/crc32"
	"os"
	"path/filepath"
	"testing"
)

// readHeaderPage returns the raw first page of the file at path.
func readHeaderPage(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if len(data) < PageSize {
		t.Fatalf("file is %d bytes, want at least one page", len(data))
	}
	return data[:PageSize]
}

// writeAt overwrites bytes in the file at path.
func writeAt(t *testing.T, path string, off int64, b []byte) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer f.Close()
	if _, err := f.WriteAt(b, off); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
}

// tearSlot simulates a write torn partway through a header slot: the magic
// survives, the payload does not, so the checksum fails.
func tearSlot(t *testing.T, path string, slot int) {
	t.Helper()
	garbage := []byte{0xDE, 0xAD, 0xBE, 0xEF, 0xDE, 0xAD, 0xBE, 0xEF}
	writeAt(t, path, int64(slotOffset(slot))+offPageCount, garbage)
}

// prepareTwoCheckpoints leaves the file with slot A at LSN 1 and three pages,
// and slot B at LSN 2 and six pages.
func prepareTwoCheckpoints(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "torn.ember")
	p, err := Open(path, Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	commit(t, p, 1, func(b *Batch) error {
		for i := 0; i < 2; i++ {
			if _, _, err := b.Alloc(); err != nil {
				return err
			}
		}
		return nil
	})
	commit(t, p, 2, func(b *Batch) error {
		for i := 0; i < 3; i++ {
			if _, _, err := b.Alloc(); err != nil {
				return err
			}
		}
		return nil
	})
	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return path
}

func TestCheckpointAlternatesHeaderSlots(t *testing.T) {
	path := prepareTwoCheckpoints(t)
	page := readHeaderPage(t, path)
	slotA, _, err := decodeHeader(page[slotAOffset : slotAOffset+slotSize])
	if err != nil {
		t.Fatalf("slot A: %v", err)
	}
	slotB, _, err := decodeHeader(page[slotBOffset : slotBOffset+slotSize])
	if err != nil {
		t.Fatalf("slot B: %v", err)
	}
	if slotA.lsn != 1 || slotA.state.PageCount != 3 {
		t.Fatalf("slot A = LSN %d, %d pages; want LSN 1, 3 pages", slotA.lsn, slotA.state.PageCount)
	}
	if slotB.lsn != 2 || slotB.state.PageCount != 6 {
		t.Fatalf("slot B = LSN %d, %d pages; want LSN 2, 6 pages", slotB.lsn, slotB.state.PageCount)
	}
}

func TestOpenSurvivesTornNewerHeaderSlot(t *testing.T) {
	path := prepareTwoCheckpoints(t)
	tearSlot(t, path, 1) // destroy the newer slot

	p, err := Open(path, Options{})
	if err != nil {
		t.Fatalf("Open after torn header write: %v", err)
	}
	defer p.Close()
	if got := p.LSN(); got != 1 {
		t.Fatalf("LSN = %d, want 1: the surviving slot should have been chosen", got)
	}
	if got := p.State().PageCount; got != 3 {
		t.Fatalf("PageCount = %d, want 3", got)
	}
	// The recovered database is still writable, and the next checkpoint
	// reclaims the torn slot.
	commit(t, p, 3, func(b *Batch) error {
		_, _, err := b.Alloc()
		return err
	})
	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	reopened, err := Open(path, Options{})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	if got := reopened.LSN(); got != 3 {
		t.Fatalf("LSN after repair = %d, want 3", got)
	}
}

func TestOpenSurvivesTornOlderHeaderSlot(t *testing.T) {
	path := prepareTwoCheckpoints(t)
	tearSlot(t, path, 0) // destroy the older slot

	p, err := Open(path, Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer p.Close()
	if got := p.LSN(); got != 2 {
		t.Fatalf("LSN = %d, want 2", got)
	}
	if got := p.State().PageCount; got != 6 {
		t.Fatalf("PageCount = %d, want 6", got)
	}
}

func TestOpenRejectsBothHeaderSlotsCorrupt(t *testing.T) {
	path := prepareTwoCheckpoints(t)
	tearSlot(t, path, 0)
	tearSlot(t, path, 1)
	if _, err := Open(path, Options{}); !errors.Is(err, ErrCorruptHeader) {
		t.Fatalf("Open = %v, want ErrCorruptHeader", err)
	}
}

func TestOpenRejectsForeignFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "foreign.bin")
	junk := make([]byte, PageSize)
	copy(junk, "this is not a database, it is a poem about one")
	if err := os.WriteFile(path, junk, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := Open(path, Options{}); !errors.Is(err, ErrNotDatabase) {
		t.Fatalf("Open = %v, want ErrNotDatabase", err)
	}
}

func TestOpenRejectsTruncatedFile(t *testing.T) {
	path := prepareTwoCheckpoints(t)
	if err := os.Truncate(path, 100); err != nil {
		t.Fatalf("Truncate: %v", err)
	}
	if _, err := Open(path, Options{}); !errors.Is(err, ErrNotDatabase) {
		t.Fatalf("Open of a file too short to hold a header = %v, want ErrNotDatabase", err)
	}
}

func TestOpenReportsUnknownFormatVersion(t *testing.T) {
	path := prepareTwoCheckpoints(t)
	page := readHeaderPage(t, path)
	for _, slot := range []int{0, 1} {
		off := slotOffset(slot)
		buf := make([]byte, slotSize)
		copy(buf, page[off:off+slotSize])
		binary.LittleEndian.PutUint16(buf[offVersion:], FormatVersion+7)
		binary.LittleEndian.PutUint32(buf[offChecksum:], crc32.Checksum(buf[:offChecksum], crcTable))
		writeAt(t, path, int64(off), buf)
	}
	_, err := Open(path, Options{})
	var verr *VersionError
	if !errors.As(err, &verr) {
		t.Fatalf("Open = %v, want a *VersionError", err)
	}
	if verr.Found != FormatVersion+7 {
		t.Fatalf("VersionError.Found = %d, want %d", verr.Found, FormatVersion+7)
	}
}

func TestHeaderSlotsCannotShareASector(t *testing.T) {
	// Torn-write protection depends on the two slots landing in different
	// sectors on every plausible device.
	const largestPlausibleSector = 4096
	gap := slotBOffset - slotAOffset
	if gap < 512 {
		t.Fatalf("header slots are %d bytes apart, too close to be independent", gap)
	}
	if slotAOffset+slotSize > slotBOffset || slotBOffset+slotSize > largestPlausibleSector {
		t.Fatal("header slots overlap or overflow the header page")
	}
}
