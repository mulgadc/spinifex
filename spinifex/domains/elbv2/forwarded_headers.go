package elbv2

import (
	"fmt"
	"strconv"
)

// Load balancer attribute keys that shape the X-Forwarded-* headers.
const (
	AttrXFFHeaderProcessing = "routing.http.xff_header_processing.mode"
	AttrXFFClientPort       = "routing.http.xff_client_port.enabled"
)

// X-Forwarded-For processing modes.
const (
	XFFModeAppend   = "append"
	XFFModePreserve = "preserve"
	XFFModeRemove   = "remove"
)

// maxXFFLines bounds how many X-Forwarded-For header lines a request may carry
// before later ones are left out of the appended value; HAProxy cannot loop.
const maxXFFLines = 8

// forwardedHeaderRules returns the http-request directives for one HTTP or
// HTTPS frontend. An unknown mode renders as the default, append.
func forwardedHeaderRules(mode string, clientPort, secure bool, listenerPort int64) []string {
	scheme := "http"
	if secure {
		scheme = "https"
	}
	var rules []string
	switch mode {
	case XFFModePreserve:
	case XFFModeRemove:
		rules = append(rules, "http-request del-header X-Forwarded-For")
	default:
		rules = appendXFFRules(clientPort)
	}
	return append(rules,
		"http-request set-header X-Forwarded-Proto "+scheme,
		"http-request set-header X-Forwarded-Port "+strconv.FormatInt(listenerPort, 10),
	)
}

// appendXFFRules joins every client X-Forwarded-For line into one value and
// appends the client address, as AWS does. req.hdr keeps only the last
// comma-separated item and req.fhdr only the last line, so each line is read
// by index into a transaction variable.
func appendXFFRules(clientPort bool) []string {
	rules := []string{
		`http-request set-var-fmt(txn.xff) "%[req.fhdr(X-Forwarded-For,1)]" if { req.fhdr_cnt(X-Forwarded-For) ge 1 }`,
	}
	for i := 2; i <= maxXFFLines; i++ {
		rules = append(rules, fmt.Sprintf(
			`http-request set-var-fmt(txn.xff) "%%[var(txn.xff)], %%[req.fhdr(X-Forwarded-For,%d)]" if { req.fhdr_cnt(X-Forwarded-For) ge %d }`, i, i))
	}

	client := "%[src]"
	if clientPort {
		client = "%[var(txn.xffc)]"
		rules = append(rules,
			`http-request set-var-fmt(txn.xffc) "[%[src]]:%[src_port]" if { src -m reg : }`,
			`http-request set-var-fmt(txn.xffc) "%[src]:%[src_port]" unless { src -m reg : }`)
	}
	return append(rules,
		`http-request set-header X-Forwarded-For "%[var(txn.xff)], `+client+`" if { var(txn.xff) -m len gt 0 }`,
		`http-request set-header X-Forwarded-For "`+client+`" unless { var(txn.xff) -m len gt 0 }`,
	)
}
