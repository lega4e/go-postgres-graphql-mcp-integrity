Feature: An MCP server over a schema and a database
  # SPEC.md §7 → MCP. A real SDK client drives a real server over an in-memory
  # transport pair, against a real postgres:19beta2 container. One scenario
  # spawns the built binary over stdio instead, so the wiring in
  # cmd/gopgql-mcp is covered as well as the library.

  Background:
    Given the SDL:
      """
      type Person @node(label: "person") {
        id: ID!
        name: String!
        nickname: String
        follows: [Person!]! @relationship(type: "follows", direction: OUT)
      }

      type Team @node(label: "team") {
        id: ID!
        name: String!
      }
      """
    And I generate and apply the initial migration via goose
    And the following persons exist:
      | name  |
      | Ada   |
      | Linus |
      | Grace |
    And "Ada" follows "Linus"
    And "Ada" follows "Grace"
    And an MCP client connected to the server

  Scenario: The server advertises both tools with input schemas
    Then the tool list is:
      | introspect |
      | query      |
    And every tool declares an input schema
    And no tool applies migrations or alters the schema

  Scenario: An agent learns how to introspect from the tool list alone
    Then the "introspect" tool description mentions "__schema"
    And the "introspect" tool description mentions "__type"
    And the "query" tool description mentions "__typename"
    And the "query" tool description carries an introspection query

  Scenario: The introspection query a real GraphQL client sends
    When I send the introspection query a GraphQL client sends
    Then the introspection result names the query root "Query"
    And the introspection result describes the types "Query, Person, Team, String, __Type"
    And the type reference of "Person.follows" is "NON_NULL, LIST, NON_NULL, Person"
    And no statement reached the database

  Scenario: A type is introspectable by name
    When I call "query" with:
      """
      {
        __type(name: "Person") {
          name
          kind
          fields { name type { kind name ofType { kind name } } }
        }
      }
      """
    Then the result JSON at "__type.name" is "Person"
    And the introspected type lists the fields "id, name, nickname, follows"
    And no statement reached the database

  Scenario: An unknown type introspects to null
    When I call "query" with:
      """
      { __type(name: "Nope") { name } }
      """
    Then the tool call succeeded
    And the result JSON at "__type" is null

  Scenario: __typename resolves on a selected object
    When I call "query" with:
      """
      { persons(name: "Ada") { name __typename } }
      """
    Then the result JSON is:
      """
      { "persons": [ { "name": "Ada", "__typename": "Person" } ] }
      """

  Scenario: The introspect tool defaults to an overview
    When I call "introspect" with no arguments
    Then the result contains "persons"
    And the result contains "Person"
    And the introspected overview omits the types' field definitions
    And no statement reached the database

  Scenario: The introspect tool drills into one type
    When I call "introspect" with type "Person"
    Then the result contains "follows"
    And the result contains "nickname"

  Scenario: The introspect tool returns the full schema on request
    When I call "introspect" asking for the full schema
    Then the result contains "__Schema"
    And the result contains "isDeprecated"

  Scenario: The introspect tool returns the SDL document
    When I call "introspect" asking for SDL
    Then the result contains "@relationship"
    And the result contains "type Person @node"

  Scenario: A scalar query returns shaped rows
    When I call "query" with:
      """
      { persons { name } }
      """
    Then the result JSON is:
      """
      { "persons": [ { "name": "Ada" }, { "name": "Linus" }, { "name": "Grace" } ] }
      """

  Scenario: An id comes back as a readable identifier
    # pgx decodes uuid to a 16-byte array, which would marshal as a JSON array
    # of numbers and could not be fed back into a query.
    When I call "query" with:
      """
      { persons(name: "Ada") { id name } }
      """
    Then the result carries a uuid in text form

  Scenario: A traversal nests without duplicating parents
    When I call "query" with:
      """
      { persons(name: "Ada") { name follows { name } } }
      """
    Then the result JSON is:
      """
      { "persons": [ { "name": "Ada", "follows": [ { "name": "Linus" }, { "name": "Grace" } ] } ] }
      """

  Scenario: A variable is bound, never interpolated
    Given the variables:
      """
      { "n": "Ada" }
      """
    When I call "query" with:
      """
      query ByName($n: String!) { persons(name: $n) { name } }
      """
    Then the result JSON is:
      """
      { "persons": [ { "name": "Ada" } ] }
      """
    And the executed statement carries a placeholder rather than "Ada"
    And the result carries no SQL

  Scenario: A missing variable is an error
    When I call "query" with:
      """
      query ByName($n: String!) { persons(name: $n) { name } }
      """
    Then the tool call failed mentioning "$n"
    And no statement reached the database

  Scenario: An unknown field is refused before the database
    When I call "query" with:
      """
      { persons { nope } }
      """
    Then the tool call failed mentioning "nope"
    And no statement reached the database

  Scenario: A selection past the depth ceiling is refused before the database
    When I call "query" with:
      """
      { persons { follows { follows { follows { follows { name } } } } } }
      """
    Then the tool call failed mentioning "MaxDepth"
    And no statement reached the database

  Scenario: A database error is reported and the server keeps serving
    # The value is bound, reaches the database, and the database rejects it —
    # a failure the compiler cannot catch.
    When I call "query" with:
      """
      { persons(id: "not-an-identifier") { name } }
      """
    Then the tool call failed mentioning "SQLSTATE"
    When I call "query" with:
      """
      { persons(name: "Ada") { name } }
      """
    Then the result JSON is:
      """
      { "persons": [ { "name": "Ada" } ] }
      """

  Scenario: Markdown renders a flat result as a table
    When I call "query" with format "markdown" and:
      """
      { persons(name: "Ada") { name nickname } }
      """
    Then the result is the table:
      """
      | name | nickname |
      | --- | --- |
      | Ada |  |
      """

  Scenario: An empty flat result is a header with no rows
    When I call "query" with format "markdown" and:
      """
      { persons(name: "Nobody") { name } }
      """
    Then the tool call succeeded
    And the result is the table:
      """
      | name |
      | --- |
      """

  Scenario: Markdown is refused for a nested selection, before execution
    When I call "query" with format "markdown" and:
      """
      { persons { name follows { name } } }
      """
    Then the tool call failed mentioning "follows"
    And the tool call failed mentioning "json"
    And no statement reached the database

  Scenario: The same nested selection succeeds in JSON
    When I call "query" with:
      """
      { persons(name: "Ada") { name follows { name } } }
      """
    Then the result JSON is:
      """
      { "persons": [ { "name": "Ada", "follows": [ { "name": "Linus" }, { "name": "Grace" } ] } ] }
      """

  Scenario: The server's connection is read-only
    Then a write on the server's connection is refused by the database

  Scenario: The built binary serves over stdio
    When I connect a client to the built binary over stdio
    Then the tool list is:
      | introspect |
      | query      |
    When I call "query" on the binary with:
      """
      { persons(name: "Ada") { name follows { name } } }
      """
    Then the result JSON is:
      """
      { "persons": [ { "name": "Ada", "follows": [ { "name": "Linus" }, { "name": "Grace" } ] } ] }
      """
