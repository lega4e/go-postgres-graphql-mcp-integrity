package bench_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lega4e/gopgql/compiler"
)

// benchmarksDoc is the committed results document the axes below are checked
// against.
var benchmarksDoc = filepath.Join("..", "..", "docs", "benchmarks.md")

// TestDocumentedAxesMatchTheBenchmark keeps docs/benchmarks.md from drifting
// away from the benchmark it documents (design D7).
//
// The axes are declared once, in bench_test.go, and this reads the document's
// axes table back and compares. Add an axis, forget the document, and CI goes
// red — which is the only mechanism that has ever kept a results file current.
func TestDocumentedAxesMatchTheBenchmark(t *testing.T) {
	content, err := os.ReadFile(benchmarksDoc)
	require.NoError(t, err, "docs/benchmarks.md is the committed output of `make bench`")

	rows := axesTable(t, string(content))

	assert.Equal(t, join(Depths), rows["depth"],
		"the depth axis in docs/benchmarks.md does not match bench_test.go's Depths")
	assert.Equal(t, join(Fanouts), rows["fan-out"],
		"the fan-out axis in docs/benchmarks.md does not match bench_test.go's Fanouts")

	strategies := make([]string, len(Strategies))
	for i, s := range Strategies {
		strategies[i] = s.String()
	}
	assert.Equal(t, strings.Join(strategies, ", "), rows["strategy"],
		"the strategy axis in docs/benchmarks.md does not match bench_test.go's Strategies")
}

// axesTable reads the `| axis | values |` table out of the document and returns
// it as a map. Parsing the document rather than searching it for substrings is
// what makes the assertion tell you *what* the document says when it is wrong.
func axesTable(t *testing.T, content string) map[string]string {
	t.Helper()

	const marker = "<!-- axes -->"
	_, after, found := strings.Cut(content, marker)
	require.True(t, found,
		"docs/benchmarks.md must carry an %s marker before the axes table, so this test knows where to read",
		marker)

	rows := map[string]string{}
	for _, line := range strings.Split(after, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "|") {
			if len(rows) > 0 {
				break // the table has ended
			}
			continue
		}
		cells := strings.Split(strings.Trim(line, "|"), "|")
		if len(cells) != 2 {
			continue
		}
		key := strings.TrimSpace(cells[0])
		if key == "axis" || strings.HasPrefix(key, "---") {
			continue
		}
		rows[key] = strings.TrimSpace(cells[1])
	}
	require.NotEmpty(t, rows, "found no axes table after %s", marker)
	return rows
}

func join(xs []int) string {
	out := make([]string, len(xs))
	for i, x := range xs {
		out[i] = strconv.Itoa(x)
	}
	return strings.Join(out, ", ")
}

// TestRowsShippedIsTheStrategysOwnNumber asserts the claim the benchmark exists
// to make visible, as an ordinary test rather than as a timing (task 7.6).
//
// A depth-d, fan-out-f query ships f^d rows to the client under Go-side shaping
// and exactly one row under SQL-side shaping. Those counts are deterministic —
// unlike every nanosecond figure the benchmark prints — so they are the part
// that gets asserted.
//
// It runs at the smallest fan-out the axes carry, because the claim is about the
// *shape* of the growth and building a 266k-vertex tree to reconfirm it would
// cost minutes for no extra evidence. The larger trees are the benchmark's job.
func TestRowsShippedIsTheStrategysOwnNumber(t *testing.T) {
	const fanout = 8
	f := fixtureFor(t, fanout)
	// Not t.Context(): the fixture is shared with the benchmarks, which outlive
	// this test.
	ctx := context.Background()

	for _, depth := range Depths {
		t.Run(fmt.Sprintf("depth=%d", depth), func(t *testing.T) {
			want := 1
			for range depth {
				want *= fanout
			}

			goSide := rowsUnder(ctx, t, f, depth, compiler.GoSide)
			sqlSide := rowsUnder(ctx, t, f, depth, compiler.SQLSide)

			assert.Equal(t, want, goSide,
				"Go-side shaping ships one flat row per matched path: %d^%d", fanout, depth)
			assert.Equal(t, 1, sqlSide,
				"SQL-side shaping ships one row, whatever the fan-out — that is the point of it")
			assert.Less(t, sqlSide, goSide,
				"at depth %d the SQL-side strategy should be shipping fewer rows", depth)
		})
	}
}

func rowsUnder(ctx context.Context, t *testing.T, f *fixture, depth int, s compiler.Shaping) int {
	t.Helper()
	cq, err := compiler.New(f.doc, compiler.WithShaping(s)).
		CompileQuery(queryFor(depth), map[string]any{"n": rootName})
	require.NoError(t, err)

	rows, _, err := measure(ctx, f.pool, cq)
	require.NoError(t, err, "execute under %s:\n%s", s, cq.SQL)
	return rows
}
