package arn

import (
	"fmt"
	"strings"
)

// ACMResourceType is the resource-type segment of an arn:aws:acm ARN.
type ACMResourceType string

const ACMCertificate ACMResourceType = "certificate"

// FormatACMCertificate builds arn:aws:acm:<region>:<account>:certificate/<id>.
func FormatACMCertificate(region, accountID, id string) string {
	return fmt.Sprintf("arn:aws:acm:%s:%s:%s/%s", region, accountID, ACMCertificate, id)
}

// ParseACMCertificateID returns the certificate id from an ACM certificate ARN.
// The region and account segments are discarded rather than validated: callers
// re-anchor on their own, so a caller-supplied anchor is never consulted.
// ok is false for any other shape.
//
// The id keeps everything after the first "/". Truncating at the last one would
// let a grant on certificate/<a> authorize a request naming certificate/<a>/<b>.
func ParseACMCertificateID(certARN string) (string, bool) {
	parts := strings.SplitN(certARN, ":", 6)
	if len(parts) != 6 || parts[0] != "arn" || parts[2] != "acm" {
		return "", false
	}
	kind, id, found := strings.Cut(parts[5], "/")
	if !found || ACMResourceType(kind) != ACMCertificate || id == "" {
		return "", false
	}
	return id, true
}
