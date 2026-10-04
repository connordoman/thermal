-- Timestamps are Unix milliseconds (UTC).

CREATE TABLE api_keys (
    -- Public identifier embedded in the key itself (thm_<id>_<secret>).
    id                   TEXT    PRIMARY KEY,
    name                 TEXT    NOT NULL,
    -- Space-separated scopes: admin, print, read.
    scopes               TEXT    NOT NULL,
    -- SHA-256 of the secret part of the key.
    secret_hash          BLOB    NOT NULL,
    -- The previous secret stays valid until previous_expires_at after a
    -- rotation with a grace period.
    previous_secret_hash BLOB,
    previous_expires_at  INTEGER,
    created_by           TEXT,
    created_at           INTEGER NOT NULL,
    rotated_at           INTEGER,
    last_used_at         INTEGER,
    expires_at           INTEGER,
    revoked_at           INTEGER
);

CREATE TABLE jobs (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    -- raw, text, markdown, utf8 or json.
    kind           TEXT    NOT NULL,
    -- queued, printing, completed, failed or canceled.
    status         TEXT    NOT NULL DEFAULT 'queued',
    priority       INTEGER NOT NULL DEFAULT 0,
    label          TEXT,
    copies         INTEGER NOT NULL DEFAULT 1,
    api_key_id     TEXT    REFERENCES api_keys (id),
    client_ip      TEXT,
    user_agent     TEXT,
    content_type   TEXT,
    -- The request body as received, kept for auditing until purged.
    source         BLOB,
    source_size    INTEGER NOT NULL,
    -- The rendered ESC/POS commands sent to the printer, until purged.
    payload        BLOB,
    payload_size   INTEGER NOT NULL,
    payload_sha256 TEXT    NOT NULL,
    attempts       INTEGER NOT NULL DEFAULT 0,
    error          TEXT,
    -- Whether the printer confirmed it finished processing the job.
    confirmed      INTEGER NOT NULL DEFAULT 0,
    retry_of       INTEGER REFERENCES jobs (id),
    created_at     INTEGER NOT NULL,
    started_at     INTEGER,
    finished_at    INTEGER,
    purged_at      INTEGER
);

CREATE INDEX jobs_queue ON jobs (status, priority DESC, id);
CREATE INDEX jobs_key ON jobs (api_key_id, id);

CREATE TABLE audit_events (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    at           INTEGER NOT NULL,
    -- The key that performed the action, or NULL for the server itself.
    actor_key_id TEXT,
    action       TEXT    NOT NULL,
    target       TEXT,
    -- JSON object with action-specific details.
    detail       TEXT,
    client_ip    TEXT
);

CREATE INDEX audit_events_at ON audit_events (at);
