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

// PostgreSQL's non-exclusive backup API ties the backup to the session that
// started it: when that session ends, for any reason, the backup is aborted.
// That is what makes the hold self-expiring — a control plane that dies mid
// snapshot cannot leave the engine in backup mode forever.
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
// The statements this carries return LSNs, so it cannot appear in a result.
const sessionSentinel = "--rds-agent-ready--"

// The backup mode currently held, and the timer that ends it regardless of what
// happens to the control plane.
type quiesceHold struct {
	label   string
	session engineSession
	expiry  *time.Timer
}

// Puts the engine into backup mode: the datadir is checkpointed and the engine
// stops writing over the pages a snapshot is about to read. The hold is released
// by Unquiesce, or by its own deadline, whichever comes first.
func (e *postgresEngine) Quiesce(ctx context.Context, label string, hold time.Duration) error {
	if label == "" {
		return errors.New("quiesce requires a backup label")
	}
	if hold <= 0 {
		return errors.New("quiesce requires a positive hold deadline")
	}

	// Held across the whole start, so a second quiesce waits and then finds the
	// first one's hold rather than opening a concurrent backup alongside it.
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.held != nil {
		return fmt.Errorf("the engine is already quiesced for backup %s", e.held.label)
	}

	// Deliberately not the command's context: the session has to outlive the call
	// that started it, and its own deadline is what bounds it instead.
	session, err := e.startSess(context.WithoutCancel(ctx), command{
		Name:  e.psql,
		Args:  e.psqlArgs(),
		Env:   []string{"PATH=" + defaultGuestPath, "RDS_BACKUP_LABEL=" + label},
		Stdin: "",
		User:  e.osUser,
	})
	if err != nil {
		return fmt.Errorf("open a backup session: %w", err)
	}

	// fast forces an immediate checkpoint rather than spreading it over the
	// checkpoint interval, which would hold the snapshot open for minutes.
	const sql = `\getenv label RDS_BACKUP_LABEL
SELECT pg_backup_start(:'label', fast => true);
`
	if err := session.Exec(ctx, sql); err != nil {
		if closeErr := session.Close(); closeErr != nil {
			slog.Warn("rds-agent: closing a failed backup session", "err", closeErr)
		}
		return fmt.Errorf("put the engine into backup mode: %w", err)
	}

	e.held = &quiesceHold{
		label:   label,
		session: session,
		expiry:  time.AfterFunc(hold, func() { e.expireQuiesce(label) }),
	}
	slog.Info("rds-agent: engine quiesced for backup", "label", label, "hold", hold)
	return nil
}

// Ends the backup cleanly. A missing hold is an error rather than a silent
// success: it means the deadline fired first, so the snapshot the control plane
// just took was not taken against a held checkpoint.
func (e *postgresEngine) Unquiesce(ctx context.Context) error {
	e.mu.Lock()
	held := e.held
	e.held = nil
	e.mu.Unlock()

	if held == nil {
		return errors.New("the engine is not quiesced; the backup hold had already expired")
	}
	held.expiry.Stop()

	// The stop has to run on the session that started the backup, and the session
	// is closed either way — the engine aborts an unstopped backup with it.
	execErr := held.session.Exec(ctx, "SELECT pg_backup_stop();\n")
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

// The deadline. Ending the session is enough — the engine aborts the backup it
// was holding — so this never has to reach the engine itself.
func (e *postgresEngine) expireQuiesce(label string) {
	e.mu.Lock()
	held := e.held
	if held == nil || held.label != label {
		e.mu.Unlock()
		return
	}
	e.held = nil
	e.mu.Unlock()

	slog.Warn("rds-agent: backup hold expired; releasing the engine", "label", label)
	if err := held.session.Close(); err != nil {
		slog.Error("rds-agent: closing an expired backup session failed", "label", label, "err", err)
	}
}

// A psql child with its stdin held open, so statements can be fed to it one at a
// time and the engine keeps seeing a single session.
type psqlSession struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
	stderr *bytes.Buffer

	mu     sync.Mutex
	closed bool
}

var _ engineSession = (*psqlSession)(nil)

func execSessionRunner(ctx context.Context, c command) (engineSession, error) {
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
	return &psqlSession{cmd: cmd, stdin: stdin, stdout: bufio.NewReader(stdout), stderr: &stderr}, nil
}

// Feeds sql and waits for the sentinel that follows it. ON_ERROR_STOP makes psql
// exit on a failed statement, so a sentinel that never arrives is the failure
// itself and the engine's own message is on stderr.
func (s *psqlSession) Exec(ctx context.Context, sql string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("the backup session is closed")
	}
	if _, err := io.WriteString(s.stdin, sql+"\\echo "+sessionSentinel+"\n"); err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(s.stderr.String()))
	}

	// Read on its own goroutine so a psql that never answers costs the caller its
	// deadline rather than blocking it forever.
	done := make(chan error, 1)
	go func() { done <- s.awaitSentinel() }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *psqlSession) awaitSentinel() error {
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
func (s *psqlSession) Close() error {
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
