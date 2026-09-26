# Go rewrite plan — `postgres-mcp` (crystaldba) → Go

Target: a drop-in MCP server for Postgres in Go 1.26, published as
`github.com/dhafinrizqullah/postgre-mcp-go`.

Upstream reference: <https://github.com/crystaldba/postgres-mcp> (MIT, ~7.3k LOC Python + 6.4k LOC tests).

## Status

| Phase | State |
| --- | --- |
| 0 — skeleton, cgo spike, both transports | done |
| 1 — 6 tools | done |
| 1.5 — `analyze_db_health`, 7 checks | done |
| 2 — polish | not started |
| 3 — index tuning (DTA) | not started |

Verified end to end against a local PostgreSQL 18: all 7 tools return real data over
stdio and streamable HTTP, and the security table in `internal/safesql` rejects
multi-statement, write, function, and `LIKE $1` cases.

Two gaps to close before release:

- **The `hypopg` branch of `explain_query` is untested.** hypopg was not installable
  in the test environment, so only its "extension missing" path is verified.
  `brew install postgresql@18-hypopg`, or a CI service container, then exercise it.
- **Index bloat has never produced a finding.** The query is valid and runs, but the
  test tables are far below the 5 MB threshold that keeps small indexes out of the
  report. Verify against a table with a large, deleted-from index.


---

## 1. Position on the original 1:1 port

A literal 1:1 port is the wrong shape. The Python code is long mostly because of
*pglast* mechanics (hand-written AST visitors, `__slots__` recursion), not because
the DBA logic is complex. In Go those mechanics collapse into protobuf reflection
over the same `libpg_query` C parser.

So: **same behaviour, phased delivery, no 1:1 line-count target.**

| Upstream Python | LOC | Go plan |
| --- | --- | --- |
| `sql/safe_sql.py` | 1036 | ~250 (generic reflection walk) |
| `sql/bind_params.py` | 816 | Phase 3 (~300) |
| `sql/sql_driver.py` + `extension_utils.py` | 518 | ~180 (pgx) |
| `server.py` | 694 | ~250 (go-sdk) |
| `database_health/*` | 1117 | ~700 (mostly SQL + thresholds) |
| `explain/*` | ~400 | ~300 |
| `top_queries/*` | ~200 | ~180 |
| `index/*` (DTA + presentation + LLM opt) | 2158 | Phase 3 (~1200) |

Phase 1 lands at roughly **2.3k LOC Go** instead of 8k. The expensive 40% is
deferred, not lost.

---

## 2. Tool scope by phase

Upstream exposes 8 tools. Phases are ordered by value-per-line, not by upstream order.

### Phase 0 — skeleton (no feature code)

- `cmd/postgres-mcp`: flags + env config, `stdio` and `streamable-http` transport, graceful shutdown.
- `internal/pg`: `pgxpool`, `statement_timeout`, `default_transaction_read_only`, ping.
- `Dockerfile` (multi-stage, `golang:1.26`), README with Claude/Cursor config snippet.
- **Spike:** prove `pganalyze/pg_query_go/v6` compiles under Go 1.26 + Apple clang. This is the only
  real unknown in the whole plan (cgo). Budget 10 minutes; if it fails, fall back to §4 "no-AST mode".

Check: server starts, `tools/list` returns `[]`, SIGINT exits 0.

### Phase 1 — the 6 tools that matter (~1.4k LOC)

| Tool | Notes |
| --- | --- |
| `list_schemas` | pure catalog SQL, trivial |
| `list_objects` | pure catalog SQL, trivial |
| `get_object_details` | column/index/constraint/fk metadata; pgx `FieldDescriptions` gives result columns for free — no AST needed for this |
| `execute_sql` | safe-SQL validation (§3) + `READ ONLY` + timeout; rows returned as JSON |
| `get_top_queries` | `pg_stat_statements`; must degrade gracefully when the extension is absent |
| `explain_query` | `EXPLAIN` (plain/JSON) + optional `hypopg` hypothetical-index cost estimate; the one non-mechanical query in this phase |

`analyze_db_health` is Phase 1.5 — it is 7 independent, purely mechanical checks
(unused/dup/bloated indexes, buffer hit rate, connections, vacuum, sequences,
replication, constraints). 700 lines, zero design risk, huge perceived value.
Ship it as one more `internal/pg/health.go`.

### Phase 2 — polish

- Progress reporting on long tools (go-sdk supports it; upstream does it too).
- `hypopg` index-cost estimates in `explain_query` output.
- `CREATE EXTENSION` allowlist handling in safe-SQL.

### Phase 3 — index tuning (DTA) — opt-in

`analyze_query_indexes` + `analyze_workload_indexes`, ported from
`index/index_opt_base.py` + `dta_calc.py` + `presentation.py` (~1.2k LOC Go).
Includes the `sql/bind_params.py` port (`$n` → constant sampled from `pg_stats`,
plus the scoped column collector) because DTA and bind-param replacement share it.

**Explicitly not ported:** `index/llm_opt.py` (instructor/OpenAI). Upstream calls it
experimental; heuristic DTA plus `hypopg` covers the use case. Reconsider only if
someone asks for it explicitly.

**Explicitly not ported:** SSE transport. Deprecated in the MCP spec; `stdio` +
`streamable-http` cover current clients.

**Explicitly not ported:** the 6.4k LOC Python test suite. Replace with the checks in §6.

---

## 3. Safety model for `execute_sql` (4 layers, cheapest first)

Upstream's guarantee is "only read-only statements, restricted function set". Reproduce it
in this order of cost:

1. **Transaction level** — `default_transaction_read_only=on` on every pooled connection
   (via `AfterConnect`) plus `SET statement_timeout`. Postgres itself rejects writes,
   DDL, `SELECT INTO`, and temp-table creation. ~15 lines, no parser needed.
2. **Protocol level** — run every user statement through pgx's extended query protocol
   (default `QueryExecModeCacheStatement`). Postgres refuses more than one statement per
   `Parse` message, which kills `SELECT 1; DROP TABLE x` injection for free. No string
   splitting, no dollar-quote parsing.
3. **Statement allowlist** — first keyword must be in `SELECT, WITH, TABLE, VALUES, EXPLAIN,
   SHOW, ANALYZE, VACUUM, SET`; reject `EXPLAIN ANALYZE` and `SELECT ... FOR UPDATE/SHARE`.
4. **AST allowlist (Phase 1)** — parse with `pg_query_go/v6` (the exact C parser `pglast`
   wraps) and walk the protobuf tree generically:
   - every visited message descriptor full name must be in the allowed-node set
     (transcribed from `SafeSqlDriver.ALLOWED_NODE_TYPES`);
   - `FuncCall.funcname` must resolve to the allowed-function set;
   - `A_Expr` with `LIKE`/`ILIKE` must have a constant right-hand side;
   - `CreateExtensionStmt` only for the allowed extension names.

   The walk is ~40 lines of `protoreflect` recursion plus ~200 lines of transcription
   and special cases. This is where the Python version spends 1.8k lines; Go does not.

Layers 1–3 alone are already a defensible read-only guarantee. Layer 4 exists for parity
with upstream's function allowlist (blocking `pg_read_file`, `lo_import`, `dblink_*`).

---

## 4. Fallback: no-AST mode

If `pg_query_go` will not build (cgo pain), ship layers 1–3 and drop layer 4.
Cost: the function allowlist. Document that limitation in the README. Everything else in
Phases 0–1.5 is unaffected, so the fallback is a real plan and not a rewrite of one.

Distribution consequence of cgo: no `CGO_ENABLED=0` static builds, no easy cross-compile.
Acceptable — ship Docker (multi-arch via `docker buildx`) as the primary artifact, plus
`go install` for anyone with a C toolchain. `goreleaser` only if someone asks for
per-platform binaries.

---

## 5. Layout and dependencies

```
cmd/postgres-mcp/main.go        flags/env, transport, signals
internal/mcpserver/server.go    tool registration (go-sdk)
internal/pg/pool.go             pgxpool, read-only, timeouts, health
internal/pg/catalog.go          list_schemas, list_objects, get_object_details
internal/pg/health.go           7 health checks
internal/pg/topqueries.go       pg_stat_statements
internal/pg/explain.go          EXPLAIN + hypopg
internal/safesql/safesql.go     validate + allowlist walk
internal/safesql/allowlist.go   transcribed allow sets
```

Dependencies (versions verified 2026-09-26):

| Module | Version | Why |
| --- | --- | --- |
| `github.com/modelcontextprotocol/go-sdk` | v1.8.0 | official SDK, stdio + streamable HTTP, progress notifications |
| `github.com/jackc/pgx/v5` | v5.11.0 | driver + pool, `FieldDescriptions`, extended protocol by default |
| `github.com/pganalyze/pg_query_go/v6` | v6.2.2 | `libpg_query` bindings — identical parser to `pglast` |
| `github.com/dustin/go-humanize` | latest | upstream uses `humanize` for byte formatting; keep the output shape |

Nothing else. No ORM, no DI container, no config library, no CLI framework.

Config via env only (`DATABASE_URL` + `PG_*` overrides) with flag overrides on top.
Upstream's ~20 flags collapse into ~6; anything below that is env-only.

---

## 6. Verification (one check per phase, no framework)

- Phase 0: `go run ./cmd/postgres-mcp` starts; `tools/list` returns empty; SIGINT → exit 0.
- Phase 1: `internal/safesql/safesql_test.go` — table-driven, no DB needed. Assert allow/deny
  for ~25 statements including the nasty ones (`SELECT 1; DROP TABLE t`, `SELECT ... FOR UPDATE`,
  `pg_read_file('/etc/passwd')`, `lo_import`, `EXPLAIN ANALYZE`, `LIKE $1`, `dblink_exec`,
  `set_config`, `pg_sleep` if not allowlisted). This is the only unit test in the project that
  earns its keep, because it is the security boundary.
- Phase 1.5: `go test ./internal/pg/ -run TestHealth` against a live DB, skipped when
  `TEST_DATABASE_URL` is unset.
- Phase 2: manual — compare tool output against upstream Python for the same database,
  field by field, for one schema with views/sequences/foreign keys.
- Phase 3: manual — run `analyze_query_indexes` on a known-bad query and confirm the
  recommendation matches upstream's on the same input.
- Always: `go vet ./...`, `gofmt -l .`, and a `golangci-lint` run in CI.

---

## 7. Risks

| Risk | Impact | Mitigation |
| --- | --- | --- |
| `pg_query_go` breaks under Go 1.26 / Apple clang | blocks layer 4 | Phase 0 spike; §4 fallback |
| Upstream behaviour drift | subtle mismatches | phase 1.5 field-by-field diff against upstream on a real schema |
| Health-check SQL is PG-version sensitive | wrong numbers, not errors | note the minimum supported version in README; keep upstream's PG14 assumptions |
| Scope creep into a framework | schedule | no abstractions without a second caller; the layout above is the whole design |

---

## 8. Definition of done for v1

`stdio` + `streamable-http`; 7 tools (`list_schemas`, `list_objects`, `get_object_details`,
`execute_sql`, `get_top_queries`, `explain_query`, `analyze_db_health`); safe-SQL allowlist
tested; Dockerfile; README with install + client config; MIT licence and attribution to
crystaldba/postgres-mcp. Index tuning ships later as a feature branch, not a blocker.
