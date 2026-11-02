package wiki

import (
	"embed"
	"fmt"
	"io/fs"

	"github.com/perber/wiki/internal/core/shared/pgdb"
)

//go:embed migrations
var pgMigrations embed.FS

// initPostgres adopts the canonical PostgreSQL pool opened by main and runs
// pending schema migrations. A nil pool is a no-op: the wiki keeps
// file/SQLite canonical storage (pre-ADR-0018 mode).
func (w *Wiki) initPostgres(options *WikiOptions) error {
	if options.PG == nil {
		return nil
	}
	w.pg = options.PG
	migrations, err := fs.Sub(pgMigrations, "migrations")
	if err != nil {
		return fmt.Errorf("postgres migrations: %w", err)
	}
	if err := pgdb.Migrate(w.shutdownCtx, w.pg, "leafwiki", migrations); err != nil {
		return fmt.Errorf("postgres migrations: %w", err)
	}
	w.log.Info("connected to postgres")
	return nil
}
