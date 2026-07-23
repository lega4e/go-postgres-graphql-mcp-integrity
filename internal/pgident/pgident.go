// Package pgident renders PostgreSQL identifiers safely.
//
// Identifiers in gopgql never come from user data — they are table, column,
// label and graph names drawn from the SDL directive model, an allowlist. This
// package quotes an identifier when it collides with a reserved word or is not
// a plain lowercase identifier, and otherwise leaves it bare so generated DDL
// stays readable (SPEC.md §5.2 shows unquoted identifiers). Quoting doubles any
// embedded quote, matching the semantics of pgx.Identifier.Sanitize without
// importing pgx — keeping the sdl/generator/compiler packages free of a
// database dependency so they compile to WASM (SPEC.md §4.1).
package pgident

import (
	"regexp"
	"strings"
)

// safeRe matches identifiers that need no quoting: a lowercase letter or
// underscore followed by lowercase letters, digits or underscores.
var safeRe = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

// reserved is the set of SQL keywords that must be double-quoted when used as
// identifiers (SPEC.md §5.3 invariant 3). PostgreSQL's reserved-word list is
// long; this covers the words a schema is realistically likely to collide with.
var reserved = map[string]bool{
	"all": true, "analyse": true, "analyze": true, "and": true, "any": true,
	"array": true, "as": true, "asc": true, "asymmetric": true, "both": true,
	"case": true, "cast": true, "check": true, "collate": true, "column": true,
	"constraint": true, "create": true, "current_catalog": true, "current_date": true,
	"current_role": true, "current_time": true, "current_timestamp": true,
	"current_user": true, "default": true, "deferrable": true, "desc": true,
	"distinct": true, "do": true, "else": true, "end": true, "except": true,
	"false": true, "fetch": true, "for": true, "foreign": true, "from": true,
	"grant": true, "graph": true, "group": true, "having": true, "in": true,
	"index": true, "initially": true, "intersect": true, "into": true, "key": true,
	"lateral": true, "leading": true, "limit": true, "localtime": true,
	"localtimestamp": true, "not": true, "null": true, "offset": true, "on": true,
	"only": true, "or": true, "order": true, "placing": true, "primary": true,
	"references": true, "returning": true, "select": true, "session_user": true,
	"some": true, "symmetric": true, "table": true, "then": true, "to": true,
	"trailing": true, "true": true, "union": true, "unique": true, "user": true,
	"using": true, "variadic": true, "when": true, "where": true, "window": true,
	"with": true,
}

// Quote returns s quoted for use as a PostgreSQL identifier when it is a
// reserved word or not a plain lowercase identifier; otherwise it returns s
// unchanged.
func Quote(s string) string {
	if reserved[strings.ToLower(s)] || !safeRe.MatchString(s) {
		return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
	}
	return s
}

// NeedsQuote reports whether Quote would alter s.
func NeedsQuote(s string) bool {
	return reserved[strings.ToLower(s)] || !safeRe.MatchString(s)
}
