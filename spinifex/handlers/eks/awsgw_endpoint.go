package handlers_eks

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// ClusterOIDCIssuer returns the public OIDC issuer URL:
//
//	{gatewayBaseURL}/oidc/eks/{region}/{accountID}/{clusterName}
//
// gatewayBaseURL is the awsgw HTTPS endpoint without a trailing slash.
func ClusterOIDCIssuer(gatewayBaseURL, region, accountID, clusterName string) (string, error) {
	if gatewayBaseURL == "" {
		return "", errors.New("eks: ClusterOIDCIssuer empty gatewayBaseURL")
	}
	if region == "" {
		return "", errors.New("eks: ClusterOIDCIssuer empty region")
	}
	if accountID == "" {
		return "", errors.New("eks: ClusterOIDCIssuer empty accountID")
	}
	if clusterName == "" {
		return "", errors.New("eks: ClusterOIDCIssuer empty clusterName")
	}
	parsed, err := url.Parse(gatewayBaseURL)
	if err != nil {
		return "", fmt.Errorf("parse gatewayBaseURL %q: %w", gatewayBaseURL, err)
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("eks: gatewayBaseURL %q missing scheme or host", gatewayBaseURL)
	}
	for name, v := range map[string]string{"region": region, "accountID": accountID, "clusterName": clusterName} {
		if strings.ContainsAny(v, "/?#") {
			return "", fmt.Errorf("eks: %s %q contains URL-unsafe characters", name, v)
		}
	}
	base := strings.TrimRight(gatewayBaseURL, "/")
	return fmt.Sprintf("%s/oidc/eks/%s/%s/%s", base, region, accountID, clusterName), nil
}
