// Package ddl is a small lexer + recursive-descent parser + AST for the exact
// DDL dialect gopgql emits into its own goose migrations.
//
// # Why this exists
//
// Reconstructing prior state by folding gopgql's own migrations (SPEC.md §3
// decision 6, §7 → M2) means reading SQL back. The established Go SQL parsers —
// vitessio/vitess, xwb1989/sqlparser, pganalyze/pg_query_go, pgplex/pgparser —
// all share one shape: a lexer produces a typed token stream, a parser consumes
// it into a typed AST, and consumers walk that AST. This package adopts the same
// shape at a fraction of the surface area: a hand-written lexer, a
// recursive-descent parser, and a handful of statement nodes.
//
// It is deliberately NOT a general PostgreSQL parser. It understands precisely
// the statements gopgql writes — CREATE TABLE, ALTER TABLE ADD/DROP COLUMN,
// ALTER TABLE ADD/DROP CONSTRAINT, ALTER TABLE RENAME TO / RENAME COLUMN,
// ALTER TABLE ALTER COLUMN SET/DROP DEFAULT, CREATE/DROP INDEX, DROP TABLE, and
// CREATE/DROP PROPERTY GRAPH — and reports an error on anything else. Every one
// of those is here because the writer emits it: a statement the writer can emit
// and the reader cannot read is not a gap, it is a corrupted prior state (design
// D3). The emitter (generator + migrate) and this reader are
// two halves of one contract; keeping them as separate lex/parse/AST layers is
// what lets each grow a new statement by adding a node and a production rather
// than by threading another special case through string surgery.
package ddl

// TokenType classifies a lexical token.
type TokenType int

const (
	// TokenEOF marks the end of the input.
	TokenEOF TokenType = iota
	// TokenWord is a bare word: a keyword, a type name, or a bare identifier.
	// The lexer does not decide which; the parser interprets a word by position
	// (case-insensitively for keywords).
	TokenWord
	// TokenQuotedIdent is a double-quoted identifier, already unquoted (with ""
	// collapsed to ") into Value. It is always an identifier, never a keyword.
	TokenQuotedIdent
	// TokenString is a single-quoted string literal, unquoted into Value.
	TokenString
	// TokenLParen, TokenRParen, TokenComma, TokenSemicolon and the bracket pair
	// are the structural punctuation of the grammar.
	TokenLParen
	TokenRParen
	TokenComma
	TokenSemicolon
	TokenLBracket
	TokenRBracket
)

// Token is one lexical unit with its source span. Pos and End are byte offsets
// into the source, so a consumer can recover the exact original text of a run
// of tokens (used to preserve a DEFAULT expression verbatim).
type Token struct {
	Type  TokenType
	Value string
	Pos   int
	End   int
}

// IsIdent reports whether the token can stand as an identifier: a bare word or
// a quoted identifier. Structural punctuation and literals cannot.
func (t Token) IsIdent() bool {
	return t.Type == TokenWord || t.Type == TokenQuotedIdent
}
