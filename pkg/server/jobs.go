package server

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// JobStatus is where an apply job is.
type JobStatus string

const (
	JobRunning     JobStatus = "running"
	JobSucceeded   JobStatus = "succeeded"
	JobFailed      JobStatus = "failed"
	JobInterrupted JobStatus = "interrupted" // the daemon stopped while it ran
)

// Job is one apply. It is written to disk as it changes, so the record of what was done to the host
// survives a restart of the daemon.
type Job struct {
	ID           JobID      `json:"id"`
	Kind         string     `json:"kind"`
	Status       JobStatus  `json:"status"`
	Actor        string     `json:"actor"`
	Sha          string     `json:"sha,omitempty"`
	Service      string     `json:"service,omitempty"`
	PlanHash     string     `json:"plan_hash"`
	Created      time.Time  `json:"created"`
	Finished     *time.Time `json:"finished,omitempty"`
	Error        string     `json:"error,omitempty"`
	Log          string     `json:"log,omitempty"`
	LogTruncated bool       `json:"log_truncated,omitempty"`
}

// JobID is j-<unix seconds>-<8 hex>. Anything else is never used to build a file name.
type JobID string

var jobIDRe = regexp.MustCompile(`^j-[0-9]{9,12}-[0-9a-f]{8}$`)

func validJobID(s string) bool { return jobIDRe.MatchString(s) }

const (
	maxJobLogBytes = 256 << 10
	keepJobs       = 200
)

// Jobs is the store of apply jobs: in memory, and one JSON file per job in a 0700 directory.
type Jobs struct {
	mu   sync.Mutex
	dir  string
	jobs map[JobID]*Job
}

// OpenJobs loads the jobs in dir (creating it). A job that was still running when the previous
// daemon stopped did not finish: it is marked interrupted, which is the truth about what state
// the host may be in, and the fix is to run the apply again (it converges and is safe to repeat).
func OpenJobs(dir string) (*Jobs, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, err
	}
	j := &Jobs{dir: dir, jobs: map[JobID]*Job{}}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		name := strings.TrimSuffix(e.Name(), ".json")
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") || !validJobID(name) {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var job Job
		if json.Unmarshal(b, &job) != nil || string(job.ID) != name {
			continue
		}
		if job.Status == JobRunning {
			now := time.Now().UTC()
			job.Status, job.Finished = JobInterrupted, &now
			job.Error = "the daemon stopped while this was running; the host may be part-way through it. Run the apply again: it converges and is safe to repeat."
			_ = j.persist(&job)
		}
		j.jobs[job.ID] = &job
	}
	j.prune()
	return j, nil
}

func (j *Jobs) persist(job *Job) error {
	b, err := json.Marshal(job)
	if err != nil {
		return err
	}
	tmp := filepath.Join(j.dir, "."+string(job.ID)+".tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(j.dir, string(job.ID)+".json"))
}

// prune keeps the newest keepJobs jobs. A running job is never removed.
func (j *Jobs) prune() {
	if len(j.jobs) <= keepJobs {
		return
	}
	all := make([]*Job, 0, len(j.jobs))
	for _, job := range j.jobs {
		all = append(all, job)
	}
	sort.Slice(all, func(a, b int) bool { return all[a].Created.After(all[b].Created) })
	for _, job := range all[keepJobs:] {
		if job.Status == JobRunning {
			continue
		}
		delete(j.jobs, job.ID)
		_ = os.Remove(filepath.Join(j.dir, string(job.ID)+".json"))
	}
}

func newJobID(now time.Time) JobID {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return JobID(fmt.Sprintf("j-%d-%s", now.Unix(), hex.EncodeToString(b[:])))
}

// Create records a new running job.
func (j *Jobs) Create(actor, sha, service, planHash string) (Job, error) {
	return j.CreateKind("apply", actor, sha, service, planHash)
}

// CreateKind records a new running job of the given kind ("apply", "instance:put", ...). service is the
// service or instance the job is about.
func (j *Jobs) CreateKind(kind, actor, sha, service, planHash string) (Job, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	now := time.Now().UTC()
	job := &Job{ID: newJobID(now), Kind: kind, Status: JobRunning, Actor: actor, Sha: sha, Service: service, PlanHash: planHash, Created: now}
	if err := j.persist(job); err != nil {
		return Job{}, err
	}
	j.jobs[job.ID] = job
	j.prune()
	return *job, nil
}

// Logf appends a timestamped line to a job's log (bounded), and saves it.
func (j *Jobs) Logf(id JobID, format string, a ...any) {
	j.mu.Lock()
	defer j.mu.Unlock()
	job := j.jobs[id]
	if job == nil || job.LogTruncated {
		return
	}
	line := time.Now().UTC().Format("15:04:05 ") + strings.TrimRight(fmt.Sprintf(format, a...), "\n") + "\n"
	if len(job.Log)+len(line) > maxJobLogBytes {
		job.LogTruncated = true
		job.Log += "... log truncated ...\n"
	} else {
		job.Log += line
	}
	_ = j.persist(job)
}

// Finish ends a job: succeeded when err is nil, else failed with the error.
func (j *Jobs) Finish(id JobID, err error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	job := j.jobs[id]
	if job == nil {
		return
	}
	now := time.Now().UTC()
	job.Finished = &now
	if err != nil {
		job.Status, job.Error = JobFailed, err.Error()
	} else {
		job.Status = JobSucceeded
	}
	_ = j.persist(job)
}

// Get returns a copy of a job.
func (j *Jobs) Get(id JobID) (Job, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	job := j.jobs[id]
	if job == nil {
		return Job{}, false
	}
	return *job, true
}

// List returns the jobs, newest first, without their logs.
func (j *Jobs) List() []Job {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := make([]Job, 0, len(j.jobs))
	for _, job := range j.jobs {
		c := *job
		c.Log = ""
		out = append(out, c)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Created.After(out[b].Created) })
	return out
}

// Running returns the job that is running, if any.
func (j *Jobs) Running() (Job, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	for _, job := range j.jobs {
		if job.Status == JobRunning {
			c := *job
			c.Log = ""
			return c, true
		}
	}
	return Job{}, false
}
