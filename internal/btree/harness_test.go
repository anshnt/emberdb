package btree

import (
	"bytes"
	"fmt"
	"path/filepath"
	"sort"
	"testing"

	"github.com/anshnt/emberdb/internal/pager"
)

// harness owns a pager and the batch the tree under test is modified through.
type harness struct {
	t     *testing.T
	pager *pager.Pager
	batch *pager.Batch
	root  pager.PageID
	lsn   uint64
}

// newHarness creates an empty tree on a temporary file.
func newHarness(t *testing.T) *harness {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tree.ember")
	p, err := pager.Open(path, pager.Options{NoSync: true})
	if err != nil {
		t.Fatalf("pager.Open: %v", err)
	}
	t.Cleanup(func() { p.Close() })
	h := &harness{t: t, pager: p, batch: p.Begin()}
	root, err := Create(h.batch)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	h.root = root
	return h
}

// put inserts an entry, failing the test on error.
func (h *harness) put(key, value string) {
	h.t.Helper()
	root, err := Put(h.batch, h.root, []byte(key), []byte(value))
	if err != nil {
		h.t.Fatalf("Put(%q): %v", key, err)
	}
	h.root = root
}

// del removes an entry and reports whether it was there.
func (h *harness) del(key string) bool {
	h.t.Helper()
	root, removed, err := Delete(h.batch, h.root, []byte(key))
	if err != nil {
		h.t.Fatalf("Delete(%q): %v", key, err)
	}
	h.root = root
	return removed
}

// get returns the value stored under key.
func (h *harness) get(key string) (string, bool) {
	h.t.Helper()
	value, found, err := Get(h.batch, h.root, []byte(key))
	if err != nil {
		h.t.Fatalf("Get(%q): %v", key, err)
	}
	return string(value), found
}

// commit publishes the batch and starts a new one, so later operations run
// against pages that have been through the pager.
func (h *harness) commit() {
	h.t.Helper()
	if err := h.pager.Commit(h.batch, h.lsn+1, false); err != nil {
		h.t.Fatalf("Commit: %v", err)
	}
	h.lsn++
	if err := h.pager.Checkpoint(h.lsn); err != nil {
		h.t.Fatalf("Checkpoint: %v", err)
	}
	h.batch = h.pager.Begin()
}

// scan returns every key in the tree, in order, by walking the leaf chain.
func (h *harness) scan() []string {
	h.t.Helper()
	c, err := First(h.batch, h.root)
	if err != nil {
		h.t.Fatalf("First: %v", err)
	}
	var keys []string
	for c.Next() {
		keys = append(keys, string(c.Key()))
	}
	if err := c.Err(); err != nil {
		h.t.Fatalf("scan: %v", err)
	}
	return keys
}

// scanBackwards returns every key in the tree in descending order, by walking
// the leaf chain's previous pointers.
func (h *harness) scanBackwards() []string {
	h.t.Helper()
	c, err := Last(h.batch, h.root)
	if err != nil {
		h.t.Fatalf("Last: %v", err)
	}
	var keys []string
	for c.Prev() {
		keys = append(keys, string(c.Key()))
	}
	if err := c.Err(); err != nil {
		h.t.Fatalf("backward scan: %v", err)
	}
	return keys
}

// validate walks the tree and checks every structural invariant, returning the
// keys it found in traversal order.
//
// It is deliberately paranoid: a B+tree that is subtly wrong still answers most
// point lookups correctly, so the tests lean on this rather than on Get.
func (h *harness) validate() []string {
	h.t.Helper()
	seen := make(map[pager.PageID]bool)
	var keys []string
	depth := -1
	var walk func(id pager.PageID, level int, low, high []byte)
	walk = func(id pager.PageID, level int, low, high []byte) {
		if seen[id] {
			h.t.Fatalf("page %d appears twice in the tree", id)
		}
		seen[id] = true
		n, err := readNode(h.batch, id)
		if err != nil {
			h.t.Fatalf("readNode(%d): %v", id, err)
		}
		var previous []byte
		for i := 0; i < n.count(); i++ {
			key, err := cellKey(n.kind(), n.cell(i))
			if err != nil {
				h.t.Fatalf("page %d cell %d: %v", id, i, err)
			}
			if previous != nil && bytes.Compare(previous, key) >= 0 {
				h.t.Fatalf("page %d: key %q follows %q, out of order", id, key, previous)
			}
			if low != nil && bytes.Compare(key, low) < 0 {
				h.t.Fatalf("page %d: key %q is below its separator bound %q", id, key, low)
			}
			if high != nil && bytes.Compare(key, high) >= 0 {
				h.t.Fatalf("page %d: key %q is at or above its separator bound %q", id, key, high)
			}
			previous = append([]byte(nil), key...)
		}
		if n.isLeaf() {
			if depth == -1 {
				depth = level
			} else if depth != level {
				h.t.Fatalf("leaf %d is at depth %d, an earlier leaf was at %d", id, level, depth)
			}
			for i := 0; i < n.count(); i++ {
				key, err := leafKey(n.cell(i))
				if err != nil {
					h.t.Fatalf("leaf %d cell %d: %v", id, i, err)
				}
				keys = append(keys, string(key))
			}
			return
		}
		if n.count() == 0 && id != h.root {
			h.t.Fatalf("internal page %d has no separators", id)
		}
		bound := low
		for i := 0; i < n.count(); i++ {
			child, err := internalChild(n.cell(i))
			if err != nil {
				h.t.Fatalf("page %d cell %d: %v", id, i, err)
			}
			key, err := internalKey(n.cell(i))
			if err != nil {
				h.t.Fatalf("page %d cell %d: %v", id, i, err)
			}
			separator := append([]byte(nil), key...)
			walk(child, level+1, bound, separator)
			bound = separator
		}
		walk(n.rightmost(), level+1, bound, high)
	}
	walk(h.root, 0, nil, nil)

	if scanned := h.scan(); !equalStrings(scanned, keys) {
		h.t.Fatalf("leaf chain yields %d keys, in-order traversal yields %d; the sibling pointers disagree with the tree", len(scanned), len(keys))
	}
	reversed := h.scanBackwards()
	forwards := make([]string, len(reversed))
	for i, k := range reversed {
		forwards[len(reversed)-1-i] = k
	}
	if !equalStrings(forwards, keys) {
		h.t.Fatalf("backward chain yields %d keys, forward traversal yields %d; the previous pointers are wrong", len(reversed), len(keys))
	}
	if !sort.StringsAreSorted(keys) {
		h.t.Fatal("keys are not globally sorted")
	}
	return keys
}

// equalStrings compares two string slices, treating nil and empty as equal.
func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// key formats an integer as a fixed-width sortable key.
func key(i int) string { return fmt.Sprintf("key%08d", i) }
