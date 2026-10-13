// Package jobs is a durable PostgreSQL-backed job runner for multi-step and long-running work
// (briefing generation, reprocessing, backfills, scheduled reports). Handlers are idempotent
// step functions so they can be ported 1:1 to Temporal activities (see ADR-006).
package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/dorinpuscasu25/recapproject/internal/platform/apperr"
	"github.com/dorinpuscasu25/recapproject/internal/platform/db"
	"github.com/dorinpuscasu25/recapproject/internal/platform/ids"
)

type Status string

const (
	Queued      Status = "queued"
	Processing  Status = "processing"
	Succeeded   Status = "succeeded"
	Failed      Status = "failed"
	Quarantined Status = "quarantined"
	Cancelled   Status = "cancelled"
)

type Job struct {
	ID             uuid.UUID       `json:"id"`
	Kind           string          `json:"kind"`
	Payload        json.RawMessage `json:"payload"`
	Status         Status          `json:"status"`
	Priority       int             `json:"priority"`
	Attempts       int             `json:"attempts"`
	MaxAttempts    int             `json:"max_attempts"`
	RunAt          time.Time       `json:"run_at"`
	IdempotencyKey *string         `json:"idempotency_key,omitempty"`
	LastError      *string         `json:"last_error,omitempty"`
	Result         json.RawMessage `json:"result,omitempty"`
	CreatedBy      *uuid.UUID      `json:"created_by,omitempty"`
	CreatedAt      time.Time       `json:"created_at"`
	UpdatedAt      time.Time       `json:"updated_at"`
	FinishedAt     *time.Time      `json:"finished_at,omitempty"`
}

// Handler executes a job. Returned result is stored as JSON. Return Permanent(err) to quarantine.
type Handler func(ctx context.Context, job *Job) (result any, err error)

type permanent struct{ error }

func Permanent(err error) error { return permanent{err} }

type EnqueueOpts struct {
	Priority       int
	MaxAttempts    int
	RunAt          time.Time
	IdempotencyKey string
	CreatedBy      *uuid.UUID
}

// Enqueue inserts a job using q (may be a transaction). With an idempotency key, an existing job
// with the same key is returned instead of creating a duplicate.
func Enqueue(ctx context.Context, q db.Querier, kind string, payload any, o EnqueueOpts) (uuid.UUID, error) {
	b, err := json.Marshal(payload)
	if err != nil {
		return uuid.Nil, err
	}
	if o.Priority == 0 {
		o.Priority = 100
	}
	if o.MaxAttempts == 0 {
		o.MaxAttempts = 5
	}
	if o.RunAt.IsZero() {
		o.RunAt = time.Now()
	}
	var key *string
	if o.IdempotencyKey != "" {
		key = &o.IdempotencyKey
	}
	var id uuid.UUID
	err = q.QueryRow(ctx, `INSERT INTO jobs (id, kind, payload, priority, max_attempts, run_at, idempotency_key, created_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		ON CONFLICT (idempotency_key) DO UPDATE SET updated_at = jobs.updated_at
		RETURNING id`, ids.New(), kind, b, o.Priority, o.MaxAttempts, o.RunAt, key, o.CreatedBy).Scan(&id)
	return id, err
}

const jobCols = `id, kind, payload, status, priority, attempts, max_attempts, run_at, idempotency_key, last_error, result, created_by, created_at, updated_at, finished_at`

func scanJob(row pgx.Row) (*Job, error) {
	var j Job
	err := row.Scan(&j.ID, &j.Kind, &j.Payload, &j.Status, &j.Priority, &j.Attempts, &j.MaxAttempts, &j.RunAt,
		&j.IdempotencyKey, &j.LastError, &j.Result, &j.CreatedBy, &j.CreatedAt, &j.UpdatedAt, &j.FinishedAt)
	if err != nil {
		return nil, err
	}
	return &j, nil
}

func Get(ctx context.Context, q db.Querier, id uuid.UUID) (*Job, error) {
	j, err := scanJob(q.QueryRow(ctx, `SELECT `+jobCols+` FROM jobs WHERE id=$1`, id))
	if db.IsNoRows(err) {
		return nil, apperr.NotFound("job_not_found", "Job not found.")
	}
	return j, err
}

func List(ctx context.Context, q db.Querier, kind, status string, limit int) ([]Job, error) {
	rows, err := q.Query(ctx, `SELECT `+jobCols+` FROM jobs WHERE ($1='' OR kind=$1) AND ($2='' OR status=$2)
		ORDER BY created_at DESC LIMIT $3`, kind, status, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *j)
	}
	return out, rows.Err()
}

// Retry requeues a failed/quarantined job.
func Retry(ctx context.Context, q db.Querier, id uuid.UUID) error {
	tag, err := q.Exec(ctx, `UPDATE jobs SET status='queued', attempts=0, run_at=now(), last_error=NULL, updated_at=now()
		WHERE id=$1 AND status IN ('failed','quarantined','cancelled')`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return apperr.Conflict("job_not_retryable", "Only failed, quarantined or cancelled jobs can be retried.")
	}
	return nil
}

func Cancel(ctx context.Context, q db.Querier, id uuid.UUID) error {
	tag, err := q.Exec(ctx, `UPDATE jobs SET status='cancelled', updated_at=now(), finished_at=now() WHERE id=$1 AND status='queued'`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return apperr.Conflict("job_not_cancellable", "Only queued jobs can be cancelled.")
	}
	return nil
}

var jobsProcessed = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "jobs_processed_total", Help: "Jobs processed by kind and outcome.",
}, []string{"kind", "outcome"})

func init() { prometheus.MustRegister(jobsProcessed) }

// Runner claims and executes jobs with bounded concurrency.
type Runner struct {
	DB           *db.DB
	Log          *slog.Logger
	Handlers     map[string]Handler
	Concurrency  int
	PollInterval time.Duration
	Lease        time.Duration
	workerID     string
}

func (r *Runner) init() {
	if r.Concurrency <= 0 {
		r.Concurrency = 2
	}
	if r.PollInterval == 0 {
		r.PollInterval = 500 * time.Millisecond
	}
	if r.Lease == 0 {
		r.Lease = 10 * time.Minute
	}
	if r.Log == nil {
		r.Log = slog.Default()
	}
	if r.workerID == "" {
		h, _ := os.Hostname()
		r.workerID = fmt.Sprintf("%s-%d", h, os.Getpid())
	}
}

func (r *Runner) kinds() []string {
	var k []string
	for name := range r.Handlers {
		k = append(k, name)
	}
	return k
}

func (r *Runner) Run(ctx context.Context) {
	r.init()
	var wg sync.WaitGroup
	for i := 0; i < r.Concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				ran, err := r.RunOnce(ctx)
				if err != nil && ctx.Err() == nil {
					r.Log.Error("job runner", "error", err)
				}
				if !ran {
					select {
					case <-ctx.Done():
					case <-time.After(r.PollInterval):
					}
				}
			}
		}()
	}
	wg.Wait()
}

// RunOnce claims one due job and runs it. Reports whether a job was executed.
func (r *Runner) RunOnce(ctx context.Context) (bool, error) {
	r.init()
	j, err := scanJob(r.DB.Pool.QueryRow(ctx, `
		WITH next AS (
			SELECT id FROM jobs
			WHERE kind = ANY($1) AND ((status='queued' AND run_at <= now()) OR (status='processing' AND locked_until < now()))
			ORDER BY priority, run_at LIMIT 1 FOR UPDATE SKIP LOCKED)
		UPDATE jobs j SET status='processing', attempts=j.attempts+1, locked_by=$2,
			locked_until=now()+$3::interval, updated_at=now()
		FROM next WHERE j.id = next.id
		RETURNING `+prefixed(jobCols), r.kinds(), r.workerID, fmt.Sprintf("%d seconds", int(r.Lease.Seconds()))))
	if db.IsNoRows(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	r.execute(ctx, j)
	return true, nil
}

func prefixed(cols string) string {
	// RETURNING inside UPDATE ... FROM needs the table alias to disambiguate.
	out := ""
	for i, c := range splitCols(cols) {
		if i > 0 {
			out += ", "
		}
		out += "j." + c
	}
	return out
}

func splitCols(s string) []string {
	var out []string
	cur := ""
	for _, ch := range s {
		switch ch {
		case ',':
			out = append(out, cur)
			cur = ""
		case ' ', '\n', '\t':
		default:
			cur += string(ch)
		}
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

func (r *Runner) execute(ctx context.Context, j *Job) {
	h := r.Handlers[j.Kind]
	log := r.Log.With("job_id", j.ID, "kind", j.Kind, "attempt", j.Attempts)
	var (
		res any
		err error
	)
	func() {
		defer func() {
			if p := recover(); p != nil {
				err = Permanent(fmt.Errorf("panic: %v", p))
			}
		}()
		res, err = h(ctx, j)
	}()
	if err == nil {
		b, _ := json.Marshal(res)
		jobsProcessed.WithLabelValues(j.Kind, "succeeded").Inc()
		_, uerr := r.DB.Pool.Exec(ctx, `UPDATE jobs SET status='succeeded', result=$2, last_error=NULL, locked_until=NULL,
			updated_at=now(), finished_at=now() WHERE id=$1`, j.ID, b)
		if uerr != nil {
			log.Error("mark job succeeded", "error", uerr)
		}
		return
	}
	var perm permanent
	if errors.As(err, &perm) || j.Attempts >= j.MaxAttempts {
		jobsProcessed.WithLabelValues(j.Kind, "quarantined").Inc()
		log.Error("job quarantined", "error", err)
		_, _ = r.DB.Pool.Exec(ctx, `UPDATE jobs SET status='quarantined', last_error=$2, locked_until=NULL, updated_at=now(), finished_at=now()
			WHERE id=$1`, j.ID, truncate(err.Error()))
		return
	}
	jobsProcessed.WithLabelValues(j.Kind, "retry").Inc()
	backoff := time.Duration(1<<min(j.Attempts, 8)) * 2 * time.Second
	log.Warn("job failed; retrying", "error", err, "backoff", backoff.String())
	_, _ = r.DB.Pool.Exec(ctx, `UPDATE jobs SET status='queued', last_error=$2, locked_until=NULL, run_at=now()+$3::interval, updated_at=now()
		WHERE id=$1`, j.ID, truncate(err.Error()), fmt.Sprintf("%d milliseconds", backoff.Milliseconds()))
}

func truncate(s string) string {
	if len(s) > 2000 {
		return s[:2000]
	}
	return s
}

// Drain runs due jobs until none remain (tests/CLI).
func (r *Runner) Drain(ctx context.Context, max int) error {
	for i := 0; i < max; i++ {
		ran, err := r.RunOnce(ctx)
		if err != nil {
			return err
		}
		if !ran {
			return nil
		}
	}
	return nil
}
