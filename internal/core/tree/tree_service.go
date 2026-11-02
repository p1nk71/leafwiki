package tree

import (
	"github.com/jackc/pgx/v5/pgxpool"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"

	"github.com/perber/wiki/internal/core/ignore"
	"github.com/perber/wiki/internal/core/treemigration"
)

// TreeService is our main component for handling tree operations
// We use this service to create pages, delete pages, update pages, etc.
type TreeService struct {
	storageDir   string
	tree         *PageNode
	store        *NodeStore
	pg           *pgxpool.Pool
	log          *slog.Logger
	nodesByID    map[string]*PageNode
	nodesByTitle map[string][]*PageNode
	childSlugs   map[string]map[string]*PageNode

	mu sync.RWMutex
}

const (
	legacyTreeFilename         = "tree.json"
	errNilTreeReconstructed    = "internal error: tree reconstruction returned nil tree"
	errPersistChildOrderFailed = "could not persist child order: %w"
	errGetPageContentFailed    = "could not get page content: %w"
	errRollbackMovedNodeFailed = "rollback moved node: %w"
)

// NewTreeService opens the tree under storageDir — or on the Postgres
// tree_files table when pg is set (ADR-0018).
func NewTreeService(storageDir string, pg *pgxpool.Pool) *TreeService {
	store := NewNodeStore(storageDir, pg)

	return &TreeService{
		storageDir:   storageDir,
		tree:         nil,
		store:        store,
		pg:           pg,
		log:          slog.Default().With("component", "TreeService"),
		nodesByID:    make(map[string]*PageNode),
		nodesByTitle: make(map[string][]*PageNode),
		childSlugs:   make(map[string]map[string]*PageNode),
	}
}

// ReadTreeFile reads a file through the tree's storage backend — used by
// the ignore cache so .leafwikiignore works identically on Postgres
// (ADR-0018).
func (t *TreeService) ReadTreeFile(path string) ([]byte, error) {
	return t.store.vfs.ReadFile(path)
}

// SetIgnoreCache sets the ignore cache for multi-level ignore resolution.
func (t *TreeService) SetIgnoreCache(ignoreCache *ignore.Cache) {
	t.store.SetIgnoreCache(ignoreCache)
}

// LoadTree reconstructs the in-memory tree from the filesystem.
// Legacy tree.json data is only used as a migration source for older schema versions.
func (t *TreeService) LoadTree() error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if err := t.store.migratePG(context.Background()); err != nil {
		return err
	}

	t.log.Info("Checking schema version...")
	var schema SchemaInfo
	var err error
	if t.pg != nil {
		// Schema version lives in the Postgres meta table; a fresh install
		// starts at the current version (no legacy layout to migrate).
		schema, err = loadSchemaPG(context.Background(), t.pg)
	} else {
		schema, err = loadSchema(t.storageDir)
	}
	if err != nil {
		t.log.Error("Error loading schema", "error", err)
		return err
	}

	if schema.Version == CurrentSchemaVersion {
		reconstructed, err := t.store.ReconstructTreeFromFS()
		if err != nil {
			return err
		}
		if reconstructed == nil {
			return errors.New(errNilTreeReconstructed)
		}
		t.tree = reconstructed
		t.rebuildIndexesLocked()
		return nil
	}

	legacyTreePath := filepath.Join(t.storageDir, legacyTreeFilename)
	if info, statErr := os.Stat(legacyTreePath); statErr == nil && !info.IsDir() {
		legacyTree, legacyErr := t.store.LoadTree(legacyTreeFilename)
		if legacyErr != nil {
			t.log.Warn("Could not load legacy tree, falling back to filesystem reconstruction", "path", legacyTreePath, "error", legacyErr)
			t.tree, err = t.store.ReconstructTreeFromFS()
			if err != nil {
				return err
			}
		} else {
			t.tree = legacyTree
		}
	} else {
		t.tree, err = t.store.ReconstructTreeFromFS()
		if err != nil {
			return err
		}
	}

	if t.tree == nil {
		return errors.New(errNilTreeReconstructed)
	}

	t.log.Info("Migrating schema", "fromVersion", schema.Version, "toVersion", CurrentSchemaVersion)
	if err := treemigration.Run(schema.Version, t.migrationDependencies()); err != nil {
		t.log.Error("Error migrating schema", "error", err)
		return err
	}

	reconstructed, err := t.store.ReconstructTreeFromFS()
	if err != nil {
		return err
	}
	if reconstructed == nil {
		return errors.New(errNilTreeReconstructed)
	}

	t.tree = reconstructed
	t.rebuildIndexesLocked()

	if err := os.Remove(legacyTreePath); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.log.Warn("Could not remove migrated legacy tree snapshot", "path", legacyTreePath, "error", err)
	}

	return nil
}

func (t *TreeService) withLockedTree(fn func() error) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	return fn()
}

func (t *TreeService) withRLockedTree(fn func() error) error {
	t.mu.RLock()
	defer t.mu.RUnlock()

	return fn()
}

// TreeHash returns the current hash of the tree
func (t *TreeService) TreeHash() string {
	var hash string
	_ = t.withRLockedTree(func() error {
		hash = t.tree.Hash()
		return nil
	})
	return hash
}

// ReconstructTreeFromFS reconstructs the tree from the filesystem.
// The slow FS scan runs without holding the write lock so readers can
// continue serving the current tree concurrently. The lock is acquired
// only for the fast in-memory swap and index rebuild.
func (t *TreeService) ReconstructTreeFromFS() error {
	return t.ReconstructTreeFromFSContext(context.Background())
}

func (t *TreeService) ReconstructTreeFromFSContext(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	newTree, err := t.store.ReconstructTreeFromFSContext(ctx)
	if err != nil {
		t.log.Error("Error reconstructing tree from filesystem", "error", err)
		return err
	}
	if newTree == nil {
		return fmt.Errorf("internal error: ReconstructTreeFromFS returned nil tree")
	}

	if err := ctx.Err(); err != nil {
		return err
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	oldTree := t.tree
	t.tree = newTree
	t.rebuildIndexesLocked()
	if t.pg != nil {
		err = saveSchemaPG(context.Background(), t.pg, CurrentSchemaVersion)
	} else {
		err = saveSchema(t.storageDir, CurrentSchemaVersion)
	}
	if err != nil {
		t.log.Error("Error saving schema after reconstruction", "error", err)
		t.tree = oldTree
		t.rebuildIndexesLocked()
		return err
	}
	return nil
}
