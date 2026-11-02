CREATE TABLE IF NOT EXISTS user_settings (
	user_id     text        PRIMARY KEY,
	language    text        NOT NULL,
	autosave    boolean     NOT NULL,
	date_format text        NOT NULL DEFAULT 'locale',
	time_format text        NOT NULL DEFAULT 'locale',
	updated_at  timestamptz NOT NULL
);
