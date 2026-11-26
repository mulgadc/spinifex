package issuer

import (
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"time"
)

const (
	signingAlgorithm = "AWS4-X509-ECDSA-SHA256"
	signingService   = "rolesanywhere"
	amzDateLayout    = "20060102T150405Z"
	scopeDateLayout  = "20060102"
	signedHeaders    = "content-type;host;x-amz-date;x-amz-x509"
)

// canonicalRequest and the string to sign follow the IAM Roles Anywhere
// signing process: SigV4 with the certificate in X-Amz-X509.
func canonicalRequest(host, amzDate, certB64, path string, body []byte) string {
	payload := sha256.Sum256(body)
	return "POST\n" + path + "\n\n" +
		"content-type:application/json\n" +
		"host:" + host + "\n" +
		"x-amz-date:" + amzDate + "\n" +
		"x-amz-x509:" + certB64 + "\n\n" +
		signedHeaders + "\n" +
		hex.EncodeToString(payload[:])
}

func stringToSign(amzDate, scope, canonical string) string {
	h := sha256.Sum256([]byte(canonical))
	return signingAlgorithm + "\n" + amzDate + "\n" + scope + "\n" + hex.EncodeToString(h[:])
}

// sign sets Content-Type, X-Amz-Date, X-Amz-X509 and Authorization on req,
// which must be a POST of body to the Roles Anywhere sessions path.
func sign(req *http.Request, body []byte, cert *ClientCertificate, region string, now time.Time) error {
	if cert == nil || cert.Certificate == nil || cert.key == nil {
		return fmt.Errorf("rolesanywhere sign: certificate required")
	}
	now = now.UTC()
	amzDate := now.Format(amzDateLayout)
	scope := now.Format(scopeDateLayout) + "/" + region + "/" + signingService + "/aws4_request"
	certB64 := base64.StdEncoding.EncodeToString(cert.Certificate.Raw)

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-X509", certB64)

	canonical := canonicalRequest(req.URL.Host, amzDate, certB64, req.URL.Path, body)
	digest := sha256.Sum256([]byte(stringToSign(amzDate, scope, canonical)))
	sig, err := ecdsa.SignASN1(rand.Reader, cert.key, digest[:])
	if err != nil {
		return fmt.Errorf("rolesanywhere sign: %w", err)
	}
	req.Header.Set("Authorization", signingAlgorithm+
		" Credential="+cert.Certificate.SerialNumber.String()+"/"+scope+
		", SignedHeaders="+signedHeaders+
		", Signature="+hex.EncodeToString(sig))
	return nil
}
