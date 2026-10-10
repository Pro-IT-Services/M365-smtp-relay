-- Schema of the first deployments, before schema versions existed (user_version 0):
-- no SMTP login columns on hosts, no conn_log, no allowed_senders setting.
CREATE TABLE settings (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
CREATE TABLE hosts (
	id           INTEGER PRIMARY KEY AUTOINCREMENT,
	name         TEXT NOT NULL,
	match        TEXT NOT NULL UNIQUE,
	sender       TEXT NOT NULL,
	rewrite_from INTEGER NOT NULL DEFAULT 1,
	enabled      INTEGER NOT NULL DEFAULT 1,
	note         TEXT NOT NULL DEFAULT '',
	created_at   INTEGER NOT NULL
);
CREATE TABLE message_log (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	ts          INTEGER NOT NULL,
	client_ip   TEXT NOT NULL,
	port        TEXT NOT NULL,
	host_id     INTEGER NOT NULL DEFAULT 0,
	host_name   TEXT NOT NULL DEFAULT '',
	sender      TEXT NOT NULL DEFAULT '',
	env_from    TEXT NOT NULL DEFAULT '',
	recipients  TEXT NOT NULL DEFAULT '',
	subject     TEXT NOT NULL DEFAULT '',
	size        INTEGER NOT NULL DEFAULT 0,
	status      TEXT NOT NULL,
	error       TEXT NOT NULL DEFAULT '',
	duration_ms INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX message_log_ts ON message_log(ts);
