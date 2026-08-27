package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/anshnt/emberdb"
	"github.com/anshnt/emberdb/internal/sql"
	"github.com/anshnt/emberdb/internal/term"
)

// historyLimit caps how many lines are written to the history file.
const historyLimit = 2000

// Shell is the interactive front end: it reads statements, runs them and
// prints what came back.
type Shell struct {
	db     *emberdb.DB
	out    io.Writer
	errOut io.Writer
	editor *term.Editor

	mode  Mode
	timer bool
	// quit is set by .quit and .exit, and by end of input.
	quit bool
	// depth guards .read against a file that reads itself.
	depth int
}

// NewShell wraps an open database.
func NewShell(db *emberdb.DB, in *os.File, out, errOut io.Writer, mode Mode, timer bool) *Shell {
	return &Shell{
		db:     db,
		out:    out,
		errOut: errOut,
		editor: term.NewEditor(in, out),
		mode:   mode,
		timer:  timer,
	}
}

// historyPath returns where the REPL remembers its history, or the empty
// string if there is nowhere sensible to put it.
func historyPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".ember_history")
}

// Repl reads and runs statements until the input ends.
func (s *Shell) Repl() error {
	interactive := s.editor.Interactive()
	path := historyPath()
	if interactive && path != "" {
		if err := s.editor.LoadHistory(path); err != nil {
			fmt.Fprintf(s.errOut, "ember: could not read history: %v\n", err)
		}
		defer func() {
			if err := s.editor.SaveHistory(path, historyLimit); err != nil {
				fmt.Fprintf(s.errOut, "ember: could not write history: %v\n", err)
			}
		}()
	}
	if interactive {
		fmt.Fprintf(s.out, "emberdb %s — %s\nType .help for commands, .quit to leave.\n\n", emberdb.Version, s.db.Path())
	}

	var pending strings.Builder
	for !s.quit {
		prompt := "ember> "
		if pending.Len() > 0 {
			prompt = "   ...> "
		}
		if !interactive {
			prompt = ""
		}
		line, err := s.editor.ReadLine(prompt)
		if err != nil {
			if errors.Is(err, term.ErrInterrupted) {
				pending.Reset()
				continue
			}
			if errors.Is(err, io.EOF) {
				if interactive {
					fmt.Fprintln(s.out)
				}
				return s.finish(pending.String())
			}
			return err
		}

		if pending.Len() == 0 {
			trimmed := strings.TrimSpace(line)
			if trimmed == "" {
				continue
			}
			if strings.HasPrefix(trimmed, ".") {
				s.editor.AddHistory(line)
				s.report(s.meta(trimmed))
				continue
			}
		}
		pending.WriteString(line)
		pending.WriteString("\n")
		if !sql.Complete(pending.String()) {
			continue
		}
		statement := pending.String()
		pending.Reset()
		s.editor.AddHistory(strings.TrimRight(statement, "\n"))
		s.report(s.Run(statement))
	}
	return nil
}

// finish runs whatever was left in the buffer when the input ended.
func (s *Shell) finish(pending string) error {
	if strings.TrimSpace(pending) == "" {
		return nil
	}
	s.report(s.Run(pending))
	return nil
}

// report prints an error without stopping the shell, since a REPL that exits
// on a typo is useless.
func (s *Shell) report(err error) {
	if err == nil {
		return
	}
	var syntax *emberdb.SyntaxError
	if errors.As(err, &syntax) {
		fmt.Fprintln(s.errOut, syntax.Detail())
		return
	}
	fmt.Fprintf(s.errOut, "%v\n", err)
}

// Run executes a script and prints each statement's result.
//
// The script is split into individual statements first, so that .timer reports
// each one rather than the batch, and so that a failure halfway through names
// the statement that failed.
func (s *Shell) Run(script string) error {
	statements, err := sql.Split(script)
	if err != nil {
		return err
	}
	for _, statement := range statements {
		start := time.Now()
		result, err := s.db.Exec(statement)
		elapsed := time.Since(start)
		if err != nil {
			return err
		}
		if err := s.present(result, elapsed); err != nil {
			return err
		}
	}
	return nil
}

// present prints one statement's result and, when the timer is on, how long it
// took and how it found its rows.
func (s *Shell) present(result *emberdb.Result, elapsed time.Duration) error {
	if len(result.Columns) > 0 {
		if err := render(s.out, s.mode, result); err != nil {
			return err
		}
		if s.editor.Interactive() {
			fmt.Fprintf(s.out, "%d row%s\n", len(result.Rows), plural(len(result.Rows)))
		}
	} else if result.RowsAffected > 0 && s.editor.Interactive() {
		fmt.Fprintf(s.out, "%d row%s affected\n", result.RowsAffected, plural(result.RowsAffected))
	}
	if s.timer {
		if result.Plan != "" {
			fmt.Fprintf(s.out, "plan: %s\n", result.Plan)
		}
		fmt.Fprintf(s.out, "time: %s\n", formatDuration(elapsed))
	}
	return nil
}

// formatDuration prints a duration at a resolution a human can read.
func formatDuration(d time.Duration) string {
	if d < time.Microsecond {
		return fmt.Sprintf("%dns", d.Nanoseconds())
	}
	return fmt.Sprintf("%.3fms", float64(d.Nanoseconds())/1e6)
}

// meta runs a dot command.
func (s *Shell) meta(line string) error {
	fields := strings.Fields(line)
	command := strings.ToLower(fields[0])
	args := fields[1:]
	switch command {
	case ".help":
		return s.help()
	case ".quit", ".exit":
		s.quit = true
		return nil
	case ".tables":
		return s.tables()
	case ".schema":
		return s.schema(args)
	case ".timer":
		return s.setTimer(args)
	case ".mode":
		return s.setMode(args)
	case ".read":
		return s.read(args)
	case ".stats":
		return s.stats()
	default:
		return fmt.Errorf("ember: unknown command %s, try .help", fields[0])
	}
}

// help lists the dot commands.
func (s *Shell) help() error {
	_, err := io.WriteString(s.out, strings.Join([]string{
		".help              show this list",
		".tables            list the tables in the database",
		".schema [TABLE]    show the CREATE statements for one table or all of them",
		".timer on|off      report the time and query plan for each statement",
		".mode MODE         set the output format: table, csv or list",
		".read FILE         run the statements in a file",
		".stats             show page, cache and log counters",
		".quit              leave",
		"",
	}, "\n"))
	return err
}

// tables lists the table names.
func (s *Shell) tables() error {
	names, err := s.db.Tables()
	if err != nil {
		return err
	}
	if len(names) == 0 {
		_, err := fmt.Fprintln(s.out, "(no tables)")
		return err
	}
	_, err = fmt.Fprintln(s.out, strings.Join(names, "  "))
	return err
}

// schema prints the CREATE statements for one table or for all of them.
func (s *Shell) schema(args []string) error {
	names := args
	if len(names) == 0 {
		all, err := s.db.Tables()
		if err != nil {
			return err
		}
		names = all
		sort.Strings(names)
	}
	if len(names) == 0 {
		_, err := fmt.Fprintln(s.out, "(no tables)")
		return err
	}
	for i, name := range names {
		info, err := s.db.TableInfo(name)
		if err != nil {
			return err
		}
		if i > 0 {
			fmt.Fprintln(s.out)
		}
		if _, err := fmt.Fprintln(s.out, info.DDL()); err != nil {
			return err
		}
	}
	return nil
}

// setTimer turns statement timing on or off.
func (s *Shell) setTimer(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("ember: usage: .timer on|off")
	}
	switch strings.ToLower(args[0]) {
	case "on":
		s.timer = true
	case "off":
		s.timer = false
	default:
		return fmt.Errorf("ember: .timer takes on or off, not %q", args[0])
	}
	return nil
}

// setMode changes the output format.
func (s *Shell) setMode(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("ember: usage: .mode table|csv|list")
	}
	mode, ok := ParseMode(args[0])
	if !ok {
		return fmt.Errorf("ember: unknown output mode %q, try table, csv or list", args[0])
	}
	s.mode = mode
	return nil
}

// read runs a file of statements. Dot commands inside the file work too, so a
// script can set the output mode before its queries.
func (s *Shell) read(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("ember: usage: .read FILE")
	}
	if s.depth > 8 {
		return fmt.Errorf("ember: .read is nested too deeply; does %s read itself?", args[0])
	}
	s.depth++
	defer func() { s.depth-- }()
	return s.RunFile(args[0])
}

// RunFile executes every statement in a file.
func (s *Shell) RunFile(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("ember: %w", err)
	}
	return s.RunScript(string(data))
}

// RunScript executes a script, honouring the dot commands in it.
//
// Statements are split on lines rather than parsed all at once so that a dot
// command can sit between them, and so that an error names the statement it
// came from rather than the whole file.
func (s *Shell) RunScript(script string) error {
	var pending strings.Builder
	for _, line := range strings.Split(script, "\n") {
		if pending.Len() == 0 {
			trimmed := strings.TrimSpace(line)
			if trimmed == "" {
				continue
			}
			if strings.HasPrefix(trimmed, ".") {
				if err := s.meta(trimmed); err != nil {
					return err
				}
				if s.quit {
					return nil
				}
				continue
			}
		}
		pending.WriteString(line)
		pending.WriteString("\n")
		if !sql.Complete(pending.String()) {
			continue
		}
		statement := pending.String()
		pending.Reset()
		if err := s.Run(statement); err != nil {
			return err
		}
	}
	if strings.TrimSpace(pending.String()) == "" {
		return nil
	}
	return s.Run(pending.String())
}

// stats prints the database's counters.
func (s *Shell) stats() error {
	stats := s.db.Stats()
	size, err := s.db.FileSize()
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(s.out, ""+
		"file            %s (%d bytes)\n"+
		"pages           %d total, %d free\n"+
		"cache           %d pages resident, %d awaiting checkpoint\n"+
		"log             %d bytes, %d fsyncs\n"+
		"last commit     transaction %d\n",
		s.db.Path(), size,
		stats.Pages, stats.FreePages,
		stats.CachedPages, stats.PendingPages,
		stats.LogBytes, stats.Syncs,
		stats.LastTxID)
	return err
}
