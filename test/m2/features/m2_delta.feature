Feature: M2 folds prior migrations and generates delta migrations
  gopgql stops being one-shot. It applies the initial migration, folds that
  migration back into a schema model, diffs it against a widened SDL, and emits
  a delta migration (ALTER TABLE plus a recreated property graph). Every
  scenario applies real SQL to a real postgres:19beta2 container and asserts on
  returned data — never on SQL text.

  Scenario: Adding a field generates a delta that adds the column
    Given the SDL is applied as the initial migration:
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
    When I generate and apply a delta migration from the SDL:
      """
      type Person @node(label: "person") {
        id: ID!
        name: String!
        email: String
        age: Int
        follows: [Person!]! @relationship(type: "follows", direction: OUT)
        followedBy: [Person!]! @relationship(type: "follows", direction: IN)
                               @hasInverse(field: "follows")
      }
      """
    And the following persons exist:
      | name  | email             | age |
      | Alice | alice@example.com | 30  |
      | Bob   | bob@example.com   | 41  |
    And I compile and execute "{ persons { name email age } }"
    Then the JSON response is:
      """
      {"persons":[
        {"name":"Alice","email":"alice@example.com","age":30},
        {"name":"Bob","email":"bob@example.com","age":41}
      ]}
      """

  Scenario: Removing a field generates a delta that drops the column
    Given the SDL is applied as the initial migration:
      """
      type Person @node(label: "person") {
        id: ID!
        name: String!
        email: String
        age: Int
        follows: [Person!]! @relationship(type: "follows", direction: OUT)
        followedBy: [Person!]! @relationship(type: "follows", direction: IN)
                               @hasInverse(field: "follows")
      }
      """
    And the following persons exist:
      | name  | email             | age |
      | Alice | alice@example.com | 30  |
    When I generate and apply a delta migration from the SDL:
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
    Then the "persons" table has no column "age"
    And I compile and execute "{ persons { name email } }"
    And the JSON response is:
      """
      {"persons":[{"name":"Alice","email":"alice@example.com"}]}
      """

  Scenario: Folded migrations produce the same schema as a direct apply
    Given the base SDL:
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
    And the widened SDL:
      """
      type Person @node(label: "person") {
        id: ID!
        name: String!
        email: String
        age: Int
        follows: [Person!]! @relationship(type: "follows", direction: OUT)
        followedBy: [Person!]! @relationship(type: "follows", direction: IN)
                               @hasInverse(field: "follows")
      }
      """
    When I apply the base SDL then a delta to the widened SDL
    And I apply the widened SDL directly as a single migration
    Then the two resulting schemas are identical
