package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/connordoman/thermal/internal/queue"
	"github.com/connordoman/thermal/internal/store"
	"github.com/connordoman/thermal/internal/store/dbq"
	"github.com/gin-gonic/gin"
)

type jobJSON struct {
	ID            int64      `json:"id"`
	Kind          string     `json:"kind"`
	Status        string     `json:"status"`
	QueuePosition *int64     `json:"queue_position,omitempty"`
	Priority      int64      `json:"priority"`
	Label         string     `json:"label,omitempty"`
	Copies        int64      `json:"copies"`
	APIKeyID      string     `json:"api_key_id,omitempty"`
	Username      string     `json:"username,omitempty"`
	ClientIP      string     `json:"client_ip,omitempty"`
	UserAgent     string     `json:"user_agent,omitempty"`
	ContentType   string     `json:"content_type,omitempty"`
	SourceSize    int64      `json:"source_size"`
	PayloadSize   int64      `json:"payload_size"`
	PayloadSHA256 string     `json:"payload_sha256"`
	Attempts      int64      `json:"attempts"`
	Error         string     `json:"error,omitempty"`
	Confirmed     bool       `json:"confirmed"`
	RetryOf       *int64     `json:"retry_of,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	StartedAt     *time.Time `json:"started_at,omitempty"`
	FinishedAt    *time.Time `json:"finished_at,omitempty"`
	PurgedAt      *time.Time `json:"purged_at,omitempty"`
}

func jobFinished(status string) bool { return queue.Finished(status) }

func toJobJSON(j dbq.GetJobRow) jobJSON {
	out := jobJSON{
		ID: j.ID, Kind: j.Kind, Status: j.Status, Priority: j.Priority, Label: j.Label.String,
		Copies: j.Copies, APIKeyID: j.ApiKeyID.String, Username: j.Username.String, ClientIP: j.ClientIp.String,
		UserAgent: j.UserAgent.String, ContentType: j.ContentType.String,
		SourceSize: j.SourceSize, PayloadSize: j.PayloadSize, PayloadSHA256: j.PayloadSha256,
		Attempts: j.Attempts, Error: j.Error.String, Confirmed: j.Confirmed != 0,
		CreatedAt:  time.UnixMilli(j.CreatedAt).UTC(),
		StartedAt:  store.Time(j.StartedAt),
		FinishedAt: store.Time(j.FinishedAt),
		PurgedAt:   store.Time(j.PurgedAt),
	}
	if j.RetryOf.Valid {
		out.RetryOf = &j.RetryOf.Int64
	}
	return out
}

// jobJSON converts a job, adding its queue position if it is queued.
func (s *Server) jobJSON(ctx context.Context, j dbq.GetJobRow) jobJSON {
	out := toJobJSON(j)
	if j.Status == queue.StatusQueued {
		if n, err := s.Store.QueuePosition(ctx, j.ID); err == nil {
			out.QueuePosition = &n
		}
	}
	return out
}

func jobID(c *gin.Context) (int64, bool) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		abort(c, http.StatusBadRequest, "invalid_parameter", "job id must be a positive integer")
		return 0, false
	}
	return id, true
}

func notFoundOr(c *gin.Context, err error, what string) {
	if errors.Is(err, sql.ErrNoRows) {
		abort(c, http.StatusNotFound, "not_found", what+" not found")
		return
	}
	abort(c, http.StatusInternalServerError, "internal", err.Error())
}

func (s *Server) listJobs(c *gin.Context) {
	q := &query{c: c}
	limit := q.int("limit", 50, 1, 500)
	p := dbq.ListJobsParams{
		Status:   store.NullString(q.str("status", "")),
		Kind:     store.NullString(q.str("kind", "")),
		ApiKeyID: store.NullString(q.str("key", "")),
		Username: store.NullString(strings.ToLower(q.str("user", ""))),
		BeforeID: store.NullInt(int64(q.int("before", 0, 0, math.MaxInt))),
		Limit:    int64(limit),
	}
	if since := q.str("since", ""); since != "" {
		t, err := time.Parse(time.RFC3339, since)
		if err != nil {
			q.fail("since", "must be an RFC 3339 time such as 2026-10-03T09:00:00Z")
		}
		p.Since = sql.NullInt64{Int64: store.Millis(t), Valid: true}
	}
	if q.err != nil {
		renderError(c, q.err)
		return
	}
	rows, err := s.Store.ListJobs(c, p)
	if err != nil {
		abort(c, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	jobs := make([]jobJSON, len(rows))
	for i, r := range rows {
		jobs[i] = toJobJSON(dbq.GetJobRow(r))
	}
	resp := gin.H{"jobs": jobs}
	if len(rows) == limit {
		resp["next"] = nextPage(c, rows[len(rows)-1].ID)
	}
	c.JSON(http.StatusOK, resp)
}

// nextPage links to the page after the one being returned: the same
// request, filters and all, continuing before the last ID.
func nextPage(c *gin.Context, lastID int64) string {
	q := c.Request.URL.Query()
	q.Set("before", itoa(lastID))
	return c.Request.URL.Path + "?" + q.Encode()
}

func (s *Server) getJob(c *gin.Context) {
	id, ok := jobID(c)
	if !ok {
		return
	}
	j, err := s.Store.GetJob(c, id)
	if err != nil {
		notFoundOr(c, err, "job")
		return
	}
	c.JSON(http.StatusOK, gin.H{"job": s.jobJSON(c, j)})
}

func (s *Server) jobPayload(c *gin.Context) {
	id, ok := jobID(c)
	if !ok {
		return
	}
	p, err := s.Store.GetJobPayload(c, id)
	if err != nil {
		notFoundOr(c, err, "job")
		return
	}
	if p == nil {
		abort(c, http.StatusGone, "purged", "the job's data has been purged")
		return
	}
	c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=\"job-%d.bin\"", id))
	c.Data(http.StatusOK, "application/octet-stream", p)
}

func (s *Server) jobSource(c *gin.Context) {
	id, ok := jobID(c)
	if !ok {
		return
	}
	src, err := s.Store.GetJobSource(c, id)
	if err != nil {
		notFoundOr(c, err, "job")
		return
	}
	if src.Source == nil {
		abort(c, http.StatusGone, "purged", "the job's data has been purged")
		return
	}
	ct := src.ContentType.String
	if ct == "" {
		ct = "application/octet-stream"
	}
	// Never let a browser render stored request bodies as HTML.
	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("Content-Security-Policy", "sandbox")
	c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=\"job-%d-source\"", id))
	c.Data(http.StatusOK, ct, src.Source)
}

func (s *Server) cancelJob(c *gin.Context) {
	id, ok := jobID(c)
	if !ok {
		return
	}
	done, err := s.Queue.Cancel(c, id)
	if err != nil {
		abort(c, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	j, err := s.Store.GetJob(c, id)
	if err != nil {
		notFoundOr(c, err, "job")
		return
	}
	if !done {
		abortWith(c, http.StatusConflict, "not_cancelable", "only queued jobs can be canceled; this one is "+j.Status, gin.H{"status": j.Status})
		return
	}
	s.audit(c, "job.canceled", fmt.Sprint(id), nil)
	c.JSON(http.StatusOK, gin.H{"job": s.jobJSON(c, j)})
}

func (s *Server) retryJob(c *gin.Context) {
	id, ok := jobID(c)
	if !ok {
		return
	}
	q := &query{c: c}
	o := q.jobOptions(s.CutFeed)
	if q.err != nil {
		renderError(c, q.err)
		return
	}
	old, err := s.Store.GetJobForRetry(c, id)
	if err != nil {
		notFoundOr(c, err, "job")
		return
	}
	if old.Payload == nil {
		abort(c, http.StatusGone, "purged", "the job's data has been purged, so it cannot be retried")
		return
	}
	who := currentPrincipal(c)
	priority := old.Priority
	if q.has("priority") {
		priority = int64(o.priority)
	}
	newID, err := s.Queue.Submit(c, dbq.CreateJobParams{
		Kind: old.Kind, Priority: priority, Label: old.Label, Copies: old.Copies,
		ApiKeyID: store.NullString(who.keyID), Username: store.NullString(who.username()), ClientIp: store.NullString(c.ClientIP()),
		UserAgent: store.NullString(truncate(c.Request.UserAgent(), 512)), ContentType: old.ContentType,
		Source: old.Source, SourceSize: old.SourceSize, Payload: old.Payload,
		PayloadSize: old.PayloadSize, PayloadSha256: old.PayloadSha256,
		RetryOf: sql.NullInt64{Int64: id, Valid: true},
	})
	if err != nil {
		abort(c, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	s.audit(c, "job.retried", fmt.Sprint(id), gin.H{"new_job": newID})
	s.respondJob(c, newID, o.wait)
}

func (s *Server) queueSummary(c *gin.Context) {
	counts, err := s.Store.CountJobsByStatus(c)
	if err != nil {
		abort(c, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	byStatus := map[string]int64{
		queue.StatusQueued: 0, queue.StatusPrinting: 0, queue.StatusCompleted: 0,
		queue.StatusFailed: 0, queue.StatusCanceled: 0,
	}
	for _, r := range counts {
		byStatus[r.Status] = r.Count
	}
	queued, err := s.Store.ListQueuedJobs(c, 100)
	if err != nil {
		abort(c, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	pending := make([]jobJSON, len(queued))
	for i, j := range queued {
		pending[i] = toJobJSON(dbq.GetJobRow(j))
		pos := int64(i)
		pending[i].QueuePosition = &pos
	}
	c.JSON(http.StatusOK, gin.H{"worker": s.Queue.State(), "counts": byStatus, "queued": pending})
}
