package tree

import (
	"io/fs"
	"os"

	"github.com/perber/wiki/internal/core/shared"
)

// nodeFS is the small filesystem seam NodeStore reads and writes the page
// tree through (ADR-0018). Paths are absolute, os-style; the Postgres
// implementation maps them onto the tree_files table.
type nodeFS interface {
	Stat(path string) (fs.FileInfo, error)
	ReadFile(path string) ([]byte, error)
	WriteFileAtomic(path string, data []byte, perm fs.FileMode) error
	WriteFile(path string, data []byte, perm fs.FileMode) error
	MkdirAll(path string, perm fs.FileMode) error
	Rename(old, new string) error
	Remove(path string) error
	RemoveAll(path string) error
	ReadDir(path string) ([]fs.DirEntry, error)
}

// osFS is nodeFS backed by the real filesystem.
type osFS struct{}

func (osFS) Stat(path string) (fs.FileInfo, error) { return os.Stat(path) }
func (osFS) ReadFile(path string) ([]byte, error)  { return os.ReadFile(path) }
func (osFS) WriteFileAtomic(path string, data []byte, perm fs.FileMode) error {
	return shared.WriteFileAtomic(path, data, perm)
}
func (osFS) WriteFile(path string, data []byte, perm fs.FileMode) error {
	return os.WriteFile(path, data, perm)
}
func (osFS) MkdirAll(path string, perm fs.FileMode) error { return os.MkdirAll(path, perm) }
func (osFS) Rename(old, new string) error                 { return os.Rename(old, new) }
func (osFS) Remove(path string) error                     { return os.Remove(path) }
func (osFS) RemoveAll(path string) error                  { return os.RemoveAll(path) }
func (osFS) ReadDir(path string) ([]fs.DirEntry, error)   { return os.ReadDir(path) }
