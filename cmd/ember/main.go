// Command ember is a shell for emberdb databases.
//
// With no arguments it opens a scratch database and starts a REPL:
//
//	ember
//
// Given a path it opens or creates that database, and given SQL files it runs
// them and exits:
//
//	ember app.ember
//	ember app.ember schema.sql seed.sql
//	ember -c "SELECT count FROM stats" app.ember
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/anshnt/emberdb"
)

func main() { os.Exit(run(os.Args[1:])) }

// run is main's body, returning an exit status instead of calling os.Exit so
// that deferred cleanup happens.
func run(args []string) int {
	flags := flag.NewFlagSet("ember", flag.ContinueOnError)
	flags.Usage = func() { usage(flags.Output(), flags) }
	var (
		command     = flags.String("c", "", "run `SQL` and exit")
		modeName    = flags.String("mode", "table", "output format: table, csv or list")
		timer       = flags.Bool("timer", false, "report the time and query plan for each statement")
		interactive = flags.Bool("i", false, "start the REPL after running the given files")
		noSync      = flags.Bool("nosync", false, "skip the fsyncs that make commits durable, for bulk loading")
		showVersion = flags.Bool("version", false, "print the version and exit")
	)
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *showVersion {
		fmt.Printf("ember %s\n", emberdb.Version)
		return 0
	}
	mode, ok := ParseMode(*modeName)
	if !ok {
		fmt.Fprintf(os.Stderr, "ember: unknown output mode %q, try table, csv or list\n", *modeName)
		return 2
	}

	path, scripts := flags.Arg(0), flags.Args()
	if len(scripts) > 0 {
		scripts = scripts[1:]
	}
	temporary := ""
	if path == "" {
		var err error
		path, err = os.MkdirTemp("", "ember")
		if err != nil {
			fmt.Fprintf(os.Stderr, "ember: %v\n", err)
			return 1
		}
		temporary = path
		path = filepath.Join(path, "scratch.ember")
		defer os.RemoveAll(temporary)
	}

	db, err := emberdb.OpenWith(path, emberdb.Options{NoSync: *noSync})
	if err != nil {
		fmt.Fprintf(os.Stderr, "ember: %v\n", err)
		return 1
	}
	defer func() {
		if err := db.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "ember: %v\n", err)
		}
	}()

	shell := NewShell(db, os.Stdin, os.Stdout, os.Stderr, mode, *timer)
	if temporary != "" && *command == "" && len(scripts) == 0 {
		fmt.Fprintln(os.Stderr, "ember: no database given, using a temporary one that will be discarded")
	}

	status := 0
	for _, script := range scripts {
		if err := shell.RunFile(script); err != nil {
			shell.report(err)
			return 1
		}
	}
	if *command != "" {
		if err := shell.RunScript(*command); err != nil {
			shell.report(err)
			return 1
		}
	}
	if *command == "" && len(scripts) == 0 || *interactive {
		if err := shell.Repl(); err != nil {
			fmt.Fprintf(os.Stderr, "ember: %v\n", err)
			return 1
		}
	}
	return status
}

// usage explains the command's arguments.
func usage(out io.Writer, flags *flag.FlagSet) {
	fmt.Fprint(out, strings.Join([]string{
		"ember is a shell for emberdb databases.",
		"",
		"usage: ember [flags] [database] [file.sql ...]",
		"",
		"With no database it opens a temporary one and starts a REPL. With SQL",
		"files it runs them and exits, unless -i is given.",
		"",
		"flags:",
		"",
	}, "\n"))
	flags.PrintDefaults()
	fmt.Fprint(out, strings.Join([]string{
		"",
		"examples:",
		"  ember app.ember                     open a database and start the REPL",
		"  ember app.ember schema.sql          run a file and exit",
		"  ember -c 'SELECT * FROM notes' app.ember",
		"  ember -mode csv -c 'SELECT * FROM notes' app.ember > notes.csv",
		"",
	}, "\n"))
}
