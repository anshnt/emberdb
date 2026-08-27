# Changelog

All notable changes to this project are documented here.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## Unreleased

Nothing yet.

## 0.1.0 - 2026-08-27

The first release. Everything below is new.

### Added

- **Pager.** A single file presented as 4 KiB pages, with a double-buffered
  CRC32-C checksummed header so a crash during a header write cannot destroy
  the last durable one, a two-tier page cache, and a free-list allocator that
  reuses reclaimed pages before extending the file.
- **Write-ahead log.** A redo log of full page images with CRC32-C framing,
  group commit, and replay that applies only committed transactions and stops
  at the first damaged record.
- **B+tree.** Insert with node splitting, delete with sibling borrow and node
  merge, overflow chains for oversized values, and forward and reverse cursors
  over a doubly linked leaf chain.
- **Transactions with snapshot isolation.** Row versions stamped with creating
  and deleting transaction ids, and versioned page images so that readers and
  writers never block one another. Dead versions are pruned when a row is next
  written.
- **Single-column indexes**, keyed by row version so that an index scan returns
  each row exactly once, with unique constraints enforced through them.
- **SQL.** A hand-written lexer and recursive-descent parser covering CREATE
  TABLE, CREATE INDEX, INSERT, SELECT with WHERE, ORDER BY, LIMIT and OFFSET,
  UPDATE, DELETE, and BEGIN/COMMIT/ROLLBACK. Typed columns — INTEGER, REAL,
  TEXT, BLOB — with three-valued null logic. Parse errors carry a line, a
  column and a caret.
- **`ember` command.** A REPL with history and line editing, `.tables`,
  `.schema`, `.timer`, `.mode`, `.read` and `.stats`, three output formats, and
  the ability to run SQL files.
- **Go library API.** `Open`, `Exec`, `Query`, and `Begin`/`View`/`Update` for
  transactions, plus schema introspection.
- **Crash-injection suite.** A child process is killed with SIGKILL at 200
  randomised points and the database is checked for durability, atomicity and
  usability after each one.
- **Benchmarks** against SQLite in a separate module, so the comparison does not
  put a third-party dependency into the engine.
