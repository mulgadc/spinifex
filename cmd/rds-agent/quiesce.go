package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Both engines tie a backup hold to the session that took it: PostgreSQL aborts
// a non-exclusive backup when its session ends, MariaDB releases a BACKUP STAGE
// with its connection. That is what makes the hold self-expiring.
type engineSession interface {
	// Runs sql and waits for the engine to finish it.
	Exec(ctx context.Context, sql string) error
	// Ends the session, releasing whatever the engine tied to it.
	Close() error
}

// Starts a child process whose lifetime the caller controls, rather than one
// that is run to completion. ctx governs the process, not one statement.
type sessionRunner func(ctx context.Context, c command) (engineSession, error)

// Written after every statement so the reader knows the engine is done with it.
// The statements this carries return LSNs and backup stages, so it cannot appear
// in a result.
const sessionSentinel = "--rds-agent-ready--"

// The backup mode currently held, and the timer that ends it regardless of what
// happens to the control plane.
type quiesceHold struct {
	label   string
	session engineSession
	expiry  *time.Timer
}

// The hold bookkeeping, which is the same for both engines: only the statements
// that take and release it differ, not the single-hold rule or the deadline that
// releases it whatever happens to the caller.
type quiesceState struct {
	// Guarded because the expiry timer releases the hold on its own goroutine.
	mu   sync.Mutex
	held *quiesceHold
}

// The two things every quiesce needs, checked before a session is opened so a
// malformed command costs the engine nothing.
func validateQuiesceRequest(label string, hold time.Duration) error {
	if label == "" {
		return errors.New("quiesce requires a backup label")
	}
	if hold <= 0 {
		return errors.New("quiesce requires a positive hold deadline")
	}
	return nil
}

// Records the hold and arms its deadline. Called with mu held, so a second
// quiesce waits and then finds this hold rather than opening a concurrent
// backup alongside it.
func (q *quiesceState) beginHoldLocked(label string, session engineSession, hold time.Duration) {
	q.held = &quiesceHold{
		label:   label,
		session: session,
		expiry:  time.AfterFunc(hold, func() { q.expire(label) }),
	}
	slog.Info("rds-agent: engine quiesced for backup", "label", label, "hold", hold)
}

// Takes the hold off the engine, if one is still there, and disarms its
// deadline. The caller ends the session, because the statement that releases the
// backup has to run on it first.
func (q *quiesceState) takeHold() *quiesceHold {
	q.mu.Lock()
	defer q.mu.Unlock()

	held := q.held
	q.held = nil
	if held != nil {
		held.expiry.Stop()
	}
	return held
}

// Ends the backup cleanly, running releaseSQL on the session that took the hold.
// A missing hold is an error rather than a silent success: it means the deadline
// fired first, so what the control plane just snapshotted was not held.
func (q *quiesceState) releaseHold(ctx context.Context, releaseSQL string) error {
	held := q.takeHold()
	if held == nil {
		return errors.New("the engine is not quiesced; the backup hold had already expired")
	}

	// The release has to run on the session that took the hold, and the session is
	// closed either way — both engines end an unreleased backup with it.
	execErr := held.session.Exec(ctx, releaseSQL)
	closeErr := held.session.Close()
	if execErr != nil {
		return fmt.Errorf("take the engine out of backup mode: %w", errors.Join(execErr, closeErr))
	}
	if closeErr != nil {
		return fmt.Errorf("close the backup session: %w", closeErr)
	}
	slog.Info("rds-agent: engine released from backup mode", "label", held.label)
	return nil
}

// The deadline. Ending the session is enough — both engines release the hold
// with it — so this never has to reach the engine itself.
func (q *quiesceState) expire(label string) {
	q.mu.Lock()
	held := q.held
	if held == nil || held.label != label {
		q.mu.Unlock()
		return
	}
	q.held = nil
	q.mu.Unlock()

	slog.Warn("rds-agent: backup hold expired; releasing the engine", "label", label)
	if err := held.session.Close(); err != nil {
		slog.Error("rds-agent: closing an expired backup session failed", "label", label, "err", err)
	}
}

// A client child with its stdin held open, so statements can be fed to it one at
// a time and the engine keeps seeing a single session.
type clientSession struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
	stderr *bytes.Buffer
	// The statement that makes this client print the sentinel, which is the only
	// part of a held session that differs between engines.
	sentinel string

	mu     sync.Mutex
	closed bool
}

var _ engineSession = (*clientSession)(nil)

func execSessionRunner(ctx context.Context, c command) (engineSession, error) {
	if c.SentinelStatement == "" {
		return nil, fmt.Errorf("%s was started as a session with no sentinel statement", c.Name)
	}
	cmd := exec.CommandContext(ctx, c.Name, c.Args...)
	cmd.Env = c.Env
	if c.User != "" {
		credential, err := lookupCredential(c.User)
		if err != nil {
			return nil, err
		}
		cmd.SysProcAttr = &syscall.SysProcAttr{Credential: credential}
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("open stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("open stdout: %w", err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", c.Name, err)
	}
	return &clientSession{
		cmd:      cmd,
		stdin:    stdin,
		stdout:   bufio.NewReader(stdout),
		stderr:   &stderr,
		sentinel: c.SentinelStatement,
	}, nil
}

// Feeds sql and waits for the sentinel that follows it. Both clients are run in
// a mode that exits on a failed statement, so a sentinel that never arrives is
// the failure itself and the engine's own message is on stderr.
func (s *clientSession) Exec(ctx context.Context, sql string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("the backup session is closed")
	}
	if _, err := io.WriteString(s.stdin, sql+s.sentinel); err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(s.stderr.String()))
	}

	// Read on its own goroutine so a client that never answers costs the caller
	// its deadline rather than blocking it forever.
	done := make(chan error, 1)
	go func() { done <- s.awaitSentinel() }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *clientSession) awaitSentinel() error {
	for {
		line, err := s.stdout.ReadString('\n')
		if strings.TrimSpace(line) == sessionSentinel {
			return nil
		}
		if err != nil {
			if message := strings.TrimSpace(s.stderr.String()); message != "" {
				return errors.New(message)
			}
			return fmt.Errorf("the backup session ended: %w", err)
		}
	}
}

// Closing stdin ends the script, and the wait reaps the child. A session that
// will not end is killed, because leaving it is what would keep the engine in
// backup mode.
func (s *clientSession) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true

	if err := s.stdin.Close(); err != nil {
		slog.Debug("rds-agent: closing the backup session stdin", "err", err)
	}
	done := make(chan error, 1)
	go func() { done <- s.cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			return fmt.Errorf("%w: %s", err, strings.TrimSpace(s.stderr.String()))
		}
		return nil
	case <-time.After(sessionCloseGrace):
		if err := s.cmd.Process.Kill(); err != nil {
			return fmt.Errorf("kill the backup session: %w", err)
		}
		<-done
		return nil
	}
}

// How long a backup session gets to end on its own before it is killed. The
// engine releases the backup on either, so this only bounds the wait.
const sessionCloseGrace = 10 * time.Second
