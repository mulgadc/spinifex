package viperblockd

import (
	"context"
	"fmt"
	"net"
	"os"
	"time"
)

const (
	// nbdkitReadyPollInterval is short because readiness normally arrives in a
	// few milliseconds; the cost of asking again is a syscall.
	nbdkitReadyPollInterval = 5 * time.Millisecond

	// nbdkitReadyDeadline bounds a startup that never completes. It is longer
	// than the second this replaced because it is now a ceiling that a healthy
	// mount never reaches, rather than a delay every mount pays.
	nbdkitReadyDeadline = 10 * time.Second

	nbdkitDialTimeout = 500 * time.Millisecond
)

// pollUntil retries check until it succeeds, nbdkit exits, the context is
// cancelled or the deadline passes. exitChan is watched throughout so a
// process that dies during startup fails the mount immediately instead of
// waiting out the deadline.
func pollUntil(ctx context.Context, check func() error, exitChan <-chan int, deadline time.Time, sleep func(time.Duration)) error {
	var lastErr error
	for {
		select {
		case code := <-exitChan:
			return fmt.Errorf("nbdkit exited unexpectedly (code=%d)", code)
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		lastErr = check()
		if lastErr == nil {
			return nil
		}

		if !time.Now().Before(deadline) {
			return fmt.Errorf("nbdkit did not become ready: %w", lastErr)
		}
		sleep(nbdkitReadyPollInterval)
	}
}

// waitForSocketFile blocks until nbdkit has created its unix socket. The file
// has to exist before it can be chmod'd, and it has to be chmod'd before a
// client running as another user can connect.
func waitForSocketFile(ctx context.Context, path string, exitChan <-chan int, deadline time.Time) error {
	return pollUntil(ctx, func() error {
		_, err := os.Stat(path)
		return err
	}, exitChan, deadline, time.Sleep)
}

// waitForNBDDial blocks until nbdkit accepts a connection on its endpoint.
// This is the question the caller actually needs answered: it is about to hand
// the URI to something that will connect to it. The previous check asked
// whether the process was still alive one second after spawn, which is both
// weaker and slower than asking directly.
func waitForNBDDial(ctx context.Context, network, address string, exitChan <-chan int, deadline time.Time) error {
	return pollUntil(ctx, func() error {
		conn, err := net.DialTimeout(network, address, nbdkitDialTimeout)
		if err != nil {
			return err
		}
		// Drop the probe connection straight away. nbdkit sees a client that
		// disconnects before the handshake, which is the same thing it sees
		// whenever a client goes away.
		return conn.Close()
	}, exitChan, deadline, time.Sleep)
}
