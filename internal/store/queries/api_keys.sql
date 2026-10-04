-- name: CreateAPIKey :exec
INSERT INTO api_keys (id, name, scopes, secret_hash, created_by, created_at, expires_at)
VALUES (?, ?, ?, ?, ?, ?, ?);

-- name: GetAPIKey :one
SELECT * FROM api_keys WHERE id = ?;

-- name: ListAPIKeys :many
SELECT * FROM api_keys
WHERE CAST(sqlc.arg(include_revoked) AS BOOLEAN) OR revoked_at IS NULL
ORDER BY created_at, id;

-- name: CountActiveAPIKeys :one
SELECT count(*) FROM api_keys WHERE revoked_at IS NULL;

-- name: UpdateAPIKey :exec
UPDATE api_keys SET name = ?, scopes = ?, expires_at = ? WHERE id = ?;

-- name: RotateAPIKey :exec
UPDATE api_keys
SET previous_secret_hash = CASE WHEN sqlc.narg(grace_until) IS NULL THEN NULL ELSE secret_hash END,
    previous_expires_at  = sqlc.narg(grace_until),
    secret_hash          = sqlc.arg(secret_hash),
    rotated_at           = sqlc.arg(rotated_at)
WHERE id = sqlc.arg(id);

-- name: RevokeAPIKey :execrows
UPDATE api_keys SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL;

-- name: TouchAPIKey :exec
UPDATE api_keys SET last_used_at = ? WHERE id = ?;
