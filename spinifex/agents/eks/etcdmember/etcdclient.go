package etcdmember

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"go.etcd.io/etcd/client/pkg/v3/transport"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// DefaultEndpoint and DefaultCertDir match where K3s serves embedded etcd and
// writes its client certificate.
const (
	DefaultEndpoint = "https://127.0.0.1:2379"
	DefaultCertDir  = "/var/lib/rancher/k3s/server/tls/etcd"
)

// EtcdClient is a Client backed by a real etcd connection.
type EtcdClient struct {
	cli *clientv3.Client
}

var _ Client = (*EtcdClient)(nil)

// Dial connects to etcd with the K3s etcd client certificate in certDir.
func Dial(endpoint, certDir string, timeout time.Duration) (*EtcdClient, error) {
	tlsInfo := transport.TLSInfo{
		CertFile:      filepath.Join(certDir, "client.crt"),
		KeyFile:       filepath.Join(certDir, "client.key"),
		TrustedCAFile: filepath.Join(certDir, "server-ca.crt"),
	}
	tlsCfg, err := tlsInfo.ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("etcd client TLS: %w", err)
	}
	cli, err := clientv3.New(clientv3.Config{
		Endpoints:   []string{endpoint},
		DialTimeout: timeout,
		TLS:         tlsCfg,
	})
	if err != nil {
		return nil, fmt.Errorf("dial etcd %s: %w", endpoint, err)
	}
	return &EtcdClient{cli: cli}, nil
}

// Close releases the connection.
func (e *EtcdClient) Close() error { return e.cli.Close() }

// MemberList implements Client.
func (e *EtcdClient) MemberList(ctx context.Context) ([]Member, error) {
	resp, err := e.cli.MemberList(ctx)
	if err != nil {
		return nil, err
	}
	members := make([]Member, 0, len(resp.Members))
	for _, m := range resp.Members {
		members = append(members, Member{ID: m.ID, Name: m.Name, PeerURLs: m.PeerURLs})
	}
	return members, nil
}

// MemberRemove implements Client.
func (e *EtcdClient) MemberRemove(ctx context.Context, id uint64) error {
	_, err := e.cli.MemberRemove(ctx, id)
	return err
}
