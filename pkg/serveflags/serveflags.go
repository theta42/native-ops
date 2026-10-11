// Package serveflags keeps the flags a deploy gave the daemon (fleet.yml's daemon.flags), so they change
// through git like everything else instead of by editing the systemd unit on the host.
//
// The flags live in the daemon's state directory, which the daemon may write (as it may its own binary).
// At startup, when a flag set is kept there, it replaces the flags on the unit's command line, all but
// --state-dir (which says where to look) and --addr when the kept set has none: the unit needs nothing
// but those two, and a host whose unit carries more (one made before this, or by hand) is brought in line
// by its next deploy without anyone logging in.
//
// A new set is staged as pending and the daemon restarts into it. Each start of a pending set is counted;
// one that has served for CommitAfter is committed. One that did not start MaxAttempts times is dropped,
// and the daemon goes back to the set it had (systemd keeps restarting it), so a bad flag can never leave
// a host without its daemon.
package serveflags

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	FileName     = "serve.flags"          // the committed set
	PendingName  = "serve.flags.pending"  // a set a deploy staged; empty means "back to the unit's flags"
	AttemptsName = "serve.flags.attempts" // how many times the pending set was started
	MaxAttempts  = 3
)

// CommitAfter is how long a pending set must serve before it is committed (a variable for tests).
var CommitAfter = 30 * time.Second

// Normalize is a flag set as one line, single spaces.
func Normalize(flags string) string { return strings.Join(strings.Fields(flags), " ") }

// Loaded is the flag set a starting daemon runs with.
type Loaded struct {
	Managed bool     // a set from a deploy replaces the unit's flags
	Text    string   // that set, normalized
	Args    []string // its arguments
	Pending bool     // it is not committed yet
	Note    string   // what happened, for the log (a pending set given up on)
}

// Load picks the flag set for this start, counting a pending set's attempts.
func Load(stateDir string) (Loaded, error) {
	pending := filepath.Join(stateDir, PendingName)
	if b, err := os.ReadFile(pending); err == nil {
		attempts := filepath.Join(stateDir, AttemptsName)
		n := 0
		if a, err := os.ReadFile(attempts); err == nil {
			n, _ = strconv.Atoi(strings.TrimSpace(string(a)))
		}
		n++
		if n > MaxAttempts {
			_ = os.Remove(pending)
			_ = os.Remove(attempts)
			l, err := committed(stateDir)
			l.Note = fmt.Sprintf("the flags a deploy set did not start %d times; back to the previous ones", MaxAttempts)
			return l, err
		}
		if err := os.WriteFile(attempts, []byte(strconv.Itoa(n)+"\n"), 0o600); err != nil {
			return Loaded{}, err
		}
		text := Normalize(string(b))
		if text == "" { // the deploy took daemon.flags away: the unit's flags again
			return Loaded{Pending: true}, nil
		}
		return Loaded{Managed: true, Text: text, Args: strings.Fields(text), Pending: true}, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return Loaded{}, err
	}
	return committed(stateDir)
}

func committed(stateDir string) (Loaded, error) {
	b, err := os.ReadFile(filepath.Join(stateDir, FileName))
	if errors.Is(err, os.ErrNotExist) {
		return Loaded{}, nil
	}
	if err != nil {
		return Loaded{}, err
	}
	text := Normalize(string(b))
	if text == "" {
		return Loaded{}, nil
	}
	return Loaded{Managed: true, Text: text, Args: strings.Fields(text)}, nil
}

// Stage writes a flag set for the next start ("" goes back to the unit's flags).
func Stage(stateDir, flags string) error {
	tmp := filepath.Join(stateDir, "."+PendingName+".tmp")
	if err := os.WriteFile(tmp, []byte(Normalize(flags)+"\n"), 0o600); err != nil {
		return err
	}
	_ = os.Remove(filepath.Join(stateDir, AttemptsName))
	return os.Rename(tmp, filepath.Join(stateDir, PendingName))
}

// Commit makes a pending set the committed one once this start has served for CommitAfter. It returns at
// once when nothing is pending.
func Commit(ctx context.Context, stateDir string, logf func(string, ...any)) {
	pending := filepath.Join(stateDir, PendingName)
	if _, err := os.Stat(pending); err != nil {
		return
	}
	select {
	case <-ctx.Done():
		return
	case <-time.After(CommitAfter):
	}
	b, err := os.ReadFile(pending)
	if err != nil {
		return
	}
	if Normalize(string(b)) == "" {
		_ = os.Remove(filepath.Join(stateDir, FileName))
		_ = os.Remove(pending)
	} else if err := os.Rename(pending, filepath.Join(stateDir, FileName)); err != nil {
		logf("could not commit the daemon's flags: %v", err)
		return
	}
	_ = os.Remove(filepath.Join(stateDir, AttemptsName))
	logf("daemon flags committed: this start has served for %s", CommitAfter)
}

// Value is the value a command line gives a flag (--name v, --name=v, -name v or -name=v), and whether
// it gives one.
func Value(args []string, name string) (string, bool) {
	val, found := "", false
	for i := 0; i < len(args); i++ {
		a := strings.TrimLeft(args[i], "-")
		if !strings.HasPrefix(args[i], "-") {
			continue
		}
		if a == name && i+1 < len(args) {
			val, found = args[i+1], true
			i++
		} else if strings.HasPrefix(a, name+"=") {
			val, found = strings.TrimPrefix(a, name+"="), true
		}
	}
	return val, found
}

// Merge is the command line a managed set runs with: the unit's --addr (unless the set has its own), the
// set, and the unit's --state-dir last, so the set can change neither where the state is nor, without
// saying so, where the daemon listens.
func Merge(unitArgs, set []string, stateDir string) []string {
	var out []string
	if addr, ok := Value(unitArgs, "addr"); ok {
		if _, own := Value(set, "addr"); !own {
			out = append(out, "--addr", addr)
		}
	}
	out = append(out, set...)
	return append(out, "--state-dir", stateDir)
}
