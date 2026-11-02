package settings

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Needs a real PostgreSQL: set LEAFWIKI_TEST_DATABASE_URL,
// e.g. postgres://postgres:postgres@localhost:5432/postgres
func testPGPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("LEAFWIKI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("LEAFWIKI_TEST_DATABASE_URL not set")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

type pgTestConfig struct {
	Enabled bool   `json:"enabled"`
	Label   string `json:"label"`
}

func TestStorePG_MissingRowReturnsZeroThenRoundTrips(t *testing.T) {
	pool := testPGPool(t)
	ctx := context.Background()
	name := "settings_test_" + t.Name()
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM settings WHERE name = $1`, name)
	})

	var injected bool
	st, err := NewPG(ctx, pool, name, pgTestConfig{Label: "zero"}, func(v *pgTestConfig) {
		injected = true
	})
	if err != nil {
		t.Fatalf("NewPG: %v", err)
	}
	if got := st.Get(); got != (pgTestConfig{Label: "zero"}) {
		t.Fatalf("Get = %+v, want zero value", got)
	}
	if !injected {
		t.Fatal("onLoad was not applied on first load")
	}

	if err := st.Put(pgTestConfig{Enabled: true, Label: "on"}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	// A fresh store sees the persisted row (and onLoad ran again).
	injected = false
	st2, err := NewPG(ctx, pool, name, pgTestConfig{}, func(v *pgTestConfig) {
		injected = true
	})
	if err != nil {
		t.Fatalf("NewPG reopen: %v", err)
	}
	if got := st2.Get(); got != (pgTestConfig{Enabled: true, Label: "on"}) {
		t.Fatalf("Get after reopen = %+v", got)
	}
	if !injected {
		t.Fatal("onLoad was not applied on reopen")
	}

	// Put overwrites the row, not appends.
	if err := st2.Put(pgTestConfig{Label: "off"}); err != nil {
		t.Fatalf("Put overwrite: %v", err)
	}
	if err := st.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if got := st.Get(); got != (pgTestConfig{Label: "off"}) {
		t.Fatalf("Get after Reload = %+v", got)
	}
}
