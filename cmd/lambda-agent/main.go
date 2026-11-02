// lambda-agent runs inside a Lambda guest. It serves the Lambda Runtime API on
// guest loopback, starts the function's bootstrap, and relays invocations
// between the host link and that runtime.
//
// A normal build has no host transport and refuses to start; the unauthenticated
// test carrier exists only in lambdatestlink builds. The agent never listens for
// the host.
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
	"os/exec"
	"syscall"
	"time"

	"github.com/mulgadc/spinifex/spinifex/agents/lambda/agent"
	"github.com/mulgadc/spinifex/spinifex/agents/lambda/runtimeapi"
)

const (
	runtimeAddr = "127.0.0.1:9001"
	taskRoot    = "/var/task"
)

func main() { os.Exit(run()) }

func run() int {
	handler := flag.String("handler", os.Getenv("_HANDLER"), "function handler passed to the bootstrap")
	flag.Parse()

	link, closer, err := openHostLink()
	if err != nil {
		slog.Error("no host link; refusing to start", "err", err)
		return 2
	}
	defer closer.Close()

	srv := runtimeapi.New()
	ln, err := net.Listen("tcp", runtimeAddr)
	if err != nil {
		slog.Error("listen runtime api", "err", err)
		return 1
	}
	hs := &http.Server{Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = hs.Serve(ln) }()
	defer hs.Close()

	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)

	cmd := exec.CommandContext(ctx, taskRoot+"/bootstrap")
	cmd.Env = []string{
		"AWS_LAMBDA_RUNTIME_API=" + runtimeAddr,
		"_HANDLER=" + *handler,
		"LAMBDA_TASK_ROOT=" + taskRoot,
		"PATH=/bin:/sbin:/usr/bin",
	}
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Start(); err != nil {
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
