package parity_test

// The catalogue: every distinct query the M1–M7 suites execute, plus the
// divergence cases design D5 and D6 predict, each with the SDL and seed it needs.
//
// It is the milestone's acceptance criterion made concrete (design D6). Restating
// "every prior milestone's query scenarios" in prose would be wrong within one
// milestone, so the list is data, and coverage_test.go fails naming any query a
// feature file executes that is missing from here.
//
// A world is an (SDL, seed) pair. Two milestone scenarios share a world only when
// they genuinely share both — M1 seeds two people for one scenario and one for
// the next, and collapsing those would quietly change what is being compared.

// world is one database state: a schema and the rows in it.
type world struct {
	// name identifies the world, and names the database it is built in.
	name string
	// sdl is the schema, applied through the real generate-and-goose pipeline.
	sdl string
	// seed are statements run after the migration, in order. They are written
	// out in full rather than parameterised: they stand in for writes gopgql
	// never sees, which is what the milestone suites do too.
	seed []string
}

// scenario is one catalogue entry: a query to run under both strategies.
type scenario struct {
	// name identifies the entry in test output.
	name string
	// world is the database state the query runs against.
	world *world
	// query is the GraphQL operation, byte-for-byte as the owning suite runs it
	// — coverage_test.go matches on this string.
	query string
	// vars are the query variables, or nil.
	vars map[string]any
	// want is the response the owning milestone suite asserts, as JSON. Parity
	// between two strategies would be satisfied by both being wrong in the same
	// way, so each response is also checked against what the milestone already
	// claims (task 6.3). Empty for a divergence case, which no milestone owns.
	want string
	// milestone is the suite this entry came from, or "M8" for a divergence
	// case the design predicts rather than a milestone that already ran it.
	milestone string
}

// --- schemas ---

// personSDL is the worked example from SPEC.md §5.2, and the schema M1, M2, M3
// and M5 all run against.
const personSDL = `type Person @node(label: "person") {
  id: ID!
  name: String!
  email: String
  follows: [Person!]! @relationship(type: "follows", direction: OUT)
  followedBy: [Person!]! @relationship(type: "follows", direction: IN)
                         @hasInverse(field: "follows")
}`

// personAgeSDL is personSDL widened with the nullable age M2's delta adds.
const personAgeSDL = `type Person @node(label: "person") {
  id: ID!
  name: String!
  email: String
  age: Int
  follows: [Person!]! @relationship(type: "follows", direction: OUT)
  followedBy: [Person!]! @relationship(type: "follows", direction: IN)
                         @hasInverse(field: "follows")
}`

// interfaceSDL is M4's: two vertex tables under one interface twice over, once
// through a shared label and once by label alternation.
const interfaceSDL = `interface Actor @node(label: "actor") {
  id: ID!
  name: String!
  follows: [Person!]! @relationship(type: "follows", direction: OUT)
}

interface Profile {
  id: ID!
  name: String!
}

type Person implements Actor & Profile @node(label: "person") {
  id: ID!
  name: String!
  email: String
  follows: [Person!]! @relationship(type: "follows", direction: OUT)
  followedBy: [Person!]! @relationship(type: "follows", direction: IN)
                         @hasInverse(field: "follows")
}

type Bot implements Actor & Profile @node(label: "bot") {
  id: ID!
  name: String!
  vendor: String
  follows: [Person!]! @relationship(type: "follows", direction: OUT, table: "bot_follows")
}`

// productSDL is M6's: a renamed column and a numeric(10,2) override.
const productSDL = `type Product @node(label: "product") {
  id: ID!
  sku: String! @unique
  title: String! @column(name: "name")
  price: Float! @column(type: "numeric(10,2)")
  category: String! @index(name: "products_category_idx", using: "btree")
}`

// constraintsSDL is M7's: a natural key alongside the surrogate id.
const constraintsSDL = `type Person @node(label: "person")
    @key(fields: ["tenant", "email"])
    @check(expr: "nickname IS NULL OR nickname <> email") {
  id: ID!
  tenant: String!
  email: String!
  name: String!
  age: Int @default(value: "0") @check(expr: "age >= 0")
  nickname: String @default(value: "'unknown'")
  follows: [Person!]! @relationship(type: "follows", direction: OUT)
  followedBy: [Person!]! @relationship(type: "follows", direction: IN)
                         @hasInverse(field: "follows")
}`

// scalarsSDL reaches every scalar the contract (design D5) has a canonical form
// for, so the divergence cases have somewhere to live: a numeric whose trailing
// zeros the database keeps, a timestamptz PostgreSQL renders with an offset and
// Go renders with a Z, a nullable column, and a relationship to fan out over.
const scalarsSDL = `type Reading @node(label: "reading") {
  id: ID!
  label: String!
  exact: Float! @column(type: "numeric(10,2)")
  approx: Float!
  count: Int!
  ok: Boolean!
  note: String
  at: DateTime!
  samples: [Reading!]! @relationship(type: "sampled", direction: OUT)
  sampledBy: [Reading!]! @relationship(type: "sampled", direction: IN)
                         @hasInverse(field: "samples")
}`

// --- worlds ---

// follow builds the statement that adds one follows edge by person name.
func follow(src, dst string) string {
	return `INSERT INTO follows (source_id, target_id)
	        SELECT a.id, b.id FROM persons a, persons b
	        WHERE a.name = '` + src + `' AND b.name = '` + dst + `'`
}

var (
	// m1TwoPeople is M1's first scenario: two people, no edges.
	m1TwoPeople = &world{
		name: "m1_two_people",
		sdl:  personSDL,
		seed: []string{`INSERT INTO persons (name, email) VALUES
		                ('Alice', 'alice@example.com'), ('Bob', 'bob@example.com')`},
	}

	// m1OnePerson is M1's alias-and-multiple-fields scenario.
	m1OnePerson = &world{
		name: "m1_one_person",
		sdl:  personSDL,
		seed: []string{`INSERT INTO persons (name, email) VALUES ('Alice', 'alice@example.com')`},
	}

	// m1Empty is M1's leak scenario, and doubles as the case the json_agg
	// empty-set trap lives in: a root field matching nothing must encode as
	// [] on both paths, never as null (task 6.6).
	m1Empty = &world{name: "m1_empty", sdl: personSDL}

	// m2Aged is M2's widened schema, after the delta added age.
	m2Aged = &world{
		name: "m2_aged",
		sdl:  personAgeSDL,
		seed: []string{`INSERT INTO persons (name, email, age) VALUES
		                ('Alice', 'alice@example.com', 30), ('Bob', 'bob@example.com', 41)`},
	}

	// m2Narrowed is M2's schema after the delta dropped age again.
	m2Narrowed = &world{
		name: "m2_narrowed",
		sdl:  personSDL,
		seed: []string{`INSERT INTO persons (name, email) VALUES ('Alice', 'alice@example.com')`},
	}

	// m3Graph is M3's follow graph.
	m3Graph = &world{
		name: "m3_graph",
		sdl:  personSDL,
		seed: []string{
			`INSERT INTO persons (name) VALUES ('Alice'), ('Bob'), ('Carol'), ('Dave')`,
			follow("Alice", "Bob"), follow("Alice", "Carol"),
			follow("Bob", "Carol"), follow("Bob", "Dave"),
		},
	}

	// m4Graph is M4's: a bot alongside the people, a cycle, and a self-follow,
	// so an unguarded pattern would return the vertex it started from.
	m4Graph = &world{
		name: "m4_graph",
		sdl:  interfaceSDL,
		seed: []string{
			`INSERT INTO persons (name) VALUES ('Alice'), ('Bob'), ('Carol'), ('Dave'), ('Erin')`,
			`INSERT INTO bots (name, vendor) VALUES ('Botty', 'acme')`,
			follow("Alice", "Bob"), follow("Bob", "Carol"), follow("Carol", "Dave"),
			follow("Dave", "Erin"), follow("Alice", "Alice"), follow("Carol", "Alice"),
			`INSERT INTO bot_follows (source_id, target_id)
			 SELECT b.id, p.id FROM bots b, persons p
			 WHERE b.name = 'Botty' AND p.name IN ('Alice', 'Erin')`,
		},
	}

	// m5Graph is M5's deliberately hostile seed: a self-follow, a three-cycle,
	// and Dave and Erin with followers on one side only — which is what makes
	// the empty branch a real case rather than a hypothetical.
	m5Graph = &world{
		name: "m5_graph",
		sdl:  personSDL,
		seed: []string{
			`INSERT INTO persons (name) VALUES ('Alice'), ('Bob'), ('Carol'), ('Dave'), ('Erin')`,
			follow("Alice", "Bob"), follow("Bob", "Carol"), follow("Carol", "Alice"),
			follow("Alice", "Alice"), follow("Dave", "Erin"),
		},
	}

	// m6Products is M6's, including the 89.00 whose trailing zero is the whole
	// point of asking for numeric rather than double precision.
	m6Products = &world{
		name: "m6_products",
		sdl:  productSDL,
		seed: []string{`INSERT INTO products (sku, name, price, category) VALUES
		                ('SKU-1', 'Chain', 1234.56, 'parts'),
		                ('SKU-2', 'Bottle cage', 19.99, 'parts'),
		                ('SKU-3', 'Helmet', 89.00, 'safety')`},
	}

	// m7Tenants is M7's: two rows share a tenant and two share an email, so
	// neither column alone selects a unique row.
	m7Tenants = &world{
		name: "m7_tenants",
		sdl:  constraintsSDL,
		seed: []string{
			`INSERT INTO persons (tenant, email, name) VALUES
			 ('acme',   'alice@example.com', 'Alice'),
			 ('acme',   'bob@example.com',   'Bob'),
			 ('globex', 'alice@example.com', 'Alicia')`,
			`INSERT INTO follows (source_id, target_id)
			 SELECT a.id, b.id FROM persons a, persons b
			 WHERE a.name = 'Alice' AND b.name = 'Bob'`,
		},
	}

	// tzWorld is scalarsWorld under another name.
	//
	// A world names the database it is built in, and TestParity has already
	// built scalarsWorld by the time the time-zone test runs, so reusing it
	// would try to CREATE DATABASE over one that exists. The seed is shared;
	// only the name differs.
	tzWorld = &world{name: "m8_timezone", sdl: scalarsSDL, seed: scalarsSeed}

	// scalarsWorld carries the leaves the two serialisers disagree about.
	//
	// `exact` keeps a trailing zero PostgreSQL preserves and Go's float64 would
	// drop; `at` is a timestamptz PostgreSQL renders as +00:00 and Go as Z, and
	// one row sits in a non-UTC offset so a session TimeZone leaking into the
	// response would show; `note` is NULL on one row, which must encode as null
	// rather than vanish. The fan-out is 12 children, enough that two orders
	// would differ.
	scalarsWorld = &world{
		name: "m8_scalars",
		sdl:  scalarsSDL,
		seed: scalarsSeed,
	}
)

// scalarsSeed is shared by scalarsWorld and tzWorld.
var scalarsSeed = []string{
	`INSERT INTO readings (label, exact, approx, count, ok, note, at) VALUES
			 ('root',  89.00,   0.1,          7, true,  NULL,     '2026-07-30T12:00:00+00:00'),
			 ('tilted', 19.90,  1234.5678,   -3, false, 'kept',   '2026-07-30T14:30:00+02:00'),
			 ('tiny',   0.10,   1e-7,         0, true,  '',       '2026-01-01T00:00:00Z')`,
	// A twelve-child fan-out off `root`, named so that insertion order,
	// alphabetical order and id order are all different.
	`INSERT INTO readings (label, exact, approx, count, ok, at)
			 SELECT 'child-' || n, n + 0.50, n / 3.0, n, n % 2 = 0,
			        '2026-07-30T12:00:00Z'::timestamptz + (n || ' minutes')::interval
			 FROM generate_series(1, 12) AS n`,
	`INSERT INTO sampled (source_id, target_id)
			 SELECT r.id, c.id FROM readings r, readings c
			 WHERE r.label = 'root' AND c.label LIKE 'child-%'`,
}

// catalogue is every entry the parity suite runs. Each is executed twice — once
// under each strategy — against the same database state.
var catalogue = []scenario{
	// --- M1 ---
	{
		name: "m1/single vertex", world: m1TwoPeople, milestone: "M1",
		query: `{ persons { name } }`,
		want:  `{"persons":[{"name":"Alice"},{"name":"Bob"}]}`,
	},
	{
		name: "m1/alias and multiple fields", world: m1OnePerson, milestone: "M1",
		query: `{ persons { fullName: name email } }`,
		want:  `{"persons":[{"fullName":"Alice","email":"alice@example.com"}]}`,
	},
	{
		name: "m1/matching nothing is [] and never null", world: m1Empty, milestone: "M1",
		query: `{ persons { name } }`,
		want:  `{"persons":[]}`,
	},

	// --- M2 ---
	{
		name: "m2/widened schema", world: m2Aged, milestone: "M2",
		query: `{ persons { name email age } }`,
		want: `{"persons":[{"name":"Alice","email":"alice@example.com","age":30},
		                   {"name":"Bob","email":"bob@example.com","age":41}]}`,
	},
	{
		name: "m2/narrowed schema", world: m2Narrowed, milestone: "M2",
		query: `{ persons { name email } }`,
		want:  `{"persons":[{"name":"Alice","email":"alice@example.com"}]}`,
	},

	// --- M3 ---
	{
		name: "m3/one hop by variable", world: m3Graph, milestone: "M3",
		query: `{ persons(name: $n) { follows { name } } }`, vars: map[string]any{"n": "Alice"},
		want: `{"persons":[{"follows":[{"name":"Bob"},{"name":"Carol"}]}]}`,
	},
	{
		name: "m3/a different variable value", world: m3Graph, milestone: "M3",
		query: `{ persons(name: $n) { follows { name } } }`, vars: map[string]any{"n": "Bob"},
		want: `{"persons":[{"follows":[{"name":"Carol"},{"name":"Dave"}]}]}`,
	},
	{
		name: "m3/deduplicated parent with its own field", world: m3Graph, milestone: "M3",
		query: `{ persons(name: $n) { name follows { name } } }`, vars: map[string]any{"n": "Alice"},
		want: `{"persons":[{"name":"Alice","follows":[{"name":"Bob"},{"name":"Carol"}]}]}`,
	},
	{
		name: "m3/IN traverses the reversed edge", world: m3Graph, milestone: "M3",
		query: `{ persons(name: $n) { followedBy { name } } }`, vars: map[string]any{"n": "Carol"},
		want: `{"persons":[{"followedBy":[{"name":"Alice"},{"name":"Bob"}]}]}`,
	},
	{
		name: "m3/many-parent many-child fan-out", world: m3Graph, milestone: "M3",
		query: `{ persons { name follows { name } } }`,
		want: `{"persons":[{"name":"Alice","follows":[{"name":"Bob"},{"name":"Carol"}]},
		                   {"name":"Bob","follows":[{"name":"Carol"},{"name":"Dave"}]}]}`,
	},

	// --- M4 ---
	{
		name: "m4/three-hop chain", world: m4Graph, milestone: "M4",
		query: `{ persons(name: $n) { name follows { name follows { name follows { name } } } } }`,
		vars:  map[string]any{"n": "Alice"},
		want: `{"persons":[{"name":"Alice","follows":[
		         {"name":"Bob","follows":[{"name":"Carol","follows":[{"name":"Dave"}]}]}]}]}`,
	},
	{
		name: "m4/self-follow excluded", world: m4Graph, milestone: "M4",
		query: `{ persons(name: $n) { follows { name } } }`, vars: map[string]any{"n": "Alice"},
		want: `{"persons":[{"follows":[{"name":"Bob"}]}]}`,
	},
	{
		name: "m4/never walks back", world: m4Graph, milestone: "M4",
		query: `{ persons(name: $n) { follows { follows { name } } } }`, vars: map[string]any{"n": "Alice"},
		want: `{"persons":[{"follows":[{"follows":[{"name":"Carol"}]}]}]}`,
	},
	{
		name: "m4/interface traversal", world: m4Graph, milestone: "M4",
		query: `{ actors { name follows { name } } }`,
		want: `{"actors":[{"name":"Alice","follows":[{"name":"Bob"}]},
		                  {"name":"Bob","follows":[{"name":"Carol"}]},
		                  {"name":"Botty","follows":[{"name":"Alice"},{"name":"Erin"}]},
		                  {"name":"Carol","follows":[{"name":"Alice"},{"name":"Dave"}]},
		                  {"name":"Dave","follows":[{"name":"Erin"}]}]}`,
	},
	{
		name: "m4/shared label spans both tables", world: m4Graph, milestone: "M4",
		query: `{ actors { name } }`,
		want: `{"actors":[{"name":"Alice"},{"name":"Bob"},{"name":"Carol"},
		                  {"name":"Dave"},{"name":"Erin"},{"name":"Botty"}]}`,
	},
	{
		name: "m4/label alternation spans both tables", world: m4Graph, milestone: "M4",
		query: `{ profiles { name } }`,
		want: `{"profiles":[{"name":"Alice"},{"name":"Bob"},{"name":"Carol"},
		                    {"name":"Dave"},{"name":"Erin"},{"name":"Botty"}]}`,
	},

	// --- M5 ---
	{
		name: "m5/two branches at the root", world: m5Graph, milestone: "M5",
		query: `{ persons(name: $n) { name follows { name } followedBy { name } } }`,
		vars:  map[string]any{"n": "Alice"},
		want: `{"persons":[{"name":"Alice","follows":[{"name":"Bob"}],
		                    "followedBy":[{"name":"Carol"}]}]}`,
	},
	{
		name: "m5/one branch empty keeps the other", world: m5Graph, milestone: "M5",
		query: `{ persons(name: $n) { name follows { name } followedBy { name } } }`,
		vars:  map[string]any{"n": "Dave"},
		want:  `{"persons":[{"name":"Dave","follows":[{"name":"Erin"}],"followedBy":[]}]}`,
	},
	{
		name: "m5/the other branch empty", world: m5Graph, milestone: "M5",
		query: `{ persons(name: $n) { name follows { name } followedBy { name } } }`,
		vars:  map[string]any{"n": "Erin"},
		want:  `{"persons":[{"name":"Erin","follows":[],"followedBy":[{"name":"Dave"}]}]}`,
	},
	{
		name: "m5/every person with both branches", world: m5Graph, milestone: "M5",
		query: `{ persons { name follows { name } followedBy { name } } }`,
		want: `{"persons":[
		         {"name":"Alice","follows":[{"name":"Bob"}],"followedBy":[{"name":"Carol"}]},
		         {"name":"Bob","follows":[{"name":"Carol"}],"followedBy":[{"name":"Alice"}]},
		         {"name":"Carol","follows":[{"name":"Alice"}],"followedBy":[{"name":"Bob"}]},
		         {"name":"Dave","follows":[{"name":"Erin"}],"followedBy":[]},
		         {"name":"Erin","follows":[],"followedBy":[{"name":"Dave"}]}]}`,
	},
	{
		name: "m5/branching below the root", world: m5Graph, milestone: "M5",
		query: `{ persons(name: $n) { name follows { name follows { name } followedBy { name } } } }`,
		vars:  map[string]any{"n": "Alice"},
		want: `{"persons":[{"name":"Alice","follows":[
		         {"name":"Bob","follows":[{"name":"Carol"}],"followedBy":[]}]}]}`,
	},

	// --- M6 ---
	{
		name: "m6/renamed column and numeric", world: m6Products, milestone: "M6",
		query: `{ products(title: $t) { title price } }`, vars: map[string]any{"t": "Chain"},
		want: `{"products":[{"title":"Chain","price":1234.56}]}`,
	},

	// --- M7 ---
	// M7 has no feature file, so its entries are registered here explicitly and
	// coverage_test.go asserts the count it expects (design D6).
	{
		name: "m7/natural key selects one row", world: m7Tenants, milestone: "M7",
		query: `{ persons(tenant: $tenant, email: $email) { name } }`,
		vars:  map[string]any{"tenant": "acme", "email": "alice@example.com"},
		want:  `{"persons":[{"name":"Alice"}]}`,
	},
	{
		name: "m7/edges traverse from a natural-key vertex", world: m7Tenants, milestone: "M7",
		query: `{ persons(tenant: $tenant, email: $email) { name follows { name } } }`,
		vars:  map[string]any{"tenant": "acme", "email": "alice@example.com"},
		want:  `{"persons":[{"name":"Alice","follows":[{"name":"Bob"}]}]}`,
	},

	// --- M8: the divergences the design predicts (task 6.6) ---
	//
	// These have no `want`: no milestone ran them, and writing one out by hand
	// would only assert that this file and the database agree about uuids and
	// timestamps. What they are for is the strategy-versus-strategy comparison,
	// which is exactly where each of them would break.
	{
		name:  "m8/numeric trailing zeros, DateTime offset, null scalar",
		world: scalarsWorld, milestone: "M8",
		query: `{ readings { label exact approx count ok note at } }`,
	},
	{
		name:  "m8/a twelve-child fan-out orders identically",
		world: scalarsWorld, milestone: "M8",
		query: `{ readings(label: $l) { label samples { label exact at } } }`,
		vars:  map[string]any{"l": "root"},
	},
	{
		name:  "m8/the id itself round-trips as canonical text",
		world: scalarsWorld, milestone: "M8",
		query: `{ readings(label: $l) { id label } }`,
		vars:  map[string]any{"l": "root"},
	},
	{
		// Every row here has an empty branch: `root` has twelve samples and no
		// sampler, each child has one sampler and no samples. A branch that
		// aggregated to null rather than [] could not hide in this one.
		name:  "m8/a branch split where every row has an empty branch",
		world: scalarsWorld, milestone: "M8",
		query: `{ readings { label samples { label } sampledBy { label } } }`,
	},
}
