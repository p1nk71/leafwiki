CREATE TABLE IF NOT EXISTS asset_files (
	path        text        PRIMARY KEY,
	parent_path text        NOT NULL DEFAULT '',
	name        text        NOT NULL,
	kind        text        NOT NULL CHECK (kind IN ('file', 'dir')),
	content     bytea,
	updated_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS asset_files_parent_idx ON asset_files(parent_path);
