BINARY  := postgres-mcp-go
PKG     := ./cmd/postgres-mcp-go
DATABASE_URL ?=

.PHONY: all build test integration smoke lint fmt vet run docker clean

all: lint test build

build:
	go build -o $(BINARY) $(PKG)

test:
	go test ./...

# Integration tests. They skip without a database; the CI job supplies one with
# pg_stat_statements, pgstattuple, and hypopg.
integration:
	@test -n "$(TEST_DATABASE_URL)" || { echo "set TEST_DATABASE_URL"; exit 2; }
	TEST_DATABASE_URL='$(TEST_DATABASE_URL)' go test ./...

# Unit tests plus the MCP handshake against a live database. Point DATABASE_URL at a
# throwaway Postgres: the handshake only reads, but analyze_db_health is chatty.
# The trailing sleep holds stdin open, so the server can answer before EOF.
smoke: build
	@test -n "$(DATABASE_URL)" || { echo "set DATABASE_URL"; exit 2; }
	@{ printf '%s\n' \
		'{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"smoke","version":"1"}}}' \
		'{"jsonrpc":"2.0","method":"notifications/initialized"}' \
		'{"jsonrpc":"2.0","id":2,"method":"tools/list"}' \
		'{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"list_schemas","arguments":{}}}' \
		'{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"analyze_db_health","arguments":{"health_type":"index,connection"}}}' \
		; sleep 2; } \
		| DATABASE_URL='$(DATABASE_URL)' ./$(BINARY) 2>/dev/null \
		| grep -c '"result"' | xargs -I{} sh -c 'test {} -eq 4 && echo "{} responses received, handshake OK" || { echo "expected 4 responses, got {}"; exit 1; }'

lint:
	golangci-lint run

fmt:
	gofmt -l -w .

vet:
	go vet ./...

run: build
	DATABASE_URL='$(DATABASE_URL)' ./$(BINARY)

docker:
	docker build -t $(BINARY):dev .

clean:
	rm -f $(BINARY)
