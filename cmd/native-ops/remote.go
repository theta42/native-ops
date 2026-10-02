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
  edge-apply                      apply edge/Caddyfile to the edge container
  backup      [--volume v] [--prune]
  restore     --volume v [--from key|latest] [--as name] [--force]   (admin)
  dns-sync                        create or update fleet.yml's dns_records
  wait        <job-id>            wait for a job (no upload)
  token-create --name n --role r [--names g --images g --domains g]   (admin; no upload)

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
	volume := flags.String("volume", "", "backup/restore: the volume")
	prune := flags.Bool("prune", false, "backup: apply retention afterwards")
	from := flags.String("from", "latest", "restore: the object key, or latest")
	as := flags.String("as", "", "restore: restore under this new volume name")
	force := flags.Bool("force", false, "restore: stop the containers that mount the volume")
	name := flags.String("name", "", "token-create: the token's name")
	role := flags.String("role", "", "token-create: viewer, planner, deployer or admin")
	names := flags.String("names", "", "token-create: instance name globs, comma separated (a scoped token)")
	images := flags.String("images", "", "token-create: image globs, comma separated")
	domains := flags.String("domains", "", "token-create: domain globs, comma separated")
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
	case "edge-apply":
		c.uploadAndWait(ctx, "/v1/edge/apply", *configDir, q)
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
	case "dns-sync":
		c.uploadAndWait(ctx, "/v1/dns/sync", *configDir, q)
	case "wait":
		if flags.NArg() < 1 {
			fatalf("wait needs a job id")
		}
		os.Exit(c.wait(ctx, flags.Arg(0)))
	case "token-create":
		body := map[string]any{"name": *name, "role": *role}
		if *names != "" || *images != "" || *domains != "" {
			body["scope"] = map[string]any{"names": splitList(*names), "images": splitList(*images), "domains": splitList(*domains)}
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
// job succeeded.
func (c *remoteClient) wait(ctx context.Context, id string) int {
	for {
		var job struct {
			Status string `json:"status"`
			Error  string `json:"error"`
			Log    string `json:"log"`
		}
		code, raw := c.json(ctx, "GET", "/v1/jobs/"+url.PathEscape(id), nil, &job)
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
	req.Header.Set("Authorization", "Bearer "+c.token)
	res, err := c.http.Do(req)
	if err != nil {
		fatalf("%s %s: %v", req.Method, req.URL.Path, err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	if out != nil {
		_ = json.Unmarshal(raw, out)
	}
	return res.StatusCode, strings.TrimSpace(string(raw))
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
