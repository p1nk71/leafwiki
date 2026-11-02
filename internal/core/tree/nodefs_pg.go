package tree

import (
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/perber/wiki/internal/core/shared/pgdb"
)

//go:embed migrations
var treeMigrations embed.FS

// pgFS is nodeFS backed by the tree_files table (ADR-0018): every page,
// section directory and sidecar file under <storageDir>/root is a row keyed
// by its root-relative path. Paths outside the root (schema.json, legacy
// tree.json) pass through to the real filesystem — scratch state stays local.
type pgFS struct {
	db     *pgdb.DB
	pool   *pgxpool.Pool
	root   string // absolute path of the tree root
	backup nodeFS
}

var errIsDirectory = errors.New("is a directory")
var errNotEmpty = errors.New("directory not empty")

func newPgFS(pool *pgxpool.Pool, root string) *pgFS {
	return &pgFS{
		db:     pgdb.NewPGDB(pool),
		pool:   pool,
		root:   filepath.Clean(root),
		backup: osFS{},
	}
}

// rel maps an absolute path to its root-relative '/'-separated form. The
// second result is false for paths outside the root, which the caller must
// pass through to the real filesystem.
func (f *pgFS) rel(path string) (string, bool) {
	abs := filepath.Clean(path)
	if abs == f.root {
		return "", true
	}
	prefix := f.root + string(filepath.Separator)
	if strings.HasPrefix(abs, prefix) {
		return filepath.ToSlash(abs[len(prefix):]), true
	}
	return "", false
}

func dirPart(rel string) string {
	if i := strings.LastIndexByte(rel, '/'); i >= 0 {
		return rel[:i]
	}
	return ""
}

func basePart(rel string) string {
	if i := strings.LastIndexByte(rel, '/'); i >= 0 {
		return rel[i+1:]
	}
	return rel
}

type pgFileInfo struct {
	name string
	size int64
	dir  bool
	mod  time.Time
}

func (i pgFileInfo) Name() string      { return i.name }
func (i pgFileInfo) Size() int64       { return i.size }
func (i pgFileInfo) IsDir() bool       { return i.dir }
func (i pgFileInfo) ModTime() time.Time { return i.mod }
func (i pgFileInfo) Sys() any          { return nil }
func (i pgFileInfo) Mode() fs.FileMode {
	if i.dir {
		return fs.ModeDir | 0o755
	}
	return 0o644
}

type pgDirEntry struct{ info pgFileInfo }

func (e pgDirEntry) Name() string { return e.info.name }
func (e pgDirEntry) IsDir() bool  { return e.info.dir }
func (e pgDirEntry) Type() fs.FileMode {
	if e.info.dir {
		return fs.ModeDir
	}
	return 0
}
func (e pgDirEntry) Info() (fs.FileInfo, error) { return e.info, nil }

func (f *pgFS) Stat(path string) (fs.FileInfo, error) {
	rel, ok := f.rel(path)
	if !ok {
		return f.backup.Stat(path)
	}
	if rel == "" {
		return pgFileInfo{name: filepath.Base(f.root), dir: true}, nil
	}
	var i pgFileInfo
	var kind string
	err := f.db.QueryRow(
		`SELECT kind, COALESCE(octet_length(content), 0), updated_at
		 FROM tree_files WHERE path = $1`, rel,
	).Scan(&kind, &i.size, &i.mod)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, &fs.PathError{Op: "stat", Path: path, Err: fs.ErrNotExist}
		}
		return nil, err
	}
	i.name = basePart(rel)
	i.dir = kind == "dir"
	return i, nil
}

func (f *pgFS) ReadFile(path string) ([]byte, error) {
	rel, ok := f.rel(path)
	if !ok {
		return f.backup.ReadFile(path)
	}
	var content []byte
	var kind string
	err := f.db.QueryRow(
		`SELECT kind, content FROM tree_files WHERE path = $1`, rel,
	).Scan(&kind, &content)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, &fs.PathError{Op: "open", Path: path, Err: fs.ErrNotExist}
		}
		return nil, err
	}
	if kind == "dir" {
		return nil, &fs.PathError{Op: "read", Path: path, Err: errIsDirectory}
	}
	return content, nil
}

// ancestors returns the directory ancestors of rel, outermost first.
func ancestors(rel string) []string {
	var out []string
	for d := dirPart(rel); d != ""; d = dirPart(d) {
		out = append([]string{d}, out...)
	}
	return out
}

func (f *pgFS) ensureDirsTx(tx *pgdb.Tx, rel string) error {
	for _, a := range ancestors(rel) {
		var kind string
		err := tx.QueryRow(`SELECT kind FROM tree_files WHERE path = $1`, a).Scan(&kind)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			if _, err := tx.Exec(
				`INSERT INTO tree_files (path, parent_path, name, kind)
				 VALUES ($1, $2, $3, 'dir')`, a, dirPart(a), basePart(a)); err != nil {
				return err
			}
		case err != nil:
			return err
		case kind != "dir":
			return &fs.PathError{Op: "mkdir", Path: a, Err: errors.New("not a directory")}
		}
	}
	return nil
}

func (f *pgFS) writeFile(path string, data []byte) error {
	rel, ok := f.rel(path)
	if !ok {
		return f.backup.WriteFile(path, data, 0o644)
	}
	if rel == "" {
		return &fs.PathError{Op: "write", Path: path, Err: errIsDirectory}
	}
	tx, err := f.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if err := f.ensureDirsTx(tx, rel); err != nil {
		return err
	}
	if _, err := tx.Exec(
		`INSERT INTO tree_files (path, parent_path, name, kind, content, updated_at)
		 VALUES ($1, $2, $3, 'file', $4, now())
		 ON CONFLICT (path) DO UPDATE
		   SET kind = 'file', content = EXCLUDED.content, updated_at = now()`,
		rel, dirPart(rel), basePart(rel), data); err != nil {
		return err
	}
	return tx.Commit()
}

func (f *pgFS) WriteFileAtomic(path string, data []byte, _ fs.FileMode) error {
	// A single tx already gives the all-or-nothing guarantee WriteFileAtomic
	// emulates on POSIX (temp + rename).
	return f.writeFile(path, data)
}

func (f *pgFS) WriteFile(path string, data []byte, _ fs.FileMode) error {
	return f.writeFile(path, data)
}

func (f *pgFS) MkdirAll(path string, _ fs.FileMode) error {
	rel, ok := f.rel(path)
	if !ok {
		return f.backup.MkdirAll(path, 0o755)
	}
	tx, err := f.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	for _, a := range append(ancestors(rel), rel) {
		if a == "" {
			continue
		}
		var kind string
		err := tx.QueryRow(`SELECT kind FROM tree_files WHERE path = $1`, a).Scan(&kind)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			if _, err := tx.Exec(
				`INSERT INTO tree_files (path, parent_path, name, kind)
				 VALUES ($1, $2, $3, 'dir')`, a, dirPart(a), basePart(a)); err != nil {
				return err
			}
		case err != nil:
			return err
		case kind != "dir":
			return &fs.PathError{Op: "mkdir", Path: a, Err: errNotEmpty}
		}
	}
	return tx.Commit()
}

func (f *pgFS) Rename(old, new string) error {
	oldRel, ok := f.rel(old)
	if !ok {
		return f.backup.Rename(old, new)
	}
	newRel, ok := f.rel(new)
	if !ok || oldRel == "" || newRel == "" {
		return fmt.Errorf("rename outside tree root: %s -> %s", old, new)
	}

	tx, err := f.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	var kind string
	if err := tx.QueryRow(`SELECT kind FROM tree_files WHERE path = $1`, oldRel).Scan(&kind); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return &fs.PathError{Op: "rename", Path: old, Err: fs.ErrNotExist}
		}
		return err
	}

	// os.Rename overwrites a file target silently and refuses a non-empty dir.
	if _, err := tx.Exec(`DELETE FROM tree_files WHERE path = $1 AND kind = 'file'`, newRel); err != nil {
		return err
	}
	var children int
	if err := tx.QueryRow(
		`SELECT count(*) FROM tree_files WHERE parent_path = $1`, newRel,
	).Scan(&children); err != nil {
		return err
	}
	if children > 0 {
		return &fs.PathError{Op: "rename", Path: new, Err: errNotEmpty}
	}

	if err := f.ensureDirsTx(tx, newRel); err != nil {
		return err
	}
	if _, err := tx.Exec(
		`UPDATE tree_files
		    SET path = concat($1::text, substring(path from $2::int)),
		        name = substring(path from '[^/]+$'),
		        parent_path = CASE WHEN strpos(path, '/') = 0 THEN ''
		                           ELSE substring(path from '^(.*)/[^/]+$') END,
		        updated_at = now()
		  WHERE path = $3 OR path LIKE $3 || '/%'`,
		newRel, len(oldRel)+1, oldRel); err != nil {
		return err
	}
	return tx.Commit()
}

func (f *pgFS) Remove(path string) error {
	rel, ok := f.rel(path)
	if !ok {
		return f.backup.Remove(path)
	}
	var kind string
	if err := f.db.QueryRow(`SELECT kind FROM tree_files WHERE path = $1`, rel).Scan(&kind); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return &fs.PathError{Op: "remove", Path: path, Err: fs.ErrNotExist}
		}
		return err
	}
	if kind == "dir" {
		var children int
		if err := f.db.QueryRow(
			`SELECT count(*) FROM tree_files WHERE parent_path = $1`, rel,
		).Scan(&children); err != nil {
			return err
		}
		if children > 0 {
			return &fs.PathError{Op: "remove", Path: path, Err: errNotEmpty}
		}
	}
	_, err := f.db.Exec(`DELETE FROM tree_files WHERE path = $1`, rel)
	return err
}

func (f *pgFS) RemoveAll(path string) error {
	rel, ok := f.rel(path)
	if !ok {
		return f.backup.RemoveAll(path)
	}
	if rel == "" {
		return fmt.Errorf("refusing to remove tree root")
	}
	_, err := f.db.Exec(`DELETE FROM tree_files WHERE path = $1 OR path LIKE $1 || '/%'`, rel)
	return err
}

func (f *pgFS) ReadDir(path string) ([]fs.DirEntry, error) {
	rel, ok := f.rel(path)
	if !ok {
		return f.backup.ReadDir(path)
	}
	if rel != "" {
		if _, err := f.Stat(path); err != nil {
			return nil, err
		}
	}
	rows, err := f.db.Query(
		`SELECT name, kind, COALESCE(octet_length(content), 0), updated_at
		 FROM tree_files WHERE parent_path = $1 ORDER BY name`, rel)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []fs.DirEntry
	for rows.Next() {
		var e pgDirEntry
		var kind string
		if err := rows.Scan(&e.info.name, &kind, &e.info.size, &e.info.mod); err != nil {
			return nil, err
		}
		e.info.dir = kind == "dir"
		out = append(out, e)
	}
	return out, rows.Err()
}
