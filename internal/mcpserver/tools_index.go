package mcpserver

import (
	"context"
	"errors"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dhafinrizqullah/postgre-mcp-go/internal/index"
	"github.com/dhafinrizqullah/postgre-mcp-go/internal/pg"
)

// maxTuningQueries caps a workload. Each query multiplies the number of
// configurations the planner is asked about, so this is also a time limit.
const maxTuningQueries = 10

type queryIndexesInput struct {
	Queries        []string `json:"queries" jsonschema:"Up to 10 SELECT statements to analyse, best or worst first"`
	MaxIndexSizeMB int      `json:"max_index_size_mb,omitempty" jsonschema:"Cap on the total size of the recommended indexes. 0 means no cap"`
	MaxRuntimeSec  int      `json:"max_runtime_seconds,omitempty" jsonschema:"Time budget for the search. Defaults to 30"`
}

func addQueryIndexes(srv *mcp.Server, db *pg.DB) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:  "analyze_query_indexes",
		Title: "Analyze Query Indexes",
		Description: "Recommend indexes for a list of SELECT statements. Every candidate is judged by " +
			"asking Postgres's own planner what it would cost with that index, so the estimate " +
			"reflects the real statistics and the real cost model. Needs the hypopg extension and " +
			"recent ANALYZE. Returns the CREATE INDEX statements to run.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: boolPtr(false)},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in queryIndexesInput) (*mcp.CallToolResult, any, error) {
		if len(in.Queries) == 0 {
			return errorResult(errors.New("provide at least one query to analyse")), nil, nil
		}
		if len(in.Queries) > maxTuningQueries {
			return errorResult(errors.New("provide at most 10 queries; pass the slowest ones first")), nil, nil
		}

		workload := make([]index.Query, 0, len(in.Queries))
		for _, q := range in.Queries {
			workload = append(workload, index.Query{Text: q, Calls: 1, MeanTimeMS: 1})
		}

		opts := tuningOptions(in.MaxIndexSizeMB, in.MaxRuntimeSec)
		tuner := index.New(db, opts)
		if err := tuner.Precheck(ctx); err != nil {
			return errorResult(err), nil, nil
		}
		result, err := tuner.Analyze(ctx, workload)
		if err != nil {
			return errorResult(err), nil, nil
		}
		return textResult(result.Text()), result, nil
	})
}

type workloadIndexesInput struct {
	MaxIndexSizeMB int     `json:"max_index_size_mb,omitempty" jsonschema:"Cap on the total size of the recommended indexes. 0 means no cap"`
	MinCalls       int     `json:"min_calls,omitempty" jsonschema:"Only consider queries called at least this often. Defaults to 20"`
	MinMeanTimeMS  float64 `json:"min_mean_time_ms,omitempty" jsonschema:"Only consider queries whose mean time is at least this. Defaults to 5"`
	Limit          int     `json:"limit,omitempty" jsonschema:"How many statements to analyse. Defaults to 10, which is the maximum"`
	MaxRuntimeSec  int     `json:"max_runtime_seconds,omitempty" jsonschema:"Time budget for the search. Defaults to 30"`
}

func addWorkloadIndexes(srv *mcp.Server, db *pg.DB) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:  "analyze_workload_indexes",
		Title: "Analyze Workload Indexes",
		Description: "Recommend indexes for the workload pg_stat_statements has recorded, ranked by " +
			"total time. Use this when you do not know which queries are slow. Needs " +
			"pg_stat_statements, the hypopg extension, and recent ANALYZE.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: boolPtr(false)},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in workloadIndexesInput) (*mcp.CallToolResult, any, error) {
		opts := tuningOptions(in.MaxIndexSizeMB, in.MaxRuntimeSec)
		if in.MinCalls > 0 {
			opts.MinCalls = in.MinCalls
		}
		if in.MinMeanTimeMS > 0 {
			opts.MinMeanTimeMS = in.MinMeanTimeMS
		}
		if in.Limit > 0 && in.Limit < opts.MaxQueries {
			opts.MaxQueries = in.Limit
		}

		statements, err := db.SlowQueries(ctx, opts.MinCalls, opts.MinMeanTimeMS, opts.MaxQueries)
		if errors.Is(err, pg.ErrNoStatStatements) {
			return errorResult(errors.New(
				"pg_stat_statements is not available. It must be installed " +
					"(CREATE EXTENSION pg_stat_statements) and listed in " +
					"shared_preload_libraries, which needs a restart. " +
					"Until then, pass the queries to analyze_query_indexes by hand")), nil, nil
		}
		if err != nil {
			return errorResult(err), nil, nil
		}
		if len(statements) == 0 {
			return errorResult(errors.New(
				"pg_stat_statements has no query matching these thresholds. " +
					"Lower min_calls or min_mean_time_ms, or check that the server has been up long enough")), nil, nil
		}

		workload := make([]index.Query, 0, len(statements))
		for _, s := range statements {
			workload = append(workload, index.Query{Text: s.Text, Calls: float64(s.Calls), MeanTimeMS: s.MeanTimeMS})
		}

		tuner := index.New(db, opts)
		if err := tuner.Precheck(ctx); err != nil {
			return errorResult(err), nil, nil
		}
		result, err := tuner.Analyze(ctx, workload)
		if err != nil {
			return errorResult(err), nil, nil
		}
		return textResult(result.Text()), result, nil
	})
}

// tuningOptions turns the tool arguments into search options, keeping upstream's
// defaults for anything the caller did not set.
func tuningOptions(maxIndexSizeMB, maxRuntimeSec int) index.Options {
	opts := index.DefaultOptions()
	if maxIndexSizeMB > 0 {
		opts.MaxIndexSizeMB = maxIndexSizeMB
	}
	if maxRuntimeSec > 0 {
		opts.MaxRuntime = time.Duration(maxRuntimeSec) * time.Second
	}
	return opts
}
