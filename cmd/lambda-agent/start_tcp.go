//go:build !lambdatestlink

package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"

	"github.com/mulgadc/spinifex/spinifex/agents/lambda/hostlink"
)

// Exit statuses: the guest refused to start, the control connection failed,
// or the host closed it.
const (
	exitRefused = 2
	exitLink    = 1
	exitClosed  = 0
)

// errNoProtocol is returned when the host sends anything: this build defines no
// message protocol, so it accepts no work.
var errNoProtocol = errors.New("host sent data, but this build accepts no invocations")

type startup struct {
	fwCfgDir  string
	fwCfgRoot string
	unload    func() error
}

func start(ctx context.Context) int {
	s := startup{fwCfgDir: hostlink.FwCfgDir, fwCfgRoot: hostlink.FwCfgRoot, unload: hostlink.UnloadFwCfg}
	code, err := s.run(ctx)
	if err != nil {
		slog.Error("lambda-agent stopping", "exit", code, "err", err)
	}
	return code
}

// run loads the identity, seals fw_cfg whether or not the load succeeded,
// then authenticates to the host and holds the connection until it closes.
func (s startup) run(ctx context.Context) (int, error) {
	id, loadErr := hostlink.LoadIdentity(s.fwCfgDir)
	if err := hostlink.SealFwCfg(s.fwCfgRoot, s.unload); err != nil {
		return exitRefused, errors.Join(loadErr, err)
	}
	if loadErr != nil {
		return exitRefused, loadErr
	}
	conn, err := hostlink.Dial(ctx, id)
	if err != nil {
		if ctx.Err() != nil {
			err = fmt.Errorf("%w: %w", err, context.Cause(ctx))
		}
		return exitLink, err
	}
	defer conn.Close()
	slog.Info("control connection open; invocations are not enabled in this build", "host", id.HostAddr)
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
