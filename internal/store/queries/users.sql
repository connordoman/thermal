-- name: CreateUser :one
INSERT INTO users (username, password_hash, scopes, root, created_by, created_at, updated_at, password_changed_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
RETURNING *;

-- name: GetUser :one
SELECT * FROM users WHERE username = ?;

-- name: GetUserByID :one
SELECT * FROM users WHERE id = ?;

-- name: GetRootUser :one
SELECT * FROM users WHERE root = 1;

-- name: ListUsers :many
SELECT * FROM users
WHERE CAST(sqlc.arg(include_disabled) AS BOOLEAN) OR disabled_at IS NULL
ORDER BY created_at, id;

-- name: UpdateUser :exec
UPDATE users SET scopes = ?, disabled_at = ?, updated_at = ? WHERE id = ?;

-- name: SetUserPassword :exec
UPDATE users SET password_hash = ?, password_changed_at = ?, updated_at = ? WHERE id = ?;

-- name: TouchUserLogin :exec
UPDATE users SET last_login_at = ? WHERE id = ?;

-- name: CreateSession :exec
INSERT INTO sessions (token_hash, user_id, created_at, expires_at, last_seen_at, client_ip, user_agent)
VALUES (?, ?, ?, ?, ?, ?, ?);

-- name: GetSession :one
SELECT sqlc.embed(sessions), sqlc.embed(users)
FROM sessions JOIN users ON users.id = sessions.user_id
WHERE sessions.token_hash = ?;

-- name: TouchSession :exec
UPDATE sessions SET last_seen_at = ? WHERE token_hash = ?;

-- name: DeleteSession :exec
DELETE FROM sessions WHERE token_hash = ?;

-- name: DeleteUserSessions :execrows
DELETE FROM sessions WHERE user_id = sqlc.arg(user_id)
  AND (token_hash != sqlc.narg(keep) OR sqlc.narg(keep) IS NULL);

-- name: DeleteExpiredSessions :execrows
DELETE FROM sessions WHERE expires_at <= ?;
