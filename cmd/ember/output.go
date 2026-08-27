package main

import (
	"encoding/csv"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/anshnt/emberdb"
)

// Mode is how results are rendered.
type Mode string

// The output modes .mode accepts.
const (
	// ModeTable draws a box around aligned columns. It is the default.
	ModeTable Mode = "table"
	// ModeCSV writes comma-separated values with a header row.
	ModeCSV Mode = "csv"
	// ModeList writes pipe-separated values with no padding.
	ModeList Mode = "list"
)

// ParseMode maps a name to a Mode.
func ParseMode(name string) (Mode, bool) {
	switch Mode(strings.ToLower(name)) {
	case ModeTable:
		return ModeTable, true
	case ModeCSV:
		return ModeCSV, true
	case ModeList:
		return ModeList, true
	default:
		return "", false
	}
}

// render writes a result in the given mode.
func render(w io.Writer, mode Mode, result *emberdb.Result) error {
	if len(result.Columns) == 0 {
		return nil
	}
	switch mode {
	case ModeCSV:
		return renderCSV(w, result)
	case ModeList:
		return renderList(w, result)
	default:
		return renderTable(w, result)
	}
}

// cells renders every value as the string it will be printed as.
func cells(result *emberdb.Result) [][]string {
	out := make([][]string, len(result.Rows))
	for i, row := range result.Rows {
		out[i] = make([]string, len(row))
		for j, v := range row {
			out[i][j] = v.String()
		}
	}
	return out
}

// renderTable draws light box-drawing rules around padded columns.
func renderTable(w io.Writer, result *emberdb.Result) error {
	body := cells(result)
	widths := make([]int, len(result.Columns))
	for i, name := range result.Columns {
		widths[i] = utf8.RuneCountInString(name)
	}
	for _, row := range body {
		for i, cell := range row {
			if n := utf8.RuneCountInString(cell); n > widths[i] {
				widths[i] = n
			}
		}
	}

	rule := func(left, middle, right string) error {
		parts := make([]string, len(widths))
		for i, width := range widths {
			parts[i] = strings.Repeat("─", width+2)
		}
		_, err := fmt.Fprintf(w, "%s%s%s\n", left, strings.Join(parts, middle), right)
		return err
	}
	writeRow := func(row []string) error {
		parts := make([]string, len(row))
		for i, cell := range row {
			parts[i] = " " + cell + strings.Repeat(" ", widths[i]-utf8.RuneCountInString(cell)) + " "
		}
		_, err := fmt.Fprintf(w, "│%s│\n", strings.Join(parts, "│"))
		return err
	}

	if err := rule("┌", "┬", "┐"); err != nil {
		return err
	}
	if err := writeRow(result.Columns); err != nil {
		return err
	}
	if err := rule("├", "┼", "┤"); err != nil {
		return err
	}
	for _, row := range body {
		if err := writeRow(row); err != nil {
			return err
		}
	}
	return rule("└", "┴", "┘")
}

// renderCSV writes the result as CSV with a header row.
func renderCSV(w io.Writer, result *emberdb.Result) error {
	out := csv.NewWriter(w)
	if err := out.Write(result.Columns); err != nil {
		return err
	}
	for _, row := range cells(result) {
		if err := out.Write(row); err != nil {
			return err
		}
	}
	out.Flush()
	return out.Error()
}

// renderList writes pipe-separated values, which is the easiest form to pipe
// into another program.
func renderList(w io.Writer, result *emberdb.Result) error {
	if _, err := fmt.Fprintln(w, strings.Join(result.Columns, "|")); err != nil {
		return err
	}
	for _, row := range cells(result) {
		if _, err := fmt.Fprintln(w, strings.Join(row, "|")); err != nil {
			return err
		}
	}
	return nil
}

// plural returns "s" unless n is one, for messages like "3 rows".
func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
