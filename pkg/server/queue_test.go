package server

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

func TestHostQueueHandsTheHostOverInOrder(t *testing.T) {
	var q hostQueue
	first, err := q.enqueue()
	if err != nil || !first.granted || !<-first.ch {
		t.Fatalf("a free host is taken at once: %+v %v", first, err)
	}
	var waiting []*ticket
	for i := 0; i < 3; i++ {
		w, err := q.enqueue()
		if err != nil || w.granted {
			t.Fatalf("a busy host queues: %+v %v", w, err)
		}
		q.name(w, JobID(fmt.Sprintf("j-1000000000-0000000%d", i)))
		waiting = append(waiting, w)
	}
	if q.position(waiting[2].id) != 3 || q.waitingCount() != 3 {
		t.Fatalf("positions: %d of %d", q.position(waiting[2].id), q.waitingCount())
	}
	if _, ok := q.tryTicket(); ok {
		t.Fatal("a change that cannot wait must not jump the queue")
	}
	for i, w := range waiting {
		select {
		case <-w.ch:
			t.Fatalf("ticket %d was granted before its turn", i)
		default:
		}
		q.release()
		if !<-w.ch {
			t.Fatalf("ticket %d was not granted on its turn", i)
		}
	}
	q.release()
	if _, ok := q.tryTicket(); !ok {
		t.Fatal("the host is free once the last job releases it")
	}
}

func TestHostQueueIsBoundedAndClosingEndsTheWaiters(t *testing.T) {
	var q hostQueue
	q.enqueue()
	var waiting []*ticket
	for i := 0; i < maxQueuedJobs; i++ {
		w, err := q.enqueue()
		if err != nil {
			t.Fatalf("place %d: %v", i, err)
		}
		waiting = append(waiting, w)
	}
	if _, err := q.enqueue(); err != errQueueFull {
		t.Fatalf("beyond %d waiting: %v", maxQueuedJobs, err)
	}
	// Giving a place up makes room, and does not release the host.
	q.abandon(waiting[0])
	if _, ok := q.tryTicket(); ok {
		t.Fatal("abandoning a place in line must not free the host")
	}
	if _, err := q.enqueue(); err != nil {
		t.Fatalf("a place given up is free again: %v", err)
	}
	q.close()
	for i, w := range waiting[1:] {
		if <-w.ch {
			t.Fatalf("waiter %d must be told it will not run", i)
		}
	}
	if _, err := q.enqueue(); err != errQueueClosed {
		t.Fatalf("a closed queue takes nothing: %v", err)
	}
	q.release() // the running job ends after the close: nobody is handed the host
	if _, ok := q.tryTicket(); ok {
		t.Fatal("a closed queue hands out nothing")
	}
}

func TestAFullQueueIsATooManyRequestsWithRetryAfter(t *testing.T) {
	rig := newImageRig(t)
	rig.b.block, rig.b.started = make(chan struct{}), make(chan struct{}, maxQueuedJobs+2)
	defer close(rig.b.block)
	for i := 0; i <= maxQueuedJobs; i++ {
		if code, b := rig.build(t, rig.deployer, fmt.Sprintf("?app=a%d&ref=main", i), goodTree(t)); code != 202 {
			t.Fatalf("build %d: %d %s", i, code, b)
		}
	}
	res, b := rig.post(t, "/v1/images/build?app=late&ref=main", rig.deployer, "application/gzip", goodTree(t))
	if res.StatusCode != 429 || !strings.Contains(b, `"code":"queue_full"`) || res.Header.Get("Retry-After") == "" {
		t.Fatalf("a build beyond the queue: %d %s %v", res.StatusCode, b, res.Header)
	}
}

// A daemon that stops (to restart on an upgraded binary, say) lets the running job finish, and the jobs
// still waiting end as interrupted, saying nothing was done: they never start on a daemon going away.
func TestStoppingTheDaemonEndsQueuedJobsWithoutRunningThem(t *testing.T) {
	rig := newImageRig(t)
	rig.b.block, rig.b.started = make(chan struct{}), make(chan struct{}, 4)
	_, running := rig.build(t, rig.deployer, "?app=first&ref=main", goodTree(t))
	<-rig.b.started
	_, queued := rig.build(t, rig.deployer, "?app=second&ref=main", goodTree(t))

	rig.srv2.queue.close()
	j := rig.waitJob(t, jobID(t, queued), JobInterrupted)
	if j.Error != notStarted || j.Started != nil {
		t.Fatalf("a queued job on a stopping daemon: %+v", j)
	}
	close(rig.b.block)
	rig.waitJob(t, jobID(t, running), JobSucceeded)
	rig.b.mu.Lock()
	defer rig.b.mu.Unlock()
	if len(rig.b.calls) != 1 {
		t.Fatalf("the queued build must never run: %+v", rig.b.calls)
	}
	if _, err := os.Stat(rig.b.calls[0].dir); !os.IsNotExist(err) {
		t.Fatalf("the finished build's upload is removed: %v", err)
	}
}

// A job record left queued by a daemon that stopped is, after the restart, what it is: never started.
func TestAJobLeftQueuedByAStoppedDaemonIsInterrupted(t *testing.T) {
	dir := t.TempDir()
	jobs, err := OpenJobs(dir)
	if err != nil {
		t.Fatal(err)
	}
	q, _ := jobs.CreateQueued("image:build", "ci", "", "crew@main", "")
	r, _ := jobs.CreateKind("deploy", "ci", "", "deploy-1", "")

	again, err := OpenJobs(dir)
	if err != nil {
		t.Fatal(err)
	}
	if j, _ := again.Get(q.ID); j.Status != JobInterrupted || j.Error != notStarted {
		t.Fatalf("a queued job after a restart: %+v", j)
	}
	if j, _ := again.Get(r.ID); j.Status != JobInterrupted || !strings.Contains(j.Error, "part-way") {
		t.Fatalf("a running job after a restart: %+v", j)
	}
}

func TestAQueuedJobCanBeWaitedOnAndFilteredBy(t *testing.T) {
	rig := newImageRig(t)
	rig.b.block, rig.b.started = make(chan struct{}), make(chan struct{}, 4)
	_, running := rig.build(t, rig.deployer, "?app=first&ref=main", goodTree(t))
	<-rig.b.started
	_, queued := rig.build(t, rig.deployer, "?app=second&ref=main", goodTree(t))
	id := jobID(t, queued)

	res, b := rig.do(t, "GET", "/v1/jobs?status=queued", rig.deployer)
	var list struct{ Jobs []Job }
	_ = json.Unmarshal([]byte(b), &list)
	if res.StatusCode != 200 || len(list.Jobs) != 1 || string(list.Jobs[0].ID) != id || list.Jobs[0].Position != 1 {
		t.Fatalf("the queued jobs: %d %s", res.StatusCode, b)
	}
	// A long poll on a queued job waits for its end, through its turn.
	go func() { time.Sleep(150 * time.Millisecond); close(rig.b.block) }()
	start := time.Now()
	res, b = rig.do(t, "GET", "/v1/jobs/"+id+"?wait=10", rig.deployer)
	var j Job
	_ = json.Unmarshal([]byte(b), &j)
	if res.StatusCode != 200 || j.Status != JobSucceeded || time.Since(start) < 100*time.Millisecond {
		t.Fatalf("waiting on a queued job: %d %s", res.StatusCode, b)
	}
	rig.waitJob(t, jobID(t, running), JobSucceeded)
}

// The scheduled image retention never queues: a round with anything running or waiting is skipped.
func TestScheduledRetentionDoesNotJoinTheQueue(t *testing.T) {
	rig := newImageRig(t)
	rig.b.block, rig.b.started = make(chan struct{}), make(chan struct{}, 4)
	rig.build(t, rig.deployer, "?app=first&ref=main", goodTree(t))
	<-rig.b.started
	rig.srv2.pruneImagesNow()
	if n := rig.srv2.queue.waitingCount(); n != 0 {
		t.Fatalf("retention joined the queue: %d waiting", n)
	}
	close(rig.b.block)
	for _, j := range rig.srv2.opts.Jobs.List() {
		if j.Kind == "image:prune" {
			t.Fatalf("a skipped round records no job: %+v", j)
		}
	}
}
