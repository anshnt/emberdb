# Contributing to emberdb

Thanks for taking a look. This is a small project with a clear shape, so most
of what follows is about keeping that shape.

## Getting set up

```
git clone https://github.com/anshnt/emberdb
cd emberdb
go test ./...
```

That is the whole setup. The core module has no third-party dependencies and
that is deliberate — please do not add one. The benchmark suite in `bench/` is
a separate module precisely so that its SQLite driver stays out of the engine.

## Before you open a pull request

Run what CI runs:

```
gofmt -l .                 # must print nothing
go vet ./...
go install honnef.co/go/tools/cmd/staticcheck@v0.8.1
staticcheck ./...
go test -race ./...
```

If you touched the pager, the log, the B+tree or the store, also run the crash
suite at full strength. It takes a couple of minutes and it is the test most
likely to catch a mistake in those layers:

```
EMBER_CRASH_ITERS=200 go test -count=1 -timeout 20m -run TestCrashRecovery ./internal/crash/
```

If you touched anything on a read or write path, check you have not made it
slower:

```
cd bench && go test -bench . -benchtime 3s
```

## What a change should look like

**Small commits with Conventional Commits subjects.** `feat:`, `fix:`,
`docs:`, `test:`, `refactor:`, `perf:`, `chore:`, `ci:`. Subject under 72
characters, imperative, lowercase after the colon. Add a body when the change
needs justifying — why, not what; the diff already says what.

**Tests that would fail without the change.** A bug fix should come with a test
that reproduces the bug. A new behaviour should come with a test for the
interesting case and at least one for the edge.

The existing tests are worth reading before writing new ones. They lean on
adversarial cases rather than happy paths: the B+tree is checked against a Go
map over randomised sequences with a structural validator, the log is tested by
being deliberately damaged, and the parser's error messages are asserted by
content and position, so a message that stops saying what went wrong fails the
build.

**Doc comments on anything exported**, and on unexported things whose reason
for existing is not obvious from the name. Comments should explain why the code
is the way it is; the tricky invariants in this codebase are mostly about
ordering and lifetime, and those are exactly what a reader cannot recover from
the code alone.

**No TODO or FIXME.** If it is worth remembering, it is worth an issue.

## Changing the file format

`FormatVersion` in `internal/pager` and `internal/wal` guards what a build will
open. If you change a layout, bump the version, update `docs/format.md` in the
same commit, and say in the pull request what happens to a database written by
the previous version.

The format tables in `docs/format.md` are meant to be exact enough to write a
reader from. If a change makes them wrong, the change is not finished.

## Reporting a bug

The most useful bug report for a database is one that reproduces. If you can
turn it into a failing test, that is the ideal; if you can give the SQL and the
sequence of statements, that is nearly as good. For anything involving crash
recovery or corruption, the seed printed by a failing crash-suite iteration
reproduces that iteration exactly.

## Scope

`docs/decisions.md` lists what is deliberately not implemented and why. If you
want to add one of those things, please open an issue first so we can agree on
the shape before you write it — several of them are real pieces of work and
half of one is worse than none.
