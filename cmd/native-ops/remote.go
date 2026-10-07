package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/theta42/native-ops/pkg/engine"
	"github.com/theta42/native-ops/pkg/server"
)

// `native-ops remote ...` is the client side of the daemon, for CI: it packs the checked-out
// configuration tree, calls the daemon's API with a token, waits for the job and exits non-zero when
// it fails. It replaces the tar | curl | jq scripts a pipeline would otherwise carry, and nothing in it
// needs anything on the host but the daemon.
//
// The daemon's URL and token come from flags or the environment (NATIVE_OPS_URL, NATIVE_OPS_TOKEN).

const remoteUsage = `Usage: native-ops remote <action> [flags]

Actions (each uploads --config-dir unless noted, and waits for the job it starts):
  plan        [--service s]       print what apply would change; exits 1 if the plan is blocked
  apply       --expect <hash>     apply an approved plan
  deploy      --tag <tag>         deploy the commit a protected deploy tag points at; the daemon reads it
                                  from the git server, nothing is uploaded
  deploy-plan --tag <tag>         print what deploying the tag would change, changing nothing (planner;
                                  no upload); exits 1 if the plan is blocked, 2 if the tag would not deploy
  deploys     [--json]            what the host runs now and the recent deploys (no upload)
  backup      [--volume v] [--prune]
  restore     --volume v [--from key|latest] [--as name] [--force]   (admin)
  wait        <job-id>            wait for a job (no upload)
  recipe-approve [<digest>]       approve an image recipe; without a digest, the one of --config-dir (admin)
  secret-sync [--prune] NAME...   push these environment variables to the daemon's secret store, e.g.
                                  from the git server's secrets; --prune removes every other one (admin)
  secret-list                     the names in the daemon's secret store, never the values (admin)
  daemon-upgrade --version v --sha256 s
                                  upgrade the daemon to a pinned release and wait until it runs it (admin)
  image-prune [--dry-run]         delete images no instance runs that retention does not keep: orphans
                                  left by rebuilds, and old tags beyond --image-keep per app (deployer; no upload)
  token-create --name n --role r [--names g --images g --domains g --labels k=v1,v2 | --secrets g]   (admin; no upload)

Common flags: --url (NATIVE_OPS_URL), --token-env (default NATIVE_OPS_TOKEN), --config-dir (.),
              --sha (commit, recorded in the audit log; default GITHUB_SHA), --timeout (default 30m)`

type remoteClient struct {
	base  string
	token string
	http  *http.Client
}

func handleRemoteCommand(ctx context.Context, args []string) {
	if len(args) < 1 {
		fmt.Println(remoteUsage)
		os.Exit(1)
	}
	action := args[0]
	flags := flag.NewFlagSet("remote "+action, flag.ExitOnError)
	base := flags.String("url", os.Getenv("NATIVE_OPS_URL"), "The daemon's URL, e.g. https://native-ops.example.com (env NATIVE_OPS_URL)")
	tokenEnv := flags.String("token-env", "NATIVE_OPS_TOKEN", "The environment variable holding the API token")
	configDir := flags.String("config-dir", ".", "The configuration tree to upload")
	sha := flags.String("sha", os.Getenv("GITHUB_SHA"), "The commit being sent (recorded in the audit log)")
	timeout := flags.Duration("timeout", 30*time.Minute, "How long to wait for the job")
	service := flags.String("service", "", "plan: only this service")
	expect := flags.String("expect", "", "apply: the hash of the approved plan")
	tag := flags.String("tag", os.Getenv("GITHUB_REF_NAME"), "deploy: the deploy tag (default: the tag the CI job runs for)")
	volume := flags.String("volume", "", "backup/restore: the volume")
	prune := flags.Bool("prune", false, "backup: apply retention afterwards; secret-sync: remove every secret not named")
	from := flags.String("from", "latest", "restore: the object key, or latest")
	as := flags.String("as", "", "restore: restore under this new volume name")
	force := flags.Bool("force", false, "restore: stop the containers that mount the volume")
	name := flags.String("name", "", "token-create: the token's name")
	role := flags.String("role", "", "token-create: viewer, planner, deployer or admin")
	names := flags.String("names", "", "token-create: instance name globs, comma separated (a scoped token)")
	images := flags.String("images", "", "token-create: image globs, comma separated")
	secretGlobs := flags.String("secrets", "", "token-create: SERVICE_* secret name globs, comma separated (a token that may only sync those secrets)")
	domains := flags.String("domains", "", "token-create: domain globs, comma separated")
	labelScope := flags.String("labels", "", "token-create: limit to instances with these labels, key=v1,v2;key2=v3")
	upVersion := flags.String("version", "", "daemon-upgrade: the release tag, e.g. v1.56.0")
	upSHA := flags.String("sha256", "", "daemon-upgrade: the SHA-256 of native-ops_<version>_linux_<arch>.tar.gz, from the release's checksums.txt")
	dryRun := flags.Bool("dry-run", false, "image-prune: only report what would be deleted")
	asJSON := flags.Bool("json", false, "deploys: print the daemon's answer as JSON")
	prune2 := flags.Bool("prune-secrets", false, "secret-sync: remove every secret on the daemon not named here (also --prune)")
	_ = flags.Parse(args[1:])

	if *base == "" {
		fatalf("set --url or NATIVE_OPS_URL to the daemon's URL")
	}
	token := os.Getenv(*tokenEnv)
	if token == "" {
		fatalf("the %s environment variable (the API token) is not set", *tokenEnv)
	}
	c := &remoteClient{base: strings.TrimRight(*base, "/"), token: token, http: &http.Client{Timeout: 5 * time.Minute}}
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()

	q := url.Values{}
	if s := strings.ToLower(*sha); len(s) >= 7 {
		q.Set("sha", s)
	}
	switch action {
	case "plan":
		if *service != "" {
			q.Set("service", *service)
		}
		os.Exit(c.plan(ctx, *configDir, q))
	case "apply":
		if *expect == "" {
			fatalf("apply needs --expect <hash>: the plan that was reviewed and approved")
		}
		q.Set("expect", *expect)
		c.uploadAndWait(ctx, "/v1/apply", *configDir, q)
	case "deploy":
		if *tag == "" {
			fatalf("deploy needs --tag")
		}
		var out struct {
			Job struct {
				ID string `json:"id"`
			} `json:"job"`
		}
		if code, raw := c.json(ctx, "POST", "/v1/deploy", map[string]string{"tag": *tag}, &out); code != http.StatusAccepted {
			fatalf("the daemon answered %d: %s", code, raw)
		}
		fmt.Printf("deploying %s (job %s)\n", *tag, out.Job.ID)
		os.Exit(c.wait(ctx, out.Job.ID))
	case "deploy-plan":
		if *tag == "" {
			fatalf("deploy-plan needs --tag")
		}
		os.Exit(c.deployPlan(ctx, *tag))
	case "deploys":
		c.deploys(ctx, *asJSON)
	case "edge-apply", "dns-sync":
		fatalf("%s is retired: edge/Caddyfile and fleet.yml's dns_records are part of the plan; commit the change and push a deploy tag", action)
	case "backup":
		if *volume != "" {
			q.Set("volume", *volume)
		}
		if *prune {
			q.Set("prune", "1")
		}
		c.uploadAndWait(ctx, "/v1/backups", *configDir, q)
	case "restore":
		if *volume == "" {
			fatalf("restore needs --volume")
		}
		q.Set("volume", *volume)
		q.Set("from", *from)
		if *as != "" {
			q.Set("as", *as)
		}
		if *force {
			q.Set("force", "1")
		}
		c.uploadAndWait(ctx, "/v1/backups/restore", *configDir, q)
	case "recipe-approve":
		digest := flags.Arg(0)
		if digest == "" {
			d, err := engine.RecipeDigest(*configDir)
			if err != nil {
				fatalf("%v", err)
			}
			digest = d
		}
		var out map[string]any
		if code, raw := c.json(ctx, "POST", "/v1/images/recipes/"+url.PathEscape(digest)+"/approve", nil, &out); code != http.StatusOK {
			fatalf("the daemon answered %d: %s", code, raw)
		}
		fmt.Printf("approved image recipe %s\n", digest)
	case "secret-sync":
		// The values come from this process's environment (a CI job maps the git server's secrets
		// into it); they go to the daemon in one request and are never printed.
		set := map[string]string{}
		var missing []string
		for _, name := range flags.Args() {
			if v := os.Getenv(name); v != "" {
				set[name] = v
			} else {
				missing = append(missing, name)
			}
		}
		for _, m := range missing {
			fmt.Fprintf(os.Stderr, "notice: %s is empty here, so it is not sent\n", m)
		}
		doPrune := *prune || *prune2
		if len(set) == 0 && !doPrune {
			fatalf("no secret to send: name environment variables that are set, e.g. secret-sync DO_API_TOKEN")
		}
		var out struct {
			Changed []string `json:"changed"`
			Removed []string `json:"removed"`
		}
		if code, raw := c.json(ctx, "PUT", "/v1/secrets", map[string]any{"secrets": set, "prune": doPrune}, &out); code != http.StatusOK {
			fatalf("the daemon answered %d: %s", code, raw)
		}
		fmt.Printf("secrets synced: %d sent, changed: %s, removed: %s\n", len(set), listOrNone(out.Changed), listOrNone(out.Removed))
	case "daemon-upgrade":
		os.Exit(c.daemonUpgrade(ctx, *upVersion, strings.ToLower(*upSHA)))
	case "image-prune":
		path := "/v1/images/prune"
		if *dryRun {
			path += "?dry_run=1"
		}
		var out struct {
			Job struct {
				ID string `json:"id"`
			} `json:"job"`
		}
		if code, raw := c.json(ctx, "POST", path, nil, &out); code != http.StatusAccepted {
			fatalf("the daemon answered %d: %s", code, raw)
		}
		os.Exit(c.wait(ctx, out.Job.ID))
	case "secret-list":
		var out struct {
			Secrets []struct {
				Name  string `json:"name"`
				SetBy string `json:"set_by"`
				SetAt string `json:"set_at"`
			} `json:"secrets"`
		}
		if code, raw := c.json(ctx, "GET", "/v1/secrets", nil, &out); code != http.StatusOK {
			fatalf("the daemon answered %d: %s", code, raw)
		}
		for _, s := range out.Secrets {
			fmt.Printf("%-32s set by %s at %s\n", s.Name, s.SetBy, s.SetAt)
		}
	case "wait":
		if flags.NArg() < 1 {
			fatalf("wait needs a job id")
		}
		os.Exit(c.wait(ctx, flags.Arg(0)))
	case "token-create":
		body := map[string]any{"name": *name, "role": *role}
		if *secretGlobs != "" {
			body["scope"] = map[string]any{"secrets": splitList(*secretGlobs)}
		} else if *names != "" || *images != "" || *domains != "" || *labelScope != "" {
			scope := map[string]any{"names": splitList(*names), "images": splitList(*images), "domains": splitList(*domains)}
			if *labelScope != "" {
				parsed, err := server.ParseLabelScope(*labelScope)
				if err != nil {
					fatalf("%v", err)
				}
				scope["labels"] = parsed
			}
			body["scope"] = scope
		}
		var out struct {
			Secret string         `json:"secret"`
			Token  map[string]any `json:"token"`
		}
		if code, raw := c.json(ctx, "POST", "/v1/tokens", body, &out); code != http.StatusCreated {
			fatalf("the daemon answered %d: %s", code, raw)
		}
		fmt.Fprintf(os.Stderr, "created token %v (%v, role %v); the secret is shown only now:\n", out.Token["id"], out.Token["name"], out.Token["role"])
		fmt.Println(out.Secret)
	default:
		fmt.Fprintf(os.Stderr, "Unknown remote action: %s\n\n%s\n", action, remoteUsage)
		os.Exit(1)
	}
}

func fatalf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "error: "+format+"\n", a...)
	os.Exit(1)
}

// plan uploads the tree to /v1/plan and prints the plan. It returns the exit status: 1 when the plan is
// blocked (apply would fail), else 0. The hash is printed last, on its own line, for an apply.
func (c *remoteClient) plan(ctx context.Context, dir string, q url.Values) int {
	var out struct {
		Text string `json:"text"`
		Hash string `json:"hash"`
		Exit int    `json:"exit"`
	}
	code, raw := c.upload(ctx, "/v1/plan", dir, q, &out)
	if code != http.StatusOK {
		fatalf("the daemon answered %d: %s", code, raw)
	}
	fmt.Print(out.Text)
	if !strings.HasSuffix(out.Text, "\n") {
		fmt.Println()
	}
	fmt.Printf("plan %s\n", out.Hash)
	if out.Exit == 1 {
		fmt.Fprintln(os.Stderr, "the plan is blocked: apply would fail")
		return 1
	}
	return 0
}

// deployPlan prints what deploying a tag would change. It returns 1 when the plan is blocked, 2 when the
// tag would not deploy (no protection rule covers it), else 0.
func (c *remoteClient) deployPlan(ctx context.Context, tag string) int {
	var out struct {
		Sha        string `json:"sha"`
		Text       string `json:"text"`
		Hash       string `json:"hash"`
		Exit       int    `json:"exit"`
		Deployable bool   `json:"deployable"`
		Reason     string `json:"reason"`
		Daemon     struct {
			Running string `json:"running"`
			Pinned  string `json:"pinned"`
			Upgrade bool   `json:"upgrade"`
		} `json:"daemon"`
	}
	if code, raw := c.json(ctx, "POST", "/v1/deploy/plan", map[string]string{"tag": tag}, &out); code != http.StatusOK {
		fatalf("the daemon answered %d: %s", code, raw)
	}
	fmt.Printf("%s is commit %s\n", tag, out.Sha[:12])
	if out.Daemon.Upgrade {
		fmt.Printf("the deploy would first upgrade the daemon from %s to %s, as fleet.yml pins; this plan was made by %s\n", out.Daemon.Running, out.Daemon.Pinned, out.Daemon.Running)
	}
	printLog(out.Text)
	fmt.Printf("plan %s\n", out.Hash)
	switch {
	case out.Exit == 1:
		fmt.Fprintln(os.Stderr, "the plan is blocked: the deploy would fail")
		return 1
	case !out.Deployable:
		fmt.Fprintf(os.Stderr, "%s would not deploy: %s\n", tag, out.Reason)
		return 2
	}
	return 0
}

// deploys prints what the host runs and the recent deploys.
func (c *remoteClient) deploys(ctx context.Context, asJSON bool) {
	type deploy struct {
		Job     string         `json:"job"`
		Tag     string         `json:"tag"`
		Sha     string         `json:"sha"`
		Status  string         `json:"status"`
		Result  string         `json:"result"`
		Actor   string         `json:"actor"`
		Started time.Time      `json:"started"`
		Error   string         `json:"error"`
		Counts  map[string]int `json:"counts"`
	}
	var out struct {
		Current *deploy  `json:"current"`
		Running *deploy  `json:"running"`
		Deploys []deploy `json:"deploys"`
	}
	code, raw := c.json(ctx, "GET", "/v1/deploys?limit=20", nil, &out)
	if code != http.StatusOK {
		fatalf("the daemon answered %d: %s", code, raw)
	}
	if asJSON {
		fmt.Print(raw)
		return
	}
	short := func(sha string) string {
		if len(sha) > 12 {
			return sha[:12]
		}
		return firstNonEmpty(sha, "-")
	}
	if out.Current != nil {
		fmt.Printf("current: %s (%s), %s %s by %s\n", out.Current.Tag, short(out.Current.Sha), out.Current.Result, out.Current.Started.Local().Format("2006-01-02 15:04"), out.Current.Actor)
	} else {
		fmt.Println("current: none (no deploy has left the host matching its tag)")
	}
	if out.Running != nil {
		fmt.Printf("running: %s (job %s)\n", out.Running.Tag, out.Running.Job)
	}
	for _, d := range out.Deploys {
		outcome := firstNonEmpty(d.Result, d.Status)
		if d.Status != "succeeded" {
			outcome = d.Status
		}
		line := fmt.Sprintf("  %s  %-28s %-12s %-15s %s", d.Started.Local().Format("2006-01-02 15:04"), d.Tag, short(d.Sha), outcome, d.Job)
		if d.Error != "" {
			e := d.Error
			if len(e) > 80 {
				e = e[:77] + "..."
			}
			line += "  " + e
		}
		fmt.Println(line)
	}
}

// uploadAndWait uploads the tree to an endpoint that starts a job, then waits for the job. It exits.
func (c *remoteClient) uploadAndWait(ctx context.Context, path, dir string, q url.Values) {
	var out struct {
		Job struct {
			ID string `json:"id"`
		} `json:"job"`
		Status string `json:"status"`
		Text   string `json:"text"`
		Error  string `json:"error"`
	}
	code, raw := c.upload(ctx, path, dir, q, &out)
	switch {
	case code == http.StatusOK && out.Status == "nothing_to_do":
		fmt.Println("nothing to do: the host already matches")
		os.Exit(0)
	case code != http.StatusAccepted:
		if out.Text != "" {
			fmt.Fprint(os.Stderr, out.Text)
		}
		fatalf("the daemon answered %d: %s", code, firstNonEmpty(out.Error, raw))
	}
	fmt.Printf("job %s started\n", out.Job.ID)
	os.Exit(c.wait(ctx, out.Job.ID))
}

// wait polls a job until it ends and prints its log (when the token may read it). It returns 0 when the
// job succeeded. A deploy that upgrades the daemon ends as succeeded (ResultDaemonUpgraded) just before
// the daemon restarts, so the wait tolerates the restart: transport errors and 5xx (the proxy's 502 while
// the daemon is down) are retried for a few minutes rather than failing the deploy.
func (c *remoteClient) wait(ctx context.Context, id string) int {
	const maxTransient = 80 // ~4 minutes at 3s: the daemon restarts and resumes within seconds
	transient := 0
	for {
		var job struct {
			Status string `json:"status"`
			Error  string `json:"error"`
			Log    string `json:"log"`
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/v1/jobs/"+url.PathEscape(id)+"?wait=30", nil)
		if err != nil {
			fatalf("%v", err)
		}
		code, raw, err := c.try(req, &job)
		if err != nil || code >= 500 {
			transient++
			if transient > maxTransient {
				if err != nil {
					fatalf("reading job %s: %v", id, err)
				}
				fatalf("reading job %s: the daemon answered %d: %s", id, code, raw)
			}
			select {
			case <-ctx.Done():
				fmt.Fprintf(os.Stderr, "gave up waiting for job %s (the daemon is unreachable)\n", id)
				return 1
			case <-time.After(3 * time.Second):
			}
			continue
		}
		transient = 0
		if code != http.StatusOK {
			fatalf("reading job %s: the daemon answered %d: %s", id, code, raw)
		}
		switch job.Status {
		case "succeeded":
			printLog(job.Log)
			fmt.Printf("job %s succeeded\n", id)
			return 0
		case "failed", "interrupted":
			printLog(job.Log)
			fmt.Fprintf(os.Stderr, "job %s %s: %s\n", id, job.Status, job.Error)
			return 1
		}
		select {
		case <-ctx.Done():
			fmt.Fprintf(os.Stderr, "gave up waiting for job %s (it is still %s)\n", id, job.Status)
			return 1
		case <-time.After(3 * time.Second):
		}
	}
}

func printLog(log string) {
	if log != "" {
		fmt.Print(log)
		if !strings.HasSuffix(log, "\n") {
			fmt.Println()
		}
	}
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func (c *remoteClient) upload(ctx context.Context, path, dir string, q url.Values, out any) (int, string) {
	body, err := packTree(dir)
	if err != nil {
		fatalf("pack %s: %v", dir, err)
	}
	u := c.base + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, "POST", u, bytes.NewReader(body))
	if err != nil {
		fatalf("%v", err)
	}
	req.Header.Set("Content-Type", "application/gzip")
	return c.do(req, out)
}

func (c *remoteClient) json(ctx context.Context, method, path string, in, out any) (int, string) {
	var rd io.Reader
	if in != nil {
		b, _ := json.Marshal(in)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		fatalf("%v", err)
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return c.do(req, out)
}

func (c *remoteClient) do(req *http.Request, out any) (int, string) {
	code, raw, err := c.try(req, out)
	if err != nil {
		fatalf("%s %s: %v", req.Method, req.URL.Path, err)
	}
	return code, raw
}

// try is do without giving up on a request that does not reach the daemon, for the waits that expect it
// to be restarting.
func (c *remoteClient) try(req *http.Request, out any) (int, string, error) {
	req.Header.Set("Authorization", "Bearer "+c.token)
	res, err := c.http.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	if out != nil {
		_ = json.Unmarshal(raw, out)
	}
	return res.StatusCode, strings.TrimSpace(string(raw)), nil
}

// tryGet is a GET through try.
func (c *remoteClient) tryGet(ctx context.Context, path string, out any) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return 0, err
	}
	code, _, err := c.try(req, out)
	return code, err
}

// packTree makes the gzip-compressed tar the daemon accepts: the regular files and directories under
// dir, relative to it, without .git. A symbolic link is an error, since the daemon would refuse it.
func packTree(dir string) ([]byte, error) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil || rel == "." {
			return err
		}
		if d.IsDir() && d.Name() == ".git" {
			return filepath.SkipDir
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case info.Mode()&fs.ModeSymlink != 0:
			return fmt.Errorf("%s is a symbolic link; the daemon accepts only files and directories", rel)
		case !info.IsDir() && !info.Mode().IsRegular():
			return fmt.Errorf("%s is not a regular file", rel)
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		if info.IsDir() {
			hdr.Name += "/"
		}
		hdr.Uname, hdr.Gname, hdr.Uid, hdr.Gid = "", "", 0, 0
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(tw, f)
		return err
	})
	if err != nil {
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func listOrNone(l []string) string {
	if len(l) == 0 {
		return "none"
	}
	return strings.Join(l, ", ")
}

// daemonUpgrade asks the daemon to upgrade itself, waits for the install job, then waits for /healthz to
// report the new version (the daemon restarts after the job). If the old version comes back instead,
// the unit's guard rolled a binary back that could not stay up, and this says so.
func (c *remoteClient) daemonUpgrade(ctx context.Context, version, sha string) int {
	if version == "" || sha == "" {
		fatalf("daemon-upgrade needs --version and --sha256 (from the release's checksums.txt)")
	}
	var out struct {
		Job struct {
			ID string `json:"id"`
		} `json:"job"`
		From string `json:"from"`
	}
	if code, raw := c.json(ctx, "POST", "/v1/daemon/upgrade", map[string]string{"version": version, "sha256": sha}, &out); code != http.StatusAccepted {
		fatalf("the daemon answered %d: %s", code, raw)
	}
	fmt.Printf("upgrading the daemon from %s to %s (job %s)\n", out.From, version, out.Job.ID)

	// The daemon exits to restart on the new binary as soon as the install job succeeds, so from here a
	// request that cannot reach it means it is restarting, not that anything failed. Follow the job while
	// it answers; then wait for /healthz to report a version. The unit's guard gives a new binary a few
	// starts before it restores the old one, so the window is long enough to see either outcome.
	deadline := time.Now().Add(upgradeWindow)
	jobDone, last := false, ""
	for time.Now().Before(deadline) {
		if !jobDone {
			var job struct {
				Status string `json:"status"`
				Error  string `json:"error"`
				Log    string `json:"log"`
			}
			code, err := c.tryGet(ctx, "/v1/jobs/"+url.PathEscape(out.Job.ID), &job)
			switch {
			case err != nil || code == http.StatusServiceUnavailable || code == http.StatusBadGateway:
				fmt.Println("the daemon is restarting")
				jobDone = true
			case code != http.StatusOK:
				fmt.Fprintf(os.Stderr, "reading job %s: the daemon answered %d\n", out.Job.ID, code)
				return 1
			case job.Status == "succeeded":
				printLog(job.Log)
				jobDone = true
			case job.Status == "failed" || job.Status == "interrupted":
				printLog(job.Log)
				fmt.Fprintf(os.Stderr, "job %s %s: %s\n", out.Job.ID, job.Status, job.Error)
				return 1
			}
		}
		if jobDone {
			var h struct {
				Version string `json:"version"`
			}
			if code, err := c.tryGet(ctx, "/healthz", &h); err == nil && code == http.StatusOK && h.Version != "" {
				if h.Version == version {
					fmt.Printf("the daemon is running %s\n", version)
					return 0
				}
				last = h.Version
			}
		}
		select {
		case <-ctx.Done():
			return 1
		case <-time.After(upgradePoll):
		}
	}
	if last == out.From {
		fmt.Fprintf(os.Stderr, "the daemon is still on %s: the new binary did not stay up and was rolled back (see journalctl -u native-ops-serve)\n", last)
		return 1
	}
	fmt.Fprintf(os.Stderr, "the daemon did not report %s within %s\n", version, upgradeWindow)
	return 1
}

// How daemon-upgrade waits for the restart (variables so tests can shorten them).
var (
	upgradeWindow = 5 * time.Minute
	upgradePoll   = 3 * time.Second
)
