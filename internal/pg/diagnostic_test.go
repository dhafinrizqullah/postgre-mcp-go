package pg

import (
	"context"
	"os"
	"testing"
	"time"
)

// Temporary diagnostic: prints what each way of calling hypopg does, so the right
// one can be chosen from evidence rather than by guessing.
func TestDiagnosticHypopgCallForms(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("no TEST_DATABASE_URL")
	}
	db, err := Connect(context.Background(), Options{DSN: dsn, MaxConns: 2, StatementTimeout: time.Minute})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(db.Close)
	ctx := context.Background()

	has, err := db.HasExtension(ctx, "hypopg")
	t.Logf("hypopg installed: %v (err %v)", has, err)
	if !has {
		t.Skip("no hypopg")
	}

	rows, err := db.Query(ctx, `
		SELECT p.oid::regprocedure AS signature, p.prosrc
		FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
		WHERE n.nspname = 'public' AND p.proname LIKE 'hypopg%index%'`)
	if err != nil {
		t.Logf("cannot list functions: %v", err)
	} else {
		for _, row := range rows {
			t.Logf("function: %v", row)
		}
	}

	forms := []string{
		`SELECT hypopg_create_index('btree (tuner_events (status))')`,
		`SELECT hypopg_create_index('btree (tuner_events (status))'::text)`,
		`SELECT * FROM hypopg_create_index('btree (tuner_events (status))')`,
		`SELECT hypopg_create_index(('btree (tuner_events (status))')::cstring)`,
		`SELECT hypopg_create_index('btree (tuner_events (status))', 'idxname')`,
	}
	for _, form := range forms {
		_, err := db.WithConnErr(ctx, func(ctx context.Context, s *Session) error {
			_, qerr := s.Query(ctx, form)
			return qerr
		})
		t.Logf("form: %-70s -> %v", form, err)
	}
}
