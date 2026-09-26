package remote

import (
	"context"
	"io"
	"os"
	"strings"
)

// ReadOnlyError is returned for a command the read-only executor will not run.
type ReadOnlyError struct{ Command string }

func (e *ReadOnlyError) Error() string {
	return "refusing to run a command that could change the host: " + e.Command
}

// ReadOnly wraps an Executor so that only commands which merely read Incus state
// can run. It fails closed: a command is allowed only if it is one of the
// shapes listed in IsReadOnly, so a code path that would change the host (now,
// or added later) is refused with an error instead of running. `native-ops
// plan` runs behind it, which is what makes "plan changes nothing" a property of
// the executor rather than a promise about every function it calls.
func ReadOnly(inner Executor) Executor { return &readOnly{inner: inner} }

type readOnly struct{ inner Executor }

func (r *readOnly) Run(ctx context.Context, command string) (string, error) {
	if !IsReadOnly(command) {
		return "", &ReadOnlyError{Command: command}
	}
	return r.inner.Run(ctx, command)
}

// RunWithInput is always refused: feeding a command stdin is how things get written.
func (r *readOnly) RunWithInput(_ context.Context, command string, _ io.Reader) (string, error) {
	return "", &ReadOnlyError{Command: command}
}

func (r *readOnly) WriteFile(_ context.Context, path string, _ []byte, _ os.FileMode) error {
	return &ReadOnlyError{Command: "write " + path}
}

func (r *readOnly) Close() error { return r.inner.Close() }

// IsReadOnly reports whether command only reads Incus state. The command must be
// plain words (bare or single-quoted): any shell operator, substitution,
// redirection or newline outside quotes makes it not read-only.
func IsReadOnly(command string) bool {
	w, ok := shellWords(command)
	if !ok || len(w) < 2 || w[0] != "incus" {
		return false
	}
	rest := w[1:]
	switch {
	case rest[0] == "list":
		// list [name|remote:] --format json
		rest, ok = trimFormatJSON(rest[1:])
		return ok && positional(rest, 0, 1)
	case rest[0] == "info":
		return positional(rest[1:], 1, 1)
	case rest[0] == "config" && len(rest) > 1 && rest[1] == "show":
		return positional(rest[2:], 1, 1)
	case rest[0] == "storage" && len(rest) > 2 && rest[1] == "volume" && rest[2] == "show":
		return positional(rest[3:], 2, 2)
	case rest[0] == "storage" && len(rest) > 2 && rest[1] == "volume" && rest[2] == "list":
		rest, ok = trimFormatJSON(rest[3:])
		return ok && positional(rest, 1, 1)
	case rest[0] == "image" && len(rest) > 1 && rest[1] == "list":
		rest, ok = trimFormatJSON(rest[2:])
		return ok && positional(rest, 0, 0)
	case rest[0] == "image" && len(rest) > 2 && rest[1] == "alias" && rest[2] == "list":
		rest, ok = trimFormatJSON(rest[3:])
		return ok && positional(rest, 0, 0)
	case rest[0] == "profile" && len(rest) > 1 && rest[1] == "list":
		rest, ok = trimFormatJSON(rest[2:])
		return ok && positional(rest, 0, 1)
	case rest[0] == "profile" && len(rest) > 1 && rest[1] == "show":
		return positional(rest[2:], 1, 1)
	case rest[0] == "file" && len(rest) == 4 && rest[1] == "pull":
		// Only to stdout: `-` as the destination. Any other destination writes a local file.
		return rest[3] == "-" && !strings.HasPrefix(rest[2], "-")
	case rest[0] == "network" && len(rest) > 1 && (rest[1] == "show" || rest[1] == "get"):
		return positional(rest[2:], 1, 2)
	}
	return false
}

// trimFormatJSON removes a trailing `--format json`.
func trimFormatJSON(w []string) ([]string, bool) {
	if len(w) >= 2 && w[len(w)-2] == "--format" && w[len(w)-1] == "json" {
		return w[:len(w)-2], true
	}
	return nil, false
}

// positional reports whether w is between min and max words, none of them an option.
func positional(w []string, min, max int) bool {
	if len(w) < min || len(w) > max {
		return false
	}
	for _, x := range w {
		if strings.HasPrefix(x, "-") {
			return false
		}
	}
	return true
}

// shellWords splits a command into words the way a POSIX shell would, but
// accepts only the safe subset: bare words of [A-Za-z0-9_.,:/=@%+-], single-quoted
// strings (inert to the shell, whatever they contain) and the '\” idiom.
// Anything else (operators, substitution, redirection, globbing, double quotes,
// a newline) returns ok=false.
func shellWords(s string) (words []string, ok bool) {
	var cur strings.Builder
	inWord := false
	flush := func() {
		if inWord {
			words = append(words, cur.String())
			cur.Reset()
			inWord = false
		}
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == ' ':
			flush()
		case c == '\'':
			inWord = true
			j := strings.IndexByte(s[i+1:], '\'')
			if j < 0 {
				return nil, false
			}
			cur.WriteString(s[i+1 : i+1+j])
			i += j + 1
		case c == '\\' && i+1 < len(s) && s[i+1] == '\'':
			inWord = true
			cur.WriteByte('\'')
			i++
		case isBare(c):
			inWord = true
			cur.WriteByte(c)
		default:
			return nil, false
		}
	}
	flush()
	return words, true
}

func isBare(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return true
	}
	return strings.IndexByte("_.,:/=@%+-", c) >= 0
}
