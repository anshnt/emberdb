package value

import (
	"bytes"
	"math"
	"math/rand"
	"sort"
	"testing"
)

func TestRowEncodingRoundTrips(t *testing.T) {
	rows := [][]Value{
		{Null()},
		{Integer(0), Integer(1), Integer(-1)},
		{Integer(math.MaxInt64), Integer(math.MinInt64)},
		{Real(0), Real(-0.5), Real(math.Pi), Real(math.Inf(1)), Real(math.Inf(-1))},
		{Text(""), Text("hello"), Text("naïve · 日本語")},
		{Blob(nil), Blob([]byte{0, 1, 2, 255})},
		{Null(), Integer(7), Real(1.5), Text("mixed"), Blob([]byte("bytes"))},
	}
	for _, row := range rows {
		encoded := AppendRow(nil, row)
		decoded, err := DecodeRow(encoded, len(row))
		if err != nil {
			t.Fatalf("DecodeRow(%v): %v", row, err)
		}
		if len(decoded) != len(row) {
			t.Fatalf("decoded %d values, want %d", len(decoded), len(row))
		}
		for i := range row {
			if decoded[i].Kind() != row[i].Kind() {
				t.Fatalf("column %d came back as %s, want %s", i, decoded[i].Kind(), row[i].Kind())
			}
			if !Equal(decoded[i], row[i]) {
				t.Fatalf("column %d came back as %v, want %v", i, decoded[i], row[i])
			}
		}
	}
}

func TestRowEncodingIsAppendedInPlace(t *testing.T) {
	prefix := []byte("header")
	encoded := AppendRow(prefix, []Value{Integer(42)})
	if !bytes.HasPrefix(encoded, prefix) {
		t.Fatal("AppendRow overwrote the bytes it was given")
	}
	decoded, err := DecodeRow(encoded[len(prefix):], 1)
	if err != nil {
		t.Fatalf("DecodeRow: %v", err)
	}
	if decoded[0].Int() != 42 {
		t.Fatalf("value = %v", decoded[0])
	}
}

func TestDecodeRowRejectsDamagedInput(t *testing.T) {
	good := AppendRow(nil, []Value{Text("abc"), Integer(9)})
	cases := map[string][]byte{
		"truncated":         good[:len(good)-3],
		"unknown type tag":  {99, 0, 0},
		"empty":             {},
		"length past input": {byte(TypeText), 200},
	}
	for name, input := range cases {
		if _, err := DecodeRow(input, 2); err == nil {
			t.Errorf("DecodeRow of %s input succeeded", name)
		}
	}
}

func TestIndexKeyOrderMatchesValueOrder(t *testing.T) {
	values := []Value{
		Null(),
		Integer(math.MinInt64), Integer(-1000000), Integer(-1), Integer(0), Integer(1), Integer(1000000), Integer(math.MaxInt64),
		Text(""), Text("\x00"), Text("\x00\x00"), Text("a"), Text("aa"), Text("ab"), Text("b"), Text("\xff"),
		Blob(nil), Blob([]byte{0}), Blob([]byte{0, 255}), Blob([]byte{1}), Blob([]byte{255}),
	}
	// Compare every pair: the encodings must agree with Compare.
	for i, a := range values {
		for j, b := range values {
			ka, kb := AppendKey(nil, a), AppendKey(nil, b)
			want := Compare(a, b)
			got := bytes.Compare(ka, kb)
			if sign(got) != sign(want) {
				t.Fatalf("values %d and %d (%v, %v): bytes compare %d, values compare %d", i, j, a, b, got, want)
			}
		}
	}
}

func TestRealIndexKeyOrder(t *testing.T) {
	reals := []float64{math.Inf(-1), -1e300, -1.5, -1, -0.5, 0, 0.5, 1, 1.5, 1e300, math.Inf(1)}
	for i := 1; i < len(reals); i++ {
		previous := AppendKey(nil, Real(reals[i-1]))
		current := AppendKey(nil, Real(reals[i]))
		if bytes.Compare(previous, current) >= 0 {
			t.Fatalf("key for %v does not sort before key for %v", reals[i-1], reals[i])
		}
	}
}

func TestIndexKeyRoundTrips(t *testing.T) {
	values := []Value{
		Null(), Integer(0), Integer(-77), Integer(math.MaxInt64),
		Real(0), Real(-2.25), Real(math.Pi),
		Text(""), Text("with\x00zeros\x00inside"), Text("plain"),
		Blob([]byte{0, 0, 0}), Blob([]byte("bytes")),
	}
	for _, v := range values {
		encoded := AppendKey(nil, v)
		decoded, read, err := DecodeKey(encoded)
		if err != nil {
			t.Fatalf("DecodeKey(%v): %v", v, err)
		}
		if read != len(encoded) {
			t.Fatalf("DecodeKey(%v) consumed %d of %d bytes", v, read, len(encoded))
		}
		if decoded.Kind() != v.Kind() || !Equal(decoded, v) {
			t.Fatalf("key for %v came back as %v", v, decoded)
		}
	}
}

func TestIndexKeyStaysOrderedWithASuffix(t *testing.T) {
	// Index keys are the encoded column value followed by a row id and a
	// transaction id. A prefix relationship between two column values must
	// survive whatever those suffixes happen to be, which is the whole
	// reason the text encoding escapes and terminates.
	pairs := [][2]Value{
		{Text("a"), Text("ab")},
		{Text(""), Text("\x00")},
		{Text("a\x00"), Text("a\x00\x00")},
		{Blob([]byte{1}), Blob([]byte{1, 0})},
	}
	suffixes := [][]byte{
		{0, 0, 0, 0, 0, 0, 0, 0},
		{255, 255, 255, 255, 255, 255, 255, 255},
		{0x7f, 0, 0, 0, 0, 0, 0, 1},
	}
	for _, pair := range pairs {
		for _, low := range suffixes {
			for _, high := range suffixes {
				a := append(AppendKey(nil, pair[0]), low...)
				b := append(AppendKey(nil, pair[1]), high...)
				if bytes.Compare(a, b) >= 0 {
					t.Fatalf("key for %v with suffix %x does not sort before %v with suffix %x", pair[0], low, pair[1], high)
				}
			}
		}
	}
}

func TestIndexKeyOrderAgainstRandomStrings(t *testing.T) {
	r := rand.New(rand.NewSource(7))
	values := make([]Value, 500)
	for i := range values {
		n := r.Intn(12)
		b := make([]byte, n)
		for j := range b {
			// Bias towards the bytes the escaping cares about.
			switch r.Intn(3) {
			case 0:
				b[j] = 0x00
			case 1:
				b[j] = 0xFF
			default:
				b[j] = byte(r.Intn(256))
			}
		}
		values[i] = Blob(b)
	}
	byValue := append([]Value(nil), values...)
	sort.Slice(byValue, func(i, j int) bool { return Compare(byValue[i], byValue[j]) < 0 })
	byKey := append([]Value(nil), values...)
	sort.Slice(byKey, func(i, j int) bool {
		return bytes.Compare(AppendKey(nil, byKey[i]), AppendKey(nil, byKey[j])) < 0
	})
	for i := range byValue {
		if !Equal(byValue[i], byKey[i]) {
			t.Fatalf("sorting by key and by value disagree at position %d", i)
		}
	}
}

func TestDecodeKeyRejectsDamagedInput(t *testing.T) {
	cases := map[string][]byte{
		"empty":             {},
		"unknown tag":       {77},
		"truncated integer": {keyTagInteger, 1, 2},
		"truncated real":    {keyTagReal, 1},
		"unterminated text": {keyTagText, 'a', 'b'},
		"escape at end":     {keyTagText, 0x00},
		"invalid escape":    {keyTagText, 0x00, 0x41, 0x00, 0x00},
	}
	for name, input := range cases {
		if _, _, err := DecodeKey(input); err == nil {
			t.Errorf("DecodeKey of %s input succeeded", name)
		}
	}
}

func TestCompareOrdersTypeClasses(t *testing.T) {
	ordered := []Value{Null(), Integer(-1), Real(0.5), Integer(1), Text("a"), Blob([]byte("a"))}
	for i := 1; i < len(ordered); i++ {
		if Compare(ordered[i-1], ordered[i]) >= 0 {
			t.Fatalf("%v does not sort before %v", ordered[i-1], ordered[i])
		}
	}
}

func TestCompareIsNumericAcrossIntegerAndReal(t *testing.T) {
	if Compare(Integer(2), Real(2.0)) != 0 {
		t.Fatal("2 and 2.0 should compare equal")
	}
	if Compare(Integer(2), Real(2.5)) >= 0 {
		t.Fatal("2 should sort before 2.5")
	}
	if Compare(Real(-0.5), Integer(0)) >= 0 {
		t.Fatal("-0.5 should sort before 0")
	}
	// Very large integers cannot be widened to float64 without loss, so they
	// are compared as integers when both sides are integers.
	if Compare(Integer(math.MaxInt64), Integer(math.MaxInt64-1)) <= 0 {
		t.Fatal("large integers must compare exactly")
	}
}

func TestTruthyFollowsSQLSemantics(t *testing.T) {
	cases := map[string]struct {
		v    Value
		want bool
	}{
		"null":       {Null(), false},
		"zero":       {Integer(0), false},
		"one":        {Integer(1), true},
		"zero real":  {Real(0), false},
		"empty text": {Text(""), false},
		"text":       {Text("x"), true},
		"empty blob": {Blob(nil), false},
	}
	for name, c := range cases {
		if got := c.v.Truthy(); got != c.want {
			t.Errorf("%s: Truthy() = %v, want %v", name, got, c.want)
		}
	}
}

func TestStringRendering(t *testing.T) {
	cases := []struct {
		v    Value
		want string
	}{
		{Null(), "NULL"},
		{Integer(-12), "-12"},
		{Real(1), "1.0"},
		{Real(1.25), "1.25"},
		{Real(math.NaN()), "NaN"},
		{Text("hi"), "hi"},
		{Blob([]byte{0xde, 0xad}), "x'dead'"},
	}
	for _, c := range cases {
		if got := c.v.String(); got != c.want {
			t.Errorf("String() of %s value = %q, want %q", c.v.Kind(), got, c.want)
		}
	}
	if got := Text("it's").SQL(); got != "'it''s'" {
		t.Errorf("SQL() = %q, want %q", got, "'it''s'")
	}
}

func TestParseType(t *testing.T) {
	for name, want := range map[string]Type{
		"integer": TypeInteger, "INT": TypeInteger,
		"real": TypeReal, "Text": TypeText, "BLOB": TypeBlob,
	} {
		got, ok := ParseType(name)
		if !ok || got != want {
			t.Errorf("ParseType(%q) = (%v, %v), want (%v, true)", name, got, ok, want)
		}
	}
	if _, ok := ParseType("datetime"); ok {
		t.Error("ParseType accepted an unknown type")
	}
}

// sign reduces a comparison result to -1, 0 or 1.
func sign(n int) int {
	switch {
	case n < 0:
		return -1
	case n > 0:
		return 1
	default:
		return 0
	}
}
