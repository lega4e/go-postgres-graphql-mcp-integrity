package sdl

import (
	"fmt"
	"strings"
)

// @readonly and schema qualification, and the boundary between what they
// deliver and what they do not (SPEC.md §7 → M12).
//
// @readonly says gopgql surfaces a table it does not own: the type contributes
// a vertex element to the property graph and is queryable like any other, and
// **no DDL and no migration is ever emitted for it** — no CREATE TABLE, no
// ALTER, no DROP, no index. Somebody else's tool creates and migrates it.
//
// @node(schema:) qualifies the table's identifier so that a table in another
// PostgreSQL schema can be *named* at all. Before this, every identifier gopgql
// emitted resolved through search_path and a table outside it was unreachable.
//
// # What is deliberately not here
//
// Two things a full "expose somebody else's schema" story needs are not
// implemented, and each is refused with a message that names it rather than
// mis-generating quietly (SPEC.md §10 forbids a silent fallback):
//
//   - **Qualification of a table gopgql creates.** `@node(schema:)` is accepted
//     only together with `@readonly`. gopgql emits no CREATE SCHEMA, so a
//     managed table in a schema gopgql did not create is a migration that
//     depends on out-of-band setup — and the whole reason qualification exists
//     is to reach tables that are already there. A managed table is created
//     where the connection's search_path puts it, exactly as before.
//
//   - **Relationships over tables gopgql does not own.** A @relationship
//     produces an edge *table*, which gopgql would have to create; over an
//     unmanaged endpoint it would also have to guess which of that table's
//     columns are the source and destination keys. Expressing those needs
//     `@relationship(sourceKey:, destKey:)` naming existing columns, which is
//     not implemented. A relationship touching an unmanaged type is therefore
//     refused, and `@relationship(schema:)` — which can only mean "an edge table
//     that already exists" — is refused with it.
//
// An unmanaged type also still needs the surrogate `id: ID!` every @node needs
// (validateKey). A table with no surrogate key at all — `dbos.operation_outputs`
// is the motivating one — needs a declared natural key to become the vertex
// identity, which reaches into the compiler's three `id` projection sites and
// the shaper's parent dedup. That is its own change and is not this one.

// validateUnmanaged enforces the boundary above for one type.
func (d *Document) validateUnmanaged(n *Node) error {
	if strings.Contains(n.Table, ".") || strings.Contains(n.Schema, ".") {
		// A dot inside a name would make the qualified identifier ambiguous
		// everywhere it is read back — a table "a.b" and a table "b" in schema
		// "a" would be indistinguishable to the fold.
		return fmt.Errorf("sdl: %s maps to %s, and a table or schema name may not contain a dot: "+
			"the qualified identifier could not be read back unambiguously", n.TypeName, qualified(n))
	}
	if n.Schema != "" && !n.ReadOnly {
		return fmt.Errorf("sdl: %s declares @node(schema: %q) without @readonly; gopgql emits no CREATE SCHEMA, "+
			"so it qualifies only tables it does not create — add @readonly, or drop the schema and let the "+
			"connection's search_path place the table", n.TypeName, n.Schema)
	}

	for _, f := range n.Fields {
		if f.Rel == nil {
			continue
		}
		if f.Rel.Schema != "" {
			return fmt.Errorf("sdl: %s.%s declares @relationship(schema: %q), which can only mean an edge table "+
				"that already exists; naming its key columns needs @relationship(sourceKey:/destKey:), which is "+
				"not implemented", n.TypeName, f.Name, f.Rel.Schema)
		}
		target := d.NodeByType(f.TypeName)
		switch {
		case n.ReadOnly:
			return fmt.Errorf("sdl: %s is @readonly and declares the relationship %s; an edge is a table gopgql "+
				"would have to create, and over a table it does not own it would also have to be told which "+
				"columns are its keys — @relationship(sourceKey:/destKey:) is not implemented",
				n.TypeName, f.Name)
		case target != nil && target.ReadOnly:
			return fmt.Errorf("sdl: %s.%s points at %s, which is @readonly; the edge table gopgql would create "+
				"references it by its surrogate id, and naming an existing table's key columns instead needs "+
				"@relationship(sourceKey:/destKey:), which is not implemented",
				n.TypeName, f.Name, target.TypeName)
		}
	}
	return nil
}

// qualified renders a node's table as it appears in DDL, for an error message.
func qualified(n *Node) string {
	if n.Schema == "" {
		return n.Table
	}
	return n.Schema + "." + n.Table
}
