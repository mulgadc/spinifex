package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"

	"github.com/mulgadc/spinifex/spinifex/agents/lambda/hostlink"
)

// Exit statuses: the guest refused to start, the control connection failed,
// or the host closed it.
const (
	exitRefused = 2
	exitLink    = 1
	exitClosed  = 0
)

type startup struct {
	fwCfgDir  string
	fwCfgRoot string
	unload    func() error
}

func defaultStartup() startup {
	return startup{fwCfgDir: hostlink.FwCfgDir, fwCfgRoot: hostlink.FwCfgRoot, unload: hostlink.UnloadFwCfg}
}

// connect loads the identity, seals fw_cfg whether or not the load succeeded,
// then dials the host. Under TLS 1.3 a host refusal is seen only on first read.
func (s startup) connect(ctx context.Context) (*tls.Conn, int, error) {
	id, loadErr := hostlink.LoadIdentity(s.fwCfgDir)
	if err := hostlink.SealFwCfg(s.fwCfgRoot, s.unload); err != nil {
		return nil, exitRefused, errors.Join(loadErr, err)
	}
	if loadErr != nil {
		return nil, exitRefused, loadErr
	}
	conn, err := hostlink.Dial(ctx, id)
	if err != nil {
		if ctx.Err() != nil {
			err = fmt.Errorf("%w: %w", err, context.Cause(ctx))
		}
		return nil, exitLink, err
	}
	return conn, 0, nil
}
