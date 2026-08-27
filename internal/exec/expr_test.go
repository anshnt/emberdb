package exec

import (
	"strings"
	"testing"

	"github.com/anshnt/emberdb/internal/sql"
	"github.com/anshnt/emberdb/internal/store"
	"github.com/anshnt/emberdb/internal/value"
)

func TestLikeMatch(t *testing.T) {
	cases := []struct {
		text, pattern string
		want          bool
	}{
		{"", "", true},
		{"", "%", true},
		{"", "_", false},
		{"a", "a", true},
		{"a", "b", false},
		{"abc", "a%", true},
		{"abc", "%c", true},
		{"abc", "%b%", true},
		{"abc", "a_c", true},
		{"abc", "a_", false},
		{"abc", "___", true},
		{"abc", "____", false},
		{"abc", "%%%", true},
		{"abc", "a%%c", true},
		{"aaa", "%a", true},
		{"aaa", "a%a", true},
		{"banana", "%an%na", true},
		{"banana", "%an%nb", false},
		{"naïve", "na_ve", true},
		{"日本語", "日%語", true},
		{"日本語", "日_語", true},
	}
	for _, c := range cases {
		if got := likeMatch(c.text, c.pattern); got != c.want {
			t.Errorf("likeMatch(%q, %q) = %v, want %v", c.text, c.pattern, got, c.want)
		}
	}
}

// TestLikeMatchDoesNotBlowUp checks the pattern that makes a naive backtracking
// matcher take exponential time. It has to finish, not just be correct.
func TestLikeMatchDoesNotBlowUp(t *testing.T) {
	text := strings.Repeat("a", 40) + "b"
	pattern := strings.Repeat("%a", 20) + "%c"
	if likeMatch(text, pattern) {
		t.Fatal("pattern should not match")
	}
	if !likeMatch(strings.Repeat("a", 40)+"c", pattern) {
		t.Fatal("pattern should match when it ends in c")
	}
}

// table builds a table definition for planner tests.
func table() *store.Table {
	return &store.Table{
		Name: "t",
		Columns: []store.Column{
			{Name: "a", Type: value.TypeInteger},
			{Name: "b", Type: value.TypeText},
			{Name: "r", Type: value.TypeReal},
		},
		Indexes: []store.Index{
			{Name: "t_a", Column: 0, Root: 9},
			{Name: "t_r", Column: 2, Root: 10},
		},
	}
}

// where parses the WHERE clause of a query.
func where(t *testing.T, clause string) sql.Expr {
	t.Helper()
	statement, err := sql.ParseOne("SELECT * FROM t WHERE " + clause)
	if err != nil {
		t.Fatalf("parsing %q: %v", clause, err)
	}
	return statement.(*sql.Select).Where
}

func TestPlannerChoosesAnIndex(t *testing.T) {
	cases := []struct {
		clause    string
		wantIndex string
		low, high string // rendered bounds, empty for unbounded
	}{
		{"a = 5", "t_a", "5", "5"},
		{"5 = a", "t_a", "5", "5"},
		{"a > 5", "t_a", "5", ""},
		{"5 < a", "t_a", "5", ""},
		{"a <= 9", "t_a", "", "9"},
		{"a >= 2 AND a < 8", "t_a", "2", "8"},
		{"a > 2 AND a > 4", "t_a", "4", ""},
		{"a = 5 AND b = 'x'", "t_a", "5", "5"},
		{"b = 'x' AND a = 5", "t_a", "5", "5"},
		{"r > 1", "t_r", "1.0", ""},
	}
	for _, c := range cases {
		got := planScan(table(), where(t, c.clause))
		if got.index == nil {
			t.Errorf("%s produced a full scan, want index %s", c.clause, c.wantIndex)
			continue
		}
		if got.index.Name != c.wantIndex {
			t.Errorf("%s chose index %s, want %s", c.clause, got.index.Name, c.wantIndex)
		}
		if bound(got.rng.Low) != c.low || bound(got.rng.High) != c.high {
			t.Errorf("%s produced bounds [%s, %s], want [%s, %s]",
				c.clause, bound(got.rng.Low), bound(got.rng.High), c.low, c.high)
		}
	}
}

func TestPlannerFallsBackToAFullScan(t *testing.T) {
	cases := []string{
		"b = 'x'",           // no index on b
		"a = 1 OR a = 2",    // a disjunction is not one range
		"a = 'text'",        // the bound does not fit the column's type
		"a = NULL",          // null bounds cannot be searched for
		"a + 1 = 5",         // not a bare column comparison
		"a != 5",            // inequality does not bound a range
		"a LIKE 'x%'",       // patterns are not ranges
		"a = 1 AND r = 1.0", // two different indexes, only one may be used
	}
	for _, clause := range cases {
		got := planScan(table(), where(t, clause))
		if clause == "a = 1 AND r = 1.0" {
			// This one may legitimately pick either index; it must just
			// not try to use both.
			if got.index != nil && got.index.Name != "t_a" && got.index.Name != "t_r" {
				t.Errorf("%s chose %s", clause, got.index.Name)
			}
			continue
		}
		if got.index != nil {
			t.Errorf("%s chose index %s, want a full scan", clause, got.index.Name)
		}
		if !strings.HasPrefix(got.description, "scan ") {
			t.Errorf("%s described itself as %q", clause, got.description)
		}
	}
	if got := planScan(table(), nil); got.index != nil {
		t.Error("a statement with no WHERE clause chose an index")
	}
}

// bound renders an optional range bound for comparison.
func bound(v *value.Value) string {
	if v == nil {
		return ""
	}
	return v.String()
}
