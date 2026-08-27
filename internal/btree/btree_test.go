package btree

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"testing"

	"github.com/anshnt/emberdb/internal/pager"
)

func TestPutGetDelete(t *testing.T) {
	h := newHarness(t)
	h.put("apple", "red")
	h.put("banana", "yellow")
	h.put("cherry", "dark")

	for _, c := range []struct{ key, want string }{
		{"apple", "red"}, {"banana", "yellow"}, {"cherry", "dark"},
	} {
		got, found := h.get(c.key)
		if !found || got != c.want {
			t.Fatalf("get(%q) = (%q, %v), want (%q, true)", c.key, got, found, c.want)
		}
	}
	if _, found := h.get("durian"); found {
		t.Fatal("get of a missing key reported found")
	}
	if !h.del("banana") {
		t.Fatal("delete of an existing key reported not found")
	}
	if h.del("banana") {
		t.Fatal("deleting the same key twice reported found twice")
	}
	if _, found := h.get("banana"); found {
		t.Fatal("deleted key is still readable")
	}
	if got := h.validate(); !equalStrings(got, []string{"apple", "cherry"}) {
		t.Fatalf("tree holds %v", got)
	}
}

func TestReplaceValueKeepsOneEntry(t *testing.T) {
	h := newHarness(t)
	h.put("k", "first")
	h.put("k", "second")
	h.put("k", "third")
	got, found := h.get("k")
	if !found || got != "third" {
		t.Fatalf("get = (%q, %v), want (\"third\", true)", got, found)
	}
	if keys := h.validate(); len(keys) != 1 {
		t.Fatalf("replacing a value left %d entries: %v", len(keys), keys)
	}
}

func TestSplitsBuildInternalLevels(t *testing.T) {
	h := newHarness(t)
	const n = 5000
	for i := 0; i < n; i++ {
		h.put(key(i), fmt.Sprintf("value-%d", i))
	}
	keys := h.validate()
	if len(keys) != n {
		t.Fatalf("tree holds %d keys, want %d", len(keys), n)
	}
	for i := 0; i < n; i++ {
		if keys[i] != key(i) {
			t.Fatalf("key %d is %q, want %q", i, keys[i], key(i))
		}
	}
	// A tree this size cannot be one page, so splitting must have produced
	// a root that is no longer a leaf.
	root, err := readNode(h.batch, h.root)
	if err != nil {
		t.Fatalf("readNode: %v", err)
	}
	if root.isLeaf() {
		t.Fatal("root is still a leaf after 5000 inserts")
	}
}

func TestDeletingEverythingLeavesAnEmptyLeafRoot(t *testing.T) {
	h := newHarness(t)
	const n = 3000
	for i := 0; i < n; i++ {
		h.put(key(i), strings.Repeat("v", 40))
	}
	h.validate()
	for i := 0; i < n; i++ {
		if !h.del(key(i)) {
			t.Fatalf("delete(%q) reported not found", key(i))
		}
	}
	if keys := h.validate(); len(keys) != 0 {
		t.Fatalf("tree still holds %d keys", len(keys))
	}
	root, err := readNode(h.batch, h.root)
	if err != nil {
		t.Fatalf("readNode: %v", err)
	}
	if !root.isLeaf() {
		t.Fatalf("root is %s after emptying the tree, want a leaf", root.kind())
	}
	if root.count() != 0 {
		t.Fatalf("root holds %d cells, want 0", root.count())
	}
	if st := h.batch.State(); st.FreeCount == 0 {
		t.Fatal("emptying a 3000-entry tree freed no pages")
	}
}

func TestDeletedPagesAreReused(t *testing.T) {
	h := newHarness(t)
	const n = 2000
	for i := 0; i < n; i++ {
		h.put(key(i), strings.Repeat("v", 60))
	}
	h.commit()
	peak := h.batch.State().PageCount
	for i := 0; i < n; i++ {
		h.del(key(i))
	}
	h.commit()
	freed := h.batch.State().FreeCount
	if freed == 0 {
		t.Fatal("no pages were freed")
	}
	// Refilling should come out of the free list, not out of new file space.
	for i := 0; i < n; i++ {
		h.put(key(i), strings.Repeat("v", 60))
	}
	h.commit()
	if grown := h.batch.State().PageCount; grown > peak {
		t.Fatalf("refilling grew the file from %d to %d pages despite %d free pages", peak, grown, freed)
	}
	h.validate()
}

func TestRangeScanUsesLeafSiblings(t *testing.T) {
	h := newHarness(t)
	const n = 2000
	for i := 0; i < n; i++ {
		h.put(key(i), fmt.Sprintf("%d", i))
	}
	c, err := Seek(h.batch, h.root, []byte(key(750)))
	if err != nil {
		t.Fatalf("Seek: %v", err)
	}
	var got []string
	for c.Next() {
		if string(c.Key()) > key(1250) {
			break
		}
		value, err := c.Value()
		if err != nil {
			t.Fatalf("Value: %v", err)
		}
		got = append(got, string(value))
	}
	if err := c.Err(); err != nil {
		t.Fatalf("cursor: %v", err)
	}
	if len(got) != 501 {
		t.Fatalf("scan returned %d rows, want 501", len(got))
	}
	for i, v := range got {
		if v != fmt.Sprintf("%d", 750+i) {
			t.Fatalf("row %d = %q, want %d", i, v, 750+i)
		}
	}
}

func TestSeekLandsOnTheFirstKeyAtOrAfterTheTarget(t *testing.T) {
	h := newHarness(t)
	for i := 0; i < 200; i++ {
		h.put(key(i*10), "x")
	}
	cases := []struct {
		seek string
		want string
	}{
		{"", key(0)},
		{key(0), key(0)},
		{key(5), key(10)},
		{key(1990), key(1990)},
		{key(1991), ""},
		{"zzz", ""},
	}
	for _, c := range cases {
		cur, err := Seek(h.batch, h.root, []byte(c.seek))
		if err != nil {
			t.Fatalf("Seek(%q): %v", c.seek, err)
		}
		got := ""
		if cur.Next() {
			got = string(cur.Key())
		}
		if err := cur.Err(); err != nil {
			t.Fatalf("Seek(%q): %v", c.seek, err)
		}
		if got != c.want {
			t.Errorf("Seek(%q) landed on %q, want %q", c.seek, got, c.want)
		}
	}
}

func TestOverflowValuesRoundTrip(t *testing.T) {
	h := newHarness(t)
	sizes := []int{1, maxInlineCell, maxInlineCell + 1, pager.PageSize, 5 * pager.PageSize, 100_000}
	for i, size := range sizes {
		h.put(key(i), strings.Repeat(string(rune('a'+i)), size))
	}
	for i, size := range sizes {
		got, found := h.get(key(i))
		if !found {
			t.Fatalf("value of size %d went missing", size)
		}
		if len(got) != size {
			t.Fatalf("value of size %d came back as %d bytes", size, len(got))
		}
		if got != strings.Repeat(string(rune('a'+i)), size) {
			t.Fatalf("value of size %d does not round-trip", size)
		}
	}
	h.validate()
}

func TestOverflowChainsAreFreedOnDeleteAndReplace(t *testing.T) {
	h := newHarness(t)
	big := strings.Repeat("x", 50_000)
	h.put("k", big)
	h.commit()
	withOverflow := h.batch.State().PageCount

	h.put("k", "small")
	h.commit()
	if got, _ := h.get("k"); got != "small" {
		t.Fatalf("replacement value is %q", got)
	}
	freedByReplace := h.batch.State().FreeCount
	if freedByReplace < 12 {
		t.Fatalf("replacing a 50 KB value freed only %d pages", freedByReplace)
	}

	h.put("k", big)
	h.commit()
	if grown := h.batch.State().PageCount; grown > withOverflow {
		t.Fatalf("rewriting the large value grew the file from %d to %d pages", withOverflow, grown)
	}
	h.del("k")
	h.commit()
	if h.batch.State().FreeCount < freedByReplace {
		t.Fatal("deleting the entry did not release its overflow chain")
	}
	h.validate()
}

func TestKeysThatArePrefixesOfEachOther(t *testing.T) {
	h := newHarness(t)
	keys := []string{"", "a", "aa", "aaa", "ab", "b", "ba", "\x00", "\x00\x00", "\xff", "\xff\xff"}
	for i, k := range keys {
		h.put(k, fmt.Sprintf("%d", i))
	}
	sorted := append([]string(nil), keys...)
	sort.Strings(sorted)
	if got := h.validate(); !equalStrings(got, sorted) {
		t.Fatalf("tree holds %q, want %q", got, sorted)
	}
	for i, k := range keys {
		if got, found := h.get(k); !found || got != fmt.Sprintf("%d", i) {
			t.Fatalf("get(%q) = (%q, %v)", k, got, found)
		}
	}
	if !h.del("a") {
		t.Fatal("delete(\"a\") reported not found")
	}
	if _, found := h.get("aa"); !found {
		t.Fatal("deleting \"a\" also removed \"aa\"")
	}
}

func TestEmptyKeyAndEmptyValue(t *testing.T) {
	h := newHarness(t)
	h.put("", "")
	got, found := h.get("")
	if !found || got != "" {
		t.Fatalf("get(\"\") = (%q, %v), want (\"\", true)", got, found)
	}
	if keys := h.validate(); len(keys) != 1 {
		t.Fatalf("tree holds %d keys", len(keys))
	}
}

func TestKeyAtAndAboveTheLimit(t *testing.T) {
	h := newHarness(t)
	atLimit := strings.Repeat("k", MaxKeySize)
	if _, err := Put(h.batch, h.root, []byte(atLimit), []byte("fits")); err != nil {
		t.Fatalf("a key of exactly MaxKeySize was rejected: %v", err)
	}
	tooLong := strings.Repeat("k", MaxKeySize+1)
	if _, err := Put(h.batch, h.root, []byte(tooLong), []byte("nope")); !errors.Is(err, ErrKeyTooLarge) {
		t.Fatalf("Put with an oversized key = %v, want ErrKeyTooLarge", err)
	}
	// Many maximum-length keys still have to split cleanly.
	for i := 0; i < 400; i++ {
		h.put(fmt.Sprintf("%s%04d", strings.Repeat("p", MaxKeySize-4), i), "v")
	}
	h.validate()
}

func TestEmptyTreeScansAndSeeksCleanly(t *testing.T) {
	h := newHarness(t)
	if keys := h.validate(); len(keys) != 0 {
		t.Fatalf("a fresh tree holds %d keys", len(keys))
	}
	c, err := Seek(h.batch, h.root, []byte("anything"))
	if err != nil {
		t.Fatalf("Seek: %v", err)
	}
	if c.Next() {
		t.Fatal("Next on an empty tree returned an entry")
	}
	if _, err := c.Value(); err == nil {
		t.Fatal("Value on an unpositioned cursor should fail")
	}
	if h.del("anything") {
		t.Fatal("delete on an empty tree reported found")
	}
}

func TestTreeSurvivesCommitAndReopen(t *testing.T) {
	h := newHarness(t)
	const n = 4000
	for i := 0; i < n; i++ {
		h.put(key(i), fmt.Sprintf("value-%d", i))
		if i%500 == 0 {
			h.commit()
		}
	}
	h.commit()
	// Reading through the pager, rather than the batch, exercises the path
	// a read-only transaction takes.
	c, err := First(h.pager, h.root)
	if err != nil {
		t.Fatalf("First: %v", err)
	}
	count := 0
	for c.Next() {
		if string(c.Key()) != key(count) {
			t.Fatalf("entry %d is %q", count, c.Key())
		}
		count++
	}
	if err := c.Err(); err != nil {
		t.Fatalf("cursor: %v", err)
	}
	if count != n {
		t.Fatalf("scanned %d entries, want %d", count, n)
	}
}

// TestAgainstMap drives randomised operation sequences through the tree and a
// Go map at the same time, checking after every batch that the two agree on
// every key and that the tree's structure is still sound.
func TestAgainstMap(t *testing.T) {
	shapes := []struct {
		name       string
		keySpace   int
		valueSize  func(*rand.Rand) int
		deleteOdds int
	}{
		{"small-keyspace-heavy-churn", 64, func(*rand.Rand) int { return 20 }, 50},
		{"wide-keyspace", 20000, func(*rand.Rand) int { return 30 }, 30},
		{"variable-values", 500, func(r *rand.Rand) int { return r.Intn(600) }, 35},
		{"overflow-values", 120, func(r *rand.Rand) int { return r.Intn(9000) }, 40},
	}
	for _, shape := range shapes {
		for seed := int64(1); seed <= 3; seed++ {
			t.Run(fmt.Sprintf("%s/seed%d", shape.name, seed), func(t *testing.T) {
				r := rand.New(rand.NewSource(seed))
				h := newHarness(t)
				model := make(map[string]string)

				const operations = 4000
				for i := 0; i < operations; i++ {
					k := key(r.Intn(shape.keySpace))
					if r.Intn(100) < shape.deleteOdds {
						_, existed := model[k]
						if got := h.del(k); got != existed {
							t.Fatalf("delete(%q) = %v, map says %v", k, got, existed)
						}
						delete(model, k)
					} else {
						v := strings.Repeat(string(rune('a'+r.Intn(26))), shape.valueSize(r))
						h.put(k, v)
						model[k] = v
					}
					if i%997 == 0 {
						h.commit()
					}
				}

				keys := h.validate()
				want := make([]string, 0, len(model))
				for k := range model {
					want = append(want, k)
				}
				sort.Strings(want)
				if !equalStrings(keys, want) {
					t.Fatalf("tree holds %d keys, map holds %d", len(keys), len(want))
				}
				for k, v := range model {
					got, found := h.get(k)
					if !found {
						t.Fatalf("key %q is in the map but not the tree", k)
					}
					if got != v {
						t.Fatalf("key %q: tree has %d bytes, map has %d", k, len(got), len(v))
					}
				}
				// Keys the map does not have must not be in the tree.
				for i := 0; i < 200; i++ {
					k := key(r.Intn(shape.keySpace))
					_, want := model[k]
					if _, got := h.get(k); got != want {
						t.Fatalf("key %q: tree says %v, map says %v", k, got, want)
					}
				}
			})
		}
	}
}

// TestAgainstMapWithReverseAndClusteredOrders exercises insertion patterns that
// stress splitting differently from random keys.
func TestAgainstMapWithReverseAndClusteredOrders(t *testing.T) {
	orders := map[string]func(i, n int) int{
		"ascending":  func(i, n int) int { return i },
		"descending": func(i, n int) int { return n - 1 - i },
		"outside-in": func(i, n int) int {
			if i%2 == 0 {
				return i / 2
			}
			return n - 1 - i/2
		},
	}
	for name, order := range orders {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			const n = 4000
			model := make(map[string]bool, n)
			for i := 0; i < n; i++ {
				k := key(order(i, n))
				h.put(k, fmt.Sprintf("v%d", i))
				model[k] = true
			}
			if keys := h.validate(); len(keys) != len(model) {
				t.Fatalf("tree holds %d keys, want %d", len(keys), len(model))
			}
			// Delete in the same order, which is the worst case for merges.
			for i := 0; i < n; i++ {
				k := key(order(i, n))
				if !h.del(k) {
					t.Fatalf("delete(%q) reported not found", k)
				}
				if i%409 == 0 {
					h.validate()
				}
			}
			if keys := h.validate(); len(keys) != 0 {
				t.Fatalf("tree still holds %d keys", len(keys))
			}
		})
	}
}

func TestReverseScanFollowsPreviousPointers(t *testing.T) {
	h := newHarness(t)
	const n = 2500
	for i := 0; i < n; i++ {
		h.put(key(i), fmt.Sprintf("%d", i))
	}
	c, err := Last(h.batch, h.root)
	if err != nil {
		t.Fatalf("Last: %v", err)
	}
	count := 0
	for c.Prev() {
		want := key(n - 1 - count)
		if got := string(c.Key()); got != want {
			t.Fatalf("backward entry %d is %q, want %q", count, got, want)
		}
		count++
	}
	if err := c.Err(); err != nil {
		t.Fatalf("cursor: %v", err)
	}
	if count != n {
		t.Fatalf("backward scan returned %d entries, want %d", count, n)
	}
}

// TestCorruptPagesAreReportedNotPanicked damages pages the way a failing disk
// would and checks the tree reports it. validate is constant time, so most of
// this has to be caught when a cell is decoded rather than when a page is
// read, and the point of the test is that it still is.
func TestCorruptPagesAreReportedNotPanicked(t *testing.T) {
	damage := map[string]func(page []byte){
		"unknown node kind": func(page []byte) {
			page[0] = 99
		},
		"impossible cell count": func(page []byte) {
			binary.LittleEndian.PutUint16(page[2:], 60000)
		},
		"slot pointing past the page": func(page []byte) {
			binary.LittleEndian.PutUint16(page[nodeHeaderSize:], 65535)
		},
		"slot pointing into the header": func(page []byte) {
			binary.LittleEndian.PutUint16(page[nodeHeaderSize:], 1)
		},
		"slots out of order": func(page []byte) {
			first := binary.LittleEndian.Uint16(page[nodeHeaderSize:])
			binary.LittleEndian.PutUint16(page[nodeHeaderSize+2:], first-1)
			binary.LittleEndian.PutUint16(page[nodeHeaderSize:], first-1)
		},
		"cell length beyond its extent": func(page []byte) {
			offset := binary.LittleEndian.Uint16(page[nodeHeaderSize:])
			page[offset+1] = 0x7f // a key length far larger than the cell
		},
	}
	for name, corrupt := range damage {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			for i := 0; i < 400; i++ {
				h.put(key(i), fmt.Sprintf("value-%d", i))
			}
			// Damage the leaf the scan will reach first.
			c, err := First(h.batch, h.root)
			if err != nil {
				t.Fatalf("First: %v", err)
			}
			page, err := h.batch.Writable(c.leaf.id)
			if err != nil {
				t.Fatalf("Writable: %v", err)
			}
			corrupt(page)

			// Whatever the damage, the tree must report an error or
			// simply not find things. It must never panic and never
			// read outside the page.
			failures := 0
			if _, _, err := Get(h.batch, h.root, []byte(key(0))); err != nil {
				failures++
			}
			cursor, err := First(h.batch, h.root)
			if err != nil {
				failures++
			} else {
				for cursor.Next() {
					_, _ = cursor.Value()
				}
				if cursor.Err() != nil {
					failures++
				}
			}
			if _, err := Put(h.batch, h.root, []byte(key(0)), []byte("x")); err != nil {
				failures++
			}
			if _, _, err := Delete(h.batch, h.root, []byte(key(0))); err != nil {
				failures++
			}
			if failures == 0 {
				t.Fatalf("damaged page went entirely unnoticed")
			}
		})
	}
}
