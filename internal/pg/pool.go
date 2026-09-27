// Package pg runs every SQL statement this server sends to Postgres.
//
// The queries live here as plain SQL constants instead of behind a repository
// layer: each one has exactly one caller, so a repository would only add a
// second name for the same thing. The logic worth testing is in
// internal/safesql, which decides what may be sent at all.
package pg

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DB is a read-only connection pool.
type DB struct {
	pool *pgxpool.Pool
}

// Options configures the pool. DSN is a libpq connection string or URL;
// anything pgx understands works, including PG* environment variables.
type Options struct {
	DSN string
	// MaxConns bounds the pool. The default is small on purpose: an MCP server
	// runs one query per tool call, and a runaway agent should not be able to
	// exhaust the database's connection slots.
	MaxConns         int32
	StatementTimeout time.Duration
	ApplicationName  string
}

// Connect opens the pool and verifies it can reach the server.
func Connect(ctx context.Context, opts Options) (*DB, error) {
	cfg, err := pgxpool.ParseConfig(opts.DSN)
	if err != nil {
		return nil, fmt.Errorf("parsing DATABASE_URL: %w", err)
	}
	if opts.MaxConns > 0 {
		cfg.MaxConns = opts.MaxConns
	}
	if opts.ApplicationName != "" {
		cfg.ConnConfig.RuntimeParams["application_name"] = opts.ApplicationName
	}
	if opts.StatementTimeout > 0 {
		cfg.ConnConfig.RuntimeParams["statement_timeout"] =
			strconv.FormatInt(opts.StatementTimeout.Milliseconds(), 10)
	}
	// Layer 1 of the read-only guarantee (see internal/safesql): Postgres itself
	// refuses writes, DDL, SELECT INTO and temp tables in a read-only
	// transaction, so a bug in this package still cannot modify the database.
	cfg.ConnConfig.RuntimeParams["default_transaction_read_only"] = "on"

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connecting: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("pinging database: %w", err)
	}
	return &DB{pool: pool}, nil
}

// Close releases every pooled connection.
func (db *DB) Close() {
	db.pool.Close()
}

// Query runs one read-only statement and returns the rows as maps keyed by
// column name. Passing no args keeps pgx on the extended query protocol, where
// Postgres rejects a second statement in the same Parse message.
func (db *DB) Query(ctx context.Context, sql string, args ...any) ([]map[string]any, error) {
	return scanRows(db.pool.Query(ctx, sql, args...))
}

// Session is a single borrowed connection, for the rare tools that must run
// several statements against the same session state. hypopg's hypothetical
// indexes are the reason this exists: they live in the session, not the catalog.
type Session struct {
	conn *pgxpool.Conn
}

// Query runs one statement on the borrowed connection.
func (s *Session) Query(ctx context.Context, sql string, args ...any) ([]map[string]any, error) {
	return scanRows(s.conn.Query(ctx, sql, args...))
}

// WithConn borrows a connection, runs fn against it, and returns it to the pool.
func (db *DB) WithConn(ctx context.Context, fn func(context.Context, *Session) error) error {
	conn, err := db.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquiring connection: %w", err)
	}
	defer conn.Release()
	return fn(ctx, &Session{conn: conn})
}

// WithConnErr is WithConn for a diagnostic that only cares about the error.
func (db *DB) WithConnErr(ctx context.Context, fn func(context.Context, *Session) error) error {
	return db.WithConn(ctx, fn)
}

// scanRows drains a result set into maps keyed by column name.
func scanRows(rows pgx.Rows, err error) ([]map[string]any, error) {
	if err != nil {
		return nil, fmt.Errorf("query failed: %w", err)
	}
	defer rows.Close()

	fields := rows.FieldDescriptions()
	out := make([]map[string]any, 0, 32)
	for rows.Next() {
		values, err := rows.Values()
		if err != nil {
			return nil, fmt.Errorf("reading row: %w", err)
		}
		row := make(map[string]any, len(fields))
		for i, field := range fields {
			row[field.Name] = jsonValue(field.DataTypeOID, values[i])
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading rows: %w", err)
	}
	return out, nil
}

// QueryOne runs a statement expected to match at most one row.
func (db *DB) QueryOne(ctx context.Context, sql string, args ...any) (map[string]any, error) {
	rows, err := db.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	switch len(rows) {
	case 0:
		return nil, nil
	case 1:
		return rows[0], nil
	default:
		return nil, fmt.Errorf("expected at most 1 row, got %d", len(rows))
	}
}

// Version reports the server version as an integer, e.g. 160004 for 16.4.
// Several tools need it because pg_stat_statements renamed its timing columns
// in PostgreSQL 13.
func (db *DB) Version(ctx context.Context) (int, error) {
	var text string
	if err := db.pool.QueryRow(ctx, "SHOW server_version").Scan(&text); err != nil {
		return 0, fmt.Errorf("reading server_version: %w", err)
	}
	major, minor, ok := parseVersion(text)
	if !ok {
		return 0, fmt.Errorf("cannot parse server_version %q", text)
	}
	return major*10000 + minor*100, nil
}

// parseVersion turns "16.4", "16.4 (Debian 16.4-1)" or "16beta1" into 16 and 4.
func parseVersion(s string) (major, minor int, ok bool) {
	if i := strings.IndexAny(s, " (-"); i >= 0 {
		s = s[:i]
	}
	var numbers []int
	for _, part := range strings.Split(s, ".") {
		digits := 0
		for digits < len(part) && part[digits] >= '0' && part[digits] <= '9' {
			digits++
		}
		if digits == 0 {
			break // no leading digits, so there is no number to read
		}
		n, err := strconv.Atoi(part[:digits])
		if err != nil {
			break
		}
		numbers = append(numbers, n)
		if digits < len(part) {
			break // trailing junk such as "beta1": stop after this number
		}
	}
	switch len(numbers) {
	case 0:
		return 0, 0, false
	case 1:
		return numbers[0], 0, true
	default:
		return numbers[0], numbers[1], true
	}
}

// HasExtension reports whether an extension is installed, so a tool can degrade
// with a useful message instead of failing on a missing catalog.
func (db *DB) HasExtension(ctx context.Context, name string) (bool, error) {
	var exists bool
	err := db.pool.QueryRow(ctx,
		"SELECT EXISTS (SELECT 1 FROM pg_extension WHERE extname = $1)", name).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("checking extension %s: %w", name, err)
	}
	return exists, nil
}

// jsonValue turns a pgx-decoded value into something encoding/json can render.
// pgx hands back Go types for most OIDs, but a few have no natural JSON form.
// intOf and floatOf read a numeric cell that pgx may have decoded as either an
// integer or a float, depending on the column type.
func intOf(row map[string]any, key string) int64 {
	switch v := row[key].(type) {
	case int64:
		return v
	case int32:
		return int64(v)
	case float64:
		return int64(v)
	default:
		return 0
	}
}

func floatOf(row map[string]any, key string) float64 {
	switch v := row[key].(type) {
	case float64:
		return v
	case int64:
		return float64(v)
	case int32:
		return float64(v)
	default:
		return 0
	}
}

func jsonValue(oid uint32, v any) any {
	switch oid {
	case pgtype.JSONOID, pgtype.JSONBOID:
		raw, ok := v.([]byte)
		if !ok {
			return v
		}
		var decoded any
		if err := json.Unmarshal(raw, &decoded); err != nil {
			return string(raw)
		}
		return decoded
	case pgtype.ByteaOID:
		if raw, ok := v.([]byte); ok {
			return string(raw)
		}
	case pgtype.NumericOID:
		// Arbitrary-precision values lose precision as float64, so hand back the
		// server's own value: int64 when it is integral, float64 otherwise.
		if n, ok := v.(pgtype.Numeric); ok {
			if decoded, err := n.Value(); err == nil {
				return decoded
			}
		}
		return fmt.Sprint(v)
	}
	return v
}
