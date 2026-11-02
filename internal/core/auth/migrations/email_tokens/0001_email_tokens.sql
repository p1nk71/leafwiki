CREATE TABLE IF NOT EXISTS email_tokens (
	id          text        PRIMARY KEY,
	token_hash  text        NOT NULL,
	user_id     text        NOT NULL,
	purpose     text        NOT NULL,
	created_at  bigint      NOT NULL,
	expires_at  bigint      NOT NULL,
	consumed_at bigint
);
CREATE INDEX IF NOT EXISTS email_tokens_user_id_idx ON email_tokens(user_id);
