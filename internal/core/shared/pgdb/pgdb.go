// Package pgdb provides the PostgreSQL connection pool and the embedded-SQL
// migration runner shared by all canonical stores (ADR-0018).
package pgdb

import (
	"context"
	"fmt"
	"io/fs"
	"sort"
	"strings"

	"database/sql"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
)

// Open creates a connection pool for dsn and verifies reachability with a
// ping. The caller owns the pool and closes it on shutdown.
func Open(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("connect to postgres: %w", err)
	}
	return pool, nil
}

// Migrate applies every pending *.sql file in fsys in lexicographic order,
// each in its own transaction. Applied files are recorded per namespace in
// schema_migrations so each package can own and evolve its tables
// independently.
func Migrate(ctx context.Context, pool *pgxpool.Pool, namespace string, fsys fs.FS) error {
	if _, err := pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		namespace  text        NOT NULL,
		name       text        NOT NULL,
		applied_at timestamptz NOT NULL DEFAULT now(),
		PRIMARY KEY (namespace, name)
	)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	applied := map[string]bool{}
	rows, err := pool.Query(ctx, `SELECT name FROM schema_migrations WHERE namespace = $1`, namespace)
	if err != nil {
		return fmt.Errorf("list applied migrations: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return fmt.Errorf("scan applied migration: %w", err)
		}
		applied[name] = true
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read applied migrations: %w", err)
	}

	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return fmt.Errorf("read migrations dir: %w", err)
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	for _, name := range names {
		if applied[name] {
			continue
		}
		body, err := fs.ReadFile(fsys, name)
		if err != nil {
			return fmt.Errorf("read migration %s: %w", name, err)
		}
		tx, err := pool.Begin(ctx)
		if err != nil {
			return fmt.Errorf("begin migration %s: %w", name, err)
		}
		if _, err := tx.Exec(ctx, string(body)); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("apply migration %s: %w", name, err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO schema_migrations (namespace, name) VALUES ($1, $2)`,
			namespace, name); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("record migration %s: %w", name, err)
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit migration %s: %w", name, err)
		}
	}
	return nil
}

// DB wraps *sql.DB so every statement is passed through Rebind first when
// the backend is Postgres. Stores keep writing '?'-style SQL for SQLite and
// work unchanged under pg.
type DB struct {
	inner *sql.DB
	pg    bool
}

// NewDB wraps db; isPG selects whether Rebind is applied.
func NewDB(db *sql.DB, isPG bool) *DB { return &DB{inner: db, pg: isPG} }

// NewPGDB adapts a pgx pool to a rebound database/sql handle. Close on the
// returned handle is a no-op — the pool owns the connections.
func NewPGDB(pool *pgxpool.Pool) *DB {
	return &DB{inner: stdlib.OpenDBFromPool(pool), pg: true}
}

func (d *DB) Exec(query string, args ...any) (sql.Result, error) {
	return d.inner.Exec(Rebind(query, d.pg), args...)
}

func (d *DB) Query(query string, args ...any) (*sql.Rows, error) {
	return d.inner.Query(Rebind(query, d.pg), args...)
}

func (d *DB) QueryRow(query string, args ...any) *sql.Row {
	return d.inner.QueryRow(Rebind(query, d.pg), args...)
}

func (d *DB) Prepare(query string) (*sql.Stmt, error) {
	return d.inner.Prepare(Rebind(query, d.pg))
}

func (d *DB) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return d.inner.ExecContext(ctx, Rebind(query, d.pg), args...)
}

func (d *DB) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return d.inner.QueryContext(ctx, Rebind(query, d.pg), args...)
}

func (d *DB) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return d.inner.QueryRowContext(ctx, Rebind(query, d.pg), args...)
}

func (d *DB) Begin() (*Tx, error) {
	tx, err := d.inner.Begin()
	if err != nil {
		return nil, err
	}
	return &Tx{Tx: tx, pg: d.pg}, nil
}

func (d *DB) BeginTx(ctx context.Context, opts *sql.TxOptions) (*Tx, error) {
	tx, err := d.inner.BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	return &Tx{Tx: tx, pg: d.pg}, nil
}

// Inner exposes the wrapped handle for SQLite-specific call sites that must
// bypass Rebind (e.g. PRAGMA).
func (d *DB) Inner() *sql.DB { return d.inner }

// Close releases the wrapped handle. For a pg pool-backed handle this is a
// no-op — the pool owns the connections.
func (d *DB) Close() error { return d.inner.Close() }

// Tx wraps *sql.Tx with the same Rebind treatment.
type Tx struct {
	*sql.Tx
	pg bool
}

func (t *Tx) Exec(query string, args ...any) (sql.Result, error) {
	return t.Tx.Exec(Rebind(query, t.pg), args...)
}

func (t *Tx) Query(query string, args ...any) (*sql.Rows, error) {
	return t.Tx.Query(Rebind(query, t.pg), args...)
}

func (t *Tx) QueryRow(query string, args ...any) *sql.Row {
	return t.Tx.QueryRow(Rebind(query, t.pg), args...)
}

func (t *Tx) Prepare(query string) (*sql.Stmt, error) {
	return t.Tx.Prepare(Rebind(query, t.pg))
}

func (t *Tx) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return t.Tx.ExecContext(ctx, Rebind(query, t.pg), args...)
}

func (t *Tx) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return t.Tx.QueryContext(ctx, Rebind(query, t.pg), args...)
}

func (t *Tx) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return t.Tx.QueryRowContext(ctx, Rebind(query, t.pg), args...)
}

// Rebind converts '?' placeholders to PostgreSQL '$N' when isPG is true;
// otherwise it returns query unchanged. Question marks inside string
// literals are not supported by this helper — stores must not embed them.
func Rebind(query string, isPG bool) string {
	if !isPG {
		return query
	}
	var b strings.Builder
	b.Grow(len(query) + 8)
	n := 0
	for _, r := range query {
		if r == '?' {
			n++
			fmt.Fprintf(&b, "$%d", n)
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}
