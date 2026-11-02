package elbv2

import (
	"fmt"
	"net"
	"path/filepath"
	"testing"
)

func TestQueryHAProxyStats(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "haproxy.sock")

	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()

		buf := make([]byte, 256)
		conn.Read(buf)

		csv := "# pxname,svname,qcur,qmax,scur,smax,slim,stot,bin,bout,dreq,dresp,ereq,econ,eresp,wretr,wredis,status,weight\n"
		csv += "bk_tg1,FRONTEND,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,OPEN,\n"
		csv += "bk_tg1,srv_i-aaa111,0,0,0,0,0,5,0,0,0,0,0,0,0,0,0,UP,1\n"
		csv += "bk_tg1,srv_i-bbb222,0,0,0,0,0,3,0,0,0,0,0,0,0,0,0,DOWN,1\n"
		csv += "bk_tg1,BACKEND,0,0,0,0,0,8,0,0,0,0,0,0,0,0,0,UP,2\n"
		fmt.Fprint(conn, csv)
	}()

	servers, err := queryHAProxyStats(sock)
	if err != nil {
		t.Fatalf("queryHAProxyStats: %v", err)
	}

	if len(servers) != 2 {
		t.Fatalf("got %d servers, want 2", len(servers))
	}

	if servers[0].Server != "srv_i-aaa111" || servers[0].Status != "UP" {
		t.Errorf("server[0] = %+v, want srv_i-aaa111/UP", servers[0])
	}
	if servers[1].Server != "srv_i-bbb222" || servers[1].Status != "DOWN" {
		t.Errorf("server[1] = %+v, want srv_i-bbb222/DOWN", servers[1])
	}
}

func TestQueryHAProxyStats_SocketNotFound(t *testing.T) {
	_, err := queryHAProxyStats("/nonexistent/haproxy.sock")
	if err == nil {
		t.Error("expected error for non-existent socket")
	}
}

func TestQueryHAProxyStats_IgnoresFailoverCopies(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "haproxy.sock")

	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()

		buf := make([]byte, 256)
		conn.Read(buf)

		csv := "# pxname,svname,qcur,qmax,scur,smax,slim,stot,bin,bout,dreq,dresp,ereq,econ,eresp,wretr,wredis,status,weight\n"
		csv += "bk_tg1,srv_i-aaa111,0,0,0,0,0,5,0,0,0,0,0,0,0,0,0,DOWN,1\n"
		csv += "bk_tg1,srv_i-aaa111" + FailoverServerSuffix + ",0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,UP,1\n"
		fmt.Fprint(conn, csv)
	}()

	servers, err := queryHAProxyStats(sock)
	if err != nil {
		t.Fatalf("queryHAProxyStats: %v", err)
	}
	if len(servers) != 1 || servers[0].Server != "srv_i-aaa111" || servers[0].Status != "DOWN" {
		t.Fatalf("got %+v, want only srv_i-aaa111/DOWN", servers)
	}
}
