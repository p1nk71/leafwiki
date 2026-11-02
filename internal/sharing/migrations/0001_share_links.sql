CREATE TABLE IF NOT EXISTS share_links (
	id                  text        PRIMARY KEY,
	token_hash          text        NOT NULL UNIQUE,
	page_id             text        NOT NULL,
	include_descendants boolean     NOT NULL,
	expires_at          timestamptz NULL,
	created_by          text        NOT NULL,
	created_at          timestamptz NOT NULL
);
CREATE INDEX IF NOT EXISTS share_links_page_id_idx ON share_links(page_id);
