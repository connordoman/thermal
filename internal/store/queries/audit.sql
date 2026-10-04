-- name: InsertAuditEvent :exec
INSERT INTO audit_events (at, actor_key_id, action, target, detail, client_ip)
VALUES (?, ?, ?, ?, ?, ?);

-- name: ListAuditEvents :many
SELECT * FROM audit_events
WHERE (action = sqlc.narg(action) OR sqlc.narg(action) IS NULL)
  AND (actor_key_id = sqlc.narg(actor_key_id) OR sqlc.narg(actor_key_id) IS NULL)
  AND (id < sqlc.narg(before_id) OR sqlc.narg(before_id) IS NULL)
ORDER BY id DESC
LIMIT sqlc.arg(limit);
