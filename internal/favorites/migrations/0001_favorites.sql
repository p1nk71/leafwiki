CREATE TABLE IF NOT EXISTS favorites (
	user_id    text        NOT NULL,
	page_id    text        NOT NULL,
	created_at timestamptz NOT NULL,
	PRIMARY KEY (user_id, page_id)
);
CREATE INDEX IF NOT EXISTS favorites_user_id_idx ON favorites(user_id);
