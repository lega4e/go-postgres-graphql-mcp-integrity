package migrate

import (
	"strings"

	"github.com/lega4e/gopgql/schema"
)

// This file holds the small, focused SQL readers the fold interpreter uses.
// They understand exactly the shapes gopgql emits — quoted or bare identifiers,
// parenthesised lists, and the column and property-graph grammars — and nothing
// more (SPEC.md §7 → M2: "not a general DDL parser").

// isDelim reports whether b is a structural delimiter that ends an identifier.
func isDelim(b byte) bool { return b == '(' || b == ')' || b == ',' }

// isSpace reports whether b is ASCII whitespace.
func isSpace(b byte) bool { return b == ' ' || b == '\t' || b == '\n' || b == '\r' }

// readIdent reads one identifier off the front of s, unquoting a
// double-quoted identifier (with "" escapes) or reading a bare token up to the
// next delimiter or whitespace. It returns the identifier and the unconsumed
// remainder.
func readIdent(s string) (ident, rest string) {
	s = strings.TrimLeft(s, " \t\n\r")
	if s == "" {
		return "", ""
	}
	if s[0] == '"' {
		var b strings.Builder
		i := 1
		for i < len(s) {
			if s[i] == '"' {
				if i+1 < len(s) && s[i+1] == '"' { // "" escape → literal quote
					b.WriteByte('"')
					i += 2
					continue
				}
				i++ // consume closing quote
				break
			}
			b.WriteByte(s[i])
			i++
		}
		return b.String(), s[i:]
	}
	j := 0
	for j < len(s) && !isDelim(s[j]) && !isSpace(s[j]) {
		j++
	}
	return s[:j], s[j:]
}

// betweenParens returns the content between the first '(' in s and its matching
// ')'. ok is false when no balanced group is present.
func betweenParens(s string) (inner string, ok bool) {
	start := strings.IndexByte(s, '(')
	if start < 0 {
		return "", false
	}
	depth := 0
	for i := start; i < len(s); i++ {
		switch s[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return s[start+1 : i], true
			}
		}
	}
	return "", false
}

// splitTopLevel splits s on sep, ignoring separators nested inside parentheses.
func splitTopLevel(s string, sep byte) []string {
	var parts []string
	depth := 0
	start := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '(':
			depth++
		case ')':
			if depth > 0 {
				depth--
			}
		case sep:
			if depth == 0 {
				parts = append(parts, s[start:i])
				start = i + 1
			}
		}
	}
	parts = append(parts, s[start:])
	return parts
}

// parseColumn parses one entry from a CREATE TABLE column list (or the tail of
// an ADD COLUMN). It returns isColumn=false for a table-level constraint such as
// "PRIMARY KEY (source_id, target_id)", which carries no column of its own.
//
// The grammar mirrors generator.ColumnDDL exactly:
//
//	<name> <type> [PRIMARY KEY | NOT NULL] [DEFAULT <expr>] [REFERENCES <t> (<c>)]
//
// so clauses are peeled from the right (REFERENCES, then DEFAULT, then the
// nullability/key marker), leaving "<name> <type>".
func parseColumn(def string) (schema.Column, bool) {
	def = strings.TrimSpace(def)
	upper := strings.ToUpper(def)
	if strings.HasPrefix(upper, "PRIMARY KEY") || strings.HasPrefix(upper, "FOREIGN KEY") ||
		strings.HasPrefix(upper, "UNIQUE") || strings.HasPrefix(upper, "CHECK") ||
		strings.HasPrefix(upper, "CONSTRAINT") {
		return schema.Column{}, false
	}

	var c schema.Column
	if i := keywordIndex(def, "REFERENCES"); i >= 0 {
		ref := strings.TrimSpace(def[i+len("REFERENCES"):])
		c.References = parseReference(ref)
		def = strings.TrimSpace(def[:i])
	}
	if i := keywordIndex(def, "DEFAULT"); i >= 0 {
		c.Default = strings.TrimSpace(def[i+len("DEFAULT"):])
		def = strings.TrimSpace(def[:i])
	}
	if i := keywordIndex(def, "PRIMARY KEY"); i >= 0 {
		c.PrimaryKey = true
		def = strings.TrimSpace(def[:i])
	} else if i := keywordIndex(def, "NOT NULL"); i >= 0 {
		c.NotNull = true
		def = strings.TrimSpace(def[:i])
	}

	name, rest := readIdent(def)
	c.Name = name
	typ := strings.TrimSpace(rest)
	if strings.HasSuffix(typ, "[]") {
		c.Array = true
		typ = strings.TrimSpace(strings.TrimSuffix(typ, "[]"))
	}
	c.Type = typ
	return c, name != ""
}

// parseReference parses a "<table> (<column>)" foreign-key target.
func parseReference(s string) *schema.Reference {
	table, rest := readIdent(s)
	inner, ok := betweenParens(rest)
	if !ok {
		return nil
	}
	col, _ := readIdent(inner)
	return &schema.Reference{Table: table, Column: col}
}

// keywordIndex returns the byte index of a whole-word, case-insensitive
// occurrence of kw in s at parenthesis depth zero, or -1. Whole-word means the
// characters around the match are whitespace, a delimiter, or a string
// boundary — so "DEFAULT" never matches inside an identifier.
func keywordIndex(s, kw string) int {
	up := strings.ToUpper(s)
	kw = strings.ToUpper(kw)
	depth := 0
	for i := 0; i+len(kw) <= len(up); i++ {
		switch s[i] {
		case '(':
			depth++
			continue
		case ')':
			if depth > 0 {
				depth--
			}
			continue
		}
		if depth != 0 {
			continue
		}
		if up[i:i+len(kw)] != kw {
			continue
		}
		if i > 0 && !isSpace(s[i-1]) && !isDelim(s[i-1]) {
			continue
		}
		after := i + len(kw)
		if after < len(s) && !isSpace(s[after]) && !isDelim(s[after]) {
			continue
		}
		return i
	}
	return -1
}

// graphModel is the parsed CREATE PROPERTY GRAPH: its name and the vertex and
// edge table entries that classify the schema's tables and carry labels,
// property lists and edge key metadata.
type graphModel struct {
	name     string
	vertices []vertexEntry
	edges    []edgeEntry
}

type vertexEntry struct {
	table      string
	label      string
	properties []string
}

type edgeEntry struct {
	table       string
	label       string
	sourceKey   string
	sourceTable string
	sourceRef   string
	destKey     string
	destTable   string
	destRef     string
	properties  []string
}

// parseGraph parses a normalised (single-spaced) CREATE PROPERTY GRAPH
// statement. The grammar is fixed by generator.GraphDDL, so parsing is
// positional over a token stream.
func parseGraph(norm string) (*graphModel, error) {
	p := &gparser{toks: tokenize(norm)}
	if err := p.expect("CREATE", "PROPERTY", "GRAPH"); err != nil {
		return nil, err
	}
	g := &graphModel{}
	name, err := p.ident()
	if err != nil {
		return nil, err
	}
	g.name = name

	if err := p.expect("VERTEX", "TABLES", "("); err != nil {
		return nil, err
	}
	for {
		v, err := p.vertexEntry()
		if err != nil {
			return nil, err
		}
		g.vertices = append(g.vertices, v)
		if p.accept(",") {
			continue
		}
		break
	}
	if err := p.expect(")"); err != nil {
		return nil, err
	}

	if p.acceptWord("EDGE") {
		if err := p.expect("TABLES", "("); err != nil {
			return nil, err
		}
		for {
			e, err := p.edgeEntry()
			if err != nil {
				return nil, err
			}
			g.edges = append(g.edges, e)
			if p.accept(",") {
				continue
			}
			break
		}
		if err := p.expect(")"); err != nil {
			return nil, err
		}
	}
	return g, nil
}
