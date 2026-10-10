package spinifexui

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const tlsRecordTypeHandshake = 0x16

// tlsSplitListener accepts connections and reads the first byte. If it looks
// like a TLS ClientHello (0x16), the connection is wrapped with TLS. Otherwise,
// a plain-HTTP redirect to HTTPS is sent and the connection is closed.
type tlsSplitListener struct {
	net.Listener

	port   int
	tlsCfg *tls.Config
}

func (ln *tlsSplitListener) Accept() (net.Conn, error) {
	for {
		conn, err := ln.Listener.Accept()
		if err != nil {
			return nil, err
		}

		// Set a deadline for the initial protocol detection byte so a
		// slow/malicious client cannot block the accept loop.
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))

		var buf [1]byte
		if _, err := io.ReadFull(conn, buf[:]); err != nil {
			_ = conn.Close()
			continue
		}

		// Clear the deadline before handing off the connection.
		_ = conn.SetReadDeadline(time.Time{})

		if buf[0] == tlsRecordTypeHandshake {
			// Prepend the peeked byte and upgrade to TLS.
			merged := &prefixConn{
				Conn: conn,
				r:    io.MultiReader(bytes.NewReader(buf[:]), conn),
			}
			return tls.Server(merged, ln.tlsCfg), nil
		}

		// Plain HTTP — send redirect and close.
		go ln.redirectHTTP(conn, buf[0])
	}
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

// prefixConn wraps a net.Conn with a reader that replays prefixed bytes.
type prefixConn struct {
	net.Conn

	r io.Reader
}

func (c *prefixConn) Read(b []byte) (int, error) {
	return c.r.Read(b)
}
