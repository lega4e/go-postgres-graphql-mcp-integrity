// Package migrate emits gopgql's schema as goose migration files and folds its
// own prior migrations back into a schema model.
//
// One edit of the SDL emits a **run of consecutive migrations**, each holding
// one kind of statement, into one directory (design D1, D2):
//
//	0003_add_email_graph_down.sql   DROP PROPERTY GRAPH IF EXISTS …
//	0004_add_email_tables.sql       ALTER TABLE …
//	0005_add_email_graph.sql        CREATE PROPERTY GRAPH …
//
// No migration ever mixes table DDL with property-graph DDL, and every file's
// Down section is the plain inverse of its own Up section, so `goose down` walks
// a generation back out in exactly reverse order.
//
// The order is not enforced by any code here. It is the file numbering: every
// migration lives in one directory in true chronological order and operates on
// the schema its predecessors produced, so a `CREATE PROPERTY GRAPH` is always
// immediately preceded by the table DDL of its *own* generation and by the drop
// of the graph the generation before it built. Applying is therefore goose's
// ordinary forward apply — `goose up` from an empty database is correct by
// construction (design D3), and gopgql contributes nothing to how migrations
// are applied.
//
// Fold interprets gopgql's own canonical goose statement set back into an
// in-memory schema.Schema, without a database and without a sidecar state
// artifact (SPEC.md §3 decision 6). Plan diffs that folded state against the
// desired state and renders the generation; Generate folds a directory, plans,
// and writes.
//
// It has no database dependency and compiles to WASM (SPEC.md §4.1).
package migrate

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/lega4e/gopgql/generator"
	"github.com/lega4e/gopgql/internal/pgident"
	"github.com/lega4e/gopgql/schema"
)

// quote renders an identifier for use in the Down section's DROP statements.
func quote(s string) string { return pgident.Quote(s) }

// dropGraphStmt renders "DROP PROPERTY GRAPH IF EXISTS <name>;".
//
// IF EXISTS because a generation is not atomic: goose runs each file in its own
// transaction, so an interrupted `goose up` can stop after the teardown and
// before the rebuild, and the next run must not fail on a graph that is already
// gone. It also lets the teardown be applied to a database that never had one.
func dropGraphStmt(name string) string {
	return fmt.Sprintf("DROP PROPERTY GRAPH IF EXISTS %s;", quote(name))
}

// Halves says which of a schema's two halves a generation manages.
//
// The zero value manages both: splitting is the default, and turning a half off
// is the deliberate act (design D4). The fields are negative so that they read
// as the flags they come from and so that "both on" needs no construction.
type Halves struct {
	NoTables bool // someone else owns the tables
	NoGraph  bool // no property graph is managed here
}

// Tables and Graph report whether each half is managed.
func (h Halves) Tables() bool { return !h.NoTables }
func (h Halves) Graph() bool  { return !h.NoGraph }

// The suffix each kind of file in a generation carries. It is a human-readable
// part of the filename and nothing ever reads it back: what a migration does is
// what its SQL does (design D1).
const (
	SuffixGraphDown = "graph_down"
	SuffixTables    = "tables"
	SuffixGraph     = "graph"
)

// Migration is one file of a generation: its version, the slug the whole
// generation shares, the suffix saying which of the three things it does, and
// the two bodies.
type Migration struct {
	Version int
	Slug    string
	Suffix  string
	Up      string
	Down    string
}

// Filename is the goose filename for the migration: NNNN_<slug>_<suffix>.sql.
//
// One slug per generation, shared by all of its files, so a generation reads as
// one unit in a directory listing and 0003 reads correctly as "the graph
// teardown step of the add_email generation" (design D2).
func (m Migration) Filename() string {
	return fmt.Sprintf("%04d_%s_%s.sql", m.Version, m.Slug, m.Suffix)
}

// Content is the migration file's text: the Up body and the Down body under
// goose's markers.
func (m Migration) Content() string { return section(m.Up, m.Down) }

// ErrHalfDisowned reports that a turned-off half contradicts the halves the
// migration directory's own history already manages (design D4a).
//
// It is a sentinel because a caller has to branch on it: the operator's next
// move depends on which half they tried to disown, and the CLI adds that
// guidance to the message.
var ErrHalfDisowned = errors.New(
	"a half cannot be turned off for a directory whose history already manages it")

// Plan renders the migrations one edit of the schema emits, in the order they
// must be applied. An unchanged schema plans nothing.
//
// prior is the state folded from the directory's own migrations (nil for a
// directory with no history); desired is the state the SDL describes.
//
//	Table work, a graph already in the history   graph_down, tables, graph
//	First generation (no graph yet)               tables, graph
//	Graph-only change, a graph in the history     graph_down, graph
//	Graph-only change, no graph yet               graph
//	Graph half off                                tables
//	Tables half off                               the graph file(s)
//	The schema stops declaring a graph            graph_down
//
// The versions are consecutive integers from firstVersion, assigned in emission
// order — that is what makes "consecutive" mean anything, and it keeps a
// generation's files adjacent and legible in a directory listing (design D2).
func Plan(prior, desired *schema.Schema, slug string, firstVersion int, halves Halves) []Migration {
	if prior == nil {
		prior = &schema.Schema{}
	}

	tablesUp, tablesDown, tablesChanged := "", "", false
	if halves.Tables() {
		tablesUp, tablesDown, tablesChanged = DeltaTables(prior, desired)
	}
	graphChanged := halves.Graph() && generator.GraphDDL(prior) != generator.GraphDDL(desired)

	// The teardown exists to unblock whatever follows it. PostgreSQL refuses to
	// drop or retype a column a live property graph exposes, so the graph has to
	// come down before its tables can move; and a graph of the same name cannot
	// be created twice, so it has to come down before it is rebuilt.
	teardown := halves.Graph() && hasGraph(prior) && (tablesChanged || graphChanged)
	// The build puts back whatever the generation should end with: a changed
	// definition, or the unchanged one the teardown above just took down.
	build := halves.Graph() && hasGraph(desired) && (graphChanged || teardown)

	var out []Migration
	version := firstVersion
	name := sanitizeName(slug)
	add := func(suffix, up, down string) {
		out = append(out, Migration{Version: version, Slug: name, Suffix: suffix, Up: up, Down: down})
		version++
	}

	if teardown {
		up, down := GraphTeardown(prior)
		add(SuffixGraphDown, up, down)
	}
	if tablesChanged {
		add(SuffixTables, tablesUp, tablesDown)
	}
	if build {
		up, down := GraphBuild(desired)
		add(SuffixGraph, up, down)
	}
	return out
}

// Generate folds the migrations already in dir, plans the generation the desired
// schema calls for, and writes it. It returns the paths written in apply order —
// nothing at all when the directory already agrees with desired.
//
// A turned-off half that contradicts the directory's own history is refused
// here, before anything is written (design D4a): which halves a directory owns
// is fixed by its first generation, and thereafter the history decides.
//
// slug is the descriptive part of every filename in the generation (e.g.
// "add_email"); it is sanitised to a safe snake_case token.
func Generate(dir string, desired *schema.Schema, slug string, halves Halves) ([]string, error) {
	files, err := migrationFiles(dir)
	if err != nil {
		return nil, err
	}
	contents, err := readMigrations(files)
	if err != nil {
		return nil, err
	}

	owned, err := OwnershipOf(contents)
	if err != nil {
		return nil, err
	}
	if err := owned.check(halves); err != nil {
		return nil, err
	}

	var prior *schema.Schema
	if len(contents) > 0 {
		if prior, err = FoldContent(contents); err != nil {
			return nil, err
		}
	}
	if err := checkManagement(owned, prior, desired); err != nil {
		return nil, err
	}

	next := 1
	if n := len(files); n > 0 {
		next = files[n-1].version + 1
	}
	planned := Plan(prior, desired, slug, next, halves)
	if len(planned) == 0 {
		if err := checkNothingOwedIsMissing(prior, desired, halves); err != nil {
			return nil, err
		}
		return nil, nil
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("migrate: create dir: %w", err)
	}
	paths := make([]string, 0, len(planned))
	for _, m := range planned {
		path := filepath.Join(dir, m.Filename())
		if err := os.WriteFile(path, []byte(m.Content()), 0o644); err != nil {
			return nil, fmt.Errorf("migrate: write %s: %w", path, err)
		}
		paths = append(paths, path)
	}
	return paths, nil
}

// ErrManagementChanged reports that a type's SDL stopped or started declaring
// @readonly, which no delta can express (SPEC.md §7 → M12).
var ErrManagementChanged = errors.New(
	"whether gopgql owns a table cannot change in a delta")

// checkManagement refuses a generation in which a table changed hands.
//
// Neither direction has a migration that could be written for it, and both fail
// *after* review rather than at generate time, which is what makes refusing here
// worth the check:
//
//   - **Dropping @readonly** (adoption). Nothing in the history creates the
//     table, so the fold holds it with no columns and the differ reads it as a
//     table that does not exist. The generation would carry a CREATE TABLE for a
//     table that is already there — a migration that reads perfectly and fails
//     at apply. Adopting a table means telling gopgql what is already in it,
//     which is a different change with its own SDL.
//
//   - **Adding @readonly** (disowning). The table's history stays in the
//     directory; gopgql simply stops emitting anything about it. Nothing marks
//     the handover, so a later reader cannot tell an abandoned table from one
//     nobody ever owned, and the next schema edit silently stops migrating a
//     table the database still has. Moving the graph half to a fresh --dir is
//     the deliberate way to do it.
//
// The evidence is a folded table's columns, and it is only readable when the
// directory manages tables at all: a --no-tables history folds *every* table
// without columns, so there the absence says nothing.
func checkManagement(owned Ownership, prior, desired *schema.Schema) error {
	if prior == nil || !owned.Tables {
		return nil
	}
	folded := map[string]bool{} // key → the history creates its table
	for _, vt := range prior.VertexTables {
		folded[vt.Key()] = len(vt.Columns) > 0
	}
	for _, vt := range desired.VertexTables {
		created, known := folded[vt.Key()]
		if !known {
			continue // a new table; either kind is fine
		}
		switch {
		case vt.Unmanaged && created:
			return fmt.Errorf("%s is now @readonly, but this directory's migrations created it: %w; "+
				"generate the graph half into a fresh --dir instead, so the history that owns the table stays "+
				"with the table", vt.QualifiedName(), ErrManagementChanged)
		case !vt.Unmanaged && !created:
			return fmt.Errorf("%s is no longer @readonly, but nothing in this directory creates it: %w; "+
				"a delta would emit CREATE TABLE for a table that already exists", vt.QualifiedName(),
				ErrManagementChanged)
		}
	}
	return nil
}

// ErrNothingWritten reports that a generation planned nothing while the SDL
// still declares owned tables the directory's own history never creates.
//
// It is a sentinel so a caller can tell "your schema is already applied" from
// "gopgql produced nothing and that is wrong", which are the same exit status
// and, until this existed, the same message.
var ErrNothingWritten = errors.New(
	"nothing was generated, but the schema declares owned tables this directory does not create")

// checkNothingOwedIsMissing refuses a no-op generation that leaves a table gopgql
// owns uncreated.
//
// "Already up to date" is only true if the history really does hold everything
// the SDL says gopgql owns. When it does not, an empty plan is a *wrong artifact*
// dressed as a successful run: exit 0, no files, no warning, and the first thing
// that knows is PostgreSQL at apply time — or, worse, a later migration whose
// foreign key references a table nothing created.
//
// This is the same failure class as gopgql#49's dropped edge label, and it is
// checked the same way: not against the specific bug that produced it, but
// against the invariant it broke. gopgql#53 reached it through withoutUnmanaged
// stripping owned vertex tables; anything else that drops an owned table between
// the SDL and the plan lands here too, because the check is on what the SDL
// declares against what the history holds and shares no code with the differ.
//
// The evidence a table was created is that the fold gave it columns — the same
// evidence checkManagement reads, and readable for the same reason: a --no-tables
// history folds every table without columns, so the question is only asked of a
// directory that generates tables at all.
func checkNothingOwedIsMissing(prior, desired *schema.Schema, halves Halves) error {
	if !halves.Tables() || desired == nil {
		return nil
	}
	created := map[string]bool{}
	if prior != nil {
		for _, vt := range prior.VertexTables {
			created[vt.Key()] = len(vt.Columns) > 0
		}
		for _, e := range prior.EdgeTables {
			created[e.Key()] = len(e.Columns) > 0
		}
	}

	var missing []string
	for _, vt := range desired.VertexTables {
		if !vt.Unmanaged && !created[vt.Key()] {
			missing = append(missing, vt.QualifiedName())
		}
	}
	for _, e := range desired.EdgeTables {
		if !e.Unmanaged && !created[e.Key()] {
			missing = append(missing, e.QualifiedName())
		}
	}
	if len(missing) == 0 {
		return nil
	}
	return fmt.Errorf("%w: %s. Every table above is one gopgql owns — no @readonly, no @relationship(table:) "+
		"onto somebody else's table — so a generation that writes nothing has left the schema unbuildable",
		ErrNothingWritten, strings.Join(missing, ", "))
}

// GraphTeardown renders the graph-teardown migration for the graph a history
// last created: Up drops it, Down puts that same definition back. It mentions
// no table in either direction.
func GraphTeardown(from *schema.Schema) (up, down string) {
	return dropGraphStmt(from.GraphName) + "\n", generator.GraphDDL(from) + "\n"
}

// GraphBuild renders the graph-build migration for the graph a schema describes:
// Up creates it, Down drops it. It mentions no table in either direction.
func GraphBuild(to *schema.Schema) (up, down string) {
	return generator.GraphDDL(to) + "\n", dropGraphStmt(to.GraphName) + "\n"
}

// hasGraph reports whether a schema declares a property graph.
//
// All three ways of having none land on the same answer: a history with no
// CREATE PROPERTY GRAPH folds to an empty graph name, a history whose last graph
// statement was a drop folds to an empty graph name, and a desired schema that
// declares no graph has one too. The vertex-table check is what keeps an
// otherwise empty schema from claiming a graph nothing could be built from.
func hasGraph(m *schema.Schema) bool {
	return m != nil && m.GraphName != "" && len(m.VertexTables) > 0
}

// Init renders both halves as one combined migration: the tables, then the
// graph.
//
// Nothing writes migrations in this shape — a generation is a run of
// single-purpose files (design D7, there is no combined layout any more). It is
// retained as the *reference* the split is measured against: the integration
// suite applies it to a fresh database and asserts the sequence produces the
// same one, and the fold tests use it to build a whole-schema history in one
// string.
func Init(m *schema.Schema) string {
	return section(generator.DDL(m), downDDL(m))
}

// downDDL is Init's inverse: the graph first (the tables it references cannot be
// dropped under it), then the tables.
func downDDL(m *schema.Schema) string {
	return dropGraphStmt(m.GraphName) + "\n" + downTablesDDL(m)
}

// section wraps an up and a down body in goose's markers.
func section(up, down string) string {
	var b strings.Builder
	b.WriteString("-- +goose Up\n")
	b.WriteString(up)
	b.WriteString("\n-- +goose Down\n")
	b.WriteString(down)
	return b.String()
}

// sanitizeName reduces an arbitrary migration name to a safe snake_case token so
// the emitted filename is a well-formed goose migration name. An empty result
// falls back to "delta".
func sanitizeName(name string) string {
	var b strings.Builder
	prevUnderscore := false
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevUnderscore = false
		default:
			if !prevUnderscore && b.Len() > 0 {
				b.WriteByte('_')
				prevUnderscore = true
			}
		}
	}
	out := strings.Trim(b.String(), "_")
	if out == "" {
		return "delta"
	}
	return out
}

// downTablesDDL builds the inverse of the table half: edge tables first (they
// carry foreign keys into vertex tables), then vertex tables, each in reverse
// creation order. Indexes are dropped implicitly with their tables. The property
// graph is not mentioned — dropping it belongs to the graph-teardown migration.
func downTablesDDL(m *schema.Schema) string {
	var stmts []string
	for i := len(m.EdgeTables) - 1; i >= 0; i-- {
		stmts = append(stmts, fmt.Sprintf("DROP TABLE IF EXISTS %s;", m.EdgeTables[i].QualifiedName()))
	}
	for i := len(m.VertexTables) - 1; i >= 0; i-- {
		// An unmanaged table is not dropped, for the same reason it is not
		// created: gopgql does not own it. The Down section of a generation
		// undoes what its Up section did, and its Up section did nothing here.
		if m.VertexTables[i].Unmanaged {
			continue
		}
		stmts = append(stmts, fmt.Sprintf("DROP TABLE IF EXISTS %s;", m.VertexTables[i].QualifiedName()))
	}
	return strings.Join(stmts, "\n") + "\n"
}
