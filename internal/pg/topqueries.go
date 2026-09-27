package pg

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
)

// pgErrCodeObjectNotInPrerequisiteState is SQLSTATE 55000, which Postgres raises
// when an extension is installed but its shared library is not preloaded.
const pgErrCodeObjectNotInPrerequisiteState = "55000"

// TopQuery is one row of get_top_queries.
type TopQuery struct {
	Query     string  `json:"query"`
	Calls     int64   `json:"calls"`
	TotalTime float64 `json:"total_exec_time_ms"`
	MeanTime  float64 `json:"mean_exec_time_ms"`
	Rows      int64   `json:"rows"`
	Score     float64 `json:"score,omitempty"`
}

// ErrNoStatStatements is returned when pg_stat_statements cannot be queried:
// either it is not installed, or it is installed without being listed in
// shared_preload_libraries. Callers turn it into installation instructions
// instead of an error, because both are setup steps, not failures.
var ErrNoStatStatements = fmt.Errorf("pg_stat_statements is not available")

// TopQueries ranks queries recorded by pg_stat_statements.
//
// sortBy is "resources" (a blend of time and block I/O), "total_time", or
// "mean_time". PostgreSQL 13 renamed the timing columns, so the column names are
// chosen from the server version.
func (db *DB) TopQueries(ctx context.Context, sortBy string, limit int) ([]TopQuery, error) {
	installed, err := db.HasExtension(ctx, "pg_stat_statements")
	if err != nil {
		return nil, err
	}
	if !installed {
		return nil, ErrNoStatStatements
	}
	version, err := db.Version(ctx)
	if err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 10
	}

	// PostgreSQL 13 split pg_stat_statements' total_time into total_exec_time.
	totalCol, meanCol := "total_exec_time", "mean_exec_time"
	if version < 130000 {
		totalCol, meanCol = "total_time", "mean_time"
	}

	orderBy := totalCol
	switch sortBy {
	case "mean_time":
		orderBy = meanCol
	case "resources":
		// Time dominates, but a query that reads many blocks hurts even when it
		// is quick, so weight shared reads and temp writes into the score.
		// ponytail: a hand-tuned blend, not a cost model. Revisit if the
		// ranking disagrees with what the slow log says.
		orderBy = fmt.Sprintf(
			`%s + (shared_blks_hit + shared_blks_read) * 10 + (temp_blks_read + temp_blks_written) * 5`,
			totalCol,
		)
	case "total_time":
	default:
		return nil, fmt.Errorf("unknown sort_by %q: use resources, total_time, or mean_time", sortBy)
	}

	rows, err := db.Query(ctx, fmt.Sprintf(`
		SELECT
			query,
			calls,
			%s AS total_exec_time,
			%s AS mean_exec_time,
			rows,
			%s AS score
		FROM pg_stat_statements
		WHERE dbid = (SELECT oid FROM pg_database WHERE datname = current_database())
		ORDER BY %s DESC
		LIMIT $1`, totalCol, meanCol, orderBy, orderBy), limit)
	if err != nil {
		// The extension can be installed without being listed in
		// shared_preload_libraries, and then every query against it fails with
		// object_not_in_prerequisite_state. That is a setup step, not a failure.
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgErrCodeObjectNotInPrerequisiteState {
			return nil, ErrNoStatStatements
		}
		return nil, err
	}

	out := make([]TopQuery, 0, len(rows))
	for _, row := range rows {
		q := TopQuery{Query: str(row, "query")}
		q.Calls = intOf(row, "calls")
		q.TotalTime = floatOf(row, "total_exec_time")
		q.MeanTime = floatOf(row, "mean_exec_time")
		q.Rows = intOf(row, "rows")
		if s, ok := row["score"].(float64); ok {
			q.Score = s
		}
		out = append(out, q)
	}
	return out, nil
}

// SlowQuery is a normalised statement from pg_stat_statements, with the weight
// the index tuner needs to rank it.
type SlowQuery struct {
	Text       string
	Calls      int64
	MeanTimeMS float64
}

// SlowQueries returns the slowest statements by mean time, for the index tuner to
// work on. Statements are already normalised, so they carry bind parameters.
func (db *DB) SlowQueries(ctx context.Context, minCalls int, minMeanTimeMS float64, limit int) ([]SlowQuery, error) {
	installed, err := db.HasExtension(ctx, "pg_stat_statements")
	if err != nil {
		return nil, err
	}
	if !installed {
		return nil, ErrNoStatStatements
	}
	version, err := db.Version(ctx)
	if err != nil {
		return nil, err
	}
	// PostgreSQL 13 renamed the timing columns.
	meanCol := "mean_exec_time"
	if version < 130000 {
		meanCol = "mean_time"
	}
	if limit <= 0 {
		limit = 10
	}

	rows, err := db.Query(ctx, fmt.Sprintf(`
		SELECT query, calls, %s AS mean_time
		FROM pg_stat_statements
		WHERE dbid = (SELECT oid FROM pg_database WHERE datname = current_database())
		  AND calls >= $1
		  AND %s >= $2
		ORDER BY calls * %s DESC
		LIMIT $3`, meanCol, meanCol, meanCol), minCalls, minMeanTimeMS, limit)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgErrCodeObjectNotInPrerequisiteState {
			return nil, ErrNoStatStatements
		}
		return nil, err
	}

	out := make([]SlowQuery, 0, len(rows))
	for _, row := range rows {
		out = append(out, SlowQuery{
			Text:       str(row, "query"),
			Calls:      intOf(row, "calls"),
			MeanTimeMS: floatOf(row, "mean_time"),
		})
	}
	return out, nil
}

// TruncateSQL shortens a query for display without breaking its shape.
func TruncateSQL(sql string, max int) string {
	sql = strings.Join(strings.Fields(sql), " ")
	if len(sql) <= max {
		return sql
	}
	return sql[:max] + "..."
}
