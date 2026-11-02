//go:build lambdatestlink

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/mulgadc/spinifex/spinifex/agents/lambda/agent"
	"github.com/mulgadc/spinifex/spinifex/agents/lambda/hostlink"
	"github.com/mulgadc/spinifex/spinifex/agents/lambda/runtimeapi"
)

const (
	runtimeAddr = "127.0.0.1:9001"
	taskRoot    = "/var/task"
)

var (
	linkPath = flag.String("link", os.Getenv("LAMBDA_AGENT_LINK"), "test carrier device path")
	handler  = flag.String("handler", os.Getenv("_HANDLER"), "function handler passed to the bootstrap")
)

func start(parent context.Context) int {
	if *linkPath == "" {
		slog.Error("no test carrier configured; refusing to start")
		return 2
	}
	// The bootstrap is tenant code; fw_cfg must be unreadable before it runs.
	if err := hostlink.SealFwCfg(hostlink.FwCfgRoot, hostlink.UnloadFwCfg); err != nil {
		slog.Error("seal fw_cfg; refusing to start", "err", err)
		return 2
	}
	dev, err := os.OpenFile(*linkPath, os.O_RDWR, 0)
	if err != nil {
		slog.Error("open test carrier; refusing to start", "err", err)
		return 2
	}
	defer dev.Close()
	link := hostlink.NewTestStream(dev)

	srv := runtimeapi.New()
	ln, err := net.Listen("tcp", runtimeAddr)
	if err != nil {
		slog.Error("listen runtime api", "err", err)
		return 1
	}
	hs := &http.Server{Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = hs.Serve(ln) }()
	defer hs.Close()

	ctx, cancel := context.WithCancelCause(parent)
	defer cancel(nil)

	cmd, err := agent.StartBootstrap(ctx, taskRoot+"/bootstrap", []string{
		"AWS_LAMBDA_RUNTIME_API=" + runtimeAddr,
		"_HANDLER=" + *handler,
		"LAMBDA_TASK_ROOT=" + taskRoot,
		"PATH=/bin:/sbin:/usr/bin",
	}, os.Stdout, os.Stderr)
	if err != nil {
		slog.Error("start bootstrap", "err", err)
		return 1
	}
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		err := cmd.Wait()
		cancel(fmt.Errorf("bootstrap exited: %w", err))
	}()

	err = agent.Run(ctx, link, srv)
	cancel(errors.New("agent stopping"))
	<-exited
	if err != nil {
		slog.Error("agent stopped", "err", err)
		return 1
	}
	return 0
}
