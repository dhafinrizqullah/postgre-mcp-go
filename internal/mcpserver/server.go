// Package mcpserver registers this server's tools with the MCP SDK.
//
// Every handler follows the same three steps: validate the arguments, validate
// the SQL with internal/safesql, then run it through internal/pg. There is no
// service layer in between, because each tool has exactly one caller and the
// logic worth testing lives in the two packages below.
package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dhafinrizqullah/postgre-mcp-go/internal/pg"
	"github.com/dhafinrizqullah/postgre-mcp-go/internal/safesql"
)

// Version is the server version reported to clients. main overrides it from
// build info.
var Version = "dev"

// New builds the MCP server with every tool registered.
func New(db *pg.DB, logger *slog.Logger) *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{
		Name:    "postgres-mcp-go",
		Title:   "Postgres MCP (Go)",
		Version: Version,
	}, &mcp.ServerOptions{
		Logger: logger,
		Instructions: "Read-only Postgres access: inspect the schema, run SELECT queries, " +
			"explain plans, and check database health. Writes are rejected.",
	})

	addListSchemas(srv, db)
	addListObjects(srv, db)
	addObjectDetails(srv, db)
	addExecuteSQL(srv, db)
	addTopQueries(srv, db)
	addExplainQuery(srv, db)
	addDatabaseHealth(srv, db)
	return srv
}

var readOnly = &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: boolPtr(false)}

func boolPtr(b bool) *bool { return &b }

// jsonResult renders a value as indented JSON text, which is what an LLM reads
// best. Structured output is left unset so the response stays plain text.
func jsonResult(v any) (*mcp.CallToolResult, any, error) {
	encoded, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return errorResult(fmt.Errorf("encoding result: %w", err)), nil, nil
	}
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: string(encoded)}},
	}, nil, nil
}

// errorResult reports a failure the model should read and react to, rather than
// a protocol error that aborts the call.
func errorResult(err error) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}},
	}
}

type listObjectsInput struct {
	SchemaName string `json:"schema_name" jsonschema:"Schema to list objects from, for example public"`
	ObjectType string `json:"object_type,omitempty" jsonschema:"One of table, view, sequence, extension. Defaults to table"`
}

func addListObjects(srv *mcp.Server, db *pg.DB) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "list_objects",
		Title:       "List Objects",
		Description: "List tables, views, sequences, or extensions in a schema.",
		Annotations: readOnly,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in listObjectsInput) (*mcp.CallToolResult, any, error) {
		if in.SchemaName == "" {
			return errorResult(errors.New("schema_name is required")), nil, nil
		}
		if in.ObjectType == "" {
			in.ObjectType = "table"
		}
		objects, err := db.ListObjects(ctx, in.SchemaName, in.ObjectType)
		if err != nil {
			return errorResult(err), nil, nil
		}
		return jsonResult(objects)
	})
}

type objectDetailsInput struct {
	SchemaName string `json:"schema_name" jsonschema:"Schema the object belongs to"`
	ObjectName string `json:"object_name" jsonschema:"Name of the table, view, sequence, or extension"`
	ObjectType string `json:"object_type,omitempty" jsonschema:"One of table, view, sequence, extension. Defaults to table"`
}

func addObjectDetails(srv *mcp.Server, db *pg.DB) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:  "get_object_details",
		Title: "Get Object Details",
		Description: "Describe one table, view, sequence, or extension: columns with types and " +
			"defaults, constraints, and index definitions.",
		Annotations: readOnly,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in objectDetailsInput) (*mcp.CallToolResult, any, error) {
		if in.SchemaName == "" || in.ObjectName == "" {
			return errorResult(errors.New("schema_name and object_name are required")), nil, nil
		}
		if in.ObjectType == "" {
			in.ObjectType = "table"
		}
		details, err := db.GetObjectDetails(ctx, in.SchemaName, in.ObjectName, in.ObjectType)
		if err != nil {
			return errorResult(err), nil, nil
		}
		return jsonResult(details)
	})
}

type executeSQLInput struct {
	SQL string `json:"sql" jsonschema:"A single read-only SQL statement. Use bind parameters like $1 and pass them in params."`
}

func addExecuteSQL(srv *mcp.Server, db *pg.DB) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:  "execute_sql",
		Title: "Execute SQL",
		Description: "Run one read-only SQL statement and return the rows. Statements are parsed and " +
			"checked against an allowlist of statement types, AST nodes, and functions, so writes, " +
			"DDL, locking reads, and file or network access are rejected. Prefer the schema tools " +
			"over hand-written catalog queries.",
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint:    true,
			OpenWorldHint:   boolPtr(false),
			DestructiveHint: boolPtr(false),
		},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in executeSQLInput) (*mcp.CallToolResult, any, error) {
		if err := safesql.Validate(in.SQL); err != nil {
			return errorResult(err), nil, nil
		}
		rows, err := db.Query(ctx, in.SQL)
		if err != nil {
			return errorResult(err), nil, nil
		}
		return jsonResult(rows)
	})
}

type topQueriesInput struct {
	SortBy string `json:"sort_by,omitempty" jsonschema:"resources, total_time, or mean_time. Defaults to resources"`
	Limit  int    `json:"limit,omitempty" jsonschema:"How many queries to return. Defaults to 10"`
}

func addTopQueries(srv *mcp.Server, db *pg.DB) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:  "get_top_queries",
		Title: "Get Top Queries",
		Description: "Rank the queries pg_stat_statements has recorded, by resource consumption, " +
			"total execution time, or mean time per call. Good starting point for finding what to tune.",
		Annotations: readOnly,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in topQueriesInput) (*mcp.CallToolResult, any, error) {
		if in.SortBy == "" {
			in.SortBy = "resources"
		}
		queries, err := db.TopQueries(ctx, in.SortBy, in.Limit)
		if errors.Is(err, pg.ErrNoStatStatements) {
			// Not a failure: this is a setup step, and the tools that do work are
			// worth naming here.
			return errorResult(errors.New(
				"pg_stat_statements is not available. It must be installed " +
					"(CREATE EXTENSION pg_stat_statements) and listed in " +
					"shared_preload_libraries, which needs a restart. " +
					"Until then, use analyze_db_health and explain_query")), nil, nil
		}
		if err != nil {
			return errorResult(err), nil, nil
		}
		return jsonResult(queries)
	})
}

type explainQueryInput struct {
	SQL               string                `json:"sql" jsonschema:"The statement to explain"`
	HypotheticalIndex []hypotheticalIndexIn `json:"hypothetical_indexes,omitempty" jsonschema:"Indexes to simulate with hypopg"`
}

type hypotheticalIndexIn struct {
	Table   string   `json:"table" jsonschema:"Table to index"`
	Columns []string `json:"columns" jsonschema:"Columns, in order, optionally with a direction such as created_at DESC"`
	Using   string   `json:"using,omitempty" jsonschema:"Index method. Defaults to btree"`
}

func addExplainQuery(srv *mcp.Server, db *pg.DB) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:  "explain_query",
		Title: "Explain Query",
		Description: "Return the execution plan for a statement. Pass hypothetical_indexes to ask the " +
			"planner what it would do with those indexes, which is the safe way to test an index idea " +
			"before creating it. EXPLAIN ANALYZE is not supported because it runs the statement.",
		Annotations: readOnly,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in explainQueryInput) (*mcp.CallToolResult, any, error) {
		if err := safesql.Validate(in.SQL); err != nil {
			return errorResult(err), nil, nil
		}
		indexes := make([]pg.HypotheticalIndex, 0, len(in.HypotheticalIndex))
		for _, h := range in.HypotheticalIndex {
			indexes = append(indexes, pg.HypotheticalIndex{
				Table:   h.Table,
				Columns: h.Columns,
				Using:   h.Using,
			})
		}
		plan, err := db.Explain(ctx, in.SQL, indexes)
		if err != nil {
			return errorResult(err), nil, nil
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: string(plan)}},
		}, nil, nil
	})
}

type databaseHealthInput struct {
	HealthType string `json:"health_type,omitempty" jsonschema:"Comma-separated checks, or all. One of index, connection, vacuum, sequence, replication, buffer, constraint"`
}

func addDatabaseHealth(srv *mcp.Server, db *pg.DB) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:  "analyze_db_health",
		Title: "Analyze Database Health",
		Description: "Check database health. index finds unused, duplicate, and bloated indexes; " +
			"connection reports slot utilisation and lock waits; vacuum reports tables nearing " +
			"transaction id wraparound; sequence reports sequences near their maximum; replication " +
			"reports replica lag and slot health; buffer reports cache hit rate; constraint reports " +
			"invalid constraints.",
		Annotations: readOnly,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in databaseHealthInput) (*mcp.CallToolResult, any, error) {
		var names []string
		if in.HealthType != "" && in.HealthType != "all" {
			names = []string{in.HealthType}
		}
		checks, err := db.Health(ctx, names)
		if err != nil {
			return errorResult(err), nil, nil
		}
		return jsonResult(checks)
	})
}

func addListSchemas(srv *mcp.Server, db *pg.DB) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "list_schemas",
		Title:       "List Schemas",
		Description: "List every schema in the database, with its owner, system schemas last.",
		Annotations: readOnly,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
		schemas, err := db.ListSchemas(ctx)
		if err != nil {
			return errorResult(err), nil, nil
		}
		return jsonResult(schemas)
	})
}
