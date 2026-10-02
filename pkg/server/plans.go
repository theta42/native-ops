package server

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// PlanRecord is a plan the daemon has made, kept so people can look at it and an admin can approve it.
// It holds what the plan says (key names, never values), who asked for it, and whether it was approved
// and used. It is written to disk, so an approval survives a restart of the daemon.
type PlanRecord struct {
	Hash     string         `json:"hash"`
	Created  time.Time      `json:"created"`   // when this plan was first seen
	LastSeen time.Time      `json:"last_seen"` // when it was last made (the same plan is often made again)
	Actor    string         `json:"actor"`     // token name of the last caller
	Sha      string         `json:"sha,omitempty"`
	Service  string         `json:"service,omitempty"`
	Exit     int            `json:"exit"` // 0 nothing to change, 1 blocked, 2 changes pending
	Counts   map[string]int `json:"counts"`
	Text     string         `json:"text,omitempty"`
	Approval *Approval      `json:"approval,omitempty"`
	Used     *PlanUse       `json:"used,omitempty"`
}

// Approval is an admin's go-ahead for one plan hash. It is single-use and it expires.
type Approval struct {
	By      string    `json:"by"`
	At      time.Time `json:"at"`
	Expires time.Time `json:"expires"`
}

// PlanUse records the apply job that consumed an approval.
type PlanUse struct {
	Job JobID     `json:"job"`
	At  time.Time `json:"at"`
}

// PlanState is what a plan is waiting for.
type PlanState string

const (
	PlanNothing  PlanState = "nothing"  // nothing to change: there is nothing to approve
	PlanBlocked  PlanState = "blocked"  // apply would fail
	PlanPending  PlanState = "pending"  // changes, waiting for an admin to approve
	PlanApproved PlanState = "approved" // approved, not yet used, not expired
	PlanExpired  PlanState = "expired"  // approved once, too long ago
	PlanUsed     PlanState = "used"     // an apply already consumed the approval
	// PlanStale: changes, never applied, and made before the host last changed (see
	// Server.lastHostChange): it describes a host that no longer exists and cannot be applied.
	PlanStale PlanState = "stale"
)

// State reports where the plan stands at now.
func (p *PlanRecord) State(now time.Time) PlanState {
	switch {
	case p.Exit == 1:
		return PlanBlocked
	case p.Exit == 0:
		return PlanNothing
	case p.Used != nil:
		return PlanUsed
	case p.Approval == nil:
		return PlanPending
	case !now.Before(p.Approval.Expires):
		return PlanExpired
	}
	return PlanApproved
}

// Errors from the plan store, so a handler can answer precisely.
var (
	ErrNoSuchPlan     = errors.New("no such plan")
	ErrNotApprovable  = errors.New("this plan cannot be approved")
	ErrNotApproved    = errors.New("the plan has not been approved")
	ErrApprovalUsed   = errors.New("the approval was already used")
	ErrApprovalExpiry = errors.New("the approval has expired")
)

const keepPlans = 200

// Plans is the store of plan records: in memory, and one JSON file per plan hash in a 0700 directory.
type Plans struct {
	mu   sync.Mutex
	dir  string
	ttl  time.Duration
	now  func() time.Time
	byID map[string]*PlanRecord
}

// OpenPlans loads the records in dir (creating it). ttl is how long an approval lasts.
func OpenPlans(dir string, ttl time.Duration) (*Plans, error) {
	if ttl <= 0 {
		ttl = time.Hour
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, err
	}
	p := &Plans{dir: dir, ttl: ttl, now: func() time.Time { return time.Now().UTC() }, byID: map[string]*PlanRecord{}}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || filepath.Ext(name) != ".json" || !hashRe.MatchString(name[:len(name)-5]) {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		var rec PlanRecord
		if json.Unmarshal(b, &rec) != nil || rec.Hash != name[:len(name)-5] {
			continue
		}
		p.byID[rec.Hash] = &rec
	}
	p.prune()
	return p, nil
}

// TTL is how long an approval lasts.
func (p *Plans) TTL() time.Duration { return p.ttl }

func (p *Plans) persist(rec *PlanRecord) error {
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	tmp := filepath.Join(p.dir, "."+rec.Hash+".tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(p.dir, rec.Hash+".json"))
}

// prune keeps the newest keepPlans records. One that is approved and still usable is never dropped.
func (p *Plans) prune() {
	if len(p.byID) <= keepPlans {
		return
	}
	now := p.now()
	all := make([]*PlanRecord, 0, len(p.byID))
	for _, rec := range p.byID {
		all = append(all, rec)
	}
	sort.Slice(all, func(a, b int) bool { return all[a].LastSeen.After(all[b].LastSeen) })
	for _, rec := range all[keepPlans:] {
		if rec.State(now) == PlanApproved {
			continue
		}
		delete(p.byID, rec.Hash)
		_ = os.Remove(filepath.Join(p.dir, rec.Hash+".json"))
	}
}

// PlanSeen is what is recorded about a plan when it is made.
type PlanSeen struct {
	Hash    string
	Actor   string
	Sha     string
	Service string
	Exit    int
	Counts  map[string]int
	Text    string
}

// Record notes that a plan was made. The same hash made again only refreshes who asked and when; an
// approval already given to it is kept, unless an apply already used it (then the plan is new again).
func (p *Plans) Record(s PlanSeen) PlanRecord {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	rec := p.byID[s.Hash]
	if rec == nil {
		rec = &PlanRecord{Hash: s.Hash, Created: now}
		p.byID[s.Hash] = rec
	}
	if rec.Used != nil {
		// The same changes are pending again after an apply used their approval: that is a new occurrence
		// and needs its own approval, not the old one.
		rec.Used, rec.Approval, rec.Created = nil, nil, now
	}
	rec.LastSeen, rec.Actor, rec.Sha, rec.Service = now, s.Actor, s.Sha, s.Service
	rec.Exit, rec.Counts, rec.Text = s.Exit, s.Counts, s.Text
	_ = p.persist(rec)
	p.prune()
	return *rec
}

// Approve gives an admin's go-ahead to a plan that has changes and is not blocked. It lasts ttl
// from now and can be used once. Approving again renews an unused approval.
func (p *Plans) Approve(hash, by string) (PlanRecord, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	rec := p.byID[hash]
	if rec == nil {
		return PlanRecord{}, ErrNoSuchPlan
	}
	if rec.Exit != 2 {
		return PlanRecord{}, ErrNotApprovable
	}
	if rec.Used != nil {
		return PlanRecord{}, ErrApprovalUsed
	}
	now := p.now()
	rec.Approval = &Approval{By: by, At: now, Expires: now.Add(p.ttl)}
	if err := p.persist(rec); err != nil {
		return PlanRecord{}, err
	}
	return *rec, nil
}

// Revoke withdraws an approval that has not been used.
func (p *Plans) Revoke(hash string) (PlanRecord, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	rec := p.byID[hash]
	if rec == nil {
		return PlanRecord{}, ErrNoSuchPlan
	}
	if rec.Used != nil {
		return PlanRecord{}, ErrApprovalUsed
	}
	rec.Approval = nil
	if err := p.persist(rec); err != nil {
		return PlanRecord{}, err
	}
	return *rec, nil
}

// Check reports whether an apply of this plan hash may go ahead now, without using the approval.
func (p *Plans) Check(hash string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.checkLocked(hash)
}

func (p *Plans) checkLocked(hash string) error {
	rec := p.byID[hash]
	if rec == nil || rec.Approval == nil {
		return ErrNotApproved
	}
	if rec.Used != nil {
		return ErrApprovalUsed
	}
	if !p.now().Before(rec.Approval.Expires) {
		return ErrApprovalExpiry
	}
	return nil
}

// Consume uses the approval for one apply job. It succeeds once.
func (p *Plans) Consume(hash string, job JobID) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.checkLocked(hash); err != nil {
		return err
	}
	rec := p.byID[hash]
	rec.Used = &PlanUse{Job: job, At: p.now()}
	return p.persist(rec)
}

// UseForDeploy records that a deploy tag approved and used the plan, for job. Pushing a protected deploy
// tag is the approval (see handleDeploy), so the record says which tag and commit, and is used at once.
func (p *Plans) UseForDeploy(hash, by string, job JobID) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	rec := p.byID[hash]
	if rec == nil {
		return ErrNoSuchPlan
	}
	now := p.now()
	rec.Approval = &Approval{By: by, At: now, Expires: now}
	rec.Used = &PlanUse{Job: job, At: now}
	return p.persist(rec)
}

// Get returns a copy of a record.
func (p *Plans) Get(hash string) (PlanRecord, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	rec := p.byID[hash]
	if rec == nil {
		return PlanRecord{}, false
	}
	return *rec, true
}

// List returns the records, most recently made first, without their text.
func (p *Plans) List() []PlanRecord {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]PlanRecord, 0, len(p.byID))
	for _, rec := range p.byID {
		c := *rec
		c.Text = ""
		out = append(out, c)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].LastSeen.After(out[b].LastSeen) })
	return out
}
