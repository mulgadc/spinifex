package sts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// ClusterJWKSResolver resolves the raw JWKS document published for an EKS
// cluster's OIDC issuer. Not found (missing account bucket or missing key)
// is (nil, false, nil); a dependency failure (KV unavailable, timeout) is
// reported as err. Implemented by the EKS handler and wired in at composition.
type ClusterJWKSResolver interface {
	LookupClusterJWKS(ctx context.Context, accountID, clusterName string) (jwks []byte, found bool, err error)
}

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

// FetchClusterJWKS resolves and decodes the JWKS for an EKS cluster via resolver.
// Returns (nil, nil) when the cluster has no JWKS published; callers map that to InvalidIdentityToken.
func FetchClusterJWKS(ctx context.Context, resolver ClusterJWKSResolver, accountID, clusterName string) (*JWKS, error) {
	if resolver == nil {
		return nil, errors.New("nil JWKS resolver")
	}
	if accountID == "" || clusterName == "" {
		return nil, errors.New("accountID and clusterName must be non-empty")
	}

	raw, found, err := resolver.LookupClusterJWKS(ctx, accountID, clusterName)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, nil
	}

	var jwks JWKS
	if err := json.Unmarshal(raw, &jwks); err != nil {
		return nil, fmt.Errorf("decode JWKS for cluster %s: %w", clusterName, err)
	}
	if len(jwks.Keys) == 0 {
		return nil, fmt.Errorf("JWKS for cluster %s has no keys", clusterName)
	}
	return &jwks, nil
}
