-- name: CreateJob :one
INSERT INTO jobs (
    kind, priority, label, copies, api_key_id, username, client_ip, user_agent,
    content_type, source, source_size, payload, payload_size, payload_sha256,
    retry_of, created_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
RETURNING id;

-- name: GetJob :one
SELECT id, kind, status, priority, label, copies, api_key_id, username,
       client_ip, user_agent, content_type, source_size, payload_size, payload_sha256,
       attempts, error, confirmed, retry_of, created_at, started_at,
       finished_at, purged_at
FROM jobs WHERE id = ?;

-- name: ListJobs :many
SELECT id, kind, status, priority, label, copies, api_key_id, username,
       client_ip, user_agent, content_type, source_size, payload_size, payload_sha256,
       attempts, error, confirmed, retry_of, created_at, started_at,
       finished_at, purged_at
FROM jobs
WHERE (status = sqlc.narg(status) OR sqlc.narg(status) IS NULL)
  AND (kind = sqlc.narg(kind) OR sqlc.narg(kind) IS NULL)
  AND (api_key_id = sqlc.narg(api_key_id) OR sqlc.narg(api_key_id) IS NULL)
  AND (username = sqlc.narg(username) OR sqlc.narg(username) IS NULL)
  AND (id < sqlc.narg(before_id) OR sqlc.narg(before_id) IS NULL)
  AND (created_at >= sqlc.narg(since) OR sqlc.narg(since) IS NULL)
ORDER BY id DESC
LIMIT sqlc.arg(limit);

-- name: GetJobPayload :one
SELECT payload FROM jobs WHERE id = ?;

-- name: GetJobSource :one
SELECT source, content_type FROM jobs WHERE id = ?;

-- name: GetJobForRetry :one
SELECT kind, priority, label, copies, content_type, source, source_size,
       payload, payload_size, payload_sha256
FROM jobs WHERE id = ?;

-- name: HasQueuedJobs :one
SELECT EXISTS (SELECT 1 FROM jobs WHERE status = 'queued');

-- name: ClaimNextJob :one
UPDATE jobs
SET status = 'printing', started_at = sqlc.arg(started_at), attempts = attempts + 1
WHERE id = (
    SELECT id FROM jobs WHERE status = 'queued'
    ORDER BY priority DESC, id LIMIT 1
)
RETURNING id, payload, copies;

-- name: FinishJob :exec
UPDATE jobs
SET status = sqlc.arg(status), error = sqlc.narg(error),
    confirmed = sqlc.arg(confirmed), finished_at = sqlc.arg(finished_at)
WHERE id = sqlc.arg(id);

-- name: CancelJob :execrows
UPDATE jobs SET status = 'canceled', finished_at = ?
WHERE id = ? AND status = 'queued';

-- name: FailInterruptedJobs :execrows
UPDATE jobs
SET status = 'failed', error = 'interrupted: the server stopped while the job was printing', finished_at = ?
WHERE status = 'printing';

-- name: QueuePosition :one
-- The number of queued jobs that will print before the given one.
SELECT count(*) FROM jobs q, jobs j
WHERE j.id = sqlc.arg(id) AND q.status = 'queued' AND q.id != j.id
  AND (q.priority > j.priority OR (q.priority = j.priority AND q.id < j.id));

-- name: CountJobsByStatus :many
SELECT status, count(*) AS count FROM jobs GROUP BY status;

-- name: PurgeJobData :execrows
UPDATE jobs SET source = NULL, payload = NULL, purged_at = sqlc.arg(now)
WHERE purged_at IS NULL AND status IN ('completed', 'failed', 'canceled')
  AND finished_at < sqlc.arg(before);

-- name: ListQueuedJobs :many
-- Queued jobs in the order they will print.
SELECT id, kind, status, priority, label, copies, api_key_id, username,
       client_ip, user_agent, content_type, source_size, payload_size, payload_sha256,
       attempts, error, confirmed, retry_of, created_at, started_at,
       finished_at, purged_at
FROM jobs WHERE status = 'queued'
ORDER BY priority DESC, id
LIMIT ?;
