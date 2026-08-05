Feature: M13 identifies a vertex without a surrogate key
  A table gopgql does not own may have no `id`, and gopgql cannot add one to it.
  A declared @key(fields:) becomes that type's identity instead: the columns the
  compiler projects, groups the response by, and compares between positions
  (SPEC.md §7 → M13).

  The fixture is the shape the requirement came from, not a synthetic stand-in —
  dbos.operation_outputs keyed (workflow_uuid, function_id) and dbos.streams
  keyed (workflow_uuid, key, offset), created by a hand-written init script,
  neither with an id column, and one of them with a column named after a
  reserved word. Items 3, 4 and M13 are therefore proven together on the real
  thing.

  Background:
    Given the schema "dbos" already exists with its own tables and rows
    And the SDL:
      """
      type Workflow @node(label: "workflow", table: "workflows", schema: "dbos") @readonly
        @key(fields: ["workflowUuid"]) {
        workflowUuid: ID! @column(name: "workflow_uuid")
        status: String!
        steps: [Step!]! @relationship(
          type: "has_step"
          direction: OUT
          table: "operation_outputs"
          schema: "dbos"
          sourceKey: ["workflow_uuid"]
          destKey: ["workflow_uuid", "function_id"]
        )
      }
      type Step @node(label: "step", table: "operation_outputs", schema: "dbos") @readonly
        @key(fields: ["workflowUuid", "functionId"]) {
        workflowUuid: ID! @column(name: "workflow_uuid")
        functionId: Int! @column(name: "function_id")
        output: String
      }
      type Stream @node(label: "stream", table: "streams", schema: "dbos") @readonly
        @key(fields: ["workflowUuid", "key", "seq"]) {
        workflowUuid: ID! @column(name: "workflow_uuid")
        key: String!
        seq: Int! @column(name: "offset")
        value: String
      }
      """
    And I generate and apply the migrations

  Scenario: The M13 exit — a vertex with a composite key and no id is queryable
    When I compile and execute:
      """
      query { streams { key seq value } }
      """
    Then the JSON response is:
      """
      {"streams":[
        {"key":"a","seq":1,"value":"a1"},
        {"key":"a","seq":2,"value":"a2"},
        {"key":"b","seq":1,"value":"b1"}
      ]}
      """

  Scenario: The M13 exit — a parent deduplicates on its whole key across a fan-out
    When I compile and execute:
      """
      query { workflows { status steps { functionId output } } }
      """
    Then the JSON response is:
      """
      {"workflows":[
        {"status":"done","steps":[{"functionId":0,"output":"s0"},{"functionId":1,"output":"s1"}]},
        {"status":"running","steps":[{"functionId":0,"output":"t0"}]}
      ]}
      """

  Scenario: One table serves as both a vertex element and an edge element
    Then the property graph exposes the elements "operation_outputs, streams, workflows"
    And the graph has an edge element on "dbos.operation_outputs"

  Scenario: A NULL in one key column does not silently drop the row
    Given a stream row whose "key" is NULL
    When I compile and execute:
      """
      query { streams { key seq value } }
      """
    Then the response holds 4 streams

  Scenario: Key values containing a space and a bracket do not collide
    Given the stream rows:
      | key   | seq | value |
      | a b   | 1   | left  |
      | a     | 9   | right |
    When I compile and execute:
      """
      query { streams { key seq value } }
      """
    Then the streams "a b"/1 and "a"/9 are distinct rows

  Scenario: No DDL is emitted for any of these tables
    Then no migration contains "CREATE TABLE dbos"
    And no migration contains "ALTER TABLE dbos"
    And no migration contains "CREATE INDEX"
    But the property graph names "dbos.operation_outputs"
