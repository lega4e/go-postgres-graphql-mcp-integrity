Feature: M4 compiles multi-hop chains, rejects past the depth limit, and spans interfaces
  Nesting keeps extending one GRAPH_TABLE, so a three-hop query is still a single
  statement. Past MaxDepth the compiler refuses: SQL/PGQ has no variable-length
  paths, so gopgql returns a typed *DepthExceededError at compile time rather
  than truncating the pattern — and because no SQL is produced, nothing reaches
  the database. A GraphQL interface makes several tables one queryable position,
  either through a label its implementors share or by alternation over their own
  labels. PostgreSQL does not enforce edge isomorphism, so every pair of vertex
  positions that could bind the same row is guarded with `<>`.

  Every scenario runs against a real postgres:19beta2 container and asserts on
  the shaped JSON — except the rejection scenarios, which assert that the
  database was never touched at all.

  The seed graph below is deliberately hostile to an unguarded pattern: Alice
  follows herself, and Alice → Bob → Carol → Alice is a cycle. Without the
  isomorphism guards a traversal would return the vertex it started from.

  Background:
    Given the SDL:
      """
      interface Actor @node(label: "actor") {
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
      }
      """
    And I generate and apply the initial migration via goose
    And the following persons exist:
      | name  |
      | Alice |
      | Bob   |
      | Carol |
      | Dave  |
      | Erin  |
    And the following bots exist:
      | name  | vendor |
      | Botty | acme   |
    And "Alice" follows "Bob"
    And "Bob" follows "Carol"
    And "Carol" follows "Dave"
    And "Dave" follows "Erin"
    And "Alice" follows "Alice"
    And "Carol" follows "Alice"
    And "Botty" follows "Alice"
    And "Botty" follows "Erin"

  Scenario: The M4 exit query — a three-hop chain returning correct rows
    When I compile and execute "{ persons(name: $n) { name follows { name follows { name follows { name } } } } }" with variable "n" bound to "Alice"
    Then the compiled query is a single GRAPH_TABLE
    And the compiled query bound the filter as a parameter
    And the JSON response is:
      """
      {"persons":[
        {"name":"Alice","follows":[
          {"name":"Bob","follows":[
            {"name":"Carol","follows":[{"name":"Dave"}]}
          ]}
        ]}
      ]}
      """

  Scenario: A four-hop selection is rejected at compile time and never reaches the database
    When I compile "{ persons(name: $n) { follows { follows { follows { follows { name } } } } } }"
    Then compilation failed with a depth error at max depth 3
    And no query reached the database

  Scenario: The depth ceiling is configurable — three hops is one too many at MaxDepth 2
    When I compile "{ persons { follows { follows { follows { name } } } } }" with a max depth of 2
    Then compilation failed with a depth error at max depth 2
    And no query reached the database

  Scenario: A self-follow is excluded — Alice follows herself, and does not follow herself
    When I compile and execute "{ persons(name: $n) { follows { name } } }" with variable "n" bound to "Alice"
    Then the compiled query guards against self-matches
    And the JSON response is:
      """
      {"persons":[{"follows":[{"name":"Bob"}]}]}
      """

  Scenario: A multi-hop path never walks back to a vertex it has already visited
    When I compile and execute "{ persons(name: $n) { follows { follows { name } } } }" with variable "n" bound to "Alice"
    Then the JSON response is:
      """
      {"persons":[{"follows":[{"follows":[{"name":"Carol"}]}]}]}
      """

  Scenario: An interface spanning two tables traverses correctly, self-matches excluded
    When I compile and execute "{ actors { name follows { name } } }"
    Then the compiled query matches the shared label "actor"
    And the compiled query guards against self-matches
    And the JSON response is:
      """
      {"actors":[
        {"name":"Alice","follows":[{"name":"Bob"}]},
        {"name":"Bob","follows":[{"name":"Carol"}]},
        {"name":"Botty","follows":[{"name":"Alice"},{"name":"Erin"}]},
        {"name":"Carol","follows":[{"name":"Alice"},{"name":"Dave"}]},
        {"name":"Dave","follows":[{"name":"Erin"}]}
      ]}
      """

  Scenario: A shared label selects rows from both of its tables
    When I compile and execute "{ actors { name } }"
    Then the JSON response is:
      """
      {"actors":[
        {"name":"Alice"},{"name":"Bob"},{"name":"Carol"},
        {"name":"Dave"},{"name":"Erin"},{"name":"Botty"}
      ]}
      """

  Scenario: An unlabelled interface spans the same two tables by label alternation
    When I compile and execute "{ profiles { name } }"
    Then the compiled query alternates the labels "bot|person"
    And the JSON response is:
      """
      {"profiles":[
        {"name":"Alice"},{"name":"Bob"},{"name":"Carol"},
        {"name":"Dave"},{"name":"Erin"},{"name":"Botty"}
      ]}
      """
