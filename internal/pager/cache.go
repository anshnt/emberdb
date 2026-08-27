package pager

import (
	"container/list"
	"sort"
)

// cache holds page images in two tiers.
//
// Pinned pages are committed but not yet checkpointed: the log has them but
// the database file does not, so they are the only copy a reader can reach and
// they must not be evicted. Checkpointing writes them through to the file and
// demotes them into the LRU tier, which is a plain fixed-capacity cache of
// pages that could be reloaded from the file at any time.
//
// Cached buffers are immutable: a writer never mutates a page in place, it
// copies the page into its transaction overlay and the new image replaces the
// cached one at commit. That makes eviction safe without reference counting.
// A caller holding a slice for an evicted page keeps reading the snapshot it
// already had; the entry is simply reloaded from the file on the next lookup.
//
// cache is not safe for concurrent use. The Pager serialises access to it.
type cache struct {
	capacity int
	pinned   map[PageID]*pageVersion
	entries  map[PageID]*list.Element
	order    *list.List // front is most recently used
	// versioned counts the pages currently holding a superseded image. It
	// exists so that pruning, which runs at the end of every transaction,
	// costs nothing at all in the common case where no reader ever
	// overlapped a writer.
	versioned int
}

// cacheEntry is the value stored in each list element.
type cacheEntry struct {
	id    PageID
	chain *pageVersion
}

// pageVersion is one image of a page together with the transaction that
// installed it, linked to the images it superseded.
//
// A reader whose snapshot predates a commit has to keep seeing the page as it
// was, or a scan in progress could follow a pointer the writer has since
// rearranged. Rather than lock readers out while a commit publishes, the cache
// keeps the superseded image alive until no snapshot can still want it. An
// image loaded from the file carries since = 0, which is older than any
// transaction.
type pageVersion struct {
	image []byte
	since uint64
	older *pageVersion
}

// at returns the newest image visible to a snapshot bounded by upper.
func (v *pageVersion) at(upper uint64) []byte {
	for current := v; current != nil; current = current.older {
		if current.since <= upper {
			return current.image
		}
	}
	// Every retained image is newer than the snapshot. The caller asked for
	// a version the cache no longer holds, which prune only allows once no
	// transaction can be reading it.
	return v.oldest().image
}

// oldest returns the last image in the chain.
func (v *pageVersion) oldest() *pageVersion {
	current := v
	for current.older != nil {
		current = current.older
	}
	return current
}

// versioned reports whether the chain holds superseded images, which makes the
// page ineligible for eviction: the file cannot supply them again.
func (v *pageVersion) versioned() bool { return v.older != nil }

// prune drops images no snapshot at or above oldest can still need: everything
// older than the newest image the oldest snapshot would pick.
func (v *pageVersion) prune(oldest uint64) {
	for current := v; current != nil; current = current.older {
		if current.since <= oldest {
			current.older = nil
			return
		}
	}
}

// newCache returns an empty cache holding at most capacity pages. A capacity
// below one is raised to one so that the cache always retains the page it was
// last asked for.
func newCache(capacity int) *cache {
	if capacity < 1 {
		capacity = 1
	}
	return &cache{
		capacity: capacity,
		pinned:   make(map[PageID]*pageVersion),
		entries:  make(map[PageID]*list.Element, capacity),
		order:    list.New(),
	}
}

// chain returns the version chain for id, preferring the pinned tier, and
// marks an LRU hit as most recently used.
func (c *cache) chain(id PageID) (*pageVersion, bool) {
	if v, ok := c.pinned[id]; ok {
		return v, true
	}
	el, ok := c.entries[id]
	if !ok {
		return nil, false
	}
	c.order.MoveToFront(el)
	return el.Value.(*cacheEntry).chain, true
}

// put installs a version chain for id in the LRU tier, evicting the least
// recently used evictable page when the cache is over capacity. A pinned page
// keeps its pinned chain: it is newer than anything the file can supply.
func (c *cache) put(id PageID, chain *pageVersion) {
	if _, ok := c.pinned[id]; ok {
		return
	}
	if el, ok := c.entries[id]; ok {
		el.Value.(*cacheEntry).chain = chain
		c.order.MoveToFront(el)
		return
	}
	c.entries[id] = c.order.PushFront(&cacheEntry{id: id, chain: chain})
	c.evict()
}

// evict trims the LRU tier back to capacity, skipping pages that still hold
// superseded images for an open snapshot.
func (c *cache) evict() {
	for el := c.order.Back(); el != nil && c.order.Len() > c.capacity; {
		previous := el.Prev()
		entry := el.Value.(*cacheEntry)
		if !entry.chain.versioned() {
			c.order.Remove(el)
			delete(c.entries, entry.id)
		}
		el = previous
	}
}

// pruneVersions drops superseded images no snapshot at or above oldest needs,
// then reclaims the space that frees up.
//
// It returns immediately when no page holds a superseded image, which is the
// usual case: only a commit that overlapped an older transaction creates one.
// Without that check this would walk the whole cache at the end of every
// transaction, which shows up as real time in a read-only workload.
func (c *cache) pruneVersions(oldest uint64) {
	if c.versioned == 0 {
		return
	}
	c.versioned = 0
	for _, v := range c.pinned {
		v.prune(oldest)
		if v.versioned() {
			c.versioned++
		}
	}
	for el := c.order.Front(); el != nil; el = el.Next() {
		chain := el.Value.(*cacheEntry).chain
		chain.prune(oldest)
		if chain.versioned() {
			c.versioned++
		}
	}
	c.evict()
}

// remove drops id from the LRU tier if present. Pinned pages are not
// removable: dropping one would lose the only copy of a committed page.
func (c *cache) remove(id PageID) {
	el, ok := c.entries[id]
	if !ok {
		return
	}
	if el.Value.(*cacheEntry).chain.versioned() {
		c.versioned--
	}
	c.order.Remove(el)
	delete(c.entries, id)
}

// pin installs image as the current version of id and marks the page
// un-evictable until the next checkpoint writes it through to the file. When
// retain is set, the image it replaces is kept for snapshots that predate
// since.
func (c *cache) pin(id PageID, image []byte, since uint64, retain bool) {
	version := &pageVersion{image: image, since: since}
	if retain {
		if previous, ok := c.chain(id); ok {
			version.older = previous
		}
	}
	if previous, ok := c.pinned[id]; ok && previous.versioned() {
		c.versioned--
	}
	c.remove(id)
	if version.versioned() {
		c.versioned++
	}
	c.pinned[id] = version
}

// pinnedIDs returns the pinned page ids in ascending order so that a
// checkpoint writes them out in file order.
func (c *cache) pinnedIDs() []PageID {
	ids := make([]PageID, 0, len(c.pinned))
	for id := range c.pinned {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

// unpinAll demotes every pinned page into the LRU tier. The caller must have
// written the pages to the file first.
func (c *cache) unpinAll() {
	for _, id := range c.pinnedIDs() {
		chain := c.pinned[id]
		delete(c.pinned, id)
		c.put(id, chain)
	}
}

// len returns the number of pages currently cached in either tier.
func (c *cache) len() int { return c.order.Len() + len(c.pinned) }

// pinnedLen returns the number of committed pages awaiting a checkpoint.
func (c *cache) pinnedLen() int { return len(c.pinned) }
