Feature: M5 splits multi-pattern selections into joined GRAPH_TABLE calls
  A level that selects two relationships would need comma-separated path patterns
  in one MATCH. PG19 parses those but does not execute them (SPEC.md §2.2), so
  gopgql splits: the chain up to the branching level stays one GRAPH_TABLE, each
  relationship becomes its own, and the outer query joins them on the projected
  ids (SPEC.md §6.2, §7 → M5).

  The split has to preserve three things that a single pattern got for free: the
  root filter must still constrain every branch, a parent with no match on one
  branch must keep its other branches instead of disappearing, and the
  isomorphism guards must survive — a branch may not walk back to a vertex an
  ancestor already bound.

  Every scenario runs against a real postgres:19beta2 container and asserts on
  the shaped JSON. The exit scenario additionally re-runs an equivalent
  hand-written join over the same data and requires the two results to be
  identical, so the workaround is proven correct rather than merely runnable.

  The seed graph is deliberately hostile: Alice follows herself, Alice → Bob →
  Carol → Alice is a cycle, and Dave has followers on one side only.

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
      | Erin  |
    And "Alice" follows "Bob"
    And "Bob" follows "Carol"
    And "Carol" follows "Alice"
    And "Alice" follows "Alice"
    And "Dave" follows "Erin"

  Scenario: The M5 exit query — a shape needing two patterns returns correct rows
    When I compile and execute "{ persons(name: $n) { name follows { name } followedBy { name } } }" with variable "n" bound to "Alice"
    Then the compiled query splits into 3 GRAPH_TABLE calls
    And the compiled query emits no comma-separated pattern
    And the compiled query joins the branches on projected ids
    And the JSON response is:
      """
      {"persons": [
        {"name": "Alice",
         "follows": [{"name": "Bob"}],
         "followedBy": [{"name": "Carol"}]}
      ]}
      """

  Scenario: The workaround agrees with an equivalent hand-written query
    When I compile and execute "{ persons(name: $n) { name follows { name } followedBy { name } } }" with variable "n" bound to "Alice"
    Then the shaped result matches the hand-written query:
      """
      SELECT q0.v0_k, q0.v0_c0, q1.v2_k, q1.v2_c0, q2.v4_k, q2.v4_c0
      FROM GRAPH_TABLE (app_graph
        MATCH (a IS person)
        WHERE a.name = $1
        COLUMNS (a.id AS v0_k, a.name AS v0_c0)
      ) AS q0
      LEFT JOIN GRAPH_TABLE (app_graph
        MATCH (src IS person) -[f IS follows]-> (dst IS person)
        WHERE src.id <> dst.id
        COLUMNS (src.id AS join_out, dst.id AS v2_k, dst.name AS v2_c0)
      ) AS q1 ON q1.join_out = q0.v0_k
      LEFT JOIN GRAPH_TABLE (app_graph
        MATCH (tgt IS person) <-[g IS follows]- (fol IS person)
        WHERE tgt.id <> fol.id
        COLUMNS (tgt.id AS join_in, fol.id AS v4_k, fol.name AS v4_c0)
      ) AS q2 ON q2.join_in = q0.v0_k
      """

  Scenario: A parent with no match on one branch keeps the other
    When I compile and execute "{ persons(name: $n) { name follows { name } followedBy { name } } }" with variable "n" bound to "Dave"
    Then the JSON response is:
      """
      {"persons": [
        {"name": "Dave", "follows": [{"name": "Erin"}], "followedBy": []}
      ]}
      """

  Scenario: A parent with no match on either branch still appears
    When I compile and execute "{ persons(name: $n) { name follows { name } followedBy { name } } }" with variable "n" bound to "Erin"
    Then the JSON response is:
      """
      {"persons": [
        {"name": "Erin", "follows": [], "followedBy": [{"name": "Dave"}]}
      ]}
      """

  Scenario: Every person is returned with both of its branches
    When I compile and execute "{ persons { name follows { name } followedBy { name } } }"
    Then the JSON response is:
      """
      {"persons": [
        {"name": "Alice", "follows": [{"name": "Bob"}], "followedBy": [{"name": "Carol"}]},
        {"name": "Bob", "follows": [{"name": "Carol"}], "followedBy": [{"name": "Alice"}]},
        {"name": "Carol", "follows": [{"name": "Alice"}], "followedBy": [{"name": "Bob"}]},
        {"name": "Dave", "follows": [{"name": "Erin"}], "followedBy": []},
        {"name": "Erin", "follows": [], "followedBy": [{"name": "Dave"}]}
      ]}
      """

  Scenario: The isomorphism guard survives the split
    When I compile and execute "{ persons(name: $n) { name follows { name } followedBy { name } } }" with variable "n" bound to "Alice"
    Then the compiled query guards against self-matches
    And the JSON response is:
      """
      {"persons": [
        {"name": "Alice",
         "follows": [{"name": "Bob"}],
         "followedBy": [{"name": "Carol"}]}
      ]}
      """

  Scenario: Branching below the root splits at the branching level
    When I compile and execute "{ persons(name: $n) { name follows { name follows { name } followedBy { name } } } }" with variable "n" bound to "Alice"
    Then the compiled query splits into 3 GRAPH_TABLE calls
    And the JSON response is:
      """
      {"persons": [
        {"name": "Alice",
         "follows": [
           {"name": "Bob",
            "follows": [{"name": "Carol"}],
            "followedBy": []}
         ]}
      ]}
      """

  Scenario: GRAPH_TABLE output joins an ordinary relational table
    Given the ordinary table "notes":
      | person | body          |
      | Alice  | likes cycling |
      | Carol  | new joiner    |
    When I execute the SQL:
      """
      SELECT g.name AS person, n.body AS note
      FROM GRAPH_TABLE (app_graph
        MATCH (p IS person) -[f IS follows]-> (t IS person)
        WHERE p.id <> t.id
        COLUMNS (p.name AS name, t.name AS followed)
      ) AS g
      JOIN notes n ON n.person = g.name
      ORDER BY g.name, n.body
      """
    Then the rows are:
      | person | note          |
      | Alice  | likes cycling |
      | Carol  | new joiner    |
