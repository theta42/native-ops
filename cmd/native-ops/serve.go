package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/theta42/native-ops/pkg/engine"
	"github.com/theta42/native-ops/pkg/provider"
	"github.com/theta42/native-ops/pkg/provider/digitalocean"
	"github.com/theta42/native-ops/pkg/remote"
	"github.com/theta42/native-ops/pkg/server"
	"github.com/theta42/native-ops/pkg/status"
)

func handleStatusCommand(ctx context.Context, args []string) {
	flags := flag.NewFlagSet("status", flag.ExitOnError)
	asJSON := flags.Bool("json", false, "Print the snapshot as JSON")
	pool := flags.String("pool", "default", "Storage pool holding the data volumes")
	_ = flags.Parse(args)

	snap, err := status.Collect(ctx, remote.NewLocalExecutor(), *pool)
	if err != nil {
		log.Fatalf("status: %v", err)
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(snap)
		return
	}
	printStatus(snap)
}

func printStatus(s *status.Snapshot) {
	fmt.Printf("%s  incus %s  pool %s  (%s)\n\n", s.Host, s.Incus, s.Pool, s.Time.Format("2006-01-02 15:04:05Z"))
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "INSTANCES (%d)\tSTATUS\tIMAGE\tIPV4\tVOLUMES\tSNAPS\n", len(s.Instances))
	for _, i := range s.Instances {
		img := i.Recorded["image"]
		if img == "" {
			img = i.Image
		}
		if len(img) > 44 {
			img = img[:43] + "…"
		}
		var vols []string
		for _, d := range i.Devices {
			if d.Type == "disk" && d.Source != "" {
				v := d.Source
				if d.HostPath {
					v += " (host path)"
				}
				vols = append(vols, v)
			}
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%d\n", i.Name, i.Status, img, strings.Join(i.IPv4, ","), strings.Join(vols, ","), i.Snapshots)
	}
	fmt.Fprintf(w, "\nVOLUMES (%d)\tUSED BY\tSHIFTED\tSNAPS\tLATEST DAILY\t\n", len(s.Volumes))
	for _, v := range s.Volumes {
		used := strings.Join(v.UsedBy, ",")
		if used == "" {
			used = "-"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%s\t\n", v.Name, used, v.Shifted, v.Snapshots, v.LatestDaily)
	}
	_ = w.Flush()
	fmt.Printf("\nimages: %d (%d MB), %d without an alias\n", s.Images.Count, s.Images.TotalBytes/1_000_000, s.Images.Unaliased)
	if len(s.Warnings) > 0 {
		fmt.Printf("\n%d thing(s) to look at:\n", len(s.Warnings))
		for _, m := range s.Warnings {
			fmt.Printf("  - %s\n", m)
		}
	}
}

func stateDirFlag(flags *flag.FlagSet) *string {
	return flags.String("state-dir", "/var/lib/native-ops", "Directory for the daemon's tokens and audit log")
}

func openStateDir(dir string) string {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		log.Fatalf("state dir %s: %v", dir, err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		log.Fatalf("state dir %s: %v", dir, err)
	}
	return dir
}

func handleServeCommand(ctx context.Context, args []string) {
	flags := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := flags.String("addr", "127.0.0.1:8686", "Listen address (put TLS in front of it, e.g. the Caddy edge)")
	pool := flags.String("pool", "default", "Storage pool holding the data volumes")
	enableApply := flags.Bool("enable-apply", false, "Serve POST /v1/apply and /v1/jobs (off by default: without it the daemon can read and plan, never change). An apply also needs an admin's approval of the plan")
	approvalTTL := flags.Duration("approval-ttl", time.Hour, "How long an admin's approval of a plan lasts")
	enableInstances := flags.Bool("enable-instances", false, "Serve the tenant-instance endpoints (PUT/DELETE /v1/instances/{name}); a token with a scope may use only these")
	instanceProfiles := flags.String("instance-profiles", "base,service", "Incus profiles a tenant instance spec may use")
	instanceImports := flags.String("instance-route-imports", "", "Caddy snippets a tenant route may import, e.g. strip-forged-identity")
	enableImageBuild := flags.Bool("enable-image-build", false, "Serve POST /v1/images/build; a token with a scope may build only the images it allows")
	enableEdgeApply := flags.Bool("enable-edge-apply", false, "Serve POST /v1/edge/apply: apply the config repo's edge/Caddyfile to the edge container (validated, with rollback). Changes the host, so it runs as a job")
	// Auth flags default from the environment, so the OIDC client secret and the rest can live in the
	// root-only /etc/native-ops/serve.env (like NATIVE_OPS_BOOTSTRAP_TOKEN) rather than a unit file an
	// unprivileged user on the host could read.
	enableAuth := flags.Bool("enable-auth", envBool("NATIVE_OPS_ENABLE_AUTH"), "Serve local sign-in and session cookies for the UI (users.json in the state dir; manage people with `native-ops user create`). Env: NATIVE_OPS_ENABLE_AUTH=1")
	oidcIssuer := flags.String("oidc-issuer", os.Getenv("NATIVE_OPS_OIDC_ISSUER"), "OpenID Connect issuer URL (e.g. https://accounts.google.com); enables OIDC sign-in. Env: NATIVE_OPS_OIDC_ISSUER")
	oidcClientID := flags.String("oidc-client-id", os.Getenv("NATIVE_OPS_OIDC_CLIENT_ID"), "OIDC client id. Env: NATIVE_OPS_OIDC_CLIENT_ID")
	oidcClientSecret := flags.String("oidc-client-secret", os.Getenv("NATIVE_OPS_OIDC_CLIENT_SECRET"), "OIDC client secret. Env: NATIVE_OPS_OIDC_CLIENT_SECRET")
	oidcRedirectURL := flags.String("oidc-redirect-url", os.Getenv("NATIVE_OPS_OIDC_REDIRECT_URL"), "OIDC redirect URL (e.g. https://native-ops.example/auth/oidc/callback). Env: NATIVE_OPS_OIDC_REDIRECT_URL")
	oidcAllowedDomain := flags.String("oidc-allowed-domain", os.Getenv("NATIVE_OPS_OIDC_ALLOWED_DOMAIN"), "Only allow sign-in from emails at this domain (e.g. opsavor.ai). Env: NATIVE_OPS_OIDC_ALLOWED_DOMAIN")
	oidcRole := flags.String("oidc-role", envOr("NATIVE_OPS_OIDC_ROLE", "viewer"), "Role a newly seen OIDC user gets: viewer, planner, deployer or admin. Env: NATIVE_OPS_OIDC_ROLE")
	oidcLabel := flags.String("oidc-label", envOr("NATIVE_OPS_OIDC_LABEL", "single sign-on"), "Text on the sign-in button. Env: NATIVE_OPS_OIDC_LABEL")
	edgeContainer := flags.String("edge-container", envOr("NATIVE_OPS_EDGE_CONTAINER", "edge"), "Incus container running the edge (Caddy): its routes and certificates are reported. Empty to skip. Env: NATIVE_OPS_EDGE_CONTAINER")
	dnsProviderFlag := flags.String("dns-provider", envOr("NATIVE_OPS_DNS_PROVIDER", ""), "DNS provider to list records from, e.g. digitalocean (needs DO_API_TOKEN in the environment). Env: NATIVE_OPS_DNS_PROVIDER")
	dnsDomains := flags.String("dns-domains", envOr("NATIVE_OPS_DNS_DOMAINS", ""), "Comma-separated zones to list DNS records for, e.g. 'opsavor.app,opsavor.work'. Env: NATIVE_OPS_DNS_DOMAINS")
	stateDir := stateDirFlag(flags)
	_ = flags.Parse(args)

	var dnsProv provider.DNSProvider
	if *dnsProviderFlag != "" {
		switch *dnsProviderFlag {
		case "digitalocean", "do":
			do, err := digitalocean.New("")
			if err != nil {
				log.Fatalf("dns provider: %v", err)
			}
			dnsProv = do
		default:
			log.Fatalf("unknown --dns-provider %q", *dnsProviderFlag)
		}
	}
	srv, closeFn, err := newDaemon(daemonConfig{
		Addr: *addr, Pool: *pool, StateDir: openStateDir(*stateDir), EnableApply: *enableApply, ApprovalTTL: *approvalTTL, EnableInstances: *enableInstances,
		InstancePolicy:   engine.InstancePolicy{Profiles: splitList(*instanceProfiles), RouteImports: splitList(*instanceImports)},
		EnableImageBuild: *enableImageBuild,
		EnableEdgeApply:  *enableEdgeApply,
		EnableAuth:       *enableAuth,
		OIDC: server.OIDCSettings{Issuer: *oidcIssuer, ClientID: *oidcClientID, ClientSecret: *oidcClientSecret,
			RedirectURL: *oidcRedirectURL, AllowedDomain: *oidcAllowedDomain, Role: server.Role(*oidcRole), Label: *oidcLabel},
		EdgeContainer: *edgeContainer,
		DNS:           dnsProv,
		DNSDomains:    splitList(*dnsDomains),
		Exec:             remote.NewLocalExecutor(), BootstrapToken: os.Getenv("NATIVE_OPS_BOOTSTRAP_TOKEN"),
	})
	if err != nil {
		log.Fatal(err)
	}
	defer closeFn()

	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	log.Printf("native-ops %s serving on http://%s (state: %s, apply: %v)", Version, *addr, *stateDir, *enableApply)
	if err := srv.ListenAndServe(ctx); err != nil {
		log.Fatalf("serve: %v", err)
	}
	log.Printf("stopped")
}

type daemonConfig struct {
	Addr, Pool, StateDir string
	EnableApply          bool
	EnableInstances      bool
	InstancePolicy       engine.InstancePolicy
	EnableImageBuild     bool
	EnableEdgeApply      bool
	ApprovalTTL          time.Duration
	Exec                 remote.Executor
	BootstrapToken       string
	// EnableAuth serves local sign-in and session cookies for the UI. OIDC (an Issuer) adds generic
	// OpenID Connect sign-in; either implies a user store and a session signer.
	EnableAuth bool
	OIDC       server.OIDCSettings
	// Host observability: the edge container whose routes and certificates are reported (empty skips),
	// and an optional DNS provider plus the zones to list records from.
	EdgeContainer string
	DNS           provider.DNSProvider
	DNSDomains    []string
}

// newDaemon builds the server. Without EnableApply the daemon can read the host and plan an
// uploaded tree against it, and nothing more; apply is an explicit opt-in.
func newDaemon(cfg daemonConfig) (*server.Server, func(), error) {
	tokens, err := server.OpenTokenStore(filepath.Join(cfg.StateDir, "tokens.json"))
	if err != nil {
		return nil, nil, fmt.Errorf("tokens: %w", err)
	}
	if cfg.BootstrapToken != "" {
		if err := tokens.SetBootstrap(cfg.BootstrapToken); err != nil {
			return nil, nil, fmt.Errorf("NATIVE_OPS_BOOTSTRAP_TOKEN: %w", err)
		}
		log.Printf("bootstrap admin token loaded from NATIVE_OPS_BOOTSTRAP_TOKEN")
	}
	audit, err := server.OpenAudit(filepath.Join(cfg.StateDir, "audit.log"))
	if err != nil {
		return nil, nil, fmt.Errorf("audit log: %w", err)
	}
	key, err := loadOrCreateKey(filepath.Join(cfg.StateDir, "plan.key"))
	if err != nil {
		audit.Close()
		return nil, nil, err
	}
	// Plans are recorded whether or not apply is on: the UI shows them, and an admin can approve one
	// ahead of time. Only an apply ever uses an approval.
	plans, err := server.OpenPlans(filepath.Join(cfg.StateDir, "plans"), cfg.ApprovalTTL)
	if err != nil {
		audit.Close()
		return nil, nil, fmt.Errorf("plans: %w", err)
	}
	opts := server.Options{
		Addr: cfg.Addr, Tokens: tokens, Audit: audit, Version: Version, Plans: plans,
		Status: func(ctx context.Context) (*status.Snapshot, error) {
			return status.CollectFull(ctx, cfg.Exec, status.Options{Pool: cfg.Pool, EdgeContainer: cfg.EdgeContainer, DNS: cfg.DNS, DNSDomains: cfg.DNSDomains})
		},
		Plan:   planSource(cfg.Exec, key),
	}
	if cfg.EnableApply || cfg.EnableInstances || cfg.EnableImageBuild || cfg.EnableEdgeApply {
		jobs, err := server.OpenJobs(filepath.Join(cfg.StateDir, "jobs"))
		if err != nil {
			audit.Close()
			return nil, nil, fmt.Errorf("jobs: %w", err)
		}
		opts.Jobs = jobs
		if cfg.EnableApply {
			opts.Apply = applySource(cfg.Exec)
		}
		if cfg.EnableInstances {
			opts.Instances, opts.InstancePolicy = engine.NewInstances(cfg.Exec), cfg.InstancePolicy
		}
		if cfg.EnableImageBuild {
			opts.ImageBuild = imageBuildSource(cfg.Exec)
		}
		if cfg.EnableEdgeApply {
			opts.EdgeApply = edgeApplySource(cfg.Exec, cfg.EdgeContainer)
		}
	}
	if cfg.EnableAuth || cfg.OIDC.Issuer != "" {
		sessionKey, err := loadOrCreateKey(filepath.Join(cfg.StateDir, "session.key"))
		if err != nil {
			audit.Close()
			return nil, nil, err
		}
		sess, err := server.NewSessions(sessionKey, 12*time.Hour, false)
		if err != nil {
			audit.Close()
			return nil, nil, err
		}
		users, err := server.OpenUserStore(filepath.Join(cfg.StateDir, "users.json"))
		if err != nil {
			audit.Close()
			return nil, nil, fmt.Errorf("users: %w", err)
		}
		opts.Users, opts.Sessions = users, sess
	}
	if cfg.OIDC.Issuer != "" {
		oidc, err := server.NewOIDC(cfg.OIDC)
		if err != nil {
			audit.Close()
			return nil, nil, err
		}
		opts.OIDC = oidc
		if cfg.OIDC.AllowedDomain == "" {
			log.Printf("warning: OIDC sign-in has no --oidc-allowed-domain: anyone the identity provider vouches for may sign in, as %s", oidc.Role)
		}
	}
	srv, err := server.New(opts)
	if err != nil {
		audit.Close()
		return nil, nil, err
	}
	return srv, func() { audit.Close() }, nil
}

// envOr returns an environment value, or a default when it is empty.
func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// envBool reports whether an environment value is a truthy flag value.
func envBool(key string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "1", "true", "yes":
		return true
	}
	return false
}

func splitList(s string) []string {	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// planSource is what POST /v1/plan plans with. engine.PlanFleet wraps exec in remote.ReadOnly, so
// an uploaded tree can be planned against this host but never applied to it. The key makes the
// plan's hash cover the environment values and hook bodies the plan itself does not print.
func planSource(exec remote.Executor, key []byte) server.PlanFunc {
	return func(ctx context.Context, dir, service string) (*engine.FleetPlan, error) {
		return engine.PlanFleet(ctx, exec, dir, service, engine.WithBindKey(key))
	}
}

// applySource is what POST /v1/apply applies with, once the server has checked the plan's hash.
// Progress goes to the job's own log, not the process log.
func applySource(exec remote.Executor) server.ApplyFunc {
	return func(ctx context.Context, dir string, plan *engine.FleetPlan, logf func(format string, a ...any)) error {
		d := engine.NewDeployer(exec)
		d.SetLogger(logf)
		return d.ApplyPlan(ctx, dir, plan)
	}
}

// imageBuildSource is what POST /v1/images/build builds with.
func imageBuildSource(exec remote.Executor) server.ImageBuildFunc {
	return func(ctx context.Context, dir, app, ref string, logf func(string, ...any)) error {
		return engine.BuildImage(ctx, exec, dir, app, ref, logf)
	}
}

// edgeApplySource is what POST /v1/edge/apply applies with: the uploaded tree's
// edge/Caddyfile to the edge container, validated and reloaded with rollback.
func edgeApplySource(exec remote.Executor, edgeContainer string) server.EdgeApplyFunc {
	return func(ctx context.Context, dir string, logf func(string, ...any)) error {
		logf("applying edge config from %s", dir)
		return engine.ApplyEdgeConfig(ctx, exec, dir, edgeContainer)
	}
}

// loadOrCreateKey returns the daemon's plan key, creating it (32 random bytes, 0600) on first
// start. It is stable across restarts, so a plan hash is still good after one. A key file that is
// too short or readable by others is refused rather than trusted.
func loadOrCreateKey(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return nil, err
		}
		if err := os.WriteFile(path, []byte(hex.EncodeToString(key)+"\n"), 0o600); err != nil {
			return nil, fmt.Errorf("plan key: %w", err)
		}
		return key, nil
	}
	if err != nil {
		return nil, fmt.Errorf("plan key: %w", err)
	}
	if st, err := os.Stat(path); err != nil || st.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("plan key %s must not be readable by anyone else (chmod 600)", path)
	}
	key, err := hex.DecodeString(strings.TrimSpace(string(b)))
	if err != nil || len(key) < 32 {
		return nil, fmt.Errorf("plan key %s is not 32 bytes of hex; delete it to make a new one", path)
	}
	return key, nil
}

func handleTokenCommand(args []string) {
	if len(args) < 1 {
		fmt.Println("Usage: native-ops token [create|list|revoke] --state-dir DIR ...")
		os.Exit(1)
	}
	action := args[0]
	flags := flag.NewFlagSet("token "+action, flag.ExitOnError)
	stateDir := stateDirFlag(flags)
	name := flags.String("name", "", "Token name (create)")
	role := flags.String("role", "viewer", "viewer, planner, deployer or admin (create)")
	names := flags.String("names", "", "Limit the token to tenant instances with these names, as globs, comma separated, e.g. 'demo-*,rest-*' (create; needs --images, and the deployer role)")
	images := flags.String("images", "", "The images such a token may launch, as globs, e.g. 'opsavor-platform:*' (create)")
	domains := flags.String("domains", "", "The domains such a token may publish a route for, as globs, e.g. '*.opsavor.app' (create)")
	id := flags.String("id", "", "Token id (revoke)")
	_ = flags.Parse(args[1:])

	store, err := server.OpenTokenStore(filepath.Join(openStateDir(*stateDir), "tokens.json"))
	if err != nil {
		log.Fatalf("tokens: %v", err)
	}
	switch action {
	case "create":
		secret, t, err := store.CreateScoped(*name, server.Role(*role), server.Scope{Names: splitList(*names), Images: splitList(*images), Domains: splitList(*domains)})
		if err != nil {
			log.Fatalf("token create: %v", err)
		}
		fmt.Printf("Created %s token %q (id %s). It is shown once; store it in your CI secret store:\n\n%s\n", t.Role, t.Name, t.ID, secret)
	case "list":
		ts, err := store.List()
		if err != nil {
			log.Fatalf("token list: %v", err)
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "ID\tNAME\tROLE\tSCOPE\tCREATED")
		for _, t := range ts {
			scope := "-"
			if t.Scoped() {
				scope = "names=" + strings.Join(t.Scope.Names, ",") + " images=" + strings.Join(t.Scope.Images, ",") + " domains=" + strings.Join(t.Scope.Domains, ",")
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", t.ID, t.Name, t.Role, scope, t.Created.Format("2006-01-02"))
		}
		_ = w.Flush()
	case "revoke":
		if err := store.Revoke(*id); err != nil {
			log.Fatalf("token revoke: %v", err)
		}
		fmt.Printf("Revoked %s\n", *id)
	default:
		fmt.Println("Usage: native-ops token [create|list|revoke] --state-dir DIR ...")
		os.Exit(1)
	}
}

// handleUserCommand manages the people who may sign in to the daemon UI: local accounts with a password
// (OIDC users are created on first sign-in). The same ladder as a token: viewer, planner, deployer, admin.
func handleUserCommand(args []string) {
	if len(args) < 1 {
		fmt.Println("Usage: native-ops user [create|list|passwd|role|disable|enable] --state-dir DIR ...")
		os.Exit(1)
	}
	action := args[0]
	flags := flag.NewFlagSet("user "+action, flag.ExitOnError)
	stateDir := stateDirFlag(flags)
	username := flags.String("username", "", "Username (create, passwd, role, disable, enable)")
	name := flags.String("name", "", "Display name (create)")
	role := flags.String("role", "viewer", "viewer, planner, deployer or admin (create, role)")
	password := flags.String("password", "", "Password, 12-72 characters (create, passwd). If empty, one line is read from stdin")
	_ = flags.Parse(args[1:])

	if *password == "" && (action == "create" || action == "passwd") {
		b, _ := io.ReadAll(os.Stdin)
		*password = strings.TrimRight(string(b), "\r\n")
	}
	store, err := server.OpenUserStore(filepath.Join(openStateDir(*stateDir), "users.json"))
	if err != nil {
		log.Fatalf("users: %v", err)
	}
	switch action {
	case "create":
		u, err := store.CreateLocal(*username, *name, server.Role(*role), *password)
		if err != nil {
			log.Fatalf("user create: %v", err)
		}
		fmt.Printf("Created %s user %q\n", u.Role, u.Username)
	case "list":
		users, err := store.List()
		if err != nil {
			log.Fatalf("user list: %v", err)
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "USERNAME\tNAME\tROLE\tPROVIDER\tEMAIL\tSTATE")
		for _, u := range users {
			state := "active"
			if u.Disabled {
				state = "disabled"
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", u.Username, u.Name, u.Role, u.Provider, u.Email, state)
		}
		_ = w.Flush()
	case "passwd":
		if err := store.SetPassword(*username, *password); err != nil {
			log.Fatalf("user passwd: %v", err)
		}
		fmt.Printf("Password set for %q\n", *username)
	case "role":
		if err := store.SetRole(*username, server.Role(*role)); err != nil {
			log.Fatalf("user role: %v", err)
		}
		fmt.Printf("%q is now %s\n", *username, *role)
	case "disable":
		if err := store.SetDisabled(*username, true); err != nil {
			log.Fatalf("user disable: %v", err)
		}
		fmt.Printf("Disabled %q\n", *username)
	case "enable":
		if err := store.SetDisabled(*username, false); err != nil {
			log.Fatalf("user enable: %v", err)
		}
		fmt.Printf("Enabled %q\n", *username)
	default:
		fmt.Println("Usage: native-ops user [create|list|passwd|role|disable|enable] --state-dir DIR ...")
		os.Exit(1)
	}
}
