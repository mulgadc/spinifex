package elbv2

import (
	"bufio"
	"fmt"
	"net"
	"strings"
	"time"
)

// ServerStatus represents a backend server's health as reported by HAProxy.
type ServerStatus struct {
	Backend string `json:"backend"`
	Server  string `json:"server"`
	Status  string `json:"status"` // "UP", "DOWN", "MAINT", etc.
}

// HealthReport is returned by the /health endpoint so the ELBv2 service can
// update target state.
type HealthReport struct {
	LBID    string         `json:"lb_id"`
	Servers []ServerStatus `json:"servers"`
}

// FailoverServerSuffix marks the unchecked backup copy of a server that HAProxy
// uses only when every checked server is down. '.' cannot appear in a sanitized
// target name, so a copy never collides with a real server.
const FailoverServerSuffix = ".fo"

// queryHAProxyStats reads backend server health from the HAProxy stats socket.
// Parses CSV "show stat" output; cols 0/1/17 = pxname/svname/status.
func queryHAProxyStats(socketPath string) ([]ServerStatus, error) {
	conn, err := net.DialTimeout("unix", socketPath, 2*time.Second)
	if err != nil {
		return nil, fmt.Errorf("connect to stats socket: %w", err)
	}
	defer conn.Close()

	if err := conn.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		return nil, fmt.Errorf("set deadline: %w", err)
	}

	if _, err := fmt.Fprintf(conn, "show stat\n"); err != nil {
		return nil, fmt.Errorf("send command: %w", err)
	}

	var servers []ServerStatus
	scanner := bufio.NewScanner(conn)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		fields := strings.Split(line, ",")
		if len(fields) < 18 {
			continue
		}

		svname := fields[1]
		if svname == "FRONTEND" || svname == "BACKEND" || strings.HasSuffix(svname, FailoverServerSuffix) {
			continue
		}

		servers = append(servers, ServerStatus{
			Backend: fields[0],
			Server:  svname,
			Status:  fields[17],
		})
	}

	return servers, scanner.Err()
}
