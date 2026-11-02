//go:build !lambdatestlink

package main

import (
	"errors"
	"io"

	"github.com/mulgadc/spinifex/spinifex/agents/lambda/hostlink"
)

// errNoHostTransport is returned until an authenticated host transport exists:
// a normal build accepts no host work.
var errNoHostTransport = errors.New("no authenticated host transport in this build")

func openHostLink() (hostlink.Link, io.Closer, error) {
	return nil, nil, errNoHostTransport
}
