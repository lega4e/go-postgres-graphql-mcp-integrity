package ddl

import (
	"fmt"
	"strings"
)

// Parse lexes src and parses it into the sequence of DDL statements it contains,
// separated by top-level semicolons. Empty statements (a stray semicolon, blank
// input) are skipped. An unrecognised statement, or any structural surprise,
// yields an error naming the offending token — this reader interprets gopgql's
// own DDL only (SPEC.md §7 → M2).
func Parse(src string) ([]Statement, error) {
	toks, err := Lex(src)
	if err != nil {
		return nil, err
	}
	p := &parser{src: src, toks: toks}
	var stmts []Statement
	for {
		for p.at(TokenSemicolon) {
			p.advance()
		}
		if p.at(TokenEOF) {
			return stmts, nil
		}
		st, err := p.statement()
		if err != nil {
			return nil, err
		}
		stmts = append(stmts, st)
		if !p.at(TokenSemicolon) && !p.at(TokenEOF) {
			return nil, p.errorf("expected ';' or end of statement, got %q", p.peek().Value)
		}
	}
}

// parser is a positional cursor over a token slice, backed by the source string
// so it can recover verbatim text (a DEFAULT expression) from token spans.
type parser struct {
	src  string
	toks []Token
	i    int
}

func (p *parser) peek() Token { return p.toks[p.i] }

func (p *parser) advance() Token {
	t := p.toks[p.i]
	if p.i < len(p.toks)-1 {
		p.i++
	}
	return t
}

func (p *parser) at(tt TokenType) bool { return p.peek().Type == tt }

// keyword reports whether the token at offset ahead is the given keyword word
// (case-insensitive).
func (p *parser) keywordAt(ahead int, word string) bool {
	j := p.i + ahead
	if j >= len(p.toks) {
		return false
	}
	t := p.toks[j]
	return t.Type == TokenWord && strings.EqualFold(t.Value, word)
}

// acceptKeyword consumes the next len(words) tokens iff they match words in
// order (case-insensitively) as bare keywords, and reports whether it did.
func (p *parser) acceptKeyword(words ...string) bool {
	for k, w := range words {
		if !p.keywordAt(k, w) {
			return false
		}
	}
	p.i += len(words)
	return true
}

// expectKeyword consumes the keyword sequence or returns an error.
func (p *parser) expectKeyword(words ...string) error {
	if !p.acceptKeyword(words...) {
		return p.errorf("expected %q, got %q", strings.Join(words, " "), p.peek().Value)
	}
	return nil
}

func (p *parser) acceptPunct(tt TokenType) bool {
	if p.at(tt) {
		p.advance()
		return true
	}
	return false
}

func (p *parser) expectPunct(tt TokenType, name string) error {
	if !p.acceptPunct(tt) {
		return p.errorf("expected %q, got %q", name, p.peek().Value)
	}
	return nil
}

// ident consumes the next token as an identifier (bare word or quoted),
// returning its unquoted value.
func (p *parser) ident(what string) (string, error) {
	t := p.peek()
	if !t.IsIdent() {
		return "", p.errorf("expected %s, got %q", what, t.Value)
	}
	p.advance()
	return t.Value, nil
}

func (p *parser) errorf(format string, args ...any) error {
	return fmt.Errorf("ddl: "+format+" at offset %d", append(args, p.peek().Pos)...)
}

// statement dispatches on the leading keyword to the matching production.
func (p *parser) statement() (Statement, error) {
	switch {
	case p.acceptKeyword("CREATE", "TABLE"):
		return p.createTable()
	case p.acceptKeyword("CREATE", "INDEX"):
		return p.createIndex()
	case p.acceptKeyword("CREATE", "PROPERTY", "GRAPH"):
		return p.createPropertyGraph()
	case p.acceptKeyword("ALTER", "TABLE"):
		return p.alterTable()
	case p.acceptKeyword("DROP", "TABLE"):
		return p.dropTable()
	case p.acceptKeyword("DROP", "INDEX"):
		return p.dropIndex()
	case p.acceptKeyword("DROP", "PROPERTY", "GRAPH"):
		return p.dropPropertyGraph()
	default:
		return nil, p.errorf("unrecognised statement starting at %q", p.peek().Value)
	}
}

func (p *parser) createTable() (Statement, error) {
	name, err := p.ident("table name")
	if err != nil {
		return nil, err
	}
	if err := p.expectPunct(TokenLParen, "("); err != nil {
		return nil, err
	}
	st := &CreateTableStmt{Name: name}
	for {
		if p.isTableConstraintStart() {
			c, err := p.tableConstraint()
			if err != nil {
				return nil, err
			}
			st.Constraints = append(st.Constraints, c)
		} else {
			c, err := p.columnDef()
			if err != nil {
				return nil, err
			}
			st.Columns = append(st.Columns, c)
		}
		if p.acceptPunct(TokenComma) {
			continue
		}
		break
	}
	if err := p.expectPunct(TokenRParen, ")"); err != nil {
		return nil, err
	}
	return st, nil
}

// columnConstraintWords are the keywords that begin a column constraint, and so
// terminate the (possibly multi-word) type that precedes them.
var columnConstraintWords = map[string]bool{
	"PRIMARY": true, "NOT": true, "NULL": true, "DEFAULT": true,
	"REFERENCES": true, "UNIQUE": true, "CHECK": true,
}

func (p *parser) columnDef() (ColumnDef, error) {
	name, err := p.ident("column name")
	if err != nil {
		return ColumnDef{}, err
	}
	c := ColumnDef{Name: name}
	if err := p.columnType(&c); err != nil {
		return ColumnDef{}, err
	}
	for {
		switch {
		case p.acceptKeyword("PRIMARY", "KEY"):
			c.PrimaryKey = true
		case p.acceptKeyword("NOT", "NULL"):
			c.NotNull = true
		case p.acceptKeyword("UNIQUE"):
			// gopgql does not emit inline UNIQUE yet (M6); tolerate it so the
			// grammar widens without a rewrite.
		case p.acceptKeyword("DEFAULT"):
			c.Default = p.defaultExpr()
		case p.acceptKeyword("REFERENCES"):
			ref, err := p.reference()
			if err != nil {
				return ColumnDef{}, err
			}
			c.References = ref
		default:
			return c, nil
		}
	}
}

// columnType reads the type of a column: one or more bare words (e.g. "double
// precision"), an optional parenthesised modifier ("numeric(10, 2)"), and an
// optional "[]" array marker. The type text is taken verbatim from the source so
// multi-word and modified types round-trip exactly.
func (p *parser) columnType(c *ColumnDef) error {
	t := p.peek()
	if t.Type != TokenWord || columnConstraintWords[strings.ToUpper(t.Value)] {
		return p.errorf("expected column type, got %q", t.Value)
	}
	start := t.Pos
	end := t.End
	p.advance()
	for p.at(TokenWord) && !columnConstraintWords[strings.ToUpper(p.peek().Value)] {
		end = p.peek().End
		p.advance()
	}
	if p.at(TokenLParen) {
		// A parenthesised type modifier, e.g. numeric(10, 2). Absorb the whole
		// balanced group into the type text.
		end = p.consumeBalanced()
	}
	c.Type = normalizeSpaces(p.src[start:end])
	if p.acceptPunct(TokenLBracket) {
		if err := p.expectPunct(TokenRBracket, "]"); err != nil {
			return err
		}
		c.Array = true
	}
	return nil
}

// defaultExpr captures the raw text of a DEFAULT expression: everything from the
// current token up to the next top-level column boundary — a comma or the
// closing ')' of the column list, or another column constraint keyword.
func (p *parser) defaultExpr() string {
	start := p.peek().Pos
	end := start
	depth := 0
	for {
		t := p.peek()
		if depth == 0 {
			if t.Type == TokenComma || t.Type == TokenRParen ||
				t.Type == TokenSemicolon || t.Type == TokenEOF {
				break
			}
			if t.Type == TokenWord && columnConstraintWords[strings.ToUpper(t.Value)] {
				break
			}
		}
		switch t.Type {
		case TokenLParen:
			depth++
		case TokenRParen:
			depth--
		}
		end = t.End
		p.advance()
	}
	return normalizeSpaces(p.src[start:end])
}

func (p *parser) reference() (*Reference, error) {
	table, err := p.ident("referenced table")
	if err != nil {
		return nil, err
	}
	if err := p.expectPunct(TokenLParen, "("); err != nil {
		return nil, err
	}
	col, err := p.ident("referenced column")
	if err != nil {
		return nil, err
	}
	if err := p.expectPunct(TokenRParen, ")"); err != nil {
		return nil, err
	}
	return &Reference{Table: table, Column: col}, nil
}

// tableConstraintWords begins a table-level constraint item in a column list.
var tableConstraintWords = map[string]bool{
	"PRIMARY": true, "FOREIGN": true, "UNIQUE": true, "CHECK": true, "CONSTRAINT": true,
}

func (p *parser) isTableConstraintStart() bool {
	t := p.peek()
	return t.Type == TokenWord && tableConstraintWords[strings.ToUpper(t.Value)]
}

// tableConstraint parses a table-level constraint item. gopgql emits only
// "PRIMARY KEY (a, b)" on edge tables; for that the column list is captured. Any
// other constraint kind is recognised by its leading keyword and its balanced
// parenthesised body (if any) is skipped — enough to keep parsing the rest of
// the table.
func (p *parser) tableConstraint() (TableConstraint, error) {
	var words []string
	for p.at(TokenWord) && !p.at(TokenLParen) {
		w := strings.ToUpper(p.peek().Value)
		words = append(words, w)
		p.advance()
		if w == "KEY" || w == "UNIQUE" || w == "CHECK" {
			break
		}
	}
	c := TableConstraint{Kind: strings.Join(words, " ")}
	if p.at(TokenLParen) {
		if c.Kind == "PRIMARY KEY" || c.Kind == "UNIQUE" {
			cols, err := p.parenIdentList()
			if err != nil {
				return TableConstraint{}, err
			}
			c.Columns = cols
		} else {
			p.consumeBalanced()
		}
	}
	return c, nil
}

func (p *parser) createIndex() (Statement, error) {
	name, err := p.ident("index name")
	if err != nil {
		return nil, err
	}
	if err := p.expectKeyword("ON"); err != nil {
		return nil, err
	}
	table, err := p.ident("indexed table")
	if err != nil {
		return nil, err
	}
	cols, err := p.parenIdentList()
	if err != nil {
		return nil, err
	}
	return &CreateIndexStmt{Name: name, Table: table, Columns: cols}, nil
}

func (p *parser) alterTable() (Statement, error) {
	name, err := p.ident("table name")
	if err != nil {
		return nil, err
	}
	switch {
	case p.acceptKeyword("ADD", "COLUMN"):
		c, err := p.columnDef()
		if err != nil {
			return nil, err
		}
		return &AlterTableStmt{Name: name, Action: &AddColumn{Column: c}}, nil
	case p.acceptKeyword("DROP", "COLUMN"):
		col, err := p.ident("column name")
		if err != nil {
			return nil, err
		}
		return &AlterTableStmt{Name: name, Action: &DropColumn{Name: col}}, nil
	default:
		return nil, p.errorf("unsupported ALTER TABLE action at %q", p.peek().Value)
	}
}

func (p *parser) dropTable() (Statement, error) {
	ifExists := p.acceptKeyword("IF", "EXISTS")
	name, err := p.ident("table name")
	if err != nil {
		return nil, err
	}
	return &DropTableStmt{Name: name, IfExists: ifExists}, nil
}

func (p *parser) dropIndex() (Statement, error) {
	ifExists := p.acceptKeyword("IF", "EXISTS")
	name, err := p.ident("index name")
	if err != nil {
		return nil, err
	}
	return &DropIndexStmt{Name: name, IfExists: ifExists}, nil
}

func (p *parser) dropPropertyGraph() (Statement, error) {
	ifExists := p.acceptKeyword("IF", "EXISTS")
	name, err := p.ident("graph name")
	if err != nil {
		return nil, err
	}
	return &DropPropertyGraphStmt{Name: name, IfExists: ifExists}, nil
}

func (p *parser) createPropertyGraph() (Statement, error) {
	name, err := p.ident("graph name")
	if err != nil {
		return nil, err
	}
	st := &CreatePropertyGraphStmt{Name: name}
	if err := p.expectKeyword("VERTEX", "TABLES"); err != nil {
		return nil, err
	}
	if err := p.expectPunct(TokenLParen, "("); err != nil {
		return nil, err
	}
	for {
		v, err := p.vertexTableDef()
		if err != nil {
			return nil, err
		}
		st.Vertices = append(st.Vertices, v)
		if p.acceptPunct(TokenComma) {
			continue
		}
		break
	}
	if err := p.expectPunct(TokenRParen, ")"); err != nil {
		return nil, err
	}
	if p.acceptKeyword("EDGE", "TABLES") {
		if err := p.expectPunct(TokenLParen, "("); err != nil {
			return nil, err
		}
		for {
			e, err := p.edgeTableDef()
			if err != nil {
				return nil, err
			}
			st.Edges = append(st.Edges, e)
			if p.acceptPunct(TokenComma) {
				continue
			}
			break
		}
		if err := p.expectPunct(TokenRParen, ")"); err != nil {
			return nil, err
		}
	}
	return st, nil
}

// vertexTableDef parses one VERTEX TABLES entry. A vertex table carries at
// least one LABEL clause and may carry several — the shared-label mapping of a
// GraphQL interface puts one label on each implementor's table (SPEC.md §7 →
// M4) — so every further clause is read into ExtraLabels.
func (p *parser) vertexTableDef() (VertexTableDef, error) {
	var v VertexTableDef
	var err error
	if v.Table, err = p.ident("vertex table"); err != nil {
		return v, err
	}
	first, err := p.labelClause("vertex label")
	if err != nil {
		return v, err
	}
	v.Label, v.Properties = first.Label, first.Properties
	for p.keywordAt(0, "LABEL") {
		extra, err := p.labelClause("vertex label")
		if err != nil {
			return v, err
		}
		v.ExtraLabels = append(v.ExtraLabels, extra)
	}
	return v, nil
}

// labelClause parses "LABEL <label> PROPERTIES ( <properties> )".
func (p *parser) labelClause(what string) (LabelDef, error) {
	var l LabelDef
	var err error
	if err = p.expectKeyword("LABEL"); err != nil {
		return l, err
	}
	if l.Label, err = p.ident(what); err != nil {
		return l, err
	}
	if err = p.expectKeyword("PROPERTIES"); err != nil {
		return l, err
	}
	l.Properties, err = p.parenIdentList()
	return l, err
}

func (p *parser) edgeTableDef() (EdgeTableDef, error) {
	var e EdgeTableDef
	var err error
	if e.Table, err = p.ident("edge table"); err != nil {
		return e, err
	}
	if e.SourceKey, e.SourceTable, e.SourceRef, err = p.keyReference("SOURCE"); err != nil {
		return e, err
	}
	if e.DestKey, e.DestTable, e.DestRef, err = p.keyReference("DESTINATION"); err != nil {
		return e, err
	}
	label, err := p.labelClause("edge label")
	if err != nil {
		return e, err
	}
	e.Label, e.Properties = label.Label, label.Properties
	return e, nil
}

// keyReference parses "<which> KEY (<key>) REFERENCES <table> (<ref>)".
func (p *parser) keyReference(which string) (key, table, ref string, err error) {
	if err = p.expectKeyword(which, "KEY"); err != nil {
		return
	}
	if err = p.expectPunct(TokenLParen, "("); err != nil {
		return
	}
	if key, err = p.ident("key column"); err != nil {
		return
	}
	if err = p.expectPunct(TokenRParen, ")"); err != nil {
		return
	}
	if err = p.expectKeyword("REFERENCES"); err != nil {
		return
	}
	if table, err = p.ident("referenced table"); err != nil {
		return
	}
	if err = p.expectPunct(TokenLParen, "("); err != nil {
		return
	}
	if ref, err = p.ident("referenced column"); err != nil {
		return
	}
	err = p.expectPunct(TokenRParen, ")")
	return
}

// parenIdentList parses "( id1, id2, ... )" and returns the identifiers.
func (p *parser) parenIdentList() ([]string, error) {
	if err := p.expectPunct(TokenLParen, "("); err != nil {
		return nil, err
	}
	var out []string
	for {
		id, err := p.ident("identifier")
		if err != nil {
			return nil, err
		}
		out = append(out, id)
		if p.acceptPunct(TokenComma) {
			continue
		}
		break
	}
	if err := p.expectPunct(TokenRParen, ")"); err != nil {
		return nil, err
	}
	return out, nil
}

// consumeBalanced consumes a parenthesised group starting at the current '(' and
// returns the source offset just past its matching ')'. The current token must
// be '('.
func (p *parser) consumeBalanced() int {
	depth := 0
	end := p.peek().End
	for !p.at(TokenEOF) {
		t := p.advance()
		end = t.End
		switch t.Type {
		case TokenLParen:
			depth++
		case TokenRParen:
			depth--
			if depth == 0 {
				return end
			}
		}
	}
	return end
}

// normalizeSpaces collapses internal whitespace runs to single spaces and trims
// the ends, so verbatim source spans compare stably.
func normalizeSpaces(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
