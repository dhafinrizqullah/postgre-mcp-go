# AGENTS.md

Read-only MCP server for PostgreSQL, written in Go. It is a port of
[crystaldba/postgres-mcp](https://github.com/crystaldba/postgres-mcp) (MIT).

Before any Go coding, review, debugging, troubleshooting, or setup task, load the `samber/cc-skills-golang@golang-how-to` skill first — it routes to whichever other Go skills the task needs.

## Required Go skills

The following Go skills from `samber/cc-skills-golang` MUST always be applied when working on this project. Load them at the start of every Go-related task, regardless of whether the user explicitly mentions them.

- `samber/cc-skills-golang@golang-code-style`
- `samber/cc-skills-golang@golang-data-structures`
- `samber/cc-skills-golang@golang-design-patterns`
- `samber/cc-skills-golang@golang-documentation`
- `samber/cc-skills-golang@golang-error-handling`
- `samber/cc-skills-golang@golang-modernize`
- `samber/cc-skills-golang@golang-naming`
- `samber/cc-skills-golang@golang-safety`
- `samber/cc-skills-golang@golang-security`
- `samber/cc-skills-golang@golang-testing`
- `samber/cc-skills-golang@golang-troubleshooting`

Also load `samber/cc-skills-golang@golang-database` for anything touching `internal/pg`; it is the layer where SQL, pooling, and timeouts live.

## Layout

```
cmd/postgres-mcp-go/   flags, transport selection, signal handling; no business logic
internal/mcpserver/    one function per tool, registering handlers with the MCP SDK
internal/index/        index recommendations: AST column extraction and the greedy search
internal/pg/           every SQL statement the server sends, plus the health checks
internal/safesql/      decides whether a statement may be run at all
tools/gen_allowlist.py regenerates internal/safesql/allowlist_gen.go from upstream
```

Flat by decision, not by accident. There is one binary, one pool, and one caller per
query, so a repository layer or a per-feature package tree would only add names.
Add structure when there is a second caller, not before.

## Security invariants

This server runs SQL chosen by a language model against someone else's database. The
read-only guarantee is four independent layers. Keep all four; do not "simplify" one away.

1. `default_transaction_read_only=on` on every pooled connection (`internal/pg.Connect`).
   Postgres itself then refuses writes, DDL, `SELECT INTO`, and temp tables.
2. pgx's extended query protocol, where Postgres rejects a second statement in one
   `Parse` message. This is what stops `SELECT 1; DROP TABLE t`.
3. The statement-type allowlist in `safesql.Validate`.
4. The AST node and function allowlists in `safesql.Validate`.

Rules that follow from this:

- Never build SQL by concatenating caller input. Use `$1` placeholders. The one
  exception is an index definition handed to `hypopg`, which is parsed as SQL by the
  extension and so cannot be a bind parameter: `pg.HypopgIndexDefinition` validates
  every identifier against a strict pattern and quotes it, and
  `pg.CreateHypopgIndex` runs the whole statement past `safesql.Validate` before
  sending it.
- `internal/safesql` is an allowlist that fails closed. A Postgres release that adds a node
  type must cause a rejection, never a silent pass.
- Tool handlers validate before they query. A new tool that accepts SQL must call
  `safesql.Validate` first.
- Logs go to stderr, always. In stdio transport, stdout carries the JSON-RPC stream.

## Testing

`internal/safesql` has the only table test that earns its keep: it is the security
boundary. When you add a rule, add the statement that should be rejected to that table.
`internal/index` tests the AST walks and the parameter splicer, which is where a silent
mistake produces a wrong recommendation rather than an error. `internal/pg` tests the two
parsers that pick wrong behaviour quietly (`parseVersion`, `HypopgIndexDefinition`).

Everything else is verified against a real database, not mocked. To try it:

```sh
make test          # unit tests, no database needed
TEST_DATABASE_URL=... go test ./...   # adds the integration tests
make smoke DATABASE_URL=...           # MCP handshake against a live database
```

Do not add a test framework, a mocking library, or fixtures directory. If a check needs a
database, it is an integration test that skips without `TEST_DATABASE_URL`.

**Keep the integration job green.** It runs a real PostgreSQL 18 with
`pg_stat_statements`, `pgstattuple`, and `hypopg`, and it is the only thing that can see
catalog and extension behaviour. Six defects in the index tuner passed every unit test
and were caught only there; see `PLAN.md` for the list.

## Deliberate deviations from the skill defaults

- **stdlib `flag`, not Cobra + Viper.** One command, seven flags, and env overrides.
  Cobra's value is subcommand trees, which this does not have.
- **No DI container.** `main` constructs the pool and the server and passes them down.
- **No ORM, no repository interfaces.** Plain SQL in `internal/pg` is the documentation.
- **`map[string]any` rows, not generated structs.** Row shapes differ per query; a struct
  per query is noise. `jsonValue` in `internal/pg/pool.go` is the single place that turns a
  pgx value into JSON.
- **The column extractor resolves an unqualified column through the catalog.** Upstream
  attributes it to whichever table it happens to visit first, which recommends indexes for
  the wrong table. Here a column in scope for several tables is looked up, and only if that
  fails is it credited to all of them.

## Regenerating the safe-SQL allowlist

`internal/safesql/allowlist_gen.go` is generated from the upstream Python project's
`safe_sql.py`. It is checked in, so a normal build never needs Python. To refresh it after
upstream changes:

```sh
python3 tools/gen_allowlist.py ../postgres-mcp/src/postgres_mcp/sql/safe_sql.py \
  > internal/safesql/allowlist_gen.go && gofmt -w internal/safesql/allowlist_gen.go
```

Deliberate differences from upstream live in `goPortAdditions` in
`internal/safesql/safesql.go`, never in the generated file, so that diffing the generated
output against upstream stays a meaningful review.
