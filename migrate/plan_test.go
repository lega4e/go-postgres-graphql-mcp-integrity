package migrate_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lega4e/gopgql/generator"
	"github.com/lega4e/gopgql/migrate"
	"github.com/lega4e/gopgql/schema"
)

// planContents renders a planned generation as the migration files it would be
// written as — the input Fold takes.
func planContents(planned []migrate.Migration) []string {
	out := make([]string, len(planned))
	for i, m := range planned {
		out[i] = m.Content()
	}
	return out
}

// suffixes names what a planned generation emits, which is the whole of the
// emission rule: which files, in which order.
func suffixes(planned []migrate.Migration) []string {
	out := make([]string, len(planned))
	for i, m := range planned {
		out[i] = m.Suffix
	}
	return out
}

// graphless is a schema that declares no property graph: the same tables, and
// nothing to expose them as. It is how "the SDL stopped declaring a graph" is
// expressed to the model, and the only way to ask for the graph to be dropped
// deliberately (design D4a).
func graphless(m *schema.Schema) *schema.Schema {
	return &schema.Schema{
		VertexTables: m.VertexTables,
		EdgeTables:   m.EdgeTables,
		Indexes:      m.Indexes,
	}
}

// TestPlanEmitsTheRunForEachSituation is design D2's table, asserted.
//
// Every row is one situation a generation can be in, and the assertion is which
// files it emits and in what order. The order is the whole point of the change:
// the teardown has to precede the table DDL because PostgreSQL refuses to alter
// a column a live property graph exposes, and the rebuild has to follow it.
func TestPlanEmitsTheRunForEachSituation(t *testing.T) {
	base := mustSchema(t, baseSDL)
	widened := mustSchema(t, widenedSDL)

	// A history with a graph over the base schema, folded back the way
	// generation folds it.
	priorWithGraph, err := migrate.FoldContent(planContents(
		migrate.Plan(nil, base, "init", 1, migrate.Halves{})))
	require.NoError(t, err, "fold the base history")

	// A history generated with --no-graph: the same tables, no graph.
	priorTablesOnly, err := migrate.FoldContent(planContents(
		migrate.Plan(nil, base, "init", 1, migrate.Halves{NoGraph: true})))
	require.NoError(t, err, "fold the tables-only history")

	for _, tc := range []struct {
		name    string
		prior   *schema.Schema
		desired *schema.Schema
		halves  migrate.Halves
		want    []string
	}{{
		name:    "first generation has nothing to tear down",
		prior:   nil,
		desired: base,
		want:    []string{migrate.SuffixTables, migrate.SuffixGraph},
	}, {
		name:    "table work under an existing graph",
		prior:   priorWithGraph,
		desired: widened,
		want: []string{
			migrate.SuffixGraphDown, migrate.SuffixTables, migrate.SuffixGraph,
		},
	}, {
		name:    "nothing changed",
		prior:   priorWithGraph,
		desired: base,
		want:    []string{},
	}, {
		name:    "the schema stops declaring a graph",
		prior:   priorWithGraph,
		desired: graphless(base),
		want:    []string{migrate.SuffixGraphDown},
	}, {
		name:    "graph half off, table work",
		prior:   priorTablesOnly,
		desired: widened,
		halves:  migrate.Halves{NoGraph: true},
		want:    []string{migrate.SuffixTables},
	}, {
		name:    "tables half off, first generation over foreign tables",
		prior:   nil,
		desired: base,
		halves:  migrate.Halves{NoTables: true},
		want:    []string{migrate.SuffixGraph},
	}, {
		name:    "tables half off, a graph already in the history",
		prior:   priorWithGraph,
		desired: widened,
		halves:  migrate.Halves{NoTables: true},
		want:    []string{migrate.SuffixGraphDown, migrate.SuffixGraph},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			planned := migrate.Plan(tc.prior, tc.desired, "add age", 7, tc.halves)
			assert.Equal(t, tc.want, suffixes(planned))
			for i, m := range planned {
				assert.Equal(t, 7+i, m.Version, "versions are consecutive from firstVersion")
				assert.Equal(t, "add_age", m.Slug, "one slug per generation, shared by every file")
			}
		})
	}
}

// TestPlanGraphOnlyChangeIsTwoFiles covers a change no table DDL is needed for:
// the graph comes down and a new definition goes up, in two files rather than one
// drop-and-create (design D2's rejected alternative — a Down that is not the
// plain inverse of its Up).
func TestPlanGraphOnlyChangeIsTwoFiles(t *testing.T) {
	base := mustSchema(t, baseSDL)

	// Relabelling the vertex changes the CREATE PROPERTY GRAPH and no table.
	relabelled := *base
	relabelled.VertexTables = append([]schema.VertexTable(nil), base.VertexTables...)
	relabelled.VertexTables[0].Label = "human"

	prior, err := migrate.FoldContent(planContents(
		migrate.Plan(nil, base, "init", 1, migrate.Halves{})))
	require.NoError(t, err, "fold")

	planned := migrate.Plan(prior, &relabelled, "relabel", 3, migrate.Halves{})
	require.Equal(t, []string{migrate.SuffixGraphDown, migrate.SuffixGraph}, suffixes(planned))
	for _, m := range planned {
		for _, body := range []string{m.Up, m.Down} {
			for _, stmt := range []string{"CREATE TABLE", "ALTER TABLE", "DROP TABLE"} {
				assert.NotContains(t, body, stmt, "a graph-only change touches no table")
			}
		}
	}
}

// TestEachDownIsTheInverseOfItsOwnUp is what makes a rollback walk a generation
// back out in reverse order: no file's Down reaches into another file's work
// (design D2).
func TestEachDownIsTheInverseOfItsOwnUp(t *testing.T) {
	base := mustSchema(t, baseSDL)
	widened := mustSchema(t, widenedSDL)
	prior, err := migrate.FoldContent(planContents(
		migrate.Plan(nil, base, "init", 1, migrate.Halves{})))
	require.NoError(t, err, "fold")

	planned := migrate.Plan(prior, widened, "add age", 3, migrate.Halves{})
	require.Len(t, planned, 3)
	teardown, tables, build := planned[0], planned[1], planned[2]

	// The teardown drops the graph the history held and restores exactly that
	// definition — not the new one.
	assert.Equal(t, "DROP PROPERTY GRAPH IF EXISTS app_graph;\n", teardown.Up)
	assert.Equal(t, generator.GraphDDL(prior)+"\n", teardown.Down)
	assert.NotContains(t, teardown.Down, "age", "the teardown restores the previous definition")

	// The tables file is the structural delta and its exact inverse.
	assert.Contains(t, tables.Up, "ALTER TABLE persons ADD COLUMN age integer;")
	assert.Contains(t, tables.Down, "ALTER TABLE persons DROP COLUMN age;")
	assert.NotContains(t, tables.Up, "PROPERTY GRAPH")
	assert.NotContains(t, tables.Down, "PROPERTY GRAPH")

	// The build creates the new definition and drops it again.
	assert.Equal(t, generator.GraphDDL(widened)+"\n", build.Up)
	assert.Equal(t, "DROP PROPERTY GRAPH IF EXISTS app_graph;\n", build.Down)
	assert.Contains(t, build.Up, "age", "the build exposes the column its own generation added")
}

// TestDeltaTablesIgnoresAGraphOnlyChange is the "emits nothing when its own
// concern is unchanged, even though the other concern changed" half of the
// renderer contract (task 2.5).
func TestDeltaTablesIgnoresAGraphOnlyChange(t *testing.T) {
	base := mustSchema(t, baseSDL)
	relabelled := *base
	relabelled.VertexTables = append([]schema.VertexTable(nil), base.VertexTables...)
	relabelled.VertexTables[0].Label = "human"

	up, down, changed := migrate.DeltaTables(base, &relabelled)
	assert.False(t, changed, "a relabelling is not table work")
	assert.Empty(t, up)
	assert.Empty(t, down)
}

// TestPlanTablesHalfOffTouchesNoTable is the partial-schema guarantee at the
// emission level (task 6.1): with the tables half off, not one statement about a
// table is emitted — not even from a prior state that has none.
func TestPlanTablesHalfOffTouchesNoTable(t *testing.T) {
	base := mustSchema(t, baseSDL)
	widened := mustSchema(t, widenedSDL)

	// A graph-only history: a property graph over tables no migration created.
	prior, err := migrate.FoldContent(planContents(
		migrate.Plan(nil, base, "init", 1, migrate.Halves{NoTables: true})))
	require.NoError(t, err, "fold a graph-only history")
	assert.Nil(t, prior.VertexTables[0].Columns,
		"a graph over tables the history never created folds with nil columns")

	planned := migrate.Plan(prior, widened, "add age", 2, migrate.Halves{NoTables: true})
	require.Equal(t, []string{migrate.SuffixGraphDown, migrate.SuffixGraph}, suffixes(planned))
	for _, m := range planned {
		for _, body := range []string{m.Up, m.Down} {
			for _, forbidden := range []string{"CREATE TABLE", "ALTER TABLE", "DROP TABLE", "INDEX"} {
				assert.NotContains(t, body, forbidden,
					"with the tables half off, %s must emit no %s", m.Filename(), forbidden)
			}
		}
	}
}

// TestFoldGraphlessHistoryKeepsTheDiffOrdered is design D6's first defect: a
// history with no CREATE PROPERTY GRAPH has nothing to classify its tables with,
// so the delta has to classify them against the desired schema — and the diff's
// ordering guarantees have to survive that.
func TestFoldGraphlessHistoryKeepsTheDiffOrdered(t *testing.T) {
	base := mustSchema(t, baseSDL)
	prior, err := migrate.FoldContent(planContents(
		migrate.Plan(nil, base, "init", 1, migrate.Halves{NoGraph: true})))
	require.NoError(t, err, "fold a graph-less history")
	require.Empty(t, prior.GraphName, "a graph-less fold declares no graph")

	// Dropping every table: the edge table has to go before the vertex table it
	// references, which only holds if the edge was recognised as an edge.
	up, _, changed := migrate.DeltaTables(prior, &schema.Schema{GraphName: base.GraphName})
	require.True(t, changed, "dropping every table is a change")
	edge := strings.Index(up, `DROP TABLE IF EXISTS follows;`)
	vertex := strings.Index(up, `DROP TABLE IF EXISTS persons;`)
	require.NotEqual(t, -1, edge, "the edge table must be dropped:\n%s", up)
	require.NotEqual(t, -1, vertex, "the vertex table must be dropped:\n%s", up)
	assert.Less(t, edge, vertex, "an edge table is dropped before the vertices it references")

	// And the other direction: created after them.
	up, _, changed = migrate.DeltaTables(&schema.Schema{}, base)
	require.True(t, changed)
	edge = strings.Index(up, `CREATE TABLE follows`)
	vertex = strings.Index(up, `CREATE TABLE persons`)
	require.NotEqual(t, -1, edge, "the edge table must be created:\n%s", up)
	require.NotEqual(t, -1, vertex, "the vertex table must be created:\n%s", up)
	assert.Less(t, vertex, edge, "an edge table is created after the vertices it references")
}

// TestFoldTakesTheGraphCreatedLast is design D6's third point, which is new to
// the single-directory layout: the history now holds DROP PROPERTY GRAPH
// statements *between* the creates, so folding the whole directory has to yield
// the definition created last. That is what the next teardown's Down is rendered
// from, so getting it wrong restores a graph nobody asked for.
func TestFoldTakesTheGraphCreatedLast(t *testing.T) {
	base := mustSchema(t, baseSDL)
	widened := mustSchema(t, widenedSDL)

	history := planContents(migrate.Plan(nil, base, "init", 1, migrate.Halves{}))
	prior, err := migrate.FoldContent(history)
	require.NoError(t, err)
	history = append(history, planContents(
		migrate.Plan(prior, widened, "add age", 3, migrate.Halves{}))...)

	folded, err := migrate.FoldContent(history)
	require.NoError(t, err, "fold a history holding a drop between two creates")
	assert.Equal(t, generator.GraphDDL(widened), generator.GraphDDL(folded),
		"the folded graph must be the one created last, not the one that was dropped")

	// Which means the next generation has nothing to do.
	assert.Empty(t, migrate.Plan(folded, widened, "again", 6, migrate.Halves{}),
		"regenerating against an unchanged schema emits nothing")
}

// TestFoldAfterTheGraphIsDroppedHasNoGraph covers the state a deliberate teardown
// leaves: the history's last graph statement is a drop, so the folded state has
// no graph and the next generation has nothing to tear down.
func TestFoldAfterTheGraphIsDroppedHasNoGraph(t *testing.T) {
	base := mustSchema(t, baseSDL)
	history := planContents(migrate.Plan(nil, base, "init", 1, migrate.Halves{}))
	prior, err := migrate.FoldContent(history)
	require.NoError(t, err)

	drop := migrate.Plan(prior, graphless(base), "drop graph", 3, migrate.Halves{})
	require.Equal(t, []string{migrate.SuffixGraphDown}, suffixes(drop))
	history = append(history, planContents(drop)...)

	folded, err := migrate.FoldContent(history)
	require.NoError(t, err)
	assert.Empty(t, folded.GraphName, "the graph was dropped and not rebuilt")

	// Declaring it again rebuilds it, with no teardown in front.
	assert.Equal(t, []string{migrate.SuffixGraph},
		suffixes(migrate.Plan(folded, base, "restore", 4, migrate.Halves{})))
}

// TestGenerateRefusesToDisownAHalfTheHistoryManages is design D4a in both
// directions: which halves a directory owns is fixed by its first generation, and
// a flag contradicting that is a sentinel error with nothing written.
func TestGenerateRefusesToDisownAHalfTheHistoryManages(t *testing.T) {
	base := mustSchema(t, baseSDL)
	widened := mustSchema(t, widenedSDL)

	for _, tc := range []struct {
		name  string
		first migrate.Halves
		then  migrate.Halves
		says  string
	}{{
		name:  "--no-graph against a history that creates a property graph",
		first: migrate.Halves{},
		then:  migrate.Halves{NoGraph: true},
		says:  "creates a property graph",
	}, {
		name:  "--no-tables against a history that creates tables",
		first: migrate.Halves{},
		then:  migrate.Halves{NoTables: true},
		says:  "creates tables",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			written, err := migrate.Generate(dir, base, "init", tc.first)
			require.NoError(t, err, "the first generation sets what the directory owns")
			require.NotEmpty(t, written)

			_, err = migrate.Generate(dir, widened, "add age", tc.then)
			require.Error(t, err, "the flag contradicts the history")
			assert.ErrorIs(t, err, migrate.ErrHalfDisowned, "the refusal must be recognisable")
			assert.Contains(t, err.Error(), tc.says)

			entries, dirErr := os.ReadDir(dir)
			require.NoError(t, dirErr)
			assert.Len(t, entries, len(written), "nothing may be written when the check fires")
		})
	}
}

// TestGenerateKeepsWorkingWhenTheFlagAgrees is the other half of D4a: a directory
// that never owned a half keeps generating with that half off, forever.
func TestGenerateKeepsWorkingWhenTheFlagAgrees(t *testing.T) {
	base := mustSchema(t, baseSDL)
	widened := mustSchema(t, widenedSDL)

	for _, tc := range []struct {
		name   string
		halves migrate.Halves
		want   []string
	}{{
		name:   "a directory that never owned the graph half",
		halves: migrate.Halves{NoGraph: true},
		want:   []string{"0002_add_age_tables.sql"},
	}, {
		name:   "a directory that never owned the tables half",
		halves: migrate.Halves{NoTables: true},
		want:   []string{"0002_add_age_graph_down.sql", "0003_add_age_graph.sql"},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			_, err := migrate.Generate(dir, base, "init", tc.halves)
			require.NoError(t, err, "first generation")

			paths, err := migrate.Generate(dir, widened, "add age", tc.halves)
			require.NoError(t, err, "the flag agrees with the history")
			names := make([]string, len(paths))
			for i, p := range paths {
				names[i] = filepath.Base(p)
			}
			assert.Equal(t, tc.want, names)
		})
	}
}

// TestOwnershipOfReadsTheStatements covers what the flag check is decided from:
// the CREATE statements in the migrations, and nothing recorded (design D1).
func TestOwnershipOfReadsTheStatements(t *testing.T) {
	base := mustSchema(t, baseSDL)
	for _, tc := range []struct {
		name   string
		halves migrate.Halves
		want   migrate.Ownership
	}{
		{"both halves", migrate.Halves{}, migrate.Ownership{Tables: true, Graph: true}},
		{"graph off", migrate.Halves{NoGraph: true}, migrate.Ownership{Tables: true}},
		{"tables off", migrate.Halves{NoTables: true}, migrate.Ownership{Graph: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			own, err := migrate.OwnershipOf(planContents(
				migrate.Plan(nil, base, "init", 1, tc.halves)))
			require.NoError(t, err)
			assert.Equal(t, tc.want, own)
		})
	}

	own, err := migrate.OwnershipOf(nil)
	require.NoError(t, err)
	assert.Equal(t, migrate.Ownership{}, own, "a directory with no history owns neither half")
}

// TestGenerateIgnoresAnEarlierLayout is design D7: there is no detection of, and
// no fallback to, any earlier layout. A directory holding the old combined
// migration, or the old per-half subdirectories, is generated into exactly as an
// empty one would be — the subdirectories are not even read.
func TestGenerateIgnoresAnEarlierLayout(t *testing.T) {
	base := mustSchema(t, baseSDL)

	t.Run("a combined migration from the original layout", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "0001_init.sql"),
			[]byte(migrate.Init(base)), 0o600))

		paths, err := migrate.Generate(dir, mustSchema(t, widenedSDL), "add age", migrate.Halves{})
		require.NoError(t, err, "an earlier layout is history like any other")
		// It is read as history, not detected as a layout: the combined file
		// creates both halves, so the generation is the ordinary run of three.
		assert.Equal(t, []string{
			"0002_add_age_graph_down.sql",
			"0003_add_age_tables.sql",
			"0004_add_age_graph.sql",
		}, baseNames(paths))
	})

	t.Run("per-half subdirectories from the merged layout", func(t *testing.T) {
		dir := t.TempDir()
		for _, sub := range []string{"tables", "graph"} {
			require.NoError(t, os.MkdirAll(filepath.Join(dir, sub), 0o750))
			require.NoError(t, os.WriteFile(filepath.Join(dir, sub, "0001_init.sql"),
				[]byte(migrate.Init(base)), 0o600))
		}
		paths, err := migrate.Generate(dir, base, "init", migrate.Halves{})
		require.NoError(t, err)
		assert.Equal(t, []string{"0001_init_tables.sql", "0002_init_graph.sql"},
			baseNames(paths), "subdirectories are not a history — this directory is empty")
	})
}

// TestErrHalfDisownedIsASentinel guards the branch the CLI makes on it.
func TestErrHalfDisownedIsASentinel(t *testing.T) {
	dir := t.TempDir()
	_, err := migrate.Generate(dir, mustSchema(t, baseSDL), "init", migrate.Halves{})
	require.NoError(t, err)
	_, err = migrate.Generate(dir, mustSchema(t, baseSDL), "again", migrate.Halves{NoGraph: true})
	require.True(t, errors.Is(err, migrate.ErrHalfDisowned))
}

// baseNames reduces written paths to their filenames.
func baseNames(paths []string) []string {
	out := make([]string, len(paths))
	for i, p := range paths {
		out[i] = filepath.Base(p)
	}
	return out
}

// TestAHalfCanBeTurnedOnLater is the asymmetry in design D4a that is easy to read
// past: a flag may not take a half *away* from a directory that manages it, but a
// directory that never managed one can start. That is the "gopgql manages the
// tables and the graph comes later" case the proposal names.
//
// The generation it produces is a single graph-build file — there is no graph in
// the history, so there is nothing to tear down first.
func TestAHalfCanBeTurnedOnLater(t *testing.T) {
	base := mustSchema(t, baseSDL)
	dir := t.TempDir()

	_, err := migrate.Generate(dir, base, "init", migrate.Halves{NoGraph: true})
	require.NoError(t, err, "tables only, to begin with")

	paths, err := migrate.Generate(dir, base, "add graph", migrate.Halves{})
	require.NoError(t, err, "turning a half on is not a contradiction")
	assert.Equal(t, []string{"0002_add_graph_graph.sql"}, baseNames(paths),
		"nothing to tear down, so just the build")

	// And the next generation behaves as any graph-bearing directory does.
	widened := mustSchema(t, widenedSDL)
	paths, err = migrate.Generate(dir, widened, "add age", migrate.Halves{})
	require.NoError(t, err)
	assert.Equal(t, []string{
		"0003_add_age_graph_down.sql",
		"0004_add_age_tables.sql",
		"0005_add_age_graph.sql",
	}, baseNames(paths))
}
