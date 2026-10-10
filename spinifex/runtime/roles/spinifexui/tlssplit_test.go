package spinifexui

import (
	"bufio"
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPrefixConn_Read_ReplaysPrefixThenStream(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	go func() {
		_, _ = client.Write([]byte("est"))
	}()

	pc := &prefixConn{
		Conn: server,
		r:    io.MultiReader(bytes.NewReader([]byte{'t'}), server),
	}

	buf := make([]byte, 4)
	n, err := io.ReadFull(pc, buf)
	require.NoError(t, err)
	assert.Equal(t, 4, n)
	assert.Equal(t, "test", string(buf))
}

// startSplitListener returns a tlsSplitListener on a random local port. Caller
// should close the returned net.Listener when done. The Accept loop runs in a
// goroutine and accepted TLS connections are pushed onto the returned channel.
func startSplitListener(t *testing.T, tlsCfg *tls.Config, redirectPort int) (*tlsSplitListener, net.Listener, <-chan net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	splitLn := &tlsSplitListener{
		Listener: ln,
		port:     redirectPort,
		tlsCfg:   tlsCfg,
	}

	accepted := make(chan net.Conn, 1)
	go func() {
		defer close(accepted)
		for {
			c, err := splitLn.Accept()
			if err != nil {
				return
			}
			accepted <- c
		}
	}()

	return splitLn, ln, accepted
}

func TestTLSSplitListener_Accept_TLSHandshakeByteWrapsTLS(t *testing.T) {
	cert := loadTLSCert(t)
	tlsCfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}

	_, ln, accepted := startSplitListener(t, tlsCfg, 8443)
	defer ln.Close()

	conn, err := net.Dial("tcp", ln.Addr().String())
	require.NoError(t, err)
	defer conn.Close()

	// First byte 0x16 = TLS ClientHello record type — listener should wrap with TLS
	// without yet performing the handshake.
	_, err = conn.Write([]byte{0x16})
	require.NoError(t, err)

	select {
	case c := <-accepted:
		require.NotNil(t, c)
		defer c.Close()
		_, ok := c.(*tls.Conn)
		assert.True(t, ok, "expected *tls.Conn, got %T", c)
	case <-time.After(2 * time.Second):
		t.Fatal("Accept did not return a TLS connection in time")
	}
}

func TestTLSSplitListener_Accept_PlainHTTPRedirectsToHTTPS(t *testing.T) {
	_, ln, _ := startSplitListener(t, &tls.Config{}, 8443)
	defer ln.Close()

	conn, err := net.Dial("tcp", ln.Addr().String())
	require.NoError(t, err)
	defer conn.Close()

	_, err = conn.Write([]byte("GET /foo?x=1 HTTP/1.1\r\nHost: example.com\r\n\r\n"))
	require.NoError(t, err)

	require.NoError(t, conn.SetReadDeadline(time.Now().Add(3*time.Second)))
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusMovedPermanently, resp.StatusCode)
	assert.Equal(t, "https://example.com:8443/foo?x=1", resp.Header.Get("Location"))
}

func TestTLSSplitListener_Accept_PlainHTTPStripsHostPort(t *testing.T) {
	_, ln, _ := startSplitListener(t, &tls.Config{}, 9443)
	defer ln.Close()

	conn, err := net.Dial("tcp", ln.Addr().String())
	require.NoError(t, err)
	defer conn.Close()

	// Host with explicit port — redirectHTTP should strip it before re-emitting.
	_, err = conn.Write([]byte("GET /a HTTP/1.1\r\nHost: example.com:80\r\n\r\n"))
	require.NoError(t, err)

	require.NoError(t, conn.SetReadDeadline(time.Now().Add(3*time.Second)))
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, "https://example.com:9443/a", resp.Header.Get("Location"))
}

func TestTLSSplitListener_Accept_MalformedHTTPClosesSilently(t *testing.T) {
	_, ln, _ := startSplitListener(t, &tls.Config{}, 8443)
	defer ln.Close()

	conn, err := net.Dial("tcp", ln.Addr().String())
	require.NoError(t, err)
	defer conn.Close()

	// Non-TLS first byte but garbage afterwards — http.ReadRequest fails and
	// redirectHTTP returns without writing a response.
	_, err = conn.Write([]byte("NOT-AN-HTTP-REQUEST\r\n\r\n"))
	require.NoError(t, err)

	require.NoError(t, conn.SetReadDeadline(time.Now().Add(2*time.Second)))
	buf := make([]byte, 256)
	n, _ := conn.Read(buf)
	assert.Equal(t, 0, n, "no response expected for malformed request")
}

func TestTLSSplitListener_Accept_ClientClosesBeforeFirstByte(t *testing.T) {
	cert := loadTLSCert(t)
	tlsCfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}

	_, ln, accepted := startSplitListener(t, tlsCfg, 8443)
	defer ln.Close()

	// Dial and immediately close — listener should drop the conn and keep looping.
	c, err := net.Dial("tcp", ln.Addr().String())
	require.NoError(t, err)
	require.NoError(t, c.Close())

	// Now send a valid TLS first byte from a fresh connection to verify the
	// loop still serves subsequent clients.
	c2, err := net.Dial("tcp", ln.Addr().String())
	require.NoError(t, err)
	defer c2.Close()
	_, err = c2.Write([]byte{0x16})
	require.NoError(t, err)

	select {
	case got := <-accepted:
		require.NotNil(t, got)
		defer got.Close()
		_, ok := got.(*tls.Conn)
		assert.True(t, ok)
	case <-time.After(2 * time.Second):
		t.Fatal("Accept loop did not recover after client disconnect")
	}
}

// loadTLSCert builds a self-signed certificate for local listener tests.
func loadTLSCert(t *testing.T) tls.Certificate {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "tlssplit-test"},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	require.NoError(t, err)

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})

	keyDER, err := x509.MarshalECPrivateKey(key)
	require.NoError(t, err)
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	require.NoError(t, err)
	return cert
}
