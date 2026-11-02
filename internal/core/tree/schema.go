package tree

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"log"
	"os"
	"path/filepath"
)

const CurrentSchemaVersion = 5

type SchemaInfo struct {
	Version int `json:"version"`
}

func loadSchema(storageDir string) (SchemaInfo, error) {
	path := filepath.Join(storageDir, "schema.json")

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			// First run / legacy install
			log.Printf("Schema file not found, assuming version 0")
			return SchemaInfo{Version: 0}, nil
		}
		log.Printf("Error reading schema file: %v", err)
		return SchemaInfo{}, err
	}

	var s SchemaInfo
	if err := json.Unmarshal(data, &s); err != nil {
		log.Printf("Error unmarshaling schema file: %v", err)
		return SchemaInfo{}, err
	}

	return s, nil
}

func saveSchema(storageDir string, version int) error {
	path := filepath.Join(storageDir, "schema.json")
	s := SchemaInfo{Version: version}

	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		log.Printf("Error marshaling schema data: %v", err)
		return err
	}

	log.Printf("Saving schema version %d to %s", version, path)
	return os.WriteFile(path, data, 0o644)
}

// loadSchemaPG reads the schema version from the Postgres meta table
// (ADR-0018). A missing row means a fresh Postgres install — there is no
// legacy file layout to migrate, so the current version applies.
func loadSchemaPG(ctx context.Context, pool *pgxpool.Pool) (SchemaInfo, error) {
	var v string
	err := pool.QueryRow(ctx, `SELECT value FROM meta WHERE key = 'schema_version'`).Scan(&v)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return SchemaInfo{Version: CurrentSchemaVersion}, nil
		}
		return SchemaInfo{}, err
	}
	var s SchemaInfo
	if err := json.Unmarshal([]byte(v), &s); err != nil {
		return SchemaInfo{}, err
	}
	return s, nil
}

func saveSchemaPG(ctx context.Context, pool *pgxpool.Pool, version int) error {
	data, err := json.Marshal(SchemaInfo{Version: version})
	if err != nil {
		return err
	}
	_, err = pool.Exec(ctx,
		`INSERT INTO meta (key, value) VALUES ('schema_version', $1)
		 ON CONFLICT (key) DO UPDATE SET value = $1`, string(data))
	return err
}
