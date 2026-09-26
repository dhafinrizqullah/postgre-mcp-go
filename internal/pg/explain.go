package pg

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// HypotheticalIndex is an index to simulate with the hypopg extension. hypopg
// asks the real planner what it would do, so the cost estimate is Postgres's own
// rather than a guess.
type HypotheticalIndex struct {
	Table   string   `json:"table"`
	Columns []string `json:"columns"`
	Using   string   `json:"using,omitempty"`
}

// Definition renders the index as hypopg expects it, e.g.
// "btree (orders (user_id, created_at DESC))".
func (h HypotheticalIndex) Definition() (string, error) {
	if h.Table == "" {
		return "", fmt.Errorf("hypothetical index needs a table")
	}
	if len(h.Columns) == 0 {
		return "", fmt.Errorf("hypothetical index on %q needs at least one column", h.Table)
	}
	method := h.Using
	if method == "" {
		method = "btree"
	}
	cols := make([]string, 0, len(h.Columns))
	for _, c := range h.Columns {
		if c == "" || strings.ContainsAny(c, "(),") {
			return "", fmt.Errorf("invalid column %q in hypothetical index on %q", c, h.Table)
		}
		cols = append(cols, c)
	}
	return fmt.Sprintf("%s (%s (%s))", method, h.Table, strings.Join(cols, ", ")), nil
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
		reset := func() error {
			if _, err := s.Query(ctx, "SELECT hypopg_reset()"); err != nil {
				return fmt.Errorf("resetting hypothetical indexes: %w", err)
			}
			return nil
		}
		if err := reset(); err != nil {
			return err
		}
		defer func() { _ = reset() }() // a leaked hypothetical index would skew the next call

		for _, def := range defs {
			if _, err := s.Query(ctx, "SELECT hypopg_create_index($1)", def); err != nil {
				return fmt.Errorf("creating hypothetical index %s: %w", def, err)
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
