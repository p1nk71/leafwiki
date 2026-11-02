CREATE TABLE IF NOT EXISTS branding_files (
	path        text        PRIMARY KEY,
	parent_path text        NOT NULL DEFAULT '',
	name        text        NOT NULL,
	kind        text        NOT NULL CHECK (kind IN ('file', 'dir')),
	content     bytea,
	updated_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS branding_files_parent_idx ON branding_files(parent_path);
