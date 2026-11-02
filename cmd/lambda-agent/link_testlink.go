//go:build lambdatestlink

package main

import (
	"errors"
	"flag"
	"io"
	"os"

	"github.com/mulgadc/spinifex/spinifex/agents/lambda/hostlink"
)

// The test carrier is unauthenticated and exists only in lambdatestlink builds
// used for local proofs; it is not the guest protocol.
var linkPath = flag.String("link", os.Getenv("LAMBDA_AGENT_LINK"), "test carrier device path")

func openHostLink() (hostlink.Link, io.Closer, error) {
	if *linkPath == "" {
		return nil, nil, errors.New("no test carrier configured")
	}
	dev, err := os.OpenFile(*linkPath, os.O_RDWR, 0)
	if err != nil {
		return nil, nil, err
	}
	return hostlink.NewTestStream(dev), dev, nil
}
