# emberdb benchmarks

A separate Go module so that the SQLite driver used for comparison never
becomes a dependency of emberdb itself.

```
cd bench
go test -bench . -benchtime 3s -count 3
```

Both engines are asked to do the same work: fsync on every commit, no prepared
statements on either side, bulk inserts batched a thousand rows to a
transaction, and a fresh database file per benchmark. `bench_test.go` states
the fairness rules in full, including where they favour emberdb by holding
SQLite back from its best.
