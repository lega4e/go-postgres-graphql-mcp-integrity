# Shaping benchmarks

gopgql has two result-shaping strategies (SPEC.md §7 → M8). **Go-side** shaping
projects one flat column per selected field and regroups the rows in Go;
**SQL-side** shaping asks PostgreSQL to assemble the nested response with
`json_build_object` / `json_agg` and returns it as a single `response`
column. This is the measurement that says when to choose which.

Regenerate with `make bench`. Do not edit by hand — it is generated, and the
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

`ns/op`, `B/op` and `allocs/op` come from the framework and are properties
of the machine below as much as of gopgql. The two columns that are properties
of the **strategy** are `rows` and `recv-B`: how many result rows the
database ships to the client, and how many bytes of column data it sends. A
depth-*d*, fan-out-*f* query ships *f^d* rows under Go-side shaping and exactly
one row under SQL-side shaping, whatever the fan-out.

Timings are never asserted by CI — a shared runner cannot hold them steady, and
a flaky performance gate gets switched off within a month. The row counts are
asserted, in `test/bench`, because they are deterministic.

## Summary

Paired from the run below: for each depth and fan-out, what each strategy costs.
`rows` and `recv-B` are what the database shipped; `ms/op` is this
machine.

| depth | fan-out | rows go-side | rows sql-side | recv-B go-side | recv-B sql-side | ms/op go-side | ms/op sql-side |
| ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| 1 | 1 | 1 | 1 | 36 | 62 | 1.2 | 1.3 |
| 2 | 1 | 1 | 1 | 57 | 96 | 2.4 | 3.4 |
| 3 | 1 | 1 | 1 | 80 | 132 | 23.4 | 18.8 |
| 1 | 8 | 8 | 1 | 288 | 188 | 2.2 | 7.9 |
| 2 | 8 | 64 | 1 | 3648 | 1580 | 3.1 | 3.2 |
| 3 | 8 | 512 | 1 | 40960 | 13740 | 25.2 | 24.6 |
| 1 | 64 | 64 | 1 | 2359 | 1251 | 72.2 | 70.7 |
| 2 | 64 | 4096 | 1 | 244032 | 91107 | 122.4 | 119.7 |
| 3 | 64 | 262144 | 1 | 22323200 | 6591459 | 9413.2 | 6438.4 |

## Results

Produced on:

| | |
| --- | --- |
| date | 2026-08-04 |
| go | go1.26.4 |
| platform | darwin/amd64 |
| cpu | Intel(R) Core(TM) i5-7360U CPU @ 2.30GHz |
| postgres | postgres:19beta2 (testcontainers) |

The timings below are from that machine and no other.

```
goos: darwin
goarch: amd64
pkg: github.com/lega4e/gopgql/test/bench
cpu: Intel(R) Core(TM) i5-7360U CPU @ 2.30GHz
BenchmarkShaping/depth=1/fanout=1/go-side-4         	       3	   1165058 ns/op	        36.00 recv-B	         1.000 rows
BenchmarkShaping/depth=1/fanout=1/sql-side-4        	       3	   1321718 ns/op	        62.00 recv-B	         1.000 rows
BenchmarkShaping/depth=2/fanout=1/go-side-4         	       3	   2428240 ns/op	        57.00 recv-B	         1.000 rows
BenchmarkShaping/depth=2/fanout=1/sql-side-4        	       3	   3388624 ns/op	        96.00 recv-B	         1.000 rows
BenchmarkShaping/depth=3/fanout=1/go-side-4         	       3	  23447511 ns/op	        80.00 recv-B	         1.000 rows
BenchmarkShaping/depth=3/fanout=1/sql-side-4        	       3	  18776351 ns/op	       132.0 recv-B	         1.000 rows
BenchmarkShaping/depth=1/fanout=8/go-side-4         	       3	   2199897 ns/op	       288.0 recv-B	         8.000 rows
BenchmarkShaping/depth=1/fanout=8/sql-side-4        	       3	   7923985 ns/op	       188.0 recv-B	         1.000 rows
BenchmarkShaping/depth=2/fanout=8/go-side-4         	       3	   3147655 ns/op	      3648 recv-B	        64.00 rows
BenchmarkShaping/depth=2/fanout=8/sql-side-4        	       3	   3200140 ns/op	      1580 recv-B	         1.000 rows
BenchmarkShaping/depth=3/fanout=8/go-side-4         	       3	  25194480 ns/op	     40960 recv-B	       512.0 rows
BenchmarkShaping/depth=3/fanout=8/sql-side-4        	       3	  24593797 ns/op	     13740 recv-B	         1.000 rows
BenchmarkShaping/depth=1/fanout=64/go-side-4        	       3	  72229858 ns/op	      2359 recv-B	        64.00 rows
BenchmarkShaping/depth=1/fanout=64/sql-side-4       	       3	  70678813 ns/op	      1251 recv-B	         1.000 rows
BenchmarkShaping/depth=2/fanout=64/go-side-4        	       3	 122373722 ns/op	    244032 recv-B	      4096 rows
BenchmarkShaping/depth=2/fanout=64/sql-side-4       	       3	 119732337 ns/op	     91107 recv-B	         1.000 rows
BenchmarkShaping/depth=3/fanout=64/go-side-4        	       3	9413164690 ns/op	  22323200 recv-B	    262144 rows
BenchmarkShaping/depth=3/fanout=64/sql-side-4       	       3	6438438687 ns/op	   6591459 recv-B	         1.000 rows
PASS
ok  	github.com/lega4e/gopgql/test/bench	158.116s
```
