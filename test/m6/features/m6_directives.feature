Feature: M6 widens the SDL with column overrides, uniqueness and indexes
  The directive @column(name:) renames the physical column, @column(type:)
  overrides the default scalar mapping, @unique makes the database itself reject
  a duplicate, and @index asks for a secondary index with an optional access
  method (SPEC.md §5, §7 → M6). The differ follows: an index or a unique
  constraint added to an SDL becomes an ALTER in the next delta migration, and
  dropping it again is its exact inverse.

  Every scenario runs against a real postgres:19beta2 container: types are read
  back from information_schema, the unique violation is a database error rather
  than a client-side check, and the index is asserted both present in pg_indexes
  and actually chosen by the planner via EXPLAIN.

  Background:
    Given the SDL:
      """
      type Product @node(label: "product") {
        id: ID!
        sku: String! @unique
        title: String! @column(name: "name")
        price: Float! @column(type: "numeric(10,2)")
        category: String! @index(name: "products_category_idx", using: "btree")
      }
      """
    And I generate and apply the initial migration via goose

  Scenario: The M6 exit — a numeric(10,2) column round-trips exact values
    Given the following products exist:
      | sku    | name        | price      | category |
      | SKU-1  | Chain       | 1234.56    | parts    |
      | SKU-2  | Bottle cage | 19.99      | parts    |
      | SKU-3  | Helmet      | 89.00      | safety   |
    Then the column "price" of table "products" has type "numeric(10,2)"
    And the value of "price" for sku "SKU-1" is exactly "1234.56"
    And the value of "price" for sku "SKU-2" is exactly "19.99"
    And the value of "price" for sku "SKU-3" is exactly "89.00"

  Scenario: The M6 exit — a @unique violation is rejected by the database
    Given the following products exist:
      | sku    | name        | price      | category |
      | SKU-1  | Chain       | 1234.56    | parts    |
    Then the constraint "products_sku_key" exists on table "products"
    And inserting sku "SKU-1" again is rejected by the database with a unique violation

  Scenario: The M6 exit — an @index exists and is used
    Given 2000 products in category "parts" and 20 in category "safety"
    Then the index "products_category_idx" exists in pg_indexes
    And it uses the "btree" access method
    And a query filtering on "category" uses the index "products_category_idx"

  Scenario: A renamed column is what the graph exposes and the compiler projects
    Given the following products exist:
      | sku    | name        | price      | category |
      | SKU-1  | Chain       | 1234.56    | parts    |
      | SKU-2  | Bottle cage | 19.99      | parts    |
    Then the table "products" has a column "name" and no column "title"
    When I compile and execute "{ products(title: $t) { title price } }" with variable "t" bound to "Chain"
    Then the compiled query projects the column "name"
    And the JSON response is:
      """
      {"products": [{"title": "Chain", "price": 1234.56}]}
      """

  Scenario: The differ adds a unique constraint and an index, and takes them away
    Given the following products exist:
      | sku    | name        | price      | category |
      | SKU-1  | Chain       | 1234.56    | parts    |
    When I generate and apply a delta migration for the SDL:
      """
      type Product @node(label: "product") {
        id: ID!
        sku: String! @unique
        title: String! @column(name: "name") @unique
        price: Float! @column(type: "numeric(10,2)")
        category: String! @index(name: "products_category_idx", using: "btree")
        vendor: String @index
      }
      """
    Then the constraint "products_name_key" exists on table "products"
    And the index "products_vendor_idx" exists in pg_indexes
    And inserting name "Chain" again is rejected by the database with a unique violation
    When I roll the delta migration back
    Then the constraint "products_name_key" does not exist on table "products"
    And the index "products_vendor_idx" does not exist in pg_indexes
