//go:build lambdatestlink

package testlink

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	lambdav1 "github.com/mulgadc/spinifex/contracts/lambda/v1"
	"github.com/mulgadc/spinifex/spinifex/agents/lambda/hostlink"
	"github.com/mulgadc/spinifex/spinifex/domains/lambda/execution"
	"github.com/mulgadc/spinifex/spinifex/domains/network/host"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/lambda/attempt"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/lambda/guestconn"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/vm"
	"github.com/nats-io/nats.go"
)

const (
	fetchTimeout     = 60 * time.Second
	handshakeTimeout = 10 * time.Second
	maxTimeoutMs     = 900_000
	fwCfgNetcfg      = "opt/spinifex/netcfg"
	fwCfgPackage     = lambdav1.FwCfgPrefix + "test-package-"
)

const markerSuffix = ".attempt"

var attemptIDPattern = regexp.MustCompile(`^[0-9a-f]{12}$`)

// Taps creates the guest's tap without replacing anything, and removes only
// a tap it created.
type Taps interface {
	CreateOwnedTap(spec vm.TapSpec, owner string) (host.OwnedTap, error)
	RemoveOwnedTap(tap host.OwnedTap) error
}

// Grants is the execution owner's side of an attempt the daemon runs.
type Grants interface {
	Start(ctx context.Context, g execution.Grant, want execution.Identity) error
	Launched(ctx context.Context, g execution.Grant, l execution.Launch) error
	Accept(ctx context.Context, g execution.Grant) error
	Released(ctx context.Context, g execution.Grant) error
	Stopped(ctx context.Context, g execution.Grant) error
	Ambiguous(ctx context.Context, g execution.Grant) error
	Current(ctx context.Context, node string) (execution.Attempt, error)
}

var _ Grants = (*execution.Store)(nil)

// IPs hands out and takes back management-bridge addresses.
type IPs interface {
	Allocate(id string) (string, error)
	ReleaseChecked(id string) error
}

// Daemon runs one test-link invocation at a time on this node.
type Daemon struct {
	Node     string
	Grants   Grants
	Taps     Taps
	IPs      IPs
	Bridge   string
	HostIP   string
	MTU      int
	ImageDir string
	Firmware string
	WorkDir  string
	HTTP     *http.Client
	// Initializer is the hard-kill library preloaded into QEMU, and
	// InitializerSHA256 the digest it must have.
	Initializer       string
	InitializerSHA256 string

	busy atomic.Bool
}

// unresolved names an attempt whose marker is still on disk. A marker goes
// only once the attempt's VM stop is proven and everything it held is
// released, so it outlives a daemon restart; nothing resolves it on its own.
func (d *Daemon) unresolved() (string, error) {
	markers, err := filepath.Glob(filepath.Join(d.WorkDir, "*"+markerSuffix))
	if err != nil {
		return "", err
	}
	if len(markers) == 0 {
		return "", nil
	}
	return strings.TrimSuffix(filepath.Base(markers[0]), markerSuffix), nil
}

// Serve answers requests on this node's subject until the subscription ends.
func (d *Daemon) Serve(ctx context.Context, nc *nats.Conn, node string) (*nats.Subscription, error) {
	return nc.Subscribe(Subject(node), func(m *nats.Msg) {
		go func() {
			rep := d.Handle(ctx, m.Data)
			body, err := json.Marshal(rep)
			if err == nil && int64(len(body)) > nc.MaxPayload() {
				body, err = json.Marshal(Reply{Outcome: OutcomeUnknown, Error: "result too large for the test path"})
			}
			if err != nil {
				slog.Error("Lambda test link: encode reply", "err", err)
				return
			}
			if err := m.Respond(body); err != nil {
				slog.Error("Lambda test link: send reply", "err", err)
			}
		}()
	})
}

// Handle runs one request under its grant. It refuses before launch when it
// can; after launch it reports done only with a result the owner accepted and
// a proven VM stop.
func (d *Daemon) Handle(ctx context.Context, data []byte) Reply {
	var req Request
	if err := json.Unmarshal(data, &req); err != nil {
		return refused("unreadable request")
	}
	if err := CheckRequest(req); err != nil {
		return refused(err.Error())
	}
	if req.Grant.Node != d.Node {
		return refused("grant names another node")
	}
	if !d.busy.CompareAndSwap(false, true) {
		return refused("another test-link attempt holds this node's slot")
	}
	// The slot stays taken while any attempt is unresolved.
	held := false
	defer func() {
		if !held {
			d.busy.Store(false)
		}
	}()
	if id, err := d.unresolved(); err != nil || id != "" {
		return refused("an unresolved test-link attempt holds this node's slot")
	}
	// Without pidfs a restarted daemon could not identify the VM it left.
	if _, err := selfInode(); err != nil {
		return refused(err.Error())
	}
	lib, err := d.sealInitializer()
	if err != nil {
		return refused(err.Error())
	}
	defer func() { _ = lib.Close() }()
	// The owner refuses a stale, replayed or mismatched grant before anything
	// is taken; a write it cannot confirm leaves the attempt possibly started.
	if err := d.Grants.Start(ctx, req.Grant, req.Identity()); err != nil {
		if errors.Is(err, execution.ErrOutcomeUnknown) {
			slog.Error("Lambda test link: grant start outcome unknown; node held", "attempt", req.AttemptID, "err", err)
			return Reply{Outcome: OutcomeUnknown, Error: "grant start outcome unknown"}
		}
		return refused(err.Error())
	}

	var rep Reply
	resolved := true
	marker := ""
	if code, err := d.fetch(ctx, req); err != nil {
		rep = refused(err.Error())
	} else {
		rep, resolved, marker = d.run(ctx, req, code, lib)
	}
	held = !d.finish(ctx, req.Grant, resolved, marker)
	return rep
}

// finish tells the owner how the attempt ended, ambiguous unless the VM stop
// is proven and everything released. It reports whether the node is free.
func (d *Daemon) finish(ctx context.Context, g execution.Grant, resolved bool, marker string) bool {
	if !resolved {
		if err := d.Grants.Ambiguous(ctx, g); err != nil {
			slog.Error("Lambda test link: ambiguous record outcome unknown", "attempt", g.AttemptID, "err", err)
		}
		return false
	}
	if err := d.Grants.Released(ctx, g); err != nil {
		// The write may have landed; the stop was proven before it either way.
		slog.Error("Lambda test link: release record outcome unknown; marker kept", "attempt", g.AttemptID, "err", err)
		return false
	}
	return d.complete(ctx, g, marker)
}

// complete removes a released attempt's marker and only then frees the node,
// so the owner keeps the node held for as long as the marker can remain.
func (d *Daemon) complete(ctx context.Context, g execution.Grant, marker string) bool {
	if !removeMarker(marker) {
		return false
	}
	if err := d.Grants.Stopped(ctx, g); err != nil {
		slog.Error("Lambda test link: stop record outcome unknown; node held until recovery", "attempt", g.AttemptID, "err", err)
		return false
	}
	return true
}

// removeMarker removes an attempt marker; "" names none. Already gone is success.
func removeMarker(marker string) bool {
	if marker == "" {
		return true
	}
	if err := os.Remove(marker); err != nil && !errors.Is(err, os.ErrNotExist) {
		slog.Error("Lambda test link: remove attempt marker", "marker", marker, "err", err)
		return false
	}
	return true
}

// CheckRequest is the daemon's validation of a test-link request.
func CheckRequest(req Request) error {
	if !attemptIDPattern.MatchString(req.AttemptID) {
		return errors.New("attempt ID must be 12 lowercase hex digits")
	}
	if req.Grant.GrantID == "" || req.Grant.AttemptID != req.AttemptID {
		return errors.New("request carries no grant for its attempt")
	}
	if _, err := lambdav1.ParseScope(scope(req)); err != nil || req.Grant.Generation == 0 ||
		strings.ContainsAny(req.Grant.GrantID+req.Grant.EnvironmentID, "/\n") {
		return errors.New("grant names no valid environment generation")
	}
	if req.RequestID == "" || req.RevisionID == "" {
		return errors.New("request and revision IDs are required")
	}
	if req.TimeoutMs <= 0 || req.TimeoutMs > maxTimeoutMs {
		return fmt.Errorf("timeout %d ms outside 1..%d", req.TimeoutMs, maxTimeoutMs)
	}
	if req.CodeSize <= 0 || req.CodeSize > MaxCodeBytes {
		return fmt.Errorf("code size %d outside 1..%d bytes", req.CodeSize, MaxCodeBytes)
	}
	if b, err := hex.DecodeString(req.CodeSHA256); err != nil || len(b) != sha256.Size {
		return errors.New("code digest is not 64 hex digits")
	}
	if _, err := lambdav1.ParseScope(scope(req)); err != nil {
		return errors.New("region, account or function ID is not a valid scope segment")
	}
	u, err := url.Parse(req.CodeURL)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return errors.New("code URL must be HTTPS")
	}
	return nil
}

// scope is the environment generation the owner granted; the guest and host
// certificates carry it, so the guest authenticates the grant's environment.
func scope(req Request) string {
	return req.Region + "/" + req.AccountID + "/" + req.FunctionID + "/" +
		req.Grant.EnvironmentID + "/" + strconv.FormatUint(req.Grant.Generation, 10)
}

// grantEntry is the test-grant fw_cfg value the guest binds its one run to:
// grant ID, attempt ID, environment ID and generation, one per line.
func grantEntry(g execution.Grant) []byte {
	return fmt.Appendf(nil, "%s\n%s\n%s\n%d\n", g.GrantID, g.AttemptID, g.EnvironmentID, g.Generation)
}

// fetch downloads the code copy over verified TLS and checks its exact size
// and digest. Errors never carry the URL.
func (d *Daemon) fetch(ctx context.Context, req Request) ([]byte, error) {
	fctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	hreq, err := http.NewRequestWithContext(fctx, http.MethodGet, req.CodeURL, nil)
	if err != nil {
		return nil, errors.New("code download: bad request")
	}
	client := d.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(hreq)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return nil, fmt.Errorf("code download: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("code download: status %d", resp.StatusCode)
	}
	code, err := io.ReadAll(io.LimitReader(resp.Body, req.CodeSize+1))
	if err != nil {
		return nil, errors.New("code download: read failed")
	}
	if int64(len(code)) != req.CodeSize {
		return nil, fmt.Errorf("code download: got %d bytes, want %d", len(code), req.CodeSize)
	}
	sum := sha256.Sum256(code)
	if hex.EncodeToString(sum[:]) != req.CodeSHA256 {
		return nil, errors.New("code download: digest mismatch")
	}
	return code, nil
}

// run holds the marker, address, tap and files from before allocation until
// the VM stop is proven, and keeps them all when it is not. resolved is true
// only once everything is released.
func (d *Daemon) run(ctx context.Context, req Request, code []byte, lib *os.File) (rep Reply, resolved bool, marker string) {
	a := &held{id: "lambda-" + req.AttemptID, dir: filepath.Join(d.WorkDir, req.AttemptID),
		marker: filepath.Join(d.WorkDir, req.AttemptID+markerSuffix)}
	f, err := os.OpenFile(a.marker, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return refused("attempt marker unavailable"), true, ""
	}
	_ = f.Close()
	marker = a.marker

	ip, err := d.IPs.Allocate(a.id)
	if err != nil {
		// A failed allocation may still have written the address, so it stays held.
		slog.Error("Lambda test link: address allocation outcome unknown; attempt held", "attempt", req.AttemptID, "err", err)
		return refused("management address unavailable"), false, marker
	}
	a.ip = true
	tapName := "lm" + req.AttemptID
	a.tap, err = d.Taps.CreateOwnedTap(vm.TapSpec{Name: tapName, Bridge: d.Bridge, MTU: d.MTU}, a.id)
	if err != nil {
		slog.Error("Lambda test link: guest tap", "attempt", req.AttemptID, "err", err)
		if a.tap.Name == "" && !errors.Is(err, host.ErrTapNotCreated) {
			// A link may exist that this attempt cannot identify, so it stays held.
			return refused("guest tap setup failed"), false, marker
		}
		return refused("guest tap setup failed"), d.release(a), marker
	}
	mac := vm.GenerateMgmtMAC(a.id)
	cfg, gen, ln, err := d.prepare(req, code, a.dir, tapName, mac, ip)
	if err != nil {
		slog.Error("Lambda test link: prepare attempt", "attempt", req.AttemptID, "err", err)
		return refused("attempt preparation failed"), d.release(a), marker
	}
	defer func() { _ = ln.Close() }()

	cmd, err := cfg.Execute()
	if err != nil {
		return refused("VM command build failed"), d.release(a), marker
	}
	hk, err := armCommand(cmd, lib, timeoutFor(req)+HardKillMargin)
	if err != nil {
		return refused("hard-kill setup failed"), d.release(a), marker
	}
	defer hk.close()
	var result Reply
	unrecorded := false
	out, runErr := attempt.Run(ctx, cmd, timeoutFor(req), func(sctx context.Context, pidfd int) error {
		// The owner holds the launch identity before the guest is sent anything.
		l, err := launchIdentity(pidfd, cmd.Process.Pid, a.tap)
		if err != nil {
			return err
		}
		if err := d.Grants.Launched(sctx, req.Grant, l); err != nil {
			unrecorded = true
			return fmt.Errorf("%w: %w", ErrLaunchNotRecorded, err)
		}
		// No frame reaches the guest until QEMU proves its own kill timer is armed.
		if err := hk.await(sctx, cmd.Process.Pid, armWait); err != nil {
			return err
		}
		r, err := d.session(sctx, req, gen, ln, a.dir, d.Grants.Accept)
		result = r
		return err
	})
	if !out.Stopped {
		slog.Error("Lambda test link: VM stop not proven; marker, tap, address and files held",
			"attempt", req.AttemptID, "tap", tapName, "err", runErr)
		return Reply{Outcome: OutcomeUnknown, Error: "VM stop not proven"}, false, marker
	}
	// What the callback set is read only once it has returned.
	select {
	case <-out.SessionDone:
	default:
		slog.Error("Lambda test link: session still running after the VM stopped; attempt held", "attempt", req.AttemptID)
		return Reply{Outcome: OutcomeUnknown, Error: "session did not finish"}, false, marker
	}
	if unrecorded {
		slog.Error("Lambda test link: launch identity write failed; attempt held as ambiguous",
			"attempt", req.AttemptID, "err", runErr)
		return Reply{Outcome: OutcomeUnknown, Error: ErrLaunchNotRecorded.Error()}, false, marker
	}
	resolved = d.release(a)
	if runErr != nil {
		slog.Warn("Lambda test link: attempt ended without a result", "attempt", req.AttemptID, "timed_out", out.TimedOut, "err", runErr)
		return Reply{Outcome: OutcomeUnknown, Error: safeRunError(runErr)}, resolved, marker
	}
	return result, resolved, marker
}

func safeRunError(err error) string {
	for _, known := range []error{ErrLaunchNotRecorded, ErrVMGone, ErrNoPidfs, ErrNotArmed, ErrResultNotAccepted, ErrFunctionTimeout, attempt.ErrTimedOut, attempt.ErrVMExited, attempt.ErrNoPidfd, context.Canceled, context.DeadlineExceeded} {
		if errors.Is(err, known) {
			return known.Error()
		}
	}
	return "guest session failed"
}

// prepare writes the package disk and fw_cfg files and opens the listener the
// guest dials.
func (d *Daemon) prepare(req Request, code []byte, dir, tap, mac, ip string) (*vm.Config, *guestconn.Generation, *net.TCPListener, error) {
	if err := os.Mkdir(dir, 0o700); err != nil {
		return nil, nil, nil, err
	}
	gen, err := guestconn.NewGeneration(lambdav1.Scope(scope(req)), timeoutFor(req)+time.Hour)
	if err != nil {
		return nil, nil, nil, err
	}
	ln, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.ParseIP(d.HostIP)})
	if err != nil {
		return nil, nil, nil, err
	}
	fail := func(err error) (*vm.Config, *guestconn.Generation, *net.TCPListener, error) {
		_ = ln.Close()
		return nil, nil, nil, err
	}
	// The guest reads exactly CodeSize bytes; the padding only makes a whole sector.
	disk := make([]byte, (len(code)+511)/512*512)
	copy(disk, code)
	diskPath := filepath.Join(dir, "package.raw")
	if err := os.WriteFile(diskPath, disk, 0o600); err != nil {
		return fail(err)
	}
	entries := gen.GuestEntries(ln.Addr().String())
	entries["test-package-size"] = []byte(strconv.FormatInt(req.CodeSize, 10))
	entries["test-package-sha256"] = []byte(req.CodeSHA256)
	entries["test-grant"] = grantEntry(req.Grant)
	var fwCfg []vm.FwCfgEntry
	for name, value := range entries {
		path := filepath.Join(dir, "fwcfg-"+name)
		if err := os.WriteFile(path, value, 0o600); err != nil {
			return fail(err)
		}
		fwCfg = append(fwCfg, vm.FwCfgEntry{Name: lambdav1.FwCfgPrefix + name, File: path})
	}
	netcfg := filepath.Join(dir, "netcfg")
	if err := os.WriteFile(netcfg, fmt.Appendf(nil, "NIC0_MAC=%s\nNIC0_CIDR=%s/24\nNIC0_DEFAULT=0\n", mac, ip), 0o600); err != nil {
		return fail(err)
	}
	fwCfg = append(fwCfg, vm.FwCfgEntry{Name: fwCfgNetcfg, File: netcfg})

	cfg := vm.NewMicroVMConfig(vm.MicroVMSpec{
		Name:         "lambda-" + req.AttemptID,
		Architecture: "x86_64",
		VCPUs:        1,
		MemoryMiB:    int(max(req.MemoryMiB, 128)),
		Firmware:     d.Firmware,
		ImageDir:     d.ImageDir,
		LogLevel:     3,
		FwCfg:        fwCfg,
		NetDevs:      []vm.NetDev{vm.TapNetDev("net0", tap, 1)},
	})
	cfg.Devices = append(cfg.Devices, vm.NetDevice(cfg.MachineType, "net0", mac, 1, d.MTU))
	cfg.Drives = append(cfg.Drives, vm.Drive{File: diskPath, Format: "raw", If: "none", ID: "pkg", ReadOnly: true})
	blk := "virtio-blk-pci,drive=pkg"
	if vm.IsMMIO(cfg.MachineType) {
		blk = "virtio-blk-device,drive=pkg"
	}
	cfg.Devices = append(cfg.Devices, vm.Device{Value: blk})
	return &cfg, gen, ln, nil
}

func timeoutFor(req Request) time.Duration {
	return time.Duration(req.TimeoutMs)*time.Millisecond + BootAllowance
}

// session accepts the guest, sends the one invocation and checks the result
// answers it before the owner records it accepted. The guest key file goes
// once the guest has authenticated.
func (d *Daemon) session(ctx context.Context, req Request, gen *guestconn.Generation, ln *net.TCPListener, dir string,
	accept func(context.Context, execution.Grant) error) (Reply, error) {
	conn, err := gen.AcceptGuest(ctx, ln, handshakeTimeout)
	if err != nil {
		return Reply{}, fmt.Errorf("accept guest: %w", err)
	}
	defer func() { _ = conn.Close() }()
	_ = os.Remove(filepath.Join(dir, "fwcfg-"+lambdav1.EntryGuestKey))

	// The function timeout runs on the host from dispatch; the boot allowance
	// covers startup only, so a guest that stops answering is cut off here.
	deadline := now().Add(time.Duration(req.TimeoutMs) * time.Millisecond)
	if err := conn.SetDeadline(deadline); err != nil {
		return Reply{}, fmt.Errorf("set guest deadline: %w", err)
	}
	rctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	stream := hostlink.NewTestStream(conn)
	if err := stream.HostSend(hostlink.Request{
		GrantID:     req.Grant.GrantID,
		AttemptID:   req.AttemptID,
		RequestID:   req.RequestID,
		FunctionARN: req.FunctionARN,
		DeadlineMs:  deadline.UnixMilli(),
		Payload:     req.Payload,
	}); err != nil {
		return Reply{}, pastDeadline(deadline, fmt.Errorf("send invocation: %w", err))
	}
	resp, err := stream.HostResult(rctx)
	if err != nil {
		return Reply{}, pastDeadline(deadline, fmt.Errorf("read result: %w", err))
	}
	// A result received once the deadline has passed is still a timeout.
	if !now().Before(deadline) {
		return Reply{}, ErrFunctionTimeout
	}
	if resp.GrantID != req.Grant.GrantID || resp.AttemptID != req.AttemptID || resp.RequestID != req.RequestID {
		return Reply{}, errors.New("result answers a different request")
	}
	if err := accept(rctx, req.Grant); err != nil {
		return Reply{}, pastDeadline(deadline, fmt.Errorf("%w: %w", ErrResultNotAccepted, err))
	}
	// An acceptance that lands after the deadline is still a timeout.
	if !now().Before(deadline) {
		return Reply{}, ErrFunctionTimeout
	}
	_ = stream.HostShutdown()
	return Reply{Outcome: OutcomeDone, Payload: resp.Payload, FunctionError: resp.FunctionError}, nil
}

// now is the clock the dispatch deadline is read from; tests replace it.
var now = time.Now

// ErrResultNotAccepted reports a result the owner would not accept under the
// attempt's grant.
var ErrResultNotAccepted = errors.New("result not accepted under the current grant")

// ErrFunctionTimeout reports that the guest did not answer within the
// function timeout counted from dispatch.
var ErrFunctionTimeout = errors.New("function timed out")

func pastDeadline(deadline time.Time, err error) error {
	if !now().Before(deadline) {
		return fmt.Errorf("%w: %w", ErrFunctionTimeout, err)
	}
	return err
}

// held is what one attempt holds, in the order it took them.
type held struct {
	id, dir, marker string
	ip              bool
	tap             host.OwnedTap
}

// release gives back what an attempt held once nothing can still use it, and
// is safe to repeat. It stops at the first failure and reports whether it all
// went; the marker is not its to remove.
func (d *Daemon) release(a *held) bool {
	if err := os.RemoveAll(a.dir); err != nil {
		slog.Error("Lambda test link: remove attempt files; attempt held", "dir", a.dir, "err", err)
		return false
	}
	if a.tap.Name != "" {
		if err := d.Taps.RemoveOwnedTap(a.tap); err != nil {
			slog.Error("Lambda test link: tap removal failed; attempt held", "tap", a.tap.Name, "err", err)
			return false
		}
	}
	if a.ip {
		if err := d.IPs.ReleaseChecked(a.id); err != nil {
			slog.Error("Lambda test link: address release unconfirmed; attempt held", "attempt", a.id, "err", err)
			return false
		}
	}
	return true
}

func refused(reason string) Reply { return Reply{Outcome: OutcomeRefused, Error: reason} }
