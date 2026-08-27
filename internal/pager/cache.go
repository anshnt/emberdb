package pager

import "container/list"

// cache is a fixed-capacity LRU of page images keyed by page id.
//
// Cached buffers are immutable: a writer never mutates a page in place, it
// copies the page into its transaction overlay and the new image replaces the
// cached one at commit. That makes eviction safe without reference counting.
// A caller holding a slice for an evicted page keeps reading the snapshot it
// already had, which is exactly the isolation the engine wants; the entry is
// simply reloaded from the file on the next lookup.
//
// cache is not safe for concurrent use. The Pager serialises access to it.
type cache struct {
	capacity int
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
		entries:  make(map[PageID]*list.Element, capacity),
		order:    list.New(),
	}
}

// get returns the cached image for id and marks it most recently used.
func (c *cache) get(id PageID) ([]byte, bool) {
	el, ok := c.entries[id]
	if !ok {
		return nil, false
	}
	c.order.MoveToFront(el)
	return el.Value.(*cacheEntry).data, true
}

// put installs data as the image for id, evicting the least recently used
// page when the cache is over capacity.
func (c *cache) put(id PageID, data []byte) {
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

// remove drops id from the cache if present.
func (c *cache) remove(id PageID) {
	el, ok := c.entries[id]
	if !ok {
		return
	}
	c.order.Remove(el)
	delete(c.entries, id)
}

// len returns the number of pages currently cached.
func (c *cache) len() int { return c.order.Len() }
