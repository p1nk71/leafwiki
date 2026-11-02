package wiki

import (
	"embed"
	"fmt"
	"io/fs"

	"github.com/perber/wiki/internal/core/shared/pgdb"
)

//go:embed migrations
var pgMigrations embed.FS

// initPostgres opens the canonical PostgreSQL pool and runs pending schema
// migrations. When DatabaseURL is empty this is a no-op and the wiki keeps
// file/SQLite canonical storage (pre-ADR-0018 mode).
func (w *Wiki) initPostgres(options *WikiOptions) error {
	if options.DatabaseURL == "" {
		return nil
	}
	pool, err := pgdb.Open(w.shutdownCtx, options.DatabaseURL)
	if err != nil {
		return fmt.Errorf("postgres: %w", err)
	}
	w.pg = pool
	migrations, err := fs.Sub(pgMigrations, "migrations")
	if err != nil {
		return fmt.Errorf("postgres migrations: %w", err)
	}
	if err := pgdb.Migrate(w.shutdownCtx, pool, "leafwiki", migrations); err != nil {
		return fmt.Errorf("postgres migrations: %w", err)
	}
	w.log.Info("connected to postgres")
	return nil
}
