# emberdb

An embedded, single-file database engine for Go, with a B+tree storage layer,
a write-ahead log, MVCC snapshot isolation and a small SQL dialect.

Work in progress. The layers land in this order: pager, write-ahead log,
B+tree, transactions, SQL, CLI.
