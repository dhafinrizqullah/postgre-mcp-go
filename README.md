# postgres-mcp-go

A read-only [MCP](https://modelcontextprotocol.io) server for PostgreSQL, in Go.

It gives an AI agent the tools it needs to work with a database — read the schema,
run a `SELECT`, get a query plan, check whether the database is healthy, and find out
which indexes are missing — and refuses everything else. It is a Go port of
[crystaldba/postgres-mcp](https://github.com/crystaldba/postgres-mcp) (MIT), with the
same nine tools.

## Why

Upstream is a Python server that needs a virtualenv, and its safe-SQL check is
hand-written against `pglast`. This port keeps the behaviour and changes two
things: the SQL is parsed by `libpg_query` through a protobuf walk instead of a
per-node visitor, and the whole thing is one static binary.

## Install

```sh
go install github.com/dhafinrizqullah/postgre-mcp-go/cmd/postgres-mcp-go@latest
```

Or with Docker:

```sh
docker build -t postgres-mcp-go .
```

`pg_query_go` uses cgo, so a C compiler is required (`build-essential`, Xcode
Command Line Tools, or the `golang:*-bookworm` image).

## Configure

Point it at a database. A read-only role is strongly recommended, since the
connection string is the only thing standing between the model and your data.

```sh
export DATABASE_URL='postgres://readonly@localhost:5432/app'
postgres-mcp-go                      # stdio, the default
postgres-mcp-go -transport http      # streamable HTTP on 127.0.0.1:8080
```

| Flag | Env | Default | Meaning |
|---|---|---|---|
| `-dsn` | `DATABASE_URL` | — | Connection string. Falls back to the standard `PG*` variables. |
| `-transport` | `MCP_TRANSPORT` | `stdio` | `stdio` or `http`. |
| `-addr` | `MCP_ADDR` | `127.0.0.1:8080` | Listen address for `http`. |
| `-max-conns` | | `4` | Pool size. |
| `-statement-timeout` | | `30s` | Server-side `statement_timeout` for every query. |
| `-verbose` | | `false` | Debug logging, on stderr. |

### Claude Desktop / Cursor

```json
{
  "mcpServers": {
    "postgres": {
      "command": "postgres-mcp-go",
      "env": { "DATABASE_URL": "postgres://readonly@localhost:5432/app" }
    }
  }
}
```

## Tools

| Tool | What it does |
|---|---|
| `list_schemas` | Every schema, with owner; system schemas last. |
| `list_objects` | Tables, views, sequences, or extensions in a schema. |
| `get_object_details` | Columns with types and defaults, constraints, index definitions. |
| `execute_sql` | One read-only statement, rows returned as JSON. |
| `explain_query` | `EXPLAIN (FORMAT JSON)`, optionally with hypothetical indexes via `hypopg`. |
| `get_top_queries` | Ranking from `pg_stat_statements` by resources, total time, or mean time. |
| `analyze_db_health` | Index, connection, vacuum, sequence, replication, buffer, constraint checks. |
| `analyze_query_indexes` | Index recommendations for a list of statements. |
| `analyze_workload_indexes` | Index recommendations for the recorded workload. |

Three optional extensions improve the output but are not required: `pg_stat_statements`
for `get_top_queries` and `analyze_workload_indexes`, `hypopg` for hypothetical indexes
and for both index tools, `pgstattuple` for index bloat. Without them the affected check
reports what to install and the rest work.

## Index tuning

`analyze_query_indexes` and `analyze_workload_indexes` do not guess. For every candidate
index they ask Postgres's own planner, through `hypopg`, what the workload would cost
with that index in place, and report the `CREATE INDEX` statements whose measured
effect is worth their size.

The search is greedy with a Pareto objective, so a 1000x speedup is accepted for 10x
the space and a 10% gain is not accepted for anything. Column candidates come from
walking the statement's syntax tree, so only columns that are actually filtered, joined,
grouped, or sorted are proposed, and a target-list alias is followed to the real column.

The numbers are planner cost estimates, not measurements, and the report says so. Check
them with `EXPLAIN` after creating the first index, and keep what the plan really uses.

## How read-only is enforced

Four independent layers. Removing any one of them weakens the guarantee, so keep
all four:

1. **Postgres itself.** Every connection sets `default_transaction_read_only=on`,
   so the server refuses writes, DDL, `SELECT INTO`, and temp tables.
2. **The wire protocol.** Statements go through the extended query protocol, where
   Postgres rejects more than one statement per `Parse` message. That is what
   stops `SELECT 1; DROP TABLE users`.
3. **Statement allowlist.** Only `SELECT`, `EXPLAIN`, `SHOW`, `VACUUM`, and a few
   others are accepted.
4. **AST allowlist.** The statement is parsed with `libpg_query` and the whole tree
   is walked: every node type must be known, every function must be on a 494-entry
   allowlist, `LIKE` patterns must be literals, and locking clauses and
   `EXPLAIN ANALYZE` are rejected. It fails closed, so a Postgres release that
   introduces a new node type causes a rejection rather than a silent pass.

The rules are transcribed from upstream's `safe_sql.py` by
`tools/gen_allowlist.py`, so the two servers agree on what is safe. The test in
`internal/safesql` covers the cases that matter, including `pg_read_file`,
`lo_import`, `dblink_connect`, `set_config`, `pg_sleep`, and `SELECT 1; DROP TABLE`.

`EXPLAIN ANALYZE` is not available. It executes the statement, which is the one
thing this server exists to avoid.

## Not ported

- **The LLM-driven index optimiser** (`index/llm_opt.py`). Upstream calls it
  experimental, it needs an OpenAI key, and the heuristic search plus `hypopg` covers
  the same ground.
- **The SSE transport.** Deprecated in the MCP spec; use stdio or streamable HTTP.

## Development

```sh
make test    # unit tests, no database needed
make lint    # golangci-lint
make smoke DATABASE_URL=...   # MCP handshake against a live database
```

CI runs the same tests plus an integration job against a real PostgreSQL 18 with
`pg_stat_statements`, `pgstattuple`, and `hypopg`. Set `TEST_DATABASE_URL` to run the
integration tests locally; they skip without it.

`AGENTS.md` has the layout, the security invariants, and the reasoning behind the
structure. `PLAN.md` records the phases and what each one cost.

## Licence

MIT. See [LICENSE](LICENSE), which also carries upstream's copyright.
