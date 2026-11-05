package config

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/theta42/native-ops/pkg/serveflags"
)

// A fleet runs one daemon per host. Each service belongs to one host (its `host:`, else fleet.yml's
// default host), and each daemon plans and applies only the services of the host it is (`serve
// --host-name`). A single-host fleet needs none of this: its one host is the default, and its daemon is
// that host whether or not it is told so.

// ErrHostNameRequired is why a daemon that was not told its host refuses to plan a fleet of several
// hosts: it cannot tell its own services from the others', and planning them all would deploy every
// host's services here.
var ErrHostNameRequired = errors.New("this daemon was not told which host it is")

// HostNames is hosts:'s names, sorted.
func (f *FleetConfig) HostNames() []string {
	out := make([]string, 0, len(f.Hosts))
	for name := range f.Hosts {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// DefaultHostName is the host a service without `host:` belongs to: default_host, else the only entry
// of hosts:, else "" (a fleet.yml with no hosts:).
func (f *FleetConfig) DefaultHostName() string {
	if f.DefaultHost != "" {
		return f.DefaultHost
	}
	if len(f.Hosts) == 1 {
		for name := range f.Hosts {
			return name
		}
	}
	return ""
}

// HostOf is the host a service belongs to: its `host:`, else the default host.
func (f *FleetConfig) HostOf(svc *ServiceConfig) string {
	if svc.Host != "" {
		return svc.Host
	}
	return f.DefaultHostName()
}

// ValidateHosts checks fleet.yml's host settings: default_host names an entry of hosts:, a fleet of two
// or more hosts names its default host, and daemon.flags (which every host's daemon gets) does not carry
// a --host-name that could be true of only one of them.
func (f *FleetConfig) ValidateHosts() error {
	names := f.HostNames()
	if f.DefaultHost != "" {
		if _, ok := f.Hosts[f.DefaultHost]; !ok {
			return fmt.Errorf("default_host %q is not an entry of hosts: (%s)", f.DefaultHost, listOrNone(names))
		}
	}
	if len(names) >= 2 && f.DefaultHost == "" {
		return fmt.Errorf("fleet.yml has %d hosts (%s) and no default_host: a service without `host:` must belong to exactly one of them, and with more than one host none is the obvious choice; set default_host: to one of them", len(names), strings.Join(names, ", "))
	}
	if f.Daemon != nil {
		if v, ok := serveflags.Value(strings.Fields(f.Daemon.Flags), "host-name"); ok {
			switch {
			case len(names) >= 2:
				return fmt.Errorf("daemon.flags sets --host-name, but every host's daemon gets daemon.flags and fleet.yml has %d hosts; give each host's daemon its own --host-name on its unit (reconcile does it for the hosts it creates)", len(names))
			case len(names) == 1 && v != names[0]:
				return fmt.Errorf("daemon.flags sets --host-name %q, but the only host in hosts: is %q", v, names[0])
			case len(names) == 0:
				return fmt.Errorf("daemon.flags sets --host-name %q, but fleet.yml has no hosts:", v)
			}
		}
	}
	return nil
}

// ValidateServiceHost checks that a service's `host:`, when set, names an entry of hosts:.
func (f *FleetConfig) ValidateServiceHost(svc *ServiceConfig) error {
	if svc.Host == "" {
		return nil
	}
	if _, ok := f.Hosts[svc.Host]; !ok {
		return fmt.Errorf("service %s: host %q is not an entry of fleet.yml's hosts: (%s)", svc.Name, svc.Host, listOrNone(f.HostNames()))
	}
	return nil
}

// DaemonHost works out which host a daemon is, from its --host-name (empty when it was not given) and
// fleet.yml, and whether its plans are a share of the fleet (filtered) rather than all of it.
//
//   - No hosts: in fleet.yml: there is nothing to tell apart; the daemon plans everything, as before
//     hosts could be named. A --host-name is then an error (it names a host fleet.yml does not have).
//   - One host: the daemon is that host, told or not, and everything is its; a --host-name naming any
//     other host is an error.
//   - Two or more: the daemon must be told (ErrHostNameRequired), and it must name one of them. Its
//     plans then hold only its own share.
func (f *FleetConfig) DaemonHost(hostName string) (host string, filtered bool, err error) {
	names := f.HostNames()
	switch {
	case len(names) == 0:
		if hostName != "" {
			return "", false, fmt.Errorf("this daemon is --host-name %q, but fleet.yml has no hosts:", hostName)
		}
		return "", false, nil
	case len(names) == 1:
		if hostName != "" && hostName != names[0] {
			return "", false, fmt.Errorf("this daemon is --host-name %q, but the only host in fleet.yml's hosts: is %q", hostName, names[0])
		}
		return names[0], false, nil
	}
	if hostName == "" {
		return "", false, fmt.Errorf("%w: fleet.yml has %d hosts (%s), and a daemon plans and applies only its own host's services; start it with --host-name <one of them> (or put the flag on its unit). Nothing was planned, rather than every host's services being planned here", ErrHostNameRequired, len(names), strings.Join(names, ", "))
	}
	if _, ok := f.Hosts[hostName]; !ok {
		return "", false, fmt.Errorf("this daemon is --host-name %q, which is not an entry of fleet.yml's hosts: (%s)", hostName, strings.Join(names, ", "))
	}
	return hostName, true, nil
}

func listOrNone(names []string) string {
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, ", ")
}
