//go:build lambdatestlink

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/mulgadc/spinifex/spinifex/agents/lambda/agent"
	"github.com/mulgadc/spinifex/spinifex/agents/lambda/hostlink"
	"github.com/mulgadc/spinifex/spinifex/agents/lambda/runtimeapi"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/lambdaenv"
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
	caBundlePath   string
	systemCAPath   string
}

func start(ctx context.Context) int {
	t := testlink{
		startup:      defaultStartup(),
		pkg:          devTaskPackage(),
		runtimeAddr:  runtimeAddr,
		caBundlePath: sdkBundlePath,
		systemCAPath: systemBundlePath,
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
	sdk, sdkErr := readSDKConfig(t.fwCfgDir)
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
	// A host that sent no credential is refused before anything is installed.
	if first.Credential == nil || !first.Credential.Complete() {
		return exitRefused, errors.New("first frame carries no execution credential")
	}
	cred := *first.Credential
	first.Credential = nil
	if err := lambdaenv.Validate(cred.Environment); err != nil {
		return exitRefused, errors.New("first frame carries an invalid function environment")
	}
	if sdkErr != nil {
		slog.Warn("lambda-agent: SDK gateway settings unavailable", "err", sdkErr)
	}
	if sdk != nil {
		if err := sdk.writeBundle(t.caBundlePath, t.systemCAPath); err != nil {
			slog.Warn("lambda-agent: SDK gateway settings unavailable", "err", err)
			sdk = nil
		}
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
	region, _, _ := strings.Cut(string(scope), "/")
	cmd, err := t.startBootstrap(ctx, bootstrapEnv(ln.Addr().String(), spec.handler, cred, region, sdk, t.caBundlePath))
	if err != nil {
		return exitLink, fmt.Errorf("start bootstrap: %w", err)
	}
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		err := cmd.Wait()
		// Once a response is being received the result stands, so only an
		// exit before that cancels the invocation.
		if srv.BootstrapExited() {
			cancel(fmt.Errorf("bootstrap exited: %w", err))
		}
	}()

	err = agent.Run(ctx, &oneInvocation{Link: link, first: &first}, srv)
	cancel(errors.New("agent stopping"))
	<-exited
	if err != nil {
		return exitLink, err
	}
	return exitClosed, nil
}

// bootstrapEnv supplies the runtime, execution-role and SDK settings.
func bootstrapEnv(runtimeAPI, handler string, cred hostlink.Credential, region string, sdk *sdkConfig, caPath string) []string {
	vars := map[string]string{
		"AWS_LAMBDA_RUNTIME_API": runtimeAPI,
		"_HANDLER":               handler,
		"LAMBDA_TASK_ROOT":       taskRoot,
		"PATH":                   "/bin:/sbin:/usr/bin",
		"AWS_ACCESS_KEY_ID":      cred.AccessKeyID,
		"AWS_SECRET_ACCESS_KEY":  cred.SecretAccessKey,
		"AWS_SESSION_TOKEN":      cred.SessionToken,
		"AWS_REGION":             region,
		"AWS_DEFAULT_REGION":     region,
	}
	if sdk != nil {
		vars["AWS_ENDPOINT_URL"], vars["AWS_CA_BUNDLE"] = sdk.endpoint, caPath
	}
	for k, v := range cred.Environment {
		vars[k] = v
	}
	keys := make([]string, 0, len(vars))
	for k := range vars {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	env := make([]string, 0, len(keys))
	for _, k := range keys {
		env = append(env, k+"="+vars[k])
	}
	return env
}

// shutdownGrace is how long past the invocation deadline the agent waits for
// the host's shutdown frame once its result is written.
const shutdownGrace = 2 * time.Second

// oneInvocation serves the host's first request and then stops the agent:
// each test VM runs a single invocation. After the result is written it waits
// for the host's shutdown frame or EOF, so the result is not cut off by the
// poweroff that follows the agent's exit.
type oneInvocation struct {
	hostlink.Link
	first      *hostlink.Request
	deadlineMs int64
}

var _ hostlink.Link = (*oneInvocation)(nil)

func (o *oneInvocation) Next(ctx context.Context) (hostlink.Request, error) {
	if err := ctx.Err(); err != nil {
		return hostlink.Request{}, err
	}
	if o.first != nil {
		req := *o.first
		o.first = nil
		o.deadlineMs = req.DeadlineMs
		return req, nil
	}
	return hostlink.Request{}, o.awaitShutdown(ctx)
}

// awaitShutdown reads the host's next frame and always ends the agent: with
// ErrShutdown for a shutdown frame, EOF, a stray frame or an expired wait.
func (o *oneInvocation) awaitShutdown(ctx context.Context) error {
	end := time.UnixMilli(o.deadlineMs)
	if now := time.Now(); end.Before(now) {
		end = now
	}
	wctx, cancel := context.WithDeadline(ctx, end.Add(shutdownGrace))
	defer cancel()
	_, err := o.Link.Next(wctx)
	switch {
	case errors.Is(err, hostlink.ErrShutdown), errors.Is(err, io.EOF):
		return hostlink.ErrShutdown
	case err == nil:
		slog.Warn("lambda-agent: frame after the invocation was not dispatched")
		return hostlink.ErrShutdown
	case ctx.Err() != nil:
		return err
	case wctx.Err() != nil:
		slog.Warn("lambda-agent: no shutdown from the host; exiting")
		return hostlink.ErrShutdown
	}
	return err
}
