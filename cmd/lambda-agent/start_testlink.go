//go:build lambdatestlink

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"time"

	"github.com/mulgadc/spinifex/spinifex/agents/lambda/agent"
	"github.com/mulgadc/spinifex/spinifex/agents/lambda/hostlink"
	"github.com/mulgadc/spinifex/spinifex/agents/lambda/runtimeapi"
)

const (
	runtimeAddr = "127.0.0.1:9001"
	taskRoot    = "/var/task"
)

// testlink relays one invocation over the test carrier on the authenticated
// connection; the carrier is not the guest protocol.
type testlink struct {
	startup
	pkg            taskPackage
	runtimeAddr    string
	startBootstrap func(ctx context.Context, env []string) (*exec.Cmd, error)
}

func start(ctx context.Context) int {
	t := testlink{
		startup:     defaultStartup(),
		pkg:         devTaskPackage(),
		runtimeAddr: runtimeAddr,
		startBootstrap: func(ctx context.Context, env []string) (*exec.Cmd, error) {
			return agent.StartBootstrap(ctx, taskRoot+"/bootstrap", env, os.Stdout, os.Stderr)
		},
	}
	code, err := t.run(ctx)
	if err != nil {
		slog.Error("lambda-agent stopping", "exit", code, "err", err)
	}
	return code
}

func (t testlink) run(parent context.Context) (int, error) {
	// The package values are fw_cfg entries, so read them before connect seals it.
	spec, specErr := readPackageSpec(t.fwCfgDir)
	grant, grantErr := readGrant(t.fwCfgDir)
	conn, scope, code, err := t.connect(parent)
	if err != nil {
		return code, errors.Join(err, specErr, grantErr)
	}
	defer conn.Close()
	if err := errors.Join(specErr, grantErr); err != nil {
		return exitRefused, err
	}
	link := hostlink.NewTestStream(conn)

	// The handshake can return before the host refuses the guest, so nothing
	// tenant-facing starts until the host's first valid frame arrives.
	first, err := link.Next(parent)
	if errors.Is(err, hostlink.ErrShutdown) {
		return exitClosed, nil
	}
	if err != nil {
		return exitLink, fmt.Errorf("first frame from host: %w", err)
	}
	if parent.Err() != nil {
		return exitLink, context.Cause(parent)
	}
	// Nothing of the tenant's is installed or run for an unbound request.
	if err := checkBinding(scope, grant, first, time.Now()); err != nil {
		return exitRefused, err
	}
	if err := t.pkg.install(spec); err != nil {
		return exitRefused, fmt.Errorf("install package: %w", err)
	}

	srv := runtimeapi.New()
	ln, err := net.Listen("tcp", t.runtimeAddr)
	if err != nil {
		return exitLink, fmt.Errorf("listen runtime api: %w", err)
	}
	hs := &http.Server{Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = hs.Serve(ln) }()
	defer hs.Close()

	ctx, cancel := context.WithCancelCause(parent)
	defer cancel(nil)
	cmd, err := t.startBootstrap(ctx, []string{
		"AWS_LAMBDA_RUNTIME_API=" + ln.Addr().String(),
		"_HANDLER=" + spec.handler,
		"LAMBDA_TASK_ROOT=" + taskRoot,
		"PATH=/bin:/sbin:/usr/bin",
	})
	if err != nil {
		return exitLink, fmt.Errorf("start bootstrap: %w", err)
	}
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		err := cmd.Wait()
		cancel(fmt.Errorf("bootstrap exited: %w", err))
	}()

	err = agent.Run(ctx, &oneInvocation{Link: link, first: &first}, srv)
	cancel(errors.New("agent stopping"))
	<-exited
	if err != nil {
		return exitLink, err
	}
	return exitClosed, nil
}

// oneInvocation serves the host's first request and then stops the agent:
// each test VM runs a single invocation.
type oneInvocation struct {
	hostlink.Link
	first *hostlink.Request
}

var _ hostlink.Link = (*oneInvocation)(nil)

func (o *oneInvocation) Next(ctx context.Context) (hostlink.Request, error) {
	if err := ctx.Err(); err != nil {
		return hostlink.Request{}, err
	}
	if o.first == nil {
		return hostlink.Request{}, hostlink.ErrShutdown
	}
	req := *o.first
	o.first = nil
	return req, nil
}
