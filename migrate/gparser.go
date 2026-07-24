package migrate

import (
	"fmt"
	"strings"
)

// tokenize splits a normalised SQL statement into tokens: the delimiters '(',
// ')' and ',' each become their own token, double-quoted identifiers are
// unquoted into a single token, and every other whitespace-separated run is one
// token. It backs the positional property-graph parser.
func tokenize(s string) []string {
	var toks []string
	i := 0
	for i < len(s) {
		c := s[i]
		switch {
		case isSpace(c):
			i++
		case c == '(' || c == ')' || c == ',':
			toks = append(toks, string(c))
			i++
		case c == '"':
			var b strings.Builder
			i++
			for i < len(s) {
				if s[i] == '"' {
					if i+1 < len(s) && s[i+1] == '"' {
						b.WriteByte('"')
						i += 2
						continue
					}
					i++
					break
				}
				b.WriteByte(s[i])
				i++
			}
			toks = append(toks, b.String())
		default:
			start := i
			for i < len(s) && !isSpace(s[i]) && !isDelim(s[i]) {
				i++
			}
			toks = append(toks, s[start:i])
		}
	}
	return toks
}

// gparser is a positional cursor over a tokenised statement.
type gparser struct {
	toks []string
	i    int
}

func (p *gparser) peek() string {
	if p.i < len(p.toks) {
		return p.toks[p.i]
	}
	return ""
}

func (p *gparser) next() string {
	t := p.peek()
	if p.i < len(p.toks) {
		p.i++
	}
	return t
}

// expect consumes the next token for each word, requiring a case-insensitive
// match. Punctuation words ("(", ")", ",") match literally.
func (p *gparser) expect(words ...string) error {
	for _, w := range words {
		got := p.next()
		if !strings.EqualFold(got, w) {
			return fmt.Errorf("expected %q, got %q", w, got)
		}
	}
	return nil
}

// accept consumes the next token if it equals tok (case-insensitive) and
// reports whether it did.
func (p *gparser) accept(tok string) bool {
	if strings.EqualFold(p.peek(), tok) {
		p.i++
		return true
	}
	return false
}

// acceptWord is accept for a keyword; it is a synonym kept for readability at
// call sites that consume optional keywords.
func (p *gparser) acceptWord(w string) bool { return p.accept(w) }

// ident consumes and returns the next token as an identifier, erroring if it is
// a structural delimiter or the stream is exhausted.
func (p *gparser) ident() (string, error) {
	t := p.next()
	if t == "" || t == "(" || t == ")" || t == "," {
		return "", fmt.Errorf("expected identifier, got %q", t)
	}
	return t, nil
}

// keyList reads a comma-separated identifier list and consumes the closing
// ')'. The opening '(' must already be consumed.
func (p *gparser) keyList() ([]string, error) {
	var out []string
	for {
		id, err := p.ident()
		if err != nil {
			return nil, err
		}
		out = append(out, id)
		if p.accept(",") {
			continue
		}
		break
	}
	if err := p.expect(")"); err != nil {
		return nil, err
	}
	return out, nil
}

func (p *gparser) vertexEntry() (vertexEntry, error) {
	var v vertexEntry
	var err error
	if v.table, err = p.ident(); err != nil {
		return v, err
	}
	if err = p.expect("LABEL"); err != nil {
		return v, err
	}
	if v.label, err = p.ident(); err != nil {
		return v, err
	}
	if err = p.expect("PROPERTIES", "("); err != nil {
		return v, err
	}
	if v.properties, err = p.keyList(); err != nil {
		return v, err
	}
	return v, nil
}

func (p *gparser) edgeEntry() (edgeEntry, error) {
	var e edgeEntry
	var err error
	if e.table, err = p.ident(); err != nil {
		return e, err
	}
	if e.sourceKey, e.sourceTable, e.sourceRef, err = p.keyReferences("SOURCE"); err != nil {
		return e, err
	}
	if e.destKey, e.destTable, e.destRef, err = p.keyReferences("DESTINATION"); err != nil {
		return e, err
	}
	if err = p.expect("LABEL"); err != nil {
		return e, err
	}
	if e.label, err = p.ident(); err != nil {
		return e, err
	}
	if err = p.expect("PROPERTIES", "("); err != nil {
		return e, err
	}
	if e.properties, err = p.keyList(); err != nil {
		return e, err
	}
	return e, nil
}

// keyReferences parses "<which> KEY (<key>) REFERENCES <table> (<ref>)".
func (p *gparser) keyReferences(which string) (key, table, ref string, err error) {
	if err = p.expect(which, "KEY", "("); err != nil {
		return
	}
	if key, err = p.ident(); err != nil {
		return
	}
	if err = p.expect(")", "REFERENCES"); err != nil {
		return
	}
	if table, err = p.ident(); err != nil {
		return
	}
	if err = p.expect("("); err != nil {
		return
	}
	if ref, err = p.ident(); err != nil {
		return
	}
	err = p.expect(")")
	return
}
