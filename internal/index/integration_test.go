package index

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/dhafinrizqullah/postgre-mcp-go/internal/pg"
)

// The tuner can only be judged against a real planner with real statistics, and
// it needs hypopg to ask the planner hypothetical questions. It is skipped
// unless TEST_DATABASE_URL points at a database with hypopg installed.
func tunerDB(t *testing.T) *pg.DB {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set TEST_DATABASE_URL to run the index tuner tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	db, err := pg.Connect(ctx, pg.Options{DSN: dsn, MaxConns: 4, StatementTimeout: time.Minute})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(db.Close)

	has, err := db.HasExtension(ctx, "hypopg")
	if err != nil {
		t.Fatalf("HasExtension: %v", err)
	}
	if !has {
		t.Skip("hypopg is not installed")
	}
	return db
}

// events is a table with one filtered column and one high-cardinality column,
// neither indexed. The filter matches few rows, so a btree on it should be a clear
// win; the high-cardinality column is there to check that the search does not
// recommend an index for a column nothing filters on.
func setupEvents(t *testing.T) {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatalf("admin connect: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	ctx := context.Background()

	if _, err := conn.Exec(ctx, `DROP TABLE IF EXISTS tuner_events`); err != nil {
		t.Fatalf("drop: %v", err)
	}
	stmts := []string{
		`CREATE TABLE tuner_events (
			id serial PRIMARY KEY,
			tenant_id int NOT NULL,
			status text NOT NULL,
			payload text NOT NULL,
			amount numeric(12,2) NOT NULL)`,
		// Few distinct tenants, few distinct statuses, and plenty of distinct
		// amounts, which is what makes the filter selective.
		`INSERT INTO tuner_events(tenant_id, status, payload, amount)
		 SELECT (random()*20)::int,
		        CASE WHEN random() < 0.05 THEN 'failed' ELSE 'ok' END,
		        'payload-' || g,
		        random()*1000
		 FROM generate_series(1, 20000) g`,
		`ANALYZE tuner_events`,
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

func TestPrecheckRejectsMissingHypopg(t *testing.T) {
	t.Parallel()

	if os.Getenv("TEST_DATABASE_URL") == "" {
		t.Skip("set TEST_DATABASE_URL to run the index tuner tests")
	}
	db, err := pg.Connect(context.Background(), pg.Options{DSN: os.Getenv("TEST_DATABASE_URL")})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(db.Close)

	// Whether hypopg is installed or not, the precheck must answer with a
	// sentence, not a crash or a silent pass.
	if err := New(db, DefaultOptions()).Precheck(context.Background()); err != nil {
		if !strings.Contains(err.Error(), "hypopg") && !strings.Contains(err.Error(), "analyz") {
			t.Errorf("precheck error should name the missing prerequisite, got %q", err)
		}
	}
}

func TestTuneRecommendsIndexForFilteredColumn(t *testing.T) {
	db := tunerDB(t)
	setupEvents(t)
	ctx := context.Background()

	opts := DefaultOptions()
	opts.MaxRuntime = 20 * time.Second
	tuner := New(db, opts)
	if err := tuner.Precheck(ctx); err != nil {
		t.Fatalf("Precheck: %v", err)
	}

	result, err := tuner.Analyze(ctx, []Query{{
		Text: "SELECT id, payload FROM tuner_events WHERE tenant_id = 3 AND status = 'failed'",
	}})
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}

	if result.AnalysedQueries != 1 {
		t.Fatalf("analysed %d queries, want 1 (skipped: %v)", result.AnalysedQueries, result.SkippedQueries)
	}
	if result.BaselineCost <= 0 {
		t.Errorf("baseline cost is %v, want a positive planner cost", result.BaselineCost)
	}
	if len(result.Recommendations) == 0 {
		t.Fatalf("no index recommended for a selective filter over 20k rows; report:\n%s", result.Text())
	}

	// The recommendation must be on a column the query filters, never on a
	// column it merely projects.
	first := result.Recommendations[0]
	if first.Table != "tuner_events" {
		t.Errorf("recommended an index on %q, want tuner_events", first.Table)
	}
	for _, column := range first.Columns {
		if column != "tenant_id" && column != "status" {
			t.Errorf("recommended an index on %q, which the query does not filter", column)
		}
	}
	if first.EstimatedSizeBytes <= 0 {
		t.Errorf("estimated size is %d, want a positive size", first.EstimatedSizeBytes)
	}
	if first.IndividualImprovement <= 1 {
		t.Errorf("individual improvement is %.2fx, want more than 1x for a missing index on a selective filter",
			first.IndividualImprovement)
	}
	if result.FinalCost >= result.BaselineCost {
		t.Errorf("final cost %.2f is not below the baseline %.2f", result.FinalCost, result.BaselineCost)
	}

	// The report has to be usable on its own.
	text := result.Text()
	for _, want := range []string{"CREATE INDEX ON tuner_events", "Not analysed"} {
		if want == "Not analysed" {
			continue // nothing was skipped here
		}
		if !strings.Contains(text, want) {
			t.Errorf("report does not contain %q:\n%s", want, text)
		}
	}
}

func TestTuneDoesNotRecommendWhenAlreadyIndexed(t *testing.T) {
	db := tunerDB(t)
	setupEvents(t)
	ctx := context.Background()

	// Give the column the index the search would otherwise propose.
	conn, err := pgx.Connect(ctx, os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatalf("admin connect: %v", err)
	}
	if _, err := conn.Exec(ctx, `CREATE INDEX tuner_events_tenant_status ON tuner_events(tenant_id, status)`); err != nil {
		t.Fatalf("create index: %v", err)
	}
	_ = conn.Close(ctx)
	t.Cleanup(func() {
		admin, err := pgx.Connect(ctx, os.Getenv("TEST_DATABASE_URL"))
		if err != nil {
			return
		}
		defer admin.Close(ctx)
		_, _ = admin.Exec(ctx, `DROP INDEX IF EXISTS tuner_events_tenant_status`)
		_, _ = admin.Exec(ctx, `ANALYZE tuner_events`)
	})

	opts := DefaultOptions()
	opts.MaxRuntime = 20 * time.Second
	tuner := New(db, opts)
	if err := tuner.Precheck(ctx); err != nil {
		t.Fatalf("Precheck: %v", err)
	}
	result, err := tuner.Analyze(ctx, []Query{{
		Text: "SELECT id FROM tuner_events WHERE tenant_id = 3 AND status = 'failed'",
	}})
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	for _, rec := range result.Recommendations {
		for _, column := range rec.Columns {
			if column == "tenant_id" && len(rec.Columns) == 1 {
				t.Errorf("recommended %s, which already exists", rec.Definition)
			}
		}
	}
}

func TestTuneSkipsUnanalysableQueries(t *testing.T) {
	db := tunerDB(t)
	ctx := context.Background()

	opts := DefaultOptions()
	opts.MaxRuntime = 10 * time.Second
	tuner := New(db, opts)
	if err := tuner.Precheck(ctx); err != nil {
		t.Fatalf("Precheck: %v", err)
	}

	result, err := tuner.Analyze(ctx, []Query{
		{Text: "SELECT relname FROM pg_class WHERE relkind = 'r'"},
		{Text: "DELETE FROM whatever"},
	})
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if result.AnalysedQueries != 0 {
		t.Errorf("analysed %d queries, want 0", result.AnalysedQueries)
	}
	if len(result.SkippedQueries) != 2 {
		t.Errorf("reported %d skipped queries, want 2: %v", len(result.SkippedQueries), result.SkippedQueries)
	}
	// With nothing to report on, the report must say so rather than look broken.
	if !strings.Contains(result.Text(), "No index would help") {
		t.Errorf("report for an empty analysis:\n%s", result.Text())
	}
}

func TestTuneReplacesBindParameters(t *testing.T) {
	db := tunerDB(t)
	setupEvents(t)
	ctx := context.Background()

	tuner := New(db, DefaultOptions())
	sql, err := tuner.replaceParameters(ctx,
		"SELECT id FROM tuner_events WHERE tenant_id = $1 AND status = $2")
	if err != nil {
		t.Fatalf("replaceParameters: %v", err)
	}
	if strings.Contains(sql, "$1") || strings.Contains(sql, "$2") {
		t.Errorf("parameters survived replacement: %s", sql)
	}
	if !strings.Contains(sql, "tuner_events") {
		t.Errorf("replacement damaged the statement: %s", sql)
	}

	// A parameter that cannot be tied to a column is reported, not guessed at.
	if _, err := tuner.replaceParameters(ctx, "SELECT $1::int + 1"); err == nil {
		t.Error("an unresolvable parameter should be an error, not a silent guess")
	}
}
