CREATE TABLE IF NOT EXISTS users (
	id                      text        PRIMARY KEY,
	username                text        NOT NULL UNIQUE,
	password                text        NOT NULL,
	email                   text        NOT NULL UNIQUE,
	role                    text        NOT NULL,
	created_at              timestamptz NOT NULL DEFAULT now(),
	totp_secret_encrypted   text        NOT NULL DEFAULT '',
	totp_enabled            integer     NOT NULL DEFAULT 0,
	totp_recovery_codes_json text       NOT NULL DEFAULT '[]',
	totp_enabled_at         timestamptz NULL,
	totp_last_reset_at      timestamptz NULL,
	must_set_password       integer     NOT NULL DEFAULT 0
);
