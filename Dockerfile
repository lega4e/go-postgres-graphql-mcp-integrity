# Builds both binaries the examples need: `gopgql` for the init container that
# migrates the schema, and `gopgql-mcp` for the MCP server itself.
FROM golang:1.27-alpine AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -o /out/gopgql ./cmd/gopgql \
 && CGO_ENABLED=0 go build -o /out/gopgql-mcp ./cmd/gopgql-mcp

FROM alpine:3.21

# postgresql-client is what the seed step runs; keeping it here means an example
# needs one image rather than two.
RUN apk add --no-cache postgresql17-client

COPY --from=build /out/gopgql /usr/local/bin/gopgql
COPY --from=build /out/gopgql-mcp /usr/local/bin/gopgql-mcp

# No default command: the init container runs `gopgql migrate`, the server
# container runs `gopgql-mcp`, and each example's compose file says which.
