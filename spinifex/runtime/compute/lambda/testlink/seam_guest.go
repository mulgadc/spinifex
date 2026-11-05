//go:build lambdatestlink && lambdatestlinkseam

package testlink

import (
	"context"
	"encoding/json"
	"math"
	"net"
	"os"
	"path/filepath"
	"time"

	lambdav1 "github.com/mulgadc/spinifex/contracts/lambda/v1"
	"github.com/mulgadc/spinifex/spinifex/agents/lambda/hostlink"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/lambda/guestconn"
)

// HandleWithGuest answers one encoded request through Handle's grant, fetch,
// session and finish steps, serving the session to guest on loopback instead
// of a launched VM. It exists only for composed tests.
func (d *Daemon) HandleWithGuest(ctx context.Context, data []byte, cred Credential, guest func(context.Context, net.Conn)) Reply {
	var req Request
	if err := json.Unmarshal(data, &req); err != nil {
		return refused("unreadable request")
	}
	if !cred.Complete() {
		return refused("no execution credential")
	}
	req.cred = cred
	if err := CheckRequest(req); err != nil {
		return refused(err.Error())
	}
	rep, _ := d.attempt(ctx, req, func([]byte) (Reply, bool, string) {
		r, err := d.loopbackSession(ctx, req, guest)
		return sessionReply(r, err), true, ""
	})
	return rep
}

func (d *Daemon) loopbackSession(ctx context.Context, req Request, guest func(context.Context, net.Conn)) (Reply, error) {
	gen, err := guestconn.NewGeneration(lambdav1.Scope(scope(req)), time.Hour)
	if err != nil {
		return Reply{}, err
	}
	ln, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		return Reply{}, err
	}
	defer func() { _ = ln.Close() }()
	dir, err := os.MkdirTemp("", "lambda-seam-")
	if err != nil {
		return Reply{}, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	guestDir := filepath.Join(dir, "guest")
	for name, b := range gen.GuestEntries(ln.Addr().String()) {
		if err := os.MkdirAll(filepath.Join(guestDir, name), 0o700); err != nil {
			return Reply{}, err
		}
		if err := os.WriteFile(filepath.Join(guestDir, name, "raw"), b, 0o600); err != nil {
			return Reply{}, err
		}
	}
	id, err := hostlink.LoadIdentity(guestDir)
	if err != nil {
		return Reply{}, err
	}
	workDir := filepath.Join(dir, "work")
	if err := os.Mkdir(workDir, 0o700); err != nil {
		return Reply{}, err
	}
	gctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		conn, err := hostlink.Dial(gctx, id)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		guest(gctx, conn)
	}()
	return d.session(ctx, req, gen, ln, workDir, time.Duration(math.MaxInt64), d.Grants.Accept)
}
