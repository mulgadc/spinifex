// Package fakeetcd serves the etcd Cluster and Maintenance gRPC APIs over mutual
// TLS with the file layout K3s uses, so a real etcd client can be driven in tests.
package fakeetcd

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

// Member is one entry of the fake cluster's member list.
type Member struct {
	ID       uint64
	Name     string
	PeerURLs []string
}

// Server is a running fake etcd.
type Server struct {
	// Endpoint is the https URL clients dial.
	Endpoint string
	// CertDir holds client.crt, client.key and server-ca.crt.
	CertDir string

	mu      sync.Mutex
	members []Member
	selfID  uint64
	removed []uint64
	failAll error
}

// Start serves members with selfID as the answering member. It stops with the test.
func Start(t *testing.T, selfID uint64, members []Member) *Server {
	t.Helper()
	dir := t.TempDir()
	caCert, caKey := newCA(t)
	writePEM(t, filepath.Join(dir, "server-ca.crt"), "CERTIFICATE", caCert.Raw)
	serverPair := issue(t, caCert, caKey, x509.ExtKeyUsageServerAuth)
	clientPair := issue(t, caCert, caKey, x509.ExtKeyUsageClientAuth)
	writeKeyPair(t, dir, "client", clientPair)

	pool := x509.NewCertPool()
	pool.AddCert(caCert)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	creds := credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{serverPair.tls},
		ClientCAs:    pool,
		ClientAuth:   tls.RequireAndVerifyClientCert,
		MinVersion:   tls.VersionTLS12,
	})
	s := &Server{Endpoint: "https://" + lis.Addr().String(), CertDir: dir, members: members, selfID: selfID}
	gs := grpc.NewServer(grpc.Creds(creds))
	pb.RegisterClusterServer(gs, &cluster{s})
	pb.RegisterMaintenanceServer(gs, &maintenance{s: s})
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)
	return s
}

// Removed returns the member IDs removed so far.
func (s *Server) Removed() []uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]uint64(nil), s.removed...)
}

// FailWith makes every later call return err.
func (s *Server) FailWith(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failAll = err
}

type cluster struct{ s *Server }

func (c *cluster) MemberList(context.Context, *pb.MemberListRequest) (*pb.MemberListResponse, error) {
	c.s.mu.Lock()
	defer c.s.mu.Unlock()
	if c.s.failAll != nil {
		return nil, c.s.failAll
	}
	resp := &pb.MemberListResponse{Header: &pb.ResponseHeader{MemberId: c.s.selfID}}
	for _, m := range c.s.members {
		resp.Members = append(resp.Members, &pb.Member{ID: m.ID, Name: m.Name, PeerURLs: m.PeerURLs})
	}
	return resp, nil
}

func (c *cluster) MemberRemove(_ context.Context, req *pb.MemberRemoveRequest) (*pb.MemberRemoveResponse, error) {
	c.s.mu.Lock()
	defer c.s.mu.Unlock()
	if c.s.failAll != nil {
		return nil, c.s.failAll
	}
	c.s.removed = append(c.s.removed, req.ID)
	return &pb.MemberRemoveResponse{Header: &pb.ResponseHeader{MemberId: c.s.selfID}}, nil
}

func (c *cluster) MemberAdd(context.Context, *pb.MemberAddRequest) (*pb.MemberAddResponse, error) {
	return nil, errors.New("not implemented")
}

func (c *cluster) MemberUpdate(context.Context, *pb.MemberUpdateRequest) (*pb.MemberUpdateResponse, error) {
	return nil, errors.New("not implemented")
}

func (c *cluster) MemberPromote(context.Context, *pb.MemberPromoteRequest) (*pb.MemberPromoteResponse, error) {
	return nil, errors.New("not implemented")
}

type maintenance struct {
	pb.UnimplementedMaintenanceServer

	s *Server
}

func (m *maintenance) Status(context.Context, *pb.StatusRequest) (*pb.StatusResponse, error) {
	m.s.mu.Lock()
	defer m.s.mu.Unlock()
	if m.s.failAll != nil {
		return nil, m.s.failAll
	}
	return &pb.StatusResponse{Header: &pb.ResponseHeader{MemberId: m.s.selfID}}, nil
}

type pair struct {
	tls tls.Certificate
	der []byte
	key *ecdsa.PrivateKey
}

func newCA(t *testing.T) (*x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("ca key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "fake-etcd-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("ca cert: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse ca: %v", err)
	}
	return cert, key
}

func issue(t *testing.T, ca *x509.Certificate, caKey *ecdsa.PrivateKey, usage x509.ExtKeyUsage) pair {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: "fake-etcd"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{usage},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:     []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatalf("cert: %v", err)
	}
	return pair{tls: tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, der: der, key: key}
}

func writeKeyPair(t *testing.T, dir, name string, p pair) {
	t.Helper()
	keyDER, err := x509.MarshalECPrivateKey(p.key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	writePEM(t, filepath.Join(dir, name+".crt"), "CERTIFICATE", p.der)
	writePEM(t, filepath.Join(dir, name+".key"), "EC PRIVATE KEY", keyDER)
}

func writePEM(t *testing.T, path, blockType string, der []byte) {
	t.Helper()
	data := pem.EncodeToMemory(&pem.Block{Type: blockType, Bytes: der})
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
