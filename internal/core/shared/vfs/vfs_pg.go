package vfs

import (
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/perber/wiki/internal/core/shared/pgdb"
)

// PG is FS backed by a Postgres files table (ADR-0018): every file and
// directory under root is a row keyed by its root-relative path. Paths
// outside the root pass through to the real filesystem — scratch state stays
// local. table names the per-domain table (tree_files, asset_files, ...) and
// is always a package-owned constant, never user input.
type PG struct {
	db     *pgdb.DB
	pool   *pgxpool.Pool
	root   string // absolute path of the virtual root
	table  string
	backup FS
}

var errIsDirectory = errors.New("is a directory")
var errNotEmpty = errors.New("directory not empty")

// NewPG builds a Postgres-backed FS rooted at root, storing rows in table.
func NewPG(pool *pgxpool.Pool, root, table string) *PG {
	return &PG{
		db:     pgdb.NewPGDB(pool),
		pool:   pool,
		root:   filepath.Clean(root),
		table:  table,
		backup: OS{},
	}
}

// Pool exposes the underlying pool so owners can run migrations.
func (f *PG) Pool() *pgxpool.Pool { return f.pool }

// rel maps an absolute path to its root-relative '/'-separated form. The
// second result is false for paths outside the root, which the caller must
// pass through to the real filesystem.
func (f *PG) rel(path string) (string, bool) {
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

type fileInfo struct {
	name string
	size int64
	dir  bool
	mod  time.Time
}

func (i fileInfo) Name() string      { return i.name }
func (i fileInfo) Size() int64       { return i.size }
func (i fileInfo) IsDir() bool       { return i.dir }
func (i fileInfo) ModTime() time.Time { return i.mod }
func (i fileInfo) Sys() any          { return nil }
func (i fileInfo) Mode() fs.FileMode {
	if i.dir {
		return fs.ModeDir | 0o755
	}
	return 0o644
}

type dirEntry struct{ info fileInfo }

func (e dirEntry) Name() string { return e.info.name }
func (e dirEntry) IsDir() bool  { return e.info.dir }
func (e dirEntry) Type() fs.FileMode {
	if e.info.dir {
		return fs.ModeDir
	}
	return 0
}
func (e dirEntry) Info() (fs.FileInfo, error) { return e.info, nil }

func (f *PG) Stat(path string) (fs.FileInfo, error) {
	rel, ok := f.rel(path)
	if !ok {
		return f.backup.Stat(path)
	}
	if rel == "" {
		return fileInfo{name: filepath.Base(f.root), dir: true}, nil
	}
	var i fileInfo
	var kind string
	err := f.db.QueryRow(
		fmt.Sprintf(`SELECT kind, COALESCE(octet_length(content), 0), updated_at
		 FROM %s WHERE path = $1`, f.table), rel,
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

func (f *PG) ReadFile(path string) ([]byte, error) {
	rel, ok := f.rel(path)
	if !ok {
		return f.backup.ReadFile(path)
	}
	var content []byte
	var kind string
	err := f.db.QueryRow(
		fmt.Sprintf(`SELECT kind, content FROM %s WHERE path = $1`, f.table), rel,
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

func (f *PG) ensureDirsTx(tx *pgdb.Tx, rel string) error {
	for _, a := range ancestors(rel) {
		var kind string
		err := tx.QueryRow(fmt.Sprintf(`SELECT kind FROM %s WHERE path = $1`, f.table), a).Scan(&kind)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			if _, err := tx.Exec(
				fmt.Sprintf(`INSERT INTO %s (path, parent_path, name, kind)
				 VALUES ($1, $2, $3, 'dir')
				 ON CONFLICT (path) DO NOTHING`, f.table), a, dirPart(a), basePart(a)); err != nil {
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

func (f *PG) writeFile(path string, data []byte) error {
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
		fmt.Sprintf(`INSERT INTO %s (path, parent_path, name, kind, content, updated_at)
		 VALUES ($1, $2, $3, 'file', $4, now())
		 ON CONFLICT (path) DO UPDATE
		   SET kind = 'file', content = EXCLUDED.content, updated_at = now()`, f.table),
		rel, dirPart(rel), basePart(rel), data); err != nil {
		return err
	}
	return tx.Commit()
}

func (f *PG) WriteFileAtomic(path string, data []byte, _ fs.FileMode) error {
	// A single tx already gives the all-or-nothing guarantee WriteFileAtomic
	// emulates on POSIX (temp + rename).
	return f.writeFile(path, data)
}

func (f *PG) WriteFile(path string, data []byte, _ fs.FileMode) error {
	return f.writeFile(path, data)
}

// WriteStream buffers r and writes it — wiki assets are small enough that
// in-memory buffering is fine.
func (f *PG) WriteStream(path string, r io.Reader) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	return f.writeFile(path, data)
}

func (f *PG) MkdirAll(path string, _ fs.FileMode) error {
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
		err := tx.QueryRow(fmt.Sprintf(`SELECT kind FROM %s WHERE path = $1`, f.table), a).Scan(&kind)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			if _, err := tx.Exec(
				fmt.Sprintf(`INSERT INTO %s (path, parent_path, name, kind)
				 VALUES ($1, $2, $3, 'dir')
				 ON CONFLICT (path) DO NOTHING`, f.table), a, dirPart(a), basePart(a)); err != nil {
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

func (f *PG) Rename(old, new string) error {
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
	if err := tx.QueryRow(fmt.Sprintf(`SELECT kind FROM %s WHERE path = $1`, f.table), oldRel).Scan(&kind); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return &fs.PathError{Op: "rename", Path: old, Err: fs.ErrNotExist}
		}
		return err
	}

	// os.Rename overwrites a file target silently and refuses a non-empty dir.
	if _, err := tx.Exec(fmt.Sprintf(`DELETE FROM %s WHERE path = $1 AND kind = 'file'`, f.table), newRel); err != nil {
		return err
	}
	var children int
	if err := tx.QueryRow(
		fmt.Sprintf(`SELECT count(*) FROM %s WHERE parent_path = $1`, f.table), newRel,
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
		fmt.Sprintf(`UPDATE %s
		    SET path = concat($1::text, substring(path from $2::int)),
		        name = substring(path from '[^/]+$'),
		        parent_path = CASE WHEN strpos(path, '/') = 0 THEN ''
		                           ELSE substring(path from '^(.*)/[^/]+$') END,
		        updated_at = now()
		  WHERE path = $3 OR path LIKE $3 || '/%%'`, f.table),
		newRel, len(oldRel)+1, oldRel); err != nil {
		return err
	}
	return tx.Commit()
}

func (f *PG) Remove(path string) error {
	rel, ok := f.rel(path)
	if !ok {
		return f.backup.Remove(path)
	}
	var kind string
	if err := f.db.QueryRow(fmt.Sprintf(`SELECT kind FROM %s WHERE path = $1`, f.table), rel).Scan(&kind); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return &fs.PathError{Op: "remove", Path: path, Err: fs.ErrNotExist}
		}
		return err
	}
	if kind == "dir" {
		var children int
		if err := f.db.QueryRow(
			fmt.Sprintf(`SELECT count(*) FROM %s WHERE parent_path = $1`, f.table), rel,
		).Scan(&children); err != nil {
			return err
		}
		if children > 0 {
			return &fs.PathError{Op: "remove", Path: path, Err: errNotEmpty}
		}
	}
	_, err := f.db.Exec(fmt.Sprintf(`DELETE FROM %s WHERE path = $1`, f.table), rel)
	return err
}

func (f *PG) RemoveAll(path string) error {
	rel, ok := f.rel(path)
	if !ok {
		return f.backup.RemoveAll(path)
	}
	if rel == "" {
		return fmt.Errorf("refusing to remove tree root")
	}
	_, err := f.db.Exec(fmt.Sprintf(`DELETE FROM %s WHERE path = $1 OR path LIKE $1 || '/%%'`, f.table), rel)
	return err
}

func (f *PG) ReadDir(path string) ([]fs.DirEntry, error) {
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
		fmt.Sprintf(`SELECT name, kind, COALESCE(octet_length(content), 0), updated_at
		 FROM %s WHERE parent_path = $1 ORDER BY name`, f.table), rel)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []fs.DirEntry
	for rows.Next() {
		var e dirEntry
		var kind string
		if err := rows.Scan(&e.info.name, &kind, &e.info.size, &e.info.mod); err != nil {
			return nil, err
		}
		e.info.dir = kind == "dir"
		out = append(out, e)
	}
	return out, rows.Err()
}
