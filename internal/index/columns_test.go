package index

import (
	"sort"
	"strings"
	"testing"

	pg_query "github.com/pganalyze/pg_query_go/v6"
)

// stubResolver answers column ownership from a fixed table, standing in for the
// catalog lookup the real extractor does.
type stubResolver struct {
	owner string
	seen  []string
}

func (s *stubResolver) Owner(_, column string, candidates []string) string {
	s.seen = append(s.seen, column+":"+strings.Join(candidates, "|"))
	return s.owner
}

func extract(t *testing.T, sql string, resolver ColumnResolver) *Columns {
	t.Helper()
	tree, err := pg_query.Parse(sql)
	if err != nil {
		t.Fatalf("parse %q: %v", sql, err)
	}
	if len(tree.GetStmts()) != 1 {
		t.Fatalf("%q produced %d statements", sql, len(tree.GetStmts()))
	}
	sel, ok := unwrap(tree.GetStmts()[0].GetStmt()).(*pg_query.SelectStmt)
	if !ok {
		t.Fatalf("%q is not a SELECT", sql)
	}
	return ExtractColumns(sel, resolver)
}

// keys renders the collected columns as "table.column:filtered" strings.
func keys(c *Columns) string {
	parts := make([]string, 0, c.Len())
	for _, u := range c.All() {
		mark := "projected"
		if u.Filtered {
			mark = "filtered"
		}
		parts = append(parts, u.Table+"."+u.Column+":"+mark)
	}
	sort.Strings(parts)
	return strings.Join(parts, " ")
}

func TestExtractColumns(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		sql      string
		resolver ColumnResolver
		want     string
	}{
		{
			name: "filter and projection are distinguished",
			sql:  "SELECT id FROM users WHERE email = 'x@y.z'",
			want: "users.email:filtered users.id:projected",
		},
		{
			name: "a star carries no column",
			sql:  "SELECT * FROM users",
			want: "",
		},
		{
			name: "qualified columns resolve through the alias",
			sql:  "SELECT u.id FROM users u JOIN orders o ON o.user_id = u.id WHERE o.amount > 5",
			want: "orders.amount:filtered orders.user_id:filtered users.id:filtered",
		},
		{
			name: "an unqualified column with one table needs no catalog",
			sql:  "SELECT id FROM users WHERE email = 'x'",
			want: "users.email:filtered users.id:projected",
		},
		{
			name:     "an unqualified column with several tables asks the catalog",
			sql:      "SELECT id FROM users u JOIN orders o ON true WHERE email = 'x'",
			resolver: &stubResolver{owner: "users"},
			want:     "users.email:filtered users.id:projected",
		},
		{
			name: "without a resolver an ambiguous column goes to every table",
			sql:  "SELECT id FROM users u JOIN orders o ON true WHERE email = 'x'",
			want: "orders.email:filtered orders.id:projected users.email:filtered users.id:projected",
		},
		{
			name: "a CTE name is never treated as an indexable table",
			sql:  "WITH x AS (SELECT user_id FROM orders WHERE amount > 1) SELECT * FROM x WHERE x.user_id = 1",
			want: "orders.amount:filtered orders.user_id:projected",
		},
		{
			name: "subquery scope does not leak into the outer query",
			sql:  "SELECT * FROM (SELECT user_id FROM orders WHERE amount > 5) t WHERE t.user_id = 1",
			want: "orders.amount:filtered orders.user_id:projected",
		},
		{
			name: "a CTE body is walked, its name is not an index target",
			sql:  "WITH x AS (SELECT user_id FROM orders WHERE amount > 1) SELECT * FROM x",
			want: "orders.amount:filtered orders.user_id:projected",
		},
		{
			name: "order by an alias reaches the real column",
			sql:  "SELECT user_id AS uid FROM orders ORDER BY uid",
			want: "orders.user_id:filtered",
		},
		{
			name: "group by a target position reaches the real column",
			sql:  "SELECT user_id FROM orders GROUP BY 1",
			want: "orders.user_id:filtered",
		},
		{
			name: "join using filters both sides",
			sql:  "SELECT * FROM users JOIN orders USING (id)",
			want: "orders.id:filtered users.id:filtered",
		},
		{
			name: "window partition and order keys are candidates",
			sql:  "SELECT row_number() OVER (PARTITION BY user_id ORDER BY created_at) FROM orders",
			want: "orders.created_at:filtered orders.user_id:filtered",
		},
		{
			name: "aggregate filter clause is a filter",
			sql:  "SELECT count(*) FILTER (WHERE amount > 0) FROM orders",
			want: "orders.amount:filtered",
		},
		{
			name: "both arms of a union are walked",
			sql:  "SELECT a FROM t WHERE b = 1 UNION ALL SELECT c FROM t WHERE d = 2",
			want: "t.a:projected t.b:filtered t.c:projected t.d:filtered",
		},
		{
			name: "a projected column in a function is not a filter",
			sql:  "SELECT lower(email) FROM users",
			want: "users.email:projected",
		},
		{
			name: "having filters",
			sql:  "SELECT user_id, count(*) FROM orders GROUP BY user_id HAVING count(*) > 5",
			want: "orders.user_id:filtered",
		},
		{
			name: "schema-qualified reference keeps the table",
			sql:  "SELECT public.orders.id FROM public.orders WHERE public.orders.amount > 1",
			want: "orders.amount:filtered orders.id:projected",
		},
		{
			name: "system catalogs are still walked, the caller filters them",
			sql:  "SELECT relname FROM pg_class WHERE relkind = 'r'",
			want: "pg_class.relkind:filtered pg_class.relname:projected",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := keys(extract(t, tc.sql, tc.resolver))
			if got != tc.want {
				t.Errorf("ExtractColumns(%q)\n got: %s\nwant: %s", tc.sql, got, tc.want)
			}
		})
	}
}

// TestExtractColumnsUsesResolver checks the resolver is actually consulted, and
// with the tables in scope, rather than a column being attributed by luck.
func TestExtractColumnsUsesResolver(t *testing.T) {
	t.Parallel()

	resolver := &stubResolver{owner: "orders"}
	extract(t, "SELECT o.id FROM users u JOIN orders o ON true WHERE email = 'x'", resolver)
	if len(resolver.seen) == 0 {
		t.Fatal("resolver was never asked")
	}
	if got := resolver.seen[0]; got != "email:orders|users" {
		t.Errorf("resolver asked %q, want %q", got, "email:orders|users")
	}
}

func TestTablesIn(t *testing.T) {
	t.Parallel()

	sel := mustSelect(t, "SELECT * FROM users u JOIN orders o ON o.user_id = u.id WHERE o.amount > 1")
	got := TablesIn(sel)
	want := []string{"orders", "users"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("TablesIn = %v, want %v", got, want)
	}
}

func TestIsSystemTable(t *testing.T) {
	t.Parallel()

	for name, want := range map[string]bool{
		"pg_class":             true,
		"information_schema":   true,
		"aurora_replica_index": true,
		"orders":               false,
		"pghero_metadata":      false,
	} {
		if got := isSystemTable(name); got != want {
			t.Errorf("isSystemTable(%q) = %v, want %v", name, got, want)
		}
	}
}

func mustSelect(t *testing.T, sql string) *pg_query.SelectStmt {
	t.Helper()
	tree, err := pg_query.Parse(sql)
	if err != nil {
		t.Fatalf("parse %q: %v", sql, err)
	}
	sel, ok := unwrap(tree.GetStmts()[0].GetStmt()).(*pg_query.SelectStmt)
	if !ok {
		t.Fatalf("%q is not a SELECT", sql)
	}
	return sel
}
