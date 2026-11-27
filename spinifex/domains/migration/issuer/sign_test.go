package issuer

import (
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Fixture made with openssl (serial 4660, subject spx-conn-1). The expected
// hashes below were computed outside this code base with python hashlib from
// the canonical request in the AWS signing guide.
const fixtureKeyPEM = `-----BEGIN PRIVATE KEY-----
MIGHAgEAMBMGByqGSM49AgEGCCqGSM49AwEHBG0wawIBAQQgqt28HAW4R9A4xdwM
7w9eE4hlN6d6/whGm7aBRsQbMcuhRANCAAR5a7eq/KOO2xBdvGkIDqXupQhgR6aJ
/Z+a/5RY4az8RiDSfrcGIcDngOovGLRXu7KYbJshUSiGbLuwpAkQACgh
-----END PRIVATE KEY-----
`

const fixtureCertPEM = `-----BEGIN CERTIFICATE-----
MIIB3jCCAYOgAwIBAgICEjQwCgYIKoZIzj0EAwIwTDETMBEGA1UEAwwKc3B4LWNv
bm4tMTEVMBMGA1UECwwMMTIzNDU2Nzg5MDEyMR4wHAYDVQQKDBVzcGluaWZleC1t
aWdyYXRpb24tYzEwIBcNMjYxMTI2MjIyOTAwWhgPMjEyNjExMDIyMjI5MDBaMEwx
EzARBgNVBAMMCnNweC1jb25uLTExFTATBgNVBAsMDDEyMzQ1Njc4OTAxMjEeMBwG
A1UECgwVc3BpbmlmZXgtbWlncmF0aW9uLWMxMFkwEwYHKoZIzj0CAQYIKoZIzj0D
AQcDQgAEeWu3qvyjjtsQXbxpCA6l7qUIYEemif2fmv+UWOGs/EYg0n63BiHA54Dq
Lxi0V7uymGybIVEohmy7sKQJEAAoIaNTMFEwHQYDVR0OBBYEFEdGQwZ95WKvRQAQ
ylpOCwu6+WKyMB8GA1UdIwQYMBaAFEdGQwZ95WKvRQAQylpOCwu6+WKyMA8GA1Ud
EwEB/wQFMAMBAf8wCgYIKoZIzj0EAwIDSQAwRgIhAO56/+Gp9tTfSt62WFz0vBgD
RapFMYgenx4xgQMKLu3JAiEAylkiYVSprtgFJFFcznzkaPfLkA3XVYlRYLiNnY7D
cEw=
-----END CERTIFICATE-----
`

const (
	fixtureCertB64 = "MIIB3jCCAYOgAwIBAgICEjQwCgYIKoZIzj0EAwIwTDETMBEGA1UEAwwKc3B4LWNvbm4tMTEVMBMGA1UECwwMMTIzNDU2Nzg5MDEyMR4wHAYDVQQKDBVzcGluaWZleC1taWdyYXRpb24tYzEwIBcNMjYxMTI2MjIyOTAwWhgPMjEyNjExMDIyMjI5MDBaMEwxEzARBgNVBAMMCnNweC1jb25uLTExFTATBgNVBAsMDDEyMzQ1Njc4OTAxMjEeMBwGA1UECgwVc3BpbmlmZXgtbWlncmF0aW9uLWMxMFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAEeWu3qvyjjtsQXbxpCA6l7qUIYEemif2fmv+UWOGs/EYg0n63BiHA54DqLxi0V7uymGybIVEohmy7sKQJEAAoIaNTMFEwHQYDVR0OBBYEFEdGQwZ95WKvRQAQylpOCwu6+WKyMB8GA1UdIwQYMBaAFEdGQwZ95WKvRQAQylpOCwu6+WKyMA8GA1UdEwEB/wQFMAMBAf8wCgYIKoZIzj0EAwIDSQAwRgIhAO56/+Gp9tTfSt62WFz0vBgDRapFMYgenx4xgQMKLu3JAiEAylkiYVSprtgFJFFcznzkaPfLkA3XVYlRYLiNnY7DcEw="
	fixtureBody    = `{"durationSeconds":900,"profileArn":"arn:aws:rolesanywhere:us-east-1:111122223333:profile/p1","roleArn":"arn:aws:iam::111122223333:role/R","trustAnchorArn":"arn:aws:rolesanywhere:us-east-1:111122223333:trust-anchor/t1"}`
	fixturePayload = "36e57fbf0133ab91f2b8bdd8730164062ea9f141b118c8a43e739101c013f64f"
	fixtureCRHash  = "4835155fcac5222fa37928be8ce77649e311dd0726c5c4d6a8659cb169ab6f63"
)

func fixtureCert(t *testing.T) *ClientCertificate {
	t.Helper()
	cb, _ := pem.Decode([]byte(fixtureCertPEM))
	require.NotNil(t, cb)
	cert, err := x509.ParseCertificate(cb.Bytes)
	require.NoError(t, err)
	kb, _ := pem.Decode([]byte(fixtureKeyPEM))
	require.NotNil(t, kb)
	k, err := x509.ParsePKCS8PrivateKey(kb.Bytes)
	require.NoError(t, err)
	key, ok := k.(*ecdsa.PrivateKey)
	require.True(t, ok)
	return &ClientCertificate{Certificate: cert, key: key}
}

func TestSign_KnownVector(t *testing.T) {
	cert := fixtureCert(t)
	now := time.Date(2021, 11, 3, 12, 0, 0, 0, time.UTC)
	req, err := http.NewRequest(http.MethodPost, "https://rolesanywhere.us-east-1.amazonaws.com/sessions", strings.NewReader(fixtureBody))
	require.NoError(t, err)
	require.NoError(t, sign(req, []byte(fixtureBody), cert, "us-east-1", now))

	wantCanonical := "POST\n/sessions\n\n" +
		"content-type:application/json\n" +
		"host:rolesanywhere.us-east-1.amazonaws.com\n" +
		"x-amz-date:20211103T120000Z\n" +
		"x-amz-x509:" + fixtureCertB64 + "\n\n" +
		"content-type;host;x-amz-date;x-amz-x509\n" + fixturePayload
	assert.Equal(t, wantCanonical, canonicalRequest("rolesanywhere.us-east-1.amazonaws.com", "20211103T120000Z", fixtureCertB64, "/sessions", []byte(fixtureBody)))

	wantSTS := "AWS4-X509-ECDSA-SHA256\n20211103T120000Z\n20211103/us-east-1/rolesanywhere/aws4_request\n" + fixtureCRHash
	assert.Equal(t, wantSTS, stringToSign("20211103T120000Z", "20211103/us-east-1/rolesanywhere/aws4_request", wantCanonical))

	assert.Equal(t, "20211103T120000Z", req.Header.Get("X-Amz-Date"))
	assert.Equal(t, fixtureCertB64, req.Header.Get("X-Amz-X509"))
	assert.Equal(t, "application/json", req.Header.Get("Content-Type"))

	auth := req.Header.Get("Authorization")
	m := regexp.MustCompile(`^AWS4-X509-ECDSA-SHA256 Credential=4660/20211103/us-east-1/rolesanywhere/aws4_request, SignedHeaders=content-type;host;x-amz-date;x-amz-x509, Signature=([0-9a-f]+)$`).FindStringSubmatch(auth)
	require.NotNil(t, m, auth)
	sig, err := hex.DecodeString(m[1])
	require.NoError(t, err)
	digest := sha256.Sum256([]byte(wantSTS))
	assert.True(t, ecdsa.VerifyASN1(&cert.key.PublicKey, digest[:], sig), "signature verifies over SHA-256 of the string to sign")
}

func TestSign_UsesUTCDateAndRegionInScope(t *testing.T) {
	cert := fixtureCert(t)
	loc := time.FixedZone("x", 14*3600)
	now := time.Date(2021, 11, 4, 1, 0, 0, 0, loc) // 2021-11-03T11:00:00Z
	req, err := http.NewRequest(http.MethodPost, "https://rolesanywhere.eu-west-2.amazonaws.com/sessions", nil)
	require.NoError(t, err)
	require.NoError(t, sign(req, nil, cert, "eu-west-2", now))
	assert.Equal(t, "20211103T110000Z", req.Header.Get("X-Amz-Date"))
	assert.Contains(t, req.Header.Get("Authorization"), "/20211103/eu-west-2/rolesanywhere/aws4_request,")
}

func TestSign_RequiresCertificate(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "https://h/sessions", nil)
	require.NoError(t, err)
	assert.Error(t, sign(req, nil, nil, "us-east-1", testNow))
	assert.Error(t, sign(req, nil, &ClientCertificate{}, "us-east-1", testNow))
}
