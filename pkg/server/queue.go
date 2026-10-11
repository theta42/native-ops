package server

import (
	"errors"
	"sync"
)

// The host runs one change at a time: two jobs that both touch Incus, the edge or the images would step
// on each other, and a plan made while another change is half done describes a host that is about to
// change under it. Most changes do not need the host *now*, only in turn: an image build dispatched by
// one app's CI while another app's deploy runs should wait for it, not fail with "busy". So a change is
// queued, first come first served, and runs when the one before it ends.
//
// Two kinds of change cannot wait in line, and still take the host only when it is free (and nothing is
// waiting, so they never jump the queue):
//
//   - an apply, which compares the plan it makes on arrival with the hash an admin approved, and uses
//     that approval up: run later, it would apply a plan nobody saw against a host that has changed;
//   - a daemon upgrade, which restarts the daemon, and the daemon's own scheduled image retention, which
//     just skips a round.

// maxQueuedJobs bounds how many changes may wait for the host. A caller beyond it is told to retry.
const maxQueuedJobs = 20

var (
	errQueueFull   = errors.New("queue full")
	errQueueClosed = errors.New("queue closed")
)

// hostQueue hands the host to one job at a time, in the order they asked.
type hostQueue struct {
	mu      sync.Mutex
	busy    bool
	closed  bool
	waiting []*ticket
}

// ticket is one job's place in the queue. Its channel receives true when the job may run, or false when
// the daemon is stopping and the job will never run.
type ticket struct {
	id      JobID
	granted bool // the host was free when the ticket was issued: the job runs at once
	ch      chan bool
}

// tryTicket takes the host if it is free and nobody is waiting.
func (q *hostQueue) tryTicket() (*ticket, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed || q.busy || len(q.waiting) > 0 {
		return nil, false
	}
	q.busy = true
	t := &ticket{granted: true, ch: make(chan bool, 1)}
	t.ch <- true
	return t, true
}

// enqueue takes the host if it is free, else a place at the end of the queue. It refuses when the queue
// is full or the daemon is stopping.
func (q *hostQueue) enqueue() (*ticket, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	switch {
	case q.closed:
		return nil, errQueueClosed
	case !q.busy:
		q.busy = true
		t := &ticket{granted: true, ch: make(chan bool, 1)}
		t.ch <- true
		return t, nil
	case len(q.waiting) >= maxQueuedJobs:
		return nil, errQueueFull
	}
	t := &ticket{ch: make(chan bool, 1)}
	q.waiting = append(q.waiting, t)
	return t, nil
}

// name records which job holds a ticket, so the queue can say where a job is.
func (q *hostQueue) name(t *ticket, id JobID) {
	q.mu.Lock()
	t.id = id
	q.mu.Unlock()
}

// abandon gives up a ticket whose job could not be recorded: its place, or the host if it held it.
func (q *hostQueue) abandon(t *ticket) {
	q.mu.Lock()
	for i, w := range q.waiting {
		if w == t {
			q.waiting = append(q.waiting[:i], q.waiting[i+1:]...)
			q.mu.Unlock()
			return
		}
	}
	q.mu.Unlock()
	q.release()
}

// release hands the host to the next job in line, or frees it.
func (q *hostQueue) release() {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed || len(q.waiting) == 0 {
		q.busy = false
		return
	}
	next := q.waiting[0]
	q.waiting = q.waiting[1:]
	next.ch <- true
}

// close is for a daemon that is stopping: nothing new is queued, and every job still waiting is told it
// will not run (its record says so, and nothing was done). The job that holds the host finishes.
func (q *hostQueue) close() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.closed = true
	for _, t := range q.waiting {
		t.ch <- false
	}
	q.waiting = nil
}

// position is where a waiting job is in line (1 is next), or 0 when it is not waiting.
func (q *hostQueue) position(id JobID) int {
	q.mu.Lock()
	defer q.mu.Unlock()
	for i, t := range q.waiting {
		if t.id == id {
			return i + 1
		}
	}
	return 0
}

// waitingCount is how many jobs wait for the host.
func (q *hostQueue) waitingCount() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.waiting)
}
