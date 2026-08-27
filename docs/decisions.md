# Design decisions

Where the project brief left a choice open, or where a first attempt turned out
to be wrong, the reasoning is recorded here.

## The database is one file; the log is not

The brief calls emberdb a single-file database, and it also calls for a
write-ahead log. Those pull in opposite directions, because a log has to be
durable before the data file is updated, and a file cannot be atomically
extended and updated at once.

**Decision.** The *database* is one file. The log is a transient sidecar named
`<database>-wal`, and it exists only between a checkpoint and the crash or
clean shutdown that follows. `Close` checkpoints and removes it, so a database
at rest really is a single file with nothing beside it. This is what SQLite's
WAL mode does, for the same reason.

## One writer at a time

**Decision.** Any number of read transactions may run at once; write
transactions are serialised.

Two writers would need page-level conflict detection, and at page granularity
almost everything conflicts — two inserts into the same table usually touch the
same leaf. Detecting that and aborting one of them would give the worst of both
worlds: the complexity of optimistic concurrency and the throughput of a lock.

The cost is that a write-heavy workload does not scale across cores. The
benefit is that a whole class of problems does not exist: no deadlock
detection, no lock manager, no write-write conflict semantics to define. LMDB
and BoltDB make the same trade.

## Snapshot isolation is enforced twice, at two levels

This is the decision that took a second attempt.

The first design gave readers a shared lock that a commit had to take
exclusively in order to publish. It is correct, and it deadlocks the most
obvious test in the suite: open a reader, then commit a writer. More generally,
any design where a scan holds a lock for its lifetime means one slow reader
stops every write in the database.

Row versions alone are not enough either. They give a reader the right
*contents*, but a scan also needs the tree to hold still. A writer that splits
the leaf a cursor is standing on, or merges the leaf it is about to walk to,
can make that cursor skip or repeat entries no matter how carefully the rows
are filtered.

**Decision.** Version the pages too. When a commit supersedes a page while an
older transaction is running, the previous image stays in the page cache and
that reader keeps seeing it. Images are released as soon as no snapshot can
reach them, and a page still owing one is not evictable.

The cost is memory proportional to the pages modified during a long reader's
lifetime, and a cache that can exceed its nominal capacity while such a reader
is open. The alternative — copy-on-write page ids, as BoltDB does — would put
that cost on disk instead, and would mean every write allocates a new page and
rewrites its parent all the way to the root.

## A column holds one storage class

**Decision.** Values are coerced to their column's declared type on the way in.
An integer widens into a REAL column; nothing else converts, and anything else
is an error rather than a silent reinterpretation.

This is stricter than SQLite, which lets any column hold any type. The reason
is the index: an index key's byte order has to match the column's value order,
and integers and reals cannot share one order-preserving encoding without
losing precision above 2^53. Keeping a column to a single class makes the
question moot, and it means a query never has to reason about a column holding
both `7` and `'seven'`.

The planner enforces the other half: a WHERE bound that does not fit the
column's type causes it to decline the index and scan instead, rather than
compare bytes that are not comparable.

## Group commit exists, but a single writer limits what it can do

**Decision.** The log coalesces fsyncs properly — sixteen concurrent commits
cost one — and the engine's single-writer rule means that within emberdb today,
the batching happens across the many page records of one transaction rather
than across transactions.

Making it work across transactions would mean releasing the writer lock before
the fsync, so the next transaction could append while the previous one waits.
That is a real technique, and it is not free: the next transaction has to see
the previous one's changes, which means publishing them before they are
durable, which means a reader could observe a transaction a crash then revokes.
Fixing *that* needs a second visibility watermark for durable-versus-committed,
and the schema changes of a not-yet-durable transaction still leak through the
catalog, which is not versioned.

The mechanism is correct and tested where it lives. Turning it on across
transactions is a change to the engine's concurrency model, not to the log.

## Garbage collection is prune-on-rewrite

**Decision.** A row's dead versions are dropped when a transaction next writes
that row, not by a background vacuum.

It costs nothing when nothing is being updated, it bounds the versions a
repeatedly updated row accumulates — the case that would otherwise grow without
limit — and it needs no background goroutine, no scheduling policy and no way
to be surprised by one.

What it does not do is reclaim space from a table that is deleted from and then
never written to again. Those versions stay until something touches the row.
A real vacuum is future work.

## DDL is not versioned

**Decision.** The catalog is a plain B+tree, not an MVCC-versioned one.

A read transaction that began before a `CREATE TABLE` may therefore see that
the table exists. It will see it empty, because the rows in it were written by
a transaction its snapshot excludes, so the result is consistent — just not the
result full DDL isolation would give.

Versioning the catalog would mean giving schema objects xmin and xmax and
teaching every lookup about them, for a case that matters mostly to concurrent
migrations. emberdb does not have those yet.

## Keys are limited to 512 bytes

**Decision.** `Put` rejects a key over 512 bytes.

Internal nodes hold separator keys inline, so an unbounded key would let a
single separator fill a page and the tree would degenerate towards a linked
list. 512 bytes keeps at least seven separators on every internal page. Values
have no such limit: anything that would crowd its neighbours off a leaf moves
to an overflow chain instead.

## The SQL subset stops where it stops

**Not implemented,** deliberately: joins, subqueries, aggregates and GROUP BY,
DROP TABLE and DROP INDEX, ALTER TABLE, multi-column indexes, prepared
statements and bound parameters, views, triggers, and foreign keys.

The brief asked for a specific subset and these are all outside it. Each would
be a real piece of work rather than an afternoon — aggregates need a grouping
operator, joins need a join operator and a planner that chooses between orders,
DROP needs whole-tree deallocation. Half-implementing any of them would be
worse than not having them.

The parser names them when it meets them rather than reporting a generic syntax
error, so `DROP TABLE t` says emberdb has no DROP rather than complaining about
an unexpected identifier.

## The CLI has no third-party dependencies either

**Decision.** The terminal handling in `internal/term` is hand-written, rather
than using `golang.org/x/term`.

The core module has no third-party dependencies, and `cmd/ember` ships inside
it, so a dependency there would be a dependency everywhere. Raw mode is one
ioctl per platform; the line editor is a key loop. The ioctl requests differ
between Linux and macOS, so those are two small build-tagged files, with a
third for every other platform that reports "not a terminal" and falls back to
reading whole lines — which is also what makes piped input work.

## Benchmarks compare like with like, not emberdb at its best

**Decision.** Neither engine uses prepared statements in the benchmarks, both
fsync every commit, and SQLite is held to one connection.

emberdb has no prepared statements, so asking SQLite to parse each statement
too is the honest comparison of the two engines doing the same work. It is
explicitly *not* SQLite at its best: with prepared statements and bound
parameters it is faster than the numbers in the README. Saying so is part of
reporting them.
