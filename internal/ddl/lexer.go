package ddl

import (
	"fmt"
	"strings"
)

// lexer scans a DDL source string into a token slice. It is intentionally small:
// gopgql's DDL uses only bare words, double-quoted identifiers, the odd
// single-quoted string, numeric fragments (folded into words), and the
// structural punctuation "( ) , ; [ ]".
type lexer struct {
	src string
	pos int
}

// Lex tokenises src into a slice terminated by a single TokenEOF. It returns an
// error only for an unterminated quoted identifier or string.
func Lex(src string) ([]Token, error) {
	l := &lexer{src: src}
	var toks []Token
	for {
		t, err := l.next()
		if err != nil {
			return nil, err
		}
		toks = append(toks, t)
		if t.Type == TokenEOF {
			return toks, nil
		}
	}
}

func (l *lexer) next() (Token, error) {
	l.skipSpace()
	if l.pos >= len(l.src) {
		return Token{Type: TokenEOF, Pos: l.pos, End: l.pos}, nil
	}
	start := l.pos
	c := l.src[l.pos]
	switch c {
	case '(':
		return l.punct(TokenLParen), nil
	case ')':
		return l.punct(TokenRParen), nil
	case ',':
		return l.punct(TokenComma), nil
	case ';':
		return l.punct(TokenSemicolon), nil
	case '[':
		return l.punct(TokenLBracket), nil
	case ']':
		return l.punct(TokenRBracket), nil
	case '"':
		return l.quoted(start, '"', TokenQuotedIdent)
	case '\'':
		return l.quoted(start, '\'', TokenString)
	default:
		return l.word(start), nil
	}
}

func (l *lexer) punct(tt TokenType) Token {
	t := Token{Type: tt, Value: string(l.src[l.pos]), Pos: l.pos, End: l.pos + 1}
	l.pos++
	return t
}

// quoted scans a delimiter-quoted run, collapsing a doubled delimiter ("" or ”)
// into a single literal character, and returns the unquoted Value.
func (l *lexer) quoted(start int, delim byte, tt TokenType) (Token, error) {
	var b strings.Builder
	l.pos++ // opening delimiter
	for l.pos < len(l.src) {
		if l.src[l.pos] == delim {
			if l.pos+1 < len(l.src) && l.src[l.pos+1] == delim {
				b.WriteByte(delim)
				l.pos += 2
				continue
			}
			l.pos++ // closing delimiter
			return Token{Type: tt, Value: b.String(), Pos: start, End: l.pos}, nil
		}
		b.WriteByte(l.src[l.pos])
		l.pos++
	}
	return Token{}, fmt.Errorf("ddl: unterminated %s at offset %d", quoteName(delim), start)
}

// word scans a bare word up to the next whitespace or structural character. A
// word carries its original spelling; the parser upper-folds it when matching a
// keyword and takes it verbatim when it is an identifier or a type name.
func (l *lexer) word(start int) Token {
	for l.pos < len(l.src) && !isSpace(l.src[l.pos]) && !isStructural(l.src[l.pos]) {
		l.pos++
	}
	return Token{Type: TokenWord, Value: l.src[start:l.pos], Pos: start, End: l.pos}
}

func (l *lexer) skipSpace() {
	for l.pos < len(l.src) && isSpace(l.src[l.pos]) {
		l.pos++
	}
}

// isStructural reports whether b begins its own token, ending a bare word.
func isStructural(b byte) bool {
	switch b {
	case '(', ')', ',', ';', '[', ']', '"', '\'':
		return true
	default:
		return false
	}
}

func isSpace(b byte) bool { return b == ' ' || b == '\t' || b == '\n' || b == '\r' }

func quoteName(delim byte) string {
	if delim == '"' {
		return "quoted identifier"
	}
	return "string literal"
}
