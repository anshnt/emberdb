package pager

import (
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// newPager opens a pager on a temporary file and closes it when the test ends.
func newPager(t *testing.T, opts Options) (*Pager, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.ember")
	p, err := Open(path, opts)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { p.Close() })
	return p, path
}

// commit runs fn against a fresh batch and commits it at the given LSN.
func commit(t *testing.T, p *Pager, lsn uint64, fn func(b *Batch) error) {
	t.Helper()
	b := p.Begin()
	if err := fn(b); err != nil {
		t.Fatalf("batch: %v", err)
	}
	if err := p.Commit(b); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if err := p.Checkpoint(lsn); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
}

func TestOpenInitialisesEmptyFile(t *testing.T) {
	p, path := newPager(t, Options{})
	st := p.State()
	if st.PageCount != 1 {
		t.Fatalf("PageCount = %d, want 1 (header page only)", st.PageCount)
	}
	if st.FreeHead != 0 || st.FreeCount != 0 {
		t.Fatalf("free list = (%d, %d), want empty", st.FreeHead, st.FreeCount)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Size() != PageSize {
		t.Fatalf("file size = %d, want %d", info.Size(), PageSize)
	}
}

func TestAllocExtendsFileAndPersists(t *testing.T) {
	p, path := newPager(t, Options{})
	var ids []PageID
	commit(t, p, 1, func(b *Batch) error {
		for i := 0; i < 4; i++ {
			id, page, err := b.Alloc()
			if err != nil {
				return err
			}
			page[0] = byte(i + 1)
			ids = append(ids, id)
		}
		return nil
	})
	want := []PageID{1, 2, 3, 4}
	for i, id := range ids {
		if id != want[i] {
			t.Fatalf("Alloc %d returned page %d, want %d", i, id, want[i])
		}
	}
	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened, err := Open(path, Options{})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	if got := reopened.State().PageCount; got != 5 {
		t.Fatalf("PageCount after reopen = %d, want 5", got)
	}
	for i, id := range ids {
		data, err := reopened.Read(id)
		if err != nil {
			t.Fatalf("Read(%d): %v", id, err)
		}
		if data[0] != byte(i+1) {
			t.Fatalf("page %d byte 0 = %d, want %d", id, data[0], i+1)
		}
	}
}

func TestFreeListReusesPagesInLIFOOrder(t *testing.T) {
	p, _ := newPager(t, Options{})
	commit(t, p, 1, func(b *Batch) error {
		for i := 0; i < 5; i++ {
			if _, _, err := b.Alloc(); err != nil {
				return err
			}
		}
		return nil
	})
	if got := p.State().PageCount; got != 6 {
		t.Fatalf("PageCount = %d, want 6", got)
	}

	commit(t, p, 2, func(b *Batch) error {
		for _, id := range []PageID{2, 4, 5} {
			if err := b.Free(id); err != nil {
				return err
			}
		}
		return nil
	})
	st := p.State()
	if st.FreeCount != 3 {
		t.Fatalf("FreeCount = %d, want 3", st.FreeCount)
	}
	if st.FreeHead != 5 {
		t.Fatalf("FreeHead = %d, want 5 (most recently freed)", st.FreeHead)
	}
	if st.PageCount != 6 {
		t.Fatalf("freeing must not change PageCount, got %d", st.PageCount)
	}

	var reused []PageID
	commit(t, p, 3, func(b *Batch) error {
		for i := 0; i < 4; i++ {
			id, _, err := b.Alloc()
			if err != nil {
				return err
			}
			reused = append(reused, id)
		}
		return nil
	})
	want := []PageID{5, 4, 2, 6} // free list drains LIFO, then the file grows
	for i := range want {
		if reused[i] != want[i] {
			t.Fatalf("Alloc %d = page %d, want %d (sequence %v)", i, reused[i], want[i], reused)
		}
	}
	if st := p.State(); st.FreeCount != 0 || st.FreeHead != 0 {
		t.Fatalf("free list = (%d, %d), want drained", st.FreeHead, st.FreeCount)
	}
}

func TestFreeListSurvivesReopen(t *testing.T) {
	p, path := newPager(t, Options{})
	commit(t, p, 1, func(b *Batch) error {
		for i := 0; i < 3; i++ {
			if _, _, err := b.Alloc(); err != nil {
				return err
			}
		}
		return b.Free(2)
	})
	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened, err := Open(path, Options{})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	st := reopened.State()
	if st.FreeHead != 2 || st.FreeCount != 1 {
		t.Fatalf("free list after reopen = (%d, %d), want (2, 1)", st.FreeHead, st.FreeCount)
	}
	b := reopened.Begin()
	id, _, err := b.Alloc()
	if err != nil {
		t.Fatalf("Alloc: %v", err)
	}
	if id != 2 {
		t.Fatalf("Alloc reused page %d, want the freed page 2", id)
	}
}

func TestBatchIsolatesUncommittedWrites(t *testing.T) {
	p, _ := newPager(t, Options{})
	commit(t, p, 1, func(b *Batch) error {
		_, page, err := b.Alloc()
		if err != nil {
			return err
		}
		copy(page, "committed")
		return nil
	})

	b := p.Begin()
	page, err := b.Writable(1)
	if err != nil {
		t.Fatalf("Writable: %v", err)
	}
	copy(page, "dirty!!!!")

	shared, err := p.Read(1)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(shared[:9]) != "committed" {
		t.Fatalf("uncommitted write leaked into the page cache: %q", shared[:9])
	}
	if string(page[:9]) != "dirty!!!!" {
		t.Fatalf("batch lost its own write: %q", page[:9])
	}

	// Dropping the batch is a complete rollback.
	b = nil
	_ = b
	again, err := p.Read(1)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(again[:9]) != "committed" {
		t.Fatalf("after rollback page reads %q", again[:9])
	}
}

func TestBatchAllocIsInvisibleUntilCommit(t *testing.T) {
	p, _ := newPager(t, Options{})
	b := p.Begin()
	id, _, err := b.Alloc()
	if err != nil {
		t.Fatalf("Alloc: %v", err)
	}
	if _, err := p.Read(id); !errors.Is(err, ErrPageOutOfRange) {
		t.Fatalf("Read of uncommitted page returned %v, want ErrPageOutOfRange", err)
	}
	if got := p.State().PageCount; got != 1 {
		t.Fatalf("PageCount = %d before commit, want 1", got)
	}
}

func TestCacheEvictsLeastRecentlyUsed(t *testing.T) {
	p, _ := newPager(t, Options{CacheSize: 4})
	commit(t, p, 1, func(b *Batch) error {
		for i := 0; i < 16; i++ {
			id, page, err := b.Alloc()
			if err != nil {
				return err
			}
			binary.LittleEndian.PutUint32(page, uint32(id))
		}
		return nil
	})
	if got := p.CachedPages(); got > 4 {
		t.Fatalf("cache holds %d pages, capacity is 4", got)
	}
	// Touch pages 1..4, then pull in a fifth; page 1 is the oldest and goes.
	for _, id := range []PageID{1, 2, 3, 4} {
		if _, err := p.Read(id); err != nil {
			t.Fatalf("Read(%d): %v", id, err)
		}
	}
	if _, err := p.Read(5); err != nil {
		t.Fatalf("Read(5): %v", err)
	}
	p.mu.Lock()
	_, stillCached := p.cache.get(1)
	p.mu.Unlock()
	if stillCached {
		t.Fatal("page 1 should have been evicted as least recently used")
	}
	// Eviction is not data loss: the page reloads from the file.
	data, err := p.Read(1)
	if err != nil {
		t.Fatalf("Read(1) after eviction: %v", err)
	}
	if got := binary.LittleEndian.Uint32(data); got != 1 {
		t.Fatalf("reloaded page 1 contains %d", got)
	}
}

func TestReadRejectsPagesPastEndOfFile(t *testing.T) {
	p, _ := newPager(t, Options{})
	if _, err := p.Read(99); !errors.Is(err, ErrPageOutOfRange) {
		t.Fatalf("Read(99) = %v, want ErrPageOutOfRange", err)
	}
}

func TestFreeRejectsHeaderPage(t *testing.T) {
	p, _ := newPager(t, Options{})
	b := p.Begin()
	if err := b.Free(HeaderPage); err == nil {
		t.Fatal("freeing the header page must fail")
	}
}

func TestMetaRoundTrips(t *testing.T) {
	p, path := newPager(t, Options{})
	var meta [MetaSize]byte
	binary.LittleEndian.PutUint32(meta[0:], 42)
	binary.LittleEndian.PutUint64(meta[8:], 1<<40)
	commit(t, p, 7, func(b *Batch) error {
		b.SetMeta(meta)
		return nil
	})
	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	reopened, err := Open(path, Options{})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	if got := reopened.State().Meta; got != meta {
		t.Fatalf("meta = %v, want %v", got, meta)
	}
	if got := reopened.LSN(); got != 7 {
		t.Fatalf("LSN = %d, want 7", got)
	}
}
