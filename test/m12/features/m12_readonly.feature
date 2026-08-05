Feature: M12 surfaces tables gopgql does not own
  A @readonly type appears in the property graph and in the read model, and
  gopgql emits no DDL and no migration for it — ever (SPEC.md §5, §7 → M12).
  The schema: argument of @node names the PostgreSQL schema it lives in, so a
  table outside search_path can be addressed at all.

  The fixture stands in for the other tool: a hand-written init script creates
  the schema, its tables and its rows before gopgql runs, exactly as `dbos
  migrate` would. Everything after that is gopgql generating over a database it
  did not build.

  Background:
    Given the schema "dbos" already exists with its own tables and rows
    And the SDL:
      """
      type Person @node(label: "person") {
        id: ID!
        name: String!
      }
      type Stream @node(label: "stream", table: "streams", schema: "dbos") @readonly {
        id: ID!
        topic: String!
        seq: Int! @column(name: "offset")
      }
      """

  Scenario: The M12 exit — a generation over an unowned table applies and queries
    When I generate the migrations
    Then no migration contains "CREATE TABLE dbos"
    And no migration contains "ALTER TABLE dbos"
    And no migration contains "DROP TABLE IF EXISTS dbos"
    And no migration contains "CREATE INDEX"
    But the property graph names "dbos.streams"
    When I apply the migrations with goose
    And I compile and execute:
      """
      query { streams { topic seq } }
      """
    Then the JSON response is:
      """
      {"streams":[{"topic":"orders","seq":1},{"topic":"orders","seq":2},{"topic":"audit","seq":7}]}
      """

  Scenario: Item 4 — a column named after a reserved word round-trips
    When I generate the migrations
    And I apply the migrations with goose
    And I compile and execute:
      """
      query { streams(seq: 7) { topic seq } }
      """
    Then the JSON response is:
      """
      {"streams":[{"topic":"audit","seq":7}]}
      """
    And the table "dbos.streams" still has a column named "offset"

  Scenario: The managed half is generated as usual, in the same generation
    When I generate the migrations
    And I apply the migrations with goose
    Then the table "public.persons" exists
    And the property graph exposes the elements "persons, streams"

  Scenario: Regenerating from the same SDL plans nothing
    When I generate the migrations
    And I apply the migrations with goose
    And I generate the migrations again
    Then nothing was generated

  Scenario: Widening an unmanaged type moves the graph and no table
    When I generate the migrations
    And I apply the migrations with goose
    And I generate a delta for the SDL:
      """
      type Person @node(label: "person") {
        id: ID!
        name: String!
      }
      type Stream @node(label: "stream", table: "streams", schema: "dbos") @readonly {
        id: ID!
        topic: String!
        seq: Int! @column(name: "offset")
        payload: JSON!
      }
      """
    Then the delta contains no "ALTER TABLE"
    And the delta rebuilds the property graph
    When I apply the migrations with goose
    And I compile and execute:
      """
      query { streams(seq: 7) { topic payload } }
      """
    Then the JSON response is:
      """
      {"streams":[{"topic":"audit","payload":{"kind":"login"}}]}
      """

  Scenario: Dropping a column from the managed type still emits its ALTER
    When I generate the migrations
    And I apply the migrations with goose
    And I generate a delta for the SDL:
      """
      type Person @node(label: "person") {
        id: ID!
      }
      type Stream @node(label: "stream", table: "streams", schema: "dbos") @readonly {
        id: ID!
        topic: String!
        seq: Int! @column(name: "offset")
      }
      """
    Then the delta contains "ALTER TABLE persons DROP COLUMN name;"
    When I apply the migrations with goose
    Then the table "public.persons" has no column "name"
