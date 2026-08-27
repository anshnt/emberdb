package pager

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"sync"
)

// DefaultCacheSize is the page cache capacity used when Options leaves it
// unset: 2048 pages, or 8 MiB.
const DefaultCacheSize = 2048

// ErrClosed reports use of a pager after Close.
var ErrClosed = errors.New("emberdb: pager is closed")

// ErrPageOutOfRange reports a reference to a page beyond the end of the file.
// In a well-formed database it means the file is corrupt.
var ErrPageOutOfRange = errors.New("emberdb: page id out of range")

// Options configures Open.
type Options struct {
	// CacheSize is the maximum number of pages held in memory. Zero selects
	// DefaultCacheSize.
	CacheSize int
	// NoSync disables the fsync calls that make a checkpoint durable. It
	// makes tests and throughput experiments faster and makes crash
	// recovery meaningless; leave it false in production use.
	NoSync bool
}

// Pager owns the database file. Its exported methods are safe for concurrent
// use.
type Pager struct {
	mu     sync.Mutex
	file   *os.File
	path   string
	hdr    header
	slot   int // slot index the live header was read from
	cache  *cache
	noSync bool
	closed bool
}

// Open opens or creates the database file at path. A new file is initialised
// with a single header page and an empty free list.
func Open(path string, opts Options) (*Pager, error) {
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, fmt.Errorf("emberdb: open %s: %w", path, err)
	}
	capacity := opts.CacheSize
	if capacity == 0 {
		capacity = DefaultCacheSize
	}
	p := &Pager{
		file:   file,
		path:   path,
		cache:  newCache(capacity),
		noSync: opts.NoSync,
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, fmt.Errorf("emberdb: stat %s: %w", path, err)
	}
	if info.Size() == 0 {
		if err := p.initialise(); err != nil {
			file.Close()
			return nil, err
		}
		return p, nil
	}
	if err := p.loadHeader(); err != nil {
		file.Close()
		return nil, err
	}
	return p, nil
}

// initialise stamps a fresh header into both slots of a zero-length file.
func (p *Pager) initialise() error {
	p.hdr = header{state: State{PageCount: 1}}
	page := make([]byte, PageSize)
	for i := 0; i < slotCount; i++ {
		p.hdr.encode(page[slotOffset(i) : slotOffset(i)+slotSize])
	}
	if _, err := p.file.WriteAt(page, 0); err != nil {
		return fmt.Errorf("emberdb: write file header: %w", err)
	}
	if err := p.sync(); err != nil {
		return err
	}
	p.slot = slotCount - 1
	return nil
}

// loadHeader picks the live header slot: of the slots that carry the magic and
// pass their checksum, the one with the highest LSN wins. A slot torn by a
// crash fails its checksum and is ignored.
func (p *Pager) loadHeader() error {
	page := make([]byte, PageSize)
	if _, err := io.ReadFull(p.file, page); err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
			return ErrNotDatabase
		}
		return fmt.Errorf("emberdb: read file header: %w", err)
	}
	var (
		best      header
		bestSlot  = -1
		anyMagic  bool
		firstFail error
	)
	for i := 0; i < slotCount; i++ {
		off := slotOffset(i)
		h, hasMagic, err := decodeHeader(page[off : off+slotSize])
		anyMagic = anyMagic || hasMagic
		if err != nil {
			if firstFail == nil && hasMagic {
				firstFail = err
			}
			continue
		}
		if bestSlot < 0 || h.lsn > best.lsn {
			best, bestSlot = h, i
		}
	}
	if bestSlot < 0 {
		if !anyMagic {
			return ErrNotDatabase
		}
		var verr *VersionError
		if errors.As(firstFail, &verr) {
			return firstFail
		}
		return ErrCorruptHeader
	}
	p.hdr, p.slot = best, bestSlot
	return nil
}

// State returns the allocator and metadata state the pager currently holds.
func (p *Pager) State() State {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.hdr.state
}

// LSN returns the log sequence number of the last checkpoint.
func (p *Pager) LSN() uint64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.hdr.lsn
}

// Path returns the path of the database file.
func (p *Pager) Path() string { return p.path }

// Read returns the current image of page id. The returned slice aliases the
// page cache and must not be modified; use a Batch to change a page.
func (p *Pager) Read(id PageID) ([]byte, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, ErrClosed
	}
	if uint32(id) >= p.hdr.state.PageCount {
		return nil, fmt.Errorf("%w: page %d of %d", ErrPageOutOfRange, id, p.hdr.state.PageCount)
	}
	return p.readLocked(id)
}

// readLocked loads a page from the cache, falling back to the file. A page
// past the physical end of the file reads as zeros: a crash can leave the file
// short of the page count recorded in a header the log is about to redo, and
// the redo will overwrite it.
func (p *Pager) readLocked(id PageID) ([]byte, error) {
	if data, ok := p.cache.get(id); ok {
		return data, nil
	}
	data := make([]byte, PageSize)
	if _, err := p.file.ReadAt(data, int64(id)*PageSize); err != nil {
		if !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, fmt.Errorf("emberdb: read page %d: %w", id, err)
		}
	}
	p.cache.put(id, data)
	return data, nil
}

// CachedPages reports how many pages the cache currently holds. It exists for
// tests and for the CLI's diagnostics.
func (p *Pager) CachedPages() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cache.len()
}

// Commit installs a batch's pages as the current database contents and adopts
// its allocator state. It writes the pages to the file but does not make them
// durable; durability is the write-ahead log's job until Checkpoint runs.
//
// The caller must have made the batch's page images durable in the log before
// calling Commit.
func (p *Pager) Commit(b *Batch) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return ErrClosed
	}
	for _, id := range b.dirtyIDs() {
		data := b.pages[id]
		if err := p.writePageLocked(id, data); err != nil {
			return err
		}
	}
	p.hdr.state = b.state
	return nil
}

// writePageLocked writes one page image through to the file and the cache.
func (p *Pager) writePageLocked(id PageID, data []byte) error {
	if len(data) != PageSize {
		return fmt.Errorf("emberdb: page %d image is %d bytes, want %d", id, len(data), PageSize)
	}
	if _, err := p.file.WriteAt(data, int64(id)*PageSize); err != nil {
		return fmt.Errorf("emberdb: write page %d: %w", id, err)
	}
	p.cache.put(id, data)
	return nil
}

// ApplyRecovered writes a page image replayed from the write-ahead log. It
// bypasses the page-count bound because recovery can legitimately restore
// pages the stale header does not yet know about.
func (p *Pager) ApplyRecovered(id PageID, data []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return ErrClosed
	}
	image := make([]byte, PageSize)
	copy(image, data)
	return p.writePageLocked(id, image)
}

// SetRecoveredState adopts allocator and metadata state read from a commit
// record during recovery.
func (p *Pager) SetRecoveredState(st State) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.hdr.state = st
}

// Checkpoint makes every page written so far durable and stamps lsn into the
// header, marking the log up to that point as no longer needed for recovery.
// It writes the header into the slot the live header did not come from, so a
// crash during the write leaves the previous header intact.
func (p *Pager) Checkpoint(lsn uint64) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return ErrClosed
	}
	return p.checkpointLocked(lsn)
}

func (p *Pager) checkpointLocked(lsn uint64) error {
	if err := p.sync(); err != nil {
		return err
	}
	next := (p.slot + 1) % slotCount
	p.hdr.lsn = lsn
	buf := make([]byte, slotSize)
	p.hdr.encode(buf)
	if _, err := p.file.WriteAt(buf, int64(slotOffset(next))); err != nil {
		return fmt.Errorf("emberdb: write file header: %w", err)
	}
	if err := p.sync(); err != nil {
		return err
	}
	p.slot = next
	// Page 0 lives in the cache like any other page; drop it so the next
	// read picks up the slot that was just written.
	p.cache.remove(HeaderPage)
	return nil
}

// sync flushes the operating system's buffers for the database file unless
// syncing was disabled in Options.
func (p *Pager) sync() error {
	if p.noSync {
		return nil
	}
	if err := p.file.Sync(); err != nil {
		return fmt.Errorf("emberdb: sync %s: %w", p.path, err)
	}
	return nil
}

// Close releases the database file. It does not checkpoint; the engine does
// that first so that a clean shutdown leaves no log behind.
func (p *Pager) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil
	}
	p.closed = true
	if err := p.file.Close(); err != nil {
		return fmt.Errorf("emberdb: close %s: %w", p.path, err)
	}
	return nil
}

// Batch accumulates the page changes of a single write transaction.
//
// Pages are copied on write into a private overlay, so readers keep seeing the
// committed image until Commit installs the batch. Discarding a batch is
// therefore a complete rollback: nothing the transaction touched ever reached
// the cache or the file.
type Batch struct {
	p     *Pager
	pages map[PageID][]byte
	state State
	base  uint32 // page count when the batch began
}

// Begin starts a batch against the pager's current state. Only one write
// transaction may be open at a time; the engine enforces that.
func (p *Pager) Begin() *Batch {
	st := p.State()
	return &Batch{
		p:     p,
		pages: make(map[PageID][]byte),
		state: st,
		base:  st.PageCount,
	}
}

// State returns the allocator and metadata state the batch would install.
func (b *Batch) State() State { return b.state }

// SetMeta replaces the engine-owned metadata region.
func (b *Batch) SetMeta(meta [MetaSize]byte) { b.state.Meta = meta }

// Meta returns the engine-owned metadata region.
func (b *Batch) Meta() [MetaSize]byte { return b.state.Meta }

// Read returns the image of page id as this transaction sees it: its own
// uncommitted write if it has one, otherwise the committed image.
func (b *Batch) Read(id PageID) ([]byte, error) {
	if data, ok := b.pages[id]; ok {
		return data, nil
	}
	if uint32(id) >= b.state.PageCount {
		return nil, fmt.Errorf("%w: page %d of %d", ErrPageOutOfRange, id, b.state.PageCount)
	}
	if uint32(id) >= b.base {
		// Allocated by this transaction by extending the file, so it has
		// no committed image; Alloc always seeds one, so reaching here
		// means the caller invented a page id.
		return nil, fmt.Errorf("%w: page %d was never allocated", ErrPageOutOfRange, id)
	}
	b.p.mu.Lock()
	defer b.p.mu.Unlock()
	if b.p.closed {
		return nil, ErrClosed
	}
	return b.p.readLocked(id)
}

// Writable returns a mutable image of page id, copying the committed image
// into the batch overlay on first use. The returned slice stays valid, and
// stays private to this transaction, until the batch is committed or dropped.
func (b *Batch) Writable(id PageID) ([]byte, error) {
	if data, ok := b.pages[id]; ok {
		return data, nil
	}
	src, err := b.Read(id)
	if err != nil {
		return nil, err
	}
	dst := make([]byte, PageSize)
	copy(dst, src)
	b.pages[id] = dst
	return dst, nil
}

// Alloc reserves a page, reusing the head of the free list when one is
// available and extending the file otherwise. The returned image is zeroed and
// already part of the batch.
func (b *Batch) Alloc() (PageID, []byte, error) {
	if b.state.FreeHead != 0 {
		id := b.state.FreeHead
		node, err := b.Read(id)
		if err != nil {
			return 0, nil, fmt.Errorf("emberdb: read free list node %d: %w", id, err)
		}
		next := PageID(binary.LittleEndian.Uint32(node))
		b.state.FreeHead = next
		b.state.FreeCount--
		page := make([]byte, PageSize)
		b.pages[id] = page
		return id, page, nil
	}
	id := PageID(b.state.PageCount)
	b.state.PageCount++
	page := make([]byte, PageSize)
	b.pages[id] = page
	return id, page, nil
}

// Free returns a page to the free list so a later Alloc can reuse it. The page
// becomes a free-list node whose first four bytes link to the previous head.
func (b *Batch) Free(id PageID) error {
	if id == HeaderPage {
		return errors.New("emberdb: refusing to free the file header page")
	}
	if uint32(id) >= b.state.PageCount {
		return fmt.Errorf("%w: cannot free page %d of %d", ErrPageOutOfRange, id, b.state.PageCount)
	}
	page := make([]byte, PageSize)
	binary.LittleEndian.PutUint32(page, uint32(b.state.FreeHead))
	b.pages[id] = page
	b.state.FreeHead = id
	b.state.FreeCount++
	return nil
}

// Dirty reports how many pages the batch would write.
func (b *Batch) Dirty() int { return len(b.pages) }

// dirtyIDs returns the batch's page ids in ascending order, which keeps the
// file writes and the log records in a deterministic order.
func (b *Batch) dirtyIDs() []PageID {
	ids := make([]PageID, 0, len(b.pages))
	for id := range b.pages {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

// Images calls fn for every page the batch would write, in ascending page
// order. It is how the write-ahead log gets at the images it must log before
// the batch may be committed.
func (b *Batch) Images(fn func(id PageID, data []byte) error) error {
	for _, id := range b.dirtyIDs() {
		if err := fn(id, b.pages[id]); err != nil {
			return err
		}
	}
	return nil
}
