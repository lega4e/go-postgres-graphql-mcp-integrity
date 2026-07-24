Feature: M3 compiles one-hop traversals with bound variables and shapes nesting
  The compiler gains nesting and arguments. A nested relationship selection
  extends the GRAPH_TABLE MATCH with a single edge; a field argument becomes a
  bind-parameter predicate; a GraphQL variable resolves to an ordered $n
  placeholder. The flat rows are regrouped Go-side into the nested response with
  no duplicate parents across the one-to-many fan-out. Every scenario runs the
  compiled SQL against a real postgres:19beta2 container and asserts on the
  shaped JSON — never on SQL text.

  A one-hop MATCH is an inner join, so only parents that have at least one
  matching edge appear (childless parents need M5's multi-pattern workaround);
  the seed graph below gives every asserted parent outgoing or incoming edges.

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
    And the following persons exist:
      | name  |
      | Alice |
      | Bob   |
      | Carol |
      | Dave  |
    And "Alice" follows "Bob"
    And "Alice" follows "Carol"
    And "Bob" follows "Carol"
    And "Bob" follows "Dave"

  Scenario: The M3 exit query — nested one-hop filtered by a bound variable
    When I compile and execute "{ persons(name: $n) { follows { name } } }" with variable "n" bound to "Alice"
    Then the compiled query bound the filter as a parameter
    And the JSON response is:
      """
      {"persons":[{"follows":[{"name":"Bob"},{"name":"Carol"}]}]}
      """

  Scenario: A different variable value returns correspondingly different rows
    When I compile and execute "{ persons(name: $n) { follows { name } } }" with variable "n" bound to "Bob"
    Then the JSON response is:
      """
      {"persons":[{"follows":[{"name":"Carol"},{"name":"Dave"}]}]}
      """

  Scenario: The deduplicated parent is visible with its own scalar field
    When I compile and execute "{ persons(name: $n) { name follows { name } } }" with variable "n" bound to "Alice"
    Then the JSON response is:
      """
      {"persons":[{"name":"Alice","follows":[{"name":"Bob"},{"name":"Carol"}]}]}
      """

  Scenario: An IN relationship traverses the reversed edge
    When I compile and execute "{ persons(name: $n) { followedBy { name } } }" with variable "n" bound to "Carol"
    Then the JSON response is:
      """
      {"persons":[{"followedBy":[{"name":"Alice"},{"name":"Bob"}]}]}
      """

  Scenario: No duplicate parents across a many-parent, many-child fan-out
    When I compile and execute "{ persons { name follows { name } } }"
    Then the JSON response is:
      """
      {"persons":[
        {"name":"Alice","follows":[{"name":"Bob"},{"name":"Carol"}]},
        {"name":"Bob","follows":[{"name":"Carol"},{"name":"Dave"}]}
      ]}
      """
