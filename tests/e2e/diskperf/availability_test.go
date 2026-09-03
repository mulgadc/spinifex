//go:build e2e

package diskperf

import (
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// oneShot builds a probe that closes stop after its first attempt, so watch
// returns after exactly one sample without waiting out probeInterval.
func oneShot(name string, attempt func() error, stop chan struct{}) probe {
	var once sync.Once
	return probe{
		Name:  name,
		Bound: time.Second,
		Attempt: func() error {
			defer once.Do(func() { close(stop) })
			return attempt()
		},
	}
}

func TestWatchRecordsAHealthySample(t *testing.T) {
	stop := make(chan struct{})
	reports := watch([]probe{oneShot("healthy", func() error { return nil }, stop)}, stop)

	if len(reports) != 1 {
		t.Fatalf("watch returned %d reports, want 1", len(reports))
	}
	r := reports[0]
	if !r.ok() {
		t.Errorf("healthy probe not ok: %s (%v)", r.summary(), r.FirstFailure)
	}
	if r.Samples != 1 {
		t.Errorf("samples = %d, want 1", r.Samples)
	}
}

func TestWatchRecordsAFailedSample(t *testing.T) {
	stop := make(chan struct{})
	boom := errors.New("connection refused")
	reports := watch([]probe{oneShot("broken", func() error { return boom }, stop)}, stop)

	r := reports[0]
	if r.ok() {
		t.Fatalf("failing probe reported ok: %s", r.summary())
	}
	if r.Failures != 1 {
		t.Errorf("failures = %d, want 1", r.Failures)
	}
	if !errors.Is(r.FirstFailure, boom) {
		t.Errorf("first failure does not wrap the cause: %v", r.FirstFailure)
	}
}

// A probe that answers, but too late, is a breach. Without this the gate would
// pass on a host that took a minute to accept a connection, which is the
// symptom it exists to catch.
func TestWatchTreatsASlowAnswerAsABreach(t *testing.T) {
	stop := make(chan struct{})
	p := oneShot("slow", func() error {
		time.Sleep(50 * time.Millisecond)
		return nil
	}, stop)
	p.Bound = time.Millisecond

	r := watch([]probe{p}, stop)[0]
	if r.ok() {
		t.Fatalf("slow probe reported ok: %s", r.summary())
	}
	if !strings.Contains(r.FirstFailure.Error(), "over the") {
		t.Errorf("breach is not reported as a timing one: %v", r.FirstFailure)
	}
}

// A probe with no samples is not a pass. The distinction matters because a
// watch that never ran and one that ran cleanly otherwise look identical.
func TestProbeReportWithNoSamplesIsNotOK(t *testing.T) {
	if (probeReport{Name: "never ran"}).ok() {
		t.Error("a report with zero samples reported ok")
	}
}

func TestReadSSHBannerAcceptsAnSSHServer(t *testing.T) {
	addr := serveOnce(t, func(c net.Conn) { _, _ = c.Write([]byte("SSH-2.0-OpenSSH_9.6\r\n")) })
	if err := readSSHBanner(addr, time.Second); err != nil {
		t.Errorf("readSSHBanner: %v", err)
	}
}

// Something listening on 22 that is not sshd does not prove the host can still
// schedule sshd, so the banner is checked rather than the connect alone.
func TestReadSSHBannerRejectsANonSSHPeer(t *testing.T) {
	addr := serveOnce(t, func(c net.Conn) { _, _ = c.Write([]byte("HTTP/1.1 400\r\n")) })
	if err := readSSHBanner(addr, time.Second); err == nil {
		t.Error("readSSHBanner accepted a peer that sent no SSH identification string")
	}
}

// A connection that is accepted and then never spoken on is exactly what a
// starved host looks like, and it must not read as healthy.
func TestReadSSHBannerRejectsASilentPeer(t *testing.T) {
	addr := serveOnce(t, func(net.Conn) { time.Sleep(time.Second) })
	if err := readSSHBanner(addr, 100*time.Millisecond); err == nil {
		t.Error("readSSHBanner accepted a peer that never sent a banner")
	}
}

func TestReadSSHBannerRejectsAClosedPort(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	if err := readSSHBanner(addr, 200*time.Millisecond); err == nil {
		t.Error("readSSHBanner accepted a closed port")
	}
}

// serveOnce starts a listener that hands one connection to handle, and returns
// its address. The listener is closed when the test ends.
func serveOnce(t *testing.T, handle func(net.Conn)) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		conn, aerr := ln.Accept()
		if aerr != nil {
			return
		}
		defer conn.Close()
		handle(conn)
	}()
	return ln.Addr().String()
}
