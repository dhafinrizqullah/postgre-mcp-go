package pg

import "testing"

// TestParseVersion covers the shapes a real server_version string takes. The
// tools branch on this number, so a wrong answer silently picks the wrong
// pg_stat_statements columns.
func TestParseVersion(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in           string
		major, minor int
		ok           bool
	}{
		{in: "18.6", major: 18, minor: 6, ok: true},
		{in: "18.6 (Homebrew)", major: 18, minor: 6, ok: true},
		{in: "16.4 (Debian 16.4-1.pgdg120+1)", major: 16, minor: 4, ok: true},
		{in: "9.6.24", major: 9, minor: 6, ok: true},
		{in: "14", major: 14, ok: true},
		{in: "16beta1", major: 16, ok: true},
		{in: "9.6beta1", major: 9, minor: 6, ok: true},
		{in: "", ok: false},
		{in: "unknown", ok: false},
		{in: "beta", ok: false},
	}
	for _, tc := range cases {
		major, minor, ok := parseVersion(tc.in)
		if ok != tc.ok || major != tc.major || minor != tc.minor {
			t.Errorf("parseVersion(%q) = %d, %d, %v; want %d, %d, %v",
				tc.in, major, minor, ok, tc.major, tc.minor, tc.ok)
		}
	}
}

// TestHypotheticalIndexDefinition checks the one place this package builds SQL
// out of caller input. Anything that gets past Definition reaches a hypopg call.
func TestHypotheticalIndexDefinition(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   HypotheticalIndex
		want string
		bad  bool
	}{
		{name: "single column defaults to btree",
			in:   HypotheticalIndex{Table: "users", Columns: []string{"email"}},
			want: "btree (users (email))"},
		{name: "method and order are kept",
			in:   HypotheticalIndex{Table: "orders", Columns: []string{"user_id", "created_at DESC"}, Using: "gist"},
			want: "gist (orders (user_id, created_at DESC))"},
		{name: "missing table is rejected",
			in: HypotheticalIndex{Columns: []string{"a"}}, bad: true},
		{name: "missing columns are rejected",
			in: HypotheticalIndex{Table: "users"}, bad: true},
		{name: "injected definition is rejected",
			in: HypotheticalIndex{Table: "users", Columns: []string{"a)) AS SELECT 1 --"}}, bad: true},
		{name: "empty column is rejected",
			in: HypotheticalIndex{Table: "users", Columns: []string{""}}, bad: true},
	}
	for _, tc := range cases {
		got, err := tc.in.Definition()
		if tc.bad {
			if err == nil {
				t.Errorf("%s: Definition() = %q, want error", tc.name, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: Definition() error = %v", tc.name, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%s: Definition() = %q, want %q", tc.name, got, tc.want)
		}
	}
}
