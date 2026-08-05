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
// # Edges over tables gopgql does not own
//
// A @relationship ordinarily produces an edge *table* that gopgql creates, with
// source_id/target_id referencing each endpoint's surrogate id. Over an
// unmanaged endpoint that is not available: the table already exists, gopgql
// must not create it, and the columns acting as its keys are whatever its owner
// chose. `@relationship(sourceKey:, destKey:)` names them, and `table:` names
// the existing table (SPEC.md §7 → M13).
//
// A relationship touching an unmanaged type **without** them is still refused,
// because the alternative is a graph whose edge silently references a column
// that does not exist — a silent fallback, which SPEC.md §10 forbids.

// validateUnmanaged enforces the boundary above for one type.
func (d *Document) validateUnmanaged(n *Node) error {
	for _, f := range n.Fields {
		if f.Rel == nil {
			continue
		}
		if err := d.validateRelationshipMapping(n, f); err != nil {
			return err
		}
	}
	return nil
}

// validateRelationshipMapping decides which of the two edge mappings a
// relationship uses, and refuses the combinations that cannot mean anything.
func (d *Document) validateRelationshipMapping(n *Node, f *Field) error {
	rel := f.Rel
	target := d.NodeByType(f.TypeName)
	existing := len(rel.SourceKey) > 0 || len(rel.DestKey) > 0
	touchesUnmanaged := n.ReadOnly || (target != nil && target.ReadOnly)

	if !existing {
		if rel.Schema != "" {
			return fmt.Errorf("sdl: %s.%s declares @relationship(schema: %q), which can only mean an edge "+
				"table that already exists; name its key columns with "+
				"@relationship(sourceKey:, destKey:) so gopgql maps that table instead of creating one",
				n.TypeName, f.Name, rel.Schema)
		}
		if touchesUnmanaged {
			which := n.TypeName
			if !n.ReadOnly {
				which = target.TypeName
			}
			return fmt.Errorf("sdl: %s.%s relates to %s, which is @readonly; gopgql would have to create an "+
				"edge table referencing a table it does not own. Map an existing table instead: "+
				"@relationship(table:, sourceKey:, destKey:)", n.TypeName, f.Name, which)
		}
		return nil
	}

	// From here the edge is mapped onto an existing table.
	if len(rel.SourceKey) == 0 || len(rel.DestKey) == 0 {
		return fmt.Errorf("sdl: %s.%s declares only one of @relationship(sourceKey:, destKey:); "+
			"an edge over an existing table needs both, because gopgql derives neither",
			n.TypeName, f.Name)
	}
	if rel.Table == "" {
		return fmt.Errorf("sdl: %s.%s declares @relationship(sourceKey:, destKey:) but no table:; "+
			"those name the columns of an existing table, so which table has to be said",
			n.TypeName, f.Name)
	}
	if target == nil {
		return nil // validateRelField reports the unmapped target
	}

	// The key columns reference each endpoint's identity, so their counts have
	// to match it — SOURCE KEY (a, b) REFERENCES t (c, d) is arity-checked by
	// PostgreSQL, and catching it here names the field instead of the SQL.
	src, dst := n, target
	if rel.Direction == In {
		src, dst = target, n
	}
	if got, want := len(rel.SourceKey), len(src.IdentityColumns()); got != want {
		return fmt.Errorf("sdl: %s.%s: @relationship(sourceKey:) names %d column(s), but %s is identified by "+
			"%d (%s)", n.TypeName, f.Name, got, src.TypeName, want, strings.Join(src.IdentityColumns(), ", "))
	}
	if got, want := len(rel.DestKey), len(dst.IdentityColumns()); got != want {
		return fmt.Errorf("sdl: %s.%s: @relationship(destKey:) names %d column(s), but %s is identified by "+
			"%d (%s)", n.TypeName, f.Name, got, dst.TypeName, want, strings.Join(dst.IdentityColumns(), ", "))
	}
	return nil
}
