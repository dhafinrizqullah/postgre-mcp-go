package pg

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/dhafinrizqullah/postgre-mcp-go/internal/safesql"
)

// HypotheticalIndex is an index to simulate with the hypopg extension. hypopg
// asks the real planner what it would do, so the cost estimate is Postgres's own
// rather than a guess.
type HypotheticalIndex struct {
	Table   string   `json:"table"`
	Columns []string `json:"columns"`
	Using   string   `json:"using,omitempty"`
}

// Definition renders the index as hypopg expects it.
func (h HypotheticalIndex) Definition() (string, error) {
	return HypopgIndexDefinition(h.Table, h.Columns, h.Using)
}

// HypopgIndexDefinition renders a full CREATE INDEX statement for hypopg to
// simulate. hypopg parses this string as SQL and never creates anything, so the
// statement still has to be valid; the index name is a placeholder that hypopg
// replaces with its own <NNNNN>btree_table_columns tag.
//
// Identifiers are quoted, because a table or column may be a reserved word or
// mixed case, and a sort direction is left outside the quotes. A table may be
// schema-qualified, so each dotted part is quoted separately.
func HypopgIndexDefinition(table string, columns []string, using string) (string, error) {
	if table == "" {
		return "", fmt.Errorf("hypothetical index needs a table")
	}
	if len(columns) == 0 {
		return "", fmt.Errorf("hypothetical index on %q needs at least one column", table)
	}
	method := using
	if method == "" {
		method = "btree"
	}
	if !safeIdentifier.MatchString(method) {
		return "", fmt.Errorf("invalid index method %q", method)
	}
	parts := strings.Split(table, ".")
	if len(parts) > 2 {
		return "", fmt.Errorf("table name %q has more than a schema and a table", table)
	}
	for _, part := range parts {
		if !safeIdentifier.MatchString(part) {
			return "", fmt.Errorf("invalid table name %q", table)
		}
	}
	qualified := make([]string, 0, len(parts))
	for _, part := range parts {
		qualified = append(qualified, quoteIdent(part))
	}

	rendered := make([]string, 0, len(columns))
	names := make([]string, 0, len(columns))
	for _, column := range columns {
		name, direction := splitDirection(column)
		if !safeIdentifier.MatchString(name) {
			return "", fmt.Errorf("invalid column %q in hypothetical index on %q", column, table)
		}
		names = append(names, name)
		if direction == "" {
			rendered = append(rendered, quoteIdent(name))
			continue
		}
		rendered = append(rendered, quoteIdent(name)+" "+direction)
	}
	indexName := hypopgIndexName(strings.Join(parts, "_"), names, method)
	return fmt.Sprintf("CREATE INDEX %s ON %s USING %s (%s)",
		indexName, strings.Join(qualified, "."), method, strings.Join(rendered, ", ")), nil
}

// splitDirection separates a sort direction from a column name, so that
// `created_at DESC` becomes `created_at` plus `DESC`, not one quoted identifier.
func splitDirection(column string) (name, direction string) {
	fields := strings.Fields(column)
	switch len(fields) {
	case 1:
		return fields[0], ""
	case 2:
		if upper := strings.ToUpper(fields[1]); upper == "ASC" || upper == "DESC" {
			return fields[0], upper
		}
	}
	return "", "" // more than one word and not a plain direction: rejected below
}

// safeIdentifier is deliberately strict. The value is quoted on the way in, but
// it is also parsed as SQL by hypopg, so an expression index or a crafted name is
// refused rather than quoted into something unexpected.
var safeIdentifier = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_$]*$`)

func quoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// hypopgIndexName builds the placeholder name. hypopg replaces it with its own
// <NNNNN>btree_table_columns tag, which is what makes a simulated index
// recognisable in a plan.
func hypopgIndexName(table string, columns []string, method string) string {
	name := "hypopg_idx_" + table + "_" + strings.Join(columns, "_") + "_" + strconv.Itoa(len(columns))
	if method != "btree" {
		name += "_" + method
	}
	return name
}

// Explain returns the plan for sql as the JSON object EXPLAIN (FORMAT JSON)
// produces.
//
// It deliberately does not support EXPLAIN ANALYZE: that executes the statement,
// which is not read-only, and internal/safesql rejects it. Pass hypothetical
// indexes to ask the planner what it would do with them.
//
// The caller must have validated sql with safesql.Validate first. Index
// definitions are interpolated into a hypopg call, so they are validated here
// rather than trusted.
func (db *DB) Explain(ctx context.Context, sql string, hypothetical []HypotheticalIndex) (json.RawMessage, error) {
	if len(hypothetical) == 0 {
		rows, err := db.Query(ctx, "EXPLAIN (FORMAT JSON) "+sql)
		if err != nil {
			return nil, err
		}
		return planColumn(rows)
	}

	defs := make([]string, 0, len(hypothetical))
	for _, h := range hypothetical {
		def, err := h.Definition()
		if err != nil {
			return nil, err
		}
		defs = append(defs, def)
	}

	// hypopg indexes are session state, so every statement has to run on the
	// same connection. Four statements, one connection, then release.
	return db.explainWithHypopg(ctx, sql, defs)
}

// CreateHypopgIndex asks hypopg to simulate one index in the current session.
//
// hypopg parses its argument as an index definition, and a bind parameter cannot
// be used there: the function sees a placeholder instead of a definition and
// fails with a syntax error on the method name. The definition is therefore
// inlined as a quoted literal, and the statement is run past safesql.Validate
// before it is sent, so it is checked against the same allowlist as any other
// statement. The literal cannot break out either, since every quote in it is
// doubled.
func CreateHypopgIndex(ctx context.Context, s *Session, definition string) error {
	statement := "SELECT hypopg_create_index('" + strings.ReplaceAll(definition, "'", "''") + "')"
	if err := safesql.Validate(statement); err != nil {
		return fmt.Errorf("refusing to simulate %s: %w", definition, err)
	}
	if _, err := s.Query(ctx, statement); err != nil {
		return fmt.Errorf("simulating index %s: %w", definition, err)
	}
	return nil
}

// ResetHypopg drops every simulated index in the current session. A caller that
// borrows a connection must always do this, or the next borrow inherits indexes it
// did not ask for.
func ResetHypopg(ctx context.Context, s *Session) error {
	if _, err := s.Query(ctx, "SELECT hypopg_reset()"); err != nil {
		return fmt.Errorf("resetting simulated indexes: %w", err)
	}
	return nil
}

func (db *DB) explainWithHypopg(ctx context.Context, sql string, defs []string) (json.RawMessage, error) {
	installed, err := db.HasExtension(ctx, "hypopg")
	if err != nil {
		return nil, err
	}
	if !installed {
		return nil, fmt.Errorf("hypothetical indexes need the hypopg extension: CREATE EXTENSION hypopg")
	}

	var plan json.RawMessage
	err = db.WithConn(ctx, func(ctx context.Context, s *Session) error {
		if err := ResetHypopg(ctx, s); err != nil {
			return err
		}
		defer func() { _ = ResetHypopg(ctx, s) }()

		for _, def := range defs {
			if err := CreateHypopgIndex(ctx, s, def); err != nil {
				return err
			}
		}
		rows, err := s.Query(ctx, "EXPLAIN (FORMAT JSON) "+sql)
		if err != nil {
			return err
		}
		plan, err = planColumn(rows)
		return err
	})
	if err != nil {
		return nil, err
	}
	return plan, nil
}

// planColumn pulls the single "QUERY PLAN" value out of an EXPLAIN result set.
// FORMAT JSON returns a list, which the JSON decoder gives us as []any.
func planColumn(rows []map[string]any) (json.RawMessage, error) {
	if len(rows) == 0 {
		return nil, fmt.Errorf("EXPLAIN returned no rows")
	}
	for key, value := range rows[0] {
		if !strings.EqualFold(key, "QUERY PLAN") {
			continue
		}
		switch v := value.(type) {
		case []any:
			if len(v) == 0 {
				return nil, fmt.Errorf("EXPLAIN returned an empty plan")
			}
		case string:
			// Some drivers hand back the JSON as text.
			return json.RawMessage(v), nil
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			return nil, fmt.Errorf("re-encoding plan: %w", err)
		}
		return encoded, nil
	}
	return nil, fmt.Errorf("EXPLAIN result has no QUERY PLAN column")
}
