-- User accounts sign in with a password and get a session cookie, for the
-- web UI. API keys are unchanged.

CREATE TABLE users (
    id                  INTEGER PRIMARY KEY AUTOINCREMENT,
    -- Lowercase; usernames never change, so jobs and audit events can name
    -- the user who acted.
    username            TEXT    NOT NULL UNIQUE,
    -- Argon2id in PHC string format.
    password_hash       TEXT    NOT NULL,
    -- Space-separated scopes, as for API keys.
    scopes              TEXT    NOT NULL,
    -- The root user is created on first start, always has the admin scope
    -- and cannot be disabled.
    root                INTEGER NOT NULL DEFAULT 0,
    created_by          TEXT,
    created_at          INTEGER NOT NULL,
    updated_at          INTEGER NOT NULL,
    password_changed_at INTEGER NOT NULL,
    last_login_at       INTEGER,
    disabled_at         INTEGER
);

CREATE UNIQUE INDEX users_root ON users (root) WHERE root = 1;

CREATE TABLE sessions (
    -- SHA-256 of the session token sent in the cookie.
    token_hash   BLOB    PRIMARY KEY,
    user_id      INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    created_at   INTEGER NOT NULL,
    expires_at   INTEGER NOT NULL,
    last_seen_at INTEGER NOT NULL,
    client_ip    TEXT,
    user_agent   TEXT
);

CREATE INDEX sessions_user ON sessions (user_id);
CREATE INDEX sessions_expiry ON sessions (expires_at);

-- Who submitted a job or performed an action, when it was a user rather
-- than an API key.
ALTER TABLE jobs ADD COLUMN username TEXT;
ALTER TABLE audit_events ADD COLUMN actor_user TEXT;

CREATE INDEX jobs_user ON jobs (username, id);
