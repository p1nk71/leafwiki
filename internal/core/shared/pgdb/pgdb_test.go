package pgdb

import (
	"context"
	"os"
	"testing"
	"testing/fstest"

	"github.com/jackc/pgx/v5/pgxpool"
)

// These tests need a real PostgreSQL: set LEAFWIKI_TEST_DATABASE_URL,
// e.g. postgres://postgres:postgres@localhost:5432/postgres
func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("LEAFWIKI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("LEAFWIKI_TEST_DATABASE_URL not set")
	}
	pool, err := Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func migrationFS(files map[string]string) fstest.MapFS {
	fsys := fstest.MapFS{}
	for name, body := range files {
		fsys[name] = &fstest.MapFile{Data: []byte(body)}
	}
	return fsys
}

func TestMigrate_AppliesFilesInOrderAndRecordsThem(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	ns := "pgdb_test_" + t.Name()

	fsys := migrationFS(map[string]string{
		"0002_second.sql": "CREATE TABLE IF NOT EXISTS pgdb_test_second (id int);",
		"0001_first.sql":  "CREATE TABLE IF NOT EXISTS pgdb_test_first (id int);",
		"not_a_migration": "ignored",
	})

	if err := Migrate(ctx, pool, ns, fsys); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DROP TABLE IF EXISTS pgdb_test_first, pgdb_test_second")
		_, _ = pool.Exec(ctx, "DELETE FROM schema_migrations WHERE namespace = $1", ns)
	})

	var names []string
	rows, err := pool.Query(ctx,
		`SELECT name FROM schema_migrations WHERE namespace = $1 ORDER BY name`, ns)
	if err != nil {
		t.Fatalf("query schema_migrations: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatalf("scan: %v", err)
		}
		names = append(names, n)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if len(names) != 2 || names[0] != "0001_first.sql" || names[1] != "0002_second.sql" {
		t.Fatalf("applied = %v, want [0001_first.sql 0002_second.sql]", names)
	}

	// second run is a no-op
	if err := Migrate(ctx, pool, ns, fsys); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
}

func TestMigrate_FailedFileLeavesNoRecord(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	ns := "pgdb_test_" + t.Name()

	fsys := migrationFS(map[string]string{
		"0001_broken.sql": "THIS IS NOT SQL;",
	})

	if err := Migrate(ctx, pool, ns, fsys); err == nil {
		t.Fatal("expected error for invalid SQL")
	}

	var count int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM schema_migrations WHERE namespace = $1`, ns,
	).Scan(&count); err != nil {
		t.Fatalf("query: %v", err)
	}
	if count != 0 {
		t.Fatalf("failed migration recorded %d rows", count)
	}
}

func TestOpen_BadDSN(t *testing.T) {
	pool, err := Open(context.Background(), "postgres://127.0.0.1:1/nope?connect_timeout=1")
	if err == nil {
		pool.Close()
		t.Fatal("expected error for unreachable database")
	}
}
