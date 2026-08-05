Feature: M10 executes against a handle the caller owns
  gopgql opens exactly one kind of pool and it is read-only. Everything else
  runs through a connection the caller passes in — a pool, a connection, or a
  transaction it opened itself (SPEC.md §7 → M10).

  The property that matters is not the signature but the visibility: a query run
  through the caller's transaction sees that transaction's uncommitted rows, and
  a pool cannot fake that. It is what lets a caller commit gopgql's work and its
  own bookkeeping together, which is the whole basis of an exactly-once append.

  Background:
    Given the SDL:
      """
      type Person @node(label: "person") {
        id: ID!
        name: String!
      }
      """
    And I generate and apply the migrations
    And the following people already exist:
      | name  |
      | Ada   |

  Scenario: The M10 exit — a query in the caller's transaction sees uncommitted rows
    When I begin a transaction and insert "Grace" without committing
    And I execute the query through that transaction:
      """
      query { persons { name } }
      """
    Then the response names "Ada, Grace"

  Scenario: The same rows are invisible to anything outside that transaction
    When I begin a transaction and insert "Grace" without committing
    And I execute the query through a separate pool:
      """
      query { persons { name } }
      """
    Then the response names "Ada"

  Scenario: The read path through the read-only pool is unaffected
    When I execute the query through a read-only pool:
      """
      query { persons { name } }
      """
    Then the response names "Ada"

  Scenario: A write on the read-only pool is refused by the database
    When I attempt a write on a read-only pool
    Then it fails with SQLSTATE "25006"
