// Package queue is an in-memory, bounded job queue with a fixed worker pool.
//
// There is no database (ADR 0003). The note file is the record, idempotency is a
// file-existence check, and a job lost to a restart costs one re-paste by a user who is
// watching for a reply.
package queue

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// Job is a unit of work. DisplayTitle is filled in after the fact once the transcript is
// fetched, so the acknowledgement can carry the real title rather than a URL.
type Job struct {
	VideoID string
	RawArg  string
	Channel string
	User    string
	// Reply is called exactly once per job: on failure with an error message, on success with
	// the finished summary. It must not block for long — it posts to Discord.
	Reply func(msg string)
	// Enqueued is set when the job is accepted, for logging.
	Enqueued time.Time
}

// Handler processes a job and returns an error if it failed. It reports success itself via
// job.Reply, since only the handler knows the summary it produced.
type Handler func(ctx context.Context, job Job) error

// Queue dispatches jobs to a bounded pool of workers.
type Queue struct {
	jobs    chan Job
	handle  Handler
	wg      sync.WaitGroup
	log     *slog.Logger
	closed  sync.Once
	started time.Time
}

// New creates a queue with the given depth and worker count.
//
// depth bounds how much work can be pending. Exceeding it is refused rather than buffered,
// because a deep queue on a shared inference host means work that won't finish for hours.
// workers is deliberately independent of depth: 2 concurrent jobs saturate the Mac Mini
// regardless of how many are waiting (ADR 0002).
func New(depth, workers int, handle Handler, log *slog.Logger) *Queue {
	if depth < 1 {
		depth = 1
	}
	if workers < 1 {
		workers = 1
	}
	q := &Queue{
		jobs:    make(chan Job, depth),
		handle:  handle,
		log:     log,
		started: time.Now(),
	}
	for i := 0; i < workers; i++ {
		q.wg.Add(1)
		go q.work(i + 1)
	}
	return q
}

// work is the worker loop. The queue knows nothing about skills; the handler is whatever
// capability is wired in, which is what keeps this package reusable (ADR 0005).
func (q *Queue) work(id int) {
	defer q.wg.Done()
	for job := range q.jobs {
		q.log.Info("job started",
			"worker", id, "video_id", job.VideoID, "channel", job.Channel)
		start := time.Now()

		if job.Reply == nil {
			q.log.Error("job has no reply func, cannot report result", "video_id", job.VideoID)
			continue
		}

		if err := q.handle(context.Background(), job); err != nil {
			q.log.Error("job failed",
				"video_id", job.VideoID, "err", err, "elapsed", time.Since(start))
			job.Reply("⚠️ " + job.VideoID + " failed: " + err.Error())
			continue
		}
		q.log.Info("job done",
			"worker", id, "video_id", job.VideoID, "elapsed", time.Since(start))
	}
}

// ErrQueueFull is returned when the pending depth is reached. It is not a failure of the
// request — the user can retry — so callers should surface it as a plain message.
var ErrQueueFull = errQueueFull{}

type errQueueFull struct{}

func (errQueueFull) Error() string { return "queue is full, try again shortly" }

// Submit enqueues a job, or returns ErrQueueFull if the queue is at depth.
//
// This is non-blocking by design. A blocking send would stall the Discord interaction handler
// and blow the 3-second acknowledgement deadline (ADR 0003).
func (q *Queue) Submit(job Job) error {
	job.Enqueued = time.Now()
	select {
	case q.jobs <- job:
		q.log.Info("job queued",
			"video_id", job.VideoID, "pending", len(q.jobs), "depth", cap(q.jobs))
		return nil
	default:
		return ErrQueueFull
	}
}

// Pending reports how many jobs are waiting. Logged, not surfaced — it exists for the boot
// log line and for diagnostics.
func (q *Queue) Pending() int { return len(q.jobs) }

// StartedAt reports when the queue was created. Logged at boot so the process lifetime is
// visible in the log; a gap between runs is the trace of a restart that may have dropped an
// in-flight job (ADR 0003).
func (q *Queue) StartedAt() time.Time { return q.started }

// Close drains the queue and waits for in-flight jobs to finish.
//
// In-flight jobs that do not complete are logged by the worker's error path. On a hard kill
// they are simply gone, which the boot log records — the accepted cost of no database.
func (q *Queue) Close(timeout time.Duration) {
	q.closed.Do(func() { close(q.jobs) })

	done := make(chan struct{})
	go func() { q.wg.Wait(); close(done) }()

	select {
	case <-done:
		q.log.Info("queue drained cleanly")
	case <-time.After(timeout):
		q.log.Warn("queue drain timed out, some jobs were abandoned",
			"timeout", timeout, "note", "re-paste the link to retry")
	}
}
