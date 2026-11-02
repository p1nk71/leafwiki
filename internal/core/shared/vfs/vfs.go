package vfs

import (
	"io"
	"io/fs"
	"os"

	"github.com/perber/wiki/internal/core/shared"
)

// FS is the small filesystem seam canonical stores read and write through
// (ADR-0018). Paths are absolute, os-style; the Postgres implementation maps
// them onto a per-domain files table.
type FS interface {
	Stat(path string) (fs.FileInfo, error)
	ReadFile(path string) ([]byte, error)
	WriteFileAtomic(path string, data []byte, perm fs.FileMode) error
	WriteFile(path string, data []byte, perm fs.FileMode) error
	MkdirAll(path string, perm fs.FileMode) error
	Rename(old, new string) error
	Remove(path string) error
	RemoveAll(path string) error
	ReadDir(path string) ([]fs.DirEntry, error)
	WriteStream(path string, r io.Reader) error
}

// OS is FS backed by the real filesystem.
type OS struct{}

func (OS) Stat(path string) (fs.FileInfo, error) { return os.Stat(path) }
func (OS) ReadFile(path string) ([]byte, error)  { return os.ReadFile(path) }
func (OS) WriteFileAtomic(path string, data []byte, perm fs.FileMode) error {
	return shared.WriteFileAtomic(path, data, perm)
}
func (OS) WriteFile(path string, data []byte, perm fs.FileMode) error {
	return os.WriteFile(path, data, perm)
}
func (OS) MkdirAll(path string, perm fs.FileMode) error { return os.MkdirAll(path, perm) }
func (OS) Rename(old, new string) error                 { return os.Rename(old, new) }
func (OS) Remove(path string) error                     { return os.Remove(path) }
func (OS) RemoveAll(path string) error                  { return os.RemoveAll(path) }
func (OS) ReadDir(path string) ([]fs.DirEntry, error)   { return os.ReadDir(path) }

func (OS) WriteStream(path string, r io.Reader) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	_, err = io.Copy(f, r)
	return err
}
