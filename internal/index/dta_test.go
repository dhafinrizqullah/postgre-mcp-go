package index

import (
	"math"
	"testing"
)

// TestSpliceLiterals covers the parts of a statement that a textual replace would
// get wrong: a parameter spelled inside a string, a comment that looks like one,
// and $10 being a different parameter from $1.
func TestSpliceLiterals(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name         string
		sql          string
		replacements []paramReplacement
		want         string
	}{
		{
			name:         "a bare parameter",
			sql:          "SELECT * FROM t WHERE id = $1",
			replacements: []paramReplacement{{"$1", "42"}},
			want:         "SELECT * FROM t WHERE id = 42",
		},
		{
			name:         "two parameters",
			sql:          "SELECT * FROM t WHERE a = $1 AND b = $2",
			replacements: []paramReplacement{{"$1", "1"}, {"$2", "'x'"}},
			want:         "SELECT * FROM t WHERE a = 1 AND b = 'x'",
		},
		{
			name:         "ten is not one",
			sql:          "SELECT * FROM t WHERE a = $10 AND b = $1",
			replacements: []paramReplacement{{"$1", "1"}, {"$10", "10"}},
			want:         "SELECT * FROM t WHERE a = 10 AND b = 1",
		},
		{
			name:         "a dollar inside a string literal is data",
			sql:          "SELECT '$1 not a param' FROM t WHERE id = $1",
			replacements: []paramReplacement{{"$1", "42"}},
			want:         "SELECT '$1 not a param' FROM t WHERE id = 42",
		},
		{
			name:         "a doubled quote does not end the literal",
			sql:          "SELECT 'it''s $1' FROM t WHERE id = $1",
			replacements: []paramReplacement{{"$1", "42"}},
			want:         "SELECT 'it''s $1' FROM t WHERE id = 42",
		},
		{
			name:         "a line comment is left alone",
			sql:          "SELECT 1 -- $1 was here\nFROM t WHERE id = $1",
			replacements: []paramReplacement{{"$1", "42"}},
			want:         "SELECT 1 -- $1 was here\nFROM t WHERE id = 42",
		},
		{
			name:         "a block comment is left alone",
			sql:          "SELECT /* $1 */ 1 FROM t WHERE id = $1",
			replacements: []paramReplacement{{"$1", "42"}},
			want:         "SELECT /* $1 */ 1 FROM t WHERE id = 42",
		},
		{
			name:         "a dollar-quoted body is left alone",
			sql:          "SELECT $tag$ $1 $tag$ FROM t WHERE id = $1",
			replacements: []paramReplacement{{"$1", "42"}},
			want:         "SELECT $tag$ $1 $tag$ FROM t WHERE id = 42",
		},
		{
			name:         "a parameter with no replacement is untouched",
			sql:          "SELECT * FROM t WHERE a = $1 AND b = $2",
			replacements: []paramReplacement{{"$1", "1"}},
			want:         "SELECT * FROM t WHERE a = 1 AND b = $2",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := spliceLiterals(tc.sql, tc.replacements)
			if got != tc.want {
				t.Errorf("spliceLiterals(%q)\n got: %q\nwant: %q", tc.sql, got, tc.want)
			}
		})
	}
}

// TestFindParams checks that a parameter is tied to the column it is compared to,
// including through a subquery and with the two operands the other way round.
func TestFindParams(t *testing.T) {
	t.Parallel()

	cases := []struct {
		sql  string
		want []paramSite
	}{
		{
			sql:  "SELECT * FROM orders WHERE user_id = $1",
			want: []paramSite{{1, "orders", "user_id"}},
		},
		{
			sql:  "SELECT * FROM orders o WHERE $1 = o.amount",
			want: []paramSite{{1, "orders", "amount"}},
		},
		{
			sql:  "SELECT * FROM users u JOIN orders o ON o.user_id = u.id WHERE o.amount > $1",
			want: []paramSite{{1, "orders", "amount"}},
		},
		{
			sql:  "SELECT * FROM orders WHERE user_id IN (SELECT id FROM users WHERE email = $1)",
			want: []paramSite{{1, "users", "email"}},
		},
		{
			sql:  "SELECT * FROM orders o JOIN users u ON u.id = o.user_id WHERE o.amount = $1 AND o.id = $2",
			want: []paramSite{{1, "orders", "amount"}, {2, "orders", "id"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.sql, func(t *testing.T) {
			t.Parallel()
			got, _ := ExtractParams(parseSelect(tc.sql))
			if len(got) != len(tc.want) {
				t.Fatalf("findParams(%q) = %v, want %v", tc.sql, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("findParams(%q)[%d] = %+v, want %+v", tc.sql, i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestConfigKeyIsOrderIndependent(t *testing.T) {
	t.Parallel()

	a := configKey([]Candidate{{Table: "t", Columns: []string{"a"}}, {Table: "t", Columns: []string{"b", "c"}}})
	b := configKey([]Candidate{{Table: "t", Columns: []string{"b", "c"}}, {Table: "t", Columns: []string{"a"}}})
	if a != b {
		t.Errorf("configKey is order dependent: %q vs %q", a, b)
	}
	// Column order within an index is not interchangeable.
	if configKey([]Candidate{{Table: "t", Columns: []string{"a", "b"}}}) ==
		configKey([]Candidate{{Table: "t", Columns: []string{"b", "a"}}}) {
		t.Error("configKey treats (a,b) and (b,a) as the same index")
	}
}

func TestObjective(t *testing.T) {
	t.Parallel()

	// The documented trade: a 1000x speedup is worth 10x the space.
	if objective(0.1, 1000, 2) >= objective(100, 100, 2) {
		t.Error("a 1000x speedup for 10x the space should beat the baseline")
	}
	// A 100x speedup for 10x the space is exactly break-even at alpha 2, which is
	// the threshold upstream documents, so neither side wins outright.
	if got, want := objective(1, 1000, 2), objective(100, 100, 2); math.Abs(got-want) > 1e-9 {
		t.Errorf("a 100x speedup for 10x the space gave %v, want break-even at %v", got, want)
	}
	// 2x the space for a 10% gain is a bad trade.
	if objective(90, 200, 2) <= objective(100, 100, 2) {
		t.Error("doubling the space for a 10% gain should be worse than not doing it")
	}
	// Degenerate inputs must not produce NaN, which would compare false for
	// everything and silently stop the search.
	for _, c := range []struct {
		cost  float64
		space int64
	}{{0, 0}, {0, 100}, {1, 0}} {
		if obj := objective(c.cost, c.space, 2); obj <= 0 {
			t.Errorf("objective(%v, %v) = %v, want +Inf", c.cost, c.space, obj)
		}
	}
}

func TestDollarTag(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{in: "$$body$$", want: "$$", ok: true},
		{in: "$tag$ body $tag$", want: "$tag$", ok: true},
		{in: "$my_tag$", want: "$my_tag$", ok: true},
		{in: "$1", ok: false},
		{in: "$1 = 1", ok: false},
		{in: "$", ok: false},
		{in: " $tag$", ok: false},
	}
	for _, tc := range cases {
		got, ok := dollarTag(tc.in)
		if got != tc.want || ok != tc.ok {
			t.Errorf("dollarTag(%q) = %q, %v; want %q, %v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}
