package settings

import (
	"context"
	"embed"
	"fmt"
	"io/fs"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/perber/wiki/internal/core/shared/pgdb"
)

//go:embed migrations
var migrations embed.FS

// NewPG builds a Store backed by a row of the settings table in Postgres
// (ADR-0018): one row per named store, value held as jsonb. Semantics match
// the file store: a missing row is not an error, Get returns zero until the
// first Put. The table is created by the package-owned migration, run here
// on first use.
func NewPG[T any](ctx context.Context, pool *pgxpool.Pool, name string, zero T, onLoad func(*T)) (*Store[T], error) {
	migs, err := fs.Sub(migrations, "migrations")
	if err != nil {
		return nil, fmt.Errorf("settings migrations: %w", err)
	}
	if err := pgdb.Migrate(ctx, pool, "settings", migs); err != nil {
		return nil, fmt.Errorf("settings migrations: %w", err)
	}
	s := &Store[T]{
		ctx:    ctx,
		pg:     pool,
		pgName: name,
		onLoad: onLoad,
		val:    zero,
	}
	if err := s.reloadLocked(); err != nil {
		return nil, err
	}
	return s, nil
}
