// The benchmark suite is a module of its own so that the comparison against
// SQLite does not put a third-party dependency into emberdb itself.
module github.com/anshnt/emberdb/bench

go 1.25.0

replace github.com/anshnt/emberdb => ../

require (
	github.com/anshnt/emberdb v0.0.0-00010101000000-000000000000
	modernc.org/sqlite v1.57.0
)

require (
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/mattn/go-isatty v0.0.24 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	golang.org/x/sys v0.47.0 // indirect
	modernc.org/libc v1.74.4 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.11.0 // indirect
)
