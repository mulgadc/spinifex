package handlers_sts

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"

	handlers_eks "github.com/mulgadc/spinifex/spinifex/handlers/eks"
	"github.com/nats-io/nats.go"
)

// JWK is one entry of an RFC 7517 JSON Web Key Set. EKS publishes EC P-256 keys;
// RSA fields are kept for future compatibility.
type JWK struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Use string `json:"use,omitempty"`
	Alg string `json:"alg,omitempty"`
	Crv string `json:"crv,omitempty"`
	X   string `json:"x,omitempty"`
	Y   string `json:"y,omitempty"`
	N   string `json:"n,omitempty"`
	E   string `json:"e,omitempty"`
}

// JWKS wraps a list of JWK entries as served at the `/keys` endpoint of an
// OIDC issuer's JWKS URL.
type JWKS struct {
	Keys []JWK `json:"keys"`
}

// FindByKID returns the JWK with the matching `kid` claim or nil if none of
// the keys match.
func (s *JWKS) FindByKID(kid string) *JWK {
	if s == nil {
		return nil
	}
	for i := range s.Keys {
		if s.Keys[i].Kid == kid {
			return &s.Keys[i]
		}
	}
	return nil
}

// eksIssuerPathSegments is the fixed path layout of a spinifex EKS OIDC issuer:
//
//	oidc / eks / {region} / {accountID} / {clusterName}
const (
	eksIssuerPathSegments = 5
	eksIssuerSegOIDC      = 0
	eksIssuerSegEKS       = 1
	eksIssuerSegRegion    = 2
	eksIssuerSegAccountID = 3
	eksIssuerSegCluster   = 4
)

// ParseEKSIssuerURL extracts (accountID, clusterName) from a spinifex EKS OIDC issuer URL:
//
//	https://{gateway-host}/oidc/eks/{region}/{accountID}/{clusterName}
//
// Strict 5-segment path parsing pins accountID and clusterName at fixed positions so a
// crafted `iss` claim cannot steer the JWKS lookup to a different cluster bucket.
func ParseEKSIssuerURL(issuer string) (accountID, clusterName string, err error) {
	u, err := url.Parse(issuer)
	if err != nil {
		return "", "", fmt.Errorf("parse issuer URL: %w", err)
	}
	if u.Scheme != "https" {
		return "", "", errors.New("issuer URL must use https scheme")
	}
	if u.Host == "" {
		return "", "", errors.New("issuer URL missing host")
	}
	segments := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(segments) != eksIssuerPathSegments ||
		segments[eksIssuerSegOIDC] != "oidc" || segments[eksIssuerSegEKS] != "eks" {
		return "", "", errors.New("issuer path must be /oidc/eks/{region}/{accountID}/{clusterName}")
	}
	if slices.Contains(segments, "") {
		return "", "", errors.New("issuer path has empty segment")
	}
	return segments[eksIssuerSegAccountID], segments[eksIssuerSegCluster], nil
}

// FetchClusterJWKS reads the JWKS for an EKS cluster from the per-account EKS KV bucket.
// Returns (nil, nil) when the cluster has no JWKS published; callers map that to InvalidIdentityToken.
func FetchClusterJWKS(js nats.JetStreamContext, accountID, clusterName string) (*JWKS, error) {
	if js == nil {
		return nil, errors.New("nil JetStream context")
	}
	if accountID == "" || clusterName == "" {
		return nil, errors.New("accountID and clusterName must be non-empty")
	}

	bucketName := handlers_eks.AccountBucketName(accountID)
	kv, err := js.KeyValue(bucketName)
	if err != nil {
		if errors.Is(err, nats.ErrBucketNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("open EKS account bucket %s: %w", bucketName, err)
	}

	entry, err := kv.Get(handlers_eks.OIDCJWKSKey(clusterName))
	if err != nil {
		if errors.Is(err, nats.ErrKeyNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("read JWKS for cluster %s: %w", clusterName, err)
	}

	var jwks JWKS
	if err := json.Unmarshal(entry.Value(), &jwks); err != nil {
		return nil, fmt.Errorf("decode JWKS for cluster %s: %w", clusterName, err)
	}
	if len(jwks.Keys) == 0 {
		return nil, fmt.Errorf("JWKS for cluster %s has no keys", clusterName)
	}
	return &jwks, nil
}
