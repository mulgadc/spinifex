package utils

import (
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// GenerateSocketFile generates a socket file path for the given name.
// Deprecated: Use GenerateUniqueSocketFile for new code to ensure uniqueness.
func GenerateSocketFile(name string) (string, error) {
	if name == "" {
		return "", errors.New("name is required")
	}

	pidPath := pidPath()

	if pidPath == "" {
		return "", errors.New("pid path is empty")
	}

	return filepath.Join(pidPath, fmt.Sprintf("%s.sock", name)), nil
}

// GenerateUniqueSocketFile generates a unique socket path with format nbd-{volname}-{unixnano}.sock.
func GenerateUniqueSocketFile(volname string) (string, error) {
	if volname == "" {
		return "", errors.New("volume name is required")
	}

	dir := NBDSocketDir()
	if dir == "" {
		return "", errors.New("nbd socket directory is empty")
	}

	timestamp := time.Now().UnixNano()
	filename := fmt.Sprintf("nbd-%s-%d.sock", volname, timestamp)
	return filepath.Join(dir, filename), nil
}

// NBDSocketDir returns the NBD socket directory (/run/spinifex/nbd under systemd; pidPath() as fallback).
func NBDSocketDir() string {
	const systemdNBDDir = "/run/spinifex/nbd"
	if dirExists(systemdNBDDir) {
		return systemdNBDDir
	}
	return pidPath()
}

// IsSocketURI reports whether the NBD URI refers to a Unix socket (ends with ".sock" or contains "unix:").
func IsSocketURI(nbdURI string) bool {
	return strings.HasSuffix(nbdURI, ".sock") || strings.Contains(nbdURI, "unix:")
}

// FormatNBDSocketURI formats a socket path as an NBD URI (nbd:unix:/path/to/socket.sock).
func FormatNBDSocketURI(socketPath string) string {
	return fmt.Sprintf("nbd:unix:%s", socketPath)
}

// FormatNBDTCPURI formats a host:port as an NBD TCP URI (nbd://host:port).
func FormatNBDTCPURI(host string, port int) string {
	return "nbd://" + net.JoinHostPort(host, strconv.Itoa(port))
}

// WaitForNBDReady polls until the NBD endpoint is reachable or timeout expires.
// Unix: checks existence on disk (avoids consuming the accept queue); TCP: dial-and-close.
func WaitForNBDReady(uri string, timeout time.Duration) error {
	serverType, path, host, port, err := ParseNBDURI(uri)
	if err != nil {
		return err
	}
	switch serverType {
	case "unix":
		return WaitForUnixSocket(path, timeout)
	case "inet":
		return waitForTCPListener(net.JoinHostPort(host, strconv.Itoa(port)), timeout)
	default:
		return fmt.Errorf("unsupported NBD server type %q for uri %s", serverType, uri)
	}
}

func waitForTCPListener(addr string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timeout waiting for tcp listener %s: %w", addr, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// ParseNBDURI parses an NBD URI into components: nbd:unix:/path → ("unix", path); nbd://host:port → ("inet", host, port).
func ParseNBDURI(nbdURI string) (serverType, path, host string, port int, err error) {
	if after, ok := strings.CutPrefix(nbdURI, "nbd:unix:"); ok {
		path = after
		if path == "" {
			return "", "", "", 0, fmt.Errorf("empty socket path in NBD URI: %s", nbdURI)
		}
		return "unix", path, "", 0, nil
	}

	if after, ok := strings.CutPrefix(nbdURI, "nbd://"); ok {
		hostPort := after
		colonIdx := strings.LastIndex(hostPort, ":")
		if colonIdx < 0 {
			return "", "", "", 0, fmt.Errorf("missing port in NBD TCP URI: %s", nbdURI)
		}
		host = hostPort[:colonIdx]
		port, err = strconv.Atoi(hostPort[colonIdx+1:])
		if err != nil {
			return "", "", "", 0, fmt.Errorf("invalid port in NBD URI: %s", nbdURI)
		}
		return "inet", "", host, port, nil
	}

	return "", "", "", 0, fmt.Errorf("unsupported NBD URI format: %s", nbdURI)
}
