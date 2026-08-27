package crash

import (
	"errors"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/anshnt/emberdb"
)

// TestMain lets the test binary act as its own crash victim: the parent
// re-executes it with EMBER_CRASH_CHILD set, which lands here and runs the
// workload instead of the tests.
func TestMain(m *testing.M) {
	if os.Getenv(envChild) != "" {
		os.Exit(RunChild())
	}
	os.Exit(m.Run())
}

// defaultIterations is how many crash points a normal test run injects. CI
// raises it through EMBER_CRASH_ITERS; the default keeps `go test ./...`
// quick enough to run constantly.
const defaultIterations = 25

// state is what a database held when it was reopened after a crash.
type state struct {
	// highest is the largest transaction number present in the ledger.
	highest int
	// rowsPerTxn counts the ledger rows found for each transaction.
	rowsPerTxn map[int]int
	// counter is the value the counters table holds, or zero if absent.
	counter int
	// scratchTxn is the transaction every scratch row is tagged with, or
	// zero when the table is empty.
	scratchTxn int
	// scratchRows is how many rows the scratch table holds.
	scratchRows int
}

func TestCrashRecovery(t *testing.T) {
	iterations := defaultIterations
	if raw := os.Getenv("EMBER_CRASH_ITERS"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 {
			t.Fatalf("EMBER_CRASH_ITERS must be a positive integer, got %q", raw)
		}
		iterations = parsed
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatalf("locating the test binary: %v", err)
	}

	started := time.Now()
	crashes := 0
	for i := 0; i < iterations; i++ {
		seed := int64(i) + 1
		// The seed drives every knob, so a failing iteration can be
		// reproduced exactly from the number the failure prints.
		random := rand.New(rand.NewSource(seed))
		rows := 1 + random.Intn(12)
		// A small checkpoint threshold on some runs makes the child
		// checkpoint often, which puts crashes inside and around the
		// riskiest moment: the log being folded into the file.
		checkpoint := int64(4 << 20)
		if random.Intn(3) == 0 {
			checkpoint = int64(8<<10) << random.Intn(6)
		}
		cycles := 1 + random.Intn(3)

		t.Run(fmt.Sprintf("seed%d", seed), func(t *testing.T) {
			dir := t.TempDir()
			database := filepath.Join(dir, "crash.ember")
			receipts := filepath.Join(dir, "receipts")
			for cycle := 0; cycle < cycles; cycle++ {
				// Delays span the whole life of a young database:
				// some kills land before the schema is committed,
				// others after thousands of rows.
				delay := time.Duration(2+random.Intn(320)) * time.Millisecond
				killChild(t, binary, database, receipts, seed+int64(cycle)*1000, rows, checkpoint, delay)
				crashes++
				verify(t, database, receipts, rows, crashesSoFar(cycle+1))
			}
		})
	}
	t.Logf("injected %d crashes across %d databases in %s", crashes, iterations, time.Since(started).Round(time.Millisecond))
}

// crashesSoFar is how many kills a database has survived, which bounds how
// many transactions may be present without a receipt.
func crashesSoFar(cycles int) int { return cycles }

// killChild starts a worker against the database and kills it after delay.
func killChild(t *testing.T, binary, database, receipts string, seed int64, rows int, checkpoint int64, delay time.Duration) {
	t.Helper()
	cmd := exec.Command(binary, "-test.run=TestCrashRecovery")
	cmd.Env = append(os.Environ(),
		envChild+"=1",
		envDatabase+"="+database,
		envReceipts+"="+receipts,
		envSeed+"="+strconv.FormatInt(seed, 10),
		envRows+"="+strconv.Itoa(rows),
		envCheckpoint+"="+strconv.FormatInt(checkpoint, 10),
	)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting the worker: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		// The worker loops forever, so exiting on its own means it hit
		// an error worth seeing.
		t.Fatalf("the worker exited before it was killed: %v\n%s", err, stderr.String())
	case <-time.After(delay):
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("killing the worker: %v", err)
	}
	<-done
	if out := stderr.String(); out != "" {
		t.Fatalf("the worker reported a problem before it was killed:\n%s", out)
	}
}

// verify reopens a killed database and checks every invariant the workload
// leaves behind.
func verify(t *testing.T, database, receipts string, rows, crashes int) {
	t.Helper()
	acknowledged := readReceipts(t, receipts)

	db, err := emberdb.OpenWith(database, emberdb.Options{})
	if err != nil {
		t.Fatalf("the database did not reopen after a crash: %v", err)
	}
	defer db.Close()
	found := readState(t, db)

	// 1. Durability. Every transaction the child was told had committed is
	//    still there in full.
	for _, txn := range acknowledged {
		if got := found.rowsPerTxn[txn]; got != rows {
			t.Fatalf("transaction %d was acknowledged as committed but has %d of its %d rows", txn, got, rows)
		}
	}

	// 2. Atomicity. Nothing is half applied, and the transactions present
	//    are exactly 1..highest with no gaps.
	for txn, got := range found.rowsPerTxn {
		if got != rows {
			t.Fatalf("transaction %d is half applied: %d of %d rows", txn, got, rows)
		}
		if txn < 1 || txn > found.highest {
			t.Fatalf("transaction %d is outside the range 1..%d", txn, found.highest)
		}
	}
	if len(found.rowsPerTxn) != found.highest {
		t.Fatalf("the ledger holds %d transactions but its highest is %d, so there is a gap",
			len(found.rowsPerTxn), found.highest)
	}

	// 3. No invention. A transaction may be present without a receipt only
	//    when it committed and the crash beat the receipt to disk, which
	//    can happen at most once per crash.
	unacknowledged := found.highest - len(acknowledged)
	if unacknowledged < 0 {
		t.Fatalf("%d transactions were acknowledged but only %d are present", len(acknowledged), found.highest)
	}
	if unacknowledged > crashes {
		t.Fatalf("%d transactions are present without a receipt after %d crashes; at most one may outrun its receipt per crash",
			unacknowledged, crashes)
	}

	// 4. Updates and deletes survived too. The counter was rewritten by
	//    every transaction, and the scratch table was emptied and refilled
	//    by it, so both must agree with the ledger.
	if found.counter != found.highest {
		t.Fatalf("the counter says %d but the ledger's highest transaction is %d", found.counter, found.highest)
	}
	switch {
	case found.highest == 0:
		if found.scratchRows != 0 {
			t.Fatalf("no transaction is present but the scratch table holds %d rows", found.scratchRows)
		}
	default:
		if found.scratchRows != rows {
			t.Fatalf("the scratch table holds %d rows, want %d from transaction %d",
				found.scratchRows, rows, found.highest)
		}
		if found.scratchTxn != found.highest {
			t.Fatalf("the scratch table is tagged with transaction %d, want %d",
				found.scratchTxn, found.highest)
		}
	}

	// 5. The recovered database is usable, not merely readable. The schema
	//    is re-applied first because a crash before the child committed it
	//    is a legitimate outcome.
	if _, err := db.Exec(schema); err != nil {
		t.Fatalf("the recovered database rejected DDL: %v", err)
	}
	if _, err := db.Exec("INSERT INTO ledger (txn, seq, payload) VALUES (-1, 0, 'after recovery')"); err != nil {
		t.Fatalf("the recovered database rejected a write: %v", err)
	}
	if _, err := db.Exec("DELETE FROM ledger WHERE txn = -1"); err != nil {
		t.Fatalf("the recovered database rejected a delete: %v", err)
	}
	if err := db.Checkpoint(); err != nil {
		t.Fatalf("the recovered database could not be checkpointed: %v", err)
	}
}

// readReceipts returns the transaction numbers the child recorded as
// committed. A line torn in half by the kill is ignored, since only whole
// lines are promises.
func readReceipts(t *testing.T, path string) []int {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		t.Fatalf("reading receipts: %v", err)
	}
	var out []int
	lines := strings.Split(string(data), "\n")
	for i, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		txn, err := strconv.Atoi(line)
		if err != nil {
			if i == len(lines)-1 {
				continue // a partial final line
			}
			t.Fatalf("receipt line %d is %q", i, line)
		}
		out = append(out, txn)
	}
	sort.Ints(out)
	return out
}

// query runs a read, reporting a table that does not exist as an empty result
// rather than an error.
func query(t *testing.T, db *emberdb.DB, statement string) (*emberdb.Result, error) {
	t.Helper()
	result, err := db.Query(statement)
	if errors.Is(err, emberdb.ErrNoSuchTable) {
		return &emberdb.Result{}, nil
	}
	return result, err
}

// readState reads back everything the invariants are checked against.
func readState(t *testing.T, db *emberdb.DB) state {
	t.Helper()
	found := state{rowsPerTxn: make(map[int]int)}

	// The schema is three statements, each committing on its own, so a
	// crash can land between them and leave one table missing. Each query
	// therefore tolerates its table not being there and reports it empty;
	// the invariants still catch a table that went missing with rows in it,
	// because the counter would no longer match the ledger.
	ledger, err := query(t, db, "SELECT txn FROM ledger")
	if err != nil {
		t.Fatalf("reading the ledger: %v", err)
	}
	for _, row := range ledger.Rows {
		txn := int(row[0].Int())
		found.rowsPerTxn[txn]++
		if txn > found.highest {
			found.highest = txn
		}
	}

	counters, err := query(t, db, "SELECT value FROM counters WHERE name = 'commits'")
	if err != nil {
		t.Fatalf("reading the counter: %v", err)
	}
	if len(counters.Rows) > 1 {
		t.Fatalf("the counters table holds %d rows for one key", len(counters.Rows))
	}
	if len(counters.Rows) == 1 {
		found.counter = int(counters.Rows[0][0].Int())
	}

	scratch, err := query(t, db, "SELECT txn FROM scratch")
	if err != nil {
		t.Fatalf("reading the scratch table: %v", err)
	}
	found.scratchRows = len(scratch.Rows)
	for _, row := range scratch.Rows {
		txn := int(row[0].Int())
		if found.scratchTxn == 0 {
			found.scratchTxn = txn
		} else if found.scratchTxn != txn {
			t.Fatalf("the scratch table mixes transactions %d and %d, so a delete and an insert were not atomic",
				found.scratchTxn, txn)
		}
	}
	return found
}
