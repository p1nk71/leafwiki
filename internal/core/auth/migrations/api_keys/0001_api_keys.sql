CREATE TABLE IF NOT EXISTS api_keys (
	id           text   PRIMARY KEY,
	name         text   NOT NULL,
	user_id      text   NOT NULL,
	prefix       text   NOT NULL UNIQUE,
	key_hash     text   NOT NULL,
	role         text   NOT NULL,
	expires_at   bigint,
	created_by   text   NOT NULL,
	created_at   bigint NOT NULL,
	last_used_at bigint,
	revoked_at   bigint
);
