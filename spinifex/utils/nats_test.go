package utils

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"log/slog"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func startTestNATSServer(t *testing.T) *server.Server {
	t.Helper()
	ns, _ := testutil.StartTestNATS(t)
	return ns
}

func TestConnectNATS_Success(t *testing.T) {
	ns := startTestNATSServer(t)

	nc, err := ConnectNATS(ns.ClientURL(), "", "")
	require.NoError(t, err)
	defer nc.Close()

	assert.True(t, nc.IsConnected())
}

func TestConnectNATS_WithToken(t *testing.T) {
	opts := &server.Options{
		Host:          "127.0.0.1",
		Port:          -1,
		NoLog:         true,
		NoSigs:        true,
		Authorization: "test-token-123",
	}

	ns, err := server.NewServer(opts)
	require.NoError(t, err)
	go ns.Start()
	require.True(t, ns.ReadyForConnections(5*time.Second))
	t.Cleanup(func() { ns.Shutdown() })

	// With correct token — should succeed
	nc, err := ConnectNATS(ns.ClientURL(), "test-token-123", "")
	require.NoError(t, err)
	defer nc.Close()
	assert.True(t, nc.IsConnected())
}

func TestConnectNATS_WrongToken(t *testing.T) {
	opts := &server.Options{
		Host:          "127.0.0.1",
		Port:          -1,
		NoLog:         true,
		NoSigs:        true,
		Authorization: "correct-token",
	}

	ns, err := server.NewServer(opts)
	require.NoError(t, err)
	go ns.Start()
	require.True(t, ns.ReadyForConnections(5*time.Second))
	t.Cleanup(func() { ns.Shutdown() })

	_, err = ConnectNATS(ns.ClientURL(), "wrong-token", "")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "NATS connect failed")
}

func TestConnectNATS_BadAddress(t *testing.T) {
	_, err := ConnectNATS("nats://127.0.0.1:1", "", "")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "NATS connect failed")
}

func TestConnectNATS_MissingCACert(t *testing.T) {
	_, err := ConnectNATS("nats://127.0.0.1:4222", "", "/nonexistent/ca.pem")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrCACertRead)
	assert.Contains(t, err.Error(), "/nonexistent/ca.pem")
}

func TestConnectNATS_MalformedCACert(t *testing.T) {
	tmp := t.TempDir()
	badCert := filepath.Join(tmp, "bad-ca.pem")
	require.NoError(t, os.WriteFile(badCert, []byte("not a PEM certificate"), 0o644))

	_, err := ConnectNATS("nats://127.0.0.1:4222", "", badCert)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrCACertParse)
}

// generateTestCA creates an ephemeral CA cert+key and writes PEM files to dir.
func generateTestCA(t *testing.T, dir, name string) (certPath, keyPath string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: name},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)

	certPath = filepath.Join(dir, name+".pem")
	keyPath = filepath.Join(dir, name+".key")
	require.NoError(t, os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644))
	keyDER, err := x509.MarshalECPrivateKey(key)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600))
	return certPath, keyPath
}

// generateTestServerCert creates a server cert signed by the given CA.
func generateTestServerCert(t *testing.T, dir, caCertPath, caKeyPath string) (certPath, keyPath string) {
	t.Helper()
	caCertPEM, err := os.ReadFile(caCertPath)
	require.NoError(t, err)
	block, _ := pem.Decode(caCertPEM)
	caCert, err := x509.ParseCertificate(block.Bytes)
	require.NoError(t, err)

	caKeyPEM, err := os.ReadFile(caKeyPath)
	require.NoError(t, err)
	keyBlock, _ := pem.Decode(caKeyPEM)
	caKey, err := x509.ParseECPrivateKey(keyBlock.Bytes)
	require.NoError(t, err)

	serverKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &serverKey.PublicKey, caKey)
	require.NoError(t, err)

	certPath = filepath.Join(dir, "server.pem")
	keyPath = filepath.Join(dir, "server.key")
	require.NoError(t, os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644))
	keyDER, err := x509.MarshalECPrivateKey(serverKey)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600))
	return certPath, keyPath
}

// startTLSNATSServer starts a NATS server with TLS using the given cert files.
func startTLSNATSServer(t *testing.T, serverCertPath, serverKeyPath, caCertPath string) *server.Server {
	t.Helper()
	cert, err := tls.LoadX509KeyPair(serverCertPath, serverKeyPath)
	require.NoError(t, err)
	caPEM, err := os.ReadFile(caCertPath)
	require.NoError(t, err)
	pool := x509.NewCertPool()
	require.True(t, pool.AppendCertsFromPEM(caPEM))

	tlsCfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientCAs:    pool,
	}

	opts := &server.Options{
		Host:      "127.0.0.1",
		Port:      -1,
		NoLog:     true,
		NoSigs:    true,
		TLSConfig: tlsCfg,
	}

	ns, err := server.NewServer(opts)
	require.NoError(t, err)
	go ns.Start()
	require.True(t, ns.ReadyForConnections(5*time.Second))
	t.Cleanup(func() { ns.Shutdown() })
	return ns
}

func TestConnectNATS_TLSSuccess(t *testing.T) {
	tmp := t.TempDir()
	caCertPath, caKeyPath := generateTestCA(t, tmp, "ca")
	serverCertPath, serverKeyPath := generateTestServerCert(t, tmp, caCertPath, caKeyPath)

	ns := startTLSNATSServer(t, serverCertPath, serverKeyPath, caCertPath)

	nc, err := ConnectNATS(ns.ClientURL(), "", caCertPath)
	require.NoError(t, err)
	defer nc.Close()
	assert.True(t, nc.IsConnected())
}

func TestConnectNATS_WrongCA(t *testing.T) {
	tmp := t.TempDir()
	caCertPath, caKeyPath := generateTestCA(t, tmp, "ca")
	serverCertPath, serverKeyPath := generateTestServerCert(t, tmp, caCertPath, caKeyPath)
	wrongCACertPath, _ := generateTestCA(t, tmp, "wrong-ca")

	ns := startTLSNATSServer(t, serverCertPath, serverKeyPath, caCertPath)

	_, err := ConnectNATS(ns.ClientURL(), "", wrongCACertPath)
	assert.Error(t, err, "connection with wrong CA should fail")
}

func TestNATSRequest_Success(t *testing.T) {
	ns := startTestNATSServer(t)

	nc, err := nats.Connect(ns.ClientURL())
	require.NoError(t, err)
	defer nc.Close()

	type Req struct {
		Name string `json:"name"`
	}
	type Resp struct {
		Greeting string `json:"greeting"`
	}

	// Mock responder
	_, err = nc.Subscribe("test.greet", func(msg *nats.Msg) {
		var req Req
		json.Unmarshal(msg.Data, &req)
		resp := Resp{Greeting: "hello " + req.Name}
		data, _ := json.Marshal(resp)
		msg.Respond(data)
	})
	require.NoError(t, err)

	result, err := NATSRequest[Resp](nc, "test.greet", Req{Name: "world"}, 2*time.Second, "")
	require.NoError(t, err)
	assert.Equal(t, "hello world", result.Greeting)
}

func TestNATSRequest_ErrorResponse(t *testing.T) {
	ns := startTestNATSServer(t)

	nc, err := nats.Connect(ns.ClientURL())
	require.NoError(t, err)
	defer nc.Close()

	// Responder returns an error payload
	_, err = nc.Subscribe("test.fail", func(msg *nats.Msg) {
		errPayload := GenerateErrorPayload("InvalidParameterValue")
		msg.Respond(errPayload)
	})
	require.NoError(t, err)

	type Resp struct{}
	_, err = NATSRequest[Resp](nc, "test.fail", struct{}{}, 2*time.Second, "")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "InvalidParameterValue")
}

func TestNATSRequest_NoResponders(t *testing.T) {
	ns := startTestNATSServer(t)

	nc, err := nats.Connect(ns.ClientURL())
	require.NoError(t, err)
	defer nc.Close()

	type Resp struct{}
	_, err = NATSRequest[Resp](nc, "test.nobody", struct{}{}, 500*time.Millisecond, "")
	assert.Error(t, err)
}

func TestNATSRequest_Timeout(t *testing.T) {
	ns := startTestNATSServer(t)

	nc, err := nats.Connect(ns.ClientURL())
	require.NoError(t, err)
	defer nc.Close()

	// Responder that never responds
	_, err = nc.QueueSubscribe("test.slow", "q", func(msg *nats.Msg) {
		time.Sleep(5 * time.Second)
	})
	require.NoError(t, err)

	type Resp struct{}
	_, err = NATSRequest[Resp](nc, "test.slow", struct{}{}, 100*time.Millisecond, "")
	assert.Error(t, err)
}

func TestNATSRequest_InvalidUnmarshal(t *testing.T) {
	ns := startTestNATSServer(t)

	nc, err := nats.Connect(ns.ClientURL())
	require.NoError(t, err)
	defer nc.Close()

	// Responder returns invalid JSON for the expected type
	_, err = nc.Subscribe("test.badjson", func(msg *nats.Msg) {
		msg.Respond([]byte(`not-json`))
	})
	require.NoError(t, err)

	type Resp struct {
		Value int `json:"value"`
	}
	_, err = NATSRequest[Resp](nc, "test.badjson", struct{}{}, 2*time.Second, "")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "unmarshal")
}

// --- NATSRequest with account ID tests ---

func TestNATSRequest_AccountIDHeader(t *testing.T) {
	ns := startTestNATSServer(t)

	nc, err := nats.Connect(ns.ClientURL())
	require.NoError(t, err)
	defer nc.Close()

	type Req struct {
		Name string `json:"name"`
	}
	type Resp struct {
		Greeting  string `json:"greeting"`
		AccountID string `json:"account_id"`
	}

	// Responder echoes back the account ID from the header
	_, err = nc.Subscribe("test.account", func(msg *nats.Msg) {
		var req Req
		json.Unmarshal(msg.Data, &req)
		acct := AccountIDFromMsg(msg)
		resp := Resp{Greeting: "hello " + req.Name, AccountID: acct}
		data, _ := json.Marshal(resp)
		msg.Respond(data)
	})
	require.NoError(t, err)

	result, err := NATSRequest[Resp](nc, "test.account", Req{Name: "world"}, 2*time.Second, "111122223333")
	require.NoError(t, err)
	assert.Equal(t, "hello world", result.Greeting)
	assert.Equal(t, "111122223333", result.AccountID)
}

func TestNATSRequest_MarshalError(t *testing.T) {
	ns := startTestNATSServer(t)

	nc, err := nats.Connect(ns.ClientURL())
	require.NoError(t, err)
	defer nc.Close()

	type Resp struct{}
	// Channels cannot be marshaled to JSON
	_, err = NATSRequest[Resp](nc, "test.marshalfail", make(chan int), 2*time.Second, "")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to marshal input")
}

// --- AccountIDFromMsg tests ---

// --- NATSScatterGather tests ---

func TestNATSScatterGather_SuccessAmongErrors(t *testing.T) {
	ns := startTestNATSServer(t)

	nc, err := nats.Connect(ns.ClientURL())
	require.NoError(t, err)
	defer nc.Close()

	type Resp struct {
		Value string `json:"value"`
	}

	// Simulate 3 nodes: 2 return errors, 1 returns success
	_, err = nc.Subscribe("test.scatter", func(msg *nats.Msg) {
		// Simulate varying response times
		resp := Resp{Value: "found"}
		data, _ := json.Marshal(resp)
		msg.Respond(data)
	})
	require.NoError(t, err)

	// Two error responders (faster)
	for range 2 {
		_, err = nc.Subscribe("test.scatter", func(msg *nats.Msg) {
			errPayload := GenerateErrorPayload("InvalidInstanceID.NotFound")
			msg.Respond(errPayload)
		})
		require.NoError(t, err)
	}

	result, err := NATSScatterGather[Resp](nc, "test.scatter", struct{}{}, 3*time.Second, 3, "")
	require.NoError(t, err)
	assert.Equal(t, "found", result.Value)
}

func TestNATSScatterGather_AllErrors(t *testing.T) {
	ns := startTestNATSServer(t)

	nc, err := nats.Connect(ns.ClientURL())
	require.NoError(t, err)
	defer nc.Close()

	type Resp struct{}

	// All 3 nodes return errors
	for range 3 {
		_, err = nc.Subscribe("test.scatter.allerr", func(msg *nats.Msg) {
			errPayload := GenerateErrorPayload("InvalidInstanceID.NotFound")
			msg.Respond(errPayload)
		})
		require.NoError(t, err)
	}

	_, err = NATSScatterGather[Resp](nc, "test.scatter.allerr", struct{}{}, 2*time.Second, 3, "")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "InvalidInstanceID.NotFound")
}

func TestNATSScatterGather_Timeout(t *testing.T) {
	ns := startTestNATSServer(t)

	nc, err := nats.Connect(ns.ClientURL())
	require.NoError(t, err)
	defer nc.Close()

	type Resp struct{}

	// No subscribers — should return a timeout error
	_, err = NATSScatterGather[Resp](nc, "test.scatter.timeout", struct{}{}, 200*time.Millisecond, 0, "")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "no responses received")
}

func TestNATSScatterGather_SingleSuccess(t *testing.T) {
	ns := startTestNATSServer(t)

	nc, err := nats.Connect(ns.ClientURL())
	require.NoError(t, err)
	defer nc.Close()

	type Resp struct {
		ID string `json:"id"`
	}

	_, err = nc.Subscribe("test.scatter.single", func(msg *nats.Msg) {
		resp := Resp{ID: "ami-123"}
		data, _ := json.Marshal(resp)
		msg.Respond(data)
	})
	require.NoError(t, err)

	result, err := NATSScatterGather[Resp](nc, "test.scatter.single", struct{}{}, 2*time.Second, 1, "acct-123")
	require.NoError(t, err)
	assert.Equal(t, "ami-123", result.ID)
}

func TestNATSScatterGather_MarshalError(t *testing.T) {
	ns := startTestNATSServer(t)

	nc, err := nats.Connect(ns.ClientURL())
	require.NoError(t, err)
	defer nc.Close()

	type Resp struct{}
	_, err = NATSScatterGather[Resp](nc, "test.scatter.marshal", make(chan int), 2*time.Second, 0, "")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to marshal input")
}

func TestNATSScatterGather_EarlyExitOnSuccess(t *testing.T) {
	ns := startTestNATSServer(t)

	nc, err := nats.Connect(ns.ClientURL())
	require.NoError(t, err)
	defer nc.Close()

	type Resp struct {
		Value string `json:"value"`
	}

	// One fast success responder — should return immediately without waiting
	_, err = nc.Subscribe("test.scatter.early", func(msg *nats.Msg) {
		resp := Resp{Value: "quick"}
		data, _ := json.Marshal(resp)
		msg.Respond(data)
	})
	require.NoError(t, err)

	start := time.Now()
	result, err := NATSScatterGather[Resp](nc, "test.scatter.early", struct{}{}, 5*time.Second, 0, "")
	elapsed := time.Since(start)

	require.NoError(t, err)
	assert.Equal(t, "quick", result.Value)
	// Should return well before the 5s timeout
	assert.Less(t, elapsed, 2*time.Second)
}

func TestAccountIDFromMsg(t *testing.T) {
	msg := nats.NewMsg("test")
	msg.Header.Set(AccountIDHeader, "444455556666")

	assert.Equal(t, "444455556666", AccountIDFromMsg(msg))
}

func TestAccountIDFromMsg_Missing(t *testing.T) {
	msg := nats.NewMsg("test")
	assert.Equal(t, "", AccountIDFromMsg(msg))
}

func TestAccountIDFromMsg_NilMsg(t *testing.T) {
	assert.Equal(t, "", AccountIDFromMsg(nil))
}

func TestAccountIDFromMsg_NilHeader(t *testing.T) {
	msg := &nats.Msg{Subject: "test"}
	assert.Equal(t, "", AccountIDFromMsg(msg))
}

// --- ConnectNATSWithRetry tests ---

func TestConnectNATSWithRetry_Success(t *testing.T) {
	ns := startTestNATSServer(t)

	nc, err := ConnectNATSWithRetry(ns.ClientURL(), "", "")
	require.NoError(t, err)
	defer nc.Close()
	assert.True(t, nc.IsConnected())
}

func TestConnectNATSWithRetry_RetriesOnFailure(t *testing.T) {
	start := time.Now()
	_, err := ConnectNATSWithRetry("nats://127.0.0.1:14222", "", "",
		WithMaxWait(500*time.Millisecond),
		WithRetryDelay(50*time.Millisecond),
	)
	elapsed := time.Since(start)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "NATS connect failed")
	assert.GreaterOrEqual(t, elapsed, 100*time.Millisecond, "should have retried at least once")
	assert.Less(t, elapsed, 5*time.Second, "should fail within a few seconds")
}

func TestConnectNATSWithRetry_TLSErrorNoRetry(t *testing.T) {
	start := time.Now()
	_, err := ConnectNATSWithRetry("nats://127.0.0.1:4222", "", "/nonexistent/ca.pem",
		WithMaxWait(5*time.Second),
	)
	elapsed := time.Since(start)

	require.Error(t, err)
	assert.ErrorIs(t, err, ErrCACertRead)
	assert.Contains(t, err.Error(), "NATS TLS configuration error")
	assert.Less(t, elapsed, time.Second, "should fail immediately without retrying")
}

// --- Disconnect/Reconnect callback tests ---

func TestConnectNATS_DisconnectCallbackFires(t *testing.T) {
	ns := startTestNATSServer(t)

	disconnected := make(chan struct{}, 1)
	nc, err := ConnectNATS(ns.ClientURL(), "", "",
		WithDisconnectHandler(func(_ *nats.Conn, _ error) {
			select {
			case disconnected <- struct{}{}:
			default:
			}
		}),
	)
	require.NoError(t, err)
	defer nc.Close()
	require.True(t, nc.IsConnected())

	ns.Shutdown()

	select {
	case <-disconnected:
	case <-time.After(3 * time.Second):
		t.Fatal("disconnect callback did not fire")
	}
}

func TestConnectNATS_ReconnectCallbackFires(t *testing.T) {
	// Pin the test NATS to a specific port so we can restart it on the same URL.
	port := freePort(t)
	ns := startTestNATSOnPort(t, port)

	reconnected := make(chan struct{}, 1)
	nc, err := ConnectNATS(ns.ClientURL(), "", "",
		WithReconnectHandler(func(_ *nats.Conn) {
			select {
			case reconnected <- struct{}{}:
			default:
			}
		}),
	)
	require.NoError(t, err)
	defer nc.Close()
	require.True(t, nc.IsConnected())

	ns.Shutdown()
	// Wait until the client noticed the drop so the reconnect path runs.
	require.Eventually(t, func() bool { return !nc.IsConnected() }, 3*time.Second, 50*time.Millisecond)

	startTestNATSOnPort(t, port)

	select {
	case <-reconnected:
	case <-time.After(5 * time.Second):
		t.Fatal("reconnect callback did not fire")
	}
}

// --- Fast-fail when disconnected ---

func TestNATSRequest_DisconnectedFastFail(t *testing.T) {
	ns := startTestNATSServer(t)
	nc, err := ConnectNATS(ns.ClientURL(), "", "")
	require.NoError(t, err)
	defer nc.Close()

	ns.Shutdown()
	require.Eventually(t, func() bool { return !nc.IsConnected() }, 3*time.Second, 50*time.Millisecond)

	start := time.Now()
	_, err = NATSRequest[map[string]any](nc, "ec2.Describe", struct{}{}, 5*time.Second, "")
	elapsed := time.Since(start)

	require.ErrorIs(t, err, ErrClusterUnavailable)
	assert.Less(t, elapsed, 500*time.Millisecond, "should bail before per-call timeout")
}

func TestNATSScatterGather_DisconnectedFastFail(t *testing.T) {
	ns := startTestNATSServer(t)
	nc, err := ConnectNATS(ns.ClientURL(), "", "")
	require.NoError(t, err)
	defer nc.Close()

	ns.Shutdown()
	require.Eventually(t, func() bool { return !nc.IsConnected() }, 3*time.Second, 50*time.Millisecond)

	start := time.Now()
	_, err = NATSScatterGather[map[string]any](nc, "ec2.Describe", struct{}{}, 5*time.Second, 0, "")
	elapsed := time.Since(start)

	require.ErrorIs(t, err, ErrClusterUnavailable)
	assert.Less(t, elapsed, 500*time.Millisecond, "should bail before fan-out timeout")
}

// --- 1c fail-fast tests: NATS request helpers reject when conn is down ---

func TestNATSRequest_NilConn_ReturnsClusterUnavailable(t *testing.T) {
	type Resp struct{}
	_, err := NATSRequest[Resp](nil, "test.never", struct{}{}, 50*time.Millisecond, "")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrClusterUnavailable)
}

func TestNATSRequest_ClosedConn_ReturnsClusterUnavailable(t *testing.T) {
	ns := startTestNATSServer(t)
	nc, err := nats.Connect(ns.ClientURL())
	require.NoError(t, err)
	nc.Close()

	type Resp struct{}
	_, err = NATSRequest[Resp](nc, "test.never", struct{}{}, 50*time.Millisecond, "")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrClusterUnavailable)
}

func TestNATSScatterGather_NilConn_ReturnsClusterUnavailable(t *testing.T) {
	type Resp struct{}
	_, err := NATSScatterGather[Resp](nil, "test.never", struct{}{}, 50*time.Millisecond, 1, "")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrClusterUnavailable)
}

func TestNATSScatterGather_ClosedConn_ReturnsClusterUnavailable(t *testing.T) {
	ns := startTestNATSServer(t)
	nc, err := nats.Connect(ns.ClientURL())
	require.NoError(t, err)
	nc.Close()

	type Resp struct{}
	_, err = NATSScatterGather[Resp](nc, "test.never", struct{}{}, 50*time.Millisecond, 1, "")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrClusterUnavailable)
}

// --- 1c callback hook plumbing: WithDisconnectHandler / WithReconnectHandler ---

func TestConnectNATS_DisconnectReconnectCallbacks(t *testing.T) {
	port := freePort(t)
	ns := startTestNATSOnPort(t, port)

	disconnects := make(chan struct{}, 4)
	reconnects := make(chan struct{}, 4)

	nc, err := ConnectNATS("nats://127.0.0.1:"+strconv.Itoa(port), "", "",
		WithDisconnectHandler(func(_ *nats.Conn, _ error) { disconnects <- struct{}{} }),
		WithReconnectHandler(func(_ *nats.Conn) { reconnects <- struct{}{} }),
	)
	require.NoError(t, err)
	defer nc.Close()

	ns.Shutdown()
	select {
	case <-disconnects:
	case <-time.After(3 * time.Second):
		t.Fatal("disconnect callback never fired")
	}

	startTestNATSOnPort(t, port)
	select {
	case <-reconnects:
	case <-time.After(5 * time.Second):
		t.Fatal("reconnect callback never fired")
	}
}

// --- helpers for restartable NATS server ---

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr, ok := l.Addr().(*net.TCPAddr)
	require.True(t, ok)
	port := addr.Port
	require.NoError(t, l.Close())
	return port
}

func startTestNATSOnPort(t *testing.T, port int) *server.Server {
	t.Helper()
	opts := &server.Options{
		Host:   "127.0.0.1",
		Port:   port,
		NoLog:  true,
		NoSigs: true,
	}
	ns, err := server.NewServer(opts)
	require.NoError(t, err)
	go ns.Start()
	require.True(t, ns.ReadyForConnections(5*time.Second))
	t.Cleanup(func() { ns.Shutdown() })
	return ns
}

// TestConnectNATSWithRetry_LogEscalatesPastThreshold drives ConnectNATSWithRetry
// against a closed port with a tight retry loop until the attempt count exceeds
// natsRetryEscalateAttempt, then asserts that at least one escalated slog.Error
// line was emitted with disconnected_for context (rate-limited to once per
// minute) while earlier attempts stayed at slog.Warn.
func TestConnectNATSWithRetry_LogEscalatesPastThreshold(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))

	_, err := ConnectNATSWithRetry("nats://127.0.0.1:1", "", "",
		WithRetryDelay(1*time.Millisecond),
		WithMaxRetryDelay(1*time.Millisecond),
		WithMaxWait(300*time.Millisecond),
	)
	require.Error(t, err)

	logs := buf.String()
	warnCount := strings.Count(logs, "level=WARN msg=\"NATS not ready, retrying...\"")
	errCount := strings.Count(logs, "level=ERROR msg=\"NATS still disconnected\"")

	assert.GreaterOrEqual(t, warnCount, 1, "expect warn logs for first 30 attempts")
	assert.GreaterOrEqual(t, errCount, 1, "expect at least one escalated error log past the threshold")
	assert.LessOrEqual(t, errCount, 2, "rate-limited to once per minute, so a sub-second test should see at most one or two")
	assert.Contains(t, logs, "disconnected_for=", "escalated error should include disconnected_for")
}

// TestConnectNATSWithRetry_NoEscalation_BelowThreshold keeps the attempt count
// under natsRetryEscalateAttempt and checks that no escalated slog.Error line
// is produced.
func TestConnectNATSWithRetry_NoEscalation_BelowThreshold(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))

	// Exponential backoff capped at 5ms × maxWait 50ms = ≲15 attempts < 30.
	_, err := ConnectNATSWithRetry("nats://127.0.0.1:1", "", "",
		WithRetryDelay(5*time.Millisecond),
		WithMaxRetryDelay(5*time.Millisecond),
		WithMaxWait(50*time.Millisecond),
	)
	require.Error(t, err)

	logs := buf.String()
	assert.NotContains(t, logs, "NATS still disconnected", "should not escalate before threshold")
	assert.Contains(t, logs, "NATS not ready, retrying...", "should still log warn lines")
}
