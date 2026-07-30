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

// Half names one of the two halves of a schema a migration directory manages.
// It spells itself the way the flag that turns it off does.
type Half int

const (
	TablesHalf Half = iota
	GraphHalf
)

// String is the half's name, as `--no-<name>` spells it.
func (h Half) String() string {
	if h == GraphHalf {
		return "graph"
	}
	return "tables"
}

// creates is the statement whose presence in a history is the evidence that a
// directory owns the half (design D1). It is what a refusal names as the thing
// it saw, so the message points at the SQL the operator can go and read.
func (h Half) creates() string {
	if h == GraphHalf {
		return "creates a property graph"
	}
	return "creates tables"
}

// ErrHalfDisowned reports that a turned-off half contradicts the halves the
// migration directory's own history already manages (design D4a).
//
// Every such refusal is a [HalfConflictError] and matches this sentinel, which
// remains the umbrella for a caller that only wants to know that the halves and
// the history disagree, not which half did.
var ErrHalfDisowned = errors.New(
	"a half cannot be turned off for a directory whose history already manages it")

// HalfConflictError is the refusal itself, carrying the half it is about.
//
// The half is a field rather than prose because the CLI has to branch on it:
// the operator's next move differs per half. Recovering the half from the
// *flags* instead — which is what the CLI used to do — works only while
// --no-tables and --no-graph can never both be set, an invariant held in
// another package with nothing asserting the link. Carrying the half on the
// error lets the branch answer the error it was actually given.
type HalfConflictError struct {
	// Half is the half the requested halves and the history disagree about.
	Half Half
}

func (e *HalfConflictError) Error() string {
	return fmt.Sprintf("--no-%s, but a migration in this directory %s: %s",
		e.Half, e.Half.creates(), ErrHalfDisowned)
}

// Is matches the umbrella sentinel, so errors.Is(err, ErrHalfDisowned) keeps
// recognising every one of these refusals.
func (e *HalfConflictError) Is(target error) bool { return target == ErrHalfDisowned }

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

	next := 1
	if n := len(files); n > 0 {
		next = files[n-1].version + 1
	}
	planned := Plan(prior, desired, slug, next, halves)
	if len(planned) == 0 {
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
		stmts = append(stmts, fmt.Sprintf("DROP TABLE IF EXISTS %s;", quote(m.EdgeTables[i].Name)))
	}
	for i := len(m.VertexTables) - 1; i >= 0; i-- {
		stmts = append(stmts, fmt.Sprintf("DROP TABLE IF EXISTS %s;", quote(m.VertexTables[i].Name)))
	}
	return strings.Join(stmts, "\n") + "\n"
}
