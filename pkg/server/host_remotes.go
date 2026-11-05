package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/theta42/native-ops/pkg/config"
	"github.com/theta42/native-ops/pkg/engine"
	"github.com/theta42/native-ops/pkg/incus"
)

// A migration names where the instance goes by an Incus remote configured on the source host
// (target_remote), or by the fleet.yml host it goes to (target_host): fleet.yml's hosts.<name>.incus_remote
// is the remote that reaches that host. The daemon learns the remotes from the fleet.yml of each deploy tag
// it applies, and keeps them in HostRemotesFile, so a migration needs no config tree of its own.

// hostRemotes is what HostRemotesFile holds: each host of fleet.yml with the Incus remote that reaches it
// ("" when fleet.yml gives none).
type hostRemotes struct {
	Hosts map[string]string `json:"hosts"`
}

// followHostRemotes records the hosts and their Incus remotes from a deploy tag's fleet.yml. A fleet.yml
// without hosts: removes the record.
func (s *Server) followHostRemotes(root string, logf func(string, ...any)) error {
	if s.opts.HostRemotesFile == "" {
		return nil
	}
	fleet, err := config.LoadFleetConfig(root)
	if err != nil {
		return err
	}
	if len(fleet.Hosts) == 0 {
		if err := os.Remove(s.opts.HostRemotesFile); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("host remotes: %w", err)
		}
		return nil
	}
	rec := hostRemotes{Hosts: map[string]string{}}
	var named []string
	for _, name := range fleet.HostNames() {
		rec.Hosts[name] = fleet.Hosts[name].IncusRemote
		if r := fleet.Hosts[name].IncusRemote; r != "" {
			named = append(named, name+" -> "+r)
		}
	}
	b, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.opts.HostRemotesFile), 0o700); err != nil {
		return fmt.Errorf("host remotes: %w", err)
	}
	tmp := s.opts.HostRemotesFile + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return fmt.Errorf("host remotes: %w", err)
	}
	if err := os.Rename(tmp, s.opts.HostRemotesFile); err != nil {
		return fmt.Errorf("host remotes: %w", err)
	}
	if len(named) > 0 && len(fleet.Hosts) >= 2 {
		logf("migrations by target_host go to the Incus remotes fleet.yml names: %s", strings.Join(named, ", "))
	}
	return nil
}

// targetRemoteOf resolves a migration's target_host to the Incus remote fleet.yml gives it. The error is
// what the caller is told (a 400 or 409).
func (s *Server) targetRemoteOf(host string) (string, int, error) {
	if !incus.ValidName(host) {
		return "", 400, errors.New("target_host must be the name of a host in fleet.yml's hosts:")
	}
	if s.opts.HostName != "" && host == s.opts.HostName {
		return "", 400, fmt.Errorf("target_host %q is this host", host)
	}
	if s.opts.HostRemotesFile == "" {
		return "", 409, errors.New("this daemon does not record fleet.yml's hosts (it does not deploy tags); give target_remote instead")
	}
	b, err := os.ReadFile(s.opts.HostRemotesFile)
	if errors.Is(err, os.ErrNotExist) {
		return "", 409, errors.New("this daemon has not deployed a fleet.yml with hosts: yet, so it does not know their Incus remotes; give target_remote instead")
	}
	if err != nil {
		return "", 500, fmt.Errorf("read the hosts' Incus remotes: %w", err)
	}
	var rec hostRemotes
	if err := json.Unmarshal(b, &rec); err != nil {
		return "", 500, fmt.Errorf("read the hosts' Incus remotes: %w", err)
	}
	remote, ok := rec.Hosts[host]
	if !ok {
		names := make([]string, 0, len(rec.Hosts))
		for n := range rec.Hosts {
			names = append(names, n)
		}
		sort.Strings(names)
		return "", 400, fmt.Errorf("target_host %q is not a host of the fleet.yml this daemon last deployed (%s)", host, strings.Join(names, ", "))
	}
	if remote == "" {
		return "", 409, fmt.Errorf("fleet.yml gives host %s no incus_remote (hosts.%s.incus_remote); set it, or give target_remote instead", host, host)
	}
	if remote == engine.LocalRemote {
		return "", 400, fmt.Errorf("fleet.yml's hosts.%s.incus_remote is %q, which is this host", host, remote)
	}
	return remote, 0, nil
}
