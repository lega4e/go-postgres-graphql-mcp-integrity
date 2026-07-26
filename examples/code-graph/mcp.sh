#!/bin/sh
# Launcher for the MCP server, used by .mcp.json.
#
# An agent spawns this and immediately starts the MCP handshake, so it must do
# as little as possible: bring the stack up yourself with `docker compose up -d`
# first, and this only attaches a server process to it.
#
#   --no-deps   don't re-evaluate the postgres/init/seed chain on every spawn.
#               That chain is `docker compose up -d`'s job; re-running it here
#               costs minutes across a session and stalls the handshake.
#   >&2         MCP speaks JSON-RPC on stdout, so nothing else may write there.
#   </dev/null  a build would otherwise consume the stdin the server reads the
#               protocol from.
#
# The build is a cold-clone fallback only: if the image is already there — which
# `docker compose up -d --build` leaves it — this is a single `run`.
set -e
cd "$(dirname "$0")"

IMAGE=gopgql-code-graph-mcp
if ! docker image inspect "$IMAGE" >/dev/null 2>&1; then
  echo "gopgql: building $IMAGE (first run only)…" >&2
  docker compose build mcp >&2 </dev/null
fi

exec docker compose run --rm -T --no-deps mcp
