// Fold reconstructs a schema.Schema from gopgql's own prior goose migrations.
//
// It is an interpreter over the canonical statement set gopgql emits — CREATE
// TABLE, ALTER TABLE ADD/DROP COLUMN, ADD/DROP CONSTRAINT, RENAME TO / RENAME
// COLUMN, ALTER COLUMN SET/DROP DEFAULT, CREATE/DROP INDEX, CREATE/DROP PROPERTY
// GRAPH — not a general DDL parser (SPEC.md §7 → M2). The reading is done by the
// internal/ddl lexer+parser, which turns each statement into a typed AST node;
// this file is the interpreter that walks those nodes. Folding replays the
// -- +goose Up sections of every migration in order, so the resulting model is
// the state a database reaches after applying them all. That is what lets the
// generator emit a delta with no database and no sidecar state file (§3
// decision 6).
//
// The emitter (generator + this package's Init/Delta) and this interpreter are
// two halves of one contract: whatever gopgql writes, Fold must read back into
// the same model. The fold_test round-trips guard that; the M2 integration
// suite proves it against a real database.
package migrate

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/lega4e/gopgql/internal/ddl"
	"github.com/lega4e/gopgql/schema"
)

// migFile is one migration file on disk, with its parsed version number.
type migFile struct {
	path    string
	version int
}

// versionRe matches a goose migration filename: a leading integer version, an
// underscore, a name, and the .sql extension.
var versionRe = regexp.MustCompile(`^(\d+)_.*\.sql$`)

// migrationFiles lists the goose migration files in dir, sorted ascending by
// version. A missing directory yields no files (nothing has been generated).
func migrationFiles(dir string) ([]migFile, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("migrate: read dir %s: %w", dir, err)
	}
	var files []migFile
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		m := versionRe.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		v, err := strconv.Atoi(m[1])
		if err != nil {
			continue
		}
		files = append(files, migFile{path: filepath.Join(dir, e.Name()), version: v})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].version < files[j].version })
	return files, nil
}

// readMigrations reads the Up/Down text of every migration file, in ascending
// version order — the order folding and ownership both have to read them in.
func readMigrations(files []migFile) ([]string, error) {
	if len(files) == 0 {
		return nil, nil
	}
	contents := make([]string, len(files))
	for i, f := range files {
		data, err := os.ReadFile(f.path)
		if err != nil {
			return nil, fmt.Errorf("migrate: read %s: %w", f.path, err)
		}
		contents[i] = string(data)
	}
	return contents, nil
}

// Ownership is the set of halves a migration history manages.
//
// It is read out of the statements the migrations hold, because nothing is
// recorded: no mode marker, no half marker, no sidecar state (design D1). The
// evidence that a directory owns the graph half is a CREATE PROPERTY GRAPH in
// it; the evidence that it owns the tables half is a CREATE TABLE. Neither can
// drift out of agreement with the SQL beside it, because it *is* the SQL.
type Ownership struct {
	Tables bool
	Graph  bool
}

// OwnershipOf reads which halves an ordered list of migration contents manages.
//
// A directory with no history owns neither, which is what makes the flags scope
// a directory's *first* generation and nothing after it (design D4a).
func OwnershipOf(contents []string) (Ownership, error) {
	var own Ownership
	for i, content := range contents {
		stmts, err := ddl.Parse(upSection(content))
		if err != nil {
			return own, fmt.Errorf("migrate: read migration %d: %w", i+1, err)
		}
		for _, stmt := range stmts {
			switch stmt.(type) {
			case *ddl.CreateTableStmt:
				own.Tables = true
			case *ddl.CreatePropertyGraphStmt:
				own.Graph = true
			}
		}
	}
	return own, nil
}

// check refuses a turned-off half that the history already manages (design D4a).
//
// Turning a half off is a statement about what this directory generates from now
// on, and letting it disagree with the history is unsafe in both directions:
// suppressing the graph half would leave table DDL running against a live graph
// that may depend on the columns it alters, and suppressing the tables half
// would silently stop emitting table DDL the graph half will later name.
func (o Ownership) check(h Halves) error {
	switch {
	case h.NoGraph && o.Graph:
		return fmt.Errorf("--no-graph, but a migration in this directory creates a property graph: %w",
			ErrHalfDisowned)
	case h.NoTables && o.Tables:
		return fmt.Errorf("--no-tables, but a migration in this directory creates tables: %w",
			ErrHalfDisowned)
	default:
		return nil
	}
}

// Fold reads the migrations in dir and folds their Up sections into the schema
// model they collectively produce. An empty or missing directory yields a nil
// schema and no error: there is no prior state.
//
// The whole directory is folded, always. There is no bounded fold any more —
// the history is one chronological sequence, so the graph the folded state holds
// is the one created last, which is exactly the graph the next generation's
// teardown migration has to render (design D6).
func Fold(dir string) (*schema.Schema, error) {
	files, err := migrationFiles(dir)
	if err != nil {
		return nil, err
	}
	contents, err := readMigrations(files)
	if err != nil {
		return nil, err
	}
	if len(contents) == 0 {
		return nil, nil
	}
	return FoldContent(contents)
}

// FoldContent folds an ordered list of goose migration file contents into the
// schema model they produce. It is the database-free core shared by Fold (which
// reads files) and the WASM playground (which folds in-memory content).
func FoldContent(contents []string) (*schema.Schema, error) {
	f := newFolder()
	for i, content := range contents {
		stmts, err := ddl.Parse(upSection(content))
		if err != nil {
			return nil, fmt.Errorf("migrate: fold migration %d: %w", i+1, err)
		}
		for _, stmt := range stmts {
			if err := f.apply(stmt); err != nil {
				return nil, fmt.Errorf("migrate: fold migration %d: %w", i+1, err)
			}
		}
	}
	return f.build()
}

// folder accumulates the effect of applying migration statements in order.
type folder struct {
	// cols maps a table's *qualified key* to its columns in physical
	// (creation/append) order. Keying by the qualified name is what keeps two
	// tables of one name in two schemas apart, and the key is exactly what the
	// DDL reader hands back — quotes removed — so emitter and reader cannot
	// disagree about which table a statement named.
	cols map[string][]schema.Column
	// indexes maps an index name to its definition; idxOrder preserves the order
	// they were created so the folded schema is deterministic.
	indexes  map[string]schema.Index
	idxOrder []string
	// cons maps a table's qualified key to its named constraints by constraint
	// name: the
	// natural key's UNIQUE and every CHECK. A single-column UNIQUE is not here —
	// it folds onto its column's Unique flag, which is where the model keeps it.
	cons map[string]map[string]foldedConstraint
	// graph is the most recent CREATE PROPERTY GRAPH, which classifies tables as
	// vertices or edges and carries labels, properties and edge key metadata.
	graph *ddl.CreatePropertyGraphStmt
}

// foldedConstraint is a named constraint as the reader saw it. A DROP carries
// only a name, so the body is whatever the matching ADD (or CREATE TABLE) said.
type foldedConstraint struct {
	kind    string   // "UNIQUE" or "CHECK"
	columns []string // UNIQUE
	expr    string   // CHECK
}

func newFolder() *folder {
	return &folder{
		cols:    map[string][]schema.Column{},
		indexes: map[string]schema.Index{},
		cons:    map[string]map[string]foldedConstraint{},
	}
}

// apply interprets a single parsed statement.
func (f *folder) apply(stmt ddl.Statement) error {
	switch s := stmt.(type) {
	case *ddl.CreateTableStmt:
		return f.applyCreateTable(s)
	case *ddl.AlterTableStmt:
		return f.applyAlterTable(s)
	case *ddl.CreateIndexStmt:
		return f.applyCreateIndex(s)
	case *ddl.DropIndexStmt:
		f.removeIndex(s.Name)
		return nil
	case *ddl.DropTableStmt:
		return f.applyDropTable(s)
	case *ddl.CreatePropertyGraphStmt:
		f.graph = s
		return nil
	case *ddl.DropPropertyGraphStmt:
		// The following CREATE PROPERTY GRAPH re-establishes the graph; a bare
		// drop just clears it.
		f.graph = nil
		return nil
	default:
		return fmt.Errorf("unrecognised statement %T (fold interprets gopgql's own DDL only)", stmt)
	}
}

// column converts a parsed column definition to the schema model's column.
func column(c ddl.ColumnDef) schema.Column {
	col := schema.Column{
		Name:       c.Name,
		Type:       c.Type,
		Array:      c.Array,
		NotNull:    c.NotNull,
		PrimaryKey: c.PrimaryKey,
		Unique:     c.Unique,
		Default:    c.Default,
	}
	if c.References != nil {
		col.References = &schema.Reference{
			Schema: c.References.Schema, Table: c.References.Table, Column: c.References.Column,
		}
	}
	return col
}

func (f *folder) applyCreateTable(s *ddl.CreateTableStmt) error {
	key := schema.QualifiedKey(s.Schema, s.Name)
	cols := make([]schema.Column, 0, len(s.Columns))
	for _, c := range s.Columns {
		cols = append(cols, column(c))
	}
	f.cols[key] = cols
	delete(f.cons, key)
	// A vertex table arrives with its natural key and its checks written inline,
	// so the same constraints have to be read out of CREATE TABLE as out of a
	// later ALTER — otherwise a schema that declared them at birth would fold
	// back without them and the differ would propose them a second time.
	// PRIMARY KEY (…) on an edge table is anonymous and structural, and is
	// already carried by the columns, so it is skipped.
	for _, c := range s.Constraints {
		if c.Name == "" {
			continue
		}
		if err := f.applyConstraint(key, c.Name, c.Kind, c.Columns, c.Expr, true); err != nil {
			return err
		}
	}
	return nil
}

func (f *folder) applyAlterTable(s *ddl.AlterTableStmt) error {
	key := schema.QualifiedKey(s.Schema, s.Name)
	switch a := s.Action.(type) {
	case *ddl.AddColumn:
		f.cols[key] = append(f.cols[key], column(a.Column))
		return nil
	case *ddl.DropColumn:
		f.cols[key] = removeColumn(f.cols[key], a.Name)
		return nil
	case *ddl.AddConstraint:
		return f.applyConstraint(key, a.Name, a.Kind, a.Columns, a.Expr, true)
	case *ddl.DropConstraint:
		// A DROP carries only the name; the kind, columns and body are recovered
		// from the naming convention the emitter and PostgreSQL share.
		return f.applyConstraint(key, a.Name, "", nil, "", false)
	case *ddl.RenameTable:
		// RENAME TO cannot move a table between schemas, so the target keeps the
		// schema the statement named the table in.
		return f.renameTable(key, schema.QualifiedKey(s.Schema, a.NewName))
	case *ddl.RenameColumn:
		return f.renameColumn(key, a.Name, a.NewName)
	case *ddl.SetDefault:
		return f.setDefault(key, a.Column, a.Default)
	case *ddl.DropDefault:
		return f.setDefault(key, a.Column, "")
	default:
		return fmt.Errorf("ALTER TABLE %s: unsupported action %T", s.Name, s.Action)
	}
}

func (f *folder) applyCreateIndex(s *ddl.CreateIndexStmt) error {
	if _, exists := f.indexes[s.Name]; !exists {
		f.idxOrder = append(f.idxOrder, s.Name)
	}
	f.indexes[s.Name] = schema.Index{
		Name: s.Name, Schema: s.Schema, Table: s.Table, Columns: s.Columns, Method: s.Method,
	}
	return nil
}

// applyConstraint folds an ADD or DROP CONSTRAINT back onto the model. present
// says whether the constraint exists after the statement.
//
// Every constraint gopgql emits now has somewhere to live. The single-column
// UNIQUE that @unique maps to folds onto schema.Column's Unique (SPEC.md §7 →
// M6); the M7 forms — a CHECK body and the multi-column UNIQUE of a natural key
// — are held here by name and assembled in build() into schema.Column.Check,
// schema.VertexTable.Checks and schema.VertexTable.NaturalKey (design D6).
//
// Which of those a statement is, is decided by its *name*, via
// schema.ClassifyConstraint: a DROP carries nothing else, so the naming rule the
// emitter follows is the only channel through which a drop can say what it
// dropped. An ADD states its kind and body outright, and those are what get
// stored; the name still decides where.
//
// A name that fits none of the rules is deliberately *not* an error. Refusing a
// statement gopgql itself emitted would make the entire prior state unreadable —
// every table, every column, every graph — to preserve one constraint. The
// failure mode of ignoring it is bounded: the differ proposes a constraint the
// database already has, PostgreSQL refuses the duplicate name, and a migration
// stops without losing data.
func (f *folder) applyConstraint(key, name, kind string, cols []string, expr string, present bool) error {
	table := key
	tcols := f.cols[key]
	if tcols == nil {
		return fmt.Errorf("ALTER TABLE %s: constraint %s on an unknown table", table, name)
	}
	// A constraint name is derived from the bare table name — PostgreSQL's own
	// implicit names carry no schema — so it is classified against that.
	_, bareTable := schema.SplitKey(key)
	role, column, _ := schema.ClassifyConstraint(bareTable, name)
	switch role {
	case schema.RoleColumnUnique:
		for i := range tcols {
			if tcols[i].Name == column {
				tcols[i].Unique = present
				f.cols[key] = tcols
				return nil
			}
		}
		return fmt.Errorf("ALTER TABLE %s: constraint %s covers unknown column %q", table, name, column)
	case schema.RoleNaturalKey, schema.RoleColumnCheck, schema.RoleTableCheck:
		if !present {
			if m := f.cons[key]; m != nil {
				delete(m, name)
			}
			return nil
		}
		if f.cons[key] == nil {
			f.cons[key] = map[string]foldedConstraint{}
		}
		f.cons[key][name] = foldedConstraint{
			kind: strings.ToUpper(kind), columns: append([]string(nil), cols...), expr: expr,
		}
		return nil
	default:
		return nil
	}
}

// renameTable moves a table, and everything the database keeps pointing at it,
// to its new name.
//
// PostgreSQL renames by identity: foreign keys and indexes follow the table
// without being restated. The folded model has to follow it too, or the next
// delta reads a column whose REFERENCES names a table that no longer exists and
// rebuilds it — which is the drop-and-recreate the rename existed to avoid. The
// property graph needs no such fixup: every delta drops and recreates it
// (SPEC.md §7 → M2), so the graph in the folded state always comes from the
// same migration as the rename.
func (f *folder) renameTable(from, to string) error {
	cols, ok := f.cols[from]
	if !ok {
		return fmt.Errorf("ALTER TABLE %s: rename of a table that was never created", from)
	}
	delete(f.cols, from)
	f.cols[to] = cols
	// Constraints follow the table but keep their names: PostgreSQL renames no
	// constraint when a table is renamed. The folded model must show the same,
	// so the delta that renamed the table can be seen to have dropped the
	// stale-named constraints and added their new-named replacements.
	if c := f.cons[from]; c != nil {
		delete(f.cons, from)
		f.cons[to] = c
	}
	toSchema, toName := schema.SplitKey(to)
	for _, tcols := range f.cols {
		for i := range tcols {
			if r := tcols[i].References; r != nil && schema.QualifiedKey(r.Schema, r.Table) == from {
				r.Schema, r.Table = toSchema, toName
			}
		}
	}
	for _, n := range f.idxOrder {
		if idx := f.indexes[n]; schema.QualifiedKey(idx.Schema, idx.Table) == from {
			idx.Schema, idx.Table = toSchema, toName
			f.indexes[n] = idx
		}
	}
	return nil
}

// renameColumn renames a column and the references and index entries that name
// it, for the same reason renameTable does.
func (f *folder) renameColumn(table, from, to string) error {
	cols, ok := f.cols[table]
	if !ok {
		return fmt.Errorf("ALTER TABLE %s: rename of a column on a table that was never created", table)
	}
	found := false
	for i := range cols {
		if cols[i].Name == from {
			cols[i].Name = to
			found = true
		}
	}
	if !found {
		return fmt.Errorf("ALTER TABLE %s: rename of unknown column %q", table, from)
	}
	for _, tcols := range f.cols {
		for i := range tcols {
			if r := tcols[i].References; r != nil &&
				schema.QualifiedKey(r.Schema, r.Table) == table && r.Column == from {
				r.Column = to
			}
		}
	}
	for _, n := range f.idxOrder {
		idx := f.indexes[n]
		if schema.QualifiedKey(idx.Schema, idx.Table) != table {
			continue
		}
		renamed := append([]string(nil), idx.Columns...)
		for i, c := range renamed {
			if c == from {
				renamed[i] = to
			}
		}
		idx.Columns = renamed
		f.indexes[n] = idx
	}
	// A UNIQUE constraint's column list follows the rename too, for the same
	// reason: the constraint is defined on the column, not on its name.
	for name, c := range f.cons[table] {
		if len(c.columns) == 0 {
			continue
		}
		moved := append([]string(nil), c.columns...)
		for i, col := range moved {
			if col == from {
				moved[i] = to
			}
		}
		c.columns = moved
		f.cons[table][name] = c
	}
	return nil
}

// setDefault records a column's default, or clears it when expr is empty. A
// default is a property of the column, so changing one must never be folded (or
// emitted) as a drop and an add: the column's data has nothing to do with it
// (design D6).
func (f *folder) setDefault(table, column, expr string) error {
	cols, ok := f.cols[table]
	if !ok {
		return fmt.Errorf("ALTER TABLE %s: default on a table that was never created", table)
	}
	for i := range cols {
		if cols[i].Name == column {
			cols[i].Default = expr
			f.cols[table] = cols
			return nil
		}
	}
	return fmt.Errorf("ALTER TABLE %s: default on unknown column %q", table, column)
}

func (f *folder) applyDropTable(s *ddl.DropTableStmt) error {
	key := schema.QualifiedKey(s.Schema, s.Name)
	delete(f.cols, key)
	delete(f.cons, key)
	// Indexes on a dropped table go with it.
	for _, idx := range f.indexList() {
		if schema.QualifiedKey(idx.Schema, idx.Table) == key {
			f.removeIndex(idx.Name)
		}
	}
	return nil
}

func (f *folder) removeIndex(name string) {
	if _, ok := f.indexes[name]; !ok {
		return
	}
	delete(f.indexes, name)
	for i, n := range f.idxOrder {
		if n == name {
			f.idxOrder = append(f.idxOrder[:i], f.idxOrder[i+1:]...)
			break
		}
	}
}

func (f *folder) indexList() []schema.Index {
	if len(f.idxOrder) == 0 {
		// Match the generator, which leaves Indexes nil when there are none,
		// so folded and freshly built schemas compare equal.
		return nil
	}
	out := make([]schema.Index, 0, len(f.idxOrder))
	for _, n := range f.idxOrder {
		out = append(out, f.indexes[n])
	}
	return out
}

// buildTablesOnly assembles a schema from the CREATE TABLE statements alone,
// for a directory that never declared a property graph. It carries no labels,
// no properties and no edge metadata, because nothing in such a directory says
// what they would be.
//
// It does carry the named constraints, though. Those live in the CREATE TABLE
// and ALTER TABLE statements the directory does hold, and attachConstraints
// recovers them from their names alone (schema.ClassifyConstraint) — no graph is
// involved. Skipping them would break the round trip design D6 rests on in
// exactly the tables half: the natural key and every check would fold back out
// of a tables-only history missing, so DeltaTables would propose them again on
// every single run and PostgreSQL would refuse the duplicate constraint name.
func (f *folder) buildTablesOnly() (*schema.Schema, error) {
	m := &schema.Schema{}
	names := make([]string, 0, len(f.cols))
	for name := range f.cols {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, key := range names {
		schemaName, name := schema.SplitKey(key)
		vt := schema.VertexTable{Name: name, Schema: schemaName, Columns: f.cols[key]}
		if err := f.attachConstraints(&vt); err != nil {
			return nil, err
		}
		m.VertexTables = append(m.VertexTables, vt)
	}
	m.Indexes = f.indexList()
	return m, nil
}

// build assembles the folded schema from the accumulated tables, indexes and
// graph. The graph classifies every table as a vertex or an edge and supplies
// labels, property lists and edge key metadata; the CREATE TABLE statements
// supply the columns.
func (f *folder) build() (*schema.Schema, error) {
	if f.graph == nil {
		// A tables-only migration directory (gopgql#38) has no CREATE PROPERTY
		// GRAPH to classify its tables with, and that is not an error — the
		// graph half is generated and applied somewhere else, or by nobody.
		// Return the tables as they were created; DeltaTables classifies them
		// against the desired schema, which is the only place the roles are
		// known. Vertex is the neutral bucket here, not a claim.
		return f.buildTablesOnly()
	}
	m := &schema.Schema{GraphName: f.graph.Name}
	for _, v := range f.graph.Vertices {
		// A missing CREATE TABLE is not an error: a graph-only directory
		// (gopgql#38) declares a property graph over tables owned elsewhere and
		// never creates them. The columns stay nil — the graph half's diff
		// compares the graph statement, which is self-describing, and the SDL
		// is a description of the slice being surfaced, not an inventory of the
		// database.
		// The columns come from the CREATE TABLE statements, which are never
		// schema-qualified (gopgql qualifies only tables it does not create), so
		// a qualified graph entry finds none — correctly: a table gopgql does
		// not own has no CREATE TABLE in this history to find them in.
		cols := f.cols[schema.QualifiedKey(v.Schema, v.Table)]
		var extra []schema.LabelProperties
		for _, l := range v.ExtraLabels {
			extra = append(extra, schema.LabelProperties{Label: l.Label, Properties: l.Properties})
		}
		vt := schema.VertexTable{
			Name:        v.Table,
			Schema:      v.Schema,
			Label:       v.Label,
			Columns:     cols,
			Properties:  v.Properties,
			ExtraLabels: extra,
		}
		if err := f.attachConstraints(&vt); err != nil {
			return nil, err
		}
		m.VertexTables = append(m.VertexTables, vt)
	}
	for _, e := range f.graph.Edges {
		cols := f.cols[schema.QualifiedKey(e.Schema, e.Table)]
		m.EdgeTables = append(m.EdgeTables, schema.EdgeTable{
			Name:         e.Table,
			Schema:       e.Schema,
			Label:        e.Label,
			Columns:      cols,
			SourceKey:    e.SourceKey,
			SourceSchema: e.SourceSchema,
			SourceTable:  e.SourceTable,
			SourceRef:    e.SourceRef,
			DestKey:      e.DestKey,
			DestSchema:   e.DestSchema,
			DestTable:    e.DestTable,
			DestRef:      e.DestRef,
			Properties:   e.Properties,
		})
	}
	m.Indexes = f.indexList()
	return m, nil
}

// attachConstraints moves the named constraints accumulated for a table onto the
// vertex table being assembled: a column check onto its column, a table-level
// check into Checks (ordered by the ordinal in its name), and the natural key
// into NaturalKey.
//
// Which constraint is which is decided by its name, so this needs no property
// graph and both builders use it — the graph-classified one and the tables-only
// one (gopgql#38).
//
// This is the reconstruction half of design D6, and it is what makes the round
// trip close: a schema that declares a check or a natural key generates DDL,
// folds back into a model that still carries them, and therefore diffs to no
// migration on the second run. Without it the differ would re-propose a
// constraint the database already has and PostgreSQL would refuse the duplicate
// name.
func (f *folder) attachConstraints(vt *schema.VertexTable) error {
	byName := f.cons[vt.Key()]
	if len(byName) == 0 {
		return nil
	}
	names := make([]string, 0, len(byName))
	for name := range byName {
		names = append(names, name)
	}
	sort.Strings(names)

	type numbered struct {
		n    int
		expr string
	}
	var tableChecks []numbered
	for _, name := range names {
		c := byName[name]
		role, column, ordinal := schema.ClassifyConstraint(vt.Name, name)
		switch role {
		case schema.RoleNaturalKey:
			vt.NaturalKey = &schema.NaturalKey{
				Name: name, Columns: append([]string(nil), c.columns...),
			}
		case schema.RoleColumnCheck:
			found := false
			for i := range vt.Columns {
				if vt.Columns[i].Name == column {
					vt.Columns[i].Check = c.expr
					found = true
					break
				}
			}
			if !found {
				return fmt.Errorf("migrate: constraint %s on %q checks unknown column %q", name, vt.Name, column)
			}
		case schema.RoleTableCheck:
			tableChecks = append(tableChecks, numbered{n: ordinal, expr: c.expr})
		}
	}
	sort.Slice(tableChecks, func(i, j int) bool { return tableChecks[i].n < tableChecks[j].n })
	for _, tc := range tableChecks {
		vt.Checks = append(vt.Checks, tc.expr)
	}
	return nil
}

// upSection returns the -- +goose Up portion of a goose migration file: the
// lines between the Up marker and the Down marker (or end of file). Comment
// lines are dropped so only executable statements remain.
func upSection(content string) string {
	var b strings.Builder
	inUp := false
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "-- +goose") {
			inUp = strings.EqualFold(trimmed, "-- +goose Up")
			continue
		}
		if !inUp {
			continue
		}
		// Strip any trailing line comment; gopgql emits none, but be tolerant.
		if i := strings.Index(line, "--"); i >= 0 {
			line = line[:i]
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}

// removeColumn returns cols without the column named name.
func removeColumn(cols []schema.Column, name string) []schema.Column {
	out := cols[:0:0]
	for _, c := range cols {
		if c.Name != name {
			out = append(out, c)
		}
	}
	return out
}
