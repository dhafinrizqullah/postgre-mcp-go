package index

import (
	"context"
	"fmt"
	"strings"

	"github.com/dhafinrizqullah/postgre-mcp-go/internal/pg"
)

// catalog reads the facts the tuner needs from the database: which table owns a
// column, how big a table is, which indexes already exist, and roughly how big a
// proposed index would be.
type catalog struct {
	db *pg.DB
	// columns caches table -> column set, for both alias disambiguation and the
	// long-text filter.
	columns map[string]map[string]bool
	// sizes caches pg_total_relation_size per table.
	sizes map[string]int64
	// existing maps table -> the column signatures already indexed, so the
	// tuner never proposes an index that is already there.
	existing map[string]map[string]bool
	// columnWidth caches pg_stats.avg_width, used to reject text indexes.
	columnWidth map[string]int
	loaded      bool
}

func newCatalog(db *pg.DB) *catalog {
	return &catalog{
		db:          db,
		columns:     map[string]map[string]bool{},
		sizes:       map[string]int64{},
		existing:    map[string]map[string]bool{},
		columnWidth: map[string]int{},
	}
}

// load reads the schema facts for the tables a workload touches, in four queries
// rather than one per lookup. Everything else in the tuner is a cache hit.
func (c *catalog) load(ctx context.Context, tables []string) error {
	if len(tables) == 0 {
		return nil
	}
	if err := c.loadColumns(ctx, tables); err != nil {
		return err
	}
	if err := c.loadExisting(ctx); err != nil {
		return err
	}
	if err := c.loadWidths(ctx, tables); err != nil {
		return err
	}
	c.loaded = true
	return nil
}

func (c *catalog) loadColumns(ctx context.Context, tables []string) error {
	rows, err := c.db.Query(ctx, `
		SELECT table_name, column_name
		FROM information_schema.columns
		WHERE table_name = ANY($1)`, tables)
	if err != nil {
		return fmt.Errorf("reading columns: %w", err)
	}
	for _, row := range rows {
		table, column := str(row, "table_name"), str(row, "column_name")
		if c.columns[table] == nil {
			c.columns[table] = map[string]bool{}
		}
		c.columns[table][column] = true
	}
	return nil
}

// loadExisting records the column signature of every index, so a candidate that
// already exists is dropped. Signatures are compared as ordered column names,
// which catches an existing index regardless of how it was written.
func (c *catalog) loadExisting(ctx context.Context) error {
	rows, err := c.db.Query(ctx, `
		SELECT
			t.relname AS table_name,
			ic.relname AS index_name,
			array_agg(a.attname ORDER BY k.ord) AS columns
		FROM pg_index i
		JOIN pg_class ic ON ic.oid = i.indexrelid
		JOIN pg_class t ON t.oid = i.indrelid
		JOIN LATERAL unnest(i.indnatts) WITH ORDINALITY AS k(attnum, ord) ON true
		JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = k.attnum
		WHERE i.indisvalid AND i.indisready AND NOT i.indisprimary
		GROUP BY t.relname, ic.relname`)
	if err != nil {
		return fmt.Errorf("reading existing indexes: %w", err)
	}
	for _, row := range rows {
		table := str(row, "table_name")
		if c.existing[table] == nil {
			c.existing[table] = map[string]bool{}
		}
		cols := anyToStrings(row["columns"])
		if len(cols) == 0 {
			// An expression or partial index: nothing comparable to match on.
			continue
		}
		c.existing[table][strings.Join(cols, ",")] = true
	}
	return nil
}

func (c *catalog) loadWidths(ctx context.Context, tables []string) error {
	rows, err := c.db.Query(ctx, `
		SELECT tablename, attname, avg_width
		FROM pg_stats
		WHERE tablename = ANY($1)`, tables)
	if err != nil {
		// pg_stats needs ANALYZE to have run. A missing row means no estimate,
		// which the caller treats as unknown rather than as zero.
		return nil //nolint:nilerr // statistics are optional here
	}
	for _, row := range rows {
		c.columnWidth[str(row, "tablename")+"."+str(row, "attname")] = int(intOf(row, "avg_width"))
	}
	return nil
}

// Owner implements ColumnResolver.
func (c *catalog) Owner(_, column string, candidates []string) string {
	found := ""
	for _, table := range candidates {
		if c.columns[table] == nil {
			continue
		}
		if c.columns[table][column] {
			if found != "" {
				// Ambiguous even in the catalog: the column exists in more than
				// one table in scope, so no answer is better than a wrong one.
				return ""
			}
			found = table
		}
	}
	return found
}

// HasColumn reports whether a table has a column, when it is known.
func (c *catalog) HasColumn(table, column string) (bool, bool) {
	cols, ok := c.columns[table]
	if !ok {
		return false, false
	}
	return cols[column], true
}

// TableSize is the on-disk size of a table with its indexes, which is the base
// the space budget is measured against.
func (c *catalog) TableSize(ctx context.Context, table string) (int64, error) {
	if size, ok := c.sizes[table]; ok {
		return size, nil
	}
	row, err := c.db.QueryOne(ctx,
		`SELECT pg_total_relation_size(quote_ident($1)) AS size`, table)
	if err != nil {
		return 0, fmt.Errorf("reading size of %s: %w", table, err)
	}
	var size int64
	if row != nil {
		size = intOf(row, "size")
	}
	c.sizes[table] = size
	return size, nil
}

// indexExists reports whether an index on exactly these columns, in this order,
// is already defined.
func (c *catalog) indexExists(table string, columns []string) bool {
	return c.existing[table] != nil && c.existing[table][strings.Join(columns, ",")]
}

// isLongText reports whether a column is too wide to be worth indexing, using the
// same 100-byte threshold as upstream.
func (c *catalog) isLongText(table, column string) bool {
	width, ok := c.columnWidth[table+"."+column]
	return ok && width > maxIndexedTextWidth
}

// maxIndexedTextWidth is the widest column worth a btree index. Past this the
// index is nearly as large as the data and the planner stops using it.
const maxIndexedTextWidth = 100

// estimateIndexSize approximates the size of an index from column statistics,
// the way upstream does: entry width times distinct values, doubled. hypopg's
// own estimate is used instead when it is available, because it is the server's.
func (c *catalog) estimateIndexSize(ctx context.Context, table string, columns []string) (int64, error) {
	row, err := c.db.QueryOne(ctx, `
		SELECT
			COALESCE(SUM(avg_width), 0) AS total_width,
			COALESCE(SUM(n_distinct), 0) AS total_distinct
		FROM pg_stats
		WHERE tablename = $1 AND attname = ANY($2)`, table, columns)
	if err != nil {
		return 0, fmt.Errorf("estimating size of index on %s: %w", table, err)
	}
	if row == nil {
		return 0, nil
	}
	// 8 bytes for the heap tuple pointer every index entry carries.
	width := intOf(row, "total_width") + 8
	distinct := float64(intOf(row, "total_distinct"))
	if distinct <= 0 {
		distinct = 1
	}
	return int64(float64(width) * distinct * 2), nil
}

func str(row map[string]any, key string) string {
	s, _ := row[key].(string)
	return s
}

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

// anyToStrings converts a Postgres array cell to a string slice. pgx decodes
// text[] as []any of strings.
func anyToStrings(v any) []string {
	switch t := v.(type) {
	case []string:
		return t
	case []any:
		out := make([]string, 0, len(t))
		for _, elem := range t {
			if s, ok := elem.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case string:
		// Postgres renders an array as {a,b}; strip the braces.
		trimmed := strings.Trim(t, "{}")
		if trimmed == "" {
			return nil
		}
		return strings.Split(trimmed, ",")
	default:
		return nil
	}
}
