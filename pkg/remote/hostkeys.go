package remote

import (
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"sync"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// Host key checking for SSH connections, chosen by the environment:
//
//   - NATIVE_OPS_SSH_KNOWN_HOSTS=<file>: a host's key is checked against the file. A key that differs
//     from the one recorded is always refused. A host not in the file yet is added to it (trust on first
//     use), unless NATIVE_OPS_SSH_STRICT=1, in which case it is refused too.
//   - unset: host keys are not checked, as before, and a warning says so once. Set the variable
//     wherever the connection crosses a network you do not control.
const (
	envKnownHosts = "NATIVE_OPS_SSH_KNOWN_HOSTS"
	envStrict     = "NATIVE_OPS_SSH_STRICT"
)

var warnInsecureOnce sync.Once

// hostKeyCallback returns the host key check the environment asks for.
func hostKeyCallback() (ssh.HostKeyCallback, error) {
	path := os.Getenv(envKnownHosts)
	if path == "" {
		warnInsecureOnce.Do(func() {
			log.Printf("warning: SSH host keys are not checked; set %s to a known_hosts file to check them", envKnownHosts)
		})
		return ssh.InsecureIgnoreHostKey(), nil
	}
	return knownHostsCallback(path, os.Getenv(envStrict) == "1")
}

// knownHostsCallback checks keys against a known_hosts file, adding unknown hosts unless strict.
func knownHostsCallback(path string, strict bool) (ssh.HostKeyCallback, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("known hosts file: %w", err)
	}
	f.Close()
	check, err := knownhosts.New(path)
	if err != nil {
		return nil, fmt.Errorf("known hosts file %s: %w", path, err)
	}
	var mu sync.Mutex
	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		err := check(hostname, remote, key)
		var ke *knownhosts.KeyError
		if err == nil || !errors.As(err, &ke) {
			return err
		}
		if len(ke.Want) > 0 {
			return fmt.Errorf("the SSH host key of %s does not match the one in %s: refusing to connect (if the host was rebuilt, remove its old line)", hostname, path)
		}
		if strict {
			return fmt.Errorf("%s is not in %s and %s=1: refusing to connect", hostname, path, envStrict)
		}
		mu.Lock()
		defer mu.Unlock()
		af, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return fmt.Errorf("record the host key of %s: %w", hostname, err)
		}
		defer af.Close()
		addrs := []string{knownhosts.Normalize(hostname)}
		if r := knownhosts.Normalize(remote.String()); r != addrs[0] {
			addrs = append(addrs, r)
		}
		if _, err := fmt.Fprintln(af, knownhosts.Line(addrs, key)); err != nil {
			return fmt.Errorf("record the host key of %s: %w", hostname, err)
		}
		log.Printf("recorded the SSH host key of %s in %s (first connection)", hostname, path)
		return nil
	}, nil
}
