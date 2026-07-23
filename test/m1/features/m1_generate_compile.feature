Feature: M1 generates a migration and compiles single-vertex queries
  From one annotated SDL document, gopgql generates the initial goose
  migration (0001_init.sql), applies it to a real postgres:19beta2 via goose,
  seeds rows, compiles GraphQL queries to GRAPH_TABLE, and asserts on the
  returned data — the nested JSON response, never on SQL text.

  Background:
    Given the SDL:
      """
      type Person @node(label: "person") {
        id: ID!
        name: String!
        email: String
        follows: [Person!]! @relationship(type: "follows", direction: OUT)
        followedBy: [Person!]! @relationship(type: "follows", direction: IN)
                               @hasInverse(field: "follows")
      }
      """
    And I generate and apply the initial migration via goose

  Scenario: Compile and execute { persons { name } }
    Given the following persons exist:
      | name  | email             |
      | Alice | alice@example.com |
      | Bob   | bob@example.com   |
    When I compile and execute "{ persons { name } }"
    Then the JSON response is:
      """
      {"persons":[{"name":"Alice"},{"name":"Bob"}]}
      """

  Scenario: Projection with multiple fields and an alias
    Given the following persons exist:
      | name  | email             |
      | Alice | alice@example.com |
    When I compile and execute "{ persons { fullName: name email } }"
    Then the JSON response is:
      """
      {"persons":[{"fullName":"Alice","email":"alice@example.com"}]}
      """

  Scenario: Rows from a prior scenario do not leak
    When I compile and execute "{ persons { name } }"
    Then the JSON response is:
      """
      {"persons":[]}
      """

  Scenario: Bind parameters work inside GRAPH_TABLE (SPEC.md §6.3 spike)
    Given the following persons exist:
      | name  |
      | Alice |
      | Bob   |
    When I filter persons by name "Alice" using a bind parameter inside GRAPH_TABLE
    Then the returned names are:
      | name  |
      | Alice |
