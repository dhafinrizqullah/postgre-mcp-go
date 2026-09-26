# pg_query_go binds libpg_query with cgo, so this build needs a C compiler. That is
# the only reason the image is not `FROM scratch`: the binary links against libc.
FROM golang:1.26-bookworm AS build

RUN apt-get update && apt-get install -y --no-install-recommends \
        gcc libc6-dev \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /src

# Dependencies first, so a source-only change does not re-download the module cache.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=1 go build -trimpath -ldflags="-s -w" -o /out/postgres-mcp-go ./cmd/postgres-mcp-go

FROM gcr.io/distroless/base-debian12:nonroot

COPY --from=build /out/postgres-mcp-go /postgres-mcp-go

ENV MCP_TRANSPORT=stdio
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/postgres-mcp-go"]
