Feature: M13 keeps two same-named edge tables in different schemas apart
  Two schemas owned by two tools may each hold a table called `links`. They are
  two tables, so they are two edges — but the key gopgql deduplicated edges on
  ignored the schema, so the second one was dropped exactly as a second label
  on one table was.

  Element aliases are unqualified, which is why each edge also has to be given
  one: `alpha.links` and `beta.links` cannot both be the element `links`.

  Background:
    Given the schemas "alpha" and "beta" each already own a "links" table
    And the SDL:
      """
      type AlphaNode @node(label: "alpha_node", table: "alpha_nodes", schema: "alpha") @readonly
        @key(fields: ["alphaId"]) {
        alphaId: ID! @column(name: "alpha_id")
        peers: [AlphaNode!]! @relationship(
          type: "ALPHA_LINK"
          direction: OUT
          table: "links"
          schema: "alpha"
          sourceKey: ["alpha_src"]
          destKey: ["alpha_dst"]
        )
      }
      type BetaNode @node(label: "beta_node", table: "beta_nodes", schema: "beta") @readonly
        @key(fields: ["betaId"]) {
        betaId: ID! @column(name: "beta_id")
        peers: [BetaNode!]! @relationship(
          type: "BETA_LINK"
          direction: OUT
          table: "links"
          schema: "beta"
          sourceKey: ["beta_src"]
          destKey: ["beta_dst"]
        )
      }
      """
    And I generate and apply the migrations

  Scenario: Each schema contributes its own edge element
    Then the property graph declares the edge labels "ALPHA_LINK" on "alpha.links"
    And the property graph declares the edge labels "BETA_LINK" on "beta.links"
    And the graph has 1 edge element on "alpha.links"
    And the graph has 1 edge element on "beta.links"

  Scenario: Generating the same schema twice writes nothing the second time
    Then generating again writes no migration
