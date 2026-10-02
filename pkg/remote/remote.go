package remote

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

// Executor defines remote or local command execution.
type Executor interface {
	Run(ctx context.Context, command string) (string, error)
	RunWithInput(ctx context.Context, command string, stdin io.Reader) (string, error)
	WriteFile(ctx context.Context, remotePath string, content []byte, perm os.FileMode) error
	Close() error
}

// LocalExecutor runs commands on the local machine.
type LocalExecutor struct{}

func NewLocalExecutor() *LocalExecutor {
	return &LocalExecutor{}
}

func (l *LocalExecutor) Run(ctx context.Context, command string) (string, error) {
	cmd := exec.CommandContext(ctx, "bash", "-c", command)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("command failed: %s (stderr: %s): %w", CommandHead(command), strings.TrimSpace(stderr.String()), err)
	}
	return stdout.String(), nil
}

func (l *LocalExecutor) RunWithInput(ctx context.Context, command string, stdin io.Reader) (string, error) {
	cmd := exec.CommandContext(ctx, "bash", "-c", command)
	cmd.Stdin = stdin
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("command with stdin failed: %s (stderr: %s): %w", CommandHead(command), strings.TrimSpace(stderr.String()), err)
	}
	return stdout.String(), nil
}

func (l *LocalExecutor) WriteFile(ctx context.Context, path string, content []byte, perm os.FileMode) error {
	return os.WriteFile(path, content, perm)
}

func (l *LocalExecutor) Close() error {
	return nil
}

// SSHExecutor runs commands over SSH on a target host.
type SSHExecutor struct {
	client *ssh.Client
	host   string
}

// NewSSHExecutor creates an SSH connection to a remote host.
func NewSSHExecutor(host string, port int, user string, privateKeyPEM []byte) (*SSHExecutor, error) {
	if port <= 0 {
		port = 22
	}
	if user == "" {
		user = "root"
	}

	signer, err := ssh.ParsePrivateKey(privateKeyPEM)
	if err != nil {
		return nil, fmt.Errorf("parse SSH private key: %w", err)
	}

	hostKeys, err := hostKeyCallback()
	if err != nil {
		return nil, err
	}
	config := &ssh.ClientConfig{
		User: user,
		Auth: []ssh.AuthMethod{
			ssh.PublicKeys(signer),
		},
		HostKeyCallback: hostKeys, // see hostkeys.go
		Timeout:         15 * time.Second,
	}

	addr := fmt.Sprintf("%s:%d", host, port)
	client, err := ssh.Dial("tcp", addr, config)
	if err != nil {
		return nil, fmt.Errorf("dial SSH to %s: %w", addr, err)
	}

	return &SSHExecutor{
		client: client,
		host:   host,
	}, nil
}

func (s *SSHExecutor) Run(ctx context.Context, command string) (string, error) {
	session, err := s.client.NewSession()
	if err != nil {
		return "", fmt.Errorf("create SSH session: %w", err)
	}
	defer session.Close()

	var stdout, stderr bytes.Buffer
	session.Stdout = &stdout
	session.Stderr = &stderr

	errChan := make(chan error, 1)
	go func() {
		errChan <- session.Run(command)
	}()

	select {
	case <-ctx.Done():
		_ = session.Signal(ssh.SIGKILL)
		return "", ctx.Err()
	case err := <-errChan:
		if err != nil {
			return stdout.String(), fmt.Errorf("SSH command '%s' failed on %s (stderr: %s): %w", CommandHead(command), s.host, strings.TrimSpace(stderr.String()), err)
		}
		return stdout.String(), nil
	}
}

func (s *SSHExecutor) RunWithInput(ctx context.Context, command string, stdin io.Reader) (string, error) {
	session, err := s.client.NewSession()
	if err != nil {
		return "", fmt.Errorf("create SSH session: %w", err)
	}
	defer session.Close()

	session.Stdin = stdin
	var stdout, stderr bytes.Buffer
	session.Stdout = &stdout
	session.Stderr = &stderr

	errChan := make(chan error, 1)
	go func() {
		errChan <- session.Run(command)
	}()

	select {
	case <-ctx.Done():
		_ = session.Signal(ssh.SIGKILL)
		return "", ctx.Err()
	case err := <-errChan:
		if err != nil {
			return stdout.String(), fmt.Errorf("SSH command '%s' with stdin failed on %s (stderr: %s): %w", CommandHead(command), s.host, strings.TrimSpace(stderr.String()), err)
		}
		return stdout.String(), nil
	}
}

func (s *SSHExecutor) WriteFile(ctx context.Context, remotePath string, content []byte, perm os.FileMode) error {
	// The content goes over stdin, so it never appears in a command line or an error.
	q := shQuote(remotePath)
	cmd := fmt.Sprintf("cat > %s && chmod %o %s", q, perm, q)
	_, err := s.RunWithInput(ctx, cmd, bytes.NewReader(content))
	return err
}

func (s *SSHExecutor) Close() error {
	if s.client != nil {
		return s.client.Close()
	}
	return nil
}

// CommandHead is the start of a command that is safe to put in an error or a log: its first few
// plain words (e.g. "incus config set 'web'"), never its arguments in full. A command can carry a
// value that must not be repeated (an instance config value, a file's content), and errors end up
// in job records that more people can read than can read the host.
func CommandHead(command string) string {
	const maxWords, maxLen = 4, 80
	words := strings.Fields(command)
	if len(words) > maxWords {
		words = append(words[:maxWords], "...")
	}
	head := strings.Join(words, " ")
	if len(head) > maxLen {
		head = head[:maxLen] + "..."
	}
	return head
}

func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
