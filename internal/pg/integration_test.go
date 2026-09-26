package pg

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// These tests run against a real database, because the things most likely to
// break are Postgres-version differences: renamed statistics columns, changed
// function signatures, catalog columns that moved. A mock proves none of that.
//
// They skip unless TEST_DATABASE_URL points at a throwaway database, because the
// fixtures need write access. The server itself never does.
func testDB(t *testing.T) *DB {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set TEST_DATABASE_URL to run the integration tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	db, err := Connect(ctx, Options{
		DSN:              dsn,
		MaxConns:         4,
		StatementTimeout: 30 * time.Second,
		ApplicationName:  "postgres-mcp-go-test",
	})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(db.Close)
	return db
}

// adminConn connects without the read-only settings, for fixture setup only.
func adminConn(t *testing.T) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatalf("admin connect: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return conn
}

const fixtureSchema = "mcp_itest"

// setupFixtures builds a small database that trips every health check: a
// duplicated index, an invalid constraint, a sequence near its ceiling, and a
// replica-shaped pg_stat_replication absence.
func setupFixtures(t *testing.T) {
	t.Helper()
	conn := adminConn(t)
	ctx := context.Background()

	_, err := conn.Exec(ctx, `DROP SCHEMA IF EXISTS `+fixtureSchema+` CASCADE`)
	if err != nil {
		t.Fatalf("drop fixture schema: %v", err)
	}
	stmts := []string{
		`CREATE SCHEMA ` + fixtureSchema,
		`CREATE TABLE ` + fixtureSchema + `.orders (
			id serial PRIMARY KEY,
			user_id int NOT NULL,
			amount numeric(10,2) NOT NULL DEFAULT 0,
			note text)`,
		`CREATE TABLE ` + fixtureSchema + `.users (id serial PRIMARY KEY, email text NOT NULL)`,
		`INSERT INTO ` + fixtureSchema + `.users(email) SELECT 'u'||g||'@example.com' FROM generate_series(1,200) g`,
		`INSERT INTO ` + fixtureSchema + `.orders(user_id, amount, note)
		 SELECT (random()*199)::int+1, random()*100, 'note-'||g FROM generate_series(1,500) g`,
		`CREATE INDEX orders_user_id_idx ON ` + fixtureSchema + `.orders(user_id)`,
		`CREATE INDEX orders_user_id_dup ON ` + fixtureSchema + `.orders(user_id)`,
		`CREATE INDEX orders_note_idx ON ` + fixtureSchema + `.orders(note)`,
		`ALTER TABLE ` + fixtureSchema + `.orders ADD CONSTRAINT amount_nonneg CHECK (amount >= 0) NOT VALID`,
		// Near its ceiling: max_value 200 with last_value 195.
		`CREATE SEQUENCE ` + fixtureSchema + `.tiny_seq MAXVALUE 200 START 195 INCREMENT 1`,
		`SELECT setval('` + fixtureSchema + `.tiny_seq', 195)`,
	}
	for _, stmt := range stmts {
		if _, err := conn.Exec(ctx, stmt); err != nil {
			t.Fatalf("fixture %q: %v", firstLine(stmt), err)
		}
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// TestIntegrationReadOnly is the assertion that matters most: the pool must
// refuse writes even when a statement somehow reaches it.
func TestIntegrationReadOnly(t *testing.T) {
	db := testDB(t)
	setupFixtures(t)
	ctx := context.Background()

	if _, err := db.Query(ctx, "SELECT 1 AS one"); err != nil {
		t.Fatalf("SELECT through the read-only pool failed: %v", err)
	}
	for _, stmt := range []string{
		`INSERT INTO ` + fixtureSchema + `.users(email) VALUES ('nope@example.com')`,
		`UPDATE ` + fixtureSchema + `.orders SET amount = 1`,
		`DELETE FROM ` + fixtureSchema + `.orders`,
		`CREATE TABLE ` + fixtureSchema + `.nope (a int)`,
		`DROP TABLE ` + fixtureSchema + `.orders`,
	} {
		if _, err := db.Query(ctx, stmt); err == nil {
			t.Errorf("read-only pool accepted %q", firstLine(stmt))
		}
	}
}

func TestIntegrationCatalog(t *testing.T) {
	db := testDB(t)
	setupFixtures(t)
	ctx := context.Background()

	schemas, err := db.ListSchemas(ctx)
	if err != nil {
		t.Fatalf("ListSchemas: %v", err)
	}
	if !containsName(schemas, fixtureSchema) {
		t.Errorf("ListSchemas did not return %s", fixtureSchema)
	}

	tables, err := db.ListObjects(ctx, fixtureSchema, "table")
	if err != nil {
		t.Fatalf("ListObjects: %v", err)
	}
	if len(tables) != 2 {
		t.Errorf("ListObjects returned %d tables, want 2", len(tables))
	}

	details, err := db.GetObjectDetails(ctx, fixtureSchema, "orders", "table")
	if err != nil {
		t.Fatalf("GetObjectDetails: %v", err)
	}
	if len(details.Columns) != 4 {
		t.Errorf("got %d columns, want 4", len(details.Columns))
	}
	// Three explicit indexes plus the primary key's.
	if len(details.Indexes) != 4 {
		t.Errorf("got %d indexes, want 4", len(details.Indexes))
	}
	if len(details.Constraints) == 0 {
		t.Error("got no constraints, want at least the NOT VALID one")
	}

	if _, err := db.GetObjectDetails(ctx, fixtureSchema, "does_not_exist", "table"); err == nil {
		t.Error("GetObjectDetails accepted a missing table")
	}
	if _, err := db.ListObjects(ctx, fixtureSchema, "trigger"); err == nil {
		t.Error("ListObjects accepted an unsupported object type")
	}
}

func containsName(schemas []Schema, name string) bool {
	for _, s := range schemas {
		if s.Name == name {
			return true
		}
	}
	return false
}

// TestIntegrationHealth checks that every sub-check runs, and that the two
// findings the fixtures plant are actually reported.
func TestIntegrationHealth(t *testing.T) {
	db := testDB(t)
	setupFixtures(t)
	ctx := context.Background()

	checks, err := db.Health(ctx, nil)
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if len(checks) != len(healthChecks) {
		t.Fatalf("got %d checks, want %d", len(checks), len(healthChecks))
	}
	byName := map[string]HealthCheck{}
	for _, check := range checks {
		if check.Summary == "check failed" {
			t.Errorf("check %s failed: %s", check.Name, check.Findings[0].Message)
		}
		byName[check.Name] = check
	}

	assertFinding(t, byName["index"], "interchangeable indexes", "orders_user_id_dup")
	assertFinding(t, byName["constraint"], "NOT VALID", "amount_nonneg")
	assertFinding(t, byName["sequence"], "values left", "tiny_seq")

	if _, err := db.Health(ctx, []string{"notacheck"}); err == nil {
		t.Error("Health accepted an unknown check name")
	}
	subset, err := db.Health(ctx, []string{"index, vacuum"})
	if err != nil {
		t.Fatalf("Health subset: %v", err)
	}
	if len(subset) != 2 {
		t.Errorf("got %d checks for a two-name request, want 2", len(subset))
	}
}

func assertFinding(t *testing.T, check HealthCheck, substrings ...string) {
	t.Helper()
	for _, finding := range check.Findings {
		matched := true
		for _, want := range substrings {
			if !strings.Contains(finding.Message, want) {
				matched = false
				break
			}
		}
		if matched {
			return
		}
	}
	t.Errorf("check %s has no finding containing %v; got %d findings", check.Name, substrings, len(check.Findings))
}

// TestIntegrationIndexBloatSQL runs the bloat query with the size threshold
// removed. The threshold logic is trivial; what can break is pgstatindex's
// signature, which changed in PostgreSQL 18.
func TestIntegrationIndexBloatSQL(t *testing.T) {
	db := testDB(t)
	setupFixtures(t)
	ctx := context.Background()

	has, err := db.HasExtension(ctx, "pgstattuple")
	if err != nil {
		t.Fatalf("HasExtension: %v", err)
	}
	if !has {
		t.Skip("pgstattuple is not installed")
	}
	rows, err := db.Query(ctx, `
		SELECT
			n.nspname AS schemaname,
			t.relname AS tablename,
			ic.relname AS indexname,
			pg_relation_size(ic.oid) AS index_size,
			s.avg_leaf_density, s.leaf_fragmentation
		FROM pg_index i
		JOIN pg_class ic ON ic.oid = i.indexrelid
		JOIN pg_class t ON t.oid = i.indrelid
		JOIN pg_namespace n ON n.oid = t.relnamespace
		JOIN LATERAL pgstatindex(ic.oid) s ON true
		WHERE n.nspname = $1
		LIMIT 5`, fixtureSchema)
	if err != nil {
		t.Fatalf("pgstatindex query failed, which means the bloat check is broken on this server: %v", err)
	}
	if len(rows) == 0 {
		t.Error("pgstatindex returned no rows for the fixture indexes")
	}
	for _, row := range rows {
		if row["avg_leaf_density"] == nil {
			t.Errorf("index %v has no avg_leaf_density", row["indexname"])
		}
	}
}

func TestIntegrationExplain(t *testing.T) {
	db := testDB(t)
	setupFixtures(t)
	ctx := context.Background()

	plan, err := db.Explain(ctx, "SELECT * FROM "+fixtureSchema+".orders WHERE user_id = 3", nil)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if !strings.Contains(string(plan), "user_id") {
		t.Errorf("plan does not mention the filtered column: %s", plan)
	}
}

func TestIntegrationExplainHypotheticalIndexes(t *testing.T) {
	db := testDB(t)
	setupFixtures(t)
	ctx := context.Background()

	has, err := db.HasExtension(ctx, "hypopg")
	if err != nil {
		t.Fatalf("HasExtension: %v", err)
	}
	if !has {
		t.Skip("hypopg is not installed")
	}

	plan, err := db.Explain(ctx,
		"SELECT * FROM "+fixtureSchema+".orders WHERE note = 'note-7'",
		[]HypotheticalIndex{{Table: fixtureSchema + ".orders", Columns: []string{"note", "user_id"}}})
	if err != nil {
		t.Fatalf("Explain with hypothetical indexes: %v", err)
	}
	// hypopg names simulated indexes with a <NNNNN>btree_<table>_<cols> tag, so
	// seeing one in the plan proves the index reached the planner.
	if !strings.Contains(string(plan), "btree_"+fixtureSchema+"_orders") {
		t.Errorf("plan does not contain the hypothetical index: %s", plan)
	}
}

func TestIntegrationTopQueries(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	has, err := db.HasExtension(ctx, "pg_stat_statements")
	if err != nil {
		t.Fatalf("HasExtension: %v", err)
	}
	if !has {
		t.Skip("pg_stat_statements is not installed")
	}
	setupFixtures(t)
	if _, err := db.TopQueries(ctx, "resources", 5); err != nil {
		t.Fatalf("TopQueries: %v", err)
	}
	for _, sortBy := range []string{"total_time", "mean_time", "resources"} {
		if _, err := db.TopQueries(ctx, sortBy, 5); err != nil {
			t.Errorf("TopQueries(%s): %v", sortBy, err)
		}
	}
	if _, err := db.TopQueries(ctx, "nonsense", 5); err == nil {
		t.Error("TopQueries accepted an unknown sort_by")
	}
}
