// Package crash holds emberdb's crash-injection suite.
//
// The suite forks a child process that hammers a database, kills it with
// SIGKILL at a randomised point, reopens the file and checks that every
// transaction the child was told had committed is still there, that no
// transaction is half applied, and that nothing the child had not committed
// survived.
//
// A SIGKILL is not a power cut: bytes the process handed to the kernel outlive
// it. What it does destroy is everything emberdb was still holding — the log's
// user-space write buffer, and the committed pages pinned in the page cache
// that no checkpoint has written through yet. Recovering from that is exactly
// the job of the write-ahead log, and it is what this suite exercises. The
// power-cut cases, a log truncated or corrupted mid-record, are covered by the
// log's own tests, which damage a log deliberately and precisely.
package crash

import (
	"fmt"
	"math/rand"
	"os"
	"strconv"
	"strings"

	"github.com/anshnt/emberdb"
)

// Environment variables the parent uses to configure a child.
const (
	envChild      = "EMBER_CRASH_CHILD"
	envDatabase   = "EMBER_CRASH_DB"
	envReceipts   = "EMBER_CRASH_RECEIPTS"
	envSeed       = "EMBER_CRASH_SEED"
	envRows       = "EMBER_CRASH_ROWS"
	envCheckpoint = "EMBER_CRASH_CHECKPOINT"
)

// schema is the workload's three tables.
//
// Each is chosen to exercise a different path and to leave an invariant behind
// that recovery can be checked against: ledger only ever grows, scratch is
// emptied and refilled every transaction so its tree splits and merges
// constantly, and counters is one row rewritten over and over, which is the
// case that produces row versions and then prunes them.
const schema = `
CREATE TABLE IF NOT EXISTS ledger (txn INTEGER NOT NULL, seq INTEGER NOT NULL, payload TEXT);
CREATE TABLE IF NOT EXISTS scratch (txn INTEGER NOT NULL, seq INTEGER NOT NULL, payload TEXT);
CREATE TABLE IF NOT EXISTS counters (name TEXT PRIMARY KEY, value INTEGER NOT NULL);
`

// RunChild performs the workload until something kills it. It never returns
// normally: the parent's SIGKILL is the only way out, and any other outcome is
// a failure the parent will see in the exit status.
func RunChild() int {
	config, err := childConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "crash child: %v\n", err)
		return 2
	}
	db, err := emberdb.OpenWith(config.database, emberdb.Options{
		CheckpointBytes: config.checkpointBytes,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "crash child: open: %v\n", err)
		return 3
	}
	if _, err := db.Exec(schema); err != nil {
		fmt.Fprintf(os.Stderr, "crash child: schema: %v\n", err)
		return 4
	}
	// Pick up where a previous incarnation was killed.
	next, err := resume(db)
	if err != nil {
		fmt.Fprintf(os.Stderr, "crash child: resume: %v\n", err)
		return 5
	}
	receipts, err := os.OpenFile(config.receipts, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "crash child: receipts: %v\n", err)
		return 6
	}

	random := rand.New(rand.NewSource(config.seed))
	for txn := next; ; txn++ {
		// Payload sizes vary so that some transactions stay inline and
		// others spill into overflow pages.
		size := 8 + random.Intn(3)*random.Intn(900)
		if random.Intn(20) == 0 {
			size = 5000 + random.Intn(9000)
		}
		if err := commitTransaction(db, txn, config.rows, size); err != nil {
			fmt.Fprintf(os.Stderr, "crash child: transaction %d: %v\n", txn, err)
			return 7
		}
		// The receipt is written only after Commit has returned, so
		// anything in this file is something emberdb promised was
		// durable.
		if _, err := fmt.Fprintf(receipts, "%d\n", txn); err != nil {
			fmt.Fprintf(os.Stderr, "crash child: receipt: %v\n", err)
			return 8
		}
		if err := receipts.Sync(); err != nil {
			fmt.Fprintf(os.Stderr, "crash child: receipt sync: %v\n", err)
			return 9
		}
	}
}

// commitTransaction writes one transaction's worth of work atomically.
func commitTransaction(db *emberdb.DB, txn, rows, size int) error {
	payload := strings.Repeat("x", size)
	return db.Update(func(tx *emberdb.Tx) error {
		for seq := 0; seq < rows; seq++ {
			if _, err := tx.Exec(fmt.Sprintf(
				"INSERT INTO ledger (txn, seq, payload) VALUES (%d, %d, '%s')", txn, seq, payload)); err != nil {
				return err
			}
		}
		if _, err := tx.Exec("DELETE FROM scratch"); err != nil {
			return err
		}
		for seq := 0; seq < rows; seq++ {
			if _, err := tx.Exec(fmt.Sprintf(
				"INSERT INTO scratch (txn, seq, payload) VALUES (%d, %d, '%s')", txn, seq, payload)); err != nil {
				return err
			}
		}
		result, err := tx.Exec(fmt.Sprintf("UPDATE counters SET value = %d WHERE name = 'commits'", txn))
		if err != nil {
			return err
		}
		if result.RowsAffected == 0 {
			_, err = tx.Exec(fmt.Sprintf("INSERT INTO counters (name, value) VALUES ('commits', %d)", txn))
		}
		return err
	})
}

// resume reads the counter the last surviving transaction left behind.
func resume(db *emberdb.DB) (int, error) {
	result, err := db.Query("SELECT value FROM counters WHERE name = 'commits'")
	if err != nil {
		return 0, err
	}
	if len(result.Rows) == 0 {
		return 1, nil
	}
	return int(result.Rows[0][0].Int()) + 1, nil
}

// config is what the parent tells a child to do.
type config struct {
	database        string
	receipts        string
	seed            int64
	rows            int
	checkpointBytes int64
}

// childConfig reads the child's settings out of the environment.
func childConfig() (config, error) {
	c := config{
		database: os.Getenv(envDatabase),
		receipts: os.Getenv(envReceipts),
	}
	if c.database == "" || c.receipts == "" {
		return c, fmt.Errorf("%s and %s must be set", envDatabase, envReceipts)
	}
	var err error
	if c.seed, err = strconv.ParseInt(os.Getenv(envSeed), 10, 64); err != nil {
		return c, fmt.Errorf("%s: %w", envSeed, err)
	}
	if c.rows, err = strconv.Atoi(os.Getenv(envRows)); err != nil {
		return c, fmt.Errorf("%s: %w", envRows, err)
	}
	if c.checkpointBytes, err = strconv.ParseInt(os.Getenv(envCheckpoint), 10, 64); err != nil {
		return c, fmt.Errorf("%s: %w", envCheckpoint, err)
	}
	return c, nil
}
