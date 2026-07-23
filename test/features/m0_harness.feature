Feature: M0 harness proves SQL/PGQ end to end
  The hand-written schema, seed data, and GRAPH_TABLE queries confirm that
  PostgreSQL 19 SQL/PGQ is available and that per-scenario reset works: each
  scenario starts from the schema-only snapshot with no rows.

  Background:
    Given the property graph "app_graph" is available

  Scenario: Query vertex properties through GRAPH_TABLE
    Given the following persons exist:
      | name  | email             |
      | Alice | alice@example.com |
      | Bob   | bob@example.com   |
    When I run the vertex query
    Then the returned names are:
      | name  |
      | Alice |
      | Bob   |

  Scenario: The previous scenario's rows do not leak
    When I run the vertex query
    Then no rows are returned

  Scenario: Traverse a follows edge through GRAPH_TABLE
    Given the following persons exist:
      | name  | email |
      | Alice |       |
      | Bob   |       |
      | Carol |       |
    And "Alice" follows "Bob"
    And "Bob" follows "Carol"
    When I run the follows query
    Then the returned follow pairs are:
      | follower | followed |
      | Alice    | Bob      |
      | Bob      | Carol    |
