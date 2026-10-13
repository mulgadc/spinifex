package spinifexui

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const tlsRecordTypeHandshake = 0x16

// errPlainHTTP fails the TLS handshake after a plain-HTTP client was redirected.
var errPlainHTTP = errors.New("plain HTTP redirected to HTTPS")

// tlsSplitListener serves TLS and plain HTTP on one port. Accept never reads:
// the first byte is sniffed during the handshake, in the connection's own
// goroutine, so a client that sends nothing cannot stall other clients.
type tlsSplitListener struct {
	net.Listener

	port   int
	tlsCfg *tls.Config
}

func (ln *tlsSplitListener) Accept() (net.Conn, error) {
	conn, err := ln.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return tls.Server(&sniffConn{Conn: conn, ln: ln}, ln.tlsCfg), nil
}

// sniffConn reads the first byte on its first Read. A TLS ClientHello (0x16)
// is replayed to the TLS layer; anything else gets an HTTPS redirect. Reads
// are serialised by tls.Conn and bounded by http.Server's handshake timeout.
type sniffConn struct {
	net.Conn

	ln *tlsSplitListener
	r  io.Reader
}

func (c *sniffConn) Read(b []byte) (int, error) {
	if c.r == nil {
		var buf [1]byte
		if _, err := io.ReadFull(c.Conn, buf[:]); err != nil {
			return 0, err
		}
		if buf[0] != tlsRecordTypeHandshake {
			c.ln.redirectHTTP(c.Conn, buf[0])
			return 0, errPlainHTTP
		}
		c.r = io.MultiReader(bytes.NewReader(buf[:]), c.Conn)
	}
	return c.r.Read(b)
}

// redirectHTTP reads an HTTP request from conn (with the first byte already
// consumed into firstByte), writes a 301 redirect to HTTPS, and closes conn.
func (ln *tlsSplitListener) redirectHTTP(conn net.Conn, firstByte byte) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	br := bufio.NewReader(io.MultiReader(bytes.NewReader([]byte{firstByte}), conn))
	req, err := http.ReadRequest(br)
	if err != nil {
		return
	}
	defer req.Body.Close()

	host := req.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}

	// Sanitize host to prevent header injection via CRLF.
	if strings.ContainsAny(host, "\r\n") {
		return
	}

	target := "https://" + net.JoinHostPort(host, strconv.Itoa(ln.port)) + req.URL.RequestURI()
	resp := &http.Response{
		StatusCode: http.StatusMovedPermanently,
		ProtoMajor: 1,
		ProtoMinor: 1,
		Header:     http.Header{"Location": {target}, "Connection": {"close"}},
		Body:       http.NoBody,
	}
	_ = resp.Write(conn)
}
