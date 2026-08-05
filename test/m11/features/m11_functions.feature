Feature: M11 calls PL/pgSQL functions the database already owns
  A mutation field carrying @function maps to a call on a function somebody else
  wrote (SPEC.md §5, §7 → M11). Arguments map to the function's parameters by
  name, an argument the operation document does not pass reaches the function's
  own SQL DEFAULT, a declared VOID return is executed rather than read, and an
  error the function raises comes back carrying its SQLSTATE.

  Every scenario runs against a real postgres:19beta2 container with
  hand-written PL/pgSQL in the fixture, and asserts on the values the functions
  actually wrote — not on the SQL gopgql emitted. A named-notation bug, a
  default that never reached the database, or a swallowed SQLSTATE are all
  invisible to a golden-file check and all fail here.

  Background:
    Given the schema "app" with its functions
    And the SDL:
      """
      type Person @node(label: "person") {
        id: ID!
        name: String!
      }
      type Mutation {
        enqueue(
          digest: String!
          userId: String! @column(name: "user_id")
          queue: String = "agent"
          priority: Int
        ): String! @function(schema: "app", name: "enqueue_workflow")

        touch(streamId: String! @column(name: "stream_id")): Boolean!
          @function(schema: "app", name: "touch_stream", returns: VOID)

        explode(code: String!): String! @function(schema: "app", name: "explode")
      }
      """

  Scenario: The M11 exit — a scalar function is called through the caller's transaction
    When I call in a transaction:
      """
      mutation { enqueue(digest: "sha256:a", userId: "u-1") }
      """
    Then the call returned "wf-1"
    And the transaction can see 1 row in "app.workflows"
    And after committing, "app.workflows" holds 1 row

  Scenario: An uncommitted call is invisible outside its transaction
    When I call in a transaction:
      """
      mutation { enqueue(digest: "sha256:a", userId: "u-1") }
      """
    Then the transaction can see 1 row in "app.workflows"
    And a pool outside the transaction sees 0 rows in "app.workflows"

  Scenario: A rolled-back call leaves nothing behind
    When I call in a transaction:
      """
      mutation { enqueue(digest: "sha256:a", userId: "u-1") }
      """
    And I roll the transaction back
    Then "app.workflows" holds 0 rows

  Scenario: Arguments map by name, not by position
    When I call in a transaction:
      """
      mutation { enqueue(userId: "u-9", digest: "sha256:z") }
      """
    And I commit the transaction
    Then the workflow "wf-1" has "digest" = "sha256:z"
    And the workflow "wf-1" has "user_id" = "u-9"

  Scenario: An argument the document omits reaches the function's own DEFAULT
    When I call in a transaction:
      """
      mutation { enqueue(digest: "d", userId: "u") }
      """
    And I commit the transaction
    Then the workflow "wf-1" has "priority" = "5"

  Scenario: NULL is not DEFAULT
    When I call in a transaction:
      """
      mutation Q($p: Int) { enqueue(digest: "d", userId: "u", priority: $p) }
      """
    And I commit the transaction
    Then the workflow "wf-1" has a null "priority"

  Scenario: A GraphQL default declared in the SDL is applied
    When I call in a transaction:
      """
      mutation { enqueue(digest: "d", userId: "u") }
      """
    And I commit the transaction
    Then the workflow "wf-1" has "queue" = "agent"

  Scenario: A declared VOID function is executed and reports success
    When I call in a transaction:
      """
      mutation { touch(streamId: "s-1") }
      """
    Then the call returned true
    And the transaction can see 1 row in "app.streams"
    And after committing, "app.streams" holds 1 row

  Scenario: The M11 exit — a raised exception surfaces with its SQLSTATE
    When I call in a transaction and it fails:
      """
      mutation { explode(code: "P0001") }
      """
    Then the error carries SQLSTATE "P0001"
    And the error message is "deliberate failure"

  Scenario: A call on the read-only pool is refused by the database
    When I call on a read-only pool and it fails:
      """
      mutation { touch(streamId: "s-1") }
      """
    Then the error carries SQLSTATE "25006"
