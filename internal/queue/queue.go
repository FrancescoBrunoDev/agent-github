package queue

import (
	"sync"

	"github.com/agent-github/agent/internal/worker"
)

// Queue is a FIFO job queue. Accepts jobs and dispatches them to the worker Manager
// as capacity allows.
type Queue struct {
	mu      sync.Mutex
	jobs    []worker.Job
	manager *worker.Manager
}

// New creates a Queue backed by the given worker Manager.
func New(manager *worker.Manager) *Queue {
	return &Queue{
		manager: manager,
	}
}

// Enqueue adds a job. If there is worker capacity, it starts immediately.
func (q *Queue) Enqueue(job worker.Job) error {
	q.mu.Lock()
	defer q.mu.Unlock()

	q.jobs = append(q.jobs, job)
	return q.tryDispatch()
}

// tryDispatch attempts to start as many queued jobs as workers allow.
// Failed jobs are dropped (not retried) — the webhook handler notifies the user.
func (q *Queue) tryDispatch() error {
	var lastErr error
	for q.manager.Active() < q.manager.Capacity() && len(q.jobs) > 0 {
		job := q.jobs[0]
		q.jobs = q.jobs[1:]

		if err := q.manager.Start(nil, job); err != nil {
			// Don't put back — drop and log. Next webhook re-deliver will retry.
			lastErr = err
			continue
		}
	}
	return lastErr
}

// Len returns the number of jobs waiting in the queue.
func (q *Queue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.jobs)
}

// List returns a snapshot of waiting jobs.
func (q *Queue) List() []worker.Job {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([]worker.Job, len(q.jobs))
	copy(out, q.jobs)
	return out
}

// IsActive checks if a job ID is currently being processed.
func (q *Queue) IsActive(jobID string) bool {
	return q.manager.IsActive(jobID)
}
