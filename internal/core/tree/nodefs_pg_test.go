package tree

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/perber/wiki/internal/core/shared/pgdb"
)

// Needs a real PostgreSQL: set LEAFWIKI_TEST_DATABASE_URL,
// e.g. postgres://postgres:postgres@localhost:5432/postgres
func testPGFS(t *testing.T) (*pgFS, *pgxpool.Pool) {
	t.Helper()
	dsn := os.Getenv("LEAFWIKI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("LEAFWIKI_TEST_DATABASE_URL not set")
	}
	pool, err := pgdb.Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	store := NewNodeStore(t.TempDir(), pool)
	if err := store.migratePG(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	root := t.TempDir() + "/root"
	vfs := newPgFS(pool, root)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM tree_files WHERE path LIKE 'pgfs_test/%' OR path = 'pgfs_test'`)
	})
	return vfs, pool
}

func TestPGFS_RoundTrip(t *testing.T) {
	vfs, _ := testPGFS(t)
	root := vfs.root

	write := func(rel, content string) {
		t.Helper()
		if err := vfs.WriteFileAtomic(root+"/"+rel, []byte(content), 0o644); err != nil {
			t.Fatalf("WriteFileAtomic %s: %v", rel, err)
		}
	}

	// files + implicit dirs
	write("a/b/page.md", "hello")
	write("a/index.md", "section")
	write(".order", "x")

	// Stat file and dir
	info, err := vfs.Stat(root + "/a/b/page.md")
	if err != nil || info.IsDir() || info.Size() != 5 {
		t.Fatalf("stat file: %v %+v", err, info)
	}
	info, err = vfs.Stat(root + "/a/b")
	if err != nil || !info.IsDir() {
		t.Fatalf("stat dir: %v %+v", err, info)
	}
	if _, err := vfs.Stat(root + "/nope.md"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("stat missing: want ErrNotExist, got %v", err)
	}

	// ReadFile
	got, err := vfs.ReadFile(root + "/a/b/page.md")
	if err != nil || string(got) != "hello" {
		t.Fatalf("read: %v %q", err, got)
	}
	if _, err := vfs.ReadFile(root + "/a/b"); err == nil {
		t.Fatal("read dir should fail")
	}

	// ReadDir
	ents, err := vfs.ReadDir(root + "/a")
	if err != nil || len(ents) != 2 {
		t.Fatalf("readdir: %v %d", err, len(ents))
	}
	if ents[0].Name() != "b" || !ents[0].IsDir() || ents[1].Name() != "index.md" {
		t.Fatalf("readdir entries: %+v %+v", ents[0], ents[1])
	}

	// Rename dir cascades descendants
	if err := vfs.Rename(root+"/a", root+"/renamed"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	got, err = vfs.ReadFile(root + "/renamed/b/page.md")
	if err != nil || string(got) != "hello" {
		t.Fatalf("read after rename: %v %q", err, got)
	}
	if _, err := vfs.Stat(root + "/a"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("old path should be gone: %v", err)
	}

	// RemoveAll on dir drops children too
	if err := vfs.RemoveAll(root + "/renamed"); err != nil {
		t.Fatalf("removeall: %v", err)
	}
	if _, err := vfs.Stat(root + "/renamed/b/page.md"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("descendant should be gone: %v", err)
	}
	if _, err := vfs.Stat(root + "/.order"); err != nil {
		t.Fatalf("unrelated file removed: %v", err)
	}
}

func TestPGFS_OutsideRootPassesThrough(t *testing.T) {
	vfs, _ := testPGFS(t)
	outside := t.TempDir() + "/schema.json"
	if err := vfs.WriteFileAtomic(outside, []byte("{}"), 0o644); err != nil {
		t.Fatalf("write outside: %v", err)
	}
	if got, err := os.ReadFile(outside); err != nil || string(got) != "{}" {
		t.Fatalf("outside file: %v %q", err, got)
	}
}
