//go:build lambdatestlink

package main

import (
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
)

const (
	sdkEndpointEntry = "test-sdk-endpoint"
	sdkCAEntry       = "test-sdk-ca"
	sdkBundlePath    = "/run/lambda-sdk/ca-bundle.pem"
	systemBundlePath = "/etc/ssl/certs/ca-certificates.crt"
)

type sdkConfig struct {
	endpoint string
	ca       []byte
}

// readSDKConfig reads optional public settings before the boot config is sealed.
func readSDKConfig(dir string) (*sdkConfig, error) {
	endpoint, endpointErr := os.ReadFile(filepath.Join(dir, sdkEndpointEntry, "raw"))
	ca, caErr := os.ReadFile(filepath.Join(dir, sdkCAEntry, "raw"))
	if errors.Is(endpointErr, os.ErrNotExist) && errors.Is(caErr, os.ErrNotExist) {
		return nil, nil
	}
	if endpointErr != nil || caErr != nil {
		return nil, errors.New("incomplete SDK gateway settings")
	}
	u, err := url.Parse(string(endpoint))
	if err != nil || u.Scheme != "https" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" ||
		net.ParseIP(u.Hostname()) == nil || u.Port() == "" {
		return nil, errors.New("invalid SDK gateway endpoint")
	}
	if len(ca) > 64<<10 || !x509.NewCertPool().AppendCertsFromPEM(ca) {
		return nil, errors.New("invalid SDK gateway CA")
	}
	return &sdkConfig{endpoint: string(endpoint), ca: ca}, nil
}

// writeBundle combines the system roots with the cluster's public root.
func (c *sdkConfig) writeBundle(path, systemPath string) error {
	system, err := os.ReadFile(systemPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read system CA bundle: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create SDK CA directory: %w", err)
	}
	if err := os.Chmod(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("make SDK CA directory readable: %w", err)
	}
	bundle := append(append(system, '\n'), c.ca...)
	if err := os.WriteFile(path, bundle, 0o644); err != nil {
		return fmt.Errorf("write SDK CA bundle: %w", err)
	}
	return os.Chmod(path, 0o644)
}
