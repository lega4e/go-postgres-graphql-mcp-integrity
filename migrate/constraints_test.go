package migrate_test

import (
	"strings"
	"testing"

	"github.com/lega4e/gopgql/migrate"
	"github.com/lega4e/gopgql/schema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// foldOf folds a chain of migration bodies — the init file, then each delta's Up
// body wrapped as a goose file — into the schema they collectively produce.
func foldOf(t *testing.T, init string, ups ...string) *schema.Schema {
	t.Helper()
	contents := []string{init}
	for _, up := range ups {
		contents = append(contents, "-- +goose Up\n"+up)
	}
	m, err := migrate.FoldContent(contents)
	require.NoError(t, err)
	return m
}

// applied returns the state reached by applying src's init migration, as the
// folder reads it back — i.e. the prior state a second run of the generator sees.
func applied(t *testing.T, src string) *schema.Schema {
	t.Helper()
	return foldOf(t, migrate.Init(mustSchema(t, src)))
}

// delta diffs a folded prior state against an SDL source's desired schema.
func delta(t *testing.T, prior *schema.Schema, src string) (up, down string, changed bool) {
	t.Helper()
	return migrate.Delta(prior, mustSchema(t, src))
}

const m7BaseSDL = `type Person @node(label: "person") {
  id: ID!
  tenant: String!
  email: String!
  age: Int
}`

// --- Task 3.1's other half: the fold reconstructs what the generator emits. ---

// TestFoldReconstructsConstraints is the hand-off task 3.1 had to close. Before
// it, a CHECK body and a natural key's multi-column UNIQUE had nowhere to live in
// the schema model, so the folder read them and dropped them — which would have
// become live the moment the generator started emitting them: the differ would
// re-propose a constraint the database already had, and PostgreSQL would refuse
// the duplicate name.
//
// It asserts the reconstruction directly, so the *reason* the round trip below is
// a no-op is visible, not merely its effect.
func TestFoldReconstructsConstraints(t *testing.T) {
	const src = `type Person @node(label: "person")
    @key(fields: ["tenant", "email"])
    @check(expr: "age < 200")
    @check(expr: "char_length(tenant) > 0") {
  id: ID!
  tenant: String!
  email: String! @unique
  age: Int @default(value: "0") @check(expr: "age >= 0")
}`
	folded := applied(t, src)
	require.Len(t, folded.VertexTables, 1)
	vt := folded.VertexTables[0]

	require.NotNil(t, vt.NaturalKey, "a natural key emitted in CREATE TABLE must fold back")
	assert.Equal(t, "persons_key", vt.NaturalKey.Name)
	assert.Equal(t, []string{"tenant", "email"}, vt.NaturalKey.Columns)

	assert.Equal(t, []string{"age < 200", "char_length(tenant) > 0"}, vt.Checks,
		"table-level checks fold back in the order their names number them")

	byName := map[string]schema.Column{}
	for _, c := range vt.Columns {
		byName[c.Name] = c
	}
	assert.Equal(t, "age >= 0", byName["age"].Check, "a column check folds back onto its column")
	assert.Equal(t, "0", byName["age"].Default)
	assert.True(t, byName["email"].Unique, "@unique still folds back onto the column, as before")

	// And therefore a second run has nothing to say.
	_, _, changed := delta(t, folded, src)
	assert.False(t, changed, "emit → fold → diff must be a no-op for an unchanged schema")
}

// TestRoundTripIsANoOpPerDirective takes each new surface on its own, so a
// regression names which one broke rather than only that something did.
func TestRoundTripIsANoOpPerDirective(t *testing.T) {
	for _, tc := range []struct{ name, src string }{
		{"default", `type Person @node(label: "person") {
  id: ID!
  age: Int @default(value: "0")
  note: String @default(value: "'n/a'")
}`},
		{"column check", `type Person @node(label: "person") {
  id: ID!
  age: Int @check(expr: "age >= 0")
}`},
		{"table check", `type Person @node(label: "person") @check(expr: "age < 200") @check(expr: "age > -1") {
  id: ID!
  age: Int
}`},
		{"natural key", `type Person @node(label: "person") @key(fields: ["tenant", "email"]) {
  id: ID!
  tenant: String!
  email: String!
}`},
		{"single-column natural key", `type Person @node(label: "person") @key(fields: ["email"]) {
  id: ID!
  email: String!
}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, changed := delta(t, applied(t, tc.src), tc.src)
			assert.False(t, changed, "a schema diffed against its own applied state needs no migration")
		})
	}
}

// TestFoldAcrossADeltaIsANoOp folds the *delta* as well as the init migration:
// the constraints a delta adds have to be read back exactly as the ones a CREATE
// TABLE writes, or the third run would emit them again.
func TestFoldAcrossADeltaIsANoOp(t *testing.T) {
	const withConstraints = `type Person @node(label: "person") @key(fields: ["tenant", "email"]) @check(expr: "age < 200") {
  id: ID!
  tenant: String!
  email: String!
  age: Int @default(value: "0") @check(expr: "age >= 0")
}`
	desired := mustSchema(t, withConstraints)
	up, _, changed := migrate.Delta(applied(t, m7BaseSDL), desired)
	require.True(t, changed)

	after := foldOf(t, migrate.Init(mustSchema(t, m7BaseSDL)), up)
	_, _, changed = migrate.Delta(after, desired)
	assert.False(t, changed, "the delta's own statements must fold back into the state they produce")
}

// --- Task 4.1: defaults ---

func TestDefaultAddedChangedRemoved(t *testing.T) {
	const noDefault = m7BaseSDL
	const withDefault = `type Person @node(label: "person") {
  id: ID!
  tenant: String!
  email: String!
  age: Int @default(value: "0")
}`
	const otherDefault = `type Person @node(label: "person") {
  id: ID!
  tenant: String!
  email: String!
  age: Int @default(value: "18")
}`

	t.Run("added", func(t *testing.T) {
		up, down, changed := delta(t, applied(t, noDefault), withDefault)
		require.True(t, changed)
		assert.Contains(t, up, `ALTER TABLE persons ALTER COLUMN age SET DEFAULT 0;`)
		assert.Contains(t, down, `ALTER TABLE persons ALTER COLUMN age DROP DEFAULT;`)
		assertNoColumnChurn(t, up)
		assertNoColumnChurn(t, down)
	})

	t.Run("changed", func(t *testing.T) {
		up, down, changed := delta(t, applied(t, withDefault), otherDefault)
		require.True(t, changed)
		assert.Contains(t, up, `ALTER TABLE persons ALTER COLUMN age SET DEFAULT 18;`)
		assert.Contains(t, down, `ALTER TABLE persons ALTER COLUMN age SET DEFAULT 0;`)
		assertNoColumnChurn(t, up)
	})

	t.Run("removed", func(t *testing.T) {
		up, down, changed := delta(t, applied(t, withDefault), noDefault)
		require.True(t, changed)
		assert.Contains(t, up, `ALTER TABLE persons ALTER COLUMN age DROP DEFAULT;`)
		assert.Contains(t, down, `ALTER TABLE persons ALTER COLUMN age SET DEFAULT 0;`)
		assertNoColumnChurn(t, up)
	})
}

// assertNoColumnChurn is the load-bearing half of task 4.1: whatever the delta
// does about a default or a constraint, it must not touch the columns themselves,
// which would throw away every row's value to change what happens to future rows.
// The statement is deliberately absolute — no column is dropped or added at all,
// not merely "not the one this test changed".
func assertNoColumnChurn(t *testing.T, body string) {
	t.Helper()
	assert.NotContains(t, body, "DROP COLUMN",
		"a default or constraint change must never drop a column")
	assert.NotContains(t, body, "ADD COLUMN",
		"a default or constraint change must never re-add a column")
}

// --- Task 4.2: checks ---

func TestCheckAddedAndRemoved(t *testing.T) {
	const withChecks = `type Person @node(label: "person") @check(expr: "age < 200") {
  id: ID!
  tenant: String!
  email: String!
  age: Int @check(expr: "age >= 0")
}`

	t.Run("added", func(t *testing.T) {
		up, down, changed := delta(t, applied(t, m7BaseSDL), withChecks)
		require.True(t, changed)
		assert.Contains(t, up, `ALTER TABLE persons ADD CONSTRAINT persons_age_check CHECK (age >= 0);`)
		assert.Contains(t, up, `ALTER TABLE persons ADD CONSTRAINT persons_check_1 CHECK (age < 200);`)
		assert.Contains(t, down, `ALTER TABLE persons DROP CONSTRAINT persons_age_check;`)
		assert.Contains(t, down, `ALTER TABLE persons DROP CONSTRAINT persons_check_1;`)
		assertNoColumnChurn(t, up)
	})

	t.Run("removed", func(t *testing.T) {
		up, down, changed := delta(t, applied(t, withChecks), m7BaseSDL)
		require.True(t, changed)
		assert.Contains(t, up, `ALTER TABLE persons DROP CONSTRAINT persons_age_check;`)
		assert.Contains(t, up, `ALTER TABLE persons DROP CONSTRAINT persons_check_1;`)
		assert.Contains(t, down, `ALTER TABLE persons ADD CONSTRAINT persons_age_check CHECK (age >= 0);`)
		assert.Contains(t, down, `ALTER TABLE persons ADD CONSTRAINT persons_check_1 CHECK (age < 200);`)
	})

	t.Run("rewritten", func(t *testing.T) {
		const rewritten = `type Person @node(label: "person") @check(expr: "age < 200") {
  id: ID!
  tenant: String!
  email: String!
  age: Int @check(expr: "age >= 18")
}`
		up, _, changed := delta(t, applied(t, withChecks), rewritten)
		require.True(t, changed)
		// PostgreSQL has no ALTER for a CHECK body, so a rewrite is a drop and an
		// add — by the same name, and in that order.
		dropAt := strings.Index(up, `DROP CONSTRAINT persons_age_check;`)
		addAt := strings.Index(up, `ADD CONSTRAINT persons_age_check CHECK (age >= 18);`)
		require.NotEqual(t, -1, dropAt)
		require.NotEqual(t, -1, addAt)
		assert.Less(t, dropAt, addAt, "the old constraint is dropped before the new one is added")
	})
}

// --- Task 4.3: natural keys ---

func TestNaturalKeyAddedChangedRemoved(t *testing.T) {
	const keyed = `type Person @node(label: "person") @key(fields: ["tenant", "email"]) {
  id: ID!
  tenant: String!
  email: String!
  age: Int
}`
	const reKeyed = `type Person @node(label: "person") @key(fields: ["email"]) {
  id: ID!
  tenant: String!
  email: String!
  age: Int
}`

	t.Run("added", func(t *testing.T) {
		up, down, changed := delta(t, applied(t, m7BaseSDL), keyed)
		require.True(t, changed)
		assert.Contains(t, up, `ALTER TABLE persons ADD CONSTRAINT persons_key UNIQUE (tenant, email);`)
		assert.Contains(t, down, `ALTER TABLE persons DROP CONSTRAINT persons_key;`)
		// The property graph is metadata and is always recreated, so the key
		// clause arrives with it.
		assert.Contains(t, up, `persons KEY (tenant, email) LABEL person`)
		assert.NotContains(t, down, `KEY (`)
	})

	t.Run("changed", func(t *testing.T) {
		up, down, changed := delta(t, applied(t, keyed), reKeyed)
		require.True(t, changed)
		dropAt := strings.Index(up, `DROP CONSTRAINT persons_key;`)
		addAt := strings.Index(up, `ADD CONSTRAINT persons_key UNIQUE (email);`)
		require.NotEqual(t, -1, dropAt)
		require.NotEqual(t, -1, addAt)
		assert.Less(t, dropAt, addAt, "the key is dropped and re-added under the same name")
		assert.Contains(t, up, `persons KEY (email) LABEL person`)
		assert.Contains(t, down, `ADD CONSTRAINT persons_key UNIQUE (tenant, email);`)
		assert.Contains(t, down, `persons KEY (tenant, email) LABEL person`)
	})

	t.Run("removed", func(t *testing.T) {
		up, down, changed := delta(t, applied(t, keyed), m7BaseSDL)
		require.True(t, changed)
		assert.Contains(t, up, `ALTER TABLE persons DROP CONSTRAINT persons_key;`)
		assert.NotContains(t, up, `persons KEY (`)
		assert.Contains(t, down, `ALTER TABLE persons ADD CONSTRAINT persons_key UNIQUE (tenant, email);`)
	})
}

// --- Task 4.4 / 4.5: renames ---

func TestRenameColumnIsHintDriven(t *testing.T) {
	const renamed = `type Person @node(label: "person") {
  id: ID!
  tenant: String!
  contact: String! @renamedFrom(name: "email")
  age: Int
}`
	up, down, changed := delta(t, applied(t, m7BaseSDL), renamed)
	require.True(t, changed)

	assert.Contains(t, up, `ALTER TABLE persons RENAME COLUMN email TO contact;`)
	// The whole point: the drop+add pair the differ would otherwise emit is gone,
	// so the column's rows travel with it.
	assert.NotContains(t, up, `DROP COLUMN email`)
	assert.NotContains(t, up, `ADD COLUMN contact`)
	assert.Contains(t, down, `ALTER TABLE persons RENAME COLUMN contact TO email;`)
	assert.NotContains(t, down, `DROP COLUMN contact`)
}

func TestRenameTableIsHintDriven(t *testing.T) {
	const renamed = `type Human @node(label: "human") @renamedFrom(name: "Person") {
  id: ID!
  tenant: String!
  email: String!
  age: Int
}`
	up, down, changed := delta(t, applied(t, m7BaseSDL), renamed)
	require.True(t, changed)

	assert.Contains(t, up, `ALTER TABLE persons RENAME TO humans;`)
	assert.NotContains(t, up, `DROP TABLE IF EXISTS persons;`)
	assert.NotContains(t, up, `CREATE TABLE humans`)
	assert.Contains(t, down, `ALTER TABLE humans RENAME TO persons;`)
}

// TestRenameWithoutAHintInfersNothing is design D2's other half. The differ
// cannot tell a rename from a drop-and-add, so without a declaration it does not
// try: guessing wrong loses data one way or the other.
func TestRenameWithoutAHintInfersNothing(t *testing.T) {
	const renamedSilently = `type Person @node(label: "person") {
  id: ID!
  tenant: String!
  contact: String!
  age: Int
}`
	up, _, changed := delta(t, applied(t, m7BaseSDL), renamedSilently)
	require.True(t, changed)
	assert.Contains(t, up, `ALTER TABLE persons DROP COLUMN email;`)
	assert.Contains(t, up, `ALTER TABLE persons ADD COLUMN contact text NOT NULL;`)
	assert.NotContains(t, up, "RENAME")
}

// TestHintWithNoPriorMatchEmitsNothing is task 4.5, and it is the case a later
// refactor is most likely to break: the hint stays in the SDL forever, so every
// run after the rename has landed re-reads a hint that now matches nothing. It
// has to be a no-op, or the schema would stop generating the moment its own
// migration was applied.
func TestHintWithNoPriorMatchEmitsNothing(t *testing.T) {
	const stillHinted = `type Person @node(label: "person") @renamedFrom(name: "Ghost") {
  id: ID!
  tenant: String! @renamedFrom(name: "ghostColumn")
  email: String!
  age: Int
}`
	up, down, changed := delta(t, applied(t, m7BaseSDL), stillHinted)
	assert.False(t, changed, "a hint naming nothing in the prior state is not a change")
	assert.Empty(t, up)
	assert.Empty(t, down)
}

// TestRenameSurvivesItsOwnMigration is the scenario the hint has to keep working
// through: apply the rename, fold it back, and re-diff the *same* SDL. The hint
// now matches nothing, the folded state already has the new names, and the second
// run must be silent — in particular it must not drop the renamed column.
func TestRenameSurvivesItsOwnMigration(t *testing.T) {
	const renamed = `type Human @node(label: "human") @renamedFrom(name: "Person") {
  id: ID!
  tenant: String!
  contact: String! @renamedFrom(name: "email")
  age: Int
}`
	up, _, changed := delta(t, applied(t, m7BaseSDL), renamed)
	require.True(t, changed)

	after := foldOf(t, migrate.Init(mustSchema(t, m7BaseSDL)), up)
	require.Len(t, after.VertexTables, 1)
	assert.Equal(t, "humans", after.VertexTables[0].Name)

	_, _, changed = delta(t, after, renamed)
	assert.False(t, changed, "re-generating after the rename has landed is a no-op")
}

// TestRenameReissuesConstraintsUnderTheirNewNames covers the trap in combining
// the two halves of this milestone. PostgreSQL renames no constraint when a table
// or column is renamed, but every name gopgql derives moves with the object — so
// the delta has to drop each constraint by the name the database really has and
// add it back under the name the model now expects. Get this wrong and a stale
// constraint is left behind for a later delta to collide with.
func TestRenameReissuesConstraintsUnderTheirNewNames(t *testing.T) {
	const before = `type Person @node(label: "person") @key(fields: ["tenant"]) @check(expr: "age < 200") {
  id: ID!
  tenant: String!
  email: String! @unique
  age: Int @check(expr: "age >= 0")
}`
	const after = `type Human @node(label: "human") @renamedFrom(name: "Person") @key(fields: ["tenant"]) @check(expr: "age < 200") {
  id: ID!
  tenant: String!
  email: String! @unique
  age: Int @check(expr: "age >= 0")
}`
	prior := applied(t, before)
	up, _, changed := migrate.Delta(prior, mustSchema(t, after))
	require.True(t, changed)

	for _, stale := range []string{"persons_key", "persons_email_key", "persons_age_check", "persons_check_1"} {
		assert.Contains(t, up, `ALTER TABLE humans DROP CONSTRAINT `+stale+`;`,
			"the constraint the database still calls %q must be dropped by that name", stale)
	}
	assert.Contains(t, up, `ALTER TABLE humans ADD CONSTRAINT humans_key UNIQUE (tenant);`)
	assert.Contains(t, up, `ALTER TABLE humans ADD CONSTRAINT humans_email_key UNIQUE (email);`)
	assert.Contains(t, up, `ALTER TABLE humans ADD CONSTRAINT humans_age_check CHECK (age >= 0);`)
	assert.Contains(t, up, `ALTER TABLE humans ADD CONSTRAINT humans_check_1 CHECK (age < 200);`)

	// The renames still come first, so every drop above names a table that exists.
	renameAt := strings.Index(up, "RENAME TO humans;")
	require.NotEqual(t, -1, renameAt)
	assert.Less(t, renameAt, strings.Index(up, "DROP CONSTRAINT persons_key;"))

	// And it all folds back to exactly the desired state.
	folded := foldOf(t, migrate.Init(mustSchema(t, before)), up)
	_, _, changed = delta(t, folded, after)
	assert.False(t, changed, "a rename plus its constraint re-issue must fold back to the desired schema")
}

// --- Task 4.6: Down is the exact inverse ---

// TestDownIsTheExactInverse applies the Up of a broad M7 delta to the prior state
// (by folding it), then applies that delta's Down on top, and asserts the result
// is the prior state again. It is the strongest statement of 4.6 available
// without a database: every statement Up emits, Down undoes.
func TestDownIsTheExactInverse(t *testing.T) {
	const after = `type Human @node(label: "human") @renamedFrom(name: "Person")
    @key(fields: ["tenant", "contact"])
    @check(expr: "age < 150") {
  id: ID!
  tenant: String!
  contact: String! @renamedFrom(name: "email") @unique
  age: Int @default(value: "18") @check(expr: "age >= 18")
}`
	prior := applied(t, m7BaseSDL)
	up, down, changed := migrate.Delta(prior, mustSchema(t, after))
	require.True(t, changed)

	init := migrate.Init(mustSchema(t, m7BaseSDL))
	roundTripped := foldOf(t, init, up, down)

	require.Equal(t, canonicalize(prior), canonicalize(roundTripped),
		"applying Up then Down must land back on the schema the migration started from")
}
