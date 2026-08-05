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
// `@node(schema:)` applies to any type, managed or not. gopgql emits no
// CREATE SCHEMA — a schema it does not own is not its to create — so a managed
// table in another schema depends on that schema already existing; that is the
// author's business, and the same is true of every table an SDL names.
//
// # What is deliberately not here — all of it M13
//
// Two things a full "expose somebody else's schema" story needs are not
// implemented, and each is refused with a message naming what is missing rather
// than mis-generating quietly (SPEC.md §10 forbids a silent fallback):
//
//   - **Relationships over tables gopgql does not own.** A @relationship
//     produces an edge *table*, which gopgql would have to create; over an
//     unmanaged endpoint it would also have to be told which of that table's
//     columns are its source and destination keys. That is
//     `@relationship(sourceKey:, destKey:)`, which M13 adds. Until then a
//     relationship touching an unmanaged type is refused, and
//     `@relationship(schema:)` — which can only mean "an edge table that already
//     exists" — is refused with it.
//
//   - **A vertex with no surrogate key.** An unmanaged type still needs
//     `id: ID!` (validateKey). `dbos.operation_outputs`, the motivating table,
//     has none: making a declared `@key(fields:)` the identity instead reaches
//     into the compiler's three `id` projection sites and the shaper's parent
//     dedup, which is M13 and lands after gopgql#10 (SPEC.md §7 → M13).

// validateUnmanaged enforces the boundary above for one type.
func (d *Document) validateUnmanaged(n *Node) error {
	if strings.Contains(n.Table, ".") || strings.Contains(n.Schema, ".") {
		// A dot inside a name would make the qualified identifier ambiguous
		// everywhere it is read back — a table "a.b" and a table "b" in schema
		// "a" would be indistinguishable to the fold.
		return fmt.Errorf("sdl: %s maps to %s, and a table or schema name may not contain a dot: "+
			"the qualified identifier could not be read back unambiguously", n.TypeName, qualified(n))
	}
	for _, f := range n.Fields {
		if f.Rel == nil {
			continue
		}
		if f.Rel.Schema != "" {
			return fmt.Errorf("sdl: %s.%s declares @relationship(schema: %q), which can only mean an edge table "+
				"that already exists; naming its key columns needs @relationship(sourceKey:/destKey:), which "+
				"arrives in M13", n.TypeName, f.Name, f.Rel.Schema)
		}
		target := d.NodeByType(f.TypeName)
		switch {
		case n.ReadOnly:
			return fmt.Errorf("sdl: %s is @readonly and declares the relationship %s; an edge is a table gopgql "+
				"would have to create, and over a table it does not own it would also have to be told which "+
				"columns are its keys — @relationship(sourceKey:/destKey:) arrives in M13",
				n.TypeName, f.Name)
		case target != nil && target.ReadOnly:
			return fmt.Errorf("sdl: %s.%s points at %s, which is @readonly; the edge table gopgql would create "+
				"references it by its surrogate id, and naming an existing table's key columns instead needs "+
				"@relationship(sourceKey:/destKey:), which arrives in M13",
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
