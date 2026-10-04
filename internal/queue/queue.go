// Package queue stores print jobs in SQLite and prints them one at a time.
//
// Any number of requests can submit jobs concurrently; each is rendered and
// stored before the request returns, so jobs survive restarts and are
// auditable. A single worker drains the queue in priority order, because a
// printer can only print one job at a time.
package queue

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"sync"
	"time"

	"github.com/connordoman/escpos"
	"github.com/connordoman/thermal/internal/console"
	"github.com/connordoman/thermal/internal/device"
	"github.com/connordoman/thermal/internal/store"
	"github.com/connordoman/thermal/internal/store/dbq"
)

// Job statuses.
const (
	StatusQueued    = "queued"
	StatusPrinting  = "printing"
	StatusCompleted = "completed"
	StatusFailed    = "failed"
	StatusCanceled  = "canceled"
)

// Finished reports whether status is final.
func Finished(status string) bool {
	return status == StatusCompleted || status == StatusFailed || status == StatusCanceled
}

// Worker states.
const (
	StateIdle     = "idle"     // nothing to print
	StatePrinting = "printing" // sending a job
	StateWaiting  = "waiting"  // the printer is not ready (paper, cover, error)
	StateOffline  = "offline"  // the printer cannot be reached
)

// State describes what the worker is doing.
type State struct {
	State      string    `json:"state"`
	Detail     string    `json:"detail,omitempty"`
	CurrentJob int64     `json:"current_job,omitempty"`
	Since      time.Time `json:"since"`
}

// Queue is the job queue and its worker.
type Queue struct {
	store     *store.Store
	dev       *device.Device
	retention time.Duration

	wake chan struct{}

	mu      sync.Mutex
	waiters map[int64][]chan struct{}
	state   State
}

// New returns a queue that prints to dev. Job bodies older than retention
// are purged; zero keeps them forever.
func New(s *store.Store, dev *device.Device, retention time.Duration) *Queue {
	return &Queue{
		store:     s,
		dev:       dev,
		retention: retention,
		wake:      make(chan struct{}, 1),
		waiters:   map[int64][]chan struct{}{},
		state:     State{State: StateIdle, Since: time.Now().UTC()},
	}
}

// State returns what the worker is doing.
func (q *Queue) State() State {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.state
}

func (q *Queue) setState(state, detail string, job int64) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.state.State != state || q.state.Detail != detail || q.state.CurrentJob != job {
		if state != q.state.State || detail != q.state.Detail {
			switch state {
			case StateOffline, StateWaiting:
				console.Warn("printer %s: %s", state, detail)
			}
		}
		q.state = State{State: state, Detail: detail, CurrentJob: job, Since: time.Now().UTC()}
	}
}

// Wake prompts the worker to check for jobs.
func (q *Queue) Wake() {
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

// Submit stores a job and wakes the worker.
func (q *Queue) Submit(ctx context.Context, p dbq.CreateJobParams) (int64, error) {
	p.CreatedAt = store.Now()
	id, err := q.store.CreateJob(ctx, p)
	if err != nil {
		return 0, err
	}
	q.Wake()
	return id, nil
}

// Cancel cancels a job that has not started printing.
func (q *Queue) Cancel(ctx context.Context, id int64) (bool, error) {
	n, err := q.store.CancelJob(ctx, dbq.CancelJobParams{FinishedAt: sql.NullInt64{Int64: store.Now(), Valid: true}, ID: id})
	if n > 0 {
		q.notify(id)
	}
	return n > 0, err
}

// Wait blocks until job id is finished or ctx ends, then returns the job.
func (q *Queue) Wait(ctx context.Context, id int64) (dbq.GetJobRow, error) {
	ch := make(chan struct{})
	q.mu.Lock()
	q.waiters[id] = append(q.waiters[id], ch)
	q.mu.Unlock()
	defer func() {
		q.mu.Lock()
		defer q.mu.Unlock()
		ws := q.waiters[id]
		for i, c := range ws {
			if c == ch {
				q.waiters[id] = append(ws[:i], ws[i+1:]...)
				break
			}
		}
		if len(q.waiters[id]) == 0 {
			delete(q.waiters, id)
		}
	}()
	// Check after subscribing so a job finishing in between is not missed.
	job, err := q.store.GetJob(ctx, id)
	if err != nil || Finished(job.Status) {
		return job, err
	}
	select {
	case <-ch:
	case <-ctx.Done():
	}
	// Use a fresh context: the caller wants the latest state even on timeout.
	return q.store.GetJob(context.WithoutCancel(ctx), id)
}

func (q *Queue) notify(id int64) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, ch := range q.waiters[id] {
		close(ch)
	}
	delete(q.waiters, id)
}

// Run prints jobs until ctx ends.
func (q *Queue) Run(ctx context.Context) {
	if n, err := q.store.FailInterruptedJobs(ctx, sql.NullInt64{Int64: store.Now(), Valid: true}); err != nil {
		console.Error("marking interrupted jobs: %v", err)
	} else if n > 0 {
		console.Warn("marked %d job(s) interrupted by the last shutdown as failed", n)
	}
	go q.purgeLoop(ctx)

	backoff := time.Second
	for {
		wait, err := q.step(ctx)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			wait = backoff
			backoff = min(backoff*2, 30*time.Second)
		} else {
			backoff = time.Second
		}
		if wait == 0 {
			continue
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-q.wake:
		case <-t.C:
		}
		t.Stop()
	}
}

// step prints at most one job. It returns how long to wait before the next
// step (0 to continue at once), or an error to back off.
func (q *Queue) step(ctx context.Context) (time.Duration, error) {
	has, err := q.store.HasQueuedJobs(ctx)
	if err != nil {
		console.Error("checking queue: %v", err)
		return 0, err
	}
	if !has {
		q.setState(StateIdle, "", 0)
		return time.Minute, nil
	}

	readable, err := q.dev.Readable(ctx)
	if err != nil {
		q.setState(StateOffline, err.Error(), 0)
		return 0, err
	}
	if readable {
		sctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		st, err := q.dev.Status(sctx)
		cancel()
		switch {
		case err == nil && !st.Ready():
			q.setState(StateWaiting, st.String(), 0)
			return 3 * time.Second, nil
		case err != nil && !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, escpos.ErrNotReadable):
			q.setState(StateOffline, err.Error(), 0)
			return 0, err
		}
	}

	job, err := q.store.ClaimNextJob(ctx, sql.NullInt64{Int64: store.Now(), Valid: true})
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil // canceled in the meantime
	}
	if err != nil {
		console.Error("claiming job: %v", err)
		return 0, err
	}
	q.setState(StatePrinting, "", job.ID)
	// A job that has started is allowed to finish during shutdown.
	confirmed, err := q.print(context.WithoutCancel(ctx), job.ID, job.Payload, int(job.Copies), readable)

	status, msg := StatusCompleted, sql.NullString{}
	if err != nil {
		// Reopen the connection for the next job, even after a timeout:
		// the printer may have been unplugged or switched off.
		q.dev.Disconnect()
		status, msg = StatusFailed, sql.NullString{String: err.Error(), Valid: true}
		console.Error("job %d failed: %v", job.ID, err)
	} else {
		console.Info("job %d printed (%d bytes × %d)", job.ID, len(job.Payload), job.Copies)
	}
	conf := int64(0)
	if confirmed {
		conf = 1
	}
	// Record the outcome even if ctx was canceled mid-job.
	if ferr := q.store.FinishJob(context.WithoutCancel(ctx), dbq.FinishJobParams{
		Status: status, Error: msg, Confirmed: conf,
		FinishedAt: sql.NullInt64{Int64: store.Now(), Valid: true}, ID: job.ID,
	}); ferr != nil {
		console.Error("recording job %d: %v", job.ID, ferr)
	}
	q.notify(job.ID)
	q.setState(StateIdle, "", 0)
	return 0, nil
}

// print sends a job. If the connection can answer, the printer is asked to
// confirm it has processed everything (escpos.Printer.SendConfirmed), so
// "completed" means printed rather than merely sent.
func (q *Queue) print(ctx context.Context, id int64, payload []byte, copies int, readable bool) (confirmed bool, err error) {
	if payload == nil {
		return false, errors.New("job data has been purged")
	}
	data := bytes.Repeat(payload, max(copies, 1))
	// Generous: the printer accepts data roughly as fast as it prints.
	timeout := 30*time.Second + time.Duration(len(data)/10_000)*time.Second
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	err = q.dev.Do(ctx, func(p *escpos.Printer) error {
		if !readable {
			_, err := p.SendRaw(ctx, data)
			return err
		}
		err := p.SendConfirmed(ctx, data)
		if errors.Is(err, escpos.ErrUnconfirmed) {
			// Everything was sent; only the confirmation is missing.
			console.Warn("job %d: %v", id, err)
			return nil
		}
		confirmed = err == nil
		return err
	})
	return confirmed, err
}

func (q *Queue) purgeLoop(ctx context.Context) {
	if q.retention <= 0 {
		return
	}
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		now := time.Now()
		n, err := q.store.PurgeJobData(ctx, dbq.PurgeJobDataParams{
			Now:    sql.NullInt64{Int64: store.Millis(now), Valid: true},
			Before: sql.NullInt64{Int64: store.Millis(now.Add(-q.retention)), Valid: true},
		})
		if err != nil && ctx.Err() == nil {
			console.Error("purging old job data: %v", err)
		} else if n > 0 {
			console.Info("purged the bodies of %d job(s) older than %v", n, q.retention)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
