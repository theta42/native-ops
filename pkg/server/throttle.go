package server

import (
	"strings"
	"sync"
	"time"
)

// loginThrottle slows password guessing against one account. After a few failed sign-ins in a row
// for a username, further attempts for it are refused for a while, and the wait doubles with every
// further failure up to a ceiling. It is keyed on the username, not the client address: the daemon
// sits behind the edge proxy, so every request comes from the same address. A success clears it.
// The lockout is short, so someone who knows a username can delay that user's sign-in but not keep
// them out (OIDC sign-in and API tokens are not affected at all).
type loginThrottle struct {
	mu    sync.Mutex
	now   func() time.Time
	state map[string]*throttleState
}

type throttleState struct {
	failures int
	until    time.Time
	last     time.Time
}

const (
	throttleFree   = 5                // failures allowed before the first wait
	throttleFirst  = 30 * time.Second // the first wait; it doubles after each further failure
	throttleMax    = 15 * time.Minute
	throttleForget = time.Hour // an account with no failure for this long starts over
)

func newLoginThrottle() *loginThrottle {
	return &loginThrottle{now: time.Now, state: map[string]*throttleState{}}
}

func throttleKey(username string) string { return strings.ToLower(strings.TrimSpace(username)) }

// Wait reports how long sign-in for username is refused (0: it may be tried now).
func (t *loginThrottle) Wait(username string) time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()
	st := t.state[throttleKey(username)]
	if st == nil {
		return 0
	}
	if d := st.until.Sub(t.now()); d > 0 {
		return d
	}
	return 0
}

// Fail records a failed sign-in.
func (t *loginThrottle) Fail(username string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	k := throttleKey(username)
	st := t.state[k]
	if st == nil || now.Sub(st.last) > throttleForget {
		st = &throttleState{}
		t.state[k] = st
	}
	st.failures++
	st.last = now
	if over := st.failures - throttleFree; over > 0 {
		wait := throttleFirst
		for i := 1; i < over && wait < throttleMax; i++ {
			wait *= 2
		}
		if wait > throttleMax {
			wait = throttleMax
		}
		st.until = now.Add(wait)
	}
	// Keep the map from growing without bound under a spray of made-up usernames.
	if len(t.state) > 10000 {
		for name, s := range t.state {
			if now.Sub(s.last) > throttleForget || now.After(s.until) {
				delete(t.state, name)
			}
		}
	}
}

// Succeed clears a username's failures.
func (t *loginThrottle) Succeed(username string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.state, throttleKey(username))
}
