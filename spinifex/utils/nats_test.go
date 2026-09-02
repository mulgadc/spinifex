package utils

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
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

	result, err := NATSRequest[Resp](context.Background(), nc, "test.greet", Req{Name: "world"}, 2*time.Second, "")
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
	_, err = NATSRequest[Resp](context.Background(), nc, "test.fail", struct{}{}, 2*time.Second, "")
	assert.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidParameterValue, err.Error())
	code, ok := awserrors.ResolveErrorCode(err)
	assert.True(t, ok)
	assert.Equal(t, awserrors.ErrorInvalidParameterValue, code)
}

func TestNATSRequest_ErrorResponseSurfacesMessage(t *testing.T) {
	ns := startTestNATSServer(t)

	nc, err := nats.Connect(ns.ClientURL())
	require.NoError(t, err)
	defer nc.Close()

	// Code is sanitized to ServerInternal, but the actionable message must
	// still reach the caller instead of the bare code.
	const reason = "eks: snapshot \"snap-bogus\" not found; refusing to restore"
	_, err = nc.Subscribe("test.fail.msg", func(msg *nats.Msg) {
		msg.Respond(GenerateErrorPayloadWithMessage("ServerInternal", reason))
	})
	require.NoError(t, err)

	type Resp struct{}
	_, err = NATSRequest[Resp](context.Background(), nc, "test.fail.msg", struct{}{}, 2*time.Second, "")
	assert.Error(t, err)
	assert.Equal(t, reason, err.Error())
	code, ok := awserrors.ResolveErrorCode(err)
	assert.True(t, ok)
	assert.Equal(t, awserrors.ErrorServerInternal, code)
}

func TestServeNATSRequest_WrappedErrorPreservesCodeAndMessage(t *testing.T) {
	ns := startTestNATSServer(t)

	nc, err := nats.Connect(ns.ClientURL())
	require.NoError(t, err)
	defer nc.Close()

	const message = "launch on node-1: InsufficientAddressCapacity"
	_, err = nc.Subscribe("test.serve.error", func(msg *nats.Msg) {
		ServeNATSRequest(msg, func(_ *struct{}) (*struct{}, error) {
			cause := errors.New(awserrors.ErrorInsufficientAddressCapacity)
			return nil, fmt.Errorf("launch on node-1: %w", cause)
		})
	})
	require.NoError(t, err)
	require.NoError(t, nc.Flush())

	_, err = NATSRequest[struct{}](context.Background(), nc, "test.serve.error", struct{}{}, 2*time.Second, "")
	require.Error(t, err)
	assert.Equal(t, message, err.Error())
	code, ok := awserrors.ResolveErrorCode(err)
	assert.True(t, ok)
	assert.Equal(t, awserrors.ErrorInsufficientAddressCapacity, code)
}

func TestNATSRequest_NoResponders(t *testing.T) {
	ns := startTestNATSServer(t)

	nc, err := nats.Connect(ns.ClientURL())
	require.NoError(t, err)
	defer nc.Close()

	type Resp struct{}
	_, err = NATSRequest[Resp](context.Background(), nc, "test.nobody", struct{}{}, 500*time.Millisecond, "")
	assert.Error(t, err)
}

func TestNATSRequest_Timeout(t *testing.T) {
	ns := startTestNATSServer(t)

	nc, err := nats.Connect(ns.ClientURL())
	require.NoError(t, err)
	defer nc.Close()

	// Responder that outlives the 100ms client timeout without replying.
	_, err = nc.QueueSubscribe("test.slow", "q", func(msg *nats.Msg) {
		time.Sleep(500 * time.Millisecond)
	})
	require.NoError(t, err)

	type Resp struct{}
	_, err = NATSRequest[Resp](context.Background(), nc, "test.slow", struct{}{}, 100*time.Millisecond, "")
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
	_, err = NATSRequest[Resp](context.Background(), nc, "test.badjson", struct{}{}, 2*time.Second, "")
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

	result, err := NATSRequest[Resp](context.Background(), nc, "test.account", Req{Name: "world"}, 2*time.Second, "111122223333")
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
	_, err = NATSRequest[Resp](context.Background(), nc, "test.marshalfail", make(chan int), 2*time.Second, "")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to marshal input")
}

// --- AccountIDFromMsg tests ---

func TestAccountIDFromMsg(t *testing.T) {
	msg := nats.NewMsg("test")
	msg.Header.Set(AccountIDHeader, "444455556666")

	assert.Equal(t, "444455556666", AccountIDFromMsg(msg))
}

func TestAccountIDFromMsg_Missing(t *testing.T) {
	msg := nats.NewMsg("test")
	assert.Empty(t, AccountIDFromMsg(msg))
}

func TestAccountIDFromMsg_NilMsg(t *testing.T) {
	assert.Empty(t, AccountIDFromMsg(nil))
}

func TestAccountIDFromMsg_NilHeader(t *testing.T) {
	msg := &nats.Msg{Subject: "test"}
	assert.Empty(t, AccountIDFromMsg(msg))
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
		WithMaxWait(100*time.Millisecond),
		WithRetryDelay(20*time.Millisecond),
	)
	elapsed := time.Since(start)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "NATS connect failed")
	// Two attempts sleep 20ms then 40ms, so anything past the first delay
	// alone can only have come from a retry.
	assert.GreaterOrEqual(t, elapsed, 40*time.Millisecond, "should have retried at least once")
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
		// The default sleep between reconnect attempts is a second, which is
		// most of this test's runtime and none of what it proves.
		WithReconnectWait(50*time.Millisecond),
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
	_, err = NATSRequest[map[string]any](context.Background(), nc, "ec2.Describe", struct{}{}, 5*time.Second, "")
	elapsed := time.Since(start)

	require.ErrorIs(t, err, ErrClusterUnavailable)
	assert.Less(t, elapsed, 500*time.Millisecond, "should bail before per-call timeout")
}

// --- 1c fail-fast tests: NATS request helpers reject when conn is down ---

func TestNATSRequest_NilConn_ReturnsClusterUnavailable(t *testing.T) {
	type Resp struct{}
	_, err := NATSRequest[Resp](context.Background(), nil, "test.never", struct{}{}, 50*time.Millisecond, "")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrClusterUnavailable)
}

func TestNATSRequest_ClosedConn_ReturnsClusterUnavailable(t *testing.T) {
	ns := startTestNATSServer(t)
	nc, err := nats.Connect(ns.ClientURL())
	require.NoError(t, err)
	nc.Close()

	type Resp struct{}
	_, err = NATSRequest[Resp](context.Background(), nc, "test.never", struct{}{}, 50*time.Millisecond, "")
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
		WithReconnectWait(50*time.Millisecond),
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

// TestConnectNATSWithRetry_LogEscalatesPastThreshold verifies that log lines escalate from Warn to Error
// once attempt count exceeds natsRetryEscalateAttempt, while earlier attempts stay at Warn.
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
	assert.Contains(t, logs, "disconnected_for_ms=", "escalated error should include disconnected_for_ms")
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

// TestAddNAT_Success pins that AddNAT returns nil only when vpcd acks the
// add-nat request with {"success":true}. The wire payload must match the
// natEvent shape vpcd unmarshals on the other end.
func TestAddNAT_Success(t *testing.T) {
	ns := startTestNATSServer(t)
	nc, err := nats.Connect(ns.ClientURL())
	require.NoError(t, err)
	defer nc.Close()

	var got natEvent
	_, err = nc.Subscribe("vpc.add-nat", func(msg *nats.Msg) {
		_ = json.Unmarshal(msg.Data, &got)
		_ = msg.Respond([]byte(`{"success":true}`))
	})
	require.NoError(t, err)

	err = AddNAT(nc, "vpc-1", "203.0.113.5", "10.0.0.5", "port-eni-1", "02:00:00:00:00:01")
	require.NoError(t, err)
	assert.Equal(t, "vpc-1", got.VpcId)
	assert.Equal(t, "203.0.113.5", got.ExternalIP)
	assert.Equal(t, "10.0.0.5", got.LogicalIP)
	assert.Equal(t, "port-eni-1", got.PortName)
	assert.Equal(t, "02:00:00:00:00:01", got.MAC)
}

// TestAddNAT_NACK is the regression for the silent-corruption bug: a vpcd failure must return a non-nil error
// so callers can roll back IPAM and ENI public IP state (previously the helper only logged a warning).
func TestAddNAT_NACK(t *testing.T) {
	ns := startTestNATSServer(t)
	nc, err := nats.Connect(ns.ClientURL())
	require.NoError(t, err)
	defer nc.Close()

	_, err = nc.Subscribe("vpc.add-nat", func(msg *nats.Msg) {
		_ = msg.Respond([]byte(`{"success":false,"error":"northd unavailable"}`))
	})
	require.NoError(t, err)

	err = AddNAT(nc, "vpc-1", "203.0.113.5", "10.0.0.5", "port-eni-1", "02:00:00:00:00:01")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "northd unavailable")
}

// TestAddNAT_NoResponders ensures a vpcd outage (no subscriber on the topic)
// surfaces as an error rather than a swallowed warning.
func TestAddNAT_NoResponders(t *testing.T) {
	ns := startTestNATSServer(t)
	nc, err := nats.Connect(ns.ClientURL())
	require.NoError(t, err)
	defer nc.Close()

	err = AddNAT(nc, "vpc-1", "203.0.113.5", "10.0.0.5", "port-eni-1", "02:00:00:00:00:01")
	require.Error(t, err)
}

// --- Gather tests ---

func TestGather_EarlyExitBeforeTimeout(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)

	for range 3 {
		_, err := nc.Subscribe("test.gather.early", func(msg *nats.Msg) {
			_ = msg.Respond([]byte(`{"ok":true}`))
		})
		require.NoError(t, err)
	}

	start := time.Now()
	frames, sum, err := Gather(context.Background(), nc, "test.gather.early", []byte("{}"),
		GatherOpts{Timeout: 5 * time.Second, ExpectedNodes: 3})
	elapsed := time.Since(start)

	require.NoError(t, err)
	assert.Len(t, frames, 3)
	assert.Equal(t, 3, sum.Received)
	assert.Equal(t, 3, sum.Successes)
	assert.False(t, sum.TimedOut)
	assert.Less(t, elapsed, 2*time.Second)
}

func TestGather_TimesOutBelowExpected(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)

	for range 2 {
		_, err := nc.Subscribe("test.gather.timeout", func(msg *nats.Msg) {
			_ = msg.Respond([]byte(`{"ok":true}`))
		})
		require.NoError(t, err)
	}

	frames, sum, err := Gather(context.Background(), nc, "test.gather.timeout", []byte("{}"),
		GatherOpts{Timeout: 300 * time.Millisecond, ExpectedNodes: 3})

	require.NoError(t, err)
	assert.Len(t, frames, 2)
	assert.Equal(t, 2, sum.Received)
	assert.True(t, sum.TimedOut)
}

func TestGather_MixedSuccessAndErrors(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)

	_, err := nc.Subscribe("test.gather.mixed", func(msg *nats.Msg) {
		_ = msg.Respond([]byte(`{"ok":true}`))
	})
	require.NoError(t, err)
	for range 2 {
		_, err = nc.Subscribe("test.gather.mixed", func(msg *nats.Msg) {
			_ = msg.Respond(GenerateErrorPayload(awserrors.ErrorInvalidInstanceIDNotFound))
		})
		require.NoError(t, err)
	}
	// A 5xx error must be counted but must not become FirstClient4xx.
	_, err = nc.Subscribe("test.gather.mixed", func(msg *nats.Msg) {
		_ = msg.Respond(GenerateErrorPayload(awserrors.ErrorBandwidthLimitExceeded))
	})
	require.NoError(t, err)

	frames, sum, err := Gather(context.Background(), nc, "test.gather.mixed", []byte("{}"),
		GatherOpts{Timeout: 2 * time.Second, ExpectedNodes: 4})

	require.NoError(t, err)
	assert.Len(t, frames, 1)
	assert.Equal(t, 4, sum.Received)
	assert.Equal(t, 1, sum.Successes)
	assert.Equal(t, 2, sum.ErrorCodes[awserrors.ErrorInvalidInstanceIDNotFound])
	assert.Equal(t, 1, sum.ErrorCodes[awserrors.ErrorBandwidthLimitExceeded])
	assert.Equal(t, awserrors.ErrorInvalidInstanceIDNotFound, sum.FirstClient4xx)
}

func TestGather_StopOnFirstSkipsErrors(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)

	for range 2 {
		_, err := nc.Subscribe("test.gather.first", func(msg *nats.Msg) {
			_ = msg.Respond(GenerateErrorPayload(awserrors.ErrorInvalidInstanceIDNotFound))
		})
		require.NoError(t, err)
	}
	// Delayed so the two error frames are processed (and skipped) first.
	_, err := nc.Subscribe("test.gather.first", func(msg *nats.Msg) {
		time.Sleep(50 * time.Millisecond)
		_ = msg.Respond([]byte(`{"value":"found"}`))
	})
	require.NoError(t, err)

	frames, sum, err := Gather(context.Background(), nc, "test.gather.first", []byte("{}"),
		GatherOpts{Timeout: 2 * time.Second, ExpectedNodes: 3, StopOnFirst: true})

	require.NoError(t, err)
	require.Len(t, frames, 1)
	var out struct {
		Value string `json:"value"`
	}
	require.NoError(t, json.Unmarshal(frames[0].Data, &out))
	assert.Equal(t, "found", out.Value)
	assert.Equal(t, 1, sum.Successes)
	assert.False(t, sum.TimedOut)
	assert.Equal(t, 2, sum.ErrorCodes[awserrors.ErrorInvalidInstanceIDNotFound])
}

func TestGather_OversizedFrameDropped(t *testing.T) {
	opts := &server.Options{
		Host:       "127.0.0.1",
		Port:       -1,
		NoLog:      true,
		NoSigs:     true,
		MaxPayload: maxScatterGatherResponseSize + 1024*1024, // headroom above the 10 MB cap
	}
	ns, err := server.NewServer(opts)
	require.NoError(t, err)
	go ns.Start()
	require.True(t, ns.ReadyForConnections(5*time.Second))
	t.Cleanup(func() { ns.Shutdown() })

	nc, err := nats.Connect(ns.ClientURL())
	require.NoError(t, err)
	t.Cleanup(func() { nc.Close() })

	big := bytes.Repeat([]byte("a"), maxScatterGatherResponseSize+1)
	_, err = nc.Subscribe("test.gather.oversized", func(msg *nats.Msg) {
		_ = msg.Respond(big)
	})
	require.NoError(t, err)

	frames, sum, err := Gather(context.Background(), nc, "test.gather.oversized", []byte("{}"),
		GatherOpts{Timeout: 2 * time.Second, ExpectedNodes: 1})

	require.NoError(t, err)
	assert.Empty(t, frames)
	assert.Equal(t, 1, sum.Received)
	assert.Equal(t, 0, sum.Successes)
}

// gatherAcctEcho reports the X-Account-ID header a Gather request carried.
type gatherAcctEcho struct {
	ID      string `json:"id"`
	Present bool   `json:"present"`
}

func TestGather_AccountIDHeaderSet(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)

	_, err := nc.Subscribe("test.gather.acct.set", func(msg *nats.Msg) {
		echo := gatherAcctEcho{
			ID:      msg.Header.Get(AccountIDHeader),
			Present: len(msg.Header.Values(AccountIDHeader)) > 0,
		}
		data, _ := json.Marshal(echo)
		_ = msg.Respond(data)
	})
	require.NoError(t, err)

	frames, _, err := Gather(context.Background(), nc, "test.gather.acct.set", []byte("{}"),
		GatherOpts{Timeout: time.Second, ExpectedNodes: 1, AccountID: "111122223333"})
	require.NoError(t, err)
	require.Len(t, frames, 1)

	var echo gatherAcctEcho
	require.NoError(t, json.Unmarshal(frames[0].Data, &echo))
	assert.True(t, echo.Present)
	assert.Equal(t, "111122223333", echo.ID)
}

func TestGather_AccountIDHeaderAbsentWhenEmpty(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)

	_, err := nc.Subscribe("test.gather.acct.empty", func(msg *nats.Msg) {
		echo := gatherAcctEcho{
			ID:      msg.Header.Get(AccountIDHeader),
			Present: len(msg.Header.Values(AccountIDHeader)) > 0,
		}
		data, _ := json.Marshal(echo)
		_ = msg.Respond(data)
	})
	require.NoError(t, err)

	frames, _, err := Gather(context.Background(), nc, "test.gather.acct.empty", []byte("{}"),
		GatherOpts{Timeout: time.Second, ExpectedNodes: 1})
	require.NoError(t, err)
	require.Len(t, frames, 1)

	var echo gatherAcctEcho
	require.NoError(t, json.Unmarshal(frames[0].Data, &echo))
	assert.False(t, echo.Present)
	assert.Empty(t, echo.ID)
}

func TestGather_NilConn_ReturnsClusterUnavailable(t *testing.T) {
	frames, sum, err := Gather(context.Background(), nil, "test.never", []byte("{}"),
		GatherOpts{Timeout: 50 * time.Millisecond, ExpectedNodes: 1})
	require.ErrorIs(t, err, ErrClusterUnavailable)
	assert.Nil(t, frames)
	assert.NotNil(t, sum.ErrorCodes)
}

func TestGather_ClosedConn_ReturnsClusterUnavailable(t *testing.T) {
	ns := startTestNATSServer(t)
	nc, err := nats.Connect(ns.ClientURL())
	require.NoError(t, err)
	nc.Close()

	_, _, err = Gather(context.Background(), nc, "test.never", []byte("{}"),
		GatherOpts{Timeout: 50 * time.Millisecond, ExpectedNodes: 1})
	require.ErrorIs(t, err, ErrClusterUnavailable)
}

// --- Gather identity-mode tests ---

// subscribeAsNode replies on subject as nodeID, carrying the X-Node-ID header
// a real daemon reply would set. delay, if non-zero, is applied before
// responding, so tests can control arrival order.
func subscribeAsNode(t *testing.T, nc *nats.Conn, subject, nodeID string, data []byte, delay time.Duration) {
	t.Helper()
	_, err := nc.Subscribe(subject, func(msg *nats.Msg) {
		if delay > 0 {
			time.Sleep(delay)
		}
		reply := nats.NewMsg(msg.Reply)
		reply.Data = data
		if nodeID != "" {
			reply.Header.Set(NodeIDHeader, nodeID)
		}
		_ = msg.RespondMsg(reply)
	})
	require.NoError(t, err)
}

// A caller that sets neither ExpectedResponders nor CollectUntilDeadline gets
// exactly the pre-identity-mode Gather: the new Summary fields stay nil, not
// merely empty, so a caller checking len(sum.Responders) == 0 cannot
// accidentally read "identity mode ran and saw nobody" from "identity mode
// never ran".
func TestGather_LegacyMode_IdentityFieldsStayNil(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)
	subscribeAsNode(t, nc, "test.gather.legacy", "node-a", []byte(`{"ok":true}`), 0)

	frames, sum, err := Gather(context.Background(), nc, "test.gather.legacy", []byte("{}"),
		GatherOpts{Timeout: time.Second, ExpectedNodes: 1})

	require.NoError(t, err)
	require.Len(t, frames, 1)
	assert.Equal(t, "node-a", frames[0].NodeID, "the frame still carries whatever node ID the reply set")
	assert.Nil(t, sum.Responders)
	assert.Nil(t, sum.SuccessResponders)
	assert.Nil(t, sum.ErrorResponders)
	assert.Nil(t, sum.ConflictNodes)
}

// ExpectedResponders under the default CollectServeData mode exits as soon as
// the distinct-node count is met, same early-exit shape as ExpectedNodes, and
// tags each frame with the node that sent it.
func TestGather_IdentityMode_NodeIDCarriedOnFramesAndEarlyExit(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)
	subscribeAsNode(t, nc, "test.gather.identity.early", "node-a", []byte(`{"ok":true}`), 0)
	subscribeAsNode(t, nc, "test.gather.identity.early", "node-b", []byte(`{"ok":true}`), 0)

	start := time.Now()
	frames, sum, err := Gather(context.Background(), nc, "test.gather.identity.early", []byte("{}"),
		GatherOpts{Timeout: 5 * time.Second, ExpectedResponders: 2})
	elapsed := time.Since(start)

	require.NoError(t, err)
	require.Len(t, frames, 2)
	assert.Less(t, elapsed, 2*time.Second)
	assert.False(t, sum.TimedOut)
	gotIDs := map[string]bool{frames[0].NodeID: true, frames[1].NodeID: true}
	assert.Equal(t, map[string]bool{"node-a": true, "node-b": true}, gotIDs)
	assert.Len(t, sum.Responders, 2)
}

// Responders is every node that answered at all; SuccessResponders and
// ErrorResponders partition it by how. A node cannot land in both from a
// single reply.
func TestGather_IdentityMode_RespondersPartitionSuccessAndError(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)
	subscribeAsNode(t, nc, "test.gather.identity.partition", "node-ok", []byte(`{"ok":true}`), 0)
	subscribeAsNode(t, nc, "test.gather.identity.partition", "node-err",
		GenerateErrorPayload(awserrors.ErrorInvalidInstanceIDNotFound), 0)

	_, sum, err := Gather(context.Background(), nc, "test.gather.identity.partition", []byte("{}"),
		GatherOpts{Timeout: 2 * time.Second, ExpectedResponders: 2})

	require.NoError(t, err)
	assert.Equal(t, map[string]bool{"node-ok": true, "node-err": true}, sum.Responders)
	assert.Equal(t, map[string]bool{"node-ok": true}, sum.SuccessResponders)
	assert.Equal(t, map[string]bool{"node-err": true}, sum.ErrorResponders)
}

// A reply with no X-Node-ID header (an older daemon, or a stray publisher on
// the subject) cannot be attributed to any node, so it is counted separately
// rather than silently folded into the responder sets — a describe-level
// completeness judgement must be able to see it and refuse to trust the sweep.
func TestGather_IdentityMode_UnidentifiedFrameCounted(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)
	subscribeAsNode(t, nc, "test.gather.identity.unident", "node-a", []byte(`{"ok":true}`), 0)
	// No node ID set on this reply.
	_, err := nc.Subscribe("test.gather.identity.unident", func(msg *nats.Msg) {
		_ = msg.Respond([]byte(`{"ok":true}`))
	})
	require.NoError(t, err)

	_, sum, err := Gather(context.Background(), nc, "test.gather.identity.unident", []byte("{}"),
		GatherOpts{Timeout: 500 * time.Millisecond, ExpectedResponders: 2})

	require.NoError(t, err)
	assert.Equal(t, 1, sum.Unidentified)
	assert.Len(t, sum.Responders, 1, "only the identified reply counts toward the responder set")
}

// A node that replies twice with disagreeing payloads (the restart-overlap
// case: an old subscription still draining alongside the new one) keeps only
// its first payload — never the second, win or lose — and is flagged in
// ConflictNodes so a completeness judgement downstream can refuse to trust it.
func TestGather_IdentityMode_DuplicateDisagreeingFrame_FirstPayloadWinsAndFlagsConflict(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)
	// Two subscribers answering as the same node ID: the first reply must be
	// the one retained regardless of arrival order tie-breaks, so give the
	// disagreeing second reply a small delay to make the ordering deterministic.
	subscribeAsNode(t, nc, "test.gather.identity.dup", "node-a", []byte(`{"value":"first"}`), 0)
	subscribeAsNode(t, nc, "test.gather.identity.dup", "node-a", []byte(`{"value":"second"}`), 50*time.Millisecond)

	frames, sum, err := Gather(context.Background(), nc, "test.gather.identity.dup", []byte("{}"),
		GatherOpts{Timeout: time.Second, Mode: CollectUntilDeadline, ExpectedResponders: 1})

	require.NoError(t, err)
	require.Len(t, frames, 1, "the disagreeing duplicate's bytes must never be retained")
	assert.JSONEq(t, `{"value":"first"}`, string(frames[0].Data))
	assert.Equal(t, map[string]bool{"node-a": true}, sum.ConflictNodes)
	assert.Equal(t, 1, sum.DuplicateFrames)
}

// A node replying twice with identical payloads (a harmless at-least-once
// redelivery) is still counted as a duplicate frame, but is not a conflict:
// there is nothing to disagree about.
func TestGather_IdentityMode_DuplicateIdenticalFrame_NotFlaggedAsConflict(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)
	subscribeAsNode(t, nc, "test.gather.identity.dupsame", "node-a", []byte(`{"value":"same"}`), 0)
	subscribeAsNode(t, nc, "test.gather.identity.dupsame", "node-a", []byte(`{"value":"same"}`), 50*time.Millisecond)

	_, sum, err := Gather(context.Background(), nc, "test.gather.identity.dupsame", []byte("{}"),
		GatherOpts{Timeout: 300 * time.Millisecond, Mode: CollectUntilDeadline, ExpectedResponders: 1})

	require.NoError(t, err)
	assert.Empty(t, sum.ConflictNodes)
	assert.Equal(t, 1, sum.DuplicateFrames)
}

// CollectUntilDeadline exists specifically because absence cannot be proven
// from a prefix of the replies: it must not exit early just because every
// currently-expected responder has already answered.
func TestGather_CollectUntilDeadline_DoesNotExitEarly(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)
	subscribeAsNode(t, nc, "test.gather.identity.deadline", "node-a", []byte(`{"ok":true}`), 0)

	const budget = 400 * time.Millisecond
	start := time.Now()
	_, sum, err := Gather(context.Background(), nc, "test.gather.identity.deadline", []byte("{}"),
		GatherOpts{Timeout: budget, Mode: CollectUntilDeadline, ExpectedResponders: 1})
	elapsed := time.Since(start)

	require.NoError(t, err)
	assert.GreaterOrEqual(t, elapsed, budget-20*time.Millisecond,
		"CollectUntilDeadline must run to the deadline even though the only expected responder already answered")
	assert.True(t, sum.TimedOut)
	assert.Equal(t, map[string]bool{"node-a": true}, sum.Responders)
}

// Adapted restart-overlap acceptance scenario: node-1 has an old subscription
// still draining alongside its new one and answers twice with disagreeing
// payloads (simulating a daemon mid-restart); node-2 never answers within the
// deadline. The mechanism this pins is what a describe-level completeness
// judgement relies on: ConflictNodes and the missing node-2 are both visible
// in Summary, so the caller can refuse the sweep rather than trust a stale
// answer. It does not assert the instance is present in the result — under
// the documented first-payload-wins rule the disagreeing reply's data is
// dropped regardless, only the conflict flag survives it.
func TestGather_CollectUntilDeadline_RestartOverlap_SurfacesConflictAndMissingNode(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)
	subscribeAsNode(t, nc, "test.gather.identity.restart", "node-1", []byte(`{"value":"stale"}`), 0)
	subscribeAsNode(t, nc, "test.gather.identity.restart", "node-1", []byte(`{"value":"fresh"}`), 50*time.Millisecond)
	// node-2 is expected but never answers.

	_, sum, err := Gather(context.Background(), nc, "test.gather.identity.restart", []byte("{}"),
		GatherOpts{Timeout: 300 * time.Millisecond, Mode: CollectUntilDeadline, ExpectedResponders: 2})

	require.NoError(t, err)
	assert.True(t, sum.ConflictNodes["node-1"], "the restart overlap must be visible as a conflict, not silently resolved")
	assert.False(t, sum.Responders["node-2"], "node-2 never answered and must not be reported as having done so")
}
