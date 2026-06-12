//go:build e2e

package harness

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"time"

	"golang.org/x/crypto/ssh"
)

// SSH is the transport scenarios use to run commands on a remote Node.
//
// It is an interface so dry-run mode and unit tests can stub it without
// opening real network connections. Run returns the command's combined
// stdout+stderr and a non-nil error if the remote command exited non-zero or
// the session failed.
type SSH interface {
	Run(ctx context.Context, node Node, cmd string) ([]byte, error)
	Close() error
}

// sshClient is the production SSH transport, backed by
// golang.org/x/crypto/ssh. One client is opened per (user, key, node) tuple
// on first use and cached for the harness lifetime.
type sshClient struct {
	user       string
	signer     ssh.Signer
	conns      map[string]*ssh.Client
	connectDur time.Duration
	runDur     time.Duration
}

var _ SSH = (*sshClient)(nil)

// NewSSH returns an SSH transport configured from a Cluster. Host keys are not
// verified: the harness targets short-lived infra where TOFU adds noise and no
// security benefit.
func NewSSH(cluster *Cluster) (SSH, error) {
	keyBytes, err := os.ReadFile(cluster.SSHKeyPath)
	if err != nil {
		return nil, fmt.Errorf("e2e harness: read ssh key %s: %w", cluster.SSHKeyPath, err)
	}
	signer, err := ssh.ParsePrivateKey(keyBytes)
	if err != nil {
		return nil, fmt.Errorf("e2e harness: parse ssh key %s: %w", cluster.SSHKeyPath, err)
	}
	return &sshClient{
		user:       cluster.SSHUser,
		signer:     signer,
		conns:      make(map[string]*ssh.Client),
		connectDur: 10 * time.Second,
		runDur:     2 * time.Minute,
	}, nil
}

func (s *sshClient) dial(node Node) (*ssh.Client, error) {
	if c, ok := s.conns[node.Addr]; ok {
		return c, nil
	}
	cfg := &ssh.ClientConfig{
		User:            s.user,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(s.signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), //nolint:gosec // test harness, not production
		Timeout:         s.connectDur,
	}
	c, err := ssh.Dial("tcp", net.JoinHostPort(node.Addr, "22"), cfg)
	if err != nil {
		return nil, fmt.Errorf("e2e harness: ssh dial %s: %w", node.Addr, err)
	}
	s.conns[node.Addr] = c
	return c, nil
}

func (s *sshClient) Run(ctx context.Context, node Node, cmd string) ([]byte, error) {
	client, err := s.dial(node)
	if err != nil {
		return nil, err
	}
	session, err := client.NewSession()
	if err != nil {
		return nil, fmt.Errorf("e2e harness: ssh new session %s: %w", node.Name, err)
	}
	defer func() { _ = session.Close() }()

	var out bytes.Buffer
	session.Stdout = &out
	session.Stderr = &out

	// Signal(KILL) is the portable way to interrupt a running command.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = session.Signal(ssh.SIGKILL)
			_ = session.Close()
		case <-done:
		}
	}()

	if err := session.Run(cmd); err != nil {
		return out.Bytes(), fmt.Errorf("e2e harness: ssh run on %s (%q): %w\noutput: %s",
			node.Name, cmd, err, out.String())
	}
	return out.Bytes(), nil
}

func (s *sshClient) Close() error {
	var firstErr error
	for addr, c := range s.conns {
		if err := c.Close(); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("e2e harness: ssh close %s: %w", addr, err)
		}
		delete(s.conns, addr)
	}
	return firstErr
}
