package server

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/theta42/native-ops/pkg/engine"
)

func (f *fixture) post(t *testing.T, path, token, contentType string, body []byte) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest("POST", f.srv.URL+path, bytes.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := res.Body.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return res, sb.String()
}

// planRecorder is a PlanFunc that records what it was given and returns a fixed plan.
type planRecorder struct {
	mu      sync.Mutex
	dirs    []string
	service []string
	fleet   string
	plan    *engine.FleetPlan
	err     error
	block   chan struct{} // when set, Plan waits for it
	started chan struct{}
}

func (p *planRecorder) fn(ctx context.Context, dir, service string) (*engine.FleetPlan, error) {
	p.mu.Lock()
	p.dirs = append(p.dirs, dir)
	p.service = append(p.service, service)
	b, _ := os.ReadFile(filepath.Join(dir, "fleet.yml"))
	p.fleet = string(b)
	p.mu.Unlock()
	if p.started != nil {
		p.started <- struct{}{}
	}
	if p.block != nil {
		<-p.block
	}
	return p.plan, p.err
}

func creatingPlan() *engine.FleetPlan {
	return &engine.FleetPlan{Services: []*engine.ServicePlan{{Service: "web", Action: engine.ActionCreate,
		Changes: []engine.Change{{Kind: engine.ChangeCreateInstance, Detail: "launch web"}}}}}
}

func goodTree(t *testing.T) []byte {
	return tgz(t, entry{name: "fleet.yml", body: "name: prod\n"}, entry{name: "services/web/service.yml", body: "image: x\n"})
}

func TestPlanEndpointPlansAnUploadedTreeAndCleansUp(t *testing.T) {
	rec := &planRecorder{plan: creatingPlan()}
	f := setupWith(t, time.Minute, nil, rec.fn)

	res, body := f.post(t, "/v1/plan?sha=abc1234&service=web", f.secret, "application/gzip", goodTree(t))
	if res.StatusCode != 200 {
		t.Fatalf("%d %s", res.StatusCode, body)
	}
	var out struct {
		Sha  string `json:"sha"`
		Hash string `json:"hash"`
		Exit int    `json:"exit"`
		Text string `json:"text"`
		Plan struct {
			Services []struct{ Service, Action string } `json:"services"`
		} `json:"plan"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	if out.Sha != "abc1234" || out.Exit != 2 || len(out.Hash) != 64 || out.Hash != creatingPlan().Hash() ||
		len(out.Plan.Services) != 1 || out.Plan.Services[0].Action != "create" || !strings.Contains(out.Text, "+ web (create)") {
		t.Fatalf("response: %s", body)
	}
	if rec.fleet != "name: prod\n" || rec.service[0] != "web" {
		t.Fatalf("plan must see the uploaded tree at its root and the service filter: %q %v", rec.fleet, rec.service)
	}
	if _, err := os.Stat(rec.dirs[0]); !os.IsNotExist(err) {
		t.Fatalf("the unpacked tree must be removed after the request, %s remains", rec.dirs[0])
	}
}

func TestPlanEndpointAcceptsATreeInsideOneTopLevelDirectory(t *testing.T) {
	rec := &planRecorder{plan: creatingPlan()}
	f := setupWith(t, time.Minute, nil, rec.fn)
	data := tgz(t, entry{name: "conf/", typ: tar.TypeDir}, entry{name: "conf/fleet.yml", body: "name: nested\n"})
	if res, body := f.post(t, "/v1/plan", f.secret, "application/gzip", data); res.StatusCode != 200 || rec.fleet != "name: nested\n" {
		t.Fatalf("%d %s %q", res.StatusCode, body, rec.fleet)
	}
	// Two top-level directories is ambiguous, not "pick one".
	two := tgz(t, entry{name: "a/fleet.yml", body: "x"}, entry{name: "b/fleet.yml", body: "y"})
	if res, _ := f.post(t, "/v1/plan", f.secret, "application/gzip", two); res.StatusCode != 400 {
		t.Fatalf("ambiguous roots must be refused, got %d", res.StatusCode)
	}
}

func TestPlanEndpointNeedsAPlannerToken(t *testing.T) {
	rec := &planRecorder{plan: creatingPlan()}
	f := setupWith(t, time.Minute, nil, rec.fn)
	for _, tok := range []string{"", "garbage"} {
		if res, _ := f.post(t, "/v1/plan", tok, "application/gzip", goodTree(t)); res.StatusCode != 401 {
			t.Errorf("token %q: %d, want 401", tok, res.StatusCode)
		}
	}
	if res, _ := f.post(t, "/v1/plan", f.viewer, "application/gzip", goodTree(t)); res.StatusCode != 403 {
		t.Errorf("a viewer must not upload a tree, got %d", res.StatusCode)
	}
	if len(rec.dirs) != 0 {
		t.Fatal("a refused request must never reach the plan, or even unpack anything")
	}
	// A planner (the pull-request pipeline's role) and anyone above it may plan.
	for name, tok := range map[string]string{"planner": f.planner, "admin": f.secret} {
		if res, body := f.post(t, "/v1/plan", tok, "application/gzip", goodTree(t)); res.StatusCode != 200 {
			t.Errorf("a %s must be able to plan, got %d %s", name, res.StatusCode, body)
		}
	}
	if res, _ := f.do(t, "GET", "/v1/plan", f.secret); res.StatusCode == 200 {
		t.Error("plan is a POST")
	}
}

func TestPlanEndpointIsOnlyServedWhenThereIsAPlanSource(t *testing.T) {
	f := setup(t, time.Minute, nil)
	if res, _ := f.post(t, "/v1/plan", f.secret, "application/gzip", goodTree(t)); res.StatusCode != 404 {
		t.Fatalf("no plan source configured, got %d", res.StatusCode)
	}
}

func TestPlanEndpointRefusesBadInputWithoutLeakingPaths(t *testing.T) {
	rec := &planRecorder{plan: creatingPlan()}
	f := setupWith(t, time.Minute, nil, rec.fn)
	tmp := os.TempDir()

	cases := []struct {
		name, path, ct string
		body           []byte
		status         int
		code           string
	}{
		{"service is not a name", "/v1/plan?service=../x", "application/gzip", goodTree(t), 400, "bad_request"},
		{"sha is not hex", "/v1/plan?sha=zzzz", "application/gzip", goodTree(t), 400, "bad_request"},
		{"sha too short", "/v1/plan?sha=abc", "application/gzip", goodTree(t), 400, "bad_request"},
		{"wrong content type", "/v1/plan", "application/json", goodTree(t), 415, "unsupported_media_type"},
		{"no content type", "/v1/plan", "", goodTree(t), 415, "unsupported_media_type"},
		{"not an archive", "/v1/plan", "application/gzip", []byte("name: f"), 400, "bad_archive"},
		{"path traversal", "/v1/plan", "application/gzip", tgz(t, entry{name: "../../etc/cron.d/x", body: "x"}), 400, "bad_archive"},
		{"symlink", "/v1/plan", "application/gzip", tgz(t, entry{name: "fleet.yml", body: "x"}, entry{name: "l", typ: tar.TypeSymlink, link: "/etc"}), 400, "bad_archive"},
		{"no fleet.yml", "/v1/plan", "application/gzip", tgz(t, entry{name: "services/web/service.yml", body: "x"}), 400, "bad_archive"},
	}
	for _, c := range cases {
		res, body := f.post(t, c.path, f.secret, c.ct, c.body)
		if res.StatusCode != c.status || !strings.Contains(body, `"code":"`+c.code+`"`) {
			t.Errorf("%s: %d %s, want %d %s", c.name, res.StatusCode, body, c.status, c.code)
		}
		if strings.Contains(body, tmp+"/native-ops-plan") || strings.Contains(body, "/var/") {
			t.Errorf("%s: the answer names a path on the host: %s", c.name, body)
		}
	}
	if len(rec.dirs) != 0 {
		t.Fatal("none of these may reach the plan")
	}
}

func TestPlanEndpointSeparatesBadConfigFromAnUnreadableHost(t *testing.T) {
	rec := &planRecorder{err: fmt.Errorf("%w: parse services/web/service.yml: yaml: line 3: bad", engine.ErrBadConfig)}
	f := setupWith(t, time.Minute, nil, rec.fn)
	res, body := f.post(t, "/v1/plan", f.secret, "application/gzip", goodTree(t))
	if res.StatusCode != 400 || !strings.Contains(body, "bad_config") || !strings.Contains(body, "line 3") {
		t.Fatalf("a manifest the caller wrote is theirs to read about: %d %s", res.StatusCode, body)
	}

	rec.err = errors.New("plan service web: command failed: incus list (stderr: /var/lib/incus/unix.socket: permission denied for user secretuser)")
	res, body = f.post(t, "/v1/plan", f.secret, "application/gzip", goodTree(t))
	if res.StatusCode != 502 || !strings.Contains(body, "host_unavailable") || strings.Contains(body, "secretuser") || strings.Contains(body, "socket") {
		t.Fatalf("host errors must be generic: %d %s", res.StatusCode, body)
	}
}

func TestPlanEndpointLimitsHowManyRunAtOnce(t *testing.T) {
	rec := &planRecorder{plan: creatingPlan(), block: make(chan struct{}), started: make(chan struct{}, maxConcurrentPlans+2)}
	f := setupWith(t, time.Minute, nil, rec.fn)

	var wg sync.WaitGroup
	for i := 0; i < maxConcurrentPlans; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			f.post(t, "/v1/plan", f.secret, "application/gzip", goodTree(t))
		}()
	}
	for i := 0; i < maxConcurrentPlans; i++ {
		select {
		case <-rec.started:
		case <-time.After(5 * time.Second):
			t.Fatal("plans did not start")
		}
	}
	res, body := f.post(t, "/v1/plan", f.secret, "application/gzip", goodTree(t))
	if res.StatusCode != 429 || res.Header.Get("Retry-After") == "" {
		t.Fatalf("one more than the limit: %d %s", res.StatusCode, body)
	}
	close(rec.block)
	wg.Wait()
	if res, _ := f.post(t, "/v1/plan", f.secret, "application/gzip", goodTree(t)); res.StatusCode != 200 {
		t.Fatalf("a slot must be free again, got %d", res.StatusCode)
	}
}

func TestPlanIsAuditedWithWhatItDidButNeverWithASecret(t *testing.T) {
	rec := &planRecorder{plan: creatingPlan()}
	f := setupWith(t, time.Minute, nil, rec.fn)
	f.post(t, "/v1/plan?sha=deadbeef&service=web", f.secret, "application/gzip", goodTree(t))
	f.post(t, "/v1/plan", f.secret, "application/gzip", []byte("junk"))

	raw, err := os.ReadFile(f.audit)
	if err != nil {
		t.Fatal(err)
	}
	log := string(raw)
	if !strings.Contains(log, `"path":"/v1/plan"`) || !strings.Contains(log, `"detail":"plan sha=deadbeef service=web exit=2 hash=`+creatingPlan().Hash()[:12]+`"`) {
		t.Fatalf("the audit log should say what the plan was and how it ended:\n%s", log)
	}
	if !strings.Contains(log, `"detail":"plan rejected: bad archive"`) {
		t.Fatalf("a rejected upload is audited too:\n%s", log)
	}
	if strings.Contains(log, f.secret) || strings.Contains(log, "sha=deadbeef&") || strings.Contains(log, "?") {
		t.Fatalf("the audit log must never hold a token or a query string:\n%s", log)
	}
}

func TestPlanEndpointStopsAnOversizedUploadEarly(t *testing.T) {
	defer func(a int64) { maxArchiveBytes = a }(maxArchiveBytes)
	maxArchiveBytes = 2048
	rec := &planRecorder{plan: creatingPlan()}
	f := setupWith(t, time.Minute, nil, rec.fn)
	big := make([]byte, 64<<10)
	rand.New(rand.NewSource(1)).Read(big) // incompressible, so the upload really is large
	res, body := f.post(t, "/v1/plan", f.secret, "application/gzip", tgz(t, entry{name: "fleet.yml", body: string(big)}))
	if res.StatusCode != 400 && res.StatusCode != 413 {
		t.Fatalf("an oversized upload must be refused, got %d %s", res.StatusCode, body)
	}
	if len(rec.dirs) != 0 {
		t.Fatal("it must not reach the plan")
	}
}
