package assets

import (
	"context"
	"embed"
	"io/fs"
	"errors"
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"

	"github.com/perber/wiki/internal/core/shared"
	sharederrors "github.com/perber/wiki/internal/core/shared/errors"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/perber/wiki/internal/core/shared/pgdb"
	"github.com/perber/wiki/internal/core/shared/vfs"
	"github.com/perber/wiki/internal/core/tree"

	"github.com/perber/wiki/internal/core/ignore"
)

//go:embed migrations
var assetMigrations embed.FS

const (
	DefaultMaxUploadSizeBytes int64 = 50 * 1024 * 1024
	errInvalidAssetName             = "Invalid asset name"
	errInvalidAssetNameFmt          = "invalid asset name %s"
	errAssetNotFound                = "Asset not found"
	errAssetNotFoundFmt             = "asset %s not found"
)

type AssetService struct {
	pg  *pgxpool.Pool
	vfs vfs.FS
	assetsDir   string
	storageDir  string
	slugger     *tree.SlugService
	log         *slog.Logger
	ignoreCache *ignore.Cache

	mu sync.RWMutex
}

// SetIgnoreCache sets the ignore cache for multi-level ignore resolution.
func (s *AssetService) SetIgnoreCache(ignoreCache *ignore.Cache) {
	s.ignoreCache = ignoreCache
}

// getIgnoreForDir returns the compiled ignore rules for the given directory,
// delegating to the shared cache. Returns nil if no rules apply.
func (s *AssetService) getIgnoreForDir(dir string) *ignore.IgnoreFile {
	if s.ignoreCache == nil {
		return nil
	}
	return s.ignoreCache.Get(dir)
}

// isPageIgnored checks if the page's path is ignored by .leafwikiignore.
func (s *AssetService) isPageIgnored(page *tree.PageNode) bool {
	if page == nil {
		return false
	}
	relPath := filepath.ToSlash(tree.GeneratePathFromPageNode(page))
	relPath = strings.TrimPrefix(relPath, "root/")
	dir := filepath.Join(s.storageDir, "root", filepath.Dir(relPath))
	ig := s.getIgnoreForDir(dir)
	if ig == nil {
		return false
	}
	isDir := page.Kind == tree.NodeKindSection
	return ig.Matches(relPath, isDir)
}

func assetPageDiskPath(assetsDir string, pageID string) string {
	normalizedAssetsDir := filepath.FromSlash(strings.ReplaceAll(assetsDir, `\`, `/`))
	return filepath.Join(normalizedAssetsDir, pageID)
}

func assetFileDiskPath(assetPath string, filename string) string {
	normalizedAssetPath := filepath.FromSlash(strings.ReplaceAll(assetPath, `\`, `/`))
	return filepath.Join(normalizedAssetPath, filename)
}

// validateFilename checks that a filename cannot escape its target directory.
// Rejects empty strings, path separators, and dot-only components like "." or "..".
func validateFilename(filename string) error {
	if filename == "" {
		return fmt.Errorf("filename must not be empty")
	}
	if strings.ContainsAny(filename, "/\\") {
		return fmt.Errorf("filename must not contain path separators")
	}
	if filename == "." || filename == ".." {
		return fmt.Errorf("filename must not be a dot component")
	}
	return nil
}

// NewAssetService opens assets under storageDir — or on the Postgres
// asset_files table when pg is set (ADR-0018). In pg mode the schema
// migration runs in Migrate, called by the composition root at boot.
func NewAssetService(storageDir string, slugger *tree.SlugService, pg *pgxpool.Pool) *AssetService {
	// Ensure the storage directory exists
	if err := os.MkdirAll(storageDir, 0755); err != nil {
		panic(fmt.Sprintf("could not create storage directory: %v", err))
	}
	assetsDir := filepath.Join(storageDir, "assets")

	s := &AssetService{
		assetsDir:  assetsDir,
		storageDir: storageDir,
		slugger:    slugger,
		pg:         pg,
		vfs:        vfs.OS{},
		log:        slog.Default().With("component", "AssetService"),
	}
	if pg != nil {
		s.vfs = vfs.NewPG(pg, assetsDir, "asset_files")
		return s
	}
	if err := s.vfs.MkdirAll(assetsDir, 0755); err != nil {
		panic(fmt.Sprintf("could not create assets directory: %v", err))
	}
	return s
}

// Migrate applies the asset_files schema when the service is Postgres-backed.
// Called once at boot by the composition root.
func (s *AssetService) Migrate(ctx context.Context) error {
	if s.pg == nil {
		return nil
	}
	migs, err := fs.Sub(assetMigrations, "migrations/asset_files")
	if err != nil {
		return fmt.Errorf("asset migrations: %w", err)
	}
	return pgdb.Migrate(ctx, s.pg, "asset_files", migs)
}

// writeStream persists r to path enforcing maxBytes. Postgres mode reads
// through the virtual filesystem (ADR-0018); file mode keeps the temp-file
// atomic write.
func (s *AssetService) writeStream(path string, r io.Reader, maxBytes int64) error {
	if s.pg != nil {
		data, err := io.ReadAll(io.LimitReader(r, maxBytes+1))
		if err != nil {
			return err
		}
		if int64(len(data)) > maxBytes {
			return shared.ErrFileTooLarge
		}
		return s.vfs.WriteStream(path, bytes.NewReader(data))
	}
	return shared.WriteStreamAtomic(path, r, maxBytes, 0o644)
}

func (s *AssetService) GetAssetsDir() string {
	return s.assetsDir
}

// IsPG reports whether the service stores assets in Postgres (ADR-0018).
func (s *AssetService) IsPG() bool { return s.pg != nil }

// FS exposes the backing filesystem — the revision service snapshots and
// restores through whichever backend is active.
func (s *AssetService) FS() vfs.FS { return s.vfs }

// ReadAssetData returns an asset's bytes by its path under assetsDir —
// the HTTP layer serves bytes, not paths, in Postgres mode.
func (s *AssetService) ReadAssetData(path string) ([]byte, error) {
	return s.vfs.ReadFile(path)
}

func (s *AssetService) ensureAssetPagePathExists(page *tree.PageNode) (string, error) {
	pagePath := assetPageDiskPath(s.assetsDir, page.ID)
	// check if the page path exists
	if _, err := s.vfs.Stat(pagePath); os.IsNotExist(err) {
		// create the page path
		if err := s.vfs.MkdirAll(pagePath, 0755); err != nil {
			return "", fmt.Errorf("could not create page path: %w", err)
		}
	}

	return pagePath, nil
}

func (s *AssetService) getAssetPagePath(page *tree.PageNode) (string, error) {
	pagePath := assetPageDiskPath(s.assetsDir, page.ID)

	// check if the page path exists
	if _, err := s.vfs.Stat(pagePath); os.IsNotExist(err) {
		return "", fmt.Errorf("page path does not exist: %w", err)
	}

	return pagePath, nil
}

func (s *AssetService) buildPublicPath(page *tree.PageNode, filename string) string {
	return "/" + path.Join("assets", page.ID, filename)
}

// SaveAssetForPage saves a file under a page's slug-based path and returns its public URL.
func (s *AssetService) SaveAssetForPage(page *tree.PageNode, file multipart.File, originalFilename string, maxBytes int64) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Guard against ignored pages
	if s.isPageIgnored(page) {
		return "", sharederrors.NewLocalizedError("asset_upload_failed", "Failed to upload asset", "page is excluded by .leafwikiignore", nil)
	}

	uploadPath, err := s.ensureAssetPagePathExists(page)
	if err != nil {
		return "", sharederrors.NewLocalizedError("asset_upload_failed", "Failed to upload asset", "failed to upload asset", err)
	}

	// Read existing filenames
	entries, _ := s.vfs.ReadDir(uploadPath)
	existing := make([]string, 0, len(entries))
	for _, e := range entries {
		existing = append(existing, e.Name())
	}

	finalFilename := s.slugger.GenerateUniqueFilename(existing, originalFilename)
	fullPath := assetFileDiskPath(uploadPath, finalFilename)

	if err := s.writeStream(fullPath, file, maxBytes); err != nil {
		if errors.Is(err, shared.ErrFileTooLarge) {
			return "", sharederrors.NewLocalizedError("asset_file_too_large", "File is too large", "file is too large", err)
		}
		return "", sharederrors.NewLocalizedError("asset_upload_failed", "Failed to upload asset", "failed to upload asset", err)
	}

	// Return public path (served from /assets)
	return s.buildPublicPath(page, finalFilename), nil
}

// ListAssetsForPage returns the full paths of all assets for a given page
func (s *AssetService) ListAssetsForPage(page *tree.PageNode) ([]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	pagePath, err := s.getAssetPagePath(page)
	if err != nil {
		return []string{}, nil
	}

	files, err := s.vfs.ReadDir(pagePath)
	if err != nil && !os.IsNotExist(err) {
		return []string{}, nil
	}

	result := []string{}
	for _, f := range files {
		if !f.IsDir() {
			result = append(result, s.buildPublicPath(page, f.Name()))
		}
	}

	return result, nil
}

// DeleteAsset removes an asset file from disk
func (s *AssetService) DeleteAsset(page *tree.PageNode, filename string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Guard against ignored pages
	if s.isPageIgnored(page) {
		return sharederrors.NewLocalizedError("asset_delete_failed", "Failed to delete asset", "page is excluded by .leafwikiignore", nil)
	}

	if err := validateFilename(filename); err != nil {
		return sharederrors.NewLocalizedError("asset_invalid_name", errInvalidAssetName, errInvalidAssetNameFmt, nil, filename)
	}

	assetPath, err := s.getAssetPagePath(page)
	if err != nil {
		return sharederrors.NewLocalizedError("asset_not_found", errAssetNotFound, errAssetNotFoundFmt, nil, filename)
	}

	fullPath := assetFileDiskPath(assetPath, filename)

	if err := s.vfs.Remove(fullPath); err != nil {
		if os.IsNotExist(err) {
			return sharederrors.NewLocalizedError("asset_not_found", errAssetNotFound, errAssetNotFoundFmt, nil, filename)
		}
		return sharederrors.NewLocalizedError("asset_delete_failed", "Failed to delete asset", "failed to delete asset %s", err, filename)
	}

	// Check if the directory is empty and remove it if so
	files, err := s.vfs.ReadDir(assetPath)
	if err == nil && len(files) == 0 {
		_ = s.vfs.Remove(assetPath) // we don't care if this fails
	}

	return nil
}

func (s *AssetService) DeleteAllAssetsForPage(page *tree.PageNode) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Guard against ignored pages
	if s.isPageIgnored(page) {
		return nil // silently skip — no assets to delete for an ignored page anyway
	}

	assetDir, err := s.getAssetPagePath(page)
	if err != nil {
		// no assets dir -> nothing to delete
		return nil
	}
	if _, err := s.vfs.Stat(assetDir); err == nil {
		return s.vfs.RemoveAll(assetDir)
	}
	return nil
}

// RenameAsset renames an asset file for a given page.
func (s *AssetService) RenameAsset(page *tree.PageNode, oldFilename, newFilename string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Guard against ignored pages
	if s.isPageIgnored(page) {
		return "", sharederrors.NewLocalizedError("asset_rename_failed", "Failed to rename asset", "page is excluded by .leafwikiignore", nil)
	}

	if err := validateFilename(oldFilename); err != nil {
		return "", sharederrors.NewLocalizedError("asset_invalid_name", errInvalidAssetName, errInvalidAssetNameFmt, nil, oldFilename)
	}

	assetPath, err := s.getAssetPagePath(page)
	if err != nil {
		return "", sharederrors.NewLocalizedError("asset_not_found", errAssetNotFound, errAssetNotFoundFmt, nil, oldFilename)
	}

	oldFullPath := assetFileDiskPath(assetPath, oldFilename)
	newFullPath := assetFileDiskPath(assetPath, newFilename)

	// Ensure that the new filename has the same extension as the old one
	oldExt := path.Ext(oldFilename)
	newExt := path.Ext(newFilename)
	if oldExt != newExt {
		return "", sharederrors.NewLocalizedError("asset_invalid_extension", "Asset extension must not change", "asset extension must not change from %s", nil, oldExt)
	}

	// Used for slug validation
	// The extension is not part of the slug, so we remove it
	newFilenameWithoutExt := newFilename[:len(newFilename)-len(newExt)]
	// Ensure that the new asset is a valid filename (slug)
	if err := s.slugger.IsValidSlug(newFilenameWithoutExt); err != nil {
		return "", sharederrors.NewLocalizedError("asset_invalid_name", errInvalidAssetName, errInvalidAssetNameFmt, nil, newFilename)
	}

	// Ensure that no file with the new name already exists
	if _, statErr := s.vfs.Stat(newFullPath); statErr == nil {
		return "", sharederrors.NewLocalizedError("asset_already_exists", "Asset already exists", "asset %s already exists", nil, newFilename)
	} else if !os.IsNotExist(statErr) {
		return "", sharederrors.NewLocalizedError("asset_rename_failed", "Failed to rename asset", "failed to rename asset %s", statErr, oldFilename)
	}

	if _, err := s.vfs.Stat(oldFullPath); os.IsNotExist(err) {
		return "", sharederrors.NewLocalizedError("asset_not_found", errAssetNotFound, errAssetNotFoundFmt, nil, oldFilename)
	}

	if err := s.vfs.Rename(oldFullPath, newFullPath); err != nil {
		return "", sharederrors.NewLocalizedError("asset_rename_failed", "Failed to rename asset", "failed to rename asset %s", err, oldFilename)
	}

	return s.buildPublicPath(page, newFilename), nil
}

func (s *AssetService) CopyAllAssets(sourcePage *tree.PageNode, targetPage *tree.PageNode) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Guard against ignored pages
	if s.isPageIgnored(sourcePage) || s.isPageIgnored(targetPage) {
		return nil // silently skip
	}

	sourceAssetPath, err := s.getAssetPagePath(sourcePage)
	if err != nil {
		// No assets to copy
		return nil
	}

	targetAssetPath, err := s.ensureAssetPagePathExists(targetPage)
	if err != nil {
		return fmt.Errorf("could not create target asset path: %w", err)
	}

	entries, err := s.vfs.ReadDir(sourceAssetPath)
	if err != nil {
		return fmt.Errorf("could not read source asset directory: %w", err)
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue // skip directories
		}
		if err := s.copySingleAsset(sourceAssetPath, targetAssetPath, entry); err != nil {
			return fmt.Errorf("could not copy asset %s: %w", entry.Name(), err)
		}
	}

	return nil
}

func (s *AssetService) copySingleAsset(sourceAssetPath string, targetAssetPath string, entry os.DirEntry) error {
	sourceFilePath := assetFileDiskPath(sourceAssetPath, entry.Name())
	targetFilePath := assetFileDiskPath(targetAssetPath, entry.Name())

	data, err := s.vfs.ReadFile(sourceFilePath)
	if err != nil {
		return fmt.Errorf("could not open source asset file: %w", err)
	}
	if err := s.vfs.WriteStream(targetFilePath, bytes.NewReader(data)); err != nil {
		return fmt.Errorf("could not copy asset file: %w", err)
	}
	return nil
}
