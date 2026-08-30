package exec

import (
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestReadOnlyDSNReachesRuntimeParams is the belt's regression test.
//
// It asserts what actually matters rather than the string ReadOnlyDSN returns:
// that pgx, parsing the result, puts default_transaction_read_only=on into the
// runtime parameters it sends at connection time. The parameter used to be
// written onto a parsed configuration by hand; it now travels in the DSN,
// because the pool is built by goga/database/pgxdb from a DSN alone. This test
// is what says the two spellings are equivalent, in every DSN form pgx accepts.
func TestReadOnlyDSNReachesRuntimeParams(t *testing.T) {
	tests := []struct {
		name string
		dsn  string
	}{
		{"url", "postgres://u:p@localhost:5432/db?sslmode=disable"},
		{"url alternate scheme", "postgresql://u:p@localhost:5432/db"},
		{"url with no query", "postgres://u:p@localhost:5432/db"},
		{"keyword value", "host=localhost port=5432 user=u password=p dbname=db sslmode=disable"},
		{"empty", ""},
		{"url already read-write", "postgres://u:p@localhost:5432/db?default_transaction_read_only=off"},
		{"keyword value already read-write", "host=localhost dbname=db default_transaction_read_only=off"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := pgxpool.ParseConfig(ReadOnlyDSN(tt.dsn))
			require.NoError(t, err)
			assert.Equal(t, "on", cfg.ConnConfig.RuntimeParams[readOnlyParam],
				"every session on this pool must start read-only; SPEC.md §3 design D4")
		})
	}
}

// TestReadOnlyDSNKeepsEverythingElse checks that adding the parameter is all
// that changes: a rewritten URL still reaches the same database as the same
// user, and the pool sizing a caller put in the DSN survives.
func TestReadOnlyDSNKeepsEverythingElse(t *testing.T) {
	cfg, err := pgxpool.ParseConfig(
		ReadOnlyDSN("postgres://alice:secret@db.example:6432/graph?sslmode=disable&pool_max_conns=7"))
	require.NoError(t, err)

	assert.Equal(t, "db.example", cfg.ConnConfig.Host)
	assert.Equal(t, uint16(6432), cfg.ConnConfig.Port)
	assert.Equal(t, "alice", cfg.ConnConfig.User)
	assert.Equal(t, "secret", cfg.ConnConfig.Password)
	assert.Equal(t, "graph", cfg.ConnConfig.Database)
	assert.Equal(t, int32(7), cfg.MaxConns)
}

// TestReadOnlyDSNSurvivesAnEncodedValue checks that rewriting the query does
// not mangle a value that needs escaping.
//
// Rewriting a URL means re-encoding its whole query, so a parameter whose value
// carries an '=' or a space has to come back out the way it went in. This is
// the case that would break silently: a search_path that lost its escaping
// would connect fine and read the wrong tables.
func TestReadOnlyDSNSurvivesAnEncodedValue(t *testing.T) {
	cfg, err := pgxpool.ParseConfig(
		ReadOnlyDSN("postgres://u:p@localhost:5432/db?options=-c%20search_path%3Dgraph%2Cpublic"))
	require.NoError(t, err)

	assert.Equal(t, "-c search_path=graph,public", cfg.ConnConfig.RuntimeParams["options"])
	assert.Equal(t, "on", cfg.ConnConfig.RuntimeParams[readOnlyParam])
}

// TestReadOnlyDSNLeavesAnUnparseableURLAlone checks that a URL net/url refuses
// is handed to pgx as it stands, rather than being turned into some other
// connection string whose error names the wrong thing.
//
// A query string net/url cannot make sense of is not this case: pgx parses URL
// DSNs with url.Parse and url.Query() itself, so a pair dropped here is a pair
// pgx would have dropped anyway.
func TestReadOnlyDSNLeavesAnUnparseableURLAlone(t *testing.T) {
	const bad = "postgres://u:p@[::1:5432/db"
	require.Error(t, func() error { _, err := pgxpool.ParseConfig(bad); return err }(),
		"the fixture has to be a DSN pgx itself rejects, or this proves nothing")
	assert.Equal(t, bad, ReadOnlyDSN(bad))
}
