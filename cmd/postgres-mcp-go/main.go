// Command postgres-mcp-go is an MCP server for PostgreSQL. It gives an AI agent
// read-only access to a database: schema introspection, query execution, query
// plans, and health checks.
//
// It is a Go port of crystaldba/postgres-mcp (MIT). See README.md.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dhafinrizqullah/postgre-mcp-go/internal/mcpserver"
	"github.com/dhafinrizqullah/postgre-mcp-go/internal/pg"
)

// Exit codes follow Unix convention: 2 for a usage error, 1 for a runtime
// failure, 0 for success. 128+n is reserved for signal termination.
const (
	exitOK      = 0
	exitFailure = 1
	exitUsage   = 2
)

func main() {
	os.Exit(run())
}

func run() int {
	var (
		dsn              = flag.String("dsn", os.Getenv("DATABASE_URL"), "Postgres connection string or URL. Defaults to $DATABASE_URL, then the standard PG* variables.")
		transport        = flag.String("transport", envOr("MCP_TRANSPORT", "stdio"), "Transport to serve: stdio or http.")
		addr             = flag.String("addr", envOr("MCP_ADDR", "127.0.0.1:8080"), "Listen address for the http transport.")
		maxConns         = flag.Int("max-conns", 4, "Maximum pooled connections.")
		statementTimeout = flag.Duration("statement-timeout", 30*time.Second, "Server-side statement_timeout for every query.")
		verbose          = flag.Bool("verbose", false, "Log debug messages to stderr.")
		showVersion      = flag.Bool("version", false, "Print the version and exit.")
	)
	flag.Usage = usage
	flag.Parse()

	if *showVersion {
		fmt.Println(version())
		return exitOK
	}
	if flag.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "unexpected argument %q\n\n", flag.Arg(0))
		usage()
		return exitUsage
	}
	if *dsn == "" {
		fmt.Fprintln(os.Stderr, "no database URL: set DATABASE_URL or pass -dsn")
		usage()
		return exitUsage
	}
	if *transport != "stdio" && *transport != "http" {
		fmt.Fprintf(os.Stderr, "unknown transport %q: use stdio or http\n", *transport)
		return exitUsage
	}

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	// stderr, always: in stdio transport, stdout carries the protocol.
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	// SIGINT and SIGTERM cancel the root context, which unwinds the server and
	// closes the pool instead of dropping in-flight work.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := pg.Connect(ctx, pg.Options{
		DSN:              *dsn,
		MaxConns:         int32(*maxConns), //nolint:gosec // bounded by a flag, and the pool clamps it
		StatementTimeout: *statementTimeout,
		ApplicationName:  "postgres-mcp-go",
	})
	if err != nil {
		logger.Error("cannot reach the database", "error", err)
		return exitFailure
	}
	defer db.Close()

	mcpserver.Version = version()
	server := mcpserver.New(db, logger)

	if *transport == "stdio" {
		logger.Info("serving over stdio", "version", mcpserver.Version)
		// A closed stdin is how an MCP client says goodbye; it is not a failure.
		if err := server.Run(ctx, &mcp.StdioTransport{}); err != nil && !errors.Is(err, context.Canceled) {
			logger.Error("server stopped", "error", err)
			return exitFailure
		}
		return exitOK
	}

	return serveHTTP(ctx, server, *addr, logger)
}

func serveHTTP(ctx context.Context, server *mcp.Server, addr string, logger *slog.Logger) int {
	handler := mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return server },
		// Stateless suits an agent that opens a fresh connection per request and
		// avoids leaking sessions when a client disappears without a DELETE.
		&mcp.StreamableHTTPOptions{Stateless: true},
	)
	httpServer := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}

	errs := make(chan error, 1)
	go func() {
		logger.Info("serving over http", "addr", addr)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errs <- err
		}
		close(errs)
	}()

	select {
	case err := <-errs:
		if err != nil {
			logger.Error("http server stopped", "error", err)
			return exitFailure
		}
		return exitOK
	case <-ctx.Done():
		logger.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			logger.Error("graceful shutdown failed", "error", err)
			return exitFailure
		}
		return exitOK
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `postgres-mcp-go - read-only MCP server for PostgreSQL

Usage:
  postgres-mcp-go [flags]

Flags:
`)
	flag.PrintDefaults()
	fmt.Fprint(os.Stderr, `
Environment:
  DATABASE_URL   Postgres connection string or URL (or use -dsn)
  MCP_TRANSPORT  stdio (default) or http
  MCP_ADDR       listen address for the http transport

Examples:
  DATABASE_URL=postgres://user:pass@localhost:5432/app postgres-mcp-go
  postgres-mcp-go -transport http -addr 127.0.0.1:8080
`)
}

// version reports the build's module version, which `go install` sets from the
// tag, and falls back to the vcs revision for local builds.
func version() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "dev"
	}
	if v := info.Main.Version; v != "" && v != "(devel)" {
		return v
	}
	for _, setting := range info.Settings {
		if setting.Key == "vcs.revision" {
			if rev := setting.Value; rev != "" {
				if len(rev) > 12 {
					rev = rev[:12]
				}
				return "devel+" + rev
			}
		}
	}
	return "dev"
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
