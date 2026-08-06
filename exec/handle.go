package exec

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The handle a caller hands gopgql, and why it is not a pgx type.
//
// gopgql runs every write and every caller-scoped read through a handle the
// caller owns (see the package doc). Until gopgql#53 that handle was pgx-shaped:
// [Handle] required Exec to return a pgconn.CommandTag and Query to return
// pgx.Rows, because *pgxpool.Pool, *pgx.Conn and pgx.Tx are what a caller
// realistically had.
//
// That stopped being true. A caller that runs gopgql inside a DBOS transaction
// has a dbos.Tx, which is sysdb.Tx — a *portable* interface as of DBOS v1.0.0,
// whose Exec returns sysdb.Result and whose Query returns sysdb.Rows. Go matches
// interfaces on exact method signatures, not on shape, so no amount of
// structural similarity makes it satisfy a pgx-typed Handle: the transaction
// simply could not reach a generated client, on the read path or the write path,
// and exactly-once appends depend on it reaching both.
//
// The fix cannot be "gopgql speaks DBOS". Every generated client would then
// carry a DBOS dependency, including one whose schema has nothing to do with
// workflows. So the interfaces below name types gopgql owns, and two adapters
// carry the two worlds into them:
//
//   - [Pgx] / [PgxQuerier] wrap the pgx handles, which are structurally close
//     but not identical (pgx.Rows.Close returns nothing, and a
//     pgconn.CommandTag's RowsAffected returns no error).
//   - [Portable] / [PortableQuerier] wrap anything already driver-agnostic. They
//     are generic over the concrete cursor and result types, which is what lets
//     dbos.Tx through without gopgql naming DBOS: `exec.Portable(tx)` infers
//     both from the argument.
//
// This is a breaking change to exec's exported surface, and deliberately so. The
// alternative — a second, parallel set of portable entry points beside the pgx
// ones — would double every signature in exec and in every generated client, to
// preserve a call that is one word shorter.

// Cursor is a result set being read, in the smallest form every driver offers.
//
// It is deliberately the intersection rather than the union: Next, Scan, Err and
// Close are what database/sql, pgx and DBOS's sysdb.Rows all provide with these
// exact signatures. Anything richer — the field descriptions pgx has, the raw
// Values it can decode a row into — is not available from a portable cursor, and
// asking for it here would put the interface back out of DBOS's reach.
type Cursor interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
	Close() error
}

// NamedCursor is a [Cursor] that can report its own output column names.
//
// The pgx adapter is one, because pgx.Rows carries field descriptions; a
// portable cursor is not. Where a cursor cannot say, the column names come from
// the compiled query, which recorded them when it emitted the SELECT list
// (compiler.Compiled.Columns). Where neither can say, the read fails saying so —
// a flat row keyed by guessed names is the silent wrong answer SPEC.md §10
// forbids.
type NamedCursor interface {
	Cursor
	Columns() []string
}

// Tag is the outcome of a statement that returns no rows, in the form every
// driver offers: how many rows it affected.
//
// The error is part of the signature because database/sql's Result has one and
// DBOS mirrors database/sql. pgx does not — a pgconn.CommandTag counts rows
// without failing — so [Pgx]'s adapter supplies a nil error, which is honest:
// the count was already known.
type Tag interface {
	RowsAffected() (int64, error)
}

// Querier is a handle gopgql can read through.
//
// It stays separate from [Handle] even though Handle embeds it: a read needs no
// Exec, and the smaller interface asks less of a caller who only wants to run a
// compiled query — mcp and conform take this one.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (Cursor, error)
}

// Handle is a database handle the caller owns and hands to gopgql for the
// duration of one operation: it queries *and* it executes.
//
// A caller inside a transaction of its own passes that transaction through and
// the statement runs in it — the property the caller cannot get from a package
// that opens its own connection, because a checkpoint committed elsewhere is a
// checkpoint that can disagree with the work.
//
// Exec is here rather than in [Querier] because a `@function` declared
// `returns: VOID` has no result set to read: [Call] runs it as a statement and
// reads the tag (SPEC.md §7 → M11). Without that path Handle would carry a
// method nothing uses, and it would collapse back into Querier.
type Handle interface {
	Querier
	Exec(ctx context.Context, sql string, args ...any) (Tag, error)
}

// pgxQuerier and pgxHandle are the pgx-shaped handles [Pgx] and [PgxQuerier]
// accept. They are interfaces rather than a type switch so that a pgx handle
// gopgql has never heard of — a wrapper carrying a tracer, a pool of pools —
// works for free.
type (
	pgxQuerier interface {
		Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	}
	pgxHandle interface {
		pgxQuerier
		Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	}
)

// The three pgx types a caller realistically has in hand are all accepted by
// [Pgx]. The assertions are here so a pgx upgrade that moves one of those
// signatures fails the build, with the type named, rather than a test somewhere
// downstream.
var (
	_ pgxHandle = (*pgxpool.Pool)(nil)
	_ pgxHandle = (*pgx.Conn)(nil)
	_ pgxHandle = (pgx.Tx)(nil)
)

// Pgx adapts a pgx pool, connection or transaction to [Handle].
//
//	rows, err := exec.Query(ctx, exec.Pgx(pool), cq)
func Pgx(h pgxHandle) Handle {
	if h == nil {
		return nil
	}
	return pgxAdapter{h: h}
}

// PgxQuerier adapts a pgx pool, connection or transaction to [Querier], for a
// caller that only reads.
func PgxQuerier(q pgxQuerier) Querier {
	if q == nil {
		return nil
	}
	return pgxAdapter{q: q}
}

// pgxAdapter carries a pgx handle as a [Handle]. q is always set; h is set only
// when the handle can execute, so a read-only pgx handle adapted with
// [PgxQuerier] cannot be widened into a writable one by an assertion.
type pgxAdapter struct {
	q pgxQuerier
	h pgxHandle
}

func (a pgxAdapter) querier() pgxQuerier {
	if a.h != nil {
		return a.h
	}
	return a.q
}

func (a pgxAdapter) Query(ctx context.Context, sql string, args ...any) (Cursor, error) {
	rows, err := a.querier().Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	return pgxCursor{rows: rows}, nil
}

func (a pgxAdapter) Exec(ctx context.Context, sql string, args ...any) (Tag, error) {
	if a.h == nil {
		return nil, fmt.Errorf("exec: this handle was adapted read-only with PgxQuerier; " +
			"adapt it with Pgx to execute a statement")
	}
	tag, err := a.h.Exec(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	return commandTag(tag), nil
}

// pgxCursor carries pgx.Rows as a [NamedCursor]. Only two things differ: Close
// reports no error, and the field descriptions become column names.
type pgxCursor struct{ rows pgx.Rows }

func (c pgxCursor) Next() bool             { return c.rows.Next() }
func (c pgxCursor) Scan(dest ...any) error { return c.rows.Scan(dest...) }
func (c pgxCursor) Err() error             { return c.rows.Err() }
func (c pgxCursor) Close() error           { c.rows.Close(); return nil }

func (c pgxCursor) Columns() []string {
	fds := c.rows.FieldDescriptions()
	out := make([]string, len(fds))
	for i, fd := range fds {
		out[i] = fd.Name
	}
	return out
}

// commandTag carries a pgconn.CommandTag as a [Tag].
type commandTag pgconn.CommandTag

func (t commandTag) RowsAffected() (int64, error) {
	return pgconn.CommandTag(t).RowsAffected(), nil
}

// portableHandle and portableQuerier describe a driver handle that is already
// driver-agnostic, generically over whatever concrete cursor and result types it
// returns. dbos.Tx (sysdb.Tx) is one; so is any hand-written handle shaped like
// database/sql.
type (
	portableQuerier[R Cursor] interface {
		Query(ctx context.Context, sql string, args ...any) (R, error)
	}
	portableHandle[R Cursor, T Tag] interface {
		portableQuerier[R]
		Exec(ctx context.Context, sql string, args ...any) (T, error)
	}
)

// Portable adapts a driver-agnostic handle to [Handle].
//
// The type parameters are inferred from the argument, so a DBOS transaction goes
// through unannotated:
//
//	dbos.RunAsTransaction(ctx, ds, func(ctx context.Context, tx dbos.Tx) (T, error) {
//	    return client.AppendEvent(ctx, exec.Portable(tx), in)
//	})
//
// and gopgql names no DBOS type to make that work, which is the whole point: a
// generated client whose schema has nothing to do with workflows does not gain a
// workflow dependency.
func Portable[R Cursor, T Tag](h portableHandle[R, T]) Handle {
	if h == nil {
		return nil
	}
	return portableAdapter[R, T]{h: h}
}

// PortableQuerier adapts a driver-agnostic handle to [Querier], for a caller that
// only reads. dbos.Tx satisfies it too — Querier is a subset of what Portable
// accepts, not a different kind of handle.
func PortableQuerier[R Cursor](q portableQuerier[R]) Querier {
	if q == nil {
		return nil
	}
	return portableReader[R]{q: q}
}

type portableReader[R Cursor] struct{ q portableQuerier[R] }

func (a portableReader[R]) Query(ctx context.Context, sql string, args ...any) (Cursor, error) {
	rows, err := a.q.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	return rows, nil
}

type portableAdapter[R Cursor, T Tag] struct{ h portableHandle[R, T] }

func (a portableAdapter[R, T]) Query(ctx context.Context, sql string, args ...any) (Cursor, error) {
	rows, err := a.h.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	return rows, nil
}

func (a portableAdapter[R, T]) Exec(ctx context.Context, sql string, args ...any) (Tag, error) {
	tag, err := a.h.Exec(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	return tag, nil
}
