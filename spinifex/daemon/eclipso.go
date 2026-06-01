package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"time"
)

// EclipsoServiceName is the systemd unit that owns the eclipso DNS process.
// Installed by scripts/systemd/eclipso.service; activated by EclipsoCtl.
const EclipsoServiceName = "eclipso.service"

// eclipsoReadyTimeout caps how long Start() waits for is-active before
// returning an error so a stuck Eclipso does not block daemon startup.
const eclipsoReadyTimeout = 30 * time.Second

// eclipsoReadyPoll is the gap between is-active probes during Start().
const eclipsoReadyPoll = 500 * time.Millisecond

// EclipsoCtl drives the eclipso.service systemd unit. Start/Stop signal
// systemctl; IsRunning probes is-active; Reload sends SIGHUP via
// `systemctl reload` so Eclipso re-reads its zone files without a full
// restart. Sprint 1a wires the controller surface; spxd start-order
// integration (after NATS + predastore + IAM seed + dns-zones bucket
// + /etc/spinifex/eclipso.env write) lands in 1b.
type EclipsoCtl struct {
	unit string
}

// NewEclipsoCtl returns a controller bound to EclipsoServiceName.
func NewEclipsoCtl() *EclipsoCtl {
	return &EclipsoCtl{unit: EclipsoServiceName}
}

// Start signals systemd to bring the unit up and waits up to
// eclipsoReadyTimeout for it to report active. Returns an error if the
// systemctl invocation fails or the unit never reaches active.
func (e *EclipsoCtl) Start(ctx context.Context) error {
	if err := runSystemctl(ctx, "start", e.unit); err != nil {
		return fmt.Errorf("systemctl start %s: %w", e.unit, err)
	}
	deadline := time.Now().Add(eclipsoReadyTimeout)
	for {
		if e.IsRunning(ctx) {
			slog.Info("eclipso started", "unit", e.unit)
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("eclipso %s did not become active within %s", e.unit, eclipsoReadyTimeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(eclipsoReadyPoll):
		}
	}
}

// Stop signals systemd to bring the unit down. Returns the systemctl
// error verbatim — caller decides whether to escalate.
func (e *EclipsoCtl) Stop(ctx context.Context) error {
	if err := runSystemctl(ctx, "stop", e.unit); err != nil {
		return fmt.Errorf("systemctl stop %s: %w", e.unit, err)
	}
	slog.Info("eclipso stopped", "unit", e.unit)
	return nil
}

// IsRunning returns true when systemctl reports the unit active.
// systemctl is-active exit codes: 0 = active, non-zero = anything else.
func (e *EclipsoCtl) IsRunning(ctx context.Context) bool {
	out, err := exec.CommandContext(ctx, "systemctl", "is-active", e.unit).Output()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) == "active"
}

// Reload asks systemd to send the unit's reload signal (SIGHUP on the
// default Eclipso unit) so it re-scans zone files without a restart.
func (e *EclipsoCtl) Reload(ctx context.Context) error {
	if err := runSystemctl(ctx, "reload", e.unit); err != nil {
		return fmt.Errorf("systemctl reload %s: %w", e.unit, err)
	}
	return nil
}

// runSystemctl is the shared dispatch shim so unit tests can intercept
// process invocation through a single seam.
var runSystemctl = func(ctx context.Context, action, unit string) error {
	return exec.CommandContext(ctx, "systemctl", action, unit).Run()
}
