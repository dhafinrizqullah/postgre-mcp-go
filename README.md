# postgres-mcp-go

A read-only [MCP](https://modelcontextprotocol.io) server for PostgreSQL, in Go.

It gives an AI agent the tools it needs to work with a database — read the schema,
run a `SELECT`, get a query plan, check whether the database is healthy — and
refuses everything else. It is a Go port of
[crystaldba/postgres-mcp](https://github.com/crystaldba/postgres-mcp) (MIT).

> **Status: v0.1.** Seven tools, matching upstream's behaviour. The index-tuning
> tools (`analyze_query_indexes`, `analyze_workload_indexes`) are not ported yet.
> See [Not ported yet](#not-ported-yet).

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

Two optional extensions improve the output but are not required: `pg_stat_statements`
for `get_top_queries`, `hypopg` for hypothetical indexes, `pgstattuple` for index
bloat. Without them the affected check reports what to install and the rest work.

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

## Not ported yet

- `analyze_query_indexes` and `analyze_workload_indexes` — upstream's
  dynamic-tuning-advisor search. It is the largest part of upstream and the most
  heuristic, so it is a separate piece of work rather than a rushed one.
- The LLM-driven index optimiser. Upstream calls it experimental.
- The SSE transport. It is deprecated in the MCP spec; use stdio or
  streamable HTTP.

## Development

```sh
make test    # unit tests, no database needed
make lint    # golangci-lint
make smoke DATABASE_URL=...   # MCP handshake against a live database
```

`AGENTS.md` has the layout, the security invariants, and the reasoning behind the
structure.

## Licence

MIT. See [LICENSE](LICENSE), which also carries upstream's copyright.
