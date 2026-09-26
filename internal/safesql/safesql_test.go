package safesql

import (
	"strings"
	"testing"
)

// TestValidate is the security boundary of this server, so it is the one test
// that earns its keep. It runs without a database: everything here is decided by
// the parser and the allowlists in allowlist_gen.go.
func TestValidate(t *testing.T) {
	t.Parallel()

	allowed := []string{
		"SELECT 1",
		"SELECT * FROM users WHERE id = 1",
		"SELECT id, email FROM users ORDER BY created_at DESC LIMIT 10",
		"SELECT count(*) FROM orders o JOIN users u ON u.id = o.user_id GROUP BY o.user_id",
		"SELECT pg_catalog.count(*) FROM orders",
		"SELECT a FROM t UNION ALL SELECT b FROM u",
		"SELECT a FROM t INTERSECT SELECT b FROM u",
		"WITH recent AS (SELECT id FROM orders WHERE created_at > now() - interval '1 day') SELECT * FROM recent",
		"SELECT row_number() OVER (PARTITION BY a ORDER BY b) FROM t",
		"SELECT CASE WHEN a > 1 THEN 'x' ELSE 'y' END, coalesce(b, 0), ARRAY[1,2,3] FROM t",
		"SELECT * FROM t WHERE name LIKE 'foo%'",
		"SELECT * FROM t WHERE name ILIKE 'foo%'",
		"SELECT * FROM t WHERE (SELECT max(id) FROM u) > 0",
		"SELECT * FROM t WHERE a = $1 AND b = $2",
		"SELECT version(), current_setting('server_version'), pg_table_size('t')",
		"SHOW statement_timeout",
		"ANALYZE t",
		"VACUUM ANALYZE t",
		"EXPLAIN SELECT * FROM t",
		"CREATE EXTENSION IF NOT EXISTS hypopg",
		"PREPARE p AS SELECT 1",
		"SELECT 1 /* ; DROP TABLE users */",
	}
	for _, sql := range allowed {
		if err := Validate(sql); err != nil {
			t.Errorf("Validate(%q) = %v, want nil", sql, err)
		}
	}

	rejected := []struct {
		sql  string
		want string
	}{
		// Multiple statements are the injection vector that matters most: the
		// first statement looks harmless to a human reader.
		{"SELECT 1; DROP TABLE users", "expected exactly 1 statement"},
		{"SELECT 1; SELECT 2", "expected exactly 1 statement"},
		{"SELECT 1 /* comment */; DROP TABLE users", "expected exactly 1 statement"},
		{"", "expected exactly 1 statement"},
		{"   ", "expected exactly 1 statement"},

		// Writes and DDL.
		{"INSERT INTO t VALUES (1)", "statement type InsertStmt is not allowed"},
		{"UPDATE t SET a = 1", "statement type UpdateStmt is not allowed"},
		{"DELETE FROM t", "statement type DeleteStmt is not allowed"},
		{"DROP TABLE t", "statement type DropStmt is not allowed"},
		{"CREATE TABLE t (a int)", "statement type CreateStmt is not allowed"},
		{"ALTER TABLE t ADD COLUMN b int", "statement type AlterTableStmt is not allowed"},
		{"TRUNCATE t", "statement type TruncateStmt is not allowed"},
		{"SET default_transaction_read_only = off", "statement type VariableSetStmt is not allowed"},
		{"COPY t FROM '/etc/passwd'", "statement type CopyStmt is not allowed"},
		{"SELECT * INTO stolen FROM t", "node type IntoClause is not allowed"},

		// Locking reads take row locks, so they are not read-only.
		{"SELECT * FROM t FOR UPDATE", "locking clause"},
		{"SELECT * FROM t FOR SHARE", "locking clause"},
		{"SELECT * FROM t FOR NO KEY UPDATE", "locking clause"},

		// EXPLAIN ANALYZE executes the statement.
		{"EXPLAIN ANALYZE SELECT 1", "EXPLAIN ANALYZE is not supported"},

		// Function allowlist: file, network, and session access.
		{"SELECT pg_read_file('/etc/passwd')", "is not allowed"},
		{"SELECT lo_import('/etc/passwd')", "is not allowed"},
		{"SELECT dblink_connect('host=evil', 'x')", "is not allowed"},
		{"SELECT set_config('role', 'postgres', false)", "is not allowed"},
		{"SELECT pg_terminate_backend(1)", "is not allowed"},
		{"SELECT query_to_xml('SELECT 1', true, true, '')", "is not allowed"},
		{"SELECT pg_sleep(30)", "is not allowed"},
		{"SELECT * FROM t WHERE a = my_leaky_udf(1)", "is not allowed"},

		// A parameterised LIKE pattern hides the scan the author intended.
		{"SELECT * FROM t WHERE name LIKE $1", "LIKE pattern must be a constant string"},
		{"SELECT * FROM t WHERE name ILIKE $1", "LIKE pattern must be a constant string"},
		{"SELECT * FROM t WHERE name LIKE 'a' || $1", "LIKE pattern must be a constant string"},

		// Extension allowlist. dblink is on upstream's list, but every dblink
		// function is absent from the function allowlist, so it cannot connect.
		{"CREATE EXTENSION pg_serialize", "is not supported"},
		{"CREATE EXTENSION pg_failover_slots", "is not supported"},
	}
	for _, tc := range rejected {
		err := Validate(tc.sql)
		if err == nil {
			t.Errorf("Validate(%q) = nil, want error containing %q", tc.sql, tc.want)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("Validate(%q) = %q, want it to contain %q", tc.sql, err, tc.want)
		}
	}
}

func TestValidateFailsClosedOnUnknownSyntax(t *testing.T) {
	t.Parallel()

	// A malformed statement must be a clean error, not a panic and not a pass.
	for _, sql := range []string{"SELECT FROM WHERE", "SELECT (", "gibberish", ";;;", "SELECT 1)"} {
		if err := Validate(sql); err == nil {
			t.Errorf("Validate(%q) = nil, want error", sql)
		}
	}
}
