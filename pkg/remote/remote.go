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
		return stdout.String(), fmt.Errorf("command failed: %s (stderr: %s): %w", command, strings.TrimSpace(stderr.String()), err)
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
		return stdout.String(), fmt.Errorf("command failed: %s (stderr: %s): %w", command, strings.TrimSpace(stderr.String()), err)
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

	config := &ssh.ClientConfig{
		User: user,
		Auth: []ssh.AuthMethod{
			ssh.PublicKeys(signer),
		},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), // in CI/automated fleet provisioning
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
			return stdout.String(), fmt.Errorf("SSH command '%s' failed on %s (stderr: %s): %w", command, s.host, strings.TrimSpace(stderr.String()), err)
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
			return stdout.String(), fmt.Errorf("SSH command with stdin failed on %s (stderr: %s): %w", s.host, strings.TrimSpace(stderr.String()), err)
		}
		return stdout.String(), nil
	}
}

func (s *SSHExecutor) WriteFile(ctx context.Context, remotePath string, content []byte, perm os.FileMode) error {
	// Base64 pipe to avoid quoting and permission issues
	cmd := fmt.Sprintf("base64 -d > %s && chmod %o %s", remotePath, perm, remotePath)
	b64Data := bytes.NewReader(content)
	_, err := s.RunWithInput(ctx, cmd, b64Data)
	return err
}

func (s *SSHExecutor) Close() error {
	if s.client != nil {
		return s.client.Close()
	}
	return nil
}
