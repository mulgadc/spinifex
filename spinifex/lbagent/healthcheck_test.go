package lbagent

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestProbeHealthTargets_Empty(t *testing.T) {
	if got := probeHealthTargets(nil); got != nil {
		t.Errorf("empty targets = %v, want nil (daemon early-returns on empty report)", got)
	}
}

func TestProbeOne_TCP(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()

	up := HealthTarget{ServerName: "srv_i-up", Address: ln.Addr().String(), Protocol: "TCP"}
	if !probeOne(up) {
		t.Error("open TCP port probed DOWN, want UP")
	}

	// Closed port: pick the listener's port after closing it.
	addr := ln.Addr().String()
	ln.Close()
	down := HealthTarget{ServerName: "srv_i-down", Address: addr, Protocol: "TCP"}
	if probeOne(down) {
		t.Error("closed TCP port probed UP, want DOWN")
	}
}

func TestProbeOne_HTTP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	addr := srv.Listener.Addr().String()
	up := HealthTarget{ServerName: "srv_i-h", Address: addr, Protocol: "HTTP", Path: "/healthz"}
	if !probeOne(up) {
		t.Error("200 health path probed DOWN, want UP")
	}

	down := HealthTarget{ServerName: "srv_i-h", Address: addr, Protocol: "HTTP", Path: "/bad"}
	if probeOne(down) {
		t.Error("500 health path probed UP, want DOWN")
	}
}

func TestProbeHealthTargets_ReportsServerName(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()

	out := probeHealthTargets([]HealthTarget{
		{ServerName: "srv_i-abc", Address: ln.Addr().String(), Protocol: "TCP"},
	})
	if len(out) != 1 {
		t.Fatalf("got %d statuses, want 1", len(out))
	}
	if out[0].Server != "srv_i-abc" {
		t.Errorf("Server = %q, want %q (daemon health checker keys on it)", out[0].Server, "srv_i-abc")
	}
	if out[0].Status != "UP" {
		t.Errorf("Status = %q, want UP", out[0].Status)
	}
}
