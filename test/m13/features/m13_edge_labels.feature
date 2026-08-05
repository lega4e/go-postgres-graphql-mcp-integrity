Feature: M13 maps two relationship labels onto one existing table
  A table gopgql does not own can carry more than one traversal. A row of
  dbos.operation_outputs is a step of the workflow that ran it, and — when that
  step started a child — the edge to the workflow it spawned. Both are edges
  over the same table under different labels (SPEC.md §7 → M13).

  gopgql collapsed the two into one and still exited 0, because it kept one edge
  per physical *table* rather than one per relationship *label*. Which label
  survived depended on type order, and the client compiled a MATCH against the
  one that did not, so the loss surfaced as a runtime error against a graph the
  generator had reported as built. Both labels are therefore asserted in the
  emitted DDL, in the catalog, and in a traversal that returns rows.

  Background:
    Given the schema "dbos" already exists with its own tables and rows
    And the SDL:
      """
      type Workflow @node(label: "workflow", table: "workflows", schema: "dbos") @readonly
        @key(fields: ["workflowUuid"]) {
        workflowUuid: ID! @column(name: "workflow_uuid")
        status: String!
        steps: [Step!]! @relationship(
          type: "HAS_STEP"
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
        childWorkflows: [Workflow!]! @relationship(
          type: "SPAWNED"
          direction: OUT
          table: "operation_outputs"
          schema: "dbos"
          sourceKey: ["workflow_uuid", "function_id"]
          destKey: ["child_workflow_uuid"]
        )
      }
      """
    And I generate and apply the migrations

  Scenario: Both labels reach CREATE PROPERTY GRAPH
    Then the property graph declares the edge labels "HAS_STEP, SPAWNED" on "dbos.operation_outputs"
    And the graph has 2 edge elements on "dbos.operation_outputs"
    And the graph has an edge element on "dbos.operation_outputs"

  Scenario: Both traversals resolve against the graph at run time
    # This is the failure the collapse actually produced: the client compiled
    # `-[e0 IS "HAS_STEP"]-> (v1 IS step) -[e1 IS "SPAWNED"]->` against a graph
    # holding only one of the two, so it failed at query time rather than at
    # generate time. Both labels have to be resolvable in one MATCH.
    #
    # A chain is one MATCH pattern, so it matches whole paths only: the answer
    # is the workflow whose step spawned a child, not every workflow. That is
    # the compiler's existing chain semantics and is not what this asserts —
    # what this asserts is that neither label is missing from the graph.
    When I compile and execute:
      """
      query { workflows { status steps { functionId childWorkflows { status } } } }
      """
    Then the JSON response is:
      """
      {"workflows":[
        {"status":"done","steps":[
          {"functionId":1,"childWorkflows":[{"status":"running"}]}
        ]}
      ]}
      """

  Scenario: Generating the same schema twice writes nothing the second time
    Then generating again writes no migration

  Scenario: Still no DDL for a table gopgql does not own
    Then no migration contains "CREATE TABLE dbos"
    And no migration contains "ALTER TABLE dbos"
    And no migration contains "CREATE INDEX"
