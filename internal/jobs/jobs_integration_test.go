package jobs_test

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/dorinpuscasu25/recapproject/internal/jobs"
	"github.com/dorinpuscasu25/recapproject/internal/testutil"
)

func status(t *testing.T, e *testutil.Env, id uuid.UUID) string {
	t.Helper()
	var s string
	if err := e.App.DB.Pool.QueryRow(e.Ctx, `SELECT status FROM jobs WHERE id=$1`, id).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

func runner(e *testutil.Env, name string, lease time.Duration, h jobs.Handler) *jobs.Runner {
	return &jobs.Runner{DB: e.App.DB, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Handlers: map[string]jobs.Handler{"test.durable": h},
		Lease: lease, WorkerID: name}
}

// A job running longer than its lease keeps it via heartbeats: a second worker never starts a
// duplicate execution.
func TestLongJobKeepsLeaseNoDuplicate(t *testing.T) {
	e := testutil.NewEnv(t)
	var runs atomic.Int32
	h := func(ctx context.Context, j *jobs.Job) (any, error) {
		runs.Add(1)
		select {
		case <-time.After(1500 * time.Millisecond):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return map[string]int{"ok": 1}, nil
	}
	id, _ := jobs.Enqueue(e.Ctx, e.App.DB.Pool, "test.durable", map[string]any{}, jobs.EnqueueOpts{})
	a, b := runner(e, "worker-a", 300*time.Millisecond, h), runner(e, "worker-b", 300*time.Millisecond, h)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); _, _ = a.RunOnce(e.Ctx) }()
	time.Sleep(200 * time.Millisecond)
	for i := 0; i < 10; i++ { // well past the 300 ms lease
		if ran, _ := b.RunOnce(e.Ctx); ran {
			t.Fatal("second worker claimed a job whose lease is being renewed")
		}
		time.Sleep(120 * time.Millisecond)
	}
	wg.Wait()
	if runs.Load() != 1 || status(t, e, id) != "succeeded" {
		t.Fatalf("runs=%d status=%s", runs.Load(), status(t, e, id))
	}
}

// A worker that dies mid-job leaves an expired lease; another worker resumes the job.
func TestCrashedWorkerJobIsRecovered(t *testing.T) {
	e := testutil.NewEnv(t)
	id, _ := jobs.Enqueue(e.Ctx, e.App.DB.Pool, "test.durable", map[string]any{}, jobs.EnqueueOpts{MaxAttempts: 3})
	if _, err := e.App.DB.Pool.Exec(e.Ctx, `UPDATE jobs SET status='processing', attempts=1, locked_by='dead-worker', locked_until=now()-interval '1 second' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	r := runner(e, "worker-b", time.Minute, func(context.Context, *jobs.Job) (any, error) { return "done", nil })
	if ran, err := r.RunOnce(e.Ctx); !ran || err != nil {
		t.Fatalf("recovery: ran=%v err=%v", ran, err)
	}
	var attempts int
	_ = e.App.DB.Pool.QueryRow(e.Ctx, `SELECT attempts FROM jobs WHERE id=$1`, id).Scan(&attempts)
	if status(t, e, id) != "succeeded" || attempts != 2 {
		t.Fatalf("status=%s attempts=%d", status(t, e, id), attempts)
	}
}

// If the lease is lost (here: taken over), the stale worker's handler is cancelled and its
// outcome is not written over the new owner's state.
func TestLostLeaseFencesOutcome(t *testing.T) {
	e := testutil.NewEnv(t)
	id, _ := jobs.Enqueue(e.Ctx, e.App.DB.Pool, "test.durable", map[string]any{}, jobs.EnqueueOpts{})
	cancelled := make(chan struct{})
	h := func(ctx context.Context, j *jobs.Job) (any, error) {
		_, _ = e.App.DB.Pool.Exec(e.Ctx, `UPDATE jobs SET locked_by='worker-new', attempts=attempts+1 WHERE id=$1`, j.ID) // takeover
		select {
		case <-ctx.Done():
			close(cancelled)
			return "stale result", nil
		case <-time.After(3 * time.Second):
			return "stale result", nil
		}
	}
	r := runner(e, "worker-old", 300*time.Millisecond, h)
	_, _ = r.RunOnce(e.Ctx)
	select {
	case <-cancelled:
	default:
		t.Fatal("handler was not cancelled after losing its lease")
	}
	var res *string
	_ = e.App.DB.Pool.QueryRow(e.Ctx, `SELECT result::text FROM jobs WHERE id=$1`, id).Scan(&res)
	if status(t, e, id) != "processing" || res != nil {
		t.Fatalf("stale outcome written: status=%s result=%v", status(t, e, id), res)
	}
}

// A running job can be cancelled; the handler stops and the job stays cancelled.
func TestCancelRunningJob(t *testing.T) {
	e := testutil.NewEnv(t)
	id, _ := jobs.Enqueue(e.Ctx, e.App.DB.Pool, "test.durable", map[string]any{}, jobs.EnqueueOpts{})
	started := make(chan struct{})
	stopped := make(chan struct{})
	h := func(ctx context.Context, j *jobs.Job) (any, error) {
		close(started)
		<-ctx.Done()
		close(stopped)
		return nil, ctx.Err()
	}
	r := runner(e, "worker-a", 300*time.Millisecond, h)
	go func() { _, _ = r.RunOnce(e.Ctx) }()
	<-started
	if err := jobs.Cancel(e.Ctx, e.App.DB.Pool, id); err != nil {
		t.Fatal(err)
	}
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("running job did not stop after cancellation")
	}
	time.Sleep(100 * time.Millisecond)
	if s := status(t, e, id); s != "cancelled" {
		t.Fatalf("status=%s", s)
	}
}
