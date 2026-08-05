#!/usr/bin/env bash
# Renders `go test -bench` output as docs/benchmarks.md.
#
# The axes table it writes is checked against the Go declaration in
# test/bench/bench_test.go by TestDocumentedAxesMatchTheBenchmark, so adding an
# axis and forgetting to re-run `make bench` turns CI red (design D7).
#
# It reads the benchmark output on stdin and writes the document on stdout,
# so `make bench` is one pipeline and the raw output is never committed
# half-rendered.
set -euo pipefail

raw="$(cat)"

goversion="$(go version | awk '{print $3}')"
cpu="$(printf '%s\n' "$raw" | sed -n 's/^cpu: *//p' | head -1)"
[ -n "$cpu" ] || cpu="unrecorded"
os="$(printf '%s\n' "$raw" | sed -n 's/^goos: *//p' | head -1)"
arch="$(printf '%s\n' "$raw" | sed -n 's/^goarch: *//p' | head -1)"

cat <<EOF
# Shaping benchmarks

gopgql has two result-shaping strategies (SPEC.md §7 → M8). **Go-side** shaping
projects one flat column per selected field and regroups the rows in Go;
**SQL-side** shaping asks PostgreSQL to assemble the nested response with
\`json_build_object\` / \`json_agg\` and returns it as a single \`response\`
column. This is the measurement that says when to choose which.

Regenerate with \`make bench\`. Do not edit by hand — it is generated, and the
axes below are checked against the benchmark's own declaration on every CI run.

## Axes

<!-- axes -->
| axis | values |
| --- | --- |
| depth | 1, 2, 3 |
| fan-out | 1, 8, 64 |
| strategy | go-side, sql-side |

The fixture is a perfect *f*-ary tree of depth 3 whose ids are derived from a
fixed seed, so two runs build the same graph in the same order.

## What to read

\`ns/op\`, \`B/op\` and \`allocs/op\` come from the framework and are properties
of the machine below as much as of gopgql. The two columns that are properties
of the **strategy** are \`rows\` and \`recv-B\`: how many result rows the
database ships to the client, and how many bytes of column data it sends. A
depth-*d*, fan-out-*f* query ships *f^d* rows under Go-side shaping and exactly
one row under SQL-side shaping, whatever the fan-out.

Timings are never asserted by CI — a shared runner cannot hold them steady, and
a flaky performance gate gets switched off within a month. The row counts are
asserted, in \`test/bench\`, because they are deterministic.

## Summary

Paired from the run below: for each depth and fan-out, what each strategy costs.
\`rows\` and \`recv-B\` are what the database shipped; \`ms/op\` is this
machine.

$(printf '%s\n' "$raw" | awk '
  /^BenchmarkShaping\/depth=/ {
    split($1, p, "/")
    sub(/depth=/, "", p[2]); sub(/fanout=/, "", p[3])
    strat = p[4]; sub(/-[0-9]+$/, "", strat)
    key = p[2] "|" p[3]
    if (!(key in seen)) { order[++n] = key; seen[key] = 1 }
    for (i = 1; i <= NF; i++) {
      if ($i == "ns/op")   ns[key, strat]   = $(i-1)
      if ($i == "recv-B")  recv[key, strat] = $(i-1)
      if ($i == "rows")    rows[key, strat] = $(i-1)
    }
  }
  END {
    print "| depth | fan-out | rows go-side | rows sql-side | recv-B go-side | recv-B sql-side | ms/op go-side | ms/op sql-side |"
    print "| ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |"
    for (i = 1; i <= n; i++) {
      k = order[i]; split(k, d, "|")
      printf "| %s | %s | %d | %d | %d | %d | %.1f | %.1f |\n", d[1], d[2],
        rows[k,"go-side"], rows[k,"sql-side"],
        recv[k,"go-side"], recv[k,"sql-side"],
        ns[k,"go-side"]/1e6, ns[k,"sql-side"]/1e6
    }
  }')

## Results

Produced on:

| | |
| --- | --- |
| date | $(date -u +%Y-%m-%d) |
| go | ${goversion} |
| platform | ${os}/${arch} |
| cpu | ${cpu} |
| postgres | postgres:19beta2 (testcontainers) |

The timings below are from that machine and no other.

\`\`\`
$(printf '%s\n' "$raw" | grep -E '^(goos|goarch|pkg|cpu|Benchmark|PASS|ok)' || printf '%s\n' "$raw")
\`\`\`
EOF
