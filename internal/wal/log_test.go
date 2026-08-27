package wal

import (
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/anshnt/emberdb/internal/pager"
)

// pageImage builds a recognisable page for page id in transaction txID.
func pageImage(txID uint64, id pager.PageID) []byte {
	data := make([]byte, pager.PageSize)
	binary.LittleEndian.PutUint64(data[0:], txID)
	binary.LittleEndian.PutUint32(data[8:], uint32(id))
	for i := 16; i < len(data); i++ {
		data[i] = byte(int(id) + i)
	}
	return data
}

// stateAt is the allocator state a transaction is pretending to install.
func stateAt(n uint32) pager.State {
	st := pager.State{PageCount: n + 1, FreeHead: pager.PageID(n), FreeCount: n}
	binary.LittleEndian.PutUint32(st.Meta[0:], n*13)
	return st
}

// collector records the page images a replay applies.
type collector struct {
	pages []pendingPage
}

func (c *collector) apply(id pager.PageID, data []byte) error {
	image := make([]byte, len(data))
	copy(image, data)
	c.pages = append(c.pages, pendingPage{id: id, data: image})
	return nil
}

// writeTransactions appends n committed transactions of pagesPer pages each
// and returns the log's path.
func writeTransactions(t *testing.T, n int, pagesPer int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.ember-wal")
	l, err := Open(path, Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	for tx := 1; tx <= n; tx++ {
		txID := uint64(tx)
		if err := l.Begin(txID); err != nil {
			t.Fatalf("Begin: %v", err)
		}
		for i := 0; i < pagesPer; i++ {
			id := pager.PageID(tx*10 + i)
			if err := l.Page(txID, id, pageImage(txID, id)); err != nil {
				t.Fatalf("Page: %v", err)
			}
		}
		lsn, err := l.Commit(txID, stateAt(uint32(tx)))
		if err != nil {
			t.Fatalf("Commit: %v", err)
		}
		if err := l.Sync(lsn); err != nil {
			t.Fatalf("Sync: %v", err)
		}
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return path
}

// replay reopens the log at path and replays it.
func replay(t *testing.T, path string) (*Log, Recovery, *collector) {
	t.Helper()
	l, err := Open(path, Options{})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { l.Close() })
	c := &collector{}
	rec, err := l.Replay(c.apply)
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	return l, rec, c
}

func TestReplayRestoresCommittedTransactions(t *testing.T) {
	path := writeTransactions(t, 4, 3)
	_, rec, c := replay(t, path)
	if rec.Commits != 4 {
		t.Fatalf("Commits = %d, want 4", rec.Commits)
	}
	if rec.Pages != 12 {
		t.Fatalf("Pages = %d, want 12", rec.Pages)
	}
	if rec.TornBytes != 0 {
		t.Fatalf("TornBytes = %d, want 0 on a cleanly closed log", rec.TornBytes)
	}
	if rec.State != stateAt(4) {
		t.Fatalf("State = %+v, want the last transaction's %+v", rec.State, stateAt(4))
	}
	if len(c.pages) != 12 {
		t.Fatalf("applied %d pages, want 12", len(c.pages))
	}
	for i, got := range c.pages {
		tx := uint64(i/3 + 1)
		id := pager.PageID(int(tx)*10 + i%3)
		if got.id != id {
			t.Fatalf("page %d applied id %d, want %d (replay must preserve log order)", i, got.id, id)
		}
		if string(got.data) != string(pageImage(tx, id)) {
			t.Fatalf("page %d image does not round-trip", i)
		}
	}
}

func TestReplayDiscardsTransactionWithoutCommit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "inflight.ember-wal")
	l, err := Open(path, Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := l.Begin(1); err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if err := l.Page(1, 7, pageImage(1, 7)); err != nil {
		t.Fatalf("Page: %v", err)
	}
	lsn, err := l.Commit(1, stateAt(1))
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	// A second transaction that writes pages but never commits: exactly the
	// state a process killed mid-transaction leaves behind.
	if err := l.Begin(2); err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if err := l.Page(2, 8, pageImage(2, 8)); err != nil {
		t.Fatalf("Page: %v", err)
	}
	if err := l.Sync(lsn); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	_, rec, c := replay(t, path)
	if rec.Commits != 1 {
		t.Fatalf("Commits = %d, want 1", rec.Commits)
	}
	if rec.Discarded != 1 {
		t.Fatalf("Discarded = %d, want 1", rec.Discarded)
	}
	if len(c.pages) != 1 || c.pages[0].id != 7 {
		t.Fatalf("applied %d pages, want only page 7 from the committed transaction", len(c.pages))
	}
}

func TestReplayDiscardsAbortedTransaction(t *testing.T) {
	path := filepath.Join(t.TempDir(), "abort.ember-wal")
	l, err := Open(path, Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := l.Begin(1); err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if err := l.Page(1, 3, pageImage(1, 3)); err != nil {
		t.Fatalf("Page: %v", err)
	}
	if err := l.Abort(1); err != nil {
		t.Fatalf("Abort: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	_, rec, c := replay(t, path)
	if rec.Commits != 0 || len(c.pages) != 0 {
		t.Fatalf("aborted transaction was replayed: %d commits, %d pages", rec.Commits, len(c.pages))
	}
	if rec.Discarded != 1 {
		t.Fatalf("Discarded = %d, want 1", rec.Discarded)
	}
}

func TestReplayStopsAtTruncatedTail(t *testing.T) {
	path := writeTransactions(t, 6, 2)
	full, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	// Cut the log at many points and check the recovered prefix is always
	// sane and never grows as the cut gets earlier.
	previous := -1
	for cut := len(full); cut >= headerSize; cut -= 617 {
		trimmed := filepath.Join(t.TempDir(), "trimmed.ember-wal")
		if err := os.WriteFile(trimmed, full[:cut], 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		_, rec, c := replay(t, trimmed)
		if rec.Commits < 0 || rec.Commits > 6 {
			t.Fatalf("cut at %d recovered %d commits", cut, rec.Commits)
		}
		if rec.Pages != rec.Commits*2 {
			t.Fatalf("cut at %d: %d commits but %d pages", cut, rec.Commits, rec.Pages)
		}
		if len(c.pages) != rec.Pages {
			t.Fatalf("cut at %d: Recovery reports %d pages, %d were applied", cut, rec.Pages, len(c.pages))
		}
		if previous >= 0 && rec.Commits > previous {
			t.Fatalf("cut at %d recovered %d commits, more than the longer log's %d", cut, rec.Commits, previous)
		}
		previous = rec.Commits
		if rec.Commits > 0 && rec.State != stateAt(uint32(rec.Commits)) {
			t.Fatalf("cut at %d: state %+v does not match the last surviving commit", cut, rec.State)
		}
	}
	if previous != 0 {
		t.Fatalf("cutting the log back to its header still recovered %d commits", previous)
	}
}

func TestReplayStopsAtCorruptedRecord(t *testing.T) {
	path := writeTransactions(t, 5, 1)
	full, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	// Each transaction is a begin, a page and a commit record. Corrupting a
	// byte deep in the third transaction's page image must cost exactly the
	// transactions from that point on.
	perTx := (len(full) - headerSize) / 5
	corruptAt := headerSize + 2*perTx + frameSize + payloadHead + 64
	damaged := append([]byte(nil), full...)
	damaged[corruptAt] ^= 0xFF
	path2 := filepath.Join(t.TempDir(), "corrupt.ember-wal")
	if err := os.WriteFile(path2, damaged, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	_, rec, c := replay(t, path2)
	if rec.Commits != 2 {
		t.Fatalf("Commits = %d, want 2: replay must stop at the first bad checksum", rec.Commits)
	}
	if len(c.pages) != 2 {
		t.Fatalf("applied %d pages, want 2", len(c.pages))
	}
	if rec.TornBytes <= 0 {
		t.Fatalf("TornBytes = %d, want the damaged tail to be reported", rec.TornBytes)
	}
}

func TestReplayTruncatesTornTailSoAppendsResume(t *testing.T) {
	path := writeTransactions(t, 3, 2)
	full, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	// Simulate a crash midway through a fourth transaction's page record.
	torn := append([]byte(nil), full...)
	torn = append(torn, make([]byte, 1500)...)
	binary.LittleEndian.PutUint32(torn[len(full):], uint32(payloadHead+pageBodySize))
	if err := os.WriteFile(path, torn, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	l, rec, _ := replay(t, path)
	if rec.Commits != 3 {
		t.Fatalf("Commits = %d, want 3", rec.Commits)
	}
	if rec.TornBytes != 1500 {
		t.Fatalf("TornBytes = %d, want 1500", rec.TornBytes)
	}
	if got := l.Size(); got != int64(len(full)) {
		t.Fatalf("log size after replay = %d, want the torn tail removed at %d", got, len(full))
	}
	// A fresh transaction lands on solid ground and survives another cycle.
	if err := l.Begin(4); err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if err := l.Page(4, 99, pageImage(4, 99)); err != nil {
		t.Fatalf("Page: %v", err)
	}
	lsn, err := l.Commit(4, stateAt(4))
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if err := l.Sync(lsn); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	_, rec2, c := replay(t, path)
	if rec2.Commits != 4 {
		t.Fatalf("Commits after reappending = %d, want 4", rec2.Commits)
	}
	if last := c.pages[len(c.pages)-1]; last.id != 99 {
		t.Fatalf("last applied page = %d, want 99", last.id)
	}
}

func TestOpenRejectsCorruptLogHeader(t *testing.T) {
	path := writeTransactions(t, 2, 1)
	f, err := os.OpenFile(path, os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	if _, err := f.WriteAt([]byte{0, 0, 0, 0}, hdrOffBaseLSN); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	if _, err := f.WriteAt([]byte{0xFF}, hdrOffChecksum); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	f.Close()
	if _, err := Open(path, Options{}); !errors.Is(err, ErrCorruptLogHeader) {
		t.Fatalf("Open = %v, want ErrCorruptLogHeader", err)
	}
}

func TestOpenRestartsLogTruncatedInsideItsHeader(t *testing.T) {
	path := writeTransactions(t, 2, 1)
	if err := os.Truncate(path, 12); err != nil {
		t.Fatalf("Truncate: %v", err)
	}
	l, err := Open(path, Options{})
	if err != nil {
		t.Fatalf("Open of a log killed before its header was durable: %v", err)
	}
	defer l.Close()
	if got := l.Size(); got != headerSize {
		t.Fatalf("Size = %d, want a fresh %d-byte header", got, headerSize)
	}
}

func TestTruncateRestartsTheLogAtANewBase(t *testing.T) {
	path := writeTransactions(t, 3, 2)
	l, rec, _ := replay(t, path)
	if err := l.Truncate(rec.LSN); err != nil {
		t.Fatalf("Truncate: %v", err)
	}
	if got := l.Size(); got != headerSize {
		t.Fatalf("Size after Truncate = %d, want %d", got, headerSize)
	}
	if got := l.LSN(); got != rec.LSN {
		t.Fatalf("LSN after Truncate = %d, want %d", got, rec.LSN)
	}
	if err := l.Begin(4); err != nil {
		t.Fatalf("Begin: %v", err)
	}
	lsn, err := l.Commit(4, stateAt(4))
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if lsn <= rec.LSN {
		t.Fatalf("LSN %d after truncation must exceed the checkpointed %d", lsn, rec.LSN)
	}
	if err := l.Sync(lsn); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	_, rec2, _ := replay(t, path)
	if rec2.Commits != 1 {
		t.Fatalf("Commits after truncation = %d, want only the new transaction", rec2.Commits)
	}
	if rec2.LSN != lsn {
		t.Fatalf("LSN = %d, want %d: sequence numbers must survive truncation", rec2.LSN, lsn)
	}
}

func TestConcurrentCommitsShareOneFsync(t *testing.T) {
	path := filepath.Join(t.TempDir(), "group.ember-wal")
	l, err := Open(path, Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer l.Close()

	const writers = 16
	lsns := make([]uint64, writers)
	var appended, release, done sync.WaitGroup
	appended.Add(writers)
	release.Add(1)
	done.Add(writers)
	for i := 0; i < writers; i++ {
		go func(i int) {
			defer done.Done()
			txID := uint64(i + 1)
			lsn, err := l.Commit(txID, stateAt(uint32(i+1)))
			if err != nil {
				t.Errorf("Commit: %v", err)
				lsns[i] = 0
				appended.Done()
				release.Wait()
				return
			}
			lsns[i] = lsn
			appended.Done()
			release.Wait()
			if err := l.Sync(lsn); err != nil {
				t.Errorf("Sync: %v", err)
			}
		}(i)
	}
	appended.Wait()
	before := l.Syncs()
	release.Done()
	done.Wait()

	if t.Failed() {
		return
	}
	if got := l.Syncs() - before; got != 1 {
		t.Fatalf("%d concurrent commits cost %d fsyncs, want 1", writers, got)
	}
	highest := uint64(0)
	for _, lsn := range lsns {
		if lsn > highest {
			highest = lsn
		}
	}
	if got := l.synced.Load(); got < highest {
		t.Fatalf("sync watermark %d is behind the highest commit %d", got, highest)
	}
}

func TestSyncIsIdempotentBelowTheWatermark(t *testing.T) {
	path := filepath.Join(t.TempDir(), "idem.ember-wal")
	l, err := Open(path, Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer l.Close()
	lsn, err := l.Commit(1, stateAt(1))
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if err := l.Sync(lsn); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	after := l.Syncs()
	for i := 0; i < 5; i++ {
		if err := l.Sync(lsn); err != nil {
			t.Fatalf("Sync: %v", err)
		}
	}
	if got := l.Syncs(); got != after {
		t.Fatalf("redundant syncs issued %d extra fsyncs", got-after)
	}
}

func TestRecordTypeNames(t *testing.T) {
	cases := map[RecordType]string{
		RecordBegin:   "begin",
		RecordPage:    "page",
		RecordCommit:  "commit",
		RecordAbort:   "abort",
		RecordType(9): "unknown(9)",
	}
	for typ, want := range cases {
		if got := typ.String(); got != want {
			t.Errorf("RecordType(%d).String() = %q, want %q", typ, got, want)
		}
	}
}

func TestReplayRejectsUnknownRecordType(t *testing.T) {
	path := filepath.Join(t.TempDir(), "future.ember-wal")
	l, err := Open(path, Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := l.append(RecordType(42), 1, []byte("from a newer build")); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	l2, err := Open(path, Options{})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer l2.Close()
	if _, err := l2.Replay(func(pager.PageID, []byte) error { return nil }); !errors.Is(err, ErrCorruptRecord) {
		t.Fatalf("Replay = %v, want ErrCorruptRecord", err)
	}
}
