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
	pinned   map[PageID][]byte
	entries  map[PageID]*list.Element
	order    *list.List // front is most recently used
}

// cacheEntry is the value stored in each list element.
type cacheEntry struct {
	id   PageID
	data []byte
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
		pinned:   make(map[PageID][]byte),
		entries:  make(map[PageID]*list.Element, capacity),
		order:    list.New(),
	}
}

// get returns the cached image for id, preferring the pinned tier, and marks
// an LRU hit as most recently used.
func (c *cache) get(id PageID) ([]byte, bool) {
	if data, ok := c.pinned[id]; ok {
		return data, true
	}
	el, ok := c.entries[id]
	if !ok {
		return nil, false
	}
	c.order.MoveToFront(el)
	return el.Value.(*cacheEntry).data, true
}

// put installs data as the image for id in the LRU tier, evicting the least
// recently used page when the cache is over capacity. A pinned page keeps its
// pinned image: that image is newer than anything the file can supply.
func (c *cache) put(id PageID, data []byte) {
	if _, ok := c.pinned[id]; ok {
		return
	}
	if el, ok := c.entries[id]; ok {
		el.Value.(*cacheEntry).data = data
		c.order.MoveToFront(el)
		return
	}
	c.entries[id] = c.order.PushFront(&cacheEntry{id: id, data: data})
	for c.order.Len() > c.capacity {
		oldest := c.order.Back()
		if oldest == nil {
			return
		}
		c.order.Remove(oldest)
		delete(c.entries, oldest.Value.(*cacheEntry).id)
	}
}

// remove drops id from the LRU tier if present. Pinned pages are not
// removable: dropping one would lose the only copy of a committed page.
func (c *cache) remove(id PageID) {
	el, ok := c.entries[id]
	if !ok {
		return
	}
	c.order.Remove(el)
	delete(c.entries, id)
}

// pin installs data as the image for id and marks it un-evictable until the
// next checkpoint writes it through to the file.
func (c *cache) pin(id PageID, data []byte) {
	c.remove(id)
	c.pinned[id] = data
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
		data := c.pinned[id]
		delete(c.pinned, id)
		c.put(id, data)
	}
}

// len returns the number of pages currently cached in either tier.
func (c *cache) len() int { return c.order.Len() + len(c.pinned) }

// pinnedLen returns the number of committed pages awaiting a checkpoint.
func (c *cache) pinnedLen() int { return len(c.pinned) }
