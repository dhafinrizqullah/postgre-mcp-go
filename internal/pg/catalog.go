package pg

import (
	"context"
	"fmt"
)

// Schema is one row of list_schemas.
type Schema struct {
	Name  string `json:"name"`
	Owner string `json:"owner"`
	Type  string `json:"type"`
}

// Object is one row of list_objects.
type Object struct {
	Schema      string `json:"schema,omitempty"`
	Name        string `json:"name"`
	Type        string `json:"type,omitempty"`
	DataType    string `json:"data_type,omitempty"`
	Version     string `json:"version,omitempty"`
	Relocatable bool   `json:"relocatable,omitempty"`
}

// ListSchemas returns every schema, system schemas included but sorted last so
// the interesting ones are easy to find.
func (db *DB) ListSchemas(ctx context.Context) ([]Schema, error) {
	rows, err := db.Query(ctx, `
		SELECT
			schema_name,
			schema_owner,
			CASE
				WHEN schema_name LIKE 'pg\_%' THEN 'System Schema'
				WHEN schema_name = 'information_schema' THEN 'System Information Schema'
				ELSE 'User Schema'
			END AS schema_type
		FROM information_schema.schemata
		ORDER BY schema_type, schema_name`)
	if err != nil {
		return nil, err
	}
	out := make([]Schema, 0, len(rows))
	for _, row := range rows {
		out = append(out, Schema{
			Name:  str(row, "schema_name"),
			Owner: str(row, "schema_owner"),
			Type:  str(row, "schema_type"),
		})
	}
	return out, nil
}

// objectTypes maps the tool's object_type argument to the catalog table that
// holds it, and is the validation set for that argument.
var objectTypes = map[string]string{
	"table":     "BASE TABLE",
	"view":      "VIEW",
	"sequence":  "",
	"extension": "",
}

// ListObjects lists tables, views, sequences, or extensions in a schema.
// Extensions are not schema-qualified in Postgres, so that type ignores the
// schema argument.
func (db *DB) ListObjects(ctx context.Context, schema, objectType string) ([]Object, error) {
	if _, ok := objectTypes[objectType]; !ok {
		return nil, fmt.Errorf("unsupported object type %q: use table, view, sequence, or extension", objectType)
	}

	var (
		rows []map[string]any
		err  error
	)
	switch objectType {
	case "table", "view":
		rows, err = db.Query(ctx, `
			SELECT table_schema, table_name, table_type
			FROM information_schema.tables
			WHERE table_schema = $1 AND table_type = $2
			ORDER BY table_name`, schema, objectTypes[objectType])
	case "sequence":
		rows, err = db.Query(ctx, `
			SELECT sequence_schema, sequence_name, 'SEQUENCE' AS table_type
			FROM information_schema.sequences
			WHERE sequence_schema = $1
			ORDER BY sequence_name`, schema)
	case "extension":
		rows, err = db.Query(ctx, `
			SELECT NULL::name AS table_schema, extname AS table_name,
			       extversion AS table_type, extrelocatable
			FROM pg_extension
			ORDER BY extname`)
	}
	if err != nil {
		return nil, err
	}

	out := make([]Object, 0, len(rows))
	for _, row := range rows {
		obj := Object{
			Schema: str(row, "table_schema"),
			Name:   str(row, "table_name"),
			Type:   str(row, "table_type"),
		}
		if objectType == "extension" {
			obj.Version = obj.Type
			obj.Type = "extension"
			obj.Relocatable = boolean(row, "extrelocatable")
		}
		out = append(out, obj)
	}
	return out, nil
}

// Column is one column of a table or view.
type Column struct {
	Name       string `json:"column"`
	DataType   string `json:"data_type"`
	IsNullable string `json:"is_nullable"`
	Default    string `json:"default"`
}

// Constraint groups the key_column_usage rows that belong to one constraint.
type Constraint struct {
	Name    string   `json:"name"`
	Type    string   `json:"type"`
	Columns []string `json:"columns"`
}

// Index is one index definition from pg_indexes.
type Index struct {
	Name       string `json:"name"`
	Definition string `json:"definition"`
}

// ObjectDetails is the full description of one database object. Only the fields
// relevant to the requested type are populated, which keeps the tool response
// small enough for an LLM to read.
type ObjectDetails struct {
	Schema      string       `json:"schema,omitempty"`
	Name        string       `json:"name"`
	Type        string       `json:"type,omitempty"`
	DataType    string       `json:"data_type,omitempty"`
	StartValue  string       `json:"start_value,omitempty"`
	Increment   string       `json:"increment,omitempty"`
	Version     string       `json:"version,omitempty"`
	Relocatable bool         `json:"relocatable,omitempty"`
	Columns     []Column     `json:"columns,omitempty"`
	Constraints []Constraint `json:"constraints,omitempty"`
	Indexes     []Index      `json:"indexes,omitempty"`
}

// GetObjectDetails describes a table, view, sequence, or extension.
func (db *DB) GetObjectDetails(ctx context.Context, schema, name, objectType string) (*ObjectDetails, error) {
	if _, ok := objectTypes[objectType]; !ok {
		return nil, fmt.Errorf("unsupported object type %q: use table, view, sequence, or extension", objectType)
	}

	switch objectType {
	case "table", "view":
		return db.tableDetails(ctx, schema, name, objectType)
	case "sequence":
		row, err := db.QueryOne(ctx, `
			SELECT sequence_schema, sequence_name, data_type, start_value, increment
			FROM information_schema.sequences
			WHERE sequence_schema = $1 AND sequence_name = $2`, schema, name)
		if err != nil || row == nil {
			return nil, err
		}
		return &ObjectDetails{
			Schema:     str(row, "sequence_schema"),
			Name:       str(row, "sequence_name"),
			DataType:   str(row, "data_type"),
			StartValue: str(row, "start_value"),
			Increment:  str(row, "increment"),
		}, nil
	default: // extension
		row, err := db.QueryOne(ctx, `
			SELECT extname, extversion, extrelocatable FROM pg_extension WHERE extname = $1`, name)
		if err != nil || row == nil {
			return nil, err
		}
		return &ObjectDetails{
			Name:        str(row, "extname"),
			Version:     str(row, "extversion"),
			Relocatable: boolean(row, "extrelocatable"),
		}, nil
	}
}

func (db *DB) tableDetails(ctx context.Context, schema, name, objectType string) (*ObjectDetails, error) {
	colRows, err := db.Query(ctx, `
		SELECT column_name, data_type, is_nullable, column_default
		FROM information_schema.columns
		WHERE table_schema = $1 AND table_name = $2
		ORDER BY ordinal_position`, schema, name)
	if err != nil {
		return nil, err
	}
	if len(colRows) == 0 {
		return nil, fmt.Errorf("%s %q not found in schema %q", objectType, name, schema)
	}
	columns := make([]Column, 0, len(colRows))
	for _, row := range colRows {
		columns = append(columns, Column{
			Name:       str(row, "column_name"),
			DataType:   str(row, "data_type"),
			IsNullable: str(row, "is_nullable"),
			Default:    str(row, "column_default"),
		})
	}

	conRows, err := db.Query(ctx, `
		SELECT tc.constraint_name, tc.constraint_type, kcu.column_name
		FROM information_schema.table_constraints AS tc
		LEFT JOIN information_schema.key_column_usage AS kcu
		  ON tc.constraint_name = kcu.constraint_name
		 AND tc.table_schema = kcu.table_schema
		WHERE tc.table_schema = $1 AND tc.table_name = $2
		ORDER BY tc.constraint_name`, schema, name)
	if err != nil {
		return nil, err
	}
	// One constraint spans as many rows as it has columns, so group them back up.
	order := make([]string, 0, len(conRows))
	byName := make(map[string]*Constraint, len(conRows))
	for _, row := range conRows {
		cname := str(row, "constraint_name")
		c, ok := byName[cname]
		if !ok {
			c = &Constraint{Name: cname, Type: str(row, "constraint_type"), Columns: []string{}}
			byName[cname] = c
			order = append(order, cname)
		}
		if col := str(row, "column_name"); col != "" {
			c.Columns = append(c.Columns, col)
		}
	}
	constraints := make([]Constraint, 0, len(order))
	for _, cname := range order {
		constraints = append(constraints, *byName[cname])
	}

	idxRows, err := db.Query(ctx, `
		SELECT indexname, indexdef FROM pg_indexes
		WHERE schemaname = $1 AND tablename = $2
		ORDER BY indexname`, schema, name)
	if err != nil {
		return nil, err
	}
	indexes := make([]Index, 0, len(idxRows))
	for _, row := range idxRows {
		indexes = append(indexes, Index{Name: str(row, "indexname"), Definition: str(row, "indexdef")})
	}

	return &ObjectDetails{
		Schema:      schema,
		Name:        name,
		Type:        objectType,
		Columns:     columns,
		Constraints: constraints,
		Indexes:     indexes,
	}, nil
}

func str(row map[string]any, key string) string {
	s, _ := row[key].(string)
	return s
}

func boolean(row map[string]any, key string) bool {
	b, _ := row[key].(bool)
	return b
}
