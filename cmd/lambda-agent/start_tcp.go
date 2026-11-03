//go:build !lambdatestlink

package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
)

// errNoProtocol is returned when the host sends anything: this build defines no
// message protocol, so it accepts no work.
var errNoProtocol = errors.New("host sent data, but this build accepts no invocations")

func start(ctx context.Context) int {
	code, err := defaultStartup().run(ctx)
	if err != nil {
		slog.Error("lambda-agent stopping", "exit", code, "err", err)
	}
	return code
}

// run authenticates to the host and holds the connection until it closes.
func (s startup) run(ctx context.Context) (int, error) {
	conn, _, code, err := s.connect(ctx)
	if err != nil {
		return code, err
	}
	defer conn.Close()
	slog.Info("control connection open; invocations are not enabled in this build", "host", conn.RemoteAddr().String())
	if err := awaitHostClose(ctx, conn); err != nil {
		return exitLink, err
	}
	return exitClosed, nil
}

// awaitHostClose reads until the host closes the connection. Under TLS 1.3 a
// host that rejects the guest certificate is only seen here, on first read.
func awaitHostClose(ctx context.Context, conn *tls.Conn) error {
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	n, err := conn.Read(make([]byte, 1))
	switch {
	case n > 0:
		return errNoProtocol
	case errors.Is(err, io.EOF):
		return nil
	case ctx.Err() != nil:
		return fmt.Errorf("control connection: %w", context.Cause(ctx))
	default:
		return fmt.Errorf("control connection: %w", err)
	}
}
