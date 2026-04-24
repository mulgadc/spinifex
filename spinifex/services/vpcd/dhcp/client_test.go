package dhcp_test

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/services/vpcd/dhcp"
)

func TestLeaseTimers(t *testing.T) {
	acq := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	l := &dhcp.Lease{
		AcquiredAt:    acq,
		LeaseDuration: time.Hour,
		T1:            30 * time.Minute,
		T2:            52*time.Minute + 30*time.Second,
	}

	if got := l.RenewAt(); !got.Equal(acq.Add(30 * time.Minute)) {
		t.Errorf("RenewAt: got %v, want %v", got, acq.Add(30*time.Minute))
	}
	if got := l.RebindAt(); !got.Equal(acq.Add(52*time.Minute + 30*time.Second)) {
		t.Errorf("RebindAt: got %v, want %v", got, acq.Add(52*time.Minute+30*time.Second))
	}
	if got := l.ExpiresAt(); !got.Equal(acq.Add(time.Hour)) {
		t.Errorf("ExpiresAt: got %v, want %v", got, acq.Add(time.Hour))
	}
}

func TestFakeAcquireReturnsLeaseAndTracksCount(t *testing.T) {
	f := dhcp.NewFake()
	mac, _ := net.ParseMAC("02:00:00:aa:bb:cc")

	lease, err := f.Acquire(context.Background(), dhcp.AcquireRequest{
		Bridge: "br-wan", ClientID: "eni-1", HWAddr: mac,
	})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if lease == nil || lease.IP == nil {
		t.Fatalf("expected lease with IP, got %+v", lease)
	}
	if f.AcquireCount() != 1 {
		t.Errorf("acquire count = %d, want 1", f.AcquireCount())
	}
	if held, ok := f.HeldLease("eni-1"); !ok || held.IP.String() != lease.IP.String() {
		t.Errorf("held lease = %v/%v, want match", held, ok)
	}
}

func TestFakeAcquireHookOverridesDefault(t *testing.T) {
	f := dhcp.NewFake()
	f.AcquireHook = func(req dhcp.AcquireRequest) (*dhcp.Lease, error) {
		return nil, errors.New("injected")
	}
	_, err := f.Acquire(context.Background(), dhcp.AcquireRequest{Bridge: "br-wan", ClientID: "x"})
	if err == nil || err.Error() != "injected" {
		t.Fatalf("expected injected error, got %v", err)
	}
}

func TestFakeRenewRefreshesAcquiredAt(t *testing.T) {
	f := dhcp.NewFake()
	lease, err := f.Acquire(context.Background(), dhcp.AcquireRequest{
		Bridge: "br-wan", ClientID: "eni-2",
	})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	firstAcq := lease.AcquiredAt

	time.Sleep(10 * time.Millisecond)
	renewed, err := f.Renew(context.Background(), lease)
	if err != nil {
		t.Fatalf("Renew: %v", err)
	}
	if !renewed.AcquiredAt.After(firstAcq) {
		t.Errorf("renewed AcquiredAt %v not after original %v", renewed.AcquiredAt, firstAcq)
	}
	if f.RenewCount() != 1 {
		t.Errorf("renew count = %d, want 1", f.RenewCount())
	}
}

func TestFakeReleaseClearsTrackedLease(t *testing.T) {
	f := dhcp.NewFake()
	lease, _ := f.Acquire(context.Background(), dhcp.AcquireRequest{
		Bridge: "br-wan", ClientID: "eni-3",
	})
	if err := f.Release(context.Background(), lease); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if _, ok := f.HeldLease("eni-3"); ok {
		t.Errorf("expected lease removed after Release")
	}
	if f.ReleaseCount() != 1 {
		t.Errorf("release count = %d, want 1", f.ReleaseCount())
	}
}

func TestFakeRenewHookSurfacesError(t *testing.T) {
	f := dhcp.NewFake()
	f.RenewHook = func(l *dhcp.Lease) (*dhcp.Lease, error) {
		return nil, errors.New("server NAK")
	}
	lease, _ := f.Acquire(context.Background(), dhcp.AcquireRequest{Bridge: "br-wan", ClientID: "eni-4"})
	if _, err := f.Renew(context.Background(), lease); err == nil {
		t.Fatal("expected hook error")
	}
}

func TestFakeReleaseHookSurfacesError(t *testing.T) {
	f := dhcp.NewFake()
	f.ReleaseHook = func(*dhcp.Lease) error { return errors.New("server unreachable") }
	lease, _ := f.Acquire(context.Background(), dhcp.AcquireRequest{Bridge: "br-wan", ClientID: "eni-rel"})
	if err := f.Release(context.Background(), lease); err == nil {
		t.Fatal("expected hook error")
	}
}

func TestFakeRenewNilLease(t *testing.T) {
	f := dhcp.NewFake()
	if _, err := f.Renew(context.Background(), nil); err == nil {
		t.Fatal("expected error for nil lease")
	}
}

func TestFakeReleaseNilIsNoop(t *testing.T) {
	f := dhcp.NewFake()
	if err := f.Release(context.Background(), nil); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}

func TestNClient4ValidatesInputs(t *testing.T) {
	c := dhcp.NewNClient4(0, 0) // defaults applied
	if _, err := c.Acquire(context.Background(), dhcp.AcquireRequest{}); err == nil {
		t.Fatal("expected bridge-required error")
	}
	mac, _ := net.ParseMAC("02:00:00:aa:bb:cc")
	if _, err := c.Acquire(context.Background(), dhcp.AcquireRequest{Bridge: "br-wan"}); err == nil {
		t.Fatal("expected hw_addr-required error")
	}
	_ = mac
}

func TestNClient4RenewRejectsNilAndMissingRaw(t *testing.T) {
	c := dhcp.NewNClient4(0, 0)
	if _, err := c.Renew(context.Background(), nil); err == nil {
		t.Fatal("expected nil-lease error")
	}
	lease := &dhcp.Lease{Bridge: "br-wan", ClientID: "x"} // no Raw bytes
	if _, err := c.Renew(context.Background(), lease); err == nil {
		t.Fatal("expected missing-raw-bytes error")
	}
}

func TestNClient4ReleaseNilIsNoop(t *testing.T) {
	c := dhcp.NewNClient4(0, 0)
	if err := c.Release(context.Background(), nil); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}

func TestNClient4ReleaseRejectsMissingRaw(t *testing.T) {
	c := dhcp.NewNClient4(0, 0)
	lease := &dhcp.Lease{Bridge: "br-wan", ClientID: "x"}
	if err := c.Release(context.Background(), lease); err == nil {
		t.Fatal("expected missing-raw-bytes error")
	}
}
