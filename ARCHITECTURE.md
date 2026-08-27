# How emberdb works

Five layers, each of which only knows about the one below it.

```
        SQL          lexer, parser, evaluator, planner
         │
       store         catalog, row versions, transactions, indexes
         │
       btree         ordered map over variable-length keys and values
         │
    ┌────┴────┐
  pager      wal      pages, cache, allocator │ durability
    └────┬────┘
       file
```

`internal/value` sits beside all of them: it defines the five storage classes
and the two encodings — a compact one for rows, and an order-preserving one
for index keys.

## The pager

`internal/pager` presents the file as an array of 4 KiB pages.

**The header is double-buffered.** Page 0 holds two 64-byte header slots, 2 KiB
apart so they cannot share a disk sector, each CRC32-C checksummed.
Checkpoints alternate between them and `Open` picks the slot that passes its
checksum with the highest LSN. A crash partway through a header write
therefore destroys at most the slot that was not live.

**The cache has two tiers.** Pinned pages are committed but not yet
checkpointed: the log has them, the file does not, so they are the only copy a
reader can reach and cannot be evicted. Everything else is a plain LRU of
pages the file could supply again.

Cached page images are immutable. A writer never mutates a page in place — it
copies the page into its transaction's overlay — which is what makes eviction
safe with no reference counting at all. A caller holding a slice for an evicted
page keeps reading the snapshot it already had.

**Writes go through a batch.** A write transaction's changes accumulate in a
private copy-on-write map rather than in the cache. That gives three things at
once: readers never see uncommitted pages, rollback is free (drop the map),
and the log has a tidy set of full page images to write before the batch is
allowed to commit.

**The allocator** threads a free list through the freed pages themselves, four
bytes per link, and drains it before extending the file.

## The write-ahead log

`internal/wal` is a redo log of full page images, framed with CRC32-C
checksums.

The ordering rule is the whole thing: **every page a transaction touched
reaches the log, and the log is fsynced, before a single byte of it is
published.** If the process dies before the fsync returns, replay finds no
commit record and the transaction never happened. If it dies after, replay
redoes it in full.

Replay buffers page records and applies them only on seeing the matching
commit, so a transaction that was still in flight contributes nothing. It stops
at the first record that is short or fails its checksum, and truncates the torn
tail so appends resume from solid ground.

**Group commit** lives in `Sync`. It checks a watermark before taking the sync
mutex and again after: the first goroutine in flushes and fsyncs everything
written so far, and the ones queued behind it find the watermark already past
their target and return without an fsync of their own. Sixteen concurrent
commits cost one fsync.

**Checkpointing** is what makes the database file catch up. It fsyncs the log,
writes every pinned page through to the file, fsyncs, stamps the header, fsyncs
again, and truncates the log. It runs when the log passes a threshold (4 MiB by
default) and on close, which is why a cleanly closed database leaves no log
behind.

## The B+tree

`internal/btree` is the ordered map tables and indexes are stored in.

Internal nodes hold separator keys and child pointers; leaves hold keys and
values and are threaded both ways, so a range scan descends once and then walks
siblings. Values too large to share a page spill into an overflow chain, which
keeps at least four entries on every leaf however large the values are.

Cells are kept contiguous at all times: a mutation rewrites the whole cell area
through a scratch buffer rather than leaving a hole and tracking a free list
inside the page. That costs a 4 KiB copy per modified node — negligible next to
the 4 KiB page image the log writes for that same node anyway — and in exchange
there is no intra-page fragmentation to get wrong.

Delete borrows from a sibling when that leaves the sibling above the 25% fill
threshold, and merges otherwise. Both are best effort: when neither is
possible the node stays sparse, which costs space and never correctness.

## Transactions and MVCC

`internal/store` turns pages into tables of typed rows.

**A row is stored as versions.** A table's tree is keyed by `(row id, creating
transaction)`, so every version of a row sits together, oldest first; the value
carries the deleting transaction, or zero while the version is live. A version
belongs to a snapshot when its creator is visible and its deleter is not.

There is no separate set of in-flight transaction ids to filter against.
emberdb admits one writer at a time and hands out ids in commit order, so every
id at or below a snapshot's bound has committed and every id above it is in
flight or does not exist yet. The bound *is* the filter.

**Readers and writers never block each other.** Row versions give a reader the
right *contents*, but a scan also needs the tree to hold still: a writer that
splits the leaf a cursor is standing on could otherwise make it skip or repeat
entries. Rather than lock readers out while a commit publishes, the page cache
keeps versions. When a commit supersedes a page while an older transaction is
running, the previous image stays in memory and that reader keeps seeing it.
The images are released once no snapshot can reach them, and a page still owing
one is not evictable.

Publishing and the commit-counter bump happen under one lock, so a transaction
either registers before a commit and gets its pages kept for it, or registers
after and sees the new state whole.

**Garbage collection is prune-on-rewrite.** When a write transaction touches a
row it drops the versions no open or future snapshot can see, index entries
included. One row updated a thousand times keeps three versions.

**Indexes are keyed by version**, not by row, which is what makes an index scan
return each row exactly once: at most one version of a row is visible to a
given snapshot. Each entry is re-read and re-checked against the version it
points at, which also discards the stale entry a transaction leaves behind when
it writes the same row twice.

## SQL

`internal/sql` lexes and parses; `internal/exec` evaluates and plans. There is
no parser generator.

The whole query is lexed up front, which makes the lookahead the grammar needs
in a few places — telling `NOT BETWEEN` from a bare `NOT`, for one — trivial.
Every token carries a line and column, and every error uses them, down to a
caret under the offending character.

Null is three-valued: a comparison against null is unknown, `NOT unknown` stays
unknown, `unknown AND false` is false while `unknown AND true` is unknown, and
a WHERE clause keeps only rows its predicate is definitely true for.

**The planner** has one decision to make, since a statement reads exactly one
table: whether the WHERE clause reduces to a bounded range on an indexed
column. It flattens the top-level AND chain, recognises `column op constant` in
either direction, and intersects the bounds. Everything else is a full scan,
which is always correct because the predicate is re-evaluated per row
regardless.

The case worth knowing about is a bound of the wrong type. `WHERE intcol =
'text'` cannot be answered by the index, because an index's byte order only
matches its column's value order within one storage class. The planner declines
rather than returning the wrong rows.

## The public API and the CLI

The root `emberdb` package is the public surface: `Open`, `Exec`, `Query`,
`Begin`/`View`/`Update`, and schema introspection. `Value` and its constructors
are re-exported so callers never need to name an internal package.

`cmd/ember` is the shell, and `internal/term` is the line editor it uses —
hand-written, because the core module has no third-party dependencies and the
CLI ships inside it.

## Where the guarantees are tested

- **The log's own tests** damage a log deliberately: truncated at forty points,
  a byte flipped mid-record, a torn tail followed by fresh appends, a corrupt
  header.
- **The B+tree's tests** run randomised operation sequences against a Go map
  across four workload shapes, and a structural validator checks key ordering,
  separator bounds, uniform leaf depth, no page reachable twice, and both
  sibling chains agreeing with the in-order traversal.
- **The crash suite** kills a real child process with SIGKILL at 200 randomised
  points and checks that every acknowledged transaction is durable and
  complete. See `internal/crash` for what a SIGKILL does and does not simulate.
